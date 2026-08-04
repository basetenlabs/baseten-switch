import SwiftUI
import AppKit

enum AppColors {
    /// Baseten brand green (#16D766), shared by active routing indicators.
    static let basetenGreen = NSColor(
        srgbRed: 0x16 / 255.0,
        green: 0xD7 / 255.0,
        blue: 0x66 / 255.0,
        alpha: 1.0)
}

enum MenubarIconMetrics {
    static let canvas = NSSize(width: 18, height: 18)
    static let glyph = NSSize(width: 11.5, height: 11.5)
    static let borderWidth: CGFloat = 1
    static let cornerRadius: CGFloat = 3.75
}

/// Native menu-bar shell: AppKit owns the NSStatusItem and NSMenu via
/// StatusItemController. The SwiftUI App struct only hosts the delegate
/// adaptor and the single-instance guard.
@main
struct BasetenSwitchApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    init() {
#if DEBUG
        // Fixture previews are separate and side-effect-free; allow them
        // alongside the packaged menubar process.
        guard PopupPreviewFixture.requested == nil,
              PopupPreviewFixture.routerWindowRequested == nil else { return }
#endif
        BasetenSwitchApp.exitIfAlreadyRunning(variant: .current())
    }

    /// Single-instance guard. If another process with our bundle identifier
    /// is already running, exit 0 immediately without activating anything.
    static func exitIfAlreadyRunning(variant: AppVariant) {
        let myPID = ProcessInfo.processInfo.processIdentifier
        let others = NSRunningApplication
            .runningApplications(withBundleIdentifier: variant.bundleIdentifier)
            .filter { $0.processIdentifier != myPID }
        guard let other = others.first else { return }
        FileHandle.standardError.write(Data(
            "[BasetenSwitch] another instance is already running (pid \(other.processIdentifier)); exiting\n".utf8))
        exit(0)
    }

    var body: some Scene {
        // No visible scene: the status item and menu are AppKit,
        // created by the delegate. Settings is the cheapest valid
        // Scene; it never opens on its own.
        Settings { EmptyView() }
    }

    /// The Baseten logo loaded from the packaged resource or source tree.
    private static let menubarLogo: NSImage = {
        guard let url = menubarIconResourceURL(),
              let img = NSImage(contentsOf: url) else {
            return NSImage(systemSymbolName: "circle.lefthalf.filled",
                           accessibilityDescription: "Baseten Switch") ?? NSImage()
        }
        return img
    }()

    /// An 18pt rounded-square template mark. The 11.5pt Baseten glyph leaves
    /// enough negative space that it matches neighboring system extras
    /// optically; the thin frame gives it a bounded silhouette similar to
    /// Notion's status item without relying on a literal light/dark color.
    static let menubarIcon = framedMenubarIcon(menubarLogo)

    static func menubarIconResourceURL() -> URL? {
        if let bundled = Bundle.main.url(forResource: "baseten-logo-white",
                                         withExtension: "svg") {
            return bundled
        }

        let packageRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let sourceAsset = packageRoot
            .appendingPathComponent("Assets/baseten-logo-white.svg")
        return FileManager.default.fileExists(atPath: sourceAsset.path)
            ? sourceAsset
            : nil
    }

    static func framedMenubarIcon(_ logo: NSImage) -> NSImage {
        let image = NSImage(size: MenubarIconMetrics.canvas)
        image.lockFocus()
        NSGraphicsContext.current?.imageInterpolation = .high

        let canvas = NSRect(origin: .zero, size: MenubarIconMetrics.canvas)
        let borderRect = canvas.insetBy(dx: 0.75, dy: 0.75)
        let border = NSBezierPath(
            roundedRect: borderRect,
            xRadius: MenubarIconMetrics.cornerRadius,
            yRadius: MenubarIconMetrics.cornerRadius)
        border.lineWidth = MenubarIconMetrics.borderWidth
        NSColor.black.setStroke()
        border.stroke()

        let glyphOrigin = NSPoint(
            x: (canvas.width - MenubarIconMetrics.glyph.width) / 2,
            y: (canvas.height - MenubarIconMetrics.glyph.height) / 2)
        logo.draw(in: NSRect(
            origin: glyphOrigin,
            size: MenubarIconMetrics.glyph))

        image.unlockFocus()
        image.isTemplate = true
        return image
    }

    /// Baseten brand green (#16D766). The active icon is a pre-tinted
    /// non-template copy: the status bar ignores tint on template
    /// images (the system owns their color), so color state requires a
    /// real color image. The inactive icon stays the template so the
    /// system keeps adapting it to light/dark menu bars; a literal
    /// black glyph would vanish on a dark menu bar.
    static let menubarIconActive: NSImage = {
        tinted(menubarIcon, AppColors.basetenGreen)
    }()

    /// Degraded amber (#F5A623): gateway up but at least one enabled
    /// client is serving via native fallback. Pre-tinted non-template
    /// for the same reason as the green icon.
    static let menubarIconDegraded: NSImage = {
        let amber = NSColor(srgbRed: 0xF5 / 255.0, green: 0xA6 / 255.0,
                            blue: 0x23 / 255.0, alpha: 1.0)
        return tinted(menubarIcon, amber)
    }()

    private static let menubarIconPreview = previewBadged(menubarIcon)
    private static let menubarIconPreviewActive = tinted(
        menubarIconPreview,
        AppColors.basetenGreen)
    private static let menubarIconPreviewDegraded = tinted(
        menubarIconPreview,
        NSColor(
            srgbRed: 0xF5 / 255.0,
            green: 0xA6 / 255.0,
            blue: 0x23 / 255.0,
            alpha: 1.0))

    /// Production Preview keeps the same 18pt optical footprint as Stable,
    /// but adds a small lower-right dot so both status items remain
    /// distinguishable when they run side by side.
    static func menubarIcon(for variant: AppVariant,
                            state: MenubarIconState = .off) -> NSImage {
        if variant.channel == .preview {
            switch state {
            case .off: return menubarIconPreview
            case .active: return menubarIconPreviewActive
            case .degraded: return menubarIconPreviewDegraded
            }
        }
        switch state {
        case .off: return menubarIcon
        case .active: return menubarIconActive
        case .degraded: return menubarIconDegraded
        }
    }

    static func previewBadged(_ base: NSImage) -> NSImage {
        let image = NSImage(size: base.size)
        image.lockFocus()
        let rect = NSRect(origin: .zero, size: base.size)
        base.draw(in: rect)
        NSColor.black.setFill()
        NSBezierPath(ovalIn: NSRect(x: 13.75, y: 0.25,
                                    width: 3.75, height: 3.75)).fill()
        image.unlockFocus()
        image.isTemplate = true
        return image
    }

    /// Draws the glyph and fills its alpha with `color` (sourceAtop),
    /// yielding a non-template colored copy at the same point size.
    static func tinted(_ base: NSImage, _ color: NSColor) -> NSImage {
        let img = NSImage(size: base.size)
        img.lockFocus()
        let rect = NSRect(origin: .zero, size: base.size)
        base.draw(in: rect)
        color.set()
        rect.fill(using: .sourceAtop)
        img.unlockFocus()
        img.isTemplate = false
        return img
    }
}

