import AppKit
import Foundation
import ServiceManagement

enum MutationPhase: String, Equatable, Sendable {
    case applying
    case reconciling
}

enum MutationRecoveryState: Equatable, Sendable {
    case disabled
    case awaitingSnapshot
    case checking
    case waitingToRetry(seconds: TimeInterval)
    case ready
    case legacyFallback
    case blocked(errorCode: String)
}

private struct MutationRecoveryKey: Equatable, Sendable {
    let configIdentity: String
    let routerBootID: String
    let runtimeIdentity: String?
}

struct PendingGlobalRouting: Equatable, Sendable {
    let operationID: String
    let requested: Bool
    var phase: MutationPhase
}

struct PendingControlMutation: Equatable, Sendable {
    let operationID: String
    let requestedTarget: String
    var phase: MutationPhase
}

enum ReasoningMutationPhase: String, Equatable, Sendable {
    case preflighting
    case applying
    case reconciling
}

struct PendingReasoningMutation: Equatable, Sendable {
    let operationID: String
    let client: String
    let provider: String
    let model: String
    let policy: ReasoningPolicyValue
    var phase: ReasoningMutationPhase
}

private struct PolicyMutationAttempt {
    let result: CLIExecutionResult
    let receipt: GlobalMutationReceipt?
    let primaryTimedOut: Bool
    let verifiedTerminalReplay: Bool
}

private struct PolicyMutationRequestIdentity {
    let operation: String
    let client: String
    let key: String
    let requestedTarget: String
}

@MainActor
protocol LoginItemServicing {
    var status: SMAppService.Status { get }
    func reconcileAtLaunch()
    func toggle()
    func openSystemSettings()
}

@MainActor
struct SystemLoginItemService: LoginItemServicing {
    var status: SMAppService.Status { LoginItem.status }
    func reconcileAtLaunch() { LoginItem.reconcileAtLaunch() }
    func toggle() { LoginItem.toggle() }
    func openSystemSettings() { LoginItem.openSystemSettings() }
}

/// Presentation store over one immutable server snapshot. Polling and child
/// process lifetimes are owned by injected coordinators, which keeps UI state
/// deterministic and prevents overlapping status reads or config mutations.
@MainActor
final class BasetenSwitchState: ObservableObject {
    @Published private(set) var routingSnapshot: RoutingSnapshot?
    @Published private(set) var snapshotIsStale = false
    @Published private(set) var liveModelCatalogState:
        LiveModelCatalogLoadState = .idle
    @Published private(set) var starting = false
    @Published var lastError: String?
    @Published private(set) var loginItemStatus: SMAppService.Status = .notRegistered
    @Published private(set) var stats: StatsSnapshot?
    @Published private(set) var cliVersion = ""
    @Published private(set) var reauthenticating = false
    @Published private(set) var runtimeTrust: RuntimeTrust
    @Published private(set) var pendingGlobalRouting: PendingGlobalRouting?
    @Published private(set) var pendingFamilyRoutes: [String: PendingControlMutation] = [:]
    @Published private(set) var pendingCodexRoute: PendingControlMutation?
    @Published private(set) var pendingSubagents: [String: PendingControlMutation] = [:]
    @Published private(set) var pendingReasoning: PendingReasoningMutation?
    @Published private(set) var reasoningWarnings: [String: String] = [:]
    @Published private(set) var mutationRecoveryState: MutationRecoveryState

    private let reader: any AdminStatusReading
    private let authReloader: any AuthReloading
    private let modelCatalogReader: any ModelCatalogReading
    private let reasoningPreflightReader: any ReasoningPreflightReading
    private let cliRunner: any CLIRunning
    private let clock: any RuntimeClock
    private let loginItemService: any LoginItemServicing
    private let previewRuntimeValidator: (RuntimeProfile) -> String?
    private let pollCoordinator: PollCoordinator?
    private let mutationCoordinator: MutationCoordinator?
    private let automaticMutationRecoveryEnabled: Bool
    private var reconcileTimers: [String: Task<Void, Never>] = [:]
    private var interactiveRefreshTask: Task<Void, Never>?
    private var interactiveStatsRequested = false
    private var clientPageRefreshTask: Task<Void, Never>?
    private var clientPageRefreshPending = false
    private var modelCatalogTask: Task<Void, Never>?
    private var modelCatalogGeneration: UInt64 = 0
    private var refreshingModelCatalog: [LiveModelCatalogEntry]?
    private var automaticCatalogRecoveryGeneration: UInt64?
    private var mutationRecoveryTask: Task<Void, Never>?
    private var mutationRecoveryKey: MutationRecoveryKey?
    private var unsupportedRecoveryRuntime: String?
    private(set) var menuVisible = false
    let variant: AppVariant

    var clients: [ClientStatus] { routingSnapshot?.clients ?? [] }
    var gatewayUp: Bool {
        routingSnapshot?.gateway == .ready && !snapshotIsStale
    }
    var uptimeSeconds: Int64 {
        projectedUptimeSeconds(snapshot: routingSnapshot, now: Date())
    }
    var activeRequests: Int { routingSnapshot?.activeRequests ?? 0 }
    var routerVersion: String { routingSnapshot?.version ?? "" }
    var auth: AuthStatus? { routingSnapshot?.auth }
    var activeConfigPath: String { routingSnapshot?.configPath ?? "" }
    var confirmedGlobalRoutingEnabled: Bool {
        routingSnapshot?.globalRoutingEnabled ?? false
    }
    var displayedGlobalRoutingEnabled: Bool {
        pendingGlobalRouting?.requested ?? confirmedGlobalRoutingEnabled
    }
    var globalMutationPhase: MutationPhase? {
        pendingGlobalRouting?.phase
    }
    var hasFallback: Bool {
        clients.contains { $0.enabled && $0.fallbackActive }
    }

    init(variant: AppVariant = .current(),
         reader: (any AdminStatusReading)? = nil,
         authReloader: (any AuthReloading)? = nil,
         modelCatalogReader: (any ModelCatalogReading)? = nil,
         reasoningPreflightReader: (any ReasoningPreflightReading)? = nil,
         cliRunner: any CLIRunning = SystemCLIRunner(),
         clock: any RuntimeClock = SystemRuntimeClock(),
         loginItemService: (any LoginItemServicing)? = nil,
         previewRuntimeValidator: @escaping (RuntimeProfile) -> String? = {
             previewRuntimeFilesystemError(runtime: $0)
         },
         startPolling: Bool = true,
         automaticMutationRecoveryEnabled: Bool? = nil) {
        self.variant = variant
        let apiClient = GatewayAPIClient(runtime: variant.runtime)
        self.reader = reader ?? apiClient
        self.authReloader = authReloader ?? apiClient
        self.modelCatalogReader = modelCatalogReader ?? apiClient
        self.reasoningPreflightReader = reasoningPreflightReader ?? apiClient
        self.cliRunner = cliRunner
        self.clock = clock
        self.loginItemService = loginItemService ?? SystemLoginItemService()
        self.previewRuntimeValidator = previewRuntimeValidator
        self.automaticMutationRecoveryEnabled =
            automaticMutationRecoveryEnabled ?? startPolling
        mutationRecoveryState = self.automaticMutationRecoveryEnabled
            ? .awaitingSnapshot
            : .disabled
        if let identityError = variant.identityError {
            runtimeTrust = .identityMismatch(reason: identityError)
            lastError = identityError
        } else {
            runtimeTrust = variant.channel == .preview ? .previewDown : .stable
        }

        let poll = PollCoordinator(
            reader: self.reader,
            clock: clock,
            // Model calls are usually long-lived streams. A one-second local
            // status read makes their activity visible without a new event
            // channel or high-frequency polling.
            interval: 1)
        pollCoordinator = poll
        mutationCoordinator = MutationCoordinator(runner: cliRunner)

        if variant.channel == .preview,
           variant.identityError == nil,
           Self.locateBasetenSwitchBinary(variant: variant) == nil {
            lastError = "Baseten Switch Preview requires BASETEN_SWITCH_GATEWAY_BIN from its launcher."
        }
        if variant.allowsLoginItem {
            self.loginItemService.reconcileAtLaunch()
            loginItemStatus = self.loginItemService.status
        }

        if startPolling {
            Task {
                await poll.start { [weak self] event in
                    self?.apply(event)
                }
            }
        }
    }

#if DEBUG
    /// Fixture initializer remains side-effect free: no login item, localhost,
    /// timer, URLSession, or child-process work.
    init(preview fixture: PopupPreviewFixture,
         variant: AppVariant = .current()) {
        self.variant = variant
        let reader = GatewayAPIClient(runtime: variant.runtime)
        self.reader = reader
        authReloader = reader
        modelCatalogReader = reader
        reasoningPreflightReader = reader
        cliRunner = SystemCLIRunner()
        clock = SystemRuntimeClock()
        loginItemService = SystemLoginItemService()
        previewRuntimeValidator = {
            previewRuntimeFilesystemError(runtime: $0)
        }
        automaticMutationRecoveryEnabled = false
        mutationRecoveryState = .disabled
        pollCoordinator = nil
        mutationCoordinator = nil
        if let identityError = variant.identityError {
            runtimeTrust = .identityMismatch(reason: identityError)
        } else {
            runtimeTrust = variant.channel == .preview ? .previewTrusted : .stable
        }
        stats = fixture.stats
        cliVersion = fixture.cliVersion
        loginItemStatus = fixture.loginItemStatus
        lastError = fixture.lastError
        liveModelCatalogState = fixture.liveModelCatalogState
        if fixture.gatewayUp {
            routingSnapshot = RoutingSnapshot(
                observedAt: Date(),
                version: fixture.routerVersion,
                uptimeSeconds: fixture.uptimeSeconds,
                globalRoutingEnabled: globalRoutingState(fixture.clients) != .off,
                auth: fixture.auth,
                clients: fixture.clients)
        } else {
            routingSnapshot = nil
            snapshotIsStale = true
        }
    }
#endif

