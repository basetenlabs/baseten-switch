import Foundation
import SwiftUI

enum ClaudeModelPickerMutationKind: Equatable, Sendable {
    case enable(convertReplacementMode: Bool)
    case add(slug: String, alias: String?)
    case remove(alias: String)
    case move(alias: String, before: String)
    case sync(convertReplacementMode: Bool)

    var command: [String] {
        switch self {
        case .enable(let convert):
            return ["claude", "picker", "enable"]
                + (convert ? ["--convert-replacement-mode"] : [])
        case .add(let slug, let alias):
            return ["claude", "picker", "add", slug]
                + (alias.map { ["--alias", $0] } ?? [])
        case .remove(let alias):
            return ["claude", "picker", "remove", alias]
        case .move(let alias, let before):
            return [
                "claude", "picker", "move", alias,
                "--before", before,
            ]
        case .sync(let convert):
            return ["claude", "picker", "sync"]
                + (convert ? ["--convert-replacement-mode"] : [])
        }
    }

    var receiptOperation: String {
        switch self {
        case .enable: return "enable_claude_picker"
        case .add: return "add_claude_picker_model"
        case .remove: return "remove_claude_picker_model"
        case .move: return "move_claude_picker_model"
        case .sync: return "sync_claude_picker"
        }
    }

    var requestedTarget: String {
        switch self {
        case .enable, .sync: return ""
        case .add(let slug, let alias):
            return alias.map { "\(slug) via \($0)" } ?? slug
        case .remove(let alias): return alias
        case .move(let alias, let before): return "\(alias) before \(before)"
        }
    }

    var progressLabel: String {
        switch self {
        case .enable: return "Enabling model picker"
        case .add: return "Adding model"
        case .remove: return "Removing model"
        case .move: return "Saving model order"
        case .sync: return "Syncing with Claude Code"
        }
    }

    var convertsReplacementMode: Bool {
        switch self {
        case .enable(let convert), .sync(let convert): return convert
        default: return false
        }
    }

    var isSync: Bool {
        if case .sync = self { return true }
        return false
    }
}

func basetenFallbackTargetStatusLine(
    fallback: BasetenModelFallbackStatus?,
    policy: FallbackPolicyStatus?,
    desiredMatchesActive: Bool,
    supportsFallbackPolicy: Bool
) -> String {
    let readiness: String
    if !supportsFallbackPolicy {
        readiness = "Unavailable"
    } else if !desiredMatchesActive {
        readiness = "Needs reload"
    } else if fallback?.ready == true {
        readiness = "Ready"
    } else {
        readiness = "Not ready"
    }
    guard let policy else { return readiness + " · Policy unavailable" }
    return readiness
        + " · 429 " + (policy.onBaseten429 ? "On" : "Off")
        + " · 5xx " + (policy.onBaseten5xx ? "On" : "Off")
}

func basetenFallbackTargetReadinessDetail(
    fallback: BasetenModelFallbackStatus?,
    desiredMatchesActive: Bool,
    supportsFallbackPolicy: Bool
) -> String? {
    guard supportsFallbackPolicy else {
        return "Update the local gateway to configure automatic fallback."
    }
    guard desiredMatchesActive else {
        return "The saved and active configurations differ. Resolve the reload error first."
    }
    guard let fallback else {
        return "Fallback target status is unavailable."
    }
    switch fallback.reason {
    case nil where fallback.ready:
        return nil
    case "not_configured":
        return "A fallback target is not configured."
    case "provider_auth_unavailable":
        return "Anthropic authentication is unavailable to the local gateway."
    case "desired_active_mismatch":
        return "The saved fallback target is not active yet."
    case "unsupported_router":
        return "Update the local gateway to configure this fallback target."
    default:
        return fallback.ready
            ? nil
            : "The fallback target is not ready."
    }
}

struct ClaudeNativeFallbackOption: Equatable, Sendable {
    let label: String
    let model: String
}

func initialClaudeNativeFallbackSelection(
    options: [ClaudeNativeFallbackOption],
    currentModel: String
) -> String {
    options.first(where: { $0.model == currentModel })?.model
        ?? ""
}

func claudeNativeFallbackSelectionDetail(
    selectedModel: String,
    currentModel: String
) -> String? {
    guard selectedModel.isEmpty else { return nil }
    if currentModel.isEmpty {
        return "No fallback model is configured. Select a model to continue."
    }
    return "Your configured fallback is not one of the current choices. Select a model to replace it."
}

func claudeNativeFallbackCatalogDetail(
    options: [ClaudeNativeFallbackOption]
) -> String? {
    options.isEmpty
        ? "Fallback model choices will appear when the model catalog is available."
        : nil
}

struct ClaudeNativeFallbackEditorDraft: Equatable, Identifiable, Sendable {
    let id: UUID
    var selectedModel: String

    init?(
        options: [ClaudeNativeFallbackOption],
        currentModel: String,
        id: UUID = UUID()
    ) {
        let selectedModel = initialClaudeNativeFallbackSelection(
            options: options,
            currentModel: currentModel)
        guard !options.isEmpty else { return nil }
        self.id = id
        self.selectedModel = selectedModel
    }
}

