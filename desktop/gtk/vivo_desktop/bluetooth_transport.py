#!/usr/bin/env python3
"""Finite BlueZ RFCOMM transport for the backend; JSON lines over private stdio.

The parent owns protocol/authentication. Adapter power is changed only when the
parent explicitly requests it. No daemon, pairing or trust change is performed.
BlueZ removes the profile when this process exits.
"""
import base64
import json
import os
import re
import signal
import socket
import sys

UUID = '8fa9c715-bd1f-596c-a1b0-13162b15c892'


def emit(value):
    print(json.dumps(value, separators=(',', ':')), flush=True)


def main():
    import dbus
    import dbus.service
    from dbus.mainloop.glib import DBusGMainLoop
    from gi.repository import GLib
    DBusGMainLoop(set_as_default=True)
    options = json.loads(sys.stdin.readline())
    bus = dbus.SystemBus()
    objects = dbus.Interface(bus.get_object('org.bluez', '/'),
                             'org.freedesktop.DBus.ObjectManager').GetManagedObjects()
    candidates = []
    address = options.get('address')
    if address and not re.fullmatch(r'(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}', address):
        raise ValueError('invalid Bluetooth address')
    for path, interfaces in objects.items():
        device = interfaces.get('org.bluez.Device1', {})
        if not device or not device.get('Paired'):
            continue
        if address:
            if str(device.get('Address', '')).lower() != address.lower():
                continue
        elif UUID not in [str(u).lower() for u in device.get('UUIDs', [])]:
            continue
        candidates.append((path, device))
    if len(candidates) != 1:
        emit({'type': 'error', 'code': 'BLUETOOTH_DEVICE_REQUIRED', 'candidates': len(candidates)}); return
    target, properties = candidates[0]
    adapter = properties['Adapter']
    if not bool(objects[adapter]['org.bluez.Adapter1'].get('Powered')):
        if options.get('powerOn') is True:
            dbus.Interface(bus.get_object('org.bluez', adapter), 'org.freedesktop.DBus.Properties').Set(
                'org.bluez.Adapter1', 'Powered', dbus.Boolean(True))
        else:
            emit({'type': 'error', 'code': 'BLUETOOTH_OFF'}); return
    loop = GLib.MainLoop()
    manager = dbus.Interface(bus.get_object('org.bluez', '/org/bluez'), 'org.bluez.ProfileManager1')
    profile_path = '/org/vivo_connect/preconnect/p' + str(os.getpid())
    connection = None
    stdin_buffer = bytearray()
    registered = False

    def fail(code):
        emit({'type': 'error', 'code': code}); loop.quit()

    def connect_failed(error):
        reason = re.sub(r'(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}', '<device>', error.get_dbus_message())[:256]
        emit({'type': 'error', 'code': str(error.get_dbus_name()).rsplit('.', 1)[-1], 'reason': reason})
        loop.quit()

    def incoming(_source, condition):
        if condition & (GLib.IO_ERR | GLib.IO_NVAL):
            fail('BLUETOOTH_DISCONNECTED'); return False
        try:
            data = connection.recv(16384)
            if not data:
                fail('BLUETOOTH_DISCONNECTED'); return False
            emit({'type': 'data', 'data': base64.b64encode(data).decode('ascii')})
            return True
        except OSError:
            fail('BLUETOOTH_IO_FAILED'); return False

    def command(_source, condition):
        nonlocal stdin_buffer
        try:
            data = os.read(sys.stdin.fileno(), 65536)
            if not data:
                loop.quit(); return False
            stdin_buffer.extend(data)
            if len(stdin_buffer) > 2 * 1024 * 1024:
                fail('BLUETOOTH_COMMAND_TOO_LARGE'); return False
            while b'\n' in stdin_buffer:
                line, _, remainder = stdin_buffer.partition(b'\n'); stdin_buffer = bytearray(remainder)
                value = json.loads(line)
                if value.get('close'):
                    loop.quit(); return False
                payload = base64.b64decode(value['send'], validate=True)
                if not connection or len(payload) > 1024 * 1024 + 14:
                    fail('BLUETOOTH_INVALID_SEND'); return False
                connection.sendall(payload)
            return True
        except (OSError, ValueError, KeyError):
            fail('BLUETOOTH_IO_FAILED'); return False

    class Profile(dbus.service.Object):
        @dbus.service.method('org.bluez.Profile1', in_signature='', out_signature='')
        def Release(self):
            loop.quit()

        @dbus.service.method('org.bluez.Profile1', in_signature='oha{sv}', out_signature='')
        def NewConnection(self, device, fd, _properties):
            nonlocal connection
            owned_fd = fd.take()
            if str(device) != str(target) or connection is not None:
                os.close(owned_fd)
                raise dbus.exceptions.DBusException('Unexpected device', name='org.bluez.Error.Rejected')
            connection = socket.socket(fileno=owned_fd)
            connection.settimeout(1)
            GLib.io_add_watch(connection.fileno(), GLib.IO_IN | GLib.IO_HUP | GLib.IO_ERR, incoming)
            emit({'type': 'ready', 'address': str(properties['Address'])})

        @dbus.service.method('org.bluez.Profile1', in_signature='o', out_signature='')
        def RequestDisconnection(self, _device):
            if connection:
                connection.close()
            loop.quit()

    profile = Profile(bus, profile_path)
    try:
        manager.RegisterProfile(profile_path, UUID, dbus.Dictionary({
            'Name': 'vivo-connect preconnection', 'Role': 'client',
            'RequireAuthentication': False, 'RequireAuthorization': False, 'AutoConnect': False
        }, signature='sv'))
        registered = True
        GLib.io_add_watch(sys.stdin.fileno(), GLib.IO_IN | GLib.IO_HUP | GLib.IO_ERR, command)
        GLib.timeout_add_seconds(min(120, max(5, int(options.get('timeout', 45)))),
                                 lambda: (fail('BLUETOOTH_TIMEOUT'), False)[1])
        for sig in (signal.SIGTERM, signal.SIGINT):
            GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, sig, lambda: (loop.quit(), False)[1])
        dbus.Interface(bus.get_object('org.bluez', target), 'org.bluez.Device1').ConnectProfile(
            UUID, reply_handler=lambda: None,
            error_handler=connect_failed, timeout=20)
        loop.run()
    finally:
        if connection:
            connection.close()
        if registered:
            manager.UnregisterProfile(profile_path)
        profile.remove_from_connection()
        bus.close()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # D-Bus errors may contain device addresses; emit only the error type.
        emit({'type': 'error', 'code': type(error).__name__})
        sys.exit(1)
