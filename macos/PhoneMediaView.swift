import AppKit
import AVFoundation
import CoreMedia
import SwiftUI
import VideoToolbox

enum PhoneMediaMode: Equatable {
    case screen
    case camera
}

/// A live preview backed by the connected Go media socket. Starts and stops with
/// `connected`; camera access is still approved on the phone by the Go service.
struct PhoneMediaView: View {
    let connected: Bool
    let mode: PhoneMediaMode
    // Each viewer owns its media connection. A mirror window must not interrupt
    // a camera preview open in the main workspace (or vice versa).
    @StateObject private var model = PhoneMediaModel()

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if mode == .camera {
                HStack(spacing: 12) {
                    Text("CAMERA")
                        .font(.system(size: 10, weight: .semibold))
                        .tracking(1.2)
                        .foregroundStyle(.secondary)
                    Text("In-app preview")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                    Spacer()
                    Picker("Camera", selection: $model.facing) {
                        Text("Rear").tag("rear")
                        Text("Front").tag("front")
                    }
                    .labelsHidden()
                    .pickerStyle(.segmented)
                    .frame(width: 170)
                    .disabled(!connected)
                }
            }
            GeometryReader { geometry in
                H264Preview(layer: model.layer)
                    .frame(width: geometry.size.width, height: geometry.size.height)
                    .contentShape(Rectangle())
                    .gesture(DragGesture(minimumDistance: 0)
                        .onChanged { value in
                            if mode == .screen && connected {
                                model.pointer(at: value.location, in: geometry.size, ending: false)
                            }
                        }
                        .onEnded { value in
                            if mode == .screen && connected {
                                model.pointer(at: value.location, in: geometry.size, ending: true)
                            }
                        })
                    .overlay {
                        if model.status != "Live phone video" {
                            VStack(spacing: 12) {
                                Image(systemName: connected ? (mode == .screen ? "rectangle.on.rectangle" : "camera") : "iphone.slash")
                                    .font(.system(size: 29, weight: .light))
                                    .foregroundStyle(.white.opacity(0.75))
                                Text(model.status)
                                    .font(.system(size: 14, weight: .medium))
                                    .foregroundStyle(.white)
                                    .multilineTextAlignment(.center)
                                if connected {
                                    Text(mode == .screen ? "Approve screen sharing in Office Kit on your phone." :
                                         "Allow camera access in Office Kit on your phone.")
                                        .font(.system(size: 12))
                                        .foregroundStyle(.white.opacity(0.65))
                                        .multilineTextAlignment(.center)
                                }
                            }
                            .padding(24)
                            .frame(maxWidth: .infinity, maxHeight: .infinity)
                            .background(Color.black.opacity(0.87))
                            .allowsHitTesting(false)
                        }
                    }
            }
                .frame(minHeight: 245, maxHeight: mode == .screen ? .infinity : 360)
                .aspectRatio(mode == .camera ? 16.0 / 9.0 : nil, contentMode: .fit)
                .background(.black)
                .clipShape(RoundedRectangle(cornerRadius: 14))
            if mode == .screen {
                HStack(spacing: 8) {
                    Button { model.navigate(key: 4) } label: { Label("Back", systemImage: "arrow.uturn.backward") }
                    Button { model.navigate(key: 3) } label: { Label("Home", systemImage: "house") }
                    Button { model.navigate(key: 187) } label: { Label("Recents", systemImage: "square.on.square") }
                    Spacer(minLength: 8)
                    Text("QUALITY")
                        .font(.system(size: 10, weight: .medium))
                        .tracking(1)
                        .foregroundStyle(.secondary)
                    Picker("Quality", selection: Binding(get: { model.quality }, set: { model.setQuality($0) })) {
                        Text("Auto").tag("auto")
                        Text("480p").tag("480p")
                        Text("720p").tag("720p")
                        Text("1080p").tag("1080p")
                    }
                    .labelsHidden()
                    .frame(width: 116)
                }
                .controlSize(.small)
                .disabled(!connected || !model.inputReady)
                HStack(spacing: 8) {
                    TextField("Type a message to send to your phone", text: $model.textToSend)
                        .onSubmit { model.sendText() }
                    Button { model.sendText() } label: { Label("Send text", systemImage: "arrow.up") }
                        .controlSize(.small)
                        .disabled(model.textToSend.isEmpty || !connected || !model.inputReady)
                }
                .disabled(!connected || !model.inputReady)
            }
            if model.status == "Live phone video" {
                Label(mode == .screen ? "Live screen · Click or drag to control your phone" : "Live camera preview", systemImage: "circle.fill")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                Label("Phone approval may be required to start the preview.", systemImage: "lock.shield")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .onAppear { model.configure(connected: connected, mode: mode) }
        .onChange(of: connected) { value in model.configure(connected: value, mode: mode) }
        .onChange(of: mode) { value in model.configure(connected: connected, mode: value) }
        .onChange(of: model.facing) { _ in model.configure(connected: connected, mode: mode) }
        .onDisappear { model.stop() }
    }
}

