import AppKit
import dnssd
import Foundation
import Network
import SwiftUI

enum PairPhase {
    case idle, preparing, scanning, connecting, approval, connected, stopping, failed
}

@MainActor
final class PairSession: NSObject, ObservableObject {
    static let shared = PairSession()

    private override init() {
        super.init()
        NotificationCenter.default.addObserver(self, selector: #selector(appWillTerminate),
                                               name: NSApplication.willTerminateNotification, object: nil)
    }

    @objc private func appWillTerminate(_ notification: Notification) {
        disconnect()
    }

    @Published private(set) var status = "Disconnected"
    @Published private(set) var qrImage: NSImage?
    @Published private(set) var connected = false
    @Published private(set) var running = false
    @Published private(set) var phase: PairPhase = .idle
    @Published private(set) var failureHint: String?
    @Published private(set) var recentTransfer = false

    private var process: Process?
    private var pairingDirectory: URL?
    private var disconnectRequested = false
    private var localNetworkBrowser: NWBrowser?
    private var localNetworkListener: NWListener?
    private var localNetworkTimeout: DispatchWorkItem?

    func connect(clipboard: Bool = false, notifications: Bool = false) {
        guard !running else { return }
        failureHint = nil
        recentTransfer = false
        disconnectRequested = false
        phase = .preparing
        status = "Checking Local Network access…"
        running = true

        // A ready browser only means discovery started; it does not prove that
        // Local Network access was granted. Publish a temporary service and
        // wait until the browser actually discovers it before showing the QR.
        let serviceName = "vokit-" + UUID().uuidString
        let serviceType = "_vokit-auth._tcp"
        let listener: NWListener
        do {
            listener = try NWListener(using: .tcp, on: .any)
        } catch {
            failNetworkPreflight("Cannot start Local Network permission check. Check Wi-Fi and retry.")
            return
        }
        listener.service = NWListener.Service(name: serviceName, type: serviceType)
        listener.newConnectionHandler = { connection in connection.cancel() }
        localNetworkListener = listener
        listener.stateUpdateHandler = { [weak self, weak listener] state in
            DispatchQueue.main.async {
                guard let self, let listener, self.localNetworkListener === listener else { return }
                if case .failed = state {
                    self.failNetworkPreflight("Local Network service could not start. Check Wi-Fi and Local Network access, then retry.")
                }
            }
        }
        let browser = NWBrowser(for: .bonjour(type: serviceType, domain: nil), using: .tcp)
        localNetworkBrowser = browser
        browser.browseResultsChangedHandler = { [weak self, weak browser] results, _ in
            DispatchQueue.main.async {
                guard let self, let browser, self.localNetworkBrowser === browser else { return }
                guard results.contains(where: { result in
                    if case .service(let name, _, _, _) = result.endpoint { return name == serviceName }
                    return false
                }) else { return }
                self.cancelNetworkPreflight()
                self.startPairing(clipboard: clipboard, notifications: notifications)
            }
        }
        browser.stateUpdateHandler = { [weak self, weak browser] state in
            DispatchQueue.main.async {
                guard let self, let browser, self.localNetworkBrowser === browser else { return }
                switch state {
                case .waiting(.dns(let code)) where code == kDNSServiceErr_PolicyDenied:
                    self.failNetworkPreflight("Local Network access was denied. Enable VOkit in System Settings → Privacy & Security → Local Network, then retry.")
                case .failed(let error):
                    self.failNetworkPreflight("Local Network access failed (\(error.localizedDescription)). Enable it in System Settings → Privacy & Security → Local Network.")
                default:
                    // Ready does not imply permission; the service must appear.
                    break
                }
            }
        }
        let timeout = DispatchWorkItem { [weak self, weak browser] in
            guard let self, let browser, self.localNetworkBrowser === browser else { return }
            self.failNetworkPreflight("Local Network access was not confirmed. Allow VOkit in System Settings → Privacy & Security → Local Network, then retry.")
        }
        localNetworkTimeout = timeout
        DispatchQueue.main.asyncAfter(deadline: .now() + 25, execute: timeout)
        listener.start(queue: .global(qos: .userInitiated))
        browser.start(queue: .global(qos: .userInitiated))
    }