    func stop() {
        for timer in reconcileTimers.values {
            timer.cancel()
        }
        reconcileTimers.removeAll()
        interactiveRefreshTask?.cancel()
        interactiveRefreshTask = nil
        interactiveStatsRequested = false
        clientPageRefreshTask?.cancel()
        clientPageRefreshTask = nil
        clientPageRefreshPending = false
        invalidateModelCatalog()
        mutationRecoveryTask?.cancel()
        mutationRecoveryTask = nil
        Task { await pollCoordinator?.stop() }
    }

    // MARK: - Polling and snapshot application

    func refresh() async {
        guard let pollCoordinator else { return }
        apply(await pollCoordinator.refresh())
    }

    /// Establishes a read barrier after a child-process mutation. A normal
    /// refresh may join a status request that began before the mutation and
    /// falsely report that the confirmed change is absent.
    private func refreshAfterMutation() async {
        guard let pollCoordinator else { return }
        apply(await pollCoordinator.refreshFresh())
    }

    private func apply(_ event: PollEvent) {
        switch event {
        case .snapshot(let snapshot):
            let previousAuth = routingSnapshot?.auth
            let meaningfulChange = routingSnapshot.map {
                !routingPresentationEqual($0, snapshot)
            } ?? true
            if meaningfulChange {
                routingSnapshot = snapshot
            }
            if snapshotIsStale {
                snapshotIsStale = false
            }
            updateRuntimeTrust(snapshot: snapshot)
            if variant.allowsLoginItem {
                let updatedStatus = loginItemService.status
                if updatedStatus != loginItemStatus {
                    loginItemStatus = updatedStatus
                }
            }
            beginAutomaticMutationRecoveryIfNeeded(snapshot)
            requestAutomaticModelCatalogRecoveryIfNeeded(
                previousAuth: previousAuth,
                currentAuth: snapshot.auth)
        case .unavailable:
            if !snapshotIsStale {
                snapshotIsStale = true
            }
            if variant.channel == .preview,
               variant.identityError == nil,
               runtimeTrust != .previewDown {
                runtimeTrust = .previewDown
            }
        case .ignoredStaleToken:
            break
        }
    }

    private var mutationRecoveryAllowsRouting: Bool {
        switch mutationRecoveryState {
        case .disabled, .ready, .legacyFallback:
            return true
        case .awaitingSnapshot, .checking, .waitingToRetry, .blocked:
            return false
        }
    }

    private var mutationRecoveryDisabledReason: String? {
        switch mutationRecoveryState {
        case .awaitingSnapshot, .checking:
            return "Checking for an unfinished routing change."
        case .waitingToRetry:
            return "Waiting to retry routing cleanup."
        case .blocked:
            return "Routing cleanup must finish before settings can be changed."
        case .disabled, .ready, .legacyFallback:
            return nil
        }
    }

    var mutationRecoveryMessage: String? {
        switch mutationRecoveryState {
        case .checking:
            return "Checking for an unfinished routing change."
        case .waitingToRetry(let seconds):
            return "Routing cleanup will retry in \(Int(seconds)) seconds."
        case .blocked(let errorCode):
            return reviewedMutationErrorMessage(
                errorCode: errorCode,
                fallback: "Routing cleanup could not be completed safely.")
        case .disabled, .awaitingSnapshot, .ready, .legacyFallback:
            return nil
        }
    }

    var canRetryMutationCleanup: Bool {
        if case .blocked = mutationRecoveryState {
            return mutationRecoveryKey != nil
        }
        return false
    }

    func retryMutationCleanup() {
        guard canRetryMutationCleanup,
              let key = mutationRecoveryKey else { return }
        mutationRecoveryTask?.cancel()
        mutationRecoveryState = .checking
        mutationRecoveryTask = Task { [weak self] in
            await self?.runCleanupRecovery(for: key)
        }
    }

    func waitForMutationRecovery() async {
        await mutationRecoveryTask?.value
    }

    private func beginAutomaticMutationRecoveryIfNeeded(
        _ snapshot: RoutingSnapshot
    ) {
        guard automaticMutationRecoveryEnabled,
              snapshot.token.isAuthoritative,
              !snapshot.configPath.isEmpty else { return }
        let runtime = recoveryRuntimeIdentifier
        let key = MutationRecoveryKey(
            configIdentity: URL(fileURLWithPath: snapshot.configPath)
                .standardizedFileURL.path,
            routerBootID: snapshot.token.routerBootID,
            runtimeIdentity: runtime)
        guard key != mutationRecoveryKey else { return }

        mutationRecoveryTask?.cancel()
        mutationRecoveryKey = key
        if let runtime,
           unsupportedRecoveryRuntime == runtime {
            mutationRecoveryState = .legacyFallback
            return
        }

        mutationRecoveryState = .checking
        mutationRecoveryTask = Task { [weak self] in
            await self?.probeAndRecoverMutation(for: key, runtime: runtime)
        }
    }

    private var recoveryRuntimeIdentifier: String? {
        guard let binary = Self.locateBasetenSwitchBinary(variant: variant)?
            .standardizedFileURL else { return nil }
        let attributes = try? FileManager.default.attributesOfItem(
            atPath: binary.path)
        let modified = (attributes?[.modificationDate] as? Date)?
            .timeIntervalSince1970 ?? 0
        let size = attributes?[.size] as? NSNumber
        let inode = attributes?[.systemFileNumber] as? NSNumber
        return "\(binary.path):\(inode?.uint64Value ?? 0):\(modified):\(size?.int64Value ?? 0)"
    }

    private func probeAndRecoverMutation(
        for key: MutationRecoveryKey,
        runtime: String?
    ) async {
        let status = await executeCLI(
            ["--json", "mutation", "status"],
            timeout: 10)
        guard recoveryKeyIsCurrent(key), !Task.isCancelled else { return }
        let receipt = MutationRecoveryReceipt(json: status.standardOutput)
        if mutationStatusIsUnsupported(status, receipt: receipt) {
            unsupportedRecoveryRuntime = runtime
            await finishMutationRecovery(for: key, state: .legacyFallback)
            return
        }
        if !status.succeeded,
           !status.timedOut,
           receipt?.errorRetryable != true,
           !isTransientRecoveryError(receipt?.errorCode ?? "") {
            await finishMutationRecovery(
                for: key,
                state: .blocked(
                    errorCode: receipt?.errorCode ?? "status_unavailable"))
            return
        }
        await runCleanupRecovery(for: key)
    }

    private func runCleanupRecovery(for key: MutationRecoveryKey) async {
        let retryDelays: [TimeInterval] = [1, 2, 4, 8, 16]
        var finalErrorCode = "cleanup_failed"
        for attempt in 0...retryDelays.count {
            guard recoveryKeyIsCurrent(key), !Task.isCancelled else { return }
            if attempt > 0 {
                let delay = retryDelays[attempt - 1]
                mutationRecoveryState = .waitingToRetry(seconds: delay)
                do {
                    try await clock.sleep(seconds: delay)
                } catch {
                    return
                }
                guard recoveryKeyIsCurrent(key), !Task.isCancelled else { return }
                mutationRecoveryState = .checking
            }

            let result = await executeCLI(
                ["--json", "mutation", "recover"],
                timeout: 10)
            guard recoveryKeyIsCurrent(key), !Task.isCancelled else { return }
            let receipt = MutationRecoveryReceipt(json: result.standardOutput)
            if result.succeeded, receipt?.ok == true {
                if receipt?.cleanupPending == true {
                    finalErrorCode = "cleanup_pending"
                    guard attempt < retryDelays.count else {
                        await finishMutationRecovery(
                            for: key,
                            state: .blocked(errorCode: finalErrorCode))
                        return
                    }
                    continue
                } else {
                    await finishMutationRecovery(for: key, state: .ready)
                    return
                }
            }

            if result.timedOut && (receipt?.errorCode.isEmpty != false) {
                finalErrorCode = "cleanup_timed_out"
            } else {
                finalErrorCode = receipt?.errorCode ?? "cleanup_failed"
            }
            let shouldRetry = (result.timedOut
                && (receipt?.errorCode.isEmpty != false))
                || receipt?.errorRetryable == true
                || isTransientRecoveryError(finalErrorCode)
            guard shouldRetry,
                  attempt < retryDelays.count else {
                await finishMutationRecovery(
                    for: key,
                    state: .blocked(errorCode: finalErrorCode))
                return
            }
        }
    }

