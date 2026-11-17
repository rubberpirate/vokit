#!/usr/bin/env python3
"""Project-local supplicant on an owned GO; no writes to system wpa/NM D-Bus."""
import errno,stat,collections,hashlib,json,os,pathlib,queue,re,select,shutil,socket,subprocess,tempfile,threading,time
from p2p_helper import Helper,ROOT,Nl80211,kernel_interfaces,station_snapshot,run

BINARY=ROOT/'build/private-p2p/bin/wpa_supplicant'
MARKER='vivo-p2p-group-only=1'

def verified_binary():
    manifest=BINARY.parent/'build.json'
    if not BINARY.is_file() or not manifest.is_file():
        raise RuntimeError('尚未构建项目独立 supplicant：先运行 python tools/build_p2p_supplicant.py（不要 sudo）')
    data=json.loads(manifest.read_text())
    if data.get('binarySha256')!=hashlib.sha256(BINARY.read_bytes()).hexdigest():
        raise RuntimeError('独立 supplicant 与构建记录不一致，请重新构建')
    for name,digest in data.get('inputs',{}).items():
        source=ROOT/name
        if not source.is_file() or hashlib.sha256(source.read_bytes()).hexdigest()!=digest:
            raise RuntimeError('独立 supplicant 构建输入已变化，请重新构建')
    return BINARY

def parse_status(text):
    return dict(line.split('=',1) for line in text.splitlines() if '=' in line)

class Control:
    """Attached datagram control; credentials remain in memory, never logs."""
    def __init__(self,server,client):
        self.socket=socket.socket(socket.AF_UNIX,socket.SOCK_DGRAM)
        self.path=pathlib.Path(client);self.events=collections.deque(maxlen=64);self.provision_requests=collections.deque(maxlen=8);self.invitation_results=collections.deque(maxlen=16)
        try:self.socket.bind(str(client));self.socket.connect(str(server));self.socket.settimeout(2)
        except BaseException:self.close();raise
    def event(self,text):
        if text.startswith('<') and '>' in text:text=text.split('>',1)[1]
        # P2P-GROUP-STARTED contains a passphrase. Keep only its event name.
        name=text.split(' ',1)[0].strip()
        if name=='P2P-INVITATION-RESULT':
            match=re.search(r'(?:^|\s)status=(\d{1,3})(?:\s|$)',text)
            if match and int(match[1])<=255:self.invitation_results.append(int(match[1]))
        if name=='P2P-PROV-DISC-PBC-REQ':
            fields=text.split()
            if len(fields)>1 and re.fullmatch(r'(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}',fields[1]):
                self.provision_requests.append((fields[1].lower(),time.monotonic()))
        if name in ('P2P-GROUP-STARTED','P2P-GROUP-REMOVED','P2P-GROUP-FORMATION-FAILURE','AP-ENABLED','AP-DISABLED','CTRL-EVENT-TERMINATING',
                    'AP-STA-CONNECTED','AP-STA-DISCONNECTED','AP-STA-POSSIBLE-PSK-MISMATCH','P2P-PROV-DISC-PBC-REQ','WPS-FAIL','WPS-SUCCESS','WPS-REG-SUCCESS','WPS-TIMEOUT'):
            self.events.append(name)
    def request(self,command):
        self.socket.send(command.encode());deadline=time.monotonic()+3
        while time.monotonic()<deadline:
            self.socket.settimeout(max(.01,deadline-time.monotonic()))
            text=self.socket.recv(65536).decode('utf8','replace').strip()
            if text.startswith('<'):self.event(text);continue
            return text
        raise TimeoutError('独立 supplicant 控制命令超时')
    def drain(self):
        while select.select([self.socket],[],[],0)[0]:
            self.event(self.socket.recv(65536).decode('utf8','replace').strip())
        events=list(self.events);self.events.clear();return events
    def close(self):
        self.socket.close()
        if self.path.exists():self.path.unlink()

