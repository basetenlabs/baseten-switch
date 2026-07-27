import Foundation

protocol RequestsReading: Sendable {
    func fetchRequests(
        filter: RequestsFilter,
        cursor: String?
    ) async throws -> RequestsSnapshot
}

final class RequestsAPIClient: RequestsReading, @unchecked Sendable {
    private let runtime: RuntimeProfile
    private let session: URLSession
    private let decoder = JSONDecoder()

    init(runtime: RuntimeProfile, session: URLSession? = nil) {
        self.runtime = runtime
        if let session {
            self.session = session
        } else {
            let configuration = URLSessionConfiguration.ephemeral
            configuration.timeoutIntervalForRequest = 5
            configuration.timeoutIntervalForResource = 10
            configuration.waitsForConnectivity = false
            configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
            configuration.urlCache = nil
            self.session = URLSession(configuration: configuration)
        }
    }

    func fetchRequests(
        filter: RequestsFilter,
        cursor: String?
    ) async throws -> RequestsSnapshot {
        var url = adminBaseURL(runtime: runtime)
        url.appendPathComponent("v1/admin/requests")
        guard var components = URLComponents(
            url: url,
            resolvingAgainstBaseURL: false
        ) else {
            throw GatewayClientError.invalidPayload
        }
        var queryItems = [
            URLQueryItem(name: "filter", value: filter.rawValue),
        ]
        if let cursor, !cursor.isEmpty {
            queryItems.append(URLQueryItem(name: "cursor", value: cursor))
        }
        components.queryItems = queryItems
        guard let requestURL = components.url else {
            throw GatewayClientError.invalidPayload
        }

        let (data, response) = try await session.data(from: requestURL)
        guard let http = response as? HTTPURLResponse,
              (200..<300).contains(http.statusCode) else {
            throw GatewayClientError.badResponse(
                (response as? HTTPURLResponse)?.statusCode ?? -1)
        }
        do {
            let snapshot = try decoder.decode(RequestsSnapshot.self, from: data)
            guard !snapshot.hasMore
                    || !(snapshot.nextCursor ?? "").isEmpty else {
                throw GatewayClientError.invalidPayload
            }
            return normalizedRequestsSnapshot(snapshot)
        } catch let error as GatewayClientError {
            throw error
        } catch {
            throw GatewayClientError.invalidPayload
        }
    }
}

actor RequestsFetchCoordinator {
    private let reader: any RequestsReading

    init(reader: any RequestsReading) {
        self.reader = reader
    }

    func fetch(
        filter: RequestsFilter,
        cursor: String?
    ) async throws -> RequestsSnapshot {
        try Task.checkCancellation()
        return try await reader.fetchRequests(filter: filter, cursor: cursor)
    }
}

@MainActor
final class RequestsStore: ObservableObject {
    @Published private(set) var snapshot: RequestsSnapshot?
    @Published private(set) var isLoading = false
    @Published private(set) var isRefreshing = false
    @Published private(set) var isLoadingOlder = false
    @Published private(set) var isStale = false
    @Published private(set) var errorMessage: String?
    @Published private(set) var lastUpdated: Date?
    @Published var filter: RequestsFilter = .all {
        didSet {
            guard filter != oldValue else { return }
            resetForFilterChange()
        }
    }

    var canLoadOlder: Bool {
        snapshot?.hasMore == true
            && !(snapshot?.nextCursor ?? "").isEmpty
            && !isLoadingOlder
    }

    private enum Operation: Equatable {
        case refresh
        case older(cursor: String)
    }

    private let coordinator: RequestsFetchCoordinator
    private let refreshInterval: TimeInterval
    private var requestTask: Task<Void, Never>?
    private var pollingTask: Task<Void, Never>?
    private var refreshPending = false
    private var hasLoadedOlder = false
    private var generation: UInt64 = 0
    private(set) var isVisible = false

    init(
        reader: any RequestsReading,
        refreshInterval: TimeInterval = 5
    ) {
        coordinator = RequestsFetchCoordinator(reader: reader)
        self.refreshInterval = refreshInterval
    }

