# VOkit

An independent native Linux companion for compatible vivo phones, built with Go, GTK4 and libadwaita. GStreamer performs video and audio decoding in native libraries. It does not require Wine or USB debugging. This project is not affiliated with or endorsed by vivo.

Connect the laptop and phone to the same network, open the desktop app, click **Connect phone**, and scan its QR with the phone's Office Kit connection feature. No phone IP entry or vivo-account sign-in is required: the fresh QR scan supplies the phone's private network address through vivo's rendezvous service. This QR bootstrap needs internet access. An optional IP override is available under Settings → Advanced connection. Approve the requested features on the phone. Closing the main window hides it while the connection and incoming transfers continue. Open the app again to reveal the same window; use the menu’s Quit action to stop the session.

Implemented features include file browsing and upload/download, image/video thumbnails and viewing, text clipboard sync, phone screen mirroring with touch and typing, and front/rear camera sharing. The X300 FE with OriginOS 6 has been tested; 720p screen mirroring was reported smooth. Camera latency varies with network and encoder settings. Compatibility with every phone, Linux distribution and laptop is not established.

The desktop shows the connected phone’s name and automatically leaves Settings after pairing. Settings has a close button. It uses a dark File Manager layout with the phone's global Recent, Images, Videos, Audio and Documents collections. More results load as you scroll; search applies to loaded results. Image previews fit the whole image as the window resizes, and videos open in a resizable player with full-screen support. The separate phone mirror keeps touch and typing enabled while streaming; quality and start/stop controls are in its three-dot menu. Its frame follows the stream’s aspect ratio in portrait and landscape. Corner checkboxes select multiple files; the toolbar checkbox selects all loaded files. Save multiple files to a folder in one operation; existing files are preserved with numbered filenames.

Phone Share transfers are received automatically while the paired app runs, including while the main window is hidden. Multi-file and repeated shares were verified on the X300 FE. The default destination is `~/Downloads/Vivo Office Kit`; change it in Settings before the next connection. Existing files are preserved, and repeated names receive a suffix. The receiver accepts regular files, with limits of 1 GiB per file, 200 files / 4 GiB per batch, and four queued batches. No additional receive prompt is required on the PC. Each file uses its own authenticated, pinned data channel to the paired phone; it never refreshes certificate trust or retries a failed request within that channel. Opening media uses the phone’s direct media endpoint through a capability-protected loopback stream, without first downloading a complete file or keeping a disk cache. Images are decoded in memory (up to 32 MiB); videos stream progressively and support seeking (up to 1 GiB). Format support depends on installed decoders. Phones that ignore byte ranges require extra network reads when seeking. Explicit Save and phone Share transfers still write to the chosen download directory.

Camera sharing publishes to a v4l2loopback device, normally `/dev/video10` named **Vivo Phone**. Select that camera in the video-call application. Local preview can be hidden independently of the webcam stream; this passed a live 90-frame Linux consumer check on the X300 FE. Audio sharing uses a PipeWire virtual source when the phone actually provides audio; the tested X300 FE advertised audio but did not send microphone packets in this run, so phone microphone sharing is not yet confirmed. Linux virtual microphones are selected by name in meeting applications, rather than through a separate `/dev/video` node. Enable “Phone and connection notifications” before pairing to forward new phone notifications to the desktop. The phone must allow notification handoff. Notification text is bounded and escaped, and is excluded from diagnostic logs. Vivo-account login is not implemented. The vendor client uses region-specific web sign-in and a login callback, then authenticated device registration/discovery and cloud connection messages; those need a separate implementation and a successful interactive sign-in test. Notes/cloud sync and Remote PC are also not complete.

## Install

Arch Linux / CachyOS and Debian / Ubuntu:

```sh
./install.sh --check --with-webcam
./install.sh --with-webcam
vokit
```

Thunar users can right-click selected files and choose **Send To → Send to phone with VOkit**. This accepts multiple files and queues them until the phone connects. You can also run `vokit /path/to/file1 /path/to/file2` or choose multiple files with the app’s Import button. Files go to the phone’s Download folder with their original names; name collisions request a renamed copy.

Run the installer as your regular desktop user. It uses sudo for dependencies and installation, builds as your user, and installs the application under `/usr/local`. Webcam setup is enabled by default; use `--without-webcam` if you do not need a virtual camera. Debian/Ubuntu needs a release with Go 1.23+, GTK 4.16+ and libadwaita 1.6+ packages. Dependency installation needs internet access; no distro repository is added.

Webcam installation requests v4l2loopback DKMS and headers matching the running kernel, preserves existing configuration, and grants the active local session access only to the **Vivo Phone** virtual camera through a udev `uaccess` rule. It attempts to load the module without stopping existing cameras. A reboot may be needed after kernel/header changes. Secure Boot may require your distribution's module-signing procedure. Phone consent and Wayland portal prompts require your interactive approval. The app never runs as root, changes audio defaults or edits the firewall.

For an Arch package built from this local source snapshot:

```sh
./scripts/build-arch-package.sh
cd build/arch-package
makepkg -si
```

The generated PKGBUILD has a checksum for the generated source archive; it does not download vendor installers. Install `v4l2loopback-dkms` and the matching kernel headers for webcam output. The package installs camera configuration and a scoped udev rule; reboot or load `v4l2loopback` after installation. Existing loaded drivers are not reconfigured automatically.

## Build

Build dependencies: Go 1.23+ and Make. Runtime dependencies: Python 3 with PyGObject, GTK 4.16+, libadwaita 1.6+, GStreamer with base/good/bad/libav plugins, `qrencode`, `wl-clipboard` on Wayland, and PipeWire tools for optional audio sharing. Install the GStreamer GTK4 sink (`gst-plugin-gtk4` on Arch, `gstreamer1.0-gtk4` on Debian/Ubuntu) for the media viewer. Screen and camera preview also include a bounded RGB fallback. Tests use Xvfb and xauth. ADB/scrcpy remain optional command-line fallbacks.

```sh
make build desktop
./build/vokit
make test
make test-desktop
make check-packaging
```

To stage an already built installation without privileges or host changes:

```sh
./install.sh --destdir /tmp/vivo-package-stage
```

`make install-app PREFIX="$HOME/.local"` installs application files for one user; it does not configure the kernel camera module. The GTK launchers and Go CLI live in `bin`; Python frontend modules live in `lib/vivo-office-kit`. Explicit Quit stops sessions owned by the desktop; hiding the window does not.

The initial Go build downloads the pinned `github.com/coder/websocket` dependency. GTK, libadwaita, PyGObject and GStreamer are system dependencies; no pip downloads are required. See `THIRD_PARTY_NOTICES` for dependency notices. No project redistribution license has been selected yet; select one before distributing a public release. Vendor binaries, installers, captured sessions and research evidence are excluded from the application source package.
