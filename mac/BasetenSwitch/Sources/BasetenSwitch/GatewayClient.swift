import Foundation

enum GatewayClientError: Error, Equatable {
    case badResponse(Int)
    case invalidPayload
}

// MARK: - Immutable routing status

struct RoutingToken: Equatable, Hashable, Sendable {
    let routerBootID: String
    let activeGeneration: UInt64

    var cliValue: String {
        "\(routerBootID):\(activeGeneration)"
    }

    var isAuthoritative: Bool {
        !routerBootID.isEmpty
    }
}

enum GatewayState: String, Equatable, Sendable {
    case ready
    case unavailable
}

struct RoutingSnapshot: Equatable, Sendable {
    let token: RoutingToken
    let activeConfigHash: String
    let desiredConfigHash: String
    let observedAt: Date
    let gateway: GatewayState
    let health: String
    let version: String
    let uptimeSeconds: Int64
    let configPath: String
    let capabilities: Set<String>
    let globalRoutingEnabled: Bool
    let fallbackPolicy: FallbackPolicyStatus?
    let reload: ReloadStatus
    let auth: AuthStatus?
    let clients: [ClientStatus]

    var supportsGlobalRouting: Bool {
        capabilities.contains("global_routing")
    }

    var supportsFallbackPolicy: Bool {
        capabilities.contains("fallback_policy")
    }

    var desiredMatchesActive: Bool {
        !activeConfigHash.isEmpty
            && activeConfigHash == desiredConfigHash
            && reload.state != "failed"
    }

    init(status: AdminStatusSnapshot, observedAt: Date) {
        token = status.token
        activeConfigHash = status.activeConfigHash
        desiredConfigHash = status.desiredConfigHash
        self.observedAt = observedAt
        gateway = .ready
        health = status.health
        version = status.version
        uptimeSeconds = status.uptimeSeconds
        configPath = status.configPath
        capabilities = Set(status.capabilities)
        globalRoutingEnabled = status.globalRoutingEnabled
        fallbackPolicy = status.fallbackPolicy
        reload = status.reload
        auth = status.auth
        clients = status.clients
    }

    init(observedAt: Date,
         version: String,
         uptimeSeconds: Int64,
         globalRoutingEnabled: Bool,
         capabilities: Set<String> = [],
         fallbackPolicy: FallbackPolicyStatus? = nil,
         auth: AuthStatus?,
         clients: [ClientStatus]) {
        token = RoutingToken(routerBootID: "", activeGeneration: 0)
        activeConfigHash = ""
        desiredConfigHash = ""
        self.observedAt = observedAt
        gateway = .ready
        health = "ready"
        self.version = version
        self.uptimeSeconds = uptimeSeconds
        configPath = ""
        self.capabilities = capabilities
        self.globalRoutingEnabled = globalRoutingEnabled
        self.fallbackPolicy = fallbackPolicy
        reload = ReloadStatus(state: "", error: "")
        self.auth = auth
        self.clients = clients
    }
}

struct ReloadStatus: Equatable, Sendable {
    var state: String
    var error: String

    init(state: String, error: String) {
        self.state = state
        self.error = error
    }

    init(dict: [String: Any]) {
        state = dict["state"] as? String ?? ""
        error = dict["error"] as? String ?? ""
    }
}

struct ClientStatus: Identifiable, Equatable, Sendable {
    let name: String
    var enabled: Bool
    var bindAddr: String
    var protocolShape: String
    /// Runtime destination resolved by the gateway.
    var effectiveRoute: String
    var nativeRoute: String
    var authSet: Bool
    var currentlyBound: Bool
    var effectiveSummary: String
    var fallback: FallbackStatus
    var subagentModel: String
    var subagentRouting: String
    var subagentEffective: String
    var unmatchedNativeModel: ModelResolution?
    var families: [FamilyEntry]
    var modelCatalog: [ModelCatalogEntry]
    var modelPicker: ClaudeModelPickerStatus?
    var basetenModelFallback: BasetenModelFallbackStatus?
    var modelOptions: ClientModelOptions

    var id: String { name }
    var fallbackActive: Bool { fallback.active }

