PREFIX ?= $(HOME)/.local
GO ?= go
PYTHON ?= python3

.PHONY: build desktop test test-desktop install install-desktop install-app install-files install-system-config check-packaging
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o build/vivolinux ./cmd/vivolinux

desktop:
	$(PYTHON) -c 'import gi; gi.require_version("Gtk", "4.0"); gi.require_version("Adw", "1"); gi.require_version("Gst", "1.0"); from gi.repository import Gtk, Adw, Gst'
	install -d build/vivo_desktop
	install -m644 desktop/gtk/vivo_desktop/*.py desktop/gtk/vivo_desktop/*.css build/vivo_desktop/
	install -m755 desktop/gtk/vivo-desktop desktop/gtk/vivo-desktop-helper build/
	install -m755 desktop/gtk/vivo-desktop build/vokit
	$(PYTHON) -m compileall -q build/vivo_desktop

test-desktop: desktop
	xvfb-run -a env GDK_BACKEND=x11 WAYLAND_DISPLAY= GSK_RENDERER=cairo GTK_THEME=Adwaita PYTHONPATH=desktop/gtk $(PYTHON) -m unittest vivo_desktop.backend_test vivo_desktop.media_test vivo_desktop.remote_pc_test vivo_desktop.remote_input_test vivo_desktop.remote_host_test
	dbus-run-session -- env PYTHONPATH=desktop/gtk $(PYTHON) -m unittest vivo_desktop.tray_test
	xvfb-run -a env GDK_BACKEND=x11 WAYLAND_DISPLAY= GSK_RENDERER=cairo GTK_THEME=Adwaita PYTHONPATH=desktop/gtk $(PYTHON) -m vivo_desktop.app --self-test

test:
	$(GO) test -race ./...

install: build
	install -Dm755 build/vivolinux "$(DESTDIR)$(PREFIX)/bin/vivolinux"

install-desktop: desktop
	install -Dm755 build/vivo-desktop-helper "$(DESTDIR)$(PREFIX)/bin/vivo-desktop-helper"
	install -Dm755 build/vivo-desktop "$(DESTDIR)$(PREFIX)/bin/vivo-desktop"
	install -Dm755 build/vokit "$(DESTDIR)$(PREFIX)/bin/vokit"

install-app: build desktop
	$(MAKE) install-files

# These targets only copy files. They never modify the running system or load drivers.
install-files:
	install -Dm755 build/vivolinux "$(DESTDIR)$(PREFIX)/bin/vivolinux"
	install -Dm755 build/vivo-desktop-helper "$(DESTDIR)$(PREFIX)/bin/vivo-desktop-helper"
	install -Dm755 build/vivo-desktop "$(DESTDIR)$(PREFIX)/bin/vivo-desktop"
	install -Dm755 build/vokit "$(DESTDIR)$(PREFIX)/bin/vokit"
	install -d "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop"
	install -m644 desktop/gtk/vivo_desktop/__init__.py desktop/gtk/vivo_desktop/app.py desktop/gtk/vivo_desktop/backend.py desktop/gtk/vivo_desktop/helper.py desktop/gtk/vivo_desktop/media.py desktop/gtk/vivo_desktop/tray.py desktop/gtk/vivo_desktop/remote_pc.py desktop/gtk/vivo_desktop/remote_host.py desktop/gtk/vivo_desktop/remote_input.py desktop/gtk/vivo_desktop/style.css "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop/"
	install -Dm644 packaging/thunar/vokit.desktop "$(DESTDIR)$(PREFIX)/share/Thunar/sendto/vokit.desktop"
	install -Dm644 packaging/desktop/vivo-office-kit.desktop "$(DESTDIR)$(PREFIX)/share/applications/vivo-office-kit.desktop"
	install -Dm644 packaging/desktop/vivo-office-kit.svg "$(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps/vivo-office-kit.svg"
	install -Dm644 README.md "$(DESTDIR)$(PREFIX)/share/doc/vivo-office-kit/README.md"
	install -Dm644 THIRD_PARTY_NOTICES "$(DESTDIR)$(PREFIX)/share/licenses/vivo-office-kit/THIRD_PARTY_NOTICES"

install-system-config:
	install -Dm644 packaging/udev/70-vivo-phone-camera.rules "$(DESTDIR)/usr/lib/udev/rules.d/70-vivo-phone-camera.rules"
	install -Dm644 packaging/modprobe/vivo-phone-camera.conf "$(DESTDIR)/etc/modprobe.d/vivo-phone-camera.conf"
	install -Dm644 packaging/modules-load/vivo-phone-camera.conf "$(DESTDIR)/etc/modules-load.d/vivo-phone-camera.conf"

check-packaging:
	python3 scripts/check-packaging.py
