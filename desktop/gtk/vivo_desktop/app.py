"""Native GTK desktop shell. Phone operations remain in the existing Go backend."""
from __future__ import annotations

import argparse
import base64
from collections import deque
from concurrent.futures import ThreadPoolExecutor
import ipaddress
import os
from pathlib import Path
import shutil
import stat
import sys

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Adw", "1")
from gi.repository import Adw, Gdk, GdkPixbuf, Gio, GLib, GObject, Gtk, Pango

CATEGORIES = (("recent", "Recent files", "document-open-recent-symbolic"),
              ("images", "Images", "image-x-generic-symbolic"),
              ("videos", "Videos", "video-x-generic-symbolic"),
              ("audio", "Audio", "audio-x-generic-symbolic"),
              ("documents", "Documents", "x-office-document-symbolic"),
              ("whatsapp", "WhatsApp", "user-available-symbolic"),
              ("storage", "Phone storage", "drive-harddisk-symbolic"))
IMAGE_EXTENSIONS = {"jpg", "jpeg", "png", "gif", "webp", "heic", "heif", "avif", "bmp"}
VIDEO_EXTENSIONS = {"mp4", "m4v", "mov", "webm", "mkv", "avi", "3gp"}


def box(vertical=False, spacing=0, css=None):
    widget = Gtk.Box(orientation=Gtk.Orientation.VERTICAL if vertical else Gtk.Orientation.HORIZONTAL,
                     spacing=spacing)
    if css:
        widget.add_css_class(css)
    return widget


def label(text, css=None, **kwargs):
    widget = Gtk.Label(label=text, xalign=0, **kwargs)
    if css:
        for name in css.split():
            widget.add_css_class(name)
    return widget


def icon_button(icon, tooltip, callback=None):
    button = Gtk.Button(icon_name=icon, tooltip_text=tooltip)
    button.update_property([Gtk.AccessibleProperty.LABEL], [tooltip])
    button.add_css_class("flat")
    if callback:
        button.connect("clicked", lambda *_: callback())
    return button


def private_regular(path, limit):
    try:
        info = os.lstat(path)
        return stat.S_ISREG(info.st_mode) and 0 < info.st_size <= limit
    except OSError:
        return False


class FileItem(GObject.Object):
    name = GObject.Property(type=str, default="")
    path = GObject.Property(type=str, default="")
    size = GObject.Property(type=GObject.TYPE_INT64, default=0)
    folder = GObject.Property(type=bool, default=False)
    page = GObject.Property(type=int, default=0)
    category = GObject.Property(type=str, default="")
    texture = GObject.Property(type=Gdk.Texture)

    def __init__(self, data, page=0, category=""):
        super().__init__(name=data["fileName"], path=data["savePath"],
                         size=max(0, int(data.get("fileSize", 0))),
                         folder=bool(data.get("isDirectory", False)), page=page, category=category)
        self.thumbnail_requested = False

    @property
    def extension(self):
        return Path(self.path).suffix.lower().lstrip(".")

    @property
    def icon(self):
        if self.folder:
            return "folder-symbolic"
        if self.extension in IMAGE_EXTENSIONS:
            return "image-x-generic-symbolic"
        if self.extension in VIDEO_EXTENSIONS:
            return "video-x-generic-symbolic"
        if self.extension in {"mp3", "m4a", "aac", "wav", "flac", "opus", "ogg"}:
            return "audio-x-generic-symbolic"
        return "text-x-generic-symbolic"


def decode_thumbnail(result):
    """Decode through native gdk-pixbuf on a worker, with bounded input and dimensions."""
    try:
        encoded = result.get("data", "")
        if not isinstance(encoded, str) or len(encoded) > 3 * 1024 * 1024:
            return None
        data = base64.b64decode(encoded, validate=True)
        if not data or len(data) > 2 * 1024 * 1024:
            return None
        loader = GdkPixbuf.PixbufLoader.new()
        valid = [False]

        def sized(current, width, height):
            valid[0] = 0 < width <= 4096 and 0 < height <= 4096 and width * height <= 16 * 1024 * 1024
            current.set_size(max(1, int(width * min(256 / width, 256 / height, 1))),
                             max(1, int(height * min(256 / width, 256 / height, 1))))

        loader.connect("size-prepared", sized)
        loader.write(data)
        loader.close()
        fmt = loader.get_format()
        if not valid[0] or not fmt or result.get("mime") not in fmt.get_mime_types():
            return None
        return loader.get_pixbuf()
    except (GLib.Error, ValueError, TypeError, ZeroDivisionError):
        return None


def available_download_path(folder, name):
    if not name or name in (".", "..") or any(char in name for char in ("/", "\\", "\x00")):
        raise ValueError("Invalid file name")
    candidate = Path(folder) / name
    index = 2
    while os.path.lexists(candidate):
        candidate = Path(folder) / f"{Path(name).stem} ({index}){Path(name).suffix}"
        index += 1
    return str(candidate)


def valid_download_receipt(receipt, size):
    return (isinstance(receipt, dict) and receipt.get("bytes") == size
            and isinstance(receipt.get("sha256"), str) and len(receipt["sha256"]) == 64
            and all(char in "0123456789abcdefABCDEF" for char in receipt["sha256"]))


