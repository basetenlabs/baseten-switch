import AppKit
import Foundation
import SwiftUI
import UniformTypeIdentifiers

struct ClaudeDesktopStatus: Equatable, Sendable {
    let configured: Bool
    let enabled: Bool
    let baseURL: String
    let modelPicker: ClaudeModelPickerStatus
    let runtimeState: String
    let currentlyBound: Bool
    let desiredMatchesActive: Bool
    let authState: String
    let requestEvidenceState: String
    let lastRequest: ClaudeDesktopRequest?
    let modelDiscovery: ClaudeDesktopModelDiscovery?
    let modelListRevision: String?

    static let placeholderKey = "baseten-switch-local"

    init?(json: String) {
        guard let data = json.data(using: .utf8),
            let dict = (try? JSONSerialization.jsonObject(with: data))
                as? [String: Any],
            dict["client"] as? String == "claude-desktop",
            let configured = dict["configured"] as? Bool,
            let enabled = dict["enabled"] as? Bool,
            let connection = dict["connection"] as? [String: Any],
            connection["provider"] as? String == "gateway",
            let baseURL = connection["base_url"] as? String,
            let url = URL(string: baseURL),
            url.scheme == "http",
            ["127.0.0.1", "localhost", "[::1]", "::1"].contains(url.host ?? ""),
            url.user == nil, url.password == nil,
            url.query == nil, url.fragment == nil,
            connection["api_key"] as? String == Self.placeholderKey,
            connection["auth_scheme"] as? String == "bearer",
            connection["model_discovery"] as? Bool == true,
            connection["auto_mode"] as? Bool == false,
            let picker = dict["model_picker"] as? [String: Any],
            let modelPicker = ClaudeModelPickerStatus(dict: picker),
            let runtime = dict["runtime"] as? [String: Any],
            let runtimeState = runtime["state"] as? String,
            let currentlyBound = runtime["currently_bound"] as? Bool,
            let desiredMatchesActive = runtime["desired_matches_active"] as? Bool,
            let auth = dict["auth"] as? [String: Any],
            let authState = auth["state"] as? String,
            let evidence = dict["request_evidence"] as? [String: Any],
            let requestEvidenceState = evidence["state"] as? String,
            evidence["scope"] as? String == "recent_500_requests"
        else {
            return nil
        }
        modelListRevision = dict["model_list_revision"] as? String
        modelDiscovery = (dict["model_discovery"] as? [String: Any])
            .flatMap(ClaudeDesktopModelDiscovery.init)
        self.configured = configured
        self.enabled = enabled
        self.baseURL = baseURL
        self.modelPicker = modelPicker
        self.runtimeState = runtimeState
        self.currentlyBound = currentlyBound
        self.desiredMatchesActive = desiredMatchesActive
        self.authState = authState
        self.requestEvidenceState = requestEvidenceState
        lastRequest = (evidence["last_request"] as? [String: Any])
            .flatMap(ClaudeDesktopRequest.init)
    }

    var gatewayReady: Bool {
        configured && enabled && runtimeState == "ready"
            && currentlyBound && desiredMatchesActive
    }
}

struct ClaudeDesktopModelDiscovery: Equatable, Sendable {
    let lastServedAt: Date
    let modelCount: Int
    let revision: String?

    init?(dict: [String: Any]) {
        guard let rawDate = dict["last_served_at"] as? String,
            let count = dict["model_count"] as? Int, count >= 0
        else { return nil }
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let fractionalDate = formatter.date(from: rawDate)
        formatter.formatOptions = [.withInternetDateTime]
        guard let date = fractionalDate ?? formatter.date(from: rawDate) else { return nil }
        lastServedAt = date
        modelCount = count
        revision = dict["revision"] as? String
    }
}

struct ClaudeDesktopRequest: Equatable, Sendable {
    let completedAt: String
    let requestedModel: String
    let servedModel: String
    let status: Int?
    let terminationReason: String

    init?(dict: [String: Any]) {
        guard let completedAt = dict["completed_at"] as? String,
            let requestedModel = dict["requested_model"] as? String,
            let servedModel = dict["served_model"] as? String
        else { return nil }
        self.completedAt = completedAt
        self.requestedModel = requestedModel
        self.servedModel = servedModel
        status = dict["status"] as? Int
        terminationReason = dict["termination_reason"] as? String ?? ""
    }
}

enum ClaudeDesktopMutation: Equatable, Sendable {
    case setup
    case add(slug: String, alias: String?)
    case remove(alias: String)
    case move(alias: String, before: String)

    var command: [String] {
        switch self {
        case .setup: return ["claude-desktop", "setup"]
        case .add(let slug, let alias):
            return ["claude-desktop", "models", "add", slug]
                + (alias.map { ["--alias", $0] } ?? [])
        case .remove(let alias):
            return ["claude-desktop", "models", "remove", alias]
        case .move(let alias, let before):
            return ["claude-desktop", "models", "move", alias, "--before", before]
        }
    }

