"""StatusNotifierItem and DBusMenu export on the existing session bus."""
import functools
import math
import sys

import cairo
from gi.repository import Gio, GLib

ITEM_INTERFACE = 'org.kde.StatusNotifierItem'
MENU_INTERFACE = 'com.canonical.dbusmenu'
WATCHER = 'org.kde.StatusNotifierWatcher'
ITEM_PATH = '/StatusNotifierItem'
MENU_PATH = '/StatusNotifierMenu'

ITEM_XML = '''<node><interface name="org.kde.StatusNotifierItem">
<property name="Category" type="s" access="read"/>
<property name="Id" type="s" access="read"/>
<property name="Title" type="s" access="read"/>
<property name="Status" type="s" access="read"/>
<property name="WindowId" type="i" access="read"/>
<property name="IconThemePath" type="s" access="read"/>
<property name="IconName" type="s" access="read"/>
<property name="IconPixmap" type="a(iiay)" access="read"/>
<property name="OverlayIconName" type="s" access="read"/>
<property name="OverlayIconPixmap" type="a(iiay)" access="read"/>
<property name="AttentionIconName" type="s" access="read"/>
<property name="AttentionIconPixmap" type="a(iiay)" access="read"/>
<property name="AttentionMovieName" type="s" access="read"/>
<property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
<property name="ItemIsMenu" type="b" access="read"/>
<property name="Menu" type="o" access="read"/>
<method name="Activate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
<method name="SecondaryActivate"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
<method name="ContextMenu"><arg type="i" direction="in"/><arg type="i" direction="in"/></method>
<method name="Scroll"><arg type="i" direction="in"/><arg type="s" direction="in"/></method>
<method name="ProvideXdgActivationToken"><arg type="s" direction="in"/></method>
<signal name="NewTitle"/><signal name="NewIcon"/><signal name="NewToolTip"/>
<signal name="NewStatus"><arg type="s"/></signal>
</interface></node>'''

MENU_XML = '''<node><interface name="com.canonical.dbusmenu">
<property name="Version" type="u" access="read"/>
<property name="TextDirection" type="s" access="read"/>
<property name="Status" type="s" access="read"/>
<property name="IconThemePath" type="as" access="read"/>
<method name="GetLayout"><arg type="i" direction="in"/><arg type="i" direction="in"/><arg type="as" direction="in"/><arg type="u" direction="out"/><arg type="(ia{sv}av)" direction="out"/></method>
<method name="GetGroupProperties"><arg type="ai" direction="in"/><arg type="as" direction="in"/><arg type="a(ia{sv})" direction="out"/></method>
<method name="GetProperty"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
<method name="Event"><arg type="i" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="in"/><arg type="u" direction="in"/></method>
<method name="EventGroup"><arg type="a(isvu)" direction="in"/><arg type="ai" direction="out"/></method>
<method name="AboutToShow"><arg type="i" direction="in"/><arg type="b" direction="out"/></method>
<method name="AboutToShowGroup"><arg type="ai" direction="in"/><arg type="ai" direction="out"/><arg type="ai" direction="out"/></method>
<signal name="LayoutUpdated"><arg type="u"/><arg type="i"/></signal>
<signal name="ItemsPropertiesUpdated"><arg type="a(ia{sv})"/><arg type="a(ias)"/></signal>
</interface></node>'''

COLORS = {'connected': (0.15, 0.78, 0.49), 'connecting': (0.98, 0.68, 0.19), 'disconnected': (0.93, 0.29, 0.32)}