    init?(dict: [String: Any]) {
        guard let name = dict["name"] as? String else { return nil }
        self.name = name
        enabled = dict["enabled"] as? Bool ?? false
        bindAddr = dict["bind_addr"] as? String ?? ""
        protocolShape = dict["protocol_shape"] as? String ?? ""
        effectiveRoute = dict["effective_route"] as? String ?? ""
        nativeRoute = dict["native_route"] as? String ?? ""
        authSet = dict["auth_set"] as? Bool ?? false
        currentlyBound = dict["currently_bound"] as? Bool ?? false
        effectiveSummary = dict["effective_summary"] as? String ?? ""
        if let fallbackDict = dict["fallback"] as? [String: Any] {
            fallback = FallbackStatus(dict: fallbackDict)
        } else {
            fallback = FallbackStatus(
                active: false,
                servedRoute: "",
                cause: "")
        }
        subagentModel = dict["subagent_model"] as? String ?? ""
        subagentRouting = dict["subagent_routing"] as? String ?? ""
        subagentEffective = dict["subagent_effective"] as? String ?? ""
        unmatchedNativeModel = (dict["unmatched_native_model"] as? [String: Any])
            .map(ModelResolution.init)

        let familyArray = dict["families"] as? [[String: Any]] ?? []
        families = familyArray.compactMap(FamilyEntry.init)
        let catalogArray = dict["model_catalog"] as? [[String: Any]] ?? []
        modelCatalog = catalogArray.compactMap(ModelCatalogEntry.init)
        modelPicker = (dict["model_picker"] as? [String: Any])
            .flatMap(ClaudeModelPickerStatus.init)
        basetenModelFallback = (dict["baseten_model_fallback"]
            as? [String: Any])
            .flatMap(BasetenModelFallbackStatus.init)
        modelOptions = decodeClientModelOptions(dict["model_options"])
    }
}

struct FallbackPolicyStatus: Equatable, Sendable {
    let onBaseten429: Bool
    let onBaseten5xx: Bool

    init(onBaseten429: Bool, onBaseten5xx: Bool) {
        self.onBaseten429 = onBaseten429
        self.onBaseten5xx = onBaseten5xx
    }

    init?(dict: [String: Any]) {
        guard let onBaseten429 = dict["on_baseten_429"] as? Bool,
              let onBaseten5xx = dict["on_baseten_5xx"] as? Bool else {
            return nil
        }
        self.onBaseten429 = onBaseten429
        self.onBaseten5xx = onBaseten5xx
    }
}

struct BasetenModelFallbackStatus: Equatable, Sendable {
    let configuredModel: String
    let resolvedModel: String
    let displayName: String
    let providerReady: Bool
    let ready: Bool
    let reason: String?
    let availableModels: [BasetenModelFallbackOption]

    init?(dict: [String: Any]) {
        guard let configuredModel = dict["configured_model"] as? String,
              let resolvedModel = dict["resolved_model"] as? String,
              let displayName = dict["display_name"] as? String,
              let providerReady = dict["provider_ready"] as? Bool,
              let ready = dict["ready"] as? Bool else {
            return nil
        }
        self.configuredModel = configuredModel
        self.resolvedModel = resolvedModel
        self.displayName = displayName
        self.providerReady = providerReady
        self.ready = ready
        reason = dict["reason"] as? String
        availableModels = decodeBasetenModelFallbackOptions(
            dict["available_models"])
    }
}

struct BasetenModelFallbackOption: Equatable, Sendable {
    let model: String
    let displayName: String

    init(model: String, displayName: String) {
        self.model = model
        self.displayName = displayName
    }

    init?(dict: [String: Any]) {
        guard let model = dict["model"] as? String,
              !model.isEmpty,
              let displayName = dict["display_name"] as? String,
              !displayName.isEmpty else {
            return nil
        }
        self.model = model
        self.displayName = displayName
    }
}

private func decodeBasetenModelFallbackOptions(
    _ raw: Any?
) -> [BasetenModelFallbackOption] {
    // Missing means an older router. A malformed additive projection fails
    // closed instead of presenting an unvalidated partial selector.
    guard let raw else { return [] }
    guard let rows = raw as? [[String: Any]] else { return [] }
    var decoded: [BasetenModelFallbackOption] = []
    for row in rows {
        guard let option = BasetenModelFallbackOption(dict: row),
              isAcceptedClaudeNativeModelID(option.model) else {
            return []
        }
        decoded.append(option)
    }
    return decoded
}

