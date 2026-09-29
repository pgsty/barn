import Foundation
import AppKit
import Virtualization
import DiskImageKit
import Darwin

func actionableVirtualizationError(_ error: Error) -> Error {
    var current = error as NSError
    // Installer failures may wrap the Virtualization error one or more levels
    // down. Keep all other Apple diagnostics intact.
    for _ in 0..<8 {
        if current.domain == VZErrorDomain, current.code == VZError.Code.virtualMachineLimitExceeded.rawValue {
            return RunnerError("virtual_machine_limit", "macOS allows two macOS virtual machines at a time, shared with other tools and with macOS restore installation. Stop one before retrying; Barn has not stopped any other VM.")
        }
        guard let underlying = current.userInfo[NSUnderlyingErrorKey] as? NSError else { break }
        current = underlying
    }
    return error
}

func vmStateName(_ state: VZVirtualMachine.State) -> String {
    switch state {
    case .stopped: return "stopped"
    case .running: return "running"
    case .paused: return "paused"
    case .error: return "error"
    case .starting: return "starting"
    case .pausing: return "pausing"
    case .resuming: return "resuming"
    case .stopping: return "stopping"
    case .saving: return "saving"
    case .restoring: return "restoring"
    @unknown default: return "unknown"
    }
}

// This is called only after install() has completed successfully. Apple forbids
// stopping *during* installation, and does not promise that installation's
// completion callback leaves the restore VM stopped. Never start the guest here.
@MainActor
func finishRestoreMachine(_ vm: VZVirtualMachine) async throws -> [String: Any] {
    let completedState = vmStateName(vm.state)
    progress("installation_completed", ["state": completedState, "can_stop": vm.canStop, "can_request_stop": vm.canRequestStop])
    let settleDeadline = Date().addingTimeInterval(3)
    while vm.state != .stopped && vm.state != .error && Date() < settleDeadline {
        try await Task.sleep(nanoseconds: 100_000_000)
    }
    var usedStop = false
    if vm.state != .stopped {
        guard vm.canStop, vm.state == .running || vm.state == .paused else {
            throw RunnerError("restore_not_stopped", "Installer completed, but restore VM remains \(vmStateName(vm.state)) (canStop=\(vm.canStop)); base was not published")
        }
        usedStop = true
        progress("stopping_restore_machine", ["state": vmStateName(vm.state), "can_stop": vm.canStop])
        var finished = false
        var stopError: Error?
        let stopTask = Task { @MainActor in
            do { try await vm.stop() }
            catch { stopError = error }
            finished = true
        }
        let deadline = Date().addingTimeInterval(30)
        while !finished && Date() < deadline {
            try await Task.sleep(nanoseconds: 100_000_000)
        }
        guard finished else {
            stopTask.cancel()
            throw RunnerError("restore_stop_timeout", "Restore VM stop did not complete within 30 seconds; base was not published")
        }
        // Apple documents a race: a concurrent guest stop can make stop() fail
        // with invalid state. A confirmed stopped state still meets our contract.
        if let stopError, vm.state != .stopped { throw stopError }
    }
    guard vm.state == .stopped else {
        throw RunnerError("restore_not_stopped", "Restore VM state is \(vmStateName(vm.state)) after stop completion; base was not published")
    }
    progress("restore_machine_stopped", ["state": "stopped", "explicit_stop": usedStop])
    return ["installer_completion_state": completedState, "restore_explicit_stop": usedStop, "restore_final_state": "stopped"]
}

@MainActor
func hardwareMetadata(_ path: URL) throws -> [String: Any] {
    let bytes = try Data(contentsOf: path)
    guard let hardware = VZMacHardwareModel(dataRepresentation: bytes) else {
        throw RunnerError("invalid_hardware", "Cached hardware model is invalid")
    }
    return ["ok": true, "supported": hardware.isSupported, "hardware_model_sha256": digest(bytes)]
}

@MainActor
func restoreMetadata(_ image: VZMacOSRestoreImage) throws -> [String: Any] {
    guard image.operatingSystemVersion.majorVersion == 27 else { throw RunnerError("unsupported_guest", "Only macOS 27 restore images are supported") }
    guard image.isSupported, let requirements = image.mostFeaturefulSupportedConfiguration else {
        throw RunnerError("unsupported_image", "Restore image has no hardware model supported by this host")
    }
    let v = image.operatingSystemVersion
    let version = "\(v.majorVersion).\(v.minorVersion)" + (v.patchVersion == 0 ? "" : ".\(v.patchVersion)")
    return ["ok": true, "version": version, "build": image.buildVersion,
            "hardware_model_sha256": digest(requirements.hardwareModel.dataRepresentation),
            "minimum_cpu": requirements.minimumSupportedCPUCount,
            "minimum_memory": requirements.minimumSupportedMemorySize, "supported": true]
}

