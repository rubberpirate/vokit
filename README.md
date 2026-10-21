# vivo Office Kit for Linux

An independent native Linux companion for compatible vivo phones, built with Go and Qt 6. It does not require Wine or USB debugging. This project is not affiliated with or endorsed by vivo.

Connect the laptop and phone to the same network, open the desktop app, and scan its QR with the phone's Office Kit connection feature. Approve the requested features on the phone. The application can stay connected in the system tray when its main window is closed.

Implemented features include file browsing and upload/download, image/video thumbnails and viewing, text clipboard sync, phone screen mirroring with touch and typing, and front/rear camera sharing. The X300 FE with OriginOS 6 has been tested; 720p screen mirroring was reported smooth. Camera latency varies with network and encoder settings. Compatibility with every phone, Linux distribution and laptop is not established.

Camera sharing publishes to a v4l2loopback device, normally `/dev/video10` named **Vivo Phone**. Select that camera in the video-call application. Local preview can be hidden independently of the webcam stream; this passed a live 90-frame Linux consumer check on the X300 FE. Audio sharing uses a PipeWire virtual source when the phone actually provides audio; the tested X300 FE advertised audio but did not send microphone packets in this run, so phone microphone sharing is not yet confirmed. Linux virtual microphones are selected by name in meeting applications, rather than through a separate `/dev/video` node. Notification forwarding, account login, Notes/cloud sync and Remote PC are not complete.

## Install

Arch Linux / CachyOS and Debian / Ubuntu:

```sh
./install.sh --check --with-webcam
./install.sh --with-webcam
vivo-desktop
```

Run the installer as your regular desktop user. It uses sudo for dependencies and installation, builds as your user, and installs the application under `/usr/local`. Webcam setup is enabled by default; use `--without-webcam` if you do not need a virtual camera. Debian/Ubuntu needs a release with Go 1.23+ and Qt 6.2+ packages. Dependency installation needs internet access; no distro repository is added.

Webcam installation requests v4l2loopback DKMS and headers matching the running kernel, preserves existing configuration, and grants the active local session access only to the **Vivo Phone** virtual camera through a udev `uaccess` rule. It attempts to load the module without stopping existing cameras. A reboot may be needed after kernel/header changes. Secure Boot may require your distribution's module-signing procedure. Phone consent and Wayland portal prompts require your interactive approval. The app never runs as root, changes audio defaults or edits the firewall.

For an Arch package built from this local source snapshot:

```sh
./scripts/build-arch-package.sh
cd build/arch-package
makepkg -si
```

The generated PKGBUILD has a checksum for the generated source archive; it does not download vendor installers. Install `v4l2loopback-dkms` and the matching kernel headers for webcam output. The package installs camera configuration and a scoped udev rule; reboot or load `v4l2loopback` after installation. Existing loaded drivers are not reconfigured automatically.

## Build

Build dependencies: Go 1.23+, a C++17 compiler, Make, CMake, pkg-config; Qt 6 Core, Gui, DBus, Widgets, Network, Multimedia and MultimediaWidgets development files; FFmpeg libavcodec, libavutil, libswscale and libswresample development files. Runtime helpers: `qrencode`, `wl-clipboard` on Wayland, `xdg-open`, and PipeWire's `pw-loopback` / `pw-cat` for audio sharing. Qt SVG/Wayland plugins provide icon and Wayland integration. ADB/scrcpy are optional command-line fallbacks.

```sh
make build desktop
./build/vivo-desktop
make test
make check-packaging
```

To stage an already built installation without privileges or host changes:

```sh
./install.sh --destdir /tmp/vivo-package-stage
```

`make install-app PREFIX="$HOME/.local"` installs application files for one user; it does not configure the kernel camera module. The GUI, CLI and desktop helper must remain together in the same `bin` directory. Explicit Quit stops sessions owned by the desktop; tray hiding does not.

The initial Go build downloads the pinned `github.com/coder/websocket` dependency. Qt and FFmpeg are dynamically linked system dependencies. See `THIRD_PARTY_NOTICES` for dependency notices. No project redistribution license has been selected yet; select one before distributing a public release. Vendor binaries, installers, captured sessions and research evidence are excluded from the application source package.
