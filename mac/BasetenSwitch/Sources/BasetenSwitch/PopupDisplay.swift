import Foundation
import CoreGraphics
import ServiceManagement

// Native-menu view-model functions: status labels, compact route and
// traffic summaries, plus the retained sparkline/feed shaping used by
// tests and earlier dashboard work. All pure, all rendering server truth; the
// dispatch logic stays in Display.swift, shared with no route computation in
// Swift.

// MARK: - System section

/// Extracts the version token from `baseten-switch --version` output
/// ("baseten-switch v0.2.1" -> "v0.2.1"). Unknown shapes pass through
/// trimmed so a future format change degrades to showing the raw line
/// rather than hiding the version.
func parseCLIVersionOutput(_ raw: String) -> String {
    let line = raw.components(separatedBy: .newlines).first ?? ""
    let trimmed = line.trimmingCharacters(in: .whitespaces)
    let prefix = "baseten-switch "
    if trimmed.hasPrefix(prefix) {
        return String(trimmed.dropFirst(prefix.count))
            .trimmingCharacters(in: .whitespaces)
    }
    return trimmed
}

/// Component-versions line for the System section. Router version is
/// server truth (admin status); the CLI version is what the resolved
/// baseten-switch binary reports. An unknown router version renders as "?"
/// so the line never silently drops a component; no CLI binary found
/// drops the CLI half entirely.
func versionsLabel(routerVersion: String, cliVersion: String) -> String {
    let router = routerVersion.isEmpty ? "?" : routerVersion
    if cliVersion.isEmpty {
        return "Router \(router)"
    }
    return "Router \(router), CLI \(cliVersion)"
}

/// Skew note: non-nil only when both versions are known and differ
/// (the doctor `binary skew` warning class, surfaced in the popup so a
/// stale pairing is loud without running doctor).
func versionSkewNote(routerVersion: String, cliVersion: String) -> String? {
    guard !routerVersion.isEmpty, !cliVersion.isEmpty,
          routerVersion != cliVersion else { return nil }
    return "Version skew: router \(routerVersion), CLI \(cliVersion)"
}

/// Auth line from the admin status auth block. Signed in shows the
/// OAuth profile; fallback-in-use is appended (or shown alone when not
/// signed in) so degraded auth is visible where the controls are.
/// Health overlays (oauth-expiry spike): "refresh_failed" means the
/// token endpoint rejected the stored credential, so every
/// baseten-routed request fails until re-login; the line goes short
/// and loud, with the error detail on authDetailLine. A transient
/// "error" deliberately renders the normal line (the next refresh
/// usually resolves it; alarming on it would flap). An empty health
/// (router predates the field) changes nothing.
func authLineLabel(auth: AuthStatus?) -> String {
    guard let auth else { return "Auth: unknown" }
    if auth.health == "refresh_failed" {
        return "Auth: reauthentication required"
    }
    // signed_in is store presence; health "signed_out" is the gateway
    // holding no OAuth client. Either signal renders the
    // not-signed-in presentation.
    if auth.signedIn && auth.health != "signed_out" {
        let who = auth.profile.isEmpty ? "OAuth" : "\(auth.profile) OAuth"
        if auth.fallbackInUse {
            return "Auth: \(who), API-key fallback in use"
        }
        return "Auth: \(who)"
    }
    if auth.fallbackInUse {
        return "Auth: API-key fallback in use"
    }
    return "Auth: not signed in"
}

/// True only in the dead-credential state: drives the warning tint on
/// the auth line, the Reauthenticate button, and (via
/// menubarIconState) the amber status icon. Transient "error" stays
/// false so the popup never alarms on a blip.
func authNeedsReauth(auth: AuthStatus?) -> Bool {
    auth?.health == "refresh_failed"
}

/// Secondary detail line under "reauthentication required": the last
/// refresh error, collapsed to one menu-safe truncated line. Nil in
/// every other state so a healthy popup gains no height.
func authDetailLine(auth: AuthStatus?) -> String? {
    guard let auth, auth.health == "refresh_failed",
          !auth.lastRefreshError.isEmpty else { return nil }
    return menuErrorLabel(auth.lastRefreshError)
}

// MARK: - Start at Login row

/// Caption under the Start at Login toggle. Non-nil exactly in the
/// states where the login item will NOT fire at next login, so a
/// broken registration is visible without opening System Settings; nil when
/// healthy so the row gains no height.
/// - requiresApproval: macOS is holding the item until the user
///   approves it in System Settings (the row's click opens that pane).
/// - notFound: no live registration. This is not proof of a failed
///   attempt: the status can flip mid-session with no register() call
///   in this session (the bundle replaced under the running app by a
///   rebuild or an update; refresh() re-reads it every 1.5s), and it
///   is the expected steady state on a bare swift-build binary where
///   SMAppService cannot register. So the caption states the fact and
///   names the fix (the toggle is the retry, per
///   loginItemToggleAction) without asserting a failure.
func startAtLoginCaption(status: SMAppService.Status) -> String? {
    switch status {
    case .requiresApproval:
        return "needs approval in System Settings"
    case .notFound:
        return "not registered, will not start at login; toggle to register"
    default:
        return nil
    }
}