    private func finishMutationRecovery(
        for key: MutationRecoveryKey,
        state: MutationRecoveryState
    ) async {
        await refreshAfterMutation()
        guard recoveryKeyIsCurrent(key), !Task.isCancelled else { return }
        mutationRecoveryState = state
    }

    private func recoveryKeyIsCurrent(_ key: MutationRecoveryKey) -> Bool {
        key == mutationRecoveryKey
    }

    private func mutationStatusIsUnsupported(
        _ result: CLIExecutionResult,
        receipt: MutationRecoveryReceipt?
    ) -> Bool {
        let unsupportedCodes = [
            "usage", "unknown_command", "unknown_subcommand",
            "unsupported_command",
        ]
        return unsupportedCodes.contains(receipt?.errorCode ?? "")
            || (result.status == 2 && !result.timedOut)
    }

    private func isTransientRecoveryError(_ errorCode: String) -> Bool {
        errorCode == "mutation_locked"
            || errorCode == "router_unavailable"
            || errorCode == "cleanup_pending"
    }

    func menuDidShow() {
        menuVisible = true
        requestInteractiveRefresh(includeStats: true)
    }

    func menuDidHide() {
        menuVisible = false
    }

    /// Coalesces menu and window refresh triggers into one bounded task. A
    /// second request can ask the current task to include stats, but cannot
    /// create another status read, stats read, or version child process.
    func requestInteractiveRefresh(includeStats: Bool) {
        if includeStats {
            interactiveStatsRequested = true
        }
        guard interactiveRefreshTask == nil else { return }
        interactiveRefreshTask = Task { [weak self] in
            guard let self else { return }
            await self.refresh()
            let shouldRefreshStats = self.interactiveStatsRequested
                && self.menuVisible
            self.interactiveStatsRequested = false
            if shouldRefreshStats {
                await self.refreshStats()
            }
            await self.refreshCLIVersion()
            self.interactiveRefreshTask = nil
            if self.interactiveStatsRequested {
                self.requestInteractiveRefresh(includeStats: false)
            }
        }
    }

    func waitForInteractiveRefresh() async {
        await interactiveRefreshTask?.value
    }

    /// Coalesces client-page toolbar refreshes into one ordered operation.
    /// Auth reload is best effort for compatibility with older routers, while
    /// status and catalog retain their existing independent error surfaces.
    func requestClientPageRefresh() {
        guard clientPageRefreshTask == nil else {
            clientPageRefreshPending = true
            return
        }
        beginModelCatalogLoading()
        clientPageRefreshTask = Task { [weak self] in
            guard let self else { return }
            do {
                _ = try await self.authReloader.reloadAuth()
            } catch {
                // Older routers do not expose the reload endpoint. Continue
                // with the normal reads so mixed-version installations remain
                // usable and surface their existing sanitized state.
            }
            guard !Task.isCancelled else { return }
            if let pollCoordinator = self.pollCoordinator {
                self.apply(await pollCoordinator.refreshFresh())
            }
            guard !Task.isCancelled else { return }
            self.requestModelCatalogRefresh()
            await self.waitForModelCatalogRefresh()
            guard !Task.isCancelled else { return }
            self.clientPageRefreshTask = nil
            if self.clientPageRefreshPending {
                self.clientPageRefreshPending = false
                self.requestClientPageRefresh()
            }
        }
    }

    func waitForClientPageRefresh() async {
        while let task = clientPageRefreshTask {
            await task.value
        }
    }

    // MARK: - Live model catalog

    func ensureModelCatalogLoaded() {
        guard case .idle = liveModelCatalogState else { return }
        requestModelCatalogRefresh()
    }

    func routerWindowDidClose() {
        clientPageRefreshTask?.cancel()
        clientPageRefreshTask = nil
        clientPageRefreshPending = false
        invalidateModelCatalog()
    }

    func requestModelCatalogRefresh() {
        automaticCatalogRecoveryGeneration = nil
        let generation = beginModelCatalogLoading()
        let reader = modelCatalogReader
        modelCatalogTask = Task { [weak self] in
            do {
                let snapshot = try await reader.fetchModelCatalog()
                guard let self,
                      !Task.isCancelled,
                      self.modelCatalogGeneration == generation else {
                    return
                }
                self.modelCatalogTask = nil
                self.applyModelCatalog(snapshot, generation: generation)
            } catch is CancellationError {
                return
            } catch {
                guard let self,
                      !Task.isCancelled,
                      self.modelCatalogGeneration == generation else {
                    return
                }
                if self.automaticCatalogRecoveryGeneration == generation {
                    self.automaticCatalogRecoveryGeneration = nil
                }
                self.refreshingModelCatalog = nil
                self.liveModelCatalogState = .error(
                    "Live model availability could not be loaded.")
                self.modelCatalogTask = nil
            }
        }
    }

    func waitForModelCatalogRefresh() async {
        while let task = modelCatalogTask {
            await task.value
        }
    }

    func modelCatalogProjection(for client: ClientStatus)
        -> ModelCatalogProjection {
        let projectionState: LiveModelCatalogLoadState
        if case .loading = liveModelCatalogState,
           let refreshingModelCatalog {
            projectionState = .ready(refreshingModelCatalog)
        } else {
            projectionState = liveModelCatalogState
        }
        return projectModelCatalog(
            configured: client.modelCatalog,
            liveState: projectionState)
    }

    var modelCatalogAllowsMutation: Bool {
        if case .ready = liveModelCatalogState {
            return true
        }
        return false
    }

    private func applyModelCatalog(
        _ snapshot: LiveModelCatalogSnapshot,
        generation: UInt64
    ) {
        refreshingModelCatalog = nil
        switch snapshot.state {
        case .ready:
            liveModelCatalogState = .ready(snapshot.models)
        case .signedOut:
            guard let reason = snapshot.signedOutReason else {
                liveModelCatalogState = .error(
                    "Live model availability could not be loaded.")
                return
            }
            liveModelCatalogState = .signedOut(reason)
        case .error:
            liveModelCatalogState = .error(
                snapshot.error.isEmpty
                    ? "Live model availability could not be loaded."
                    : snapshot.error)
        }

        guard automaticCatalogRecoveryGeneration == generation else {
            return
        }
        automaticCatalogRecoveryGeneration = nil
        if snapshot.state == .signedOut,
           snapshot.signedOutReason == .notSignedIn {
            requestModelCatalogRefresh()
        }
    }

    private func requestAutomaticModelCatalogRecoveryIfNeeded(
        previousAuth: AuthStatus?,
        currentAuth: AuthStatus?
    ) {
        guard clientPageRefreshTask == nil,
              selectedProfileIsUnavailable(previousAuth),
              selectedProfileIsHealthy(currentAuth) else {
            return
        }
        switch liveModelCatalogState {
        case .signedOut(.notSignedIn):
            requestModelCatalogRefresh()
        case .loading:
            automaticCatalogRecoveryGeneration = modelCatalogGeneration
        case .idle, .ready, .signedOut, .error:
            break
        }
    }

    private func selectedProfileIsUnavailable(_ auth: AuthStatus?) -> Bool {
        guard let auth else { return false }
        return !auth.signedIn || auth.health == "signed_out"
    }

    private func selectedProfileIsHealthy(_ auth: AuthStatus?) -> Bool {
        guard let auth else { return false }
        return auth.signedIn && auth.health == "ok"
    }

    private func invalidateModelCatalog() {
        refreshingModelCatalog = nil
        automaticCatalogRecoveryGeneration = nil
        modelCatalogGeneration &+= 1
        modelCatalogTask?.cancel()
        modelCatalogTask = nil
        liveModelCatalogState = .idle
    }

    @discardableResult
    private func beginModelCatalogLoading() -> UInt64 {
        automaticCatalogRecoveryGeneration = nil
        if case .ready(let models) = liveModelCatalogState {
            refreshingModelCatalog = models
        } else if case .loading = liveModelCatalogState {
            // Preserve the same last confirmed catalog across an ordered
            // auth/status/catalog refresh or a coalesced refresh rerun.
        } else {
            refreshingModelCatalog = nil
        }
        modelCatalogGeneration &+= 1
        modelCatalogTask?.cancel()
        modelCatalogTask = nil
        liveModelCatalogState = .loading
        return modelCatalogGeneration
    }

