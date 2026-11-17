#!/usr/bin/env python3
"""Temporary, root-only P2P GO helper. Never replaces the station connection.

Defaults to a project-local GO-only supplicant, iproute2 and dnsmasq. The legacy
system D-Bus route requires --driver system. Ctrl-C removes only its own group.
"""
import argparse,fcntl,queue,threading,ipaddress,json,os,pathlib,re,signal,subprocess,tempfile,time,urllib.request,urllib.error,uuid
from wifi_capabilities import Nl80211,attributes,number
ROOT=pathlib.Path(__file__).resolve().parent.parent
SERVICE='fi.w1.wpa_supplicant1'; BASE='/fi/w1/wpa_supplicant1'; IFACE=SERVICE+'.Interface'; P2P=IFACE+'.P2PDevice'; GROUP=SERVICE+'.Group'

def process_identity(pid):
    """Linux start time prevents a reused PID from extending a GUI-owned group."""
    try:
        proc=pathlib.Path('/proc')/str(pid)
        fields=(proc/'stat').read_text().rsplit(')',1)[1].split()
        if fields[0]=='Z':return None
        return proc.stat().st_uid,fields[19]
    except (OSError,ValueError,IndexError):return None

def watch_owner(helper,pid,start_time):
    expected=(pathlib.Path(helper.args.state_dir)/'config.json').stat().st_uid,str(start_time)
    lease=getattr(helper.args,'owner_lease',None)
    def alive():return process_identity(pid)==expected and (not lease or pathlib.Path(lease).is_file())
    if not alive():raise RuntimeError('启动窗口已关闭或进程身份不匹配，取消无线启动')
    def watch():
        while not helper.stop:
            if not alive():
                helper.stop=True
                return
            time.sleep(.5)
    threading.Thread(target=watch,daemon=True).start()

def run(args,timeout=10):
    result=subprocess.run(args,check=False,capture_output=True,text=True,timeout=timeout,env={**os.environ,'LC_ALL':'C'})
    if result.returncode:
        operation=args[0]
        if args[0]=='busctl':
            action=next((name for name in ['call','get-property','set-property'] if name in args),None)
            if action:
                index=args.index(action)
                if len(args)>index+4:operation=f'busctl {action} {args[index+3]}.{args[index+4]} [{args[index+2]}]'
        raise RuntimeError(f'{operation} 操作失败：'+result.stderr.strip()[:500])
    return result.stdout

def dbus(action,obj,interface,name,*args):
    output=run(['busctl','--system','--json=short',action,SERVICE,obj,interface,name,*map(str,args)])
    if not output.strip():return None
    value=json.loads(output)
    data=value.get('data',[])
    # busctl wraps call return arguments, but get-property returns the value itself.
    return data[0] if action=='call' and value.get('type')=='o' and isinstance(data,list) else data

def prop(obj,interface,name):return dbus('get-property',obj,interface,name)
def objects():return prop(BASE,SERVICE,'Interfaces')
def object_disappeared(error):
    message=str(error)
    return any(marker in message for marker in ('UnknownObject','UnknownMethod','No such object','doesn\'t exist'))

def named_objects(name):
    matches=[]
    for obj in objects():
        try:current=prop(obj,IFACE,'Ifname')
        except RuntimeError as error:
            if object_disappeared(error):continue
            raise
        if current==name:matches.append(obj)
    return matches
def unbox(value):
    if isinstance(value,dict):
        if set(value)=={'type','data'}:return unbox(value['data'])
        return {key:unbox(v) for key,v in value.items()}
    if isinstance(value,list):return [unbox(v) for v in value]
    return value

