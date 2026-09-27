import Foundation
import AppKit
import Virtualization
import DiskImageKit
import Darwin

@MainActor
final class Runtime: NSObject, @preconcurrency VZVirtualMachineDelegate, NSWindowDelegate, NSApplicationDelegate {
    let instance: String
    let socketPath: String
    let slot: URL
    var vm: VZVirtualMachine!
    var window: NSWindow?
    var serverFD: Int32 = -1
    var lockFD: Int32 = -1
    var networkControlFD: Int32 = -1
    var signals: [DispatchSourceSignal] = []
    var requestedShutdown = false
    var stopping = false
    var presentingExitPrompt = false
    var shutdownProcess: Process?

    init(args: Arguments) throws {
        instance = try args.string("--instance")
        guard UUID(uuidString: instance) != nil else { throw RunnerError("argument", "Instance must be a UUID") }
        socketPath = try args.string("--socket")
        slot = try args.path("--slot")
        super.init()
    }

    func start(_ args: Arguments) async throws {
        try args.validate(values: ["--base", "--slot", "--socket", "--instance", "--mac", "--network-socket", "--network-slot", "--cpu", "--memory"], flags: ["--nat"])
        let base = try args.path("--base")
        _ = try baseMetadata(base)
        lockFD = fm_lock(slot.appendingPathComponent("runner.lock").path)
        guard lockFD >= 0 else { throw RunnerError("locked", "Slot is already running or being modified") }
        let hardwareData = try Data(contentsOf: base.appendingPathComponent("hardware-model.bin"))
        guard let hardware = VZMacHardwareModel(dataRepresentation: hardwareData), hardware.isSupported else {
            throw RunnerError("unsupported_hardware", "Base hardware model is unsupported on this host")
        }
        let machineData = try Data(contentsOf: slot.appendingPathComponent("machine-id.bin"))
        guard let machine = VZMacMachineIdentifier(dataRepresentation: machineData) else { throw RunnerError("invalid_identity", "Invalid machine identifier") }
        let disk = try DiskImage(opening: .open(url: base.appendingPathComponent("disk.asif"), mode: .readOnly))
        let overlay = try DiskImage(opening: .open(url: slot.appendingPathComponent("disk.asif"), mode: .readWrite))
        let stack = try disk.appending(overlay)
        let attachment = try VZDiskImageStorageDeviceAttachment(diskImage: stack)
        let auxiliary = VZMacAuxiliaryStorage(url: slot.appendingPathComponent("auxiliary-storage.bin"))
        let network: VZNetworkDeviceAttachment
        if args.flags.contains("--nat") {
            guard args.values["--network-socket"] == nil else { throw RunnerError("argument", "--nat and --network-socket are mutually exclusive") }
            network = VZNATNetworkDeviceAttachment()
            progress("diagnostic_nat", ["fixed_ip": false])
        } else {
            let path = try args.string("--network-socket")
            let slotNumber = try args.integer("--network-slot", default: 0)
            var error = [CChar](repeating: 0, count: 1024)
            let fd = farrow_mac_network_connect(path, Int32(slotNumber), &networkControlFD, &error, error.count)
            guard fd >= 0 else { throw RunnerError("network_helper", String(cString: error)) }
            let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
            let networkAttachment = VZFileHandleNetworkDeviceAttachment(fileHandle: handle)
            networkAttachment.maximumTransmissionUnit = 1500
            network = networkAttachment
        }
        let config = try configuration(hardware: hardware, machine: machine, auxiliary: auxiliary, disk: attachment,
                                       cpu: args.integer("--cpu", default: 4), memory: UInt64(args.integer("--memory", default: 8 * 1024 * 1024 * 1024)),
                                       network: network, mac: args.string("--mac"))
        vm = VZVirtualMachine(configuration: config)
        vm.delegate = self
        let input = try readLineJSON(fd: STDIN_FILENO)
        let options = VZMacOSVirtualMachineStartOptions()
        if input["provision"] as? Bool == true {
            guard let username = input["username"] as? String, !username.isEmpty,
                  let password = input["password"] as? String, !password.isEmpty else {
                throw RunnerError("provisioning_input", "Initial provisioning requires username and password on stdin")
            }
            let provision = VZMacGuestProvisioningOptions()
            provision.username = username
            provision.fullName = input["full_name"] as? String ?? username
            provision.password = password
            provision.enablesRemoteLogin = true
            provision.logsInAutomatically = true
            try options.setGuestProvisioning(provision)
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
                    if errno == EINTR || errno == EAGAIN { continue }
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
            guard vm.state == .running else { throw RunnerError("not_running", "VM is not running") }
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
            installRuntimeMenu(application: NSApplication.shared, backgroundTarget: self,
                               backgroundAction: #selector(keepRunningInBackground(_:)))
        }
        if window == nil {
            let view = VZVirtualMachineView(frame: NSRect(x: 0, y: 0, width: 1152, height: 720))
            view.virtualMachine = vm
            view.capturesSystemKeys = true
            view.automaticallyReconfiguresDisplay = true
            let created = NSWindow(contentRect: view.frame, styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
            created.title = "Farrow — \(slot.lastPathComponent)"
            created.contentView = view
            created.isReleasedWhenClosed = false
            created.delegate = self
            created.center()
            window = created
        }
        NSApplication.shared.setActivationPolicy(.regular)
        window?.makeKeyAndOrderFront(nil)
        NSApplication.shared.activate(ignoringOtherApps: true)
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        keepRunningInBackground(sender)
        return false
    }

    @objc func keepRunningInBackground(_ sender: Any?) {
        window?.orderOut(nil)
        NSApplication.shared.setActivationPolicy(.accessory)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func requestDesktopShutdown() async throws {
        guard shutdownProcess == nil else { return }
        let parent = Bundle.main.bundleURL.deletingLastPathComponent()
        let candidates = [parent.appendingPathComponent("farrow"),
                          parent.deletingLastPathComponent().appendingPathComponent("bin/farrow")]
        guard Bundle.main.bundleURL.pathExtension == "app",
              let cli = candidates.first(where: { FileManager.default.isExecutableFile(atPath: $0.path) }) else {
            try await stop(force: false)
            return
        }
        let process = Process()
        process.executableURL = cli
        process.arguments = ["mac", "stop", slot.lastPathComponent]
        var environment = ProcessInfo.processInfo.environment
        environment["FARROW_HOME"] = slot.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().path
        process.environment = environment
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = FileHandle.standardError
        process.standardError = FileHandle.standardError
        process.terminationHandler = { [weak self] completed in
            Task { @MainActor in
                guard let self else { return }
                self.shutdownProcess = nil
                if completed.terminationStatus != 0, self.vm.state != .stopped {
                    self.showShutdownError("The normal shutdown did not complete. Check farrow mac logs \(self.slot.lastPathComponent) for details.")
                }
            }
        }
        try process.run()
        shutdownProcess = process
        progress("desktop_shutdown_requested", ["instance": instance, "method": "farrow mac stop"])
    }

    func showShutdownError(_ message: String) {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "Could not complete shutdown"
        alert.informativeText = "The virtual machine is still running. Shut down from its Apple menu, or run farrow mac stop \(slot.lastPathComponent).\n\n\(message)"
        alert.runModal()
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if vm?.state == .stopped { return .terminateNow }
        guard !presentingExitPrompt else { return .terminateCancel }
        presentingExitPrompt = true
        let choice = RuntimeExitChoice(response: runtimeExitPrompt(slot: slot.lastPathComponent).runModal())
        presentingExitPrompt = false
        switch choice {
        case .background:
            keepRunningInBackground(sender)
        case .shutdown:
            // Use the same pinned-SSH shutdown and stop verification as the CLI.
            // Keep the window and RPC loop alive until the guest actually stops.
            showWindow()
            Task { @MainActor in
                do { try await self.requestDesktopShutdown() }
                catch {
                    self.showShutdownError(error.localizedDescription)
                }
            }
        case .cancel: break
        }
        return .terminateCancel
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
                    do { try await self.stop(force: false) }
                    catch { progress("shutdown_failed", errorObject(error)) }
                }
            }
            source.resume()
            signals.append(source)
        }
    }

    func cleanup() {
        if serverFD >= 0 { close(serverFD); serverFD = -1; unlink(socketPath) }
        if networkControlFD >= 0 { close(networkControlFD); networkControlFD = -1 }
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
