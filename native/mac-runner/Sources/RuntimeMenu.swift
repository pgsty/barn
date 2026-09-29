import AppKit

// Actions the runtime implements. String selectors keep the menu testable
// without a virtual machine.
enum RuntimeAction {
    static let toggleClipboard = NSSelectorFromString("toggleClipboard:")
    static let restart = NSSelectorFromString("restartGuest:")
    static let shutDown = NSSelectorFromString("shutDownGuest:")
    static let background = NSSelectorFromString("keepRunningInBackground:")
}

// Menu items carry no key equivalents: VZVirtualMachineView captures system
// keys for the guest, and these host commands must stay reachable anyway.
@MainActor
func installRuntimeMenu(application: NSApplication, runtime: AnyObject) {
    let main = NSMenu()

    let appItem = NSMenuItem(title: "Farrow Mac", action: nil, keyEquivalent: "")
    let appMenu = NSMenu(title: "Farrow Mac")
    let quit = NSMenuItem(title: "Quit Farrow Mac…", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "")
    quit.target = application
    appMenu.addItem(quit)
    appItem.submenu = appMenu
    main.addItem(appItem)

    let machineItem = NSMenuItem(title: "Machine", action: nil, keyEquivalent: "")
    let machineMenu = NSMenu(title: "Machine")
    let clipboard = NSMenuItem(title: "Share Clipboard", action: RuntimeAction.toggleClipboard, keyEquivalent: "")
    clipboard.target = runtime
    machineMenu.addItem(clipboard)
    machineMenu.addItem(.separator())
    let restart = NSMenuItem(title: "Restart…", action: RuntimeAction.restart, keyEquivalent: "")
    restart.target = runtime
    machineMenu.addItem(restart)
    let shutdown = NSMenuItem(title: "Shut Down…", action: RuntimeAction.shutDown, keyEquivalent: "")
    shutdown.target = runtime
    machineMenu.addItem(shutdown)
    machineItem.submenu = machineMenu
    main.addItem(machineItem)

    let viewItem = NSMenuItem(title: "View", action: nil, keyEquivalent: "")
    let viewMenu = NSMenu(title: "View")
    viewMenu.addItem(NSMenuItem(title: "Enter Full Screen", action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: ""))
    viewItem.submenu = viewMenu
    main.addItem(viewItem)

    let windowItem = NSMenuItem(title: "Window", action: nil, keyEquivalent: "")
    let windowMenu = NSMenu(title: "Window")
    let background = NSMenuItem(title: "Keep Running in Background", action: RuntimeAction.background, keyEquivalent: "")
    background.target = runtime
    windowMenu.addItem(background)
    windowMenu.addItem(NSMenuItem(title: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: ""))
    windowItem.submenu = windowMenu
    main.addItem(windowItem)

    application.mainMenu = main
    application.windowsMenu = windowMenu
}
