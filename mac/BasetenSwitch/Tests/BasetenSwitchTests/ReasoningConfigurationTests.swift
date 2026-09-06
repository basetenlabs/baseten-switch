import Foundation
import ServiceManagement
import XCTest
@testable import BasetenSwitch

private final class ReasoningURLProtocol: URLProtocol {
    static var responseData = Data()
    static var statusCode = 200
    static var observedRequest: URLRequest?
    static var observedBody = Data()

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest)
        -> URLRequest {
        request
    }

    override func startLoading() {
        Self.observedRequest = request
        if let body = request.httpBody {
            Self.observedBody = body
        } else if let stream = request.httpBodyStream {
            stream.open()
            defer { stream.close() }
            var body = Data()
            let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: 4_096)
            defer { buffer.deallocate() }
            while stream.hasBytesAvailable {
                let count = stream.read(buffer, maxLength: 4_096)
                guard count > 0 else { break }
                body.append(buffer, count: count)
            }
            Self.observedBody = body
        }
        let response = HTTPURLResponse(
            url: request.url!,
            statusCode: Self.statusCode,
            httpVersion: nil,
            headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(
            self,
            didReceive: response,
            cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Self.responseData)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

private actor ReasoningAdminReader: AdminStatusReading {
    let status: AdminStatusSnapshot

    init(status: AdminStatusSnapshot) {
        self.status = status
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        status
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }
}

private actor FixedReasoningPreflightReader: ReasoningPreflightReading {
    let snapshot: ReasoningPreflightSnapshot
    private(set) var calls = 0
    private(set) var clients: [String] = []

    init(snapshot: ReasoningPreflightSnapshot) {
        self.snapshot = snapshot
    }

    func preflightReasoning(
        client: String,
        provider: String,
        model: String,
        policy: ReasoningPolicyValue
    ) async throws -> ReasoningPreflightSnapshot {
        calls += 1
        clients.append(client)
        return snapshot
    }
}

private actor ReasoningRecordingRunner: CLIRunning {
    private(set) var arguments: [[String]] = []

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        return CLIExecutionResult(
            status: 1,
            standardOutput: "",
            standardError: "fixture failure",
            timedOut: false)
    }
}

private actor ReasoningTerminalReplayRunner: CLIRunning {
    private(set) var arguments: [[String]] = []

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        arguments.append(request.arguments)
        let reconciling = request.arguments.contains("reconcile")
        let operationID: String
        if let index = request.arguments.firstIndex(of: "--operation-id"),
           request.arguments.indices.contains(index + 1) {
            operationID = request.arguments[index + 1]
        } else {
            operationID = request.arguments.last ?? ""
        }
        var object: [String: Any] = [
            "ok": reconciling,
            "operation_id": operationID,
            "operation": "set_model_reasoning",
            "client": "claude-code",
            "desired_config_hash": "sha256:active",
            "active_config_hash": "sha256:active",
            "active_token": "boot-a:4",
            "applied": reconciling,
            "reconciliation_required": !reconciling,
            "request_fingerprint":
                "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
            "identity_strength": "exact",
            "error": reconciling
                ? NSNull()
                : [
                    "code": "activation_indeterminate",
                    "message": "reconciliation required",
                ],
        ]
        if !reconciling {
            object["key"] = "zai-org/GLM-5.2"
            object["requested_target"] = "default"
        }
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: reconciling ? 0 : 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }
}

@MainActor
private final class ReasoningLoginItemService: LoginItemServicing {
    var status: SMAppService.Status = .notRegistered
    func reconcileAtLaunch() {}
    func toggle() {}
    func openSystemSettings() {}
}

final class ReasoningConfigurationTests: XCTestCase {
    private let model = "zai-org/GLM-5.2"

    func testCatalogReasoningDecodePreservesOptionOrderNullAndProvenance() {
        let entry = LiveModelCatalogEntry(dict: [
            "slug": model,
            "display_name": "GLM 5.2",
            "reasoning": [
                "supported": true,
                "options": [
                    ["type": "toggle"],
                    [
                        "type": "effort",
                        "values": ["low", NSNull(), "high"],
                    ],
                    [
                        "type": "budget_tokens",
                        "min": 512,
                        "max": 8_192,
                    ],
                ],
                "source": "models_dev",
                "loaded_from": "runtime_cache",
                "revision": "sha256:catalog",
                "captured_at": "2026-07-25T18:00:00Z",
                "stale": true,
            ],
        ])

        XCTAssertEqual(entry?.reasoning?.supported, true)
        XCTAssertEqual(
            entry?.reasoning?.options.map(\.type),
            [.toggle, .effort, .budgetTokens])
        XCTAssertEqual(
            entry?.reasoning?.options[1].values,
            ["low", nil, "high"])
        XCTAssertEqual(entry?.reasoning?.options[2].minimum, 512)
        XCTAssertEqual(entry?.reasoning?.options[2].maximum, 8_192)
        XCTAssertEqual(entry?.reasoning?.source, "models_dev")
        XCTAssertEqual(entry?.reasoning?.loadedFrom, "runtime_cache")
        XCTAssertEqual(entry?.reasoning?.revision, "sha256:catalog")
        XCTAssertEqual(entry?.reasoning?.stale, true)
    }