def inspect_group_policy(interface,paths=None):
    paths=list(objects() if paths is None else paths)
    names={obj:prop(obj,IFACE,'Ifname') for obj in paths}
    stations=[obj for obj,name in names.items() if name==interface]
    if len(stations)!=1:raise RuntimeError('supplicant 未唯一管理指定校园网接口')
    dedicated=[obj for obj,name in names.items() if name=='p2p-dev-'+interface]
    # D-Bus GroupAdd forwards to the global P2P management object. With a single
    # controlled station it is normal for Interfaces to contain only that name.
    # Do not create a second P2P_DEVICE: the existing driver's device may be
    # internal and the advertised limit for that type is usually one.
    other_stations=[name for name in names.values() if not name.startswith('p2p-') and name!=interface]
    if other_stations:raise RuntimeError('存在其他受管无线接口，无法唯一确认全局 P2P 管理归属，拒绝自动创建')
    parent=dedicated[0] if len(dedicated)==1 else stations[0]
    config=unbox(prop(parent,P2P,'P2PDeviceConfig'))
    if not isinstance(config,dict):raise RuntimeError('无法读取 P2PDeviceConfig，拒绝猜测组接口策略')
    no_group_iface=config.get('NoGroupIface')
    state=prop(stations[0],IFACE,'State')
    return parent,{'controlInterface':names[parent], 'dedicatedP2pParent':bool(dedicated),
                   'stationState':state,'noGroupIface':no_group_iface,
                   'policyAllowsSeparateGroup':no_group_iface is False and state=='completed'}

def require_group_policy(policy):
    if policy['noGroupIface'] is not False:
        raise RuntimeError('NoGroupIface 为 true 或未知，拒绝在可能复用校园网接口的情况下创建 GO；未修改 supplicant 配置')
    if policy['stationState']!='completed':raise RuntimeError('校园网 station 未处于 completed 状态，拒绝创建 P2P 组')

def station_snapshot(interface):
    address=json.loads(run(['ip','-j','address','show','dev',interface]))
    ips=sorted(x['local'] for d in address for x in d.get('addr_info',[]) if x.get('family')=='inet')
    connection=run(['nmcli','-g','GENERAL.CON-UUID','device','show',interface]).strip()
    return {'addresses':ips,'connection':connection}
def choose_subnet(routes):
    networks=[ipaddress.ip_network(r['dst'],strict=False) for r in routes if r.get('dst') not in (None,'default')]
    for index in range(49,64):
        candidate=ipaddress.ip_network(f'192.168.{index}.0/24')
        if not any(candidate.overlaps(n) for n in networks if n.version==4):return candidate
    raise RuntimeError('没有可用的独立 P2P 子网（现有路由与候选子网冲突）')
def plan_frequency(cap):
    combinations=[c for c in cap.get('combinations',[]) if c.get('stationAndP2pGo')]
    max_channels=max((c['channels'] for c in combinations),default=0)
    channels={c['frequency']:c for c in cap.get('frequencies',[])}
    current=cap.get('frequency',0)
    def allowed(mhz):
        c=channels.get(mhz)
        return c is not None and not any(c.get(flag,True) for flag in ['disabled','noIr','radar','no20MHz'])
    if not max_channels:raise RuntimeError('缺少 Station + P2P GO 并发组合')
    if allowed(current):chosen=current;reason='same-channel'
    else:
        if max_channels<2:raise RuntimeError('当前校园网信道不允许直接启动 GO，且并发组合只允许同信道；拒绝更改校园网信道或监管设置')
        chosen=next((mhz for mhz in [2412,2437,2462,5180,5200,5220,5240] if allowed(mhz)),None)
        if chosen is None:raise RuntimeError('没有内核明确允许的非 DFS P2P 频率，拒绝修改监管域或校园网')
        reason='separate-channel'
    return {'stationFrequency':current,'goFrequency':chosen,'maxChannels':max_channels,'reason':reason,
            'stationChannelFlags':channels.get(current), 'goChannelFlags':channels[chosen]}

def group_info(obj):
    name=prop(obj,IFACE,'Ifname');group=prop(obj,P2P,'Group')
    if group in ('/',None):return None
    if prop(group,GROUP,'Role')!='GO':return None
    return {'object':obj,'group':group,'interface':name}
def bytes_value(value):
    return bytes(value) if isinstance(value,list) else bytes.fromhex(value.replace(':',''))

def kernel_interfaces():
    client=Nl80211()
    try:return client.interfaces()
    finally:client.close()

def link_is_inactive(name):
    records=json.loads(run(['ip','-j','address','show','dev',name]))
    return len(records)==1 and 'UP' not in records[0].get('flags',[]) and not records[0].get('addr_info')

