import json
from pathlib import Path
import tempfile
import threading
import unittest

from gi.repository import GLib
from .backend import Backend


class ReconnectionTests(unittest.TestCase):
    def wait_exit(self, backend, on_event, resume=False):
        loop = GLib.MainLoop()
        finished = []
        def exited(code):
            finished.append(code)
            loop.quit()
        timer = GLib.timeout_add_seconds(5, lambda: (loop.quit(), False)[1])
        backend.pair('', True, '/tmp', False, False, on_event, exited, resume=resume)
        loop.run()
        if finished: GLib.source_remove(timer)
        self.assertTrue(finished, 'reconnect worker did not stop')

    def fixture(self, folder, mode):
        script = Path(folder)/'cli'
        script.write_text('''#!/usr/bin/env python3
import json, pathlib, sys, time
root=pathlib.Path(__file__).parent
record=root/'remembered.json'
if '--forget-remembered' in sys.argv:
    record.unlink(missing_ok=True)
    raise SystemExit(0)
with (root/'attempts').open('a') as log: log.write(json.dumps(sys.argv[1:])+'\\n')
if '--resume' in sys.argv and MODE=='reject':
    print(json.dumps({'stage':'reconnect_failed','code':'pairing_required'}),flush=True)
    raise SystemExit(1)
if '--resume' in sys.argv and MODE in ('login', 'bluetooth'):
    print(json.dumps({'stage':'reconnect_failed','code':MODE+'_required'}),flush=True)
    raise SystemExit(1)
record.write_text(json.dumps({'version':1,'region':'overseas','wifi_direct':False}))
record.chmod(0o600)
print(json.dumps({'stage':'control_connected'}),flush=True)
if '--resume' not in sys.argv and MODE=='retry': raise SystemExit(1)
time.sleep(60)
'''.replace("MODE", repr(mode)))
        script.chmod(0o700)
        backend = Backend(str(script))
        backend.state_dir = lambda: Path(folder)
        backend.retry_delay = lambda attempt: .01
        return backend

    def test_transport_failure_retries_once_and_manual_disconnect_forgets(self):
        with tempfile.TemporaryDirectory() as folder:
            backend = self.fixture(folder, 'retry')
            events, threads = [], []
            owner = threading.get_ident()
            def event(value):
                events.append(value['stage'])
                threads.append(threading.get_ident())
                if events.count('control_connected') == 2:
                    backend.disconnect()
            try:
                self.wait_exit(backend, event)
                attempts = [json.loads(line) for line in (Path(folder)/'attempts').read_text().splitlines()]
                self.assertEqual(len(attempts), 2)
                self.assertNotIn('--resume', attempts[0])
                self.assertIn('--resume', attempts[1])
                self.assertIn('reconnect_wait', events)
                self.assertNotIn('qr_ready', events)
                self.assertTrue(all(thread == owner for thread in threads))
                self.assertIsNone(backend.remembered())
            finally:
                backend.close()

    def test_authorization_rejection_stops_retries_and_forgets(self):
        with tempfile.TemporaryDirectory() as folder:
            backend = self.fixture(folder, 'reject')
            events = []
            try:
                self.wait_exit(backend, lambda value: events.append(value['stage']), resume=True)
                self.assertIn('reconnect_pairing_required', events)
                self.assertNotIn('reconnect_wait', events)
                self.assertIsNone(backend.remembered())
                self.assertEqual(len((Path(folder)/'attempts').read_text().splitlines()), 1)
            finally:
                backend.close()

    def test_quit_preserves_authorization_for_the_next_start(self):
        with tempfile.TemporaryDirectory() as folder:
            backend = self.fixture(folder, 'keep')
            try:
                self.wait_exit(backend, lambda value: backend.close() if value['stage']=='control_connected' else None)
                self.assertIsNotNone(backend.remembered())
            finally:
                backend.close()

    def test_missing_login_or_bluetooth_pauses_without_forgetting_phone(self):
        for mode in ('login', 'bluetooth'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as folder:
                backend = self.fixture(folder, mode)
                record = Path(folder)/'remembered.json'
                record.write_text(json.dumps({'version':1, 'region':'overseas', 'wifi_direct':True}))
                record.chmod(0o600)
                events = []
                try:
                    self.wait_exit(backend, lambda value: events.append(value['stage']), resume=True)
                    self.assertIn('reconnect_'+mode+'_required', events)
                    self.assertNotIn('reconnect_wait', events)
                    self.assertIsNotNone(backend.remembered())
                finally:
                    backend.close()

    def test_backoff_is_bounded(self):
        self.assertEqual([Backend.retry_delay(i) for i in range(1, 8)], [5,10,20,40,60,60,60])

    def test_reconnect_is_nonmodal_and_qr_can_close_without_disconnect(self):
        from .app import Adw, OfficeWindow, _TestBackend, _TestSettings
        import base64
        class Fake(_TestBackend):
            def __init__(self):
                super().__init__()
                self.disconnects = 0
            def pair(self, *args, **kwargs): self.options = kwargs
            def disconnect(self): self.disconnects += 1
        backend = Fake()
        app = Adw.Application(application_id='org.vokit.ReconnectPresentation')
        app.backend, app.settings = backend, _TestSettings()
        app.register(None)
        window = OfficeWindow(app)
        try:
            window._start_pair(wifi_direct=True, resume=True)
            self.assertTrue(backend.options['resume'])
            self.assertIsNone(window.pair_progress_dialog)
            self.assertIn('Reconnecting', window.connection_status.get_text())
            window._pair_event({'stage': 'reconnect_wait', 'count': 5})
            self.assertIsNone(window.pair_progress_dialog)
            self.assertIn('Reconnecting', window.connection_status.get_text())
            window._show_pair_progress(True)
            window._pair_event({'stage': 'control_connected'})
            self.assertIsNone(window.pair_progress_dialog)
            self.assertEqual(backend.disconnects, 0)
            with tempfile.TemporaryDirectory() as folder:
                path = Path(folder)/'qr.png'
                path.write_bytes(base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aFukAAAAASUVORK5CYII='))
                window._pair_event({'stage':'qr_ready', 'qr_path':str(path)})
                dialog = window.qr_dialog
                self.assertIsNotNone(dialog)
                button = dialog.get_child().get_last_child()
                self.assertEqual(button.get_label(), 'Close')
                button.emit('clicked')
                self.assertIsNone(window.qr_dialog)
                self.assertEqual(backend.disconnects, 0)
        finally:
            window.owned = False
            window.shutdown()
            window.destroy()
            backend.close()

    def test_account_buttons_login_and_logout_without_exposing_credentials(self):
        from .app import Adw, OfficeWindow, _TestBackend, _TestSettings
        from types import SimpleNamespace
        import time
        class Fake(_TestBackend):
            logged = False
            def remembered(self): return None
            def set_auto_reconnect(self, enabled): self.auto_enabled = enabled
            def call(self, args, done):
                self.calls.append(args)
                def complete():
                    if args[0]=='account-login': self.logged=True
                    if args[0]=='account-logout': self.logged=False
                    done({'logged_in':self.logged} if args[0].startswith('account-') else None,
                         None if args[0].startswith('account-') else 'offline')
                    return False
                GLib.idle_add(complete)
                return SimpleNamespace(terminate=lambda: None)
        backend = Fake()
        app = Adw.Application(application_id='org.vokit.AccountButtons')
        app.backend, app.settings = backend, _TestSettings()
        app.register(None)
        window = OfficeWindow(app)
        def spin():
            deadline=time.monotonic()+.1
            while time.monotonic()<deadline:
                while GLib.MainContext.default().pending(): GLib.MainContext.default().iteration(False)
                time.sleep(.002)
        try:
            spin()
            self.assertFalse(window.account_logged_in)
            window._account_action()
            self.assertEqual(window.account_button.get_label(), 'Cancel sign-in')
            spin()
            self.assertTrue(window.account_logged_in)
            self.assertEqual(window.account_button.get_label(), 'Sign out')
            window._account_action()
            spin()
            self.assertFalse(window.account_logged_in)
            self.assertEqual(window.account_button.get_label(), 'Sign in')
            self.assertFalse(backend.auto_enabled)
            self.assertIn('account-login', [args[0] for args in backend.calls])
            self.assertIn('account-logout', [args[0] for args in backend.calls])
            self.assertNotIn('token', app.settings.values)
        finally:
            window.shutdown()
            window.destroy()
            backend.close()

    def test_startup_resumes_once_and_respects_disable_and_external_sessions(self):
        from .app import Adw, OfficeWindow, _TestBackend, _TestSettings
        import time
        for index, (enabled, external, should_resume) in enumerate(((True, False, True), (False, False, False), (True, True, False))):
            with self.subTest(enabled=enabled, external=external):
                class Fake(_TestBackend):
                    def __init__(self):
                        super().__init__()
                        self.pairs = []
                    def remembered(self): return {'region':'china', 'wifi_direct': True}
                    def pair(self, *args, **options): self.pairs.append(options)
                    def call(self, args, done):
                        GLib.idle_add(lambda: done({'connected':False} if external else None, None if external else 'offline') or False)
                backend = Fake()
                app = Adw.Application(application_id='org.vokit.ResumeTest.Case' + str(index))
                app.backend, app.settings = backend, _TestSettings()
                app.settings.set('region', 'china')
                app.settings.set('auto_reconnect', enabled)
                app.register(None)
                window = OfficeWindow(app)
                def spin():
                    deadline = time.monotonic()+.2
                    while time.monotonic()<deadline:
                        while GLib.MainContext.default().pending(): GLib.MainContext.default().iteration(False)
                        time.sleep(.002)
                try:
                    spin()
                    self.assertEqual(len(backend.pairs), int(should_resume))
                    if should_resume:
                        self.assertTrue(backend.pairs[0]['resume'])
                        self.assertTrue(backend.pairs[0]['wifi_direct'])
                    window._poll_status()
                    spin()
                    self.assertEqual(len(backend.pairs), int(should_resume), 'startup queued duplicate attempts')
                finally:
                    window._close_pair_progress()
                    window.owned = False
                    window.shutdown()
                    window.destroy()
                    backend.close()
