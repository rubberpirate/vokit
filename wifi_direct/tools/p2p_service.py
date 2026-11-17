#!/usr/bin/env python3
"""Fixed-configuration system service; never executes code from the user's tree."""
import json,os,pathlib,re,signal,stat,sys,time
from types import SimpleNamespace

# Python -I excludes the script directory. It is an installed root-owned path.
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))
from p2p_helper import Helper,Nl80211,kernel_interfaces,station_snapshot
from p2p_private import PrivateHelper,inactive_private_group
from p2p_persistent import PersistentGroup

CONFIG=pathlib.Path('/etc/vivo-connect-p2p/service.json')

def read_json_file(file,uid,private=False):
    fd=os.open(file,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK|os.O_CLOEXEC)
    with os.fdopen(fd,'rb') as stream:
        info=os.fstat(stream.fileno())
        forbidden=0o077 if private else 0o022
        if not stat.S_ISREG(info.st_mode) or info.st_uid!=uid or info.st_mode&forbidden or info.st_size>65536:
            raise RuntimeError('配置文件的所有者、权限或大小不符合要求')
        data=stream.read(65537)
        if len(data)>65536:raise RuntimeError('配置文件过大')
        return json.loads(data)

def load_config(file=CONFIG):
    value=read_json_file(file,0)
    if not isinstance(value,dict):raise ValueError('服务配置无效')
    if type(value.get('uid')) is not int or value['uid']<=0:raise ValueError('必须绑定普通用户')
    if type(value.get('port')) is not int or not 0<value['port']<65536:raise ValueError('服务端口无效')
    if not isinstance(value.get('interface'),str) or not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.:-]{0,14}',value['interface']):raise ValueError('服务网卡无效')
    state=value.get('state_dir')
    if not isinstance(state,str) or not pathlib.Path(state).is_absolute():raise ValueError('状态目录必须为绝对路径')
    return value

def user_config(config):
    value=read_json_file(pathlib.Path(config['state_dir'])/'config.json',config['uid'],private=True)
    if not isinstance(value,dict) or not isinstance(value.get('apiToken'),str) or not re.fullmatch(r'[A-Za-z0-9._~-]{4,256}',value['apiToken']):raise ValueError('本机 API 配置无效')
    return value

class ServiceHelper(PrivateHelper):
    def __init__(self,config):
        self.service_config=config
        # Read and validate before the legacy constructor reads the same JSON.
        checked=user_config(config)
        super().__init__(SimpleNamespace(interface=config['interface'],state_dir=config['state_dir'],port=config['port'],
            driver='private',runtime_dir='/run/vivo-connect-p2p',owner_pid=None,recover_stale=None,service_managed=True))
        self.config=checked;self.registration=None;self.persistent=None
    def profile(self):
        if self.persistent is None:self.persistent=PersistentGroup('/var/lib/vivo-connect-p2p/'+str(self.service_config['uid'])+'-'+self.service_config['interface'])
        return self.persistent
    def supplicant_config(self,config):return self.profile().prepare(config)
    def interface_name(self):return 'p2p-vc-'+self.profile().mac.hex()[-6:]
    def create_private_interface(self,client,phy,name):client.create_go(phy,name,mac=self.profile().mac)
    def recover_private_orphans(self,cap):
        super().recover_private_orphans(cap)
        # systemd may remove RuntimeDirectory and its interface.json after an
        # interrupted shutdown. The root-owned persistent profile still proves
        # the exact GO name and MAC. Never recover a transmitting/foreign VIF.
        name=self.interface_name()
        info=next((i for i in kernel_interfaces() if i['name']==name),None)
        if not inactive_private_group(info,cap['phy']):return
        try:address=bytes.fromhex((pathlib.Path('/sys/class/net')/name/'address').read_text().strip().replace(':',''))
        except (OSError,ValueError):return
        if address!=self.profile().mac:return
        if self.stop or station_snapshot(self.args.interface)!=self.original:raise RuntimeError('校园网状态变化，取消服务残留恢复')
        current=next((i for i in kernel_interfaces() if i['name']==name),None)
        if current!=info:return
        client=Nl80211()
        try:client.delete_interface(info['index'])
        finally:client.close()
        self.stage('recovered-service-orphan',interface=name)
        if station_snapshot(self.args.interface)!=self.original:raise RuntimeError('服务残留恢复后校园网状态变化，已停止')
    def group_command(self):return self.profile().command(self.control,self.frequency_plan['goFrequency'])
    def group_started(self):self.profile().save(self.control)
    def on_heartbeat(self,response):
        super().on_heartbeat(response)
        self.profile().maintain(self,response)
    def api(self,route,body=None,method=None):
        if route=='/api/p2p/register':
            body={**body,'management':'system-service','persistentGroup':True};self.registration=body
        if route!='/api/p2p/heartbeat':return super().api(route,body,method)
        # Losing the unprivileged backend must not destroy an otherwise live GO.
        # Do not retain a pairing window while the backend cannot authorize it.
        try:
            self.config=user_config(self.service_config)
            return super().api(route,body,method)
        except (RuntimeError,OSError,ValueError):
            try:
                if self.registration:return super().api('/api/p2p/register',self.registration)
            except (RuntimeError,OSError,ValueError):pass
            return {'pairingActive':False}

def failure_diagnostics(error,stage):
    text=str(error).lower()
    reasons=[label for fragment,label in [('operation not permitted','operation-not-permitted'),('permission denied','permission-denied'),('read-only file system','read-only-filesystem'),('address already in use','address-in-use'),('dhcp 服务启动失败','dhcp-start-failed'),('已有无线助手','helper-already-active')] if fragment in text]
    return {'stage':'service-retry','failedAt':stage,'errorType':type(error).__name__,'errno':getattr(error,'errno',None),'reasons':reasons}

def main():
    if os.geteuid()!=0:raise RuntimeError('此入口仅由系统服务启动')
    config=load_config();stopping=False;current=None
    def stop(*unused):
        nonlocal stopping
        stopping=True
        if current:current.stop=True
    for name in (signal.SIGTERM,signal.SIGINT):signal.signal(name,stop)
    while not stopping:
        try:
            # No wireless changes until the configured user's backend is ready.
            current=ServiceHelper(config)
            status=current.api('/api/p2p/status')
            if status.get('ready'):raise RuntimeError('已有无线助手，等待其退出')
            current.start()
        except (RuntimeError,OSError,ValueError) as error:
            # Report only fixed labels and errno; never print exception text,
            # JSON, API credentials, addresses or group passwords.
            print(json.dumps(failure_diagnostics(error,getattr(current,'diagnostic_stage','initialization'))),flush=True)
        finally:
            if current:
                try:current.cleanup()
                finally:
                    if hasattr(current,'lock'):os.close(current.lock)
            current=None
        for _ in range(20):
            if stopping:break
            time.sleep(.5)

if __name__=='__main__':main()