DIAGNOSTICS={
    'Could not configure driver mode':'driver-mode-rejected',
    'Failed to initialize driver interface':'driver-init-failed',
    'Failed to enable P2P Device interface':'unexpected-p2p-device-attempt',
    'Failed to init P2P':'p2p-init-failed',
    'Operation not permitted':'permission-denied',
    'Device or resource busy':'driver-busy',
    'Failed to start AP functionality':'go-start-failed',
    'VIVO: refusing':'go-only-guard-refused',
    'Failed to set beacon':'beacon-start-failed',
}

def owned_runtime_groups(root=pathlib.Path('/run'),owner_uid=0):
    """Root-protected control sockets attest which private netdevs we created."""
    for runtime in root.glob('vivo-p2p-*'):
        try:
            metadata=runtime.lstat()
            if not stat.S_ISDIR(metadata.st_mode) or metadata.st_uid!=owner_uid or metadata.st_mode&0o077:continue
            config=runtime/'supplicant.conf'
            if config.is_symlink() or config.stat().st_uid!=owner_uid:continue
            content=config.read_text()
            if not content.startswith('ctrl_interface='+str(runtime/'ctrl')+'\n') or '\ndevice_name=vivo-connect-linux\n' not in content:continue
            ctrl=runtime/'ctrl'
            if ctrl.is_symlink() or ctrl.stat().st_uid!=owner_uid:continue
            for server in ctrl.iterdir():
                info=server.lstat()
                if re.fullmatch(r'p2p-vc-[0-9a-f]{6}',server.name) and stat.S_ISSOCK(info.st_mode) and info.st_uid==owner_uid:
                    yield runtime,server,None
            record=runtime/'interface.json'
            if record.is_file() and not record.is_symlink() and record.stat().st_uid==owner_uid:
                proof=json.loads(record.read_text())
                if re.fullmatch(r'p2p-vc-[0-9a-f]{6}',proof.get('name','')):
                    server=ctrl/proof['name']
                    if not server.exists():yield runtime,server,proof
        except (OSError,ValueError):continue

def inactive_private_group(info,phy):
    return bool(info and info.get('phy')==phy and info.get('type')==9 and not info.get('frequency')
                and re.fullmatch(r'p2p-vc-[0-9a-f]{6}',info.get('name','')))

