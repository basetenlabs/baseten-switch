import AppKit
import SwiftUI

enum RouterWindowToolbarItems {
    static let refresh = NSToolbarItem.Identifier(
        "co.baseten.switch.refresh")
    static let defaultIdentifiers = [refresh]
}

enum RouterWindowDestination: Hashable {
    case overview
    case traffic
    case requests
    case client(String)
}

@MainActor
final class RouterWindowNavigation: ObservableObject {
    @Published var selection: RouterWindowDestination?

    func prepareForShow(clientName: String?) {
        if let clientName {
            selection = .client(clientName)
        } else if selection == nil {
            selection = .overview
        }
    }

    func prepareForShow(destination: RouterWindowDestination) {
        selection = destination
    }
}

/// Owns one durable configuration window. Opening it promotes the LSUIElement
/// process to a regular application so it appears in the Dock and Cmd+Tab;
/// closing it returns the still-running status app to accessory mode.
@MainActor
final class RouterWindowController: NSObject, NSWindowDelegate, NSToolbarDelegate,
                                    NSToolbarItemValidation {
    private enum Metrics {
        static let defaultSize = NSSize(width: 820, height: 600)
        static let minimumSize = NSSize(width: 680, height: 480)
    }

    private let state: BasetenSwitchState
    private let variant: AppVariant
    private let isPreview: Bool
    private let windowOpenChanged: (Bool) -> Void
    private let navigation = RouterWindowNavigation()
    private let trafficStore: TrafficStore
    private let requestsStore: RequestsStore
    private let doorStore: DoorStatusStore
    private var presentationState = RouterWindowPresentationState()
    private var window: NSWindow?

    init(state: BasetenSwitchState,
         variant: AppVariant = .current(),
         isPreview: Bool = false,
         doorReader: (any DoorStatusReading)? = nil,
         windowOpenChanged: @escaping (Bool) -> Void = { _ in }) {
        self.state = state
        self.variant = variant
        self.isPreview = isPreview
        self.windowOpenChanged = windowOpenChanged
        trafficStore = TrafficStore(
            reader: TrafficAPIClient(runtime: variant.runtime))
        requestsStore = RequestsStore(
            reader: RequestsAPIClient(runtime: variant.runtime))
        doorStore = DoorStatusStore(
            urls: variant.runtime.doorURLs,
            reader: doorReader
                ?? previewDoorStatusReader(isPreviewFixture: isPreview))
        super.init()
    }

    func show(clientName: String? = nil) {
        navigation.prepareForShow(clientName: clientName)
        showPreparedWindow()
    }

    func show(destination: RouterWindowDestination) {
        navigation.prepareForShow(destination: destination)
        showPreparedWindow()
    }

    private func showPreparedWindow() {
        updateVisibleStore(for: navigation.selection)
        ensureModelCatalogLoaded(for: navigation.selection)
        if !isPreview, presentationState.transition(to: true) {
            windowOpenChanged(true)
        }
        let window = window ?? makeWindow()
        if window.isMiniaturized {
            window.deminiaturize(nil)
        }
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
    }

    func windowDidBecomeKey(_ notification: Notification) {
        guard !isPreview, presentationState.transition(to: true) else { return }
        windowOpenChanged(true)
    }

    func windowWillClose(_ notification: Notification) {
        trafficStore.stop()
        requestsStore.stop()
        doorStore.stop()
        if !isPreview {
            state.routerWindowDidClose()
        }
        guard !isPreview, presentationState.transition(to: false) else { return }
        windowOpenChanged(false)
    }

    private func makeWindow() -> NSWindow {
#if DEBUG
        let controller: NSViewController
        if let level = AXHierarchyFixtureLevel.requested {
            controller = NSHostingController(
                rootView: AXHierarchyFixtureView(level: level))
        } else {
            controller = NSHostingController(rootView: RouterConfigurationView(
                state: state,
                trafficStore: trafficStore,
                requestsStore: requestsStore,
                doorStore: doorStore,
                navigation: navigation,
                variant: variant,
                isPreview: isPreview))
        }
#else
        let root = RouterConfigurationView(
            state: state,
            trafficStore: trafficStore,
            requestsStore: requestsStore,
            doorStore: doorStore,
            navigation: navigation,
            variant: variant,
            isPreview: isPreview)
        let controller = NSHostingController(rootView: root)
#endif
        let window = NSWindow(
            contentRect: NSRect(origin: .zero, size: Metrics.defaultSize),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false)
        window.title = variant.displayName
        window.titlebarAppearsTransparent = false
        window.toolbarStyle = .unified
        window.toolbar = makeToolbar()
        installRouterWindowContentController(controller, in: window)
        window.setContentSize(Metrics.defaultSize)
        window.minSize = Metrics.minimumSize
        window.isReleasedWhenClosed = false
        window.tabbingMode = .disallowed
        window.collectionBehavior = [.managed, .participatesInCycle]
        if isPreview {
            window.center()
        } else {
            let frameName = variant.windowFrameAutosaveName
            if !window.setFrameUsingName(frameName) {
                window.center()
            }
            window.setFrameAutosaveName(frameName)
        }
        window.delegate = self
        self.window = window
        return window
    }

    private func makeToolbar() -> NSToolbar {
        let toolbar = NSToolbar(
            identifier: NSToolbar.Identifier(
                "co.baseten.switch.router-window"))
        toolbar.delegate = self
        toolbar.displayMode = .iconOnly
        toolbar.allowsUserCustomization = false
        toolbar.autosavesConfiguration = false
        return toolbar
    }

    nonisolated func toolbarAllowedItemIdentifiers(_ toolbar: NSToolbar)
        -> [NSToolbarItem.Identifier] {
        RouterWindowToolbarItems.defaultIdentifiers
    }

    nonisolated func toolbarDefaultItemIdentifiers(_ toolbar: NSToolbar)
        -> [NSToolbarItem.Identifier] {
        RouterWindowToolbarItems.defaultIdentifiers
    }

    nonisolated func toolbar(
        _ toolbar: NSToolbar,
        itemForItemIdentifier itemIdentifier: NSToolbarItem.Identifier,
        willBeInsertedIntoToolbar flag: Bool
    ) -> NSToolbarItem? {
        MainActor.assumeIsolated {
            let item = NSToolbarItem(itemIdentifier: itemIdentifier)
            switch itemIdentifier {
            case RouterWindowToolbarItems.refresh:
                item.label = "Refresh"
                item.paletteLabel = "Refresh"
                item.toolTip = "Refresh router status"
                item.image = NSImage(
                    systemSymbolName: "arrow.clockwise",
                    accessibilityDescription: "Refresh")
                item.target = self
                item.action = #selector(refreshRouterStatus(_:))
            default:
                return nil
            }
            return item
        }
    }

    func validateToolbarItem(_ item: NSToolbarItem) -> Bool {
        switch item.itemIdentifier {
        case RouterWindowToolbarItems.refresh:
            return !isPreview
        default:
            return false
        }
    }

    @objc private func refreshRouterStatus(_ sender: Any?) {
        guard !isPreview else { return }
        if toolbarRefreshesTraffic(selection: navigation.selection) {
            trafficStore.requestRefresh()
        } else if toolbarRefreshesRequests(selection: navigation.selection) {
            requestsStore.refresh()
        } else if toolbarRefreshesModelCatalog(
            selection: navigation.selection
        ) {
            state.requestClientPageRefresh()
            if navigation.selection == .client("claude-code") {
                state.requestClaudeModelPickerDiagnosticsRefresh()
            }
        } else {
            state.requestInteractiveRefresh(includeStats: false)
            if navigation.selection == .overview {
                doorStore.requestRefresh()
            }
        }
    }

    private func updateVisibleStore(
        for selection: RouterWindowDestination?
    ) {
        if selection == .traffic {
            doorStore.stop()
            requestsStore.stop()
            trafficStore.start()
        } else if selection == .requests {
            doorStore.stop()
            trafficStore.stop()
            requestsStore.start()
        } else if selection == .overview || selection == nil {
            trafficStore.stop()
            requestsStore.stop()
            doorStore.start()
        } else {
            trafficStore.stop()
            requestsStore.stop()
            doorStore.stop()
        }
    }

    private func ensureModelCatalogLoaded(
        for selection: RouterWindowDestination?
    ) {
        guard !isPreview,
              toolbarRefreshesModelCatalog(selection: selection) else {
            return
        }
        state.ensureModelCatalogLoaded()
        if selection == .client("claude-code") {
            state.ensureClaudeModelPickerDiagnosticsLoaded()
        }
    }
}

