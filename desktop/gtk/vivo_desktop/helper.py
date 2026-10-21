"""Private JSON-pipe clipboard and notification helper; never logs contents."""
import json
import os
import signal
import selectors
import shutil
import subprocess
import sys
import threading

import gi
gi.require_version("Gio", "2.0")
from gi.repository import Gio, GLib

TEXT_LIMIT = 64 * 1024
LINE_LIMIT = 2 * 1024 * 1024
MESSAGES = {"connected":"Phone connected.", "disconnected":"Phone disconnected.", "connecting":"Connecting to phone.", "files_received":"Files received from your phone."}


def valid_text(value):
    try: return isinstance(value,str) and bool(value) and len(value.encode("utf-8")) <= TEXT_LIMIT and "\x00" not in value
    except UnicodeError: return False


class Helper:
    def __init__(self, clipboard_enabled):
        self.loop = GLib.MainLoop()
        self.clipboard = None
        self.bus = None
        self.last_text = None
        self.read_cancel = None
        self.reader_generation = 0
        self.stopping = False
        self.observer = None
        self.observer_ready = False
        if clipboard_enabled:
            gi.require_version("Gtk", "4.0")
            from gi.repository import Gdk, Gtk
            Gtk.init()
            display = Gdk.Display.get_default()
            if display is None: raise RuntimeError("Desktop display unavailable.")
            self.clipboard = display.get_clipboard()
            self.clipboard.connect("changed", self.clipboard_changed)
            if os.environ.get("WAYLAND_DISPLAY") and shutil.which("wl-paste"):
                self.observer = subprocess.Popen([os.path.realpath(sys.argv[0]),"--watch-clipboard"],stdout=subprocess.PIPE,stdin=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
                def watch():
                    while not self.stopping:
                        line=self.observer.stdout.readline(LINE_LIMIT+1)
                        if not line or len(line)>LINE_LIMIT: return
                        try: value=json.loads(line)
                        except (ValueError,UnicodeError): return
                        GLib.idle_add(self.observed,value.get("text",""))
                threading.Thread(target=watch,daemon=True,name="vivo-clipboard-watch").start()
        Gio.bus_get(Gio.BusType.SESSION, None, self.bus_ready)

    def emit(self, value):
        if self.stopping: return
        try:
            sys.stdout.write(json.dumps(value, ensure_ascii=True, separators=(",",":"))+"\n")
            sys.stdout.flush()
        except (OSError, ValueError):
            self.stop()

    def stop(self):
        self.stopping = True
        if self.read_cancel: self.read_cancel.cancel()
        if self.observer and self.observer.poll() is None:
            self.observer.terminate()
        self.loop.quit()
        return False

    def observed(self, text):
        if self.stopping: return False
        if not self.observer_ready:
            self.observer_ready=True
            self.last_text=text
        elif valid_text(text) and text!=self.last_text:
            self.last_text=text
            self.emit({"event":"text","text":text})
        return False

    def bus_ready(self, _source, result):
        try: self.bus = Gio.bus_get_finish(result)
        except GLib.Error: self.bus = None

    def acknowledge(self, kind, ok, request):
        value = {"event":kind, "ok":bool(ok)}
        if "id" in request: value["id"] = request["id"]
        self.emit(value)

    def request(self, line):
        if self.stopping: return False
        try:
            value = json.loads(line)
            if not isinstance(value,dict): raise ValueError()
            if value.get("op")=="set_text":
                text = value.get("text")
                ok = self.clipboard is not None and valid_text(text)
                if ok:
                    self.clipboard.set(text)
                self.acknowledge("set",ok,value)
            elif value.get("op")=="notify":
                state = value.get("state")
                if not self.bus or state not in MESSAGES:
                    self.acknowledge("notification",False,value)
                else:
                    parameters = GLib.Variant("(susssasa{sv}i)",("VOkit",0,"phone-symbolic","VOkit",MESSAGES[state],[],{"urgency":GLib.Variant("y",1)},4000))
                    def completed(connection,result):
                        try: connection.call_finish(result); ok=True
                        except GLib.Error: ok=False
                        self.acknowledge("notification",ok,value)
                    self.bus.call("org.freedesktop.Notifications","/org/freedesktop/Notifications","org.freedesktop.Notifications","Notify",parameters,GLib.VariantType.new("(u)"),Gio.DBusCallFlags.NONE,3000,None,completed)
            else:
                self.emit({"event":"error","error":"invalid_request"})
        except (ValueError, UnicodeError, TypeError, GLib.Error):
            self.emit({"event":"error","error":"invalid_request"})
        return False

    def clipboard_changed(self, clipboard):
        self.reader_generation += 1
        generation = self.reader_generation
        if self.read_cancel: self.read_cancel.cancel()
        cancel = self.read_cancel = Gio.Cancellable()
        mime_types = clipboard.get_formats().get_mime_types()
        if any(mime.startswith("image/") for mime in mime_types): return
        if not any(mime.startswith("text/plain") or mime=="UTF8_STRING" for mime in mime_types): return
        def opened(source,result):
            try: stream,_mime = source.read_finish(result)
            except GLib.Error: return
            data = bytearray()
            def read_more():
                stream.read_bytes_async(min(8192,TEXT_LIMIT+1-len(data)),GLib.PRIORITY_DEFAULT,cancel,received)
            def received(source,result):
                try: chunk = source.read_bytes_finish(result).get_data()
                except GLib.Error: return
                data.extend(chunk)
                if generation!=self.reader_generation or len(data)>TEXT_LIMIT:
                    cancel.cancel()
                    stream.close_async(GLib.PRIORITY_DEFAULT,None,None)
                    return
                if chunk:
                    read_more()
                    return
                stream.close_async(GLib.PRIORITY_DEFAULT,None,None)
                try: text=data.decode("utf-8")
                except UnicodeError: return
                if valid_text(text) and text!=self.last_text:
                    self.last_text=text
                    self.emit({"event":"text","text":text})
            read_more()
        clipboard.read_async(["text/plain;charset=utf-8","UTF8_STRING","text/plain"],GLib.PRIORITY_DEFAULT,cancel,opened)

    def read_stdin(self):
        while not self.stopping:
            line = sys.stdin.buffer.readline(LINE_LIMIT+1)
            if not line:
                GLib.idle_add(self.stop)
                return
            if len(line)>LINE_LIMIT:
                while line and not line.endswith(b"\n"): line=sys.stdin.buffer.readline(LINE_LIMIT+1)
                GLib.idle_add(self.emit,{"event":"error","error":"line_too_large"})
                continue
            GLib.idle_add(self.request,line)

    def run(self):
        self.emit({"event":"ready"})
        threading.Thread(target=self.read_stdin,daemon=True,name="vivo-helper-input").start()
        GLib.unix_signal_add(GLib.PRIORITY_DEFAULT,signal.SIGTERM,self.stop)
        GLib.unix_signal_add(GLib.PRIORITY_DEFAULT,signal.SIGINT,self.stop)
        self.loop.run()
        return 0


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    if args==["--watch-clipboard"]:
        # Keep Wayland data-control observation tied to the helper's lifetime.
        import ctypes
        ctypes.CDLL(None).prctl(1,signal.SIGTERM,0,0,0)
        if os.getppid()==1: return 1
        os.execvp("wl-paste",["wl-paste","--type","text","--watch",os.path.realpath(sys.argv[0]),"--clipboard-event"])
    if args==["--clipboard-event"]:
        data=bytearray()
        if os.environ.get("CLIPBOARD_STATE")=="data":
            with selectors.DefaultSelector() as selector:
                selector.register(sys.stdin,selectors.EVENT_READ)
                while len(data)<=TEXT_LIMIT and selector.select(2):
                    chunk=os.read(sys.stdin.fileno(),min(4096,TEXT_LIMIT+1-len(data)))
                    if not chunk: break
                    data.extend(chunk)
        try: text=data.decode("utf-8")
        except UnicodeError: text=""
        print(json.dumps({"text":text if valid_text(text) else ""},ensure_ascii=True))
        return 0
    if args not in ([],["--notifications-only"]): return 2
    try: return Helper(not args).run()
    except (RuntimeError, GLib.Error): return 1


if __name__=="__main__":
    raise SystemExit(main())
