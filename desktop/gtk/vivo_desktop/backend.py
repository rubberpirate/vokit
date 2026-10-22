"""Bounded child-process bridge. GTK callbacks always run in the main context."""
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


def deliver(callback, *args):
    def invoke():
        callback(*args)
        return False
    GLib.idle_add(invoke)


class Settings:
    KEYS = {"phone", "manual_phone", "receive_dir", "clipboard", "notifications", "theme", "window_width", "window_height", "list_mode", "remote_country"}

    def __init__(self, path=None):
        self.path = Path(path) if path else Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home()/".config"))) / "vivo-office-kit/preferences.json"
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
        self.pool = concurrent.futures.ThreadPoolExecutor(max_workers=8, thread_name_prefix="vivo-action")
        self.slots = threading.BoundedSemaphore(16)
        self.handles = set()
        self.lock = threading.Lock()
        self.closed = False
        self.pair_process = None

    @property
    def owned(self):
        return bool(self.pair_process and self.pair_process.active)

    def _spawn(self, handle, arguments):
        process = subprocess.Popen([self.cli_path, *arguments], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        handle.process = process
        if handle.cancelled.is_set() or self.closed: handle.terminate()
        with self.lock: self.handles.add(handle)
        return process

    @staticmethod
    def _read(handle, timeout, limit, event=None):
        process = handle.process
        result, pending = bytearray(), bytearray()
        deadline = time.monotonic()+timeout
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                if handle.cancelled.is_set(): raise RuntimeError("Action cancelled.")
                if time.monotonic() > deadline: raise RuntimeError("Phone action timed out.")
                ready = selector.select(0.2)
                if not ready: continue
                chunk = os.read(process.stdout.fileno(), 8192)
                if not chunk: break
                if event is None:
                    if len(result)+len(chunk)>limit: raise RuntimeError("Response exceeded its safety limit.")
                    result.extend(chunk)
                else:
                    pending.extend(chunk)
                    while b"\n" in pending:
                        line, _, rest = pending.partition(b"\n")
                        pending = bytearray(rest)
                        if len(line)>limit: raise RuntimeError("Event exceeded its safety limit.")
                        value = json.loads(line)
                        if not isinstance(value,dict): raise RuntimeError("Invalid connection event.")
                        deliver(event, value)
                    if len(pending)>limit: raise RuntimeError("Event exceeded its safety limit.")
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
            deliver(done, None, "Action queue is busy. Try again.")
            return handle
        def work():
            value, error = None, None
            try:
                process = self._spawn(handle, list(args))
                timeout = 4 if args and args[0]=="native-status" else 600 if args and args[0] in ("preview","download","upload") else 90
                output = self._read(handle, timeout, JSON_LIMIT)
                code = process.wait(timeout=2)
                value = json.loads(output)
                if not isinstance(value,dict): raise ValueError()
                if code:
                    error = "Approve file access on your phone." if value.get("error")=="file_permission_required" else "Phone action failed. Check connection and access."
            except RuntimeError as exception:
                error = str(exception)
            except (OSError, ValueError, subprocess.SubprocessError):
                error = "Native session unavailable. Connect your phone or reconnect the session."
            finally:
                self._finish(handle)
                self.slots.release()
                deliver(done, value, error)
        self.pool.submit(work)
        return handle

    def pair(self, phone, trust, receive_dir, clipboard, notifications, on_event, on_exit):
        if self.owned: return self.pair_process
        handle = ProcessHandle()
        self.pair_process = handle
        args = ["pair","--persistent","--receive-dir",str(receive_dir),"--state-dir",str(Path(os.environ.get("XDG_DATA_HOME",str(Path.home()/".local/share")))/"vivo-office-kit/pairing")]
        if phone: args.extend(["--phone", phone])
        if trust: args.append("--trust-scanned-phone")
        if clipboard: args.append("--clipboard")
        if notifications: args.append("--notifications")
        def work():
            code = 1
            try:
                process = self._spawn(handle, args)
                self._read(handle, 365*24*3600, EVENT_LIMIT, on_event)
                code = process.wait(timeout=2)
            except (OSError, RuntimeError, ValueError, subprocess.SubprocessError):
                pass
            finally:
                self._finish(handle)
                deliver(on_exit, code)
        threading.Thread(target=work, daemon=True, name="vivo-pair").start()
        return handle

    def disconnect(self):
        if self.pair_process: self.pair_process.terminate()

    def close(self):
        self.closed = True
        self.disconnect()
        with self.lock: handles = list(self.handles)
        for handle in handles: handle.terminate()
        self.pool.shutdown(wait=False, cancel_futures=True)
