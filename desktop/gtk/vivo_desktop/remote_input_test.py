import unittest
from unittest.mock import patch
import json
import os
import queue
import stat
import threading
from types import SimpleNamespace

from .remote_input import RemoteInput, key_arguments, monitor_details, translate


class MonitorSelectionTest(unittest.TestCase):
    def setUp(self):
        self.monitors = [
            {'name': 'HDMI-A-1', 'width': 1920, 'height': 1080, 'x': 5142, 'y': 1640, 'scale': 1},
            {'name': 'eDP-1', 'width': 1920, 'height': 1080, 'x': 4312, 'y': 2720, 'scale': 1.25},
        ]

    def test_identical_monitors_require_identity_or_position(self):
        self.assertIsNone(monitor_details(self.monitors, 1920, 1080))
        self.assertEqual(monitor_details(self.monitors, 1920, 1080, mapping_id='eDP-1'),
                         ((4312, 2720, 1536, 864), 1.25))
        self.assertEqual(monitor_details(self.monitors, 1920, 1080, position=(5142, 1640)),
                         ((5142, 1640, 1920, 1080), 1))

    def test_connector_identity_takes_precedence_over_stream_local_position(self):
        self.assertEqual(monitor_details(self.monitors, 1920, 1080,
                                         position=(0, 0), mapping_id='eDP-1'),
                         ((4312, 2720, 1536, 864), 1.25))

    def test_opaque_identity_and_conflicting_size_do_not_guess(self):
        self.assertIsNone(monitor_details(self.monitors, 1920, 1080, mapping_id='opaque-region'))
        self.assertIsNone(monitor_details(self.monitors, 1280, 720, mapping_id='eDP-1'))

    def test_unique_rotated_monitor_keeps_scaled_coordinates(self):
        self.monitors[0]['transform'] = 1
        self.assertEqual(monitor_details(self.monitors, 1080, 1920),
                         ((5142, 1640, 1080, 1920), 1))


class InputTranslationTest(unittest.TestCase):
    def test_monitor_scaling_and_drag_release(self):
        event = {'type': 'mouse', 'position': {'clientX': 1920, 'clientY': 1080},
                 'view': {'width': 1920, 'height': 1080}, 'button': {'action': 'rightUp'}}
        self.assertEqual(translate(event, (100, 200, 960, 540)),
                         [('move', (1059, 739)), ('button', (273, 'up'))])

    def test_typed_text_is_data_and_shortcuts_are_validated(self):
        text = 'a; exec should stay text'
        self.assertEqual(translate({'type': 'key', 'value': text}, (0, 0, 1, 1)), [('text', text)])
        self.assertEqual(translate({'type': 'key', 'key': 'c', 'modifiers': ['control']}, (0, 0, 1, 1)), [('key', ('CTRL', 'c'))])
        for data in ({'type': 'key', 'key': 'c;exec invalid'},
                     {'type': 'key', 'key': 'c', 'modifiers': ['ctrl,exec invalid']},
                     {'type': 'changeWidgetState', 'name': 'shutdown'},
                     {'type': 'mouse', 'position': {'clientX': float('nan')}, 'view': {}, 'button': {}},
                     {'type': 'key', 'value': '\x00'}):
            self.assertIsNone(translate(data, (0, 0, 1920, 1080)))

    def test_super_aliases_and_compositor_shortcuts_use_virtual_keyboard(self):
        for key in ('win','windows','super','command','meta','logo'):
            self.assertEqual(translate({'type':'key','key':key,'value':key}, (0,0,1,1)), [('key', ('','Super_L'))])
        self.assertEqual(translate({'type':'key','key':'escape','value':'a'}, (0,0,1,1)), [('text', 'a')])
        self.assertEqual(key_arguments('', 'Super_L'), ['wtype','-M','logo','-k','Super_L','-m','logo'])
        self.assertEqual(key_arguments('SUPER SHIFT','e'), ['wtype','-M','logo','-M','shift','-k','e','-m','shift','-m','logo'])
        self.assertEqual(translate({'type':'key','key':'enter','value':'enter'}, (0,0,1,1)), [('text', 'enter')])


