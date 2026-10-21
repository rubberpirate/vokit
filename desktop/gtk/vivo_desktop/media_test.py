import json
import os
import socket
import struct
import tempfile
import threading
import time
import unittest
from types import SimpleNamespace

from .media import FramedRecords, picture_point, MediaSession


class MediaProtocolTests(unittest.TestCase):
    def test_fragmented_records_and_limits(self):
        parser = FramedRecords()
        data = struct.pack('!BI', 1, 2) + b'{}' + struct.pack('!BI', 2, 3) + b'abc'
        records = []
        for byte in data:
            records.extend(parser.feed(bytes([byte])))
        self.assertEqual(records, [(1, b'{}'), (2, b'abc')])
        for kind, size in [(0, 0), (5, 0), (2, 16 * 1024 * 1024 + 1), (4, 524289), (1, 65537)]:
            with self.assertRaises(ValueError):
                FramedRecords().feed(struct.pack('!BI', kind, size))

    def test_letterbox_mapping_and_drag_clamping(self):
        self.assertIsNone(picture_point(10, 300, 800, 600, 720, 1560))
        self.assertEqual(picture_point(400, 300, 800, 600, 720, 1560), (360, 780))
        self.assertEqual(picture_point(-10, 700, 800, 600, 720, 1560, True), (0, 1559))
        self.assertIsNone(picture_point(0, 0, 0, 0, 0, 0))

    def test_private_session_quality_and_input(self):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, 'media.sock')
            server = socket.socket(socket.AF_UNIX)
            server.bind(path)
            server.listen()
            received = []
            finished = threading.Event()
            def serve():
                peer, _ = server.accept()
                with peer, peer.makefile('rb') as stream:
                    received.append(json.loads(stream.readline()))
                    body = b'{"video_width":64,"video_height":96}'
                    record = struct.pack('!BI', 1, len(body)) + body
                    for byte in record:
                        peer.sendall(bytes([byte]))
                    received.append(json.loads(stream.readline()))
                    received.append(json.loads(stream.readline()))
                finished.set()
            thread = threading.Thread(target=serve, daemon=True)
            thread.start()
            class Decoder:
                geometry = (0, 0)
                def push(self, body): pass
                def close(self): pass
            session = MediaSession(SimpleNamespace(media_socket=path), Decoder(), lambda text: None)
            session.start()
            session.send({'kind':'text', 'text':'hello'})
            self.assertTrue(finished.wait(2))
            session.stop()
            thread.join(1)
            server.close()
            self.assertEqual(received[0], {'source':'screen'})
            self.assertEqual({item['kind'] for item in received[1:]}, {'text', 'quality'})
            self.assertIn({'kind':'quality', 'quality':'720p'}, received)


