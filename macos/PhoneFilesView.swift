import AppKit
import Foundation
import SwiftUI

// Preview downloads are validated by internal/vivo/media_files.go.
// Keep the preview action and requested thumbnails on the same file types.
private let previewExtensions: Set<String> = [
    "jpg", "jpeg", "png", "gif", "mp4", "mov", "m4v", "3gp", "webm"
]

private struct PhoneFile: Decodable, Identifiable {
    let fileName: String
    let savePath: String
    let fileSize: Int64
    let isDirectory: Bool
    let id: String
    let page: Int
    let category: String

    enum CodingKeys: String, CodingKey {
        case fileName, savePath, fileSize, isDirectory
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        fileName = try values.decode(String.self, forKey: .fileName)
        savePath = try values.decode(String.self, forKey: .savePath)
        fileSize = max(0, try values.decodeIfPresent(Int64.self, forKey: .fileSize) ?? 0)
        isDirectory = try values.decodeIfPresent(Bool.self, forKey: .isDirectory) ?? false
        id = savePath
        page = 0
        category = ""
    }

    init(fileName: String, savePath: String, fileSize: Int64, isDirectory: Bool, page: Int, category: String) {
        self.fileName = fileName
        self.savePath = savePath
        self.fileSize = max(0, fileSize)
        self.isDirectory = isDirectory
        self.id = savePath
        self.page = page
        self.category = category
    }
}

private struct PhoneFilePage: Decodable {
    let dataList: [PhoneFile]
    let totalCount: Int64
}

@MainActor
private final class PhoneFilesState: ObservableObject {
    static let shared = PhoneFilesState()
    @Published var category = "recent"
    @Published var directory = "/storage/emulated/0"
    @Published var files: [PhoneFile] = []
    @Published var page = 0
    @Published var hasMore = false
    @Published var busy = false
    @Published var status = ""
    @Published var selection: String?
    @Published var search = ""
    @Published var showGrid = UserDefaults.standard.object(forKey: "macFilesGrid") as? Bool ?? true {
        didSet { UserDefaults.standard.set(showGrid, forKey: "macFilesGrid") }
    }
    var pendingRefresh = false
}

/// Browse and transfer files through the active VOkit native phone session.
@MainActor
struct PhoneFilesView: View {
    let connected: Bool

    @ObservedObject private var state = PhoneFilesState.shared
    private var category: String { get { state.category } nonmutating set { state.category = newValue } }
    private var directory: String { get { state.directory } nonmutating set { state.directory = newValue } }
    private var files: [PhoneFile] { get { state.files } nonmutating set { state.files = newValue } }
    private var page: Int { get { state.page } nonmutating set { state.page = newValue } }
    private var hasMore: Bool { get { state.hasMore } nonmutating set { state.hasMore = newValue } }
    private var busy: Bool { get { state.busy } nonmutating set { state.busy = newValue } }
    private var status: String { get { state.status } nonmutating set { state.status = newValue } }
    private var selection: String? { get { state.selection } nonmutating set { state.selection = newValue } }
    private var search: String { get { state.search } nonmutating set { state.search = newValue } }
    private var showGrid: Bool { get { state.showGrid } nonmutating set { state.showGrid = newValue } }
    @ObservedObject private var thumbnails = PhoneThumbnailCache.shared

    private let categories: [(String, String)] = [
        ("recent", "Recent files"), ("images", "Images"), ("videos", "Videos"),
        ("", "Phone storage"),
        ("audio", "Audio"), ("documents", "Documents")
    ]

    private var visibleFiles: [PhoneFile] {
        guard !search.isEmpty else { return files }
        return files.filter { $0.fileName.localizedStandardContains(search) }
    }

