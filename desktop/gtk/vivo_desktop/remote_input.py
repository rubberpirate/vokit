"""Bounded input for the monitor the user explicitly shares with Remote PC."""
import json
import math
import os
from pathlib import Path
import queue
import re
import shutil
import socket
import stat
import subprocess
import threading


KEYS = {'enter': 'Return', 'return': 'Return', 'backspace': 'BackSpace',
        'tab': 'Tab', 'escape': 'Escape', 'esc': 'Escape', 'delete': 'Delete',
        'insert': 'Insert', 'home': 'Home', 'end': 'End', 'pageup': 'Prior',
        'pagedown': 'Next', 'up': 'Up', 'down': 'Down', 'left': 'Left',
        'right': 'Right', 'space': 'space', 'capslock': 'Caps_Lock',
        'win': 'Super_L', 'windows': 'Super_L', 'super': 'Super_L',
        'command': 'Super_L', 'meta': 'Super_L', 'logo': 'Super_L',
        'control': 'Control_L', 'ctrl': 'Control_L', 'shift': 'Shift_L', 'alt': 'Alt_L'}
MODIFIERS = {'control': 'CTRL', 'ctrl': 'CTRL', 'shift': 'SHIFT',
             'alt': 'ALT', 'command': 'SUPER', 'win': 'SUPER', 'super': 'SUPER',
             'windows': 'SUPER', 'meta': 'SUPER', 'logo': 'SUPER'}


def finite(value, minimum, maximum):
    return type(value) in (int, float) and math.isfinite(value) and minimum <= value <= maximum


def translate(data, geometry):
    """Only generate fixed input operations; phone strings never become IPC code."""
    if not isinstance(data, dict):
        return None
    x, y, width, height = geometry
    if data.get('type') == 'mouse':
        button, position, view = data.get('button'), data.get('position'), data.get('view')
        if not all(isinstance(value, dict) for value in (button, position, view)):
            return None
        action = button.get('action')
        if action == 'scroll':
            # shortcut: Hyprland's IPC cannot inject wheel events; add a virtual
            # pointer backend when this compositor provides a stable wheel API.
            return []
        if action not in ('move', 'left', 'middle', 'right', 'down', 'up', 'rightDown', 'rightUp', 'keepDrag', 'rightKeepDrag'):
            return None
        vx, vy = view.get('width'), view.get('height')
        px, py = position.get('clientX'), position.get('clientY')
        if not finite(vx, 1, 32768) or not finite(vy, 1, 32768) or not finite(px, 0, vx) or not finite(py, 0, vy):
            return None
        result = [('move', (x + min(width - 1, int(px * width / vx)), y + min(height - 1, int(py * height / vy))))]
        if action in ('left', 'middle', 'right'):
            key = {'left': 272, 'middle': 274, 'right': 273}[action]
            if type(button.get('doubble', False)) is not bool:
                return None
            result.extend([('button', (key, 'down')), ('button', (key, 'up'))] * (2 if button.get('doubble') else 1))
        elif action in ('down', 'up', 'rightDown', 'rightUp'):
            result.append(('button', (273 if action.startswith('right') else 272, 'down' if action.endswith('Down') or action == 'down' else 'up')))
        return result
    if data.get('type') != 'key':
        return None
    modifiers = data.get('modifiers', [])
    if not isinstance(modifiers, list) or len(modifiers) > 4 or any(not isinstance(item, str) or item.lower() not in MODIFIERS for item in modifiers):
        return None
    key, value = data.get('key', ''), data.get('value', '')
    if not isinstance(key, str) or not isinstance(value, str) or len(value.encode('utf-8')) > 4096 or '\x00' in value:
        return None
    mapped = KEYS.get(key.lower())
    if value and not modifiers and mapped != 'Super_L':
        return [('text', value)]
    if mapped is None:
        if re.fullmatch(r'[A-Za-z0-9]', key) or re.fullmatch(r'[fF](?:[1-9]|1[0-9]|2[0-4])', key):
            mapped = key
        else:
            return None
    return [('key', (' '.join(dict.fromkeys(MODIFIERS[item.lower()] for item in modifiers)), mapped))]


def key_arguments(modifiers, key):
    """Virtual keyboard events also reach compositor shortcuts, unlike IPC pass."""
    flags = {'CTRL': 'ctrl', 'SHIFT': 'shift', 'ALT': 'alt', 'SUPER': 'logo'}
    names = [flags[item] for item in modifiers.split()]
    modifier_key = {'Super_L': 'logo', 'Control_L': 'ctrl', 'Shift_L': 'shift', 'Alt_L': 'alt'}.get(key)
    if modifier_key and modifier_key not in names:
        names.append(modifier_key)
    args = ['wtype']
    for name in names:
        args.extend(['-M', name])
    args.extend(['-k', key])
    for name in reversed(names):
        args.extend(['-m', name])
    return args


