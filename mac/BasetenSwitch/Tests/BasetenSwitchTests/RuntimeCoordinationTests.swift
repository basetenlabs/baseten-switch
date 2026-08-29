import Foundation
import ServiceManagement
import XCTest
@testable import BasetenSwitch

private actor CountingAdminReader: AdminStatusReading {
    private(set) var statusCalls = 0
    private(set) var statsCalls = 0
    let status: AdminStatusSnapshot
    let delayNanoseconds: UInt64

    init(status: AdminStatusSnapshot,
         delayNanoseconds: UInt64 = 0) {
        self.status = status
        self.delayNanoseconds = delayNanoseconds
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        statusCalls += 1
        if delayNanoseconds > 0 {
            try await Task.sleep(nanoseconds: delayNanoseconds)
        }
        return status
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        statsCalls += 1
        if delayNanoseconds > 0 {
            try await Task.sleep(nanoseconds: delayNanoseconds)
        }
        return StatsSnapshot(dict: [
            "window_seconds": windowSeconds,
            "bucket_seconds": bucketSeconds,
        ])
    }
}

private actor SequencedAdminReader: AdminStatusReading {
    private var statuses: [AdminStatusSnapshot]
    private var index = 0
    private(set) var statusCalls = 0
    private(set) var statsCalls = 0

    init(statuses: [AdminStatusSnapshot]) {
        self.statuses = statuses
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        statusCalls += 1
        guard !statuses.isEmpty else {
            throw GatewayClientError.invalidPayload
        }
        let status = statuses[min(index, statuses.count - 1)]
        index += 1
        return status
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        statsCalls += 1
        return StatsSnapshot(dict: [:])
    }
}

private actor SuspendingMutationAdminReader: AdminStatusReading {
    private let initial: AdminStatusSnapshot
    private let stale: AdminStatusSnapshot
    private let confirmed: AdminStatusSnapshot
    private var continuation: CheckedContinuation<Void, Never>?
    private(set) var statusCalls = 0

    init(initial: AdminStatusSnapshot,
         stale: AdminStatusSnapshot,
         confirmed: AdminStatusSnapshot) {
        self.initial = initial
        self.stale = stale
        self.confirmed = confirmed
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        statusCalls += 1
        switch statusCalls {
        case 1:
            return initial
        case 2:
            await withCheckedContinuation { continuation in
                self.continuation = continuation
            }
            return stale
        default:
            return confirmed
        }
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }

    func waitForStaleRequest() async {
        while statusCalls < 2 {
            await Task.yield()
        }
    }

    func releaseStaleRequest() {
        continuation?.resume()
        continuation = nil
    }
}

private struct FixedClock: RuntimeClock {
    let now: Date

    func sleep(seconds: TimeInterval) async throws {
        try await Task.sleep(
            nanoseconds: UInt64(max(seconds, 0) * 1_000_000_000))
    }
}

private final class RecordingClock: RuntimeClock, @unchecked Sendable {
    let now: Date
    private let queue = DispatchQueue(label: "mutation-recovery-test-clock")
    private var values: [TimeInterval] = []

    init(now: Date = Date(timeIntervalSince1970: 100)) {
        self.now = now
    }

    func sleep(seconds: TimeInterval) async throws {
        try Task.checkCancellation()
        queue.sync {
            values.append(seconds)
        }
    }

    var sleeps: [TimeInterval] {
        queue.sync { values }
    }
}

private actor RecoveryScriptRunner: CLIRunning {
    private(set) var arguments: [[String]] = []
    private let statusResult: CLIExecutionResult
    private var recoverResults: [CLIExecutionResult]

    init(statusResult: CLIExecutionResult,
         recoverResults: [CLIExecutionResult]) {
        self.statusResult = statusResult
        self.recoverResults = recoverResults
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        if request.arguments.suffix(2) == ["mutation", "status"] {
            return statusResult
        }
        if request.arguments.suffix(2) == ["mutation", "recover"] {
            if !recoverResults.isEmpty {
                return recoverResults.removeFirst()
            }
        }
        return CLIExecutionResult(
            status: 1,
            standardOutput: "",
            standardError: "",
            timedOut: false)
    }
}

private actor PrimaryFailureRunner: CLIRunning {
    enum Mode: Equatable {
        case blocker
        case doubleTimeout
        case failedReconciliation
        case malformedSecondary
        case mismatchedSecondary
        case routerIdentityMismatch
        case routerStateMismatch
        case timedTypedError
        case timedSuccess
    }

    private(set) var arguments: [[String]] = []
    private let mode: Mode

    init(mode: Mode) {
        self.mode = mode
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        let args = request.arguments
        let operationID: String
        if let flag = args.firstIndex(of: "--operation-id"),
           args.indices.contains(flag + 1) {
            operationID = args[flag + 1]
        } else {
            operationID = args.last ?? ""
        }
        let reconciling = args.suffix(3).dropLast().contains("mutation")
            || (args.contains("mutation") && args.contains("reconcile"))
        if mode == .doubleTimeout
            || mode == .malformedSecondary
            || mode == .mismatchedSecondary {
            if !reconciling || mode == .doubleTimeout {
                return CLIExecutionResult(
                    status: -1,
                    standardOutput: "",
                    standardError: "",
                    timedOut: true)
            }
            if mode == .malformedSecondary {
                return CLIExecutionResult(
                    status: 1,
                    standardOutput: "{not-json",
                    standardError: "",
                    timedOut: false)
            }
            let object: [String: Any] = [
                "ok": true,
                "operation_id": "different-operation",
                "operation": "set_global_routing",
                "requested": true,
                "desired_config_hash": "sha256:active",
                "active_config_hash": "sha256:active",
                "active_token": "boot-a:4",
                "applied": true,
                "reconciliation_required": false,
                "error": NSNull(),
            ]
            let data = try! JSONSerialization.data(withJSONObject: object)
            return CLIExecutionResult(
                status: 0,
                standardOutput: String(decoding: data, as: UTF8.self),
                standardError: "",
                timedOut: false)
        }
        if mode == .timedSuccess {
            let object: [String: Any] = [
                "ok": true,
                "operation_id": operationID,
                "operation": "set_global_routing",
                "requested": true,
                "desired_config_hash": "sha256:active",
                "active_config_hash": "sha256:active",
                "active_token": "boot-a:4",
                "applied": true,
                "reconciliation_required": false,
                "error": NSNull(),
            ]
            let data = try! JSONSerialization.data(withJSONObject: object)
            return CLIExecutionResult(
                status: reconciling ? 0 : -1,
                standardOutput: String(decoding: data, as: UTF8.self),
                standardError: "",
                timedOut: !reconciling)
        }
        let errorCode: String
        let blockingID: String?
        let reconciliationRequired: Bool
        if mode == .blocker {
            errorCode = "unfinished_mutation"
            blockingID = "older-operation"
            reconciliationRequired = false
        } else if mode == .routerIdentityMismatch {
            errorCode = "router_identity_mismatch"
            blockingID = nil
            reconciliationRequired = false
        } else if mode == .routerStateMismatch {
            errorCode = "router_state_mismatch"
            blockingID = nil
            reconciliationRequired = false
        } else if mode == .timedTypedError {
            errorCode = "stale_config_hash"
            blockingID = nil
            reconciliationRequired = false
        } else if reconciling {
            errorCode = "journal_not_found"
            blockingID = nil
            reconciliationRequired = false
        } else {
            errorCode = "activation_indeterminate"
            blockingID = nil
            reconciliationRequired = true
        }
        var object: [String: Any] = [
            "ok": false,
            "operation_id": operationID,
            "operation": "set_global_routing",
            "applied": false,
            "reconciliation_required": reconciliationRequired,
            "error": [
                "code": errorCode,
                "message": "open /Users/example/private/model-id.json: no such file",
                "retryable": false,
            ],
        ]
        if let blockingID {
            object["blocking_operation_id"] = blockingID
        }
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: mode == .timedTypedError ? -1 : 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: mode == .timedTypedError)
    }
}

