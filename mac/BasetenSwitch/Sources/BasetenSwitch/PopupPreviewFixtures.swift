#if DEBUG
import Foundation
import ServiceManagement

/// Side-effect-free states for the native menu preview. These mirror admin API
/// payloads closely enough to exercise long model names, warnings, empty
/// states, and the full control hierarchy without touching the live gateway.
struct PopupPreviewFixture: Identifiable {
    static let environmentKey = "BASETEN_SWITCH_POPUP_PREVIEW"
    static let routerWindowEnvironmentKey = "BASETEN_SWITCH_ROUTER_WINDOW_PREVIEW"

    let id: String
    let name: String
    let gatewayUp: Bool
    let uptimeSeconds: Int64
    let clients: [ClientStatus]
    let routerVersion: String
    let cliVersion: String
    let auth: AuthStatus?
    let stats: StatsSnapshot?
    let loginItemStatus: SMAppService.Status
    let lastError: String?

    var liveModelCatalogState: LiveModelCatalogLoadState {
        guard gatewayUp else {
            return .error("The gateway is unavailable.")
        }
        return .ready([
            LiveModelCatalogEntry(dict: [
                "slug": "zai-org/GLM-5.2",
                "display_name": "GLM 5.2",
                "reasoning": [
                    "supported": true,
                    "options": [
                        ["type": "toggle"],
                    ],
                    "source": "models_dev",
                    "loaded_from": "runtime_cache",
                    "revision": "sha256:preview-reasoning-fixture",
                    "captured_at": "2026-07-25T18:00:00Z",
                    "stale": false,
                ],
            ])!,
        ])
    }

    static let all: [PopupPreviewFixture] = [
        .healthy,
        .nativeOnly,
        .mixed,
        .fallback,
        .authenticationExpired,
        .actionFailure,
        .gatewayDown,
    ]

    static var requested: PopupPreviewFixture? {
        guard let id = ProcessInfo.processInfo.environment[environmentKey] else {
            return nil
        }
        return all.first { $0.id == id }
    }

    static var routerWindowRequested: PopupPreviewFixture? {
        guard let id = ProcessInfo.processInfo.environment[routerWindowEnvironmentKey] else {
            return nil
        }
        return all.first { $0.id == id }
    }

    static let healthy = PopupPreviewFixture(
        id: "healthy",
        name: "Healthy",
        gatewayUp: true,
        uptimeSeconds: 3_247,
        clients: [client()],
        routerVersion: "v0.2.1",
        cliVersion: "v0.2.1",
        auth: auth(),
        stats: stats(),
        loginItemStatus: .enabled,
        lastError: nil
    )

    static let nativeOnly = PopupPreviewFixture(
        id: "native-off",
        name: "Global routing Off",
        gatewayUp: true,
        uptimeSeconds: 3_247,
        clients: [
            client(
                route: "anthropic",
                effectiveModel: ""),
        ],
        routerVersion: "v0.2.1",
        cliVersion: "v0.2.1",
        auth: auth(),
        stats: stats(),
        loginItemStatus: .enabled,
        lastError: nil
    )

    static let mixed = PopupPreviewFixture(
        id: "mixed",
        name: "Mixed routes",
        gatewayUp: true,
        uptimeSeconds: 7_421,
        clients: [
            client(),
            client(name: "codex", route: "openai", nativeRoute: "openai",
                   effectiveModel: ""),
        ],
        routerVersion: "v0.2.1",
        cliVersion: "v0.2.1",
        auth: auth(),
        stats: stats(),
        loginItemStatus: .enabled,
        lastError: nil
    )

    static let fallback = PopupPreviewFixture(
        id: "fallback",
        name: "Fallback active",
        gatewayUp: true,
        uptimeSeconds: 18_905,
        clients: [client(fallbackActive: true)],
        routerVersion: "v0.2.1",
        cliVersion: "v0.2.1",
        auth: auth(),
        stats: stats(fallbackActive: true),
        loginItemStatus: .enabled,
        lastError: nil
    )

    static let authenticationExpired = PopupPreviewFixture(
        id: "authentication-expired",
        name: "Authentication expired",
        gatewayUp: true,
        uptimeSeconds: 86_771,
        clients: [client()],
        routerVersion: "v0.2.0",
        cliVersion: "v0.2.1",
        auth: auth(health: "refresh_failed",
                   lastError: "oauth2: invalid_grant: refresh token expired"),
        stats: stats(),
        loginItemStatus: .requiresApproval,
        lastError: nil
    )

    static let gatewayDown = PopupPreviewFixture(
        id: "gateway-down",
        name: "Gateway stopped",
        gatewayUp: false,
        uptimeSeconds: 0,
        clients: [],
        routerVersion: "",
        cliVersion: "v0.2.1",
        auth: nil,
        stats: nil,
        loginItemStatus: .notRegistered,
        lastError: nil
    )

    static let actionFailure = PopupPreviewFixture(
        id: "action-failure",
        name: "Action failure",
        gatewayUp: true,
        uptimeSeconds: 3_247,
        clients: [client()],
        routerVersion: "v0.2.1",
        cliVersion: "v0.2.1",
        auth: auth(),
        stats: stats(),
        loginItemStatus: .enabled,
        lastError: "baseten-switch not found (checked $BASETEN_SWITCH_GATEWAY_BIN, ~/.local/bin, brew opt paths)"
    )

