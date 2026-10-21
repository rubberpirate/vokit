import os
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

    def test_pair_events_and_receive_folder_survive_spaces(self):
        backend = Backend(self.cli('import sys,json\nprint(json.dumps({"stage":"control_connected","count":len(sys.argv)}),flush=True)\n'))
        events = []
        value, error = self.wait(lambda done: backend.pair("192.168.1.12", True, str(self.root / "Receive files"), False, False, events.append, lambda code: done({"exit":code},None)))
        self.assertIsNone(error)
        self.assertEqual(value["exit"], 0)
        self.assertEqual(events[0]["stage"], "control_connected")
        backend.close()


if __name__ == "__main__":
    unittest.main()
