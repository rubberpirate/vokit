import AppKit
import Foundation
@preconcurrency import UserNotifications

// Private newline-delimited JSON helper used by internal/desktop/bridge.go.
// Clipboard contents travel only over the helper's private stdin/stdout pipe.
@MainActor
final class PhoneDesktopBridge: NSObject {
    private let pasteboard = NSPasteboard.general
    private let center = UNUserNotificationCenter.current()
    private var clipboardEnabled: Bool
    private var lastChangeCount: Int
    private var nextClipboardPoll: Timer?
    private let outputLock = NSLock()

    init(clipboardEnabled: Bool) {
        self.clipboardEnabled = clipboardEnabled
        lastChangeCount = NSPasteboard.general.changeCount
        super.init()
    }

    func run() {
        emit(["event": "ready"])
        nextClipboardPoll = Timer.scheduledTimer(withTimeInterval: 0.35, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.pollClipboard() }
        }
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            while let line = readLine() {
                guard let data = line.data(using: .utf8),
                      let request = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                    continue
                }
                Task { @MainActor in self?.handle(request) }
            }
            DispatchQueue.main.async { NSApplication.shared.terminate(nil) }
        }
        NSApplication.shared.run()
    }

    private func pollClipboard() {
        guard clipboardEnabled else { return }
        let count = pasteboard.changeCount
        guard count != lastChangeCount else { return }
        lastChangeCount = count
        guard let text = pasteboard.string(forType: .string), isValidClipboardText(text) else { return }
        emit(["event": "text", "text": text])
    }

    private func handle(_ request: [String: Any]) {
        let id = request["id"] as? String ?? ""
        switch request["op"] as? String {
        case "set_text":
            guard let text = request["text"] as? String, isValidClipboardText(text) else {
                acknowledge(id, event: "set", ok: false)
                return
            }
            clipboardEnabled = true
            pasteboard.clearContents()
            let succeeded = pasteboard.setString(text, forType: .string)
            lastChangeCount = pasteboard.changeCount
            acknowledge(id, event: "set", ok: succeeded)
        case "notify":
            guard let state = request["state"] as? String,
                  let (title, body) = statusNotification(state) else {
                acknowledge(id, ok: false)
                return
            }
            sendNotification(title: title, body: body, id: id)
        case "phone_notification":
            guard let title = request["title"] as? String,
                  let body = request["body"] as? String,
                  let app = request["app"] as? String,
                  title.utf8.count <= 1024, body.utf8.count <= 8192, app.utf8.count <= 256,
                  !title.contains("\0"), !body.contains("\0"), !app.contains("\0") else {
                acknowledge(id, ok: false)
                return
            }
            let safeTitle = title.isEmpty ? (app.isEmpty ? "Phone notification" : app) : title
            sendNotification(title: safeTitle, body: body, id: id)
        default:
            acknowledge(id, ok: false)
        }
    }

    private func sendNotification(title: String, body: String, id: String) {
        let notificationCenter = center
        center.requestAuthorization(options: [.alert, .sound]) { [weak self] granted, _ in
            guard let self else { return }
            guard granted else {
                Task { @MainActor [weak self] in self?.acknowledge(id, ok: false) }
                return
            }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = body
            let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
            notificationCenter.add(request) { [weak self] error in
                Task { @MainActor [weak self] in self?.acknowledge(id, ok: error == nil) }
            }
        }
    }

    private func statusNotification(_ state: String) -> (String, String)? {
        switch state {
        case "connected": return ("VOkit connected", "Phone connection is ready.")
        case "disconnected": return ("VOkit disconnected", "Phone connection ended.")
        case "connecting": return ("VOkit connecting", "Connecting to your phone.")
        case "files_received": return ("VOkit files received", "Files from your phone are ready.")
        default: return nil
        }
    }

    private func isValidClipboardText(_ text: String) -> Bool {
        !text.isEmpty && text.utf8.count <= 64 * 1024 && !text.contains("\0")
    }

    private func acknowledge(_ id: String, event: String = "notification", ok: Bool) {
        emit(["event": event, "id": id, "ok": ok])
    }

    private func emit(_ value: [String: Any]) {
        guard let data = try? JSONSerialization.data(withJSONObject: value, options: [.fragmentsAllowed]),
              let line = String(data: data, encoding: .utf8) else { return }
        outputLock.lock()
        defer { outputLock.unlock() }
        FileHandle.standardOutput.write(Data((line + "\n").utf8))
    }
}

@main
@MainActor
struct PhoneDesktopBridgeMain {
    static func main() {
        let notificationsOnly = CommandLine.arguments.contains("--notifications-only")
        let helper = PhoneDesktopBridge(clipboardEnabled: !notificationsOnly)
        helper.run()
    }
}