    private static func client(name: String = "claude-code",
                               route: String = "baseten",
                               nativeRoute: String = "anthropic",
                               effectiveModel: String = "zai-org/GLM-5.2",
                               fallbackActive: Bool = false) -> ClientStatus {
        var dict: [String: Any] = [
            "name": name,
            "enabled": true,
            "bind_addr": "127.0.0.1:45272",
            "protocol_shape": "anthropic",
            "effective_route": route,
            "native_route": nativeRoute,
            "auth_set": true,
            "currently_bound": true,
            "effective_summary": route == "baseten"
                ? "Baseten · \(shortModelName(effectiveModel))"
                : "Native · \(capitalizeFamily(nativeRoute))",
            "fallback": [
                "active": fallbackActive,
                "served_route": fallbackActive ? nativeRoute : "",
                "cause": fallbackActive ? "http_429" : "",
            ],
            "subagent_model": "",
            "subagent_routing": "off",
            "subagent_effective": "inherit",
            "unmatched_native_model": [
                "configured_target": "zai-org/GLM-5.2",
                "effective_route": route,
                "effective_model": route == "baseten"
                    ? effectiveModel
                    : "",
                "effective_source": route == "baseten"
                    ? "default_baseten"
                    : "global_off",
            ],
            "families": [
                family("fable", model: "zai-org/GLM-5.2"),
                family("opus", model: "zai-org/GLM-5.2"),
                family("sonnet", model: "moonshotai/Kimi-K2.7-Code"),
                family("haiku", model: "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"),
            ],
            "model_catalog": [
                catalog("GLM-5.2", target: "zai-org/GLM-5.2"),
                catalog("Kimi-K2.7-Code", target: "moonshotai/Kimi-K2.7-Code"),
                catalog("NVIDIA-Nemotron-3-Ultra-550B-A55B",
                        target: "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"),
            ],
            "model_options": [
                "baseten": [
                    "zai-org/GLM-5.2": [
                        "reasoning": [
                            "configured": ["mode": "default"],
                            "effective": ["mode": "off"],
                            "source": "compatibility_default",
                            "available_modes": ["off", "follow_harness"],
                            "available_efforts": [],
                            "available": true,
                            "unavailable_reason": "",
                            "error": "",
                        ],
                    ],
                ],
            ],
        ]
        if name == "claude-code" {
            dict["model_picker"] = [
                "enabled": true,
                "models": [
                    [
                        "alias": "claude-baseten-glm-5-2",
                        "slug": "zai-org/GLM-5.2",
                        "label": "GLM 5.2 via Baseten",
                        "description": "Served by Baseten.",
                    ],
                    [
                        "alias": "claude-baseten-kimi-k2-7-code",
                        "slug": "moonshotai/Kimi-K2.7-Code",
                        "label": "Kimi K2.7 Code via Baseten",
                        "description": "Served by Baseten.",
                    ],
                ],
            ]
        }
        return ClientStatus(dict: dict)!
    }

    private static func family(_ family: String,
                               model: String) -> [String: Any] {
        return [
            "family": family,
            "configured_target": model,
            "configured_source": "explicit",
            "effective_route": "baseten",
            "effective_model": model,
            "effective_source": "family_mapping",
        ]
    }

    private static func catalog(_ label: String, target: String) -> [String: Any] {
        [
            "label": label,
            "storage_target": target,
            "slug": target,
            "alias": "",
            "available": true,
        ]
    }

    private static func auth(health: String = "ok",
                             lastError: String = "") -> AuthStatus {
        AuthStatus(dict: [
            "signed_in": true,
            "profile": "developer@example.com",
            "fallback_enabled": false,
            "fallback_in_use": false,
            "health": health,
            "last_refresh_error": lastError,
        ])
    }

    private static func stats(fallbackActive: Bool = false) -> StatsSnapshot {
        let now = Date().timeIntervalSince1970
        return StatsSnapshot(dict: [
            "window_seconds": 3600,
            "bucket_seconds": 600,
            "buckets": [2, 5, 3, 9, 7, 12].enumerated().map { index, requests in
                [
                    "ts": now - Double((5 - index) * 600),
                    "requests": requests,
                    "errors": fallbackActive && index == 4 ? 2 : 0,
                    "p50_ms": 820,
                    "p95_ms": 2_400,
                ]
            },
            "fallback_active": ["claude-code": fallbackActive],
            "recent": [
                [
                    "ts": now - 92,
                    "client": "claude-code",
                    "route": "baseten",
                    "route_effective": fallbackActive ? "anthropic" : "",
                    "requested_model": "claude-opus-4-8",
                    "upstream_model": "zai-org/GLM-5.2",
                    "status": 200,
                    "duration_ms": 1_240,
                    "subagent": false,
                ],
                [
                    "ts": now - 18,
                    "client": "claude-code",
                    "route": "baseten",
                    "route_effective": "",
                    "requested_model": "claude-haiku-4-5",
                    "upstream_model": "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B",
                    "status": 200,
                    "duration_ms": 780,
                    "subagent": true,
                ],
            ],
        ])
    }
}

#endif
