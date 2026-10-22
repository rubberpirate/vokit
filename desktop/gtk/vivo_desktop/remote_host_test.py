import json
import time
import unittest
import zlib
from unittest.mock import Mock, patch

from .remote_host import GLib, Gst, GstWebRTC, RemoteHost, validated_input, capture_input, input_stages


def spin(condition, timeout=8):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        while GLib.MainContext.default().pending():
            GLib.MainContext.default().iteration(False)
        if condition():
            return True
        time.sleep(.005)
    return False


class RemoteHostTests(unittest.TestCase):
    def test_input_boundary(self):
        for kind in ('mouse', 'key', 'changeWidgetState'):
            data = {'type': kind, 'name': 'move', 'value': 1, 'x': .3}
            self.assertEqual(validated_input(json.dumps({'msgId': 1, 'data': data})), data)
        mouse = {'type': 'mouse', 'position': {'clientX': .5, 'clientY': .5}, 'view': {'width': 1280, 'height': 720}, 'button': {'action': 'left', 'doubble': False}}
        self.assertEqual(validated_input(json.dumps({'data': mouse})), mouse)
        key = {'type': 'key', 'modifiers': ['ctrl'], 'key': 'a', 'value': 'a', 'language': 'en'}
        self.assertEqual(validated_input(json.dumps({'data': key})), key)
        for value in ('{}', '[]', '{', json.dumps({'data': {'type': 'shutdown'}}),
                      json.dumps({'data': {'type': 'mouse', 'value': {'x': {'x': {'x': {'x': 1}}}}}}),
                      json.dumps({'data': {'type': 'mouse', 'value': float('nan')}}),
                      json.dumps({'data': {'type': 'key', 'value': 'x' * 1025}})):
            self.assertIsNone(validated_input(value))

    def test_vendor_mouse_screen_metadata_cannot_choose_control_target(self):
        mouse = {'type':'mouse', 'view':{'width':1280,'height':720},
                 'position':{'clientX':640,'clientY':360},
                 'button':{'action':'left','doubble':False},
                 'screen':{'width':9999,'height':9999,'x':50000,'y':-50000,
                           'id':8,'index':8,'selected':True,'title':'Remote monitor'}}
        safe = validated_input(json.dumps({'data':mouse}))
        self.assertIsNotNone(safe)
        self.assertNotIn('screen',safe)
        self.assertEqual(capture_input(safe,(1920,1080))['position'], {'clientX':960,'clientY':540})
        for screen in ([], {'width':float('nan')}, {'nested':{'invalid':1}}, {'title':'x'*1025}):
            mouse['screen'] = screen
            self.assertIsNone(validated_input(json.dumps({'data':mouse})))

    def test_input_shape_diagnostics_exclude_content(self):
        stages = input_stages(json.dumps({'data':{'type':'key','key':'escape','value':'private typed text'}}))
        self.assertEqual(stages, ['key-message','key-has-text','key-escape'])
        stages = input_stages(json.dumps({'data':{'type':'mouse','position':{'clientX':1,'clientY':2},'view':{'width':1280,'height':720,'extra':True},'button':{'action':'move'}}}))
        self.assertIn('mouse-extra-view',stages)
        self.assertIn('mouse-rejected',stages)
        self.assertNotIn('extra',stages)

    def test_invalid_requests_stop_without_capture(self):
        events = []
        host = RemoteHost(events.append)
        host.request({'op': 'offer', 'sdp': 'invalid'})
        self.assertTrue(host.closed)
        self.assertEqual(events, [{'event': 'error', 'error': 'Invalid remote host request.'}])

    def test_letterbox_mouse_mapping_and_outside_release(self):
        mouse = {'type': 'mouse', 'view': {'width': 1280, 'height': 720}, 'position': {'clientX': 640, 'clientY': 360}, 'button': {'action': 'left'}}
        mapped = capture_input(mouse, (1200, 1920))
        self.assertEqual(mapped['position'], {'clientX': 600, 'clientY': 960})
        self.assertEqual(mapped['view'], {'width': 1200, 'height': 1920})
        mouse['position']['clientX'] = 0
        self.assertIsNone(capture_input(mouse, (1200, 1920)))
        mouse['button']['action'] = 'up'
        self.assertEqual(capture_input(mouse, (1200, 1920))['position']['clientX'], 0)
        key = {'type': 'key', 'key': 'a'}
        self.assertIs(capture_input(key, None), key)
        mouse['button']['action'] = 'left'
        mouse['position'] = {'clientX': 320, 'clientY': 180}
        mouse['view'] = {'width': 640, 'height': 360}
        self.assertEqual(capture_input(mouse, (1920, 1200))['position'], {'clientX': 960, 'clientY': 600})

    def test_input_requires_control_consent_and_connected_peer(self):
        events = []
        host = RemoteHost(events.append)
        backend = Mock(available=True)
        host.input = backend
        host._input_idle({'type': 'key', 'key': 'a'})
        backend.handle.assert_not_called()
        host.connected = True
        host._input_idle({'type': 'key', 'key': 'a'})
        backend.handle.assert_not_called()
        host.allow_control = True
        host._input_idle({'type': 'key', 'key': 'a'})
        backend.handle.assert_called_once_with({'type': 'key', 'key': 'a'})
        host.reset(rebuild=False)
        backend.close.assert_called_once()
        self.assertFalse(host.connected)
        self.assertIsNone(host.input)
        host.stop()

    def test_source_metadata_uses_portal_identity_and_physical_dimensions(self):
        events = []
        host = RemoteHost(events.append)
        host.geometry, host.position, host.mapping_id = (1920, 1080), (0, 0), 'HDMI-A-1'
        host.allow_control = True
        backend = Mock(available=True, geometry=(5142, 1640, 1536, 864), scale=1.25)
        host.input_class = Mock(return_value=backend)
        with patch('vivo_desktop.remote_host.screen_locked', return_value=False):
            host._captured_idle((1920, 1080))
        host.input_class.assert_called_once_with(1920, 1080, position=(0, 0), mapping_id='HDMI-A-1')
        ready = next(event for event in events if event['event'] == 'ready')
        self.assertEqual((ready['capture_width'], ready['capture_height']), (1920, 1080))
        self.assertEqual((ready['screen_x'], ready['screen_y'], ready['screen_scale']), (5142, 1640, 1.25))
        self.assertIs(ready['screen_locked'], False)
        self.assertTrue(ready['control'])
        host.stop()

    @unittest.skipUnless(all(Gst.ElementFactory.find(name) for name in ('webrtcbin', 'nicesrc', 'x264enc', 'videotestsrc')), 'GStreamer WebRTC and H264 plugins required')
    def test_synthetic_video_and_negotiated_data_channel(self):
        events = []
        host = RemoteHost(events.append)
        inputs = []
        deliver_input = host._input_idle
        def observed_input(data, channel=None):
            inputs.append(data)
            return deliver_input(data, channel)
        host._input_idle = observed_input
        phone = Gst.Pipeline.new('phone-test')
        peer = Gst.ElementFactory.make('webrtcbin', 'phone')
        peer.set_property('bundle-policy', GstWebRTC.WebRTCBundlePolicy.MAX_BUNDLE)
        phone.add(peer)
        peer.emit('add-transceiver', GstWebRTC.WebRTCRTPTransceiverDirection.RECVONLY, Gst.Caps.from_string('application/x-rtp,media=audio,encoding-name=OPUS,clock-rate=48000,payload=111'))
        caps = Gst.Caps.from_string('application/x-rtp,media=video,encoding-name=H264,clock-rate=90000,payload=98,packetization-mode=(string)1,profile-level-id=(string)42e01f,level-asymmetry-allowed=(string)1')
        peer.emit('add-transceiver', GstWebRTC.WebRTCRTPTransceiverDirection.RECVONLY, caps)
        frames = []
        decoder = Gst.parse_bin_from_description('rtph264depay ! h264parse ! avdec_h264 ! videoconvert ! video/x-raw,format=GRAY8 ! fakesink name=decoded', True)
        sink = decoder.get_by_name('decoded')
        sink.set_property('sync', False)
        sink.set_property('async', False)
        sink.set_property('signal-handoffs', True)
        def decoded(_sink, buffer, _pad):
            ok, data = buffer.map(Gst.MapFlags.READ)
            if ok:
                frames.append((buffer.pts, max(data.data) - min(data.data), zlib.crc32(data.data)))
                buffer.unmap(data)
        sink.connect('handoff', decoded)
        phone.add(decoder)
        peer.connect('pad-added', lambda _peer, pad: pad.link(decoder.get_static_pad('sink')))
        phone.set_state(Gst.State.PLAYING)
        options = Gst.Structure.new_empty('application/data-channel')
        options.set_value('negotiated', True)
        options.set_value('id', 100)
        options.set_value('ordered', True)
        channel = peer.emit('create-data-channel', 'vivodatachannel', options)
        received = []
        def phone_data(channel, message):
            data = message.get_data()
            received.append(data)
            if data[0] == 0xFE:
                channel.emit('send-data', GLib.Bytes.new(b'\xff' + data[1:]))
        channel.connect('on-message-data', phone_data)
        offers = []
        def offered(promise, *_):
            reply = promise.get_reply()
            offer = reply.get_value('offer')
            peer.emit('set-local-description', offer, None)
            offers.append(offer)
        try:
            host.capture_ready('videotestsrc is-live=true pattern=ball')
            self.assertTrue(spin(lambda: any(event['event'] == 'ready' for event in events)))
            ready = next(event for event in events if event['event'] == 'ready')
            self.assertEqual((ready['width'], ready['height'], ready['control']), (1280, 720, False))
            self.assertEqual((ready['capture_width'], ready['capture_height']), (1280, 720))
            peer.emit('create-offer', None, Gst.Promise.new_with_change_func(offered, None, None))
            self.assertTrue(spin(lambda: offers and peer.get_property('ice-gathering-state') == GstWebRTC.WebRTCICEGatheringState.COMPLETE))
            host.request({'op': 'offer', 'sdp': peer.get_property('local-description').sdp.as_text()})
            self.assertTrue(spin(lambda: any(event['event'] == 'answer' for event in events)), events)
            answer = next(event['sdp'] for event in events if event['event'] == 'answer')
            from .remote_host import GstSdp
            _, message = GstSdp.SDPMessage.new_from_text(answer)
            peer.emit('set-remote-description', GstWebRTC.WebRTCSessionDescription.new(GstWebRTC.WebRTCSDPType.ANSWER, message), None)
            self.assertTrue(spin(lambda: frames and channel.get_property('ready-state') == GstWebRTC.WebRTCDataChannelState.OPEN), events)
            self.assertIn({'event': 'connected'}, events)
            channel.emit('send-string', '{"msgId":1,"data":{"type":"mouse","name":"move","x":0.5,"y":0.5}}')
            self.assertTrue(spin(lambda: inputs))
            self.assertEqual(inputs[0]['type'], 'mouse')
            channel.emit('send-data', GLib.Bytes.new(b'\xfe\x00heartbeat'))
            self.assertTrue(spin(lambda: b'\xff\x00heartbeat' in received))
            self.assertTrue(spin(lambda: len(frames) >= 60 and frames[-1][0] - frames[0][0] > 2 * Gst.SECOND and any(e.get('stage') == 'heartbeat-reply' for e in events)), events)
            self.assertGreater(max(frame[1] for frame in frames), 100, 'Decoded screen is black or uniform')
            self.assertGreater(frames[-1][0] - frames[0][0], Gst.SECOND // 2, 'Video timestamps stalled')
            self.assertGreater(len({frame[2] for frame in frames}), 5, 'Video froze after the first frame')
            self.assertTrue(any(data[0] == 0xFE and len(data) == 5 for data in received))
            self.assertGreater(host.capture_frames, 20)
            self.assertGreater(host.rtp_packets, 20)
            self.assertIn('a=rtpmap:98 H264/90000', answer)
            self.assertIn('a=candidate:', answer)
            self.assertIsInstance(host.pipeline.get_clock(), Gst.SystemClock)
            self.assertIn('packetization-mode=1', answer)
            self.assertIn('level-asymmetry-allowed=1', answer)
            self.assertIn('3.1', host.pipeline.get_by_name('parsed').get_static_pad('src').get_current_caps().to_string())
            stages = {event.get('stage') for event in events if event['event'] == 'diagnostic'}
            self.assertTrue({'capture-frame', 'h264-sps', 'h264-pps', 'h264-idr', 'rtp-packet', 'channel-open', 'video-keyframe-request'} <= stages, stages)
            host.request({'op': 'reset'})
            self.assertFalse(host.closed)
            self.assertFalse(host.answer_pending)
            self.assertIsNotNone(host.peer)
            self.assertIsNone(host.heartbeat_timer)
        finally:
            host.stop()
            phone.set_state(Gst.State.NULL)
