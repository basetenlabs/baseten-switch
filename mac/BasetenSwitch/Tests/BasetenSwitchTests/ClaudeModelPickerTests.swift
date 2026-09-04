import Foundation
import ServiceManagement
import XCTest
@testable import BasetenSwitch

private actor PickerSequencedAdminReader: AdminStatusReading {
    private var snapshots: [AdminStatusSnapshot]

    init(_ snapshots: [AdminStatusSnapshot]) {
        self.snapshots = snapshots
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        guard !snapshots.isEmpty else {
            throw GatewayClientError.invalidPayload
        }
        if snapshots.count == 1 { return snapshots[0] }
        return snapshots.removeFirst()
    }

    func fetchStats(
        windowSeconds: Int,
        bucketSeconds: Int
    ) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }
}

private actor PickerCatalogReader: ModelCatalogReading {
    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        LiveModelCatalogSnapshot(dict: [
            "state": "ready",
            "signed_out_reason": "",
            "models": [[
                "slug": "org/b",
                "display_name": "B",
            ]],
            "fetched_at": "2026-08-29T00:00:00Z",
            "error": "",
        ])!
    }
}

private actor PickerMutationRunner: CLIRunning {
    private(set) var requests: [CLIExecutionRequest] = []

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        requests.append(request)
        if request.arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]) {
            return pickerDiagnosticsResult()
        }
        if request.arguments.contains("--dry-run") {
            let addPreview: [String: Any] = [
                "alias": "claude-baseten-b",
                "slug": "org/b",
                "label": "B via Baseten",
                "description": "Served by Baseten.",
            ]
            let object: [String: Any]
            if request.arguments.contains("enable") {
                object = ["models": [
                    [
                        "alias": "claude-baseten-a",
                        "slug": "org/a",
                        "label": "A via Baseten",
                        "description": "Served by Baseten.",
                    ],
                    addPreview,
                ]]
            } else {
                object = addPreview
            }
            let data = try! JSONSerialization.data(withJSONObject: object)
            return CLIExecutionResult(
                status: 0,
                standardOutput: String(decoding: data, as: UTF8.self),
                standardError: "",
                timedOut: false)
        }
        let operationID = argumentValue("--operation-id", request.arguments)
            ?? ""
        let removing = request.arguments.contains("remove")
        let target = request.arguments.last ?? ""
        let configHash = removing
            ? "sha256:picker-removed"
            : "sha256:picker-updated"
        let object: [String: Any] = [
            "ok": true,
            "operation_id": operationID,
            "operation": removing
                ? "remove_claude_picker_model"
                : "add_claude_picker_model",
            "client": "claude-code",
            "key": "model_picker",
            "requested_target": target,
            "desired_config_hash": configHash,
            "active_config_hash": configHash,
            "active_token": removing ? "picker-boot:3" : "picker-boot:2",
            "applied": true,
            "reconciliation_required": false,
            "outcome": "applied",
            "warnings": removing
                ? ["The saved default still references this alias."]
                : [],
            "error": NSNull(),
        ]
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: 0,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(
        _ flag: String,
        _ arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

private actor PickerAmbiguousAliasRunner: CLIRunning {
    private(set) var requests: [CLIExecutionRequest] = []

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        requests.append(request)
        if request.arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]) {
            return pickerDiagnosticsResult()
        }
        if request.arguments.contains("--dry-run") {
            if let alias = argumentValue("--alias", request.arguments) {
                return jsonResult(status: 0, object: [
                    "alias": alias,
                    "slug": "org/model",
                    "label": "Model via Baseten",
                    "description": "Served by Baseten.",
                ])
            }
            return jsonResult(status: 1, object: [
                "ok": false,
                "slug": "org/model",
                "alias_choices": [
                    [
                        "alias": "claude-baseten-a",
                        "slug": "org/model",
                        "label": "Model via Baseten",
                        "description": "Served by Baseten.",
                    ],
                    [
                        "alias": "claude-baseten-b",
                        "slug": "org/model",
                        "label": "Model via Baseten",
                        "description": "Served by Baseten.",
                    ],
                ],
                "error": [
                    "code": "ambiguous_alias",
                    "message": "select one",
                    "retryable": false,
                ],
            ])
        }

        let operationID = argumentValue("--operation-id", request.arguments)
            ?? ""
        return jsonResult(status: 0, object: [
            "ok": true,
            "operation_id": operationID,
            "operation": "add_claude_picker_model",
            "client": "claude-code",
            "key": "model_picker",
            "requested_target":
                "org/model via claude-baseten-b",
            "desired_config_hash": "sha256:picker-ambiguous-updated",
            "active_config_hash": "sha256:picker-ambiguous-updated",
            "active_token": "picker-boot:2",
            "applied": true,
            "reconciliation_required": false,
            "outcome": "applied",
            "warnings": [],
            "error": NSNull(),
        ])
    }

    private func jsonResult(
        status: Int32,
        object: [String: Any]
    ) -> CLIExecutionResult {
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: status,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(
        _ flag: String,
        _ arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

private actor PickerPreviewFailureRunner: CLIRunning {
    private let malformed: Bool

    init(malformed: Bool = false) {
        self.malformed = malformed
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        guard request.arguments.contains("--dry-run") else {
            return CLIExecutionResult(
                status: 1,
                standardOutput: "",
                standardError: "unexpected command",
                timedOut: false)
        }
        if malformed {
            return CLIExecutionResult(
                status: 1,
                standardOutput: "not json",
                standardError: "",
                timedOut: false)
        }
        let data = try! JSONSerialization.data(withJSONObject: [
            "ok": false,
            "error": [
                "code": ClaudeModelPickerContextMinimumFailure.code,
                "message": "Choose a model with at least 200000 tokens.",
                "retryable": false,
            ],
        ])
        return CLIExecutionResult(
            status: 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }
}

private actor PickerMutationContextFailureRunner: CLIRunning {
    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        if request.arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]) {
            return pickerDiagnosticsResult()
        }
        let operationID = argumentValue(
            "--operation-id",
            request.arguments) ?? ""
        let data = try! JSONSerialization.data(withJSONObject: [
            "ok": false,
            "operation_id": operationID,
            "operation": "add_claude_picker_model",
            "client": "claude-code",
            "key": "model_picker",
            "requested_target": "org/b",
            "applied": false,
            "reconciliation_required": false,
            "error": [
                "code": ClaudeModelPickerContextMinimumFailure.code,
                "message": "Choose a model with at least 200000 tokens.",
                "retryable": false,
            ],
        ])
        return CLIExecutionResult(
            status: 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(
        _ flag: String,
        _ arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

private actor PickerSettingsRecoveryRunner: CLIRunning {
    private(set) var requests: [CLIExecutionRequest] = []

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        requests.append(request)
        if request.arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]) {
            return pickerDiagnosticsResult()
        }
        let operationID = argumentValue(
            "--operation-id",
            request.arguments) ?? ""
        let syncing = request.arguments.suffix(3).elementsEqual([
            "claude", "picker", "sync",
        ])
        let operation = syncing
            ? "sync_claude_picker"
            : "add_claude_picker_model"
        let object: [String: Any] = [
            "ok": syncing,
            "operation_id": operationID,
            "operation": operation,
            "client": "claude-code",
            "key": "model_picker",
            "requested_target": syncing ? "" : "org/b",
            "desired_config_hash": "sha256:picker-updated",
            "active_config_hash": "sha256:picker-updated",
            "active_token": "picker-boot:2",
            "applied": true,
            "reconciliation_required": !syncing,
            "reconciliation_action": syncing ? "" : "claude_picker_sync",
            "outcome": syncing ? "applied" : "settings_sync_pending",
            "error": syncing ? NSNull() : [
                "code": "settings_sync_failed",
                "message": "settings sync pending",
                "retryable": true,
            ],
        ]
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: syncing ? 0 : 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(
        _ flag: String,
        _ arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

private actor PickerManualResolutionRunner: CLIRunning {
    private(set) var requests: [CLIExecutionRequest] = []
    private static let fingerprint =
        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        requests.append(request)
        if request.arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]) {
            return pickerDiagnosticsResult(userFileSync: "blocked")
        }
        let operationID = argumentValue(
            "--operation-id",
            request.arguments)
            ?? request.arguments.last
            ?? ""
        let reconciling = request.arguments.contains("reconcile")
        let object: [String: Any] = [
            "ok": reconciling,
            "operation_id": operationID,
            "operation": "add_claude_picker_model",
            "client": "claude-code",
            "key": "model_picker",
            "requested_target": "org/b",
            "desired_config_hash": "sha256:picker-updated",
            "active_config_hash": reconciling
                ? "sha256:picker-updated"
                : "sha256:picker-initial",
            "active_token": reconciling ? "picker-boot:2" : "picker-boot:1",
            "applied": reconciling,
            "reconciliation_required": !reconciling,
            "reconciliation_action": reconciling
                ? ""
                : "mutation_reconcile_then_manual_claude_settings_resolution",
            "outcome": reconciling ? "applied" : "manual_resolution_required",
            "identity_strength": "exact",
            "request_fingerprint": Self.fingerprint,
            "error": reconciling ? NSNull() : [
                "code": "manual_resolution_required",
                "message": "Claude settings require manual resolution",
                "retryable": false,
            ],
        ]
        let data = try! JSONSerialization.data(withJSONObject: object)
        return CLIExecutionResult(
            status: reconciling ? 0 : 1,
            standardOutput: String(decoding: data, as: UTF8.self),
            standardError: "",
            timedOut: false)
    }

    private func argumentValue(
        _ flag: String,
        _ arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}

private func pickerDiagnosticsResult(
    userFileSync: String = "synced"
) -> CLIExecutionResult {
    let data = try! JSONSerialization.data(withJSONObject: [
        "enabled": true,
        "configuration": "enabled",
        "user_file_sync": userFileSync,
        "known_policy": "no_known_conflict",
        "allowlist_policy": "no_known_conflict",
        "managed_policy": "unverified",
        "replacement_mode": "append",
        "runtime_verification": "unverified",
        "configured_rows": 2,
        "installed_rows": 2,
        "settings_path": "/tmp/claude/settings.json",
        "legacy_discovery_enabled": false,
    ])
    return CLIExecutionResult(
        status: 0,
        standardOutput: String(decoding: data, as: UTF8.self),
        standardError: "",
        timedOut: false)
}

@MainActor
private final class PickerLoginItemService: LoginItemServicing {
    var status: SMAppService.Status = .notRegistered
    func reconcileAtLaunch() {}
    func toggle() {}
    func openSystemSettings() {}
}

final class ClaudeModelPickerTests: XCTestCase {
    func testClientStatusDecodesOrderedAliasOnlyPickerProjection() throws {
        let client = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code",
            "model_picker": [
                "enabled": true,
                "models": [
                    [
                        "alias": "claude-baseten-kimi-k2-5",
                        "slug": "moonshotai/Kimi-K2.5",
                        "label": "Kimi K2.5 via Baseten",
                        "description": "Served by Baseten.",
                        "context_tokens": 1_048_576,
                    ],
                    [
                        "alias": "claude-baseten-glm-5-2",
                        "slug": "zai-org/GLM-5.2",
                        "label": "GLM 5.2 via Baseten",
                        "description": "Served by Baseten.",
                        "context_tokens": 200_000,
                    ],
                ],
            ],
        ]))

        XCTAssertEqual(client.modelPicker?.enabled, true)
        XCTAssertEqual(client.modelPicker?.models.map(\.alias), [
            "claude-baseten-kimi-k2-5",
            "claude-baseten-glm-5-2",
        ])
        XCTAssertEqual(client.modelPicker?.models.map(\.contextTokens), [
            1_048_576,
            200_000,
        ])
        XCTAssertEqual(
            client.modelPicker?.models.last?.label,
            "GLM 5.2 via Baseten")
    }

    func testPickerDiagnosticsDecodeIndependentStatusAxes() throws {
        let diagnostics = try XCTUnwrap(ClaudeModelPickerDiagnostics(json: """
        {
          "enabled": true,
          "configuration": "enabled",
          "user_file_sync": "out_of_sync",
          "known_policy": "possible_allowlist_conflict",
          "allowlist_policy": "possible_conflict",
          "managed_policy": "unverified",
          "replacement_mode": "replace",
          "runtime_verification": "unverified",
          "configured_rows": 3,
          "installed_rows": 2,
          "settings_path": "/private/path/not-presented",
          "legacy_discovery_enabled": true,
          "saved_model": "claude-baseten-old",
          "saved_model_unconfigured": true,
          "saved_model_context_mismatch": true,
          "message": "Configured rows are not all installed."
        }
        """))

        XCTAssertTrue(diagnostics.enabled)
        XCTAssertEqual(diagnostics.userFileSync, "out_of_sync")
        XCTAssertEqual(diagnostics.configuration, "enabled")
        XCTAssertEqual(
            diagnostics.knownPolicy,
            "possible_allowlist_conflict")
        XCTAssertEqual(diagnostics.runtimeVerification, "unverified")
        XCTAssertEqual(diagnostics.allowlistPolicy, "possible_conflict")
        XCTAssertEqual(diagnostics.managedPolicy, "unverified")
        XCTAssertEqual(diagnostics.replacementMode, "replace")
        XCTAssertEqual(diagnostics.configuredRows, 3)
        XCTAssertEqual(diagnostics.installedRows, 2)
        XCTAssertTrue(diagnostics.legacyDiscoveryEnabled)
        XCTAssertEqual(diagnostics.savedModel, "claude-baseten-old")
        XCTAssertTrue(diagnostics.savedModelUnconfigured)
        XCTAssertTrue(diagnostics.savedModelContextMismatch)
        XCTAssertEqual(
            claudeModelPickerDiagnosticLabel(
                key: "runtime",
                value: diagnostics.runtimeVerification),
            "Unverified")
    }

    func testConfiguredRowStatePrefersAllowlistConflictOtherwiseRuntime()
    {
        XCTAssertEqual(
            claudeModelPickerConfiguredRowState(
                allowlistPolicy: "possible_conflict",
                knownPolicy: "possible_allowlist_conflict"),
            "Possible allowlist conflict")
        XCTAssertEqual(
            claudeModelPickerConfiguredRowState(
                allowlistPolicy: "no_known_conflict",
                knownPolicy: "no_known_conflict"),
            "Runtime unverified")
    }

    func testRetrySyncRequiresActionableOutOfSyncConfiguredPicker() {
        XCTAssertTrue(claudeModelPickerCanRetrySync(
            userFileSync: "out_of_sync",
            canEditConfiguredRows: true,
            hasConfiguredPicker: true))
        XCTAssertFalse(claudeModelPickerCanRetrySync(
            userFileSync: "synced",
            canEditConfiguredRows: true,
            hasConfiguredPicker: true))
        XCTAssertFalse(claudeModelPickerCanRetrySync(
            userFileSync: "blocked",
            canEditConfiguredRows: true,
            hasConfiguredPicker: true))
        XCTAssertFalse(claudeModelPickerCanRetrySync(
            userFileSync: "out_of_sync",
            canEditConfiguredRows: false,
            hasConfiguredPicker: true))
        XCTAssertFalse(claudeModelPickerCanRetrySync(
            userFileSync: "out_of_sync",
            canEditConfiguredRows: true,
            hasConfiguredPicker: false))
    }

    func testMalformedPickerProjectionFailsClosedWithoutDroppingRows() throws {
        let client = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code",
            "model_picker": [
                "enabled": true,
                "models": [
                    [
                        "alias": "claude-baseten-glm-5-2",
                        "slug": "zai-org/GLM-5.2",
                    ],
                    ["alias": "missing-slug"],
                ],
            ],
        ]))

        XCTAssertNil(client.modelPicker)
    }

    func testProjectionSubtractsSelectedSlugsDeduplicatesAndSorts() throws {
        let status = try pickerStatus([
            pickerRow(
                alias: "claude-baseten-glm-5-2",
                slug: "zai-org/GLM-5.2",
                label: "GLM 5.2 via Baseten"),
        ])
        let projection = projectClaudeModelPicker(
            status: status,
            liveState: .ready([
                liveModel("zai-org/GLM-5.2", "GLM 5.2"),
                liveModel("zeta/Zed", "Zed"),
                liveModel("alpha/Alpha", "Alpha"),
                liveModel("alpha/Alpha", "Duplicate Alpha"),
            ]))

        XCTAssertEqual(projection.configured.map(\.slug), [
            "zai-org/GLM-5.2",
        ])
        XCTAssertEqual(projection.availableToAdd.map(\.slug), [
            "alpha/Alpha",
            "zeta/Zed",
        ])
    }

    func testOfflineProjectionKeepsConfiguredRowsAndDisablesAdditions()
        throws {
        let status = try pickerStatus([
            pickerRow(
                alias: "claude-baseten-glm-5-2",
                slug: "zai-org/GLM-5.2",
                label: "GLM 5.2 via Baseten"),
        ])

        for state in [
            LiveModelCatalogLoadState.idle,
            .loading,
            .signedOut(.notSignedIn),
            .error("unavailable"),
        ] {
            let projection = projectClaudeModelPicker(
                status: status,
                liveState: state)
            XCTAssertEqual(projection.configured, status.models)
            XCTAssertTrue(projection.availableToAdd.isEmpty)
        }
    }

    func testAddPreviewDecodesCoreOwnedExactPresentation() throws {
        let preview = try XCTUnwrap(ClaudeModelPickerAddPreview(json: """
        {
          "alias": "claude-baseten-glm-5-2",
          "slug": "zai-org/GLM-5.2",
          "label": "GLM 5.2 via Baseten",
          "description": "Served by Baseten."
        }
        """))

        XCTAssertEqual(preview.alias, "claude-baseten-glm-5-2")
        XCTAssertEqual(preview.slug, "zai-org/GLM-5.2")
        XCTAssertEqual(preview.label, "GLM 5.2 via Baseten")
        XCTAssertEqual(preview.description, "Served by Baseten.")
        XCTAssertNil(ClaudeModelPickerAddPreview(
            json: "{\"slug\":\"zai-org/GLM-5.2\"}"))
    }

    func testExactAddAndRemoveActionsMapDirectlyToMutations() throws {
        let preview = try XCTUnwrap(ClaudeModelPickerAddPreview(json: """
        {
          "alias": "claude-baseten-choice",
          "slug": "org/model",
          "label": "Model via Baseten",
          "description": "Served by Baseten."
        }
        """))

        XCTAssertEqual(
            claudeModelPickerAddMutation(
                preview: preview,
                explicitAlias: "claude-baseten-choice"),
            .add(
                slug: "org/model",
                alias: "claude-baseten-choice"))
        XCTAssertEqual(
            claudeModelPickerRemoveMutation(alias: "claude-baseten-choice"),
            .remove(alias: "claude-baseten-choice"))
    }

    func testRestartNoticeCopyIsExact() {
        XCTAssertEqual(
            claudeModelPickerRestartNotice,
            "Restart Claude Code to see additions or removals.")
    }

    func testAddPreviewDecodesStructuredAmbiguousAliasChoices() throws {
        let outcome = try XCTUnwrap(ClaudeModelPickerAddPreviewOutcome(
            json: """
            {
              "ok": false,
              "slug": "org/model",
              "alias_choices": [
                {
                  "alias": "claude-baseten-a",
                  "slug": "org/model",
                  "label": "Model via Baseten",
                  "description": "Served by Baseten."
                },
                {
                  "alias": "claude-baseten-b",
                  "slug": "org/model",
                  "label": "Model via Baseten",
                  "description": "Served by Baseten."
                }
              ],
              "error": {
                "code": "ambiguous_alias",
                "message": "select one",
                "retryable": false
              }
            }
            """,
            status: 1,
            explicitAlias: nil))

        guard case .aliasChoices(let slug, let choices) = outcome else {
            return XCTFail("expected structured alias choices")
        }
        XCTAssertEqual(slug, "org/model")
        XCTAssertEqual(
            choices.map(\.alias),
            ["claude-baseten-a", "claude-baseten-b"])
        XCTAssertNil(ClaudeModelPickerAddPreviewOutcome(
            json: "{\"ok\":false,\"error\":{\"code\":\"ambiguous_alias\"}}",
            status: 1,
            explicitAlias: nil))
    }

    func testEnablePreviewPreservesCoreOwnedExactOrderAndPresentation()
        throws {
        let preview = try XCTUnwrap(ClaudeModelPickerEnablePreview(json: """
        {
          "models": [
            {
              "alias": "claude-baseten-z",
              "slug": "org/z",
              "label": "Z via Baseten",
              "description": "Served by Baseten."
            },
            {
              "alias": "claude-baseten-a",
              "slug": "org/a",
              "label": "A via Baseten",
              "description": "Served by Baseten."
            }
          ]
        }
        """))

        XCTAssertEqual(
            preview.models.map(\.alias),
            ["claude-baseten-z", "claude-baseten-a"])
        XCTAssertEqual(preview.models.first?.label, "Z via Baseten")
        XCTAssertNil(ClaudeModelPickerEnablePreview(
            json: "{\"models\":[]}"))
    }

    func testMoveCommandsSupportBothDirectionsIncludingFinalPosition()
        throws {
        let rows = [
            pickerRow(alias: "a", slug: "org/a", label: "A"),
            pickerRow(alias: "b", slug: "org/b", label: "B"),
            pickerRow(alias: "c", slug: "org/c", label: "C"),
        ]

        XCTAssertEqual(claudeModelPickerMoveUpCommand(rows: rows, index: 1), [
            "move", "b", "--before", "a",
        ])
        XCTAssertEqual(claudeModelPickerMoveDownCommand(
            rows: rows,
            index: 1), [
                "move", "c", "--before", "b",
            ])
        XCTAssertNil(claudeModelPickerMoveUpCommand(rows: rows, index: 0))
        XCTAssertNil(claudeModelPickerMoveDownCommand(rows: rows, index: 2))
    }

    func testMutationCommandsAndReceiptOperationsMatchCLIContract() {
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.enable(
                convertReplacementMode: false).command,
            ["claude", "picker", "enable"])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.add(
                slug: "org/model",
                alias: nil).command,
            ["claude", "picker", "add", "org/model"])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.add(
                slug: "org/model",
                alias: "claude-baseten-model").command,
            [
                "claude", "picker", "add", "org/model",
                "--alias", "claude-baseten-model",
            ])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.add(
                slug: "org/model",
                alias: "claude-baseten-model").requestedTarget,
            "org/model via claude-baseten-model")
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.remove(alias: "alias").command,
            ["claude", "picker", "remove", "alias"])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.move(
                alias: "b",
                before: "a").command,
            ["claude", "picker", "move", "b", "--before", "a"])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.move(
                alias: "b",
                before: "a").requestedTarget,
            "b before a")
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.sync(
                convertReplacementMode: false).command,
            ["claude", "picker", "sync"])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.enable(
                convertReplacementMode: true).command,
            [
                "claude", "picker", "enable",
                "--convert-replacement-mode",
            ])
        XCTAssertEqual(
            ClaudeModelPickerMutationKind.add(
                slug: "org/model",
                alias: nil)
                .receiptOperation,
            "add_claude_picker_model")
    }

    func testReceiptDecodesReconcileThenManualSettingsAction() throws {
        let receipt = try XCTUnwrap(GlobalMutationReceipt(json: """
        {
          "ok": false,
          "operation_id": "op-manual",
          "operation": "remove_claude_picker_model",
          "client": "claude-code",
          "key": "model_picker",
          "requested_target": "claude-baseten-model",
          "applied": false,
          "reconciliation_required": true,
          "reconciliation_action": "mutation_reconcile_then_manual_claude_settings_resolution",
          "outcome": "manual_resolution_required",
          "warnings": [
            "The saved default still references this alias."
          ],
          "error": {
            "code": "manual_resolution_required",
            "message": "manual resolution required",
            "retryable": false
          }
        }
        """))

        XCTAssertEqual(
            receipt.reconciliationAction,
            "mutation_reconcile_then_manual_claude_settings_resolution")
        XCTAssertTrue(receipt.reconciliationRequired)
        XCTAssertFalse(receipt.applied)
        XCTAssertEqual(receipt.errorCode, "manual_resolution_required")
        XCTAssertFalse(receipt.errorRetryable)
        XCTAssertEqual(receipt.warnings, [
            "The saved default still references this alias.",
        ])
    }

    func testMutationConfirmationUsesActiveOrderedProjection() throws {
        let before = try pickerStatus([
            pickerRow(alias: "a", slug: "org/a", label: "A"),
            pickerRow(alias: "b", slug: "org/b", label: "B"),
        ])
        let afterMove = try pickerStatus([
            pickerRow(alias: "b", slug: "org/b", label: "B"),
            pickerRow(alias: "a", slug: "org/a", label: "A"),
        ])

        XCTAssertTrue(claudeModelPickerMutationConfirmed(
            .move(alias: "b", before: "a"),
            status: afterMove))
        XCTAssertFalse(claudeModelPickerMutationConfirmed(
            .move(alias: "b", before: "a"),
            status: before))
        XCTAssertTrue(claudeModelPickerMutationConfirmed(
            .remove(alias: "b"),
            status: try pickerStatus([
                pickerRow(alias: "a", slug: "org/a", label: "A"),
            ])))
        XCTAssertFalse(claudeModelPickerMutationConfirmed(
            .add(slug: "org/c", alias: nil),
            status: afterMove))
    }

    @MainActor
    func testContextMinimumFailureUsesActionableMessageForAddAndEnablePreviews()
        async throws {
        let state = previewFailureState(runner: PickerPreviewFailureRunner())

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()

        let addPreview = await state.previewClaudeModelPickerAdd(slug: "org/b")
        XCTAssertNil(addPreview)
        XCTAssertEqual(
            state.lastError,
            "Choose a model with at least 200000 tokens.")
        XCTAssertEqual(
            claudeModelPickerAddPreviewErrorMessage(state.lastError),
            "Choose a model with at least 200000 tokens.")

        let enablePreview = await state.previewClaudeModelPickerEnable()
        XCTAssertNil(enablePreview)
        XCTAssertEqual(
            state.lastError,
            "Choose a model with at least 200000 tokens.")
        state.stop()
    }

    func testAddPreviewInlineErrorFallsBackWhenStateHasNoMessage() {
        XCTAssertEqual(
            claudeModelPickerAddPreviewErrorMessage(nil),
            "Switch could not generate an exact preview for this model. No changes were made.")
        XCTAssertEqual(
            claudeModelPickerAddPreviewErrorMessage("  \n  "),
            "Switch could not generate an exact preview for this model. No changes were made.")
    }

    @MainActor
    func testMalformedPreviewFailureUsesGenericMessages() async throws {
        let state = previewFailureState(
            runner: PickerPreviewFailureRunner(malformed: true))

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()

        let addPreview = await state.previewClaudeModelPickerAdd(slug: "org/b")
        XCTAssertNil(addPreview)
        XCTAssertEqual(
            state.lastError,
            "Switch could not generate a safe model picker preview.")

        let enablePreview = await state.previewClaudeModelPickerEnable()
        XCTAssertNil(enablePreview)
        XCTAssertEqual(
            state.lastError,
            "Switch could not generate a safe model picker setup preview.")
        state.stop()
    }

    @MainActor
    func testMutationTimeContextMinimumFailureUsesActionableMessage()
        async throws {
        let state = previewFailureState(
            runner: PickerMutationContextFailureRunner())

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()
        await state.setClaudeModelPicker(.add(slug: "org/b", alias: nil))

        XCTAssertEqual(
            state.lastError,
            "Choose a model with at least 200000 tokens.")
        XCTAssertNil(state.claudeModelPickerNotice)
        XCTAssertNil(state.pendingClaudeModelPicker)
        state.stop()
    }

    @MainActor
    func testExactPreviewDispatchesAddThenDirectRemovePreservesWarnings()
        async throws {
        let initial = adminStatus(
            generation: 1,
            hash: "sha256:picker-initial",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
            ])
        let updated = adminStatus(
            generation: 2,
            hash: "sha256:picker-updated",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
                pickerRow(alias: "b", slug: "org/b", label: "B"),
            ])
        let removed = adminStatus(
            generation: 3,
            hash: "sha256:picker-removed",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
            ])
        let reader = PickerSequencedAdminReader([initial, updated, removed])
        let runner = PickerMutationRunner()
        let state = BasetenSwitchState(
            variant: AppVariant.resolve(
                infoDictionary: [:],
                homeDirectory: "/tmp/baseten-switch-picker-tests",
                environment: [
                    "BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true",
                    "BASETEN_SWITCH_CONFIG_PATH": "/tmp/picker-gateway.yaml",
                ]),
            reader: reader,
            modelCatalogReader: PickerCatalogReader(),
            cliRunner: runner,
            loginItemService: PickerLoginItemService(),
            startPolling: false)

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()
        XCTAssertTrue(state.canMutateClaudeModelPicker)

        let enablePreview = await state.previewClaudeModelPickerEnable()
        XCTAssertEqual(enablePreview?.models.map(\.alias), [
            "claude-baseten-a", "claude-baseten-b",
        ])

        let outcome = await state.previewClaudeModelPickerAdd(slug: "org/b")
        guard case .preview(let preview, let explicitAlias) = outcome else {
            return XCTFail("expected an exact preview")
        }
        XCTAssertEqual(preview.alias, "claude-baseten-b")
        XCTAssertEqual(preview.label, "B via Baseten")
        XCTAssertNil(explicitAlias)

        await state.setClaudeModelPicker(.add(slug: "org/b", alias: nil))

        let requests = await runner.requests
        XCTAssertEqual(requests.count, 4)
        XCTAssertEqual(requests[0].arguments.suffix(5), [
            "claude", "picker", "enable", "--dry-run", "--json",
        ])
        XCTAssertEqual(requests[1].arguments.suffix(6), [
            "claude", "picker", "add", "org/b", "--dry-run", "--json",
        ])
        let request = requests[2]
        XCTAssertEqual(
            argumentValue("--if-active-token", request.arguments),
            "picker-boot:1")
        XCTAssertEqual(
            argumentValue("--if-config-hash", request.arguments),
            "sha256:picker-initial")
        XCTAssertTrue(request.arguments.suffix(4).elementsEqual([
            "claude", "picker", "add", "org/b",
        ]))
        XCTAssertTrue(requests[3].arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]))
        XCTAssertEqual(
            state.claudeModelPickerDiagnostics?.userFileSync,
            "synced")
        XCTAssertNil(state.pendingClaudeModelPicker)
        XCTAssertNil(state.lastError)
        XCTAssertEqual(
            state.clients.first?.modelPicker?.models.map(\.alias),
            ["a", "b"])
        XCTAssertEqual(state.claudeModelPickerNotice, "Added to /model.")
        XCTAssertTrue(state.claudeModelPickerWarnings.isEmpty)

        await state.setClaudeModelPicker(.remove(alias: "b"))

        let requestsAfterRemove = await runner.requests
        XCTAssertEqual(requestsAfterRemove.count, 6)
        XCTAssertTrue(requestsAfterRemove[4].arguments.suffix(4)
            .elementsEqual([
                "claude", "picker", "remove", "b",
            ]))
        XCTAssertTrue(requestsAfterRemove[5].arguments.suffix(4)
            .elementsEqual([
                "claude", "picker", "status", "--json",
            ]))
        XCTAssertEqual(
            state.clients.first?.modelPicker?.models.map(\.alias),
            ["a"])
        XCTAssertEqual(
            state.claudeModelPickerNotice,
            "Saved. The routing alias remains available to existing sessions.")
        XCTAssertEqual(state.claudeModelPickerWarnings, [
            "The saved default still references this alias.",
        ])
        state.stop()
    }

    @MainActor
    func testAmbiguousAliasSelectionRepeatsPreviewWithAliasThenAdds()
        async throws {
        let initial = adminStatus(
            generation: 1,
            hash: "sha256:picker-ambiguous-initial",
            rows: [
                pickerRow(alias: "existing", slug: "org/existing", label: "Existing"),
            ])
        let updated = adminStatus(
            generation: 2,
            hash: "sha256:picker-ambiguous-updated",
            rows: [
                pickerRow(alias: "existing", slug: "org/existing", label: "Existing"),
                pickerRow(
                    alias: "claude-baseten-b",
                    slug: "org/model",
                    label: "Model"),
            ])
        let reader = PickerSequencedAdminReader([initial, updated])
        let runner = PickerAmbiguousAliasRunner()
        let state = BasetenSwitchState(
            variant: AppVariant.resolve(
                infoDictionary: [:],
                homeDirectory: "/tmp/baseten-switch-picker-alias-tests",
                environment: [
                    "BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true",
                    "BASETEN_SWITCH_CONFIG_PATH": "/tmp/picker-gateway.yaml",
                ]),
            reader: reader,
            modelCatalogReader: PickerCatalogReader(),
            cliRunner: runner,
            loginItemService: PickerLoginItemService(),
            startPolling: false)

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()

        let ambiguous = await state.previewClaudeModelPickerAdd(
            slug: "org/model")
        guard case .aliasChoices(let slug, let choices) = ambiguous else {
            return XCTFail("expected alias choices")
        }
        XCTAssertEqual(slug, "org/model")
        XCTAssertEqual(choices.map(\.alias), [
            "claude-baseten-a", "claude-baseten-b",
        ])

        let exact = await state.previewClaudeModelPickerAdd(
            slug: slug,
            alias: choices[1].alias)
        guard case .preview(let preview, let explicitAlias) = exact else {
            return XCTFail("expected exact preview after alias choice")
        }
        XCTAssertEqual(explicitAlias, "claude-baseten-b")
        let mutation = claudeModelPickerAddMutation(
            preview: preview,
            explicitAlias: explicitAlias)
        XCTAssertEqual(
            mutation,
            .add(slug: "org/model", alias: "claude-baseten-b"))

        await state.setClaudeModelPicker(mutation)

        let requests = await runner.requests
        XCTAssertEqual(requests.count, 4)
        XCTAssertEqual(requests[0].arguments.suffix(6), [
            "claude", "picker", "add", "org/model", "--dry-run", "--json",
        ])
        XCTAssertEqual(requests[1].arguments.suffix(8), [
            "claude", "picker", "add", "org/model", "--dry-run", "--json",
            "--alias", "claude-baseten-b",
        ])
        XCTAssertEqual(requests[2].arguments.suffix(6), [
            "claude", "picker", "add", "org/model", "--alias",
            "claude-baseten-b",
        ])
        XCTAssertTrue(requests[3].arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]))
        XCTAssertEqual(
            state.clients.first?.modelPicker?.models.map(\.alias),
            ["existing", "claude-baseten-b"])
        XCTAssertEqual(state.claudeModelPickerNotice, "Added to /model.")
        state.stop()
    }

    @MainActor
    func testSettingsPendingUsesPickerSyncInsteadOfGenericReconcile()
        async throws {
        let initial = adminStatus(
            generation: 1,
            hash: "sha256:picker-initial",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
            ])
        let updated = adminStatus(
            generation: 2,
            hash: "sha256:picker-updated",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
                pickerRow(alias: "b", slug: "org/b", label: "B"),
            ])
        let reader = PickerSequencedAdminReader([
            initial,
            updated,
            updated,
        ])
        let runner = PickerSettingsRecoveryRunner()
        let state = BasetenSwitchState(
            variant: AppVariant.resolve(
                infoDictionary: [:],
                homeDirectory: "/tmp/baseten-switch-picker-recovery-tests",
                environment: [
                    "BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true",
                    "BASETEN_SWITCH_CONFIG_PATH": "/tmp/picker-gateway.yaml",
                ]),
            reader: reader,
            modelCatalogReader: PickerCatalogReader(),
            cliRunner: runner,
            loginItemService: PickerLoginItemService(),
            startPolling: false)

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()
        await state.setClaudeModelPicker(.add(slug: "org/b", alias: nil))

        let requests = await runner.requests
        XCTAssertEqual(requests.count, 3)
        XCTAssertTrue(requests[0].arguments.suffix(4).elementsEqual([
            "claude", "picker", "add", "org/b",
        ]))
        XCTAssertTrue(requests[1].arguments.suffix(3).elementsEqual([
            "claude", "picker", "sync",
        ]))
        XCTAssertTrue(requests[2].arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]))
        XCTAssertFalse(requests.flatMap(\.arguments).contains("reconcile"))
        XCTAssertNil(state.lastError)
        XCTAssertEqual(state.claudeModelPickerNotice, "Added to /model.")
        state.stop()
    }

    @MainActor
    func testManualSettingsActionReconcilesRouterThenRemainsUnconfirmed()
        async throws {
        let initial = adminStatus(
            generation: 1,
            hash: "sha256:picker-initial",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
            ])
        let updated = adminStatus(
            generation: 2,
            hash: "sha256:picker-updated",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
                pickerRow(alias: "b", slug: "org/b", label: "B"),
            ])
        let reader = PickerSequencedAdminReader([
            initial,
            updated,
            updated,
        ])
        let runner = PickerManualResolutionRunner()
        let state = BasetenSwitchState(
            variant: AppVariant.resolve(
                infoDictionary: [:],
                homeDirectory: "/tmp/baseten-switch-picker-manual-tests",
                environment: [
                    "BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true",
                    "BASETEN_SWITCH_CONFIG_PATH": "/tmp/picker-gateway.yaml",
                ]),
            reader: reader,
            modelCatalogReader: PickerCatalogReader(),
            cliRunner: runner,
            loginItemService: PickerLoginItemService(),
            startPolling: false)

        await state.refresh()
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()
        await state.setClaudeModelPicker(.add(slug: "org/b", alias: nil))

        let requests = await runner.requests
        XCTAssertEqual(requests.count, 3)
        XCTAssertTrue(requests[0].arguments.suffix(4).elementsEqual([
            "claude", "picker", "add", "org/b",
        ]))
        XCTAssertTrue(requests[1].arguments.suffix(4).elementsEqual([
            "--json", "mutation", "reconcile",
            argumentValue("--operation-id", requests[0].arguments) ?? "",
        ]))
        XCTAssertTrue(requests[2].arguments.suffix(4).elementsEqual([
            "claude", "picker", "status", "--json",
        ]))
        XCTAssertFalse(requests.flatMap(\.arguments).contains("sync"))
        XCTAssertNil(state.claudeModelPickerNotice)
        XCTAssertTrue(
            state.lastError?.contains("manual resolution") == true)
        XCTAssertEqual(
            state.claudeModelPickerDiagnostics?.userFileSync,
            "blocked")
        state.stop()
    }

    private func pickerStatus(
        _ rows: [ClaudeModelPickerRow]
    ) throws -> ClaudeModelPickerStatus {
        try XCTUnwrap(ClaudeModelPickerStatus(dict: [
            "enabled": true,
            "models": rows.map {
                [
                    "alias": $0.alias,
                    "slug": $0.slug,
                    "label": $0.label,
                    "description": $0.description,
                ]
            },
        ]))
    }

    @MainActor
    private func previewFailureState(
        runner: any CLIRunning
    ) -> BasetenSwitchState {
        let initial = adminStatus(
            generation: 1,
            hash: "sha256:picker-preview-failure",
            rows: [
                pickerRow(alias: "a", slug: "org/a", label: "A"),
            ])
        return BasetenSwitchState(
            variant: AppVariant.resolve(
                infoDictionary: [:],
                homeDirectory: "/tmp/baseten-switch-picker-preview-failure-tests",
                environment: [
                    "BASETEN_SWITCH_GATEWAY_BIN": "/usr/bin/true",
                    "BASETEN_SWITCH_CONFIG_PATH": "/tmp/picker-gateway.yaml",
                ]),
            reader: PickerSequencedAdminReader([initial]),
            modelCatalogReader: PickerCatalogReader(),
            cliRunner: runner,
            loginItemService: PickerLoginItemService(),
            startPolling: false)
    }

    private func pickerRow(
        alias: String,
        slug: String,
        label: String
    ) -> ClaudeModelPickerRow {
        ClaudeModelPickerRow(dict: [
            "alias": alias,
            "slug": slug,
            "label": label,
            "description": "Served by Baseten.",
        ])!
    }

    private func liveModel(
        _ slug: String,
        _ displayName: String
    ) -> LiveModelCatalogEntry {
        LiveModelCatalogEntry(dict: [
            "slug": slug,
            "display_name": displayName,
        ])!
    }

    private func adminStatus(
        generation: Int,
        hash: String,
        rows: [ClaudeModelPickerRow]
    ) -> AdminStatusSnapshot {
        AdminStatusSnapshot(dict: [
            "router_boot_id": "picker-boot",
            "active_generation": generation,
            "active_config_hash": hash,
            "desired_config_hash": hash,
            "capabilities": ["global_routing"],
            "health": "ready",
            "config_path": "/tmp/picker-gateway.yaml",
            "reload": ["state": "ready", "error": ""],
            "global_routing_enabled": true,
            "clients": [[
                "name": "claude-code",
                "enabled": true,
                "protocol_shape": "anthropic",
                "model_picker": [
                    "enabled": true,
                    "models": rows.map {
                        [
                            "alias": $0.alias,
                            "slug": $0.slug,
                            "label": $0.label,
                            "description": $0.description,
                        ]
                    },
                ],
            ]],
        ])
    }

    private func argumentValue(
        _ flag: String,
        _ arguments: [String]
    ) -> String? {
        guard let index = arguments.firstIndex(of: flag),
              arguments.indices.contains(index + 1) else { return nil }
        return arguments[index + 1]
    }
}