    var operation: String {
        switch self {
        case .setup: return "setup_claude_desktop"
        case .add: return "add_claude_desktop_model"
        case .remove: return "remove_claude_desktop_model"
        case .move: return "move_claude_desktop_model"
        }
    }

    var key: String {
        self == .setup ? "connection" : "model_picker"
    }

    var requestedTarget: String {
        switch self {
        case .setup: return ""
        case .add(let slug, let alias):
            return alias.map { "\(slug) via \($0)" } ?? slug
        case .remove(let alias): return alias
        case .move(let alias, let before): return "\(alias) before \(before)"
        }
    }

    var progressLabel: String {
        switch self {
        case .setup: return "Setting up Claude Desktop"
        case .add: return "Adding model"
        case .remove: return "Removing model"
        case .move: return "Saving model order"
        }
    }

    func isConfirmed(by status: ClaudeDesktopStatus?) -> Bool {
        guard let status, status.gatewayReady else { return false }
        switch self {
        case .setup: return true
        case .add(let slug, let alias):
            return status.modelPicker.models.contains {
                $0.slug == slug && (alias == nil || $0.alias == alias)
            }
        case .remove(let alias):
            return !status.modelPicker.models.contains { $0.alias == alias }
        case .move(let alias, let before):
            let rows = status.modelPicker.models
            guard let from = rows.firstIndex(where: { $0.alias == alias }),
                let to = rows.firstIndex(where: { $0.alias == before })
            else {
                return false
            }
            return from + 1 == to
        }
    }
}

struct PendingClaudeDesktopMutation: Equatable, Sendable {
    let operationID: String
    let kind: ClaudeDesktopMutation
    var phase: MutationPhase
}

func claudeDesktopGatewayTitle(
    _ status: ClaudeDesktopStatus?,
    statusIsCurrent: Bool = true
) -> String {
    guard let status else { return "Checking local gateway" }
    guard status.configured else { return "Set up Claude Desktop" }
    guard statusIsCurrent, status.gatewayReady else {
        return status.runtimeState == "pending"
            ? "Saved settings need to be applied"
            : "Local gateway unavailable"
    }
    switch status.authState {
    case "ready": return "Local gateway ready"
    case "signed_out": return "Sign in to Baseten"
    case "refresh_failed": return "Baseten authentication needs attention"
    default: return "Checking Baseten authentication"
    }
}

func claudeDesktopGatewayDetail(
    _ status: ClaudeDesktopStatus?,
    statusIsCurrent: Bool = true
) -> String {
    guard let status else {
        return "Switch reads gateway status without changing Claude Desktop."
    }
    guard status.configured else {
        return "Connect Claude Desktop to make your Baseten models available in Claude."
    }
    guard statusIsCurrent, status.gatewayReady else {
        return
            "Configured models remain saved. Requests fail if Claude Desktop is still configured for this gateway."
    }
    switch status.authState {
    case "signed_out", "refresh_failed":
        return "Restore Baseten authentication to use the configured models."
    case "ready":
        return "Choose the conversation model in Claude Desktop. Routing overrides can stay Off."
    default:
        return "The listener is ready. Baseten authentication has not been confirmed."
    }
}

func discoverableClientNames(_ clients: [ClientStatus]) -> [String] {
    let names = clients.map(\.name)
    return names.contains("claude-desktop") ? names : names + ["claude-desktop"]
}

struct ClaudeDesktopView: View {
    @ObservedObject var state: BasetenSwitchState
    let isPreview: Bool
    let viewRequests: () -> Void

    private enum Page { case home, models, connection }
    @State private var page: Page = .home
    @State private var searchText = ""
    @State private var previewingSlug: String?
    @State private var aliasChoices: DesktopAliasChoices?
    @State private var addError: String?
    @State private var preparingConnection = false
    @State private var setupNotice: String?
    @State private var copiedSetting: String?
    @State private var manualSetupStartedAt = Date()
    @ObservedObject private var restart: ClaudeDesktopRestart
    @StateObject private var setup: ClaudeDesktopSetup

    init(state: BasetenSwitchState, isPreview: Bool, viewRequests: @escaping () -> Void) {
        self.state = state
        self.restart = state.claudeDesktopRestart
        self.isPreview = isPreview
        self.viewRequests = viewRequests
        _setup = StateObject(
            wrappedValue: ClaudeDesktopSetup(
                store:
                    ClaudeDesktopConfigurationStore(
                        configurationName:
                            state.variant.channel == .stable ? "Baseten Switch" : "Baseten Switch Preview")))
    }