    private func cancelNetworkPreflight() {
        localNetworkTimeout?.cancel()
        localNetworkTimeout = nil
        localNetworkBrowser?.cancel()
        localNetworkBrowser = nil
        localNetworkListener?.cancel()
        localNetworkListener = nil
    }

    private func failNetworkPreflight(_ message: String) {
        cancelNetworkPreflight()
        running = false
        connected = false
        phase = .failed
        status = "Local Network access needed"
        failureHint = message
    }

    private func startPairing(clipboard: Bool, notifications: Bool) {
        guard running, !disconnectRequested else { return }

        let fileManager = FileManager.default
        guard let cli = Bundle.main.resourceURL?.appendingPathComponent("vivolinux"),
              fileManager.isExecutableFile(atPath: cli.path) else {
            status = "VOkit backend missing. Rebuild the app."
            failureHint = status
            phase = .failed
            running = false
            return
        }

        let home = fileManager.homeDirectoryForCurrentUser
        let stateDir = home.appendingPathComponent("Library/Application Support/VOkit/Pairing", isDirectory: true)
        let receiveDir = home.appendingPathComponent("Downloads/Vivo Office Kit", isDirectory: true)
        do {
            try fileManager.createDirectory(at: stateDir, withIntermediateDirectories: true,
                                            attributes: [.posixPermissions: 0o700])
        } catch {
            status = "Cannot prepare private pairing data."
            failureHint = status
            phase = .failed
            running = false
            return
        }

        let task = Process()
        task.executableURL = cli
        // This button explicitly authorizes first-use trust of the scanned phone.
        task.arguments = ["pair", "--persistent", "--trust-scanned-phone", "--file-access",
                          "--state-dir", stateDir.path, "--receive-dir", receiveDir.path]
        if clipboard || notifications {
            guard let helper = Bundle.main.resourceURL?.appendingPathComponent("VOkitHelper.app/Contents/MacOS/VOkitHelper"),
                  fileManager.isExecutableFile(atPath: helper.path) else {
                status = "VOkit desktop helper missing. Rebuild the app."
                failureHint = status
                phase = .failed
                running = false
                return
            }
            task.arguments?.append(contentsOf: ["--desktop-helper", helper.path])
        }
        if clipboard { task.arguments?.append("--clipboard") }
        if notifications { task.arguments?.append("--notifications") }
        // Finder-launched GUI apps normally do not inherit Homebrew's PATH.
        var environment = ProcessInfo.processInfo.environment
        environment["PATH"] = ["/opt/homebrew/bin", "/usr/local/bin", environment["PATH"] ?? ""].joined(separator: ":")
        task.environment = environment
        let output = Pipe()
        let errors = Pipe()
        task.standardOutput = output
        task.standardError = errors
        task.standardInput = FileHandle.nullDevice
        task.terminationHandler = { [weak self] finished in
            DispatchQueue.main.async {
                guard let self, self.process === finished else { return }
                self.process = nil
                self.running = false
                self.connected = false
                self.qrImage = nil
                if self.disconnectRequested || finished.terminationStatus == 0 {
                    self.phase = .idle
                    self.status = "Disconnected"
                    self.failureHint = nil
                } else {
                    self.phase = .failed
                    self.status = "Pairing interrupted"
                    if self.failureHint == nil {
                        self.failureHint = "The phone could not complete pairing. Check Local Network access and try a fresh QR code."
                    }
                }
            }
        }

        do {
            try task.run()
        } catch {
            status = "Unable to start the VOkit backend."
            failureHint = status
            phase = .failed
            running = false
            return
        }
        process = task
        pairingDirectory = stateDir
        running = true
        connected = false
        qrImage = nil
        status = "Preparing secure QR pairing…"
        phase = .preparing
        consumeEvents(output)
        drain(errors) // Only map known errors to safe descriptions.
    }