private struct H264Preview: NSViewRepresentable {
    let layer: AVSampleBufferDisplayLayer
    func makeNSView(context: Context) -> NSView {
        let view = NSView()
        view.wantsLayer = true
        view.layer = layer
        layer.videoGravity = .resizeAspect
        return view
    }
    func updateNSView(_ view: NSView, context: Context) {
        view.layer = layer
        layer.videoGravity = .resizeAspect
    }
}

private final class PhoneMediaModel: ObservableObject {
    @Published var status = "Connect the phone to start."
    @Published var facing = "rear"
    @Published var textToSend = ""
    @Published private(set) var videoWidth = 720
    @Published private(set) var videoHeight = 1560
    @Published private(set) var inputReady = false
    @Published private(set) var quality = "720p"
    let layer = AVSampleBufferDisplayLayer()
    private var worker: DispatchWorkItem?
    private var socketFD: Int32 = -1
    private var generation = 0
    private var pointerDown = false
    private var lastPointerMove = Date.distantPast

    func configure(connected: Bool, mode: PhoneMediaMode) {
        stop()
        guard connected else { status = "Connect the phone to start."; return }
        generation += 1
        let run = generation
        status = mode == .screen ? "Requesting phone screen permission…" : "Requesting phone camera permission…"
        let chosenFacing = facing
        let item = DispatchWorkItem { [weak self] in self?.run(generation: run, mode: mode, facing: chosenFacing) }
        worker = item
        DispatchQueue.global(qos: .userInitiated).async(execute: item)
    }

    func stop() {
        generation += 1
        worker?.cancel()
        worker = nil
        if socketFD >= 0 { shutdown(socketFD, SHUT_RDWR); socketFD = -1 }
        inputReady = false
        pointerDown = false
        videoWidth = 720
        videoHeight = 1560
        layer.flushAndRemoveImage()
    }

    func setQuality(_ choice: String) {
        guard ["auto", "480p", "720p", "1080p"].contains(choice) else { return }
        quality = choice
        sendInput(["kind": "quality", "quality": choice])
    }

    func navigate(key: Int) {
        guard [3, 4, 187].contains(key) else { return }
        sendInput(["kind": "key", "action": "down", "keycode": key])
        sendInput(["kind": "key", "action": "up", "keycode": key])
    }

    func sendText() {
        let value = textToSend
        guard !value.isEmpty, !value.contains("\0"), value.utf8.count <= 4096 else { return }
        sendInput(["kind": "text", "text": value])
        textToSend = ""
    }

    func pointer(at position: CGPoint, in size: CGSize, ending: Bool) {
        guard inputReady, size.width > 0, size.height > 0 else { return }
        let width = videoWidth, height = videoHeight
        let videoRatio = CGFloat(width) / CGFloat(height)
        let drawnWidth = min(size.width, size.height * videoRatio)
        let drawnHeight = drawnWidth / videoRatio
        let originX = (size.width - drawnWidth) / 2
        let originY = (size.height - drawnHeight) / 2
        if !pointerDown && (position.x < originX || position.x > originX + drawnWidth ||
                            position.y < originY || position.y > originY + drawnHeight) { return }
        let x = max(0, min(width - 1, Int((position.x - originX) * CGFloat(width) / drawnWidth)))
        let y = max(0, min(height - 1, Int((position.y - originY) * CGFloat(height) / drawnHeight)))
        let action: String
        if ending { action = pointerDown ? "up" : "down" }
        else if !pointerDown { action = "down" }
        else {
            if Date().timeIntervalSince(lastPointerMove) < 0.035 { return }
            action = "move"
            lastPointerMove = Date()
        }
        sendInput(["kind": "pointer", "action": action, "buttons": 1,
                   "x": x, "y": y, "width": width, "height": height])
        if ending {
            if !pointerDown {
                sendInput(["kind": "pointer", "action": "up", "buttons": 1,
                           "x": x, "y": y, "width": width, "height": height])
            }
            pointerDown = false
        } else { pointerDown = true }
    }

    private func sendInput(_ value: [String: Any]) {
        guard inputReady, socketFD >= 0,
              let data = try? JSONSerialization.data(withJSONObject: value), data.count <= 8191 else { return }
        if !writeAll(socketFD, data + Data([10])) {
            status = "Phone input unavailable. Reconnect and retry."
        }
    }

