import Foundation

struct DoorFallbackRule: Decodable, Equatable, Sendable {
    let prefix: String
    let base: String

    private enum CodingKeys: String, CodingKey {
        case prefix
        case base
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        prefix = try values.decodeIfPresent(String.self, forKey: .prefix) ?? ""
        base = try values.decodeIfPresent(String.self, forKey: .base) ?? ""
    }
}

struct DoorStatusPayload: Decodable, Equatable, Sendable {
    let port: Int
    let shape: String
    let router: String
    let fallbackBase: String
    let fallbacks: [DoorFallbackRule]
    let tripped: Bool
    let cooldownRemainingMilliseconds: Int64
    let lastTransitionUnix: Int64
    let version: String

    private enum CodingKeys: String, CodingKey {
        case port
        case shape
        case router
        case fallbackBase = "fallback_base"
        case fallbacks
        case tripped
        case cooldownRemainingMilliseconds = "cooldown_remaining_ms"
        case lastTransitionUnix = "last_transition_unix"
        case version
    }

    init(
        port: Int,
        shape: String,
        router: String,
        fallbackBase: String,
        fallbacks: [DoorFallbackRule] = [],
        tripped: Bool,
        cooldownRemainingMilliseconds: Int64,
        lastTransitionUnix: Int64,
        version: String
    ) {
        self.port = port
        self.shape = shape
        self.router = router
        self.fallbackBase = fallbackBase
        self.fallbacks = fallbacks
        self.tripped = tripped
        self.cooldownRemainingMilliseconds = cooldownRemainingMilliseconds
        self.lastTransitionUnix = lastTransitionUnix
        self.version = version
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        port = try values.decodeIfPresent(Int.self, forKey: .port) ?? 0
        shape = try values.decodeIfPresent(String.self, forKey: .shape) ?? ""
        router = try values.decodeIfPresent(String.self, forKey: .router) ?? ""
        fallbackBase = try values.decodeIfPresent(
            String.self,
            forKey: .fallbackBase) ?? ""
        fallbacks = try values.decodeIfPresent(
            [DoorFallbackRule].self,
            forKey: .fallbacks) ?? []
        tripped = try values.decodeIfPresent(Bool.self, forKey: .tripped) ?? false
        cooldownRemainingMilliseconds = try values.decodeIfPresent(
            Int64.self,
            forKey: .cooldownRemainingMilliseconds) ?? 0
        lastTransitionUnix = try values.decodeIfPresent(
            Int64.self,
            forKey: .lastTransitionUnix) ?? 0
        version = try values.decodeIfPresent(String.self, forKey: .version) ?? ""
    }
}

struct DoorProbeSnapshot: Identifiable, Equatable, Sendable {
    let url: URL
    let status: DoorStatusPayload?
    let errorMessage: String?

    var id: URL { url }
    var isReachable: Bool { status != nil }
}

protocol DoorStatusReading: Sendable {
    func fetchDoorStatus(at url: URL) async throws -> DoorStatusPayload
}

final class DoorStatusAPIClient: DoorStatusReading, @unchecked Sendable {
    private let session: URLSession
    private let decoder = JSONDecoder()

    init(session: URLSession? = nil) {
        if let session {
            self.session = session
        } else {
            let configuration = URLSessionConfiguration.ephemeral
            configuration.timeoutIntervalForRequest = 1
            configuration.timeoutIntervalForResource = 2
            configuration.waitsForConnectivity = false
            configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
            configuration.urlCache = nil
            self.session = URLSession(configuration: configuration)
        }
    }

    func fetchDoorStatus(at url: URL) async throws -> DoorStatusPayload {
        let (data, response) = try await session.data(from: url)
        guard let http = response as? HTTPURLResponse,
              (200..<300).contains(http.statusCode) else {
            throw GatewayClientError.badResponse(
                (response as? HTTPURLResponse)?.statusCode ?? -1)
        }
        do {
            return try decoder.decode(DoorStatusPayload.self, from: data)
        } catch {
            throw GatewayClientError.invalidPayload
        }
    }
}

private struct DisabledDoorStatusReader: DoorStatusReading {
    func fetchDoorStatus(at url: URL) async throws -> DoorStatusPayload {
        throw CancellationError()
    }
}

