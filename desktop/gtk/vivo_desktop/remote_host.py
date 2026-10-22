"""Consented desktop capture and a private JSON-line WebRTC host.

The parent owns authentication and encrypted signaling. Capture and optional
bounded input begin only after an explicit local start request. No recordings.
"""
import json
import math
import os
import re
import signal
import sys
import threading
import uuid

import gi

gi.require_version('Gst', '1.0')
gi.require_version('GstWebRTC', '1.0')
gi.require_version('GstSdp', '1.0')
gi.require_version('GstVideo', '1.0')
from gi.repository import Gio, GLib, Gst, GstSdp, GstVideo, GstWebRTC

Gst.init(None)
LINE_LIMIT = 1024 * 1024
PORTAL = 'org.freedesktop.portal.Desktop'
PORTAL_PATH = '/org/freedesktop/portal/desktop'
SCREENCAST = 'org.freedesktop.portal.ScreenCast'


def screen_locked():
    """Read the session's reported lock state; omit it when unavailable."""
    try:
        bus = Gio.bus_get_sync(Gio.BusType.SYSTEM, None)
        path = bus.call_sync('org.freedesktop.login1', '/org/freedesktop/login1',
                             'org.freedesktop.login1.Manager', 'GetSessionByPID',
                             GLib.Variant('(u)', (os.getpid(),)), GLib.VariantType.new('(o)'),
                             Gio.DBusCallFlags.NONE, 1000, None).unpack()[0]
        value = bus.call_sync('org.freedesktop.login1', path, 'org.freedesktop.DBus.Properties',
                              'Get', GLib.Variant('(ss)', ('org.freedesktop.login1.Session', 'LockedHint')),
                              GLib.VariantType.new('(v)'), Gio.DBusCallFlags.NONE, 1000, None).unpack()[0]
        return value if type(value) is bool else None
    except GLib.Error:
        return None


def validated_input(message):
    """Keep vendor input metadata bounded; never interpret arbitrary actions."""
    if not isinstance(message, str) or len(message) > 16384:
        return None
    try:
        envelope = json.loads(message)
        data = envelope.get('data') if isinstance(envelope, dict) else None
        if not isinstance(data, dict) or data.get('type') not in ('mouse', 'key', 'changeWidgetState'):
            return None
        if len(data) > 24 or any(not isinstance(k, str) or len(k) > 64 for k in data):
            return None
        allowed = {'type', 'name', 'key', 'value', 'language', 'modifiers', 'position', 'view', 'button', 'x', 'y'}
        def scalar(value):
            if isinstance(value, str):
                return len(value) <= 1024 and '\x00' not in value
            if isinstance(value, (int, float)) and not isinstance(value, bool):
                return math.isfinite(value) and abs(value) <= 2 ** 31
            return value is None or isinstance(value, bool)
        # Ignore optional screen metadata; only the local capture selects a target.
        if 'screen' in data:
            screen = data['screen']
            if not isinstance(screen, dict) or len(screen) > 24 or any(not isinstance(k, str) or len(k) > 64 or not scalar(v) for k, v in screen.items()):
                return None
            data = {key: value for key, value in data.items() if key != 'screen'}
        if not data.keys() <= allowed:
            return None
        for key, value in data.items():
            if key in ('position', 'view', 'button'):
                fields = {'position': {'clientX', 'clientY', 'scrollX', 'scrollY'}, 'view': {'width', 'height'}, 'button': {'action', 'doubble'}}[key]
                if not isinstance(value, dict) or not value.keys() <= fields or not all(scalar(v) for v in value.values()):
                    return None
                for field, number in value.items():
                    if field not in ('action', 'doubble') and (isinstance(number, bool) or not isinstance(number, (int, float))):
                        return None
                if key == 'button' and ('action' in value and value['action'] not in ('left', 'middle', 'right', 'move', 'scroll', 'down', 'rightDown', 'keepDrag', 'rightKeepDrag', 'up', 'rightUp')):
                    return None
            elif key == 'modifiers':
                if not isinstance(value, list) or len(value) > 8 or not all(isinstance(v, str) and len(v) <= 32 for v in value):
                    return None
            elif not scalar(value):
                return None
        return data
    except (ValueError, TypeError, OverflowError):
        return None