// SharedFolder is one host directory the guest sees under
// /Volumes/My Shared Files/<name>.
struct SharedFolder {
    let name: String
    let path: String
    let readOnly: Bool
}

@MainActor
func configuration(hardware: VZMacHardwareModel, machine: VZMacMachineIdentifier,
                   auxiliary: VZMacAuxiliaryStorage, disk: VZDiskImageStorageDeviceAttachment,
                   cpu: Int, memory: UInt64, network: VZNetworkDeviceAttachment?, mac: String?,
                   shares: [SharedFolder] = [], display: NSSize? = nil) throws -> VZVirtualMachineConfiguration {
    let config = VZVirtualMachineConfiguration()
    config.bootLoader = VZMacOSBootLoader()
    config.cpuCount = cpu
    config.memorySize = memory
    let platform = VZMacPlatformConfiguration()
    platform.hardwareModel = hardware
    platform.machineIdentifier = machine
    platform.auxiliaryStorage = auxiliary
    config.platform = platform
    config.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: disk)]
    let graphics = VZMacGraphicsDeviceConfiguration()
    if let display, let screen = NSScreen.main {
        graphics.displays = [VZMacGraphicsDisplayConfiguration(for: screen, sizeInPoints: display)]
    } else {
        graphics.displays = [VZMacGraphicsDisplayConfiguration(widthInPixels: 1920, heightInPixels: 1200, pixelsPerInch: 144)]
    }
    config.graphicsDevices = [graphics]
    // A macOS guest mounts this one device under /Volumes/My Shared Files.
    if !shares.isEmpty {
        var directories: [String: VZSharedDirectory] = [:]
        for share in shares {
            directories[share.name] = VZSharedDirectory(url: URL(fileURLWithPath: share.path, isDirectory: true), readOnly: share.readOnly)
        }
        let device = VZVirtioFileSystemDeviceConfiguration(tag: VZVirtioFileSystemDeviceConfiguration.macOSGuestAutomountTag)
        device.share = VZMultipleDirectoryShare(directories: directories)
        config.directorySharingDevices = [device]
    }
    config.keyboards = [VZMacKeyboardConfiguration()]
    config.pointingDevices = [VZMacTrackpadConfiguration()]
    config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    if let network {
        let nic = VZVirtioNetworkDeviceConfiguration()
        nic.attachment = network
        if let mac {
            guard let address = VZMACAddress(string: mac) else { throw RunnerError("invalid_mac", "Invalid network MAC address") }
            nic.macAddress = address
        }
        config.networkDevices = [nic]
    }
    try config.validate()
    return config
}