func toolbarRefreshesTraffic(
    selection: RouterWindowDestination?
) -> Bool {
    selection == .traffic
}

func toolbarRefreshesRequests(
    selection: RouterWindowDestination?
) -> Bool {
    selection == .requests
}

func toolbarRefreshesModelCatalog(
    selection: RouterWindowDestination?
) -> Bool {
    guard let selection else { return false }
    if case .client = selection {
        return true
    }
    return false
}

func livePathConfiguredRoute(
    client: ClientStatus,
    globalRoutingEnabled: Bool
) -> String {
    guard client.enabled else { return "Disabled" }
    guard globalRoutingEnabled else {
        let native = client.nativeRoute.isEmpty
            ? "Native provider"
            : "Native · \(client.nativeRoute.capitalized)"
        return native
    }

    let targets = client.families.compactMap { family -> String? in
        guard let target = family.configuredTarget, !target.isEmpty else {
            return nil
        }
        return target
    }
    let canonicalTargets = Set(targets.map { target in
        catalogModelEntry(target, catalog: client.modelCatalog)?.slug
            ?? target
    })
    if canonicalTargets == ["native"] {
        return "Native provider"
    }
    if canonicalTargets.count == 1, let target = targets.first {
        return "Model mappings · \(catalogModelDisplayLabel(target, catalog: client.modelCatalog))"
    }
    if canonicalTargets.count > 1 {
        return "Per-model mappings"
    }
    return "Default routing rules"
}

func livePathGatewayHealth(_ snapshot: RoutingSnapshot?) -> String {
    guard let health = snapshot?.health, !health.isEmpty else {
        return "Running"
    }
    return health.capitalized
}

func livePathEffectiveRoute(_ client: ClientStatus) -> String {
    if !client.effectiveSummary.isEmpty {
        guard client.effectiveRoute == "baseten",
              let effectiveModel = client.unmatchedNativeModel?.effectiveModel,
              !effectiveModel.isEmpty,
              catalogModelEntry(
                effectiveModel,
                catalog: client.modelCatalog) != nil else {
            return client.effectiveSummary
        }
        return "Baseten · \(catalogModelDisplayLabel(effectiveModel, catalog: client.modelCatalog))"
    }
    if !client.effectiveRoute.isEmpty {
        return client.effectiveRoute.capitalized
    }
    return "Unavailable"
}

func livePathModel(_ client: ClientStatus) -> String {
    if let effectiveModel = client.unmatchedNativeModel?.effectiveModel,
       !effectiveModel.isEmpty {
        return catalogModelDisplayLabel(
            effectiveModel,
            catalog: client.modelCatalog)
    }
    if client.effectiveRoute == client.nativeRoute,
       !client.nativeRoute.isEmpty {
        return "\(client.nativeRoute.capitalized) selects the model"
    }
    return "No model reported"
}

func livePathFallback(_ client: ClientStatus) -> String {
    if client.fallbackActive {
        let route = client.fallback.servedRoute.isEmpty
            ? "native provider"
            : client.fallback.servedRoute
        if client.fallback.cause.isEmpty {
            return "Active · \(route)"
        }
        return "Active · \(route) · \(client.fallback.cause)"
    }
    if client.nativeRoute.isEmpty {
        return "Inactive"
    }
    return "Ready · \(client.nativeRoute)"
}

/// SwiftUI's unified macOS toolbar gives its root hosting view a full-height
/// surface. Constrain the application content to AppKit's non-obscured layout
/// guide so scrolling and rubber-band bounce can never paint beneath the
/// titlebar or toolbar.
@MainActor
func installRouterWindowContentController(
    _ hostedController: NSViewController,
    in window: NSWindow
) {
    let container = NSViewController()
    container.view = NSView()
    container.addChild(hostedController)
    window.contentViewController = container

    let hostedView = hostedController.view
    hostedView.translatesAutoresizingMaskIntoConstraints = false
    container.view.addSubview(hostedView)

    guard let contentLayoutGuide =
        window.contentLayoutGuide as? NSLayoutGuide else {
        assertionFailure("NSWindow did not provide a content layout guide")
        return
    }
    NSLayoutConstraint.activate([
        hostedView.leadingAnchor.constraint(
            equalTo: contentLayoutGuide.leadingAnchor),
        hostedView.trailingAnchor.constraint(
            equalTo: contentLayoutGuide.trailingAnchor),
        hostedView.topAnchor.constraint(
            equalTo: contentLayoutGuide.topAnchor),
        hostedView.bottomAnchor.constraint(
            equalTo: contentLayoutGuide.bottomAnchor),
    ])
}

struct RouterWindowPresentationState {
    private(set) var isOpen = false

    mutating func transition(to open: Bool) -> Bool {
        guard open != isOpen else { return false }
        isOpen = open
        return true
    }
}

struct WindowGlobalRoutingPresentation: Equatable {
    let status: String
    let overviewTitle: String
    let overviewDescription: String
    let clientDescription: String
}

func windowGlobalRoutingPresentation(
    enabled: Bool,
    phase: MutationPhase?
) -> WindowGlobalRoutingPresentation {
    let stateLabel = enabled ? "On" : "Off"
    if let phase {
        let phaseLabel = phase == .applying ? "Applying" : "Reconciling"
        let pendingDescription =
            "\(phaseLabel) the request to turn routing \(stateLabel). Waiting for gateway confirmation. Saved mappings remain editable."
        return WindowGlobalRoutingPresentation(
            status: "\(phaseLabel) · \(stateLabel)",
            overviewTitle: "\(phaseLabel) routing \(stateLabel)…",
            overviewDescription: pendingDescription,
            clientDescription: pendingDescription)
    }

    return WindowGlobalRoutingPresentation(
        status: stateLabel,
        overviewTitle: enabled
            ? "Routing rules are active"
            : "Native providers only",
        overviewDescription: enabled
            ? "Supported requests use the saved per-model mappings. Use the menu bar to turn routing Off."
            : "All supported requests use their native providers. Saved mappings remain editable. Use the menu bar to turn routing On.",
        clientDescription: enabled
            ? "Global routing is On. Claude Code requests use the model mappings below."
            : "Global routing is Off. Native Claude models use Anthropic. Saved mappings remain editable and apply next time routing is On.")
}

func pendingGlobalRoutingStatus(
    enabled: Bool,
    phase: MutationPhase
) -> String {
    let phaseLabel = phase == .applying ? "Applying" : "Reconciling"
    return "\(phaseLabel) routing \(enabled ? "On" : "Off") · waiting for gateway confirmation"
}

struct ClientPagePresentation: Equatable {
    let headerDescription: String
    let activationCommand: String?
    let showsModelRouting: Bool
    let showsReasoning: Bool
    let showsSubagents: Bool
}

