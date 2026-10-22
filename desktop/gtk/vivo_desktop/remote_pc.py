"""Remote PC window and its owned native session. Credentials stay in memory."""
import json
import os
from pathlib import Path
import selectors
import subprocess
import threading

import gi
gi.require_version('Gtk', '4.0')
gi.require_version('Adw', '1')
from gi.repository import Adw, GLib, Gtk


class RemotePCProcess:
    def __init__(self, cli, country, event):
        self.cli, self.country, self.event = cli, country, event
        self.process = None
        self.closed = threading.Event()
        self.lock = threading.Lock()
        self.thread = threading.Thread(target=self._run, daemon=True, name='vivo-remote-pc')
        self.thread.start()

    def _emit(self, value):
        def received():
            if not self.closed.is_set():
                self.event(value)
            return GLib.SOURCE_REMOVE
        GLib.idle_add(received)

    def _run(self):
        directory = Path(os.environ.get('XDG_DATA_HOME', str(Path.home() / '.local/share'))) / 'vivo-office-kit/remote-pc' / self.country.lower()
        helper = Path(self.cli).resolve().with_name('vivo-desktop-helper')
        try:
            process = subprocess.Popen([self.cli, 'remote-pc', '--state-dir', str(directory), '--desktop-helper', str(helper), '--country', self.country],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            with self.lock:
                self.process = process
            if self.closed.is_set():
                process.terminate()
            pending = bytearray()
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while not self.closed.is_set():
                    if not selector.select(.2):
                        continue
                    chunk = os.read(process.stdout.fileno(), 8192)
                    if not chunk:
                        break
                    pending.extend(chunk)
                    if len(pending) > 1024 * 1024:
                        raise ValueError('Oversized Remote PC event')
                    while b'\n' in pending:
                        line, _, remaining = pending.partition(b'\n')
                        pending = bytearray(remaining)
                        value = json.loads(line)
                        if not isinstance(value, dict):
                            raise ValueError('Invalid Remote PC event')
                        self._emit(value)
            if not self.closed.is_set():
                self._emit({'event': 'error', 'error': 'Remote PC stopped. Enable access to reconnect.'})
        except (OSError, ValueError, UnicodeError):
            self._emit({'event': 'error', 'error': 'Cannot start Remote PC. Check the desktop installation.'})
        finally:
            self._finish()

    def send(self, operation):
        with self.lock:
            if self.closed.is_set() or not self.process or self.process.poll() is not None:
                return False
            try:
                self.process.stdin.write((json.dumps({'op': operation}) + '\n').encode('utf-8'))
                self.process.stdin.flush()
                return True
            except OSError:
                return False

    def _finish(self):
        process = self.process
        if not process:
            return
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        process.stdin.close()
        process.stdout.close()

    def stop(self):
        self.send('stop')
        self.closed.set()


class RemotePCWindow(Adw.ApplicationWindow):
    def __init__(self, app, backend, preferences):
        super().__init__(application=app, title='Remote PC', default_width=730, default_height=570)
        self.backend, self.preferences = backend, preferences
        self.session = None
        self.generation = 0
        self.country = preferences.get('remote_country', 'IN')
        if not isinstance(self.country, str) or len(self.country) != 2 or not self.country.isascii() or not self.country.isalpha():
            self.country = 'IN'
        self.country = self.country.upper()
        self.connect('close-request', self._close)
        toolbar = Adw.ToolbarView()
        header = Adw.HeaderBar()
        header.set_title_widget(Gtk.Label(label='Remote PC'))
        settings = Gtk.Button(icon_name='preferences-system-symbolic', tooltip_text='Remote PC settings')
        settings.connect('clicked', self._settings)
        header.pack_end(settings)
        toolbar.add_top_bar(header)
        page = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=24)
        for edge in ('top', 'bottom', 'start', 'end'):
            getattr(page, 'set_margin_' + edge)(24)
        self.status = Gtk.Label(label='Remote access is off', xalign=0, wrap=True)
        self.status.add_css_class('remote-pc-status')
        page.append(self.status)
        title = Gtk.Label(label='View and control this PC from your phone', xalign=0, wrap=True)
        title.add_css_class('title-2')
        page.append(title)
        description = Gtk.Label(label='Experimental: use the same Wi-Fi network. Open Remote PC in Office Kit on your phone and enter the device code and temporary password.', xalign=0, wrap=True)
        description.add_css_class('dim-label')
        page.append(description)
        credentials = Gtk.Grid(column_spacing=24, row_spacing=14, halign=Gtk.Align.CENTER, vexpand=True, valign=Gtk.Align.CENTER)
        credentials.attach(Gtk.Label(label='Device code', xalign=0, css_classes=['dim-label']), 0, 0, 2, 1)
        self.code = Gtk.Label(label='—', xalign=0, selectable=True, css_classes=['title-1', 'monospace'])
        credentials.attach(self.code, 0, 1, 1, 1)
        self.copy = Gtk.Button(label='Copy ID', sensitive=False)
        self.copy.set_size_request(150, -1)
        self.copy.connect('clicked', lambda *_: self.get_clipboard().set(self.code.get_text().replace(' ', '')))
        credentials.attach(self.copy, 1, 1, 1, 1)
        credentials.attach(Gtk.Label(label='Temporary password', xalign=0, css_classes=['dim-label']), 0, 2, 2, 1)
        self.password = Gtk.PasswordEntry(show_peek_icon=True, editable=False, sensitive=False)
        self.password.update_property([Gtk.AccessibleProperty.LABEL], ['Temporary password'])
        credentials.attach(self.password, 0, 3, 1, 1)
        self.rotate = Gtk.Button(label='Update password', sensitive=False)
        self.rotate.connect('clicked', self._rotate)
        credentials.attach(self.rotate, 1, 3, 1, 1)
        page.append(credentials)
        self.access = Gtk.Button(label='Enable remote access', halign=Gtk.Align.CENTER)
        self.access.add_css_class('suggested-action')
        self.access.connect('clicked', self._toggle)
        page.append(self.access)
        self.consent = Gtk.Label(label='Choose a screen to share. The connected phone can use your keyboard and mouse until you stop access or close this window.', wrap=True, xalign=0)
        self.consent.add_css_class('dim-label')
        page.append(self.consent)
        scroll = Gtk.ScrolledWindow(hscrollbar_policy=Gtk.PolicyType.NEVER)
        scroll.set_child(page)
        toolbar.set_content(scroll)
        self.set_content(toolbar)

    def _toggle(self, *_args):
        if self.session:
            self.stop()
            return
        self.generation += 1
        generation = self.generation
        self.status.set_text('Starting screen sharing…')
        self.access.set_label('Stop remote access')
        self.session = RemotePCProcess(self.backend.cli_path, self.country,
                                       lambda event: self._event(event) if generation == self.generation else None)

    def _event(self, event):
        kind = event.get('event')
        if kind == 'credentials':
            code = event.get('device_code', '')
            password = event.get('temporary_password', '')
            if not isinstance(code, str) or not code.isascii() or not code.isdigit() or not 6 <= len(code) <= 20 or not isinstance(password, str) or not 1 <= len(password) <= 256:
                return
            groups = []
            while code:
                groups.insert(0, code[-3:])
                code = code[:-3]
            self.code.set_text(' '.join(groups))
            self.password.set_text(password)
            self.password.set_sensitive(True)
            self.copy.set_sensitive(True)
            self.rotate.set_sensitive(True)
        elif kind == 'status':
            message = event.get('message')
            if isinstance(message, str) and len(message) <= 1024:
                self.status.set_text(message)
        elif kind == 'connected':
            message = event.get('message', 'Phone connected')
            self.status.set_text(message if isinstance(message, str) and len(message) <= 1024 else 'Phone connected')
            self.rotate.set_sensitive(False)
        elif kind == 'disconnected':
            self.status.set_text('Waiting for your phone')
            self.rotate.set_sensitive(bool(self.password.get_text()))
        elif kind == 'error':
            message = event.get('error', 'Remote PC could not connect.')
            self.stop()
            self.status.set_text(message if isinstance(message, str) and len(message) <= 1024 else 'Remote PC could not connect.')

    def _rotate(self, *_args):
        if self.session and self.session.send('rotate_password'):
            self.rotate.set_sensitive(False)
            self.status.set_text('Updating temporary password…')

    def _settings(self, *_args):
        dialog = Adw.Dialog(title='Remote PC settings', content_width=360)
        content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
        for edge in ('top', 'bottom', 'start', 'end'):
            getattr(content, 'set_margin_' + edge)(24)
        content.append(Gtk.Label(label='Country code', xalign=0))
        country = Gtk.Entry(text=self.country, max_length=2, sensitive=self.session is None)
        country.update_property([Gtk.AccessibleProperty.LABEL], ['Two-letter country code'])
        content.append(country)
        save = Gtk.Button(label='Done')
        def completed(*_):
            value = country.get_text().upper()
            if len(value) == 2 and value.isascii() and value.isalpha() and self.session is None:
                self.country = value
                self.preferences.set('remote_country', value)
                self.preferences.save()
            dialog.close()
        save.connect('clicked', completed)
        content.append(save)
        dialog.set_child(content)
        dialog.present(self)

    def stop(self):
        self.generation += 1
        if self.session:
            self.session.stop()
            self.session = None
        self.password.set_text('')
        self.password.set_sensitive(False)
        self.code.set_text('—')
        self.copy.set_sensitive(False)
        self.rotate.set_sensitive(False)
        self.access.set_label('Enable remote access')
        self.status.set_text('Remote access is off')

    def _close(self, *_args):
        self.stop()
        self.set_visible(False)
        return True