struct FallbackStatus: Equatable, Sendable {
    var active: Bool
    var servedRoute: String
    var cause: String

    init(active: Bool, servedRoute: String, cause: String) {
        self.active = active
        self.servedRoute = servedRoute
        self.cause = cause
    }

    init(dict: [String: Any]) {
        active = dict["active"] as? Bool ?? false
        servedRoute = dict["served_route"] as? String ?? ""
        cause = dict["cause"] as? String ?? ""
    }
}

struct ModelResolution: Equatable, Sendable {
    var configuredTarget: String?
    var effectiveRoute: String
    var effectiveModel: String
    var effectiveSource: String

    init(dict: [String: Any]) {
        configuredTarget = dict["configured_target"] as? String
        effectiveRoute = dict["effective_route"] as? String ?? ""
        effectiveModel = dict["effective_model"] as? String ?? ""
        effectiveSource = dict["effective_source"] as? String ?? ""
    }
}

/// One server-computed family row.
struct FamilyEntry: Equatable, Sendable {
    var family: String
    var configuredTarget: String?
    var configuredSource: String
    var effectiveRoute: String
    var effectiveModel: String
    var effectiveSource: String

    init?(dict: [String: Any]) {
        guard let family = dict["family"] as? String else { return nil }
        self.family = family
        configuredTarget = dict["configured_target"] as? String
        configuredSource = dict["configured_source"] as? String ?? ""

        effectiveRoute = dict["effective_route"] as? String ?? ""
        effectiveModel = dict["effective_model"] as? String ?? ""
        effectiveSource = dict["effective_source"] as? String ?? ""
    }
}

struct ModelCatalogEntry: Equatable, Sendable {
    var label: String
    /// Immutable storage target sent back to the CLI.
    var target: String
    var slug: String
    var alias: String
    var available: Bool

    init?(dict: [String: Any]) {
        guard let storageTarget = dict["storage_target"] as? String,
              !storageTarget.isEmpty else { return nil }
        target = storageTarget
        slug = dict["slug"] as? String ?? storageTarget
        alias = dict["alias"] as? String ?? ""
        label = dict["label"] as? String ?? ""
        available = dict["available"] as? Bool ?? true
    }
}

enum LiveModelCatalogResponseState: String, Equatable, Sendable {
    case ready
    case signedOut = "signed_out"
    case error
}

enum LiveModelCatalogSignedOutReason: String, Equatable, Sendable {
    case notSignedIn = "not_signed_in"
    case sessionExpired = "session_expired"
    case credentialRejected = "credential_rejected"
}

enum ReasoningOptionType: String, Equatable, Sendable {
    case toggle
    case effort
    case budgetTokens = "budget_tokens"
}

struct ReasoningOption: Equatable, Sendable {
    let type: ReasoningOptionType
    let values: [String?]
    let minimum: Int64?
    let maximum: Int64?

    init?(dict: [String: Any]) {
        guard let rawType = dict["type"] as? String,
              let type = ReasoningOptionType(rawValue: rawType) else {
            return nil
        }
        self.type = type
        switch type {
        case .toggle:
            guard dict["values"] == nil,
                  dict["min"] == nil,
                  dict["max"] == nil else { return nil }
            values = []
            minimum = nil
            maximum = nil
        case .effort:
            guard let rawValues = dict["values"] as? [Any],
                  dict["min"] == nil,
                  dict["max"] == nil else { return nil }
            var decoded: [String?] = []
            for value in rawValues {
                if value is NSNull {
                    decoded.append(nil)
                } else if let value = value as? String {
                    decoded.append(value)
                } else {
                    return nil
                }
            }
            values = decoded
            minimum = nil
            maximum = nil
        case .budgetTokens:
            guard dict["values"] == nil else { return nil }
            values = []
            minimum = (dict["min"] as? NSNumber)?.int64Value
            maximum = (dict["max"] as? NSNumber)?.int64Value
        }
    }
}