    func refreshStats() async {
        guard menuVisible else { return }
        do {
            stats = try await reader.fetchStats(
                windowSeconds: 3_600,
                bucketSeconds: 60)
        } catch {
            // Keep the last confirmed value. Stats are supplemental and must
            // never make routing state appear unavailable.
        }
    }

    func refreshCLIVersion() async {
        guard let binary = Self.locateBasetenSwitchBinary(variant: variant) else {
            cliVersion = ""
            return
        }
        let result = await cliRunner.run(CLIExecutionRequest(
            binary: binary,
            arguments: ["--version"],
            environment: processEnvironment(),
            timeout: 3))
        cliVersion = parseCLIVersionOutput(result.standardOutput)
    }

    // MARK: - Global routing

    var canMutate: Bool {
        mutationAllowed(allowWhenPreviewDown: false)
    }

    var canMutateRouting: Bool {
        guard gatewayUp,
              canMutate,
              mutationRecoveryAllowsRouting,
              let snapshot = routingSnapshot else { return false }
        return snapshot.supportsGlobalRouting
            && snapshot.token.isAuthoritative
            && snapshot.token.activeGeneration > 0
            && snapshot.desiredMatchesActive
    }

    var routingMutationDisabledReason: String? {
        if let pendingGlobalRouting {
            return pendingGlobalRoutingDisabledReason(pendingGlobalRouting)
        }
        guard gatewayUp else { return "The local gateway is unavailable." }
        guard canMutate else { return runtimeTrustError }
        if let recoveryReason = mutationRecoveryDisabledReason {
            return recoveryReason
        }
        guard let snapshot = routingSnapshot,
              snapshot.supportsGlobalRouting else {
            return "Update the local gateway to configure global routing."
        }
        guard snapshot.desiredMatchesActive else {
            return "The saved and active configurations differ. Resolve the reload error first."
        }
        return nil
    }

    @discardableResult
    func requestGlobalRouting(_ enabled: Bool) -> Bool {
        guard beginGlobalRouting(enabled) else { return false }
        Task { await finishGlobalRouting(enabled) }
        return true
    }

    func setAllRoutesThroughBaseten(_ enabled: Bool) async {
        guard beginGlobalRouting(enabled) else { return }
        await finishGlobalRouting(enabled)
    }

    private func beginGlobalRouting(_ enabled: Bool) -> Bool {
        guard canMutateRouting,
              pendingGlobalRouting == nil,
              confirmedGlobalRoutingEnabled != enabled else { return false }
        let operationID = UUID().uuidString.lowercased()
        pendingGlobalRouting = PendingGlobalRouting(
            operationID: operationID,
            requested: enabled,
            phase: .applying)
        scheduleReconciling(
            key: "global",
            operationID: operationID)
        return true
    }

    private func finishGlobalRouting(_ enabled: Bool) async {
        guard let pending = pendingGlobalRouting,
              let snapshot = routingSnapshot else { return }

        let arguments = [
            "--json",
            "--operation-id", pending.operationID,
            "--if-active-token", snapshot.token.cliValue,
            "--if-config-hash", snapshot.desiredConfigHash,
            enabled ? "on" : "off",
        ]
        let attempt = await executePolicyMutation(
            arguments,
            operationID: pending.operationID)
        await refreshAfterMutation()

        let receipt = attempt.receipt
        let confirmed = attempt.result.succeeded
            && receipt?.ok == true
            && receipt?.operationID == pending.operationID
            && receipt?.operation == "set_global_routing"
            && receipt?.applied == true
            && receipt?.requested == enabled
            && routingSnapshot?.globalRoutingEnabled == enabled
            && hashesConfirm(receipt)
        if confirmed {
            lastError = nil
        } else if attempt.primaryTimedOut {
            lastError = "The routing change timed out and could not be confirmed."
        } else {
            lastError = mutationFailureMessage(
                receipt,
                fallback: "The routing change failed and the last confirmed setting was restored.")
        }
        clearPending(key: "global", operationID: pending.operationID)
        pendingGlobalRouting = nil
    }

    private func hashesConfirm(_ receipt: GlobalMutationReceipt?) -> Bool {
        guard let receipt,
              let current = routingSnapshot else { return false }
        let expected = receipt.activeConfigHash.isEmpty
            ? receipt.desiredConfigHash
            : receipt.activeConfigHash
        return !expected.isEmpty
            && current.activeConfigHash == expected
            && current.desiredConfigHash == expected
            && receipt.activeToken == current.token.cliValue
    }

    private func receiptRequestIdentityConfirms(
        _ receipt: GlobalMutationReceipt?,
        key: String,
        requestedTarget: String,
        verifiedTerminalReplay: Bool
    ) -> Bool {
        guard let receipt else { return false }
        if receipt.key == key
            && receipt.requestedTarget == requestedTarget {
            return true
        }
        return verifiedTerminalReplay
            && receipt.identityStrength == "exact"
            && !receipt.requestFingerprint.isEmpty
    }

    // MARK: - Client model routing

    func isCodexRouteMutationPending() -> Bool {
        pendingCodexRoute != nil
    }

    func pendingCodexRouteTarget() -> String? {
        pendingCodexRoute?.requestedTarget
    }

    func requestCodexRoute(_ client: ClientStatus,
                           model: ModelCatalogEntry) {
        guard beginCodexRoute(client, model: model) else { return }
        Task { await finishCodexRoute(client, model: model) }
    }

    func routeCodex(_ client: ClientStatus,
                    model: ModelCatalogEntry) async {
        guard beginCodexRoute(client, model: model) else { return }
        await finishCodexRoute(client, model: model)
    }

    private func beginCodexRoute(
        _ client: ClientStatus,
        model: ModelCatalogEntry
    ) -> Bool {
        guard client.name == "codex",
              canMutateRouting,
              pendingCodexRoute == nil,
              !codexRouteChoiceChecked(client: client, model: model) else {
            return false
        }
        let operationID = UUID().uuidString.lowercased()
        pendingCodexRoute = PendingControlMutation(
            operationID: operationID,
            requestedTarget: model.slug,
            phase: .applying)
        scheduleReconciling(
            key: "codex-route",
            operationID: operationID)
        return true
    }

    private func finishCodexRoute(
        _ client: ClientStatus,
        model: ModelCatalogEntry
    ) async {
        guard let pending = pendingCodexRoute,
              let snapshot = routingSnapshot else { return }
        let arguments = mutationArguments(
            operationID: pending.operationID,
            snapshot: snapshot,
            command: codexRouteDispatchArgs(model: model))
        let attempt = await executePolicyMutation(
            arguments,
            operationID: pending.operationID,
            requestIdentity: PolicyMutationRequestIdentity(
                operation: "set_codex_route",
                client: client.name,
                key: "default_model",
                requestedTarget: model.slug))
        await refreshAfterMutation()
        let receipt = attempt.receipt
        let confirmed = !snapshotIsStale
            && attempt.result.succeeded
            && receipt?.ok == true
            && receipt?.operationID == pending.operationID
            && receipt?.operation == "set_codex_route"
            && receipt?.client == client.name
            && receiptRequestIdentityConfirms(
                receipt,
                key: "default_model",
                requestedTarget: model.slug,
                verifiedTerminalReplay: attempt.verifiedTerminalReplay)
            && receipt?.applied == true
            && hashesConfirm(receipt)
            && codexRouteMutationConfirmed(
                cliResult: attempt.result,
                snapshot: routingSnapshot,
                model: model)
        if confirmed {
            lastError = nil
        } else {
            lastError = attempt.primaryTimedOut
                ? "The Codex model change timed out and could not be confirmed."
                : mutationFailureMessage(
                    receipt,
                    fallback: "The selected Codex model was not present in the active router state.")
        }
        clearPending(
            key: "codex-route",
            operationID: pending.operationID)
        pendingCodexRoute = nil
    }

    // MARK: - Claude family and subagent configuration

    func isFamilyMutationPending(client: String, family: String) -> Bool {
        pendingFamilyRoutes[familyKey(client: client, family: family)] != nil
    }

    func pendingFamilyTarget(client: String, family: String) -> String? {
        pendingFamilyRoutes[familyKey(client: client, family: family)]?
            .requestedTarget
    }

    func requestFamilyRoute(_ client: ClientStatus,
                            family: String,
                            choice: FamilyChoice) {
        guard beginFamilyRoute(client, family: family, choice: choice) else {
            return
        }
        Task { await finishFamilyRoute(client, family: family, choice: choice) }
    }

    func routeFamily(_ client: ClientStatus,
                     family: String,
                     choice: FamilyChoice) async {
        guard beginFamilyRoute(client, family: family, choice: choice) else {
            return
        }
        await finishFamilyRoute(client, family: family, choice: choice)
    }