    private func report(_ message: String, generation run: Int) {
        DispatchQueue.main.async { [weak self] in
            guard let self, self.generation == run else { return }
            if self.status != message { self.status = message }
        }
    }

    private func run(generation run: Int, mode: PhoneMediaMode, facing: String) {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { report("Phone media service unavailable.", generation: run); return }
        // A disconnected phone must never terminate the macOS app with SIGPIPE.
        var noSigPipe: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        var writeTimeout = timeval(tv_sec: 0, tv_usec: 200_000)
        _ = setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &writeTimeout, socklen_t(MemoryLayout<timeval>.size))
        defer {
            DispatchQueue.main.async { [weak self] in
                if self?.socketFD == fd { self?.socketFD = -1 }
                close(fd)
            }
        }
        let runtime = ProcessInfo.processInfo.environment["XDG_RUNTIME_DIR"]
        let runtimeSocket = runtime.map { "\($0)/vivolinux/media.sock" }
        let path = if let runtimeSocket, runtimeSocket.utf8.count < 100 {
            runtimeSocket
        } else {
            "/tmp/vivolinux-\(getuid())/media.sock"
        }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(path.utf8) + [0]
        guard pathBytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
            report("Phone media socket path is invalid.", generation: run); return
        }
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: pathBytes) }
        let connected = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
        guard connected else { report("Phone media service unavailable. Reconnect and retry.", generation: run); return }
        DispatchQueue.main.async { [weak self] in
            guard let self, self.generation == run else { shutdown(fd, SHUT_RDWR); return }
            self.socketFD = fd
        }
        let request: [String: Any] = mode == .screen
            ? ["source": "screen"]
            : ["source": "camera", "facing": facing, "quality": "720p"]
        guard let requestData = try? JSONSerialization.data(withJSONObject: request),
              writeAll(fd, Data(requestData) + Data([10])) else {
            report("Could not start phone video.", generation: run); return
        }
        var decoder = AnnexBVideoDecoder(layer: layer)
        var buffer = Data()
        var videoStarted = false
        while true {
            var chunk = [UInt8](repeating: 0, count: 64 * 1024)
            let count = recv(fd, &chunk, chunk.count, 0)
            if count <= 0 { break }
            buffer.append(contentsOf: chunk.prefix(count))
            while buffer.count >= 5 {
                let kind = buffer[0]
                let size = Int(buffer[1]) << 24 | Int(buffer[2]) << 16 | Int(buffer[3]) << 8 | Int(buffer[4])
                guard size <= 16 * 1024 * 1024 else { report("Invalid phone video packet.", generation: run); return }
                guard buffer.count >= size + 5 else { break }
                let payload = Data(buffer[5..<(size + 5)])
                buffer.removeSubrange(0..<(size + 5))
                if kind == 2 {
                    if decoder.append(payload) {
                        videoStarted = true
                        report("Live phone video", generation: run)
                    }
                } else if kind == 1 || kind == 3 {
                    if let object = try? JSONSerialization.jsonObject(with: payload) as? [String: Any] {
                        if mode == .screen, kind == 1,
                           let width = object["video_width"] as? Int,
                           let height = object["video_height"] as? Int,
                           width > 0, height > 0, width <= 16384, height <= 16384 {
                            DispatchQueue.main.async { [weak self] in
                                guard let self, self.generation == run else { return }
                                self.videoWidth = width
                                self.videoHeight = height
                                self.inputReady = true
                                self.setQuality(self.quality)
                            }
                        }
                        if let error = object["error"] as? String { report(error, generation: run) }
                        else if object["stage"] as? String == "permission_wait" { report("Approve access on the phone.", generation: run) }
                        else if object["stage"] as? String == "starting" { report("Starting phone video…", generation: run) }
                    }
                }
            }
        }
        DispatchQueue.main.async { [weak self] in
            guard let self, self.generation == run else { return }
            self.inputReady = false
            self.status = videoStarted ? "Phone video ended. Reopen the preview to retry."
                                       : "Phone video stopped. Check permission and reconnect."
        }
    }
}

private func writeAll(_ fd: Int32, _ data: Data) -> Bool {
    data.withUnsafeBytes { bytes in
        guard let base = bytes.baseAddress else { return false }
        var offset = 0
        while offset < bytes.count {
            let written = Darwin.send(fd, base.advanced(by: offset), bytes.count - offset, 0)
            if written <= 0 { return false }
            offset += written
        }
        return true
    }
}

