#!/usr/bin/env python3
"""Exercise installation in a temporary root without privilege or host changes."""
import os
import configparser
import json
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile

root = Path(__file__).resolve().parent.parent
for script in ('install.sh', 'scripts/build-arch-package.sh', 'scripts/refresh-desktop.sh'):
    subprocess.run(['bash', '-n', str(root / script)], check=True)
icon_sizes = (16, 24, 32, 48, 64, 128, 256, 512, 1024)
app_id = 'io.github.vivolinux.OfficeKit'
with tempfile.TemporaryDirectory(prefix='vivo-install-check-') as temporary:
    stage = Path(temporary) / 'stage'
    old_launcher = stage / 'usr/share/applications/vivo-office-kit.desktop'
    old_launcher.parent.mkdir(parents=True)
    old_launcher.write_text('[Desktop Entry]\nName=Old VOkit\n')
    # Staging requires built binaries, and cannot accidentally rebuild or run them.
    result = subprocess.run(['make', '-C', str(root), 'PREFIX=/usr', f'DESTDIR={stage}', 'install-files', 'install-system-config'], check=True, capture_output=True, text=True)
    expected = {
        'usr/share/Thunar/sendto/vokit.desktop', 'usr/bin/vivolinux', 'usr/bin/vokit', 'usr/bin/vivo-desktop', 'usr/bin/vivo-desktop-helper',
        f'usr/share/applications/{app_id}.desktop',
        'usr/share/doc/vivo-office-kit/README.md',
        'usr/share/licenses/vivo-office-kit/THIRD_PARTY_NOTICES',
        'usr/lib/udev/rules.d/70-vivo-phone-camera.rules',
        'etc/modprobe.d/vivo-phone-camera.conf',
        'etc/modules-load.d/vivo-phone-camera.conf',
        'usr/lib/vivo-office-kit/vivo_desktop/__init__.py',
        'usr/lib/vivo-office-kit/vivo_desktop/app.py',
        'usr/lib/vivo-office-kit/vivo_desktop/backend.py',
        'usr/lib/vivo-office-kit/vivo_desktop/helper.py',
        'usr/lib/vivo-office-kit/vivo_desktop/media.py',
        'usr/lib/vivo-office-kit/vivo_desktop/tray.py',
        'usr/lib/vivo-office-kit/vivo_desktop/i18n.py',
        'usr/bin/vivo-bluetooth-helper',
        'usr/lib/vivo-office-kit/vivo_desktop/bluetooth_transport.py',
        'usr/lib/vivo-office-kit/vivo_desktop/style.css',
    }
    expected.update(f'usr/share/icons/hicolor/{size}x{size}/apps/{app_id}.png' for size in icon_sizes)
    direct_root = 'usr/lib/vivo-office-kit/wifi-direct/'
    expected.update(direct_root + name for name in (
        'tools/install_p2p_service.py', 'tools/p2p_service.py', 'tools/p2p_persistent.py',
        'tools/p2p_helper.py', 'tools/p2p_private.py', 'tools/wifi_capabilities.py',
        'tools/p2p_supplicant.config', 'tools/build_p2p_supplicant.py',
        'patches/wpa_supplicant-2.12-private-go.patch', 'packaging/systemd/vivo-connect-p2p.service',
        'build/private-p2p/bin/wpa_supplicant', 'build/private-p2p/bin/build.json',
        'build/private-p2p/bin/COPYING'))
    subprocess.run(['python3', '-I', str(stage / direct_root / 'tools/install_p2p_service.py'),
                    'install', '--user', 'nobody', '--interface', 'wlan0', '--state-dir', '/tmp/vokit-test',
                    '--destdir', str(stage / 'service-preview')], check=True, capture_output=True)
    shutil.rmtree(stage / 'service-preview')
    for language in ('zh_CN', 'es', 'fr', 'de', 'ja'):
        name = f'usr/lib/vivo-office-kit/vivo_desktop/locales/{language}.json'
        expected.add(name)
        catalog = json.loads((stage / name).read_text())
        assert len(catalog) >= 200 and catalog['Settings'] != 'Settings'
    actual = {str(path.relative_to(stage)) for path in stage.rglob('*') if path.is_file()}
    assert actual == expected, (actual - expected, expected - actual)
    assert not old_launcher.exists(), 'upgrade left a duplicate menu entry'
    launcher = stage / f'usr/share/applications/{app_id}.desktop'
    entry = configparser.ConfigParser(interpolation=None)
    entry.read(launcher)
    desktop = entry['Desktop Entry']
    assert desktop['Exec'] == '"/usr/bin/vokit" %F'
    assert desktop['TryExec'] == '/usr/bin/vokit'
    assert desktop['Icon'] == app_id and desktop['StartupWMClass'] == app_id
    assert '互传' in desktop['Keywords[zh_CN]']
    if shutil.which('desktop-file-validate'):
        subprocess.run(['desktop-file-validate', str(launcher)], check=True)
    for size in icon_sizes:
        png = stage / f'usr/share/icons/hicolor/{size}x{size}/apps/{app_id}.png'
        header = png.read_bytes()[:26]
        assert header[:8] == b'\x89PNG\r\n\x1a\n'
        assert struct.unpack('>II', header[16:24]) == (size, size)
        assert header[25] == 6, 'app icon must preserve RGBA transparency'
    # Refreshing a staged installation must not invoke any host cache tools.
    subprocess.run(['make', '-C', str(root), 'PREFIX=/usr', f'DESTDIR={stage}', 'refresh-desktop'], check=True, capture_output=True)
    assert not (stage / 'usr/share/icons/hicolor/icon-theme.cache').exists()
    for name in expected:
        mode = (stage / name).stat().st_mode & 0o7777
        assert mode == (0o755 if name.startswith('usr/bin/') or name.endswith('/wpa_supplicant') else 0o644), (name, oct(mode))
    subprocess.run([str(root / 'install.sh'), '--dry-run', '--destdir', str(stage)], check=True, capture_output=True)
    rejected = subprocess.run([str(root / 'install.sh'), '--destdir', '/'], capture_output=True)
    assert rejected.returncode != 0
    rejected = subprocess.run([str(root / 'install.sh'), '--destdir', '/tmp/..'], capture_output=True)
    assert rejected.returncode != 0
    system_command = Path(temporary) / 'system-vokit'
    system_command.write_text('#!/bin/sh\n')
    system_command.chmod(0o755)
    result = subprocess.run(['make', '-C', str(root), f'SYSTEM_VOKIT={system_command}',
                             f'PREFIX={Path.home() / ".local"}', 'check-install-conflicts'], capture_output=True)
    assert result.returncode != 0 and b'override' in result.stderr
    subprocess.run(['make', '-C', str(root), f'SYSTEM_VOKIT={system_command}',
                    f'PREFIX={Path.home() / ".local"}', f'DESTDIR={stage}', 'check-install-conflicts'],
                   check=True, capture_output=True)
rule = (root / 'packaging/udev/70-vivo-phone-camera.rules').read_text()
assert 'ATTR{name}=="Vivo Phone"' in rule and 'TAG+="uaccess"' in rule
assert 'MODE="0666"' not in rule and 'GROUP=' not in rule
print(f'Packaging checks passed: {len(expected)} staged files, multi-size RGBA icons, launcher identity, upgrade cleanup and cache staging.')