    private func beginFamilyRoute(_ client: ClientStatus,
                                  family: String,
                                  choice: FamilyChoice) -> Bool {
        let key = familyKey(client: client.name, family: family)
        guard canMutateRouting,
              pendingFamilyRoutes[key] == nil else { return false }
        let operationID = UUID().uuidString.lowercased()
        pendingFamilyRoutes[key] = PendingControlMutation(
            operationID: operationID,
            requestedTarget: familyChoiceArg(choice),
            phase: .applying)
        scheduleReconciling(key: key, operationID: operationID)
        return true
    }

    private func finishFamilyRoute(_ client: ClientStatus,
                                   family: String,
                                   choice: FamilyChoice) async {
        let key = familyKey(client: client.name, family: family)
        guard let pending = pendingFamilyRoutes[key],
              let snapshot = routingSnapshot else { return }
        let requestedTarget = familyChoiceArg(choice)
        let arguments = mutationArguments(
            operationID: pending.operationID,
            snapshot: snapshot,
            command: familyDispatchArgs(
                client: client.name,
                family: family,
                choice: choice))
        let attempt = await executePolicyMutation(
            arguments,
            operationID: pending.operationID,
            requestIdentity: PolicyMutationRequestIdentity(
                operation: "set_claude_route",
                client: client.name,
                key: family,
                requestedTarget: requestedTarget))
        await refreshAfterMutation()
        let receipt = attempt.receipt
        let confirmed = !snapshotIsStale
            && attempt.result.succeeded
            && receipt?.ok == true
            && receipt?.operationID == pending.operationID
            && receipt?.operation == "set_claude_route"
            && receipt?.client == client.name
            && receiptRequestIdentityConfirms(
                receipt,
                key: family,
                requestedTarget: requestedTarget,
                verifiedTerminalReplay: attempt.verifiedTerminalReplay)
            && receipt?.applied == true
            && hashesConfirm(receipt)
            && familyMutationConfirmed(
            cliResult: attempt.result,
            snapshot: routingSnapshot,
            clientName: client.name,
            familyName: family,
            choice: choice)
        if confirmed {
            lastError = nil
        } else {
            lastError = attempt.primaryTimedOut
                ? "\(capitalizeFamily(family)) mapping timed out and could not be confirmed."
                : mutationFailureMessage(
                    receipt,
                    fallback: "\(capitalizeFamily(family)) mapping was not present in the active router state.")
        }
        clearPending(key: key, operationID: pending.operationID)
        pendingFamilyRoutes.removeValue(forKey: key)
    }

    func isSubagentMutationPending(client: String) -> Bool {
        pendingSubagents[client] != nil
    }

    func pendingSubagentTarget(client: String) -> String? {
        pendingSubagents[client]?.requestedTarget
    }

    func requestSubagents(_ client: ClientStatus, choice: SubagentChoice) {
        guard beginSubagents(client, choice: choice) else { return }
        Task { await finishSubagents(client, choice: choice) }
    }

    func setSubagents(_ client: ClientStatus,
                      choice: SubagentChoice) async {
        guard beginSubagents(client, choice: choice) else { return }
        await finishSubagents(client, choice: choice)
    }

    private func beginSubagents(_ client: ClientStatus,
                                choice: SubagentChoice) -> Bool {
        guard canMutateRouting,
              pendingSubagents[client.name] == nil else { return false }
        let operationID = UUID().uuidString.lowercased()
        pendingSubagents[client.name] = PendingControlMutation(
            operationID: operationID,
            requestedTarget: subagentChoiceArg(choice),
            phase: .applying)
        scheduleReconciling(key: "subagent:\(client.name)",
                            operationID: operationID)
        return true
    }

    private func finishSubagents(_ client: ClientStatus,
                                 choice: SubagentChoice) async {
        guard let pending = pendingSubagents[client.name],
              let snapshot = routingSnapshot else { return }
        let requestedTarget = subagentChoiceArg(choice)
        let arguments = mutationArguments(
            operationID: pending.operationID,
            snapshot: snapshot,
            command: subagentDispatchArgs(client: client.name, choice: choice))
        let attempt = await executePolicyMutation(
            arguments,
            operationID: pending.operationID,
            requestIdentity: PolicyMutationRequestIdentity(
                operation: "set_claude_subagents",
                client: client.name,
                key: "subagents",
                requestedTarget: requestedTarget))
        await refreshAfterMutation()
        let receipt = attempt.receipt
        let confirmed = !snapshotIsStale
            && attempt.result.succeeded
            && receipt?.ok == true
            && receipt?.operationID == pending.operationID
            && receipt?.operation == "set_claude_subagents"
            && receipt?.client == client.name
            && receiptRequestIdentityConfirms(
                receipt,
                key: "subagents",
                requestedTarget: requestedTarget,
                verifiedTerminalReplay: attempt.verifiedTerminalReplay)
            && receipt?.applied == true
            && hashesConfirm(receipt)
            && subagentMutationConfirmed(
            cliResult: attempt.result,
            snapshot: routingSnapshot,
            clientName: client.name,
            choice: choice)
        if confirmed {
            lastError = nil
        } else {
            lastError = attempt.primaryTimedOut
                ? "The Subagents change timed out and could not be confirmed."
                : mutationFailureMessage(
                    receipt,
                    fallback: "The Subagents setting was not present in the active router state.")
        }
        clearPending(
            key: "subagent:\(client.name)",
            operationID: pending.operationID)
        pendingSubagents.removeValue(forKey: client.name)
    }

    func toggleSubagents(_ client: ClientStatus) async {
        let choice: SubagentChoice = client.subagentRouting == "off"
            ? client.modelCatalog.first.map(SubagentChoice.catalog) ?? .off
            : .off
        await setSubagents(client, choice: choice)
    }

    // MARK: - Client reasoning configuration

    var canMutateReasoning: Bool {
        guard gatewayUp,
              canMutate,
              mutationRecoveryAllowsRouting,
              let snapshot = routingSnapshot else { return false }
        return snapshot.token.isAuthoritative
            && snapshot.token.activeGeneration > 0
            && snapshot.desiredMatchesActive
    }

    func isReasoningMutationPending(
        client: String,
        provider: String,
        model: String
    ) -> Bool {
        pendingReasoning?.client == client
            && pendingReasoning?.provider == provider
            && pendingReasoning?.model == model
    }

    func pendingReasoningPolicy(
        client: String,
        provider: String,
        model: String
    ) -> ReasoningPolicyValue? {
        guard isReasoningMutationPending(
            client: client,
            provider: provider,
            model: model) else { return nil }
        return pendingReasoning?.policy
    }

    func reasoningWarning(
        client: String,
        provider: String,
        model: String
    ) -> String? {
        reasoningWarnings[
            reasoningKey(client: client, provider: provider, model: model)]
    }

    @discardableResult
    func requestReasoning(
        client: String,
        provider: String,
        model: String,
        policy: ReasoningPolicyValue
    ) -> Bool {
        guard canMutateReasoning,
              pendingReasoning == nil,
              policy.mode == .default
                || policy.mode == .off
                || policy.mode == .followHarness
                || policy.mode == .fixed else {
            return false
        }
        let operationID = UUID().uuidString.lowercased()
        pendingReasoning = PendingReasoningMutation(
            operationID: operationID,
            client: client,
            provider: provider,
            model: model,
            policy: policy,
            phase: policy.mode == .default ? .applying : .preflighting)
        if policy.mode == .default {
            scheduleReconciling(
                key: "reasoning",
                operationID: operationID)
            Task {
                await finishReasoningMutation(
                    operationID: operationID,
                    client: client,
                    provider: provider,
                    model: model,
                    policy: policy)
            }
            return true
        }
        Task {
            await preflightReasoningMutation(
                operationID: operationID,
                client: client,
                provider: provider,
                model: model,
                policy: policy)
        }
        return true
    }

    private func preflightReasoningMutation(
        operationID: String,
        client: String,
        provider: String,
        model: String,
        policy: ReasoningPolicyValue
    ) async {
        do {
            let preflight = try await reasoningPreflightReader
                .preflightReasoning(
                    client: client,
                    provider: provider,
                    model: model,
                    policy: policy)
            guard pendingReasoning?.operationID == operationID else { return }
            let key = reasoningKey(
                client: client,
                provider: provider,
                model: model)
            if preflight.warning.isEmpty {
                reasoningWarnings.removeValue(forKey: key)
            } else {
                reasoningWarnings[key] = preflight.warning
            }
            guard preflight.error.isEmpty else {
                lastError = menuErrorLabel(
                    redactDiagnosticText(preflight.error),
                    limit: 180)
                clearReasoningMutation(operationID: operationID)
                return
            }
            pendingReasoning?.phase = .applying
            scheduleReconciling(
                key: "reasoning",
                operationID: operationID)
            await finishReasoningMutation(
                operationID: operationID,
                client: client,
                provider: provider,
                model: model,
                policy: policy)
        } catch {
            guard pendingReasoning?.operationID == operationID else { return }
            lastError =
                "Reasoning compatibility could not be checked. No change was made."
            clearReasoningMutation(operationID: operationID)
        }
    }

