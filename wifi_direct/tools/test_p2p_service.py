#!/usr/bin/env python3
"""Offline service and installer checks; never creates interfaces or starts systemd."""
import json,os,pathlib,tempfile,unittest
from types import SimpleNamespace
from unittest.mock import patch
import p2p_service as service
import install_p2p_service as installer

class ServiceTests(unittest.TestCase):
    def setUp(self):
        self.temporary=tempfile.TemporaryDirectory();self.addCleanup(self.temporary.cleanup)
        self.root=pathlib.Path(self.temporary.name)
        self.config={'uid':os.getuid(),'state_dir':str(self.root),'interface':'wlp2s0','port':8765}
        self.file=self.root/'config.json'
        self.file.write_text(json.dumps({'apiToken':'test-token'}));self.file.chmod(0o600)

    def test_user_config_rejects_symlink_wrong_owner_permissions_and_oversize(self):
        self.assertEqual(service.user_config(self.config)['apiToken'],'test-token')
        self.file.chmod(0o644)
        with self.assertRaises(RuntimeError):service.user_config(self.config)
        self.file.chmod(0o600)
        with self.assertRaises(RuntimeError):service.user_config({**self.config,'uid':os.getuid()+1})
        self.file.write_text('x'*65537)
        with self.assertRaises(RuntimeError):service.user_config(self.config)
        self.file.unlink();self.file.symlink_to(self.root/'target')
        with self.assertRaises(OSError):service.user_config(self.config)

    def test_retry_diagnostics_are_actionable_without_exposing_error_text(self):
        diagnostic=service.failure_diagnostics(RuntimeError('Operation not permitted secret-password secret-token'),'group-configure-address')
        self.assertEqual(diagnostic['failedAt'],'group-configure-address')
        self.assertIn('operation-not-permitted',diagnostic['reasons'])
        self.assertNotIn('secret',json.dumps(diagnostic))

    def test_service_configuration_binds_user_interface_and_port(self):
        valid={**self.config,'uid':1000}
        with patch.object(service,'read_json_file',return_value=valid):
            self.assertEqual(service.load_config()['interface'],'wlp2s0')
        for values in ({'uid':0},{'uid':True},{'port':0},{'interface':'../wlan0'},{'state_dir':'relative'}):
            with patch.object(service,'read_json_file',return_value={**valid,**values}):
                with self.assertRaises(ValueError):service.load_config()

    def test_backend_restart_reregisters_same_group_without_cleanup(self):
        helper=service.ServiceHelper(self.config)
        calls=[]
        def api(_self,route,body=None,method=None):
            calls.append((route,body))
            if route=='/api/p2p/heartbeat':raise RuntimeError('backend restarted')
            return {'ready':True,'pairingActive':False}
        with patch.object(service.Helper,'api',api),patch.object(helper,'cleanup') as cleanup:
            helper.api('/api/p2p/register',{'id':'same-group','ssid':'DIRECT-test','passphrase':'secret-password'})
            result=helper.api('/api/p2p/heartbeat',{'id':'same-group'})
            self.assertTrue(result['ready']);cleanup.assert_not_called()
        self.assertEqual(calls[-1][1]['id'],'same-group')
        self.assertEqual(calls[-1][1]['management'],'system-service')
        self.assertEqual(helper.args.runtime_dir,'/run/vivo-connect-p2p')
        self.assertIsNone(helper.args.owner_pid)

    def test_backend_outage_closes_pairing_window_and_retains_group(self):
        helper=service.ServiceHelper(self.config)
        helper.registration={'id':'same-group'}
        with patch.object(service.Helper,'api',side_effect=RuntimeError('offline')),patch.object(helper,'cleanup') as cleanup:
            self.assertEqual(helper.api('/api/p2p/heartbeat',{'id':'same-group'}),{'pairingActive':False})
            cleanup.assert_not_called()

    def test_staged_install_is_snapshot_and_uninstall_preserves_user_data(self):
        # Build record and binary are validated against the existing local build.
        stage=self.root/'stage'
        with patch.object(installer.subprocess,'run') as run:
            installer.install(installer.ROOT,stage,self.config)
            run.assert_not_called()
            prefix=installer.target(stage,installer.PREFIX)
            self.assertTrue((prefix/'tools/p2p_service.py').is_file())
            self.assertFalse((prefix/'tools/p2p_service.py').is_symlink())
            self.assertEqual((prefix/'tools/p2p_service.py').stat().st_mode&0o777,0o644)
            self.assertEqual((prefix/'build/private-p2p/bin/wpa_supplicant').stat().st_mode&0o777,0o755)
            unit=installer.target(stage,installer.UNIT).read_text()
            self.assertIn('/usr/bin/python3 -I /usr/local/lib/vivo-connect-p2p/tools/p2p_service.py',unit)
            self.assertNotIn(str(installer.ROOT),unit)
            self.assertIn('NoNewPrivileges=yes',unit)
            self.assertIn('AmbientCapabilities=CAP_SETUID',unit)
            self.assertIn('CAP_SETUID',next(line for line in unit.splitlines() if line.startswith('CapabilityBoundingSet=')))
            self.assertIn('CAP_KILL',next(line for line in unit.splitlines() if line.startswith('CapabilityBoundingSet=')))
            self.assertEqual(json.loads(installer.target(stage,installer.CONFIG).read_text()),self.config)
            installer.uninstall(stage);run.assert_not_called()
            self.assertFalse(prefix.exists());self.assertTrue(self.file.exists())

    def test_installation_refuses_symlink_destinations_and_mismatched_binary(self):
        link=self.root/'link';link.symlink_to(self.root/'actual')
        with self.assertRaises(RuntimeError):installer.write_file(link/'file',b'test',0o644,False)
        with patch.object(installer,'digest',return_value='tampered'):
            with self.assertRaises(RuntimeError):installer.verify_source(installer.ROOT)

    def test_package_refresh_updates_existing_snapshot_without_enabling_a_disabled_service(self):
        for active in (True,False):
            with patch.object(installer,'CONFIG',self.file),patch.object(service,'load_config',return_value=self.config),patch.object(installer,'install') as install,patch.object(installer.subprocess,'run',return_value=SimpleNamespace(returncode=0 if active else 3)) as run:
                self.assertTrue(installer.refresh())
                install.assert_called_once_with(installer.ROOT,pathlib.Path('/'),self.config,enable=False)
                self.assertEqual(any(call.args[0][:2]==['systemctl','start'] for call in run.call_args_list),active)

    def test_service_recovers_only_its_inactive_persistent_mac_when_runtime_record_was_removed(self):
        for frequency,address,recover in [(0,'02:11:22:33:44:55',True),(2412,'02:11:22:33:44:55',False),(0,'02:11:22:33:44:56',False)]:
            helper=service.ServiceHelper(self.config);helper.original={'station':'unchanged'}
            helper.persistent=SimpleNamespace(mac=bytes.fromhex('021122334455'))
            info={'name':helper.interface_name(),'index':7,'phy':0,'type':9,'frequency':frequency}
            with patch.object(service.PrivateHelper,'recover_private_orphans'),patch.object(service,'kernel_interfaces',return_value=[info]),patch.object(service.pathlib.Path,'read_text',return_value=address),patch.object(service,'station_snapshot',return_value=helper.original),patch.object(service,'Nl80211') as client:
                helper.recover_private_orphans({'phy':0})
                self.assertEqual(client.return_value.delete_interface.called,recover)
                if recover:client.return_value.delete_interface.assert_called_once_with(7)

    def test_service_stop_cleans_helper_and_releases_exclusive_lock(self):
        handlers={};cleaned=[]
        fd=os.open(self.root/'lock',os.O_CREAT|os.O_RDWR,0o600)
        helper=SimpleNamespace(stop=False,lock=fd,api=lambda route:{'ready':False},cleanup=lambda:cleaned.append(True))
        helper.start=lambda:handlers[service.signal.SIGTERM](service.signal.SIGTERM,None)
        with patch.object(service.os,'geteuid',return_value=0),patch.object(service,'load_config',return_value=self.config), \
             patch.object(service,'ServiceHelper',return_value=helper),patch.object(service.signal,'signal',side_effect=lambda name,callback:handlers.update({name:callback})):
            service.main()
        self.assertTrue(helper.stop);self.assertEqual(cleaned,[True])
        with self.assertRaises(OSError):os.fstat(fd)

if __name__=='__main__':unittest.main(verbosity=2)
