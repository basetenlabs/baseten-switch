import AppKit
import Combine
import QuartzCore

/// AppKit owns the status item, menu tracking, keyboard behavior, and the
/// native global switch. The menu is rebuilt only immediately before a
/// meaningful presentation, never in response to an unchanged background
/// poll and never while AppKit is tracking it.
@MainActor
final class StatusItemController: NSObject, NSMenuDelegate {
    private let state: BasetenSwitchState
    private let variant: AppVariant
    private let isPreview: Bool
    private let forceActivity: Bool
    private let openConfiguration: (String?) -> Void
    private let openTraffic: () -> Void
    private let statusItem: NSStatusItem
    private let menu = NSMenu()
    private var stateSink: AnyCancellable?
    private var menuIsOpen = false
    private var menuNeedsRebuild = true
    private var displayedIconState: MenubarIconState?
    private var displayedActivity: Bool?
    private weak var headerView: StatusMenuHeaderView?

    init(state: BasetenSwitchState,
         variant: AppVariant = .current(),
         isPreview: Bool = false,
         forceActivity: Bool = false,
         openConfiguration: @escaping (String?) -> Void = { _ in },
         openTraffic: @escaping () -> Void = {}) {
        self.state = state
        self.variant = variant
        self.isPreview = isPreview
        self.forceActivity = forceActivity
        self.openConfiguration = openConfiguration
        self.openTraffic = openTraffic
        statusItem = NSStatusBar.system.statusItem(
            withLength: NSStatusItem.squareLength)
        super.init()

        statusItem.autosaveName = isPreview
            ? "baseten-switch-fixture-\(ProcessInfo.processInfo.processIdentifier)"
            : variant.statusItemAutosaveName
        statusItem.button?.toolTip = variant.displayName
        statusItem.button?.setAccessibilityLabel(variant.displayName)

        menu.autoenablesItems = false
        menu.delegate = self
        statusItem.menu = menu

        stateSink = state.objectWillChange
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in
                // ObservableObject announces before mutation. Defer one turn
                // so projections read the committed immutable snapshot.
                DispatchQueue.main.async {
                    guard let self else { return }
                    self.menuNeedsRebuild = true
                    self.updateIconIfNeeded()
                    if self.menuIsOpen {
                        self.updateTrackedHeader()
                    }
                }
            }

        updateIconIfNeeded()
        rebuildMenu()
    }

    func menuNeedsUpdate(_ menu: NSMenu) {
        guard menuNeedsRebuild, !menuIsOpen else { return }
        rebuildMenu()
    }

    func menuWillOpen(_ menu: NSMenu) {
        menuIsOpen = true
        state.menuDidShow()
    }

    func menuDidClose(_ menu: NSMenu) {
        menuIsOpen = false
        state.menuDidHide()
    }

#if DEBUG
    func openForPreview() {
        rebuildMenu()
        guard let button = statusItem.button else { return }
        NSApp.activate(ignoringOtherApps: true)
        menu.update()
        menu.popUp(
            positioning: nil,
            at: NSPoint(x: button.bounds.minX,
                        y: button.bounds.minY - 2),
            in: button)
    }
