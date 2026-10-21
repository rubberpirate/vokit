"""Native GTK media surfaces and the bounded private media socket transport."""
import collections
import fcntl
import json
import os
import queue
import re
import select
import shutil
import socket
import struct
import subprocess
import threading
import time
import uuid
from pathlib import Path

import gi

gi.require_version('Gtk', '4.0')
gi.require_version('Adw', '1')
gi.require_version('Gst', '1.0')
gi.require_version('GstVideo', '1.0')
from gi.repository import Adw, Gdk, Gio, GLib, Gst, GstVideo, Gtk

Gst.init(None)
RECORD_LIMIT = 16 * 1024 * 1024


class FramedRecords:
    def __init__(self):
        self.buffer = bytearray()

    def feed(self, data):
        self.buffer.extend(data)
        records = []
        while len(self.buffer) >= 5:
            kind, size = struct.unpack_from('!BI', self.buffer)
            limit = {1: 65536, 2: RECORD_LIMIT, 3: 65536, 4: 524288}.get(kind, -1)
            if size > limit:
                raise ValueError('Invalid phone media response.')
            if len(self.buffer) < size + 5:
                break
            records.append((kind, bytes(self.buffer[5:5 + size])))
            del self.buffer[:5 + size]
        return records


def picture_point(x, y, area_width, area_height, width, height, clamp=False):
    if min(area_width, area_height, width, height) <= 0:
        return None
    scale = min(area_width / width, area_height / height)
    left, top = (area_width - width * scale) / 2, (area_height - height * scale) / 2
    x, y = (x - left) / scale, (y - top) / scale
    if not clamp and not (0 <= x < width and 0 <= y < height):
        return None
    return min(width - 1, max(0, int(x + 1e-7))), min(height - 1, max(0, int(y + 1e-7)))


def _push(source, data):
    buffer = Gst.Buffer.new_allocate(None, len(data), None)
    buffer.fill(0, data)
    result = source.emit('push-buffer', buffer)
    if result not in (Gst.FlowReturn.OK, Gst.FlowReturn.FLUSHING):
        raise RuntimeError('Cannot decode phone media.')


def _appsrc(source, caps):
    source.set_property('caps', Gst.Caps.from_string(caps))
    source.set_property('is-live', True)
    source.set_property('do-timestamp', True)
    source.set_property('format', Gst.Format.TIME)
    source.set_property('max-bytes', 4 * 1024 * 1024)
    source.set_property('max-buffers', 8)
    # Preserve compressed reference pictures; overload stops rather than corrupts the stream.
    source.set_property('block', False)


