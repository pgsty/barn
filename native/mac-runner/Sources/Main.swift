import Foundation
import AppKit
import Virtualization
import Darwin

// The command and RPC contract the Go CLI speaks. Bump on any change.
let protocolVersion = 2

@main
struct Main {
    @MainActor static var runtime: Runtime?

    @MainActor static func main() {
        umask(0o077)
        signal(SIGPIPE, SIG_IGN)
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        if CommandLine.arguments.count == 1 && Bundle.main.bundleURL.pathExtension == "app" {
            let alert = NSAlert()
            alert.messageText = "Farrow Mac runs from Terminal"
            alert.informativeText = "Run farrow mac up to create and start a macOS virtual machine, then farrow mac open to show its desktop."
            alert.addButton(withTitle: "OK")
            app.activate(ignoringOtherApps: true)
            alert.runModal()
            return
        }
        Task { @MainActor in
            do {
                let args = try Arguments(Array(CommandLine.arguments.dropFirst()))
                let response: [String: Any]
                switch args.command {
                case "probe":
                    try args.validate(values: [])
                    let host = ProcessInfo.processInfo.operatingSystemVersion
                    response = ["ok": true, "protocol_version": protocolVersion, "architecture": "arm64", "virtualization_supported": VZVirtualMachine.isSupported,
                                "version": Bundle.main.object(forInfoDictionaryKey: "FarrowVersion") as? String ?? "standalone",
                                "commit": Bundle.main.object(forInfoDictionaryKey: "FarrowCommit") as? String ?? "unknown",
                                "host_version": "\(host.majorVersion).\(host.minorVersion).\(host.patchVersion)", "cpu_count": ProcessInfo.processInfo.activeProcessorCount,
                                "physical_memory": ProcessInfo.processInfo.physicalMemory, "pid": getpid()]
                case "metadata":
                    try args.validate(values: ["--ipsw"])
                    response = try restoreMetadata(await VZMacOSRestoreImage.image(from: args.path("--ipsw")))
                case "discover":
                    try args.validate(values: [])
                    let image = try await VZMacOSRestoreImage.latestSupported
                    var data = try restoreMetadata(image)
                    data["url"] = image.url.absoluteString
                    response = data
                case "hardware":
                    try args.validate(values: ["--path"])
                    response = try hardwareMetadata(args.path("--path"))
                case "restore": response = try await restore(args)
                case "clone": response = try clone(args)
                case "run":
                    let manager = try Runtime(args: args)
                    runtime = manager
                    try await manager.start(args)
                    return
                case "rpc": response = try rpc(args)
                case "help", "--help":
                    response = ["ok": true, "protocol_version": protocolVersion, "commands": [
                        "probe", "metadata --ipsw PATH", "discover", "hardware --path PATH",
                        "restore --ipsw PATH --base DIR [--cpu N] [--memory BYTES] [--disk-size BYTES]",
                        "clone --base DIR --slot DIR",
                        "run --base DIR --slot DIR --socket PATH --instance UUID --mac MAC --gateway IP --address IP [--cpu N] [--memory BYTES] [--recovery]",
                        "rpc --socket PATH --instance UUID --method status|open|stop [--force]"]]
                default: throw RunnerError("argument", "Unknown runner command \(args.command)")
                }
                emit(response)
                exit(response["ok"] as? Bool == false ? 1 : 0)
            } catch {
                runtime?.cleanup()
                emit(errorObject(error))
                exit(1)
            }
        }
        app.run()
    }
}
