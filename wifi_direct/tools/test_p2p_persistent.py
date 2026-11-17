#!/usr/bin/env python3
"""Offline durable-profile/reinvitation tests. No wireless commands are executed."""
import os,pathlib,tempfile,unittest
from types import SimpleNamespace
from unittest.mock import Mock,patch
from p2p_persistent import PersistentGroup,atomic_private
from wifi_capabilities import Nl80211,attributes
import struct

class PersistentTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=pathlib.Path(self.tmp.name);self.profile=PersistentGroup(self.root/'state')
        self.runtime=self.root/'runtime.conf'
        self.runtime.write_text('ctrl_interface=/run/test/ctrl\ndevice_name=vivo-connect-linux\n')
        self.peers='02:01:02:03:04:05 02:01:02:03:04:06/ff:ff:ff:ff:ff:ff'
        def response(command):
            if command=='LIST_NETWORKS':return 'network id / ssid / bssid / flags\n7\tDIRECT-test\tany\t[P2P-PERSISTENT]\n'
            if command=='GET_NETWORK 7 mode':return '3'
            if command=='GET_NETWORK 7 p2p_client_list':return self.peers
            if command.startswith('P2P_PEER '):return 'device_name=known phone'
            return 'OK'
        self.control=Mock();self.control.request.side_effect=response
        self.helper=SimpleNamespace(control=self.control,owned={'name':'p2p-vc-123456'},progress={'associatedStations':0},stage=Mock())

    def test_identity_ssid_psk_and_clients_survive_process_restart(self):
        self.profile.prepare(self.runtime)
        networks='network={\n ssid="DIRECT-old"\n psk="old-group-password"\n disabled=2\n mode=3\n p2p_client_list=02:01:02:03:04:05\n}\n'
        atomic_private(self.profile.file,('ctrl_interface=/run/old/ctrl\n'+networks).encode())
        restored=PersistentGroup(self.root/'state')
        self.assertEqual(restored.mac,self.profile.mac)
        result=restored.prepare(self.runtime).read_text()
        self.assertIn(networks,result);self.assertNotIn('/run/old',result)
        self.assertIn('update_config=1',result);self.assertIn('persistent_reconnect=1',result)
        self.assertEqual(restored.file.stat().st_mode&0o777,0o600)
        self.assertEqual(restored.command(self.control,2412),'P2P_GROUP_ADD persistent=7 freq=2412')

    def test_new_group_is_persistent_and_save_failure_is_not_ignored(self):
        self.control.request.side_effect=lambda command:'network id / ssid / bssid / flags\n' if command=='LIST_NETWORKS' else 'FAIL'
        self.assertEqual(self.profile.command(self.control,2412),'P2P_GROUP_ADD persistent freq=2412')
        with self.assertRaises(RuntimeError):self.profile.save(self.control)

    def test_only_paired_peers_are_invited_and_retries_are_throttled(self):
        self.profile.maintain(self.helper,{'reconnectRequested':True})
        commands=[c.args[0] for c in self.control.request.call_args_list]
        self.assertIn('SAVE_CONFIG',commands)
        self.assertIn('P2P_INVITE group=p2p-vc-123456 peer=02:01:02:03:04:05',commands)
        self.control.reset_mock();self.profile.maintain(self.helper,{'reconnectRequested':True})
        self.assertFalse(any(c.args[0].startswith('P2P_INVITE') for c in self.control.request.call_args_list))
        self.assertNotIn('WPS_PBC',commands)

    def test_pause_active_pairing_association_or_no_history_never_starts_invitation(self):
        for response in ({'reconnectRequested':False},{'reconnectRequested':True,'pairingActive':True}):
            self.profile.maintain(self.helper,response)
        self.helper.progress['associatedStations']=1
        self.profile.maintain(self.helper,{'reconnectRequested':True})
        self.assertFalse(any(c.args[0].startswith(('P2P_INVITE','P2P_FIND')) for c in self.control.request.call_args_list))
        self.helper.progress['associatedStations']=0;self.peers='FAIL';self.profile.next_invite=0
        self.profile.maintain(self.helper,{'reconnectRequested':True})
        self.assertFalse(any(c.args[0].startswith('P2P_INVITE') for c in self.control.request.call_args_list))

    def test_discovery_targets_exact_saved_peer_then_invites_it(self):
        original=self.control.request.side_effect
        self.control.request.side_effect=lambda command:'FAIL' if command.startswith('P2P_PEER ') else original(command)
        self.profile.maintain(self.helper,{'reconnectRequested':True})
        self.assertTrue(any(c.args[0]=='P2P_FIND 5 type=social dev_id=02:01:02:03:04:05' for c in self.control.request.call_args_list))
        self.profile.next_invite=0;self.control.request.side_effect=original;self.control.reset_mock()
        self.profile.maintain(self.helper,{'reconnectRequested':True})
        self.assertTrue(any(c.args[0]=='P2P_INVITE group=p2p-vc-123456 peer=02:01:02:03:04:05' for c in self.control.request.call_args_list))

    def test_untrusted_or_multicast_peer_entries_and_symlink_state_are_rejected(self):
        self.peers='any 01:01:02:03:04:05 00:00:00:00:00:00 02:01:02:03:04:05/00:00:00:00:00:00;bad'
        self.assertEqual(self.profile.peers(self.control),[])
        self.profile.file.symlink_to(self.runtime)
        with self.assertRaises(OSError):self.profile.prepare(self.runtime)

    def test_stable_mac_is_supplied_to_netlink_without_touching_station(self):
        nl=object.__new__(Nl80211);nl.family=42;nl.request=Mock()
        nl.create_go(0,'p2p-vc-123456',mac=self.profile.mac)
        args=nl.request.call_args.args[2];fields=dict(attributes(args))
        self.assertEqual(fields[6],self.profile.mac)
        self.assertEqual(struct.unpack('I',fields[5])[0],9)
        with self.assertRaises(ValueError):nl.create_go(0,'wlp2s0',mac=self.profile.mac)

if __name__=='__main__':unittest.main(verbosity=2)