def input_stages(message):
    """Describe structure with fixed labels, never user text or key characters."""
    if not isinstance(message, str) or len(message) > 16384:
        return ()
    try:
        envelope = json.loads(message)
        data = envelope.get('data') if isinstance(envelope, dict) else None
        if not isinstance(data, dict):
            return ()
        if data.get('type') == 'key':
            stages = ['key-message']
            if isinstance(data.get('value'), str) and data['value']:
                stages.append('key-has-text')
            if data.get('modifiers'):
                stages.append('key-has-modifiers')
            key = data.get('key')
            if isinstance(key, str) and key.lower() in ('escape', 'esc'):
                stages.append('key-escape')
            elif isinstance(key, str) and key.lower() in ('win', 'windows', 'command', 'super', 'meta', 'logo'):
                stages.append('key-super')
            return stages
        if data.get('type') != 'mouse':
            return ()
        stages = ['mouse-message']
        if not data.keys() <= {'type', 'name', 'position', 'view', 'button', 'screen', 'x', 'y'}:
            stages.append('mouse-extra-fields')
        for key, expected in (('position', {'clientX', 'clientY', 'scrollX', 'scrollY'}), ('view', {'width', 'height'}), ('button', {'action', 'doubble'})):
            value = data.get(key)
            if not isinstance(value, dict):
                stages.append('mouse-missing-' + key)
            elif not value.keys() <= expected:
                stages.append('mouse-extra-' + key)
        if validated_input(message) is None:
            stages.append('mouse-rejected')
        return stages
    except (ValueError, TypeError, RecursionError):
        return ()


def capture_input(data, geometry):
    """Undo the bounded video's letterbox before mapping to compositor space."""
    if data.get('type') != 'mouse':
        return data
    if not geometry:
        return None
    width, height = geometry
    view, position = data.get('view'), data.get('position')
    if not isinstance(view, dict) or not isinstance(position, dict):
        return None
    vx, vy = view.get('width'), view.get('height')
    px, py = position.get('clientX'), position.get('clientY')
    if any(type(value) not in (int, float) or not math.isfinite(value) for value in (vx, vy, px, py)) or min(vx, vy, width, height) <= 0:
        return None
    scale = min(1280 / width, 720 / height)
    left, top = (1280 - width * scale) / 2, (720 - height * scale) / 2
    x, y = (px * 1280 / vx - left) / scale, (py * 720 / vy - top) / scale
    if not (0 <= x <= width and 0 <= y <= height):
        # Releasing outside the shared image still releases the held mouse key.
        if data.get('button', {}).get('action') not in ('up', 'rightUp'):
            return None
        x, y = min(width, max(0, x)), min(height, max(0, y))
    return dict(data, view={'width': width, 'height': height}, position=dict(position, clientX=x, clientY=y))


