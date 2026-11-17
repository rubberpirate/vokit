#!/usr/bin/env bash
# Staged packages must never refresh the host desktop or write cache artifacts.
set -euo pipefail
prefix=${1:?installation prefix required}
stage=${2:-}
[[ -z "$stage" ]] || exit 0
if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$prefix/share/applications" || echo 'VOkit: desktop cache refresh failed; the launcher is installed.' >&2
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
    gtk-update-icon-cache --force --ignore-theme-index "$prefix/share/icons/hicolor" || echo 'VOkit: icon cache refresh failed; icons are installed.' >&2
fi
