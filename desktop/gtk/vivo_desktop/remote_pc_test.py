import unittest
from types import SimpleNamespace
from unittest.mock import Mock

import gi
gi.require_version('Gtk', '4.0')
gi.require_version('Adw', '1')
from gi.repository import Adw, Gtk

from .remote_pc import RemotePCWindow


class RemotePCWindowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        Gtk.init()
        Adw.init()

    def test_credentials_update_and_close_stops_access(self):
        preferences = Mock()
        preferences.get.return_value = 'IN'
        window = RemotePCWindow(None, SimpleNamespace(cli_path='/unused'), preferences)
        try:
            self.assertFalse(window.rotate.get_sensitive())
            window._event({'event': 'credentials', 'device_code': '1234567890', 'temporary_password': '654321'})
            self.assertEqual(window.code.get_text(), '1 234 567 890')
            self.assertEqual(window.password.get_text(), '654321')
            self.assertTrue(window.copy.get_sensitive())
            window._event({'event': 'connected'})
            self.assertFalse(window.rotate.get_sensitive())
            session = Mock()
            window.session = session
            window._close()
            session.stop.assert_called_once()
            self.assertEqual(window.password.get_text(), '')
            self.assertFalse(window.copy.get_sensitive())
            self.assertIsNone(window.session)
            window._event({'event': 'credentials', 'device_code': 'bad\nID', 'temporary_password': 'value'})
            self.assertEqual(window.code.get_text(), '—')
        finally:
            window.destroy()


if __name__ == '__main__':
    unittest.main()