    func disconnect() {
        if localNetworkBrowser != nil {
            disconnectRequested = true
            cancelNetworkPreflight()
            running = false
            phase = .idle
            status = "Disconnected"
            return
        }
        guard let task = process else { return }
        disconnectRequested = true
        task.terminate()
        phase = .stopping
        status = "Disconnecting…"
    }

    func openDownloads() {
        let folder = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Downloads/Vivo Office Kit", isDirectory: true)
        try? FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        NSWorkspace.shared.open(folder)
    }

    private func consumeEvents(_ pipe: Pipe) {
        DispatchQueue.global(qos: .utility).async { [weak self] in
            var pending = Data()
            while true {
                let chunk = pipe.fileHandleForReading.availableData
                if chunk.isEmpty { break }
                pending.append(chunk)
                // Protocol emits line-delimited, bounded JSON events. Stop on malformed output.
                if pending.count > 2 * 1024 * 1024 { break }
                while let newline = pending.firstIndex(of: 10) {
                    let line = Data(pending[..<newline])
                    pending.removeSubrange(...newline)
                    guard let json = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
                          let stage = json["stage"] as? String else { continue }
                    let path = json["qr_path"] as? String
                    let code = json["code"] as? String
                    DispatchQueue.main.async { [weak self] in
                        self?.handle(stage: stage, qrPath: path, code: code)
                    }
                }
            }
        }
    }

    private func drain(_ pipe: Pipe) {
        DispatchQueue.global(qos: .utility).async { [weak self] in
            var recent = Data()
            while true {
                let chunk = pipe.fileHandleForReading.availableData
                if chunk.isEmpty { break }
                recent.append(chunk)
                if recent.count > 64 * 1024 { recent.removeFirst(recent.count - 32 * 1024) }
            }
            // stderr may contain certificates and protocol identifiers. Never display raw text.
            let log = String(data: recent, encoding: .utf8) ?? ""
            let hint: String?
            if log.contains("install qrencode") {
                hint = "Install qrencode with Homebrew, then try again."
            } else if log.contains("no private IPv4 LAN address") {
                hint = "Connect your Mac to Wi-Fi before pairing."
            } else if log.contains("no reachable phone") {
                hint = "The phone was not reachable over the local network."
            } else if log.contains("context deadline exceeded") {
                hint = "Connection timed out. Approve the phone prompt and retry."
            } else if log.contains("connection refused") {
                hint = "The phone refused the connection. Check phone approval."
            } else {
                hint = nil
            }
            guard let hint else { return }
            DispatchQueue.main.async { [weak self] in
                guard let self, !self.disconnectRequested else { return }
                self.failureHint = hint
            }
        }
    }

    private func handle(stage: String, qrPath: String?, code: String?) {
        guard running, !disconnectRequested else { return }
        switch stage {
        case "qr_ready":
            guard let root = pairingDirectory, let qrPath else { return }
            let path = URL(fileURLWithPath: qrPath).standardizedFileURL
            guard path.path.hasPrefix(root.standardizedFileURL.path + "/"),
                  let attributes = try? FileManager.default.attributesOfItem(atPath: path.path),
                  let size = attributes[.size] as? NSNumber, size.intValue > 0, size.intValue < 1024 * 1024,
                  let image = NSImage(contentsOf: path) else { return }
            qrImage = image
            phase = .scanning
            status = "Scan with Office Kit on your vivo phone"
        case "scan_received":
            qrImage = nil
            phase = .connecting
            status = "Phone found. Establishing secure connection…"
        case "phone_negotiated":
            phase = .approval
            status = "Phone paired. Awaiting approval…"
        case "control_connected":
            connected = true
            phase = .connected
            qrImage = nil
            status = "Connected to phone"
        case "control_disconnected":
            connected = false
            phase = .failed
            failureHint = "Phone disconnected. Check both devices are on the same Wi-Fi."
            status = "Phone disconnected"
        case "incoming_share_completed":
            recentTransfer = true
            status = "File received in Downloads/Vivo Office Kit"
        case "scan_bootstrap_failed", "scan_result_failed", "phone_negotiation_failed", "phone_candidates_unavailable":
            switch code {
            case "timeout", "expired":
                failureHint = "Pairing timed out. Approve access on the phone and scan a new code."
            case "no_usable_lan_endpoint":
                failureHint = "The scanned phone did not provide a reachable Wi-Fi address."
            case "network_or_tls":
                failureHint = "The secure connection could not reach the phone. Check Wi-Fi and Local Network access."
            case "phone_not_listening":
                failureHint = "The phone received the scan but is not accepting Wi-Fi connections. Keep Office Kit open on the phone and scan a new code."
            case "certificate_trust":
                failureHint = "Phone identity verification failed. Start a fresh pairing attempt."
            case "phone_rejected_or_incompatible":
                failureHint = "The phone rejected this connection. Check Office Kit on the phone."
            default:
                failureHint = "Pairing could not finish. Check Wi-Fi, phone approval and try again."
            }
        default:
            break
        }
    }
}