struct ReasoningCapability: Equatable, Sendable {
    let supported: Bool
    let options: [ReasoningOption]
    let source: String
    let loadedFrom: String
    let revision: String
    let capturedAt: String
    let stale: Bool

    init?(dict: [String: Any]) {
        guard let supported = dict["supported"] as? Bool,
              let rawOptions = dict["options"] as? [[String: Any]],
              let source = dict["source"] as? String,
              let loadedFrom = dict["loaded_from"] as? String,
              let revision = dict["revision"] as? String,
              let capturedAt = dict["captured_at"] as? String,
              let stale = dict["stale"] as? Bool else {
            return nil
        }
        let options = rawOptions.compactMap(ReasoningOption.init)
        guard options.count == rawOptions.count else { return nil }
        self.supported = supported
        self.options = options
        self.source = source
        self.loadedFrom = loadedFrom
        self.revision = revision
        self.capturedAt = capturedAt
        self.stale = stale
    }
}

struct LiveModelCatalogEntry: Equatable, Sendable {
    let slug: String
    let displayName: String
    let reasoning: ReasoningCapability?

    init?(dict: [String: Any]) {
        guard let slug = dict["slug"] as? String,
              !slug.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              let displayName = dict["display_name"] as? String else {
            return nil
        }
        self.slug = slug
        self.displayName = displayName
        if let rawReasoning = dict["reasoning"] {
            guard let reasoningDict = rawReasoning as? [String: Any],
                  let reasoning = ReasoningCapability(
                    dict: reasoningDict) else {
                return nil
            }
            self.reasoning = reasoning
        } else {
            reasoning = nil
        }
    }

    var displayLabel: String {
        displayName.isEmpty ? slug : displayName
    }
}

enum ReasoningPolicyMode: String, Equatable, Sendable {
    case `default`
    case off
    case followHarness = "follow_harness"
    case fixed
    case passthrough
}

struct ReasoningPolicyValue: Equatable, Sendable {
    let mode: ReasoningPolicyMode
    let effort: String

    init(mode: ReasoningPolicyMode, effort: String = "") {
        self.mode = mode
        self.effort = effort
    }

    init?(dict: [String: Any]) {
        guard let rawMode = dict["mode"] as? String,
              let mode = ReasoningPolicyMode(rawValue: rawMode) else {
            return nil
        }
        self.mode = mode
        effort = dict["effort"] as? String ?? ""
    }

    var jsonObject: [String: Any] {
        var object: [String: Any] = ["mode": mode.rawValue]
        if !effort.isEmpty {
            object["effort"] = effort
        }
        return object
    }
}

struct ClientReasoningStatus: Equatable, Sendable {
    let configured: ReasoningPolicyValue
    let effective: ReasoningPolicyValue
    let source: String
    let availableModes: [ReasoningPolicyMode]
    let availableEfforts: [String]
    let available: Bool
    let unavailableReason: String
    let error: String

    init?(dict: [String: Any]) {
        guard let configuredDict = dict["configured"] as? [String: Any],
              let configured = ReasoningPolicyValue(dict: configuredDict),
              let effectiveDict = dict["effective"] as? [String: Any],
              let effective = ReasoningPolicyValue(dict: effectiveDict),
              let source = dict["source"] as? String,
              let rawModes = dict["available_modes"] as? [String],
              let availableEfforts = dict["available_efforts"] as? [String],
              let available = dict["available"] as? Bool,
              let unavailableReason = dict["unavailable_reason"] as? String,
              let error = dict["error"] as? String else {
            return nil
        }
        let modes = rawModes.compactMap(ReasoningPolicyMode.init)
        guard modes.count == rawModes.count else { return nil }
        self.configured = configured
        self.effective = effective
        self.source = source
        availableModes = modes
        self.availableEfforts = availableEfforts
        self.available = available
        self.unavailableReason = unavailableReason
        self.error = error
    }
}

struct ClientModelOptionStatus: Equatable, Sendable {
    let reasoning: ClientReasoningStatus?

    init?(dict: [String: Any]) {
        if let rawReasoning = dict["reasoning"] {
            guard let reasoningDict = rawReasoning as? [String: Any],
                  let reasoning = ClientReasoningStatus(
                    dict: reasoningDict) else {
                return nil
            }
            self.reasoning = reasoning
        } else {
            reasoning = nil
        }
    }
}

