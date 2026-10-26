#!/bin/sh
set -eu

if [ "$(uname -s)" != Darwin ]; then
  printf '%s\n' 'macOS is required to build the native VOkit app.' >&2
  exit 1
fi

for tool in go swiftc; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf '%s\n' "Missing build tool: $tool" >&2
    exit 1
  fi
done

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
app="${VOKIT_MACOS_APP_PATH:-$root/build/VOkit.app}"
mode="${VOKIT_MACOS_BUILD_MODE:-release}"
version="${VOKIT_MACOS_VERSION:-}"
if [ -z "$version" ]; then
  version=$(sed -n 's/^pkgver=//p' "$root/packaging/arch/PKGBUILD")
fi
build_number="${VOKIT_MACOS_BUILD_NUMBER:-1}"

case "$mode" in
  release) swift_optimization=-O ;;
  debug) swift_optimization=-Onone ;;
  *) printf '%s\n' "Invalid macOS build mode: $mode (expected release or debug)" >&2; exit 1 ;;
esac
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  printf '%s\n' "Invalid macOS version: $version" >&2
  exit 1
fi
case "$build_number" in
  ''|*[!0-9]*) printf '%s\n' "Invalid macOS build number: $build_number" >&2; exit 1 ;;
esac

mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$root/macos/Info.plist" "$app/Contents/Info.plist"
plutil -replace CFBundleShortVersionString -string "$version" "$app/Contents/Info.plist"
plutil -replace CFBundleVersion -string "$build_number" "$app/Contents/Info.plist"
(
  cd "$root"
  if [ "$mode" = debug ]; then
    CGO_ENABLED=0 go build -gcflags='all=-N -l' -o "$app/Contents/Resources/vivolinux" ./cmd/vivolinux
  else
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$app/Contents/Resources/vivolinux" ./cmd/vivolinux
  fi
)
swiftc -target "$(uname -m)-apple-macos13.0" -parse-as-library "$swift_optimization" \
  -framework AppKit -framework SwiftUI -framework Network \
  -framework AVFoundation -framework VideoToolbox -framework CoreMedia \
  -o "$app/Contents/MacOS/VOkit" "$root"/macos/*.swift
helper="$app/Contents/Resources/VOkitHelper.app"
mkdir -p "$helper/Contents/MacOS"
cp "$root/macos/helper/Info.plist" "$helper/Contents/Info.plist"
plutil -replace CFBundleShortVersionString -string "$version" "$helper/Contents/Info.plist"
plutil -replace CFBundleVersion -string "$build_number" "$helper/Contents/Info.plist"
swiftc -target "$(uname -m)-apple-macos13.0" -parse-as-library "$swift_optimization" \
  -framework AppKit -framework UserNotifications \
  -o "$helper/Contents/MacOS/VOkitHelper" \
  "$root/macos/helper/PhoneDesktopHelper.swift"
codesign --force --sign - "$helper"
codesign --force --sign - "$app/Contents/Resources/vivolinux"
codesign --force --sign - "$app"
codesign --verify --deep --strict "$app"
printf 'Built: %s\n' "$app"
printf '%s\n' 'Install qrencode to use QR pairing: brew install qrencode'