    private func finishReasoningMutation(
        operationID: String,
        client: String,
        provider: String,
        model: String,
        policy: ReasoningPolicyValue
    ) async {
        guard pendingReasoning?.operationID == operationID,
              let snapshot = routingSnapshot else { return }
        let requestedTarget = reasoningRequestedTarget(policy)
        let command = reasoningDispatchArgs(
            client: client,
            protocolShape: snapshot.clients
                .first(where: { $0.name == client })?
                .protocolShape,
            provider: provider,
            model: model,
            policy: policy)
        let arguments = mutationArguments(
            operationID: operationID,
            snapshot: snapshot,
            command: command)
        let attempt = await executePolicyMutation(
            arguments,
            operationID: operationID,
            requestIdentity: PolicyMutationRequestIdentity(
                operation: "set_model_reasoning",
                client: client,
                key: model,
                requestedTarget: requestedTarget))
        await refreshAfterMutation()
        let receipt = attempt.receipt
        let confirmed = !snapshotIsStale
            && attempt.result.succeeded
            && receipt?.ok == true
            && receipt?.operationID == operationID
            && receipt?.operation == "set_model_reasoning"
            && receipt?.client == client
            && receiptRequestIdentityConfirms(
                receipt,
                key: model,
                requestedTarget: requestedTarget,
                verifiedTerminalReplay: attempt.verifiedTerminalReplay)
            && receipt?.applied == true
            && hashesConfirm(receipt)
            && reasoningMutationConfirmed(
                snapshot: routingSnapshot,
                client: client,
                provider: provider,
                model: model,
                policy: policy)
        if confirmed {
            lastError = nil
        } else {
            lastError = attempt.primaryTimedOut
                ? "The reasoning change timed out and could not be confirmed."
                : mutationFailureMessage(
                    receipt,
                    fallback:
                        "The reasoning setting was not present in the active router state.")
        }
        clearReasoningMutation(operationID: operationID)
    }

    private func clearReasoningMutation(operationID: String) {
        guard pendingReasoning?.operationID == operationID else { return }
        clearPending(key: "reasoning", operationID: operationID)
        pendingReasoning = nil
    }

    // MARK: - Supporting actions

    func reauthenticate() async {
        guard !reauthenticating else { return }
        guard variant.channel == .stable else {
            lastError = "Authentication changes are disabled in Baseten Switch Preview."
            return
        }
        guard let binary = Self.locateBasetenSwitchBinary(variant: variant) else {
            lastError = "baseten-switch is not installed."
            return
        }

        reauthenticating = true
        defer { reauthenticating = false }
        let script = reauthAppleScript(binaryPath: binary.path)
        let result = await cliRunner.run(CLIExecutionRequest(
            binary: URL(fileURLWithPath: "/usr/bin/osascript"),
            arguments: ["-e", script],
            environment: processEnvironment(),
            timeout: 10))
        lastError = result.succeeded
            ? nil
            : "Opening Terminal for reauthentication failed."
    }

    func startSystem() async {
        guard !starting else { return }
        starting = true
        defer { starting = false }
        let result = await executeCLI(
            ["up"],
            timeout: 30,
            allowWhenPreviewDown: true)
        guard result.succeeded else {
            lastError = result.timedOut
                ? "Starting Baseten Switch timed out."
                : "Baseten Switch could not be started."
            return
        }
        try? await clock.sleep(seconds: 1.5)
        await refresh()
    }

    var canStartSystem: Bool {
        mutationAllowed(allowWhenPreviewDown: true)
            && Self.locateBasetenSwitchBinary(variant: variant) != nil
    }

    func toggleStartAtLogin() {
        guard variant.allowsLoginItem else { return }
        loginItemService.toggle()
        loginItemStatus = loginItemService.status
    }

    func openLoginItemSettings() {
        guard variant.allowsLoginItem else { return }
        loginItemService.openSystemSettings()
        loginItemStatus = loginItemService.status
    }

    func quit() {
        NSApplication.shared.terminate(nil)
    }

    // MARK: - Process and trust helpers

    private func mutationArguments(
        operationID: String,
        snapshot: RoutingSnapshot,
        command: [String]
    ) -> [String] {
        [
            "--json",
            "--operation-id", operationID,
            "--if-active-token", snapshot.token.cliValue,
            "--if-config-hash", snapshot.desiredConfigHash,
        ] + command
    }

    private func receiptMatchesDispatchedRequest(
        _ receipt: GlobalMutationReceipt?,
        identity: PolicyMutationRequestIdentity?
    ) -> Bool {
        guard let receipt, let identity else { return false }
        return receipt.operation == identity.operation
            && receipt.client == identity.client
            && receipt.key == identity.key
            && receipt.requestedTarget == identity.requestedTarget
    }

    /// Policy mutations reconcile only when their result is missing or
    /// explicitly indeterminate. A valid deterministic failure is primary
    /// evidence and must not be replaced by a failed secondary lookup.
    private func executePolicyMutation(
        _ arguments: [String],
        operationID: String,
        requestIdentity: PolicyMutationRequestIdentity? = nil
    ) async -> PolicyMutationAttempt {
        let primary = await executeCLI(arguments, timeout: 30)
        let primaryReceipt = GlobalMutationReceipt(
            json: primary.standardOutput)
        let primaryReceiptMatches = primaryReceipt?.operationID == operationID
        let primaryRequiresRecovery = receiptRequiresMutationRecovery(
            primaryReceipt)
        if primaryRequiresRecovery {
            mutationRecoveryState = .checking
        }
        let primaryTypedError = primaryReceiptMatches
            && primaryReceipt?.errorCode.isEmpty == false
        if primaryReceiptMatches,
           primaryReceipt?.errorCode == "unfinished_mutation",
           primaryReceipt?.blockingOperationID.isEmpty == false {
            retainMutationRecoveryGateIfNeeded(primaryReceipt)
            return PolicyMutationAttempt(
                result: primary,
                receipt: primaryReceipt,
                primaryTimedOut: false,
                verifiedTerminalReplay: false)
        }
        if primaryReceiptMatches,
           primaryReceipt?.reconciliationRequired == false,
           !primary.timedOut || primaryTypedError {
            retainMutationRecoveryGateIfNeeded(primaryReceipt)
            return PolicyMutationAttempt(
                result: primary,
                receipt: primaryReceipt,
                primaryTimedOut: primary.timedOut && !primaryTypedError,
                verifiedTerminalReplay: false)
        }

        let needsReconciliation = primary.timedOut
            || !primaryReceiptMatches
            || primaryReceipt?.reconciliationRequired == true
        guard needsReconciliation else {
            return PolicyMutationAttempt(
                result: primary,
                receipt: primaryReceipt,
                primaryTimedOut: primary.timedOut && !primaryTypedError,
                verifiedTerminalReplay: false)
        }

        markReconciling(operationID: operationID)
        let reconciled = await executeCLI(
            ["--json", "mutation", "reconcile", operationID],
            timeout: 10)
        let reconciledReceipt = GlobalMutationReceipt(
            json: reconciled.standardOutput)
        let shouldUseReconciliation = reconciled.succeeded
            && reconciledReceipt?.operationID == operationID
            && reconciledReceipt?.ok == true
            && reconciledReceipt?.reconciliationRequired == false
            && reconciledReceipt?.cleanupPending == false
        let verifiedTerminalReplay = shouldUseReconciliation
            && receiptMatchesDispatchedRequest(
                primaryReceipt,
                identity: requestIdentity)
            && primaryReceipt?.identityStrength == "exact"
            && reconciledReceipt?.identityStrength == "exact"
            && isValidMutationRequestFingerprint(
                primaryReceipt?.requestFingerprint ?? "")
            && primaryReceipt?.requestFingerprint
                == reconciledReceipt?.requestFingerprint
        if shouldUseReconciliation {
            if primaryRequiresRecovery {
                mutationRecoveryState = .ready
            }
            return PolicyMutationAttempt(
                result: reconciled,
                receipt: reconciledReceipt,
                primaryTimedOut: false,
                verifiedTerminalReplay: verifiedTerminalReplay)
        }
        let unresolvedReceipt: GlobalMutationReceipt?
        if receiptRequiresMutationRecovery(reconciledReceipt) {
            unresolvedReceipt = reconciledReceipt
        } else if primaryRequiresRecovery {
            unresolvedReceipt = primaryReceipt
        } else {
            unresolvedReceipt = reconciledReceipt ?? primaryReceipt
        }
        retainMutationRecoveryGate(unresolvedReceipt)
        return PolicyMutationAttempt(
            result: primary,
            receipt: primaryReceipt,
            primaryTimedOut: primary.timedOut && !primaryTypedError,
            verifiedTerminalReplay: false)
    }