@functools.lru_cache(maxsize=3)
def icon_pixmaps(state):
    """SNI pixels use network-order ARGB, independently of Cairo's native order."""
    result = []
    for size in (24, 32, 64):
        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, size, size)
        ctx = cairo.Context(surface)
        ctx.scale(size / 32, size / 32)
        ctx.set_source_rgb(*COLORS[state])
        ctx.arc(16, 16, 15, 0, math.tau)
        ctx.fill()
        ctx.set_source_rgb(1, 1, 1)
        ctx.set_line_width(1.8)
        ctx.set_line_join(cairo.LINE_JOIN_ROUND)
        ctx.rectangle(10.5, 6.5, 11, 19)
        ctx.stroke()
        ctx.move_to(14, 22.5)
        ctx.line_to(18, 22.5)
        ctx.stroke()
        surface.flush()
        data = bytes(surface.get_data())
        pixels = bytearray()
        for offset in range(0, len(data), 4):
            if sys.byteorder == 'little':
                blue, green, red, alpha = data[offset:offset + 4]
            else:
                alpha, red, green, blue = data[offset:offset + 4]
            pixels.extend((alpha, min(255, red * 255 // alpha) if alpha else 0,
                           min(255, green * 255 // alpha) if alpha else 0,
                           min(255, blue * 255 // alpha) if alpha else 0))
        result.append((size, size, bytes(pixels)))
    return result


class StatusTray:
    def __init__(self, bus, callback):
        self.bus, self.callback = bus, callback
        self.closed = False
        self.registered = False
        self.owner = None
        self.state = None
        self.activation_token = ''
        self.revision = 0
        self.cancel = Gio.Cancellable()
        self.items = {}
        self.properties = {}
        self.registrations = []
        try:
            for path, xml, getter in ((ITEM_PATH, ITEM_XML, self._item_property), (MENU_PATH, MENU_XML, self._menu_property)):
                interface = Gio.DBusNodeInfo.new_for_xml(xml).interfaces[0]
                self.registrations.append(bus.register_object(path, interface, self._method, getter, None))
        except GLib.Error:
            for registration in self.registrations:
                bus.unregister_object(registration)
            raise
        self.watch = Gio.bus_watch_name_on_connection(bus, WATCHER, Gio.BusNameWatcherFlags.NONE, self._appeared, self._vanished)
        self.update(False, False, '', False, False)

    def _appeared(self, connection, name, owner):
        self.owner = owner
        def completed(connection, result):
            try:
                connection.call_finish(result)
                if not self.closed and self.owner == owner:
                    self.registered = True
            except GLib.Error:
                if self.owner == owner:
                    self.registered = False
        connection.call(WATCHER, '/StatusNotifierWatcher', WATCHER, 'RegisterStatusNotifierItem',
                        GLib.Variant('(s)', (ITEM_PATH,)), None, Gio.DBusCallFlags.NONE, 3000, self.cancel, completed)

    def _vanished(self, *_):
        self.owner, self.registered = None, False

    @staticmethod
    def _entry(label, action=None, enabled=True, icon=None):
        properties = {'label': GLib.Variant('s', label), 'enabled': GLib.Variant('b', enabled),
                      'visible': GLib.Variant('b', True)}
        if icon:
            properties['icon-name'] = GLib.Variant('s', icon)
        return properties, action

    def update(self, connected, connecting, device, owned, mirroring, external=False):
        state = (bool(connected), bool(connecting), str(device), bool(owned), bool(mirroring), bool(external))
        if self.closed or state == self.state:
            return
        self.state = state
        color = 'connected' if connected else 'connecting' if connecting else 'disconnected'
        status = f'Connected · {device}' if connected and device else 'Connected' if connected else 'Connecting…' if connecting else 'Disconnected'
        pixmaps = icon_pixmaps(color)
        self.properties = {
            'Category': GLib.Variant('s', 'ApplicationStatus'), 'Id': GLib.Variant('s', 'vokit'),
            'Title': GLib.Variant('s', 'VOkit — ' + status), 'Status': GLib.Variant('s', 'Active'),
            'WindowId': GLib.Variant('i', 0), 'IconThemePath': GLib.Variant('s', ''),
            'IconName': GLib.Variant('s', ''), 'IconPixmap': GLib.Variant('a(iiay)', pixmaps),
            'OverlayIconName': GLib.Variant('s', ''), 'OverlayIconPixmap': GLib.Variant('a(iiay)', []),
            'AttentionIconName': GLib.Variant('s', ''), 'AttentionIconPixmap': GLib.Variant('a(iiay)', []),
            'AttentionMovieName': GLib.Variant('s', ''), 'ToolTip': GLib.Variant('(sa(iiay)ss)', ('', pixmaps, 'VOkit', status)),
            'ItemIsMenu': GLib.Variant('b', False), 'Menu': GLib.Variant('o', MENU_PATH),
        }
        separator = ({'type': GLib.Variant('s', 'separator')}, None)
        self.items = {
            0: ({'children-display': GLib.Variant('s', 'submenu')}, None),
            1: self._entry(status.replace('_', '__'), enabled=False), 2: separator,
            3: self._entry('Open VOkit', 'show', icon='phone-symbolic'),
            4: self._entry('Phone mirroring', 'mirror', connected, 'video-display-symbolic'),
            5: self._entry('Stop mirroring', 'stop-mirror', mirroring, 'media-playback-stop-symbolic'),
            6: self._entry('Camera & microphone', 'camera', connected, 'camera-video-symbolic'),
            7: self._entry('Browse phone files', 'files', connected, 'folder-remote-symbolic'),
            8: self._entry('Settings', 'settings', icon='preferences-system-symbolic'),
            9: self._entry('Disconnect phone' if owned else 'Connected session' if external else 'Connect phone',
                           'connection', owned or not external, 'network-wireless-symbolic'),
            10: separator, 11: self._entry('Quit VOkit', 'quit', icon='application-exit-symbolic'),
        }
        self.revision = self.revision % 0xffffffff + 1
        for signal in ('NewIcon', 'NewTitle', 'NewToolTip'):
            self.bus.emit_signal(None, ITEM_PATH, ITEM_INTERFACE, signal, None)
        self.bus.emit_signal(None, ITEM_PATH, 'org.freedesktop.DBus.Properties', 'PropertiesChanged',
                             GLib.Variant('(sa{sv}as)', (ITEM_INTERFACE, self.properties, [])))
        self.bus.emit_signal(None, MENU_PATH, MENU_INTERFACE, 'LayoutUpdated', GLib.Variant('(ui)', (self.revision, 0)))

    def _item_property(self, connection, sender, path, interface, name):
        return self.properties.get(name)

    def _menu_property(self, connection, sender, path, interface, name):
        return {'Version': GLib.Variant('u', 3), 'TextDirection': GLib.Variant('s', 'ltr'),
                'Status': GLib.Variant('s', 'normal'), 'IconThemePath': GLib.Variant('as', [])}.get(name)

    def _props(self, item, names):
        properties = self.items[item][0]
        return {key: value for key, value in properties.items() if not names or key in names}

    def _layout(self, item, depth, names):
        children = []
        if item == 0 and depth != 0:
            children = [GLib.Variant('(ia{sv}av)', self._layout(key, depth - 1, names)) for key in self.items if key != 0]
        return item, self._props(item, names), children

    def _schedule(self, action):
        token, self.activation_token = self.activation_token, ''
        def dispatch():
            if not self.closed:
                self.callback(action, token)
            return GLib.SOURCE_REMOVE
        GLib.idle_add(dispatch)

    def _event(self, item, event):
        properties, action = self.items[item]
        if event == 'clicked' and action and properties.get('enabled', GLib.Variant('b', True)).unpack():
            self._schedule(action)

    def _method(self, connection, sender, path, interface, method, parameters, invocation):
        try:
            values = parameters.unpack()
            if interface == ITEM_INTERFACE:
                if method == 'ProvideXdgActivationToken':
                    if len(values[0]) <= 4096 and '\x00' not in values[0]:
                        self.activation_token = values[0]
                elif method in ('Activate', 'ContextMenu'):
                    self._schedule('show')
                elif method == 'SecondaryActivate' and self.state[0]:
                    self._schedule('mirror')
                invocation.return_value(None)
            elif method == 'GetLayout':
                parent, depth, names = values
                invocation.return_value(GLib.Variant('(u(ia{sv}av))', (self.revision, self._layout(parent, depth, names))))
            elif method == 'GetGroupProperties':
                ids, names = values
                invocation.return_value(GLib.Variant('(a(ia{sv}))', ([(item, self._props(item, names)) for item in ids if item in self.items],)))
            elif method == 'GetProperty':
                invocation.return_value(GLib.Variant('(v)', (self.items[values[0]][0][values[1]],)))
            elif method == 'Event':
                self._event(values[0], values[1])
                invocation.return_value(None)
            elif method == 'EventGroup':
                failed = []
                for item, event, data, timestamp in values[0]:
                    if item in self.items:
                        self._event(item, event)
                    else:
                        failed.append(item)
                invocation.return_value(GLib.Variant('(ai)', (failed,)))
            elif method == 'AboutToShow':
                if values[0] not in self.items:
                    raise KeyError()
                invocation.return_value(GLib.Variant('(b)', (False,)))
            elif method == 'AboutToShowGroup':
                invocation.return_value(GLib.Variant('(aiai)', ([], [item for item in values[0] if item not in self.items])))
            else:
                invocation.return_dbus_error('org.freedesktop.DBus.Error.UnknownMethod', 'Unknown tray method')
        except (KeyError, ValueError, TypeError):
            invocation.return_dbus_error('org.freedesktop.DBus.Error.InvalidArgs', 'Invalid tray menu item or property')

    def close(self):
        if self.closed:
            return
        self.closed = True
        self.registered = False
        self.cancel.cancel()
        Gio.bus_unwatch_name(self.watch)
        for registration in self.registrations:
            self.bus.unregister_object(registration)
