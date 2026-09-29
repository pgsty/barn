import Foundation
import AppKit
import Virtualization
import DiskImageKit
import Darwin

// One line of stdin from Go. It carries the first-boot password, which never
// appears in arguments, progress or files written by the runner.
struct RunInput {
    var name = "mac"
    var subtitle = ""
    var provision: (username: String, password: String, fullName: String)?
    var shares: [SharedFolder] = []
    var guest: GuestEndpoint?
    var clipboard = false
    var window = false

    init(_ object: [String: Any]) throws {
        if let name = object["name"] as? String, !name.isEmpty { self.name = name }
        subtitle = object["subtitle"] as? String ?? ""
        if let provision = object["provision"] as? [String: Any] {
            guard let username = provision["username"] as? String, !username.isEmpty,
                  let password = provision["password"] as? String, !password.isEmpty else {
                throw RunnerError("provisioning_input", "First boot requires a username and password on stdin")
            }
            self.provision = (username, password, provision["full_name"] as? String ?? username)
        }
        for item in object["shares"] as? [[String: Any]] ?? [] {
            guard let name = item["name"] as? String, !name.isEmpty, let path = item["path"] as? String, path.hasPrefix("/") else {
                throw RunnerError("argument", "Invalid shared folder")
            }
            shares.append(SharedFolder(name: name, path: path, readOnly: item["readonly"] as? Bool ?? false))
        }
        if let guest = object["guest"] as? [String: Any] {
            guard let ssh = guest["ssh"] as? [String], ssh.count >= 2, ssh[0] == "/usr/bin/ssh",
                  let knownHosts = guest["known_hosts"] as? String, knownHosts.hasPrefix("/") else {
                throw RunnerError("argument", "Invalid guest endpoint")
            }
            self.guest = GuestEndpoint(ssh: ssh, knownHosts: knownHosts)
        }
        clipboard = object["clipboard"] as? Bool ?? false
        window = object["window"] as? Bool ?? false
    }
}

@MainActor
final class Runtime: NSObject, @preconcurrency VZVirtualMachineDelegate, NSWindowDelegate, NSApplicationDelegate, NSMenuItemValidation {
    let instance: String
    let socketPath: String
    let slot: URL
    var input = try! RunInput([:])
    var vm: VZVirtualMachine!
    var window: NSWindow?
    var serverFD: Int32 = -1
    var lockFD: Int32 = -1
    var vmnetNetwork: vmnet_network_ref?
    var signals: [DispatchSourceSignal] = []
    var requestedShutdown = false
    var stopping = false
    var presentingExitPrompt = false
    var endingSession = false
    var hideAfterFullScreen = false
    var shutdownProcess: Process?
    var clipboard: ClipboardSync?

    var name: String { input.name }

    init(args: Arguments) throws {
        instance = try args.string("--instance")
        guard UUID(uuidString: instance) != nil else { throw RunnerError("argument", "Instance must be a UUID") }
        socketPath = try args.string("--socket")
        slot = try args.path("--slot")
        super.init()
    }

