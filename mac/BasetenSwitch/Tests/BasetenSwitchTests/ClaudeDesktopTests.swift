import Foundation
import ServiceManagement
import XCTest
@testable import BasetenSwitch

private func desktopRow(_ leaf: String) -> [String: Any] {
    [
        "alias": "claude-baseten-\(leaf)",
        "slug": "example/\(leaf)",
        "label": leaf.capitalized,
        "description": "Served by Baseten.",
        "context_tokens": 128_000,
    ]
}

private func desktopStatusObject(
    configured: Bool = true,
    runtime: String = "ready",
    rows: [[String: Any]] = [desktopRow("alpha")],
    lastRequest: [String: Any]? = nil
) -> [String: Any] {
    [
        "client": "claude-desktop",
        "configured": configured,
        "enabled": configured,
        "connection": [
            "provider": "gateway",
            "base_url": "http://127.0.0.1:45274",
            "api_key": "baseten-switch-local",
            "auth_scheme": "bearer",
            "model_discovery": true,
            "auto_mode": false,
        ],
        "model_picker": ["enabled": configured, "models": rows],
        "runtime": [
            "state": runtime,
            "currently_bound": configured && runtime == "ready",
            "desired_matches_active": runtime == "ready",
        ],
        "auth": ["state": "ready"],
        "request_evidence": [
            "state": lastRequest == nil ? "not_observed" : "observed",
            "scope": "recent_500_requests",
            "last_request": lastRequest as Any? ?? NSNull(),
        ],
    ]
}

private func desktopJSON(_ object: [String: Any]) -> String {
    guard let data = try? JSONSerialization.data(withJSONObject: object) else {
        XCTFail("Invalid synthetic Desktop JSON fixture")
        return "{}"
    }
    return String(decoding: data, as: UTF8.self)
}