func clientPagePresentation(
    clientName: String,
    clientEnabled: Bool,
    globalRoutingEnabled: Bool,
    globalMutationPhase: MutationPhase?
) -> ClientPagePresentation {
    let displayName = clientDisplayName(clientName)
    if !clientEnabled {
        let command: String?
        switch clientName {
        case "claude-code":
            command = "baseten-switch claude on"
        case "codex":
            command = "baseten-switch codex on"
        default:
            command = nil
        }
        return ClientPagePresentation(
            headerDescription: "\(displayName) is not configured.",
            activationCommand: command,
            showsModelRouting: false,
            showsReasoning: false,
            showsSubagents: false)
    }

    let showsModelRouting =
        clientName == "claude-code" || clientName == "codex"
    let showsClaudeControls = clientName == "claude-code"
    let showsReasoning = showsModelRouting
    if let globalMutationPhase {
        let phaseLabel = globalMutationPhase == .applying
            ? "Applying"
            : "Reconciling"
        return ClientPagePresentation(
            headerDescription:
                "\(phaseLabel) the request to turn routing \(globalRoutingEnabled ? "On" : "Off"). Waiting for gateway confirmation. Saved settings remain visible.",
            activationCommand: nil,
            showsModelRouting: showsModelRouting,
            showsReasoning: showsReasoning,
            showsSubagents: showsClaudeControls)
    }

    switch clientName {
    case "claude-code":
        return ClientPagePresentation(
            headerDescription: globalRoutingEnabled
                ? "Global routing is On. Claude Code requests use the model mappings below."
                : "Global routing is Off. Native Claude models use Anthropic. Saved mappings remain editable and apply next time routing is On.",
            activationCommand: nil,
            showsModelRouting: true,
            showsReasoning: true,
            showsSubagents: true)
    case "codex":
        return ClientPagePresentation(
            headerDescription: globalRoutingEnabled
                ? "Global routing is On. Codex requests use the configured Baseten route. Reasoning settings below apply to the selected model."
                : "Global routing is Off. The Baseten profile requires routing On; start Codex without the profile to use OpenAI.",
            activationCommand: nil,
            showsModelRouting: true,
            showsReasoning: true,
            showsSubagents: false)
    default:
        return ClientPagePresentation(
            headerDescription:
                "This client has no dedicated configuration controls.",
            activationCommand: nil,
            showsModelRouting: false,
            showsReasoning: false,
            showsSubagents: false)
    }
}

private struct RouterConfigurationView: View {
    @ObservedObject var state: BasetenSwitchState
    @ObservedObject var trafficStore: TrafficStore
    @ObservedObject var requestsStore: RequestsStore
    @ObservedObject var doorStore: DoorStatusStore
    @ObservedObject var navigation: RouterWindowNavigation
    let variant: AppVariant
    let isPreview: Bool

    var body: some View {
        NavigationSplitView {
            List(selection: $navigation.selection) {
                Label("Overview", systemImage: "switch.2")
                    .tag(RouterWindowDestination.overview)
                    .accessibilityIdentifier("sidebar-overview")

                Label("Traffic", systemImage: "chart.bar.xaxis")
                    .tag(RouterWindowDestination.traffic)
                    .accessibilityIdentifier("sidebar-traffic")

                Label("Requests", systemImage: "list.bullet.rectangle")
                    .tag(RouterWindowDestination.requests)
                    .accessibilityIdentifier("sidebar-requests")

                Section("Clients") {
                    ForEach(state.clients) { client in
                        Label {
                            Text(clientDisplayName(client.name))
                        } icon: {
                            ClientBrandMark(clientName: client.name)
                        }
                            .tag(RouterWindowDestination.client(client.name))
                            .accessibilityIdentifier(
                                "sidebar-client-\(client.name)")
                    }
                }
            }
            .listStyle(.sidebar)
            .navigationSplitViewColumnWidth(
                min: 180,
                ideal: 200,
                max: 240)
        } detail: {
            detail
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(minWidth: 680, minHeight: 480)
        .onAppear {
            if navigation.selection == nil {
                navigation.selection = .overview
            }
            updateVisibleStore(for: navigation.selection)
        }
        .onChange(of: state.clients.map(\.name)) { names in
            reconcileSelection(with: names)
        }
        .onChange(of: navigation.selection) { selection in
            updateVisibleStore(for: selection)
        }
        .onDisappear {
            trafficStore.stop()
            requestsStore.stop()
            doorStore.stop()
        }
    }

    @ViewBuilder
    private var detail: some View {
        switch navigation.selection {
        case .traffic:
            TrafficView(store: trafficStore)
        case .requests:
            RequestsView(store: requestsStore)
        case .client(let name):
            if let client = state.clients.first(where: { $0.name == name }) {
                ClientRoutingView(
                    state: state,
                    client: client,
                    isPreview: isPreview)
            } else {
                Text("This routing client is no longer available.")
                    .foregroundStyle(.secondary)
            }
        case .overview, .none:
            RoutingOverviewView(
                state: state,
                doorStore: doorStore,
                variant: variant,
                isPreview: isPreview)
        }
    }

    private func reconcileSelection(with names: [String]) {
        if case .client(let selected) = navigation.selection,
           !names.contains(selected) {
            navigation.selection = .overview
        }
    }

    private func updateVisibleStore(
        for selection: RouterWindowDestination?
    ) {
        if selection == .traffic {
            doorStore.stop()
            requestsStore.stop()
            trafficStore.start()
        } else if selection == .requests {
            doorStore.stop()
            trafficStore.stop()
            requestsStore.start()
        } else if selection == .overview || selection == nil {
            trafficStore.stop()
            requestsStore.stop()
            doorStore.start()
        } else {
            trafficStore.stop()
            requestsStore.stop()
            doorStore.stop()
        }
        guard !isPreview,
              toolbarRefreshesModelCatalog(selection: selection) else {
            return
        }
        state.ensureModelCatalogLoaded()
        if selection == .client("claude-code") {
            state.ensureClaudeModelPickerDiagnosticsLoaded()
        }
    }
}

private struct RoutingOverviewView: View {
    @ObservedObject var state: BasetenSwitchState
    @ObservedObject var doorStore: DoorStatusStore
    let variant: AppVariant
    let isPreview: Bool

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                Label("Overview", systemImage: "switch.2")
                    .font(.title2.weight(.semibold))

                liveRequestPath

                if state.routingSnapshot != nil {
                    routingGroup
                    statusGroup
                } else {
                    stopped
                }