    func start(_ args: Arguments) async throws {
        try args.validate(values: ["--base", "--slot", "--socket", "--instance", "--mac", "--gateway", "--address", "--cpu", "--memory"], flags: ["--recovery"])
        let base = try args.path("--base")
        _ = try baseMetadata(base)
        input = try RunInput(readLineJSON(fd: STDIN_FILENO))
        lockFD = fm_lock(slot.appendingPathComponent("runner.lock").path)
        guard lockFD >= 0 else { throw RunnerError("locked", "This machine is already running or being changed") }
        let hardwareData = try Data(contentsOf: base.appendingPathComponent("hardware-model.bin"))
        guard let hardware = VZMacHardwareModel(dataRepresentation: hardwareData), hardware.isSupported else {
            throw RunnerError("unsupported_hardware", "The base hardware model is not supported on this Mac")
        }
        let machineData = try Data(contentsOf: slot.appendingPathComponent("machine-id.bin"))
        guard let machine = VZMacMachineIdentifier(dataRepresentation: machineData) else { throw RunnerError("invalid_identity", "Invalid machine identifier") }
        let disk = try DiskImage(opening: .open(url: base.appendingPathComponent("disk.asif"), mode: .readOnly))
        let overlay = try DiskImage(opening: .open(url: slot.appendingPathComponent("disk.asif"), mode: .readWrite))
        let attachment = try VZDiskImageStorageDeviceAttachment(diskImage: try disk.appending(overlay))
        let auxiliary = VZMacAuxiliaryStorage(url: slot.appendingPathComponent("auxiliary-storage.bin"))
        // The machine's private network exists only inside this process: the
        // host is the gateway and the guest MAC has one DHCP reservation.
        let gateway = try args.string("--gateway"), address = try args.string("--address"), mac = try args.string("--mac")
        var vmnetStatus: UInt32 = 0
        guard let created = fm_vmnet_network_create(gateway, address, mac, &vmnetStatus) else {
            throw RunnerError("vmnet_network", vmnetFailure(vmnetStatus))
        }
        vmnetNetwork = created
        progress("network", ["mode": "vmnet", "gateway": gateway, "address": address])
        let config = try configuration(hardware: hardware, machine: machine, auxiliary: auxiliary, disk: attachment,
                                       cpu: args.integer("--cpu", default: 4), memory: UInt64(args.integer("--memory", default: 8 * 1024 * 1024 * 1024)),
                                       network: VZVmnetNetworkDeviceAttachment(network: created), mac: mac,
                                       shares: input.shares, display: displaySize())
        vm = VZVirtualMachine(configuration: config)
        vm.delegate = self
        let options = VZMacOSVirtualMachineStartOptions()
        options.startUpFromMacOSRecovery = args.flags.contains("--recovery")
        if let provision = input.provision {
            let provisioning = VZMacGuestProvisioningOptions()
            provisioning.username = provision.username
            provisioning.fullName = provision.fullName
            provisioning.password = provision.password
            provisioning.enablesRemoteLogin = true
            provisioning.logsInAutomatically = true
            try options.setGuestProvisioning(provisioning)
        }
        if let guest = input.guest {
            clipboard = ClipboardSync(guest: guest, enabled: input.clipboard)
        }
        var error = [CChar](repeating: 0, count: 1024)
        serverFD = fm_unix_listen(socketPath, &error, error.count)
        guard serverFD >= 0 else { throw RunnerError("rpc_listen", String(cString: error)) }
        listen()
        handleSignals()
        NSApplication.shared.delegate = self
        progress("starting", ["instance": instance, "pid": getpid()])
        do { try await vm.start(options: options) }
        catch {
            cleanup()
            throw actionableVirtualizationError(error)
        }
        emit(status())
        progress("running", ["instance": instance, "pid": getpid()])
        if input.window || options.startUpFromMacOSRecovery { showWindow() }
    }

    // A window-sized display at the main screen's pixel density. The view
    // reconfigures the guest resolution whenever the window is resized.
    func displaySize() -> NSSize {
        guard let screen = NSScreen.main else { return NSSize(width: 1440, height: 900) }
        let visible = screen.visibleFrame.size
        return NSSize(width: min(1440, (visible.width * 0.85).rounded()), height: min(900, (visible.height * 0.85).rounded()))
    }

    func status() -> [String: Any] {
        var state = vm.map { vmStateName($0.state) } ?? "unknown"
        if stopping && state == "running" { state = "stopping" }
        return ["ok": true, "instance": instance, "pid": getpid(), "state": state, "window_visible": window?.isVisible ?? false]
    }

    func listen() {
        let server = serverFD
        DispatchQueue.global(qos: .utility).async { [weak self] in
            while true {
                let client = fm_unix_accept(server)
                if client < 0 {
                    if errno == EINTR || errno == EAGAIN || errno == EACCES { continue }
                    return
                }
                DispatchQueue.global(qos: .utility).async {
                    do {
                        let request = try readLineJSON(fd: client)
                        Task { @MainActor [weak self] in
                            defer { close(client) }
                            guard let self else { return }
                            do { try sendJSON(await self.handle(request), fd: client) }
                            catch { try? sendJSON(errorObject(error), fd: client) }
                        }
                    } catch {
                        try? sendJSON(errorObject(error), fd: client)
                        close(client)
                    }
                }
            }
        }
    }