struct ClaudeNativeFallbackEditorView: View {
    let options: [ClaudeNativeFallbackOption]
    let currentModel: String
    let onCancel: () -> Void
    let onSave: (String) -> Void

    @State private var selectedModel: String

    init(
        options: [ClaudeNativeFallbackOption],
        presentedModel: String,
        currentModel: String,
        onCancel: @escaping () -> Void,
        onSave: @escaping (String) -> Void
    ) {
        self.options = options
        self.currentModel = currentModel
        self.onCancel = onCancel
        self.onSave = onSave
        _selectedModel = State(initialValue:
            initialClaudeNativeFallbackSelection(
                options: options,
                currentModel: presentedModel))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            VStack(alignment: .leading, spacing: 6) {
                Text("Choose a fallback model")
                    .font(.title3.weight(.semibold))
                    .accessibilityAddTraits(.isHeader)
                Text("Select the Claude model to use when requests fall back from Baseten.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }

            ScrollView {
                Picker("Fallback model", selection: $selectedModel) {
                    ForEach(options, id: \.model) { option in
                        Text(option.label)
                            .tag(option.model)
                    }
                }
                .pickerStyle(.radioGroup)
                .labelsHidden()
                .frame(maxWidth: .infinity, alignment: .leading)
                .accessibilityLabel("Fallback model")
                .accessibilityIdentifier("claude-fallback-target-model")
            }
            .frame(maxHeight: 220)
            .padding(12)
            .background(
                Color.primary.opacity(0.045),
                in: RoundedRectangle(cornerRadius: 8))

            if let detail = claudeNativeFallbackSelectionDetail(
                selectedModel: selectedModel,
                currentModel: currentModel
            ) {
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }

            HStack(spacing: 8) {
                Spacer()
                Button("Cancel", action: onCancel)
                    .keyboardShortcut(.cancelAction)
                Button("Save") {
                    onSave(selectedModel)
                }
                .buttonStyle(.borderedProminent)
                .keyboardShortcut(.defaultAction)
                .disabled(
                    !isAcceptedClaudeNativeModelID(selectedModel)
                        || selectedModel == currentModel)
                .accessibilityIdentifier("claude-fallback-target-save")
            }
        }
        .padding(20)
        .frame(width: 440)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("claude-fallback-target-editor")
    }
}

func claudeNativeFallbackOptions(
    client: ClientStatus
) -> [ClaudeNativeFallbackOption] {
    var options: [ClaudeNativeFallbackOption] = []
    for available in client.basetenModelFallback?.availableModels ?? []
    where isAcceptedClaudeNativeModelID(available.model) {
        options.append(ClaudeNativeFallbackOption(
            label: available.displayName,
            model: available.model))
    }
    var seen = Set<String>()
    return options.filter { option in
        seen.insert(option.model).inserted
    }
}

struct PendingClaudeModelPickerMutation: Equatable, Sendable {
    let operationID: String
    let kind: ClaudeModelPickerMutationKind
    var phase: MutationPhase
}

private enum ClaudeModelPickerConfirmation {
    case convertReplacementMode
    case chooseAlias(slug: String, choices: [ClaudeModelPickerRow])
}

enum ClaudeModelPickerEnableDecision: Equatable, Sendable {
    case enableDirectly
    case confirmReplacementModeConversion
}

func claudeModelPickerEnableDecision(
    replacementMode: String?
) -> ClaudeModelPickerEnableDecision {
    replacementMode == "replace"
        ? .confirmReplacementModeConversion
        : .enableDirectly
}

struct ClaudeModelPickerEnablePreview: Equatable, Sendable {
    let models: [ClaudeModelPickerRow]

    init(models: [ClaudeModelPickerRow]) {
        self.models = models
    }

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any],
              let rawModels = dict["models"] as? [[String: Any]] else {
            return nil
        }
        let models = rawModels.compactMap(ClaudeModelPickerRow.init)
        guard !models.isEmpty, models.count == rawModels.count else {
            return nil
        }
        self.models = models
    }
}

struct ClaudeModelPickerContextMinimumFailure: Equatable, Sendable {
    static let code = "context_window_below_claude_picker_minimum"

    let message: String

    init?(json: String, status: Int32) {
        guard status != 0,
              let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any],
              dict["ok"] as? Bool == false,
              let error = dict["error"] as? [String: Any],
              error["code"] as? String == Self.code,
              error["retryable"] as? Bool == false,
              let rawMessage = error["message"] as? String else {
            return nil
        }
        let message = rawMessage.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !message.isEmpty else { return nil }
        self.message = message
    }
}

struct ClaudeModelPickerRow: Equatable, Identifiable, Sendable {
    let alias: String
    let slug: String
    let label: String
    let description: String
    let contextTokens: Int64

    var id: String { alias }

