import Foundation
import AppKit
import Virtualization
import Darwin

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
            alert.messageText = "Start Farrow Mac from Terminal"
            alert.informativeText = "Run farrow mac up to prepare and start mac1, then farrow mac open to display its desktop. Use farrow mac up mac2 for the second instance."
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
                    response = ["ok": true, "protocol_version": 1, "architecture": "arm64", "virtualization_supported": VZVirtualMachine.isSupported,
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
                case "secret": response = try keychain(args)
                case "help", "--help":
                    response = ["ok": true, "protocol_version": 1, "commands": ["probe", "metadata --ipsw PATH", "discover", "hardware --path PATH", "restore --ipsw PATH --base DIR", "clone --base DIR --slot DIR", "run --base DIR --slot DIR --socket PATH --instance UUID --mac MAC --network-socket PATH --network-slot 1|2", "rpc --socket PATH --instance UUID --method status|open|stop [--force]", "secret set|get|delete --service farrow.mac.ROOT_HASH --account INSTANCE_UUID"]]
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