    func handle(_ request: [String: Any]) async throws -> [String: Any] {
        guard request["instance"] as? String == instance else { throw RunnerError("identity_mismatch", "RPC instance identity does not match") }
        switch request["method"] as? String {
        case "status": return status()
        case "open":
            guard vm.state == .running else { throw RunnerError("not_running", "The VM is not running") }
            showWindow()
            return status()
        case "stop":
            try await stop(force: request["force"] as? Bool == true)
            return status()
        default: throw RunnerError("method", "Unknown RPC method")
        }
    }

    func stop(force: Bool) async throws {
        if vm.state == .stopped { finish(); return }
        if force {
            stopping = true
            try await vm.stop()
            finish()
        } else if !requestedShutdown {
            try vm.requestStop()
            stopping = true
            requestedShutdown = true
            progress("shutdown_requested", ["instance": instance])
        }
    }

    func showWindow() {
        if NSApplication.shared.mainMenu == nil {
            installRuntimeMenu(application: NSApplication.shared, runtime: self)
        }
        if window == nil {
            let size = displaySize()
            let view = VZVirtualMachineView(frame: NSRect(origin: .zero, size: size))
            view.virtualMachine = vm
            view.capturesSystemKeys = true
            view.automaticallyReconfiguresDisplay = true
            let created = NSWindow(contentRect: view.frame, styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
            created.title = name
            created.subtitle = input.subtitle
            created.contentView = view
            created.isReleasedWhenClosed = false
            created.delegate = self
            created.collectionBehavior.insert(.fullScreenPrimary)
            created.contentMinSize = NSSize(width: 640, height: 400)
            created.tabbingMode = .disallowed
            if !created.setFrameUsingName("BarnMac.\(name)") { created.center() }
            created.setFrameAutosaveName("BarnMac.\(name)")
            window = created
            clipboard?.window = created
        }
        NSApplication.shared.setActivationPolicy(.regular)
        window?.makeKeyAndOrderFront(nil)
        if let view = window?.contentView { window?.makeFirstResponder(view) }
        NSApplication.shared.activate(ignoringOtherApps: true)
    }

    // The clipboard follows focus: the host clipboard goes to the guest when
    // its window becomes key, and the guest clipboard comes back when it
    // resigns key. Nothing is read while the window is hidden.
    func windowDidBecomeKey(_ notification: Notification) { clipboard?.windowBecameKey() }
    func windowDidResignKey(_ notification: Notification) { clipboard?.windowResignedKey() }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        keepRunningInBackground(sender)
        return false
    }

    @objc func keepRunningInBackground(_ sender: Any?) {
        clipboard?.windowResignedKey()
        // Hiding a full-screen window would leave its empty Space behind.
        if let window, window.styleMask.contains(.fullScreen) {
            hideAfterFullScreen = true
            window.toggleFullScreen(nil)
            return
        }
        window?.orderOut(nil)
        NSApplication.shared.setActivationPolicy(.accessory)
    }

    func windowDidExitFullScreen(_ notification: Notification) {
        guard hideAfterFullScreen else { return }
        hideAfterFullScreen = false
        window?.orderOut(nil)
        NSApplication.shared.setActivationPolicy(.accessory)
    }

    @objc func toggleClipboard(_ sender: Any?) {
        clipboard?.enabled.toggle()
    }

    @objc func restartGuest(_ sender: Any?) {
        guard let clipboard, vm.state == .running else { return }
        let alert = NSAlert()
        alert.messageText = "Restart \(name)?"
        alert.informativeText = "macOS restarts immediately. Unsaved changes in the guest are lost."
        alert.addButton(withTitle: "Restart")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        Task { @MainActor in
            // Reach the guest first: once the restart starts, sshd can close
            // the connection before ssh reports success.
            guard await clipboard.run(command: "/usr/bin/true") == 0 else {
                self.showShutdownError("The restart request did not reach the guest over SSH.")
                return
            }
            let status = await clipboard.run(command: "sudo -n /sbin/shutdown -r now")
            if status != 0 && status != 255 {
                self.showShutdownError("The guest refused the restart request.")
            }
        }
    }