private struct PairStep: View {
    let index: Int
    let title: String
    let description: String
    let progress: Int

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            ZStack {
                Circle()
                    .fill(progress > index ? Color.green.opacity(0.12) :
                          progress == index ? Color.accentColor.opacity(0.13) : Color.secondary.opacity(0.10))
                    .frame(width: 30, height: 30)
                if progress > index {
                    Image(systemName: "checkmark")
                        .font(.system(size: 12, weight: .bold))
                        .foregroundColor(.green)
                } else {
                    Text("\(index)")
                        .font(.system(size: 12, weight: .bold))
                        .foregroundColor(progress == index ? .accentColor : .secondary)
                }
            }
            VStack(alignment: .leading, spacing: 3) {
                Text(title)
                    .font(.system(size: 13, weight: progress == index ? .semibold : .medium))
                Text(description)
                    .font(.system(size: 11))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

private enum DesktopTab: String {
    case connection, files, camera, settings
}

@MainActor
private final class MacNavigationState: ObservableObject {
    static let shared = MacNavigationState()
    @Published var selectedTab: DesktopTab = .connection
    @Published var pairClipboard = UserDefaults.standard.bool(forKey: "pairClipboard") {
        didSet { UserDefaults.standard.set(pairClipboard, forKey: "pairClipboard") }
    }
    @Published var pairNotifications = UserDefaults.standard.bool(forKey: "pairNotifications") {
        didSet { UserDefaults.standard.set(pairNotifications, forKey: "pairNotifications") }
    }
}

struct ContentView: View {
    @Environment(\.openWindow) private var openWindow
    @ObservedObject private var session = PairSession.shared
    @ObservedObject private var navigation = MacNavigationState.shared
    private var selectedTab: DesktopTab { get { navigation.selectedTab } nonmutating set { navigation.selectedTab = newValue } }
    private var pairClipboard: Bool { get { navigation.pairClipboard } nonmutating set { navigation.pairClipboard = newValue } }
    private var pairNotifications: Bool { get { navigation.pairNotifications } nonmutating set { navigation.pairNotifications = newValue } }

    private var progress: Int {
        switch session.phase {
        case .idle, .preparing, .failed: return 1
        case .scanning: return 1
        case .connecting: return 2
        case .approval: return 3
        case .connected, .stopping: return 4
        }
    }

    private var heading: String {
        switch selectedTab {
        case .files: return "Phone files"
        case .camera: return "Phone camera"
        case .settings: return "Connection settings"
        case .connection: break
        }
        switch session.phase {
        case .idle: return "Connect your phone"
        case .preparing: return "Preparing pairing"
        case .scanning: return "Scan this QR code"
        case .connecting: return "Connecting to phone"
        case .approval: return "Approve on your phone"
        case .connected: return "Your phone is ready"
        case .stopping: return "Disconnecting"
        case .failed: return "Pairing interrupted"
        }
    }

    private var description: String {
        switch selectedTab {
        case .files: return "Browse, preview, download and upload files with phone approval."
        case .camera: return "Watch your phone camera after granting access on the phone."
        case .settings: return "Choose optional data sharing before starting a new connection."
        case .connection: break
        }
        switch session.phase {
        case .idle: return "Pair a compatible vivo phone securely over Wi-Fi."
        case .preparing: return "Checking Local Network privacy before showing the pairing code…"
        case .scanning: return "Open Office Kit on your phone and scan the code below."
        case .connecting: return "Code scanned. Locating your phone on the local network…"
        case .approval: return "Accept the connection request in Office Kit to continue."
        case .connected: return "Explore your files, mirror your screen, or use the phone camera."
        case .stopping: return "Ending the active connection…"
        case .failed: return "The last attempt ended before a connection was established."
        }
    }

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Rectangle()
                .fill(Color.primary.opacity(0.08))
                .frame(width: 1)
            VStack(alignment: .leading, spacing: 0) {
                HStack {
                    Text(selectedTab == .settings ? "PREFERENCES" :
                         selectedTab == .connection ? "PAIR YOUR PHONE" : "WORKSPACE")
                        .font(.system(size: 10, weight: .semibold))
                        .tracking(1.6)
                        .foregroundStyle(.secondary)
                    Spacer()
                    Menu {
                        Text(session.connected ? "Phone connected over local Wi-Fi" : session.running ? "Pairing in progress" : "No connected phone")
                        if session.connected {
                            Button("Open received files") { session.openDownloads() }
                        }
                        if session.running {
                            Divider()
                            Button("Disconnect phone") { session.disconnect() }
                        }
                    } label: {
                        HStack(spacing: 7) {
                            Circle()
                                .fill(session.connected ? Color.green : session.running ? Color.orange : Color.secondary)
                                .frame(width: 8, height: 8)
                            Text(session.connected ? "Phone connected" : session.running ? "Pairing…" : "Not connected")
                        }
                        .font(.system(size: 12, weight: .medium))
                    }
                    .menuStyle(.borderlessButton)
                    .fixedSize()
                    .accessibilityLabel("Phone connection status")
                }
                .padding(.bottom, 18)
                Text(heading)
                    .font(.system(size: 27, weight: .bold, design: .rounded))
                    .padding(.bottom, 7)
                Text(description)
                    .font(.system(size: 13))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Group {
                    switch selectedTab {
                    case .connection: connectionCard
                    case .files: PhoneFilesView(connected: session.connected)
                    case .camera: PhoneMediaView(connected: session.connected, mode: .camera)
                    case .settings: settingsCard
                    }
                }
                .padding(.top, 22)
                .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)

            }
            .padding(26)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(minWidth: 870, idealWidth: 950, minHeight: 600, idealHeight: 660)
        .background(Color(nsColor: .windowBackgroundColor))
        .onChange(of: session.connected) { isConnected in
            if isConnected { selectedTab = .files }
            else if selectedTab != .settings { selectedTab = .connection }
        }
        .onAppear { if session.connected && selectedTab == .connection { selectedTab = .files } }
    }