class InputDispatchTest(unittest.TestCase):
    def test_safe_no_op_detects_lua_and_legacy_modes(self):
        monitors = [{'name': 'eDP-1', 'width': 1920, 'height': 1080, 'x': 100, 'y': 200}]
        for reply, expected in [('ok', True), ('Invalid dispatcher', False)]:
            with self.subTest(reply=reply), \
                    patch.dict(os.environ, {'HYPRLAND_INSTANCE_SIGNATURE': 'test_123', 'XDG_RUNTIME_DIR': '/tmp'}), \
                    patch('vivo_desktop.remote_input.shutil.which', return_value='/usr/bin/wtype'), \
                    patch('vivo_desktop.remote_input.os.stat', return_value=SimpleNamespace(st_mode=stat.S_IFSOCK, st_uid=os.getuid())), \
                    patch.object(RemoteInput, '_ipc', side_effect=[json.dumps(monitors), reply]) as ipc, \
                    patch('vivo_desktop.remote_input.threading.Thread'):
                backend = RemoteInput(1920, 1080, mapping_id='eDP-1')
                self.assertTrue(backend.available)
                self.assertEqual(backend.lua_dispatch, expected)
                self.assertEqual([call.args[0] for call in ipc.call_args_list],
                                 ['j/monitors', 'dispatch hl.dsp.no_op()'])

    def backend(self, lua):
        backend = RemoteInput.__new__(RemoteInput)
        backend.lua_dispatch = lua
        backend.available = True
        backend.failure = None
        backend.held = set()
        backend.stopping = threading.Event()
        backend.work = queue.Queue()
        return backend

    def test_worker_uses_lua_dispatchers_and_releases_drag_on_stop(self):
        backend = self.backend(True)
        backend.work.put([('move', (5142, 1640)), ('button', (272, 'down')),
                          ('key', ('CTRL SHIFT', 'Return'))])
        commands = []
        def ipc(command):
            commands.append(command)
            return 'ok'
        backend._ipc = ipc
        def typed(*args, **kwargs):
            backend.stopping.set()
            return SimpleNamespace(returncode=0)
        with patch('vivo_desktop.remote_input.subprocess.run', side_effect=typed) as run:
            backend._worker()
        self.assertEqual(run.call_args.args[0], ['wtype','-M','ctrl','-M','shift','-k','Return','-m','shift','-m','ctrl'])
        self.assertEqual(commands, [
            'dispatch hl.dsp.cursor.move({x=5142,y=1640})',
            'dispatch hl.dsp.send_key_state({mods="",key="mouse:272",state="down"})',
            'dispatch hl.dsp.send_key_state({mods="",key="mouse:272",state="up"})',
        ])
        self.assertEqual(backend.held, set())
        self.assertIsNone(backend.failure)

    def test_rejected_legacy_move_reports_fixed_failure_and_releases_button(self):
        backend = self.backend(False)
        backend.work.put([('button', (273, 'down')), ('move', (100, 200))])
        commands = []
        def ipc(command):
            commands.append(command)
            return 'error: private compositor detail' if 'movecursor' in command else 'ok'
        backend._ipc = ipc
        backend._worker()
        self.assertEqual(commands, ['dispatch sendkeystate ,mouse:273,down,',
                                    'dispatch movecursor 100 200',
                                    'dispatch sendkeystate ,mouse:273,up,'])
        self.assertFalse(backend.available)
        self.assertEqual(backend.failure, 'input-move-failed')
        self.assertEqual(backend.held, set())

    def test_text_timeout_is_bounded_and_keeps_text_out_of_ipc(self):
        import subprocess
        backend = self.backend(True)
        text = '"; arbitrary text stays data'
        backend.work.put([('text', text)])
        with patch('vivo_desktop.remote_input.subprocess.run', side_effect=subprocess.TimeoutExpired('wtype', 3)) as run, \
                patch.object(backend, '_ipc') as ipc:
            backend._worker()
        self.assertEqual(run.call_args.args[0], ['wtype', '--', text])
        ipc.assert_not_called()
        self.assertFalse(backend.available)
        self.assertEqual(backend.failure, 'input-text-failed')


if __name__ == '__main__':
    unittest.main()