def parse_group_event(record):
    if record.get('type')!='signal' or record.get('interface')!=P2P:return None
    member=record.get('member')
    if member not in ('GroupStarted','GroupFormationFailure','GroupFinished'):return None
    payload=unbox(record.get('payload',{}).get('data',[]))
    if isinstance(payload,list) and len(payload)==1:payload=payload[0]
    return {'member':member,'path':record.get('path'),'payload':payload}

class GroupEvents:
    def __init__(self,bus_args=None,sender=SERVICE):
        self.events=queue.Queue();self.ready=threading.Event();self.bus_args=bus_args or ['--system']
        self.probe_path='/io/vivo/Connect/P2pProbe/'+uuid.uuid4().hex
        self.probe_interface='io.vivo.Connect.P2pProbe';self.probe_token=uuid.uuid4().hex
        matches=["type='signal',"+("sender='"+sender+"'," if sender else "")+"interface='"+P2P+"',member='"+member+"'" for member in ('GroupStarted','GroupFormationFailure','GroupFinished')]
        matches.append("type='signal',interface='"+self.probe_interface+"',member='Ready',path='"+self.probe_path+"'")
        self.process=subprocess.Popen(['busctl',*self.bus_args,'--json=short',*['--match='+m for m in matches],'monitor'],
            stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,env={**os.environ,'LC_ALL':'C'})
        def read_messages():
            for line in self.process.stdout:
                if len(line)>1048576:continue
                try:
                    record=json.loads(line)
                    if record.get('path')==self.probe_path and record.get('interface')==self.probe_interface and record.get('member')=='Ready':
                        if unbox(record.get('payload',{}).get('data'))==[self.probe_token]:self.ready.set()
                        continue
                    event=parse_group_event(record)
                except (ValueError,TypeError):continue
                if event:self.events.put(event)
        def read_status():
            for line in self.process.stderr:pass # Drain diagnostics; readiness uses the explicit probe signal.
        self.readers=[threading.Thread(target=f,daemon=True) for f in (read_messages,read_status)]
        for reader in self.readers:reader.start()
    def wait_ready(self):
        for _ in range(20):
            if self.process.poll() is not None:break
            run(['busctl',*self.bus_args,'emit',self.probe_path,self.probe_interface,'Ready','s',self.probe_token],timeout=2)
            if self.ready.wait(.15):return
        raise RuntimeError('无法在 GroupAdd 前确认 P2P 事件订阅已就绪，拒绝盲目创建')
    def drain(self):
        result=[]
        while True:
            try:result.append(self.events.get_nowait())
            except queue.Empty:return result
    def close(self):
        self.process.terminate()
        try:self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:self.process.kill();self.process.wait()
        for reader in self.readers:reader.join(timeout=1)