    init?(dict: [String: Any]) {
        guard let alias = dict["alias"] as? String,
              !alias.isEmpty,
              let slug = dict["slug"] as? String,
              !slug.isEmpty,
              let label = dict["label"] as? String,
              !label.isEmpty,
              let description = dict["description"] as? String,
              !description.isEmpty else {
            return nil
        }
        self.alias = alias
        self.slug = slug
        self.label = label
        self.description = description
        self.contextTokens = Int64(dict["context_tokens"] as? Int ?? 0)
    }
}

struct ClaudeModelPickerStatus: Equatable, Sendable {
    let enabled: Bool
    let models: [ClaudeModelPickerRow]

    init?(dict: [String: Any]) {
        guard let enabled = dict["enabled"] as? Bool,
              let rawModels = dict["models"] as? [[String: Any]] else {
            return nil
        }
        let models = rawModels.compactMap(ClaudeModelPickerRow.init)
        guard models.count == rawModels.count else { return nil }
        self.enabled = enabled
        self.models = models
    }
}

struct ClaudeModelPickerDiagnostics: Equatable, Sendable {
    let enabled: Bool
    let configuration: String
    let userFileSync: String
    let knownPolicy: String
    let allowlistPolicy: String
    let managedPolicy: String
    let replacementMode: String
    let runtimeVerification: String
    let configuredRows: Int
    let installedRows: Int
    let legacyDiscoveryEnabled: Bool
    let savedModel: String?
    let savedModelUnconfigured: Bool
    let savedModelContextMismatch: Bool
    let message: String

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any],
              let enabled = dict["enabled"] as? Bool,
              let configuration = dict["configuration"] as? String,
              !configuration.isEmpty,
              let userFileSync = dict["user_file_sync"] as? String,
              !userFileSync.isEmpty,
              let knownPolicy = dict["known_policy"] as? String,
              !knownPolicy.isEmpty,
              let allowlistPolicy = dict["allowlist_policy"] as? String,
              !allowlistPolicy.isEmpty,
              let managedPolicy = dict["managed_policy"] as? String,
              !managedPolicy.isEmpty,
              let replacementMode = dict["replacement_mode"] as? String,
              !replacementMode.isEmpty,
              let runtimeVerification =
                dict["runtime_verification"] as? String,
              !runtimeVerification.isEmpty,
              let configuredRows = dict["configured_rows"] as? Int,
              configuredRows >= 0,
              let installedRows = dict["installed_rows"] as? Int,
              installedRows >= 0,
              let legacyDiscoveryEnabled =
                dict["legacy_discovery_enabled"] as? Bool else {
            return nil
        }
        self.enabled = enabled
        self.configuration = configuration
        self.userFileSync = userFileSync
        self.knownPolicy = knownPolicy
        self.allowlistPolicy = allowlistPolicy
        self.managedPolicy = managedPolicy
        self.replacementMode = replacementMode
        self.runtimeVerification = runtimeVerification
        self.configuredRows = configuredRows
        self.installedRows = installedRows
        self.legacyDiscoveryEnabled = legacyDiscoveryEnabled
        savedModel = dict["saved_model"] as? String
        savedModelUnconfigured =
            dict["saved_model_unconfigured"] as? Bool ?? false
        savedModelContextMismatch =
            dict["saved_model_context_mismatch"] as? Bool ?? false
        message = dict["message"] as? String ?? ""
    }
}

func claudeModelPickerDiagnosticLabel(
    key: String,
    value: String
) -> String {
    switch (key, value) {
    case ("sync", "synced"):
        return "Synced"
    case ("sync", "out_of_sync"):
        return "Out of sync"
    case ("policy", "no_known_conflict"):
        return "No known conflict"
    case ("policy", "possible_allowlist_conflict"):
        return "Possible allowlist conflict"
    case ("policy", "possible_conflict"):
        return "Possible conflict"
    case ("policy", "blocked"):
        return "Blocked"
    case ("runtime", "unverified"):
        return "Unverified"
    default:
        return value.replacingOccurrences(of: "_", with: " ")
            .localizedCapitalized
    }
}

func claudeModelPickerConfiguredRowState(
    allowlistPolicy: String,
    knownPolicy: String
) -> String {
    if allowlistPolicy == "possible_conflict"
        || knownPolicy == "possible_allowlist_conflict" {
        return "Possible allowlist conflict"
    }
    return "Runtime unverified"
}

func claudeModelPickerCanRetrySync(
    userFileSync: String?,
    canEditConfiguredRows: Bool,
    hasConfiguredPicker: Bool
) -> Bool {
    userFileSync == "out_of_sync"
        && canEditConfiguredRows
        && hasConfiguredPicker
}

func claudeModelPickerCanAddModels(
    canEditConfiguredRows: Bool,
    modelCatalogAllowsMutation: Bool,
    pickerEnabled: Bool
) -> Bool {
    canEditConfiguredRows
        && modelCatalogAllowsMutation
        && pickerEnabled
}

let claudeModelPickerRestartNotice =
    "Restart Claude Code to see additions or removals."

func claudeModelPickerRemoveMutation(
    alias: String
) -> ClaudeModelPickerMutationKind {
    .remove(alias: alias)
}

