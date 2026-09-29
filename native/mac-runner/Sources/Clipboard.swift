import AppKit
import Foundation

// How the runner reaches the guest: an OpenSSH argv from Go ending with
// user@host, pinned to this instance's host key.
struct GuestEndpoint {
    let ssh: [String]
    let knownHosts: String
}

// ClipboardSync shares plain text with the guest over SSH, following window
// focus instead of polling. The guest needs no agent: it runs pbcopy and
// pbpaste in its logged-in desktop session. Concealed items, such as those
// from password managers, never leave the host.
@MainActor
final class ClipboardSync {
    static let limit = 1 << 20
    static let concealed = NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType")
    static let transient = NSPasteboard.PasteboardType("org.nspasteboard.TransientType")

    let guest: GuestEndpoint
    var enabled: Bool
    // The desktop window; a guest copy never lands while it has focus again.
    weak var window: NSWindow?
    private var hostChange = NSPasteboard.general.changeCount
    private var pushedOnce = false
    private var lastText: String?
    private var busy = false

    init(guest: GuestEndpoint, enabled: Bool) {
        self.guest = guest
        self.enabled = enabled
    }

    // The guest is reachable only once first boot pinned its host key.
    private var ready: Bool { FileManager.default.fileExists(atPath: guest.knownHosts) }

    func windowBecameKey() {
        let pasteboard = NSPasteboard.general
        let change = pasteboard.changeCount
        guard enabled, ready, !busy, !pushedOnce || change != hostChange else { return }
        let types = pasteboard.types ?? []
        guard !types.contains(Self.concealed), !types.contains(Self.transient),
              let text = pasteboard.string(forType: .string), text != lastText,
              let data = text.data(using: .utf8), data.count <= Self.limit else {
            // Deliberately not shared: wait for the next host copy.
            hostChange = change
            pushedOnce = true
            return
        }
        busy = true
        execute(command: "LANG=en_US.UTF-8 /usr/bin/pbcopy", input: data, capture: false) { [weak self] status, _ in
            guard let self else { return }
            self.busy = false
            // A failed push, such as before sshd is up, is retried the next
            // time the window gains focus.
            guard status == 0 else { return }
            self.lastText = text
            self.hostChange = change
            self.pushedOnce = true
        }
    }

    func windowResignedKey() {
        guard enabled, ready, !busy else { return }
        busy = true
        let leftAt = NSPasteboard.general.changeCount
        let limit = Self.limit + 1
        execute(command: "LANG=en_US.UTF-8 /usr/bin/pbpaste | /usr/bin/head -c \(limit)", input: nil, capture: true) { [weak self] status, output in
            guard let self else { return }
            self.busy = false
            guard status == 0, output.count <= Self.limit, !output.isEmpty,
                  let text = String(data: output, encoding: .utf8), text != self.lastText else { return }
            let pasteboard = NSPasteboard.general
            // A host copy made meanwhile, or focus back in the guest, wins.
            guard pasteboard.changeCount == leftAt, self.window?.isKeyWindow != true else { return }
            pasteboard.clearContents()
            pasteboard.setString(text, forType: .string)
            self.lastText = text
            self.hostChange = pasteboard.changeCount
        }
    }

    // run executes one fixed guest command, such as a restart request, and
    // reports ssh's exit status, or nil when it did not exit normally. A guest
    // that stops or restarts can close the connection first: ssh then exits
    // 255 although the request was carried out.
    func run(command: String, completion: @escaping (Int32?) -> Void) {
        execute(command: command, input: nil, capture: false) { status, _ in completion(status) }
    }

    // run awaits one guest command's exit status.
    func run(command: String) async -> Int32? {
        await withCheckedContinuation { continuation in
            run(command: command) { status in continuation.resume(returning: status) }
        }
    }

    private func execute(command: String, input: Data?, capture: Bool, completion: @escaping (Int32?, Data) -> Void) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: guest.ssh[0])
        process.arguments = Array(guest.ssh.dropFirst()) + [command]
        let stdin = Pipe(), stdout = Pipe()
        process.standardInput = input == nil ? FileHandle.nullDevice : stdin
        process.standardOutput = capture ? stdout : FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        // One reader drains stdout to EOF; completion waits for it and for the
        // process, so no chunk is lost or reordered.
        let collected = OutputBuffer()
        let outputDone = DispatchGroup()
        if capture { outputDone.enter() }
        process.terminationHandler = { finished in
            let status: Int32? = finished.terminationReason == .exit ? finished.terminationStatus : nil
            outputDone.notify(queue: .main) { completion(status, collected.data) }
        }
        do { try process.run() } catch {
            if capture { outputDone.leave() }
            completion(nil, Data())
            return
        }
        if capture {
            DispatchQueue.global(qos: .utility).async {
                collected.append(stdout.fileHandleForReading.readDataToEndOfFile())
                outputDone.leave()
            }
        }
        if let input {
            DispatchQueue.global(qos: .utility).async {
                try? stdin.fileHandleForWriting.write(contentsOf: input)
                try? stdin.fileHandleForWriting.close()
            }
        }
        // A guest that stops answering must not leave the menu or focus waiting.
        DispatchQueue.main.asyncAfter(deadline: .now() + 8) {
            if process.isRunning { process.terminate() }
        }
    }
}

final class OutputBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var buffer = Data()
    func append(_ chunk: Data) { lock.lock(); buffer.append(chunk); lock.unlock() }
    var data: Data { lock.lock(); defer { lock.unlock() }; return buffer }
}