class ScreenCast:
    """Use the portal's session lifetime and PipeWire FD, without persistent grants."""
    def __init__(self, ready, failed):
        self.ready, self.failed = ready, failed
        self.bus = None
        self.session = None
        self.fd = None
        self.subscriptions = set()
        self.closed = False

    def start(self):
        Gio.bus_get(Gio.BusType.SESSION, None, self._bus_ready)

    def _bus_ready(self, _source, result):
        try:
            self.bus = Gio.bus_get_finish(result)
            self._request('CreateSession', lambda options: GLib.Variant('(a{sv})', (dict(options, session_handle_token=GLib.Variant('s', 'vivo_' + uuid.uuid4().hex)),)), self._created)
        except GLib.Error:
            self.failed('Screen sharing portal is unavailable.')

    def _request(self, method, parameters, completed):
        if self.closed:
            return
        token = 'vivo_' + uuid.uuid4().hex
        sender = self.bus.get_unique_name()[1:].replace('.', '_')
        path = '/org/freedesktop/portal/desktop/request/' + sender + '/' + token
        options = {'handle_token': GLib.Variant('s', token)}
        subscription = None
        def response(_bus, _sender, _path, _interface, _signal, body):
            self.bus.signal_unsubscribe(subscription)
            self.subscriptions.discard(subscription)
            if self.closed:
                return
            code, result = body.unpack()
            if code:
                self.failed('Screen sharing was cancelled or denied.')
            else:
                try:
                    completed(result)
                except (GLib.Error, ValueError, KeyError, TypeError, OSError):
                    self.failed('Cannot start desktop screen sharing.')
        subscription = self.bus.signal_subscribe(PORTAL, 'org.freedesktop.portal.Request', 'Response', path, None, Gio.DBusSignalFlags.NONE, response)
        self.subscriptions.add(subscription)
        def called(bus, result):
            try:
                bus.call_finish(result)
            except GLib.Error:
                if not self.closed:
                    self.failed('Screen sharing portal is unavailable.')
        self.bus.call(PORTAL, PORTAL_PATH, SCREENCAST, method, parameters(options), GLib.VariantType.new('(o)'), Gio.DBusCallFlags.NONE, 120000, None, called)

    def _created(self, result):
        self.session = result['session_handle']
        self._request('SelectSources', lambda options: GLib.Variant('(oa{sv})', (self.session, dict(options, types=GLib.Variant('u', 1), multiple=GLib.Variant('b', False), cursor_mode=GLib.Variant('u', 2)))), self._selected)

    def _selected(self, _result):
        self._request('Start', lambda options: GLib.Variant('(osa{sv})', (self.session, '', options)), self._started)

    def _started(self, result):
        streams = result.get('streams', [])
        if not streams:
            raise ValueError('No selected source')
        node, properties = streams[0]
        def opened(bus, result):
            try:
                body, descriptors = bus.call_with_unix_fd_list_finish(result)
                fd = descriptors.get(body.unpack()[0])
                if self.closed:
                    os.close(fd)
                    return
                self.fd = fd
                # Release portal buffers before a blocked encoder/queue can retain them.
                self.ready('pipewiresrc fd=%d path=%d do-timestamp=true provide-clock=false use-bufferpool=false keepalive-time=100' % (fd, node), properties.get('size'), properties.get('position'), properties.get('mapping_id'))
            except (GLib.Error, OSError, ValueError):
                self.failed('Cannot open the selected desktop stream.')
        self.bus.call_with_unix_fd_list(PORTAL, PORTAL_PATH, SCREENCAST, 'OpenPipeWireRemote', GLib.Variant('(oa{sv})', (self.session, {})), GLib.VariantType.new('(h)'), Gio.DBusCallFlags.NONE, 10000, None, None, opened)
        self.subscriptions.add(self.bus.signal_subscribe(PORTAL, 'org.freedesktop.portal.Session', 'Closed', self.session, None, Gio.DBusSignalFlags.NONE, lambda *args: self.failed('Desktop screen sharing ended.')))

    def close(self):
        self.closed = True
        if self.bus:
            for subscription in self.subscriptions:
                self.bus.signal_unsubscribe(subscription)
            self.subscriptions.clear()
            if self.session:
                self.bus.call(PORTAL, self.session, 'org.freedesktop.portal.Session', 'Close', None, None, Gio.DBusCallFlags.NONE, 2000, None, None)
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None


