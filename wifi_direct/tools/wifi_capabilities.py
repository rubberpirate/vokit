#!/usr/bin/env python3
"""Read nl80211 capabilities without changing any interface or connection."""
import argparse,json,socket,struct

def attributes(data):
    result=[]; offset=0
    while offset+4<=len(data):
        size,kind=struct.unpack_from('HH',data,offset)
        if size<4 or offset+size>len(data):raise ValueError('Malformed netlink attribute')
        result.append((kind&0x3fff,data[offset+4:offset+size]));offset+=(size+3)&~3
    return result

def attr(kind,value):
    size=len(value)+4;return struct.pack('HH',size,kind)+value+b'\0'*((-size)%4)
def number(data):return int.from_bytes(data,'little')
def parse_combinations(data):
    result=[]
    for _,raw in attributes(data):
        props=dict(attributes(raw));limits=[]
        for _,limit in attributes(props.get(1,b'')):
            fields=dict(attributes(limit));limits.append({'max':number(fields.get(1,b'')), 'types':[t for t,_ in attributes(fields.get(2,b''))]})
        combo={'maxInterfaces':number(props.get(2,b'')),'channels':number(props.get(4,b'')),'limits':limits}
        required={2:1,9:1};supported={t for l in limits for t in l['types']}
        combo['stationAndP2pGo']=combo['maxInterfaces']>=2 and combo['channels']>=1 and set(required)<=supported and all(sum(required.get(t,0) for t in l['types'])<=l['max'] for l in limits)
        result.append(combo)
    return result

class Nl80211:
    def __init__(self):
        self.socket=socket.socket(socket.AF_NETLINK,socket.SOCK_RAW,16);self.socket.bind((0,0));self.socket.settimeout(5);self.seq=0
        family=self.request(0x10,3,attr(2,b'nl80211\0'))[0];self.family=number(dict(attributes(family)).get(1,b''))
    def request(self,family,command,data=b'',dump=False,ack=False):
        self.seq+=1;payload=struct.pack('BBH',command,1,0)+data
        packet=struct.pack('IHHII',16+len(payload),family,1|(0x300 if dump else 0)|(4 if ack else 0),self.seq,0)+payload
        self.socket.sendto(packet,(0,0));result=[]
        while True:
            data=self.socket.recv(262144);offset=0
            while offset+16<=len(data):
                length,kind,flags,seq,pid=struct.unpack_from('IHHII',data,offset);body=data[offset+16:offset+length];offset+=(length+3)&~3
                if seq!=self.seq:continue
                if kind==2:
                    error=struct.unpack_from('i',body)[0]
                    if error:raise OSError(-error,'nl80211 request failed')
                    if ack and not dump:return result
                elif kind==3:return result
                else:
                    result.append(body[4:])
                    if not dump:return result
    def interfaces(self):
        result=[]
        for reply in self.request(self.family,5,dump=True):
            info=dict(attributes(reply));name=info.get(4,b'').rstrip(b'\0').decode()
            if name:result.append({'name':name,'index':number(info.get(3,b'')),'phy':number(info.get(1,b'')),
                                   'type':number(info.get(5,b'')),'frequency':number(info.get(38,b''))})
        return result
    def delete_interface(self,index):
        # Mutating operation: caller must verify explicit ownership and type.
        self.request(self.family,8,attr(3,struct.pack('I',index)),ack=True)
    def create_go(self,phy,name,mac=None):
        # Allocate only a GO data interface, never another station/P2P_DEVICE.
        if not name.startswith('p2p-vc-') or len(name)>15:raise ValueError('Invalid private GO name')
        args=attr(1,struct.pack('I',phy))+attr(4,name.encode()+b'\0')+attr(5,struct.pack('I',9))
        if mac is not None:
            if not isinstance(mac,bytes) or len(mac)!=6 or mac[0]&1:raise ValueError('Invalid GO MAC')
            args+=attr(6,mac)
        self.request(self.family,7,args,ack=True)
    def probe(self,interface,include_channels=False):
        index=socket.if_nametoindex(interface);args=attr(3,struct.pack('I',index))
        info=dict(attributes(self.request(self.family,5,args)[0]));phy=number(info[1]);combos=[];frequencies={}
        for response in self.request(self.family,1,attr(174,b''),dump=True):
            props=dict(attributes(response))
            if number(props.get(1,b'\xff'))!=phy:continue
            if 120 in props:combos.extend(parse_combinations(props[120]))
            if include_channels:
                for _,band in attributes(props.get(22,b'')):
                    for _,raw in attributes(dict(attributes(band)).get(1,b'')):
                        values=dict(attributes(raw));mhz=number(values.get(1,b''))
                        frequencies[mhz]={'frequency':mhz,'disabled':2 in values,'noIr':3 in values,'radar':5 in values,'no20MHz':16 in values,'irConcurrent':15 in values}
        result={'interface':interface,'phy':phy,'frequency':number(info.get(38,b'')), 'combinations':combos,'supportsStationAndP2pGo':any(c['stationAndP2pGo'] for c in combos)}
        if include_channels:result['frequencies']=list(frequencies.values())
        return result
    def close(self):self.socket.close()

if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('interface');args=parser.parse_args();client=Nl80211()
    try:print(json.dumps(client.probe(args.interface)))
    finally:client.close()
