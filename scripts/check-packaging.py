#!/usr/bin/env python3
"""Exercise installation in a temporary root without privilege or host changes."""
import os
from pathlib import Path
import subprocess
import tempfile
import xml.etree.ElementTree as ET

root = Path(__file__).resolve().parent.parent
for script in ('install.sh', 'scripts/build-arch-package.sh'):
    subprocess.run(['bash', '-n', str(root / script)], check=True)
ET.parse(root / 'packaging/desktop/vivo-office-kit.svg')
with tempfile.TemporaryDirectory(prefix='vivo-install-check-') as temporary:
    stage = Path(temporary) / 'stage'
    # Staging requires built binaries, and cannot accidentally rebuild or run them.
    result = subprocess.run(['make', '-C', str(root), 'PREFIX=/usr', f'DESTDIR={stage}', 'install-files', 'install-system-config'], check=True, capture_output=True, text=True)
    expected = {
        'usr/bin/vivolinux', 'usr/bin/vokit', 'usr/bin/vivo-desktop', 'usr/bin/vivo-desktop-helper',
        'usr/share/applications/vivo-office-kit.desktop',
        'usr/share/icons/hicolor/scalable/apps/vivo-office-kit.svg',
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
        'usr/lib/vivo-office-kit/vivo_desktop/style.css',
    }
    actual = {str(path.relative_to(stage)) for path in stage.rglob('*') if path.is_file()}
    assert actual == expected, (actual - expected, expected - actual)
    for name in expected:
        mode = (stage / name).stat().st_mode & 0o7777
        assert mode == (0o755 if name.startswith('usr/bin/') else 0o644), (name, oct(mode))
    subprocess.run([str(root / 'install.sh'), '--dry-run', '--destdir', str(stage)], check=True, capture_output=True)
    rejected = subprocess.run([str(root / 'install.sh'), '--destdir', '/'], capture_output=True)
    assert rejected.returncode != 0
    rejected = subprocess.run([str(root / 'install.sh'), '--destdir', '/tmp/..'], capture_output=True)
    assert rejected.returncode != 0
rule = (root / 'packaging/udev/70-vivo-phone-camera.rules').read_text()
assert 'ATTR{name}=="Vivo Phone"' in rule and 'TAG+="uaccess"' in rule
assert 'MODE="0666"' not in rule and 'GROUP=' not in rule
print('Packaging checks passed: 17 staged files, permissions, GTK launchers, installer validation.')