private actor DesktopFixture: CLIRunning, AdminStatusReading, ModelCatalogReading {
    private var configured: Bool
    private var rows: [[String: Any]]
    private var generation = 1
    private var available = true
    private let wrongReceiptClient: Bool
    private let contextPreviewFailure: Bool
    private(set) var requests: [CLIExecutionRequest] = []

    init(configured: Bool = true, wrongReceiptClient: Bool = false, contextPreviewFailure: Bool = false) {
        self.configured = configured
        self.wrongReceiptClient = wrongReceiptClient
        self.contextPreviewFailure = contextPreviewFailure
        rows = configured ? [desktopRow("alpha")] : []
    }

    func setUnavailable() { available = false }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        guard available else { throw GatewayClientError.invalidPayload }
        return AdminStatusSnapshot(dict: [
            "router_boot_id": "desktop-test",
            "active_generation": generation,
            "active_config_hash": "sha256:desktop-\(generation)",
            "desired_config_hash": "sha256:desktop-\(generation)",
            "capabilities": ["global_routing"],
            "health": "ready",
            "config_path": "/tmp/desktop-gateway.yaml",
            "reload": ["state": "ready", "error": ""],
            "global_routing_enabled": false,
            "clients": configured ? [[
                "name": "claude-desktop",
                "enabled": true,
                "allow_native": false,
                "currently_bound": true,
                "protocol_shape": "anthropic",
                "model_picker": ["enabled": true, "models": rows],
            ]] : [],
        ])
    }

    func fetchStats(windowSeconds: Int, bucketSeconds: Int) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }

    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        try XCTUnwrap(LiveModelCatalogSnapshot(dict: [
            "state": "ready",
            "signed_out_reason": "",
            "models": [
                ["slug": "example/alpha", "display_name": "Alpha"],
                ["slug": "example/beta", "display_name": "Beta"],
            ],
            "fetched_at": "2026-01-01T00:00:00Z",
            "error": "",
        ]))
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        requests.append(request)
        guard let start = request.arguments.firstIndex(of: "claude-desktop") else {
            return result(["ok": false], status: 1)
        }
        let command = Array(request.arguments.dropFirst(start + 1))
        if command.first == "status" {
            return result(desktopStatusObject(
                configured: configured,
                runtime: available ? (configured ? "ready" : "not_configured") : "unavailable",
                rows: rows))
        }
        if request.arguments.contains("--dry-run") {
            if contextPreviewFailure {
                return result([
                    "ok": false,
                    "error": [
                        "code": ClaudeModelPickerContextMinimumFailure.code,
                        "message": "This model has a known context limit below 200K tokens. Choose a model with at least 200K tokens.",
                        "retryable": false,
                    ],
                ], status: 1)
            }
            guard command.count > 2,
                  let leaf = command[2].split(separator: "/").last.map(String.init) else {
                return result(["ok": false], status: 1)
            }
            return result(desktopRow(leaf))
        }
        let kind: ClaudeDesktopMutation
        if command.first == "setup" {
            configured = true
            kind = .setup
        } else {
            guard command.count > 2 else { return result(["ok": false], status: 1) }
            switch command[1] {
            case "add":
                let slug = command[2]
                let alias = argument("--alias", request.arguments)
                guard let leaf = slug.split(separator: "/").last.map(String.init) else {
                    return result(["ok": false], status: 1)
                }
                var row = desktopRow(leaf)
                if let alias { row["alias"] = alias }
                rows.append(row)
                kind = .add(slug: slug, alias: alias)
            case "remove":
                let alias = command[2]
                rows.removeAll { $0["alias"] as? String == alias }
                kind = .remove(alias: alias)
            case "move":
                let alias = command[2]
                guard let before = argument("--before", command),
                      let index = rows.firstIndex(where: { $0["alias"] as? String == alias }) else {
                    return result(["ok": false], status: 1)
                }
                let row = rows.remove(at: index)
                guard let beforeIndex = rows.firstIndex(where: { $0["alias"] as? String == before }) else {
                    return result(["ok": false], status: 1)
                }
                rows.insert(row, at: beforeIndex)
                kind = .move(alias: alias, before: before)
            default:
                return result(["ok": false], status: 1)
            }
        }
        guard let operationID = argument("--operation-id", request.arguments) else {
            return result(["ok": false], status: 1)
        }
        generation += 1
        return result([
            "ok": true,
            "operation_id": operationID,
            "operation": kind.operation,
            "client": wrongReceiptClient ? "claude-code" : "claude-desktop",
            "key": kind.key,
            "requested_target": kind.requestedTarget,
            "desired_config_hash": "sha256:desktop-\(generation)",
            "active_config_hash": "sha256:desktop-\(generation)",
            "active_token": "desktop-test:\(generation)",
            "applied": true,
            "reconciliation_required": false,
            "outcome": "applied",
            "error": NSNull(),
        ])
    }

    private func argument(_ flag: String, _ arguments: [String]) -> String? {
        guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count else {
            return nil
        }
        return arguments[index + 1]
    }

    private func result(_ object: [String: Any], status: Int32 = 0) -> CLIExecutionResult {
        CLIExecutionResult(status: status, standardOutput: desktopJSON(object),
                           standardError: "", timedOut: false)
    }
}

@MainActor private final class DesktopLoginItemService: LoginItemServicing {
    var status: SMAppService.Status = .notRegistered
    func reconcileAtLaunch() {}
    func toggle() {}
    func openSystemSettings() {}
}

final class ClaudeDesktopTests: XCTestCase {
    func testDesktopRemainsDiscoverableBeforeConfigurationWithoutDuplicates() throws {
        XCTAssertEqual(discoverableClientNames([]), ["claude-desktop"])
        let code = try XCTUnwrap(ClientStatus(dict: ["name": "claude-code"]))
        let desktop = try XCTUnwrap(ClientStatus(dict: ["name": "claude-desktop"]))
        XCTAssertEqual(discoverableClientNames([code]), ["claude-code", "claude-desktop"])
        XCTAssertEqual(discoverableClientNames([code, desktop]), ["claude-code", "claude-desktop"])
        XCTAssertEqual(clientDisplayName("claude-desktop"), "Claude Desktop")
        XCTAssertEqual(clientBrandMarkKind("claude-desktop"), .systemSymbol("desktopcomputer"))
    }

