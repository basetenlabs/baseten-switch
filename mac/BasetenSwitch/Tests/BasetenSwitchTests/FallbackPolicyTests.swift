import XCTest
@testable import BasetenSwitch

final class FallbackPolicyTests: XCTestCase {
    func testAdminStatusDecodesResolvedPolicyAndClaudeTarget() throws {
        let status = AdminStatusSnapshot(dict: [
            "capabilities": ["global_routing", "fallback_policy"],
            "fallback_policy": [
                "on_baseten_429": true,
                "on_baseten_5xx": false,
            ],
            "clients": [[
                "name": "claude-code",
                "baseten_model_fallback": [
                    "configured_model": "claude-opus-5",
                    "resolved_model": "claude-opus-5",
                    "display_name": "Opus",
                    "provider_ready": true,
                    "ready": true,
                    "reason": NSNull(),
                    "available_models": [
                        [
                            "model": "claude-opus-5",
                            "display_name": "Opus",
                        ],
                        [
                            "model": "claude-sonnet-5",
                            "display_name": "Sonnet",
                        ],
                    ],
                ],
            ]],
        ])

        XCTAssertEqual(
            status.fallbackPolicy,
            FallbackPolicyStatus(
                onBaseten429: true,
                onBaseten5xx: false))
        let fallback = try XCTUnwrap(
            status.clients.first?.basetenModelFallback)
        XCTAssertEqual(fallback.resolvedModel, "claude-opus-5")
        XCTAssertEqual(fallback.displayName, "Opus")
        XCTAssertTrue(fallback.providerReady)
        XCTAssertTrue(fallback.ready)
        XCTAssertNil(fallback.reason)
        XCTAssertEqual(fallback.availableModels, [
            BasetenModelFallbackOption(
                model: "claude-opus-5",
                displayName: "Opus"),
            BasetenModelFallbackOption(
                model: "claude-sonnet-5",
                displayName: "Sonnet"),
        ])
    }

    func testMissingOrMalformedPolicyDoesNotDecodeAsOff() {
        XCTAssertNil(AdminStatusSnapshot(dict: [:]).fallbackPolicy)
        XCTAssertNil(AdminStatusSnapshot(dict: [
            "fallback_policy": ["on_baseten_429": false],
        ]).fallbackPolicy)
    }

    func testFallbackMutationCommandsUseTypedConfigNamespace() {
        XCTAssertEqual(
            fallbackPolicyDispatchArgs(
                trigger: .http429,
                enabled: false),
            ["config", "fallback", "429", "off"])
        XCTAssertEqual(
            fallbackPolicyDispatchArgs(
                trigger: .http5xx,
                enabled: true),
            ["config", "fallback", "5xx", "on"])
        XCTAssertEqual(
            basetenModelFallbackDispatchArgs(
                client: "claude-code",
                model: "claude-opus-5"),
            [
                "config", "fallback", "model", "claude-code",
                "claude-opus-5",
            ])
    }