    private var sidebar: some View {
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 0) {
                    HStack(spacing: 10) {
                        Image(systemName: "iphone.radiowaves.left.and.right")
                            .font(.system(size: 19))
                            .foregroundStyle(.tint)
                            .frame(width: 40, height: 40)
                            .background(Color.accentColor.opacity(0.10), in: RoundedRectangle(cornerRadius: 11))
                        VStack(alignment: .leading, spacing: 2) {
                            Text("VOkit")
                                .font(.system(size: 19, weight: .bold, design: .rounded))
                            Text("for macOS")
                                .font(.system(size: 11))
                                .foregroundStyle(.secondary)
                        }
                    }
                    .padding(.bottom, 30)

                    Text("WORKSPACE")
                        .font(.system(size: 10, weight: .bold))
                        .tracking(1.2)
                        .foregroundStyle(.secondary)
                        .padding(.bottom, 12)
                    if !session.connected {
                        tabButton(.connection, title: "Connect phone", icon: "iphone.radiowaves.left.and.right")
                    }
                    tabButton(.files, title: "Phone files", icon: "folder")
                    Button {
                        openWindow(id: "phone-mirror")
                    } label: {
                        Label("Mirror screen", systemImage: "rectangle.on.rectangle")
                            .font(.system(size: 13, weight: .medium))
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.vertical, 9)
                            .padding(.horizontal, 12)
                    }
                    .buttonStyle(.plain)
                    .disabled(!session.connected)
                    .foregroundStyle(session.connected ? .primary : .tertiary)
                    .padding(.bottom, 4)
                    tabButton(.camera, title: "Phone camera", icon: "camera")
                    tabButton(.settings, title: "Settings", icon: "gearshape")

