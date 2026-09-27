import AppKit

@MainActor
final class BackgroundMenuTarget: NSObject {
    var requested = false
    @objc func background(_ sender: Any?) { requested = true }
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
        let target = BackgroundMenuTarget()
        installRuntimeMenu(application: application, backgroundTarget: target,
                           backgroundAction: #selector(BackgroundMenuTarget.background(_:)))
        let main = application.mainMenu!
        precondition(main.items.count == 2)
        let appMenu = main.items[0].submenu!
        let quit = appMenu.items.first { $0.title == "Quit Farrow Mac…" }!
        precondition(quit.target === application)
        precondition(quit.action == #selector(NSApplication.terminate(_:)))
        precondition(quit.keyEquivalent.isEmpty)
        let windowMenu = main.items[1].submenu!
        precondition(application.windowsMenu === windowMenu)
        let background = windowMenu.items.first { $0.title == "Keep Running in Background" }!
        precondition(background.target === target)
        precondition(background.keyEquivalent.isEmpty)
        windowMenu.performActionForItem(at: windowMenu.index(of: background))
        precondition(target.requested)
        print("Exit prompt and host menu tests passed (no VM or visible window)")
    }
}