/// Owns the app-level objects for the AppKit shell. State and the
/// status-item controller are created here (not in the App struct) so
/// their lifecycle is tied to the running application, not SwiftUI
/// scene evaluation.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var state: BasetenSwitchState?
    private var statusController: StatusItemController?
    private var routerWindowController: RouterWindowController?
#if DEBUG
    private var previewController: StatusItemController?
    private var previewRouterWindowController: RouterWindowController?
    private var previewHostWindow: NSWindow?
#endif

    func applicationDidFinishLaunching(_ notification: Notification) {
        let variant = AppVariant.current()
        installApplicationMainMenu(displayName: variant.displayName)
#if DEBUG
        if let fixture = PopupPreviewFixture.routerWindowRequested {
            showRouterWindowPreview(fixture)
            return
        }
        if let fixture = PopupPreviewFixture.requested {
            showPreview(fixture)
            return
        }
#endif
        // Accessory policy: no Dock icon, no app menu. The packaged
        // .app sets LSUIElement; this covers bare `swift run` too.
        NSApp.setActivationPolicy(.accessory)
        let state = BasetenSwitchState(variant: variant)
        let routerWindowController = RouterWindowController(
            state: state,
            variant: variant,
            windowOpenChanged: { isOpen in
                let policy: NSApplication.ActivationPolicy = isOpen ? .regular : .accessory
                if !NSApp.setActivationPolicy(policy) {
                    FileHandle.standardError.write(Data(
                        "[BasetenSwitch] failed to set activation policy to \(policy.rawValue)\n".utf8))
                }
                if isOpen { NSApp.activate(ignoringOtherApps: true) }
            })
        self.state = state
        self.routerWindowController = routerWindowController
        self.statusController = StatusItemController(
            state: state,
            variant: variant,
            openConfiguration: { [weak routerWindowController] clientName in
                routerWindowController?.show(clientName: clientName)
            },
            openTraffic: { [weak routerWindowController] in
                routerWindowController?.show(destination: .traffic)
            })
    }

    /// Opening the already-running LSUIElement app from Finder or `open`
    /// should reveal its durable configuration surface, not silently no-op.
    func applicationShouldHandleReopen(_ sender: NSApplication,
                                       hasVisibleWindows flag: Bool) -> Bool {
        if flag {
            NSApp.activate(ignoringOtherApps: true)
        } else {
            routerWindowController?.show()
        }
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        state?.stop()
    }

