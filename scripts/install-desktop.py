#!/usr/bin/env python3
"""Install the app-ID launcher with absolute executable paths, including user installs."""
import os
from pathlib import Path
import sys

prefix, stage, source = sys.argv[1:]
executable = str(Path(prefix) / 'bin/vokit')
# Desktop Entry Exec uses its own escaping, not shell argument parsing.
quoted = '"' + ''.join('\\' + char if char in '\\"`$' else char for char in executable) + '"'
# Backslashes are first decoded as Desktop Entry string escapes, then by Exec.
escaped = quoted.replace('\\', '\\\\').replace('%', '%%')
text = Path(source).read_text()
text = text.replace('Exec=vokit %F\n', f'Exec={escaped} %F\n')
try_exec = executable.replace('\\', '\\\\')
text = text.replace('TryExec=vokit\n', f'TryExec={try_exec}\n')
target = Path(stage + prefix) / 'share/applications/io.github.vivolinux.OfficeKit.desktop'
target.parent.mkdir(parents=True, exist_ok=True)
target.write_text(text)
os.chmod(target, 0o644)
