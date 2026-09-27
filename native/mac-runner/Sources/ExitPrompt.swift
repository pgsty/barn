import AppKit

enum RuntimeExitChoice {
    case background, shutdown, cancel

    init(response: NSApplication.ModalResponse) {
        switch response {
        case .alertFirstButtonReturn: self = .background
        case .alertSecondButtonReturn: self = .shutdown
        default: self = .cancel
        }
    }
}

@MainActor
func runtimeExitPrompt(slot: String) -> NSAlert {
    let alert = NSAlert()
    alert.alertStyle = .informational
    alert.messageText = "Keep \(slot) running?"
    alert.informativeText = "The virtual machine is still running. You can leave it in the background or shut it down normally. The window stays open until shutdown completes. If SSH is unavailable, macOS may ask for confirmation inside the guest. Closing the window keeps it running in the background."
    alert.addButton(withTitle: "Keep Running in Background")
    alert.addButton(withTitle: "Request Shutdown")
    let cancel = alert.addButton(withTitle: "Cancel")
    // Enter and Escape must never accidentally request shutdown or hide a VM.
    alert.buttons[0].keyEquivalent = ""
    cancel.keyEquivalent = "\r"
    return alert
}