typealias ClientModelOptions =
    [String: [String: ClientModelOptionStatus]]

private func decodeClientModelOptions(_ raw: Any?) -> ClientModelOptions {
    guard let providers = raw as? [String: Any] else { return [:] }
    var decoded: ClientModelOptions = [:]
    for (provider, rawModels) in providers {
        guard let models = rawModels as? [String: Any] else { continue }
        var decodedModels: [String: ClientModelOptionStatus] = [:]
        for (model, rawOption) in models {
            guard let optionDict = rawOption as? [String: Any],
                  let option = ClientModelOptionStatus(
                    dict: optionDict) else {
                continue
            }
            decodedModels[model] = option
        }
        if !decodedModels.isEmpty {
            decoded[provider] = decodedModels
        }
    }
    return decoded
}

struct ReasoningPreflightClient: Equatable, Sendable {
    let name: String
    let enabled: Bool
    let reachable: Bool
    let supported: Bool
    let reachability: [String]
    let failureBehaviors: [String]
    let availableModes: [ReasoningPolicyMode]
    let availableEfforts: [String]
    let unavailableReason: String
    let error: String

    init?(dict: [String: Any]) {
        guard let name = dict["name"] as? String,
              let enabled = dict["enabled"] as? Bool,
              let reachable = dict["reachable"] as? Bool,
              let supported = dict["supported"] as? Bool,
              let reachability = dict["reachability"] as? [String],
              let failureBehaviors = dict["failure_behaviors"] as? [String],
              let rawModes = dict["available_modes"] as? [String],
              let availableEfforts = dict["available_efforts"] as? [String],
              let unavailableReason = dict["unavailable_reason"] as? String,
              let error = dict["error"] as? String else {
            return nil
        }
        let modes = rawModes.compactMap(ReasoningPolicyMode.init)
        guard modes.count == rawModes.count else { return nil }
        self.name = name
        self.enabled = enabled
        self.reachable = reachable
        self.supported = supported
        self.reachability = reachability
        self.failureBehaviors = failureBehaviors
        availableModes = modes
        self.availableEfforts = availableEfforts
        self.unavailableReason = unavailableReason
        self.error = error
    }
}

struct ReasoningPreflightSnapshot: Equatable, Sendable {
    let provider: String
    let model: String
    let policy: ReasoningPolicyValue
    let available: Bool
    let error: String
    let warning: String
    let clients: [ReasoningPreflightClient]

    init?(dict: [String: Any]) {
        guard let provider = dict["provider"] as? String,
              let model = dict["model"] as? String,
              let policyDict = dict["policy"] as? [String: Any],
              let policy = ReasoningPolicyValue(dict: policyDict),
              let available = dict["available"] as? Bool,
              let error = dict["error"] as? String,
              let warning = dict["warning"] as? String,
              let rawClients = dict["clients"] as? [[String: Any]] else {
            return nil
        }
        let clients = rawClients.compactMap(ReasoningPreflightClient.init)
        guard clients.count == rawClients.count else { return nil }
        self.provider = provider
        self.model = model
        self.policy = policy
        self.available = available
        self.error = error
        self.warning = warning
        self.clients = clients
    }
}

struct LiveModelCatalogSnapshot: Equatable, Sendable {
    let state: LiveModelCatalogResponseState
    let signedOutReason: LiveModelCatalogSignedOutReason?
    let models: [LiveModelCatalogEntry]
    let fetchedAt: String
    let error: String

    init?(dict: [String: Any]) {
        guard let rawState = dict["state"] as? String,
              let state = LiveModelCatalogResponseState(rawValue: rawState),
              let rawSignedOutReason = dict["signed_out_reason"] as? String,
              let array = dict["models"] as? [[String: Any]],
              let fetchedAt = dict["fetched_at"] as? String,
              let error = dict["error"] as? String else {
            return nil
        }
        self.state = state
        switch state {
        case .signedOut:
            guard let reason = LiveModelCatalogSignedOutReason(
                rawValue: rawSignedOutReason) else {
                return nil
            }
            signedOutReason = reason
        case .ready, .error:
            guard rawSignedOutReason.isEmpty else { return nil }
            signedOutReason = nil
        }
        var models: [LiveModelCatalogEntry] = []
        for row in array {
            guard let model = LiveModelCatalogEntry(dict: row) else {
                return nil
            }
            models.append(model)
        }
        self.models = models
        self.fetchedAt = fetchedAt
        self.error = error
    }
}