    func testSetupProjectionAcceptsOnlyLoopbackAndNonSecretPlaceholder() throws {
        let object = desktopStatusObject()
        let status = try XCTUnwrap(ClaudeDesktopStatus(json: desktopJSON(object)))
        XCTAssertEqual(status.baseURL, "http://127.0.0.1:45274")
        XCTAssertEqual(ClaudeDesktopStatus.placeholderKey, "baseten-switch-local")
        XCTAssertEqual(status.modelPicker.models.first?.contextTokens, 128_000)

        for connectionOverride in [
            ["api_key": "synthetic-provider-secret"],
            ["base_url": "https://example.invalid"],
            ["base_url": "http://user:password@127.0.0.1:45274"],
            ["base_url": "http://127.0.0.1:45274?api_key=synthetic"],
        ] {
            var invalid = object
            var connection = try XCTUnwrap(invalid["connection"] as? [String: Any])
            connectionOverride.forEach { connection[$0.key] = $0.value }
            invalid["connection"] = connection
            XCTAssertNil(ClaudeDesktopStatus(json: desktopJSON(invalid)))
        }
    }

    func testHistoricalRequestDoesNotEstablishCurrentGatewayOrDesktopMode() throws {
        let last: [String: Any] = [
            "completed_at": "2026-01-01T00:00:00Z",
            "requested_model": "claude-baseten-alpha",
            "served_model": "example/alpha",
            "status": 200,
            "termination_reason": "completed",
        ]
        let unavailable = try XCTUnwrap(ClaudeDesktopStatus(json: desktopJSON(
            desktopStatusObject(runtime: "unavailable", lastRequest: last))))
        XCTAssertFalse(unavailable.gatewayReady)
        XCTAssertEqual(unavailable.lastRequest?.status, 200)
        XCTAssertEqual(claudeDesktopGatewayTitle(unavailable), "Local gateway unavailable")
        XCTAssertTrue(claudeDesktopGatewayDetail(unavailable).contains("if Claude Desktop is still configured"))
        let ready = try XCTUnwrap(ClaudeDesktopStatus(json: desktopJSON(desktopStatusObject())))
        XCTAssertEqual(claudeDesktopGatewayTitle(ready, statusIsCurrent: false), "Local gateway unavailable")
        XCTAssertEqual(claudeDesktopGatewayTitle(ready), "Local gateway ready")
    }