struct ClaudeModelPickerProjection: Equatable {
    let configured: [ClaudeModelPickerRow]
    let availableToAdd: [LiveModelCatalogEntry]
}

func projectClaudeModelPicker(
    status: ClaudeModelPickerStatus?,
    liveState: LiveModelCatalogLoadState
) -> ClaudeModelPickerProjection {
    let configured = status?.models ?? []
    guard case .ready(let liveModels) = liveState else {
        return ClaudeModelPickerProjection(
            configured: configured,
            availableToAdd: [])
    }

    let selectedSlugs = Set(configured.map(\.slug))
    var seenSlugs = Set<String>()
    let available = liveModels
        .filter { model in
            !selectedSlugs.contains(model.slug)
                && seenSlugs.insert(model.slug).inserted
        }
        .sorted { lhs, rhs in
            let labelOrder = lhs.displayLabel.localizedCaseInsensitiveCompare(
                rhs.displayLabel)
            if labelOrder != .orderedSame {
                return labelOrder == .orderedAscending
            }
            return lhs.slug < rhs.slug
        }
    return ClaudeModelPickerProjection(
        configured: configured,
        availableToAdd: available)
}

struct ClaudeModelPickerAddPreview: Equatable, Sendable {
    let slug: String
    let alias: String
    let label: String
    let description: String

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any],
              let alias = dict["alias"] as? String,
              !alias.isEmpty,
              let slug = dict["slug"] as? String,
              !slug.isEmpty,
              let label = dict["label"] as? String,
              !label.isEmpty,
              let description = dict["description"] as? String,
              !description.isEmpty else {
            return nil
        }
        self.slug = slug
        self.alias = alias
        self.label = label
        self.description = description
    }
}

func claudeModelPickerAddMutation(
    preview: ClaudeModelPickerAddPreview,
    explicitAlias: String?
) -> ClaudeModelPickerMutationKind {
    .add(slug: preview.slug, alias: explicitAlias)
}

enum ClaudeModelPickerAddPreviewOutcome: Equatable, Sendable {
    case preview(ClaudeModelPickerAddPreview, explicitAlias: String?)
    case aliasChoices(slug: String, choices: [ClaudeModelPickerRow])

    init?(json: String, status: Int32, explicitAlias: String?) {
        if status == 0, let preview = ClaudeModelPickerAddPreview(json: json) {
            self = .preview(preview, explicitAlias: explicitAlias)
            return
        }
        guard status == 1,
              explicitAlias == nil,
              let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any],
              dict["ok"] as? Bool == false,
              let slug = dict["slug"] as? String,
              !slug.isEmpty,
              let error = dict["error"] as? [String: Any],
              error["code"] as? String == "ambiguous_alias",
              error["retryable"] as? Bool == false,
              let rawChoices = dict["alias_choices"] as? [[String: Any]] else {
            return nil
        }
        let choices = rawChoices.compactMap(ClaudeModelPickerRow.init)
        guard choices.count > 1,
              choices.count == rawChoices.count,
              Set(choices.map(\.alias)).count == choices.count,
              choices.allSatisfy({ $0.slug == slug }) else {
            return nil
        }
        self = .aliasChoices(slug: slug, choices: choices)
    }
}

func claudeModelPickerAddPreviewErrorMessage(
    _ error: String?
) -> String {
    let message = error?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
    guard !message.isEmpty else {
        return "Switch could not generate an exact preview for this model. No changes were made."
    }
    return message
}

func claudeModelPickerMoveUpCommand(
    rows: [ClaudeModelPickerRow],
    index: Int
) -> [String]? {
    guard index > 0, rows.indices.contains(index) else { return nil }
    return [
        "move", rows[index].alias,
        "--before", rows[index - 1].alias,
    ]
}

func claudeModelPickerMoveDownCommand(
    rows: [ClaudeModelPickerRow],
    index: Int
) -> [String]? {
    guard rows.indices.contains(index), rows.indices.contains(index + 1) else {
        return nil
    }
    // Moving the following row before the current row is a one-step swap and
    // also supports moving the penultimate row to the final position.
    return [
        "move", rows[index + 1].alias,
        "--before", rows[index].alias,
    ]
}

func claudeModelPickerMutationConfirmed(
    _ kind: ClaudeModelPickerMutationKind,
    status: ClaudeModelPickerStatus?
) -> Bool {
    switch kind {
    case .enable:
        return status?.enabled == true
    case .add(let slug, let alias):
        return status?.models.contains(where: { row in
            if let alias { return row.alias == alias }
            return row.slug == slug
        }) == true
    case .remove(let alias):
        return status?.models.contains(where: { $0.alias == alias }) == false
    case .move(let alias, let before):
        guard let rows = status?.models,
              let aliasIndex = rows.firstIndex(where: { $0.alias == alias }),
              let beforeIndex = rows.firstIndex(where: {
                  $0.alias == before
              }) else { return false }
        return aliasIndex + 1 == beforeIndex
    case .sync:
        return status != nil
    }
}