    private func receiptRequiresMutationRecovery(
        _ receipt: GlobalMutationReceipt?
    ) -> Bool {
        receipt?.cleanupPending == true
            || receipt?.reconciliationRequired == true
    }

    private func retainMutationRecoveryGateIfNeeded(
        _ receipt: GlobalMutationReceipt?
    ) {
        guard receiptRequiresMutationRecovery(receipt) else { return }
        retainMutationRecoveryGate(receipt)
    }

    private func retainMutationRecoveryGate(
        _ receipt: GlobalMutationReceipt?
    ) {
        let errorCode: String
        if receipt?.cleanupPending == true {
            errorCode = "cleanup_pending"
        } else if receipt?.errorCode.isEmpty == false {
            errorCode = receipt?.errorCode ?? "reconciliation_required"
        } else {
            errorCode = "reconciliation_required"
        }
        mutationRecoveryState = .blocked(errorCode: errorCode)
    }

    private func markReconciling(operationID: String) {
        if pendingGlobalRouting?.operationID == operationID {
            pendingGlobalRouting?.phase = .reconciling
        }
        for key in pendingFamilyRoutes.keys
        where pendingFamilyRoutes[key]?.operationID == operationID {
            pendingFamilyRoutes[key]?.phase = .reconciling
        }
        if pendingCodexRoute?.operationID == operationID {
            pendingCodexRoute?.phase = .reconciling
        }
        for key in pendingSubagents.keys
        where pendingSubagents[key]?.operationID == operationID {
            pendingSubagents[key]?.phase = .reconciling
        }
        if pendingReasoning?.operationID == operationID {
            pendingReasoning?.phase = .reconciling
        }
    }

    private func mutationFailureMessage(
        _ receipt: GlobalMutationReceipt?,
        fallback: String
    ) -> String {
        reviewedMutationErrorMessage(
            errorCode: receipt?.errorCode ?? "",
            fallback: fallback)
    }

    private func reviewedMutationErrorMessage(
        errorCode: String,
        fallback: String
    ) -> String {
        switch errorCode {
        case "mutation_locked":
            return "Another routing change is still in progress. Try again shortly."
        case "unfinished_mutation":
            return "A previous routing change still needs cleanup."
        case "router_unavailable":
            return "The local gateway is unavailable. Try again after it reconnects."
        case "router_state_mismatch", "router_identity_mismatch":
            return "The app and routing command are connected to different local gateways. Restart Baseten Switch and try again."
        case "router_unsupported":
            return "Update the local gateway before changing routing settings."
        case "journal_not_found":
            return "The routing change outcome is no longer available. Refresh and try again."
        case "journal_invalid", "journal_conflict", "terminal_conflict":
            return "Saved routing recovery data needs manual attention."
        case "commit_recovery_required":
            return "A configuration update needs manual recovery before routing can change."
        case "external_change":
            return "The configuration changed outside Baseten Switch. Refresh before trying again."
        case "stale_active_token", "stale_config_hash", "precondition_failed":
            return "Routing settings changed before this request completed. Refresh and try again."
        case "operation_id_conflict":
            return "This routing request conflicts with an earlier request. Try again."
        case "activation_failed_rolled_back", "mutation_not_applied":
            return "The routing change was not applied. The last confirmed setting remains active."
        case "terminal_write_failed", "activation_indeterminate":
            return "The routing change could not be confirmed safely. Retry cleanup."
        case "status_unavailable":
            return "Routing cleanup status could not be checked."
        case "cleanup_failed":
            return "Routing cleanup could not be completed safely."
        case "cleanup_pending":
            return "Routing cleanup is still pending."
        default:
            return fallback
        }
    }

    private func executeCLI(
        _ arguments: [String],
        timeout: TimeInterval,
        allowWhenPreviewDown: Bool = false
    ) async -> CLIExecutionResult {
        guard mutationAllowed(allowWhenPreviewDown: allowWhenPreviewDown) else {
            lastError = runtimeTrustError
            return failedCLIResult
        }
        if variant.channel == .preview,
           let error = previewRuntimeValidator(variant.runtime) {
            lastError = error
            return failedCLIResult
        }
        guard let binary = Self.locateBasetenSwitchBinary(variant: variant) else {
            lastError = variant.channel == .preview
                ? "Baseten Switch Preview requires BASETEN_SWITCH_GATEWAY_BIN from its launcher."
                : "baseten-switch is not installed."
            return failedCLIResult
        }
        guard let mutationCoordinator else {
            return failedCLIResult
        }
        return await mutationCoordinator.perform(CLIExecutionRequest(
            binary: binary,
            arguments: arguments,
            environment: processEnvironment(),
            timeout: timeout))
    }

    private var failedCLIResult: CLIExecutionResult {
        CLIExecutionResult(
            status: -1,
            standardOutput: "",
            standardError: "",
            timedOut: false)
    }

    private func processEnvironment() -> [String: String] {
        allowlistedCLIEnvironment(
            overrides: cliEnvironmentOverrides(
                configPath: activeConfigPath,
                variant: variant))
    }

    private func mutationAllowed(allowWhenPreviewDown: Bool) -> Bool {
        switch runtimeTrust {
        case .stable, .previewTrusted:
            return true
        case .previewDown:
            return allowWhenPreviewDown
        case .previewMismatch, .identityMismatch:
            return false
        }
    }

    private var runtimeTrustError: String {
        switch runtimeTrust {
        case .stable, .previewTrusted:
            return ""
        case .previewDown:
            return "Baseten Switch Preview is not running."
        case .previewMismatch(let expected, let reported):
            return "Preview runtime mismatch. Expected \(expected), reported \(reported)."
        case .identityMismatch(let reason):
            return reason
        }
    }

    private func updateRuntimeTrust(snapshot: RoutingSnapshot) {
        let updated = runtimeTrustForSnapshot(
            variant: variant,
            snapshot: snapshot,
            gatewayUp: true)
        if runtimeTrust != updated {
            runtimeTrust = updated
        }
        if case .previewMismatch = updated {
            lastError = runtimeTrustError
        } else if case .identityMismatch = updated {
            lastError = runtimeTrustError
        }
    }

    private func familyKey(client: String, family: String) -> String {
        "family:\(client):\(family)"
    }

    private func reasoningKey(
        client: String,
        provider: String,
        model: String
    ) -> String {
        "\(client):\(provider):\(model)"
    }

    private func scheduleReconciling(key: String, operationID: String) {
        reconcileTimers[key]?.cancel()
        reconcileTimers[key] = Task { [weak self] in
            try? await Task.sleep(nanoseconds: 10_000_000_000)
            guard !Task.isCancelled, let self else { return }
            if key == "global",
               self.pendingGlobalRouting?.operationID == operationID {
                self.pendingGlobalRouting?.phase = .reconciling
            } else if key.hasPrefix("family:"),
                      self.pendingFamilyRoutes[key]?.operationID == operationID {
                self.pendingFamilyRoutes[key]?.phase = .reconciling
            } else if key == "codex-route",
                      self.pendingCodexRoute?.operationID == operationID {
                self.pendingCodexRoute?.phase = .reconciling
            } else if key.hasPrefix("subagent:") {
                let client = String(key.dropFirst("subagent:".count))
                if self.pendingSubagents[client]?.operationID == operationID {
                    self.pendingSubagents[client]?.phase = .reconciling
                }
            } else if key == "reasoning",
                      self.pendingReasoning?.operationID == operationID {
                self.pendingReasoning?.phase = .reconciling
            }
        }
    }

    private func clearPending(key: String, operationID: String) {
        _ = operationID
        reconcileTimers[key]?.cancel()
        reconcileTimers.removeValue(forKey: key)
    }

    nonisolated static func locateBasetenSwitchBinary(
        variant: AppVariant = .current()
    ) -> URL? {
        if variant.channel == .preview {
            guard let path = variant.runtime.environment["BASETEN_SWITCH_GATEWAY_BIN"],
                  FileManager.default.isExecutableFile(atPath: path) else {
                return nil
            }
            return URL(fileURLWithPath: path)
        }
        var candidates: [String] = []
        if let raw = variant.runtime.environment["BASETEN_SWITCH_GATEWAY_BIN"],
           !raw.isEmpty {
            candidates.append(raw)
        } else if let raw = ProcessInfo.processInfo.environment["BASETEN_SWITCH_GATEWAY_BIN"],
           !raw.isEmpty {
            candidates.append(raw)
        }
        candidates.append("\(NSHomeDirectory())/.local/bin/baseten-switch")
        candidates.append("/opt/homebrew/opt/baseten-switch/bin/baseten-switch")
        candidates.append("/usr/local/opt/baseten-switch/bin/baseten-switch")
        return candidates
            .first(where: FileManager.default.isExecutableFile)
            .map(URL.init(fileURLWithPath:))
    }
}