    func start() {
        guard !isVisible else { return }
        isVisible = true
        refresh()
        pollingTask = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                do {
                    try await Task.sleep(
                        nanoseconds: UInt64(
                            max(self.refreshInterval, 0.1)
                                * 1_000_000_000))
                } catch {
                    return
                }
                guard !Task.isCancelled else { return }
                self.refresh()
            }
        }
    }

    func stop() {
        isVisible = false
        pollingTask?.cancel()
        pollingTask = nil
        requestTask?.cancel()
        requestTask = nil
        refreshPending = false
        isLoading = false
        isRefreshing = false
        isLoadingOlder = false
    }

    func refresh() {
        guard isVisible else { return }
        guard requestTask == nil else {
            refreshPending = true
            return
        }
        begin(.refresh)
    }

    func loadOlder() {
        guard isVisible,
              requestTask == nil,
              let cursor = snapshot?.nextCursor,
              !cursor.isEmpty,
              snapshot?.hasMore == true else {
            return
        }
        begin(.older(cursor: cursor))
    }

    private func begin(_ operation: Operation) {
        generation &+= 1
        let requestGeneration = generation
        let requestFilter = filter
        let cursor: String?
        switch operation {
        case .refresh:
            cursor = nil
            isLoading = snapshot == nil
            isRefreshing = snapshot != nil
        case .older(let value):
            cursor = value
            isLoadingOlder = true
        }

        requestTask = Task { [weak self] in
            guard let self else { return }
            let result: Result<RequestsSnapshot, Error>
            do {
                result = .success(
                    try await self.coordinator.fetch(
                        filter: requestFilter,
                        cursor: cursor))
            } catch {
                result = .failure(error)
            }
            guard !Task.isCancelled else { return }
            self.complete(
                result,
                operation: operation,
                filter: requestFilter,
                generation: requestGeneration)
        }
    }

    private func complete(
        _ result: Result<RequestsSnapshot, Error>,
        operation: Operation,
        filter requestedFilter: RequestsFilter,
        generation requestGeneration: UInt64
    ) {
        guard requestGeneration == generation else { return }
        requestTask = nil
        isLoading = false
        isRefreshing = false
        isLoadingOlder = false

        guard requestedFilter == filter else {
            requestNextRefreshIfNeeded()
            return
        }

        switch result {
        case .success(let page):
            switch operation {
            case .refresh:
                snapshot = mergeRequests(
                    current: snapshot,
                    incoming: page,
                    preserveCurrentCursor: hasLoadedOlder)
            case .older(let requestedCursor):
                guard !page.hasMore
                        || page.nextCursor != requestedCursor else {
                    isStale = snapshot != nil
                    errorMessage =
                        "The gateway returned a repeated request cursor."
                    requestNextRefreshIfNeeded()
                    return
                }
                snapshot = mergeRequests(
                    current: snapshot,
                    incoming: page,
                    preserveCurrentCursor: false)
                hasLoadedOlder = true
            }
            lastUpdated = Date()
            isStale = false
            errorMessage = nil
        case .failure(let error):
            isStale = snapshot != nil
            errorMessage = requestsErrorMessage(error)
        }
        requestNextRefreshIfNeeded()
    }

    private func requestNextRefreshIfNeeded() {
        if refreshPending, isVisible {
            refreshPending = false
            refresh()
        }
    }

    private func resetForFilterChange() {
        generation &+= 1
        requestTask?.cancel()
        requestTask = nil
        refreshPending = false
        hasLoadedOlder = false
        snapshot = nil
        isLoading = false
        isRefreshing = false
        isLoadingOlder = false
        isStale = false
        errorMessage = nil
        lastUpdated = nil
        if isVisible {
            refresh()
        }
    }
}

func normalizedRequestsSnapshot(
    _ snapshot: RequestsSnapshot
) -> RequestsSnapshot {
    RequestsSnapshot(
        items: deduplicatedRequests(snapshot.items),
        nextCursor: snapshot.nextCursor,
        hasMore: snapshot.hasMore,
        coverage: snapshot.coverage)
}

func mergeRequests(
    current: RequestsSnapshot?,
    incoming: RequestsSnapshot,
    preserveCurrentCursor: Bool
) -> RequestsSnapshot {
    guard let current else {
        return normalizedRequestsSnapshot(incoming)
    }
    let coverage = mergedRequestCoverage(current.coverage, incoming.coverage)
    return RequestsSnapshot(
        items: deduplicatedRequests(current.items + incoming.items),
        nextCursor: preserveCurrentCursor
            ? current.nextCursor
            : incoming.nextCursor,
        hasMore: preserveCurrentCursor
            ? current.hasMore
            : incoming.hasMore,
        coverage: coverage)
}

func deduplicatedRequests(_ items: [RequestItem]) -> [RequestItem] {
    var byID: [String: RequestItem] = [:]
    for item in items {
        byID[item.eventID] = item
    }
    return byID.values.sorted {
        if $0.completedAt != $1.completedAt {
            return $0.completedAt > $1.completedAt
        }
        return $0.eventID > $1.eventID
    }
}

private func mergedRequestCoverage(
    _ current: RequestCoverage,
    _ incoming: RequestCoverage
) -> RequestCoverage {
    guard !current.complete || !incoming.complete else {
        return RequestCoverage()
    }
    let reasons = [current.reason, incoming.reason]
        .filter { !$0.isEmpty }
        .reduce(into: [String]()) { result, reason in
            if !result.contains(reason) {
                result.append(reason)
            }
        }
    return RequestCoverage(
        complete: false,
        reason: reasons.joined(separator: "; "))
}

func requestsErrorMessage(_ error: Error) -> String {
    if let gatewayError = error as? GatewayClientError {
        switch gatewayError {
        case .badResponse(let status):
            if status == 404 {
                return "Request history is not available in this gateway build."
            }
            return "The gateway returned HTTP \(status)."
        case .invalidPayload:
            return "The gateway returned request data the app could not read."
        }
    }
    if error is CancellationError {
        return ""
    }
    return "Requests could not be refreshed."
}
