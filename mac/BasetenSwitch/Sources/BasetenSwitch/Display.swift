import Foundation

// Display-only formatting, no route math. Flips shell out to
// `baseten-switch on|off`, which own route computation; this file only renders
// what /v1/admin/status reports.

/// Menubar icon state. Precedence: degraded > active > off.
/// - degraded: gateway up AND shared auth attention requires action OR any
///   enabled client reports an active fallback. Refresh failure remains
///   route-independent; signed-out attention is route-aware.
/// - active: gateway up AND any enabled client routes through
///   Baseten (route == "baseten").
/// - off: everything else, including gateway down with stale
///   fallback flags (a down gateway cannot be degraded).
/// Disabled clients never influence the icon.
/// Door-trip state is currently Overview-only. The menu-bar icon remains an
/// admin-status projection so its lightweight polling contract does not
/// inherit the full-app door probes.
enum MenubarIconState: Equatable {
    case off
    case active
    case degraded
}

/// auth defaults to nil (unknown), so an unavailable admin auth block does not
/// claim that reauthentication is required.
func menubarIconState(gatewayUp: Bool, clients: [ClientStatus],
                      auth: AuthStatus? = nil) -> MenubarIconState {
    guard gatewayUp else { return .off }
    if authAttention(
        gatewayUp: gatewayUp,
        globalRoutingEnabled: clients.contains(
            where: clientRequiresBasetenAuth),
        clients: clients,
        auth: auth) != .none {
        return .degraded
    }
    if clients.contains(where: { $0.enabled && $0.fallbackActive }) {
        return .degraded
    }
    if clients.contains(where: { $0.enabled && $0.effectiveRoute == "baseten" }) {
        return .active
    }
    return .off
}

/// Global routing icon projection. The global gate is authoritative.
func menubarIconState(gatewayUp: Bool,
                      globalRoutingEnabled: Bool,
                      clients: [ClientStatus],
                      auth: AuthStatus? = nil) -> MenubarIconState {
    guard gatewayUp else { return .off }
    if authAttention(
        gatewayUp: gatewayUp,
        globalRoutingEnabled: globalRoutingEnabled,
        clients: clients,
        auth: auth) != .none
        || clients.contains(where: { $0.enabled && $0.fallbackActive }) {
        return .degraded
    }
    return globalRoutingEnabled ? .active : .off
}

/// A selectable choice in a family's Model routing submenu. The three
/// classes mirror the family mapping targets: Native
/// (passthrough), a model_catalog entry (Baseten target), and Default
/// (remove the explicit family mapping). Equatable so the checkmark
/// lookup and the reselect-noop guard can compare choices directly.
enum FamilyChoice: Equatable {
    case native
    case defaultMapping
    case catalog(ModelCatalogEntry)
}

/// A selectable choice in the Subagents submenu: Inherit (subagents
/// follow the main thread's routing; the config wire value stays
/// "off", hence the case name), or a model_catalog entry. Equatable
/// for the checkmark lookup and the reselect-noop guard.
enum SubagentChoice: Equatable {
    case off
    case catalog(ModelCatalogEntry)
}

enum LiveModelCatalogLoadState: Equatable, Sendable {
    case idle
    case loading
    case ready([LiveModelCatalogEntry])
    case signedOut(LiveModelCatalogSignedOutReason)
    case error(String)
}

func liveModelCatalogSignedOutMessage(
    _ reason: LiveModelCatalogSignedOutReason
) -> String {
    switch reason {
    case .notSignedIn:
        return "Sign in to Baseten to load Model APIs."
    case .sessionExpired:
        return "Your Baseten session expired. Sign in again to load Model APIs."
    case .credentialRejected:
        return "Your Baseten API key was rejected. Update it and try again."
    }
}

struct ModelCatalogProjection: Equatable {
    let selectable: [ModelCatalogEntry]
}