    func testCatalogRejectsMalformedReasoningInsteadOfPartiallyDecoding() {
        XCTAssertNil(LiveModelCatalogEntry(dict: [
            "slug": model,
            "display_name": "GLM 5.2",
            "reasoning": [
                "supported": true,
                "options": [
                    ["type": "effort", "values": ["low", 42]],
                ],
                "source": "models_dev",
                "loaded_from": "network",
                "revision": "sha256:catalog",
                "captured_at": "2026-07-25T18:00:00Z",
                "stale": false,
            ],
        ]))
    }

    func testClientStatusDecodesConfiguredAndEffectiveReasoningProjection() {
        let client = reasoningClient(
            configured: ["mode": "fixed", "effort": "high"],
            effective: ["mode": "fixed", "effort": "high"],
            availableModes: ["off", "follow_harness"],
            availableEfforts: ["low", "medium", "high"])
        let status = client.modelOptions["baseten"]?[model]?.reasoning

        XCTAssertEqual(status?.configured.mode, .fixed)
        XCTAssertEqual(status?.configured.effort, "high")
        XCTAssertEqual(status?.effective.mode, .fixed)
        XCTAssertEqual(status?.availableModes, [.off, .followHarness])
        XCTAssertEqual(
            status?.availableEfforts,
            ["low", "medium", "high"])
        XCTAssertEqual(status?.source, "user_config")
    }

    func testDefaultUnsupportedAdapterExplainsDefaultPolicyFailure() {
        func status(mode: String) -> ClientReasoningStatus {
            ClientReasoningStatus(dict: [
                "configured": ["mode": mode],
                "effective": ["mode": "passthrough"],
                "source": "internal_passthrough",
                "available_modes": [],
                "available_efforts": [],
                "available": false,
                "unavailable_reason": "protocol_adapter_unsupported",
                "error": "no reviewed reasoning control is available",
            ])!
        }
        XCTAssertTrue(reasoningShowsUnavailableWarning(
            status(mode: "default")))
        XCTAssertEqual(
            reasoningUnavailableMessage(status(mode: "default")),
            "This client protocol cannot apply the model’s default reasoning policy.")
        XCTAssertTrue(reasoningShowsUnavailableWarning(
            status(mode: "follow_harness")))
        XCTAssertEqual(
            reasoningUnavailableMessage(status(mode: "follow_harness")),
            "This client protocol cannot apply the saved reasoning policy.")
    }

    func testPreflightPostsTypedPolicyForSelectedClient() async throws {
        ReasoningURLProtocol.responseData = Data("""
        {
          "provider": "baseten",
          "model": "zai-org/GLM-5.2",
          "policy": {"mode": "on"},
          "available": true,
          "error": "",
          "warning": "",
          "clients": [
            {
              "name": "claude-code",
              "enabled": true,
              "reachable": true,
              "supported": true,
              "reachability": ["family:opus"],
              "failure_behaviors": [],
              "available_modes": ["on", "off", "follow_harness"],
              "available_efforts": [],
              "unavailable_reason": "",
              "error": ""
            }
          ]
        }
        """.utf8)
        ReasoningURLProtocol.statusCode = 200
        ReasoningURLProtocol.observedRequest = nil
        ReasoningURLProtocol.observedBody = Data()
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ReasoningURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(),
            session: URLSession(configuration: configuration))