    @objc func shutDownGuest(_ sender: Any?) {
        let alert = NSAlert()
        alert.messageText = "Shut down \(name)?"
        alert.informativeText = "macOS shuts down now and the window closes when it stops. Unsaved changes in the guest are lost."
        alert.addButton(withTitle: "Shut Down")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        Task { @MainActor in
            do { try await self.requestDesktopShutdown() }
            catch { self.showShutdownError(error.localizedDescription) }
        }
    }

    func validateMenuItem(_ item: NSMenuItem) -> Bool {
        switch item.action {
        case #selector(toggleClipboard(_:)):
            item.state = clipboard?.enabled == true ? .on : .off
            return clipboard != nil
        case #selector(restartGuest(_:)):
            return clipboard != nil && vm?.state == .running
        case #selector(shutDownGuest(_:)):
            return vm?.state == .running && shutdownProcess == nil
        default:
            return true
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if vm?.state == .running { showWindow() }
        return true
    }

    // Shutdown goes through barn mac stop so the CLI keeps its record and
    // its graceful-then-forced policy.
    func requestDesktopShutdown() async throws {
        guard shutdownProcess == nil else { return }
        let parent = Bundle.main.bundleURL.deletingLastPathComponent()
        let candidates = [parent.appendingPathComponent("barn"),
                          parent.deletingLastPathComponent().appendingPathComponent("bin/barn"),
                          parent.deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("bin/barn")]
        guard Bundle.main.bundleURL.pathExtension == "app",
              let cli = candidates.first(where: { FileManager.default.isExecutableFile(atPath: $0.path) }) else {
            try await requestGuestShutdown()
            return
        }
        let process = Process()
        process.executableURL = cli
        process.arguments = ["mac", "stop", name]
        var environment = ProcessInfo.processInfo.environment
        environment["BARN_HOME"] = slot.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().path
        process.environment = environment
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = FileHandle.standardError
        process.standardError = FileHandle.standardError
        process.terminationHandler = { [weak self] completed in
            Task { @MainActor in
                guard let self else { return }
                self.shutdownProcess = nil
                if completed.terminationStatus != 0, self.vm.state != .stopped {
                    self.showShutdownError("The normal shutdown did not complete. Run barn mac logs \(self.name) for details.")
                }
            }
        }
        try process.run()
        shutdownProcess = process
        progress("desktop_shutdown_requested", ["instance": instance, "method": "barn mac stop"])
    }

    // requestGuestShutdown is the runner's own normal stop, for signals and a
    // desktop without its CLI: macOS shuts down over the pinned SSH connection,
    // as barn mac stop does, because Virtualization's request does not stop
    // an initialized guest. The VM is powered off after the same grace period.
    func requestGuestShutdown() async throws {
        if vm.state == .stopped { finish(); return }
        guard !requestedShutdown else { return }
        var method = "virtualization"
        if let clipboard, await clipboard.run(command: "/usr/bin/true") == 0 {
            let status = await clipboard.run(command: "sudo -n /sbin/shutdown -h now")
            if status == 0 || status == 255 { method = "ssh" }
        }
        if method != "ssh" { try vm.requestStop() }
        requestedShutdown = true
        stopping = true
        progress("shutdown_requested", ["instance": instance, "method": method])
        DispatchQueue.main.asyncAfter(deadline: .now() + 120) { [weak self] in
            Task { @MainActor in
                guard let self, self.vm.state != .stopped else { return }
                progress("shutdown_timeout", ["instance": self.instance])
                try? await self.stop(force: true)
            }
        }
    }

    func showShutdownError(_ message: String) {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "\(name) is still running"
        alert.informativeText = "Shut it down from its Apple menu, or run barn mac stop \(name).\n\n\(message)"
        alert.runModal()
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if vm?.state == .stopped { return .terminateNow }
        if sessionIsEnding() {
            // Logging out, restarting or shutting down the Mac never waits for
            // a choice and is never cancelled: the guest shuts down normally,
            // is powered off if it takes too long, and the Mac continues.
            if !endingSession {
                endingSession = true
                Task { @MainActor in await self.stopForSessionEnd() }
            }
            return .terminateLater
        }
        guard !presentingExitPrompt else { return .terminateCancel }
        presentingExitPrompt = true
        let choice = RuntimeExitChoice(response: runtimeExitPrompt(slot: name).runModal())
        presentingExitPrompt = false
        switch choice {
        case .background:
            keepRunningInBackground(sender)
        case .shutdown:
            // Keep the window and RPC loop alive until the guest actually stops.
            showWindow()
            Task { @MainActor in
                do { try await self.requestDesktopShutdown() }
                catch { self.showShutdownError(error.localizedDescription) }
            }
        case .cancel: break
        }
        return .terminateCancel
    }

    // sessionIsEnding reports whether the system asked the app to quit for a
    // logout, restart or shutdown, rather than the user choosing Quit.
    func sessionIsEnding() -> Bool {
        guard let event = NSAppleEventManager.shared().currentAppleEvent,
              let reason = event.attributeDescriptor(forKeyword: AEKeyword(kAEQuitReason))?.enumCodeValue else { return false }
        let ending = [kAELogOut, kAEReallyLogOut, kAEShowRestartDialog, kAERestart, kAEShowShutdownDialog, kAEShutDown]
        return ending.contains { OSType($0) == reason }
    }

    func stopForSessionEnd() async {
        do { try await requestDesktopShutdown() }
        catch { progress("shutdown_failed", errorObject(error)) }
        let deadline = Date().addingTimeInterval(60)
        while vm.state != .stopped && Date() < deadline {
            try? await Task.sleep(nanoseconds: 500_000_000)
        }
        if vm.state != .stopped {
            stopping = true
            try? await vm.stop()
        }
        NSApplication.shared.reply(toApplicationShouldTerminate: true)
    }

    func guestDidStop(_ virtualMachine: VZVirtualMachine) { progress("stopped", ["instance": instance]); finish() }
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        progress("vm_failed", errorObject(error)); finish(exitCode: 1)
    }

