"""Bounded child-process bridge. GTK callbacks always run in the main context."""
from .i18n import gettext as _
from .i18n import current_language
import concurrent.futures
import configparser
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time

from gi.repository import GLib

JSON_LIMIT = 1024 * 1024
EVENT_LIMIT = 2 * 1024 * 1024


class Autostart:
    """Manage this application's per-user XDG autostart entry."""
    MARKER = 'X-VOkit-Managed'

    def __init__(self, directory=None, executable=None):
        config = Path(os.environ.get('XDG_CONFIG_HOME') or Path.home() / '.config')
        if not config.is_absolute():
            config = Path.home() / '.config'
        self.path = Path(directory) / 'io.github.vivolinux.OfficeKit.desktop' if directory else config / 'autostart/io.github.vivolinux.OfficeKit.desktop'
        bindir = Path(os.environ.get('VIVOLINUX_BIN_DIR', str(Path(sys.argv[0]).resolve().parent)))
        self.executable = Path(executable) if executable else bindir / 'vokit'

    def _managed(self):
        try:
            info = self.path.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_size > 65536:
                return False
            parser = configparser.ConfigParser(interpolation=None)
            parser.read(self.path, encoding='utf-8')
            return parser.getboolean('Desktop Entry', self.MARKER, fallback=False)
        except (OSError, ValueError, UnicodeError, configparser.Error):
            return False

    def enabled(self):
        if not self._managed():
            return False
        try:
            parser = configparser.ConfigParser(interpolation=None)
            parser.read(self.path, encoding='utf-8')
            return not parser.getboolean('Desktop Entry', 'Hidden', fallback=False)
        except (OSError, ValueError, UnicodeError, configparser.Error):
            return False

    def _command(self):
        command = '"' + ''.join('\\' + c if c in '\\"`$' else c for c in str(self.executable.absolute())) + '"'
        return command.replace('\\', '\\\\').replace('%', '%%')

    def _write(self, content):
        fd, temporary = tempfile.mkstemp(prefix='.vokit-autostart-', dir=self.path.parent)
        try:
            with os.fdopen(fd, 'w', encoding='utf-8') as stream:
                stream.write(content)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.path)
        finally:
            try: os.unlink(temporary)
            except FileNotFoundError: pass

    def migrate_background(self):
        """Update the old managed entry while preserving user desktop settings."""
        if not self.enabled():
            return
        old = 'Exec=' + self._command() + '\n'
        text = self.path.read_text(encoding='utf-8')
        if old in text:
            self._write(text.replace(old, 'Exec=' + self._command() + ' --background\n', 1))

    def set_enabled(self, enabled):
        exists = os.path.lexists(self.path)
        if exists and not self._managed():
            raise OSError(_('The existing startup entry is not managed by VOkit.'))
        if not enabled:
            if exists:
                self.path.unlink()
            return
        if not self.executable.is_file() or not os.access(self.executable, os.X_OK) or any(c in str(self.executable) for c in '\r\n'):
            raise OSError(_('The VOkit executable could not be found.'))
        self.path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        # Desktop Entry values and Exec quoting each have their own escapes.
        content = ('[Desktop Entry]\nType=Application\nName=VOkit\n'
                   f'Exec={self._command()} --background\nX-VOkit-Managed=true\n')
        self._write(content)


def deliver(callback, *args):
    def invoke():
        callback(*args)
        return False
    GLib.idle_add(invoke)


class Settings:
    KEYS = {"phone", "manual_phone", "receive_dir", "clipboard", "notifications", "wifi_direct", "region", "auto_reconnect", "language", "theme", "window_width", "window_height", "list_mode"}

    def __init__(self, path=None):
        self.path = Path(path) if path else Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home()/".config"))) / 'vivo-office-kit/preferences.json'
        self.values = {}
        try:
            info = self.path.lstat()
            if stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_size <= 65536:
                value = json.loads(self.path.read_text())
                if isinstance(value, dict):
                    self.values = {k:v for k,v in value.items() if k in self.KEYS}
        except (OSError, ValueError):
            pass
        if path is None and not self.values:
            # Migrate non-secret preferences from the previous frontend only.
            legacy = Path.home()/".config/Unknown Organization/vivo-desktop.conf"
            parser = configparser.ConfigParser(interpolation=None)
            try:
                if legacy.is_file() and legacy.stat().st_size <= 65536:
                    parser.read(legacy)
                    for key, section, option in (("phone","connection","phone"),("receive_dir","transfer","receiveDirectory")):
                        if parser.has_option(section, option): self.values[key] = parser.get(section, option)
                    for key in ("clipboard", "notifications"):
                        if parser.has_option("connection", key): self.values[key] = parser.getboolean("connection", key)
            except (OSError, ValueError, configparser.Error):
                pass

    def get(self, key, default=None):
        return self.values.get(key, default)

    def set(self, key, value):
        if key in self.KEYS:
            self.values[key] = value

    def save(self):
        self.path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        descriptor, name = tempfile.mkstemp(prefix=".preferences-", dir=self.path.parent)
        try:
            with os.fdopen(descriptor, "w") as stream:
                json.dump(self.values, stream, ensure_ascii=False)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(name, self.path)
        finally:
            try: os.unlink(name)
            except FileNotFoundError: pass


