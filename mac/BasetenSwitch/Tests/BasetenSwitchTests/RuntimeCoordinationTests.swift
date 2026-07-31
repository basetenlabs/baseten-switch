import AppKit
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

    init(statuses: [AdminStatusSnapshot]) {
        self.statuses = statuses
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        guard !statuses.isEmpty else {
            throw GatewayClientError.invalidPayload
        }
        let status = statuses[min(index, statuses.count - 1)]
        index += 1
        return status
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }
}

private struct FixedClock: RuntimeClock {
    let now: Date

    func sleep(seconds: TimeInterval) async throws {
        try await Task.sleep(
            nanoseconds: UInt64(max(seconds, 0) * 1_000_000_000))
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

private actor ReconciliationReceiptRunner: CLIRunning {
    private(set) var arguments: [[String]] = []
    private var journalOperation = ""
    private var journalClient = ""
    private var journalKey = ""
    private var journalTarget = ""

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
            journalOperation = operation
            journalClient = client
            journalKey = key
            journalTarget = target
        }
        let ok = reconciling
        let object: [String: Any] = [
            "ok": ok,
            "operation_id": operationID,
            "operation": operation,
            "client": client,
            "key": key,
            "requested_target": target,
            "desired_config_hash": "sha256:new",
            "active_token": "boot-a:5",
            "active_config_hash": "sha256:new",
            "applied": ok,
            "reconciliation_required": !ok,
            "error": ok
                ? NSNull()
                : [
                    "code": "activation_indeterminate",
                    "message": "reconciliation required",
                ],
        ]
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

    private func previewVariant() -> AppVariant {
        AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "preview",
                "CFBundleDisplayName": "Baseten Switch Preview",
                "CFBundleExecutable": "BasetenSwitchPreview",
            ],
            bundleIdentifier: "co.baseten.switch.preview",
            runningExecutableName: "BasetenSwitchPreview",
            homeDirectory: "/tmp/baseten-switch-home",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true"])
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
        subagentRouting: String = "off"
    ) -> AdminStatusSnapshot {
        AdminStatusSnapshot(dict: [
            "router_boot_id": "boot-a",
            "active_generation": generation,
            "active_config_hash": hash,
            "desired_config_hash": hash,
                        "capabilities": ["global_routing"],
            "health": "ready",
            "config_path": previewConfigPath,
            "global_routing_enabled": false,
            "clients": [[
                "name": "claude-code",
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