@unittest.skipUnless(os.environ.get('DISPLAY') or os.environ.get('WAYLAND_DISPLAY'), 'GTK display required; run under xvfb-run')
class NativeMediaTests(unittest.TestCase):
    @staticmethod
    def spin(condition, timeout=2):
        from gi.repository import GLib
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            while GLib.MainContext.default().pending() and time.monotonic() < deadline:
                GLib.MainContext.default().iteration(False)
            if condition():
                return True
            time.sleep(.002)
        return False

    def test_sparse_h264_native_and_rgb_fallback(self):
        from .media import Gtk, VideoPipeline
        Gtk.init()
        fixture = bytes.fromhex(
            '000000010910000000016742c00adc436c0440000003004000000523c489e00000000168ce0fc800'
            '00016588843a118a000218f1c00040f63800087949c9c9d75d75d75d75d75d75d75d75d75d75e000')
        for fallback in (False, True):
            with self.subTest(fallback=fallback):
                picture = Gtk.Picture()
                errors = []
                decoder = VideoPipeline(picture, errors.append, force_fallback=fallback)
                try:
                    decoder.push(fixture)
                    self.assertTrue(self.spin(lambda: decoder.decoded_frames == 1))
                    self.assertEqual(decoder.geometry, (64, 96))
                    self.assertTrue(self.spin(lambda: picture.get_paintable() is not None and picture.get_paintable().get_intrinsic_width() == 64))
                    decoder.set_preview(False)
                    decoder.push(fixture)
                    self.assertTrue(self.spin(lambda: decoder.decoded_frames == 2))
                    self.assertTrue(decoder.valve.get_property('drop'))
                    self.assertEqual(errors, [])
                    with self.assertRaises((OSError, ValueError)):
                        decoder.set_webcam('/tmp/not-a-camera')
                finally:
                    decoder.close()

    def test_camera_controls_and_always_active_screen_input(self):
        from .media import Adw, Gtk, MediaPanel, MirrorWindow
        Adw.init()
        backend = SimpleNamespace(media_socket='/tmp/not-used')
        camera = MediaPanel(backend)
        camera.preview_toggle.set_active(False)
        self.assertFalse(camera.frame.get_visible())
        camera.preview_toggle.set_active(True)
        self.assertTrue(camera.frame.get_visible())
        self.assertEqual(camera.source.get_selected(), 0)
        camera.source.set_selected(1)
        self.assertEqual(camera.source.get_selected(), 1)
        screen = MediaPanel(backend, mode='screen')
        screen._set_status("Live")
        self.assertFalse(screen.status.get_visible())
        screen._set_status("Connection lost")
        self.assertTrue(screen.status.get_visible())
        screen.connected = True
        inputs = []
        screen.session = SimpleNamespace(send=inputs.append)
        screen._navigate(187)
        screen._navigate(3)
        screen._navigate(4)
        screen._commit(None, 'hello')
        self.assertEqual([event['keycode'] for event in inputs if event['kind'] == 'key'], [187, 187, 3, 3, 4, 4])
        self.assertEqual(inputs[-1], {'kind':'text', 'text':'hello'})
        app = Adw.Application(application_id='org.vivo.MediaTest')
        app.register(None)
        window = MirrorWindow(app, backend)
        window.destroy()

    def test_mirror_fits_portrait_and_landscape_without_empty_frame(self):
        from .media import Adw, MirrorWindow
        Adw.init()
        app = Adw.Application(application_id='org.vokit.MirrorFitTest')
        app.register(None)
        window = MirrorWindow(app, SimpleNamespace(media_socket='/tmp/not-used'))
        try:
            window.present()
            for width, height in ((720, 1560), (1560, 720)):
                window.panel._geometry(width, height)
                target_width, target_height = window.panel.picture.fitted_size
                self.assertTrue(self.spin(lambda: abs(window.panel.picture.get_width() - target_width) <= 2
                                          and abs(window.panel.picture.get_height() - target_height) <= 2))
                self.assertAlmostEqual(target_width / target_height, width / height, delta=.005)
                self.assertLessEqual(abs(window.panel.frame.get_width() - target_width), 2)
                self.assertLessEqual(abs(window.panel.frame.get_height() - target_height), 2)
        finally:
            window.destroy()

    def test_microphone_is_not_created_without_decoded_audio(self):
        from .media import PhoneAudio
        audio = PhoneAudio()
        try:
            self.assertFalse(audio.active)
            self.assertEqual(audio.processes, [])
            with self.assertRaises(ValueError):
                audio.push(b'invalid')
            self.assertEqual(audio.processes, [])
        finally:
            audio.close()

    def test_valid_aac_decodes_before_microphone_publication(self):
        from .media import PhoneAudio
        class SamplesOnly(PhoneAudio):
            def _publish(self):
                pass
        audio = SamplesOnly()
        try:
            audio.push(bytes.fromhex('fff14c40127ffcdc004c61766336332e312e313032000268a359a1288d35874a995f6f8a711e697b92243c2249fffeae481374c37362b4f5b36d748babb83b668efc37b8b056ca9ad3252255cdb519ccb44a44a4489929128181819103031b366c1819122446cd9b368912296596cdc012a2e237044aa9c8d64d8d708cf6d15748ce5d056c8cd392aa1c069929c9a26b9296aefff14c4014dffc013494dac95d964bab25d414e9bfdfceadb5a97ff6f1fbf9d6b8d5ebffe2f5fbf9d6b8d5ebffec5fe3ceb5ad6ac6d4ecdbae79e60d65b2cf70a2f7fb28183740fa50afa503e9f4d6603e9f4fa5007d3e9f4d607d3e9f4d741801f4fa7d3599b006b9eb458b549fcdb748843133e5133e5133e5f288983e5f2e31307cbe5c4983e5f2f94511003e5f2f9718a2005973d67024242424242424242424242424e7fff14c4010bffc013895b2c95d944b9d364bab25ce973faf5c69c3abd7fe93f5eb8be35aaffe9ebdbad5f1abd78ff5f8eb57ad5ec31fb97bfa6a8de5030232b8995f2a15f2a04e3e5f21351d3194d979031335933593df420c1f46c060fa7d00799f4fa0039d83e9f4fa481f1c462ea23698f4c7747747747faae1f129f69f53ea7d4fa9fefff14c40019ffc011881b470'))
            self.assertTrue(self.spin(lambda: not audio.pcm.empty()))
            pcm = audio.pcm.get_nowait()
            self.assertTrue(pcm)
            self.assertEqual(len(pcm) % 2, 0)
            self.assertTrue(any(pcm))
            self.assertEqual(audio.processes, [])
        finally:
            audio.close()

    def test_image_viewer_closes_and_releases_once(self):
        from gi.repository import GdkPixbuf
        from .media import Adw, MediaViewer
        app = Adw.Application(application_id='org.vivo.ViewerTest')
        app.register(None)
        closed = []
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, 'image.png')
            image = GdkPixbuf.Pixbuf.new(GdkPixbuf.Colorspace.RGB, False, 8, 64, 96)
            image.fill(0x45aaffff)
            image.savev(path, 'png', [], [])
            viewer = MediaViewer(app, path, 'image/png', lambda: closed.append(True))
            self.assertTrue(self.spin(lambda: viewer.media.get_paintable() is not None))
            viewer._close()
            viewer._close()
            self.assertEqual(closed, [True])
            viewer.destroy()