struct HealthSnapshot: Equatable, Sendable {
    var ok: Bool
    var uptimeSeconds: Int64
}

struct AuthStatus: Equatable, Sendable {
    var signedIn: Bool
    var authType: String
    var profile: String
    var fallbackEnabled: Bool
    var fallbackInUse: Bool
    var health: String
    var lastRefreshError: String

    init(dict: [String: Any]) {
        signedIn = dict["signed_in"] as? Bool ?? false
        authType = dict["auth_type"] as? String ?? ""
        profile = dict["profile"] as? String ?? ""
        fallbackEnabled = dict["fallback_enabled"] as? Bool ?? false
        fallbackInUse = dict["fallback_in_use"] as? Bool ?? false
        health = dict["health"] as? String ?? ""
        lastRefreshError = dict["last_refresh_error"] as? String ?? ""
    }
}

struct AdminStatusSnapshot: Equatable, Sendable {
    var token: RoutingToken
    var activeConfigHash: String
    var desiredConfigHash: String
    var capabilities: [String]
    var health: String
    var version: String
    var uptimeSeconds: Int64
    var configPath: String
    var reload: ReloadStatus
    var globalRoutingEnabled: Bool
    var fallbackPolicy: FallbackPolicyStatus?
    var auth: AuthStatus?
    var clients: [ClientStatus]

    init(dict: [String: Any]) {
        let generationNumber = dict["active_generation"] as? NSNumber
        token = RoutingToken(
            routerBootID: dict["router_boot_id"] as? String ?? "",
            activeGeneration: generationNumber?.uint64Value ?? 0)
        activeConfigHash = dict["active_config_hash"] as? String ?? ""
        desiredConfigHash = dict["desired_config_hash"] as? String ?? ""
        capabilities = dict["capabilities"] as? [String] ?? []
        health = dict["health"] as? String ?? "ready"
        version = dict["version"] as? String ?? ""
        uptimeSeconds = (dict["uptime_seconds"] as? NSNumber)?.int64Value ?? 0
        configPath = dict["config_path"] as? String ?? ""
        reload = (dict["reload"] as? [String: Any])
            .map(ReloadStatus.init)
            ?? ReloadStatus(state: "", error: "")
        auth = (dict["auth"] as? [String: Any]).map(AuthStatus.init)
        let array = dict["clients"] as? [[String: Any]] ?? []
        clients = array.compactMap(ClientStatus.init)

        globalRoutingEnabled = dict["global_routing_enabled"] as? Bool ?? false
        fallbackPolicy = (dict["fallback_policy"] as? [String: Any])
            .flatMap(FallbackPolicyStatus.init)
    }
}

// MARK: - Injected admin API

protocol AdminStatusReading: Sendable {
    func fetchStatus() async throws -> AdminStatusSnapshot
    func fetchStats(windowSeconds: Int, bucketSeconds: Int) async throws -> StatsSnapshot
}

protocol AuthReloading: Sendable {
    func reloadAuth() async throws -> AuthStatus
}

protocol ModelCatalogReading: Sendable {
    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot
}

protocol ReasoningPreflightReading: Sendable {
    func preflightReasoning(
        client: String,
        provider: String,
        model: String,
        policy: ReasoningPolicyValue
    ) async throws -> ReasoningPreflightSnapshot
}

