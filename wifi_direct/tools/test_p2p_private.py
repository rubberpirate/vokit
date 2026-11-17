#!/usr/bin/env python3
"""Private P2P lifecycle guards: simulated kernel/process, no real Wi-Fi changes."""
import os,errno,collections,contextlib,io,json,pathlib,socket,struct,tempfile,threading,unittest,time
from types import SimpleNamespace
from unittest.mock import Mock,patch
import p2p_helper as h
import p2p_private as p
from wifi_capabilities import Nl80211,attr,attributes

@contextlib.contextmanager
def private_environment(fail_add=False,frequency=2412):
    with tempfile.TemporaryDirectory() as directory,contextlib.ExitStack() as stack:
        root=pathlib.Path(directory);(root/'config.json').write_text('{"apiToken":"test-only"}')
        runtime=root/'runtime';runtime.mkdir();(runtime/'ctrl').mkdir()
        helper=p.PrivateHelper(SimpleNamespace(interface='wlp2s0',state_dir=str(root),port=8765,recover_stale=None))
        helper.id='a'*32;name='p2p-vc-aaaaaa';(runtime/'ctrl'/name).touch()
        state={'created':False,'frequency':0};commands=[];api_calls=[]
        station={'name':'wlp2s0','index':3,'phy':0,'type':2,'frequency':5785}
        def interfaces():
            return [station]+([{'name':name,'index':7,'phy':0,'type':9,'frequency':state['frequency']}] if state['created'] else [])
        channel=lambda f,no_ir=False:{'frequency':f,'disabled':False,'noIr':no_ir,'radar':False,'no20MHz':False}
        cap={'phy':0,'frequency':5785,'supportsStationAndP2pGo':True,'combinations':[{'stationAndP2pGo':True,'channels':2}],
             'frequencies':[channel(5785,True),channel(2412)]}
        nl=Mock();nl.probe.return_value=cap
        nl.create_go.side_effect=lambda phy,n:state.update(created=True)
        nl.delete_interface.side_effect=lambda index:state.update(created=False)
        nl.request.return_value=[attr(1,struct.pack('I',0))+attr(5,struct.pack('I',9))]
        def command(args,timeout=10):
            commands.append(args)
            if args[:4]==['ip','-j','route','show']:return '[]'
            if args[:4]==['ip','-j','route','get']:return json.dumps([{'dev':name}])
            return ''
        def api(route,body=None):
            api_calls.append((route,body))
            if route=='/api/p2p/register':helper.stop=True
            return {'ready':True}
        control=Mock();control.drain.return_value=[];control.provision_requests=collections.deque()
        def request(command):
            if command=='VIVO_P2P_GROUP_ONLY':return p.MARKER
            if command.startswith('P2P_GROUP_ADD'):
                state['frequency']=frequency
                return 'FAIL' if fail_add else 'OK'
            if command=='STATUS':return 'wpa_state=COMPLETED\nmode=P2P GO\nssid=DIRECT-xy\nbssid=02:01:02:03:04:05\n'
            if command=='P2P_GET_PASSPHRASE':return 'private-test-password'
            return 'OK'
        control.request.side_effect=request
        process=Mock();process.poll.return_value=None;process.stdout=[]
        stage=stack.enter_context(patch.object(helper,'stage'))
        for module in (h,p):
            stack.enter_context(patch.object(module,'Nl80211',return_value=nl))
            stack.enter_context(patch.object(module,'kernel_interfaces',side_effect=interfaces))
            stack.enter_context(patch.object(module,'station_snapshot',return_value={'addresses':['10.0.0.2'],'connection':'campus'}))
            stack.enter_context(patch.object(module,'run',side_effect=command))
        stack.enter_context(patch.object(p,'verified_binary',return_value=pathlib.Path('/private/wpa_supplicant')))
        stack.enter_context(patch.object(p,'Control',return_value=control))
        stack.enter_context(patch.object(p.tempfile,'mkdtemp',return_value=str(runtime)))
        stack.enter_context(patch.object(h.os,'geteuid',return_value=0))
        real_open=h.os.open
        stack.enter_context(patch.object(h.os,'open',side_effect=lambda path,*args,**kwargs:99 if str(path)=='/run/lock/vivo-connect-p2p.lock' else real_open(path,*args,**kwargs)))
        stack.enter_context(patch.object(h.fcntl,'flock'))
        stack.enter_context(patch.object(h.time,'sleep'))
        stack.enter_context(patch.object(socket,'if_nametoindex',return_value=7))
        # Any system supplicant access is a regression in private mode.
        system=stack.enter_context(patch.object(h,'dbus',side_effect=AssertionError('system D-Bus used')))
        stack.enter_context(patch.object(h,'objects',side_effect=AssertionError('system objects used')))
        popen=stack.enter_context(patch.object(p.subprocess,'Popen',return_value=process))
        stack.enter_context(patch.object(helper,'api',side_effect=api))
        stack.enter_context(patch('builtins.print'))
        yield SimpleNamespace(helper=helper,state=state,nl=nl,control=control,process=process,stage=stage,popen=popen,commands=commands,api_calls=api_calls,system=system,name=name)