        let snapshot = try await client.preflightReasoning(
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .on))

        let request = try XCTUnwrap(ReasoningURLProtocol.observedRequest)
        XCTAssertEqual(request.httpMethod, "POST")
        XCTAssertEqual(
            request.url?.path,
            "/v1/admin/reasoning/preflight")
        let body = ReasoningURLProtocol.observedBody
        XCTAssertFalse(body.isEmpty)
        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: body) as? [String: Any])
        XCTAssertEqual(object["provider"] as? String, "baseten")
        XCTAssertEqual(object["client"] as? String, "claude-code")
        XCTAssertEqual(object["model"] as? String, model)
        XCTAssertEqual(
            (object["policy"] as? [String: Any])?["mode"] as? String,
            "on")
        XCTAssertEqual(snapshot.clients.map(\.name), ["claude-code"])
        XCTAssertEqual(
            snapshot.clients[0].availableModes,
            [.on, .off, .followHarness])
    }

    func testDisplayDeduplicatesFamiliesAndUsesGatewayOrderedChoices() {
        let client = reasoningClient(
            configured: ["mode": "default"],
            effective: ["mode": "off"],
            availableModes: ["off", "on", "follow_harness"],
            availableEfforts: ["low", "high"],
            families: ["fable", "opus", "sonnet", "haiku"])
        let live = [reasoningLiveModel(stale: false)]

        let rows = reasoningRowsForDisplay(client: client, liveModels: live)

        XCTAssertEqual(rows.count, 1)
        XCTAssertEqual(rows[0].model, model)
        XCTAssertEqual(
            rows[0].mappingFamilies,
            ["Fable", "Haiku", "Opus", "Sonnet"])
        XCTAssertEqual(
            reasoningChoices(status: rows[0].status),
            [.on, .off])
        XCTAssertEqual(
            reasoningSelection(status: rows[0].status),
            .off)
    }

    func testBinaryControlsRequireAvailableOnAndOffProjection() {
        func choices(
            modes: [String],
            available: Bool = true
        ) -> [ReasoningChoice] {
            let dictionary = reasoningStatusDictionary(
                configured: ["mode": "default"],
                effective: ["mode": "passthrough"],
                availableModes: modes,
                availableEfforts: ["low", "medium", "high"])
            var projected = dictionary
            projected["available"] = available
            return reasoningChoices(
                status: ClientReasoningStatus(dict: projected)!)
        }

        XCTAssertEqual(choices(modes: ["on", "off"]), [.on, .off])
        XCTAssertTrue(choices(modes: ["on"]).isEmpty)
        XCTAssertTrue(choices(modes: ["off"]).isEmpty)
        XCTAssertTrue(
            choices(modes: ["on", "off"], available: false).isEmpty)
    }

    func testDisplayIncludesOnlySelectedModelsFromBroadGatewayOptions() {
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [
                ("fable", model),
                ("opus", model),
                ("sonnet", model),
                ("haiku", model),
            ])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(rows.map(\.model), [model])
        XCTAssertEqual(
            rows[0].mappingFamilies,
            ["Fable", "Haiku", "Opus", "Sonnet"])
    }

    func testDisplayAddsAnotherSelectedFamilyModel() {
        let kimi = "moonshotai/Kimi-K2.7-Code"
        let kimiAlias = "claude-baseten-kimi-k2-7-code"
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [
                ("fable", model),
                ("opus", model),
                ("sonnet", kimiAlias),
                ("haiku", model),
            ])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(Set(rows.map(\.model)), [model, kimi])
    }

    func testDisplayExcludesHiddenDefaultWhenVisibleFamiliesSelectOtherTargets() {
        let gpt = "openai/gpt-oss-120b"
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [
                ("fable", gpt),
                ("opus", "native"),
                ("sonnet", gpt),
                ("haiku", "native"),
            ])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(rows.map(\.model), [gpt])
    }

    func testDisplayIncludesTargetFromInheritedDefaultFamily() {
        let gpt = "openai/gpt-oss-120b"
        let client = reasoningVisibilityClient(
            defaultModel: gpt,
            families: [("opus", model)],
            familySources: ["opus": "default"])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(rows.map(\.model), [model])
        XCTAssertEqual(rows[0].mappingFamilies, ["Opus"])
    }

    func testDisplayUsesUnmatchedDefaultForFamilylessClient() {
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(rows.map(\.model), [model])
        XCTAssertTrue(rows[0].mappingFamilies.isEmpty)
    }

    func testDisplayIncludesActiveExplicitSubagentOverride() {
        let nemotron = "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"
        let nemotronAlias = "claude-baseten-nemotron-ultra"
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [("opus", model)],
            subagentModel: nemotronAlias,
            subagentRouting: "on",
            subagentEffective: nemotronAlias)

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(Set(rows.map(\.model)), [model, nemotron])
    }

    func testDisplayIncludesEnabledPickerOnlyFastModelWithBinaryControls() {
        let fast = "zai-org/GLM-5.2-Fast"
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [("opus", model)],
            pickerEnabled: true,
            pickerModels: [
                ("claude-baseten-glm-5-2-fast", fast),
            ])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])
        let fastRow = try! XCTUnwrap(rows.first { $0.model == fast })

        XCTAssertEqual(reasoningSelection(status: fastRow.status), .off)
        XCTAssertEqual(reasoningChoices(status: fastRow.status), [.on, .off])
        XCTAssertTrue(fastRow.mappingFamilies.isEmpty)
    }

    func testDisplayExcludesModelsFromDisabledPicker() {
        let fast = "zai-org/GLM-5.2-Fast"
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [("opus", model)],
            pickerEnabled: false,
            pickerModels: [
                ("claude-baseten-glm-5-2-fast", fast),
            ])

        XCTAssertEqual(
            reasoningRowsForDisplay(client: client, liveModels: []).map(\.model),
            [model])
    }

    func testDisplayDeduplicatesPickerAliasesAndRoutingSelections() {
        let fast = "zai-org/GLM-5.2-Fast"
        let client = reasoningVisibilityClient(
            defaultModel: fast,
            families: [("opus", fast)],
            subagentModel: fast,
            subagentRouting: "on",
            subagentEffective: fast,
            pickerEnabled: true,
            pickerModels: [
                ("claude-baseten-glm-fast", fast),
                ("claude-baseten-glm-fast-long", fast),
                ("native", "native"),
            ])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(rows.map(\.model), [fast])
        XCTAssertEqual(rows[0].mappingFamilies, ["Opus"])
    }

    func testDisplayRetainsUnavailablePickerModelAsReadOnly() {
        let retired = "moonshotai/Retired-Reasoner"
        let client = reasoningVisibilityClient(
            defaultModel: model,
            families: [("opus", model)],
            unavailableModels: [retired],
            pickerEnabled: true,
            pickerModels: [
                ("claude-baseten-retired-reasoner", retired),
            ])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])
        let retiredRow = try! XCTUnwrap(rows.first { $0.model == retired })

        XCTAssertFalse(retiredRow.status.available)
        XCTAssertTrue(reasoningChoices(status: retiredRow.status).isEmpty)
        XCTAssertEqual(reasoningSelection(status: retiredRow.status), .off)
        XCTAssertEqual(
            retiredRow.status.unavailableReason,
            "catalog_metadata_missing")
    }

    func testDisplayDoesNotInventPickerControlsWithoutAdminProjection() {
        let pickerOnly = ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "bind_addr": "127.0.0.1:8789",
            "model_picker": [
                "enabled": true,
                "models": [[
                    "alias": "claude-baseten-example-reasoner",
                    "slug": "example-org/Example-Reasoner",
                    "label": "Example Reasoner",
                    "description": "Served by Baseten.",
                    "context_tokens": 32_000,
                ]],
            ],
            "model_catalog": [[
                "label": "Example Reasoner",
                "storage_target": "claude-baseten-example-reasoner",
                "slug": "example-org/Example-Reasoner",
                "alias": "claude-baseten-example-reasoner",
                "available": true,
            ]],
            "model_options": ["baseten": [:]],
        ])!

        XCTAssertTrue(
            reasoningRowsForDisplay(client: pickerOnly, liveModels: []).isEmpty)
    }

    func testDisplayExcludesInactiveAndInheritedSubagentTargets() {
        let nemotron = "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"
        let inactive = reasoningVisibilityClient(
            defaultModel: model,
            families: [("opus", model)],
            subagentModel: nemotron,
            subagentRouting: "off",
            subagentEffective: "inherit")
        let inherited = reasoningVisibilityClient(
            defaultModel: model,
            families: [("opus", model)],
            subagentModel: nemotron,
            subagentRouting: "on",
            subagentEffective: "inherit")

        XCTAssertEqual(
            reasoningRowsForDisplay(
                client: inactive,
                liveModels: []).map(\.model),
            [model])
        XCTAssertEqual(
            reasoningRowsForDisplay(
                client: inherited,
                liveModels: []).map(\.model),
            [model])
    }

    func testDisplayRetainsUnavailableSelectedModelWhileRoutingIsOff() {
        let unavailable = "moonshotai/Retired-Reasoner"
        let client = reasoningVisibilityClient(
            defaultModel: unavailable,
            families: [("opus", unavailable)],
            unavailableModels: [unavailable])

        let rows = reasoningRowsForDisplay(client: client, liveModels: [])

        XCTAssertEqual(rows.map(\.model), [unavailable])
        XCTAssertEqual(rows[0].displayName, "Retired Reasoner")
        XCTAssertFalse(rows[0].status.available)
        XCTAssertEqual(
            rows[0].status.unavailableReason,
            "catalog_metadata_missing")
    }

    func testExecutableDefaultPassthroughWithoutControlsIsNeutralReadOnly() {
        let status = reasoningStatus(
            configured: ["mode": "default"],
            effective: ["mode": "passthrough"],
            availableModes: [],
            availableEfforts: [])

        XCTAssertTrue(
            reasoningUsesDefaultPassthroughReadOnlyState(status))
        XCTAssertTrue(reasoningChoices(status: status).isEmpty)
        XCTAssertEqual(
            reasoningDefaultPassthroughReadOnlyLabel(),
            "Uses provider default")
        XCTAssertEqual(
            reasoningSelection(status: status),
            .defaultPassthrough)
        XCTAssertFalse(reasoningShowsResetAction(status))
        XCTAssertFalse(reasoningShowsUnavailableWarning(status))
    }

    func testExplicitFollowHarnessRemainsLegacyAndCanResetToDefault() {
        let status = reasoningStatus(
            configured: ["mode": "follow_harness"],
            effective: ["mode": "follow_harness"],
            availableModes: ["on", "off", "follow_harness"],
            availableEfforts: [])

        XCTAssertFalse(
            reasoningUsesDefaultPassthroughReadOnlyState(status))
        XCTAssertEqual(
            reasoningChoices(status: status),
            [.on, .off])
        XCTAssertEqual(
            reasoningSelection(status: status),
            .followHarness)
        XCTAssertTrue(reasoningShowsResetAction(status))
    }

    func testBinaryControlsDoNotRelabelLegacyFollowHarness() {
        let defaultStatus = reasoningStatus(
            configured: ["mode": "default"],
            effective: ["mode": "off"],
            availableModes: ["off", "on", "follow_harness"],
            availableEfforts: [])

        XCTAssertFalse(
            reasoningUsesDefaultPassthroughReadOnlyState(defaultStatus))
        XCTAssertEqual(
            reasoningChoices(status: defaultStatus),
            [.on, .off])
        XCTAssertEqual(reasoningSelection(status: defaultStatus), .off)

        let explicitStatus = reasoningStatus(
            configured: ["mode": "follow_harness"],
            effective: ["mode": "follow_harness"],
            availableModes: ["off", "on", "follow_harness"],
            availableEfforts: [])
        XCTAssertFalse(
            reasoningUsesDefaultPassthroughReadOnlyState(explicitStatus))
        XCTAssertEqual(
            reasoningChoices(status: explicitStatus),
            [.on, .off])
        XCTAssertEqual(
            reasoningSelection(status: explicitStatus),
            .followHarness)
        XCTAssertNil(reasoningSelection(status: explicitStatus).policy)
        XCTAssertTrue(reasoningShowsResetAction(explicitStatus))
    }

    func testResetIsShownForEveryExplicitPolicy() {
        let explicitOff = reasoningStatus(
            configured: ["mode": "off"],
            effective: ["mode": "off"],
            availableModes: ["off", "on"],
            availableEfforts: [])
        XCTAssertTrue(reasoningShowsResetAction(explicitOff))

        let explicitOn = reasoningStatus(
            configured: ["mode": "on"],
            effective: ["mode": "on"],
            availableModes: ["off", "on"],
            availableEfforts: [])
        XCTAssertTrue(reasoningShowsResetAction(explicitOn))

        let followHarness = reasoningStatus(
            configured: ["mode": "follow_harness"],
            effective: ["mode": "follow_harness"],
            availableModes: ["off", "on", "follow_harness"],
            availableEfforts: [])
        XCTAssertTrue(reasoningShowsResetAction(followHarness))

        let fixed = reasoningStatus(
            configured: ["mode": "fixed", "effort": "high"],
            effective: ["mode": "fixed", "effort": "high"],
            availableModes: ["follow_harness"],
            availableEfforts: ["low", "high"])
        XCTAssertTrue(reasoningShowsResetAction(fixed))

        let inherited = reasoningStatus(
            configured: ["mode": "default"],
            effective: ["mode": "off"],
            availableModes: ["off", "on"],
            availableEfforts: [])
        XCTAssertFalse(reasoningShowsResetAction(inherited))
    }

    func testUnavailableDefaultOffDoesNotSynthesizeOffChoice() {
        let status = ClientReasoningStatus(dict: [
            "configured": ["mode": "default"],
            "effective": ["mode": "off"],
            "source": "compatibility_default",
            "available_modes": ["follow_harness"],
            "available_efforts": [],
            "available": false,
            "unavailable_reason": "protocol_adapter_unsupported",
            "error": "no reviewed Off transform is available",
        ])!

        XCTAssertFalse(
            reasoningUsesDefaultPassthroughReadOnlyState(status))
        XCTAssertEqual(
            reasoningChoices(status: status),
            [])
        XCTAssertFalse(reasoningChoices(status: status).contains(.off))
        XCTAssertEqual(reasoningSelection(status: status), .off)
        XCTAssertTrue(reasoningShowsUnavailableWarning(status))
        XCTAssertEqual(
            reasoningUnavailableMessage(status),
            "This client protocol cannot apply the model’s default reasoning policy.")
    }

    func testRemovedEffortRemainsUnavailable() {
        let removed = reasoningStatus(
            configured: ["mode": "fixed", "effort": "xhigh"],
            effective: ["mode": "fixed", "effort": "xhigh"],
            availableModes: ["follow_harness"],
            availableEfforts: ["low", "high"])
        XCTAssertEqual(
            reasoningSelection(status: removed),
            .unavailable("xhigh"))
        XCTAssertTrue(reasoningChoices(status: removed).isEmpty)
        XCTAssertTrue(reasoningShowsResetAction(removed))
    }

    func testNoVerifiedControlIsReadOnlyInsteadOfDefaultPicker() {
        let noControl = reasoningStatus(
            configured: ["mode": "default"],
            effective: ["mode": "passthrough"],
            availableModes: [],
            availableEfforts: [])

        XCTAssertTrue(reasoningChoices(status: noControl).isEmpty)
        XCTAssertEqual(
            reasoningSelection(status: noControl),
            .defaultPassthrough)
    }

    func testPendingDefaultResetIsNotReportedAsUnavailable() {
        let explicit = reasoningStatus(
            configured: ["mode": "fixed", "effort": "high"],
            effective: ["mode": "fixed", "effort": "high"],
            availableModes: ["follow_harness"],
            availableEfforts: ["low", "high"])

        let selection = reasoningSelection(
            status: explicit,
            pending: ReasoningPolicyValue(mode: .default))

        XCTAssertEqual(selection, .pendingDefault)
        XCTAssertEqual(
            reasoningChoiceLabel(selection, clientName: "claude-code"),
            "Resetting to Safe Default…")
        XCTAssertFalse(
            reasoningChoiceLabel(selection, clientName: "claude-code")
                .contains("Unavailable"))
    }

    func testReasoningCaptionPreservesPolicySemanticsForDeduplicatedModel() {
        let defaultClient = reasoningClient(
            configured: ["mode": "default"],
            effective: ["mode": "off"],
            availableModes: ["off", "on", "follow_harness"],
            availableEfforts: [],
            source: "compatibility_default",
            families: ["fable", "opus", "sonnet", "haiku"])
        let defaultRow = reasoningRowsForDisplay(
            client: defaultClient,
            liveModels: [reasoningLiveModel(stale: false)])[0]
        XCTAssertEqual(
            reasoningCaption(row: defaultRow, clientName: defaultClient.name),
            "Safe default: Off.")

        let followClient = reasoningClient(
            configured: ["mode": "follow_harness"],
            effective: ["mode": "follow_harness"],
            availableModes: ["off", "on", "follow_harness"],
            availableEfforts: [],
            families: ["fable", "opus", "sonnet", "haiku"])
        let followRow = reasoningRowsForDisplay(
            client: followClient,
            liveModels: [reasoningLiveModel(stale: false)])[0]
        XCTAssertEqual(
            reasoningCaption(row: followRow, clientName: followClient.name),
            "Saved legacy policy: follow Claude Code’s reasoning setting when supported.")

        let onStatus = reasoningStatus(
            configured: ["mode": "on"],
            effective: ["mode": "on"],
            availableModes: ["on", "off"],
            availableEfforts: [])
        let onRow = ReasoningDisplayRow(
            provider: "baseten",
            model: model,
            displayName: "GLM 5.2",
            status: onStatus,
            capability: nil,
            mappingFamilies: ["Opus"])
        XCTAssertEqual(
            reasoningCaption(row: onRow, clientName: "claude-code"),
            "Switch enables reasoning. Claude Code effort is forwarded unchanged.")

        var unavailableDictionary = reasoningStatusDictionary(
            configured: ["mode": "on"],
            effective: ["mode": "passthrough"],
            availableModes: [],
            availableEfforts: [])
        unavailableDictionary["available"] = false
        let unavailableRow = ReasoningDisplayRow(
            provider: "baseten",
            model: model,
            displayName: "GLM 5.2",
            status: ClientReasoningStatus(dict: unavailableDictionary)!,
            capability: nil,
            mappingFamilies: ["Opus"])
        XCTAssertEqual(
            reasoningCaption(
                row: unavailableRow,
                clientName: "claude-code"),
            "Saved policy: On.")
    }

    func testReasoningDispatchAndReconciliationUseExactTypedProjection() {
        XCTAssertEqual(
            reasoningDispatchArgs(
                client: "claude-code",
                provider: "baseten",
                model: model,
                policy: ReasoningPolicyValue(mode: .on)),
            ["claude", "reasoning", "baseten", model, "on"])
        XCTAssertEqual(
            reasoningDispatchArgs(
                client: "claude-code",
                provider: "baseten",
                model: model,
                policy: ReasoningPolicyValue(mode: .followHarness)),
            ["claude", "reasoning", "baseten", model, "follow-harness"])
        XCTAssertEqual(
            reasoningDispatchArgs(
                client: "codex",
                provider: "baseten",
                model: model,
                policy: ReasoningPolicyValue(mode: .fixed, effort: "high")),
            ["codex", "reasoning", "baseten", model, "effort", "high"])
        XCTAssertEqual(
            reasoningDispatchArgs(
                client: "anthropic-fallback",
                protocolShape: "anthropic",
                provider: "baseten",
                model: model,
                policy: ReasoningPolicyValue(mode: .off)),
            ["claude", "reasoning", "baseten", model, "off"])

        let fixed = routingSnapshot(client: reasoningClient(
            configured: ["mode": "fixed", "effort": "high"],
            effective: ["mode": "fixed", "effort": "high"],
            availableModes: ["follow_harness"],
            availableEfforts: ["high"]))
        XCTAssertTrue(reasoningMutationConfirmed(
            snapshot: fixed,
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .fixed, effort: "high")))
        XCTAssertFalse(reasoningMutationConfirmed(
            snapshot: fixed,
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .off)))

        let noProjection = routingSnapshot(client: ClientStatus(
            dict: ["name": "claude-code"])!)
        XCTAssertTrue(reasoningMutationConfirmed(
            snapshot: noProjection,
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .default)))
    }

    @MainActor
    func testReasoningMutationIsSingleFlightAndAppliesOnlySelectedClient()
        async {
        let preflight = FixedReasoningPreflightReader(
            snapshot: preflightSnapshot())
        let runner = ReasoningRecordingRunner()
        let state = makeState(
            preflight: preflight,
            runner: runner)
        await state.refresh()

        XCTAssertTrue(state.requestReasoning(
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .on)))
        XCTAssertFalse(state.requestReasoning(
            client: "claude-code",
            provider: "baseten",
            model: "another/model",
            policy: ReasoningPolicyValue(mode: .off)))
        await waitUntil { state.pendingReasoning == nil }

        let preflightCalls = await preflight.calls
        XCTAssertEqual(preflightCalls, 1)
        let calls = await runner.arguments
        XCTAssertEqual(
            Array(calls[0].suffix(5)),
            [
                "claude",
                "reasoning",
                "baseten",
                model,
                "on",
            ])
        XCTAssertNil(state.pendingReasoning)
        state.stop()
    }

    @MainActor
    func testFallbackAnthropicReasoningUsesClaudeVerbAndSelectedIdentity()
        async {
        let preflight = FixedReasoningPreflightReader(
            snapshot: preflightSnapshot())
        let runner = ReasoningRecordingRunner()
        let state = makeState(
            preflight: preflight,
            runner: runner,
            clientName: "anthropic-fallback",
            protocolShape: "anthropic")
        await state.refresh()

        XCTAssertTrue(state.requestReasoning(
            client: "anthropic-fallback",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .on)))
        await waitUntil { state.pendingReasoning == nil }

        let clients = await preflight.clients
        XCTAssertEqual(clients, ["anthropic-fallback"])
        let calls = await runner.arguments
        XCTAssertEqual(
            Array(calls[0].suffix(5)),
            [
                "claude",
                "reasoning",
                "baseten",
                model,
                "on",
            ])
        state.stop()
    }

    @MainActor
    func testDefaultResetSkipsPreflightAndUsesTypedCLIPath() async {
        let preflight = FixedReasoningPreflightReader(
            snapshot: preflightSnapshot())
        let runner = ReasoningRecordingRunner()
        let state = makeState(
            preflight: preflight,
            runner: runner)
        await state.refresh()

        XCTAssertTrue(state.requestReasoning(
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .default)))
        await waitUntil { state.pendingReasoning == nil }

        let preflightCalls = await preflight.calls
        XCTAssertEqual(preflightCalls, 0)
        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(
            Array(calls[0].suffix(5)),
            ["claude", "reasoning", "baseten", model, "default"])
        XCTAssertEqual(
            Array(calls[1].prefix(3)),
            ["--json", "mutation", "reconcile"])
        state.stop()
    }

    @MainActor
    func testReasoningAcceptsVerifiedTargetlessTerminalReplay() async {
        let preflight = FixedReasoningPreflightReader(
            snapshot: preflightSnapshot())
        let runner = ReasoningTerminalReplayRunner()
        let state = makeState(
            preflight: preflight,
            runner: runner)
        await state.refresh()

        XCTAssertTrue(state.requestReasoning(
            client: "claude-code",
            provider: "baseten",
            model: model,
            policy: ReasoningPolicyValue(mode: .default)))
        await waitUntil { state.pendingReasoning == nil }

        XCTAssertNil(state.lastError)
        let calls = await runner.arguments
        XCTAssertEqual(calls.count, 2)
        XCTAssertEqual(
            Array(calls[1].prefix(3)),
            ["--json", "mutation", "reconcile"])
        state.stop()
    }

    @MainActor
    func testHealthyPreviewFixtureRendersOneDeterministicReasoningRow() {
        let state = BasetenSwitchState(preview: .healthy)
        guard case .ready(let live) = state.liveModelCatalogState else {
            return XCTFail("expected a ready fixture catalog")
        }
        let client = try! XCTUnwrap(state.clients.first)

        let rows = reasoningRowsForDisplay(client: client, liveModels: live)

        XCTAssertEqual(rows.count, 1)
        XCTAssertEqual(rows[0].displayName, "GLM 5.2")
        XCTAssertEqual(reasoningSelection(status: rows[0].status), .off)
        state.stop()
    }

    private func reasoningClient(
        configured: [String: Any],
        effective: [String: Any],
        availableModes: [String],
        availableEfforts: [String],
        source: String = "user_config",
        families: [String] = ["opus"]
    ) -> ClientStatus {
        ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "bind_addr": "127.0.0.1:8789",
            "families": families.map {
                [
                    "family": $0,
                    "configured_target": model,
                    "configured_source": "explicit",
                ]
            },
            "model_catalog": [[
                "label": "GLM 5.2",
                "storage_target": model,
                "slug": model,
                "available": true,
            ]],
            "model_options": [
                "baseten": [
                    model: [
                        "reasoning": reasoningStatusDictionary(
                            configured: configured,
                            effective: effective,
                            availableModes: availableModes,
                            availableEfforts: availableEfforts,
                            source: source),
                    ],
                ],
            ],
        ])!
    }

    private func reasoningStatus(
        configured: [String: Any],
        effective: [String: Any],
        availableModes: [String],
        availableEfforts: [String],
        source: String = "user_config"
    ) -> ClientReasoningStatus {
        ClientReasoningStatus(dict: reasoningStatusDictionary(
            configured: configured,
            effective: effective,
            availableModes: availableModes,
            availableEfforts: availableEfforts,
            source: source))!
    }

    private func reasoningStatusDictionary(
        configured: [String: Any],
        effective: [String: Any],
        availableModes: [String],
        availableEfforts: [String],
        source: String = "user_config"
    ) -> [String: Any] {
        [
            "configured": configured,
            "effective": effective,
            "source": source,
            "available_modes": availableModes,
            "available_efforts": availableEfforts,
            "available": true,
            "unavailable_reason": "",
            "error": "",
        ]
    }

    private func reasoningVisibilityClient(
        defaultModel: String,
        families: [(String, String)],
        familySources: [String: String] = [:],
        subagentModel: String = "",
        subagentRouting: String = "off",
        subagentEffective: String = "inherit",
        unavailableModels: Set<String> = [],
        pickerEnabled: Bool? = nil,
        pickerModels: [(alias: String, slug: String)] = []
    ) -> ClientStatus {
        let fast = "zai-org/GLM-5.2-Fast"
        let kimi = "moonshotai/Kimi-K2.7-Code"
        let nemotron = "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"
        let retired = "moonshotai/Retired-Reasoner"
        let gpt = "openai/gpt-oss-120b"
        let catalogModels = [
            (model, "GLM 5.2", "claude-baseten-glm-5-2"),
            (fast, "GLM 5.2 Fast", "claude-baseten-glm-5-2-fast"),
            (kimi, "Kimi K2.7 Code", "claude-baseten-kimi-k2-7-code"),
            (nemotron, "Nemotron Ultra", "claude-baseten-nemotron-ultra"),
            (retired, "Retired Reasoner", "claude-baseten-retired-reasoner"),
            (gpt, "GPT OSS 120B", "claude-baseten-gpt-oss-120b"),
        ]
        let options = Dictionary(uniqueKeysWithValues: catalogModels.map {
            slug, _, _ in
            let available = !unavailableModels.contains(slug)
            return (
                slug,
                [
                    "reasoning": [
                        "configured": ["mode": "default"],
                        "effective": ["mode": "off"],
                        "source": "compatibility_default",
                        "available_modes": ["off", "on", "follow_harness"],
                        "available_efforts": [],
                        "available": available,
                        "unavailable_reason": available
                            ? ""
                            : "catalog_metadata_missing",
                        "error": "",
                    ] as [String: Any],
                ] as [String: Any])
        })
        var dictionary: [String: Any] = [
            "name": "claude-code",
            "enabled": true,
            "bind_addr": "127.0.0.1:8789",
            "effective_route": "anthropic",
            "subagent_model": subagentModel,
            "subagent_routing": subagentRouting,
            "subagent_effective": subagentEffective,
            "unmatched_native_model": [
                "configured_target": defaultModel,
                "effective_route": "anthropic",
                "effective_model": "",
                "effective_source": "global_off",
            ],
            "families": families.map {
                [
                    "family": $0.0,
                    "configured_target": $0.1,
                    "configured_source":
                        familySources[$0.0] ?? "explicit",
                    "effective_route": "anthropic",
                    "effective_model": "",
                    "effective_source": "global_off",
                ]
            },
            "model_catalog": catalogModels.map {
                [
                    "label": $0.1,
                    "storage_target": $0.2,
                    "slug": $0.0,
                    "alias": $0.2,
                    "available": !unavailableModels.contains($0.0),
                ] as [String: Any]
            },
            "model_options": ["baseten": options],
        ]
        if let pickerEnabled {
            dictionary["model_picker"] = [
                "enabled": pickerEnabled,
                "models": pickerModels.map { pickerModel in
                    [
                        "alias": pickerModel.alias,
                        "slug": pickerModel.slug,
                        "label": "Picker model",
                        "description": "Served by Baseten.",
                        "context_tokens": 32_000,
                    ] as [String: Any]
                },
            ]
        }
        return ClientStatus(dict: dictionary)!
    }

    private func reasoningLiveModel(stale: Bool) -> LiveModelCatalogEntry {
        LiveModelCatalogEntry(dict: [
            "slug": model,
            "display_name": "GLM 5.2",
            "reasoning": [
                "supported": true,
                "options": [["type": "toggle"]],
                "source": "models_dev",
                "loaded_from": "runtime_cache",
                "revision": "sha256:catalog",
                "captured_at": "2026-07-25T18:00:00Z",
                "stale": stale,
            ],
        ])!
    }

    private func routingSnapshot(client: ClientStatus) -> RoutingSnapshot {
        let providerOptions = client.modelOptions["baseten"] ?? [:]
        return RoutingSnapshot(
            status: AdminStatusSnapshot(dict: [
                "router_boot_id": "boot-a",
                "active_generation": 4,
                "active_config_hash": "sha256:active",
                "desired_config_hash": "sha256:active",
                "health": "ready",
                "clients": [[
                    "name": client.name,
                    "enabled": client.enabled,
                    "model_options": [
                        "baseten": providerOptions.mapValues {
                            option in
                            guard let reasoning = option.reasoning else {
                                return [String: Any]()
                            }
                            return [
                                "reasoning": [
                                    "configured":
                                        reasoning.configured.jsonObject,
                                    "effective":
                                        reasoning.effective.jsonObject,
                                    "source": reasoning.source,
                                    "available_modes":
                                        reasoning.availableModes.map(\.rawValue),
                                    "available_efforts":
                                        reasoning.availableEfforts,
                                    "available": reasoning.available,
                                    "unavailable_reason":
                                        reasoning.unavailableReason,
                                    "error": reasoning.error,
                                ],
                            ]
                        },
                    ],
                ]],
            ]),
            observedAt: Date())
    }

    private func preflightSnapshot() -> ReasoningPreflightSnapshot {
        ReasoningPreflightSnapshot(dict: [
            "provider": "baseten",
            "model": model,
            "policy": ["mode": "on"],
            "available": true,
            "error": "",
            "warning": "",
            "clients": [
                preflightClient(
                    name: "claude-code",
                    supported: true),
            ],
        ])!
    }

    private func preflightClient(
        name: String,
        supported: Bool
    ) -> [String: Any] {
        [
            "name": name,
            "enabled": true,
            "reachable": true,
            "supported": supported,
            "reachability": ["family:opus"],
            "failure_behaviors": supported
                ? []
                : ["native_fallback", "local_error"],
            "available_modes": supported
                ? ["on", "off", "follow_harness"]
                : ["off"],
            "available_efforts": [],
            "unavailable_reason": supported ? "" : "adapter_unsupported",
            "error": "",
        ]
    }

    @MainActor
    private func makeState(
        preflight: FixedReasoningPreflightReader,
        runner: any CLIRunning,
        clientName: String = "claude-code",
        protocolShape: String = "anthropic"
    ) -> BasetenSwitchState {
        let status = AdminStatusSnapshot(dict: [
            "router_boot_id": "boot-a",
            "active_generation": 4,
            "active_config_hash": "sha256:active",
            "desired_config_hash": "sha256:active",
            "health": "ready",
            "global_routing_enabled": true,
            "clients": [[
                "name": clientName,
                "enabled": true,
                "bind_addr": "127.0.0.1:8789",
                "protocol_shape": protocolShape,
                "model_options": [
                    "baseten": [
                        model: [
                            "reasoning": reasoningStatusDictionary(
                                configured: ["mode": "default"],
                                effective: ["mode": "off"],
                                availableModes: [
                                    "off", "on", "follow_harness",
                                ],
                                availableEfforts: []),
                        ],
                    ],
                ],
            ]],
        ])
        let variant = AppVariant.resolve(
            infoDictionary: [
                "BasetenSwitchBuildChannel": "stable",
                "CFBundleDisplayName": "Baseten Switch",
                "CFBundleExecutable": "BasetenSwitch",
            ],
            bundleIdentifier: "co.baseten.switch",
            runningExecutableName: "BasetenSwitch",
            homeDirectory: "/tmp/baseten-switch-reasoning-tests",
            environment: ["BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true"])
        return BasetenSwitchState(
            variant: variant,
            reader: ReasoningAdminReader(status: status),
            reasoningPreflightReader: preflight,
            cliRunner: runner,
            loginItemService: ReasoningLoginItemService(),
            startPolling: false)
    }

    @MainActor
    private func waitUntil(
        _ predicate: @MainActor () -> Bool
    ) async {
        for _ in 0..<100 {
            if predicate() {
                return
            }
            try? await Task.sleep(nanoseconds: 10_000_000)
        }
        XCTFail("Timed out waiting for state change")
    }
}
