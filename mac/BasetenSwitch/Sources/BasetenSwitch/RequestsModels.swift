import Foundation

enum RequestsFilter: String, CaseIterable, Identifiable, Sendable {
    case all
    case fallbacks
    case errors

    var id: String { rawValue }

    var label: String {
        switch self {
        case .all: "All"
        case .fallbacks: "Fallbacks"
        case .errors: "Errors"
        }
    }
}

struct RequestCoverage: Decodable, Equatable, Sendable {
    var complete: Bool
    var reason: String

    enum CodingKeys: String, CodingKey {
        case complete
        case reason
    }

    init(complete: Bool = true, reason: String = "") {
        self.complete = complete
        self.reason = reason
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        complete = try values.decode(Bool.self, forKey: .complete)
        reason = try values.decodeIfPresent(String.self, forKey: .reason) ?? ""
    }
}

struct RequestFallback: Decodable, Equatable, Sendable {
    var attempted: Bool
    var count: Int
    var trigger: String?

    enum CodingKeys: String, CodingKey {
        case attempted
        case count
        case trigger
    }

    init(attempted: Bool, count: Int = 0, trigger: String? = nil) {
        self.attempted = attempted
        self.count = count
        self.trigger = trigger
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        attempted = try values.decodeIfPresent(
            Bool.self,
            forKey: .attempted) ?? false
        count = try values.decodeIfPresent(Int.self, forKey: .count) ?? 0
        trigger = try values.decodeIfPresent(String.self, forKey: .trigger)
    }
}

struct RequestItem: Decodable, Equatable, Identifiable, Sendable {
    var eventID: String
    var completedAt: Date
    var client: String
    var configuredRoute: String
    var effectiveProvider: String
    var requestedModel: String
    var servedModel: String
    var status: Int?
    var durationMs: Int64?
    var terminationReason: String?
    var subagent: Bool
    var fallback: RequestFallback?

    var id: String { eventID }

    var isFallback: Bool {
        guard let fallback else { return false }
        return fallback.attempted
            || fallback.count > 0
            || !(fallback.trigger ?? "").isEmpty
    }

    var isError: Bool {
        if let status, status >= 400 {
            return true
        }
        guard let terminationReason, !terminationReason.isEmpty else {
            return false
        }
        return terminationReason != "completed"
    }

    enum CodingKeys: String, CodingKey {
        case eventID = "event_id"
        case completedAt = "completed_at"
        case client
        case configuredRoute = "configured_route"
        case effectiveProvider = "effective_provider"
        case requestedModel = "requested_model"
        case servedModel = "served_model"
        case status
        case durationMs = "duration_ms"
        case terminationReason = "termination_reason"
        case subagent
        case fallback
    }

    init(
        eventID: String,
        completedAt: Date,
        client: String,
        configuredRoute: String,
        effectiveProvider: String,
        requestedModel: String,
        servedModel: String,
        status: Int?,
        durationMs: Int64?,
        terminationReason: String?,
        subagent: Bool,
        fallback: RequestFallback?
    ) {
        self.eventID = eventID
        self.completedAt = completedAt
        self.client = client
        self.configuredRoute = configuredRoute
        self.effectiveProvider = effectiveProvider
        self.requestedModel = requestedModel
        self.servedModel = servedModel
        self.status = status
        self.durationMs = durationMs
        self.terminationReason = terminationReason
        self.subagent = subagent
        self.fallback = fallback
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        eventID = try values.decode(String.self, forKey: .eventID)
        completedAt = try values.decodeRequestDate(forKey: .completedAt)
        client = try values.decodeIfPresent(String.self, forKey: .client) ?? ""
        configuredRoute = try values.decodeIfPresent(
            String.self,
            forKey: .configuredRoute) ?? ""
        effectiveProvider = try values.decodeIfPresent(
            String.self,
            forKey: .effectiveProvider) ?? ""
        requestedModel = try values.decodeIfPresent(
            String.self,
            forKey: .requestedModel) ?? ""
        servedModel = try values.decodeIfPresent(
            String.self,
            forKey: .servedModel) ?? ""
        status = try values.decodeIfPresent(Int.self, forKey: .status)
        durationMs = try values.decodeIfPresent(Int64.self, forKey: .durationMs)
        terminationReason = try values.decodeIfPresent(
            String.self,
            forKey: .terminationReason)
        subagent = try values.decodeIfPresent(
            Bool.self,
            forKey: .subagent) ?? false
        fallback = try values.decodeIfPresent(
            RequestFallback.self,
            forKey: .fallback)
    }
}

struct RequestsSnapshot: Decodable, Equatable, Sendable {
    var items: [RequestItem]
    var nextCursor: String?
    var hasMore: Bool
    var coverage: RequestCoverage

    enum CodingKeys: String, CodingKey {
        case items
        case nextCursor = "next_cursor"
        case hasMore = "has_more"
        case coverage
    }

    init(
        items: [RequestItem],
        nextCursor: String?,
        hasMore: Bool,
        coverage: RequestCoverage
    ) {
        self.items = items
        self.nextCursor = nextCursor
        self.hasMore = hasMore
        self.coverage = coverage
    }
}

func requestFallbackLabel(_ trigger: String?) -> String {
    switch trigger {
    case "image_input_unsupported":
        return "Image input unsupported"
    case "ttft_timeout":
        return "First token timeout"
    case "auth_unavailable":
        return "Authentication unavailable"
    case "reasoning_policy_error":
        return "Reasoning policy error"
    case "transport_error":
        return "Transport error"
    case "cooldown":
        return "Provider cooldown"
    case let value? where value.hasPrefix("http_"):
        return "HTTP \(value.dropFirst("http_".count))"
    case let value? where !value.isEmpty:
        return value
            .split(separator: "_")
            .map { $0.capitalized }
            .joined(separator: " ")
    default:
        return "Fallback"
    }
}

private extension KeyedDecodingContainer {
    func decodeRequestDate(forKey key: Key) throws -> Date {
        if let seconds = try? decode(Double.self, forKey: key) {
            return Date(timeIntervalSince1970: seconds)
        }
        let value = try decode(String.self, forKey: key)
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [
            .withInternetDateTime,
            .withFractionalSeconds,
        ]
        if let date = fractional.date(from: value) {
            return date
        }
        let wholeSeconds = ISO8601DateFormatter()
        wholeSeconds.formatOptions = [.withInternetDateTime]
        if let date = wholeSeconds.date(from: value) {
            return date
        }
        throw DecodingError.dataCorruptedError(
            forKey: key,
            in: self,
            debugDescription: "Expected an RFC 3339 or Unix timestamp")
    }
}