/// Projects the authenticated Model API response into the picker list.
/// Configured entries contribute alias metadata only, so an existing saved
/// alias can still match its live model. Availability, labels, ordering, and
/// raw-slug dispatch all come from the live response.
func projectModelCatalog(
    configured: [ModelCatalogEntry],
    liveState: LiveModelCatalogLoadState
) -> ModelCatalogProjection {
    guard case .ready(let live) = liveState else {
        return ModelCatalogProjection(selectable: [])
    }

    var configuredBySlug: [String: ModelCatalogEntry] = [:]
    for configuredModel in configured {
        guard !configuredModel.slug.isEmpty,
              configuredBySlug[configuredModel.slug] == nil else {
            continue
        }
        configuredBySlug[configuredModel.slug] = configuredModel
    }

    var seenSlugs = Set<String>()
    var selectable: [ModelCatalogEntry] = []
    for liveModel in live {
        guard seenSlugs.insert(liveModel.slug).inserted else {
            continue
        }
        guard let projected = ModelCatalogEntry(dict: [
            "label": liveModel.displayLabel,
            "storage_target": liveModel.slug,
            "slug": liveModel.slug,
            "alias": configuredBySlug[liveModel.slug]?.alias ?? "",
            "available": true,
        ]) else { continue }
        selectable.append(projected)
    }

    return ModelCatalogProjection(selectable: selectable)
}

enum ReasoningChoice: Hashable, Sendable {
    case defaultPassthrough
    case pendingDefault
    case on
    case off
    case followHarness
    case effort(String)
    case unavailable(String)

    var policy: ReasoningPolicyValue? {
        switch self {
        case .defaultPassthrough, .pendingDefault,
             .followHarness, .effort, .unavailable:
            return nil
        case .on:
            return ReasoningPolicyValue(mode: .on)
        case .off:
            return ReasoningPolicyValue(mode: .off)
        }
    }
}

struct ReasoningDisplayRow: Identifiable, Equatable, Sendable {
    let provider: String
    let model: String
    let displayName: String
    let status: ClientReasoningStatus
    let capability: ReasoningCapability?
    let mappingFamilies: [String]

    var id: String { "\(provider):\(model)" }
}

func reasoningRowsForDisplay(
    client: ClientStatus,
    liveModels: [LiveModelCatalogEntry]
) -> [ReasoningDisplayRow] {
    let liveBySlug = Dictionary(
        liveModels.map { ($0.slug, $0) },
        uniquingKeysWith: { first, _ in first })
    let options = client.modelOptions["baseten"] ?? [:]
    var selectedModels = Set<String>()
    func select(_ target: String?) {
        guard let target, !target.isEmpty, target != "native" else {
            return
        }
        selectedModels.insert(
            canonicalModelID(target, catalog: client.modelCatalog))
    }
    if client.families.isEmpty {
        select(client.unmatchedNativeModel?.configuredTarget)
    }
    for family in client.families {
        select(family.configuredTarget)
    }
    if client.subagentRouting != "off",
       client.subagentEffective != "inherit" {
        select(client.subagentModel)
    }

    var seenModels = Set<String>()
    return options.compactMap { optionModel, option in
        let model = canonicalModelID(
            optionModel,
            catalog: client.modelCatalog)
        guard selectedModels.contains(model),
              let status = option.reasoning,
              seenModels.insert(model).inserted else {
            return nil
        }
        let configuredFamilies = client.families.compactMap {
            family -> String? in
            guard let target = family.configuredTarget,
                  canonicalModelID(target, catalog: client.modelCatalog)
                    == model else {
                return nil
            }
            return capitalizeFamily(family.family)
        }
        let displayName = liveBySlug[model]?.displayLabel
            ?? client.modelCatalog.first(where: { $0.slug == model })
                .map(modelDisplayLabel)
            ?? shortModelName(model)
        return ReasoningDisplayRow(
            provider: "baseten",
            model: model,
            displayName: displayName,
            status: status,
            capability: liveBySlug[model]?.reasoning,
            mappingFamilies: Array(Set(configuredFamilies)).sorted())
    }
    .sorted {
        $0.displayName.localizedCaseInsensitiveCompare($1.displayName)
            == .orderedAscending
    }
}

func reasoningChoices(
    status: ClientReasoningStatus
) -> [ReasoningChoice] {
    guard status.available,
          status.availableModes.contains(.on),
          status.availableModes.contains(.off) else {
        return []
    }
    return [.on, .off]
}