class RemoteHost:
    def __init__(self, emit=None):
        self.loop = GLib.MainLoop()
        self.emit = emit or self._stdout
        self.portal = None
        self.source = None
        self.pipeline = None
        self.peer = None
        self.channel = None
        self.answer_pending = False
        self.timer = None
        self.closed = False
        self.started = False
        self.allow_control = False
        self.input_class = None
        self.input = None
        self.connected = False
        self.geometry = None
        self.position = None
        self.mapping_id = None
        self.ready_sent = False
        self.diagnostics = set()
        self.flow_timer = None
        self.heartbeat_timer = None
        self.heartbeat_mask = None
        self.capture_frames = self.rtp_packets = 0

    def diagnostic(self, stage):
        if not self.closed and stage not in self.diagnostics:
            self.diagnostics.add(stage)
            self.emit({'event': 'diagnostic', 'stage': stage})
        return False

    def _pipeline_diagnostic(self, pipeline, stage):
        if pipeline == self.pipeline:
            self.diagnostic(stage)
        return False

    @staticmethod
    def _stdout(value):
        sys.stdout.write(json.dumps(value, ensure_ascii=True, separators=(',', ':')) + '\n')
        sys.stdout.flush()

    def fail(self, text):
        if not self.closed:
            self.emit({'event': 'error', 'error': text})
            self.stop()
        return False

    def start(self, allow_control=False):
        self.started = True
        self.allow_control = allow_control
        if allow_control:
            try:
                from .remote_input import RemoteInput
                self.input_class = RemoteInput
            except (ImportError, OSError):
                self.input_class = None
        if not all(Gst.ElementFactory.find(name) for name in ('webrtcbin', 'rtph264pay', 'h264parse', 'videoconvert', 'videoscale')):
            return self.fail('Required GStreamer WebRTC plugins are unavailable.')
        if os.environ.get('WAYLAND_DISPLAY'):
            self.portal = ScreenCast(self.capture_ready, self.fail)
            self.portal.start()
        elif os.environ.get('DISPLAY'):
            self.capture_ready('ximagesrc use-damage=false show-pointer=true')
        else:
            self.fail('No desktop display is available for screen sharing.')
        return False

    def capture_ready(self, source, size=None, position=None, mapping_id=None):
        if self.closed:
            return
        self.source = source
        self.geometry = size
        self.position = position
        self.mapping_id = mapping_id
        try:
            self._pipeline()
        except (GLib.Error, RuntimeError):
            self.fail('Cannot initialize desktop video encoding.')

    def _pipeline(self, payload=96):
        if Gst.ElementFactory.find('x264enc'):
            encoder = 'x264enc name=encoder tune=zerolatency speed-preset=ultrafast bitrate=2500 key-int-max=30 byte-stream=true'
        elif Gst.ElementFactory.find('openh264enc'):
            encoder = 'openh264enc name=encoder bitrate=2500000 gop-size=30 complexity=low'
        else:
            raise RuntimeError('No H264 encoder')
        self.pipeline = Gst.parse_launch(self.source + ' name=capture ! queue max-size-buffers=2 leaky=downstream ! videoconvert ! videoscale add-borders=true ! videorate drop-only=true ! video/x-raw,format=I420,width=1280,height=720,framerate=30/1 ! ' + encoder + ' ! video/x-h264,profile=constrained-baseline,level=(string)3.1 ! h264parse name=parsed ! video/x-h264,stream-format=byte-stream,alignment=au ! rtph264pay name=packets config-interval=-1 pt=%d ! application/x-rtp,media=video,encoding-name=H264,clock-rate=90000,payload=%d ! webrtcbin name=peer bundle-policy=max-bundle' % (payload, payload))
        self.pipeline.get_by_name('capture').get_static_pad('src').add_probe(Gst.PadProbeType.BUFFER, self._captured)
        self.pipeline.get_by_name('parsed').get_static_pad('src').add_probe(Gst.PadProbeType.BUFFER, self._encoded)
        self.pipeline.get_by_name('packets').get_static_pad('src').add_probe(Gst.PadProbeType.BUFFER, self._rtp)
        self.peer = self.pipeline.get_by_name('peer')
        self.pipeline.use_clock(Gst.SystemClock.obtain())
        self.peer.connect('notify::ice-gathering-state', self._gathered)
        self.peer.connect('notify::connection-state', self._connection)
        pipeline = self.pipeline
        bus = pipeline.get_bus()
        bus.add_signal_watch()
        bus.connect('message::error', lambda *_: GLib.idle_add(self._pipeline_error, pipeline))
        if self.pipeline.set_state(Gst.State.PLAYING) == Gst.StateChangeReturn.FAILURE:
            raise RuntimeError('Video unavailable')
        options = Gst.Structure.new_empty('application/data-channel')
        options.set_value('negotiated', True)
        options.set_value('id', 100)
        options.set_value('ordered', True)
        self.channel = self.peer.emit('create-data-channel', 'vivodatachannel', options)
        if self.channel is None:
            raise RuntimeError('Data channel unavailable')
        self.channel.connect('on-message-string', self._input)
        self.channel.connect('on-message-data', self._binary)
        self.channel.connect('on-open', lambda channel: GLib.idle_add(self._channel_open, channel))
        self.channel.connect('on-close', lambda channel: GLib.idle_add(self._channel_closed, channel))
        self.flow_timer = GLib.timeout_add_seconds(5, self._flow, self.pipeline)

    def _flow(self, pipeline):
        if self.closed or pipeline != self.pipeline:
            return False
        self.emit({'event': 'flow', 'capture_frames': self.capture_frames, 'rtp_packets': self.rtp_packets})
        return True

    def _pipeline_error(self, pipeline):
        if pipeline == self.pipeline:
            self.fail('Desktop video transport failed.')
        return False

    def _captured(self, pad, _probe):
        self.capture_frames += 1
        if self.capture_frames != 1:
            return Gst.PadProbeReturn.OK
        caps = pad.get_current_caps()
        size = None
        if caps:
            structure = caps.get_structure(0)
            size = (structure.get_value('width'), structure.get_value('height'))
        GLib.idle_add(self._captured_idle, size, pad.get_parent_element().get_parent())
        return Gst.PadProbeReturn.OK

    def _captured_idle(self, size, pipeline=None):
        if self.closed or (pipeline is not None and pipeline != self.pipeline):
            return False
        if size and all(type(value) is int and value > 0 for value in size):
            self.geometry = size
        self._create_input()
        self.diagnostic('capture-frame')
        if not self.ready_sent:
            self.ready_sent = True
            event = {'event': 'ready', 'width': 1280, 'height': 720, 'control': bool(self.input and self.input.available)}
            if self.geometry:
                event.update(capture_width=self.geometry[0], capture_height=self.geometry[1])
            if self.position:
                event.update(screen_x=self.position[0], screen_y=self.position[1])
            if self.input and self.input.available:
                event.update(screen_x=self.input.geometry[0], screen_y=self.input.geometry[1],
                             screen_scale=self.input.scale)
            locked = screen_locked()
            if locked is not None:
                event['screen_locked'] = locked
            self.emit(event)
        return False

    def _encoded(self, pad, probe):
        buffer = probe.get_buffer()
        if buffer:
            data = buffer.extract_dup(0, buffer.get_size())
            kinds = {match[0] & 31 for match in re.findall(b'\x00\x00(?:\x00)?\x01(.)', data, re.S)}
            for kind, stage in ((7, 'h264-sps'), (8, 'h264-pps'), (5, 'h264-idr')):
                if kind in kinds:
                    GLib.idle_add(self._pipeline_diagnostic, pad.get_parent_element().get_parent(), stage)
            if {5, 7, 8} <= kinds:
                return Gst.PadProbeReturn.REMOVE
        return Gst.PadProbeReturn.OK

    def _rtp(self, pad, _probe):
        self.rtp_packets += 1
        if self.rtp_packets == 1:
            GLib.idle_add(self._pipeline_diagnostic, pad.get_parent_element().get_parent(), 'rtp-packet')
        return Gst.PadProbeReturn.OK

    def _channel_open(self, channel):
        if self.closed or channel != self.channel:
            return False
        self.diagnostic('channel-open')
        # A fresh IDR and parameter sets let a newly attached decoder start now.
        encoder = self.pipeline.get_by_name('encoder')
        if encoder.get_static_pad('src').send_event(GstVideo.video_event_new_upstream_force_key_unit(Gst.CLOCK_TIME_NONE, True, 0)):
            self.diagnostic('video-keyframe-request')
        if self.heartbeat_timer is None:
            self._heartbeat(channel)
            self.heartbeat_timer = GLib.timeout_add_seconds(1, self._heartbeat, channel)
        return False

    def _heartbeat(self, channel):
        if self.closed or channel != self.channel or channel.get_property('ready-state') != GstWebRTC.WebRTCDataChannelState.OPEN:
            return False
        self.heartbeat_mask = os.urandom(4)
        channel.emit('send-data', GLib.Bytes.new(b'\xfe' + self.heartbeat_mask))
        return True

    def _create_input(self):
        if self.allow_control and self.input_class and not self.input and self.geometry:
            try:
                self.input = self.input_class(*self.geometry, position=self.position, mapping_id=self.mapping_id)
            except Exception:
                self.input = None

    def _close_input(self):
        self.connected = False
        if self.input:
            self.input.close()
            self.input = None

    def _channel_closed(self, channel):
        if not self.closed and channel == self.channel:
            was_connected = self.connected
            self._close_input()
            if was_connected:
                self.emit({'event': 'disconnected'})
        return False

    def _binary(self, channel, message):
        if self.closed or channel != self.channel or message is None:
            return
        data = message.get_data()
        if not data or len(data) > 65536:
            return
        if data[0] == 0xFE:
            GLib.idle_add(self.diagnostic, 'heartbeat-request')
            channel.emit('send-data', GLib.Bytes.new(b'\xff' + data[1:]))
        elif data[0] == 0xFF:
            if data[1:] == self.heartbeat_mask:
                GLib.idle_add(self.diagnostic, 'heartbeat-reply')
        elif data[0] == ord('{'):
            try:
                self._input(channel, data.decode('utf-8'))
            except UnicodeError:
                pass

    def _input(self, channel, message):
        if channel == self.channel:
            GLib.idle_add(self.diagnostic, 'input-message')
            for stage in input_stages(message):
                GLib.idle_add(self.diagnostic, stage)
        data = validated_input(message)
        if data is not None:
            GLib.idle_add(self._input_idle, data, channel)

    def _input_idle(self, data, channel=None):
        if channel is not None and (channel != self.channel or channel.get_property('ready-state') != GstWebRTC.WebRTCDataChannelState.OPEN):
            return False
        if not self.closed and self.connected:
            self.diagnostic('input-received')
            if data.get('type') == 'mouse':
                self.diagnostic('mouse-received')
            if self.allow_control and self.input and self.input.available:
                mapped = capture_input(data, self.geometry)
                if mapped is not None:
                    if self.input.handle(mapped):
                        self.diagnostic('input-queued')
                        if data.get('type') == 'mouse':
                            self.diagnostic('mouse-queued')
                elif data.get('type') == 'mouse':
                    self.diagnostic('mouse-outside')
            else:
                self.diagnostic('input-unavailable')
                failure = getattr(self.input, 'failure', None)
                if failure in ('input-move-failed', 'input-button-failed', 'input-key-failed', 'input-text-failed', 'input-backend-failed'):
                    self.diagnostic(failure)
        return False

    def request(self, value):
        if self.closed:
            return False
        if not isinstance(value, dict):
            return self.fail('Invalid remote host request.')
        op = value.get('op')
        if op == 'start':
            if self.started or not isinstance(value.get('allow_control', False), bool):
                return self.fail('Invalid remote host request.')
            self.start(value.get('allow_control', False))
            return False
        if op == 'stop':
            return self.stop()
        if op == 'reset':
            self.reset()
            return False
        if op != 'offer' or not self.source or self.answer_pending:
            return self.fail('Invalid remote host request.')
        sdp, stun = value.get('sdp'), value.get('stun_server', '')
        if not isinstance(sdp, str) or len(sdp) > 524288 or not sdp.startswith('v=0') or '\x00' in sdp:
            return self.fail('Invalid remote session description.')
        if not isinstance(stun, str) or len(stun) > 256 or (stun and not re.fullmatch(r'stun://[A-Za-z0-9.\-\[\]:]+', stun)):
            return self.fail('Invalid remote ICE server.')
        # Match the phone's H264 dynamic payload instead of assuming 96.
        match = re.search(r'^a=rtpmap:(\d+) H264/90000\s*$', sdp, re.M | re.I)
        if not match or not 96 <= int(match[1]) <= 127:
            return self.fail('The phone did not offer H264 desktop video.')
        try:
            self.reset(rebuild=False)
            self._pipeline(int(match[1]))
            if stun:
                self.peer.set_property('stun-server', stun)
            result, message = GstSdp.SDPMessage.new_from_text(sdp)
            if result != GstSdp.SDPResult.OK:
                raise ValueError('SDP')
            self.answer_pending = True
            peer = self.peer
            def remote_set(promise, *_):
                GLib.idle_add(self._create_answer, peer, promise)
            promise = Gst.Promise.new_with_change_func(remote_set, None, None)
            peer.emit('set-remote-description', GstWebRTC.WebRTCSessionDescription.new(GstWebRTC.WebRTCSDPType.OFFER, message), promise)
            self.timer = GLib.timeout_add_seconds(30, self._timeout)
        except (GLib.Error, ValueError, RuntimeError):
            self.fail('Cannot negotiate desktop video with the phone.')
        return False

    def _create_answer(self, peer, promise):
        if self.closed or peer != self.peer:
            return False
        if promise.wait() != Gst.PromiseResult.REPLIED or (promise.get_reply() and promise.get_reply().has_field('error')):
            return self.fail('Cannot accept the phone video offer.')
        def answered(result, *_):
            GLib.idle_add(self._set_answer, peer, result)
        peer.emit('create-answer', None, Gst.Promise.new_with_change_func(answered, None, None))
        return False

    def _set_answer(self, peer, promise):
        if self.closed or peer != self.peer:
            return False
        reply = promise.get_reply()
        if not reply or not reply.has_field('answer'):
            return self.fail('Cannot create the desktop video answer.')
        peer.emit('set-local-description', reply.get_value('answer'), None)
        self._gathered(peer, None)
        return False

    def _gathered(self, peer, _property):
        GLib.idle_add(self._answer_ready, peer)

    def _answer_ready(self, peer):
        if self.closed or peer != self.peer or not self.answer_pending:
            return False
        if peer.get_property('ice-gathering-state') == GstWebRTC.WebRTCICEGatheringState.COMPLETE:
            description = peer.get_property('local-description')
            if description:
                self.answer_pending = False
                if self.timer:
                    GLib.source_remove(self.timer)
                    self.timer = None
                self.emit({'event': 'answer', 'sdp': description.sdp.as_text()})
        return False

    def _connection(self, peer, _property):
        GLib.idle_add(self._connection_idle, peer)

    def _connection_idle(self, peer):
        if not self.closed and peer == self.peer:
            state = peer.get_property('connection-state')
            if state == GstWebRTC.WebRTCPeerConnectionState.CONNECTED:
                self._create_input()
                self.connected = True
                self.emit({'event': 'connected'})
            elif state in (GstWebRTC.WebRTCPeerConnectionState.DISCONNECTED, GstWebRTC.WebRTCPeerConnectionState.FAILED, GstWebRTC.WebRTCPeerConnectionState.CLOSED):
                self._close_input()
                self.emit({'event': 'disconnected'})
        return False

    def _timeout(self):
        self.timer = None
        return self.fail('Desktop ICE gathering timed out.')

    def reset(self, rebuild=True):
        self._close_input()
        self.answer_pending = False
        for timer in (self.flow_timer, self.heartbeat_timer):
            if timer:
                GLib.source_remove(timer)
        self.flow_timer = self.heartbeat_timer = self.heartbeat_mask = None
        if self.timer:
            GLib.source_remove(self.timer)
            self.timer = None
        if self.pipeline:
            self.pipeline.get_bus().remove_signal_watch()
            self.pipeline.set_state(Gst.State.NULL)
        self.pipeline = self.peer = self.channel = None
        self.capture_frames = self.rtp_packets = 0
        self.diagnostics.clear()
        if rebuild and self.source and not self.closed:
            self.ready_sent = False
            try:
                self._pipeline()
            except (GLib.Error, RuntimeError):
                self.fail('Cannot restart desktop video capture.')

    def stop(self):
        if self.closed:
            return False
        self.closed = True
        self.reset(rebuild=False)
        if self.portal:
            self.portal.close()
        self.loop.quit()
        return False

    def read(self):
        while not self.closed:
            line = sys.stdin.buffer.readline(LINE_LIMIT + 1)
            if not line:
                GLib.idle_add(self.stop)
                return
            if len(line) > LINE_LIMIT:
                GLib.idle_add(self.fail, 'Remote host request is too large.')
                return
            try:
                value = json.loads(line)
            except (ValueError, UnicodeError):
                GLib.idle_add(self.fail, 'Invalid remote host request.')
                return
            GLib.idle_add(self.request, value)


def main():
    host = RemoteHost()
    signal.signal(signal.SIGTERM, lambda *_: GLib.idle_add(host.stop))
    signal.signal(signal.SIGINT, lambda *_: GLib.idle_add(host.stop))
    threading.Thread(target=host.read, daemon=True, name='remote-host-input').start()
    try:
        host.loop.run()
    finally:
        host.stop()


if __name__ == '__main__':
    main()
