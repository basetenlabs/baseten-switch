import Foundation

// Models for GET /v1/admin/stats (the glance-strip contract). Same
// tolerant JSONSerialization decode style as ClientStatus: absent keys
// take zero values, unknown keys are ignored, and a malformed payload
// decodes to an empty snapshot rather than failing. The gateway
// computes the windowing server-side; the app never parses telemetry
// files or re-buckets anything.

/// One fixed-width, zero-filled histogram bucket (oldest first in the
/// snapshot's `buckets`).
struct StatsBucket: Equatable {
    var ts: Int64
    var requests: Int
    var errors: Int
    var p50Ms: Double
    var p95Ms: Double

    init(dict: [String: Any]) {
        self.ts = (dict["ts"] as? NSNumber)?.int64Value ?? 0
        self.requests = (dict["requests"] as? NSNumber)?.intValue ?? 0
        self.errors = (dict["errors"] as? NSNumber)?.intValue ?? 0
        self.p50Ms = (dict["p50_ms"] as? NSNumber)?.doubleValue ?? 0
        self.p95Ms = (dict["p95_ms"] as? NSNumber)?.doubleValue ?? 0
    }
}

/// One row of the recent-requests feed (oldest first in the snapshot's
/// `recent`; the popup renders newest at the top via feedRows).
struct RecentRequest: Equatable {
    var ts: Double
    var client: String
    var route: String
    var routeEffective: String
    var requestedModel: String
    var upstreamModel: String
    var status: Int
    var durationMs: Double
    var subagent: Bool

    init(dict: [String: Any]) {
        self.ts = (dict["ts"] as? NSNumber)?.doubleValue ?? 0
        self.client = dict["client"] as? String ?? ""
        self.route = dict["route"] as? String ?? ""
        self.routeEffective = dict["route_effective"] as? String ?? ""
        self.requestedModel = dict["requested_model"] as? String ?? ""
        self.upstreamModel = dict["upstream_model"] as? String ?? ""
        self.status = (dict["status"] as? NSNumber)?.intValue ?? 0
        self.durationMs = (dict["duration_ms"] as? NSNumber)?.doubleValue ?? 0
        self.subagent = dict["subagent"] as? Bool ?? false
    }
}

/// The full stats payload. The glance strip renders exclusively from
/// the last fetched snapshot (keep-last-value replay).
struct StatsSnapshot: Equatable {
    var windowSeconds: Int
    var bucketSeconds: Int
    var buckets: [StatsBucket]
    var fallbackActive: [String: Bool]
    var recent: [RecentRequest]

    init(dict: [String: Any]) {
        self.windowSeconds = (dict["window_seconds"] as? NSNumber)?.intValue ?? 0
        self.bucketSeconds = (dict["bucket_seconds"] as? NSNumber)?.intValue ?? 0
        let bucketArr = dict["buckets"] as? [[String: Any]] ?? []
        self.buckets = bucketArr.map { StatsBucket(dict: $0) }
        var fallback: [String: Bool] = [:]
        for (name, flag) in dict["fallback_active"] as? [String: Any] ?? [:] {
            fallback[name] = (flag as? Bool) ?? false
        }
        self.fallbackActive = fallback
        let recentArr = dict["recent"] as? [[String: Any]] ?? []
        self.recent = recentArr.map { RecentRequest(dict: $0) }
    }
}