actor DoorFetchCoordinator {
    private let reader: any DoorStatusReading

    init(reader: any DoorStatusReading) {
        self.reader = reader
    }

    func fetch(urls: [URL]) async -> [DoorProbeSnapshot] {
        await withTaskGroup(
            of: (Int, DoorProbeSnapshot).self
        ) { group in
            for (index, url) in urls.enumerated() {
                group.addTask { [reader] in
                    do {
                        return (
                            index,
                            DoorProbeSnapshot(
                                url: url,
                                status: try await reader.fetchDoorStatus(at: url),
                                errorMessage: nil))
                    } catch {
                        return (
                            index,
                            DoorProbeSnapshot(
                                url: url,
                                status: nil,
                                errorMessage: doorProbeErrorMessage(error)))
                    }
                }
            }

            var results: [(Int, DoorProbeSnapshot)] = []
            for await result in group {
                results.append(result)
            }
            return results
                .sorted { $0.0 < $1.0 }
                .map(\.1)
        }
    }
}

@MainActor
final class DoorStatusStore: ObservableObject {
    @Published private(set) var probes: [DoorProbeSnapshot] = []
    @Published private(set) var isRefreshing = false
    @Published private(set) var lastUpdated: Date?

    private let urls: [URL]
    private let coordinator: DoorFetchCoordinator
    private let refreshInterval: TimeInterval
    private var refreshTask: Task<Void, Never>?
    private var pollingTask: Task<Void, Never>?
    private var refreshPending = false
    private var generation: UInt64 = 0
    private(set) var isVisible = false

    init(
        urls: [URL],
        reader: any DoorStatusReading,
        refreshInterval: TimeInterval = 5
    ) {
        self.urls = urls
        coordinator = DoorFetchCoordinator(reader: reader)
        self.refreshInterval = refreshInterval
    }

    func start() {
        guard !isVisible else { return }
        isVisible = true
        requestRefresh()
        pollingTask = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                do {
                    try await Task.sleep(
                        nanoseconds: UInt64(
                            max(self.refreshInterval, 0.1) * 1_000_000_000))
                } catch {
                    return
                }
                guard !Task.isCancelled else { return }
                self.requestRefresh()
            }
        }
    }

    func stop() {
        isVisible = false
        pollingTask?.cancel()
        pollingTask = nil
        refreshTask?.cancel()
        refreshTask = nil
        refreshPending = false
        isRefreshing = false
    }

    func requestRefresh() {
        guard isVisible, !urls.isEmpty else { return }
        guard refreshTask == nil else {
            refreshPending = true
            return
        }

        generation &+= 1
        let requestGeneration = generation
        isRefreshing = true
        refreshTask = Task { [weak self] in
            guard let self else { return }
            let result = await self.coordinator.fetch(urls: self.urls)
            guard !Task.isCancelled else { return }
            self.complete(result, generation: requestGeneration)
        }
    }

    private func complete(
        _ result: [DoorProbeSnapshot],
        generation requestGeneration: UInt64
    ) {
        guard requestGeneration == generation else { return }
        refreshTask = nil
        isRefreshing = false
        probes = result
        lastUpdated = Date()

        if refreshPending, isVisible {
            refreshPending = false
            requestRefresh()
        }
    }
}

func doorProbeErrorMessage(_ error: Error) -> String {
    if let gatewayError = error as? GatewayClientError {
        switch gatewayError {
        case .badResponse(let status):
            return status > 0
                ? "Door returned HTTP \(status)"
                : "Door did not return HTTP"
        case .invalidPayload:
            return "Door returned an invalid status payload"
        }
    }
    return "Door is unreachable"
}

func doorStateLabel(_ probe: DoorProbeSnapshot) -> String {
    guard let status = probe.status else { return "Unreachable" }
    return status.tripped ? "Native fallback active" : "Router path ready"
}

func doorCooldownLabel(milliseconds: Int64) -> String {
    guard milliseconds > 0 else { return "None" }
    let seconds = max(1, Int(ceil(Double(milliseconds) / 1_000)))
    return "\(seconds)s remaining"
}

func doorTransitionLabel(unix: Int64, now: Date = Date()) -> String {
    guard unix > 0 else { return "No transition recorded" }
    let elapsed = max(
        0,
        Int(now.timeIntervalSince1970 - TimeInterval(unix)))
    if elapsed < 60 { return "\(elapsed)s ago" }
    if elapsed < 3_600 { return "\(elapsed / 60)m ago" }
    if elapsed < 86_400 { return "\(elapsed / 3_600)h ago" }
    return "\(elapsed / 86_400)d ago"
}

func previewDoorStatusReader(isPreviewFixture: Bool) -> any DoorStatusReading {
    isPreviewFixture ? DisabledDoorStatusReader() : DoorStatusAPIClient()
}