                    if !session.connected && selectedTab == .connection {
                        Divider().padding(.vertical, 20)
                        Text("PAIRING STEPS")
                            .font(.system(size: 10, weight: .bold))
                            .tracking(1.2)
                            .foregroundStyle(.secondary)
                            .padding(.bottom, 16)
                        PairStep(index: 1, title: "Open Office Kit", description: "Use your phone's Office Kit scanner.", progress: progress)
                            .padding(.bottom, 18)
                        PairStep(index: 2, title: "Scan and connect", description: "Keep both devices on the same Wi-Fi.", progress: progress)
                            .padding(.bottom, 18)
                        PairStep(index: 3, title: "Approve access", description: "Accept the prompt on your phone.", progress: progress)
                    }
                }
                .padding(19)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            Divider()
            Button { session.openDownloads() } label: {
                Label("Received files", systemImage: "square.and.arrow.down")
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .buttonStyle(.plain)
            .padding(19)
        }
        .frame(width: 236)
        .background(Color(nsColor: .controlBackgroundColor))
    }

    private func tabButton(_ tab: DesktopTab, title: String, icon: String) -> some View {
        Button { selectedTab = tab } label: {
            Label(title, systemImage: icon)
                .font(.system(size: 13, weight: selectedTab == tab ? .semibold : .medium))
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 9)
                .padding(.horizontal, 12)
                .background(selectedTab == tab ? Color.accentColor.opacity(0.12) : .clear,
                            in: RoundedRectangle(cornerRadius: 9))
        }
        .buttonStyle(.plain)
        .disabled(!session.connected && tab != .connection && tab != .settings)
        .foregroundStyle(!session.connected && tab != .connection && tab != .settings ? .tertiary : .primary)
        .padding(.bottom, 4)
    }

    private var settingsCard: some View {
        VStack(alignment: .leading, spacing: 19) {
            Label("Sharing permissions", systemImage: "hand.raised")
                .font(.system(size: 15, weight: .semibold))
            VStack(alignment: .leading, spacing: 5) {
                Toggle("Synchronize text clipboard", isOn: Binding(get: { pairClipboard }, set: { pairClipboard = $0 }))
                Text("Copies plain text between Mac and phone, up to 64 KiB per item.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            VStack(alignment: .leading, spacing: 5) {
                Toggle("Forward phone notifications", isOn: Binding(get: { pairNotifications }, set: { pairNotifications = $0 }))
                Text("Show phone notifications in macOS Notification Center after phone approval.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if session.running {
                Label("Changes take effect on your next connection.", systemImage: "info.circle")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Divider()

            Label("Local Network access", systemImage: "wifi")
                .font(.system(size: 15, weight: .semibold))
            Text("macOS requests access when you pair for the first time. Once allowed, it won't ask again. If pairing can't find your phone, check access in System Settings.")
                .font(.system(size: 12))
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Button {
                if let url = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork") {
                    NSWorkspace.shared.open(url)
                }
            } label: {
                Label("Open Local Network settings", systemImage: "arrow.up.right.square")
            }
            .controlSize(.small)
        }
        .padding(22)
        .frame(maxWidth: .infinity, alignment: .topLeading)
        .background(Color(nsColor: .controlBackgroundColor).opacity(0.65), in: RoundedRectangle(cornerRadius: 17))
        .overlay(RoundedRectangle(cornerRadius: 17).strokeBorder(Color.primary.opacity(0.07)))
    }

    private var connectionCard: some View {
        VStack(spacing: 17) {
            if let qr = session.qrImage {
                Image(nsImage: qr)
                    .interpolation(.none)
                    .resizable()
                    .scaledToFit()
                    .frame(width: 212, height: 212)
                    .padding(12)
                    .background(.white, in: RoundedRectangle(cornerRadius: 15))
                    .accessibilityLabel("Scan this QR code using vivo Office Kit")
                Label("Waiting for your phone to scan", systemImage: "qrcode.viewfinder")
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(.secondary)
            } else {
                ZStack {
                    RoundedRectangle(cornerRadius: 21)
                        .fill(session.connected ? Color.green.opacity(0.10) : Color.accentColor.opacity(0.07))
                        .frame(width: 90, height: 90)
                    if session.connected {
                        Image(systemName: "checkmark.circle.fill")
                            .foregroundColor(.green)
                            .font(.system(size: 41))
                    } else if session.running && session.phase != .stopping {
                        ProgressView()
                            .controlSize(.large)
                    } else {
                        Image(systemName: session.phase == .failed ? "wifi.exclamationmark" : "iphone.and.arrow.forward")
                            .font(.system(size: 37))
                            .foregroundStyle(.tint)
                    }
                }
                .padding(.top, 20)

                if session.connected {
                    Text(session.recentTransfer ? "File received successfully" : "Connected and ready")
                        .font(.system(size: 15, weight: .semibold))
                } else if session.phase == .failed {
                    Text(session.failureHint ?? "Check Local Network access, Wi-Fi and phone approval.")
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.horizontal, 20)
                    Text("If macOS just asked for Local Network access, allow it and scan a fresh code.")
                        .font(.system(size: 11))
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.horizontal, 26)
                } else if !session.running {
                    Text("Your phone is one scan away")
                        .font(.system(size: 15, weight: .semibold))
                    Text("No cable or account sign-in required")
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                } else {
                    Text("Keep VOkit open while your phone connects")
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                }
            }

            HStack(spacing: 10) {
                if session.running {
                    Button(session.phase == .stopping ? "Disconnecting…" : session.connected ? "Disconnect" : "Cancel connection") {
                        session.disconnect()
                    }
                    .disabled(session.phase == .stopping)
                } else {
                    Button {
                        session.connect(clipboard: pairClipboard, notifications: pairNotifications)
                    } label: {
                        Label(session.phase == .failed ? "Try pairing again" : "Start pairing",
                              systemImage: session.phase == .failed ? "arrow.clockwise" : "qrcode")
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.large)
                }
                if session.connected {
                    Button("Received files") { session.openDownloads() }
                }
            }
            .padding(.bottom, 12)
        }
        .frame(maxWidth: .infinity)
        .frame(minHeight: 326)
        .background(Color(nsColor: .controlBackgroundColor).opacity(0.65), in: RoundedRectangle(cornerRadius: 20))
        .overlay(RoundedRectangle(cornerRadius: 20).strokeBorder(Color.primary.opacity(0.075)))
    }
}

