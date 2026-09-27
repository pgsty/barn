import Foundation
import DiskImageKit
import Virtualization

// Deliberately never starts a VM: verifies immutable-base assembly and refusal paths.
@main
struct ImagesTest {
    @MainActor static func main() throws {
        let fm = FileManager.default
        let root = fm.temporaryDirectory.appendingPathComponent("farrow-runner-test-\(UUID().uuidString)")
        defer { try? fm.removeItem(at: root) }
        let base = root.appendingPathComponent("base")
        try fm.createDirectory(at: base, withIntermediateDirectories: true)
        let empty = try DiskImage(creating: .asif(url: base.appendingPathComponent("disk.asif"), blockCount: 1024 * 1024, blockSize: .bytes512))
        let baseUUID = empty.layerUUID
        try writeJSON(["ready": true, "first_boot": false], to: base.appendingPathComponent("base.json"))
        try Data("synthetic auxiliary bytes; no OS installed".utf8).write(to: base.appendingPathComponent("auxiliary-storage.bin"))
        var identities: [Data] = []
        for name in ["mac1", "mac2"] {
            let slot = root.appendingPathComponent(name)
            try fm.createDirectory(at: slot, withIntermediateDirectories: true)
            try writeJSON(["instance": name], to: slot.appendingPathComponent("state.json"))
            let args = try Arguments(["clone", "--base", base.path, "--slot", slot.path])
            let response = try clone(args)
            precondition(response["ok"] as? Bool == true)
            identities.append(try Data(contentsOf: slot.appendingPathComponent("machine-id.bin")))
            let auxiliaryCopy = try Data(contentsOf: slot.appendingPathComponent("auxiliary-storage.bin"))
            let auxiliaryBase = try Data(contentsOf: base.appendingPathComponent("auxiliary-storage.bin"))
            precondition(auxiliaryCopy == auxiliaryBase)
            let overlay = try DiskImage(opening: .open(url: slot.appendingPathComponent("disk.asif"), mode: .readOnly))
            precondition(overlay.parentUUID == baseUUID)
            precondition(fm.fileExists(atPath: slot.appendingPathComponent("state.json").path))
            do { _ = try clone(args); fatalError("clone overwrote existing image") }
            catch let e as RunnerError { precondition(e.code == "slot_exists") }
            let identityAfterRefusal = try Data(contentsOf: slot.appendingPathComponent("machine-id.bin"))
            precondition(identityAfterRefusal == identities.last!)
        }
        precondition(identities[0] != identities[1])
        try writeJSON(["ready": true, "first_boot": true], to: base.appendingPathComponent("base.json"))
        do { _ = try baseMetadata(base); fatalError("booted base was accepted") }
        catch let e as RunnerError { precondition(e.code == "incomplete_base") }
        let limit = NSError(domain: VZErrorDomain, code: VZError.Code.virtualMachineLimitExceeded.rawValue)
        let wrappedLimit = NSError(domain: "installer", code: 1, userInfo: [NSUnderlyingErrorKey: limit])
        for error in [limit, wrappedLimit] {
            guard let mapped = actionableVirtualizationError(error) as? RunnerError else { fatalError("VM quota error was not mapped") }
            precondition(mapped.code == "virtual_machine_limit")
            precondition(mapped.message.contains("other tools") && mapped.message.contains("restore"))
        }
        let unrelated = NSError(domain: "installer", code: 2)
        precondition((actionableVirtualizationError(unrelated) as NSError) == unrelated)
        let invalidHardware = root.appendingPathComponent("invalid-hardware.bin")
        try Data("invalid hardware".utf8).write(to: invalidHardware)
        do { _ = try hardwareMetadata(invalidHardware); fatalError("invalid hardware was accepted") }
        catch let e as RunnerError { precondition(e.code == "invalid_hardware") }
        emit(["ok": true, "tests": ["independent_machine_ids", "overlay_parent_uuid", "auxiliary_copy", "preserve_slot_state", "refuse_overwrite", "reject_booted_base", "direct_and_wrapped_vm_quota", "preserve_unrelated_apple_error"], "vm_started": false])
    }
}