class StreamURLTests(unittest.TestCase):
    def test_only_loopback_capability_urls_are_accepted(self):
        from .media import valid_stream_url
        valid = 'http://127.0.0.1:1234/preview/' + 'a' * 64
        self.assertTrue(valid_stream_url(valid))
        for value in (None, '/tmp/image.jpg', valid + '?token=x', valid + '#x',
                      valid.replace('127.0.0.1', 'phone.local'), valid.replace(':1234', ''),
                      valid.replace('127.0.0.1', 'user@127.0.0.1')):
            self.assertFalse(valid_stream_url(value))


@unittest.skipUnless(os.environ.get('DISPLAY') or os.environ.get('WAYLAND_DISPLAY'), 'GTK display required')
class HTTPViewerTests(unittest.TestCase):
    def test_image_stream_in_memory_and_capability_release(self):
        import base64
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
        from .media import Adw, MediaViewer, release_stream
        Adw.init()
        image = base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=')
        released = threading.Event()
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_GET(self):
                self.send_response(200)
                self.send_header('Content-Type', 'image/png')
                self.send_header('Content-Length', str(len(image)))
                self.end_headers()
                self.wfile.write(image)
            def do_DELETE(self):
                released.set()
                self.send_response(204)
                self.end_headers()
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        url = f'http://127.0.0.1:{server.server_port}/preview/' + 'a' * 64
        app = Adw.Application(application_id='org.vivo.StreamViewerTest')
        app.register(None)
        viewer = MediaViewer(app, url, 'image/png', lambda: release_stream(url))
        try:
            self.assertTrue(NativeMediaTests.spin(lambda: viewer.media.get_paintable() is not None))
            self.assertFalse(viewer.message.get_visible())
            viewer._close()
            self.assertTrue(released.wait(2))
        finally:
            viewer._close()
            viewer.destroy()
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    unittest.main()