#endif

    // MARK: - Menu

    private func rebuildMenu() {
        guard !menuIsOpen else { return }
        menu.removeAllItems()

        let headerItem = NSMenuItem()
        headerItem.title = "Global routing"
        let toggle = globalRoutingTogglePresentation(
            state: state,
            isPreviewFixture: isPreview)
        let header = StatusMenuHeaderView(
            productName: variant.displayName,
            subtitle: headerSubtitle,
            isOn: toggle.isOn,
            accessibilityStatus: toggle.accessibilityStatus,
            switchEnabled: toggle.isEnabled,
            disabledReason: toggle.disabledReason,
            onToggle: { [weak self] enabled in
                self?.changeGlobalRouting(enabled) ?? false
            })
        headerItem.view = header
        headerView = header
        menu.addItem(headerItem)
        menu.addItem(.separator())

        addWarnings()

        if state.routingSnapshot != nil {
            if state.clients.isEmpty {
                menu.addItem(disabledItem("No Routing Clients Configured"))
            } else {
                for client in state.clients {
                    menu.addItem(clientMenuItem(client))
                }
            }
        } else {
            let start = actionItem(
                state.starting
                    ? "Starting \(variant.displayName)…"
                    : "Start \(variant.displayName)",
                action: #selector(startSystem))
            start.isEnabled = !state.starting && state.canStartSystem
            menu.addItem(start)
        }

        menu.addItem(trafficMenuItem())
        menu.addItem(.separator())
        menu.addItem(actionItem(
            "Open \(variant.displayName)",
            action: #selector(openConfigurationWindow(_:))))

        if variant.allowsLoginItem {
            let login = actionItem(
                startAtLoginTitle,
                action: #selector(toggleStartAtLogin))
            login.state = state.loginItemStatus == .enabled ? .on : .off
            menu.addItem(login)
        }

        menu.addItem(.separator())
        menu.addItem(actionItem(
            "Quit \(variant.displayName)",
            action: #selector(quit),
            keyEquivalent: "q"))
        menuNeedsRebuild = false
    }

    private var headerSubtitle: String {
        if let pending = state.pendingGlobalRouting {
            return pending.phase == .applying
                ? "Applying routing change…"
                : "Reconciling routing change…"
        }
        if case .previewMismatch = state.runtimeTrust {
            return "Preview runtime needs attention"
        }
        if case .identityMismatch = state.runtimeTrust {
            return "App identity needs attention"
        }
        if !state.gatewayUp {
            return "Local gateway is unavailable"
        }
        if authNeedsReauth(auth: state.auth) {
            return "Authentication required"
        }
        return state.confirmedGlobalRoutingEnabled
            ? "Routing rules are active"
            : "Native providers only"
    }

    /// Updating labels and control state in the existing custom header is
    /// safe while AppKit tracks the menu. Structure and item geometry remain
    /// untouched, so keyboard selection and submenu placement stay stable.
    private func updateTrackedHeader() {
        let toggle = globalRoutingTogglePresentation(
            state: state,
            isPreviewFixture: isPreview)
        headerView?.update(
            subtitle: headerSubtitle,
            isOn: toggle.isOn,
            accessibilityStatus: toggle.accessibilityStatus,
            switchEnabled: toggle.isEnabled,
            disabledReason: toggle.disabledReason)
    }

    private func addWarnings() {
        var added = false

        if state.snapshotIsStale, state.routingSnapshot != nil {
            let item = disabledItem("Showing Last Confirmed Routing State")
            item.image = symbol("wifi.exclamationmark")
            menu.addItem(item)
            added = true
        }

        if authNeedsReauth(auth: state.auth) {
            let item = variant.channel == .preview
                ? disabledItem("Preview Authentication Disabled")
                : actionItem(
                    "Authentication Required…",
                    action: #selector(reauthenticate))
            item.image = symbol("key.fill")
            item.isEnabled = variant.channel == .stable && !state.reauthenticating
            menu.addItem(item)
            added = true
        }

        for client in state.clients where client.enabled && client.fallbackActive {
            let route = client.fallback.servedRoute.isEmpty
                ? client.nativeRoute
                : client.fallback.servedRoute
            let suffix = route.isEmpty ? "" : " via \(capitalizeFamily(route))"
            let item = disabledItem(
                "\(clientDisplayName(client.name)) Fallback Active\(suffix)")
            item.image = symbol("arrow.triangle.branch")
            menu.addItem(item)
            added = true
        }

        if let note = versionSkewNote(
            routerVersion: state.routerVersion,
            cliVersion: state.cliVersion) {
            let item = disabledItem(note)
            item.image = symbol("arrow.triangle.2.circlepath")
            menu.addItem(item)
            added = true
        }

        if let error = state.lastError {
            let item = disabledItem("Last Action Failed")
            item.image = symbol("exclamationmark.triangle")
            item.toolTip = menuErrorLabel(error)
            menu.addItem(item)
            added = true
        }

        if let message = state.mutationRecoveryMessage {
            let item = disabledItem("Routing Cleanup Required")
            item.image = symbol("exclamationmark.triangle")
            item.toolTip = menuErrorLabel(message)
            menu.addItem(item)
            if state.canRetryMutationCleanup {
                menu.addItem(actionItem(
                    "Retry cleanup",
                    action: #selector(retryMutationCleanup(_:))))
            }
            added = true
        }

        if added {
            menu.addItem(.separator())
        }
    }

    private func clientMenuItem(_ client: ClientStatus) -> NSMenuItem {
        let item = NSMenuItem(
            title: serverAuthoredClientMenuTitle(client),
            action: nil,
            keyEquivalent: "")
        item.image = ClientBrandAssets.menuImage(for: client.name)

        let submenu = NSMenu(title: clientDisplayName(client.name))
        submenu.autoenablesItems = false
        let configure = actionItem(
            "Configure \(clientDisplayName(client.name))…",
            action: #selector(openConfigurationWindow(_:)))
        configure.representedObject = client.name
        submenu.addItem(configure)
        item.submenu = submenu
        return item
    }

    private func trafficMenuItem() -> NSMenuItem {
        let item = NSMenuItem(
            title: "Traffic: \(glanceHeaderLabel(state.stats))",
            action: nil,
            keyEquivalent: "")
        let submenu = NSMenu(title: "Traffic")
        submenu.autoenablesItems = false

        if let latest = state.stats?.recent.last {
            let model = compactFeedModelLabel(
                requested: latest.requestedModel,
                upstream: latest.upstreamModel)
            let route = compactFeedRouteLabel(
                route: latest.route,
                routeEffective: latest.routeEffective)
            submenu.addItem(disabledItem("Latest: \(model) via \(route)"))
        } else {
            submenu.addItem(disabledItem("No Recent Requests"))
        }

        let errors = state.stats?.buckets.reduce(0) {
            $0 + $1.errors
        } ?? 0
        if errors > 0 {
            submenu.addItem(disabledItem("Errors in Window: \(errors)"))
        }

        submenu.addItem(.separator())
        submenu.addItem(actionItem(
            "Open Traffic…",
            action: #selector(openNativeTraffic)))
        item.submenu = submenu
        return item
    }

    private func actionItem(_ title: String,
                            action: Selector,
                            keyEquivalent: String = "") -> NSMenuItem {
        let item = NSMenuItem(
            title: title,
            action: action,
            keyEquivalent: keyEquivalent)
        item.target = self
        item.isEnabled = true
        return item
    }

    private func disabledItem(_ title: String) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        return item
    }

    private func symbol(_ name: String) -> NSImage? {
        guard let image = NSImage(
            systemSymbolName: name,
            accessibilityDescription: nil) else { return nil }
        image.isTemplate = true
        image.size = NSSize(width: 16, height: 16)
        return image
    }

    private var startAtLoginTitle: String {
        state.loginItemStatus == .requiresApproval
            ? "Start at Login (Approval Required)…"
            : "Start at Login"
    }

    // MARK: - Actions

    private func changeGlobalRouting(_ enabled: Bool) -> Bool {
        guard !isPreview else { return false }
        return state.requestGlobalRouting(enabled)
    }

    @objc private func reauthenticate() {
        guard !isPreview, variant.channel == .stable else { return }
        Task { await state.reauthenticate() }
    }

    @objc private func startSystem() {
        guard !isPreview else { return }
        Task { await state.startSystem() }
    }

    @objc private func openConfigurationWindow(_ sender: NSMenuItem) {
        guard !isPreview else { return }
        openConfiguration(sender.representedObject as? String)
    }

    @objc private func retryMutationCleanup(_ sender: NSMenuItem) {
        state.retryMutationCleanup()
    }

    @objc private func openNativeTraffic() {
        openTraffic()
    }

    @objc private func toggleStartAtLogin() {
        guard !isPreview else { return }
        if startAtLoginOpensSystemSettings(status: state.loginItemStatus) {
            state.openLoginItemSettings()
        } else {
            state.toggleStartAtLogin()
        }
    }

    @objc private func quit() {
        guard !isPreview else { return }
        state.quit()
    }

    // MARK: - Status icon

    private func updateIconIfNeeded() {
        let projected = menubarIconState(
            gatewayUp: state.gatewayUp,
            globalRoutingEnabled: state.confirmedGlobalRoutingEnabled,
            clients: state.clients,
            auth: state.auth)
        let hasActivity = forceActivity || state.activeRequests > 0
        guard projected != displayedIconState
                || hasActivity != displayedActivity else { return }
        displayedIconState = projected
        displayedActivity = hasActivity
        statusItem.button?.image = BasetenSwitchApp.menubarIcon(
            for: variant,
            state: menubarIconState(projected, hasActivity: hasActivity))
        updateActivityPulse(hasActivity)
    }

    private func updateActivityPulse(_ active: Bool) {
        guard let button = statusItem.button else { return }
        let animationKey = "baseten-switch.activityPulse"
        button.layer?.removeAnimation(forKey: animationKey)
        button.alphaValue = 1
        button.toolTip = active
            ? "\(variant.displayName) is sending or receiving data"
            : variant.displayName
        button.setAccessibilityLabel(active
            ? "\(variant.displayName), sending or receiving data"
            : variant.displayName)

        guard active,
              !NSWorkspace.shared.accessibilityDisplayShouldReduceMotion else {
            return
        }
        button.wantsLayer = true
        let animation = CABasicAnimation(keyPath: "opacity")
        animation.fromValue = 1.0
        animation.toValue = 0.35
        animation.duration = 0.7
        animation.autoreverses = true
        animation.repeatCount = .infinity
        animation.timingFunction = CAMediaTimingFunction(name: .easeInEaseOut)
        button.layer?.add(animation, forKey: animationKey)
    }
}

@MainActor
final class StatusMenuHeaderView: NSView {
    private enum Metrics {
        static let width: CGFloat = 360
        static let height: CGFloat = 58
        static let horizontalInset: CGFloat = 14
        static let titleTopInset: CGFloat = 8
    }

    private let detail: NSTextField
    private let toggleModel: StatusHeaderToggleModel
    private let toggleControl: StatusHeaderToggleControl

    init(productName: String = "Baseten Switch",
         subtitle: String,
         isOn: Bool,
         accessibilityStatus: String,
         switchEnabled: Bool,
         disabledReason: String?,
         onToggle: @escaping (Bool) -> Bool) {
        let toggleModel = StatusHeaderToggleModel(
            isOn: isOn,
            isEnabled: switchEnabled,
            accessibilityStatus: accessibilityStatus,
            accessibilityHelp: disabledReason
                ?? "Routes supported coding traffic using the saved model mappings.",
            onToggle: onToggle)
        self.toggleModel = toggleModel
        toggleControl = StatusHeaderToggleControl(model: toggleModel)
        detail = NSTextField(labelWithString: subtitle)
        super.init(frame: NSRect(
            x: 0,
            y: 0,
            width: Metrics.width,
            height: Metrics.height))

        let title = NSTextField(labelWithString: productName)
        title.font = .systemFont(ofSize: 14, weight: .medium)
        title.textColor = .labelColor
        title.translatesAutoresizingMaskIntoConstraints = false

        detail.font = .systemFont(ofSize: 12)
        detail.textColor = .secondaryLabelColor
        detail.lineBreakMode = .byTruncatingTail
        detail.translatesAutoresizingMaskIntoConstraints = false

        toggleControl.translatesAutoresizingMaskIntoConstraints = false

        addSubview(title)
        addSubview(detail)
        addSubview(toggleControl)
        NSLayoutConstraint.activate([
            title.leadingAnchor.constraint(
                equalTo: leadingAnchor,
                constant: Metrics.horizontalInset),
            title.topAnchor.constraint(
                equalTo: topAnchor,
                constant: Metrics.titleTopInset),
            title.trailingAnchor.constraint(
                lessThanOrEqualTo: toggleControl.leadingAnchor,
                constant: -12),
            detail.leadingAnchor.constraint(equalTo: title.leadingAnchor),
            detail.topAnchor.constraint(
                equalTo: title.bottomAnchor,
                constant: 1),
            detail.trailingAnchor.constraint(
                lessThanOrEqualTo: toggleControl.leadingAnchor,
                constant: -12),
            toggleControl.trailingAnchor.constraint(
                equalTo: trailingAnchor,
                constant: -Metrics.horizontalInset),
            toggleControl.centerYAnchor.constraint(equalTo: centerYAnchor),
        ])
    }

    func update(subtitle: String,
                isOn: Bool,
                accessibilityStatus: String,
                switchEnabled: Bool,
                disabledReason: String?) {
        detail.stringValue = subtitle
        toggleModel.update(
            isOn: isOn,
            isEnabled: switchEnabled,
            accessibilityStatus: accessibilityStatus,
            accessibilityHelp: disabledReason
                ?? "Routes supported coding traffic using the saved model mappings.")
        toggleControl.synchronize()
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { nil }
}

struct GlobalRoutingTogglePresentation: Equatable {
    let isOn: Bool
    let isEnabled: Bool
    let accessibilityStatus: String
    let disabledReason: String?
}

@MainActor
func globalRoutingTogglePresentation(
    state: BasetenSwitchState,
    isPreviewFixture: Bool
) -> GlobalRoutingTogglePresentation {
    let status: String
    if state.pendingGlobalRouting != nil {
        status = "Applying"
    } else if !isPreviewFixture && !state.canMutateRouting {
        status = "Unavailable"
    } else {
        status = state.displayedGlobalRoutingEnabled ? "On" : "Off"
    }
    return GlobalRoutingTogglePresentation(
        isOn: state.displayedGlobalRoutingEnabled,
        isEnabled: isPreviewFixture
            || (state.canMutateRouting
                && state.pendingGlobalRouting == nil),
        accessibilityStatus: status,
        disabledReason: state.routingMutationDisabledReason)
}

@MainActor
final class StatusHeaderToggleModel: ObservableObject {
    @Published private(set) var isOn: Bool
    @Published private(set) var isEnabled: Bool
    @Published private(set) var accessibilityStatus: String
    @Published private(set) var accessibilityHelp: String

    private var pendingRequest: Bool?
    private let onToggle: (Bool) -> Bool

    init(isOn: Bool,
         isEnabled: Bool,
         accessibilityStatus: String,
         accessibilityHelp: String,
         onToggle: @escaping (Bool) -> Bool) {
        self.isOn = isOn
        self.isEnabled = isEnabled
        self.accessibilityStatus = accessibilityStatus
        self.accessibilityHelp = accessibilityHelp
        self.onToggle = onToggle
    }

    func update(isOn: Bool,
                isEnabled: Bool,
                accessibilityStatus: String,
                accessibilityHelp: String) {
        pendingRequest = nil
        self.isOn = isOn
        self.isEnabled = isEnabled
        self.accessibilityStatus = accessibilityStatus
        self.accessibilityHelp = accessibilityHelp
    }

    @discardableResult
    func requestChange(to enabled: Bool) -> Bool {
        guard isEnabled,
              enabled != isOn,
              pendingRequest == nil else { return false }
        pendingRequest = enabled
        guard onToggle(enabled) else {
            pendingRequest = nil
            return false
        }
        return true
    }
}

enum StatusHeaderKeyboardCommand: Equatable {
    case toggle

    static func resolve(charactersIgnoringModifiers: String?,
                        modifierFlags: NSEvent.ModifierFlags,
                        isRepeat: Bool) -> Self? {
        let blockingModifiers: NSEvent.ModifierFlags = [
            .command, .control, .option, .shift,
        ]
        guard !isRepeat,
              modifierFlags.intersection(blockingModifiers).isEmpty else {
            return nil
        }
        guard charactersIgnoringModifiers == " "
                || charactersIgnoringModifiers == "\r"
                || charactersIgnoringModifiers == "\u{3}" else {
            return nil
        }
        return .toggle
    }
}

enum StatusHeaderToggleAppearance {
    static let controlSize = NSSize(width: 52, height: 28)
    static let trackRect = NSRect(x: 2, y: 2, width: 48, height: 24)
    static let thumbDiameter: CGFloat = 20
    static let thumbInset: CGFloat = 2
    static let onThumbShadowOpacity: Float = 0.22

    static let offTrack = NSColor(
        name: nil,
        dynamicProvider: { appearance in
            let match = appearance.bestMatch(from: [.aqua, .darkAqua])
            if match == .darkAqua {
                return NSColor(
                    srgbRed: 0x62 / 255.0,
                    green: 0x67 / 255.0,
                    blue: 0x6C / 255.0,
                    alpha: 1)
            }
            return NSColor(
                srgbRed: 0xC8 / 255.0,
                green: 0xCD / 255.0,
                blue: 0xD0 / 255.0,
                alpha: 1)
        })

    static func trackColor(isOn: Bool) -> NSColor {
        isOn ? AppColors.basetenGreen : offTrack
    }

    static func thumbRect(isOn: Bool) -> NSRect {
        let x = isOn
            ? trackRect.maxX - thumbInset - thumbDiameter
            : trackRect.minX + thumbInset
        return NSRect(
            x: x,
            y: trackRect.midY - thumbDiameter / 2,
            width: thumbDiameter,
            height: thumbDiameter)
    }

    static func thumbShadowOpacity(isOn: Bool) -> Float {
        isOn ? onThumbShadowOpacity : 0
    }
}

enum StatusHeaderToggleAnimationPolicy {
    static let stateChangeDuration: TimeInterval = 0.20

    static func duration(previousState: Bool?,
                         nextState: Bool,
                         reduceMotion: Bool) -> TimeInterval? {
        guard let previousState,
              previousState != nextState,
              !reduceMotion else {
            return nil
        }
        return stateChangeDuration
    }
}

/// A small AppKit switch drawn explicitly because SwiftUI's `.tint` is not
/// honored when a switch is hosted in an NSMenu custom item. NSButton still
/// owns native press, action, focus, keyboard, and accessibility semantics;
/// only the track and thumb rendering are custom. Core Animation owns the
/// state transition so it remains smooth while NSMenu is tracking events.
@MainActor
final class StatusHeaderToggleControl: NSButton {
    private enum AnimationKey {
        static let thumbPosition = "routing-state-thumb-position"
        static let thumbShadow = "routing-state-thumb-shadow"
        static let trackColor = "routing-state-track-color"
    }

    private let model: StatusHeaderToggleModel
    private let trackLayer = CAShapeLayer()
    private let interactionLayer = CAShapeLayer()
    private let thumbLayer = CALayer()
    private var trackingArea: NSTrackingArea?
    private var synchronizedState: Bool?
    private var isHovering = false {
        didSet {
            if oldValue != isHovering {
                updateInteractionAppearance()
            }
        }
    }

    init(
        model: StatusHeaderToggleModel,
        accessibilityIdentifier: String = "global-routing-switch"
    ) {
        self.model = model
        super.init(frame: NSRect(origin: .zero,
                                 size: StatusHeaderToggleAppearance.controlSize))
        title = ""
        isBordered = false
        setButtonType(.momentaryPushIn)
        target = self
        action = #selector(activateSwitch)
        focusRingType = .exterior
        setAccessibilityIdentifier(accessibilityIdentifier)
        // AppKit exposes NSSwitch to accessibility as a check box.
        setAccessibilityRole(.checkBox)
        setAccessibilityLabel("Global routing")
        configureLayers()
        synchronize()
    }

    override var acceptsFirstResponder: Bool {
        model.isEnabled
    }

    override var canBecomeKeyView: Bool {
        model.isEnabled
    }

    override func becomeFirstResponder() -> Bool {
        guard model.isEnabled else { return false }
        return super.becomeFirstResponder()
    }

    override var intrinsicContentSize: NSSize {
        StatusHeaderToggleAppearance.controlSize
    }

    func synchronize(reduceMotion: Bool? = nil) {
        isEnabled = model.isEnabled
        toolTip = model.accessibilityHelp
        setAccessibilityValue(NSNumber(value: model.isOn))
        setAccessibilityHelp(
            "Current status: \(model.accessibilityStatus). \(model.accessibilityHelp)")

        let previousState = synchronizedState
        synchronizedState = model.isOn
        let duration = StatusHeaderToggleAnimationPolicy.duration(
            previousState: previousState,
            nextState: model.isOn,
            reduceMotion: reduceMotion
                ?? NSWorkspace.shared.accessibilityDisplayShouldReduceMotion)
        updateStateLayers(isOn: model.isOn, duration: duration)
        updateInteractionAppearance()
    }

    @objc private func activateSwitch() {
        model.requestChange(to: !model.isOn)
        // The server projection remains authoritative. Never let NSButton's
        // momentary cell state masquerade as a confirmed route change.
        state = .off
        updateInteractionAppearance()
    }

    override func keyDown(with event: NSEvent) {
        let command = StatusHeaderKeyboardCommand.resolve(
            charactersIgnoringModifiers: event.charactersIgnoringModifiers,
            modifierFlags: event.modifierFlags,
            isRepeat: event.isARepeat)
        guard command == .toggle else {
            super.keyDown(with: event)
            return
        }
        activateSwitch()
    }

    override func updateTrackingAreas() {
        if let trackingArea {
            removeTrackingArea(trackingArea)
        }
        let area = NSTrackingArea(
            rect: bounds,
            options: [.activeInActiveApp, .mouseEnteredAndExited],
            owner: self,
            userInfo: nil)
        addTrackingArea(area)
        trackingArea = area
        super.updateTrackingAreas()
    }

    override func mouseEntered(with event: NSEvent) {
        isHovering = true
    }

    override func mouseExited(with event: NSEvent) {
        isHovering = false
    }

    override func viewDidChangeEffectiveAppearance() {
        super.viewDidChangeEffectiveAppearance()
        updateStateLayers(isOn: model.isOn, duration: nil)
        updateInteractionAppearance()
    }

    override func highlight(_ flag: Bool) {
        super.highlight(flag)
        updateInteractionAppearance()
    }

    private func configureLayers() {
        wantsLayer = true
        let rootLayer = CALayer()
        rootLayer.masksToBounds = false
        layer = rootLayer

        let trackRect = StatusHeaderToggleAppearance.trackRect
        trackLayer.frame = trackRect
        trackLayer.path = CGPath(
            roundedRect: CGRect(origin: .zero, size: trackRect.size),
            cornerWidth: trackRect.height / 2,
            cornerHeight: trackRect.height / 2,
            transform: nil)
        interactionLayer.frame = trackRect
        interactionLayer.path = trackLayer.path

        thumbLayer.bounds = CGRect(
            x: 0,
            y: 0,
            width: StatusHeaderToggleAppearance.thumbDiameter,
            height: StatusHeaderToggleAppearance.thumbDiameter)
        thumbLayer.cornerRadius =
            StatusHeaderToggleAppearance.thumbDiameter / 2
        thumbLayer.backgroundColor = NSColor.white.cgColor
        thumbLayer.shadowColor = NSColor.black.cgColor
        thumbLayer.shadowOpacity = 0
        thumbLayer.shadowRadius = 1.5
        thumbLayer.shadowOffset = CGSize(width: 0, height: -0.5)

        rootLayer.addSublayer(trackLayer)
        rootLayer.addSublayer(interactionLayer)
        rootLayer.addSublayer(thumbLayer)
    }

    private func updateStateLayers(isOn: Bool, duration: TimeInterval?) {
        let thumbRect = StatusHeaderToggleAppearance.thumbRect(isOn: isOn)
        let targetPosition = CGPoint(x: thumbRect.midX, y: thumbRect.midY)
        let targetShadowOpacity =
            StatusHeaderToggleAppearance.thumbShadowOpacity(isOn: isOn)
        var targetColor = NSColor.clear.cgColor
        effectiveAppearance.performAsCurrentDrawingAppearance {
            targetColor = StatusHeaderToggleAppearance
                .trackColor(isOn: isOn)
                .cgColor
        }

        let presentedPosition =
            thumbLayer.presentation()?.position ?? thumbLayer.position
        let presentedShadowOpacity =
            thumbLayer.presentation()?.shadowOpacity
                ?? thumbLayer.shadowOpacity
        let presentedColor =
            trackLayer.presentation()?.fillColor
                ?? trackLayer.fillColor

        CATransaction.begin()
        CATransaction.setDisableActions(true)
        thumbLayer.position = targetPosition
        thumbLayer.shadowOpacity = targetShadowOpacity
        trackLayer.fillColor = targetColor
        CATransaction.commit()

        guard let duration else {
            thumbLayer.removeAnimation(forKey: AnimationKey.thumbPosition)
            thumbLayer.removeAnimation(forKey: AnimationKey.thumbShadow)
            trackLayer.removeAnimation(forKey: AnimationKey.trackColor)
            return
        }

        let timing = CAMediaTimingFunction(name: .easeInEaseOut)
        let thumbAnimation = CABasicAnimation(keyPath: "position")
        thumbAnimation.fromValue = NSValue(point: presentedPosition)
        thumbAnimation.toValue = NSValue(point: targetPosition)
        thumbAnimation.duration = duration
        thumbAnimation.timingFunction = timing

        let shadowAnimation = CABasicAnimation(keyPath: "shadowOpacity")
        shadowAnimation.fromValue = presentedShadowOpacity
        shadowAnimation.toValue = targetShadowOpacity
        shadowAnimation.duration = duration
        shadowAnimation.timingFunction = timing

        let trackAnimation = CABasicAnimation(keyPath: "fillColor")
        trackAnimation.fromValue = presentedColor
        trackAnimation.toValue = targetColor
        trackAnimation.duration = duration
        trackAnimation.timingFunction = timing

        thumbLayer.add(
            thumbAnimation,
            forKey: AnimationKey.thumbPosition)
        thumbLayer.add(
            shadowAnimation,
            forKey: AnimationKey.thumbShadow)
        trackLayer.add(
            trackAnimation,
            forKey: AnimationKey.trackColor)
    }

    private func updateInteractionAppearance() {
        let enabledAlpha: Float = model.isEnabled ? 1 : 0.5
        let hoverAlpha: CGFloat
        if model.isEnabled && (isHovering || isHighlighted) {
            hoverAlpha = isHighlighted ? 0.14 : 0.07
        } else {
            hoverAlpha = 0
        }
        let overlayColor = NSColor(
            white: model.isOn ? 1 : 0,
            alpha: hoverAlpha)

        CATransaction.begin()
        CATransaction.setDisableActions(true)
        trackLayer.opacity = enabledAlpha
        interactionLayer.fillColor = overlayColor.cgColor
        thumbLayer.opacity = model.isEnabled ? 1 : 0.72
        thumbLayer.transform = isHighlighted
            ? CATransform3DMakeScale(0.95, 0.95, 1)
            : CATransform3DIdentity
        CATransaction.commit()
    }

#if DEBUG
    var hasStateTransitionAnimationForTesting: Bool {
        thumbLayer.animation(forKey: AnimationKey.thumbPosition) != nil
            && thumbLayer.animation(forKey: AnimationKey.thumbShadow) != nil
            && trackLayer.animation(forKey: AnimationKey.trackColor) != nil
    }

    var thumbPositionForTesting: CGPoint {
        thumbLayer.position
    }

    var thumbShadowOpacityForTesting: Float {
        thumbLayer.shadowOpacity
    }

    var thumbShadowAnimationEndpointsForTesting: (Float, Float)? {
        guard let animation = thumbLayer.animation(
            forKey: AnimationKey.thumbShadow) as? CABasicAnimation,
            let from = animation.fromValue as? NSNumber,
            let to = animation.toValue as? NSNumber else {
            return nil
        }
        return (from.floatValue, to.floatValue)
    }

    var trackColorForTesting: NSColor? {
        guard let color = trackLayer.fillColor else { return nil }
        return NSColor(cgColor: color)
    }
#endif

    override var focusRingMaskBounds: NSRect {
        StatusHeaderToggleAppearance.trackRect.insetBy(dx: -2, dy: -2)
    }

    override func drawFocusRingMask() {
        let rect = StatusHeaderToggleAppearance.trackRect
            .insetBy(dx: -2, dy: -2)
        NSBezierPath(
            roundedRect: rect,
            xRadius: rect.height / 2,
            yRadius: rect.height / 2)
            .fill()
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        nil
    }
}

func serverAuthoredClientMenuTitle(_ client: ClientStatus) -> String {
    let name = clientDisplayName(client.name)
    guard client.enabled else { return "\(name): Not configured" }
    if !client.effectiveSummary.isEmpty {
        return "\(name): \(client.effectiveSummary)"
    }
    return clientMenuTitle(client)
}
