import XCTest
@testable import BasetenSwitch

// Popup section view-model tests: System labels (versions, skew,
// auth), glance-strip shaping (sparkline geometry, feed rows), all
// pure functions. UI behaviors needing a live session (positioning,
// dismissal, pinning) are excluded; see the rehaul manual feel-test
// list.

final class PopupDisplayTests: XCTestCase {

    private let utc = TimeZone(identifier: "UTC")!

    // MARK: - System section

    func testParseCLIVersionOutput() {
        XCTAssertEqual(parseCLIVersionOutput("baseten-switch v0.2.1\n"), "v0.2.1")
        XCTAssertEqual(parseCLIVersionOutput("baseten-switch v0.2.1"), "v0.2.1")
        // Unknown shapes pass through trimmed, never hidden.
        XCTAssertEqual(parseCLIVersionOutput("  weird output \nsecond line"),
                       "weird output")
        XCTAssertEqual(parseCLIVersionOutput(""), "")
    }

    func testVersionsLabel() {
        XCTAssertEqual(versionsLabel(routerVersion: "v0.2.1", cliVersion: "v0.2.1"),
                       "Router v0.2.1, CLI v0.2.1")
        // No CLI binary found: CLI half drops.
        XCTAssertEqual(versionsLabel(routerVersion: "v0.2.1", cliVersion: ""),
                       "Router v0.2.1")
        // Unknown router version renders as "?", never silently blank.
        XCTAssertEqual(versionsLabel(routerVersion: "", cliVersion: "v0.2.1"),
                       "Router ?, CLI v0.2.1")
    }

    func testVersionSkewNote() {
        // Matching versions: no note.
        XCTAssertNil(versionSkewNote(routerVersion: "v0.2.1", cliVersion: "v0.2.1"))
        // Either side unknown: no note (cannot claim skew).
        XCTAssertNil(versionSkewNote(routerVersion: "", cliVersion: "v0.2.1"))
        XCTAssertNil(versionSkewNote(routerVersion: "v0.2.1", cliVersion: ""))
        // Differing versions: the skew note names both.
        XCTAssertEqual(
            versionSkewNote(routerVersion: "v0.2.0", cliVersion: "v0.2.1"),
            "Version skew: router v0.2.0, CLI v0.2.1")
    }

    private func auth(signedIn: Bool, profile: String,
                      fallbackInUse: Bool, health: String = "",
                      lastError: String = "") -> AuthStatus {
        AuthStatus(dict: ["signed_in": signedIn, "profile": profile,
                          "fallback_enabled": true,
                          "fallback_in_use": fallbackInUse,
                          "health": health,
                          "last_refresh_error": lastError])
    }

