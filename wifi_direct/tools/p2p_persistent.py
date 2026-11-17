"""Persistent private GO profile and invitations to previously paired peers only."""
import hashlib,os,pathlib,re,stat,tempfile,time

MAC=re.compile(r'(?:[0-9a-f]{2}:){5}[0-9a-f]{2}',re.I)
def atomic_private(file,data):
    with tempfile.NamedTemporaryFile(dir=file.parent,delete=False) as stream:
        temporary=pathlib.Path(stream.name)
        try:
            stream.write(data);stream.flush();os.fsync(stream.fileno());os.fchmod(stream.fileno(),0o600)
            os.replace(temporary,file)
        finally:temporary.unlink(missing_ok=True)

def private_data(file):
    fd=os.open(file,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC|os.O_NONBLOCK)
    with os.fdopen(fd,'rb') as stream:
        info=os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077 or info.st_size>1048576:raise RuntimeError('持久组文件权限或大小无效')
        result=stream.read(1048577)
        if len(result)>1048576:raise RuntimeError('持久组文件过大')
        return result

class PersistentGroup:
    def __init__(self,directory):
        self.directory=pathlib.Path(directory)
        self.directory.mkdir(mode=0o700,parents=True,exist_ok=True)
        info=self.directory.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode&0o077:raise RuntimeError('持久组目录必须由服务用户独占')
        identity=self.directory/'go-address'
        if identity.exists() or identity.is_symlink():raw=private_data(identity)
        else:
            raw=bytearray(os.urandom(6));raw[0]=(raw[0]|2)&0xfe;raw=bytes(raw)
            atomic_private(identity,raw)
        if len(raw)!=6 or raw[0]&1 or not raw[0]&2:raise RuntimeError('持久组地址无效')
        self.mac=raw;self.file=self.directory/'supplicant.conf';self.last_signature=None;self.next_invite=0;self.peer_index=0
    def prepare(self,runtime_config):
        # Keep the saved network blocks intact, replacing only per-run control
        # paths and fixed service policy. Only the private supplicant writes it.
        old=private_data(self.file).decode('utf8') if self.file.exists() or self.file.is_symlink() else ''
        blocks=''
        start=re.search(r'^network=\{',old,re.M)
        if start:blocks=old[start.start():]
        header=runtime_config.read_text()+'update_config=1\npersistent_reconnect=1\np2p_device_random_mac_addr=0\np2p_interface_random_mac_addr=0\n'
        atomic_private(self.file,(header+blocks).encode())
        return self.file
    def networks(self,control):
        networks=[]
        for line in control.request('LIST_NETWORKS').splitlines()[1:]:
            fields=line.split('\t')
            if len(fields)>=4 and fields[0].isdigit() and '[P2P-PERSISTENT]' in fields[3]:
                network=int(fields[0])
                if control.request(f'GET_NETWORK {network} mode').strip()=='3':networks.append(network)
        return networks
    def command(self,control,frequency):
        networks=self.networks(control)
        return 'P2P_GROUP_ADD '+('persistent='+str(networks[0]) if networks else 'persistent')+' freq='+str(frequency)
    def peers(self,control):
        peers=[]
        for network in self.networks(control):
            value=control.request(f'GET_NETWORK {network} p2p_client_list').strip()
            for item in value.split():
                # Address/mask pairs are a general config representation; only
                # exact peer addresses can be used as an invitation target.
                parts=item.split('/')
                if len(parts)>2 or (len(parts)==2 and parts[1].lower()!='ff:ff:ff:ff:ff:ff'):continue
                if MAC.fullmatch(parts[0]) and int(parts[0][:2],16)&1==0 and parts[0].lower()!='00:00:00:00:00:00':peers.append(parts[0].lower())
        return list(dict.fromkeys(peers))[:32]
    def save(self,control):
        # Hash the client list, not log or expose it. SAVE_CONFIG persists the
        # actual SSID, PSK and persistent client state, including empty groups.
        signature=hashlib.sha256((' '.join(map(str,self.networks(control)))+' '+str(self.peers(control))).encode()).digest()
        if signature!=self.last_signature:
            if control.request('SAVE_CONFIG').strip()!='OK':raise RuntimeError('无法保存持久组，拒绝假装支持免扫码重连')
            self.last_signature=signature
    def maintain(self,helper,response):
        self.save(helper.control)
        helper.progress['persistentNetworks']=len(self.networks(helper.control))
        helper.progress['persistentPeers']=len(self.peers(helper.control))
        if response.get('reconnectRequested') is not True or response.get('pairingActive') or helper.progress['associatedStations']>0:return
        now=time.monotonic()
        if now<self.next_invite:return
        peers=self.peers(helper.control)
        if not peers:return
        peer=peers[self.peer_index%len(peers)];self.peer_index+=1;self.next_invite=now+15
        if helper.control.request('P2P_PEER '+peer).startswith('FAIL'):
            result=helper.control.request('P2P_FIND 5 type=social dev_id='+peer)
            helper.progress['reconnectDiscoveries']=helper.progress.get('reconnectDiscoveries',0)+1
            helper.stage('persistent-peer-discovery',accepted=result.strip()=='OK')
            # Try the same peer after discovery, rather than skipping it.
            self.peer_index-=1;self.next_invite=now+6;return
        result=helper.control.request('P2P_INVITE group='+helper.owned['name']+' peer='+peer)
        helper.progress['reconnectInvitations']=helper.progress.get('reconnectInvitations',0)+1
        helper.stage('persistent-peer-invitation',accepted=result.strip()=='OK')