/// True when clicking the Start at Login row should open System
/// Settings > Login Items instead of flipping the registration:
/// pending approval can only be resolved there, so one click lands
/// the user in the right pane.
func startAtLoginOpensSystemSettings(status: SMAppService.Status) -> Bool {
    status == .requiresApproval
}

// MARK: - Glance strip

/// Header line above the sparkline: total requests over the window.
/// Nil or bucketless snapshots read "No request data" so the strip
/// never renders a misleading zero.
func glanceHeaderLabel(_ snapshot: StatsSnapshot?) -> String {
    guard let snapshot, !snapshot.buckets.isEmpty else {
        return "No request data"
    }
    let total = snapshot.buckets.reduce(0) { $0 + $1.requests }
    let minutes = max(snapshot.windowSeconds / 60, 1)
    let noun = total == 1 ? "request" : "requests"
    return "\(total) \(noun), last \(minutes)m"
}

/// Clients with fallback currently active, sorted for stable display.
func fallbackActiveClients(_ map: [String: Bool]) -> [String] {
    map.filter { $0.value }.keys.sorted()
}

// MARK: - Compact routing summary

enum GlobalRoutingState: Equatable {
    case off
    case on
    case mixed
}

/// State of the global `baseten-switch on|off` header switch. Disabled clients do
/// not participate; an empty enabled set is off. Mixed is explicit so the
/// menu never claims all clients share a route when they do not.
func globalRoutingState(_ clients: [ClientStatus]) -> GlobalRoutingState {
    let enabled = clients.filter(\.enabled)
    guard !enabled.isEmpty else { return .off }
    let basetenCount = enabled.filter { $0.effectiveRoute == "baseten" }.count
    if basetenCount == 0 { return .off }
    if basetenCount == enabled.count { return .on }
    return .mixed
}

func globalRoutingSubtitle(gatewayUp: Bool, clients: [ClientStatus],
                           auth: AuthStatus?) -> String {
    guard gatewayUp else { return "Gateway stopped" }
    if authNeedsReauth(auth: auth) { return "Authentication required" }

    let enabled = clients.filter(\.enabled)
    guard !enabled.isEmpty else { return "No enabled routing clients" }

    let fallbackCount = enabled.filter(\.fallbackActive).count
    if fallbackCount > 0 {
        let noun = fallbackCount == 1 ? "route" : "routes"
        return "\(fallbackCount) \(noun) using fallback"
    }

    let basetenCount = enabled.filter { $0.effectiveRoute == "baseten" }.count
    if basetenCount == enabled.count { return "Routing through Baseten" }
    if basetenCount == 0 { return "Using native providers" }
    return "\(basetenCount) of \(enabled.count) routes through Baseten"
}

/// Friendly names for the harness identifiers served by admin status.
/// Unknown identifiers stay visible verbatim instead of being guessed.
func clientDisplayName(_ name: String) -> String {
    switch name {
    case "claude-code": return "Claude Code"
    case "codex": return "Codex"
    case "opencode": return "OpenCode"
    default: return name
    }
}

/// SF Symbol used only as visual identity; routing behavior remains server truth.
func clientIconName(_ name: String) -> String {
    switch name {
    case "claude-code": return "terminal"
    case "codex": return "chevron.left.forwardslash.chevron.right"
    case "opencode": return "curlybraces"
    default: return "app.dashed"
    }
}

/// A compact destination label for the quick-switch row. It formats only
/// fields already resolved by the gateway and never computes a route.
func routingDestinationLabel(_ client: ClientStatus) -> String {
    if client.fallbackActive {
        let native = client.nativeRoute.isEmpty ? "native" : client.nativeRoute
        return "Fallback · \(capitalizeFamily(native))"
    }
    if client.effectiveRoute == "baseten" {
        let model = shortModelName(
            client.unmatchedNativeModel?.effectiveModel ?? "")
        return model.isEmpty ? "Baseten" : "Baseten · \(model)"
    }
    let native = client.effectiveRoute.isEmpty ? client.nativeRoute : client.effectiveRoute
    return native.isEmpty ? "Native" : "Native · \(capitalizeFamily(native))"
}

func clientMenuTitle(_ client: ClientStatus) -> String {
    "\(clientDisplayName(client.name)): \(routingDestinationLabel(client))"
}

/// Provider-qualified slugs are too long for a 360pt popup. Preserve the
/// model leaf exactly, which is the useful differentiator at this scale.
func shortModelName(_ model: String) -> String {
    model.split(separator: "/", omittingEmptySubsequences: true).last.map(String.init) ?? ""
}

