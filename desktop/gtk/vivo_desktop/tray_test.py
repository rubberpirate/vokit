"""Exercise the exported tray protocol on a disposable D-Bus session."""
import os
import time
import unittest

from gi.repository import Gio, GLib
from .tray import StatusTray, ITEM_INTERFACE, MENU_INTERFACE, ITEM_PATH, MENU_PATH, WATCHER


def new_connection():
    return Gio.DBusConnection.new_for_address_sync(os.environ['DBUS_SESSION_BUS_ADDRESS'],
        Gio.DBusConnectionFlags.AUTHENTICATION_CLIENT | Gio.DBusConnectionFlags.MESSAGE_BUS_CONNECTION, None, None)


class TrayTests(unittest.TestCase):
    def setUp(self):
        self.provider, self.client = new_connection(), new_connection()
        self.actions = []
        self.tray = StatusTray(self.provider, lambda action, token: self.actions.append((action, token)))
        self.name = self.provider.get_unique_name()
        self.addCleanup(self.client.close_sync, None)
        self.addCleanup(self.provider.close_sync, None)
        self.addCleanup(self.tray.close)

    @staticmethod
    def spin(condition, timeout=3):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            while GLib.MainContext.default().pending() and time.monotonic() < deadline:
                GLib.MainContext.default().iteration(False)
            if condition():
                return True
            time.sleep(.003)
        return False

    def call(self, path, interface, method, parameters):
        loop, values, errors = GLib.MainLoop(), [], []
        def completed(connection, result):
            try:
                values.append(connection.call_finish(result).unpack())
            except GLib.Error as error:
                errors.append(error)
            loop.quit()
        self.client.call(self.name, path, interface, method, parameters, None,
                         Gio.DBusCallFlags.NONE, 2000, None, completed)
        loop.run()
        if errors:
            raise errors[0]
        return values[0]

    def properties(self):
        return self.call(ITEM_PATH, 'org.freedesktop.DBus.Properties', 'GetAll', GLib.Variant('(s)', (ITEM_INTERFACE,)))[0]

    def menu(self):
        result = self.call(MENU_PATH, MENU_INTERFACE, 'GetLayout', GLib.Variant('(iias)', (0, -1, [])))
        return {item[0]: item[1] for item in result[1][2]}

    def event(self, item):
        self.call(MENU_PATH, MENU_INTERFACE, 'Event', GLib.Variant('(isvu)', (item, 'clicked', GLib.Variant('i', 0), 0)))

    def test_three_colors_are_visible_and_tooltips_describe_connection(self):
        for connected, connecting, expected in ((False, False, 'Disconnected'), (False, True, 'Connecting'), (True, False, 'Connected')):
            self.tray.update(connected, connecting, 'Fixture phone', connected or connecting, False)
            properties = self.properties()
            self.assertEqual(properties['Id'], 'vokit')
            self.assertEqual(properties['Status'], 'Active', 'Disconnected icon must remain visible')
            self.assertIn(expected, properties['ToolTip'][3])
            self.assertEqual(properties['Menu'], MENU_PATH)
            self.assertEqual(properties['IconName'], '', 'Theme icon must not replace status colors')
            for width, height, pixels in properties['IconPixmap']:
                self.assertEqual(width, height)
                self.assertEqual(len(pixels), width * height * 4)
                alpha, red, green, blue = pixels[(height//2*width+width//2)*4:][:4]
                self.assertEqual(alpha, 255)
                if connected:
                    self.assertGreater(green, max(red, blue))
                elif connecting:
                    self.assertGreater(red, green)
                    self.assertGreater(green, blue)
                else:
                    self.assertGreater(red, max(green, blue))
        revision = self.tray.revision
        self.tray.update(True, False, 'Fixture phone', True, False)
        self.assertEqual(self.tray.revision, revision, 'Unchanged polls must not redraw the tray')

    def test_menu_enablement_actions_tokens_and_single_item_layout(self):
        menu = self.menu()
        self.assertFalse(menu[4]['enabled'])
        self.assertFalse(menu[5]['enabled'])
        self.event(4)
        self.assertFalse(self.spin(lambda: bool(self.actions), .03))
        self.tray.update(True, False, 'Fixture phone', True, True)
        menu = self.menu()
        self.assertTrue(menu[4]['enabled'])
        self.assertTrue(menu[5]['enabled'])
        self.assertEqual(menu[9]['label'], 'Disconnect phone')
        self.call(ITEM_PATH, ITEM_INTERFACE, 'ProvideXdgActivationToken', GLib.Variant('(s)', ('fixture-activation',)))
        self.event(4)
        self.assertTrue(self.spin(lambda: len(self.actions) == 1))
        self.assertEqual(self.actions[0], ('mirror', 'fixture-activation'))
        self.call(ITEM_PATH, ITEM_INTERFACE, 'Activate', GLib.Variant('(ii)', (0, 0)))
        self.assertTrue(self.spin(lambda: len(self.actions) == 2))
        self.assertEqual(self.actions[1], ('show', ''))
        self.tray.update(True, False, 'Fixture phone', False, False, True)
        self.assertFalse(self.menu()[9]['enabled'])
        self.event(9)
        self.assertFalse(self.spin(lambda: len(self.actions) > 2, .03))
        root = self.call(MENU_PATH, MENU_INTERFACE, 'GetLayout', GLib.Variant('(iias)', (0, 0, ['children-display'])))
        self.assertEqual(root[1][2], [])
        self.assertEqual(list(root[1][1]), ['children-display'])
        with self.assertRaises(GLib.Error):
            self.call(MENU_PATH, MENU_INTERFACE, 'GetLayout', GLib.Variant('(iias)', (999, -1, [])))

    def test_watcher_registration_recovers_after_panel_restart_and_quit_removes_item(self):
        registrations = []
        xml = '''<node><interface name="org.kde.StatusNotifierWatcher"><method name="RegisterStatusNotifierItem"><arg type="s" direction="in"/></method></interface></node>'''
        def start_watcher():
            bus = new_connection()
            def register(connection, sender, path, interface, method, parameters, invocation):
                registrations.append((sender, parameters.unpack()[0]))
                invocation.return_value(None)
            object_id = bus.register_object('/StatusNotifierWatcher', Gio.DBusNodeInfo.new_for_xml(xml).interfaces[0], register, None, None)
            bus.call_sync('org.freedesktop.DBus', '/org/freedesktop/DBus', 'org.freedesktop.DBus', 'RequestName',
                          GLib.Variant('(su)', (WATCHER, 0)), None, Gio.DBusCallFlags.NONE, 2000, None)
            return bus, object_id
        watcher, object_id = start_watcher()
        try:
            self.assertTrue(self.spin(lambda: self.tray.registered))
            self.assertEqual(registrations[-1], (self.name, ITEM_PATH))
            watcher.unregister_object(object_id)
            watcher.close_sync(None)
            self.assertTrue(self.spin(lambda: not self.tray.registered))
            watcher, object_id = start_watcher()
            self.assertTrue(self.spin(lambda: self.tray.registered and len(registrations) == 2))
            self.tray.close()
            self.tray.close()
            with self.assertRaises(GLib.Error):
                self.properties()
        finally:
            watcher.unregister_object(object_id)
            watcher.close_sync(None)


if __name__ == '__main__':
    unittest.main()