    func testGlobalOffKeepsDesktopAuthAttentionAndHonestCopy() throws {
        let desktop = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-desktop", "enabled": true,
            "allow_native": false, "currently_bound": true,
        ]))
        let code = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code", "enabled": true, "effective_route": "baseten",
        ]))
        let auth = AuthStatus(dict: ["health": "signed_out", "signed_in": false])
        XCTAssertTrue(code.allowNative)
        XCTAssertFalse(desktop.allowNative)
        XCTAssertEqual(authAttention(gatewayUp: true, globalRoutingEnabled: false,
                                     clients: [desktop], auth: auth), .signIn)
        XCTAssertEqual(authAttention(gatewayUp: true, globalRoutingEnabled: false,
                                     clients: [code], auth: auth), .none)
        let presentation = windowGlobalRoutingPresentation(enabled: false, phase: nil)
        XCTAssertEqual(presentation.overviewTitle, "Routing overrides are Off")
        XCTAssertTrue(presentation.overviewDescription.contains("Override clients"))
        XCTAssertTrue(presentation.overviewDescription.contains("Claude Desktop’s gateway remains active when configured"))
        XCTAssertEqual(livePathConfiguredRoute(client: desktop, globalRoutingEnabled: false),
                       "Selected Baseten models · independent of routing overrides")
        XCTAssertEqual(routingDestinationLabel(desktop), "Baseten gateway")
    }

    func testSharedModelSearchMatchesLabelAndSlugWithoutChangingOrder() throws {
        let beta = try XCTUnwrap(LiveModelCatalogEntry(dict: ["slug": "example/beta", "display_name": "Beta"]))
        let alpha = try XCTUnwrap(LiveModelCatalogEntry(dict: ["slug": "other/alpha", "display_name": "Alpha"]))
        let models = [beta, alpha]
        XCTAssertEqual(filterAvailableModels(models, query: " ").map(\.slug), ["example/beta", "other/alpha"])
        XCTAssertEqual(filterAvailableModels(models, query: " ALPHA ").map(\.slug), ["other/alpha"])
        XCTAssertEqual(filterAvailableModels(models, query: "example/").map(\.slug), ["example/beta"])
    }

    @MainActor
    func testStatusDoesNotSetupOrStartListenerAndSetupRequiresExplicitMutation() async throws {
        let fixture = DesktopFixture(configured: false)
        let state = makeState(fixture)
        await state.refresh()
        state.requestClaudeDesktopStatusRefresh()
        await state.waitForClaudeDesktopStatusRefresh()
        XCTAssertEqual(state.claudeDesktopStatus?.configured, false)
        XCTAssertEqual(state.clients, [])
        let reads = await fixture.requests
        XCTAssertEqual(reads.map(\.arguments), [["claude-desktop", "status", "--json"]])

        await state.setClaudeDesktop(.setup)
        XCTAssertNil(state.lastError)
        XCTAssertTrue(state.claudeDesktopStatus?.gatewayReady == true)
        XCTAssertEqual(state.claudeDesktopNotice, "Local gateway is ready. Connect Claude Desktop to finish setup.")
        let requests = await fixture.requests
        let setup = try XCTUnwrap(requests.first { $0.arguments.contains("setup") })
        XCTAssertEqual(Array(setup.arguments.suffix(2)), ["claude-desktop", "setup"])
        XCTAssertTrue(setup.arguments.contains("--if-active-token"))
        XCTAssertTrue(setup.arguments.contains("--if-config-hash"))
        XCTAssertFalse(requests.contains { $0.arguments.contains("up") })
        XCTAssertFalse(requests.contains { $0.arguments.contains("claude") })
        state.stop()
    }

    @MainActor
    func testAddMoveRemoveAreImmediateDesktopActionsWithActiveReadback() async throws {
        let fixture = DesktopFixture()
        let state = makeState(fixture)
        await state.refresh()
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        let preview = await state.previewClaudeDesktopAdd(slug: "example/beta")
        guard case .preview(let value, nil) = preview else {
            return XCTFail("Expected exact Desktop add preview")
        }
        XCTAssertEqual(value.alias, "claude-baseten-beta")
        await state.setClaudeDesktop(.add(slug: value.slug, alias: nil))
        XCTAssertNil(state.lastError)
        XCTAssertEqual(state.claudeDesktopStatus?.modelPicker.models.map(\.slug), ["example/alpha", "example/beta"])
        await state.setClaudeDesktop(.move(alias: "claude-baseten-beta", before: "claude-baseten-alpha"))
        XCTAssertEqual(state.claudeDesktopStatus?.modelPicker.models.map(\.slug), ["example/beta", "example/alpha"])
        await state.setClaudeDesktop(.remove(alias: "claude-baseten-alpha"))
        XCTAssertNil(state.lastError)
        XCTAssertEqual(state.claudeDesktopStatus?.modelPicker.models.map(\.slug), ["example/beta"])
        XCTAssertNil(state.claudeDesktopNotice)
        let requests = await fixture.requests
        XCTAssertTrue(requests.contains { $0.arguments == ["claude-desktop", "models", "add", "example/beta", "--dry-run", "--json"] })
        XCTAssertFalse(requests.contains { $0.arguments.contains("claude") || $0.arguments.contains("sync") })
        XCTAssertTrue(requests.allSatisfy { $0.environment["BASETEN_SWITCH_API_KEY"] == nil })
        state.stop()
    }

    @MainActor
    func testAddPreviewShowsContextMinimumFailureWithoutMutating() async {
        let fixture = DesktopFixture(contextPreviewFailure: true)
        let state = makeState(fixture)
        await state.refresh()
        state.requestClaudeDesktopStatusRefresh()
        await state.waitForClaudeDesktopStatusRefresh()
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        let preview = await state.previewClaudeDesktopAdd(slug: "example/beta")
        XCTAssertNil(preview)
        XCTAssertEqual(state.lastError, "This model has a known context limit below 200K tokens. Choose a model with at least 200K tokens.")
        XCTAssertEqual(state.claudeDesktopStatus?.modelPicker.models.map(\.slug), ["example/alpha"])
        XCTAssertNil(state.pendingClaudeDesktop)
        let requests = await fixture.requests
        let adds = requests.filter { $0.arguments.contains("add") }
        XCTAssertEqual(adds.count, 1)
        XCTAssertTrue(adds.allSatisfy { $0.arguments.contains("--dry-run") })
        state.stop()
    }

    @MainActor
    func testWrongClientReceiptCannotReportSuccessEvenWhenStatusChanged() async {
        let fixture = DesktopFixture(wrongReceiptClient: true)
        let state = makeState(fixture)
        await state.refresh()
        await state.setClaudeDesktop(.remove(alias: "claude-baseten-alpha"))
        XCTAssertEqual(state.claudeDesktopStatus?.modelPicker.models.count, 0)
        XCTAssertNotNil(state.lastError)
        XCTAssertNil(state.claudeDesktopNotice)
        state.stop()
    }

    @MainActor
    func testUnavailableGatewayPreservesConfiguredModelsWithoutClaimingReady() async {
        let fixture = DesktopFixture()
        let state = makeState(fixture)
        await state.refresh()
        state.requestClaudeDesktopStatusRefresh()
        await state.waitForClaudeDesktopStatusRefresh()
        await fixture.setUnavailable()
        await state.refresh()
        state.requestClaudeDesktopStatusRefresh()
        await state.waitForClaudeDesktopStatusRefresh()
        XCTAssertFalse(state.gatewayUp)
        XCTAssertFalse(state.canMutateClaudeDesktop)
        XCTAssertEqual(state.claudeDesktopStatus?.modelPicker.models.map(\.slug), ["example/alpha"])
        XCTAssertEqual(state.claudeDesktopStatus?.gatewayReady, false)
        state.stop()
    }

    func testPreviewChildrenUseDedicatedDesktopPort() {
        let preview = AppVariant.resolve(
            infoDictionary: ["BasetenSwitchBuildChannel": "preview"],
            homeDirectory: "/tmp/desktop-preview-tests", environment: [:])
        XCTAssertEqual(preview.runtime.environment["BASETEN_SWITCH_CLAUDE_DESKTOP_ADDR"], "127.0.0.1:45374")
        XCTAssertEqual(cliEnvironmentOverrides(configPath: "", variant: preview)["BASETEN_SWITCH_CLAUDE_DESKTOP_ADDR"], "127.0.0.1:45374")
    }

    @MainActor
    private func makeState(_ fixture: DesktopFixture) -> BasetenSwitchState {
        BasetenSwitchState(
            variant: AppVariant.resolve(infoDictionary: [:],
                homeDirectory: "/tmp/desktop-ui-tests",
                environment: [
                    "BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true",
                    "BASETEN_SWITCH_CONFIG_PATH": "/tmp/desktop-gateway.yaml",
                ]),
            reader: fixture, modelCatalogReader: fixture, cliRunner: fixture,
            loginItemService: DesktopLoginItemService(), startPolling: false)
    }
}