func reasoningUsesDefaultPassthroughReadOnlyState(
    _ status: ClientReasoningStatus
) -> Bool {
    guard status.available,
          status.configured.mode == .default,
          status.effective.mode == .passthrough,
          status.availableModes.isEmpty,
          status.availableEfforts.isEmpty else {
        return false
    }
    return true
}

func reasoningDefaultPassthroughReadOnlyLabel() -> String {
    "Uses provider default"
}

func reasoningShowsResetAction(
    _ status: ClientReasoningStatus
) -> Bool {
    status.configured.mode != .default
}

func reasoningSelection(
    status: ClientReasoningStatus,
    pending: ReasoningPolicyValue? = nil
) -> ReasoningChoice {
    let policy = pending
        ?? (status.configured.mode == .default
            ? status.effective
            : status.configured)
    switch policy.mode {
    case .on:
        return .on
    case .off:
        return .off
    case .followHarness:
        return .followHarness
    case .passthrough:
        return .defaultPassthrough
    case .fixed:
        if status.availableEfforts.contains(policy.effort) {
            return .effort(policy.effort)
        }
        return .unavailable(policy.effort)
    case .default:
        return .pendingDefault
    }
}

func reasoningChoiceLabel(
    _ choice: ReasoningChoice,
    clientName: String
) -> String {
    switch choice {
    case .defaultPassthrough:
        return "Default · Pass through unchanged"
    case .pendingDefault:
        return "Resetting to Safe Default…"
    case .on:
        return "On"
    case .off:
        return "Off"
    case .followHarness:
        return "Follow \(clientDisplayName(clientName)) (Legacy)"
    case .effort(let effort):
        return "\(reasoningEffortLabel(effort)) (Legacy)"
    case .unavailable(let value):
        return "\(reasoningEffortLabel(value)) (Unavailable)"
    }
}

func reasoningEffortLabel(_ effort: String) -> String {
    switch effort.lowercased() {
    case "xhigh":
        return "XHigh"
    case "none":
        return "Off"
    default:
        return effort.prefix(1).uppercased() + effort.dropFirst()
    }
}

func reasoningCaption(
    row: ReasoningDisplayRow,
    clientName: String
) -> String {
    let client = clientDisplayName(clientName)
    switch row.status.configured.mode {
    case .on:
        guard row.status.available else { return "Saved policy: On." }
        return "Switch enables reasoning. \(client) effort is forwarded unchanged."
    case .off:
        guard row.status.available else { return "Saved policy: Off." }
        return "Switch disables reasoning when \(client) uses this model."
    case .followHarness:
        return "Saved legacy policy: follow \(client)’s reasoning setting when supported."
    case .fixed:
        return "Saved legacy effort: \(reasoningEffortLabel(row.status.configured.effort))."
    case .default, .passthrough:
        if row.status.source == "compatibility_default",
           row.status.effective.mode == .off {
            return "Safe default: Off."
        }
        if row.status.effective.mode == .passthrough {
            return "Default: provider behavior is unchanged."
        }
        return "Used when \(client) routes to this Baseten model."
    }
}

func reasoningLegacySelectionLabel(
    _ selection: ReasoningChoice,
    clientName: String
) -> String? {
    switch selection {
    case .followHarness, .effort, .unavailable:
        return reasoningChoiceLabel(selection, clientName: clientName)
    case .defaultPassthrough, .pendingDefault, .on, .off:
        return nil
    }
}

private func canonicalModelID(
    _ target: String,
    catalog: [ModelCatalogEntry]
) -> String {
    catalog.first(where: {
        target == $0.target || target == $0.slug || target == $0.alias
    })?.slug ?? target
}

func formatUptime(_ seconds: Int64) -> String {
    let s = max(seconds, 0)
    let h = s / 3600
    let m = (s % 3600) / 60
    let sec = s % 60
    if h > 0 { return "\(h)h \(m)m" }
    if m > 0 { return "\(m)m \(sec)s" }
    return "\(sec)s"
}