@MainActor
func restore(_ args: Arguments) async throws -> [String: Any] {
    try args.validate(values: ["--ipsw", "--base", "--cpu", "--memory", "--disk-size"])
    let ipsw = try args.path("--ipsw"), base = try args.path("--base")
    let fm = FileManager.default
    try fm.createDirectory(at: base, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    let existing = try fm.contentsOfDirectory(atPath: base.path)
    let allowed = Set(["metadata.json", "manifest.json", "restore.log", "restore.lock"])
    guard Set(existing).subtracting(allowed).isEmpty else { throw RunnerError("base_exists", "Refusing to overwrite base image files; incomplete restores must be explicitly cleaned before retry") }
    let lock = fm_lock(base.appendingPathComponent("restore.lock").path)
    guard lock >= 0 else { throw RunnerError("locked", "Base restore is already active") }
    defer { close(lock) }
    progress("validating_image")
    let image = try await VZMacOSRestoreImage.image(from: ipsw)
    var metadata = try restoreMetadata(image)
    let requirements = image.mostFeaturefulSupportedConfiguration!
    let cpu = max(try args.integer("--cpu", default: 4), requirements.minimumSupportedCPUCount)
    let memory = max(UInt64(try args.integer("--memory", default: 8 * 1024 * 1024 * 1024)), requirements.minimumSupportedMemorySize)
    let size = try args.integer("--disk-size", default: 100 * 1024 * 1024 * 1024)
    guard size % 512 == 0 else { throw RunnerError("disk_size", "Disk size must be a multiple of 512 bytes") }
    let machine = VZMacMachineIdentifier()
    try writePrivate(requirements.hardwareModel.dataRepresentation, to: base.appendingPathComponent("hardware-model.bin"))
    try writePrivate(machine.dataRepresentation, to: base.appendingPathComponent("machine-id.bin"))
    let auxiliary = try VZMacAuxiliaryStorage(creatingStorageAt: base.appendingPathComponent("auxiliary-storage.bin"), hardwareModel: requirements.hardwareModel)
    let imageDisk = try DiskImage(creating: .asif(url: base.appendingPathComponent("disk.asif"), blockCount: size / 512, blockSize: .bytes512))
    let attachment = try VZDiskImageStorageDeviceAttachment(diskImage: imageDisk)
    let config = try configuration(hardware: requirements.hardwareModel, machine: machine, auxiliary: auxiliary,
                                   disk: attachment, cpu: cpu, memory: memory, network: VZNATNetworkDeviceAttachment(), mac: nil)
    let vm = VZVirtualMachine(configuration: config)
    let installer = VZMacOSInstaller(virtualMachine: vm, restoringFromImageAt: ipsw)
    var lastPercent = -1
    let observation = installer.progress.observe(\.fractionCompleted, options: [.initial, .new]) { p, _ in
        let percent = Int(p.fractionCompleted * 100)
        if percent != lastPercent { lastPercent = percent; progress("restoring", ["fraction": p.fractionCompleted]) }
    }
    let started = Date()
    do { try await installer.install() }
    catch {
        observation.invalidate()
        let actionable = actionableVirtualizationError(error)
        progress("restore_failed", errorObject(actionable))
        throw actionable
    }
    observation.invalidate()
    let completion = try await finishRestoreMachine(vm)
    metadata.merge(completion) { _, latest in latest }
    metadata["base"] = base.path
    metadata["disk_size"] = size
    metadata["recipe_version"] = 1
    metadata["restored_at"] = ISO8601DateFormatter().string(from: Date())
    metadata["restore_seconds"] = Date().timeIntervalSince(started)
    metadata["first_boot"] = false
    metadata["ready"] = true
    for file in ["disk.asif", "hardware-model.bin", "machine-id.bin", "auxiliary-storage.bin"] {
        guard chmod(base.appendingPathComponent(file).path, 0o400) == 0 else { throw RunnerError("readonly_base", "Cannot make restored base read-only") }
    }
    try writeJSON(metadata, to: base.appendingPathComponent("base.json"), mode: 0o400)
    progress("restored", ["seconds": Date().timeIntervalSince(started)])
    return metadata
}

func baseMetadata(_ base: URL) throws -> [String: Any] {
    let metadata = try object(from: Data(contentsOf: base.appendingPathComponent("base.json")))
    guard metadata["ready"] as? Bool == true, metadata["first_boot"] as? Bool == false else {
        throw RunnerError("incomplete_base", "Base is not a completed, unbooted restore")
    }
    return metadata
}

@MainActor
func clone(_ args: Arguments) throws -> [String: Any] {
    try args.validate(values: ["--base", "--slot"])
    let base = try args.path("--base"), slot = try args.path("--slot")
    _ = try baseMetadata(base)
    let fm = FileManager.default
    try fm.createDirectory(at: slot, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    let lock = fm_lock(slot.appendingPathComponent("runner.lock").path)
    guard lock >= 0 else { throw RunnerError("locked", "Slot is in use") }
    defer { close(lock) }
    let files = ["disk.asif", "machine-id.bin", "auxiliary-storage.bin"]
    for file in files where fm.fileExists(atPath: slot.appendingPathComponent(file).path) {
        throw RunnerError("slot_exists", "Refusing to replace existing \(file)")
    }
    // All three paths were absent while holding the slot lock. Clean even a
    // partially created file if an Apple/file API throws before returning.
    let created = files.map { slot.appendingPathComponent($0) }
    do {
        let disk = try DiskImage(opening: .open(url: base.appendingPathComponent("disk.asif"), mode: .readOnly))
        let overlayURL = slot.appendingPathComponent("disk.asif")
        let stack = try disk.appending(.asifLayer(url: overlayURL, type: .overlay))
        let attachment = try VZDiskImageStorageDeviceAttachment(diskImage: stack)
        let auxiliaryURL = slot.appendingPathComponent("auxiliary-storage.bin")
        try fm.copyItem(at: base.appendingPathComponent("auxiliary-storage.bin"), to: auxiliaryURL)
        guard chmod(auxiliaryURL.path, 0o600) == 0, chmod(overlayURL.path, 0o600) == 0 else { throw RunnerError("file_permissions", "Cannot secure slot images") }
        let machine = VZMacMachineIdentifier()
        let machineURL = slot.appendingPathComponent("machine-id.bin")
        try writePrivate(machine.dataRepresentation, to: machineURL)
        withExtendedLifetime(attachment) {}
        return ["ok": true, "slot": slot.path, "machine_id_sha256": digest(machine.dataRepresentation)]
    } catch {
        for url in created { try? fm.removeItem(at: url) }
        throw error
    }
}
