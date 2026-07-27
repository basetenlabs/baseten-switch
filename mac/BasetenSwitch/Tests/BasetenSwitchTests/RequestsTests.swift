import Foundation
import XCTest
@testable import BasetenSwitch

private enum RequestsTestError: Error {
    case unavailable
}

private final class RequestsURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest)
        -> URLRequest {
        request
    }

    override func startLoading() {
        do {
            guard let handler = Self.handler else {
                throw RequestsTestError.unavailable
            }
            let (response, data) = try handler(request)
            client?.urlProtocol(
                self,
                didReceive: response,
                cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

private actor RequestsSequenceReader: RequestsReading {
    enum Outcome: Sendable {
        case snapshot(RequestsSnapshot)
        case failure
    }

    struct Call: Equatable, Sendable {
        var filter: RequestsFilter
        var cursor: String?
    }

    private var outcomes: [Outcome]
    private let delay: UInt64
    private(set) var calls: [Call] = []
    private(set) var active = 0
    private(set) var maximumActive = 0

    init(
        outcomes: [Outcome],
        delayNanoseconds: UInt64 = 0
    ) {
        self.outcomes = outcomes
        delay = delayNanoseconds
    }

    func fetchRequests(
        filter: RequestsFilter,
        cursor: String?
    ) async throws -> RequestsSnapshot {
        calls.append(Call(filter: filter, cursor: cursor))
        active += 1
        maximumActive = max(maximumActive, active)
        defer { active -= 1 }
        if delay > 0 {
            try await Task.sleep(nanoseconds: delay)
        }
        let outcome = outcomes.isEmpty
            ? .failure
            : outcomes.removeFirst()
        switch outcome {
        case .snapshot(let snapshot):
            return snapshot
        case .failure:
            throw RequestsTestError.unavailable
        }
    }
}

final class RequestsTests: XCTestCase {
    override func tearDown() {
        RequestsURLProtocol.handler = nil
        super.tearDown()
    }

    func testApprovedFixtureDecodesCompactRequestContract() throws {
        let snapshot = try approvedFixture()

        XCTAssertEqual(snapshot.items.map(\.eventID), [
            "request-newest",
            "request-older",
        ])
        XCTAssertEqual(snapshot.nextCursor, "older-page-token")
        XCTAssertTrue(snapshot.hasMore)
        XCTAssertFalse(snapshot.coverage.complete)
        XCTAssertEqual(
            snapshot.coverage.reason,
            "history truncated by retention")

        let fallback = try XCTUnwrap(snapshot.items.first?.fallback)
        XCTAssertTrue(fallback.attempted)
        XCTAssertEqual(fallback.count, 1)
        XCTAssertEqual(fallback.trigger, "image_input_unsupported")
        XCTAssertTrue(snapshot.items[0].isFallback)
        XCTAssertNil(snapshot.items[1].status)
        XCTAssertTrue(snapshot.items[1].isError)
    }

    func testFallbackReasonLabelsMakeImageFallbackProminent() {
        XCTAssertEqual(
            requestFallbackLabel("image_input_unsupported"),
            "Image input unsupported")
        XCTAssertEqual(
            requestFallbackLabel("ttft_timeout"),
            "First token timeout")
        XCTAssertEqual(
            requestFallbackLabel("http_503"),
            "HTTP 503")
        XCTAssertEqual(
            requestFallbackLabel("future_reason"),
            "Future Reason")
        XCTAssertEqual(requestFallbackLabel(nil), "Fallback")
    }

    func testRequestsClientUsesFilterAndCursorQuery() async throws {
        let fixtureData = try Data(contentsOf: approvedFixtureURL())
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RequestsURLProtocol.self]
        let session = URLSession(configuration: configuration)
        let runtime = RuntimeProfile(
            adminBaseURL: URL(string: "http://127.0.0.1:45373")!,
            doorURLs: [],
            expectedConfigPath: nil,
            environment: [:])
        RequestsURLProtocol.handler = { request in
            let url = try XCTUnwrap(request.url)
            XCTAssertEqual(url.path, "/v1/admin/requests")
            let items = URLComponents(
                url: url,
                resolvingAgainstBaseURL: false)?.queryItems
            XCTAssertEqual(
                items?.first(where: { $0.name == "filter" })?.value,
                "fallbacks")
            XCTAssertEqual(
                items?.first(where: { $0.name == "cursor" })?.value,
                "page-two")
            return (
                HTTPURLResponse(
                    url: url,
                    statusCode: 200,
                    httpVersion: nil,
                    headerFields: nil)!,
                fixtureData)
        }

        let result = try await RequestsAPIClient(
            runtime: runtime,
            session: session)
            .fetchRequests(filter: .fallbacks, cursor: "page-two")

        XCTAssertEqual(result.items.first?.eventID, "request-newest")
    }

    func testDecodeAcceptsWholeSecondAndUnixTimestamps() throws {
        let stringItem = try requestItem(
            id: "whole-second",
            completedAt: #""2026-07-26T18:01:00Z""#)
        let unixItem = try requestItem(
            id: "unix",
            completedAt: "1785088860.25")

        XCTAssertEqual(
            stringItem.completedAt.timeIntervalSince1970,
            1_785_088_860,
            accuracy: 0.001)
        XCTAssertEqual(
            unixItem.completedAt.timeIntervalSince1970,
            1_785_088_860.25,
            accuracy: 0.001)
    }

    func testMergeSortsNewestFirstAndDeduplicatesByEventID() {
        let oldVersion = item(
            id: "same",
            timestamp: 100,
            status: 500)
        let newerEvent = item(id: "new", timestamp: 300)
        let refreshedVersion = item(
            id: "same",
            timestamp: 200,
            status: 200)
        let current = snapshot(
            items: [oldVersion],
            cursor: "old-boundary",
            hasMore: true)
        let incoming = snapshot(
            items: [newerEvent, refreshedVersion],
            cursor: "new-boundary",
            hasMore: true)

        let merged = mergeRequests(
            current: current,
            incoming: incoming,
            preserveCurrentCursor: true)

        XCTAssertEqual(merged.items.map(\.eventID), ["new", "same"])
        XCTAssertEqual(merged.items.last?.status, 200)
        XCTAssertEqual(merged.nextCursor, "old-boundary")
    }

    @MainActor
    func testFilterChangeClearsPagesAndFetchesSelectedFilter() async {
        let reader = RequestsSequenceReader(outcomes: [
            .snapshot(snapshot(items: [item(id: "all", timestamp: 100)])),
            .snapshot(snapshot(items: [
                item(
                    id: "fallback",
                    timestamp: 200,
                    fallback: RequestFallback(
                        attempted: true,
                        count: 1,
                        trigger: "image_input_unsupported")),
            ])),
        ])
        let store = RequestsStore(
            reader: reader,
            refreshInterval: 3_600)

        store.start()
        await waitUntil { store.snapshot?.items.first?.eventID == "all" }
        store.filter = .fallbacks
        XCTAssertNil(store.snapshot)
        await waitUntil {
            store.snapshot?.items.first?.eventID == "fallback"
        }

        let calls = await reader.calls
        XCTAssertEqual(calls, [
            RequestsSequenceReader.Call(filter: .all, cursor: nil),
            RequestsSequenceReader.Call(filter: .fallbacks, cursor: nil),
        ])
        store.stop()
    }

    @MainActor
    func testStoreLoadsOlderAndKeepsCursorBoundaryDuringPolling()
        async {
        let reader = RequestsSequenceReader(outcomes: [
            .snapshot(snapshot(
                items: [item(id: "initial", timestamp: 200)],
                cursor: "page-two",
                hasMore: true)),
            .snapshot(snapshot(
                items: [item(id: "older", timestamp: 100)],
                cursor: "page-three",
                hasMore: true)),
            .snapshot(snapshot(
                items: [item(id: "new", timestamp: 300)],
                cursor: "shifted-first-page",
                hasMore: true)),
        ])
        let store = RequestsStore(
            reader: reader,
            refreshInterval: 3_600)

        store.start()
        await waitUntil { store.snapshot?.items.count == 1 }
        store.loadOlder()
        await waitUntil { store.snapshot?.items.count == 2 }
        store.refresh()
        await waitUntil { store.snapshot?.items.count == 3 }

        XCTAssertEqual(
            store.snapshot?.items.map(\.eventID),
            ["new", "initial", "older"])
        XCTAssertEqual(store.snapshot?.nextCursor, "page-three")
        let calls = await reader.calls
        XCTAssertEqual(calls.map(\.cursor), [
            nil,
            "page-two",
            nil,
        ])
        store.stop()
    }

    @MainActor
    func testStoreKeepsLastGoodPageAndMarksItStale() async {
        let good = snapshot(items: [item(id: "good", timestamp: 100)])
        let reader = RequestsSequenceReader(outcomes: [
            .snapshot(good),
            .failure,
        ])
        let store = RequestsStore(
            reader: reader,
            refreshInterval: 3_600)

        store.start()
        await waitUntil { store.snapshot != nil }
        store.refresh()
        await waitUntil { store.isStale }

        XCTAssertEqual(store.snapshot, good)
        XCTAssertNotNil(store.errorMessage)
        store.stop()
    }

    @MainActor
    func testStoreCoalescesPollingAndManualRefreshes() async {
        let page = snapshot(items: [item(id: "one", timestamp: 100)])
        let reader = RequestsSequenceReader(
            outcomes: [
                .snapshot(page),
                .snapshot(page),
            ],
            delayNanoseconds: 40_000_000)
        let store = RequestsStore(
            reader: reader,
            refreshInterval: 3_600)

        store.start()
        store.refresh()
        store.refresh()
        await waitUntil {
            let calls = await reader.calls
            return calls.count == 2
                && !store.isRefreshing
                && !store.isLoading
        }

        let maximumActive = await reader.maximumActive
        XCTAssertEqual(maximumActive, 1)
        store.stop()
        XCTAssertFalse(store.isVisible)
    }

    private func approvedFixture() throws -> RequestsSnapshot {
        let data = try Data(contentsOf: approvedFixtureURL())
        return try JSONDecoder().decode(RequestsSnapshot.self, from: data)
    }

    private func approvedFixtureURL() -> URL {
        var url = URL(fileURLWithPath: #filePath)
        for _ in 0..<2 {
            url.deleteLastPathComponent()
        }
        url.appendPathComponent("Fixtures/native-requests-mock.json")
        return url
    }

    private func requestItem(
        id: String,
        completedAt: String
    ) throws -> RequestItem {
        let data = Data(
            """
            {
              "event_id": "\(id)",
              "completed_at": \(completedAt)
            }
            """.utf8)
        return try JSONDecoder().decode(RequestItem.self, from: data)
    }

    private func item(
        id: String,
        timestamp: TimeInterval,
        status: Int? = 200,
        fallback: RequestFallback? = nil
    ) -> RequestItem {
        RequestItem(
            eventID: id,
            completedAt: Date(timeIntervalSince1970: timestamp),
            client: "codex",
            configuredRoute: "baseten",
            effectiveProvider: "baseten",
            requestedModel: "requested",
            servedModel: "served",
            status: status,
            durationMs: 10,
            terminationReason: status == nil ? nil : "completed",
            subagent: false,
            fallback: fallback)
    }

    private func snapshot(
        items: [RequestItem],
        cursor: String? = nil,
        hasMore: Bool = false,
        coverage: RequestCoverage = RequestCoverage()
    ) -> RequestsSnapshot {
        RequestsSnapshot(
            items: items,
            nextCursor: cursor,
            hasMore: hasMore,
            coverage: coverage)
    }

    @MainActor
    private func waitUntil(
        timeout: TimeInterval = 2,
        _ condition: @escaping () async -> Bool
    ) async {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if await condition() {
                return
            }
            try? await Task.sleep(nanoseconds: 10_000_000)
        }
        XCTFail("Timed out waiting for RequestsStore state")
    }
}