    func handleSignals() {
        signal(SIGPIPE, SIG_IGN)
        signal(SIGHUP, SIG_IGN)
        for number in [SIGINT, SIGTERM] {
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
            source.setEventHandler { [weak self] in
                Task { @MainActor in
                    guard let self else { return }
                    do { try await self.requestGuestShutdown() }
                    catch { progress("shutdown_failed", errorObject(error)) }
                }
            }
            source.resume()
            signals.append(source)
        }
    }

    func cleanup() {
        if serverFD >= 0 { close(serverFD); serverFD = -1; unlink(socketPath) }
        if lockFD >= 0 { close(lockFD); lockFD = -1 }
    }

    func finish(exitCode: Int32 = 0) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { self.cleanup(); exit(exitCode) }
    }
}

func rpc(_ args: Arguments) throws -> [String: Any] {
    try args.validate(values: ["--socket", "--instance", "--method"], flags: ["--force"])
    let path = try args.string("--socket")
    var error = [CChar](repeating: 0, count: 1024)
    let fd = fm_unix_connect(path, &error, error.count)
    guard fd >= 0 else { throw RunnerError("rpc_connect", String(cString: error)) }
    defer { close(fd) }
    let instance = try args.string("--instance")
    try sendJSON(["instance": instance, "method": args.string("--method"), "force": args.flags.contains("--force")], fd: fd)
    let response = try readLineJSON(fd: fd)
    if response["ok"] as? Bool == true, response["instance"] as? String != instance {
        throw RunnerError("identity_mismatch", "RPC response does not match expected instance")
    }
    return response
}

// vmnetFailure explains the vmnet statuses a user can act on.
func vmnetFailure(_ status: UInt32) -> String {
    switch status {
    case 1009: // VMNET_SHARING_SERVICE_BUSY
        return "Cannot create this machine's private network: the macOS network sharing service is busy (vmnet status 1009). Turn off Internet Sharing or quit the other tool that uses it, then start again"
    case 1010: // VMNET_NOT_AUTHORIZED
        return "macOS did not allow this machine's private network (vmnet status 1010). Check Barn Mac.app with barn mac doctor"
    default:
        return "Cannot create this machine's private network (vmnet status \(status))"
    }
}