class OfficeWindow(Adw.ApplicationWindow):
    def __init__(self, app):
        super().__init__(application=app, title="VOkit")
        self.app = app
        self.backend = app.backend
        self.preferences = app.settings
        self.connected = False
        self.event_connected = False
        self.external_session = False
        self.owned = False
        self.shutting_down = False
        self.category = "recent"
        self.directory = "/storage/emulated/0/Download"
        self.generation = 0
        self.loading = False
        self.loaded = False
        self.has_more = False
        self.next_page = 0
        self.query = ""
        self.polling = False
        self.active_receive_directory = None
        self.thumbnail_pending = deque()
        self.thumbnail_active = 0
        self.decoder = ThreadPoolExecutor(max_workers=2, thread_name_prefix="vivo-thumbnail")
        self.media_panel = None
        self.mirror_window = None
        self.qr_dialog = None
        self.preview_busy = False
        self.transfer_busy = False
        self.pending_sends = deque()
        self.selection_updating = False
        self.viewers = set()
        self.nav_labels = []
        self.nav_buttons = {}
        display = Gdk.Display.get_default()
        monitor = display.get_monitors().get_item(0) if display else None
        available = monitor.get_geometry() if monitor else None
        self.set_default_size(min(1400, available.width if available else 1400),
                              min(900, available.height if available else 900))
        self.set_size_request(min(760, available.width if available else 760),
                              min(520, available.height if available else 520))
        self.add_css_class("office-window")
        self.connect("close-request", self._hide)
        self._build_ui()
        self._apply_theme(self.preferences.get("theme", "dark"))
        self.poll_source = GLib.timeout_add_seconds(3, self._poll_status)
        def initial_status():
            self._poll_status()
            return GLib.SOURCE_REMOVE
        GLib.idle_add(initial_status)

    def _build_ui(self):
        toolbar = Adw.ToolbarView()
        header = Adw.HeaderBar()
        header.add_css_class("office-header")
        header.set_title_widget(label("VOkit", "edition"))
        self.search = Gtk.SearchEntry(placeholder_text="Search loaded phone files", hexpand=True)
        self.search.set_size_request(300, -1)
        self.search.connect("search-changed", self._search_changed)
        header.pack_end(self.search)
        menu = Gio.Menu()
        menu.append("Settings", "app.preferences")
        menu.append("Quit VOkit", "app.quit")
        header.pack_end(Gtk.MenuButton(icon_name="open-menu-symbolic", menu_model=menu))
        toolbar.add_top_bar(header)
        shell = box(spacing=6)
        nav = box(True, 12, "outer-nav")
        nav.set_size_request(94, -1)
        brand = label("VOkit", "brand", halign=Gtk.Align.CENTER)
        brand.set_margin_bottom(14)
        nav.append(brand)
        first = None
        for key, title, icon in (("home", "Home", "go-home-symbolic"),
                                 ("files", "My phone", "phone-symbolic"),
                                 ("connect", "Connect", "list-add-symbolic"),
                                 ("settings", "Settings", "preferences-system-symbolic")):
            if key == "settings":
                nav.append(Gtk.Box(vexpand=True))
            button = Gtk.ToggleButton()
            if first:
                button.set_group(first)
            else:
                first = button
            content = box(True, 7)
            content.set_halign(Gtk.Align.CENTER)
            content.append(Gtk.Image(icon_name=icon, pixel_size=22))
            title_label = label(title, "nav-label", halign=Gtk.Align.CENTER)
            if key == "files":
                self.phone_nav_label = title_label
                title_label.set_max_width_chars(14)
                title_label.set_ellipsize(Pango.EllipsizeMode.END)
            self.nav_labels.append(title_label)
            content.append(title_label)
            button.set_child(content)
            button.set_tooltip_text(title)
            button.update_property([Gtk.AccessibleProperty.LABEL], [title])
            button.connect("clicked", lambda current, destination=key: self._navigate(destination))
            self.nav_buttons[key] = button
            nav.append(button)
        self.nav_buttons["files"].set_active(True)
        shell.append(nav)
        workspace = box(True, 16, "workspace")
        workspace.set_hexpand(True)
        cards = box(spacing=14)
        self.device_card = box(True, 9, "summary-card")
        self.device_card.set_hexpand(True)
        title_row = box(spacing=10)
        self.device_title = label("vivo phone", "device-title")
        title_row.append(self.device_title)
        self.connection_pill = label("Connected", "connection-pill")
        self.connection_pill.set_visible(False)
        title_row.append(self.connection_pill)
        self.device_card.append(title_row)
        self.connection_status = label("Connect your phone to get started", "muted small", wrap=True)
        self.connection_status.set_max_width_chars(44)
        self.connection_status.set_hexpand(True)
        self.device_card.append(self.connection_status)
        actions = box(spacing=8)
        self.connect_button = Gtk.Button(label="Connect phone", halign=Gtk.Align.START)
        self.connect_button.connect("clicked", lambda *_: self._connection_action())
        actions.append(self.connect_button)
        actions.append(Gtk.Box(hexpand=True))
        actions.append(icon_button("preferences-system-symbolic", "Connection settings", lambda: self._navigate("settings")))
        self.device_card.append(actions)
        cards.append(self.device_card)
        mirror = box(True, 9, "summary-card")
        mirror.add_css_class("mirror-card")
        mirror.set_hexpand(True)
        mirror_title = box(spacing=10)
        mirror_title.append(Gtk.Image(icon_name="video-display-symbolic", pixel_size=23))
        mirror_title.append(label("Phone Mirroring", "mirror-title"))
        mirror.append(mirror_title)
        description = label("Your phone screen, with touch and keyboard control", "muted small", wrap=True)
        description.set_max_width_chars(48)
        mirror.append(description)
        self.mirror_button = Gtk.Button(label="Open phone window", halign=Gtk.Align.START)
        self.mirror_button.add_css_class("suggested-action")
        self.mirror_button.connect("clicked", lambda *_: self._open_mirror())
        self.mirror_button.set_sensitive(False)
        mirror.append(self.mirror_button)
        cards.append(mirror)
        workspace.append(cards)
        self.pages = Gtk.Stack(vexpand=True, hexpand=True, transition_type=Gtk.StackTransitionType.CROSSFADE)
        self.pages.add_named(self._files_page(), "files")
        self.pages.add_named(self._home_page(), "home")
        self.pages.add_named(self._settings_page(), "settings")
        self.camera_page = box(True, 12)
        self.pages.add_named(self.camera_page, "camera")
        self.pages.set_visible_child_name("files")
        workspace.append(self.pages)
        shell.append(workspace)
        toolbar.set_content(shell)
        self.set_content(toolbar)
        breakpoint = Adw.Breakpoint.new(Adw.BreakpointCondition.parse("max-width: 1000sp"))
        breakpoint.add_setter(self.category_scroll, "visible", False)
        breakpoint.add_setter(self.category_dropdown, "visible", True)
        for nav_label in self.nav_labels:
            breakpoint.add_setter(nav_label, "visible", False)
        self.add_breakpoint(breakpoint)

    def _files_page(self):
        page = box(True, 7)
        manager = box(css="file-manager")
        manager.set_vexpand(True)
        rail = box(True, css="category-rail")
        rail.append(label("File Manager", "file-heading"))
        self.category_list = Gtk.ListBox(selection_mode=Gtk.SelectionMode.SINGLE)
        self.category_list.add_css_class("navigation-sidebar")
        for key, title, icon in CATEGORIES:
            row = Gtk.ListBoxRow()
            row.category_key = key
            content = box(spacing=10)
            content.append(Gtk.Image(icon_name=icon, pixel_size=17))
            content.append(label(title))
            row.set_child(content)
            self.category_list.append(row)
        self.category_list.select_row(self.category_list.get_row_at_index(0))
        self.category_list.connect("row-selected", self._category_selected)
        rail.append(self.category_list)
        rail.append(Gtk.Box(vexpand=True))
        rail.append(label("Search filters loaded files.\nScroll to load more.", "scope-note", wrap=True))
        self.category_scroll = Gtk.ScrolledWindow(hscrollbar_policy=Gtk.PolicyType.NEVER,
                                                  min_content_width=188, propagate_natural_width=True)
        self.category_scroll.set_child(rail)
        manager.append(self.category_scroll)
        content = box(True, 12, "file-content")
        content.set_hexpand(True)
        controls = box(spacing=6, css="file-toolbar")
        self.import_button = Gtk.Button(label="Import computer files")
        self.import_button.add_css_class("suggested-action")
        self.import_button.set_tooltip_text("Import into the phone Download folder")
        self.import_button.connect("clicked", lambda *_: self._import_file())
        controls.append(self.import_button)
        controls.append(Gtk.Box(hexpand=True))
        self.spinner = Gtk.Spinner()
        controls.append(self.spinner)
        self.download_button = icon_button("document-save-symbolic", "Save selected file to computer", self._download_file)
        self.preview_button = icon_button("document-open-symbolic", "Open selected file preview", self._open_selected)
        controls.append(self.download_button)
        controls.append(self.preview_button)
        self.select_all = Gtk.CheckButton(tooltip_text="Select all loaded files")
        self.select_all.update_property([Gtk.AccessibleProperty.LABEL], ["Select all loaded files"])
        self.select_all.connect("toggled", self._select_all_files)
        controls.append(self.select_all)
        grid_button = Gtk.ToggleButton(icon_name="view-grid-symbolic", tooltip_text="Thumbnail grid")
        list_button = Gtk.ToggleButton(icon_name="view-list-symbolic", tooltip_text="File list")
        grid_button.set_group(list_button)
        grid_button.set_active(True)
        for button in (grid_button, list_button):
            button.add_css_class("flat")
            controls.append(button)
        grid_button.connect("toggled", lambda current: self._set_file_view("grid") if current.get_active() else None)
        list_button.connect("toggled", lambda current: self._set_file_view("list") if current.get_active() else None)
        content.append(controls)
        self.category_dropdown = Gtk.DropDown.new_from_strings([item[1] for item in CATEGORIES])
        self.category_dropdown.set_visible(False)
        self.category_dropdown.connect("notify::selected", self._dropdown_category)
        content.append(self.category_dropdown)
        path_row = box(spacing=8)
        self.up_button = icon_button("go-up-symbolic", "Parent folder", self._parent_folder)
        self.up_button.set_visible(False)
        path_row.append(self.up_button)
        path_row.append(icon_button("drive-harddisk-symbolic", "Phone storage", lambda: self._select_category("storage")))
        self.breadcrumb = label("Recent files · Phone files", "breadcrumb", hexpand=True)
        path_row.append(self.breadcrumb)
        self.path_entry = Gtk.Entry(text=self.directory, hexpand=True, tooltip_text="Phone directory")
        self.path_entry.add_css_class("breadcrumb-entry")
        self.path_entry.set_visible(False)
        self.path_entry.connect("activate", lambda current: self._open_folder(current.get_text()))
        path_row.append(self.path_entry)
        content.append(path_row)
        self.store = Gio.ListStore.new(FileItem)
        self.filter = Gtk.CustomFilter.new(lambda item: self.query in item.name.casefold())
        self.filtered = Gtk.FilterListModel.new(self.store, self.filter)
        self.selection = Gtk.MultiSelection.new(self.filtered)
        self.selection.connect("selection-changed", lambda *_: self._update_controls())
        self.file_views = Gtk.Stack(vexpand=True, hexpand=True)
        self.grid = Gtk.GridView.new(self.selection, self._file_factory(False))
        self.grid.set_min_columns(1)
        self.grid.set_max_columns(12)
        self.grid.set_single_click_activate(False)
        self.grid.add_css_class("files-grid")
        self.grid.connect("activate", self._activate_file)
        self.list = Gtk.ListView.new(self.selection, self._file_factory(True))
        self.list.add_css_class("files-list")
        self.list.connect("activate", self._activate_file)
        self.file_scrollers = []
        for name, view in (("grid", self.grid), ("list", self.list)):
            scroll = Gtk.ScrolledWindow(hscrollbar_policy=Gtk.PolicyType.NEVER, vexpand=True)
            scroll.set_child(view)
            adjustment = scroll.get_vadjustment()
            adjustment.connect("value-changed", lambda *_: self._maybe_more())
            adjustment.connect("changed", lambda *_: GLib.idle_add(self._maybe_more))
            self.file_views.add_named(scroll, name)
            self.file_scrollers.append(scroll)
        self.file_views.set_visible_child_name("grid")
        self.empty = box(True, 12)
        self.empty.set_halign(Gtk.Align.CENTER)
        self.empty.set_valign(Gtk.Align.CENTER)
        self.empty.set_can_target(False)
        self.empty_icon = Gtk.Image(icon_name="folder-remote-symbolic", pixel_size=52)
        self.empty_icon.add_css_class("empty-icon")
        self.empty.append(self.empty_icon)
        self.empty_title = label("Your files, within reach", "empty-title", halign=Gtk.Align.CENTER)
        self.empty.append(self.empty_title)
        self.empty_description = label("Connect your phone to browse its files", "muted", wrap=True, halign=Gtk.Align.CENTER)
        self.empty.append(self.empty_description)
        overlay = Gtk.Overlay(vexpand=True)
        overlay.set_child(self.file_views)
        overlay.add_overlay(self.empty)
        content.append(overlay)
        self.filtered.connect("items-changed", lambda *_: self._files_changed())
        manager.append(content)
        page.append(manager)
        self.receipt = label("", "file-status", wrap=True, selectable=True)
        page.append(self.receipt)
        self._update_controls()
        return page

    def _file_factory(self, list_mode):
        factory = Gtk.SignalListItemFactory()

        def setup(_factory, list_item):
            tile = box(not list_mode, 6, "file-row" if list_mode else "file-tile")
            if not list_mode:
                tile.set_size_request(152, 198)
            overlay = Gtk.Overlay()
            overlay.set_size_request(44 if list_mode else 128, 44 if list_mode else 128)
            picture = Gtk.Picture(content_fit=Gtk.ContentFit.COVER, can_shrink=True)
            picture.set_size_request(44 if list_mode else 128, 44 if list_mode else 128)
            fallback = Gtk.Image(pixel_size=32 if list_mode else 56)
            overlay.set_child(picture)
            overlay.add_overlay(fallback)
            fallback.set_halign(Gtk.Align.CENTER)
            fallback.set_valign(Gtk.Align.CENTER)
            play = Gtk.Image(icon_name="media-playback-start-symbolic", pixel_size=16)
            play.add_css_class("video-play")
            play.set_halign(Gtk.Align.CENTER)
            play.set_valign(Gtk.Align.CENTER)
            overlay.add_overlay(play)
            checked = Gtk.CheckButton(halign=Gtk.Align.END, valign=Gtk.Align.START,
                                      tooltip_text="Select file", margin_end=3, margin_top=3)
            checked.add_css_class("file-checkbox")
            checked.update_property([Gtk.AccessibleProperty.LABEL], ["Select file"])
            overlay.add_overlay(checked)
            list_item.check_binding = False
            def toggled(current):
                position = list_item.get_position()
                if not list_item.check_binding and position < self.filtered.get_n_items():
                    if current.get_active():
                        self.selection.select_item(position, False)
                    else:
                        self.selection.unselect_item(position)
            checked.connect("toggled", toggled)
            def selected_changed(*_):
                list_item.check_binding = True
                checked.set_active(list_item.get_selected())
                list_item.check_binding = False
            list_item.connect("notify::selected", selected_changed)
            tile.append(overlay)
            name = label("", "file-name", wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR,
                         ellipsize=Pango.EllipsizeMode.END, lines=2)
            name.set_max_width_chars(22)
            if list_mode:
                name.set_hexpand(True)
                tile.append(name)
            else:
                name.set_size_request(128, 34)
                tile.append(name)
            size = label("", "file-size")
            tile.append(size)
            list_item.set_child(tile)
            list_item.parts = (picture, fallback, play, name, size, checked)
            list_item.texture_handler = None
            list_item.bound_item = None

        def bind(_factory, list_item):
            item = list_item.get_item()
            picture, fallback, play, name, size, checked = list_item.parts
            checked.set_sensitive(not item.folder)
            list_item.check_binding = True
            checked.set_active(list_item.get_selected())
            list_item.check_binding = False
            name.set_text(item.name)
            name.set_tooltip_text(item.name)
            size.set_text("Folder" if item.folder else GLib.format_size(item.size))
            fallback.set_from_icon_name(item.icon)
            play.set_visible(not list_mode and item.extension in VIDEO_EXTENSIONS)
            list_item.get_child().update_property([Gtk.AccessibleProperty.LABEL],
                [f"{item.name}, {'folder' if item.folder else GLib.format_size(item.size)}"])

            def texture_changed(*_):
                picture.set_paintable(item.texture)
                fallback.set_visible(item.texture is None)

            texture_changed()
            list_item.bound_item = item
            list_item.texture_handler = item.connect("notify::texture", texture_changed)
            self._queue_thumbnail(item)

        def unbind(_factory, list_item):
            if list_item.bound_item and list_item.texture_handler:
                list_item.bound_item.disconnect(list_item.texture_handler)
            list_item.texture_handler = None
            list_item.bound_item = None
            list_item.parts[0].set_paintable(None)

        factory.connect("setup", setup)
        factory.connect("bind", bind)
        factory.connect("unbind", unbind)
        return factory

    def _home_page(self):
        page = Adw.StatusPage(title="Your phone, at your desk",
                              description="Browse and transfer files, control your phone screen, or share its camera and microphone.",
                              icon_name="phone-symbolic")
        actions = box(True, 12)
        actions.set_halign(Gtk.Align.CENTER)
        files = Gtk.Button(label="Browse phone files")
        files.add_css_class("suggested-action")
        files.connect("clicked", lambda *_: self._navigate("files"))
        actions.append(files)
        camera = Gtk.Button(label="Camera & microphone")
        camera.connect("clicked", lambda *_: self._open_camera())
        actions.append(camera)
        page.set_child(actions)
        return page

    def _settings_page(self):
        container = box(True, 8)
        heading = box(spacing=8)
        heading.set_margin_start(18)
        heading.set_margin_end(18)
        heading.append(label("Settings", "device-title", hexpand=True))
        self.close_settings_button = icon_button("window-close-symbolic", "Close Settings", lambda: self._navigate("files"))
        heading.append(self.close_settings_button)
        container.append(heading)
        page = Adw.PreferencesPage()
        page.add_css_class("settings-content")
        connection = Adw.PreferencesGroup(title="Connect your phone", description="Scan the pairing QR in Office Kit and approve the connection on your phone. No IP address or vivo account is needed.")
        advanced = Adw.ExpanderRow(title="Advanced connection", subtitle="Optional phone address override")
        self.phone_entry = Adw.EntryRow(title="Phone IP override (optional)", text=self.preferences.get("manual_phone", ""))
        advanced.add_row(self.phone_entry)
        connection.add(advanced)
        self.clipboard_switch = Adw.SwitchRow(title="Share plain-text clipboard", active=bool(self.preferences.get("clipboard", False)))
        self.notification_switch = Adw.SwitchRow(title="Phone and connection notifications",
            subtitle="Forward phone notifications to this desktop. Applies after reconnecting.", active=bool(self.preferences.get("notifications", True)))
        connection.add(self.clipboard_switch)
        connection.add(self.notification_switch)
        self.phone_entry.connect("notify::text", lambda *_: self._save_preferences())
        self.clipboard_switch.connect("notify::active", lambda *_: self._save_preferences())
        self.notification_switch.connect("notify::active", lambda *_: self._save_preferences())
        page.add(connection)
        receive = Adw.PreferencesGroup(title="Incoming files", description="New connections automatically receive phone transfers here. Folder changes apply when you reconnect.")
        downloads = GLib.get_user_special_dir(GLib.UserDirectory.DIRECTORY_DOWNLOAD) or str(Path.home() / "Downloads")
        self.receive_entry = Adw.EntryRow(title="Receive files folder", text=self.preferences.get("receive_dir", str(Path(downloads) / "Vivo Office Kit")))
        self.receive_entry.add_suffix(icon_button("folder-open-symbolic", "Choose receive folder", self._choose_receive_folder))
        self.receive_entry.connect("notify::text", lambda *_: self._save_preferences())
        receive.add(self.receive_entry)
        page.add(receive)
        appearance = Adw.PreferencesGroup(title="Appearance")
        self.theme_row = Adw.ComboRow(title="Theme", model=Gtk.StringList.new(["Dark", "Follow system", "Light"]))
        self.theme_row.set_selected({"dark": 0, "system": 1, "light": 2}.get(self.preferences.get("theme", "dark"), 0))
        self.theme_row.connect("notify::selected", self._theme_changed)
        appearance.add(self.theme_row)
        page.add(appearance)
        pair_group = Adw.PreferencesGroup()
        pair_row = Adw.ActionRow(title="Pair with Office Kit", subtitle="Create a QR, then scan it using your phone.")
        pair_button = Gtk.Button(label="Create pairing QR", valign=Gtk.Align.CENTER)
        pair_button.add_css_class("suggested-action")
        pair_button.connect("clicked", lambda *_: self._start_pair())
        pair_row.add_suffix(pair_button)
        pair_group.add(pair_row)
        page.add(pair_group)
        scroll = Gtk.ScrolledWindow(hscrollbar_policy=Gtk.PolicyType.NEVER)
        scroll.set_vexpand(True)
        scroll.set_child(page)
        container.append(scroll)
        return container

    def _navigate(self, destination):
        if destination == "connect":
            if self.connected:
                self._navigate("files")
            else:
                self._start_pair()
            return
        self.pages.set_visible_child_name(destination)
        if destination in self.nav_buttons:
            self.nav_buttons[destination].set_active(True)
        if destination == "files" and self.connected and not self.loaded and not self.loading:
            self._load_files(True)

    def _set_file_view(self, name):
        if hasattr(self, "file_views"):
            self.file_views.set_visible_child_name(name)
            GLib.idle_add(self._maybe_more)

    def _category_selected(self, _list, row):
        if row:
            self._change_category(row.category_key)

    def _dropdown_category(self, current, _param):
        index = current.get_selected()
        if index < len(CATEGORIES):
            self.category_list.select_row(self.category_list.get_row_at_index(index))

    def _select_category(self, key):
        index = next(i for i, item in enumerate(CATEGORIES) if item[0] == key)
        self.category_list.select_row(self.category_list.get_row_at_index(index))
        if self.category == key:
            self._change_category(key)

    def _change_category(self, key):
        if not hasattr(self, "store"):
            return
        if key == "whatsapp":
            self.category = ""
            self.directory = "/storage/emulated/0/Android/media/com.whatsapp/WhatsApp/Media"
        elif key == "storage":
            self.category = ""
            self.directory = "/storage/emulated/0"
        else:
            self.category = key
        index = next(i for i, item in enumerate(CATEGORIES) if item[0] == key)
        if self.category_dropdown.get_selected() != index:
            self.category_dropdown.set_selected(index)
        self.breadcrumb.set_text(CATEGORIES[index][1] + " · Phone files")
        self._update_scope()
        self._load_files(True)

    def _update_scope(self):
        folder = not self.category
        self.path_entry.set_visible(folder)
        self.path_entry.set_text(self.directory)
        self.up_button.set_visible(folder)
        self.breadcrumb.set_visible(not folder)
        self.import_button.set_tooltip_text("Import into the current phone folder" if folder else "Import into the phone Download folder")

    def _open_folder(self, path):
        if not path.startswith("/") or "\\" in path or "\x00" in path or os.path.normpath(path) != path:
            self.receipt.set_text("Enter an absolute phone folder without traversal components.")
            return
        self.category = ""
        self.directory = path
        self._update_scope()
        self._load_files(True)

    def _parent_folder(self):
        self._open_folder(str(Path(self.directory).parent))

    def _search_changed(self, entry):
        self.query = entry.get_text().casefold()
        self.filter.changed(Gtk.FilterChange.DIFFERENT)
        self._files_changed()
        GLib.idle_add(self._maybe_more)

    def _listing_arguments(self, page):
        args = ["files", "--page", str(page)]
        return args + (["--category", self.category] if self.category else ["--path", self.directory])

    def _load_files(self, reset=False):
        if self.shutting_down:
            return
        if reset:
            self.generation += 1
            self.store.remove_all()
            self.thumbnail_pending.clear()
            self.next_page = 0
            self.has_more = False
            self.loaded = False
            self.loading = False
        if not self.connected or self.loading:
            self._files_changed()
            return
        generation = self.generation
        category = self.category
        page = self.next_page
        self.loading = True
        self.spinner.start()
        self.receipt.set_text("Reading phone files…" if page == 0 else "Loading more files…")
        self._update_controls()

        def completed(result, error):
            if self.shutting_down or generation != self.generation:
                return
            self.loading = False
            self.loaded = True
            self.spinner.stop()
            entries = result.get("dataList") if isinstance(result, dict) else None
            if error or not isinstance(entries, list) or len(entries) > 2000:
                self.has_more = False
                self.receipt.set_text("Cannot browse these files. Check the phone connection and approve file access.")
            else:
                existing = {self.store.get_item(i).path for i in range(self.store.get_n_items())}
                added = 0
                for data in entries:
                    if not self._valid_entry(data) or data["savePath"] in existing:
                        continue
                    existing.add(data["savePath"])
                    self.store.append(FileItem(data, page, category))
                    added += 1
                total = max(0, int(result.get("totalCount", 0)))
                self.next_page = page + 1
                count = self.store.get_n_items()
                # Some phone builds omit totals or report only the current page.
                self.has_more = added > 0 and (len(entries) >= 200 or count < total) and self.next_page <= 1000000
                count_label = f"{count:,} files and folders" + (f" · {total:,} total" if total > count else "")
                self.receipt.set_text(count_label + (" · Scroll for more" if self.has_more else ""))
            self._files_changed()
            self._update_controls()
            GLib.idle_add(self._maybe_more)

        self.backend.call(self._listing_arguments(page), completed)

    @staticmethod
    def _valid_entry(data):
        if not isinstance(data, dict):
            return False
        path, name = data.get("savePath"), data.get("fileName")
        if not isinstance(path, str) or not isinstance(name, str) or not name or not path.startswith("/"):
            return False
        return ("\x00" not in path and "\\" not in path and os.path.normpath(path) == path
                and name not in {".", ".."} and not any(char in name for char in "/\\\x00")
                and (data.get("isDirectory") or Path(path).name == name))

    def _maybe_more(self):
        if self.shutting_down or not self.connected or self.loading or not self.has_more:
            return GLib.SOURCE_REMOVE
        scroll = self.file_views.get_visible_child()
        adjustment = scroll.get_vadjustment()
        if (self.filtered.get_n_items() == 0 or
                adjustment.get_value() + adjustment.get_page_size() >= adjustment.get_upper() - 150):
            self._load_files()
        return GLib.SOURCE_REMOVE

    def _files_changed(self):
        count = self.filtered.get_n_items()
        self.empty.set_visible(count == 0)
        if not self.connected:
            self.empty_title.set_text("Your files, within reach")
            self.empty_description.set_text("Connect your phone to browse its files")
        elif self.loading:
            self.empty_title.set_text("Reading your phone…")
            self.empty_description.set_text("Approve file access on your phone if prompted")
        elif self.query:
            self.empty_title.set_text("No matching files")
            self.empty_description.set_text("Try another name" + (" · More files are loading" if self.has_more else ""))
        else:
            self.empty_title.set_text("No files here yet")
            self.empty_description.set_text("Choose another category or import a computer file")

    def _queue_thumbnail(self, item):
        if item.thumbnail_requested or item.folder or item.extension not in IMAGE_EXTENSIONS | VIDEO_EXTENSIONS:
            return
        item.thumbnail_requested = True
        self.thumbnail_pending.append((self.generation, item))
        self._pump_thumbnails()

    def _pump_thumbnails(self):
        while self.thumbnail_active < 3 and self.thumbnail_pending and not self.shutting_down:
            generation, item = self.thumbnail_pending.popleft()
            if generation != self.generation:
                continue
            self.thumbnail_active += 1
            args = self._selected_arguments("thumbnail", item) + ["--width", "192", "--height", "192"]

            def completed(result, error, current=item, current_generation=generation):
                if self.shutting_down or current_generation != self.generation or error or not result:
                    self.thumbnail_active -= 1
                    self._pump_thumbnails()
                    return
                future = self.decoder.submit(decode_thumbnail, result)

                def decoded(job):
                    try:
                        pixbuf = job.result()
                    except Exception:
                        pixbuf = None
                    GLib.idle_add(self._thumbnail_decoded, current_generation, current, pixbuf)

                future.add_done_callback(decoded)

            self.backend.call(args, completed)

    def _thumbnail_decoded(self, generation, item, pixbuf):
        self.thumbnail_active -= 1
        if not self.shutting_down and generation == self.generation and pixbuf is not None:
            item.texture = Gdk.Texture.new_for_pixbuf(pixbuf)
        self._pump_thumbnails()
        return GLib.SOURCE_REMOVE

    @staticmethod
    def _selected_arguments(command, item):
        args = [command, "--remote", item.path, "--page", str(item.page)]
        return args + (["--category", item.category] if item.category else [])

    def _activate_file(self, _view, position):
        item = self.filtered.get_item(position)
        if item:
            if item.folder:
                self._open_folder(item.path)
            else:
                self._preview_file(item)

    def _open_selected(self):
        items = self._selected_items()
        item = items[0] if len(items) == 1 else None
        if item:
            self._open_folder(item.path) if item.folder else self._preview_file(item)

    def _preview_file(self, item):
        if not self.connected or self.preview_busy or item.folder:
            return
        if item.extension not in IMAGE_EXTENSIONS | VIDEO_EXTENSIONS:
            self.receipt.set_text("Download this file to open it with an installed application.")
            return
        self.preview_busy = True
        self._update_controls()
        self.receipt.set_text("Opening media stream…")

        def completed(result, error):
            self.preview_busy = False
            self._update_controls()
            from .media import MediaViewer, valid_stream_url, release_stream
            mime = result.get("mime", "") if isinstance(result, dict) else ""
            url = result.get("url", "") if isinstance(result, dict) else ""
            valid = (not error and valid_stream_url(url)
                     and (mime.startswith("image/") or mime.startswith("video/")))
            if self.shutting_down or not valid:
                release_stream(url)
                if not self.shutting_down:
                    self.receipt.set_text("Cannot open this stream. Check phone access and media format.")
                return
            try:
                viewer = MediaViewer(self.app, url, mime, on_close=lambda: release_stream(url))
                viewer.set_title(item.name)
                self.viewers.add(viewer)
                viewer.connect("close-request", lambda current: self.viewers.discard(current) or False)
                viewer.present()
                self.receipt.set_text("")
            except (ImportError, GLib.Error, RuntimeError, ValueError):
                release_stream(url)
                self.receipt.set_text("The media stream could not be opened.")

        self.backend.call(self._selected_arguments("stream", item), completed)

    def _selected_items(self):
        selected = self.selection.get_selection()
        return [self.filtered.get_item(selected.get_nth(index)) for index in range(selected.get_size())
                if selected.get_nth(index) < self.filtered.get_n_items()]

    def _select_all_files(self, *_):
        if self.selection_updating:
            return
        if self.select_all.get_active():
            chosen = Gtk.Bitset.new_empty()
            for index in range(self.filtered.get_n_items()):
                if not self.filtered.get_item(index).folder:
                    chosen.add(index)
            self.selection.set_selection(chosen, Gtk.Bitset.new_range(0, self.filtered.get_n_items()))
        else:
            self.selection.unselect_all()

    def _download_file(self):
        items = [item for item in self._selected_items() if not item.folder]
        if len(items) > 1:
            self._download_many(items)
            return
        item = items[0] if items else None
        if not item or not self.connected:
            return
        dialog = Gtk.FileDialog(title="Save phone file", initial_name=item.name)

        def selected(current, result):
            try:
                output = current.save_finish(result).get_path()
            except GLib.Error:
                return
            if not output or os.path.lexists(output):
                self.receipt.set_text("Choose a new file name. Existing files are never replaced.")
                return
            if not self.connected:
                self.receipt.set_text("Reconnect the phone before saving this file.")
                return
            self.receipt.set_text("Saving phone file…")

            def completed(receipt, error):
                if error or not isinstance(receipt, dict):
                    self.receipt.set_text("The file could not be saved. Check the connection and phone permission.")
                    return
                digest = receipt.get("sha256", "")
                if len(digest) != 64 or any(char not in "0123456789abcdefABCDEF" for char in digest):
                    self.receipt.set_text("The download did not return a valid integrity receipt.")
                else:
                    self.receipt.set_text(f"Saved {GLib.format_size(receipt.get('bytes', 0))} · SHA-256: {digest}")

            self.backend.call(self._selected_arguments("download", item) + ["--output", output], completed)

        dialog.save(self, None, selected)

    def _download_many(self, items):
        if not self.connected or self.transfer_busy:
            return
        dialog = Gtk.FileDialog(title="Save selected files to folder")
        def selected(current, result):
            try:
                folder = current.select_folder_finish(result).get_path()
            except GLib.Error:
                return
            if not folder or self.transfer_busy or not self.connected:
                return
            jobs = deque(items)
            total, failed = len(jobs), []
            self.transfer_busy = True
            self._update_controls()
            def next_file():
                if self.shutting_down:
                    return
                if not jobs or not self.connected:
                    failed.extend(jobs)
                    self.transfer_busy = False
                    self.receipt.set_text(f"Saved {total - len(failed)} of {total} files" +
                                          (f" · {len(failed)} failed; select them to retry" if failed else ""))
                    self._update_controls()
                    self._send_pending()
                    return
                item = jobs.popleft()
                output = available_download_path(folder, item.name)
                self.receipt.set_text(f"Saving file {total - len(jobs)} of {total}…")
                def completed(receipt, error):
                    if error or not valid_download_receipt(receipt, item.size):
                        failed.append(item)
                    next_file()
                self.backend.call(self._selected_arguments("download", item) + ["--output", output], completed)
            next_file()
        dialog.select_folder(self, None, selected)

    def _import_file(self):
        if not self.connected or self.transfer_busy:
            return
        dialog = Gtk.FileDialog(title="Send files to phone")
        def selected(current, result):
            try:
                files = current.open_multiple_finish(result)
            except GLib.Error:
                return
            self.send_files([files.get_item(index) for index in range(files.get_n_items())])
        dialog.open_multiple(self, None, selected)

    def send_files(self, files):
        if len(files) + len(self.pending_sends) > 200:
            self.receipt.set_text("Send up to 200 files at a time.")
            return
        paths = [file.get_path() for file in files]
        if any(not path or not private_regular(path, 1024 * 1024 * 1024) for path in paths):
            self.receipt.set_text("Choose regular local files of at most 1 GiB each, without symbolic links.")
            return
        self.pending_sends.extend(paths)
        if not self.connected:
            self._navigate("settings")
            self.connection_status.set_text(f"Connect your phone to send {len(self.pending_sends)} queued files")
        elif not self.transfer_busy:
            self._send_pending()

    def _send_pending(self):
        if not self.pending_sends or self.transfer_busy or not self.connected or self.shutting_down:
            return
        self.transfer_busy = True
        self._update_controls()
        total, failed = len(self.pending_sends), []
        done = 0
        def next_file():
            nonlocal done
            if self.shutting_down:
                return
            if not self.pending_sends or not self.connected:
                self.transfer_busy = False
                self.receipt.set_text(f"Sent {done - len(failed)} of {done} files" +
                                      (f" · {len(failed)} failed; send them again to retry" if failed else "") +
                                      (f" · {len(self.pending_sends)} waiting for connection" if self.pending_sends else ""))
                self._update_controls()
                return
            local = self.pending_sends.popleft()
            done += 1
            self.receipt.set_text(f"Sending file {done} of {max(total, done + len(self.pending_sends))}…")
            def completed(result, error):
                if error or not isinstance(result, dict):
                    failed.append(local)
                next_file()
            self.backend.call(["upload", "--local", local, "--to", "/storage/emulated/0/Download"], completed)
        next_file()

    def _poll_status(self):
        if self.shutting_down:
            return GLib.SOURCE_REMOVE
        if self.polling:
            return GLib.SOURCE_CONTINUE
        self.polling = True

        def completed(result, error):
            self.polling = False
            if self.shutting_down:
                return
            was_connected = self.connected
            if error or not isinstance(result, dict):
                self.connected = bool(self.owned and self.event_connected)
                self.external_session = False
                self.connection_status.set_text("Connected to phone; local service unavailable — reconnect" if self.connected
                    else "Waiting for your phone…" if self.owned else "Connect your phone to get started")
            else:
                self.connected = bool(result.get("connected"))
                device_name = result.get("device_name") or "vivo phone"
                self.device_title.set_text(device_name)
                self.phone_nav_label.set_text(device_name)
                self.nav_buttons["files"].set_tooltip_text(device_name)
                self.nav_buttons["files"].update_property([Gtk.AccessibleProperty.LABEL], [device_name])
                self.search.set_placeholder_text(f"Search {device_name}’s loaded files")
                self.external_session = not self.owned
                if self.connected:
                    self.connection_status.set_text("Connected · Your phone is ready" if result.get("receive_ready")
                        else "Connected · Reconnect to enable incoming files")
                else:
                    self.connection_status.set_text("Waiting for your phone…" if self.owned else "Connect your phone to get started")
            if not self.connected and self.pending_sends:
                self.connection_status.set_text(f"Connect your phone to send {len(self.pending_sends)} queued files")
            if was_connected and not self.connected:
                self.generation += 1
                self.store.remove_all()
                self.loaded = self.has_more = False
                self.thumbnail_pending.clear()
            self._update_controls()
            self._files_changed()
            if self.connected and not self.loaded and not self.loading:
                self._load_files(True)
            if self.connected and not was_connected:
                self._navigate("files")
                self._send_pending()

        self.backend.call(["native-status"], completed)
        return GLib.SOURCE_CONTINUE

    def _update_controls(self):
        if not hasattr(self, "download_button"):
            return
        selected = self._selected_items() if hasattr(self, "selection") else []
        files = [item for item in selected if not item.folder]
        item = selected[0] if len(selected) == 1 else None
        self.import_button.set_sensitive(self.connected and not self.shutting_down and not self.transfer_busy)
        self.download_button.set_sensitive(self.connected and bool(files) and not self.transfer_busy)
        self.download_button.set_tooltip_text(f"Save {len(files)} selected files" if len(files) > 1 else "Save selected file to computer")
        self.selection_updating = True
        count = sum(not self.filtered.get_item(i).folder for i in range(self.filtered.get_n_items()))
        self.select_all.set_active(bool(count) and len(files) == count)
        self.select_all.set_inconsistent(0 < len(files) < count)
        self.selection_updating = False
        self.preview_button.set_sensitive(self.connected and item is not None and not self.preview_busy)
        self.mirror_button.set_sensitive(self.connected and not self.shutting_down)
        self.connection_pill.set_visible(self.connected)
        self.connect_button.set_label("Disconnect" if self.owned else "Connected" if self.connected else "Connect phone")
        self.connect_button.set_sensitive(self.owned or not self.external_session)
        if self.media_panel:
            self.media_panel.set_connected(self.connected)
        if self.mirror_window:
            self.mirror_window.set_connected(self.connected)
        if hasattr(self.app, "update_tray"):
            self.app.update_tray()

    def _connection_action(self):
        if self.owned:
            self.backend.disconnect()
            self.connection_status.set_text("Disconnecting…")
        else:
            self._start_pair()

    def _start_pair(self):
        if self.owned or self.external_session:
            self.connection_status.set_text("Use the existing connection, or disconnect its owner before pairing again.")
            return
        phone = self.phone_entry.get_text().strip()
        if phone:
            try:
                address = ipaddress.ip_address(phone)
                if not address.is_private or address.is_loopback or address.is_unspecified:
                    raise ValueError
            except ValueError:
                self.connection_status.set_text("Enter a private IP override, or leave it empty to discover the scanned phone.")
                return
        receive = self.receive_entry.get_text().strip()
        if not os.path.isabs(receive):
            self.connection_status.set_text("Choose an absolute receive folder in Settings.")
            return
        self._save_preferences()
        self.active_receive_directory = receive
        self.event_connected = False
        self.connection_status.set_text("Creating a pairing QR…")
        self.owned = True
        try:
            self.backend.pair(phone, True, receive,
                self.clipboard_switch.get_active(), self.notification_switch.get_active(),
                self._pair_event, self._pair_exited)
        except (OSError, RuntimeError, ValueError):
            self.owned = False
            self.connection_status.set_text("The connection could not start. Check the installed backend.")
        self._update_controls()

    def _pair_event(self, event):
        if self.shutting_down or not isinstance(event, dict):
            return
        stage = event.get("stage")
        if stage == "qr_ready":
            path = event.get("qr_path", "")
            if not private_regular(path, 2 * 1024 * 1024):
                self.connection_status.set_text("The pairing QR is unavailable. Reconnect and try again.")
                return
            if self.qr_dialog:
                self.qr_dialog.close()
            self.qr_dialog = Adw.Dialog(title="Scan with Office Kit", content_width=360)
            content = box(True, 16)
            for method in (content.set_margin_top, content.set_margin_bottom, content.set_margin_start, content.set_margin_end):
                method(24)
            content.append(label("Scan this QR with Office Kit on your phone", wrap=True))
            picture = Gtk.Picture.new_for_filename(path)
            picture.set_content_fit(Gtk.ContentFit.CONTAIN)
            picture.set_size_request(280, 280)
            content.append(picture)
            content.append(label("Approve this computer on the phone", "muted small", wrap=True))
            self.qr_dialog.set_child(content)
            self.qr_dialog.present(self)
            self.connection_status.set_text("Scan the QR, then approve the connection on your phone")
        elif stage == "control_connected":
            self.event_connected = True
            self._navigate("files")
            if self.qr_dialog:
                self.qr_dialog.close()
                self.qr_dialog = None
            self._poll_status()
        elif stage == "control_disconnected":
            self.event_connected = False
            self._poll_status()
        elif stage == "incoming_share_started":
            self.receipt.set_text(f"Receiving {event.get('count', 0)} phone files…")
        elif stage == "incoming_share_completed":
            self.receipt.set_text(f"Received {event.get('count', 0)} phone files" +
                (f" in {self.active_receive_directory}" if self.active_receive_directory else ""))
        elif stage in {"incoming_share_failed", "incoming_share_rejected", "incoming_share_queue_full"}:
            self.receipt.set_text("An incoming transfer could not finish. Check the receive folder and share again.")
        elif stage in {"file_permission_denied", "file_permission_timeout"}:
            self.receipt.set_text("Approve file access in Office Kit on the phone to browse and transfer files.")

    def _pair_exited(self, *_args):
        self.owned = self.event_connected = False
        if self.qr_dialog:
            self.qr_dialog.close()
            self.qr_dialog = None
        self._poll_status()
        self._update_controls()

    def _choose_receive_folder(self):
        dialog = Gtk.FileDialog(title="Choose receive folder")
        current = self.receive_entry.get_text().strip()
        if os.path.isabs(current):
            dialog.set_initial_folder(Gio.File.new_for_path(current))

        def selected(current_dialog, result):
            try:
                selected_file = current_dialog.select_folder_finish(result)
                if selected_file.get_path():
                    self.receive_entry.set_text(selected_file.get_path())
            except GLib.Error:
                pass

        dialog.select_folder(self, None, selected)

    def _save_preferences(self):
        if not hasattr(self, "receive_entry"):
            return
        for key, value in (("manual_phone", self.phone_entry.get_text().strip()),
                           ("receive_dir", self.receive_entry.get_text().strip()),
                           ("clipboard", self.clipboard_switch.get_active()),
                           ("notifications", self.notification_switch.get_active())):
            self.preferences.set(key, value)
        self.preferences.save()

    def _theme_changed(self, current, _param):
        theme = ("dark", "system", "light")[current.get_selected()]
        self.preferences.set("theme", theme)
        self.preferences.save()
        self._apply_theme(theme)

    def _apply_theme(self, theme):
        style = Adw.StyleManager.get_default()
        style.set_color_scheme({"dark": Adw.ColorScheme.FORCE_DARK,
                                "light": Adw.ColorScheme.FORCE_LIGHT,
                                "system": Adw.ColorScheme.DEFAULT}.get(theme, Adw.ColorScheme.FORCE_DARK))

    def _open_mirror(self, activation_token=""):
        if not self.connected:
            return
        try:
            from .media import MirrorWindow
            if self.mirror_window is None:
                self.mirror_window = MirrorWindow(self.app, self.backend)
            self.mirror_window.set_connected(self.connected)
            if activation_token:
                self.mirror_window.set_startup_id(activation_token)
            self.mirror_window.present()
            self.mirror_window.start()
        except (ImportError, GLib.Error, RuntimeError):
            self.receipt.set_text("Phone mirroring is unavailable. Check the media connection and reconnect.")

    def _open_camera(self):
        try:
            from .media import MediaPanel
            if self.media_panel is None:
                self.media_panel = MediaPanel(self.backend, mode="camera")
                self.camera_page.append(self.media_panel)
            self.media_panel.set_connected(self.connected)
            self.pages.set_visible_child_name("camera")
        except (ImportError, GLib.Error, RuntimeError):
            self.receipt.set_text("Camera sharing is unavailable. Check the installed media components.")

    def _hide(self, *_args):
        self.set_visible(False)
        return True

    def shutdown(self):
        if self.shutting_down:
            return
        self.shutting_down = True
        self.generation += 1
        self._save_preferences()
        if self.poll_source:
            GLib.source_remove(self.poll_source)
            self.poll_source = None
        self.thumbnail_pending.clear()
        self.decoder.shutdown(wait=False, cancel_futures=True)
        if self.media_panel:
            self.media_panel.stop()
        if self.mirror_window:
            self.mirror_window.stop()
        for viewer in tuple(self.viewers):
            viewer.close()