    private var status: ClaudeDesktopStatus? { state.claudeDesktopStatus }
    private var configured: [ClaudeModelPickerRow] { status?.modelPicker.models ?? [] }
    private var projection: ClaudeModelPickerProjection {
        projectClaudeModelPicker(
            status: status?.modelPicker,
            liveState: state.displayedModelCatalogState)
    }
    private var canEdit: Bool {
        !isPreview && state.canMutateClaudeDesktop && !preparingConnection
            && !setup.phase.isWorking
    }
    private var connectionSettings: ClaudeDesktopConnectionSettings? {
        status.map {
            ClaudeDesktopConnectionSettings(
                baseURL: $0.baseURL, apiKey: ClaudeDesktopStatus.placeholderKey)
        }
    }
    private var statusIsCurrent: Bool {
        state.gatewayUp && state.routingSnapshot?.desiredMatchesActive == true
            && state.claudeDesktopStatusError == nil
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                header
                if let error = state.lastError ?? state.claudeDesktopStatusError {
                    Label(error, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption).foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if let pending = state.pendingClaudeDesktop {
                    HStack {
                        ProgressView().controlSize(.small)
                        Text(pending.kind.progressLabel).font(.caption)
                    }
                }
                switch page {
                case .home: home
                case .models: models
                case .connection: connection
                }
            }
            .padding(28)
            .frame(maxWidth: 760, alignment: .leading)
            .frame(maxWidth: .infinity, alignment: .topLeading)
        }
        .accessibilityIdentifier("client-claude-desktop")
        .task {
            guard !isPreview else { return }
            state.ensureModelCatalogLoaded()
            while !Task.isCancelled {
                state.requestClaudeDesktopStatusRefresh()
                await state.waitForClaudeDesktopStatusRefresh()
                if let connectionSettings { setup.load(connection: connectionSettings) }
                setup.observe(state.claudeDesktopStatus, isCurrent: statusIsCurrent)
                if case .configured = setup.phase { restart.setConfigured() }
                do { try await Task.sleep(nanoseconds: 5_000_000_000) } catch { return }
            }
        }
        .sheet(
            isPresented: Binding(
                get: {
                    if case .confirming = setup.phase { return true }
                    return false
                },
                set: { if !$0 { setup.cancelRestart() } })
        ) {
            restartConfirmation
        }
        .alert("Restart Claude Desktop?", isPresented: Binding(
            get: { restart.phase == .confirming },
            set: { if !$0 { restart.cancel() } })
        ) {
            Button("Cancel", role: .cancel) { restart.cancel() }
            Button("Restart") { Task { await restart.restart() } }
                .keyboardShortcut(.defaultAction)
        } message: {
            Text("Save your work in Claude before continuing. Switch will quit and reopen Claude Desktop.")
        }
        .confirmationDialog(
            "Choose an alias",
            isPresented: Binding(
                get: { aliasChoices != nil },
                set: { if !$0 { aliasChoices = nil } }),
            titleVisibility: .visible,
            presenting: aliasChoices
        ) { choices in
            ForEach(choices.rows) { row in
                Button(row.alias) { add(slug: choices.slug, alias: row.alias) }
            }
            Button("Cancel", role: .cancel) { aliasChoices = nil }
        } message: { _ in
            Text("More than one saved alias routes to this model. Choose the exact alias to add.")
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Text("Claude Desktop").font(.title2.weight(.semibold))
                Text("Beta").font(.caption).foregroundStyle(.secondary)
                    .padding(.horizontal, 6).padding(.vertical, 2)
                    .overlay(RoundedRectangle(cornerRadius: 4).stroke(.quaternary))
            }
            Text("Your Baseten models, available directly in Claude.")
                .font(.callout).foregroundStyle(.secondary)
        }
    }

    private var home: some View {
        VStack(alignment: .leading, spacing: 24) {
            gatewayCallout
            if let setupNotice {
                Text(setupNotice).font(.callout).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            automaticSetup
            selectedModels
        }
    }

    @ViewBuilder private var automaticSetup: some View {
        switch setup.phase {
        case .applying:
            ProgressView("Configuring and reopening Claude Desktop…")
                .controlSize(.small)
        case .waiting, .notObserved:
            VStack(alignment: .leading, spacing: 18) {
                Text("Waiting for Claude Desktop").font(.headline)
                Text("Switch will confirm the connection when Claude requests its model list.")
                    .font(.callout).foregroundStyle(.secondary)
                VStack(alignment: .leading, spacing: 12) {
                    Label(
                        statusIsCurrent && status?.gatewayReady == true
                            ? "Local gateway is running" : "Local gateway is unavailable",
                        systemImage: statusIsCurrent && status?.gatewayReady == true
                            ? "checkmark" : "exclamationmark.triangle")
                    Label("Connection settings are ready", systemImage: "checkmark")
                    HStack(spacing: 10) {
                        ProgressView().controlSize(.small)
                        Text("Waiting for Claude to load your models…")
                    }
                }
                .font(.callout).padding(18).frame(maxWidth: .infinity, alignment: .leading)
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(.quaternary))
                if case .notObserved = setup.phase {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Claude hasn’t connected yet").font(.callout.weight(.medium))
                        Text(
                            "Open Claude Desktop and finish any startup prompts, then check again. You can also follow the manual setup guide."
                        )
                        .font(.callout).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                    }
                    .padding(14).frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color.secondary.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
                }
                HStack(spacing: 14) {
                    Button("Open Claude Desktop", action: openClaudeDesktop)
                        .buttonStyle(.borderedProminent)
                    Button("Check again", action: checkConnection)
                    manualSetupButton
                }
                .disabled(isPreview)
            }
        case .connected where setup.connectionIsConfirmed(by: status, isCurrent: statusIsCurrent):
            VStack(alignment: .leading, spacing: 18) {
                Label("Gateway connection confirmed", systemImage: "checkmark.circle.fill")
                    .font(.headline).foregroundStyle(.green)
                Text("Your model list is ready. Choose the conversation model in Claude Desktop.")
                    .font(.callout).foregroundStyle(.secondary)
                if let discovery = status?.modelDiscovery {
                    LabeledContent(
                        "Model list served to Claude",
                        value:
                            "\(discovery.modelCount) models"
                    )
                    .font(.callout)
                }
                requestEvidence
                HStack(spacing: 14) {
                    Button("Open Claude Desktop", action: openClaudeDesktop)
                        .buttonStyle(.borderedProminent)
                    manualSetupButton
                    Button("View requests", action: viewRequests).buttonStyle(.link)
                }
                .disabled(isPreview)
            }
        case .configured, .connected:
            VStack(alignment: .leading, spacing: 14) {
                Text("Claude Desktop is configured").font(.headline)
                Text("Open Claude to use the gateway. Saved settings alone do not confirm a live connection.")
                    .font(.callout).foregroundStyle(.secondary)
                requestEvidence
                HStack(spacing: 14) {
                    Button("Open Claude Desktop", action: openClaudeDesktop)
                        .buttonStyle(.borderedProminent)
                    Button("Reconnect…", action: beginConnection)
                        .disabled(!canEdit)
                    manualSetupButton
                }
                .disabled(isPreview)
            }
        case .unavailable(let message):
            VStack(alignment: .leading, spacing: 16) {
                Text("Automatic setup unavailable").font(.headline)
                Text(message).font(.callout).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                HStack(spacing: 14) {
                    Button("Set up manually") { showManualSetup() }
                        .buttonStyle(.borderedProminent)
                    Button("Check again", action: beginConnection).disabled(!canEdit)
                }
            }
        case .ready, .confirming:
            VStack(alignment: .leading, spacing: 20) {
                Text("Connect Claude to your Baseten models").font(.headline)
                Text(
                    "Switch will configure Claude Desktop to use your local gateway. Choose which models appear in Claude, then pick a model when you start a chat."
                )
                .font(.callout).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                VStack(alignment: .leading, spacing: 8) {
                    Text("Before you connect").font(.callout.weight(.semibold))
                    Text(
                        "Claude will use gateway mode, with Baseten billing and separate local chat history. Your existing Claude account history stays in standard mode."
                    )
                    Text(
                        "Claude Desktop needs to restart. You can review the restart before Switch changes Claude’s settings."
                    )
                }
                .font(.callout).fixedSize(horizontal: false, vertical: true)
                .padding(.vertical, 16).padding(.horizontal, 18)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.secondary.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
                HStack(spacing: 16) {
                    Button("Connect Claude Desktop…", action: beginConnection)
                        .buttonStyle(.borderedProminent)
                        .disabled(
                            isPreview || preparingConnection || state.hasAnyConfigMutation
                                || (state.gatewayUp && !state.canMutateClaudeDesktop)
                        )
                        .accessibilityIdentifier("claude-desktop-connect")
                    manualSetupButton
                    if preparingConnection { ProgressView().controlSize(.small) }
                }
            }
        }
    }

    private var manualSetupButton: some View {
        Button("Set up manually") { showManualSetup() }.buttonStyle(.link)
            .accessibilityIdentifier("claude-desktop-manual-setup")
    }

    private var selectedModels: some View {
        VStack(alignment: .leading, spacing: 12) {
            Divider()
            HStack {
                Text("Models ready for Claude Desktop").font(.subheadline.weight(.semibold))
                Spacer()
                Button("Manage models", action: showModels).buttonStyle(.link)
                    .disabled(isPreview || preparingConnection || setup.phase.isWorking)
            }
            if configured.isEmpty {
                Text("Choose at least one Baseten model before connecting Claude Desktop.")
                    .font(.callout).foregroundStyle(.secondary)
            } else {
                Text(configured.map(\.label).joined(separator: " · "))
                    .font(.callout).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.top, 4)
    }

    private var restartConfirmation: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Restart Claude Desktop to connect?").font(.headline)
            Text(
                "Save your work in Claude before continuing. Switch will configure the gateway and restart Claude Desktop."
            )
            .font(.callout).fixedSize(horizontal: false, vertical: true)
            Text("Your existing configuration will be backed up.")
                .font(.caption).foregroundStyle(.secondary)
            HStack(spacing: 10) {
                Spacer()
                Button("Cancel", role: .cancel) { setup.cancelRestart() }
                    .keyboardShortcut(.cancelAction)
                Button("Restart and connect") { Task { await setup.connect() } }
                    .buttonStyle(.borderedProminent).keyboardShortcut(.defaultAction)
                    .accessibilityIdentifier("claude-desktop-restart-connect")
            }
        }
        .padding(24).frame(width: 440)
    }

    private var gatewayCallout: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(
                claudeDesktopGatewayTitle(status, statusIsCurrent: statusIsCurrent),
                systemImage:
                    statusIsCurrent && status?.gatewayReady == true && status?.authState == "ready"
                    ? "checkmark.circle.fill" : "network"
            )
            .font(.body.weight(.medium))
            Text(claudeDesktopGatewayDetail(status, statusIsCurrent: statusIsCurrent))
                .font(.caption).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if status?.configured == true && (!statusIsCurrent || status?.gatewayReady != true) {
                if !state.gatewayUp {
                    Button("Start Local Gateway") {
                        guard !isPreview else { return }
                        Task {
                            await state.startSystem()
                            state.requestClaudeDesktopStatusRefresh()
                        }
                    }
                    .disabled(isPreview || state.starting || !state.canStartSystem)
                } else {
                    Button("Apply Gateway Setup") { state.requestClaudeDesktop(.setup) }
                        .disabled(!canEdit)
                }
                Button("Review Setup") { showManualSetup() }.buttonStyle(.link)
            }
            if status?.authState == "signed_out" || status?.authState == "refresh_failed" {
                Button(
                    status?.authState == "signed_out" ? "Sign In to Baseten…" : "Reauthenticate with Baseten…"
                ) {
                    guard !isPreview else { return }
                    Task { await state.reauthenticate() }
                }
                .disabled(isPreview || state.reauthenticating || state.variant.channel != .stable)
            }
        }
        .padding(14).frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.secondary.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).stroke(.quaternary))
        .accessibilityIdentifier("claude-desktop-gateway-status")
    }

    private var requestEvidence: some View {
        RoutingSectionCard {
            Text("Endpoint requests").font(.subheadline.weight(.semibold))
        } content: {
            VStack(alignment: .leading, spacing: 8) {
                if let last = status?.lastRequest,
                    status?.requestEvidenceState == "observed"
                {
                    LabeledContent(
                        "Last request",
                        value: last.status.map { "HTTP \($0)" }
                            ?? "No HTTP response")
                    LabeledContent(
                        "Model",
                        value: last.servedModel.isEmpty
                            ? last.requestedModel : last.servedModel)
                    Text(last.completedAt).foregroundStyle(.secondary)
                } else {
                    Text(
                        status?.requestEvidenceState == "not_observed"
                            ? "No requests observed in the recent request window."
                            : "Recent request evidence is unavailable."
                    )
                    .foregroundStyle(.secondary)
                }
                Text("Traffic at this endpoint does not establish Claude Desktop’s current deployment mode.")
                    .foregroundStyle(.secondary)
            }
            .font(.caption).textSelection(.enabled)
        }
    }

    private var restartNoticeText: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Restart Claude Desktop to apply changes")
                .font(.system(size: 13, weight: .semibold))
            Text("Your changes are saved. Restart Claude to load the updated model list.")
                .font(.system(size: 13))
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    @ViewBuilder
    private var restartNoticeAction: some View {
        if restart.phase.isWorking {
            ProgressView(restart.phase == .quitting ? "Quitting Claude Desktop…" : "Opening Claude Desktop…")
                .controlSize(.small)
        } else {
            Button {
                Task { await restart.prepare() }
            } label: {
                Label(restart.isRunning ? "Restart Claude Desktop…" : "Open Claude Desktop",
                      systemImage: "arrow.clockwise")
            }
            .buttonStyle(.bordered)
            .fixedSize()
            .disabled(isPreview)
            .accessibilityIdentifier("claude-desktop-restart-models")
        }
    }

    private var restartNotice: some View {
        VStack(alignment: .leading, spacing: 10) {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .top, spacing: 12) {
                    restartNoticeText
                        .frame(minWidth: 260, idealWidth: 280, maxWidth: .infinity, alignment: .leading)
                    restartNoticeAction
                }
                VStack(alignment: .leading, spacing: 10) {
                    restartNoticeText
                    restartNoticeAction
                }
            }
            if case .failed(let message) = restart.phase {
                Text(message).font(.system(size: 13)).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if restart.phase == .waiting {
                Text("Waiting for Claude to load the updated model list. If it does not load, try opening Claude again.")
                    .font(.system(size: 13)).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.orange.opacity(0.10), in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.orange.opacity(0.25)))
        .accessibilityIdentifier("claude-desktop-restart-notice")
    }

    private var models: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Models").font(.headline)
            Text("Choose which models appear in Claude Desktop’s model picker.")
                .font(.callout).foregroundStyle(.secondary)
            if status?.configured == true && (!statusIsCurrent || status?.gatewayReady != true) {
                gatewayCallout
            }
            if restart.needsRestart { restartNotice }
            Text("Configured for Claude Desktop").font(.subheadline.weight(.semibold))
            if !restart.needsRestart {
                Text("Claude loads this list when it starts. Changes take effect after a restart.")
                    .font(.system(size: 13)).foregroundStyle(.secondary)
            }
            if configured.isEmpty {
                Text("No Baseten models are configured.").font(.caption).foregroundStyle(.secondary)
            }
            ForEach(configured) { row in
                let index = configured.firstIndex(where: { $0.id == row.id }) ?? 0
                ConfiguredModelRowView(
                    row: row,
                    rowState: nil,
                    canEdit: canEdit,
                    canMoveUp: index > 0,
                    canMoveDown: index < configured.count - 1,
                    accessibilityPrefix: "claude-desktop",
                    moveUp: { move(index: index, down: false) },
                    moveDown: { move(index: index, down: true) },
                    remove: { state.requestClaudeDesktop(.remove(alias: row.alias)) })
                if row.id != configured.last?.id { Divider() }
            }
            Divider()
            HStack {
                Text("Available to add").font(.subheadline.weight(.semibold))
                Spacer()
                Button(action: state.requestClientPageRefresh) {
                    if state.modelCatalogIsRefreshing {
                        HStack(spacing: 6) {
                            ProgressView().controlSize(.mini)
                            Text("Refreshing…")
                        }
                    } else {
                        Label("Refresh models", systemImage: "arrow.clockwise")
                    }
                }
                .controlSize(.small)
                .disabled(isPreview || state.modelCatalogIsRefreshing)
                .accessibilityIdentifier("claude-desktop-refresh-models")
            }
            Text(
                "Models are available to try. Chat, tools, images, and long conversations may work differently by model. Provider errors are shown without switching to Claude."
            )
            .font(.caption).foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
            TextField("Search models", text: $searchText)
                .textFieldStyle(.roundedBorder)
                .accessibilityIdentifier("claude-desktop-search")
            if let addError {
                Text(addError).font(.caption).foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            availableModels
            Text(
                "Removing a model keeps its chat history. Requests for its removed model ID fail until you select an available model in Claude Desktop."
            )
            .font(.caption).foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
            Button("Done") { page = .home }.buttonStyle(.borderedProminent)
        }
        .accessibilityIdentifier("claude-desktop-models")
    }

    @ViewBuilder private var availableModels: some View {
        switch state.displayedModelCatalogState {
        case .idle, .loading:
            ProgressView("Loading Baseten Model APIs…").controlSize(.small)
        case .signedOut(let reason):
            Text(liveModelCatalogSignedOutMessage(reason))
                .font(.caption).foregroundStyle(.orange)
        case .error:
            Text(
                "Available models could not be loaded. Use Refresh models to try again. Your configured models remain saved."
            )
            .font(.caption).foregroundStyle(.orange)
        case .ready(let catalog):
            let available = filterAvailableModels(projection.availableToAdd, query: searchText)
            if available.isEmpty {
                Text(
                    searchText.isEmpty
                        ? (catalog.isEmpty
                            ? "No models were loaded. Use Refresh models to try again."
                            : "Every available model is already configured.")
                        : "No available models match your search."
                )
                .font(.caption).foregroundStyle(.secondary)
            }
            ForEach(available, id: \.slug) { model in
                AvailableModelRowView(
                    model: model,
                    canAdd: canEdit && status?.configured == true
                        && state.modelCatalogAllowsMutation && previewingSlug == nil,
                    previewing: previewingSlug == model.slug,
                    accessibilityPrefix: "claude-desktop",
                    add: { add(slug: model.slug, alias: nil) })
            }
        }
    }

    private var connection: some View {
        VStack(alignment: .leading, spacing: 22) {
            Button("Back to connection") { page = .home }.buttonStyle(.link)
            VStack(alignment: .leading, spacing: 8) {
                Text("Set up Claude Desktop manually").font(.headline)
                Text("Keep Switch open while you follow these steps in Claude.")
                    .font(.callout).foregroundStyle(.secondary)
            }
            gatewayCallout
            Text(
                "Gateway mode uses Baseten billing and separate local chat history. Your Claude account history stays in standard mode."
            )
            .font(.caption).foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
            if status?.configured != true {
                Button("Prepare local gateway", action: prepareManualGateway)
                    .disabled(isPreview || preparingConnection || state.hasAnyConfigMutation)
                    .accessibilityIdentifier("claude-desktop-setup")
            }
            VStack(alignment: .leading, spacing: 9) {
                Text("1. Open Claude’s configuration window")
                    .font(.subheadline.weight(.semibold))
                Text("In Claude’s menu bar, choose Help › Troubleshooting › Enable Developer Mode.")
                Text("Then choose Developer › Configure Third-Party Inference…")
                Button("Open Claude Desktop", action: openClaudeDesktop).disabled(isPreview)
                Text(
                    "The Developer menu appears after enabling it. If these settings are organization-managed, contact your administrator."
                )
                .font(.caption).foregroundStyle(.secondary)
            }
            .font(.callout).fixedSize(horizontal: false, vertical: true)
            VStack(alignment: .leading, spacing: 12) {
                Text("2. Enter the connection settings")
                    .font(.subheadline.weight(.semibold))
                Text("Open Connection in Claude’s configuration window and use these values.")
                    .font(.callout).foregroundStyle(.secondary)
                if let status {
                    VStack(alignment: .leading, spacing: 0) {
                        setupValue("Inference provider", "Gateway")
                        Divider()
                        setupValue("Credential kind", "Static API key")
                        Divider()
                        setupValue("Gateway base URL", status.baseURL, copyable: true)
                        Divider()
                        setupValue("Gateway API key", ClaudeDesktopStatus.placeholderKey, copyable: true)
                        Divider()
                        setupValue("Gateway auth scheme", "bearer", copyable: true)
                        Divider()
                        Text("This is Switch’s local gateway key. Your Baseten API key stays in Switch.")
                            .font(.caption).foregroundStyle(.secondary)
                            .padding(14).fixedSize(horizontal: false, vertical: true)
                    }
                    .overlay(RoundedRectangle(cornerRadius: 8).stroke(.quaternary))
                    .clipShape(RoundedRectangle(cornerRadius: 8))
                }
                Text(
                    "Under Models, enable model discovery and leave the explicit model list empty. Under Capabilities, turn Chat on and Auto mode off."
                )
                .font(.callout).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                HStack(spacing: 12) {
                    Button("Save configuration file…", action: exportConfiguration)
                        .disabled(isPreview || connectionSettings == nil)
                    Text("Or import this file from Claude’s configuration picker.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Divider()
            VStack(alignment: .leading, spacing: 9) {
                Text("3. Apply, restart and check the connection")
                    .font(.subheadline.weight(.semibold))
                Text(
                    "In Claude, click Apply Changes, then Save & Restart. Start with your third-party configuration if Claude asks, then open its model picker."
                )
                Text("Return here to check whether Claude has reached the Switch gateway.")
                    .foregroundStyle(.secondary)
                HStack(spacing: 12) {
                    Button("Check connection", action: checkConnection)
                        .buttonStyle(.borderedProminent).disabled(isPreview)
                    if let discovery = status?.modelDiscovery,
                        discovery.lastServedAt >= manualSetupStartedAt,
                        statusIsCurrent && status?.gatewayReady == true
                    {
                        Text(
                            "A model list was served at \(discovery.lastServedAt.formatted(date: .omitted, time: .standard))."
                        )
                        .font(.caption).foregroundStyle(.secondary)
                    } else {
                        Text("No connection from Claude observed yet.")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                }
            }
            .font(.callout).fixedSize(horizontal: false, vertical: true)
            Divider()
            VStack(alignment: .leading, spacing: 8) {
                Text("Return to standard Claude").font(.subheadline.weight(.semibold))
                Text(
                    "In Claude Desktop, leave the third-party configuration and use standard Claude sign-in. Turning Switch routing overrides off does not change Claude’s mode."
                )
                .font(.caption).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            }
        }
        .accessibilityIdentifier("claude-desktop-connection")
    }

    private func setupValue(_ label: String, _ value: String, copyable: Bool = false) -> some View {
        HStack(spacing: 12) {
            Text(label).foregroundStyle(.secondary)
                .frame(width: 166, alignment: .leading)
            Text(value).textSelection(.enabled)
                .font(copyable ? .system(.caption, design: .monospaced) : .callout)
                .lineLimit(1).truncationMode(.middle)
                .frame(maxWidth: .infinity, alignment: .leading)
            if copyable {
                Button(copiedSetting == label ? "Copied" : "Copy") {
                    NSPasteboard.general.clearContents()
                    if NSPasteboard.general.setString(value, forType: .string) {
                        copiedSetting = label
                    }
                }
                .controlSize(.small).frame(width: 60)
                .accessibilityLabel("Copy \(label)")
                .accessibilityIdentifier(
                    "claude-desktop-copy-\(label.lowercased().replacingOccurrences(of: " ", with: "-"))")
            } else {
                Color.clear.frame(width: 60, height: 1).accessibilityHidden(true)
            }
        }
        .font(.callout).padding(.vertical, 10).padding(.horizontal, 14)
        .frame(minHeight: 44)
    }

    private func beginConnection() {
        guard !isPreview, !preparingConnection, !setup.phase.isWorking else { return }
        preparingConnection = true
        setupNotice = nil
        Task {
            defer { preparingConnection = false }
            guard await ensureGateway() else { return }
            guard status?.authState == "ready" else {
                setupNotice = "Sign in to Baseten before connecting Claude Desktop."
                return
            }
            guard !configured.isEmpty else {
                setupNotice = "Choose at least one Baseten model, then return to connect Claude Desktop."
                page = .models
                return
            }
            guard let connectionSettings else { return }
            setup.prepare(connection: connectionSettings)
        }
    }

    private func showModels() {
        guard !isPreview, !preparingConnection else { return }
        preparingConnection = true
        Task {
            defer { preparingConnection = false }
            if await ensureGateway() { page = .models }
        }
    }

    private func prepareManualGateway() {
        guard !isPreview, !preparingConnection else { return }
        preparingConnection = true
        Task {
            defer { preparingConnection = false }
            _ = await ensureGateway()
        }
    }

    private func ensureGateway() async -> Bool {
        if !state.gatewayUp { await state.startSystem() }
        guard state.gatewayUp else {
            setupNotice = "Start the local gateway before connecting Claude Desktop."
            return false
        }
        if status?.gatewayReady != true || !statusIsCurrent {
            await state.setClaudeDesktop(.setup)
        }
        state.requestClaudeDesktopStatusRefresh()
        await state.waitForClaudeDesktopStatusRefresh()
        return statusIsCurrent && status?.gatewayReady == true
    }

    private func showManualSetup() {
        manualSetupStartedAt = Date()
        page = .connection
    }

    private func checkConnection() {
        guard !isPreview else { return }
        if case .connection = page {
            setup.beginManualVerification(since: manualSetupStartedAt)
        }
        Task {
            state.requestClaudeDesktopStatusRefresh()
            await state.waitForClaudeDesktopStatusRefresh()
            setup.observe(state.claudeDesktopStatus, isCurrent: statusIsCurrent)
        }
    }

    private func exportConfiguration() {
        guard !isPreview, let connectionSettings,
            let data = try? connectionSettings.configurationData()
        else { return }
        let panel = NSSavePanel()
        panel.allowedContentTypes = [.json]
        panel.nameFieldStringValue = "baseten-switch-claude-desktop.json"
        panel.begin { response in
            guard response == .OK, let url = panel.url else { return }
            do { try data.write(to: url, options: .atomic) } catch {
                state.lastError = "The Claude Desktop configuration file could not be saved."
            }
        }
    }

    private func add(slug: String, alias: String?) {
        guard canEdit else { return }
        previewingSlug = slug
        addError = nil
        Task {
            let outcome = await state.previewClaudeDesktopAdd(slug: slug, alias: alias)
            guard previewingSlug == slug else { return }
            previewingSlug = nil
            switch outcome {
            case .preview(let preview, let explicitAlias):
                state.requestClaudeDesktop(.add(slug: preview.slug, alias: explicitAlias))
            case .aliasChoices(let choiceSlug, let choices):
                aliasChoices = DesktopAliasChoices(slug: choiceSlug, rows: choices)
            case nil:
                addError = claudeModelPickerAddPreviewErrorMessage(state.lastError)
            }
        }
    }

    private func move(index: Int, down: Bool) {
        let command =
            down
            ? claudeModelPickerMoveDownCommand(rows: configured, index: index)
            : claudeModelPickerMoveUpCommand(rows: configured, index: index)
        guard canEdit, let command else { return }
        state.requestClaudeDesktop(.move(alias: command[1], before: command[3]))
    }

    private func openClaudeDesktop() {
        guard !isPreview else { return }
        guard
            let url = NSWorkspace.shared.urlForApplication(
                withBundleIdentifier: "com.anthropic.claudefordesktop")
        else {
            state.lastError =
                "Claude Desktop is not installed. Install it, then open its third-party setup window."
            return
        }
        NSWorkspace.shared.openApplication(at: url, configuration: .init()) { _, error in
            if error != nil {
                Task { @MainActor in state.lastError = "Claude Desktop could not be opened." }
            }
        }
    }
}

private struct DesktopAliasChoices: Identifiable {
    let slug: String
    let rows: [ClaudeModelPickerRow]
    var id: String { slug }
}