/// Disabled status line for the top of the menu. Text alone carries
/// the state; the native menu style has no room for the colored dot.
func gatewayStatusLabel(up: Bool, uptimeSeconds: Int64) -> String {
    if up {
        return "Gateway: up \(formatUptime(uptimeSeconds))"
    }
    return "Gateway: not running"
}

/// Menu item text for a client Toggle ("name -> destination"). On the
/// baseten route show the resolved upstream model (or "Baseten (?)"
/// when the gateway has not resolved one, keeping the gap visible);
/// otherwise show the native route as reported by the gateway
/// (`route`, falling back to the status API's `native_route` when
/// route is empty). A client with an active fallback gets a
/// " (fallback active)" suffix on any route.
func clientRowLabel(name: String, route: String, nativeRoute: String,
                    selectedModel: String, fallbackActive: Bool = false) -> String {
    let suffix = fallbackActive ? " (fallback active)" : ""
    if route == "baseten" {
        if !selectedModel.isEmpty {
            return "\(name) -> \(selectedModel)\(suffix)"
        }
        return "\(name) -> Baseten (?)\(suffix)"
    }
    let native = route.isEmpty ? nativeRoute : route
    if native.isEmpty {
        return name + suffix
    }
    return "\(name) -> Native (\(native))\(suffix)"
}

/// Menu item text for the per-client subagent toggle. Renders only
/// when the gateway reports a non-empty subagent_model for the client.
/// Mirrors clientRowLabel's plain "prefix: value" shape so the row
/// reads as a sibling under the client row.
func subagentRowLabel(model: String) -> String {
    "Subagents: \(model)"
}

/// Menu item text for the per-client subagents submenu row. Shows the
/// configured override when one is active. Otherwise Claude Code keeps
/// control of the requested subagent model, and the gateway applies that
/// model's normal routing rule. The gateway wire value stays "off".
func subagentMenuRowLabel(model: String, routing: String) -> String {
    if routing != "off" && !model.isEmpty {
        return "Subagents: \(model)"
    }
    return "Subagents: Claude Code model"
}

/// Menu item text for one family row in the Model routing submenu. The
/// label shows the EFFECTIVE destination from the server's `families`
/// table, never re-derived in Swift. When effective_model is non-empty
/// the family is served by that Baseten model (alias or slug); when
/// empty, effective_route names the native route the family passes
/// through to (e.g. "anthropic"), shown as "Native (<route>)". A bare
/// empty effective_route falls back to "default" so the row is never
/// blank. `family` is capitalized for display ("opus" -> "Opus").
func familyRowLabel(family: String, effectiveRoute: String,
                    effectiveModel: String) -> String {
    let dest: String
    if !effectiveModel.isEmpty {
        dest = effectiveModel
    } else if !effectiveRoute.isEmpty {
        dest = "Native (\(effectiveRoute))"
    } else {
        dest = "default"
    }
    return "\(capitalizeFamily(family)): \(dest)"
}

/// Capitalize the family token for display. The server sends lowercase
/// family words (opus, sonnet, haiku, fable); the menu title-cases the
/// first letter only, so "opus" -> "Opus" and "claude-opus-4-8" (not a
/// family word, but defensive) is left untouched.
func capitalizeFamily(_ family: String) -> String {
    guard let first = family.first else { return family }
    return String(first).uppercased() + family.dropFirst()
}

/// The checkmark state for one choice in a family's submenu. Returns
/// true when the choice matches the family's current pin:
/// - "native" checks Native when pin == "native".
/// - a catalog target checks the entry whose target or slug matches
///   the pin (pin is an alias id or a raw slug).
/// - "default" checks Default when the explicit family pin is empty.
/// This is a pure lookup; no route math.
func familyChoiceChecked(pin: String, choice: FamilyChoice) -> Bool {
    switch choice {
    case .native:
        return pin == "native"
    case .defaultMapping:
        return pin.isEmpty
    case .catalog(let entry):
        return pin == entry.target || pin == entry.slug
    }
}

/// The CLI argument for a family route dispatch. "native", the catalog
/// entry's target (alias id or slug), or "default" to remove the pin.
/// Matches `baseten-switch claude route <family> <target|default>`.
func familyChoiceArg(_ choice: FamilyChoice) -> String {
    switch choice {
    case .native: return "native"
    case .defaultMapping: return "default"
    case .catalog(let entry): return entry.target
    }
}