class Helper:
    def __init__(self,args):
        self.args=args;self.group=None;self.dhcp=None;self.registered=False;self.stop=False;self.id=str(uuid.uuid4());self.events=None;self.before_kernel=None;self.group_requested=False
        self.config=json.loads((pathlib.Path(args.state_dir)/'config.json').read_text());self.backend=f'http://127.0.0.1:{args.port}'
    def stage(self,name,**safe):
        self.diagnostic_stage=name
        print(json.dumps({'stage':name,**safe},ensure_ascii=False),flush=True)
    def api(self,route,body=None,method=None):
        data=json.dumps(body).encode() if body is not None else None
        request=urllib.request.Request(self.backend+route,data=data,method=method or ('POST' if data else 'GET'),headers={'Authorization':'Bearer '+self.config['apiToken'],'Content-Type':'application/json'})
        # Explicitly bypass proxy environment variables for the loopback helper channel.
        opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(request,timeout=10) as response:return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code==401:raise RuntimeError('本机后端鉴权失败，请确认界面和无线助手使用相同的 --state-dir') from error
            raise RuntimeError(f'本机后端接口 {route} 返回 HTTP {error.code}') from error
        except (urllib.error.URLError,TimeoutError) as error:
            raise RuntimeError(f'无法连接本机后端 {self.backend}，请先在项目目录运行 npm start，并确认后端 PORT 与界面 --port 一致') from error
    def recover_stale(self,name,phy):
        if name==self.args.interface:raise RuntimeError('拒绝清理校园网 station 接口')
        if not re.fullmatch(r'p2p-[A-Za-z0-9_.:-]{1,11}',name):raise RuntimeError('恢复目标必须是 P2P 接口名')
        candidates=[i for i in kernel_interfaces() if i['name']==name]
        if not candidates:
            self.stage('stale-already-gone',interface=name);return
        if len(candidates)!=1:raise RuntimeError('指定残留接口不存在或不唯一')
        info=candidates[0]
        if info['phy']!=phy or info['type']!=9 or info['frequency'] or not name.startswith('p2p-') or not link_is_inactive(name):
            raise RuntimeError('仅允许清理同 phy、DOWN、无地址且频率为 0 的指定 P2P GO；未清理')
        registered=named_objects(name)
        for obj in registered:
            # The explicit recovery target is never the station object.
            dbus('call',BASE,SERVICE,'RemoveInterface','o',obj)
        remaining=[i for i in kernel_interfaces() if i['index']==info['index']]
        if remaining:
            # A registered object must be gone before removing its orphan netdev.
            if named_objects(name):raise RuntimeError('supplicant 仍管理该组接口，拒绝直接删除内核接口')
            current=remaining[0]
            if current!=info or not link_is_inactive(name):raise RuntimeError('残留接口状态已变化，取消清理')
            client=Nl80211()
            try:client.delete_interface(info['index'])
            finally:client.close()
        self.stage('stale-recovered',interface=name)
    def find_started_group(self,event):
        data=event.get('payload')
        if event.get('path') not in self.before or not isinstance(data,dict) or data.get('role')!='GO':return None
        obj=data.get('interface_object');group=data.get('group_object')
        if not isinstance(obj,str) or not isinstance(group,str) or obj in self.before:return None
        name=prop(obj,IFACE,'Ifname')
        if name==self.args.interface:return None
        return {'object':obj,'group':group,'interface':name}
    def consume_group_events(self,events,pending=None):
        relevant=[e for e in events if e.get('path') in self.before]
        # A start and its removal may already be queued together. Process the
        # terminal event before dereferencing an object that no longer exists.
        for event in relevant:
            if event['member']=='GroupFormationFailure':
                raise RuntimeError('supplicant 报告 P2P 组形成失败；未配置组地址或 DHCP')
            if event['member']=='GroupFinished':
                raise RuntimeError('P2P 组启动后已被撤销；若 NetworkManager 日志含 not trying to connect，请参阅 docs/backend.md 的 NetworkManager 外部组回收修复；重试属性读取无法解决')
        for event in relevant:
            if event['member']=='GroupStarted':pending=event
        if pending:
            try:candidate=self.find_started_group(pending)
            except RuntimeError as error:
                if not object_disappeared(error):raise
                self.stage('group-object-unavailable',reason='started-object-disappeared-or-not-readable')
                return pending
            if candidate:self.group=candidate;return None
        return pending
    def cleanup_pending_interfaces(self):
        if not getattr(self,'group_requested',False) or getattr(self,'before_kernel',None) is None:return
        for info in kernel_interfaces():
            if info['index'] in self.before_kernel or info['phy']!=self.station_phy or info['type']!=9 or info['name']==self.args.interface:continue
            matching=named_objects(info['name'])
            if matching:
                for obj in matching:dbus('call',obj,P2P,'Disconnect')
            elif not info['frequency'] and link_is_inactive(info['name']):
                client=Nl80211()
                try:client.delete_interface(info['index'])
                finally:client.close()
            else:self.stage('cleanup-pending',interface=info['name'],reason='active-unregistered-group-not-deleted')
    def recover_private_orphans(self,cap):pass
    def preflight(self):
        if self.stop:raise RuntimeError('启动窗口已关闭，取消无线启动')
        if os.geteuid()!=0:raise RuntimeError('需要管理员权限：请用 sudo python tools/p2p_helper.py --interface '+self.args.interface)
        self.lock=os.open('/run/lock/vivo-connect-p2p.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW|os.O_CLOEXEC,0o600)
        try:fcntl.flock(self.lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:raise RuntimeError('已有 vivo P2P 助手运行')
        if not re.fullmatch(r'[A-Za-z0-9_.:-]{1,15}',self.args.interface):raise RuntimeError('无效的 station 接口名称')
        client=Nl80211()
        try:cap=client.probe(self.args.interface,include_channels=True)
        finally:client.close()
        if not cap['supportsStationAndP2pGo']:raise RuntimeError('网卡未报告 Station + P2P GO 并发能力，拒绝替换校园网连接')
        if not cap['frequency']:raise RuntimeError('station 当前没有已连接频率，请先连接校园网')
        self.frequency_plan=plan_frequency(cap)
        self.stage('preflight',frequencyPlan=self.frequency_plan)
        self.original=station_snapshot(self.args.interface)
        if not self.original['addresses'] or self.original['connection'] in ('','--'):raise RuntimeError('无法确认需保留的校园网连接')
        self.api('/api/p2p/status') # Verify backend version and credentials before network changes.
        self.station_phy=cap['phy']
        if getattr(self.args,'recover_stale',None):
            self.recover_stale(self.args.recover_stale,cap['phy'])
            if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('清理后校园网状态变化，已停止')
        self.recover_private_orphans(cap)
        existing=kernel_interfaces()
        groups=[i for i in existing if i['phy']==cap['phy'] and i['type'] in (8,9)]
        if groups:raise RuntimeError('已有 P2P 数据接口 '+','.join(i['name'] for i in groups)+'；拒绝叠加创建。仅可对已确认的无活动残留使用 --recover-stale <接口名>')
        self.before_kernel={i['index'] for i in existing}
        return cap
    def start(self):
        cap=self.preflight()
        self.before=set(objects())
        for obj in self.before:
            try:
                if prop(obj,P2P,'Group')!='/':raise RuntimeError('已有活动 P2P 组；拒绝接管或删除其他程序的组')
            except subprocess.SubprocessError:raise
        parent,policy=inspect_group_policy(self.args.interface,self.before)
        require_group_policy(policy)
        if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('校园网状态在准备期间变化，取消创建')
        self.parent=parent
        self.events=GroupEvents();self.events.wait_ready()
        self.stage('create-group',controlInterface=policy['controlInterface'],goFrequency=self.frequency_plan['goFrequency'])
        self.group_requested=True
        dbus('call',parent,P2P,'GroupAdd','a{sv}',2,'persistent','b','false','frequency','i',self.frequency_plan['goFrequency'])
        last_errors=[];pending=None
        for attempt in range(150):
            if self.stop:raise RuntimeError('已取消')
            pending=self.consume_group_events(self.events.drain(),pending)
            # Retain property polling as a fallback; do not hide every getter error.
            if not self.group:
                for obj in set(objects())-self.before:
                    try:info=group_info(obj)
                    except RuntimeError as error:
                        if not object_disappeared(error):raise
                        if len(last_errors)<3:last_errors.append(str(error))
                        continue
                    if info and info['interface']!=self.args.interface:self.group=info;break
            if self.group:
                active=[i for i in kernel_interfaces() if i['name']==self.group['interface']]
                if active and active[0]['type']==9 and active[0]['phy']==cap['phy'] and active[0]['frequency']:
                    if active[0]['frequency']!=self.frequency_plan['goFrequency']:raise RuntimeError('实际 GO 频率与规划不一致，已停止')
                    break
            if attempt%5==0 and station_snapshot(self.args.interface)!=self.original:raise RuntimeError('等待 P2P 组时校园网连接变化，已取消')
            time.sleep(.2)
        else:
            candidates=[i for i in kernel_interfaces() if i['phy']==cap['phy'] and i['type']==9]
            self.stage('group-start-timeout',kernelGroups=candidates,propertyErrors=last_errors)
            raise RuntimeError('未收到有效 GroupStarted 或 GO 未启用；GroupAdd 接受不代表组已就绪，正在清理本次半创建接口')
        self.configure_group(cap)
    def credentials(self):
        ssid=bytes_value(prop(self.group['group'],GROUP,'SSID')).decode('utf8')
        passphrase=prop(self.group['group'],GROUP,'Passphrase')
        bssid=':'.join(f'{x:02x}' for x in bytes_value(prop(self.group['group'],GROUP,'BSSID')))
        return ssid,passphrase,bssid
    def check_group_alive(self):pass
    def heartbeat_body(self):return {'id':self.id}
    def on_heartbeat(self,response):pass
    def dhcp_command(self,group_iface,network,ip):
        return ['dnsmasq','--keep-in-foreground','--conf-file=/dev/null','--port=0','--bind-interfaces','--interface='+group_iface,
            '--dhcp-range='+str(network.network_address+20)+','+str(network.network_address+200)+',255.255.255.0,1h',
            '--dhcp-option=3,'+ip,'--dhcp-option=6','--leasefile-ro','--pid-file=']
    def start_dhcp(self,group_iface,network,ip):
        return subprocess.Popen(self.dhcp_command(group_iface,network,ip),stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    def configure_group(self,cap):
        group_iface=self.group['interface'];self.stage('group-created',interface=group_iface)
        self.stage('group-check-station')
        if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('检测到校园网连接发生变化，正在回滚本次 P2P 组')
        self.stage('group-check-interface')
        client=Nl80211()
        try:
            values=dict(attributes(client.request(client.family,5,__import__('wifi_capabilities').attr(3,__import__('struct').pack('I',__import__('socket').if_nametoindex(group_iface))))[0]))
            if number(values.get(1,b''))!=cap['phy'] or number(values.get(5,b''))!=9:raise RuntimeError('新接口并非同网卡的 P2P GO，拒绝配置')
        finally:client.close()
        self.stage('group-select-subnet')
        network=choose_subnet(json.loads(run(['ip','-j','route','show','table','main'])))
        ip=str(network.network_address+1);self.address=ip+'/24'
        self.stage('group-configure-address')
        run(['ip','link','set','dev',group_iface,'up']);run(['ip','address','add',self.address,'dev',group_iface])
        self.stage('group-check-route')
        route=json.loads(run(['ip','-j','route','get',str(network.network_address+20),'from',ip]))
        if not route or route[0].get('dev')!=group_iface:raise RuntimeError('独立子网被路由策略重定向，拒绝修改 VPN/校园网策略；请为 P2P 子网配置直连后重试')
        self.stage('group-read-credentials')
        ssid,passphrase,bssid=self.credentials()
        self.stage('group-start-dhcp')
        self.dhcp=self.start_dhcp(group_iface,network,ip)
        time.sleep(.3)
        if self.dhcp.poll() is not None:
            reader=getattr(self,'dhcp_reader',None)
            if reader:reader.join(timeout=1)
            self.stage('group-dhcp-failed',diagnostics=list(getattr(self,'dhcp_diagnostics',[])))
            raise RuntimeError('独立接口 DHCP 服务启动失败（可能端口占用或权限问题）')
        self.stage('group-register-backend')
        if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('校园网连接发生变化，正在回滚')
        self.api('/api/p2p/register',{'id':self.id,'stationInterface':self.args.interface,'groupInterface':group_iface,'ip':ip,'cidr':str(network),
            'ssid':ssid,'passphrase':passphrase,'bssid':bssid,'frequency':self.frequency_plan['goFrequency'],'stationFrequency':cap['frequency']})
        self.registered=True
        print(json.dumps({'status':'ready','interface':group_iface,'gateway':ip,'stationPreserved':True,'frequency':self.frequency_plan['goFrequency'],'stationFrequency':cap['frequency']},ensure_ascii=False),flush=True)
        if getattr(self.args,'owner_pid',None):self.stage('gui-owned',cleanupOnWindowExit=True)
        elif getattr(self.args,'service_managed',False):self.stage('system-service-owned',cleanupOnWindowExit=False)
        else:print('保持本终端运行；另一个终端执行 npm run scan 并选择 Wi-Fi Direct。Ctrl-C 清理本次直连网络。',flush=True)
        while not self.stop:
            time.sleep(2)
            if self.stop:break
            self.check_group_alive()
            if self.dhcp.poll() is not None:raise RuntimeError('P2P DHCP 已退出，正在清理')
            if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('校园网连接发生变化，停止本次 P2P 组')
            self.on_heartbeat(self.api('/api/p2p/heartbeat',self.heartbeat_body()))
    def cleanup(self):
        if self.registered:
            try:self.api('/api/p2p/unregister',{'id':self.id})
            except Exception:pass
        if self.dhcp is not None:
            self.dhcp.terminate()
            try:self.dhcp.wait(timeout=3)
            except subprocess.TimeoutExpired:self.dhcp.kill();self.dhcp.wait()
        if self.group:
            if getattr(self,'address',None):
                try:run(['ip','address','del',self.address,'dev',self.group['interface']])
                except Exception:pass
            try:dbus('call',self.group['object'],P2P,'Disconnect')
            except Exception:pass
        try:self.cleanup_pending_interfaces()
        except Exception as error:self.stage('cleanup-error',message=str(error))
        if getattr(self,'events',None):self.events.close()
        # Never RemoveInterface/Disconnect the station, touch its NM profile,
        # alter default routes, enable forwarding, or stop the system supplicant.

if __name__=='__main__':
    parser=argparse.ArgumentParser()
    parser.add_argument('--interface',required=True)
    parser.add_argument('--driver',choices=['private','system'],default='private',help='private uses a project-local GO-only supplicant; system is the legacy D-Bus route')
    parser.add_argument('--check',action='store_true')
    parser.add_argument('--recover-stale',metavar='INTERFACE')
    parser.add_argument('--state-dir',default=str(ROOT/'.vivo-connect'))
    parser.add_argument('--port',type=int,default=8765)
    parser.add_argument('--owner-pid',type=int)
    parser.add_argument('--owner-start-time')
    parser.add_argument('--owner-lease')
    args=parser.parse_args()
    if not 0<args.port<65536:parser.error('port 不合法')
    if args.owner_lease and not args.owner_pid:parser.error('owner-lease 需要 owner-pid')
    if bool(args.owner_pid) != bool(args.owner_start_time):parser.error('owner-pid 与 owner-start-time 必须同时提供')
    if args.owner_pid is not None and (args.owner_pid<=1 or not args.owner_start_time.isdecimal()):parser.error('owner 进程身份不合法')
    if args.driver=='private':
        from p2p_private import PrivateHelper,verified_binary
        if args.recover_stale:parser.error('--recover-stale 仅用于 --driver system；独立模式不接管系统 supplicant 的残留组')
        if args.check:
            client=Nl80211()
            try:cap=client.probe(args.interface,include_channels=True)
            finally:client.close()
            plan=plan_frequency(cap);cap.pop('frequencies',None)
            print(json.dumps({'driver':'private','binary':str(verified_binary()),'capabilities':cap,'frequencyPlan':plan,'networkChanged':False,'systemSupplicantChanged':False},ensure_ascii=False))
            raise SystemExit(0)
    elif args.check:
        if os.geteuid()!=0:parser.error('--check 读取系统 supplicant 需要 sudo')
        client=Nl80211()
        try:cap=client.probe(args.interface,include_channels=True)
        finally:client.close()
        paths=objects();names=[prop(obj,IFACE,'Ifname') for obj in paths]
        _,policy=inspect_group_policy(args.interface,paths)
        frequency_plan=plan_frequency(cap);cap.pop('frequencies',None)
        print(json.dumps({'capabilities':cap,'frequencyPlan':frequency_plan,'supplicantInterfaces':names,**policy,'readyForConcurrentGroup':cap['supportsStationAndP2pGo'] and policy['policyAllowsSeparateGroup'],'networkChanged':False},ensure_ascii=False))
        raise SystemExit(0)
    helper=PrivateHelper(args) if args.driver=='private' else Helper(args)
    def stop(*unused):helper.stop=True
    for name in [signal.SIGINT,signal.SIGTERM]:signal.signal(name,stop)
    try:
        if args.owner_pid is not None:watch_owner(helper,args.owner_pid,args.owner_start_time)
        helper.start()
    except Exception as error:print(str(error),file=__import__('sys').stderr);raise SystemExit(1)
    finally:helper.cleanup()