/// Converts each Annex-B access unit into length-prefixed samples for VideoToolbox.
private struct AnnexBVideoDecoder {
    let layer: AVSampleBufferDisplayLayer
    private var sps: Data?
    private var pps: Data?
    private var format: CMVideoFormatDescription?
    private var time = CMTime(value: 0, timescale: 30)

    mutating func append(_ bytes: Data) -> Bool {
        let units = nals(bytes)
        var parametersChanged = false
        for unit in units {
            guard let first = unit.first else { continue }
            switch first & 0x1f {
            case 7:
                if sps != unit { sps = unit; parametersChanged = true }
            case 8:
                if pps != unit { pps = unit; parametersChanged = true }
            default: break
            }
        }
        if (format == nil || parametersChanged), let sps, let pps {
            var description: CMFormatDescription?
            let status = sps.withUnsafeBytes { spsBytes in
                pps.withUnsafeBytes { ppsBytes in
                    var pointers: [UnsafePointer<UInt8>] = [spsBytes.bindMemory(to: UInt8.self).baseAddress!, ppsBytes.bindMemory(to: UInt8.self).baseAddress!]
                    var sizes = [sps.count, pps.count]
                    return pointers.withUnsafeMutableBufferPointer { p in
                        sizes.withUnsafeMutableBufferPointer { s in
                            CMVideoFormatDescriptionCreateFromH264ParameterSets(allocator: kCFAllocatorDefault, parameterSetCount: 2, parameterSetPointers: p.baseAddress!, parameterSetSizes: s.baseAddress!, nalUnitHeaderLength: 4, formatDescriptionOut: &description)
                        }
                    }
                }
            }
            if status == noErr {
                if format != nil { layer.flush() }
                format = description
            }
        }
        guard let format else { return false }
        let pictures = units.filter { unit in guard let type = unit.first.map({ $0 & 0x1f }) else { return false }; return type == 1 || type == 5 }
        guard !pictures.isEmpty else { return false }
        var sample = Data()
        for unit in units where unit.first.map({ ($0 & 0x1f) != 7 && ($0 & 0x1f) != 8 }) == true {
            var size = UInt32(unit.count).bigEndian
            withUnsafeBytes(of: &size) { sample.append(contentsOf: $0) }
            sample.append(unit)
        }
        var block: CMBlockBuffer?
        let blockStatus = sample.withUnsafeBytes { raw in
            CMBlockBufferCreateWithMemoryBlock(allocator: kCFAllocatorDefault, memoryBlock: nil, blockLength: sample.count, blockAllocator: kCFAllocatorDefault, customBlockSource: nil, offsetToData: 0, dataLength: sample.count, flags: 0, blockBufferOut: &block) == kCMBlockBufferNoErr && CMBlockBufferReplaceDataBytes(with: raw.baseAddress!, blockBuffer: block!, offsetIntoDestination: 0, dataLength: sample.count) == kCMBlockBufferNoErr
        }
        guard blockStatus, let block else { return false }
        var timing = CMSampleTimingInfo(duration: CMTime(value: 1, timescale: 30), presentationTimeStamp: time, decodeTimeStamp: .invalid)
        var size = sample.count
        var buffer: CMSampleBuffer?
        let result = CMSampleBufferCreateReady(allocator: kCFAllocatorDefault, dataBuffer: block, formatDescription: format, sampleCount: 1, sampleTimingEntryCount: 1, sampleTimingArray: &timing, sampleSizeEntryCount: 1, sampleSizeArray: &size, sampleBufferOut: &buffer)
        guard result == noErr, let buffer else { return false }
        layer.enqueue(buffer)
        time = CMTimeAdd(time, timing.duration)
        return true
    }

    private func nals(_ data: Data) -> [Data] {
        let bytes = [UInt8](data)
        func start(_ i: Int) -> Int {
            if i + 3 < bytes.count && bytes[i...i+3].elementsEqual([0, 0, 0, 1]) { return 4 }
            if i + 2 < bytes.count && bytes[i...i+2].elementsEqual([0, 0, 1]) { return 3 }
            return 0
        }
        var offsets: [(Int, Int)] = []
        var i = 0
        while i < bytes.count {
            let length = start(i)
            if length > 0 { offsets.append((i, length)); i += length } else { i += 1 }
        }
        guard !offsets.isEmpty else { return [] }
        return offsets.indices.compactMap { index in
            let begin = offsets[index].0 + offsets[index].1
            let end = index + 1 < offsets.count ? offsets[index + 1].0 : bytes.count
            return begin < end ? Data(bytes[begin..<end]) : nil
        }
    }
}