func routingCountLabel(_ clients: [ClientStatus]) -> String {
    let fallback = clients.filter { $0.enabled && $0.fallbackActive }.count
    if fallback > 0 {
        let noun = fallback == 1 ? "fallback" : "fallbacks"
        return "\(fallback) \(noun)"
    }
    let active = clients.filter { $0.enabled && $0.effectiveRoute == "baseten" }.count
    let noun = active == 1 ? "route" : "routes"
    return "\(active) Baseten \(noun)"
}

/// Latest-request labels optimize for recognition over audit detail; the
/// full requested-to-upstream mapping remains available in the dashboard.
func compactFeedModelLabel(requested: String, upstream: String) -> String {
    shortModelName(upstream.isEmpty ? requested : upstream)
}

func compactFeedRouteLabel(route: String, routeEffective: String) -> String {
    if !routeEffective.isEmpty {
        return "\(capitalizeFamily(routeEffective)) fallback"
    }
    return capitalizeFamily(route)
}

/// Maps bucket request counts to sparkline points in a drawing space
/// of `size` (origin top-left, as SwiftUI Canvas). Points are evenly
/// spaced across the width, oldest first; y is normalized against the
/// max count (min 1 so an all-zero window draws a flat baseline). A
/// single bucket centers horizontally; empty input yields no points.
func sparklinePoints(requests: [Int], in size: CGSize) -> [CGPoint] {
    guard !requests.isEmpty, size.width > 0, size.height > 0 else { return [] }
    let maxCount = max(requests.max() ?? 0, 1)
    func y(_ v: Int) -> CGFloat {
        size.height * (1 - CGFloat(v) / CGFloat(maxCount))
    }
    if requests.count == 1 {
        return [CGPoint(x: size.width / 2, y: y(requests[0]))]
    }
    let step = size.width / CGFloat(requests.count - 1)
    return requests.enumerated().map { i, v in
        CGPoint(x: CGFloat(i) * step, y: y(v))
    }
}

/// Indices of buckets carrying at least one error, for the error tint.
func sparklineErrorIndices(_ buckets: [StatsBucket]) -> [Int] {
    buckets.enumerated().compactMap { $1.errors > 0 ? $0 : nil }
}

/// One formatted row of the recent-requests feed.
struct FeedRow: Equatable, Identifiable {
    var id: String
    var time: String
    var model: String
    var route: String
    /// True when `route` came from a non-empty route_effective (a
    /// fallback occurred), driving the orange tint in the feed.
    var isFallback: Bool
    var subagent: Bool
    var status: Int
    var isError: Bool
}

/// Route column text: the effective route when a fallback occurred
/// (route_effective non-empty), tagged "(fb)" to match the dashboard
/// and CLI idiom; otherwise the plain requested route.
func feedRouteLabel(route: String, routeEffective: String) -> String {
    if routeEffective.isEmpty {
        return route
    }
    return "\(routeEffective) (fb)"
}

/// Model column text: requested->upstream arrow only when the gateway
/// rewrote the model; otherwise the single name (whichever is known).
func feedModelLabel(requested: String, upstream: String) -> String {
    if upstream.isEmpty || upstream == requested {
        return requested
    }
    if requested.isEmpty {
        return upstream
    }
    return "\(requested) -> \(upstream)"
}

/// HH:mm:ss for a feed timestamp. The zone parameter exists for
/// hermetic tests; callers use the default.
func feedTimeLabel(ts: Double, timeZone: TimeZone = .current) -> String {
    var cal = Calendar(identifier: .gregorian)
    cal.timeZone = timeZone
    let parts = cal.dateComponents([.hour, .minute, .second],
                                   from: Date(timeIntervalSince1970: ts))
    return String(format: "%02d:%02d:%02d",
                  parts.hour ?? 0, parts.minute ?? 0, parts.second ?? 0)
}

/// Shapes the contract's oldest-first `recent` array into display
/// rows, newest first, capped at `limit` so the popup stays compact
/// while preserving the server's bounded feed. Error is status >= 400 only;
/// status 0 is a client cancel or connect failure, deliberately not an error.
func feedRows(_ recent: [RecentRequest], limit: Int = 8,
              timeZone: TimeZone = .current) -> [FeedRow] {
    recent.suffix(limit).reversed().enumerated().map { i, r in
        FeedRow(id: "\(r.ts)-\(i)",
                time: feedTimeLabel(ts: r.ts, timeZone: timeZone),
                model: feedModelLabel(requested: r.requestedModel,
                                      upstream: r.upstreamModel),
                route: feedRouteLabel(route: r.route,
                                      routeEffective: r.routeEffective),
                isFallback: !r.routeEffective.isEmpty,
                subagent: r.subagent,
                status: r.status,
                isError: r.status >= 400)
    }
}
