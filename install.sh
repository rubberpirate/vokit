#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
mode=install
webcam=1
stage=
while (($#)); do
    case "$1" in
        --check|--dry-run) mode=check ;;
        --with-webcam) webcam=1 ;;
        --without-webcam) webcam=0 ;;
        --destdir) shift; stage=${1:?--destdir needs a directory} ;;
        --help) echo 'Usage: ./install.sh [--check|--dry-run] [--with-webcam|--without-webcam] [--destdir ABSOLUTE_DIRECTORY]'; exit 0 ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
    shift
done
if [[ -n "$stage" ]]; then
    [[ "$stage" = /* ]] || { echo 'DESTDIR must be an absolute staging directory other than /.' >&2; exit 2; }
    stage=$(realpath -m -- "$stage")
    [[ "$stage" != / ]] || { echo 'DESTDIR must not resolve to the system root.' >&2; exit 2; }
    [[ "$mode" != check ]] || { echo "Would stage application and camera configuration under $stage"; exit 0; }
    make PREFIX=/usr DESTDIR="$stage" install-files install-system-config
    exit 0
fi
# Build as the desktop user; elevate only package management and file installation.
[[ $EUID != 0 ]] || { echo 'Run as your regular desktop user; the installer uses sudo when needed.' >&2; exit 2; }
. /etc/os-release
family=" ${ID:-} ${ID_LIKE:-} "
if [[ "$family" == *arch* || "$family" == *cachyos* ]]; then
    packages=(go make python-gobject python-cairo gtk4 libadwaita gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad gst-libav gst-plugin-gtk4 qrencode wl-clipboard pipewire-audio xdg-utils)
    pkgcmd=(sudo pacman -S --needed)
    if ((webcam)); then
        kernelbase=$(cat "/usr/lib/modules/$(uname -r)/pkgbase" 2>/dev/null || true)
        [[ "$kernelbase" =~ ^[a-zA-Z0-9._+-]+$ ]] || { echo 'Cannot determine running kernel package; install matching kernel headers first.' >&2; exit 1; }
        packages+=(v4l2loopback-dkms "${kernelbase}-headers")
    fi
elif [[ "$family" == *debian* || "$family" == *ubuntu* ]]; then
    packages=(golang-go make python3-gi python3-gi-cairo gir1.2-gtk-4.0 gir1.2-adw-1 gir1.2-gstreamer-1.0 gir1.2-gst-plugins-base-1.0 libgtk-4-media-gstreamer gstreamer1.0-gtk4 gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-bad gstreamer1.0-libav qrencode wl-clipboard pipewire-bin xdg-utils)
    pkgcmd=(sudo apt-get install)
    ((webcam == 0)) || packages+=(v4l2loopback-dkms "linux-headers-$(uname -r)")
else
    echo 'Automatic dependency installation supports Arch/CachyOS and Debian/Ubuntu.' >&2
    echo 'Install dependencies from README.md, then use make install-app PREFIX=/usr/local.' >&2
    exit 2
fi
printf 'Dependency command:'; printf ' %q' "${pkgcmd[@]}" "${packages[@]}"; printf '\n'
if [[ "$mode" == check ]]; then
    echo 'Would build as current user and install application under /usr/local.'
    ((webcam == 0)) || echo 'Would preserve existing camera configuration, install uaccess rule and attempt a non-destructive module load.'
    exit 0
fi
"${pkgcmd[@]}" "${packages[@]}"
go_version=$(go env GOVERSION)
[[ "$(printf '%s\n' go1.23 "$go_version" | sort -V | head -n1)" == go1.23 ]] || { echo 'Go 1.23 or later is required; install a newer Go toolchain.' >&2; exit 1; }
make build desktop
sudo make PREFIX=/usr/local install-files
if ((webcam)); then
    # Existing administrator configuration always wins, including a different video number.
    for entry in 'udev/70-vivo-phone-camera.rules:/usr/lib/udev/rules.d/70-vivo-phone-camera.rules' 'modprobe/vivo-phone-camera.conf:/etc/modprobe.d/vivo-phone-camera.conf' 'modules-load/vivo-phone-camera.conf:/etc/modules-load.d/vivo-phone-camera.conf'; do
        source_file=${entry%%:*}; destination=${entry#*:}
        if [[ "$source_file" == modprobe/* ]] && sudo grep -RqsE '^[[:space:]]*options[[:space:]]+v4l2loopback[[:space:]]' /etc/modprobe.d /usr/lib/modprobe.d; then
            echo "Preserving existing v4l2loopback module options; select its existing video node in the app."
        elif sudo test -e "$destination"; then
            echo "Preserving $destination"
        else
            sudo install -Dm644 "packaging/$source_file" "$destination"
        fi
    done
    sudo udevadm control --reload-rules
    sudo udevadm trigger --subsystem-match=video4linux
    if [[ -d /sys/module/v4l2loopback ]]; then
        echo 'v4l2loopback is already loaded; no devices were stopped or reconfigured. Select the Vivo Phone node in the app.'
    elif ! sudo modprobe v4l2loopback; then
        echo 'Camera module could not load. Check matching headers/DKMS and Secure Boot signing, then reboot. The app remains installed.' >&2
    fi
fi
echo 'Installed. Launch VOkit from your application menu or run vokit as your regular user.'
echo 'Phone consent prompts and Wayland portal prompts must be approved interactively. No audio defaults or firewall settings were changed.'
