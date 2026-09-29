import AppKit

@MainActor
final class RuntimeStub: NSObject {
    var background = false
    var clipboard = false
    @objc func keepRunningInBackground(_ sender: Any?) { background = true }
    @objc func toggleClipboard(_ sender: Any?) { clipboard.toggle() }
    @objc func restartGuest(_ sender: Any?) {}
    @objc func shutDownGuest(_ sender: Any?) {}
}

@main
struct ExitPromptTest {
    @MainActor
    static func main() {
        let prompt = runtimeExitPrompt(slot: "mac2")
        precondition(prompt.messageText.contains("mac2"))
        precondition(prompt.buttons.count == 3)
        precondition(prompt.buttons[0].keyEquivalent.isEmpty)
        precondition(prompt.buttons[2].keyEquivalent == "\r")
        precondition(RuntimeExitChoice(response: .alertFirstButtonReturn) == .background)
        precondition(RuntimeExitChoice(response: .alertSecondButtonReturn) == .shutdown)
        precondition(RuntimeExitChoice(response: .alertThirdButtonReturn) == .cancel)
        precondition(RuntimeExitChoice(response: .abort) == .cancel)
        precondition(prompt.informativeText.contains("window stays open until shutdown completes"))

        let application = NSApplication.shared
        let stub = RuntimeStub()
        installRuntimeMenu(application: application, runtime: stub)
        let main = application.mainMenu!
        precondition(main.items.map(\.title) == ["Barn Mac", "Machine", "View", "Window"])
        let quit = main.items[0].submenu!.items.first { $0.title == "Quit Barn Mac…" }!
        precondition(quit.target === application && quit.action == #selector(NSApplication.terminate(_:)))
        let machine = main.items[1].submenu!
        for item in machine.items where !item.isSeparatorItem {
            precondition(item.target === stub && item.keyEquivalent.isEmpty && stub.responds(to: item.action!))
        }
        precondition(machine.items.filter { !$0.isSeparatorItem }.map(\.title) == ["Share Clipboard", "Restart…", "Shut Down…"])
        machine.performActionForItem(at: 0)
        precondition(stub.clipboard)
        let windowMenu = main.items[3].submenu!
        precondition(application.windowsMenu === windowMenu)
        let background = windowMenu.items.first { $0.title == "Keep Running in Background" }!
        precondition(background.target === stub && background.keyEquivalent.isEmpty)
        windowMenu.performActionForItem(at: windowMenu.index(of: background))
        precondition(stub.background)
        print("Exit prompt and host menu tests passed (no VM or visible window)")
    }
}
