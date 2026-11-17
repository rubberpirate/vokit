import ast
import json
import os
from pathlib import Path
from string import Formatter
import unittest
from unittest.mock import patch

from . import i18n


class LocaleTests(unittest.TestCase):
    def test_system_locale_precedence_and_fallback(self):
        for env, expected in (
            ({'LANG': 'zh_CN.UTF-8'}, 'zh_CN'),
            ({'LANG': 'en_US.UTF-8', 'LC_MESSAGES': 'ja_JP.UTF-8'}, 'ja'),
            ({'LANG': 'zh_CN.UTF-8', 'LC_MESSAGES': 'ja_JP.UTF-8', 'LC_ALL': 'de_DE.UTF-8'}, 'de'),
            ({'LANG': 'fr_CA.UTF-8'}, 'fr'),
            ({'LANG': 'es_MX.UTF-8'}, 'es'),
            ({'LANG': 'en_US.UTF-8', 'LANGUAGE': 'it:fr:en'}, 'fr'),
            ({'LANG': 'zh-Hans-CN'}, 'zh_CN'),
            ({'LANG': 'zh_TW.UTF-8'}, 'zh_CN'),
            ({'LANG': 'C.UTF-8', 'LANGUAGE': 'zh_CN'}, 'zh_CN'),
            ({'LANG': 'zh_CN.UTF-8', 'LC_ALL': 'C.UTF-8', 'LANGUAGE': 'zh_CN:en_US'}, 'zh_CN'),
            ({'LANG': 'C', 'LANGUAGE': 'zh_CN'}, 'en'),
            ({'LANG': 'ja_JP.UTF-8', 'LC_ALL': 'POSIX'}, 'en'),
            ({'LANG': 'it_IT.UTF-8'}, 'en'),
            ({}, 'en'),
        ):
            with self.subTest(env=env):
                self.assertEqual(i18n.resolve_language('system', env), expected)
        self.assertEqual(i18n.resolve_language('en', {'LANG': 'zh_CN.UTF-8'}), 'en')
        self.assertEqual(i18n.resolve_language('ja', {'LC_ALL': 'C'}), 'ja')
        self.assertEqual(i18n.resolve_language('bad', {'LANG': 'fr_FR.UTF-8'}), 'fr')

    def test_all_catalogs_cover_source_and_preserve_format_fields(self):
        root = Path(__file__).parent
        messages = set()
        for name in ('app', 'media', 'backend', 'helper', 'tray'):
            for node in ast.walk(ast.parse((root / (name + '.py')).read_text())):
                if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                        and node.func.id in ('_', 'N_') and node.args
                        and isinstance(node.args[0], ast.Constant)):
                    messages.add(node.args[0].value)
        self.assertGreater(len(messages), 200)
        def fields(text):
            return sorted((field, spec, conversion) for _, field, spec, conversion in Formatter().parse(text) if field is not None)
        for code, name in i18n.LANGUAGES:
            if code in ('system', 'en'):
                continue
            catalog = json.loads((root / 'locales' / (code + '.json')).read_text())
            # Catalogs can retain dormant messages across UI/install variants.
            self.assertFalse(messages - set(catalog), (code, messages - set(catalog)))
            for message, translation in catalog.items():
                self.assertIsInstance(translation, str)
                self.assertTrue(translation.strip())
                self.assertEqual(fields(message), fields(translation), (code, message))
                # File names, commands, widget IDs and protocol keys are never translated.
                self.assertFalse(message.startswith('--'), message)
                self.assertFalse(message.endswith('-symbolic'), message)

    def test_missing_catalog_falls_back_to_english(self):
        with patch.dict(os.environ, {'VOKIT_LANGUAGE': 'system'}):
            try:
                with patch.object(Path, 'read_text', side_effect=OSError):
                    i18n.configure('ja')
                self.assertEqual(i18n.gettext('Settings'), 'Settings')
            finally:
                i18n.configure('en')

    def test_language_does_not_persist_connection_secrets(self):
        from .backend import Settings
        import tempfile
        with tempfile.TemporaryDirectory() as folder:
            settings = Settings(Path(folder) / 'preferences.json')
            settings.set('language', 'ja')
            settings.set('region', 'china')
            settings.set('token', 'must-not-persist')
            settings.save()
            loaded = Settings(settings.path)
            self.assertEqual(loaded.get('language'), 'ja')
            self.assertEqual(loaded.get('region'), 'china')
            self.assertIsNone(loaded.get('token'))


class LocalizedWindowTests(unittest.TestCase):
    def test_all_six_languages_build_main_and_media_windows(self):
        from .app import Adw, OfficeWindow, _TestBackend, _TestSettings
        from .media import MediaPanel, MirrorWindow
        with patch.dict(os.environ, {'VOKIT_LANGUAGE': 'system'}):
            try:
                for code, language in i18n.LANGUAGES:
                    if code == 'system':
                        continue
                    with self.subTest(language=code):
                        i18n.configure(code)
                        app = Adw.Application(application_id='org.vokit.LocaleTest.' + code)
                        app.backend, app.settings = _TestBackend(), _TestSettings()
                        app.register(None)
                        window = OfficeWindow(app)
                        mirror = MirrorWindow(app, app.backend)
                        camera = MediaPanel(app.backend, mode='camera')
                        try:
                            self.assertEqual(window.connect_button.get_label(), i18n.gettext('Connect phone'))
                            self.assertEqual(window.nav_labels[0].get_text(), i18n.gettext('Home'))
                            self.assertEqual(window.language_row.get_title(), i18n.gettext('Language'))
                            self.assertEqual(window.category_dropdown.get_model().get_string(0), i18n.gettext('Recent files'))
                            self.assertEqual(mirror.get_title(), i18n.gettext('Phone mirror'))
                            self.assertEqual(camera.start_button.get_label(), i18n.gettext('Start camera'))
                            window._pair_event({'stage': 'incoming_share_started', 'count': 3})
                            self.assertEqual(window.receipt.get_text(), i18n.gettext('Receiving {value0} phone files…').format(value0=3))
                            window.language_row.set_selected(3)  # Spanish; pairing region stays independent.
                            self.assertEqual(app.settings.get('language'), 'es')
                            self.assertEqual(window.region_row.get_selected(), 0)
                        finally:
                            camera.stop()
                            mirror.destroy()
                            window.shutdown()
                            window.destroy()
                            app.backend.close()
            finally:
                i18n.configure('en')