    var body: some View {
        HStack(spacing: 0) {
            VStack(alignment: .leading, spacing: 4) {
                Text("LIBRARY")
                    .font(.system(size: 10, weight: .bold))
                    .tracking(1.3)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 10)
                    .padding(.bottom, 12)
                ForEach(categories, id: \.0) { item in
                    Button {
                        category = item.0
                    } label: {
                        Label(item.1, systemImage: categoryIcon(item.0))
                            .font(.system(size: 12, weight: category == item.0 ? .semibold : .regular))
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.vertical, 10)
                            .padding(.horizontal, 10)
                            .background(category == item.0 ? Color.accentColor.opacity(0.13) : .clear,
                                        in: RoundedRectangle(cornerRadius: 9))
                    }
                    .buttonStyle(.plain)
                    .disabled(!connected)
                }
                Spacer(minLength: 0)
            }
            .padding(12)
            .frame(width: 162)
            .background(Color(nsColor: .controlBackgroundColor).opacity(0.65))
            Divider()
            VStack(spacing: 0) {
            HStack(spacing: 12) {
                VStack(alignment: .leading, spacing: 3) {
                    Text(categories.first(where: { $0.0 == category })?.1 ?? "Phone files")
                        .font(.title3.weight(.semibold))
                    Text(connected ? (category.isEmpty ? directory : "Browse files on your phone") : "Connect your phone to browse files")
                        .font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                }
                Spacer(minLength: 8)
                if category.isEmpty {
                    Button { openFolder(URL(fileURLWithPath: directory).deletingLastPathComponent().path) } label: {
                        Label("Up", systemImage: "arrow.up")
                    }
                    .help("Open parent folder")
                    .disabled(!connected || busy || directory == "/")
                }
            }
            .padding(.horizontal, 18).padding(.vertical, 13)
            .background(.bar)

            HStack(spacing: 8) {
                Button { chooseUpload() } label: { Label("Import files", systemImage: "square.and.arrow.up") }
                    .disabled(!connected || busy)
                Divider().frame(height: 18)
                Button { downloadSelected() } label: { Label("Download", systemImage: "arrow.down.to.line") }
                    .disabled(!connected || busy || selectedFile == nil || selectedFile?.isDirectory == true)
                Button { previewSelected() } label: { Label("Preview", systemImage: "eye") }
                    .disabled(!connected || busy || selectedFile == nil || selectedFile?.isDirectory == true)
                Spacer()
                TextField("Search loaded files", text: Binding(get: { search }, set: { search = $0 }))
                    .textFieldStyle(.roundedBorder)
                    .frame(maxWidth: 180)
                    .disabled(!connected)
                HStack(spacing: 0) {
                    Button { showGrid = true } label: { Image(systemName: "square.grid.2x2") }
                        .help("Thumbnail grid")
                        .tint(showGrid ? Color.accentColor : Color.secondary)
                    Button { showGrid = false } label: { Image(systemName: "list.bullet") }
                        .help("List")
                        .tint(showGrid ? Color.secondary : Color.accentColor)
                }
                .padding(.leading, 4)
            }
            .buttonStyle(.borderless)
            .padding(.horizontal, 18).padding(.vertical, 9)
            .background(Color(nsColor: .controlBackgroundColor))

            ScrollView {
                if showGrid {
                    LazyVGrid(columns: [GridItem(.adaptive(minimum: 132, maximum: 190), spacing: 14)], spacing: 17) {
                        ForEach(visibleFiles) { file in fileTile(file) }
                    }
                    .padding(16)
                } else {
                    LazyVStack(spacing: 4) {
                        ForEach(visibleFiles) { file in fileRow(file) }
                    }
                    .padding(12)
                }
                if hasMore {
                    Button(busy ? "Loading…" : "Load more from phone") { loadFiles(reset: false) }
                        .disabled(busy || !connected)
                        .padding()
                }
            }
            .overlay {
                if visibleFiles.isEmpty {
                    VStack(spacing: 10) {
                        Image(systemName: emptySymbol)
                            .font(.system(size: 30, weight: .regular))
                            .foregroundStyle(isLoadError ? Color.orange : Color.secondary)
                        Text(emptyTitle).font(.headline)
                        Text(emptyMessage).font(.callout).foregroundStyle(.secondary)
                            .multilineTextAlignment(.center).frame(maxWidth: 320)
                        if connected && !busy && search.isEmpty {
                            Button("Refresh") { loadFiles(reset: true) }.padding(.top, 3)
                        }
                    }
                    .padding(24)
                }
            }
            .onChange(of: category) { _ in
                loadFiles(reset: true)
            }

            HStack {
                Text(status.isEmpty ? (connected ? "\(files.count) items" : "Phone disconnected") : status)
                    .foregroundStyle(.secondary).lineLimit(2)
                if hasMore && !search.isEmpty { Text("Search covers loaded files").foregroundStyle(.secondary) }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button { loadFiles(reset: true) } label: { Image(systemName: "arrow.clockwise") }
                    .buttonStyle(.borderless).help("Refresh phone files").disabled(!connected || busy)
            }
            .padding(.horizontal)
            .padding(.vertical, 8)
            }
        }
        .onChange(of: connected) { value in
            if value { loadFiles(reset: true) }
            else {
                files = []; hasMore = false; state.pendingRefresh = false
                status = "Phone disconnected"; thumbnails.clear()
            }
        }
        .onAppear { if connected { loadFiles(reset: true) } }
    }

    private var selectedFile: PhoneFile? { files.first { $0.savePath == selection } }

    private func categoryIcon(_ value: String) -> String {
        switch value {
        case "recent": return "clock.arrow.circlepath"
        case "images": return "photo"
        case "videos": return "play.rectangle"
        case "audio": return "waveform"
        case "documents": return "doc.text"
        default: return "externaldrive"
        }
    }

    private func fileTile(_ file: PhoneFile) -> some View {
        Button {
            if file.isDirectory { openFolder(file.savePath) }
            else { selection = file.savePath }
        } label: {
            VStack(alignment: .leading, spacing: 5) {
                fileArtwork(file)
                    .frame(maxWidth: .infinity)
                    .frame(height: 117)
                    .background(Color.primary.opacity(0.045), in: RoundedRectangle(cornerRadius: 9))
                    .clipShape(RoundedRectangle(cornerRadius: 9))
                Text(file.fileName)
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(.primary)
                    .lineLimit(2)
                    .frame(height: 30, alignment: .topLeading)
                Text(file.isDirectory ? "Folder" : ByteCountFormatter.string(fromByteCount: file.fileSize, countStyle: .file))
                    .font(.system(size: 10)).foregroundStyle(.secondary)
            }
            .padding(5)
            .background(selection == file.savePath ? Color.accentColor.opacity(0.12) : .clear,
                        in: RoundedRectangle(cornerRadius: 11))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .simultaneousGesture(TapGesture(count: 2).onEnded {
            if !file.isDirectory { selection = file.savePath; previewSelected() }
        })
        .contextMenu { fileMenu(file) }
        .onAppear { if connected { thumbnails.request(file) } }
    }

    private func fileRow(_ file: PhoneFile) -> some View {
        Button {
            if file.isDirectory { openFolder(file.savePath) }
            else { selection = file.savePath }
        } label: {
            HStack(spacing: 12) {
                fileArtwork(file)
                    .frame(width: 46, height: 43)
                    .background(Color.primary.opacity(0.045), in: RoundedRectangle(cornerRadius: 7))
                    .clipShape(RoundedRectangle(cornerRadius: 7))
                Text(file.fileName).lineLimit(1).foregroundStyle(.primary)
                Spacer()
                Text(file.isDirectory ? "Folder" : ByteCountFormatter.string(fromByteCount: file.fileSize, countStyle: .file))
                    .foregroundStyle(.secondary).font(.caption)
            }
            .padding(6)
            .background(selection == file.savePath ? Color.accentColor.opacity(0.12) : .clear,
                        in: RoundedRectangle(cornerRadius: 9))
        }
        .buttonStyle(.plain)
        .simultaneousGesture(TapGesture(count: 2).onEnded {
            if !file.isDirectory { selection = file.savePath; previewSelected() }
        })
        .contextMenu { fileMenu(file) }
        .onAppear { if connected { thumbnails.request(file) } }
    }

    private func fileArtwork(_ file: PhoneFile) -> some View {
        Group {
            if let thumbnail = thumbnails.images[file.savePath] {
                Image(nsImage: thumbnail).resizable().scaledToFill()
            } else {
                Image(systemName: file.isDirectory ? "folder.fill" : icon(for: file.fileName))
                    .font(.system(size: 26))
                    .foregroundStyle(file.isDirectory ? Color.accentColor : Color.secondary)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
    }

    @ViewBuilder private func fileMenu(_ file: PhoneFile) -> some View {
        if file.isDirectory {
            Button("Open folder") { openFolder(file.savePath) }
        } else {
            Button("Preview") { selection = file.savePath; previewSelected() }
            Button("Download…") { selection = file.savePath; downloadSelected() }
        }
    }

    private var emptyTitle: String {
        if !connected { return "Phone Not Connected" }
        if busy { return "Loading Phone Files" }
        if !search.isEmpty && visibleFiles.isEmpty { return "No matching loaded files" }
        if isLoadError { return "Couldn’t Load Files" }
        return category.isEmpty ? "This Folder Is Empty" : "No Files Found"
    }

    private var emptySymbol: String {
        if !connected { return "iphone.slash" }
        if busy { return "arrow.triangle.2.circlepath" }
        if isLoadError { return "exclamationmark.icloud" }
        return category.isEmpty ? "folder" : "doc.on.doc"
    }

    private var emptyMessage: String {
        if !connected { return "Connect your phone to browse and transfer files." }
        if busy { return status.isEmpty ? "Reading the selected location…" : status }
        if !search.isEmpty && visibleFiles.isEmpty { return "Try another search or load more files." }
        if isLoadError { return status }
        return category.isEmpty ? "There are no items in this location." : "Try another collection or refresh the list."
    }

    private var isLoadError: Bool {
        !status.isEmpty && !status.contains("files and folders") && !status.hasPrefix("Reading") && !status.hasPrefix("Loading")
    }

    private func loadFiles(reset: Bool) {
        guard connected else { return }
        if busy {
            if reset { state.pendingRefresh = true }
            return
        }
        if reset { files = []; page = 0; hasMore = false; selection = nil }
        if reset { thumbnails.clear() }
        let requestedPage = page
        busy = true
        status = requestedPage == 0 ? "Reading phone files…" : "Loading more files…"
        var args = ["files", "--page", String(requestedPage)]
        args += category.isEmpty ? ["--path", directory] : ["--category", category]
        run(args, as: PhoneFilePage.self) { result in
            busy = false
            if state.pendingRefresh {
                state.pendingRefresh = false
                if PairSession.shared.connected { loadFiles(reset: true) }
                return
            }
            guard PairSession.shared.connected else {
                files = []; hasMore = false; status = "Phone disconnected"
                return
            }
            switch result {
            case .success(let listing):
                guard listing.dataList.count <= 2000 else { status = "Phone returned too many files."; return }
                let valid = listing.dataList.filter(validPhoneFile)
                let existing = Set(files.map(\.savePath))
                let added = valid.filter { !existing.contains($0.savePath) }.map {
                    PhoneFile(fileName: $0.fileName, savePath: $0.savePath, fileSize: $0.fileSize,
                              isDirectory: $0.isDirectory, page: requestedPage, category: category)
                }
                files.append(contentsOf: added)
                page += 1
                hasMore = !added.isEmpty && (listing.dataList.count >= 200 || Int64(files.count) < listing.totalCount)
                status = "\(files.count) files and folders" + (hasMore ? " · More available" : "")
            case .failure(let error):
                status = error.localizedDescription
                hasMore = false
            }
        }
    }

    private func validPhoneFile(_ file: PhoneFile) -> Bool {
        let path = NSString(string: file.savePath).standardizingPath
        return file.fileName != "." && file.fileName != ".." && !file.fileName.contains("/") &&
            !file.fileName.contains("\\") && !file.fileName.contains("\0") && file.savePath.hasPrefix("/") &&
            path == file.savePath && !file.savePath.contains("\\") &&
            (file.isDirectory || URL(fileURLWithPath: file.savePath).lastPathComponent == file.fileName)
    }

    private func openFolder(_ path: String) {
        guard path.hasPrefix("/"), NSString(string: path).standardizingPath == path,
              !path.contains("\\"), !path.contains("\0") else { status = "Invalid phone folder."; return }
        directory = path
        if category.isEmpty {
            loadFiles(reset: true)
        } else {
            // The category change reloads the newly selected directory.
            category = ""
        }
    }

    private func downloadSelected() {
        guard let file = selectedFile, !file.isDirectory, connected else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = file.fileName
        panel.canCreateDirectories = true
        guard panel.runModal() == .OK, let url = panel.url else { return }
        guard !FileManager.default.fileExists(atPath: url.path) else { status = "Choose a new name. Existing files are never replaced."; return }
        busy = true; status = "Saving phone file…"
        run(["download", "--remote", file.savePath, "--output", url.path, "--page", String(file.page)] + categoryArg(file), as: TransferReceipt.self) { result in
            busy = false
            switch result {
            case .success(let receipt): status = "Saved \(ByteCountFormatter.string(fromByteCount: receipt.bytes, countStyle: .file)) · SHA-256 \(receipt.sha256)"
            case .failure(let error): status = error.localizedDescription
            }
        }
    }

    private func chooseUpload() {
        guard connected else { return }
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = false; panel.canChooseDirectories = false; panel.canChooseFiles = true
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            let values = try url.resourceValues(forKeys: [.isRegularFileKey, .fileSizeKey])
            guard values.isRegularFile == true, (values.fileSize ?? 0) <= 1_073_741_824 else {
                status = "Choose a regular file up to 1 GiB."; return
            }
        } catch { status = "Cannot read selected file."; return }
        let destination = category.isEmpty ? directory : "/storage/emulated/0/Download"
        busy = true; status = "Uploading to \(destination)…"
        run(["upload", "--local", url.path, "--to", destination], as: UploadReceipt.self) { result in
            busy = false
            switch result {
            case .success: status = "Upload complete"; loadFiles(reset: true)
            case .failure(let error): status = error.localizedDescription
            }
        }
    }

    private func previewSelected() {
        guard let file = selectedFile, !file.isDirectory, connected else { return }
        let ext = URL(fileURLWithPath: file.fileName).pathExtension.lowercased()
        guard previewExtensions.contains(ext) else {
            status = "Preview supports image and video files. Download other files to open them."; return
        }
        let cache = FileManager.default.temporaryDirectory
            .appendingPathComponent("VOkitPreview-\(UUID().uuidString).\(ext)")
        busy = true; status = "Preparing preview…"
        run(["preview", "--remote", file.savePath, "--output", cache.path, "--page", String(file.page)] + categoryArg(file), as: PreviewReceipt.self) { result in
            busy = false
            switch result {
            case .success:
                NSWorkspace.shared.open(cache)
                status = "Opened \(file.fileName)"
            case .failure(let error): status = error.localizedDescription
            }
        }
    }

    private func categoryArg(_ file: PhoneFile) -> [String] { file.category.isEmpty ? [] : ["--category", file.category] }

    private func icon(for name: String) -> String {
        switch URL(fileURLWithPath: name).pathExtension.lowercased() {
        case "jpg", "jpeg", "png", "gif", "heic", "webp", "bmp": return "photo"
        case "mp4", "mov", "m4v", "3gp", "webm": return "film"
        case "mp3", "m4a", "aac", "wav", "flac", "ogg", "opus": return "music.note"
        default: return "doc"
        }
    }

    private func run<T: Decodable>(_ arguments: [String], as type: T.Type, completion: @escaping (Result<T, Error>) -> Void) {
        guard let cli = Bundle.main.resourceURL?.appendingPathComponent("vivolinux"),
              FileManager.default.isExecutableFile(atPath: cli.path) else {
            completion(.failure(PhoneFilesError.backendMissing)); return
        }
        let process = Process()
        process.executableURL = cli
        let environment = ProcessInfo.processInfo.environment
        let runtimeSocket = environment["XDG_RUNTIME_DIR"].map {
            URL(fileURLWithPath: $0).appendingPathComponent("vivolinux/native.sock").path
        }
        let socket = runtimeSocket.flatMap { $0.utf8.count < 100 ? $0 : nil }
            ?? "/tmp/vivolinux-\(getuid())/native.sock"
        process.arguments = arguments + ["--socket", socket]
        process.standardInput = FileHandle.nullDevice
        let output = Pipe(); process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        DispatchQueue.global(qos: .userInitiated).async {
            do { try process.run() }
            catch {
                DispatchQueue.main.async { completion(.failure(PhoneFilesError.operationFailed)) }
                return
            }
            // Read while the subprocess runs: a large folder response can fill
            // a pipe and prevent the process from ever exiting otherwise.
            let data = output.fileHandleForReading.readDataToEndOfFile()
            process.waitUntilExit()
            let result: Result<T, Error>
            if process.terminationStatus != 0 {
                result = .failure(PhoneFilesError.operationFailed)
            } else {
                do {
                    // The Go native CLI emits the result itself, not a
                    // { result: ... } envelope.
                    result = .success(try JSONDecoder().decode(T.self, from: data))
                } catch { result = .failure(PhoneFilesError.invalidResponse) }
            }
            DispatchQueue.main.async { completion(result) }
        }
    }
}

private struct TransferReceipt: Decodable { let bytes: Int64; let sha256: String }
private struct UploadReceipt: Decodable {
    let bytes: Int64
    let localSHA256: String?
    enum CodingKeys: String, CodingKey { case bytes; case localSHA256 = "local_sha256" }
}
private struct PreviewReceipt: Decodable { let path: String }

private enum PhoneFilesError: LocalizedError {
    case backendMissing, operationFailed, invalidResponse, remote(String)
    var errorDescription: String? {
        switch self {
        case .backendMissing: return "VOkit backend is missing. Rebuild the app."
        case .operationFailed: return "Phone operation failed. Check the connection and file permission."
        case .invalidResponse: return "Phone returned an invalid response."
        case .remote(let message): return message == "file_permission_required" ? "Approve file access on the phone and retry." : "Phone file operation failed."
        }
    }
}

private struct PhoneThumbnailResponse: Decodable { let data: Data }

/// Loads only visible thumbnails, with bounded concurrency and memory use.
/// The Go backend validates each thumbnail and checks the active phone's file permission.
@MainActor
private final class PhoneThumbnailCache: ObservableObject {
    static let shared = PhoneThumbnailCache()
    @Published private(set) var images: [String: NSImage] = [:]
    private var requested = Set<String>()
    private var queued: [PhoneFile] = []
    private var order: [String] = []
    private var active = 0
    private var generation = 0

    func clear() {
        generation += 1
        queued.removeAll()
        requested.removeAll()
        order.removeAll()
        images.removeAll()
    }

    func request(_ file: PhoneFile) {
        let ext = URL(fileURLWithPath: file.fileName).pathExtension.lowercased()
        // Bound pending work while allowing large libraries to be browsed past
        // the first few hundred files. Requests reset on collection refresh.
        guard !file.isDirectory, previewExtensions.contains(ext),
              !requested.contains(file.savePath), queued.count < 64 else { return }
        requested.insert(file.savePath)
        queued.append(file)
        startNext()
    }

    private func startNext() {
        guard active < 3, !queued.isEmpty else { return }
        let file = queued.removeFirst()
        let run = generation
        active += 1
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let image: NSImage? = {
                guard let cli = Bundle.main.resourceURL?.appendingPathComponent("vivolinux"),
                      FileManager.default.isExecutableFile(atPath: cli.path) else { return nil }
                let process = Process()
                process.executableURL = cli
                var args = ["thumbnail", "--remote", file.savePath, "--page", String(file.page),
                            "--width", "240", "--height", "240"]
                if !file.category.isEmpty { args += ["--category", file.category] }
                let runtime = ProcessInfo.processInfo.environment["XDG_RUNTIME_DIR"]
                let runtimeSocket = runtime.map { "\($0)/vivolinux/native.sock" }
                let socket = runtimeSocket.flatMap { $0.utf8.count < 100 ? $0 : nil }
                    ?? "/tmp/vivolinux-\(getuid())/native.sock"
                process.arguments = args + ["--socket", socket]
                process.standardInput = FileHandle.nullDevice
                process.standardError = FileHandle.nullDevice
                let output = Pipe()
                process.standardOutput = output
                guard (try? process.run()) != nil else { return nil }
                let data = output.fileHandleForReading.readDataToEndOfFile()
                process.waitUntilExit()
                guard process.terminationStatus == 0, data.count <= 16 * 1024 * 1024,
                      let decoded = try? JSONDecoder().decode(PhoneThumbnailResponse.self, from: data),
                      let image = NSImage(data: decoded.data) else { return nil }
                return image
            }()
            DispatchQueue.main.async { [weak self] in
                guard let self else { return }
                self.active -= 1
                if self.generation == run, PairSession.shared.connected, let image {
                    self.images[file.savePath] = image
                    self.order.append(file.savePath)
                    if self.order.count > 120 {
                        let oldest = self.order.removeFirst()
                        self.images.removeValue(forKey: oldest)
                        // An evicted tile may come into view again and needs
                        // a fresh request. Keep in-flight and failed entries
                        // deduplicated during this file browsing session.
                        self.requested.remove(oldest)
                    }
                }
                self.startNext()
            }
        }
        startNext()
    }
}