class VideoPipeline:
    def __init__(self, picture, status, geometry_changed=None, force_fallback=False):
        self.picture, self.status = picture, status
        self.geometry_changed = geometry_changed
        self.geometry = (0, 0)
        self.decoded_frames = 0
        self.closed = False
        self.pending = None
        self.pending_idle = False
        self.lock = threading.Lock()
        self.webcam = None
        self.webcam_pad = None
        self.native_sink = None if force_fallback else Gst.ElementFactory.find('gtk4paintablesink')
        sink_description = 'gtk4paintablesink name=preview sync=false async=false' if self.native_sink else 'appsink name=preview sync=false async=false emit-signals=true max-buffers=1 drop=true caps=video/x-raw,format=RGB'
        self.pipeline = Gst.parse_launch(
            'appsrc name=source ! h264parse config-interval=-1 ! avdec_h264 max-threads=2 ! '
            'tee name=decoded decoded. ! queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream ! '
            'valve name=visible drop-mode=transform-to-gap ! videoconvert ! ' + sink_description)
        self.source = self.pipeline.get_by_name('source')
        _appsrc(self.source, 'video/x-h264,stream-format=byte-stream,alignment=au')
        self.tee = self.pipeline.get_by_name('decoded')
        self.valve = self.pipeline.get_by_name('visible')
        self.sink = self.pipeline.get_by_name('preview')
        if self.native_sink:
            self.picture.set_paintable(self.sink.get_property('paintable'))
        else:
            self.sink.connect('new-sample', self._sample)
        self.tee.get_static_pad('sink').add_probe(Gst.PadProbeType.EVENT_DOWNSTREAM, self._caps)
        self.tee.get_static_pad('sink').add_probe(Gst.PadProbeType.BUFFER, self._decoded)
        self.bus = self.pipeline.get_bus()
        self.bus.add_signal_watch()
        self.bus_handler = self.bus.connect('message::error', self._error)
        self.pipeline.set_state(Gst.State.PLAYING)

    def _decoded(self, pad, probe):
        self.decoded_frames += 1
        return Gst.PadProbeReturn.OK

    def _caps(self, pad, probe):
        event = probe.get_event()
        if event.type == Gst.EventType.CAPS:
            caps = event.parse_caps().get_structure(0)
            width, height = caps.get_value('width'), caps.get_value('height')
            if width <= 0 or height <= 0 or width > 4096 or height > 4096 or width * height > 16 * 1024 * 1024:
                GLib.idle_add(self.status, 'Phone video dimensions are unsupported.')
                return Gst.PadProbeReturn.DROP
            self.geometry = (width, height)
            if self.geometry_changed:
                GLib.idle_add(self._geometry_idle, width, height)
        return Gst.PadProbeReturn.OK

    def _geometry_idle(self, width, height):
        if not self.closed:
            self.geometry_changed(width, height)
        return False

    def _sample(self, sink):
        sample = sink.emit('pull-sample')
        if not sample or self.closed:
            return Gst.FlowReturn.OK
        info = GstVideo.VideoInfo.new_from_caps(sample.get_caps())
        if not info or info.width > 4096 or info.height > 4096:
            return Gst.FlowReturn.ERROR
        buffer = sample.get_buffer()
        ok, mapped = buffer.map(Gst.MapFlags.READ)
        if ok:
            data = bytes(mapped.data)
            buffer.unmap(mapped)
            with self.lock:
                self.pending = (info.width, info.height, info.stride[0], data)
                if not self.pending_idle:
                    self.pending_idle = True
                    GLib.idle_add(self._present)
        return Gst.FlowReturn.OK

    def _present(self):
        with self.lock:
            frame, self.pending = self.pending, None
            self.pending_idle = False
        if frame and not self.closed:
            width, height, stride, data = frame
            texture = Gdk.MemoryTexture.new(width, height, Gdk.MemoryFormat.R8G8B8, GLib.Bytes.new(data), stride)
            self.picture.set_paintable(texture)
        return False

    def _error(self, bus, message):
        if self.closed:
            return
        if self.webcam and (message.src == self.webcam or message.src.has_as_ancestor(self.webcam)):
            self.set_webcam('')
            self.status('Camera live · webcam device unavailable')
        else:
            self.status('Cannot decode phone video. Stop and retry.')

    def push(self, body):
        if self.source.get_property('current-level-bytes') > 8 * 1024 * 1024:
            raise RuntimeError('Phone video decoder is overloaded. Stop and retry.')
        _push(self.source, body)

    def set_preview(self, visible):
        self.valve.set_property('drop', not visible)
        self.picture.set_visible(visible)

    def set_webcam(self, path):
        if self.webcam:
            self.webcam.set_state(Gst.State.NULL)
            self.tee.release_request_pad(self.webcam_pad)
            self.pipeline.remove(self.webcam)
            self.webcam, self.webcam_pad = None, None
        if not path:
            return
        if not re.fullmatch(r'/dev/video[0-9]{1,3}', path):
            raise ValueError('Choose a /dev/video webcam loopback device.')
        fd = os.open(path, os.O_WRONLY | os.O_NONBLOCK | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            capability = bytearray(104)
            fcntl.ioctl(fd, 0x80685600, capability)
            if bytes(capability[:16]).split(b'\0')[0] not in (b'v4l2loopback', b'v4l2 loopback'):
                raise ValueError('The webcam device is not a virtual loopback camera.')
        finally:
            os.close(fd)
        branch = Gst.parse_bin_from_description(
            'queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream ! '
            'videoconvert ! videoscale ! video/x-raw,format=YUY2,width=1280,height=720 ! '
            'v4l2sink name=webcam sync=false async=false', True)
        branch.get_by_name('webcam').set_property('device', path)
        self.pipeline.add(branch)
        pad = self.tee.request_pad_simple('src_%u')
        if pad.link(branch.get_static_pad('sink')) != Gst.PadLinkReturn.OK:
            self.tee.release_request_pad(pad)
            self.pipeline.remove(branch)
            raise RuntimeError('Cannot connect the virtual webcam.')
        self.webcam, self.webcam_pad = branch, pad
        branch.sync_state_with_parent()

    def close(self):
        self.closed = True
        self.pipeline.set_state(Gst.State.NULL)
        self.bus.disconnect(self.bus_handler)
        self.bus.remove_signal_watch()
        with self.lock:
            self.pending = None


class PhoneAudio:
    """Publish a virtual source only after the phone yields valid decoded PCM."""
    def __init__(self):
        self.closed = threading.Event()
        self.pcm = queue.Queue(maxsize=4)
        self.processes = []
        self.status = 'Waiting for phone microphone audio…'
        self.active = False
        self.pipeline = Gst.parse_launch(
            'appsrc name=source ! aacparse ! avdec_aac ! audioconvert ! audioresample ! '
            'audio/x-raw,format=S16LE,rate=48000,channels=1,layout=interleaved ! '
            'appsink name=pcm emit-signals=true sync=false max-buffers=4 drop=true')
        self.source = self.pipeline.get_by_name('source')
        _appsrc(self.source, 'audio/mpeg,mpegversion=4,stream-format=adts')
        self.pipeline.get_by_name('pcm').connect('new-sample', self._sample)
        self.pipeline.set_state(Gst.State.PLAYING)
        self.thread = threading.Thread(target=self._publish, daemon=True)
        self.thread.start()

    def _sample(self, sink):
        sample = sink.emit('pull-sample')
        if sample and not self.closed.is_set():
            data = sample.get_buffer().extract_dup(0, sample.get_buffer().get_size())
            if data and len(data) <= 65536:
                try:
                    self.pcm.put_nowait(data)
                except queue.Full:
                    pass
        return Gst.FlowReturn.OK

    def push(self, data):
        # Each private record contains complete ADTS packets, never arbitrary AAC bytes.
        offset = 0
        while offset < len(data):
            part = data[offset:]
            if len(part) < 7 or part[0] != 255 or part[1] & 0xf6 != 0xf0 or (part[2] >> 2) & 15 >= 13:
                raise ValueError('Phone microphone audio is unavailable.')
            size = ((part[3] & 3) << 11) | (part[4] << 3) | (part[5] >> 5)
            if size <= (7 if part[1] & 1 else 9) or size > len(part):
                raise ValueError('Phone microphone audio is unavailable.')
            offset += size
        if data:
            _push(self.source, data)

    def _publish(self):
        try:
            while not self.closed.is_set():
                try:
                    data = self.pcm.get(timeout=.1)
                except queue.Empty:
                    continue
                if not self.processes:
                    loop, cat = shutil.which('pw-loopback'), shutil.which('pw-cat')
                    if not loop or not cat:
                        raise RuntimeError('Install PipeWire tools to share phone microphone audio.')
                    name = 'vivo-phone-' + uuid.uuid4().hex
                    process = subprocess.Popen([loop, '-n', name, '-c', '1', '-m', 'MONO', '-l', '10',
                        '-i', '{ node.name = ' + name + '-input node.description = "Vivo Phone Microphone Input" media.class = Audio/Sink node.virtual = true node.autoconnect = false }',
                        '-o', '{ node.name = ' + name + '-source node.description = "Vivo Phone Microphone" media.class = Audio/Source node.virtual = true node.autoconnect = false }'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    self.processes.append(process)
                    if self.closed.wait(.2) or process.poll() is not None:
                        raise RuntimeError('Cannot create the PipeWire microphone source.')
                    feed = subprocess.Popen([cat, '--playback', '--raw', '--target', name + '-input', '--rate', '48000', '--channels', '1', '--format', 's16', '--latency', '20ms', '-'], stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, bufsize=0)
                    self.processes.append(feed)
                    os.set_blocking(feed.stdin.fileno(), False)
                    self.active = True
                    self.status = 'Microphone: Vivo Phone Microphone'
                feed = self.processes[-1]
                if feed.poll() is not None:
                    raise RuntimeError('Phone microphone audio is unavailable.')
                if select.select([], [feed.stdin], [], .02)[1]:
                    # Keep latency bounded; discard unwritten audio rather than queueing seconds.
                    os.write(feed.stdin.fileno(), data)
        except (OSError, RuntimeError) as error:
            self.status = str(error)
        finally:
            self.active = False
            for process in reversed(self.processes):
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=.5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=.5)

    def close(self):
        self.closed.set()
        self.pipeline.set_state(Gst.State.NULL)


class MediaSession:
    def __init__(self, backend, decoder, status, mode='screen', facing='rear', quality='720p', microphone=False):
        self.backend, self.decoder, self.status_callback = backend, decoder, status
        self.mode, self.quality = mode, quality
        self.init = {'source': mode}
        if mode == 'camera':
            self.init.update(facing=facing, quality=quality)
            if microphone:
                self.init['microphone'] = True
        self.microphone = microphone
        self.closed = threading.Event()
        self.lock = threading.Lock()
        self.outgoing = collections.deque()
        self.sock = None
        self.message = None
        self.audio = None
        self.started = time.monotonic()
        self.error = False

    def start(self):
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def send(self, event):
        if self.closed.is_set():
            return
        data = json.dumps(event, ensure_ascii=False, separators=(',', ':')).encode() + b'\n'
        if len(data) > 8192:
            return
        move = event.get('kind') == 'pointer' and event.get('action') == 'move'
        with self.lock:
            if move and self.outgoing and self.outgoing[-1][1]:
                self.outgoing[-1] = (data, True)
            elif len(self.outgoing) < 64:
                self.outgoing.append((data, move))
            else:
                self.closed.set()
                self._status('Phone input is overloaded. Stop and retry.', True)

    def _status(self, text, error=False):
        if self.error and not error:
            return
        self.error = self.error or error
        if text != self.message:
            self.message = text
            GLib.idle_add(self._status_idle, text)

    def _status_idle(self, text):
        self.status_callback(text)
        return False

    def _live_status(self):
        text = 'Live'
        if self.microphone:
            text += ' · ' + (self.audio.status if self.audio else 'Phone microphone unavailable on this connection.')
            if self.audio and not self.audio.active and time.monotonic() - self.started > 10:
                text = 'Live · No microphone audio received from the phone. Webcam sharing continues.'
        self._status(text)

    def _run(self):
        try:
            if self.microphone:
                self.audio = PhoneAudio()
            sock = socket.socket(socket.AF_UNIX)
            self.sock = sock
            sock.settimeout(3)
            sock.connect(str(self.backend.media_socket))
            sock.sendall(json.dumps(self.init, separators=(',', ':')).encode() + b'\n')
            sock.settimeout(.05)
            self._status('Waiting for phone permission and video…')
            parser = FramedRecords()
            quality_sent = False
            last_data = time.monotonic()
            video = False
            while not self.closed.is_set():
                with self.lock:
                    outgoing = list(self.outgoing)
                    self.outgoing.clear()
                for data, _ in outgoing:
                    sock.sendall(data)
                try:
                    data = sock.recv(65536)
                except socket.timeout:
                    if video:
                        self._live_status()
                    if time.monotonic() - last_data > 60:
                        raise RuntimeError('Phone video timed out. Stop and retry.')
                    continue
                if not data:
                    break
                last_data = time.monotonic()
                for kind, body in parser.feed(data):
                    if kind == 2:
                        self.decoder.push(body)
                        video = True
                        self._live_status()
                    elif kind == 4:
                        if self.audio:
                            try:
                                self.audio.push(body)
                            except ValueError:
                                self.audio.close()
                                self.audio = None
                    else:
                        info = json.loads(body)
                        if not isinstance(info, dict):
                            raise ValueError('Invalid phone media response.')
                        if kind == 1:
                            if info.get('microphone_available') is False and self.audio:
                                self.audio.close()
                                self.audio = None
                            if not quality_sent and self.mode == 'screen' and info.get('video_width', 0) > 0 and info.get('video_height', 0) > 0:
                                self.send({'kind':'quality', 'quality':self.quality})
                                quality_sent = True
                        if info.get('error'):
                            self._status(str(info['error'])[:240], True)
                        elif 'denied' in str(info.get('stage', '')):
                            self._status('Phone permission denied. Approve it on the phone and retry.', True)
                        elif any(word in str(info.get('stage', '')) for word in ('error', 'failed')):
                            self._status('Phone media stopped. Check the phone and retry.', True)
                        elif info.get('stage') == 'permission_wait':
                            self._status('Approve screen or camera access on the phone.')
                        elif info.get('stage') == 'starting':
                            self._status('Starting phone video…')
            self._status('Preview stopped.' if self.closed.is_set() else 'Phone media disconnected.')
        except (OSError, ValueError, RuntimeError, GLib.Error) as error:
            if not self.closed.is_set():
                self._status(str(error)[:240] if isinstance(error, (ValueError, RuntimeError)) else 'Phone media service unavailable. Reconnect and retry.', True)
        finally:
            self.closed.set()
            if self.sock:
                self.sock.close()
            if self.audio:
                self.audio.close()
            self.decoder.close()

    def stop(self):
        self.closed.set()
        if self.sock:
            try:
                self.sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def _button(icon, label, callback):
    button = Gtk.Button.new_from_icon_name(icon)
    button.set_tooltip_text(label)
    button.update_property([Gtk.AccessibleProperty.LABEL], [label])
    button.add_css_class('flat')
    button.connect('clicked', callback)
    return button


class MediaPanel(Gtk.Box):
    def __init__(self, backend, mode='camera'):
        super().__init__(orientation=Gtk.Orientation.VERTICAL, spacing=10)
        self.backend, self.mode = backend, mode
        self.session = None
        self.decoder = None
        self.connected = False
        self.pressed = False
        self.point = None
        self.origin = (0, 0)
        self.screen_quality = '720p'
        self.picture = Gtk.Picture(can_shrink=True, content_fit=Gtk.ContentFit.CONTAIN, hexpand=True, vexpand=True)
        self.picture.set_focusable(mode == 'screen')
        self.picture.update_property([Gtk.AccessibleProperty.LABEL], ['Live phone screen. Tap, swipe or type.' if mode == 'screen' else 'Live phone camera'])
        self.frame = Gtk.AspectFrame(ratio=720 / 1560 if mode == 'screen' else 16 / 9, obey_child=False, hexpand=True, vexpand=True)
        self.frame.set_child(self.picture)
        self.frame.add_css_class('phone-screen' if mode == 'screen' else 'camera-preview')
        self.frame.set_overflow(Gtk.Overflow.HIDDEN)
        self.status = Gtk.Label(label='Connect the phone to start.', xalign=0, wrap=True)
        self.status.add_css_class('dim-label')
        self.status.add_css_class('caption')
        if mode == 'camera':
            row = Gtk.Box(spacing=8)
            self.source = Gtk.DropDown.new_from_strings(['Rear camera', 'Front camera'])
            self.source.set_hexpand(True)
            self.source.connect('notify::selected', self._source_changed)
            row.append(self.source)
            self.start_button = Gtk.Button(label='Start camera')
            self.start_button.add_css_class('suggested-action')
            self.start_button.connect('clicked', lambda *_: self.start())
            row.append(self.start_button)
            self.stop_button = Gtk.Button(label='Stop')
            self.stop_button.connect('clicked', lambda *_: self.stop())
            row.append(self.stop_button)
            self.append(row)
            quality_row = Gtk.Box(spacing=8)
            quality_row.append(Gtk.Label(label='Camera quality', xalign=0, hexpand=True))
            self.quality = Gtk.DropDown.new_from_strings(['720p · Low latency', '1080p'])
            quality_row.append(self.quality)
            self.append(quality_row)
        self.append(self.frame)
        if mode == 'camera':
            self.preview_toggle = Gtk.CheckButton(label='Show camera preview', active=True)
            self.preview_toggle.connect('toggled', self._preview_changed)
            self.append(self.preview_toggle)
            row = Gtk.Box(spacing=8)
            self.webcam = Gtk.CheckButton(label='Use as webcam')
            self.webcam.connect('toggled', self._webcam_changed)
            row.append(self.webcam)
            self.device = Gtk.Entry(text='/dev/video10', hexpand=True)
            self.device.set_tooltip_text('Virtual webcam output device')
            row.append(self.device)
            self.append(row)
            self.microphone = Gtk.CheckButton(label='Share phone microphone')
            self.microphone.set_tooltip_text('Creates Vivo Phone Microphone only when the phone supplies microphone audio.')
            self.append(self.microphone)
        else:
            self._input_controllers()
            row = Gtk.Box(homogeneous=True, spacing=12)
            for icon, label, code in [('view-list-symbolic', 'Recents', 187), ('go-home-symbolic', 'Home', 3), ('go-previous-symbolic', 'Back', 4)]:
                row.append(_button(icon, label, lambda button, key=code: self._navigate(key)))
            self.append(row)
        self.append(self.status)
        self.set_connected(False)

    def _set_status(self, text):
        self.status.set_text(text)
        self.status.set_tooltip_text(text)
        self._update_controls()
        return False

    def _update_controls(self):
        active = bool(self.session and not self.session.closed.is_set())
        if self.mode == 'camera':
            self.start_button.set_sensitive(self.connected and not active)
            self.stop_button.set_sensitive(active)
            self.quality.set_sensitive(not active)
            self.microphone.set_sensitive(not active)
            self.device.set_sensitive(not active and self.webcam.get_active())

    def set_connected(self, connected):
        self.connected = connected
        if not connected:
            self.stop()
        self._update_controls()

    def start(self):
        if not self.connected or self.session and not self.session.closed.is_set():
            return
        try:
            self.decoder = VideoPipeline(self.picture, self._set_status, self._geometry)
        except (GLib.Error, RuntimeError) as error:
            self._set_status('Video decoder unavailable. Install the GStreamer H.264 and GTK plugins.')
            return
        camera = self.mode == 'camera'
        if camera:
            self.decoder.set_preview(self.preview_toggle.get_active())
            self._webcam_changed()
        session = MediaSession(self.backend, self.decoder, lambda text: self._session_status(session, text), self.mode,
            'front' if camera and self.source.get_selected() == 1 else 'rear',
            ('1080p' if self.quality.get_selected() == 1 else '720p') if camera else self.screen_quality,
            camera and self.microphone.get_active())
        self.session = session
        session.start()
        self._update_controls()

    def stop(self):
        self._release()
        if self.session:
            self.session.stop()
            self.session = None
        self.decoder = None
        self.picture.set_paintable(None)
        self.status.set_text('Preview stopped.' if self.connected else 'Connect the phone to start.')
        self._update_controls()

    def _session_status(self, session, text):
        if self.session is session:
            self._set_status(text)

    def _geometry(self, width, height):
        self.frame.set_ratio(width / height)

    def _source_changed(self, *_):
        if self.session:
            self.session.send({'kind':'camera', 'facing':'front' if self.source.get_selected() else 'rear'})

    def _preview_changed(self, *_):
        visible = self.preview_toggle.get_active()
        self.frame.set_visible(visible)
        if self.decoder:
            self.decoder.set_preview(visible)

    def _webcam_changed(self, *_):
        if self.decoder:
            try:
                self.decoder.set_webcam(self.device.get_text() if self.webcam.get_active() else '')
            except (OSError, ValueError, RuntimeError, GLib.Error):
                self._set_status('Camera live · webcam device unavailable. Choose a v4l2loopback device.')
        self._update_controls()

    def set_quality(self, quality):
        if quality in ('480p', '720p', '1080p', 'auto'):
            self.screen_quality = quality
            if self.session:
                self.session.quality = quality
                self.session.send({'kind':'quality', 'quality':quality})

    def _send(self, event):
        if self.connected and self.mode == 'screen' and self.session:
            self.session.send(event)

    def _navigate(self, code):
        self._send({'kind':'key', 'action':'down', 'keycode':code})
        self._send({'kind':'key', 'action':'up', 'keycode':code})
        self.picture.grab_focus()

    def _position(self, x, y, clamp=False):
        width, height = self.decoder.geometry if self.decoder else (0, 0)
        return picture_point(x, y, self.picture.get_width(), self.picture.get_height(), width, height, clamp)

    def _pointer(self, action, point):
        width, height = self.decoder.geometry
        self._send({'kind':'pointer', 'action':action, 'buttons':1, 'x':point[0], 'y':point[1], 'width':width, 'height':height})

    def _drag_begin(self, gesture, x, y):
        point = self._position(x, y)
        if point is None or not self.session:
            return
        self.picture.grab_focus()
        self.origin = (x, y)
        self.point, self.pressed = point, True
        self._pointer('down', point)

    def _drag_update(self, gesture, x, y):
        if self.pressed:
            self.point = self._position(self.origin[0] + x, self.origin[1] + y, True)
            if self.point:
                self._pointer('move', self.point)

    def _release(self, *_):
        if self.pressed and self.decoder and self.point:
            self._pointer('up', self.point)
        self.pressed = False

    def _scroll(self, controller, dx, dy):
        event = controller.get_current_event()
        if not event:
            return False
        position = event.get_position()
        point = self._position(*position[-2:])
        if not point or not self.decoder:
            return False
        width, height = self.decoder.geometry
        horizontal, vertical = int(-dx), int(-dy)
        for index in range(min(8, max(abs(horizontal), abs(vertical)))):
            self._send({'kind':'scroll', 'x':point[0], 'y':point[1], 'width':width, 'height':height,
                'hscroll':(1 if horizontal > 0 else -1) if index < abs(horizontal) else 0,
                'vscroll':(1 if vertical > 0 else -1) if index < abs(vertical) else 0})
        return True

    def _commit(self, context, text):
        text = text.replace('\r', '\n')
        if text and all(ord(char) >= 32 or char == '\n' for char in text) and len(text.encode()) <= 4096:
            self._send({'kind':'text', 'text':text})

    def _key(self, controller, keyval, keycode, state):
        if keyval == Gdk.KEY_Escape:
            self._navigate(4)
            return True
        if keyval in (Gdk.KEY_Return, Gdk.KEY_KP_Enter):
            self._commit(None, '\n')
            return True
        return False

    def _input_controllers(self):
        drag = Gtk.GestureDrag(button=1)
        drag.connect('drag-begin', self._drag_begin)
        drag.connect('drag-update', self._drag_update)
        drag.connect('drag-end', lambda gesture, x, y: (self._drag_update(gesture, x, y), self._release()))
        drag.connect('cancel', self._release)
        self.picture.add_controller(drag)
        scroll = Gtk.EventControllerScroll(flags=Gtk.EventControllerScrollFlags.BOTH_AXES | Gtk.EventControllerScrollFlags.DISCRETE)
        scroll.connect('scroll', self._scroll)
        self.picture.add_controller(scroll)
        im = Gtk.IMMulticontext()
        im.set_client_widget(self.picture)
        im.connect('commit', self._commit)
        keys = Gtk.EventControllerKey()
        keys.set_im_context(im)
        keys.connect('key-pressed', self._key)
        self.picture.add_controller(keys)
        focus = Gtk.EventControllerFocus()
        focus.connect('enter', lambda *_: im.focus_in())
        focus.connect('leave', lambda *_: (self._release(), im.focus_out()))
        self.picture.add_controller(focus)


class MirrorWindow(Adw.Window):
    def __init__(self, app, backend):
        super().__init__(application=app, title='Phone mirror', default_width=400, default_height=850)
        self.panel = MediaPanel(backend, mode='screen')
        self.panel.set_spacing(4)
        self.panel.set_margin_start(4)
        self.panel.set_margin_end(4)
        self.panel.set_margin_bottom(4)
        toolbar = Adw.ToolbarView()
        header = Adw.HeaderBar()
        header.add_css_class('flat')
        header.set_title_widget(Gtk.Label(label='Phone mirror'))
        menu = Gio.Menu()
        quality_menu = Gio.Menu()
        self.actions = Gio.SimpleActionGroup()
        quality_action = Gio.SimpleAction.new_stateful('quality', GLib.VariantType.new('s'), GLib.Variant('s', '720p'))
        def quality_changed(action, value):
            action.set_state(value)
            self.panel.set_quality(value.get_string())
        quality_action.connect('activate', quality_changed)
        self.actions.add_action(quality_action)
        for label, value in [('480p', '480p'), ('720p', '720p'), ('1080p', '1080p'), ('Phone automatic', 'auto')]:
            item = Gio.MenuItem.new(label, None)
            item.set_action_and_target_value('mirror.quality', GLib.Variant('s', value))
            quality_menu.append_item(item)
        menu.append_submenu('Screen quality', quality_menu)
        for name, label, callback in [('start', 'Start mirroring', self.start), ('stop', 'Stop mirroring', self.stop)]:
            action = Gio.SimpleAction.new(name, None)
            action.connect('activate', lambda action, parameter, fn=callback: fn())
            self.actions.add_action(action)
            menu.append(label, 'mirror.' + name)
        self.insert_action_group('mirror', self.actions)
        button = Gtk.MenuButton(icon_name='view-more-symbolic', menu_model=menu, tooltip_text='Mirror settings')
        header.pack_end(button)
        toolbar.add_top_bar(header)
        toolbar.set_content(self.panel)
        self.set_content(toolbar)
        self.connect('close-request', self._close)

    def _close(self, *_):
        self.stop()
        self.set_visible(False)
        return True

    def set_connected(self, connected):
        self.panel.set_connected(connected)
        self.actions.lookup_action('start').set_enabled(connected)

    def start(self):
        self.panel.start()

    def stop(self):
        self.panel.stop()


class MediaViewer(Adw.Window):
    def __init__(self, app, path, mime, on_close=None):
        super().__init__(application=app, title=Path(path).name, default_width=960, default_height=720)
        self.on_close = on_close
        self.video = None
        toolbar = Adw.ToolbarView()
        header = Adw.HeaderBar()
        toolbar.add_top_bar(header)
        if mime.startswith('image/'):
            self.media = Gtk.Picture.new_for_filename(str(path))
            self.media.set_content_fit(Gtk.ContentFit.CONTAIN)
            self.media.set_can_shrink(True)
        else:
            self.video = Gtk.MediaFile.new_for_filename(str(path))
            self.media = Gtk.Video(media_stream=self.video, autoplay=True)
            self.video.play()
        self.media.set_hexpand(True)
        self.media.set_vexpand(True)
        toolbar.set_content(self.media)
        self.set_content(toolbar)
        keys = Gtk.EventControllerKey()
        keys.connect('key-pressed', self._key)
        self.add_controller(keys)
        self.connect('close-request', self._close)

    def _key(self, controller, keyval, keycode, state):
        if keyval == Gdk.KEY_F11:
            self.unfullscreen() if self.is_fullscreen() else self.fullscreen()
            return True
        if keyval == Gdk.KEY_Escape and self.is_fullscreen():
            self.unfullscreen()
            return True
        return False

    def _close(self, *_):
        if self.video:
            self.video.pause()
            self.video.clear()
        if self.on_close:
            callback, self.on_close = self.on_close, None
            callback()
        return False