class PrivateHelper(Helper):
    def __init__(self,args):
        super().__init__(args);self.private=None;self.control=None;self.runtime=None;self.owned=None
        self.diagnostics=collections.deque(maxlen=16);self.reader=None;self.dhcp_reader=None
        self.link_events=queue.SimpleQueue();self.progress={'associatedStations':0,'authorizedStations':0,'dhcpDiscoveries':0,'dhcpOffers':0,'dhcpAcks':0,'provisionRequests':0,'pairingAuthorizations':0,'wpsFailures':0,'wpsSuccesses':0,'pskFailures':0};self.last_progress=None
        self.pairing_id=None;self.pairing_peer=None;self.pairing_until=0
    def recover_private_orphans(self,cap):
        for runtime,server,proof in owned_runtime_groups(pathlib.Path(getattr(getattr(self,'args',None),'runtime_dir','/run'))):
            info=next((i for i in kernel_interfaces() if i['name']==server.name),None)
            if not inactive_private_group(info,cap['phy']):continue
            if proof and any(info.get(k)!=proof.get(k) for k in ('name','index','phy','type')):continue
            control=None
            try:
                control=Control(server,runtime/'recovery-client')
                control.request('PING')
                continue # An answering or busy service must never be taken over.
            except OSError as error:
                if error.errno not in (errno.ECONNREFUSED,errno.ENOENT):continue
            except (TimeoutError,ValueError):continue
            finally:
                if control:control.close()
            if self.stop:raise RuntimeError('已取消')
            if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('校园网状态已变化，停止残留接口恢复')
            current=next((i for i in kernel_interfaces() if i['name']==server.name),None)
            if current!=info or not inactive_private_group(current,cap['phy']):continue
            client=Nl80211()
            try:client.delete_interface(current['index'])
            finally:client.close()
            self.stage('recovered-private-orphan',interface=server.name)
            if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('残留接口恢复后校园网状态变化，已停止')

    def same_owned(self):
        if not self.owned:return None
        return next((i for i in kernel_interfaces() if all(i[k]==self.owned[k] for k in ('index','name','phy','type'))),None)
    def guard(self):
        if self.stop:raise RuntimeError('已取消')
        if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('校园网连接变化，正在停止本次独立直连组')
        if self.private is not None and self.private.poll() is not None:
            self.stage('private-supplicant-exited',exitCode=self.private.returncode,diagnostics=list(self.diagnostics))
            raise RuntimeError('独立 supplicant 已退出；阶段输出不包含组密码')
    def start(self):
        binary=verified_binary() # No interface is created for a missing/stale build.
        cap=self.preflight()
        self.runtime=pathlib.Path(tempfile.mkdtemp(prefix='vivo-p2p-',dir=getattr(self.args,'runtime_dir','/run')))
        config=self.runtime/'supplicant.conf'
        config.write_text('ctrl_interface='+str(self.runtime/'ctrl')+'\n'
                          'device_name=vivo-connect-linux\ndevice_type=1-0050F204-1\n'
                          'config_methods=virtual_push_button\np2p_no_group_iface=1\nap_scan=1\n')
        config.chmod(0o600)
        process_config=self.supplicant_config(config)
        name=self.interface_name()
        self.stage('private-create-interface',interface=name,phy=cap['phy'],goFrequency=self.frequency_plan['goFrequency'])
        client=Nl80211()
        try:self.create_private_interface(client,cap['phy'],name)
        finally:client.close()
        # Record ownership immediately, including for failed process startup.
        self.owned=next((i for i in kernel_interfaces() if i['name']==name),None)
        if not self.owned or self.owned['phy']!=cap['phy'] or self.owned['type']!=9:
            raise RuntimeError('内核未创建独立 P2P GO，拒绝启动进程')
        record=self.runtime/'interface.json'
        record.write_text(json.dumps({k:self.owned[k] for k in ('name','index','phy','type')}));record.chmod(0o600)
        self.guard()
        self.private=subprocess.Popen([str(binary),'-Dnl80211','-i',name,'-c',str(process_config)],
            stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,env={**os.environ,'LC_ALL':'C'})
        def read_output():
            for line in self.private.stdout:
                for marker,label in DIAGNOSTICS.items():
                    if marker in line and label not in self.diagnostics:self.diagnostics.append(label)
        self.reader=threading.Thread(target=read_output,daemon=True);self.reader.start()
        server=self.runtime/'ctrl'/name
        for _ in range(100):
            self.guard()
            if server.exists():break
            time.sleep(.2)
        else:raise RuntimeError('独立控制套接字未就绪')
        self.control=Control(server,self.runtime/'client')
        if self.control.request('VIVO_P2P_GROUP_ONLY')!=MARKER:raise RuntimeError('进程未确认 GO 专用构建，拒绝继续')
        if self.control.request('ATTACH')!='OK':raise RuntimeError('无法订阅独立组事件')
        self.stage('private-create-group',interface=name,control='private-unix',systemDbus=False)
        if self.control.request(self.group_command())!='OK':
            raise RuntimeError('独立 supplicant 拒绝创建 GO；未退回系统 GroupAdd')
        for _ in range(150):
            self.guard();events=self.control.drain()
            if any(e in events for e in ('P2P-GROUP-REMOVED','P2P-GROUP-FORMATION-FAILURE','AP-DISABLED','CTRL-EVENT-TERMINATING')):
                self.stage('private-group-failed',events=events,diagnostics=list(self.diagnostics));raise RuntimeError('独立 P2P GO 未能保持运行')
            status=parse_status(self.control.request('STATUS'));info=self.same_owned()
            if info and info['frequency']==self.frequency_plan['goFrequency'] and status.get('wpa_state')=='COMPLETED' and status.get('mode')=='P2P GO':break
            time.sleep(.2)
        else:
            self.stage('private-group-timeout',kernelInterface=self.same_owned(),diagnostics=list(self.diagnostics))
            raise RuntimeError('独立 GO 未就绪；没有配置地址或 DHCP')
        self.group={'interface':name}
        self.group_started()
        self.configure_group(cap)
    def supplicant_config(self,config):return config
    def interface_name(self):return 'p2p-vc-'+self.id.replace('-','')[:6]
    def create_private_interface(self,client,phy,name):client.create_go(phy,name)
    def group_command(self):return 'P2P_GROUP_ADD freq='+str(self.frequency_plan['goFrequency'])
    def group_started(self):pass
    def credentials(self):
        status=parse_status(self.control.request('STATUS'))
        # Android's connectGroupOwner targets the GO's P2P Device Address,
        # which need not equal the data interface's BSSID.
        ssid=status.get('ssid','');bssid=status.get('p2p_device_address') or status.get('bssid','');password=self.control.request('P2P_GET_PASSPHRASE')
        if not ssid.startswith('DIRECT-') or not 8<=len(password)<=63 or password.startswith('FAIL'):
            raise RuntimeError('无法从独立 GO 读取有效连接参数')
        return ssid,password,bssid
    def check_group_alive(self):
        self.guard();events=self.control.drain()
        if any(e in events for e in ('P2P-GROUP-REMOVED','P2P-GROUP-FORMATION-FAILURE','AP-DISABLED','CTRL-EVENT-TERMINATING')):
            raise RuntimeError('独立 P2P 组已结束')
        status=parse_status(self.control.request('STATUS'));info=self.same_owned()
        if not info or info['frequency']!=self.frequency_plan['goFrequency'] or status.get('wpa_state')!='COMPLETED' or status.get('mode')!='P2P GO':
            raise RuntimeError('独立 GO 运行状态发生变化')
        self.update_progress(events)
    def start_dhcp(self,group_iface,network,ip):
        process=subprocess.Popen(self.dhcp_command(group_iface,network,ip)+['--log-dhcp','--log-facility=-'],
            stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,env={**os.environ,'LC_ALL':'C'})
        self.dhcp_diagnostics=[]
        def read_dhcp():
            for line in process.stdout:
                # Hostnames, MACs and raw lease lines never leave this reader.
                for marker,key in [('DHCPDISCOVER(','dhcpDiscoveries'),('DHCPOFFER(','dhcpOffers'),('DHCPACK(','dhcpAcks')]:
                    if marker in line:self.link_events.put(key)
                for marker,key in [('setting capabilities failed','capability-setup-failed'),('missing required capability','missing-capability'),('Permission denied','permission-denied'),('Operation not permitted','operation-not-permitted'),('Address already in use','address-in-use'),('Read-only file system','read-only-filesystem'),('unknown interface','interface-unavailable')]:
                    if marker.lower() in line.lower() and key not in self.dhcp_diagnostics:self.dhcp_diagnostics.append(key)
        self.dhcp_reader=threading.Thread(target=read_dhcp,daemon=True);self.dhcp_reader.start()
        return process
    def update_progress(self,events):
        results=getattr(self.control,'invitation_results',None)
        if isinstance(results,collections.deque):
            while results:
                status=results.popleft()
                self.progress['invitationStatus']=status
                self.stage('persistent-invitation-result',status=status,success=status==0)
        mapping={'P2P-PROV-DISC-PBC-REQ':'provisionRequests','WPS-FAIL':'wpsFailures','WPS-TIMEOUT':'wpsFailures','WPS-SUCCESS':'wpsSuccesses','WPS-REG-SUCCESS':'wpsSuccesses','AP-STA-POSSIBLE-PSK-MISMATCH':'pskFailures'}
        for event in events:
            if event in mapping:self.progress[mapping[event]]+=1
        while not self.link_events.empty():
            key=self.link_events.get_nowait();self.progress[key]+=1
        associated=authorized=0;answer=self.control.request('STA-FIRST');seen=set()
        for _ in range(32):
            lines=answer.splitlines();mac=lines[0] if lines else ''
            if not re.fullmatch(r'(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}',mac) or mac in seen:break
            seen.add(mac);flags=parse_status(answer).get('flags','')
            associated+=int('[ASSOC]' in flags);authorized+=int('[AUTHORIZED]' in flags)
            answer=self.control.request('STA-NEXT '+mac)
        self.progress.update(associatedStations=associated,authorizedStations=authorized)
        if self.progress!=self.last_progress:
            self.stage('private-link-progress',**self.progress);self.last_progress=dict(self.progress)
    def heartbeat_body(self):return {'id':self.id,'progress':dict(self.progress)}
    def on_heartbeat(self,response):
        now=time.monotonic()
        active=response.get('pairingActive') is True and isinstance(response.get('pairingId'),str)
        pairing_id=response.get('pairingId') if active else None
        if self.pairing_peer and (pairing_id!=self.pairing_id or now>=self.pairing_until):
            self.control.request('WPS_CANCEL');self.pairing_peer=None
            self.stage('private-pairing-window-closed')
        self.pairing_id=pairing_id
        while self.control.provision_requests:
            peer,created=self.control.provision_requests.popleft()
            if not active or now-created>15 or self.pairing_peer or self.progress['authorizedStations']>0:continue
            # Upstream WPS_PBC's p2p_dev_addr filter authorizes only this
            # requesting P2P Device, not every nearby WPS enrollee.
            result=self.control.request('WPS_PBC p2p_dev_addr='+peer)
            if result!='OK':
                self.stage('private-pairing-authorization-failed',reason='wps-command-rejected');continue
            self.pairing_peer=peer;self.pairing_until=now+120
            self.progress['pairingAuthorizations']+=1
            self.stage('private-pairing-authorized',method='wps-pbc',peerRestricted=True,expiresInSeconds=120)
    def cleanup(self):
        if self.registered:
            try:self.api('/api/p2p/unregister',{'id':self.id})
            except Exception:pass
        if self.dhcp is not None:
            try:
                self.dhcp.terminate()
                try:self.dhcp.wait(timeout=3)
                except subprocess.TimeoutExpired:self.dhcp.kill();self.dhcp.wait()
            except OSError as error:
                # Continue cleaning the owned supplicant/interface even if a
                # dropped-UID DHCP child cannot be signalled by an old unit.
                self.stage('private-child-stop-failed',child='dhcp',errno=error.errno)
            if self.dhcp_reader:self.dhcp_reader.join(timeout=1)
        if self.control:
            # Only this process's group. Never issue system supplicant commands.
            try:self.control.request('P2P_GROUP_REMOVE '+self.owned['name'])
            except Exception:pass
        if self.private:
            self.private.terminate()
            try:self.private.wait(timeout=3)
            except subprocess.TimeoutExpired:self.private.kill();self.private.wait()
            if self.reader:self.reader.join(timeout=1)
        if self.control:self.control.close();self.control=None
        try:
            if self.same_owned():
                if getattr(self,'address',None):
                    try:run(['ip','address','del',self.address,'dev',self.owned['name']])
                    except RuntimeError:pass
                client=Nl80211()
                try:client.delete_interface(self.owned['index'])
                finally:client.close()
                self.stage('private-group-cleaned',interface=self.owned['name'])
            elif self.owned:self.stage('private-cleanup-skipped',reason='owned-interface-gone-or-identity-changed')
        except Exception as error:self.stage('cleanup-error',message=str(error))
        if self.runtime:shutil.rmtree(self.runtime)
