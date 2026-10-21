PREFIX ?= $(HOME)/.local
GO ?= go

.PHONY: build desktop test install install-desktop install-app install-files install-system-config check-packaging
build:
	CGO_ENABLED=0 $(GO) build -trimpath -o build/vivolinux ./cmd/vivolinux

desktop:
	cmake -S desktop -B build/desktop -DCMAKE_BUILD_TYPE=Release
	cmake --build build/desktop --parallel 2
	cp build/desktop/vivo-desktop-helper build/vivo-desktop-helper.next
	mv -f build/vivo-desktop-helper.next build/vivo-desktop-helper
	cp build/desktop/vivo-desktop build/vivo-desktop.next
	mv -f build/vivo-desktop.next build/vivo-desktop
	ctest --test-dir build/desktop --output-on-failure

test:
	$(GO) test -race ./...

install: build
	install -Dm755 build/vivolinux "$(DESTDIR)$(PREFIX)/bin/vivolinux"

install-desktop: desktop
	install -Dm755 build/vivo-desktop-helper "$(DESTDIR)$(PREFIX)/bin/vivo-desktop-helper"
	install -Dm755 build/vivo-desktop "$(DESTDIR)$(PREFIX)/bin/vivo-desktop"

install-app: build desktop
	$(MAKE) install-files

# These targets only copy files. They never modify the running system or load drivers.
install-files:
	install -Dm755 build/vivolinux "$(DESTDIR)$(PREFIX)/bin/vivolinux"
	install -Dm755 build/vivo-desktop-helper "$(DESTDIR)$(PREFIX)/bin/vivo-desktop-helper"
	install -Dm755 build/vivo-desktop "$(DESTDIR)$(PREFIX)/bin/vivo-desktop"
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
