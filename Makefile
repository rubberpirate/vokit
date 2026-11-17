PREFIX ?= $(HOME)/.local
GO ?= go
PYTHON ?= python3
SYSTEM_VOKIT ?= /usr/bin/vokit

.PHONY: build desktop wifi-direct test test-desktop install install-desktop install-app install-files install-system-config check-packaging refresh-desktop check-install-conflicts
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o build/vivolinux ./cmd/vivolinux

wifi-direct:
	$(PYTHON) wifi_direct/tools/build_p2p_supplicant.py $(if $(WIFI_DIRECT_ARCHIVE),--archive "$(WIFI_DIRECT_ARCHIVE)",)

desktop:
	$(PYTHON) -c 'import gi; gi.require_version("Gtk", "4.0"); gi.require_version("Adw", "1"); gi.require_version("Gst", "1.0"); from gi.repository import Gtk, Adw, Gst'
	install -d build/vivo_desktop
	install -m644 desktop/gtk/vivo_desktop/*.py desktop/gtk/vivo_desktop/*.css build/vivo_desktop/
	install -d build/vivo_desktop/locales
	install -m644 desktop/gtk/vivo_desktop/locales/*.json build/vivo_desktop/locales/
	install -m755 desktop/gtk/vivo-desktop desktop/gtk/vivo-desktop-helper build/
	install -m755 desktop/gtk/vivo-bluetooth-helper build/
	install -m755 desktop/gtk/vivo-desktop build/vokit
	install -d build/icons
	cp -R packaging/icons/hicolor build/icons/
	$(PYTHON) -m compileall -q build/vivo_desktop

test-desktop: desktop
	xvfb-run -a env VOKIT_LANGUAGE=en GDK_BACKEND=x11 WAYLAND_DISPLAY= GSK_RENDERER=cairo GTK_THEME=Adwaita PYTHONPATH=desktop/gtk $(PYTHON) -m unittest vivo_desktop.i18n_test vivo_desktop.reconnect_test vivo_desktop.backend_test vivo_desktop.media_test
	dbus-run-session -- env VOKIT_LANGUAGE=en PYTHONPATH=desktop/gtk $(PYTHON) -m unittest vivo_desktop.tray_test
	xvfb-run -a env VOKIT_LANGUAGE=en GDK_BACKEND=x11 WAYLAND_DISPLAY= GSK_RENDERER=cairo GTK_THEME=Adwaita PYTHONPATH=desktop/gtk $(PYTHON) -m vivo_desktop.app --self-test

test:
	$(GO) test -race ./...

install: build
	install -Dm755 build/vivolinux "$(DESTDIR)$(PREFIX)/bin/vivolinux"

check-install-conflicts:
	@if [ -z "$(DESTDIR)" ] && [ "$(PREFIX)" = "$(HOME)/.local" ] && [ -x "$(SYSTEM_VOKIT)" ]; then \
		echo 'A system installation of VOkit already exists at $(SYSTEM_VOKIT).' >&2; \
		echo 'A user installation would override it. Upgrade the system package instead.' >&2; exit 1; \
	fi

install-desktop: check-install-conflicts desktop
	install -Dm755 build/vivo-bluetooth-helper "$(DESTDIR)$(PREFIX)/bin/vivo-bluetooth-helper"
	install -Dm755 build/vivo-desktop-helper "$(DESTDIR)$(PREFIX)/bin/vivo-desktop-helper"
	install -Dm755 build/vivo-desktop "$(DESTDIR)$(PREFIX)/bin/vivo-desktop"
	install -Dm755 build/vokit "$(DESTDIR)$(PREFIX)/bin/vokit"

install-app: build desktop wifi-direct
	$(MAKE) install-files
	$(MAKE) refresh-desktop

# These targets only copy files. They never modify the running system or load drivers.
install-files: check-install-conflicts
	install -Dm755 build/vivo-bluetooth-helper "$(DESTDIR)$(PREFIX)/bin/vivo-bluetooth-helper"
	install -Dm755 build/vivolinux "$(DESTDIR)$(PREFIX)/bin/vivolinux"
	install -Dm755 build/vivo-desktop-helper "$(DESTDIR)$(PREFIX)/bin/vivo-desktop-helper"
	install -Dm755 build/vivo-desktop "$(DESTDIR)$(PREFIX)/bin/vivo-desktop"
	install -Dm755 build/vokit "$(DESTDIR)$(PREFIX)/bin/vokit"
	install -d "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop"
	install -m644 desktop/gtk/vivo_desktop/__init__.py desktop/gtk/vivo_desktop/app.py desktop/gtk/vivo_desktop/backend.py desktop/gtk/vivo_desktop/helper.py desktop/gtk/vivo_desktop/media.py desktop/gtk/vivo_desktop/tray.py desktop/gtk/vivo_desktop/i18n.py desktop/gtk/vivo_desktop/style.css "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop/"
	install -m644 desktop/gtk/vivo_desktop/bluetooth_transport.py "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop/"
	install -d "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop/locales"
	install -m644 desktop/gtk/vivo_desktop/locales/*.json "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/vivo_desktop/locales/"
	install -d "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/tools" "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/patches" "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/packaging/systemd" "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/build/private-p2p/bin"
	install -m644 $(addprefix wifi_direct/tools/,install_p2p_service.py p2p_service.py p2p_persistent.py p2p_helper.py p2p_private.py wifi_capabilities.py p2p_supplicant.config build_p2p_supplicant.py) "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/tools/"
	install -m644 wifi_direct/patches/*.patch "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/patches/"
	install -m644 wifi_direct/packaging/systemd/*.service "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/packaging/systemd/"
	install -m755 wifi_direct/build/private-p2p/bin/wpa_supplicant "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/build/private-p2p/bin/"
	install -m644 wifi_direct/build/private-p2p/bin/build.json wifi_direct/build/private-p2p/bin/COPYING "$(DESTDIR)$(PREFIX)/lib/vivo-office-kit/wifi-direct/build/private-p2p/bin/"
	install -Dm644 packaging/thunar/vokit.desktop "$(DESTDIR)$(PREFIX)/share/Thunar/sendto/vokit.desktop"
	$(PYTHON) scripts/install-desktop.py "$(PREFIX)" "$(DESTDIR)" packaging/desktop/vivo-office-kit.desktop
	for size in 16 24 32 48 64 128 256 512 1024; do \
		install -Dm644 "packaging/icons/hicolor/$${size}x$${size}/apps/io.github.vivolinux.OfficeKit.png" "$(DESTDIR)$(PREFIX)/share/icons/hicolor/$${size}x$${size}/apps/io.github.vivolinux.OfficeKit.png"; \
	done
	rm -f "$(DESTDIR)$(PREFIX)/share/applications/vivo-office-kit.desktop"
	install -Dm644 README.md "$(DESTDIR)$(PREFIX)/share/doc/vivo-office-kit/README.md"
	install -Dm644 THIRD_PARTY_NOTICES "$(DESTDIR)$(PREFIX)/share/licenses/vivo-office-kit/THIRD_PARTY_NOTICES"

refresh-desktop:
	@bash scripts/refresh-desktop.sh "$(PREFIX)" "$(DESTDIR)"

install-system-config:
	install -Dm644 packaging/udev/70-vivo-phone-camera.rules "$(DESTDIR)/usr/lib/udev/rules.d/70-vivo-phone-camera.rules"
	install -Dm644 packaging/modprobe/vivo-phone-camera.conf "$(DESTDIR)/etc/modprobe.d/vivo-phone-camera.conf"
	install -Dm644 packaging/modules-load/vivo-phone-camera.conf "$(DESTDIR)/etc/modules-load.d/vivo-phone-camera.conf"

check-packaging:
	python3 scripts/check-packaging.py