    func testNativeTargetSelectorUsesServerOptionsAndExcludesAliases() throws {
        let client = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code",
            "baseten_model_fallback": [
                "configured_model": "claude-opus-5",
                "resolved_model": "claude-opus-5",
                "display_name": "Opus",
                "provider_ready": true,
                "ready": true,
                "available_models": [
                    [
                        "model": "claude-opus-5",
                        "display_name": "Opus",
                    ],
                    [
                        "model": "claude-sonnet-5",
                        "display_name": "Sonnet",
                    ],
                    [
                        "model": "claude-haiku-4-5",
                        "display_name": "Haiku",
                    ],
                ],
            ],
            "families": [
                [
                    "family": "sonnet",
                    "configured_target": "native",
                    "effective_route": "anthropic",
                    "effective_model": "claude-sonnet-5",
                ],
                [
                    "family": "haiku",
                    "configured_target": "zai-org/GLM-5.3-Flash",
                    "effective_route": "baseten",
                    "effective_model": "zai-org/GLM-5.3-Flash",
                ],
            ],
        ]))

        XCTAssertEqual(
            claudeNativeFallbackOptions(client: client),
            [
                ClaudeNativeFallbackOption(
                    label: "Opus",
                    model: "claude-opus-5"),
                ClaudeNativeFallbackOption(
                    label: "Sonnet",
                    model: "claude-sonnet-5"),
                ClaudeNativeFallbackOption(
                    label: "Haiku",
                    model: "claude-haiku-4-5"),
            ])
        XCTAssertFalse(
            claudeNativeFallbackOptions(client: client).contains(where: {
                $0.model == "claude-baseten-glm-5-3"
                    || $0.model == "anthropic-baseten-kimi"
                    || $0.model == "claude-sonnet-4-6"
            }))
        XCTAssertFalse(isAcceptedClaudeNativeModelID("opus"))
        XCTAssertFalse(
            isAcceptedClaudeNativeModelID("zai-org/GLM-5.3-Flash"))
        XCTAssertFalse(
            isAcceptedClaudeNativeModelID("claude-baseten-glm-5-3"))
        XCTAssertFalse(
            isAcceptedClaudeNativeModelID("anthropic-baseten-kimi"))
        XCTAssertTrue(isAcceptedClaudeNativeModelID("claude-opus-5"))
        XCTAssertTrue(isAcceptedClaudeNativeModelID("claude-3-7-sonnet"))
    }

    func testFallbackTargetEditorPresentationStartsWithCurrentModel() throws {
        let options = [
            ClaudeNativeFallbackOption(
                label: "Opus",
                model: "claude-opus-5"),
            ClaudeNativeFallbackOption(
                label: "Sonnet",
                model: "claude-sonnet-5"),
        ]

        var presentedDraft: ClaudeNativeFallbackEditorDraft?
        XCTAssertNil(presentedDraft)

        let draftID = UUID()
        presentedDraft = ClaudeNativeFallbackEditorDraft(
            options: options,
            currentModel: "claude-sonnet-5",
            id: draftID)
        let draft = try XCTUnwrap(presentedDraft)
        XCTAssertEqual(
            draft.selectedModel,
            "claude-sonnet-5")
        XCTAssertEqual(
            draft.id,
            draftID)

        presentedDraft = nil
        XCTAssertNil(presentedDraft)
    }

    func testFallbackTargetEditorDoesNotPresentWithoutOptions() {
        XCTAssertNil(ClaudeNativeFallbackEditorDraft(
            options: [],
            currentModel: "claude-sonnet-5"))
    }

    func testFallbackTargetEditorStateUsesPresentedModel() {
        let options = [
            ClaudeNativeFallbackOption(
                label: "Opus",
                model: "claude-opus-5"),
            ClaudeNativeFallbackOption(
                label: "Sonnet",
                model: "claude-sonnet-5"),
        ]

        XCTAssertEqual(
            initialClaudeNativeFallbackSelection(
                options: options,
                currentModel: "claude-sonnet-5"),
            "claude-sonnet-5")
    }

    func testNativeTargetSelectorFailsClosedOnServerAlias() throws {
        let client = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code",
            "baseten_model_fallback": [
                "configured_model": "claude-opus-5",
                "resolved_model": "claude-opus-5",
                "display_name": "Opus",
                "provider_ready": true,
                "ready": true,
                "available_models": [
                    [
                        "model": "claude-sonnet-5",
                        "display_name": "Sonnet",
                    ],
                    [
                        "model": "claude-baseten-glm-5-3",
                        "display_name": "Invalid alias",
                    ],
                ],
            ],
        ]))
        XCTAssertEqual(
            claudeNativeFallbackOptions(client: client),
            [ClaudeNativeFallbackOption(
                label: "Opus",
                model: "claude-opus-5")])
        XCTAssertTrue(
            client.basetenModelFallback?.availableModels.isEmpty == true)
    }

    func testOldRouterSelectorFallsBackOnlyToCurrentTarget() throws {
        let client = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code",
            "baseten_model_fallback": [
                "configured_model": "claude-opus-5",
                "resolved_model": "claude-opus-5",
                "display_name": "Opus",
                "provider_ready": true,
                "ready": true,
            ],
            "families": [[
                "family": "sonnet",
                "configured_target": "native",
                "effective_route": "anthropic",
                "effective_model": "claude-sonnet-5",
            ]],
        ]))
        XCTAssertEqual(
            claudeNativeFallbackOptions(client: client),
            [ClaudeNativeFallbackOption(
                label: "Opus",
                model: "claude-opus-5")])
    }

    func testFallbackTargetStatusKeepsReadinessIndependentFromPolicy() throws {
        let client = try XCTUnwrap(ClientStatus(dict: [
            "name": "claude-code",
            "baseten_model_fallback": [
                "configured_model": "claude-opus-5",
                "resolved_model": "claude-opus-5",
                "display_name": "Opus",
                "provider_ready": true,
                "ready": true,
            ],
        ]))
        XCTAssertEqual(
            basetenFallbackTargetStatusLine(
                fallback: client.basetenModelFallback,
                policy: FallbackPolicyStatus(
                    onBaseten429: false,
                    onBaseten5xx: false),
                desiredMatchesActive: true,
                supportsFallbackPolicy: true),
            "Ready · 429 Off · 5xx Off")
    }

    func testSuppressedReasonDecodesFromTelemetryWireName() throws {
        let data = Data("""
        {
          "attempted": false,
          "count": 0,
          "trigger": null,
          "suppressed_reason": "native_target_unconfigured"
        }
        """.utf8)
        let fallback = try JSONDecoder().decode(
            RequestFallback.self,
            from: data)
        XCTAssertFalse(fallback.attempted)
        XCTAssertNil(fallback.trigger)
        XCTAssertEqual(
            fallback.suppressedReason,
            "native_target_unconfigured")
    }

    func testStructuredAndLegacyMutationWarningsDecodeTogether() throws {
        let receipt = try XCTUnwrap(GlobalMutationReceipt(json: """
        {
          "warnings": [
            {"code": "cross_provider_history_may_be_incompatible"},
            "legacy warning",
            {"message": "ignored without code"}
          ]
        }
        """))
        XCTAssertEqual(receipt.warnings, [
            "cross_provider_history_may_be_incompatible",
            "legacy warning",
        ])
    }

    func testUnsupportedPolicyUsesUnavailablePresentation() {
        XCTAssertEqual(
            automaticFallbackUnavailableMessage(
                supportsFallbackPolicy: false,
                policy: nil),
            "Update the local gateway to configure automatic fallback.")
        XCTAssertEqual(
            automaticFallbackUnavailableMessage(
                supportsFallbackPolicy: true,
                policy: nil),
            "Update the local gateway to configure automatic fallback.")
        XCTAssertNil(automaticFallbackUnavailableMessage(
            supportsFallbackPolicy: true,
            policy: FallbackPolicyStatus(
                onBaseten429: false,
                onBaseten5xx: false)))
    }
}