                if let error = state.lastError, !error.isEmpty {
                    warningBanner(
                        menuErrorLabel(error, limit: 180),
                        symbol: "exclamationmark.triangle.fill",
                        color: .red)
                }
                if let message = state.mutationRecoveryMessage {
                    mutationRecoveryWarningBanner(
                        message,
                        state: state)
                }
            }
            .padding(28)
            .frame(maxWidth: 680, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .topLeading)
        }
        .accessibilityIdentifier("routing-overview")
    }

    private var liveRequestPath: some View {
        RoutingSectionCard {
            HStack {
                Label("Live Request Path", systemImage: "point.3.connected.trianglepath.dotted")
                    .font(.headline)
                Spacer()
                if doorStore.isRefreshing {
                    ProgressView()
                        .controlSize(.small)
                        .accessibilityLabel("Refreshing door status")
                }
            }
        } content: {
            VStack(alignment: .leading, spacing: 10) {
                livePathRow(
                    label: "Gateway",
                    value: state.gatewayUp
                        ? "\(livePathGatewayHealth(state.routingSnapshot)) · \(formatUptime(state.uptimeSeconds))"
                        : "Unavailable",
                    symbol: state.gatewayUp
                        ? "checkmark.circle.fill"
                        : "xmark.circle.fill",
                    color: state.gatewayUp
                        ? Color(nsColor: AppColors.basetenGreen)
                        : .red)

                if state.snapshotIsStale {
                    Label(
                        "Gateway details are from the last confirmed status.",
                        systemImage: "clock.arrow.circlepath")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }

                if state.clients.isEmpty {
                    livePathRow(
                        label: "Client",
                        value: "Status unavailable",
                        symbol: "questionmark.circle",
                        color: .secondary)
                } else {
                    ForEach(state.clients) { client in
                        Divider()
                        clientPath(client)
                    }
                }

                Divider()
                if doorStore.probes.isEmpty {
                    livePathRow(
                        label: "Door",
                        value: doorStore.isRefreshing
                            ? "Checking…"
                            : "Status unavailable",
                        symbol: "questionmark.circle",
                        color: .secondary)
                } else {
                    ForEach(doorStore.probes) { probe in
                        doorPath(probe)
                    }
                }
            }
            .padding(6)
        }
        .accessibilityIdentifier("overview-live-request-path")
    }

    @ViewBuilder
    private func clientPath(_ client: ClientStatus) -> some View {
        livePathRow(
            label: clientDisplayName(client.name),
            value: client.enabled
                ? (client.currentlyBound
                    ? "Enabled · Bound at \(client.bindAddr)"
                    : "Enabled · Not bound")
                : "Disabled",
            symbol: client.enabled && client.currentlyBound
                ? "checkmark.circle.fill"
                : "minus.circle",
            color: client.enabled && client.currentlyBound
                ? Color(nsColor: AppColors.basetenGreen)
                : .secondary)
        livePathValueRow(
            label: "Configured",
            value: livePathConfiguredRoute(
                client: client,
                globalRoutingEnabled: state.confirmedGlobalRoutingEnabled))
        livePathValueRow(
            label: "Effective",
            value: livePathEffectiveRoute(client))
        livePathValueRow(
            label: "Model",
            value: livePathModel(client))
        livePathValueRow(
            label: "Router fallback",
            value: livePathFallback(client),
            valueColor: client.fallbackActive ? .orange : .secondary)
    }

    @ViewBuilder
    private func doorPath(_ probe: DoorProbeSnapshot) -> some View {
        let status = probe.status
        livePathRow(
            label: "Door \(probe.url.port.map(String.init) ?? "")",
            value: doorStateLabel(probe),
            symbol: !probe.isReachable
                ? "xmark.circle.fill"
                : (status?.tripped == true
                    ? "arrow.triangle.branch"
                    : "checkmark.circle.fill"),
            color: !probe.isReachable
                ? .red
                : (status?.tripped == true
                    ? .orange
                    : Color(nsColor: AppColors.basetenGreen)))
        if let status {
            livePathValueRow(
                label: "Cooldown",
                value: doorCooldownLabel(
                    milliseconds: status.cooldownRemainingMilliseconds))
            livePathValueRow(
                label: "Last transition",
                value: doorTransitionLabel(unix: status.lastTransitionUnix))
        } else if let message = probe.errorMessage {
            livePathValueRow(
                label: "Probe",
                value: message,
                valueColor: .red)
        }
    }

    private func livePathRow(
        label: String,
        value: String,
        symbol: String,
        color: Color
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Label(label, systemImage: symbol)
                .foregroundStyle(color)
            Spacer(minLength: 12)
            Text(value)
                .multilineTextAlignment(.trailing)
        }
        .font(.callout)
        .accessibilityElement(children: .combine)
    }

    private func livePathValueRow(
        label: String,
        value: String,
        valueColor: Color = .secondary
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label)
                .foregroundStyle(.secondary)
            Spacer(minLength: 12)
            Text(value)
                .foregroundStyle(valueColor)
                .multilineTextAlignment(.trailing)
        }
        .font(.caption)
        .padding(.leading, 24)
        .accessibilityElement(children: .combine)
    }

    private var routingGroup: some View {
        RoutingSectionCard {
            Label("Global Routing", systemImage: "arrow.triangle.branch")
                .font(.headline)
        } content: {
            VStack(alignment: .leading, spacing: 4) {
                Text(globalTitle)
                    .font(.body.weight(.medium))
                Text(globalDescription)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .padding(6)
        }
        .accessibilityIdentifier("overview-global-routing")
    }

    private var statusGroup: some View {
        RoutingSectionCard {
            Label("System Status", systemImage: "info.circle")
                .font(.headline)
        } content: {
            VStack(alignment: .leading, spacing: 8) {
                statusRow(
                    label: "Gateway",
                    value: state.gatewayUp
                        ? "Running · \(formatUptime(state.uptimeSeconds))"
                        : "Unavailable")
                statusRow(
                    label: "Routing",
                    value: globalPresentation.status)
                if state.snapshotIsStale {
                    Label(
                        "Showing the last confirmed routing state.",
                        systemImage: "wifi.exclamationmark")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
                if state.hasFallback {
                    Label(
                        "Native fallback is serving at least one client.",
                        systemImage: "arrow.triangle.branch")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
            }
            .padding(6)
        }
    }

    private var stopped: some View {
        VStack(spacing: 14) {
            Image(systemName: "power")
                .font(.system(size: 34))
                .foregroundStyle(.secondary)
            Text("\(variant.displayName) Is Stopped")
                .font(.title2.weight(.semibold))
            Text("Start the local gateway to inspect and configure routing.")
                .foregroundStyle(.secondary)
            Button(state.starting ? "Starting…" : "Start \(variant.displayName)") {
                guard !isPreview else { return }
                Task { await state.startSystem() }
            }
            .buttonStyle(.borderedProminent)
            .disabled(state.starting || !state.canStartSystem)
            .accessibilityIdentifier("start-baseten-switch")
        }
        .frame(maxWidth: .infinity, minHeight: 320)
    }

    private var globalTitle: String {
        globalPresentation.overviewTitle
    }

    private var globalDescription: String {
        globalPresentation.overviewDescription
    }

    private var globalPresentation: WindowGlobalRoutingPresentation {
        windowGlobalRoutingPresentation(
            enabled: state.displayedGlobalRoutingEnabled,
            phase: state.globalMutationPhase)
    }

    private func statusRow(label: String, value: String) -> some View {
        HStack {
            Text(label)
                .foregroundStyle(.secondary)
            Spacer()
            Text(value)
        }
    }
}

private struct ClientRoutingView: View {
    @ObservedObject var state: BasetenSwitchState
    let client: ClientStatus
    let isPreview: Bool

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                header
                if let command = presentation.activationCommand {
                    disabledClientCallout(command: command)
                } else {
                    warnings
                    if presentation.showsModelRouting {
                        modelRoutingSection
                    }
                    if client.name == "claude-code" {
                        ClaudeModelPickerSectionView(
                            state: state,
                            client: client,
                            isPreview: isPreview)
                    }
                    if presentation.showsReasoning {
                        reasoningSection
                    }
                    if presentation.showsSubagents {
                        subagentSection
                    }
                }
            }
            .padding(28)
            .frame(maxWidth: 720, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .topLeading)
        }
        .accessibilityIdentifier("client-routing-\(client.name)")
    }

    private var presentation: ClientPagePresentation {
        clientPagePresentation(
            clientName: client.name,
            clientEnabled: client.enabled,
            globalRoutingEnabled: state.displayedGlobalRoutingEnabled,
            globalMutationPhase: state.globalMutationPhase)
    }

    private var header: some View {
        HStack(alignment: .center, spacing: 12) {
            ClientBrandMark(clientName: client.name, size: 24)
                .frame(width: 38, height: 38)
                .background(
                    Color.secondary.opacity(0.1),
                    in: RoundedRectangle(cornerRadius: 9))
            VStack(alignment: .leading, spacing: 3) {
                Text(clientDisplayName(client.name))
                    .font(.title2.weight(.semibold))
                Text(clientHeaderDescription)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer()
        }
    }

    private func disabledClientCallout(command: String) -> some View {
        warningBanner(
            "\(clientDisplayName(client.name)) is disabled. Run \(command) to configure and enable it.",
            symbol: "pause.circle.fill",
            color: .secondary)
            .accessibilityIdentifier("client-disabled-callout")
    }

    @ViewBuilder
    private var warnings: some View {
        if let error = state.lastError {
            warningBanner(
                menuErrorLabel(error, limit: 160),
                symbol: "exclamationmark.triangle.fill",
                color: .red)
        }
        if let message = state.mutationRecoveryMessage {
            mutationRecoveryWarningBanner(
                message,
                state: state)
        }
        let attention = authAttention(
            gatewayUp: state.gatewayUp,
            globalRoutingEnabled: state.confirmedGlobalRoutingEnabled,
            clients: state.clients,
            auth: state.auth)
        if let message = authAttentionWarningMessage(attention) {
            warningBanner(
                message,
                symbol: "key.fill",
                color: .orange)
        }
        if client.fallbackActive {
            warningBanner(
                "Requests are currently using the native fallback provider.",
                symbol: "arrow.triangle.branch",
                color: .orange)
        }
        if state.snapshotIsStale {
            warningBanner(
                "The gateway is unavailable. Showing last confirmed settings.",
                symbol: "wifi.exclamationmark",
                color: .orange)
        }
    }

    private var modelRoutingSection: some View {
        return RoutingSectionCard {
            Label("Model Routing", systemImage: "cube")
                .font(.headline)
        } content: {
            VStack(spacing: 0) {
                if !isPreview {
                    modelCatalogStatus
                }
                if client.name == "codex" {
                    codexRouteRow
                } else {
                    let families = familyEntriesForDisplay(client)
                    ForEach(
                        Array(families.enumerated()),
                        id: \.element.family
                    ) { index, family in
                        familyRow(family)
                        if index < families.count - 1 {
                            Divider()
                        }
                    }
                }
            }
        }
    }

    @ViewBuilder
    private var reasoningSection: some View {
        let rows = reasoningRowsForDisplay(
            client: client,
            liveModels: liveReasoningModels)
        if !rows.isEmpty {
            RoutingSectionCard {
                Label("Reasoning", systemImage: "brain.head.profile")
                    .font(.headline)
            } content: {
                VStack(alignment: .leading, spacing: 10) {
                Text(
                    "These settings apply when \(clientDisplayName(client.name)) uses the selected Baseten model.")
                        .font(.caption)
                        .foregroundStyle(.secondary)

                    reasoningCatalogStateMessage

                    if rows.contains(where: { $0.capability?.stale == true }) {
                        warningBanner(
                            "Reasoning metadata is from a validated local cache. Saved options remain available while Baseten Switch refreshes the catalog.",
                            symbol: "clock.arrow.circlepath",
                            color: .orange)
                    }

                    ForEach(Array(rows.enumerated()), id: \.element.id) {
                        index, row in
                        reasoningRow(row)
                        if index < rows.count - 1 {
                            Divider()
                        }
                    }
                }
            }
            .accessibilityIdentifier("reasoning-configuration")
        }
    }

    @ViewBuilder
    private func reasoningRow(_ row: ReasoningDisplayRow) -> some View {
        let defaultPassthroughReadOnly =
            reasoningUsesDefaultPassthroughReadOnlyState(row.status)
        let choices = reasoningChoices(status: row.status)
        let pending = state.isReasoningMutationPending(
            client: client.name,
            provider: row.provider,
            model: row.model)
        let selection = reasoningSelection(
            status: row.status,
            pending: state.pendingReasoningPolicy(
                client: client.name,
                provider: row.provider,
                model: row.model))
        HStack(alignment: .center, spacing: 14) {
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 6) {
                    Text(row.displayName)
                        .font(.body.weight(.medium))
                        .lineLimit(1)
                        .fixedSize(horizontal: true, vertical: false)
                    if row.mappingFamilies.count > 1 {
                        Text("\(row.mappingFamilies.count) mappings")
                            .font(.caption2.weight(.medium))
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                            .fixedSize(horizontal: true, vertical: false)
                            .padding(.horizontal, 6)
                            .padding(.vertical, 2)
                            .background(
                                Color.secondary.opacity(0.1),
                                in: Capsule())
                    }
                }
                Text(reasoningCaption(row: row, clientName: client.name))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                if let warning = state.reasoningWarning(
                    client: client.name,
                    provider: row.provider,
                    model: row.model),
                   !warning.isEmpty {
                    Text(warning)
                        .font(.caption)
                        .foregroundStyle(.orange)
                }
                if reasoningShowsUnavailableWarning(row.status) {
                    Text(reasoningUnavailableMessage(row.status))
                        .font(.caption)
                        .foregroundStyle(
                            row.status.unavailableReason
                                == "configured_effort_unavailable"
                                ? Color.red
                                : Color.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            Spacer(minLength: 12)
            if pending {
                ProgressView()
                    .controlSize(.small)
                    .accessibilityLabel(
                        "Saving \(row.displayName) reasoning")
            }
            if defaultPassthroughReadOnly {
                Label(
                    reasoningDefaultPassthroughReadOnlyLabel(),
                    systemImage: "info.circle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            } else if choices.isEmpty {
                Label(
                    "No configurable reasoning control",
                    systemImage: "info.circle")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                VStack(alignment: .trailing, spacing: 6) {
                    if selection == .pendingDefault {
                        Text("Resetting to Safe Default…")
                            .font(.caption.weight(.medium))
                            .foregroundStyle(.secondary)
                    } else if case .unavailable(let value) = selection {
                        Text(
                            "Saved: \(reasoningEffortLabel(value)) (Unavailable)")
                            .font(.caption.weight(.medium))
                            .foregroundStyle(.red)
                    }
                    Picker(
                        "\(row.displayName) reasoning",
                        selection: reasoningBinding(row)
                    ) {
                        ForEach(choices, id: \.self) { choice in
                            Text(reasoningChoiceLabel(
                                choice,
                                clientName: client.name))
                                .tag(choice)
                        }
                    }
                    .labelsHidden()
                    .pickerStyle(.segmented)
                    .fixedSize()
                    .disabled(
                        isPreview
                            || !state.canMutateReasoning
                            || !reasoningCatalogAllowsMutation
                            || state.pendingReasoning != nil)
                    .accessibilityIdentifier(
                        "reasoning-\(row.provider)-\(row.model)")
                    if reasoningShowsResetAction(row.status) {
                        Button("Reset to Safe Default") {
                            guard !isPreview else { return }
                            state.requestReasoning(
                                client: client.name,
                                provider: row.provider,
                                model: row.model,
                                policy: ReasoningPolicyValue(
                                    mode: .default))
                        }
                        .buttonStyle(.link)
                        .font(.caption)
                        .disabled(
                            isPreview
                                || !state.canMutateReasoning
                                || !reasoningCatalogAllowsMutation
                                || state.pendingReasoning != nil)
                        .accessibilityIdentifier(
                            "reasoning-reset-\(row.provider)-\(row.model)")
                    }
                }
            }
        }
        .padding(.vertical, 8)
        .padding(.horizontal, 4)
        .accessibilityElement(children: .contain)
    }

    private func reasoningBinding(
        _ row: ReasoningDisplayRow
    ) -> Binding<ReasoningChoice> {
        Binding(
            get: {
                reasoningSelection(
                    status: row.status,
                    pending: state.pendingReasoningPolicy(
                        client: client.name,
                        provider: row.provider,
                        model: row.model))
            },
            set: { choice in
                guard !isPreview,
                      let policy = choice.policy,
                      choice != reasoningSelection(
                        status: row.status) else {
                    return
                }
                state.requestReasoning(
                    client: client.name,
                    provider: row.provider,
                    model: row.model,
                    policy: policy)
            })
    }

    private var liveReasoningModels: [LiveModelCatalogEntry] {
        guard case .ready(let models) = state.liveModelCatalogState else {
            return []
        }
        return models
    }

    @ViewBuilder
    private var reasoningCatalogStateMessage: some View {
        switch state.liveModelCatalogState {
        case .ready:
            EmptyView()
        case .idle, .loading:
            warningBanner(
                "Loading reasoning options. Saved settings remain visible while editing is unavailable.",
                symbol: "arrow.clockwise",
                color: .secondary)
        case .signedOut(let reason):
            warningBanner(
                "\(liveModelCatalogSignedOutMessage(reason)) Saved reasoning settings remain visible.",
                symbol: "key.fill",
                color: .orange)
        case .error:
            warningBanner(
                "Reasoning options are unavailable. Saved settings are preserved. Refresh after the model catalog is available.",
                symbol: "exclamationmark.triangle.fill",
                color: .orange)
        }
    }

    private var reasoningCatalogAllowsMutation: Bool {
        if case .ready = state.liveModelCatalogState {
            return true
        }
        return false
    }

    private func familyRow(_ family: FamilyEntry) -> some View {
        let projection = state.modelCatalogProjection(for: client)
        let catalog = projection.selectable
        let pendingTarget = state.pendingFamilyTarget(
            client: client.name,
            family: family.family)
        let pending = state.isFamilyMutationPending(
            client: client.name,
            family: family.family)
        let selection = familyPickerSelection(
            family,
            catalog: catalog,
            pendingTarget: pendingTarget)

        return HStack(alignment: .center, spacing: 14) {
            VStack(alignment: .leading, spacing: 3) {
                Text(capitalizeFamily(family.family))
                Text(familyConfiguredLabel(
                    family,
                    catalog: client.modelCatalog))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(familyEffectiveStatus(
                    family,
                    globalRoutingEnabled:
                        state.displayedGlobalRoutingEnabled,
                    globalMutationPhase: state.globalMutationPhase,
                    catalog: client.modelCatalog))
                    .font(.caption)
                    .foregroundStyle(
                        state.globalMutationPhase == nil
                            && state.displayedGlobalRoutingEnabled
                            ? Color.secondary
                            : Color.orange)
            }
            Spacer()
            if pending {
                ProgressView()
                    .controlSize(.small)
                    .accessibilityLabel(
                        "Saving \(capitalizeFamily(family.family)) model mapping")
            }
            Picker(
                "\(capitalizeFamily(family.family)) model mapping",
                selection: familyBinding(family)
            ) {
                Text(defaultFamilyOptionLabel(family, client: client))
                    .tag(FamilyPickerSelection.defaultTarget)
                Text("Native Provider (Anthropic)")
                    .tag(FamilyPickerSelection.native)
                Divider()
                if !projection.selectable.isEmpty {
                    Section("Baseten") {
                        ForEach(
                            projection.selectable,
                            id: \.target
                        ) { model in
                            Text(modelDisplayLabel(model))
                                .tag(FamilyPickerSelection.catalog(model.target))
                        }
                    }
                }
                if case .custom(let target) = selection {
                    Text("Unavailable · \(catalogModelDisplayLabel(target, catalog: client.modelCatalog))")
                        .tag(FamilyPickerSelection.custom(target))
                }
            }
            .labelsHidden()
            .pickerStyle(.menu)
            .frame(width: 230, alignment: .trailing)
            .disabled(
                !canEdit || pending || !state.modelCatalogAllowsMutation)
            .help(controlHelp)
            .accessibilityLabel(
                "\(capitalizeFamily(family.family)) model mapping")
            .accessibilityValue(familyConfiguredLabel(
                family,
                catalog: client.modelCatalog))
            .accessibilityIdentifier(
                "family-mapping-\(family.family)")
        }
        .padding(.vertical, 9)
        .padding(.horizontal, 4)
    }

    private var codexRouteRow: some View {
        let projection = state.modelCatalogProjection(for: client)
        let selection = codexRoutePickerSelection(
            client,
            catalog: projection.selectable,
            pendingTarget: state.pendingCodexRouteTarget())
        let pending = state.isCodexRouteMutationPending()

        return HStack(alignment: .center, spacing: 14) {
            VStack(alignment: .leading, spacing: 3) {
                Text("Codex Requests")
                Text(codexRouteConfiguredLabel(
                    client,
                    catalog: client.modelCatalog))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(codexRouteEffectiveStatus(
                    client,
                    globalRoutingEnabled:
                        state.displayedGlobalRoutingEnabled,
                    globalMutationPhase: state.globalMutationPhase,
                    catalog: client.modelCatalog))
                    .font(.caption)
                    .foregroundStyle(
                        state.globalMutationPhase == nil
                            && state.displayedGlobalRoutingEnabled
                            ? Color.secondary
                            : Color.orange)
                Text(codexRoutePickerSummary())
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer()
            if pending {
                ProgressView()
                    .controlSize(.small)
                    .accessibilityLabel("Saving Codex model")
            }
            Picker(
                "Codex model",
                selection: codexRouteBinding
            ) {
                if !projection.selectable.isEmpty {
                    Section("Baseten") {
                        ForEach(
                            projection.selectable,
                            id: \.target
                        ) { model in
                            Text(modelDisplayLabel(model))
                                .tag(CodexRoutePickerSelection.catalog(
                                    model.target))
                        }
                    }
                }
                if case .custom(let target) = selection {
                    Text("Unavailable · \(catalogModelDisplayLabel(target, catalog: client.modelCatalog))")
                        .tag(CodexRoutePickerSelection.custom(target))
                }
            }
            .labelsHidden()
            .pickerStyle(.menu)
            .frame(width: 230, alignment: .trailing)
            .disabled(
                !canEdit || pending || !state.modelCatalogAllowsMutation
                    || projection.selectable.isEmpty)
            .help(controlHelp)
            .accessibilityLabel("Codex model")
            .accessibilityValue(codexRouteConfiguredLabel(
                client,
                catalog: client.modelCatalog))
            .accessibilityIdentifier("codex-model-routing")
        }
        .padding(.vertical, 9)
        .padding(.horizontal, 4)
    }

    @ViewBuilder
    private var modelCatalogStatus: some View {
        switch state.liveModelCatalogState {
        case .idle, .loading:
            VStack(spacing: 0) {
                HStack(spacing: 8) {
                    ProgressView()
                        .controlSize(.small)
                    Text("Loading Baseten Model APIs...")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Spacer()
                }
                .padding(.vertical, 8)
                Divider()
            }
        case .signedOut(let reason):
            VStack(spacing: 0) {
                Text(liveModelCatalogSignedOutMessage(reason))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.vertical, 8)
                Divider()
            }
        case .error(let message):
            VStack(spacing: 0) {
                Text(message)
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.vertical, 8)
                Divider()
            }
        case .ready(let models):
            if models.isEmpty {
                VStack(spacing: 0) {
                    Text("No Baseten Model APIs found.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.vertical, 8)
                    Divider()
                }
            }
        }
    }

    private var subagentSection: some View {
        let projection = state.modelCatalogProjection(for: client)
        let pending = state.isSubagentMutationPending(client: client.name)
        return RoutingSectionCard {
            Label("Subagents", systemImage: "person.2")
                .font(.headline)
        } content: {
            HStack(spacing: 16) {
                VStack(alignment: .leading, spacing: 3) {
                    Text("Subagent Requests")
                    Text(subagentEffectiveDescription)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                if pending {
                    ProgressView()
                        .controlSize(.small)
                        .accessibilityLabel("Saving Subagents model")
                }
                Picker(
                    "Subagent Requests",
                    selection: subagentBinding
                ) {
                    Text("Follow requested model’s family mapping")
                        .tag(SubagentPickerSelection.useClaudeCodeModel)
                    Divider()
                    if !projection.selectable.isEmpty {
                        Section("Baseten") {
                            ForEach(
                                projection.selectable,
                                id: \.target
                            ) { model in
                                Text("Always use \(modelDisplayLabel(model))")
                                    .tag(SubagentPickerSelection.catalog(
                                        model.target))
                            }
                        }
                    }
                    if case .custom(let model) = displayedSubagentSelection {
                        Text("Always use unavailable · \(catalogModelDisplayLabel(model, catalog: client.modelCatalog))")
                            .tag(SubagentPickerSelection.custom(model))
                    }
                }
                .labelsHidden()
                .pickerStyle(.menu)
                .frame(width: 230, alignment: .trailing)
                .disabled(
                    !canEdit || pending || !state.modelCatalogAllowsMutation)
                .help(subagentControlHelp)
                .accessibilityLabel("Subagent routing behavior")
                .accessibilityValue(subagentEffectiveDescription)
                .accessibilityIdentifier("subagent-model-mapping")
            }
            .padding(4)
        }
    }

    private var clientHeaderDescription: String {
        presentation.headerDescription
    }

    private var canEdit: Bool {
        client.enabled && (isPreview || state.canMutateRouting)
    }

    private var controlHelp: String {
        state.routingMutationDisabledReason
            ?? "Choose the saved model mapping. This remains editable while global routing is Off."
    }

    private var subagentControlHelp: String {
        state.routingMutationDisabledReason
            ?? "Let Claude Code choose the model, then use its family mapping or the default mapping. Or force every detected subagent request to one model."
    }

    private func familyBinding(_ family: FamilyEntry)
        -> Binding<FamilyPickerSelection> {
        let catalog = state.modelCatalogProjection(for: client).selectable
        return Binding(
            get: {
                familyPickerSelection(
                    family,
                    catalog: catalog,
                    pendingTarget: state.pendingFamilyTarget(
                        client: client.name,
                        family: family.family))
            },
            set: { selection in
                guard !isPreview,
                      state.modelCatalogAllowsMutation,
                      let choice = familyChoice(
                        selection,
                        catalog: catalog),
                      !familyChoiceChecked(
                        family: family,
                        choice: choice) else { return }
                state.requestFamilyRoute(
                    client,
                    family: family.family,
                    choice: choice)
            })
    }

    private var codexRouteBinding: Binding<CodexRoutePickerSelection> {
        let catalog = state.modelCatalogProjection(for: client).selectable
        return Binding(
            get: {
                codexRoutePickerSelection(
                    client,
                    catalog: catalog,
                    pendingTarget: state.pendingCodexRouteTarget())
            },
            set: { selection in
                guard !isPreview,
                      state.modelCatalogAllowsMutation,
                      let model = codexRouteChoice(
                        selection,
                        catalog: catalog),
                      !codexRouteChoiceChecked(
                        client: client,
                        model: model) else { return }
                state.requestCodexRoute(client, model: model)
            })
    }

    private var displayedSubagentSelection: SubagentPickerSelection {
        let catalog = state.modelCatalogProjection(for: client).selectable
        return subagentPickerSelection(
            client,
            catalog: catalog,
            pendingTarget: state.pendingSubagentTarget(client: client.name))
    }

    private var subagentBinding: Binding<SubagentPickerSelection> {
        let catalog = state.modelCatalogProjection(for: client).selectable
        return Binding(
            get: { displayedSubagentSelection },
            set: { selection in
                guard !isPreview,
                      state.modelCatalogAllowsMutation,
                      let choice = subagentChoice(
                        selection,
                        catalog: catalog),
                      !subagentChoiceChecked(
                        subagentModel: client.subagentModel,
                        subagentRouting: client.subagentRouting,
                        choice: choice) else { return }
                state.requestSubagents(client, choice: choice)
            })
    }

    private var subagentEffectiveDescription: String {
        subagentRoutingDescription(
            client: client,
            globalRoutingEnabled: state.displayedGlobalRoutingEnabled,
            globalMutationPhase: state.globalMutationPhase)
    }
}

func reasoningUnavailableMessage(
    _ status: ClientReasoningStatus
) -> String {
    switch status.unavailableReason {
    case "catalog_capability_unknown":
        return "Reasoning options are unavailable. The saved setting is preserved."
    case "configured_effort_unavailable":
        return "The saved effort is no longer available. Choose another effort or reset to default."
    case "protocol_adapter_unsupported":
        if status.configured.mode == .default {
            return "This client protocol cannot apply the model’s default reasoning policy."
        }
        return "This client protocol cannot apply the saved reasoning policy."
    case "reasoning_unsupported":
        return "The model catalog marks reasoning as unsupported."
    default:
        return status.error
    }
}

func reasoningShowsUnavailableWarning(
    _ status: ClientReasoningStatus
) -> Bool {
    !status.error.isEmpty
}

struct RoutingSectionCard<LabelContent: View, CardContent: View>: View {
    private let label: LabelContent
    private let content: CardContent

    init(@ViewBuilder label: () -> LabelContent,
         @ViewBuilder content: () -> CardContent) {
        self.label = label()
        self.content = content()
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            label
            content
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(12)
        .background(
            Color(nsColor: .controlBackgroundColor),
            in: RoundedRectangle(cornerRadius: 9))
        .overlay {
            RoundedRectangle(cornerRadius: 9)
                .stroke(Color(nsColor: .separatorColor), lineWidth: 1)
        }
    }
}

enum FamilyPickerSelection: Hashable {
    case defaultTarget
    case native
    case catalog(String)
    case custom(String)
}

enum CodexRoutePickerSelection: Hashable {
    case catalog(String)
    case custom(String)
}

func codexRoutePickerSelection(
    _ client: ClientStatus,
    catalog: [ModelCatalogEntry],
    pendingTarget: String? = nil
) -> CodexRoutePickerSelection {
    let target = pendingTarget
        ?? client.unmatchedNativeModel?.configuredTarget
        ?? ""
    if let model = catalog.first(where: {
        target == $0.target
            || target == $0.slug
            || target == $0.alias
    }) {
        return .catalog(model.target)
    }
    return .custom(target)
}

func codexRouteChoice(
    _ selection: CodexRoutePickerSelection,
    catalog: [ModelCatalogEntry]
) -> ModelCatalogEntry? {
    guard case .catalog(let target) = selection else { return nil }
    return catalog.first(where: { $0.target == target })
}

func codexRouteChoiceChecked(
    client: ClientStatus,
    model: ModelCatalogEntry
) -> Bool {
    guard let target = client.unmatchedNativeModel?.configuredTarget else {
        return false
    }
    return target == model.target
        || target == model.slug
        || (!model.alias.isEmpty && target == model.alias)
}

func codexRouteConfiguredLabel(
    _ client: ClientStatus,
    catalog: [ModelCatalogEntry] = []
) -> String {
    guard let target = client.unmatchedNativeModel?.configuredTarget,
          !target.isEmpty else {
        return "Saved model unavailable"
    }
    return "Saved: \(catalogModelDisplayLabel(target, catalog: catalog))"
}

func codexRoutePickerSummary() -> String {
    "Choose the Baseten model used for Codex requests."
}

func codexRouteEffectiveStatus(
    _ client: ClientStatus,
    globalRoutingEnabled: Bool,
    globalMutationPhase: MutationPhase? = nil,
    catalog: [ModelCatalogEntry] = []
) -> String {
    if let globalMutationPhase {
        return pendingGlobalRoutingStatus(
            enabled: globalRoutingEnabled,
            phase: globalMutationPhase)
    }
    guard globalRoutingEnabled else {
        return "Saved route applies when global routing is On"
    }
    guard let resolution = client.unmatchedNativeModel,
          !resolution.effectiveModel.isEmpty else {
        return "Effective route unavailable"
    }
    let label = catalogModelDisplayLabel(
        resolution.effectiveModel,
        catalog: catalog)
    return "Currently \(label)"
}

func familyEntriesForDisplay(_ client: ClientStatus) -> [FamilyEntry] {
    guard client.name == "claude-code" else { return [] }
    var byName: [String: FamilyEntry] = [:]
    for family in client.families where byName[family.family] == nil {
        byName[family.family] = family
    }
    let defaultTarget = client.unmatchedNativeModel?.configuredTarget
    return ["fable", "opus", "sonnet", "haiku"].compactMap { name in
        if let existing = byName[name] {
            return existing
        }
        var dict: [String: Any] = [
            "family": name,
            "configured_source": "default",
            "effective_route": "",
            "effective_model": "",
        ]
        if let defaultTarget {
            dict["configured_target"] = defaultTarget
        }
        return FamilyEntry(dict: dict)
    }
}

func familyPickerSelection(
    _ family: FamilyEntry,
    catalog: [ModelCatalogEntry],
    pendingTarget: String? = nil
) -> FamilyPickerSelection {
    if let pendingTarget {
        if pendingTarget == "default" { return .defaultTarget }
        if pendingTarget == "native" { return .native }
        if let model = catalog.first(where: {
            pendingTarget == $0.target
                || pendingTarget == $0.slug
                || pendingTarget == $0.alias
        }) {
            return .catalog(model.target)
        }
        return .custom(pendingTarget)
    }
    if family.configuredSource == "default" {
        return .defaultTarget
    }
    guard let target = family.configuredTarget, !target.isEmpty else {
        return .defaultTarget
    }
    if target == "native" { return .native }
    if let model = catalog.first(where: {
        target == $0.target || target == $0.slug || target == $0.alias
    }) {
        return model.available ? .catalog(model.target) : .custom(target)
    }
    return .custom(target)
}

func familyChoice(_ selection: FamilyPickerSelection,
                  catalog: [ModelCatalogEntry]) -> FamilyChoice? {
    switch selection {
    case .defaultTarget:
        return .defaultMapping
    case .native:
        return .native
    case .catalog(let target):
        return catalog.first(where: { $0.target == target })
            .map(FamilyChoice.catalog)
    case .custom:
        return nil
    }
}

func familyChoiceChecked(family: FamilyEntry,
                         choice: FamilyChoice) -> Bool {
    switch choice {
    case .defaultMapping:
        return family.configuredSource == "default"
    case .native:
        return family.configuredTarget == "native"
    case .catalog(let entry):
        return family.configuredSource != "default"
            && (family.configuredTarget == entry.target
                || family.configuredTarget == entry.slug
                || family.configuredTarget == entry.alias)
    }
}

func defaultFamilyOptionLabel(_ family: FamilyEntry,
                              client: ClientStatus) -> String {
    let target = family.configuredSource == "default"
        ? family.configuredTarget
        : client.unmatchedNativeModel?.configuredTarget
    guard let target, !target.isEmpty else { return "Default" }
    return "Default · \(catalogModelDisplayLabel(target, catalog: client.modelCatalog))"
}

func familyConfiguredLabel(
    _ family: FamilyEntry,
    catalog: [ModelCatalogEntry] = []
) -> String {
    if family.configuredSource == "default" {
        if let target = family.configuredTarget, !target.isEmpty {
            return "Saved: Default · \(catalogModelDisplayLabel(target, catalog: catalog))"
        }
        return "Saved: Default"
    }
    guard let target = family.configuredTarget, !target.isEmpty else {
        return "Saved: Default"
    }
    if target == "native" {
        return "Saved: Native Provider (Anthropic)"
    }
    return "Saved: \(catalogModelDisplayLabel(target, catalog: catalog))"
}

func familyEffectiveStatus(
    _ family: FamilyEntry,
    globalRoutingEnabled: Bool,
    globalMutationPhase: MutationPhase? = nil,
    catalog: [ModelCatalogEntry] = []
) -> String {
    if let globalMutationPhase {
        return pendingGlobalRoutingStatus(
            enabled: globalRoutingEnabled,
            phase: globalMutationPhase)
    }
    if !globalRoutingEnabled {
        return "Currently using Anthropic while global routing is Off"
    }
    if !family.effectiveModel.isEmpty {
        return "Currently \(catalogModelDisplayLabel(family.effectiveModel, catalog: catalog))"
    }
    if !family.effectiveRoute.isEmpty {
        return "Currently \(capitalizeFamily(family.effectiveRoute))"
    }
    return "Effective route unavailable"
}

enum SubagentPickerSelection: Hashable {
    case useClaudeCodeModel
    case catalog(String)
    case custom(String)
}

func subagentPickerSelection(
    _ client: ClientStatus,
    catalog: [ModelCatalogEntry]? = nil,
    pendingTarget: String? = nil
) -> SubagentPickerSelection {
    let catalog = catalog ?? client.modelCatalog
    if let pendingTarget {
        if pendingTarget == "off" || pendingTarget == "inherit" {
            return .useClaudeCodeModel
        }
        if let model = catalog.first(where: {
            pendingTarget == $0.target
                || pendingTarget == $0.slug
                || pendingTarget == $0.alias
        }) {
            return .catalog(model.target)
        }
        return .custom(pendingTarget)
    }
    guard client.subagentRouting != "off",
          !client.subagentModel.isEmpty else {
        return .useClaudeCodeModel
    }
    if let model = catalog.first(where: {
        client.subagentModel == $0.target
            || client.subagentModel == $0.slug
            || client.subagentModel == $0.alias
    }) {
        return model.available
            ? .catalog(model.target)
            : .custom(client.subagentModel)
    }
    return .custom(client.subagentModel)
}

func subagentChoice(_ selection: SubagentPickerSelection,
                    catalog: [ModelCatalogEntry]) -> SubagentChoice? {
    switch selection {
    case .useClaudeCodeModel:
        return .off
    case .catalog(let target):
        return catalog.first(where: { $0.target == target })
            .map(SubagentChoice.catalog)
    case .custom:
        return nil
    }
}

func subagentRoutingDescription(
    client: ClientStatus,
    globalRoutingEnabled: Bool,
    globalMutationPhase: MutationPhase? = nil
) -> String {
    let configuredOverride = client.subagentRouting != "off"
        && !client.subagentModel.isEmpty
        ? client.subagentModel
        : nil
    let configuredOverrideLabel = configuredOverride.map {
        catalogModelDisplayLabel($0, catalog: client.modelCatalog)
    }
    if let globalMutationPhase {
        let pending = pendingGlobalRoutingStatus(
            enabled: globalRoutingEnabled,
            phase: globalMutationPhase)
        if globalRoutingEnabled {
            if let configuredOverrideLabel {
                return "\(pending). Saved override \(configuredOverrideLabel) will apply after confirmation."
            }
            return "\(pending). Claude Code’s requested model will use its family mapping or the default mapping after confirmation."
        }
        if let configuredOverrideLabel {
            return "\(pending). Saved override \(configuredOverrideLabel) will remain configured but inactive after confirmation."
        }
        return "\(pending). A native model requested by Claude Code will use Anthropic after confirmation."
    }
    if !globalRoutingEnabled {
        if let configuredOverrideLabel {
            return "Routing is Off. Claude Code’s requested model uses Anthropic; saved override \(configuredOverrideLabel) applies when routing is On."
        }
        return "No subagent override. Routing is Off, so native models requested by Claude Code use Anthropic."
    }
    if configuredOverride == nil || client.subagentEffective == "inherit" {
        return "No subagent override. Claude Code chooses the model, then Baseten Switch applies that model’s family mapping or the default mapping."
    }
    let effective = configuredOverride ?? client.subagentEffective
    let effectiveLabel = catalogModelDisplayLabel(
        effective,
        catalog: client.modelCatalog)
    return "Overrides every detected subagent request to \(effectiveLabel) before routing."
}

func modelDisplayLabel(_ model: ModelCatalogEntry) -> String {
    if !model.label.isEmpty { return model.label }
    let slug = shortModelName(model.slug)
    return slug.isEmpty ? model.target : slug
}

func catalogModelDisplayLabel(
    _ model: String,
    catalog: [ModelCatalogEntry]
) -> String {
    guard let entry = catalogModelEntry(model, catalog: catalog) else {
        return shortModelName(model)
    }
    return modelDisplayLabel(entry)
}

private func catalogModelEntry(
    _ model: String,
    catalog: [ModelCatalogEntry]
) -> ModelCatalogEntry? {
    catalog.first(where: {
        model == $0.target || model == $0.slug || model == $0.alias
    })
}

func warningBanner(_ text: String,
                   symbol: String,
                   color: Color) -> some View {
    Label(text, systemImage: symbol)
        .font(.callout)
        .foregroundStyle(color)
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            color.opacity(0.09),
            in: RoundedRectangle(cornerRadius: 8))
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isStaticText)
}

@MainActor
func mutationRecoveryWarningBanner(
    _ text: String,
    state: BasetenSwitchState
) -> some View {
    HStack(spacing: 10) {
        Label(text, systemImage: "exclamationmark.triangle.fill")
            .font(.callout)
            .foregroundStyle(.orange)
        Spacer()
        if state.canRetryMutationCleanup {
            Button("Retry cleanup") {
                state.retryMutationCleanup()
            }
            .buttonStyle(.bordered)
            .controlSize(.small)
        }
    }
    .padding(10)
    .frame(maxWidth: .infinity, alignment: .leading)
    .background(
        Color.orange.opacity(0.09),
        in: RoundedRectangle(cornerRadius: 8))
    .accessibilityIdentifier("mutation-recovery-warning")
}