class OfficeApplication(Adw.Application):
    def __init__(self, backend=None, settings=None, testing=False):
        super().__init__(application_id="io.github.vivolinux.OfficeKit",
                         flags=(Gio.ApplicationFlags.NON_UNIQUE if testing else Gio.ApplicationFlags.DEFAULT_FLAGS) | Gio.ApplicationFlags.HANDLES_OPEN)
        if backend is None or settings is None:
            from .backend import Backend, Settings
            backend = backend if backend is not None else Backend()
            settings = settings if settings is not None else Settings()
        self.backend = backend
        self.settings = settings
        self.testing = testing
        self.window = None
        self.test_exit = 0
        self.tray = None

    def do_startup(self):
        Adw.Application.do_startup(self)
        provider = Gtk.CssProvider()
        provider.load_from_path(str(Path(__file__).with_name("style.css")))
        Gtk.StyleContext.add_provider_for_display(Gdk.Display.get_default(), provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
        for name, callback in (("quit", self._quit), ("preferences", self._preferences), ("search", self._focus_search)):
            action = Gio.SimpleAction.new(name, None)
            action.connect("activate", callback)
            self.add_action(action)
        self.set_accels_for_action("app.quit", ["<Control>q"])
        self.set_accels_for_action("app.search", ["<Control>f"])
        if not self.testing and self.get_dbus_connection():
            try:
                from .tray import StatusTray
                self.tray = StatusTray(self.get_dbus_connection(), self._tray_action)
            except (ImportError, GLib.Error):
                print("VOkit: system tray unavailable; reopen from the application menu.", file=sys.stderr)
        self.hold()

    def _ensure_window(self):
        if self.window is None:
            self.window = OfficeWindow(self)
            if self.testing:
                GLib.timeout_add(350, self._self_test)
        return self.window

    def do_activate(self):
        self._ensure_window().present()
        self.update_tray()

    def update_tray(self):
        if not self.tray or not self.window:
            return
        window = self.window
        mirror = window.mirror_window
        session = mirror.panel.session if mirror else None
        active = bool(session and not session.closed.is_set())
        self.tray.update(window.connected, window.owned and not window.connected,
                         window.device_title.get_text(), window.owned, active, window.external_session)

    def _tray_action(self, action, token=""):
        if action == "quit":
            self._quit()
            return
        window = self._ensure_window()
        if token:
            window.set_startup_id(token)
        if action == "mirror":
            window._open_mirror(token)
        elif action == "stop-mirror":
            if window.mirror_window:
                window.mirror_window.stop()
                window.mirror_window.set_visible(False)
        elif action == "connection":
            if not window.owned:
                window.present()
            window._connection_action()
        else:
            window.present()
            if action == "camera":
                window._open_camera()
            elif action in ("files", "settings"):
                window._navigate(action)
        self.update_tray()

    def do_open(self, files, count, hint):
        self.activate()
        self.window.send_files(files)

    def _preferences(self, *_args):
        self.activate()
        self.window._navigate("settings")

    def _focus_search(self, *_args):
        self.activate()
        self.window.search.grab_focus()

    def _quit(self, *_args):
        if self.tray:
            self.tray.close()
        if self.window:
            self.window.shutdown()
        self.backend.close()
        self.quit()

    def do_shutdown(self):
        if self.tray:
            self.tray.close()
        if self.window:
            self.window.shutdown()
        self.backend.close()
        Adw.Application.do_shutdown(self)

    def _self_test(self):
        # Exercise reaching the bottom on both small and large test displays.
        window = self.window
        if window.store.get_n_items() < 5:
            attempts = getattr(self, "_test_attempts", 0) + 1
            self._test_attempts = attempts
            if attempts <= 30:
                adjustment = window.file_views.get_visible_child().get_vadjustment()
                adjustment.set_value(max(0, adjustment.get_upper()-adjustment.get_page_size()))
                window._maybe_more()
                GLib.timeout_add(100, self._self_test)
                return GLib.SOURCE_REMOVE
        try:
            window = self.window
            assert window.connected and window.category == "recent"
            assert window.get_title() == "VOkit"
            assert window.device_title.get_text() == "Fixture phone"
            window._navigate("settings")
            window.close_settings_button.emit("clicked")
            assert window.pages.get_visible_child_name() == "files"
            assert isinstance(window.grid, Gtk.GridView) and isinstance(window.store, Gio.ListStore)
            assert isinstance(window.selection, Gtk.MultiSelection)
            window.selection.select_item(0, False)
            window.selection.select_item(1, False)
            assert len(window._selected_items()) == 2 and window.download_button.get_sensitive()
            assert not window.preview_button.get_sensitive()
            window.select_all.set_active(True)
            assert len(window._selected_items()) == 5
            window.select_all.set_active(False)
            assert window._selected_items() == []
            assert window.store.get_n_items() == 5, "automatic page loading"
            assert window.store.get_item(0).page == 0 and window.store.get_item(4).page == 1
            assert window.store.get_item(4).category == "recent"
            item = window.store.get_item(4)
            args = window._selected_arguments("download", item)
            assert args[args.index("--page") + 1] == "1" and args[args.index("--category") + 1] == "recent"
            window.query = "PICTURE".casefold()
            window.filter.changed(Gtk.FilterChange.DIFFERENT)
            assert window.filtered.get_n_items() == 1
            window.query = "absent"
            window.filter.changed(Gtk.FilterChange.DIFFERENT)
            assert window.filtered.get_n_items() == 0
            window.query = ""
            window.filter.changed(Gtk.FilterChange.DIFFERENT)
            for category in ("recent", "images", "videos", "audio", "documents"):
                window.category = category
                listing = window._listing_arguments(2)
                assert "--path" not in listing and listing[-1] == category
            window.category = ""
            assert "--path" in window._listing_arguments(0)
            window.category = "recent"
            assert window.phone_entry.get_text() == ""
            window._save_preferences()
            assert "trust" not in self.settings.values
            window._apply_theme("light")
            assert not Adw.StyleManager.get_default().get_dark()
            window._apply_theme("dark")
            assert Adw.StyleManager.get_default().get_dark()
            window._pair_event({"stage": "incoming_share_completed", "count": 3})
            assert "Received 3" in window.receipt.get_text()
            assert not OfficeWindow._valid_entry({"fileName": "bad", "savePath": "/storage/../bad"})
            window.close()
            assert not window.get_visible() and not self.backend.closed
            self.activate()
            assert window.get_visible()
            print("GTK desktop self-test passed", flush=True)
        except Exception as error:
            print(f"GTK desktop self-test failed: {type(error).__name__}: {error}", file=sys.stderr, flush=True)
            self.test_exit = 1
        self._quit()
        return GLib.SOURCE_REMOVE


class _TestSettings:
    def __init__(self):
        self.values = {}

    def get(self, key, default=None):
        return self.values.get(key, default)

    def set(self, key, value):
        self.values[key] = value

    def save(self):
        pass


class _TestBackend:
    """No processes, network, pairing state, or real user preferences are accessed."""
    def __init__(self):
        self.closed = False
        self.calls = []
        self.cli_path = "unused-test-backend"
        self.native_socket = self.media_socket = "unused-test-socket"

    def call(self, args, done):
        self.calls.append(args)
        if args[0] == "native-status":
            result = {"connected": True, "device_name": "Fixture phone", "receive_ready": True, "receive_enabled": True}
        elif args[0] == "files":
            page = int(args[args.index("--page") + 1])
            names = ["picture.jpg", "song.mp3", "document.pdf"] if page == 0 else ["Spring afternoon.jpg", "clip.mp4"] if page == 1 else []
            result = {"dataList": [{"fileName": name, "savePath": "/storage/emulated/0/Download/" + name,
                                    "fileSize": 145000 + index, "isDirectory": False}
                                   for index, name in enumerate(names)], "totalCount": 5}
        else:
            result = None
        GLib.idle_add(lambda: done(result, None if result is not None else "test unavailable") or GLib.SOURCE_REMOVE)

    def close(self):
        self.closed = True

    def disconnect(self):
        pass


def main(argv=None):
    parser = argparse.ArgumentParser(description="VOkit native GTK desktop")
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("files", nargs="*", help="Local files to send to the connected phone")
    options = parser.parse_args(argv)
    app = OfficeApplication(_TestBackend(), _TestSettings(), testing=True) if options.self_test else OfficeApplication()
    code = app.run([sys.argv[0]] + [Gio.File.new_for_commandline_arg(path).get_uri() for path in options.files])
    return app.test_exit or code


if __name__ == "__main__":
    raise SystemExit(main())