/// The full argv for a family route dispatch:
/// ["claude", "route", family, choice]. The verb validates the family
/// and target against gateway.yaml, SIGHUPs, and verifies via admin
/// status; the app shells out and re-polls.
func familyDispatchArgs(client: String, family: String, choice: FamilyChoice) -> [String] {
    // client is unused in the dispatch today (claude route targets the
    // claude-code client implicitly, like claude subagents), but kept in
    // the signature so the call site is uniform with subagentDispatchArgs
    // and a future multi-client generalization needs no view changes.
    _ = client
    return ["claude", "route", family, familyChoiceArg(choice)]
}

/// The Codex profile keeps a compatibility-safe request model while this
/// command changes only the Baseten model selected by the gateway.
func codexRouteDispatchArgs(model: ModelCatalogEntry) -> [String] {
    ["codex", "route", model.slug]
}

/// The checkmark state for one choice in the subagents submenu. Returns
/// true when the choice matches the effective subagent state:
/// - Inherit (case .off) checks when routing is "off" (or no model
///   configured); subagents then follow the main thread's routing.
/// - a catalog target checks when routing is on AND subagent_model
///   matches the entry's storage target, slug, or configured alias.
/// Pure lookup; no route math.
func subagentChoiceChecked(subagentModel: String, subagentRouting: String,
                           choice: SubagentChoice) -> Bool {
    let routingOn = subagentRouting != "off" && !subagentModel.isEmpty
    switch choice {
    case .off:
        return !routingOn
    case .catalog(let entry):
        return routingOn
            && (subagentModel == entry.target
                || subagentModel == entry.slug
                || subagentModel == entry.alias)
    }
}

/// The CLI argument for a subagent dispatch: the catalog entry's target
/// (alias id or slug) to set the model, or "off" to return to inherit
/// (subagents follow the main thread; "off" is the CLI wire value the
/// verb accepts everywhere). Matches
/// `baseten-switch claude subagents <model>|off`.
func subagentChoiceArg(_ choice: SubagentChoice) -> String {
    switch choice {
    case .off: return "inherit"
    case .catalog(let entry): return entry.target
    }
}

/// The full argv for a subagent dispatch: ["claude", "subagents", target].
func subagentDispatchArgs(client: String, choice: SubagentChoice) -> [String] {
    _ = client
    return ["claude", "subagents", subagentChoiceArg(choice)]
}

/// POSIX single-quote shell quoting ('\'' splice for embedded quotes)
/// so an unusual binary path (spaces, quotes) cannot alter the
/// Terminal command it is spliced into.
func shellQuote(_ s: String) -> String {
    "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
}

/// An AppleScript double-quoted string literal (backslash, then quote
/// escaping, per the AppleScript text syntax).
func appleScriptStringLiteral(_ s: String) -> String {
    "\"" + s
        .replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"") + "\""
}

/// The osascript source for the Reauthenticate button: tell Terminal
/// to run the interactive device-flow login. The command is the fixed
/// verb "auth login" on the locally-resolved baseten-switch path; nothing
/// server-supplied ever reaches this string (admin payload fields are
/// rendered as text only, never spliced into commands), so the
/// command-injection surface through Terminal stays closed. The path
/// is shell-quoted, then the whole command AppleScript-escaped.
func reauthAppleScript(binaryPath: String) -> String {
    let command = shellQuote(binaryPath) + " auth login"
    return """
    tell application "Terminal"
        activate
        do script \(appleScriptStringLiteral(command))
    end tell
    """
}

/// One-line diagnostic text for compact summaries and menu tooltips:
/// newlines collapse to spaces and long details truncate with "...".
func menuErrorLabel(_ raw: String, limit: Int = 80) -> String {
    let oneLine = raw
        .components(separatedBy: .newlines)
        .joined(separator: " ")
        .trimmingCharacters(in: .whitespaces)
    guard oneLine.count > limit else { return oneLine }
    return String(oneLine.prefix(max(limit - 3, 1))) + "..."
}