    func testAuthLineLabel() {
        XCTAssertEqual(authLineLabel(auth: nil), "Auth: unknown")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "user@example",
                                     fallbackInUse: false)),
            "Auth: user@example OAuth")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "",
                                     fallbackInUse: false)),
            "Auth: OAuth")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "user@example",
                                     fallbackInUse: true)),
            "Auth: user@example OAuth, API-key fallback in use")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: false, profile: "",
                                     fallbackInUse: true)),
            "Auth: API-key fallback in use")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: false, profile: "",
                                     fallbackInUse: false)),
            "Auth: not signed in")
    }

    // Health-driven auth line states (oauth-expiry spike).
    func testAuthLineLabelHealthStates() {
        // refresh_failed: short and loud, overriding the profile and
        // fallback suffix (detail moves to authDetailLine).
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "zak@baseten",
                                     fallbackInUse: true,
                                     health: "refresh_failed")),
            "Auth: reauthentication required")
        // signed_out: the existing not-signed-in presentation, even if
        // signed_in (store presence) disagrees with the gateway.
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "zak@baseten",
                                     fallbackInUse: false,
                                     health: "signed_out")),
            "Auth: not signed in")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: false, profile: "",
                                     fallbackInUse: true,
                                     health: "signed_out")),
            "Auth: API-key fallback in use")
        // error is transient: the normal line, no alarm.
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "zak@baseten",
                                     fallbackInUse: false, health: "error")),
            "Auth: zak@baseten OAuth")
        // ok and empty health: unchanged.
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "zak@baseten",
                                     fallbackInUse: false, health: "ok")),
            "Auth: zak@baseten OAuth")
        XCTAssertEqual(
            authLineLabel(auth: auth(signedIn: true, profile: "zak@baseten",
                                     fallbackInUse: false, health: "")),
            "Auth: zak@baseten OAuth")
    }

    // The reauth predicate gates the warning tint, the button, and the
    // amber icon: true only for refresh_failed.
    func testAuthNeedsReauth() {
        XCTAssertTrue(authNeedsReauth(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false,
            health: "refresh_failed")))
        XCTAssertFalse(authNeedsReauth(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false, health: "ok")))
        XCTAssertFalse(authNeedsReauth(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false, health: "error")))
        XCTAssertFalse(authNeedsReauth(auth: auth(
            signedIn: false, profile: "", fallbackInUse: false,
            health: "signed_out")))
        XCTAssertFalse(authNeedsReauth(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false, health: "")))
        XCTAssertFalse(authNeedsReauth(auth: nil))
    }

    // The detail line renders only in the dead-credential state, and
    // reuses the menu-safe truncation.
    func testAuthDetailLine() {
        XCTAssertNil(authDetailLine(auth: nil))
        XCTAssertNil(authDetailLine(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false, health: "ok")))
        // A transient error's message stays hidden (no alarm).
        XCTAssertNil(authDetailLine(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false,
            health: "error", lastError: "net timeout")))
        // Dead but no recorded error: no empty line.
        XCTAssertNil(authDetailLine(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false,
            health: "refresh_failed")))
        XCTAssertEqual(
            authDetailLine(auth: auth(
                signedIn: true, profile: "", fallbackInUse: false,
                health: "refresh_failed",
                lastError: "oauth2: \"invalid_grant\"")),
            #"oauth2: "invalid_grant""#)
        // Long or multi-line errors collapse to one truncated line.
        let long = authDetailLine(auth: auth(
            signedIn: true, profile: "", fallbackInUse: false,
            health: "refresh_failed",
            lastError: "line one\n" + String(repeating: "x", count: 200)))
        XCTAssertEqual(long?.count, 80)
        XCTAssertEqual(long?.hasSuffix("..."), true)
        XCTAssertEqual(long?.contains("\n"), false)
    }

    // MARK: - Glance header and fallback badge

    private func snapshot(_ json: String) -> StatsSnapshot {
        let obj = try! JSONSerialization.jsonObject(
            with: Data(json.utf8), options: [])
        return StatsSnapshot(dict: obj as! [String: Any])
    }

    func testGlanceHeaderLabel() {
        XCTAssertEqual(glanceHeaderLabel(nil), "No request data")
        XCTAssertEqual(glanceHeaderLabel(snapshot("{}")), "No request data")
        let s = snapshot("""
        {"window_seconds": 3600, "buckets": [
          {"ts": 1, "requests": 3}, {"ts": 2, "requests": 0}, {"ts": 3, "requests": 9}
        ]}
        """)
        XCTAssertEqual(glanceHeaderLabel(s), "12 requests, last 60m")
        let one = snapshot(
            #"{"window_seconds": 60, "buckets": [{"ts": 1, "requests": 1}]}"#)
        XCTAssertEqual(glanceHeaderLabel(one), "1 request, last 1m")
    }

    func testFallbackActiveClients() {
        XCTAssertEqual(fallbackActiveClients([:]), [])
        XCTAssertEqual(fallbackActiveClients(["claude-code": false]), [])
        // Sorted, actives only.
        XCTAssertEqual(
            fallbackActiveClients(["codex": true, "claude-code": true, "x": false]),
            ["claude-code", "codex"])
    }

    func testCompactRoutingLabels() {
        XCTAssertEqual(clientDisplayName("claude-code"), "Claude Code")
        XCTAssertEqual(clientDisplayName("codex"), "Codex")
        XCTAssertEqual(clientDisplayName("custom"), "custom")
        XCTAssertEqual(shortModelName("zai-org/GLM-5.2"), "GLM-5.2")
        XCTAssertEqual(shortModelName("GLM-5.2"), "GLM-5.2")
        XCTAssertEqual(shortModelName(""), "")

        let baseten = ClientStatus(dict: [
            "name": "claude-code", "enabled": true, "effective_route": "baseten",
            "unmatched_native_model": [
                "effective_model": "zai-org/GLM-5.2",
            ],
        ])!
        XCTAssertEqual(routingDestinationLabel(baseten), "Baseten · GLM-5.2")
        XCTAssertEqual(clientMenuTitle(baseten), "Claude Code: Baseten · GLM-5.2")

        let native = ClientStatus(dict: [
            "name": "codex", "enabled": true, "effective_route": "openai",
        ])!
        XCTAssertEqual(routingDestinationLabel(native), "Native · Openai")

        let fallback = ClientStatus(dict: [
            "name": "claude-code", "enabled": true, "effective_route": "baseten",
            "native_route": "anthropic",
            "fallback": ["active": true],
        ])!
        XCTAssertEqual(routingDestinationLabel(fallback),
                       "Fallback · Anthropic")
        XCTAssertEqual(routingCountLabel([baseten, native]), "1 Baseten route")
        XCTAssertEqual(routingCountLabel([native]), "0 Baseten routes")
        XCTAssertEqual(routingCountLabel([fallback]), "1 fallback")
        XCTAssertEqual(compactFeedModelLabel(
            requested: "claude-haiku-4-5",
            upstream: "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"),
            "NVIDIA-Nemotron-3-Ultra-550B-A55B")
        XCTAssertEqual(compactFeedModelLabel(requested: "claude-haiku-4-5",
                                             upstream: ""),
                       "claude-haiku-4-5")
        XCTAssertEqual(compactFeedRouteLabel(route: "baseten", routeEffective: ""),
                       "Baseten")
        XCTAssertEqual(compactFeedRouteLabel(route: "baseten",
                                             routeEffective: "anthropic"),
                       "Anthropic fallback")
    }

    func testGlobalRoutingStateAndSubtitle() {
        let baseten = ClientStatus(dict: [
            "name": "claude-code", "enabled": true, "effective_route": "baseten",
        ])!
        let native = ClientStatus(dict: [
            "name": "codex", "enabled": true, "effective_route": "openai",
        ])!
        let disabled = ClientStatus(dict: [
            "name": "disabled", "enabled": false, "effective_route": "baseten",
        ])!
        let fallback = ClientStatus(dict: [
            "name": "claude-code", "enabled": true, "effective_route": "baseten",
            "fallback": ["active": true],
        ])!

        XCTAssertEqual(globalRoutingState([]), .off)
        XCTAssertEqual(globalRoutingState([disabled]), .off)
        XCTAssertEqual(globalRoutingState([baseten, disabled]), .on)
        XCTAssertEqual(globalRoutingState([native, disabled]), .off)
        XCTAssertEqual(globalRoutingState([baseten, native]), .mixed)

        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: false, clients: [baseten],
                                             auth: nil),
                       "Gateway stopped")
        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: true, clients: [], auth: nil),
                       "No enabled routing clients")
        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: true, clients: [baseten],
                                             auth: nil),
                       "Routing through Baseten")
        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: true, clients: [native],
                                             auth: nil),
                       "Using native providers")
        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: true,
                                             clients: [baseten, native], auth: nil),
                       "1 of 2 routes through Baseten")
        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: true, clients: [fallback],
                                             auth: nil),
                       "1 route using fallback")
        let deadAuth = AuthStatus(dict: ["health": "refresh_failed"])
        XCTAssertEqual(globalRoutingSubtitle(gatewayUp: true, clients: [baseten],
                                             auth: deadAuth),
                       "Authentication required")
    }

    // MARK: - Sparkline bucket-to-points mapping

    func testSparklinePointsEmpty() {
        XCTAssertEqual(sparklinePoints(requests: [], in: CGSize(width: 100, height: 30)), [])
        XCTAssertEqual(sparklinePoints(requests: [1], in: .zero), [])
    }

    func testSparklinePointsSingleBucketCenters() {
        let pts = sparklinePoints(requests: [4], in: CGSize(width: 100, height: 30))
        XCTAssertEqual(pts, [CGPoint(x: 50, y: 0)]) // max of itself: top
    }

    func testSparklinePointsSpacingAndNormalization() {
        let pts = sparklinePoints(requests: [0, 5, 10],
                                  in: CGSize(width: 100, height: 40))
        XCTAssertEqual(pts.count, 3)
        // Evenly spaced across the width, oldest first.
        XCTAssertEqual(pts[0].x, 0)
        XCTAssertEqual(pts[1].x, 50)
        XCTAssertEqual(pts[2].x, 100)
        // y normalized against the max (10): 0 -> bottom, max -> top.
        XCTAssertEqual(pts[0].y, 40)
        XCTAssertEqual(pts[1].y, 20)
        XCTAssertEqual(pts[2].y, 0)
    }

    func testSparklinePointsAllZeroDrawsBaseline() {
        // Max clamps to 1 so an idle window is a flat bottom line.
        let pts = sparklinePoints(requests: [0, 0],
                                  in: CGSize(width: 10, height: 20))
        XCTAssertEqual(pts.map(\.y), [20, 20])
    }

    func testSparklineErrorIndices() {
        let s = snapshot("""
        {"buckets": [
          {"ts": 1, "requests": 3, "errors": 0},
          {"ts": 2, "requests": 2, "errors": 1},
          {"ts": 3, "requests": 0, "errors": 0},
          {"ts": 4, "requests": 5, "errors": 2}
        ]}
        """)
        XCTAssertEqual(sparklineErrorIndices(s.buckets), [1, 3])
        XCTAssertEqual(sparklineErrorIndices([]), [])
    }

    // MARK: - Feed rows

    func testFeedModelLabel() {
        // Same model: no arrow.
        XCTAssertEqual(feedModelLabel(requested: "claude-fable-5",
                                      upstream: "claude-fable-5"),
                       "claude-fable-5")
        // Rewritten: requested -> upstream arrow.
        XCTAssertEqual(feedModelLabel(requested: "claude-opus-4-8",
                                      upstream: "zai-org/GLM-5.2"),
                       "claude-opus-4-8 -> zai-org/GLM-5.2")
        // Only one side known: show it alone, no dangling arrow.
        XCTAssertEqual(feedModelLabel(requested: "claude-opus-4-8", upstream: ""),
                       "claude-opus-4-8")
        XCTAssertEqual(feedModelLabel(requested: "", upstream: "zai-org/GLM-5.2"),
                       "zai-org/GLM-5.2")
        XCTAssertEqual(feedModelLabel(requested: "", upstream: ""), "")
    }

    func testFeedTimeLabel() {
        // 1783997990.1 = 2026-07-14 02:59:50.1 UTC.
        XCTAssertEqual(feedTimeLabel(ts: 1_783_997_990.1, timeZone: utc), "02:59:50")
        XCTAssertEqual(feedTimeLabel(ts: 0, timeZone: utc), "00:00:00")
    }

    func testFeedRowsNewestFirstWithBadges() {
        let s = snapshot("""
        {"recent": [
          {"ts": 100, "requested_model": "claude-fable-5",
           "upstream_model": "claude-fable-5", "status": 200, "subagent": false},
          {"ts": 200, "requested_model": "claude-opus-4-8",
           "upstream_model": "zai-org/GLM-5.2", "status": 502, "subagent": true}
        ]}
        """)
        let rows = feedRows(s.recent, timeZone: utc)
        XCTAssertEqual(rows.count, 2)
        // Contract order is oldest first; the feed shows newest first.
        XCTAssertEqual(rows[0].time, feedTimeLabel(ts: 200, timeZone: utc))
        XCTAssertEqual(rows[0].model, "claude-opus-4-8 -> zai-org/GLM-5.2")
        XCTAssertTrue(rows[0].subagent)
        XCTAssertEqual(rows[0].status, 502)
        XCTAssertTrue(rows[0].isError)
        XCTAssertEqual(rows[1].model, "claude-fable-5")
        XCTAssertFalse(rows[1].subagent)
        XCTAssertEqual(rows[1].status, 200)
        XCTAssertFalse(rows[1].isError)
    }

    func testFeedRowsLimitKeepsNewest() {
        let recent = (1...20).map { i -> RecentRequest in
            RecentRequest(dict: ["ts": Double(i), "requested_model": "m\(i)",
                                 "upstream_model": "m\(i)", "status": 200])
        }
        let rows = feedRows(recent, limit: 8, timeZone: utc)
        XCTAssertEqual(rows.count, 8)
        // Newest (ts 20) first; the cut drops the oldest.
        XCTAssertEqual(rows.first?.model, "m20")
        XCTAssertEqual(rows.last?.model, "m13")
    }

    func testFeedRowsErrorClassification() {
        let mk = { (status: Int) -> RecentRequest in
            RecentRequest(dict: ["ts": 1.0, "status": status])
        }
        XCTAssertFalse(feedRows([mk(200)], timeZone: self.utc)[0].isError)
        XCTAssertFalse(feedRows([mk(302)], timeZone: self.utc)[0].isError)
        XCTAssertTrue(feedRows([mk(404)], timeZone: self.utc)[0].isError)
        XCTAssertTrue(feedRows([mk(502)], timeZone: self.utc)[0].isError)
        // Status 0 (client cancel or connect failure) is not an error,
        // matching the unified status >= 400 definition across surfaces.
        XCTAssertFalse(feedRows([mk(0)], timeZone: self.utc)[0].isError)
    }

    func testFeedRouteLabel() {
        // No fallback: the plain requested route.
        XCTAssertEqual(feedRouteLabel(route: "baseten", routeEffective: ""),
                       "baseten")
        // Fallback: the effective route tagged "(fb)".
        XCTAssertEqual(feedRouteLabel(route: "baseten", routeEffective: "anthropic"),
                       "anthropic (fb)")
    }

    func testFeedRowsRouteAndFallback() {
        let s = snapshot("""
        {"recent": [
          {"ts": 100, "route": "baseten", "route_effective": "",
           "status": 200},
          {"ts": 200, "route": "baseten", "route_effective": "anthropic",
           "status": 200}
        ]}
        """)
        let rows = feedRows(s.recent, timeZone: utc)
        // Newest first: the fallback row, then the plain row.
        XCTAssertEqual(rows[0].route, "anthropic (fb)")
        XCTAssertTrue(rows[0].isFallback)
        XCTAssertEqual(rows[1].route, "baseten")
        XCTAssertFalse(rows[1].isFallback)
    }
}