func reasoningDispatchArgs(
    client: String,
    protocolShape: String? = nil,
    provider: String,
    model: String,
    policy: ReasoningPolicyValue
) -> [String] {
    let harness: String
    switch protocolShape {
    case "anthropic":
        harness = "claude"
    case "openai":
        harness = "codex"
    default:
        harness = client == "claude-code" ? "claude" : client
    }
    var arguments = [harness, "reasoning", provider, model]
    switch policy.mode {
    case .off:
        arguments.append("off")
    case .followHarness:
        arguments.append("follow-harness")
    case .fixed:
        arguments.append(contentsOf: ["effort", policy.effort])
    case .default:
        arguments.append("default")
    case .passthrough:
        return []
    }
    return arguments
}

func reasoningRequestedTarget(_ policy: ReasoningPolicyValue) -> String {
    switch policy.mode {
    case .default:
        return "default"
    case .off:
        return "off"
    case .followHarness:
        return "follow_harness"
    case .fixed:
        return "effort:\(policy.effort)"
    case .passthrough:
        return "passthrough"
    }
}

func reasoningMutationConfirmed(
    snapshot: RoutingSnapshot?,
    client: String,
    provider: String,
    model: String,
    policy: ReasoningPolicyValue
) -> Bool {
    guard let snapshot, snapshot.desiredMatchesActive else { return false }
    let projected = snapshot.clients
        .first(where: { $0.name == client })?
        .modelOptions[provider]?[model]?.reasoning
    if policy.mode == .default {
        return projected == nil || projected?.configured.mode == .default
    }
    return projected?.configured.mode == policy.mode
        && projected?.configured.effort == policy.effort
}

func routingPresentationEqual(_ lhs: RoutingSnapshot,
                              _ rhs: RoutingSnapshot) -> Bool {
    lhs.token == rhs.token
        && lhs.activeConfigHash == rhs.activeConfigHash
        && lhs.desiredConfigHash == rhs.desiredConfigHash
        && lhs.gateway == rhs.gateway
        && lhs.health == rhs.health
        && lhs.version == rhs.version
        && lhs.activeRequests == rhs.activeRequests
        && lhs.configPath == rhs.configPath
        && lhs.capabilities == rhs.capabilities
        && lhs.globalRoutingEnabled == rhs.globalRoutingEnabled
        && lhs.reload == rhs.reload
        && lhs.auth == rhs.auth
        && lhs.clients == rhs.clients
}

func projectedUptimeSeconds(snapshot: RoutingSnapshot?,
                            now: Date) -> Int64 {
    guard let snapshot else { return 0 }
    let elapsed = max(0, now.timeIntervalSince(snapshot.observedAt))
    return snapshot.uptimeSeconds + Int64(elapsed)
}

func pendingGlobalRoutingDisabledReason(
    _ pending: PendingGlobalRouting
) -> String {
    let requestedState = pending.requested ? "On" : "Off"
    switch pending.phase {
    case .applying:
        return "Waiting for the gateway to confirm the routing change to \(requestedState)."
    case .reconciling:
        return "The routing change to \(requestedState) is taking longer than expected. Waiting for gateway confirmation."
    }
}

func familyMutationConfirmed(
    cliResult: CLIExecutionResult,
    snapshot: RoutingSnapshot?,
    clientName: String,
    familyName: String,
    choice: FamilyChoice
) -> Bool {
    guard cliResult.succeeded,
          let snapshot,
          snapshot.desiredMatchesActive,
          let client = snapshot.clients.first(where: { $0.name == clientName }),
          let family = client.families.first(where: {
              $0.family == familyName
          }) else { return false }
    return familyChoiceChecked(family: family, choice: choice)
}

func codexRouteMutationConfirmed(
    cliResult: CLIExecutionResult,
    snapshot: RoutingSnapshot?,
    model: ModelCatalogEntry
) -> Bool {
    guard cliResult.succeeded,
          let snapshot,
          snapshot.desiredMatchesActive,
          let client = snapshot.clients.first(where: {
              $0.name == "codex"
          }) else { return false }
    return codexRouteChoiceChecked(client: client, model: model)
}

func subagentMutationConfirmed(
    cliResult: CLIExecutionResult,
    snapshot: RoutingSnapshot?,
    clientName: String,
    choice: SubagentChoice
) -> Bool {
    guard cliResult.succeeded,
          let snapshot,
          snapshot.desiredMatchesActive,
          let client = snapshot.clients.first(where: {
              $0.name == clientName
          }) else { return false }
    return subagentChoiceChecked(
        subagentModel: client.subagentModel,
        subagentRouting: client.subagentRouting,
        choice: choice)
}

func cliEnvironmentOverrides(configPath: String) -> [String: String] {
    configPath.isEmpty ? [:] : ["BASETEN_SWITCH_CONFIG_PATH": configPath]
}

func cliEnvironmentOverrides(configPath: String,
                             variant: AppVariant) -> [String: String] {
    if variant.channel == .preview {
        return variant.runtime.environment
    }
    let coordinationKeys = [
        "BASETEN_SWITCH_ADMIN_ADDR",
        "BASETEN_SWITCH_GATEWAY_PIDFILE",
    ]
    var overrides = variant.runtime.environment.filter {
        coordinationKeys.contains($0.key)
    }
    let effectiveConfigPath = configPath.isEmpty
        ? variant.runtime.environment["BASETEN_SWITCH_CONFIG_PATH"] ?? ""
        : configPath
    if !effectiveConfigPath.isEmpty {
        // The explicit launch path is a safe pre-status fallback. Once status
        // is available, the router-reported path is authoritative.
        overrides["BASETEN_SWITCH_CONFIG_PATH"] = effectiveConfigPath
    }
    return overrides
}

func canonicalPath(_ path: String) -> String {
    guard !path.isEmpty else { return "" }
    return URL(fileURLWithPath: path)
        .standardizedFileURL
        .resolvingSymlinksInPath()
        .path
}

func runtimeTrustForStatus(
    channel: AppChannel,
    expectedConfigPath: String?,
    reportedConfigPath: String?,
    gatewayUp: Bool
) -> RuntimeTrust {
    guard channel == .preview else { return .stable }
    guard gatewayUp else { return .previewDown }
    let expected = canonicalPath(expectedConfigPath ?? "")
    let reported = canonicalPath(reportedConfigPath ?? "")
    guard !expected.isEmpty, expected == reported else {
        return .previewMismatch(
            expected: expected.isEmpty ? "(missing)" : expected,
            reported: reported.isEmpty ? "(missing)" : reported)
    }
    return .previewTrusted
}

func runtimeTrustForSnapshot(
    variant: AppVariant,
    snapshot: RoutingSnapshot,
    gatewayUp: Bool
) -> RuntimeTrust {
    if let error = variant.identityError {
        return .identityMismatch(reason: error)
    }
    guard variant.channel == .preview else { return .stable }
    guard gatewayUp else { return .previewDown }

    let pathTrust = runtimeTrustForStatus(
        channel: .preview,
        expectedConfigPath: variant.runtime.expectedConfigPath,
        reportedConfigPath: snapshot.configPath,
        gatewayUp: true)
    guard pathTrust == .previewTrusted else { return pathTrust }

    let runtime = variant.runtime
    let expectedEnvironment = [
        "BASETEN_SWITCH_CONFIG_PATH": variant.runtime.expectedConfigPath ?? "",
        "BASETEN_SWITCH_ADMIN_ADDR": "127.0.0.1:45373",
        "BASETEN_SWITCH_ADMIN_URL": "http://127.0.0.1:45373",
        "BASETEN_SWITCH_GATEWAY_ADMIN": "http://127.0.0.1:45373",
        "BASETEN_SWITCH_GATEWAY_PORT": "45373",
        "BASETEN_SWITCH_DOOR_PORTS": "45371",
    ]
    for (key, expected) in expectedEnvironment
    where runtime.environment[key] != expected {
        return .identityMismatch(
            reason: "Preview runtime identity mismatch: \(key) must be \(expected).")
    }
    guard runtime.adminBaseURL.scheme == "http",
          runtime.adminBaseURL.host == "127.0.0.1",
          runtime.adminBaseURL.port == 45_373 else {
        return .identityMismatch(
            reason: "Preview runtime identity mismatch: the admin port is not isolated.")
    }
    guard runtime.doorURLs == [
        URL(string: "http://127.0.0.1:45371/doorz")!,
    ] else {
        return .identityMismatch(
            reason: "Preview runtime identity mismatch: the door port is not isolated.")
    }
    if let wrongClient = snapshot.clients.first(where: {
        !$0.bindAddr.isEmpty && $0.bindAddr != "127.0.0.1:45372"
    }) {
        return .identityMismatch(
            reason: "Preview runtime identity mismatch: \(wrongClient.name) is bound to \(wrongClient.bindAddr), not 127.0.0.1:45372.")
    }
    return .previewTrusted
}
