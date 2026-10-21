#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
version=0.3.0
output=${1:-build/arch-package}
mkdir -p -- "$output"
output=$(realpath -- "$output")
archive="$output/vivo-office-kit-linux-$version.tar.gz"
# Explicit application source roots keep vendor installers, captures and credentials out.
python3 - "$archive" "$version" <<'PY'
import pathlib, sys, tarfile
root = pathlib.Path.cwd()
with tarfile.open(sys.argv[1], 'w:gz') as archive:
    for name in ('Makefile', 'README.md', 'THIRD_PARTY_NOTICES', 'go.mod', 'go.sum', 'install.sh', 'cmd', 'internal', 'desktop', 'packaging', 'scripts'):
        entry = root / name
        paths = [entry] if entry.is_file() else sorted(entry.rglob('*'))
        for path in paths:
            if not path.is_file() or path.is_symlink() or '__pycache__' in path.parts:
                continue
            if entry.is_dir() and path.suffix not in ('.go', '.py', '.css', '.sh', '.desktop', '.svg', '.rules', '.conf', '.install') and path.name not in ('PKGBUILD', 'vivo-desktop', 'vivo-desktop-helper'):
                continue
            archive.add(path, arcname=f'vivo-office-kit-linux-{sys.argv[2]}/{path.relative_to(root)}', recursive=False)
PY
cp packaging/arch/PKGBUILD "$output/PKGBUILD"
cp packaging/arch/vivo-office-kit-linux.install "$output/vivo-office-kit-linux.install"
digest=$(sha256sum "$archive"); digest=${digest%% *}
sed -i "s/sha256sums=('SKIP')/sha256sums=('$digest')/" "$output/PKGBUILD"
echo "Source package prepared in $output"
echo "Run: cd '$output' && makepkg -si"