private actor CleanupPendingMutationRunner: CLIRunning {
    enum Mode: Equatable {
        case rejected
        case priorActive
    }

    private(set) var arguments: [[String]] = []
    private let mode: Mode

    init(mode: Mode) {
        self.mode = mode
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        let args = request.arguments
        let operationID = argumentValue("--operation-id", in: args)
            ?? args.last
            ?? ""
        let reconciling = args.contains("reconcile")
        let rejected = mode == .rejected
        let object: [String: Any]
        if reconciling {
            object = [
                "ok": true,
                "operation_id": operationID,
                "operation": "set_global_routing",
                "requested": true,
                "desired_config_hash": "sha256:new",
                "active_config_hash": "sha256:active",
                "active_token": "boot-a:4",
                "applied": false,
                "reconciliation_required": false,
                "cleanup_pending": true,
                "outcome": "not_applied",
                "identity_strength": "exact",
                "request_fingerprint":
                    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                "error": NSNull(),
            ]
        } else {
            object = [
                "ok": false,
                "operation_id": operationID,
                "operation": "set_global_routing",
                "requested": true,
                "desired_config_hash": rejected
                    ? "sha256:active"
                    : "sha256:new",
                "active_config_hash": "sha256:active",
                "active_token": "boot-a:4",
                "applied": false,
                "reconciliation_required": !rejected,
                "cleanup_pending": rejected,
                "outcome": rejected ? "rejected" : "",
                "error": [
                    "code": rejected
                        ? "stale_config_hash"
                        : "activation_indeterminate",
                    "message": "reviewed mutation error",
                    "retryable": false,
                ],
            ]
        }
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: reconciling ? 0 : 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(
        _ flag: String,
        in arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

private actor ConcurrencyRecordingRunner: CLIRunning {
    private(set) var active = 0
    private(set) var maximumActive = 0
    private(set) var arguments: [[String]] = []

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        active += 1
        maximumActive = max(maximumActive, active)
        arguments.append(request.arguments)
        try? await Task.sleep(nanoseconds: 50_000_000)
        active -= 1
        return CLIExecutionResult(
            status: 0,
            standardOutput: "",
            standardError: "",
            timedOut: false)
    }
}

private actor BlockingFirstRunner: CLIRunning {
    private(set) var arguments: [[String]] = []
    private var firstContinuation: CheckedContinuation<Void, Never>?

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        if arguments.count == 1 {
            await withCheckedContinuation { continuation in
                firstContinuation = continuation
            }
        }
        return CLIExecutionResult(
            status: 0,
            standardOutput: "",
            standardError: "",
            timedOut: false)
    }

    func waitUntilFirstStarted() async {
        while arguments.isEmpty {
            await Task.yield()
        }
    }

    func releaseFirst() {
        firstContinuation?.resume()
        firstContinuation = nil
    }
}

private actor ReconciliationReceiptRunner: CLIRunning {
    private static let requestFingerprint =
        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    private(set) var arguments: [[String]] = []
    private var journalOperation = ""
    private var journalClient = ""
    private var journalKey = ""
    private var journalTarget = ""
    private let reconciledFingerprint: String
    private let primaryTargetOverride: String?
    private let clientOverride: String?

    init(
        reconciledFingerprint: String = requestFingerprint,
        primaryTargetOverride: String? = nil,
        clientOverride: String? = nil
    ) {
        self.reconciledFingerprint = reconciledFingerprint
        self.primaryTargetOverride = primaryTargetOverride
        self.clientOverride = clientOverride
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        let args = request.arguments
        let operationID = argumentValue("--operation-id", in: args)
            ?? args.last
            ?? ""
        let reconciling = args.contains("mutation")
        let routeIndex = args.firstIndex(of: "route")
        let codexCommand = args.contains("codex")
        let subagentIndex = args.firstIndex(of: "subagents")
        var operation: String
        var client: String
        var key: String
        var target: String
        if codexCommand {
            operation = "set_codex_route"
            client = "codex"
            key = "default_model"
            target = routeIndex.flatMap {
                args.indices.contains($0 + 1) ? args[$0 + 1] : nil
            } ?? ""
        } else {
            operation = routeIndex == nil
                ? "set_claude_subagents"
                : "set_claude_route"
            client = "claude-code"
            key = routeIndex.flatMap {
                args.indices.contains($0 + 1) ? args[$0 + 1] : nil
            } ?? "subagents"
            target = routeIndex.flatMap {
                args.indices.contains($0 + 2) ? args[$0 + 2] : nil
            } ?? subagentIndex.flatMap {
                args.indices.contains($0 + 1) ? args[$0 + 1] : nil
            } ?? ""
        }
        if reconciling {
            operation = journalOperation
            client = journalClient
            key = journalKey
            target = journalTarget
        } else {
            client = clientOverride ?? client
            journalOperation = operation
            journalClient = client
            journalKey = key
            journalTarget = target
        }
        let ok = reconciling
        var object: [String: Any] = [
            "ok": ok,
            "operation_id": operationID,
            "operation": operation,
            "client": client,
            "desired_config_hash": "sha256:new",
            "active_token": "boot-a:5",
            "active_config_hash": "sha256:new",
            "applied": ok,
            "reconciliation_required": !ok,
            "request_fingerprint": reconciling
                ? reconciledFingerprint
                : Self.requestFingerprint,
            "identity_strength": "exact",
            "error": ok
                ? NSNull()
                : [
                    "code": "activation_indeterminate",
                    "message": "reconciliation required",
                ],
        ]
        if !reconciling {
            object["key"] = key
            object["requested_target"] = primaryTargetOverride ?? target
        }
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: ok ? 0 : 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(_ flag: String,
                               in arguments: [String]) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

@MainActor
private final class FakeLoginItemService: LoginItemServicing {
    var status: SMAppService.Status = .notRegistered
    private(set) var reconcileCalls = 0

    func reconcileAtLaunch() {
        reconcileCalls += 1
    }

    func toggle() {}
    func openSystemSettings() {}
}

final class RuntimeCoordinationTests: XCTestCase {
    private let previewConfigPath =
        "/tmp/baseten-switch-home/.config/baseten-switch-preview/gateway.yaml"

    private func previewVariant(
        binaryPath: String = "/usr/bin/true"
    ) -> AppVariant {
        AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitchPreview",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": binaryPath])
    }

    private func stableTestVariant(
        binaryPath: String = "/usr/bin/true"
    ) -> AppVariant {
        AppVariant.resolve(
            infoDictionary: [:],
            homeDirectory: "/tmp/baseten-switch-home",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": binaryPath])
    }

    @MainActor
    private func drainMainQueue() async {
        await withCheckedContinuation { continuation in
            DispatchQueue.main.async {
                continuation.resume()
            }
        }
    }

    private func isolatedPreviewVariant() throws -> AppVariant {
        let home = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let root = home
            .appendingPathComponent(".config/baseten-switch-preview", isDirectory: true)
        let manager = FileManager.default
        try manager.createDirectory(
            at: root,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: NSNumber(value: 0o700)])
        for name in ["logs", "backups", "claude", "baseten"] {
            try manager.createDirectory(
                at: root.appendingPathComponent(name, isDirectory: true),
                withIntermediateDirectories: false,
                attributes: [.posixPermissions: NSNumber(value: 0o700)])
        }
        for name in ["gateway.yaml", "env", "auth.json"] {
            let path = root.appendingPathComponent(name)
            XCTAssertTrue(manager.createFile(
                atPath: path.path,
                contents: Data(),
                attributes: [.posixPermissions: NSNumber(value: 0o600)]))
        }
        addTeardownBlock {
            try? manager.removeItem(at: home)
        }
        return AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitchPreview",
            homeDirectory: home.path,
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true"])
    }

    private func previewStatus(
        generation: Int,
        hash: String,
        familyTarget: String,
        subagentModel: String = "",
        subagentRouting: String = "off",
        bootID: String = "boot-a",
        clientName: String = "claude-code"
    ) -> AdminStatusSnapshot {
        AdminStatusSnapshot(dict: [
            "router_boot_id": bootID,
            "active_generation": generation,
            "active_config_hash": hash,
            "desired_config_hash": hash,
                        "capabilities": ["global_routing"],
            "health": "ready",
            "config_path": previewConfigPath,
            "global_routing_enabled": false,
            "clients": [[
                "name": clientName,
                "enabled": true,
                "bind_addr": "127.0.0.1:45372",
                "protocol_shape": "anthropic",
                "subagent_model": subagentModel,
                "subagent_routing": subagentRouting,
                "families": [[
                    "family": "opus",
                    "configured_target": familyTarget,
                    "configured_source": "explicit",
                    "effective_route": "anthropic",
                    "effective_source": "global_off",
                ]],
                "model_catalog": [[
                    "label": "GLM-5.2",
                    "storage_target": "zai-org/GLM-5.2",
                    "available": true,
                ]],
            ]],
        ])
    }

    private func menuAuthStatus(
        generation: Int,
        signedIn: Bool,
        health: String
    ) -> AdminStatusSnapshot {
        AdminStatusSnapshot(dict: [
            "router_boot_id": "boot-menu-auth",
            "active_generation": generation,
            "active_config_hash": "sha256:menu-auth-\(generation)",
            "desired_config_hash": "sha256:menu-auth-\(generation)",
            "capabilities": ["global_routing"],
            "health": "ready",
            "config_path": previewConfigPath,
            "global_routing_enabled": true,
            "auth": [
                "signed_in": signedIn,
                "health": health,
                "fallback_in_use": false,
            ],
            "clients": [[
                "name": "claude-code",
                "enabled": true,
                "bind_addr": "127.0.0.1:45372",
                "protocol_shape": "anthropic",
                "effective_route": "baseten",
                "native_route": "anthropic",
            ]],
        ])
    }

    private func currentStatus(
        bootID: String = "boot-a",
        generation: Int = 4,
        enabled: Bool = true
    ) -> AdminStatusSnapshot {
        AdminStatusSnapshot(dict: [
            "router_boot_id": bootID,
            "active_generation": generation,
            "active_config_hash": "sha256:active",
            "desired_config_hash": "sha256:active",
                        "capabilities": ["global_routing"],
            "health": "ready",
            "global_routing_enabled": enabled,
            "clients": [],
        ])
    }

    private func codexStatus(
        generation: Int,
        hash: String,
        configuredTarget: String,
        effectiveModel: String? = nil
    ) -> AdminStatusSnapshot {
        AdminStatusSnapshot(dict: [
            "router_boot_id": "boot-a",
            "active_generation": generation,
            "active_config_hash": hash,
            "desired_config_hash": hash,
            "capabilities": ["global_routing"],
            "health": "ready",
            "config_path": previewConfigPath,
            "global_routing_enabled": true,
            "clients": [[
                "name": "codex",
                "enabled": true,
                "bind_addr": "127.0.0.1:45372",
                "protocol_shape": "openai",
                "unmatched_native_model": [
                    "configured_target": configuredTarget,
                    "effective_route": "baseten",
                    "effective_model": effectiveModel ?? configuredTarget,
                    "effective_source": "default_model",
                ],
                "model_catalog": [
                    [
                        "label": "GLM 5.2",
                        "storage_target": "zai-org/GLM-5.2",
                        "slug": "zai-org/GLM-5.2",
                        "available": true,
                    ],
                    [
                        "label": "Qwen 3 Coder",
                        "storage_target": "Qwen/Qwen3-Coder",
                        "slug": "Qwen/Qwen3-Coder",
                        "available": true,
                    ],
                ],
            ]],
        ])
    }

    private func routingSnapshot(client: [String: Any])
        -> RoutingSnapshot {
        RoutingSnapshot(
            status: AdminStatusSnapshot(dict: [
                "router_boot_id": "boot-a",
                "active_generation": 4,
                "active_config_hash": "sha256:active",
                "desired_config_hash": "sha256:active",
                                "capabilities": ["global_routing"],
                "global_routing_enabled": true,
                "clients": [client],
            ]),
            observedAt: Date(timeIntervalSince1970: 100))
    }

    func testRoutingTokenRejectsOlderGenerationAndRetiredBoot() {
        var acceptance = RoutingTokenAcceptance()
        XCTAssertTrue(acceptance.accept(RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 4)))
        XCTAssertTrue(acceptance.accept(RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 5)))
        XCTAssertFalse(acceptance.accept(RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 3)))

        XCTAssertTrue(acceptance.accept(RoutingToken(
            routerBootID: "boot-b",
            activeGeneration: 1)))
        XCTAssertFalse(acceptance.accept(RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 99)))
        XCTAssertEqual(acceptance.current, RoutingToken(
            routerBootID: "boot-b",
            activeGeneration: 1))
    }

    func testMissingRoutingTokenIsRejected() {
        var acceptance = RoutingTokenAcceptance()
        let missing = RoutingToken(
            routerBootID: "",
            activeGeneration: 0)
        XCTAssertFalse(missing.isAuthoritative)
        XCTAssertFalse(acceptance.accept(missing))
        XCTAssertNil(acceptance.current)
    }

    func testPollEventAcceptanceRejectsLateOlderRequestWithSameToken() {
        var acceptance = PollEventAcceptance()
        let token = RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 4)

        XCTAssertTrue(acceptance.accept(requestID: 2, token: token))
        XCTAssertFalse(acceptance.accept(requestID: 1, token: token))
        XCTAssertEqual(acceptance.latestRequestID, 2)
    }

    func testRejectedNewerTokenStillBlocksOlderRequest() {
        var acceptance = PollEventAcceptance()
        let current = RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 4)
        let stale = RoutingToken(
            routerBootID: "boot-a",
            activeGeneration: 3)

        XCTAssertTrue(acceptance.accept(requestID: 0, token: current))
        XCTAssertFalse(acceptance.accept(requestID: 2, token: stale))
        XCTAssertEqual(acceptance.latestRequestID, 2)
        XCTAssertFalse(acceptance.accept(requestID: 1, token: current))
    }

    func testPollCoordinatorCoalescesConcurrentRefreshes() async {
        let reader = CountingAdminReader(
            status: currentStatus(),
            delayNanoseconds: 100_000_000)
        let coordinator = PollCoordinator(
            reader: reader,
            clock: FixedClock(now: Date(timeIntervalSince1970: 100)),
            interval: 5)

        async let first = coordinator.refresh()
        async let second = coordinator.refresh()
        let events = await [first, second]

        let statusCalls = await reader.statusCalls
        XCTAssertEqual(statusCalls, 1)
        XCTAssertEqual(events.count, 2)
        for event in events {
            guard case .snapshot(let snapshot) = event else {
                return XCTFail("expected a routing snapshot")
            }
            XCTAssertEqual(snapshot.token.routerBootID, "boot-a")
            XCTAssertEqual(snapshot.observedAt.timeIntervalSince1970, 100)
        }
    }

    func testGlobalMutationReceiptDecodesTypedResult() {
        let receipt = GlobalMutationReceipt(json: """
        {
          "ok": true,
          "operation_id": "op-1",
          "operation": "set_claude_route",
          "client": "claude-code",
          "key": "opus",
          "requested_target": "native",
          "requested": false,
          "desired_config_hash": "sha256:new",
          "active_token": "boot-a:5",
          "active_config_hash": "sha256:new",
          "applied": true,
          "reconciliation_required": true,
          "blocking_operation_id": "op-older",
          "outcome": "applied",
          "cleanup_pending": true,
          "request_fingerprint": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
          "identity_strength": "exact",
          "error": null
        }
        """)

        XCTAssertEqual(receipt?.ok, true)
        XCTAssertEqual(receipt?.operationID, "op-1")
        XCTAssertEqual(receipt?.operation, "set_claude_route")
        XCTAssertEqual(receipt?.client, "claude-code")
        XCTAssertEqual(receipt?.key, "opus")
        XCTAssertEqual(receipt?.requestedTarget, "native")
        XCTAssertEqual(receipt?.requested, false)
        XCTAssertEqual(receipt?.activeToken, "boot-a:5")
        XCTAssertEqual(receipt?.activeConfigHash, "sha256:new")
        XCTAssertEqual(receipt?.applied, true)
        XCTAssertEqual(receipt?.reconciliationRequired, true)
        XCTAssertEqual(receipt?.blockingOperationID, "op-older")
        XCTAssertEqual(receipt?.outcome, "applied")
        XCTAssertEqual(receipt?.cleanupPending, true)
        XCTAssertEqual(
            receipt?.requestFingerprint,
            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
        XCTAssertEqual(receipt?.identityStrength, "exact")
        XCTAssertTrue(isValidMutationRequestFingerprint(
            receipt?.requestFingerprint ?? ""))
        XCTAssertFalse(isValidMutationRequestFingerprint(
            "sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
    }

    @MainActor
    func testStartupRecoveryProbesOnceThenRunsCleanupOnly() async {
        let runner = RecoveryScriptRunner(
            statusResult: recoveryStatusResult(
                classification: "none"),
            recoverResults: [recoveryResult(
                ok: true,
                classification: "none")])
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            clock: RecordingClock(),
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        XCTAssertFalse(state.canMutateRouting)
        await state.refresh()
        await state.waitForMutationRecovery()

        XCTAssertEqual(state.mutationRecoveryState, .ready)
        XCTAssertTrue(state.canMutateRouting)
        let initialCalls = await runner.arguments
        XCTAssertEqual(initialCalls, [
            ["--json", "mutation", "status"],
            ["--json", "mutation", "recover"],
        ])

        await state.refresh()
        let repeatedCalls = await runner.arguments
        XCTAssertEqual(repeatedCalls.count, 2)
        state.stop()
    }

    @MainActor
    func testStartupRecoveryUsesBoundedBackoffAndManualRetry() async {
        let transient = recoveryResult(
            ok: false,
            errorCode: "mutation_locked",
            status: 1)
        let clock = RecordingClock()
        let runner = RecoveryScriptRunner(
            statusResult: recoveryStatusResult(
                classification: "desired_active"),
            recoverResults: Array(repeating: transient, count: 6) + [
                recoveryResult(ok: true, classification: "none"),
            ])
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            clock: clock,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()

        XCTAssertEqual(
            state.mutationRecoveryState,
            .blocked(errorCode: "mutation_locked"))
        XCTAssertEqual(clock.sleeps, [1, 2, 4, 8, 16])
        XCTAssertTrue(state.canRetryMutationCleanup)
        let automaticCalls = await runner.arguments
        XCTAssertFalse(automaticCalls.joined().contains("reconcile"))

        state.retryMutationCleanup()
        await state.waitForMutationRecovery()
        XCTAssertEqual(state.mutationRecoveryState, .ready)
        XCTAssertEqual(clock.sleeps, [1, 2, 4, 8, 16])
        state.stop()
    }

    @MainActor
    func testCleanupPendingSuccessRemainsGatedAndRetries() async {
        let pending = recoveryResult(
            ok: true,
            classification: "cleanup_pending",
            cleanupPending: true)
        let clock = RecordingClock()
        let runner = RecoveryScriptRunner(
            statusResult: recoveryStatusResult(
                classification: "cleanup_pending"),
            recoverResults: Array(repeating: pending, count: 6))
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            clock: clock,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()

        XCTAssertEqual(
            state.mutationRecoveryState,
            .blocked(errorCode: "cleanup_pending"))
        XCTAssertFalse(state.canMutateRouting)
        XCTAssertTrue(state.canRetryMutationCleanup)
        XCTAssertEqual(clock.sleeps, [1, 2, 4, 8, 16])
        state.stop()
    }

    @MainActor
    func testRetryableCleanupPredicateChangeRetriesThenRecovers() async {
        let clock = RecordingClock()
        let runner = RecoveryScriptRunner(
            statusResult: recoveryStatusResult(
                classification: "desired_active"),
            recoverResults: [
                recoveryResult(
                    ok: false,
                    errorCode: "cleanup_predicate_changed",
                    status: 1,
                    errorRetryable: true),
                recoveryResult(ok: true, classification: "none"),
            ])
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            clock: clock,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()

        XCTAssertEqual(state.mutationRecoveryState, .ready)
        XCTAssertEqual(clock.sleeps, [1])
        state.stop()
    }

    @MainActor
    func testTimedOutCleanupProcessRetriesThenRecovers() async {
        let clock = RecordingClock()
        let runner = RecoveryScriptRunner(
            statusResult: recoveryStatusResult(
                classification: "desired_active"),
            recoverResults: [
                recoveryResult(
                    ok: false,
                    status: -1,
                    timedOut: true),
                recoveryResult(ok: true, classification: "none"),
            ])
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            clock: clock,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()

        XCTAssertEqual(state.mutationRecoveryState, .ready)
        XCTAssertEqual(clock.sleeps, [1])
        state.stop()
    }

    @MainActor
    func testTimedOutStatusProbeStillRunsCleanupRecovery() async {
        let runner = RecoveryScriptRunner(
            statusResult: recoveryResult(
                ok: false,
                status: -1,
                timedOut: true),
            recoverResults: [
                recoveryResult(ok: true, classification: "none"),
            ])
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            clock: RecordingClock(),
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()

        XCTAssertEqual(state.mutationRecoveryState, .ready)
        let calls = await runner.arguments
        XCTAssertEqual(calls, [
            ["--json", "mutation", "status"],
            ["--json", "mutation", "recover"],
        ])
        state.stop()
    }

    @MainActor
    func testUnsupportedStatusFallsBackWithoutRepeatedProbe() async {
        let runner = RecoveryScriptRunner(
            statusResult: recoveryResult(
                ok: false,
                errorCode: "usage",
                status: 2),
            recoverResults: [])
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: SequencedAdminReader(statuses: [
                previewStatus(
                    generation: 4,
                    hash: "sha256:active",
                    familyTarget: "native"),
                previewStatus(
                    generation: 4,
                    hash: "sha256:active",
                    familyTarget: "native"),
                previewStatus(
                    generation: 1,
                    hash: "sha256:active",
                    familyTarget: "native",
                    bootID: "boot-b"),
            ]),
            cliRunner: runner,
            clock: RecordingClock(),
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()
        XCTAssertEqual(state.mutationRecoveryState, .legacyFallback)
        XCTAssertTrue(state.canMutateRouting)
        let initialCalls = await runner.arguments
        XCTAssertEqual(initialCalls.count, 1)

        await state.refresh()
        let repeatedCalls = await runner.arguments
        XCTAssertEqual(repeatedCalls.count, 1)
        state.stop()
    }

    @MainActor
    func testInPlaceCLIUpgradeInvalidatesLegacyFallback() async throws {
        let root = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let binary = root.appendingPathComponent("baseten-switch")
        try FileManager.default.createDirectory(
            at: root,
            withIntermediateDirectories: true)
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: binary)
        try FileManager.default.setAttributes(
            [.posixPermissions: NSNumber(value: 0o755)],
            ofItemAtPath: binary.path)
        addTeardownBlock {
            try? FileManager.default.removeItem(at: root)
        }

        let runner = RecoveryScriptRunner(
            statusResult: recoveryResult(
                ok: false,
                errorCode: "usage",
                status: 2),
            recoverResults: [])
        let state = BasetenSwitchState(
            variant: previewVariant(binaryPath: binary.path),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await state.waitForMutationRecovery()
        let initialCalls = await runner.arguments
        XCTAssertEqual(initialCalls.count, 1)

        try Data("#!/bin/sh\nexit 0\n# upgraded\n".utf8)
            .write(to: binary, options: .atomic)
        try FileManager.default.setAttributes(
            [
                .posixPermissions: NSNumber(value: 0o755),
                .modificationDate: Date(timeIntervalSinceNow: 5),
            ],
            ofItemAtPath: binary.path)

        await state.refresh()
        await state.waitForMutationRecovery()
        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(calls.last, ["--json", "mutation", "status"])
        state.stop()
    }

    @MainActor
    func testReasoningControlsStayDisabledDuringStartupRecovery() async {
        let runner = BlockingFirstRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false,
            automaticMutationRecoveryEnabled: true)

        await state.refresh()
        await runner.waitUntilFirstStarted()
        XCTAssertFalse(state.canMutateReasoning)

        state.stop()
        await runner.releaseFirst()
    }

    @MainActor
    func testBlockingOperationPreservesPrimaryAndNeverReconcilesNewID() async {
        let runner = PrimaryFailureRunner(mode: .blocker)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(
            state.lastError,
            "A previous routing change still needs cleanup.")
        XCTAssertFalse(state.lastError?.contains("/Users/") == true)
        XCTAssertFalse(state.lastError?.contains("model-id") == true)
        state.stop()
    }

    @MainActor
    func testRouterMismatchFailuresUseReviewedActionableMessage() async {
        for mode in [
            PrimaryFailureRunner.Mode.routerStateMismatch,
            .routerIdentityMismatch,
        ] {
            let runner = PrimaryFailureRunner(mode: mode)
            let state = mutationTestState(runner: runner)
            await state.refresh()

            await state.setAllRoutesThroughBaseten(true)

            XCTAssertEqual(
                state.lastError,
                "The app and routing command are connected to different local gateways. Restart Baseten Switch and try again.")
            XCTAssertFalse(state.lastError?.contains("/Users/") == true)
            XCTAssertFalse(state.lastError?.contains("model-id") == true)
            state.stop()
        }
    }

    @MainActor
    func testFailedSecondaryReconciliationCannotMaskPrimaryFailure() async {
        let runner = PrimaryFailureRunner(mode: .failedReconciliation)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(Array(calls[1].prefix(3)), [
            "--json", "mutation", "reconcile",
        ])
        XCTAssertEqual(
            state.lastError,
            "The routing change could not be confirmed safely. Retry cleanup.")
        XCTAssertFalse(state.lastError?.contains("/Users/") == true)
        state.stop()
    }

    @MainActor
    func testDoubleTimeoutRetainsRecoveryGate() async {
        let runner = PrimaryFailureRunner(mode: .doubleTimeout)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(
            state.mutationRecoveryState,
            .blocked(errorCode: "reconciliation_required"))
        XCTAssertFalse(state.canMutateRouting)
        state.stop()
    }

    @MainActor
    func testUnusableSecondaryReceiptRetainsRecoveryGate() async {
        for mode in [
            PrimaryFailureRunner.Mode.malformedSecondary,
            .mismatchedSecondary,
        ] {
            let runner = PrimaryFailureRunner(mode: mode)
            let state = mutationTestState(runner: runner)
            await state.refresh()

            await state.setAllRoutesThroughBaseten(true)

            let calls = await runner.arguments
            XCTAssertEqual(calls.count, 2)
            XCTAssertEqual(
                state.mutationRecoveryState,
                .blocked(errorCode: "reconciliation_required"))
            XCTAssertFalse(state.canMutateRouting)
            state.stop()
        }
    }

    @MainActor
    func testMatchingTypedPrimaryErrorOutranksProcessTimeout() async {
        let runner = PrimaryFailureRunner(mode: .timedTypedError)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(
            state.lastError,
            "Routing settings changed before this request completed. Refresh and try again.")
        XCTAssertFalse(state.lastError?.contains("timed out") == true)
        state.stop()
    }

    @MainActor
    func testTimedOutSuccessReceiptStillReconciles() async {
        let runner = PrimaryFailureRunner(mode: .timedSuccess)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(
            Array(calls[1].prefix(3)),
            ["--json", "mutation", "reconcile"])
        XCTAssertFalse(state.lastError?.contains("timed out") == true)
        state.stop()
    }

    @MainActor
    func testRejectedReceiptWithCleanupPendingRetainsRecoveryGate() async {
        let runner = CleanupPendingMutationRunner(mode: .rejected)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 1)
        XCTAssertEqual(
            state.mutationRecoveryState,
            .blocked(errorCode: "cleanup_pending"))
        XCTAssertFalse(state.canMutateRouting)
        state.stop()
    }

    @MainActor
    func testPriorActiveReconcileWithCleanupPendingIsNotResolved() async {
        let runner = CleanupPendingMutationRunner(mode: .priorActive)
        let state = mutationTestState(runner: runner)
        await state.refresh()

        await state.setAllRoutesThroughBaseten(true)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertTrue(calls[1].contains("reconcile"))
        XCTAssertEqual(
            state.mutationRecoveryState,
            .blocked(errorCode: "cleanup_pending"))
        XCTAssertFalse(state.canMutateRouting)
        state.stop()
    }

    func testMutationCoordinatorSerializesChildProcesses() async {
        let runner = ConcurrencyRecordingRunner()
        let coordinator = MutationCoordinator(runner: runner)
        let request = CLIExecutionRequest(
            binary: URL(fileURLWithPath: "/usr/bin/true"),
            arguments: ["on"],
            environment: [:],
            timeout: 1)

        async let first = coordinator.perform(request)
        async let second = coordinator.perform(request)
        _ = await [first, second]

        let maximumActive = await runner.maximumActive
        let arguments = await runner.arguments
        XCTAssertEqual(maximumActive, 1)
        XCTAssertEqual(arguments, [["on"], ["on"]])
    }

    func testMutationCoordinatorDropsCanceledQueuedRequest() async {
        let runner = BlockingFirstRunner()
        let coordinator = MutationCoordinator(runner: runner)
        let firstRequest = CLIExecutionRequest(
            binary: URL(fileURLWithPath: "/usr/bin/true"),
            arguments: ["first"],
            environment: [:],
            timeout: 1)
        let secondRequest = CLIExecutionRequest(
            binary: URL(fileURLWithPath: "/usr/bin/true"),
            arguments: ["stale"],
            environment: [:],
            timeout: 1)

        let first = Task { await coordinator.perform(firstRequest) }
        await runner.waitUntilFirstStarted()
        let stale = Task { await coordinator.perform(secondRequest) }
        try? await Task.sleep(nanoseconds: 20_000_000)
        stale.cancel()
        let staleResult = await stale.value
        await runner.releaseFirst()
        _ = await first.value

        XCTAssertEqual(staleResult.status, -1)
        let calls = await runner.arguments
        XCTAssertEqual(calls, [["first"]])
    }

    func testSuccessfulCLIWithUnchangedFamilyStateIsNotConfirmed() {
        let success = CLIExecutionResult(
            status: 0,
            standardOutput: "",
            standardError: "",
            timedOut: false)
        let unchanged = routingSnapshot(client: [
            "name": "claude-code",
            "families": [[
                "family": "opus",
                "configured_target": "zai-org/GLM-5.2",
                "configured_source": "explicit",
            ]],
        ])
        XCTAssertFalse(familyMutationConfirmed(
            cliResult: success,
            snapshot: unchanged,
            clientName: "claude-code",
            familyName: "opus",
            choice: .native))

        let confirmed = routingSnapshot(client: [
            "name": "claude-code",
            "families": [[
                "family": "opus",
                "configured_target": "native",
                "configured_source": "explicit",
            ]],
        ])
        XCTAssertTrue(familyMutationConfirmed(
            cliResult: success,
            snapshot: confirmed,
            clientName: "claude-code",
            familyName: "opus",
            choice: .native))
    }

    func testSuccessfulCLIWithUnchangedSubagentStateIsNotConfirmed() {
        let success = CLIExecutionResult(
            status: 0,
            standardOutput: "",
            standardError: "",
            timedOut: false)
        let model = ModelCatalogEntry(dict: [
            "label": "GLM-5.2",
            "storage_target": "zai-org/GLM-5.2",
            "available": true,
        ])!
        let unchanged = routingSnapshot(client: [
            "name": "claude-code",
            "subagent_model": "",
            "subagent_routing": "off",
        ])
        XCTAssertFalse(subagentMutationConfirmed(
            cliResult: success,
            snapshot: unchanged,
            clientName: "claude-code",
            choice: .catalog(model)))

        let confirmed = routingSnapshot(client: [
            "name": "claude-code",
            "subagent_model": "zai-org/GLM-5.2",
            "subagent_routing": "on",
        ])
        XCTAssertTrue(subagentMutationConfirmed(
            cliResult: success,
            snapshot: confirmed,
            clientName: "claude-code",
            choice: .catalog(model)))
    }

    func testCLIEnvironmentUsesAllowlistAndExplicitOverrides() {
        let environment = allowlistedCLIEnvironment(
            ambient: [
                "HOME": "/Users/test",
                "PATH": "/attacker/bin:/usr/bin",
                "BASETEN_API_KEY": "secret",
                "ANTHROPIC_AUTH_TOKEN": "secret",
                "SSH_AUTH_SOCK": "/tmp/attacker-agent.sock",
                "UNRELATED": "not-required",
            ],
            overrides: [
                "BASETEN_SWITCH_CONFIG_PATH": "/tmp/preview/gateway.yaml",
                "BASETEN_SWITCH_GATEWAY_TOKEN": "preview-token",
            ])

        XCTAssertEqual(environment["HOME"], "/Users/test")
        XCTAssertEqual(
            environment["PATH"],
            "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
        XCTAssertFalse(environment["PATH"]?.contains("/attacker") ?? true)
        XCTAssertEqual(
            environment["BASETEN_SWITCH_CONFIG_PATH"],
            "/tmp/preview/gateway.yaml")
        XCTAssertEqual(environment["BASETEN_SWITCH_GATEWAY_TOKEN"], "preview-token")
        XCTAssertNil(environment["BASETEN_API_KEY"])
        XCTAssertNil(environment["ANTHROPIC_AUTH_TOKEN"])
        XCTAssertNil(environment["UNRELATED"])
        XCTAssertNil(environment["SSH_AUTH_SOCK"])
    }

    func testPreviewRuntimeFilesystemAcceptsPrivateRegularTree() throws {
        let variant = try isolatedPreviewVariant()
        XCTAssertNil(previewRuntimeFilesystemError(runtime: variant.runtime))
    }

    func testPreviewRuntimeFilesystemRejectsAuthSymlinkEscape() throws {
        let variant = try isolatedPreviewVariant()
        let manager = FileManager.default
        let authPath = variant.runtime.environment["BASETEN_SWITCH_AUTH_FILE"]!
        try manager.removeItem(atPath: authPath)
        let stable = URL(fileURLWithPath: authPath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("stable-auth.json")
        XCTAssertTrue(manager.createFile(
            atPath: stable.path,
            contents: Data("stable-secret".utf8),
            attributes: [.posixPermissions: NSNumber(value: 0o600)]))
        try manager.createSymbolicLink(atPath: authPath, withDestinationPath: stable.path)

        let error = previewRuntimeFilesystemError(runtime: variant.runtime)
        XCTAssertTrue(error?.contains("symlink") == true, error ?? "missing error")
    }

    func testPreviewRuntimeFilesystemRejectsPIDFileSymlinkEscape() throws {
        let variant = try isolatedPreviewVariant()
        let manager = FileManager.default
        let pidPath = variant.runtime.environment["BASETEN_SWITCH_GATEWAY_PIDFILE"]!
        let stable = URL(fileURLWithPath: pidPath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("stable.pid")
        XCTAssertTrue(manager.createFile(
            atPath: stable.path,
            contents: Data("1234\n".utf8),
            attributes: [.posixPermissions: NSNumber(value: 0o600)]))
        try manager.createSymbolicLink(atPath: pidPath, withDestinationPath: stable.path)

        let error = previewRuntimeFilesystemError(runtime: variant.runtime)
        XCTAssertTrue(error?.contains("symlink") == true, error ?? "missing error")
    }

    func testPreviewRuntimeFilesystemRejectsUnsafeEnvPermissions() throws {
        let variant = try isolatedPreviewVariant()
        let envPath = variant.runtime.environment["BASETEN_SWITCH_ENV_FILE"]!
        try FileManager.default.setAttributes(
            [.posixPermissions: NSNumber(value: 0o644)],
            ofItemAtPath: envPath)

        let error = previewRuntimeFilesystemError(runtime: variant.runtime)
        XCTAssertTrue(
            error?.contains("permissions are unsafe") == true,
            error ?? "missing error")
    }

    func testDiagnosticRedactionRemovesCredentialValues() {
        let redacted = redactDiagnosticText("""
        BASETEN_API_KEY=sk-sensitive
        Authorization: Bearer test-authorization-secret
        "OPENAI_API_KEY": "sk-json-secret"
        command --auth-token token-flag-secret
        harmless context
        """)

        XCTAssertFalse(redacted.contains("sk-sensitive"))
        XCTAssertFalse(redacted.contains("test-authorization-secret"))
        XCTAssertFalse(redacted.contains("sk-json-secret"))
        XCTAssertFalse(redacted.contains("token-flag-secret"))
        XCTAssertTrue(redacted.contains("BASETEN_API_KEY=<redacted>"))
        XCTAssertTrue(redacted.contains("Authorization: <redacted>"))
        XCTAssertTrue(redacted.contains("--auth-token <redacted>"))
        XCTAssertTrue(redacted.contains("harmless context"))
    }

    @MainActor
    func testInteractiveRefreshCoalescesStatsAndVersionProcesses() async {
        let reader = CountingAdminReader(
            status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "zai-org/GLM-5.2"),
            delayNanoseconds: 30_000_000)
        let runner = ConcurrencyRecordingRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)

        for _ in 0..<25 {
            state.menuDidShow()
            state.requestInteractiveRefresh(includeStats: false)
        }
        await state.waitForInteractiveRefresh()

        let statusCalls = await reader.statusCalls
        let statsCalls = await reader.statsCalls
        let maximumActive = await runner.maximumActive
        let arguments = await runner.arguments
        XCTAssertEqual(statusCalls, 1)
        XCTAssertEqual(statsCalls, 1)
        XCTAssertEqual(maximumActive, 1)
        XCTAssertEqual(arguments, [["--version"]])
        state.menuDidHide()
        state.stop()
    }

    @MainActor
    func testTrackedStatusMenuRefreshProjectsAndRebuildsAuthAttention() async {
        let reader = SequencedAdminReader(statuses: [
            menuAuthStatus(generation: 4, signedIn: true, health: "ok"),
            menuAuthStatus(
                generation: 5,
                signedIn: false,
                health: "signed_out"),
            menuAuthStatus(generation: 6, signedIn: true, health: "ok"),
        ])
        let runner = ConcurrencyRecordingRunner()
        let variant = stableTestVariant()
        let state = BasetenSwitchState(
            variant: variant,
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)

        await state.refresh()
        let controller = StatusItemController(state: state, variant: variant)
        XCTAssertEqual(
            controller.fixedHeaderSubtitleForTesting,
            "Routing rules are active")
        XCTAssertEqual(controller.displayedIconStateForTesting, .active)
        let healthyMenuTitles = controller.menuItemTitlesForTesting
        XCTAssertFalse(healthyMenuTitles.contains("Sign In to Baseten…"))

        // Opening the menu schedules one asynchronous status pass. A later
        // signed-out snapshot must immediately project warning copy into the
        // fixed header and amber icon. AppKit can add the action row on the
        // next structural rebuild without mutating the tracked menu.
        controller.menuWillOpenForTesting()
        await state.waitForInteractiveRefresh()
        await drainMainQueue()

        XCTAssertEqual(
            controller.fixedHeaderSubtitleForTesting,
            "Sign in to use Baseten")
        XCTAssertEqual(controller.displayedIconStateForTesting, .degraded)
        XCTAssertEqual(
            controller.menuItemTitlesForTesting,
            healthyMenuTitles)
        XCTAssertFalse(
            controller.menuItemTitlesForTesting.contains("Sign In to Baseten…"))

        controller.menuDidCloseForTesting()
        controller.menuNeedsUpdateForTesting()
        XCTAssertEqual(
            controller.fixedHeaderSubtitleForTesting,
            "Sign in to use Baseten")
        XCTAssertTrue(
            controller.menuItemTitlesForTesting.contains("Sign In to Baseten…"))

        // A later healthy snapshot clears all three projections after the
        // closed menu is rebuilt.
        await state.refresh()
        await drainMainQueue()
        controller.menuNeedsUpdateForTesting()
        XCTAssertEqual(
            controller.fixedHeaderSubtitleForTesting,
            "Routing rules are active")
        XCTAssertEqual(controller.displayedIconStateForTesting, .active)
        XCTAssertFalse(
            controller.menuItemTitlesForTesting.contains("Sign In to Baseten…"))

        let statusCalls = await reader.statusCalls
        let statsCalls = await reader.statsCalls
        let versionCalls = await runner.arguments
        XCTAssertEqual(statusCalls, 3)
        XCTAssertEqual(statsCalls, 1)
        XCTAssertEqual(versionCalls, [["--version"]])
        state.stop()
    }

    @MainActor
    func testFamilyMutationUsesCASAndReconcilesBeforeClearingPending() async {
        let reader = SequencedAdminReader(statuses: [
            previewStatus(
                generation: 4,
                hash: "sha256:old",
                familyTarget: "zai-org/GLM-5.2"),
            previewStatus(
                generation: 5,
                hash: "sha256:new",
                familyTarget: "native"),
        ])
        let runner = ReconciliationReceiptRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)

        await state.routeFamily(client, family: "opus", choice: .native)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(
            Array(calls[0].prefix(1)),
            ["--json"])
        XCTAssertTrue(calls[0].contains("--operation-id"))
        XCTAssertEqual(
            argumentValue("--if-active-token", in: calls[0]),
            "boot-a:4")
        XCTAssertEqual(
            argumentValue("--if-config-hash", in: calls[0]),
            "sha256:old")
        XCTAssertEqual(
            Array(calls[0].suffix(4)),
            ["claude", "route", "opus", "native"])
        let operationID = try! XCTUnwrap(
            argumentValue("--operation-id", in: calls[0]))
        XCTAssertEqual(
            calls[1],
            ["--json", "mutation", "reconcile", operationID])
        XCTAssertFalse(
            state.isFamilyMutationPending(
                client: "claude-code",
                family: "opus"))
        XCTAssertNil(state.lastError)
        state.stop()
    }

    @MainActor
    func testFamilyMutationWaitsOutPreMutationStatusRequest() async {
        let oldStatus = previewStatus(
            generation: 4,
            hash: "sha256:old",
            familyTarget: "zai-org/GLM-5.2")
        let confirmedStatus = previewStatus(
            generation: 5,
            hash: "sha256:new",
            familyTarget: "native")
        let reader = SuspendingMutationAdminReader(
            initial: oldStatus,
            stale: oldStatus,
            confirmed: confirmedStatus)
        let runner = ReconciliationReceiptRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)

        let stalePoll = Task { @MainActor in
            await state.refresh()
        }
        await reader.waitForStaleRequest()
        let mutation = Task { @MainActor in
            await state.routeFamily(client, family: "opus", choice: .native)
        }
        while (await runner.arguments).count < 2 {
            await Task.yield()
        }
        await reader.releaseStaleRequest()
        await stalePoll.value
        await mutation.value

        let statusCalls = await reader.statusCalls
        XCTAssertEqual(statusCalls, 3)
        XCTAssertNil(state.lastError)
        XCTAssertEqual(
            state.clients.first?.families.first(where: {
                $0.family == "opus"
            })?.configuredTarget,
            "native")
        state.stop()
    }

    @MainActor
    func testTargetlessReplayUsesSelectedFallbackAdapterClient() async {
        let reader = SequencedAdminReader(statuses: [
            previewStatus(
                generation: 4,
                hash: "sha256:old",
                familyTarget: "zai-org/GLM-5.2",
                clientName: "anthropic"),
            previewStatus(
                generation: 5,
                hash: "sha256:new",
                familyTarget: "native",
                clientName: "anthropic"),
        ])
        let runner = ReconciliationReceiptRunner(
            clientOverride: "anthropic")
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)

        await state.routeFamily(client, family: "opus", choice: .native)

        XCTAssertNil(state.lastError)
        state.stop()
    }

    @MainActor
    func testTargetlessTerminalReplayRequiresMatchingExactFingerprint() async {
        let reader = SequencedAdminReader(statuses: [
            previewStatus(
                generation: 4,
                hash: "sha256:old",
                familyTarget: "zai-org/GLM-5.2"),
            previewStatus(
                generation: 5,
                hash: "sha256:new",
                familyTarget: "native"),
        ])
        let runner = ReconciliationReceiptRunner(
            reconciledFingerprint:
                "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)

        await state.routeFamily(client, family: "opus", choice: .native)

        XCTAssertEqual(
            state.lastError,
            "Opus mapping was not present in the active router state.")
        state.stop()
    }

    @MainActor
    func testTerminalFingerprintMustBindTheDispatchedRequest() async {
        let reader = SequencedAdminReader(statuses: [
            previewStatus(
                generation: 4,
                hash: "sha256:old",
                familyTarget: "zai-org/GLM-5.2"),
            previewStatus(
                generation: 5,
                hash: "sha256:new",
                familyTarget: "native"),
        ])
        let runner = ReconciliationReceiptRunner(
            primaryTargetOverride: "different-target")
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)

        await state.routeFamily(client, family: "opus", choice: .native)

        XCTAssertEqual(
            state.lastError,
            "Opus mapping was not present in the active router state.")
        state.stop()
    }

    @MainActor
    func testSubagentMutationUsesCASAndReconcilesBeforeClearingPending() async {
        let reader = SequencedAdminReader(statuses: [
            previewStatus(
                generation: 4,
                hash: "sha256:old",
                familyTarget: "zai-org/GLM-5.2",
                subagentModel: "zai-org/GLM-5.2",
                subagentRouting: "on"),
            previewStatus(
                generation: 5,
                hash: "sha256:new",
                familyTarget: "zai-org/GLM-5.2",
                subagentRouting: "off"),
        ])
        let runner = ReconciliationReceiptRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)

        await state.setSubagents(client, choice: .off)

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(Array(calls[0].prefix(1)), ["--json"])
        XCTAssertTrue(calls[0].contains("--operation-id"))
        XCTAssertEqual(
            argumentValue("--if-active-token", in: calls[0]),
            "boot-a:4")
        XCTAssertEqual(
            argumentValue("--if-config-hash", in: calls[0]),
            "sha256:old")
        XCTAssertEqual(
            Array(calls[0].suffix(3)),
            ["claude", "subagents", "inherit"])
        let operationID = try! XCTUnwrap(
            argumentValue("--operation-id", in: calls[0]))
        XCTAssertEqual(
            calls[1],
            ["--json", "mutation", "reconcile", operationID])
        XCTAssertFalse(
            state.isSubagentMutationPending(client: "claude-code"))
        XCTAssertNil(state.lastError)
        state.stop()
    }

    @MainActor
    func testMalformedPreviewIdentityNeverRegistersStableLoginItem() {
        let malformedPreview = AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch",
                "CFBundleExecutable": "BasetenSwitch",
            ],
            bundleIdentifier: "co.baseten.switch",
            runningExecutableName: "BasetenSwitch",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true"])
        let loginItem = FakeLoginItemService()

        let state = BasetenSwitchState(
            variant: malformedPreview,
            reader: CountingAdminReader(
                status: previewStatus(
                    generation: 4,
                    hash: "sha256:active",
                    familyTarget: "zai-org/GLM-5.2")),
            cliRunner: ConcurrencyRecordingRunner(),
            loginItemService: loginItem,
            startPolling: false)

        XCTAssertEqual(malformedPreview.channel, .preview)
        XCTAssertNotNil(malformedPreview.identityError)
        XCTAssertFalse(malformedPreview.allowsLoginItem)
        XCTAssertEqual(loginItem.reconcileCalls, 0)
        guard case .identityMismatch = state.runtimeTrust else {
            return XCTFail("malformed Preview must fail closed")
        }
        XCTAssertFalse(state.canMutateRouting)
        state.stop()
    }

    private func recoveryResult(
        ok: Bool,
        classification: String = "",
        errorCode: String = "",
        status: Int32 = 0,
        cleanupPending: Bool = false,
        errorRetryable: Bool? = nil,
        timedOut: Bool = false
    ) -> CLIExecutionResult {
        var object: [String: Any] = [
            "ok": ok,
            "classification": classification,
            "cleanup_pending": cleanupPending,
            "error": NSNull(),
        ]
        if !errorCode.isEmpty {
            object["error"] = [
                "code": errorCode,
                "message": "private backend detail must not be displayed",
                "retryable": errorRetryable
                    ?? (errorCode == "mutation_locked"
                        || errorCode == "router_unavailable"),
            ]
        }
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: status,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: timedOut)
    }

    private func recoveryStatusResult(
        classification: String,
        errorCode: String = "",
        status: Int32 = 0
    ) -> CLIExecutionResult {
        var object: [String: Any] = [
            "classification": classification,
        ]
        if !errorCode.isEmpty {
            object["error"] = [
                "code": errorCode,
                "message": "reviewed status text",
                "retryable": errorCode == "mutation_locked",
            ]
        }
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: status,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    @MainActor
    private func mutationTestState(
        runner: any CLIRunning
    ) -> BasetenSwitchState {
        BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(status: previewStatus(
                generation: 4,
                hash: "sha256:active",
                familyTarget: "native")),
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
    }

    private func argumentValue(_ flag: String,
                               in arguments: [String]) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }

    func testSystemRunnerMarksTimeoutBeforeTerminatingProcess() async {
        let started = Date()
        let result = await SystemCLIRunner().run(CLIExecutionRequest(
            binary: URL(fileURLWithPath: "/bin/sh"),
            arguments: ["-c", "sleep 2"],
            environment: ["PATH": "/usr/bin:/bin"],
            timeout: 0.1))

        XCTAssertTrue(result.timedOut)
        XCTAssertFalse(result.succeeded)
        XCTAssertLessThan(Date().timeIntervalSince(started), 1.5)
    }

    @MainActor
    func testStateUsesInjectedAdminReaderAndOneImmutableSnapshot() async {
        let reader = CountingAdminReader(
            status: currentStatus(enabled: false))
        let loginItems = FakeLoginItemService()
        let state = BasetenSwitchState(
            variant: AppVariant.resolve(
                infoDictionary: [:],
                homeDirectory: "/tmp/baseten-switch-home",
                environment: [:]),
            reader: reader,
            loginItemService: loginItems,
            startPolling: false)

        XCTAssertNil(state.routingSnapshot)
        XCTAssertEqual(loginItems.reconcileCalls, 1)
        await state.refresh()

        let statusCalls = await reader.statusCalls
        XCTAssertEqual(statusCalls, 1)
        XCTAssertEqual(state.routingSnapshot?.token.routerBootID, "boot-a")
        XCTAssertFalse(state.confirmedGlobalRoutingEnabled)
        XCTAssertTrue(state.gatewayUp)
        XCTAssertTrue(state.canMutateRouting)
        state.stop()
    }

    @MainActor
    func testDisplayedGlobalRoutingProjectsPendingIntentForEverySurface() async {
        let runner = ConcurrencyRecordingRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: CountingAdminReader(
                status: previewStatus(
                    generation: 4,
                    hash: "sha256:active",
                    familyTarget: "zai-org/GLM-5.2")),
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()

        XCTAssertFalse(state.confirmedGlobalRoutingEnabled)
        XCTAssertFalse(state.displayedGlobalRoutingEnabled)

        state.requestGlobalRouting(true)

        // The menu switch and the read-only window status both use this
        // projection. Pending user intent must win over the last confirmed
        // snapshot so neither surface snaps back during reconciliation.
        XCTAssertFalse(state.confirmedGlobalRoutingEnabled)
        XCTAssertTrue(state.displayedGlobalRoutingEnabled)
        XCTAssertEqual(state.pendingGlobalRouting?.requested, true)
        XCTAssertEqual(state.globalMutationPhase, .applying)
        XCTAssertEqual(
            state.routingMutationDisabledReason,
            "Waiting for the gateway to confirm the routing change to On.")

        for _ in 0..<100 where state.pendingGlobalRouting != nil {
            try? await Task.sleep(nanoseconds: 10_000_000)
        }
        XCTAssertNil(state.pendingGlobalRouting)
        XCTAssertFalse(state.displayedGlobalRoutingEnabled)
        state.stop()
    }

    func testWindowRoutingPresentationProjectsPendingStateAndPhase() {
        let applyingOn = windowGlobalRoutingPresentation(
            enabled: true,
            phase: .applying)
        XCTAssertEqual(applyingOn.status, "Applying · On")
        XCTAssertEqual(
            applyingOn.overviewTitle,
            "Applying routing On…")
        XCTAssertEqual(
            applyingOn.overviewDescription,
            "Applying the request to turn routing On. Waiting for gateway confirmation. Saved mappings remain editable.")
        XCTAssertEqual(
            applyingOn.clientDescription,
            applyingOn.overviewDescription)

        let reconcilingOff = windowGlobalRoutingPresentation(
            enabled: false,
            phase: .reconciling)
        XCTAssertEqual(reconcilingOff.status, "Reconciling · Off")
        XCTAssertEqual(
            reconcilingOff.overviewTitle,
            "Reconciling routing Off…")
        XCTAssertEqual(
            reconcilingOff.clientDescription,
            "Reconciling the request to turn routing Off. Waiting for gateway confirmation. Saved mappings remain editable.")
    }

    func testClientPagePresentationStrictlyIsolatesHarnessContent() {
        for routingEnabled in [true, false] {
            let claude = clientPagePresentation(
                clientName: "claude-code",
                clientEnabled: true,
                globalRoutingEnabled: routingEnabled,
                globalMutationPhase: nil)
            XCTAssertTrue(claude.showsModelRouting)
            XCTAssertTrue(claude.showsReasoning)
            XCTAssertTrue(claude.showsSubagents)
            XCTAssertNil(claude.activationCommand)
            XCTAssertTrue(claude.headerDescription.contains("Claude"))
            XCTAssertFalse(claude.headerDescription.contains("Codex"))
            XCTAssertFalse(claude.headerDescription.contains("OpenAI"))
        }

        for routingEnabled in [true, false] {
            let codex = clientPagePresentation(
                clientName: "codex",
                clientEnabled: true,
                globalRoutingEnabled: routingEnabled,
                globalMutationPhase: nil)
            XCTAssertTrue(codex.showsModelRouting)
            XCTAssertTrue(codex.showsReasoning)
            XCTAssertFalse(codex.showsSubagents)
            XCTAssertNil(codex.activationCommand)
            XCTAssertTrue(codex.headerDescription.contains("Codex"))
            XCTAssertFalse(codex.headerDescription.contains("Claude"))
            XCTAssertFalse(codex.headerDescription.contains("Anthropic"))
        }

        let codex = ClientStatus(dict: [
            "name": "codex",
            "enabled": true,
            "protocol_shape": "openai",
        ])!
        XCTAssertEqual(familyEntriesForDisplay(codex), [])
    }

    func testCodexPickerUsesUnmatchedDefaultMappingAndRawSlugDispatch() {
        let client = ClientStatus(dict: [
            "name": "codex",
            "enabled": true,
            "unmatched_native_model": [
                "configured_target": "zai-org/GLM-5.2",
            ],
        ])!
        let glm = ModelCatalogEntry(dict: [
            "label": "GLM 5.2",
            "storage_target": "zai-org/GLM-5.2",
            "slug": "zai-org/GLM-5.2",
        ])!
        let qwen = ModelCatalogEntry(dict: [
            "label": "Qwen 3 Coder",
            "storage_target": "Qwen/Qwen3-Coder",
            "slug": "Qwen/Qwen3-Coder",
        ])!
        let catalog = [glm, qwen]

        XCTAssertEqual(
            codexRoutePickerSelection(client, catalog: catalog),
            .catalog(glm.target))
        XCTAssertEqual(
            codexRoutePickerSelection(
                client,
                catalog: catalog,
                pendingTarget: qwen.slug),
            .catalog(qwen.target))
        XCTAssertEqual(
            codexRouteChoice(.catalog(qwen.target), catalog: catalog),
            qwen)
        XCTAssertEqual(
            codexRouteDispatchArgs(model: qwen),
            ["codex", "route", "Qwen/Qwen3-Coder"])
        XCTAssertTrue(codexRouteChoiceChecked(client: client, model: glm))
        XCTAssertFalse(codexRouteChoiceChecked(client: client, model: qwen))
        XCTAssertEqual(
            modelDisplayLabel(glm),
            "GLM 5.2")
        XCTAssertEqual(
            modelDisplayLabel(qwen),
            "Qwen 3 Coder")
    }

    func testCodexPickerGuidanceIsPersistentAndProviderCorrect() {
        let guidance = codexRoutePickerSummary()
        XCTAssertEqual(
            guidance,
            "Choose the Baseten model used for Codex requests.")
        XCTAssertTrue(guidance.contains("Codex"))
        XCTAssertTrue(guidance.contains("Baseten"))
        XCTAssertFalse(guidance.contains("Claude"))
        XCTAssertFalse(guidance.contains("Anthropic"))
        XCTAssertFalse(guidance.contains("OpenAI"))
    }

    func testCodexReasoningFollowsUnmatchedDefaultMappingOnly() {
        let client = ClientStatus(dict: [
            "name": "codex",
            "enabled": true,
            "unmatched_native_model": [
                "configured_target": "Qwen/Qwen3-Coder",
            ],
            "model_options": [
                "baseten": [
                    "zai-org/GLM-5.2": [
                        "reasoning": [
                            "available": true,
                            "configured": ["mode": "default"],
                            "effective": ["mode": "passthrough"],
                            "source": "default",
                            "available_modes": [],
                            "available_efforts": [],
                            "unavailable_reason": "",
                            "error": "",
                        ],
                    ],
                    "Qwen/Qwen3-Coder": [
                        "reasoning": [
                            "available": true,
                            "configured": ["mode": "default"],
                            "effective": ["mode": "passthrough"],
                            "source": "default",
                            "available_modes": [],
                            "available_efforts": [],
                            "unavailable_reason": "",
                            "error": "",
                        ],
                    ],
                ],
            ],
        ])!

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])
        XCTAssertEqual(rows.map(\.model), ["Qwen/Qwen3-Coder"])
    }

    @MainActor
    func testCodexMutationUsesCASShowsPendingAndConfirmsDefaultModel() async {
        let reader = SequencedAdminReader(statuses: [
            codexStatus(
                generation: 4,
                hash: "sha256:old",
                configuredTarget: "zai-org/GLM-5.2"),
            codexStatus(
                generation: 5,
                hash: "sha256:new",
                configuredTarget: "Qwen/Qwen3-Coder"),
        ])
        let runner = ReconciliationReceiptRunner()
        let state = BasetenSwitchState(
            variant: previewVariant(),
            reader: reader,
            cliRunner: runner,
            loginItemService: FakeLoginItemService(),
            previewRuntimeValidator: { _ in nil },
            startPolling: false)
        await state.refresh()
        let client = try! XCTUnwrap(state.clients.first)
        let model = try! XCTUnwrap(
            client.modelCatalog.first(where: {
                $0.slug == "Qwen/Qwen3-Coder"
            }))

        state.requestCodexRoute(client, model: model)
        XCTAssertTrue(state.isCodexRouteMutationPending())
        XCTAssertEqual(
            state.pendingCodexRouteTarget(),
            "Qwen/Qwen3-Coder")
        XCTAssertEqual(
            codexRoutePickerSelection(
                client,
                catalog: client.modelCatalog,
                pendingTarget: state.pendingCodexRouteTarget()),
            .catalog(model.target))

        for _ in 0..<100 where state.isCodexRouteMutationPending() {
            try? await Task.sleep(nanoseconds: 10_000_000)
        }

        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(
            argumentValue("--if-active-token", in: calls[0]),
            "boot-a:4")
        XCTAssertEqual(
            argumentValue("--if-config-hash", in: calls[0]),
            "sha256:old")
        XCTAssertEqual(
            Array(calls[0].suffix(3)),
            ["codex", "route", "Qwen/Qwen3-Coder"])
        let operationID = try! XCTUnwrap(
            argumentValue("--operation-id", in: calls[0]))
        XCTAssertEqual(
            calls[1],
            ["--json", "mutation", "reconcile", operationID])
        XCTAssertFalse(state.isCodexRouteMutationPending())
        XCTAssertEqual(
            state.clients.first?.unmatchedNativeModel?.configuredTarget,
            "Qwen/Qwen3-Coder")
        XCTAssertNil(state.lastError)
        state.stop()
    }

    func testDisabledClientPagesExposeOnlyExactActivationCommand() {
        for (name, command) in [
            ("claude-code", "baseten-switch claude on"),
            ("codex", "baseten-switch codex on"),
        ] {
            let presentation = clientPagePresentation(
                clientName: name,
                clientEnabled: false,
                globalRoutingEnabled: true,
                globalMutationPhase: nil)
            XCTAssertEqual(presentation.activationCommand, command)
            XCTAssertFalse(presentation.showsModelRouting)
            XCTAssertFalse(presentation.showsReasoning)
            XCTAssertFalse(presentation.showsSubagents)
        }
    }

    func testFamilyAndSubagentCopyUsePendingGlobalProjection() {
        let family = FamilyEntry(dict: [
            "family": "opus",
            "configured_target": "zai-org/GLM-5.2",
            "configured_source": "explicit",
            "effective_route": "anthropic",
            "effective_source": "global_off",
        ])!
        XCTAssertEqual(
            familyEffectiveStatus(
                family,
                globalRoutingEnabled: true,
                globalMutationPhase: .applying),
            "Applying routing On · waiting for gateway confirmation")

        let client = ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "subagent_model": "zai-org/GLM-5.2",
            "subagent_routing": "on",
            "subagent_effective": "zai-org/GLM-5.2",
        ])!
        XCTAssertEqual(
            subagentRoutingDescription(
                client: client,
                globalRoutingEnabled: false,
                globalMutationPhase: .reconciling),
            "Reconciling routing Off · waiting for gateway confirmation. Saved override GLM-5.2 will remain configured but inactive after confirmation.")
    }

    func testPendingRoutingDisabledReasonCoversBothPhases() {
        XCTAssertEqual(
            pendingGlobalRoutingDisabledReason(PendingGlobalRouting(
                operationID: "operation-a",
                requested: true,
                phase: .applying)),
            "Waiting for the gateway to confirm the routing change to On.")
        XCTAssertEqual(
            pendingGlobalRoutingDisabledReason(PendingGlobalRouting(
                operationID: "operation-b",
                requested: false,
                phase: .reconciling)),
            "The routing change to Off is taking longer than expected. Waiting for gateway confirmation.")
    }

    func testServerSummaryAndFourFamilyRowsArePresentationTruth() {
        let client = ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "effective_summary": "Custom routing",
            "unmatched_native_model": [
                "configured_target": "zai-org/GLM-5.2",
            ],
            "families": [[
                "family": "opus",
                "configured_target": "native",
                "configured_source": "explicit",
                "effective_route": "anthropic",
            ]],
            "model_catalog": [[
                "label": "GLM-5.2",
                "storage_target": "zai-org/GLM-5.2",
                "available": true,
            ]],
        ])!

        XCTAssertEqual(
            serverAuthoredClientMenuTitle(client),
            "Claude Code: Custom routing")
        let families = familyEntriesForDisplay(client)
        XCTAssertEqual(
            families.map(\.family),
            ["fable", "opus", "sonnet", "haiku"])
        XCTAssertEqual(
            familyConfiguredLabel(families[1]),
            "Saved: Native Provider (Anthropic)")
        XCTAssertEqual(
            familyEffectiveStatus(
                families[1],
                globalRoutingEnabled: false),
            "Currently using Anthropic while global routing is Off")
    }
}