class PrivateTests(unittest.TestCase):
    def test_orphan_recovery_requires_private_inactive_group_and_protected_record(self):
        info={'name':'p2p-vc-aaaaaa','index':9,'phy':0,'type':9,'frequency':0}
        self.assertTrue(p.inactive_private_group(info,0))
        for changed in [{'type':2},{'frequency':2412},{'phy':1},{'name':'wlp2s0'},{'name':'p2p-other'}]:
            self.assertFalse(p.inactive_private_group({**info,**changed},0))
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);runtime=root/'vivo-p2p-test';runtime.mkdir(mode=0o700)
            ctrl=runtime/'ctrl';ctrl.mkdir()
            (runtime/'supplicant.conf').write_text('ctrl_interface='+str(ctrl)+'\ndevice_name=vivo-connect-linux\n')
            (runtime/'interface.json').write_text(json.dumps(info))
            self.assertEqual(list(p.owned_runtime_groups(root,os.getuid()))[0][2],info)
            runtime.chmod(0o755)
            self.assertEqual(list(p.owned_runtime_groups(root,os.getuid())),[])
    def test_orphan_recovery_never_removes_answering_control_service(self):
        helper=object.__new__(p.PrivateHelper)
        info={'name':'p2p-vc-aaaaaa','index':9,'phy':0,'type':9,'frequency':0}
        control=Mock();control.request.return_value='PONG'
        with patch.object(p,'owned_runtime_groups',return_value=[(pathlib.Path('/run/vivo-test'),pathlib.Path('/run/vivo-test/ctrl/p2p-vc-aaaaaa'),None)]),patch.object(p,'kernel_interfaces',return_value=[info]),patch.object(p,'Control',return_value=control),patch.object(p,'Nl80211') as nl:
            helper.recover_private_orphans({'phy':0})
            nl.assert_not_called()
    def test_orphan_recovery_deletes_only_attested_inactive_interface(self):
        helper=object.__new__(p.PrivateHelper);helper.stop=False;helper.args=SimpleNamespace(interface='wlp2s0');helper.original={'campus':'preserved'};helper.stage=Mock()
        info={'name':'p2p-vc-aaaaaa','index':9,'phy':0,'type':9,'frequency':0}
        with patch.object(p,'owned_runtime_groups',return_value=[(pathlib.Path('/run/vivo-test'),pathlib.Path('/run/vivo-test/ctrl/p2p-vc-aaaaaa'),info)]),patch.object(p,'kernel_interfaces',return_value=[info]),patch.object(p,'Control',side_effect=ConnectionRefusedError(errno.ECONNREFUSED,'gone')),patch.object(p,'station_snapshot',return_value=helper.original),patch.object(p,'Nl80211') as nl:
            helper.recover_private_orphans({'phy':0})
            nl.return_value.delete_interface.assert_called_once_with(9)
            nl.return_value.close.assert_called_once()

    def test_create_go_packet_never_allocates_station_or_p2p_device(self):
        client=object.__new__(Nl80211);client.seq=0;client.family=42;client.socket=Mock()
        client.socket.recv.return_value=struct.pack('IHHIIi',20,2,0,1,0,0)
        client.create_go(0,'p2p-vc-abcdef')
        packet=client.socket.sendto.call_args.args[0];values=dict(attributes(packet[20:]))
        self.assertEqual(packet[16],7);self.assertEqual(values[5],struct.pack('I',9))
        self.assertEqual(values[4],b'p2p-vc-abcdef\0');self.assertEqual(values[1],struct.pack('I',0))
    def test_campus_interface_name_cannot_be_allocated_by_private_api(self):
        client=object.__new__(Nl80211);client.request=Mock()
        with self.assertRaises(ValueError):client.create_go(0,'wlp2s0')
        client.request.assert_not_called()
    def test_full_private_start_reuses_backend_without_system_dbus(self):
        with private_environment() as e:
            e.helper.start()
            self.assertTrue(e.helper.registered)
            e.nl.create_go.assert_called_once_with(0,e.name)
            self.assertIn(['ip','address','add','192.168.49.1/24','dev',e.name],e.commands)
            body=next(body for route,body in e.api_calls if route=='/api/p2p/register')
            self.assertEqual(body['groupInterface'],e.name);self.assertEqual(body['passphrase'],'private-test-password')
            self.assertNotIn('private-test-password',str(e.stage.call_args_list))
            args=e.popen.call_args_list[0].args[0]
            self.assertEqual(args[:4],['/private/wpa_supplicant','-Dnl80211','-i',e.name]);self.assertNotIn('-u',args)
            e.helper.cleanup();e.nl.delete_interface.assert_called_once_with(7);e.system.assert_not_called()
            self.assertTrue(any(route=='/api/p2p/unregister' for route,body in e.api_calls))
            self.assertFalse(any('wlp2s0' in c for c in e.commands))
    def test_rejected_group_never_starts_dhcp_and_cleans_allocated_go(self):
        with private_environment(fail_add=True) as e:
            with self.assertRaisesRegex(RuntimeError,'拒绝创建 GO'):e.helper.start()
            e.helper.cleanup()
            self.assertEqual(e.popen.call_count,1);self.assertFalse(e.helper.registered)
            e.nl.delete_interface.assert_called_once_with(7)
            self.assertFalse(any(c[:3]==['ip','address','add'] for c in e.commands))
    def test_dhcp_signal_failure_does_not_abandon_owned_interface_cleanup(self):
        with private_environment() as e:
            e.helper.start()
            e.helper.dhcp=Mock()
            e.helper.dhcp.terminate.side_effect=PermissionError(errno.EPERM,'test-owned-child')
            e.helper.cleanup()
            e.process.terminate.assert_called()
            e.nl.delete_interface.assert_called_once_with(7)
            e.stage.assert_any_call('private-child-stop-failed',child='dhcp',errno=errno.EPERM)
    def test_wrong_kernel_frequency_does_not_configure_group(self):
        with private_environment(frequency=2437) as e:
            with self.assertRaisesRegex(RuntimeError,'未就绪'):e.helper.start()
            e.helper.cleanup()
            self.assertEqual(e.popen.call_count,1);self.assertFalse(e.helper.registered)
    def test_wrong_build_marker_is_rejected_before_group_add(self):
        with private_environment() as e:
            e.control.request.side_effect=None;e.control.request.return_value='UNKNOWN COMMAND'
            with self.assertRaisesRegex(RuntimeError,'GO 专用构建'):e.helper.start()
            e.helper.cleanup();self.assertEqual(e.control.request.call_args_list[0].args[0],'VIVO_P2P_GROUP_ONLY')
            self.assertFalse(any(c.args[0].startswith('P2P_GROUP_ADD') for c in e.control.request.call_args_list))
    def test_cleanup_will_not_delete_a_replaced_interface(self):
        with private_environment() as e:
            e.helper.start()
            with patch.object(p,'kernel_interfaces',return_value=[{'name':e.name,'index':8,'phy':0,'type':9,'frequency':2412}]):e.helper.cleanup()
            e.nl.delete_interface.assert_not_called()
    def test_campus_change_stops_private_group(self):
        with private_environment() as e:
            e.helper.start();e.helper.stop=False
            with patch.object(p,'station_snapshot',return_value={'addresses':['10.0.0.3'],'connection':'different'}):
                with self.assertRaisesRegex(RuntimeError,'校园网连接变化'):e.helper.check_group_alive()
            e.helper.cleanup();e.nl.delete_interface.assert_called_once_with(7)
    def test_group_removal_is_detected_even_if_process_still_alive(self):
        with private_environment() as e:
            e.helper.start();e.helper.stop=False;e.control.drain.return_value=['P2P-GROUP-REMOVED']
            with self.assertRaisesRegex(RuntimeError,'已结束'):e.helper.check_group_alive()
            e.helper.cleanup()
    def test_invitation_result_records_only_numeric_status(self):
        control=object.__new__(p.Control)
        control.events=collections.deque();control.provision_requests=collections.deque();control.invitation_results=collections.deque(maxlen=16)
        control.event('<3>P2P-INVITATION-RESULT status=8 peer=02:01:02:03:04:05 passphrase="do-not-retain"')
        control.event('<3>P2P-INVITATION-RESULT status=999')
        control.event('<3>P2P-INVITATION-RESULT status=invalid')
        self.assertEqual(list(control.invitation_results),[8])
        self.assertEqual(list(control.events),[])
    def test_missing_binary_never_enters_network_preflight(self):
        helper=object.__new__(p.PrivateHelper);helper.preflight=Mock()
        with patch.object(p,'verified_binary',side_effect=RuntimeError('missing')):
            with self.assertRaisesRegex(RuntimeError,'missing'):helper.start()
        helper.preflight.assert_not_called()
    def test_control_does_not_retain_credentials_from_unsolicited_events(self):
        with tempfile.TemporaryDirectory() as directory:
            path=pathlib.Path(directory);server=socket.socket(socket.AF_UNIX,socket.SOCK_DGRAM)
            server.bind(str(path/'server'));control=p.Control(path/'server',path/'client')
            def respond():
                command,client=server.recvfrom(4096)
                server.sendto(b'<3>P2P-GROUP-STARTED p2p-vc-abcdef GO passphrase="do-not-log"',client)
                server.sendto(b'wpa_state=COMPLETED\nmode=P2P GO\n',client)
            thread=threading.Thread(target=respond);thread.start()
            try:
                status=p.parse_status(control.request('STATUS'))
                self.assertEqual(status['mode'],'P2P GO');self.assertEqual(control.drain(),['P2P-GROUP-STARTED'])
                self.assertNotIn('do-not-log',str(control.__dict__))
            finally:control.close();server.close();thread.join(timeout=2)
    def test_manifest_detects_tampered_binary_or_build_input(self):
        import hashlib
        with tempfile.TemporaryDirectory() as directory:
            root=pathlib.Path(directory);binary=root/'wpa_supplicant';binary.write_bytes(b'good');input_file=root/'config';input_file.write_text('good')
            data={'binarySha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'inputs':{'config':hashlib.sha256(input_file.read_bytes()).hexdigest()}}
            (root/'build.json').write_text(json.dumps(data))
            with patch.object(p,'BINARY',binary),patch.object(p,'ROOT',root):
                self.assertEqual(p.verified_binary(),binary)
                input_file.write_text('changed')
                with self.assertRaisesRegex(RuntimeError,'构建输入已变化'):p.verified_binary()
                binary.write_bytes(b'bad')
                with self.assertRaisesRegex(RuntimeError,'构建记录不一致'):p.verified_binary()
    def test_progress_counts_current_station_flags_and_only_safe_events(self):
        with private_environment() as e:
            def response(command):
                if command=='STA-FIRST':return '02:01:02:03:04:05\nflags=[AUTH][ASSOC][AUTHORIZED]\n'
                return 'FAIL'
            e.control.request.side_effect=response;e.helper.control=e.control
            e.helper.link_events.put('dhcpDiscoveries');e.helper.link_events.put('dhcpAcks')
            e.helper.update_progress(['P2P-PROV-DISC-PBC-REQ','AP-STA-POSSIBLE-PSK-MISMATCH'])
            progress=e.helper.heartbeat_body()['progress']
            for field in ['associatedStations','authorizedStations','dhcpDiscoveries','dhcpAcks','provisionRequests','pskFailures']:self.assertEqual(progress[field],1)
            self.assertNotIn('02:01:02:03:04:05',str(e.stage.call_args_list))
    def test_qr_uses_p2p_device_address_when_it_differs_from_bssid(self):
        with private_environment() as e:
            e.helper.control=e.control
            e.control.request.side_effect=lambda command:'wpa_state=COMPLETED\nssid=DIRECT-xy\nbssid=02:01:02:03:04:05\np2p_device_address=02:09:08:07:06:05\n' if command=='STATUS' else 'test-password'
            self.assertEqual(e.helper.credentials(),('DIRECT-xy','test-password','02:09:08:07:06:05'))
    def test_wps_authorizes_only_requesting_peer_during_active_qr(self):
        with private_environment() as e:
            e.helper.control=e.control;e.control.provision_requests.append(('02:01:02:03:04:05',time.monotonic()))
            e.helper.on_heartbeat({'pairingActive':True,'pairingId':'qr-job'})
            e.control.request.assert_called_once_with('WPS_PBC p2p_dev_addr=02:01:02:03:04:05')
            self.assertEqual(e.helper.progress['pairingAuthorizations'],1)
            self.assertNotIn('02:01:02:03:04:05',str(e.stage.call_args_list))
            e.helper.on_heartbeat({'pairingActive':False})
            self.assertEqual(e.control.request.call_args.args[0],'WPS_CANCEL');self.assertIsNone(e.helper.pairing_peer)
    def test_unsolicited_stale_or_already_connected_peer_does_not_enable_wps(self):
        for active,age,connected in [(False,0,0),(True,20,0),(True,0,1)]:
            with private_environment() as e:
                e.helper.control=e.control;e.helper.progress['authorizedStations']=connected
                e.control.provision_requests.append(('02:01:02:03:04:05',time.monotonic()-age))
                e.helper.on_heartbeat({'pairingActive':active,'pairingId':'qr-job'})
                e.control.request.assert_not_called()
    def test_control_keeps_only_valid_pbc_peer_for_targeted_authorization(self):
        control=object.__new__(p.Control);control.events=collections.deque();control.provision_requests=collections.deque()
        control.event('<3>P2P-PROV-DISC-PBC-REQ 02:01:02:03:04:05 dev_name=private-name')
        control.event('<3>P2P-PROV-DISC-PBC-REQ not-a-mac private-name')
        self.assertEqual(len(control.provision_requests),1);self.assertEqual(control.provision_requests[0][0],'02:01:02:03:04:05')
        self.assertNotIn('private-name',str(control.__dict__))

if __name__=='__main__':unittest.main(verbosity=2)