class ProcessHandle:
    def __init__(self):
        self.process = None
        self.cancelled = threading.Event()
        self.finished = threading.Event()
        self.forget_remembered = False

    @property
    def active(self):
        return not self.finished.is_set()

    def poll(self):
        return self.process.poll() if self.process else (0 if self.finished.is_set() else None)

    def terminate(self):
        self.cancelled.set()
        if self.process and self.process.poll() is None:
            try: self.process.terminate()
            except ProcessLookupError: pass

    def kill(self):
        self.cancelled.set()
        if self.process and self.process.poll() is None:
            try: self.process.kill()
            except ProcessLookupError: pass


class Backend:
    def __init__(self, cli_path=None):
        bindir = Path(os.environ.get("VIVOLINUX_BIN_DIR", str(Path(sys.argv[0]).resolve().parent)))
        self.cli_path = str(cli_path or (bindir/"vivolinux" if (bindir/"vivolinux").is_file() else shutil.which("vivolinux") or Path.cwd()/"build/vivolinux"))
        runtime = Path(os.environ.get("XDG_RUNTIME_DIR", f"/tmp/vivolinux-{os.getuid()}"))
        directory = runtime/"vivolinux" if os.environ.get("XDG_RUNTIME_DIR") else runtime
        self.native_socket, self.media_socket = str(directory/"native.sock"), str(directory/"media.sock")
        self.pool = concurrent.futures.ThreadPoolExecutor(max_workers=8, thread_name_prefix='vivo-action')
        self.slots = threading.BoundedSemaphore(16)
        self.handles = set()
        self.lock = threading.Lock()
        self.closed = False
        self.pair_process = None
        self.reconnect_enabled = True
        self.bluetooth_initialized = False

    @property
    def owned(self):
        return bool(self.pair_process and self.pair_process.active)

    def _spawn(self, handle, arguments):
        process = subprocess.Popen([self.cli_path, *arguments], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   env={**os.environ, "VOKIT_LANGUAGE": current_language()})
        handle.process = process
        if handle.cancelled.is_set() or self.closed: handle.terminate()
        with self.lock: self.handles.add(handle)
        return process

    @staticmethod
    def _read(handle, timeout, limit, event=None, observe=None):
        process = handle.process
        result, pending = bytearray(), bytearray()
        deadline = time.monotonic()+timeout
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                if handle.cancelled.is_set(): raise RuntimeError(_('Action cancelled.'))
                if time.monotonic() > deadline: raise RuntimeError(_('Phone action timed out.'))
                ready = selector.select(0.2)
                if not ready: continue
                chunk = os.read(process.stdout.fileno(), 8192)
                if not chunk: break
                if event is None:
                    if len(result)+len(chunk)>limit: raise RuntimeError(_('Response exceeded its safety limit.'))
                    result.extend(chunk)
                else:
                    pending.extend(chunk)
                    while b"\n" in pending:
                        line, separator, rest = pending.partition(b"\n")
                        pending = bytearray(rest)
                        if len(line)>limit: raise RuntimeError(_('Event exceeded its safety limit.'))
                        value = json.loads(line)
                        if not isinstance(value,dict): raise RuntimeError(_('Invalid connection event.'))
                        if observe: observe(value)
                        deliver(event, value)
                    if len(pending)>limit: raise RuntimeError(_('Event exceeded its safety limit.'))
        return bytes(result)

    def _finish(self, handle):
        if handle.process:
            if handle.process.poll() is None:
                handle.terminate()
                try: handle.process.wait(timeout=2)
                except subprocess.TimeoutExpired: handle.kill(); handle.process.wait()
            handle.process.stdout.close()
        handle.finished.set()
        with self.lock: self.handles.discard(handle)

    def call(self, args, done):
        handle = ProcessHandle()
        if self.closed or not self.slots.acquire(blocking=False):
            handle.finished.set()
            deliver(done, None, _('Action queue is busy. Try again.'))
            return handle
        def work():
            value, error = None, None
            try:
                process = self._spawn(handle, list(args))
                timeout = 4 if args and args[0]=='native-status' else 900 if args and args[0]=='account-login' else 600 if args and args[0] in ("preview","download","upload") else 90
                output = self._read(handle, timeout, JSON_LIMIT)
                code = process.wait(timeout=2)
                value = json.loads(output)
                if not isinstance(value,dict): raise ValueError()
                if code:
                    error = _('Approve file access on your phone.') if value.get("error")=="file_permission_required" else _('Phone action failed. Check connection and access.')
            except RuntimeError as exception:
                error = str(exception)
            except (OSError, ValueError, subprocess.SubprocessError):
                error = _('Native session unavailable. Connect your phone or reconnect the session.')
            finally:
                self._finish(handle)
                self.slots.release()
                deliver(done, value, error)
        self.pool.submit(work)
        return handle

    @staticmethod
    def state_dir():
        return Path(os.environ.get("XDG_DATA_HOME", str(Path.home()/".local/share"))) / "vivo-office-kit/pairing"

    def remembered(self):
        """Return only non-secret routing metadata from a private saved record."""
        try:
            fd = os.open(self.state_dir()/"remembered.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(fd) as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_size > 16384:
                    return None
                value = json.loads(stream.read(16385))
            if (value.get("version") == 1 and value.get("region") in ("china", "overseas")
                    and type(value.get("wifi_direct")) is bool):
                return {"region": value["region"], "wifi_direct": value["wifi_direct"]}
        except (OSError, ValueError, TypeError, AttributeError):
            pass
        return None

    def _forget_remembered(self):
        try:
            subprocess.run([self.cli_path, "pair", "--forget-remembered", "--state-dir", str(self.state_dir())],
                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
        except (OSError, subprocess.SubprocessError):
            pass

    @staticmethod
    def retry_delay(attempt):
        return min(60, 5 * 2 ** min(max(0, attempt - 1), 4))

    def pair(self, phone, trust, receive_dir, clipboard, notifications, on_event, on_exit,
             wifi_direct=False, region="overseas", auto_reconnect=True, resume=False):
        if region not in ("overseas", "china"):
            raise ValueError(_('Unknown pairing region'))
        if self.owned: return self.pair_process
        handle = ProcessHandle()
        self.pair_process = handle
        self.reconnect_enabled = auto_reconnect
        args = ["pair", "--persistent", "--receive-dir", str(receive_dir), "--state-dir", str(self.state_dir())]
        if phone: args.extend(["--phone", phone])
        if trust: args.append("--trust-scanned-phone")
        if clipboard: args.append("--clipboard")
        if notifications: args.append("--notifications")
        if wifi_direct: args.append("--wifi-direct")
        args.extend(["--region", region])
        if auto_reconnect: args.append("--remember")
        if resume: args.append("--resume")
        def work():
            code, attempt, authorized = 1, 0, False
            try:
                while not handle.cancelled.is_set() and not self.closed:
                    observed = {"connected": False, "pairing_required": False, "paused": None}
                    def observe(value):
                        if value.get("stage") == "bluetooth_attempt": self.bluetooth_initialized = True
                        if value.get("stage") == "control_connected": observed["connected"] = True
                        if value.get("stage") == "reconnect_failed" and value.get("code") == "pairing_required":
                            observed["pairing_required"] = True
                        if value.get("stage") == "phone_remembered" and not self.reconnect_enabled:
                            self._forget_remembered()
                        if value.get("stage") == "reconnect_failed" and value.get("code") in ("login_required", "bluetooth_required"):
                            observed["paused"] = value["code"]
                    if wifi_direct and "--resume" in args and not self.bluetooth_initialized:
                        args.append("--bluetooth-power-on")
                    process = self._spawn(handle, args)
                    self._read(handle, 365*24*3600, EVENT_LIMIT, on_event, observe)
                    code = process.wait(timeout=2)
                    process.stdout.close()
                    if "--bluetooth-power-on" in args: args.remove("--bluetooth-power-on")
                    authorized = authorized or observed["connected"]
                    if handle.cancelled.is_set() or self.closed: break
                    if observed["pairing_required"]:
                        self._forget_remembered()
                        deliver(on_event, {"stage": "reconnect_pairing_required"})
                        break
                    if observed["paused"]:
                        deliver(on_event, {"stage": "reconnect_" + observed["paused"]})
                        break
                    if not self.reconnect_enabled or not self.remembered() or (not authorized and "--resume" not in args):
                        break
                    attempt = 1 if observed["connected"] else attempt + 1
                    delay = self.retry_delay(attempt)
                    if wifi_direct: delay = max(35, delay)
                    deliver(on_event, {"stage": "reconnect_wait", "count": delay})
                    if handle.cancelled.wait(delay): break
                    if not self.reconnect_enabled: break
                    if "--resume" not in args: args.append("--resume")
                    deliver(on_event, {"stage": "reconnect_attempt"})
            except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
                pass
            finally:
                self._finish(handle)
                if handle.forget_remembered: self._forget_remembered()
                deliver(on_exit, code)
        threading.Thread(target=work, daemon=True, name="vivo-pair").start()
        return handle

    def disconnect(self, forget=True):
        if self.pair_process:
            if forget: self.pair_process.forget_remembered = True
            self.pair_process.terminate()
        elif forget:
            self.pool.submit(self._forget_remembered)

    def set_auto_reconnect(self, enabled):
        self.reconnect_enabled = bool(enabled)
        if not enabled:
            self.pool.submit(self._forget_remembered)

    def close(self):
        self.closed = True
        self.disconnect(forget=False)
        with self.lock: handles = list(self.handles)
        for handle in handles: handle.terminate()
        self.pool.shutdown(wait=False, cancel_futures=True)