def monitor_details(monitors, width, height, position=None, mapping_id=None):
    """Resolve only the shared monitor; equal-sized displays need an identity."""
    if not isinstance(monitors, list):
        return None
    # Portal IDs are opaque; only use one as a connector when it exactly matches.
    named = [monitor for monitor in monitors if isinstance(monitor, dict)
             and isinstance(mapping_id, str) and mapping_id
             and monitor.get('name') == mapping_id]
    candidates = []
    for monitor in named if named else monitors:
        if not isinstance(monitor, dict) or monitor.get('disabled', False):
            continue
        scale = monitor.get('scale', 1)
        if not finite(scale, .25, 8):
            continue
        mw, mh = monitor.get('width'), monitor.get('height')
        transform = monitor.get('transform', 0)
        if not finite(mw, 1, 32768) or not finite(mh, 1, 32768) or type(transform) is not int:
            continue
        if transform % 2:
            mw, mh = mh, mw
        if (mw, mh) != (width, height):
            continue
        mx, my = monitor.get('x', 0), monitor.get('y', 0)
        if not finite(mx, -65536, 65536) or not finite(my, -65536, 65536):
            continue
        # Hyprland's portal reports stream-local (0, 0), even for other outputs.
        if not named and position is not None and tuple(position) != (mx, my):
            continue
        geometry = (int(mx), int(my), max(1, int(mw / scale)), max(1, int(mh / scale)))
        candidates.append((geometry, scale))
    return candidates[0] if len(candidates) == 1 else None


class RemoteInput:
    def __init__(self, width, height, position=None, mapping_id=None):
        self.available = False
        self.geometry = None
        self.scale = None
        self.lua_dispatch = False
        self.failure = None
        self.path = None
        self.held = set()
        self.stopping = threading.Event()
        self.work = queue.Queue(maxsize=64)
        self.thread = None
        signature = os.environ.get('HYPRLAND_INSTANCE_SIGNATURE', '')
        runtime = os.environ.get('XDG_RUNTIME_DIR', '')
        if not runtime or not re.fullmatch(r'[A-Za-z0-9_]+', signature) or not shutil.which('wtype'):
            return
        self.path = str(Path(runtime) / 'hypr' / signature / '.socket.sock')
        try:
            info = os.stat(self.path, follow_symlinks=False)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                return
            monitors = json.loads(self._ipc('j/monitors'))
            details = monitor_details(monitors, width, height, position, mapping_id)
            if details is None:
                return
            self.geometry, self.scale = details
            # Lua-configured Hyprland expects dispatcher closures, not old names.
            self.lua_dispatch = self._ipc('dispatch hl.dsp.no_op()').strip().lower() == 'ok'
        except (OSError, ValueError, TypeError, KeyError):
            return
        self.available = True
        self.thread = threading.Thread(target=self._worker, name='vivo-remote-input', daemon=True)
        self.thread.start()

    def _ipc(self, command):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(.5)
            client.connect(self.path)
            client.sendall(command.encode('ascii'))
            data = bytearray()
            while len(data) <= 65536:
                chunk = client.recv(min(4096, 65537 - len(data)))
                if not chunk:
                    break
                data.extend(chunk)
            if len(data) > 65536:
                raise ValueError('Compositor reply exceeds size limit')
            return data.decode('utf-8')

    def handle(self, data):
        if not self.available or self.stopping.is_set():
            return False
        try:
            operations = translate(data, self.geometry)
            if operations is None:
                return False
            if not operations:
                return True
            self.work.put_nowait(operations)
            return True
        except (ValueError, UnicodeError, queue.Full):
            return False

    def _command(self, kind, value):
        if not self.lua_dispatch:
            if kind == 'move':
                return 'dispatch movecursor %d %d' % value
            if kind == 'button':
                return 'dispatch sendkeystate ,mouse:%d,%s,' % value
        if kind == 'move':
            return 'dispatch hl.dsp.cursor.move({x=%d,y=%d})' % value
        if kind == 'button':
            key, state = value
            return 'dispatch hl.dsp.send_key_state({mods="",key="mouse:%d",state=%s})' % (key, json.dumps(state))
        raise ValueError('Unsupported compositor input operation')

    def _worker(self):
        kind = 'backend'
        try:
            while not self.stopping.is_set():
                try:
                    operations = self.work.get(timeout=.1)
                except queue.Empty:
                    continue
                for kind, value in operations:
                    if self.stopping.is_set():
                        break
                    if kind == 'move':
                        reply = self._ipc(self._command(kind, value))
                    elif kind == 'button':
                        key, state = value
                        reply = self._ipc(self._command(kind, value))
                        if state == 'down':
                            self.held.add(key)
                        else:
                            self.held.discard(key)
                    else:
                        args = key_arguments(*value) if kind == 'key' else ['wtype', '--', value]
                        result = subprocess.run(args, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3)
                        reply = 'ok' if result.returncode == 0 else ''
                    if not reply.strip().lower().startswith('ok'):
                        raise OSError('Desktop input rejected')
        except (OSError, ValueError, subprocess.TimeoutExpired):
            self.failure = 'input-%s-failed' % kind
            self.available = False
            self.stopping.set()
        finally:
            for key in list(self.held):
                try:
                    self._ipc(self._command('button', (key, 'up')))
                except (OSError, ValueError):
                    pass
            self.held.clear()

    def close(self):
        self.available = False
        self.stopping.set()
        if self.thread and self.thread is not threading.current_thread():
            self.thread.join(timeout=4)