final class GatewayAPIClient: AdminStatusReading, AuthReloading,
                              ModelCatalogReading,
                              ReasoningPreflightReading,
                              @unchecked Sendable {
    private static let adminRequestTimeout: TimeInterval = 2
    private static let modelCatalogRequestTimeout: TimeInterval = 4
    private let runtime: RuntimeProfile
    private let session: URLSession

    init(runtime: RuntimeProfile, session: URLSession? = nil) {
        self.runtime = runtime
        if let session {
            self.session = session
        } else {
            let configuration = URLSessionConfiguration.ephemeral
            configuration.timeoutIntervalForRequest = 2
            configuration.timeoutIntervalForResource = 5
            configuration.waitsForConnectivity = false
            configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
            configuration.urlCache = nil
            self.session = URLSession(configuration: configuration)
        }
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        let object = try await getJSON("v1/admin/status")
        guard let dict = object as? [String: Any] else {
            throw GatewayClientError.invalidPayload
        }
        return AdminStatusSnapshot(dict: dict)
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        let object = try await getJSON("v1/admin/stats", query: [
            URLQueryItem(name: "window", value: String(windowSeconds)),
            URLQueryItem(name: "bucket", value: String(bucketSeconds)),
        ])
        guard let dict = object as? [String: Any] else {
            throw GatewayClientError.invalidPayload
        }
        return StatsSnapshot(dict: dict)
    }

    func reloadAuth() async throws -> AuthStatus {
        let object = try await postEmptyJSON("v1/admin/auth/reload")
        guard let dict = object as? [String: Any] else {
            throw GatewayClientError.invalidPayload
        }
        return AuthStatus(dict: dict)
    }

    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        let object = try await getJSON(
            "v1/admin/model-catalog",
            timeoutInterval: Self.modelCatalogRequestTimeout)
        guard let dict = object as? [String: Any],
              let snapshot = LiveModelCatalogSnapshot(dict: dict) else {
            throw GatewayClientError.invalidPayload
        }
        return snapshot
    }

    func preflightReasoning(
        client: String,
        provider: String,
        model: String,
        policy: ReasoningPolicyValue
    ) async throws -> ReasoningPreflightSnapshot {
        let object = try await postJSON(
            "v1/admin/reasoning/preflight",
            body: [
                "client": client,
                "provider": provider,
                "model": model,
                "policy": policy.jsonObject,
            ])
        guard let dict = object as? [String: Any],
              let snapshot = ReasoningPreflightSnapshot(
                dict: dict) else {
            throw GatewayClientError.invalidPayload
        }
        return snapshot
    }

    private func getJSON(_ path: String,
                         query: [URLQueryItem] = [],
                         timeoutInterval: TimeInterval =
                            GatewayAPIClient.adminRequestTimeout) async throws
        -> Any {
        var url = adminBaseURL(runtime: runtime)
        url.appendPathComponent(path)
        if !query.isEmpty,
           var components = URLComponents(url: url, resolvingAgainstBaseURL: false) {
            components.queryItems = query
            if let queriedURL = components.url {
                url = queriedURL
            }
        }
        var request = URLRequest(url: url)
        request.timeoutInterval = timeoutInterval
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse,
              (200..<300).contains(http.statusCode) else {
            throw GatewayClientError.badResponse(
                (response as? HTTPURLResponse)?.statusCode ?? -1)
        }
        return try JSONSerialization.jsonObject(with: data)
    }

    private func postJSON(
        _ path: String,
        body: [String: Any]
    ) async throws -> Any {
        var request = URLRequest(
            url: adminBaseURL(runtime: runtime)
                .appendingPathComponent(path))
        request.httpMethod = "POST"
        request.setValue(
            "application/json",
            forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(
            withJSONObject: body)
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse,
              (200..<300).contains(http.statusCode) else {
            throw GatewayClientError.badResponse(
                (response as? HTTPURLResponse)?.statusCode ?? -1)
        }
        return try JSONSerialization.jsonObject(with: data)
    }

    private func postEmptyJSON(_ path: String) async throws -> Any {
        var request = URLRequest(
            url: adminBaseURL(runtime: runtime)
                .appendingPathComponent(path))
        request.httpMethod = "POST"
        request.setValue("1", forHTTPHeaderField: "X-Baseten-Switch-Admin")
        request.httpBody = Data()
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse,
              (200..<300).contains(http.statusCode) else {
            throw GatewayClientError.badResponse(
                (response as? HTTPURLResponse)?.statusCode ?? -1)
        }
        return try JSONSerialization.jsonObject(with: data)
    }
}

func adminBaseURL(runtime: RuntimeProfile = .stable()) -> URL {
    var url = runtime.adminBaseURL
    if url.path.isEmpty {
        url.appendPathComponent("")
    }
    return url
}