#if DEBUG
    /// Render fixture state in the real native menu. Preview actions are
    /// intercepted by StatusItemController, so no CLI mutation can escape.
    private func showPreview(_ fixture: PopupPreviewFixture) {
        NSApp.setActivationPolicy(.accessory)
        if ProcessInfo.processInfo.environment["BASETEN_SWITCH_POPUP_APPEARANCE"] == "dark" {
            NSApp.appearance = NSAppearance(named: .darkAqua)
        }
        let variant = AppVariant.current()
        let state = BasetenSwitchState(preview: fixture, variant: variant)
        let forceActivity =
            ProcessInfo.processInfo.environment["BASETEN_SWITCH_MENUBAR_ACTIVITY"] == "1"
        let controller = StatusItemController(
            state: state,
            variant: variant,
            isPreview: true,
            forceActivity: forceActivity)
        self.state = state
        previewController = controller
        let hasHostWindow =
            ProcessInfo.processInfo.environment["BASETEN_SWITCH_POPUP_HOST_WINDOW"] == "1"
        if hasHostWindow {
            let host = NSWindow(
                contentRect: NSRect(x: 0, y: 0, width: 320, height: 96),
                styleMask: [.titled, .closable],
                backing: .buffered,
                defer: false)
            host.title = "Baseten Switch Menu Fixture"
            host.contentView = NSHostingView(rootView:
                Text("Side-effect-free native menu fixture")
                    .foregroundStyle(.secondary)
                    .padding(24))
            host.center()
            host.makeKeyAndOrderFront(nil)
            previewHostWindow = host
        }
        if ProcessInfo.processInfo.environment["BASETEN_SWITCH_POPUP_AUTO_OPEN"] != "0" {
            let menuDelay = hasHostWindow ? 2.0 : 0.35
            DispatchQueue.main.asyncAfter(deadline: .now() + menuDelay) {
                controller.openForPreview()
            }
        }
    }

    private func showRouterWindowPreview(_ fixture: PopupPreviewFixture) {
        NSApp.setActivationPolicy(.accessory)
        if ProcessInfo.processInfo.environment["BASETEN_SWITCH_POPUP_APPEARANCE"] == "dark" {
            NSApp.appearance = NSAppearance(named: .darkAqua)
        } else {
            NSApp.appearance = NSAppearance(named: .aqua)
        }
        let variant = AppVariant.current()
        let state = BasetenSwitchState(preview: fixture, variant: variant)
        let controller = RouterWindowController(
            state: state, variant: variant, isPreview: true)
        self.state = state
        previewRouterWindowController = controller
        controller.show(clientName: fixture.clients.first?.name)
    }
#endif
}
