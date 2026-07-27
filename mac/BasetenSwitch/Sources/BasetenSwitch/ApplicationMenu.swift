import AppKit

struct ApplicationMainMenu {
    let menu: NSMenu
    let servicesMenu: NSMenu
    let windowsMenu: NSMenu
}

/// Build the conventional AppKit menu hierarchy expected when the status app
/// temporarily promotes itself to a regular Dock and Cmd+Tab application.
/// Keeping this hierarchy installed while the app is an accessory is harmless:
/// AppKit hides it until the application becomes active.
@MainActor
func makeApplicationMainMenu(displayName: String) -> ApplicationMainMenu {
    let main = NSMenu(title: "Main Menu")

    let applicationRoot = NSMenuItem(title: displayName, action: nil,
                                     keyEquivalent: "")
    let application = NSMenu(title: displayName)
    applicationRoot.submenu = application
    application.addItem(
        withTitle: "About \(displayName)",
        action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)),
        keyEquivalent: "")
    application.addItem(.separator())

    let servicesRoot = NSMenuItem(title: "Services", action: nil,
                                  keyEquivalent: "")
    let services = NSMenu(title: "Services")
    servicesRoot.submenu = services
    application.addItem(servicesRoot)
    application.addItem(.separator())

    application.addItem(
        withTitle: "Hide \(displayName)",
        action: #selector(NSApplication.hide(_:)),
        keyEquivalent: "h")
    let hideOthers = application.addItem(
        withTitle: "Hide Others",
        action: #selector(NSApplication.hideOtherApplications(_:)),
        keyEquivalent: "h")
    hideOthers.keyEquivalentModifierMask = [.command, .option]
    application.addItem(
        withTitle: "Show All",
        action: #selector(NSApplication.unhideAllApplications(_:)),
        keyEquivalent: "")
    application.addItem(.separator())
    application.addItem(
        withTitle: "Quit \(displayName)",
        action: #selector(NSApplication.terminate(_:)),
        keyEquivalent: "q")
    main.addItem(applicationRoot)

    let fileRoot = NSMenuItem(title: "File", action: nil, keyEquivalent: "")
    let file = NSMenu(title: "File")
    fileRoot.submenu = file
    file.addItem(
        withTitle: "Close",
        action: #selector(NSWindow.performClose(_:)),
        keyEquivalent: "w")
    main.addItem(fileRoot)

    let editRoot = NSMenuItem(title: "Edit", action: nil, keyEquivalent: "")
    let edit = NSMenu(title: "Edit")
    editRoot.submenu = edit
    edit.addItem(
        withTitle: "Undo",
        action: Selector(("undo:")),
        keyEquivalent: "z")
    edit.addItem(
        withTitle: "Redo",
        action: Selector(("redo:")),
        keyEquivalent: "Z")
    edit.addItem(.separator())
    edit.addItem(
        withTitle: "Cut",
        action: #selector(NSText.cut(_:)),
        keyEquivalent: "x")
    edit.addItem(
        withTitle: "Copy",
        action: #selector(NSText.copy(_:)),
        keyEquivalent: "c")
    edit.addItem(
        withTitle: "Paste",
        action: #selector(NSText.paste(_:)),
        keyEquivalent: "v")
    edit.addItem(
        withTitle: "Select All",
        action: #selector(NSText.selectAll(_:)),
        keyEquivalent: "a")
    main.addItem(editRoot)

    let windowRoot = NSMenuItem(title: "Window", action: nil,
                                keyEquivalent: "")
    let windows = NSMenu(title: "Window")
    windowRoot.submenu = windows
    windows.addItem(
        withTitle: "Minimize",
        action: #selector(NSWindow.performMiniaturize(_:)),
        keyEquivalent: "m")
    windows.addItem(
        withTitle: "Zoom",
        action: #selector(NSWindow.performZoom(_:)),
        keyEquivalent: "")
    windows.addItem(.separator())
    windows.addItem(
        withTitle: "Bring All to Front",
        action: #selector(NSApplication.arrangeInFront(_:)),
        keyEquivalent: "")
    main.addItem(windowRoot)

    return ApplicationMainMenu(
        menu: main,
        servicesMenu: services,
        windowsMenu: windows)
}

@MainActor
func installApplicationMainMenu(displayName: String) {
    let hierarchy = makeApplicationMainMenu(displayName: displayName)
    NSApp.mainMenu = hierarchy.menu
    NSApp.servicesMenu = hierarchy.servicesMenu
    NSApp.windowsMenu = hierarchy.windowsMenu
}