@main
struct VOkitMacApp: App {
    var body: some Scene {
        WindowGroup {
            ContentView()
        }
        .windowResizability(.contentSize)
        Window("Phone mirroring", id: "phone-mirror") {
            PhoneMirrorWindowView()
        }
        .defaultSize(width: 480, height: 760)
        MenuBarExtra("VOkit", systemImage: "iphone.radiowaves.left.and.right") {
            VOkitMenuView()
        }
    }
}

private struct PhoneMirrorWindowView: View {
    @ObservedObject private var session = PairSession.shared

    var body: some View {
        PhoneMediaView(connected: session.connected, mode: .screen)
            .padding(16)
            .frame(minWidth: 360, minHeight: 480)
    }
}

private struct VOkitMenuView: View {
    @ObservedObject private var session = PairSession.shared

    var body: some View {
        Text(session.connected ? "Phone connected" : session.running ? "Pairing…" : "Disconnected")
        Button("Show VOkit") {
            NSApp.activate(ignoringOtherApps: true)
            for window in NSApp.windows where window.canBecomeKey { window.makeKeyAndOrderFront(nil) }
        }
        Button("Open received files") { session.openDownloads() }
        if session.running {
            Button("Disconnect phone") { session.disconnect() }
        }
        Divider()
        Button("Quit VOkit") {
            session.disconnect()
            NSApp.terminate(nil)
        }
    }
}
