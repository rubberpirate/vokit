import os
import json
from pathlib import Path
import tempfile
import threading
import unittest

from gi.repository import GLib
from .backend import Backend, Settings


class BackendTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self):
        self.temp.cleanup()

    def cli(self, source):
        path = self.root / "vivolinux"
        path.write_text("#!/usr/bin/env python3\n" + source)
        path.chmod(0o700)
        return str(path)

    def wait(self, start):
        loop = GLib.MainLoop()
        received = []
        owner = threading.get_ident()
        def done(value, error):
            received.append((value, error, threading.get_ident()))
            loop.quit()
        timer = GLib.timeout_add_seconds(5, lambda: (loop.quit(), False)[1])
        start(done)
        loop.run()
        if received:
            GLib.source_remove(timer)
        self.assertEqual(len(received), 1)
        self.assertEqual(received[0][2], owner, "UI callback ran outside the GTK thread")
        return received[0][:2]

    def test_json_result_and_main_thread_delivery(self):
        backend = Backend(self.cli('print(\'{"connected":true,"file_permission":true}\')\n'))
        try:
            value, error = self.wait(lambda done: backend.call(["native-status"], done))
            self.assertIsNone(error)
            self.assertTrue(value["connected"])
        finally:
            backend.close()

    def test_oversized_output_is_stopped(self):
        backend = Backend(self.cli("import sys\nsys.stdout.write('x' * (2 * 1024 * 1024))\n"))
        try:
            value, error = self.wait(lambda done: backend.call(["files"], done))
            self.assertIsNone(value)
            self.assertIn("limit", error)
        finally:
            backend.close()

    def test_settings_private_and_never_persist_trust(self):
        path = self.root / "preferences.json"
        settings = Settings(path)
        settings.set("phone", "192.168.1.12")
        settings.set("trust", True)
        settings.set("token", "must-not-persist")
        settings.save()
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertNotIn("must-not-persist", path.read_text())
        loaded = Settings(path)
        self.assertEqual(loaded.get("phone"), "192.168.1.12")
        self.assertIsNone(loaded.get("trust"))

    def test_pair_discovers_phone_without_an_ip_argument(self):
        backend = Backend(self.cli('import sys,json\nprint(json.dumps({"stage":"arguments","has_phone":"--phone" in sys.argv,"trust":"--trust-scanned-phone" in sys.argv}),flush=True)\n'))
        events = []
        try:
            value, error = self.wait(lambda done: backend.pair("", True, str(self.root / "Receive files"), False, False, events.append, lambda code: done({"exit":code}, None)))
            self.assertIsNone(error)
            self.assertEqual(value["exit"], 0)
            self.assertFalse(events[0]["has_phone"])
            self.assertTrue(events[0]["trust"])
        finally:
            backend.close()

    def test_pair_events_and_receive_folder_survive_spaces(self):
        backend = Backend(self.cli('import sys,json\nprint(json.dumps({"stage":"control_connected","count":len(sys.argv)}),flush=True)\n'))
        events = []
        value, error = self.wait(lambda done: backend.pair("192.168.1.12", True, str(self.root / "Receive files"), False, False, events.append, lambda code: done({"exit":code},None)))
        self.assertIsNone(error)
        self.assertEqual(value["exit"], 0)
        self.assertEqual(events[0]["stage"], "control_connected")
        backend.close()


class TransferBoundaryTests(unittest.TestCase):
    def test_batch_download_names_preserve_existing_files(self):
        from .app import available_download_path, valid_download_receipt
        with tempfile.TemporaryDirectory() as directory:
            original = Path(directory) / 'photo.jpg'
            original.write_bytes(b'keep')
            (Path(directory) / 'photo (2).jpg').write_bytes(b'keep too')
            self.assertEqual(Path(available_download_path(directory, 'photo.jpg')).name, 'photo (3).jpg')
            for name in ('../escape', '/escape', '.', 'bad\\name'):
                with self.assertRaises(ValueError): available_download_path(directory, name)
            self.assertEqual(original.read_bytes(), b'keep')
        self.assertTrue(valid_download_receipt({'bytes':12,'sha256':'a'*64}, 12))
        self.assertFalse(valid_download_receipt({'bytes':13,'sha256':'a'*64}, 12))
        self.assertFalse(valid_download_receipt({'bytes':12,'sha256':'x'*64}, 12))

    def test_phone_notifications_escape_markup_and_reject_oversized_text(self):
        from .helper import Helper
        class Bus:
            def __init__(self): self.parameters = []
            def call(self, *args):
                self.parameters.append(args[4].unpack())
                args[-1](self, None)
            def call_finish(self, result): pass
        helper = Helper.__new__(Helper)
        helper.stopping, helper.bus = False, Bus()
        acknowledgements = []
        helper.emit = acknowledgements.append
        helper.request(json.dumps({'op':'phone_notification','id':'1','app':'Fixture app',
                                   'title':'Fixture title','body':'<b>Fixture & text</b>'}))
        self.assertEqual(helper.bus.parameters[0][4], '&lt;b&gt;Fixture &amp; text&lt;/b&gt;')
        self.assertTrue(acknowledgements[-1]['ok'])
        helper.request(json.dumps({'op':'phone_notification','id':'2','app':'Fixture app',
                                   'title':'x'*1025,'body':'body'}))
        self.assertFalse(acknowledgements[-1]['ok'])
        self.assertEqual(len(helper.bus.parameters), 1)


