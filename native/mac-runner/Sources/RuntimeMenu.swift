import AppKit

// Leave keyboard shortcuts with VZVirtualMachineView. These explicit host
// menu items remain available even while the guest captures Command-Q/W.
@MainActor
func installRuntimeMenu(application: NSApplication, backgroundTarget: AnyObject, backgroundAction: Selector) {
    let main = NSMenu()
    let appItem = NSMenuItem(title: "Farrow Mac", action: nil, keyEquivalent: "")
    let appMenu = NSMenu(title: "Farrow Mac")
    let quit = NSMenuItem(title: "Quit Farrow Mac…", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "")
    quit.target = application
    appMenu.addItem(quit)
    appItem.submenu = appMenu
    main.addItem(appItem)

    let windowItem = NSMenuItem(title: "Window", action: nil, keyEquivalent: "")
    let windowMenu = NSMenu(title: "Window")
    let background = NSMenuItem(title: "Keep Running in Background", action: backgroundAction, keyEquivalent: "")
    background.target = backgroundTarget
    windowMenu.addItem(background)
    windowItem.submenu = windowMenu
    main.addItem(windowItem)
    application.mainMenu = main
    application.windowsMenu = windowMenu
}