func claudeModelPickerReceiptMatches(
    _ receipt: GlobalMutationReceipt?,
    operationID: String,
    kind: ClaudeModelPickerMutationKind
) -> Bool {
    guard let receipt else { return false }
    return receipt.operationID == operationID
        && receipt.operation == kind.receiptOperation
        && receipt.client == "claude-code"
        && receipt.key == "model_picker"
        && (kind.requestedTarget.isEmpty
            || receipt.requestedTarget == kind.requestedTarget)
}

func claudeModelPickerSuccessMessage(
    _ kind: ClaudeModelPickerMutationKind
) -> String {
    switch kind {
    case .enable:
        return "Enabled. Choose a Baseten model to add to /model."
    case .add:
        return "Added to /model."
    case .remove:
        return "Saved. The routing alias remains available to existing sessions."
    case .move:
        return "Saved. Reopen /model in Claude Code to verify the order."
    case .sync:
        return "Synced. Reopen /model in Claude Code to verify the configured models."
    }
}

struct ClaudeModelPickerSectionView: View {
    @ObservedObject var state: BasetenSwitchState
    let client: ClientStatus
    let isPreview: Bool

    @State private var searchText = ""
    @State private var confirmation: ClaudeModelPickerConfirmation?
    @State private var addPreviewError: String?
    @State private var previewingSlug: String?
    @State private var fallbackTargetEditorDraft:
        ClaudeNativeFallbackEditorDraft?
    var body: some View {
        RoutingSectionCard {
            HStack {
                Label("Claude Code /model", systemImage: "list.bullet")
                    .font(.headline)
                Spacer()
                if let pending = state.pendingClaudeModelPicker {
                    ProgressView()
                        .controlSize(.small)
                        .accessibilityLabel(pending.kind.progressLabel)
                }
            }
        } content: {
            VStack(alignment: .leading, spacing: 16) {
                Text("Claude's built-in models stay current automatically. Choose which Baseten models are appended to /model.")
                    .font(.caption)
                    .foregroundStyle(.secondary)

                pickerStateCallout

                diagnosticsSection

                basetenFallbackSection
                Divider()
                configuredSection
                Divider()
                availableSection

                if let notice = state.claudeModelPickerNotice {
                    Label(notice, systemImage: "checkmark.circle.fill")
                        .font(.caption)
                        .foregroundStyle(.green)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("claude-picker-saved")
                }
                ForEach(
                    Array(state.claudeModelPickerWarnings.enumerated()),
                    id: \.offset
                ) { _, warning in
                    Label(
                        warning,
                        systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("claude-picker-warning")
                }
                if let reason = state.claudeModelPickerMutationDisabledReason,
                   state.pendingClaudeModelPicker == nil,
                   !isPreview {
                    Text(reason)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        .accessibilityIdentifier("claude-model-picker")
        .confirmationDialog(
            confirmationTitle,
            isPresented: Binding(
                get: { confirmation != nil },
                set: { if !$0 { confirmation = nil } }),
            titleVisibility: .visible
        ) {
            confirmationActions
        } message: {
            switch confirmation {
            case .convertReplacementMode:
                Text("Claude Code currently replaces its built-in model list. Enabling the picker will keep those models available and append any Baseten models you add.")
            case .chooseAlias:
                Text("More than one configured alias routes to this model. Choose the exact alias to add.")
            case nil:
                EmptyView()
            }
        }
        .sheet(item: $fallbackTargetEditorDraft) { draft in
            ClaudeNativeFallbackEditorView(
                options: fallbackTargetOptions,
                presentedModel: draft.selectedModel,
                currentModel: fallbackProjection?.resolvedModel ?? "",
                onCancel: {
                    fallbackTargetEditorDraft = nil
                },
                onSave: { target in
                    fallbackTargetEditorDraft = nil
                    state.requestBasetenModelFallback(
                        client: client.name,
                        model: target)
                })
        }
    }

    private var basetenFallbackSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline, spacing: 10) {
                VStack(alignment: .leading, spacing: 3) {
                    Text("Fallback for Baseten models")
                        .font(.subheadline.weight(.semibold))
                    Text(displayedFallbackTarget)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                Spacer()
                Text(displayedFallbackName)
                    .font(.callout.weight(.medium))
                if state.pendingBasetenModelFallback != nil {
                    ProgressView()
                        .controlSize(.small)
                        .accessibilityLabel("Changing fallback target")
                }
                Button("Change") {
                    guard !isPreview else { return }
                    fallbackTargetEditorDraft =
                        ClaudeNativeFallbackEditorDraft(
                            options: fallbackTargetOptions,
                            currentModel:
                                fallbackProjection?.resolvedModel ?? "")
                }
                .disabled(!canEditFallbackTarget)
                .accessibilityIdentifier("claude-fallback-target-change")
            }
            Text("Used when a Baseten model returns a rate limit or server error and Automatic Fallback is enabled.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if let detail = claudeNativeFallbackCatalogDetail(
                options: fallbackTargetOptions
            ) {
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier(
                        "claude-fallback-target-catalog-unavailable")
            }
            Text(fallbackTargetStatusLine)
                .font(.caption.weight(.medium))
                .foregroundStyle(fallbackTargetStatusColor)
                .accessibilityIdentifier("claude-fallback-target-status")
            if let detail = fallbackTargetReadinessDetail {
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .accessibilityIdentifier("claude-baseten-model-fallback")
    }

    private var fallbackProjection: BasetenModelFallbackStatus? {
        client.basetenModelFallback
    }

    private var fallbackTargetOptions: [ClaudeNativeFallbackOption] {
        claudeNativeFallbackOptions(client: client)
    }

    private var displayedFallbackTarget: String {
        if let pending = state.pendingBasetenModelFallback,
           pending.client == client.name {
            return pending.requestedModel
        }
        return fallbackProjection?.resolvedModel.isEmpty == false
            ? fallbackProjection?.resolvedModel ?? ""
            : "Not configured"
    }

    private var displayedFallbackName: String {
        if state.pendingBasetenModelFallback?.client == client.name {
            return "Changing"
        }
        let label = fallbackProjection?.displayName ?? ""
        return label.isEmpty ? "Unavailable" : label
    }

    private var canEditFallbackTarget: Bool {
        !isPreview
            && fallbackProjection != nil
            && !fallbackTargetOptions.isEmpty
            && state.canMutateFallbackSettings
    }

    private var fallbackTargetStatusLine: String {
        basetenFallbackTargetStatusLine(
            fallback: fallbackProjection,
            policy: state.confirmedFallbackPolicy,
            desiredMatchesActive:
                state.routingSnapshot?.desiredMatchesActive == true,
            supportsFallbackPolicy:
                state.routingSnapshot?.supportsFallbackPolicy == true)
    }

    private var fallbackTargetReadinessDetail: String? {
        basetenFallbackTargetReadinessDetail(
            fallback: fallbackProjection,
            desiredMatchesActive:
                state.routingSnapshot?.desiredMatchesActive == true,
            supportsFallbackPolicy:
                state.routingSnapshot?.supportsFallbackPolicy == true)
    }

    private var fallbackTargetStatusColor: Color {
        fallbackProjection?.ready == true
            && state.routingSnapshot?.desiredMatchesActive == true
            ? Color(nsColor: AppColors.basetenGreen)
            : .orange
    }

    @ViewBuilder
    private var diagnosticsSection: some View {
        if state.claudeModelPickerDiagnosticsLoading {
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Text("Checking Claude picker status…")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        } else if let diagnostics = state.claudeModelPickerDiagnostics {
            VStack(alignment: .leading, spacing: 6) {
                diagnosticRow(
                    "Configuration",
                    claudeModelPickerDiagnosticLabel(
                        key: "configuration",
                        value: diagnostics.configuration))
                diagnosticRow(
                    "Claude settings",
                    claudeModelPickerDiagnosticLabel(
                        key: "sync",
                        value: diagnostics.userFileSync))
                diagnosticRow(
                    "Overall policy",
                    claudeModelPickerDiagnosticLabel(
                        key: "policy",
                        value: diagnostics.knownPolicy))
                diagnosticRow(
                    "Allowlist policy",
                    claudeModelPickerDiagnosticLabel(
                        key: "policy",
                        value: diagnostics.allowlistPolicy))
                diagnosticRow(
                    "Managed policy",
                    claudeModelPickerDiagnosticLabel(
                        key: "policy",
                        value: diagnostics.managedPolicy))
                diagnosticRow(
                    "Picker mode",
                    claudeModelPickerDiagnosticLabel(
                        key: "replacement",
                        value: diagnostics.replacementMode))
                diagnosticRow(
                    "Runtime visibility",
                    claudeModelPickerDiagnosticLabel(
                        key: "runtime",
                        value: diagnostics.runtimeVerification))
                if !diagnostics.message.isEmpty {
                    Label(
                        diagnostics.message,
                        systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if diagnostics.savedModelUnconfigured,
                   let savedModel = diagnostics.savedModel {
                    Label(
                        "Claude Code's saved default, \(savedModel), is not in the curated picker list.",
                        systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if diagnostics.savedModelContextMismatch,
                   let savedModel = diagnostics.savedModel {
                    Label(
                        "Claude Code's saved default, \(savedModel), still requests 1M context. Its configured picker row now uses the 200K context bucket.",
                        systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if canRetryModelPickerSync {
                    HStack {
                        Spacer()
                        Button("Retry sync") {
                            state.requestClaudeModelPicker(
                                .sync(convertReplacementMode: false))
                        }
                        .accessibilityIdentifier("claude-picker-retry-sync")
                    }
                }
            }
            .padding(10)
            .background(
                Color.secondary.opacity(0.06),
                in: RoundedRectangle(cornerRadius: 8))
            .accessibilityIdentifier("claude-picker-diagnostics")
        } else if let error = state.claudeModelPickerDiagnosticsError {
            Label(error, systemImage: "exclamationmark.triangle.fill")
                .font(.caption)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    private func diagnosticRow(_ label: String, _ value: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label)
                .foregroundStyle(.secondary)
            Spacer()
            Text(value)
                .multilineTextAlignment(.trailing)
        }
        .font(.caption)
    }

    @ViewBuilder
    private var pickerStateCallout: some View {
        if client.modelPicker == nil {
            HStack(alignment: .center, spacing: 12) {
                VStack(alignment: .leading, spacing: 3) {
                    Text("Enable the Claude Code model picker")
                        .font(.body.weight(.medium))
                    Text("Then choose which Baseten models to add. Claude's built-in models stay available.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                Button("Enable Model Picker") {
                    enableModelPicker()
                }
                .buttonStyle(.borderedProminent)
                .disabled(!canEditConfiguredRows)
                .accessibilityIdentifier("claude-picker-enable")
            }
            .padding(12)
            .background(
                Color.accentColor.opacity(0.08),
                in: RoundedRectangle(cornerRadius: 8))
        } else if client.modelPicker?.enabled == false {
            HStack(spacing: 10) {
                Label(
                    "The saved picker rows are not currently installed in Claude Code.",
                    systemImage: "pause.circle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
                Spacer()
                Button("Enable") {
                    enableModelPicker()
                }
                .disabled(!canEditConfiguredRows)
                .accessibilityIdentifier("claude-picker-enable")
            }
        }
    }

    private var configuredSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Configured for /model")
                .font(.subheadline.weight(.semibold))
            Text(claudeModelPickerRestartNotice)
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if projection.configured.isEmpty {
                Text("No Baseten models are configured for /model.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .accessibilityIdentifier("claude-picker-configured-empty")
            } else {
                ForEach(Array(projection.configured.enumerated()), id: \.element.id) {
                    index, row in
                    configuredRow(row, index: index)
                    if index < projection.configured.count - 1 {
                        Divider()
                    }
                }
            }
        }
    }

    private func configuredRow(
        _ row: ClaudeModelPickerRow,
        index: Int
    ) -> some View {
        HStack(alignment: .center, spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text(rowDisplayLabel(row))
                    .font(.body.weight(.medium))
                Text(row.description)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(row.alias)
                    .font(.caption2.monospaced())
                    .foregroundStyle(.tertiary)
                    .textSelection(.enabled)
                Text(configuredRowState)
                    .font(.caption2)
                    .foregroundStyle(
                        configuredRowHasAllowlistConflict
                            ? .orange
                            : .secondary)
            }
            Spacer(minLength: 12)
            Button {
                moveUp(index: index)
            } label: {
                Image(systemName: "arrow.up")
            }
            .buttonStyle(.borderless)
            .disabled(!canEditConfiguredRows || index == 0)
            .help("Move up")
            .accessibilityLabel("Move \(rowDisplayLabel(row)) up")
            .accessibilityIdentifier("claude-picker-move-up-\(row.alias)")

            Button {
                moveDown(index: index)
            } label: {
                Image(systemName: "arrow.down")
            }
            .buttonStyle(.borderless)
            .disabled(
                !canEditConfiguredRows
                    || index == projection.configured.count - 1)
            .help("Move down")
            .accessibilityLabel("Move \(rowDisplayLabel(row)) down")
            .accessibilityIdentifier("claude-picker-move-down-\(row.alias)")

            Button("Remove") {
                guard !isPreview else { return }
                state.requestClaudeModelPicker(
                    claudeModelPickerRemoveMutation(alias: row.alias))
            }
            .disabled(!canEditConfiguredRows)
            .accessibilityIdentifier("claude-picker-remove-\(row.alias)")
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .contain)
    }

    private var availableSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Available to add")
                .font(.subheadline.weight(.semibold))
            if client.modelPicker?.enabled != true {
                Text("Enable the model picker before adding models.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            TextField("Search models", text: $searchText)
                .textFieldStyle(.roundedBorder)
                .accessibilityIdentifier("claude-picker-search")

            if let addPreviewError {
                Label(
                    addPreviewError,
                    systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier(
                        "claude-picker-add-preview-error")
            }

            switch state.liveModelCatalogState {
            case .idle, .loading:
                HStack(spacing: 8) {
                    ProgressView().controlSize(.small)
                    Text("Loading Baseten Model APIs…")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            case .signedOut(let reason):
                Label(
                    liveModelCatalogSignedOutMessage(reason),
                    systemImage: "key.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
            case .error:
                Label(
                    "Available models could not be loaded. Configured rows remain editable.",
                    systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.orange)
            case .ready:
                if filteredAvailable.isEmpty {
                    Text(searchText.isEmpty
                        ? "Every available model is already configured."
                        : "No available models match your search.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(filteredAvailable, id: \.slug) { model in
                        availableRow(model)
                    }
                }
            }
        }
    }

    private func availableRow(
        _ model: LiveModelCatalogEntry
    ) -> some View {
        HStack(alignment: .center, spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text(model.displayLabel)
                    .font(.body.weight(.medium))
                Text(model.slug)
                    .font(.caption2.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            Spacer(minLength: 12)
            if previewingSlug == model.slug {
                ProgressView()
                    .controlSize(.small)
                    .accessibilityLabel(
                        "Previewing \(model.displayLabel)")
            }
            Button("Add") {
                beginAddPreview(slug: model.slug, alias: nil)
            }
            .disabled(!canAddModels || previewingSlug != nil)
            .accessibilityIdentifier("claude-picker-add-\(model.slug)")
        }
        .padding(.vertical, 3)
    }

    private var projection: ClaudeModelPickerProjection {
        state.claudeModelPickerProjection(for: client)
    }

    private var filteredAvailable: [LiveModelCatalogEntry] {
        let query = searchText.trimmingCharacters(
            in: .whitespacesAndNewlines)
        guard !query.isEmpty else { return projection.availableToAdd }
        return projection.availableToAdd.filter {
            $0.displayLabel.localizedCaseInsensitiveContains(query)
                || $0.slug.localizedCaseInsensitiveContains(query)
        }
    }

    private var canEditConfiguredRows: Bool {
        !isPreview
            && state.canMutateClaudeModelPicker
            && state.pendingClaudeModelPicker == nil
            && state.claudeModelPickerDiagnostics != nil
            && state.claudeModelPickerDiagnostics?.userFileSync != "blocked"
            && state.claudeModelPickerDiagnostics?.replacementMode != "blocked"
    }

    private var canAddModels: Bool {
        claudeModelPickerCanAddModels(
            canEditConfiguredRows: canEditConfiguredRows,
            modelCatalogAllowsMutation:
                state.modelCatalogAllowsMutation,
            pickerEnabled: client.modelPicker?.enabled == true)
    }

    private var canRetryModelPickerSync: Bool {
        claudeModelPickerCanRetrySync(
            userFileSync:
                state.claudeModelPickerDiagnostics?.userFileSync,
            canEditConfiguredRows: canEditConfiguredRows,
            hasConfiguredPicker: client.modelPicker != nil)
    }

    private var configuredRowState: String {
        claudeModelPickerConfiguredRowState(
            allowlistPolicy:
                state.claudeModelPickerDiagnostics?.allowlistPolicy ?? "",
            knownPolicy:
                state.claudeModelPickerDiagnostics?.knownPolicy ?? "")
    }

    private var configuredRowHasAllowlistConflict: Bool {
        configuredRowState == "Possible allowlist conflict"
    }

    private var confirmationTitle: String {
        switch confirmation {
        case .convertReplacementMode:
            return "Keep Claude's built-in models?"
        case .chooseAlias:
            return "Choose an alias"
        case nil:
            return "Confirm model picker change"
        }
    }

    @ViewBuilder
    private var confirmationActions: some View {
        switch confirmation {
        case .convertReplacementMode:
            Button("Convert and Enable") {
                guard !isPreview else { return }
                state.requestClaudeModelPicker(
                    .enable(convertReplacementMode: true))
                confirmation = nil
            }
        case .chooseAlias(let slug, let choices):
            ForEach(choices, id: \.alias) { choice in
                Button(choice.alias) {
                    confirmation = nil
                    beginAddPreview(slug: slug, alias: choice.alias)
                }
            }
        case nil:
            EmptyView()
        }
        Button("Cancel", role: .cancel) { confirmation = nil }
    }

    private func enableModelPicker() {
        guard !isPreview else { return }
        switch claudeModelPickerEnableDecision(
            replacementMode:
                state.claudeModelPickerDiagnostics?.replacementMode
        ) {
        case .enableDirectly:
            state.requestClaudeModelPicker(
                .enable(convertReplacementMode: false))
        case .confirmReplacementModeConversion:
            confirmation = .convertReplacementMode
        }
    }

    private func beginAddPreview(slug: String, alias: String?) {
        guard !isPreview else { return }
        addPreviewError = nil
        previewingSlug = slug
        Task {
            let outcome = await state.previewClaudeModelPickerAdd(
                slug: slug,
                alias: alias)
            guard previewingSlug == slug else { return }
            previewingSlug = nil
            switch outcome {
            case .preview(let preview, let explicitAlias):
                state.requestClaudeModelPicker(
                    claudeModelPickerAddMutation(
                        preview: preview,
                        explicitAlias: explicitAlias))
            case .aliasChoices(let choiceSlug, let choices):
                confirmation = .chooseAlias(
                    slug: choiceSlug,
                    choices: choices)
            case nil:
                addPreviewError = claudeModelPickerAddPreviewErrorMessage(
                    state.lastError)
            }
        }
    }

    private func rowDisplayLabel(_ row: ClaudeModelPickerRow) -> String {
        row.label
    }

    private func moveUp(index: Int) {
        guard !isPreview,
              let command = claudeModelPickerMoveUpCommand(
                rows: projection.configured,
                index: index) else { return }
        state.requestClaudeModelPicker(.move(
            alias: command[1],
            before: command[3]))
    }

    private func moveDown(index: Int) {
        guard !isPreview,
              let command = claudeModelPickerMoveDownCommand(
                rows: projection.configured,
                index: index) else { return }
        state.requestClaudeModelPicker(.move(
            alias: command[1],
            before: command[3]))
    }
}