class BatchDispatchTests(unittest.TestCase):
    @staticmethod
    def spin(condition):
        import time
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            while GLib.MainContext.default().pending() and time.monotonic() < deadline:
                GLib.MainContext.default().iteration(False)
            if condition(): return True
            time.sleep(.003)
        return False

    def test_connect_opens_pairing_without_ip_or_trust_toggle(self):
        from .app import Adw, OfficeWindow, _TestBackend, _TestSettings
        class Backend(_TestBackend):
            def pair(self, *args): self.pair_arguments = args
        backend = Backend()
        app = Adw.Application(application_id="org.vokit.QRConnectTest")
        settings = _TestSettings()
        settings.set("phone", "192.168.1.12")  # Legacy test IP must not constrain discovery.
        app.backend, app.settings = backend, settings
        app.register(None)
        window = OfficeWindow(app)
        try:
            self.assertEqual(window.phone_entry.get_text(), "")
            window._connection_action()
            self.assertTrue(window.owned)
            self.assertEqual(backend.pair_arguments[0], "")
            self.assertTrue(backend.pair_arguments[1])
            self.assertNotIn("trust", settings.values)
        finally:
            window.shutdown()
            window.destroy()
            backend.close()

    def test_send_queue_and_batch_download_use_all_selected_files(self):
        from unittest.mock import patch
        from .app import Adw, OfficeWindow, _TestBackend, _TestSettings
        from gi.repository import Gio
        class Backend(_TestBackend):
            def call(self, args, done):
                if args[0] in ('upload', 'download'):
                    self.calls.append(args)
                    GLib.idle_add(lambda: done({'bytes':12,'sha256':'a'*64}, None) or False)
                else:
                    super().call(args, done)
        backend = Backend()
        app = Adw.Application(application_id="org.vokit.BatchDispatchTest")
        app.backend, app.settings = backend, _TestSettings()
        app.register(None)
        window = OfficeWindow(app)
        try:
            self.assertTrue(self.spin(lambda: window.connected and window.store.get_n_items() >= 3))
            with tempfile.TemporaryDirectory() as directory:
                files = []
                for name in ('with spaces.txt', 'literal$(name).txt'):
                    path = Path(directory)/name
                    path.write_text('fixture text')
                    files.append(Gio.File.new_for_path(str(path)))
                window.send_files(files)
                self.assertTrue(self.spin(lambda: not window.transfer_busy and not window.pending_sends))
                uploads = [args for args in backend.calls if args[0] == 'upload']
                self.assertEqual([args[args.index('--local')+1] for args in uploads], [file.get_path() for file in files])
                self.assertTrue(all(args[-1] == '/storage/emulated/0/Download' for args in uploads))
                items = [window.store.get_item(0), window.store.get_item(1)]
                for item in items: item.size = 12
                class Dialog:
                    def __init__(self, **kwargs): pass
                    def select_folder(self, parent, cancel, callback): callback(self, None)
                    def select_folder_finish(self, result): return Gio.File.new_for_path(directory)
                with patch('vivo_desktop.app.Gtk.FileDialog', Dialog):
                    window._download_many(items)
                self.assertTrue(self.spin(lambda: not window.transfer_busy))
                downloads = [args for args in backend.calls if args[0] == 'download']
                self.assertEqual(len(downloads), 2)
                for args, item in zip(downloads, items):
                    self.assertEqual(args[args.index('--remote')+1], item.path)
                    self.assertEqual(args[args.index('--page')+1], str(item.page))
                    self.assertEqual(args[args.index('--category')+1], item.category)
                self.assertEqual(window.receipt.get_text(), 'Saved 2 of 2 files')
                self.assertLessEqual(sum(args[0] == 'native-status' for args in backend.calls), 2)
        finally:
            window.shutdown()
            window.destroy()
            backend.close()


if __name__ == "__main__":
    unittest.main()
