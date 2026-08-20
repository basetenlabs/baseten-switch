import AppKit
import Foundation
import XCTest
@testable import BasetenSwitch

private enum TrafficTestError: Error {
    case unavailable
}

private final class TrafficURLProtocol: URLProtocol {
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
                throw TrafficTestError.unavailable
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

private actor TrafficSequenceReader: TrafficAnalyticsReading {
    enum Outcome: Sendable {
        case snapshot(TrafficAnalyticsSnapshot)
        case failure
    }

    private var outcomes: [Outcome]
    private let delay: UInt64
    private(set) var calls = 0
    private(set) var active = 0
    private(set) var maximumActive = 0

    init(
        outcomes: [Outcome],
        delayNanoseconds: UInt64 = 0
    ) {
        self.outcomes = outcomes
        delay = delayNanoseconds
    }

    func fetchAnalytics(since: Date, until: Date) async throws
        -> TrafficAnalyticsSnapshot {
        calls += 1
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
            throw TrafficTestError.unavailable
        }
    }
}

final class TrafficTests: XCTestCase {
    override func tearDown() {
        TrafficURLProtocol.handler = nil
        super.tearDown()
    }

    func testTrafficChartMotionRespectsReduceMotion() {
        XCTAssertEqual(TrafficChartMotion.duration, 0.45)
        XCTAssertNotNil(
            TrafficChartMotion.animation(reduceMotion: false))
        XCTAssertNil(
            TrafficChartMotion.animation(reduceMotion: true))
    }

    @MainActor
    func testTrafficDestinationAndToolbarRefreshRouting() {
        let navigation = RouterWindowNavigation()
        navigation.prepareForShow(destination: .traffic)

        XCTAssertEqual(navigation.selection, .traffic)
        XCTAssertTrue(
            toolbarRefreshesTraffic(selection: navigation.selection))
        XCTAssertFalse(
            toolbarRefreshesTraffic(selection: .overview))
        XCTAssertFalse(
            toolbarRefreshesTraffic(selection: .client("claude-code")))
        XCTAssertFalse(
            toolbarRefreshesModelCatalog(selection: .traffic))
        XCTAssertFalse(
            toolbarRefreshesModelCatalog(selection: .overview))
        XCTAssertTrue(
            toolbarRefreshesModelCatalog(
                selection: .client("claude-code")))
    }

    func testApprovedFixtureDecodesFullAnalyticsContract() throws {
        let snapshot = try approvedFixture()

        XCTAssertEqual(snapshot.coverage.requestRows, 1_184)
        XCTAssertTrue(snapshot.coverage.complete)
        XCTAssertEqual(snapshot.cost.summary.actualClaudeCostUSD, 172.4)
        XCTAssertEqual(snapshot.cost.summary.actualBasetenCostUSD, 143.5)
        XCTAssertEqual(snapshot.cost.summary.savedUSD, 595.1)
        XCTAssertEqual(snapshot.cost.providers.map(\.provider), [
            "Claude", "Baseten",
        ])
        XCTAssertEqual(snapshot.cost.models.map(\.label), [
            "Fable", "Opus", "Sonnet", "Haiku", "GLM 5.2", "Kimi K3",
        ])
        XCTAssertEqual(
            snapshot.cost.savings.mappings.count,
            8)
        XCTAssertEqual(snapshot.performance.providers.count, 2)
        XCTAssertEqual(snapshot.performance.models.count, 6)
        XCTAssertEqual(
            snapshot.performance.providers.last?
                .medianOutputTokensPerSecond,
            138)
    }

    func testModelContractKeepsIdentitySeparateFromVisibleLabels() throws {
        let costRows = try JSONDecoder().decode(
            [TrafficCostRow].self,
            from: Data(
                #"""
                [
                  {
                    "provider": "Baseten",
                    "model_id": "org-a/shared",
                    "display_name": "Shared name",
                    "model": "ignored unknown label",
                    "requests": 1,
                    "tokens": 2
                  },
                  {
                    "provider": "Baseten",
                    "model_id": "org-b/shared",
                    "display_name": "Shared name",
                    "requests": 1,
                    "tokens": 2
                  },
                  {
                    "provider": "Baseten",
                    "model_id": "moonshotai/Kimi-K2.7-Code",
                    "requests": 1,
                    "tokens": 2
                  }
                ]
                """#.utf8))

        XCTAssertEqual(costRows[0].modelID, "org-a/shared")
        XCTAssertEqual(costRows[0].displayName, "Shared name")
        XCTAssertEqual(costRows[0].label, "Shared name")
        XCTAssertEqual(costRows[1].label, "Shared name")
        XCTAssertNotEqual(costRows[0].id, costRows[1].id)
        XCTAssertEqual(
            costRows[2].label,
            "moonshotai/Kimi-K2.7-Code",
            "Missing display names fall back to the raw ID without parsing")

        let savings = try JSONDecoder().decode(
            TrafficSavings.self,
            from: Data(
                #"""
                {
                  "by_baseten_model": [
                    {
                      "model_id": "zai-org/GLM-5.2",
                      "display_name": "GLM 5.2",
                      "actual_baseten_cost_usd": 1,
                      "estimated_native_cost_usd": 2,
                      "saved_usd": 1,
                      "saved_percent": 50
                    }
                  ],
                  "mappings": [
                    {
                      "baseten_model_id": "zai-org/GLM-5.2",
                      "baseten_display_name": "GLM 5.2",
                      "requested_claude_family": "Opus",
                      "actual_baseten_cost_usd": 1,
                      "estimated_native_cost_usd": 2
                    }
                  ]
                }
                """#.utf8))

        XCTAssertEqual(
            savings.byBasetenModel[0].id,
            "zai-org/GLM-5.2")
        XCTAssertEqual(savings.byBasetenModel[0].label, "GLM 5.2")
        XCTAssertEqual(
            savings.mappings[0].id,
            "zai-org/GLM-5.2:Opus")
        XCTAssertEqual(savings.mappings[0].label, "GLM 5.2")

        let performance = try JSONDecoder().decode(
            TrafficPerformanceRow.self,
            from: Data(
                #"""
                {
                  "provider": "Baseten",
                  "model_id": "moonshotai/Kimi-K3",
                  "display_name": "Kimi K3",
                  "requests": 1,
                  "tokens": 2,
                  "ttft_samples": 1,
                  "median_ttft_ms": 100,
                  "output_tps_samples": 1,
                  "median_output_tokens_per_second": 50
                }
                """#.utf8))
        XCTAssertEqual(performance.id, "Baseten:moonshotai/Kimi-K3")
        XCTAssertEqual(performance.label, "Kimi K3")
    }

    func testClaudeSparkUsesOfficialPressKitGeometry() throws {
        let path = ClaudeBrandShape().path(
            in: ClaudeBrandShape.sourceViewBox)
        var movePoint: CGPoint?
        var linePoints: [CGPoint] = []
        var curve: (
            to: CGPoint,
            control1: CGPoint,
            control2: CGPoint
        )?
        var closeCount = 0

        path.forEach { element in
            switch element {
            case .move(let point):
                movePoint = point
            case .line(let point):
                linePoints.append(point)
            case .curve(let to, let control1, let control2):
                curve = (to, control1, control2)
            case .quadCurve:
                XCTFail("Official Claude Spark has no quadratic curves")
            case .closeSubpath:
                closeCount += 1
            }
        }

        let start = try XCTUnwrap(movePoint)
        XCTAssertEqual(start.x, 18.7657, accuracy: 0.00001)
        XCTAssertEqual(start.y, 62.4437, accuracy: 0.00001)
        XCTAssertEqual(linePoints.count, 156)
        XCTAssertEqual(closeCount, 1)

        let officialCurve = try XCTUnwrap(curve)
        XCTAssertEqual(officialCurve.to.x, 22.0038, accuracy: 0.00001)
        XCTAssertEqual(officialCurve.to.y, 4.65957, accuracy: 0.00001)
        XCTAssertEqual(
            officialCurve.control1.x,
            22.1538,
            accuracy: 0.00001)
        XCTAssertEqual(
            officialCurve.control1.y,
            6.51831,
            accuracy: 0.00001)
        XCTAssertEqual(
            officialCurve.control2.x,
            22.0038,
            accuracy: 0.00001)
        XCTAssertEqual(
            officialCurve.control2.y,
            5.68714,
            accuracy: 0.00001)

        XCTAssertEqual(path.boundingRect.minX, 0.399902, accuracy: 0.00001)
        XCTAssertEqual(path.boundingRect.minY, 0.200195, accuracy: 0.00001)
        XCTAssertEqual(path.boundingRect.maxX, 93.9999, accuracy: 0.00001)
        XCTAssertEqual(path.boundingRect.maxY, 93.8002, accuracy: 0.00001)
    }

    func testPerformanceCardsMatchApprovedProviderComparison() throws {
        let snapshot = try approvedFixture()
        let claude = try XCTUnwrap(
            snapshot.performance.providers.first {
                $0.provider == "Claude"
            })
        let baseten = try XCTUnwrap(
            snapshot.performance.providers.first {
                $0.provider == "Baseten"
            })

        XCTAssertEqual(
            trafficPerformanceCardContents(
                claude: claude,
                baseten: baseten),
            [
                TrafficPerformanceCardContent(
                    title: "Baseten median TTFT",
                    value: "245 ms",
                    detail: "Claude 610 ms · 2.5× faster",
                    brand: .baseten,
                    detailBrand: .claude),
                TrafficPerformanceCardContent(
                    title: "Baseten median output speed",
                    value: "138 tok/s",
                    detail: "Claude 54 tok/s · 2.6× faster",
                    brand: .baseten,
                    detailBrand: .claude),
                TrafficPerformanceCardContent(
                    title: "Baseten token volume",
                    value: "96.3M",
                    detail: "Claude 18.4M tokens",
                    brand: .baseten,
                    detailBrand: .claude),
            ])
    }

    func testPerformanceComparisonOmitsUnsafeRatios() {
        XCTAssertNil(
            trafficPerformanceSpeedComparison(
                baseten: nil,
                claude: 10,
                lowerIsBetter: true))
        XCTAssertNil(
            trafficPerformanceSpeedComparison(
                baseten: 0,
                claude: 10,
                lowerIsBetter: true))
        XCTAssertNil(
            trafficPerformanceSpeedComparison(
                baseten: 10,
                claude: 0,
                lowerIsBetter: false))
        XCTAssertNil(
            trafficPerformanceSpeedComparison(
                baseten: .infinity,
                claude: 10,
                lowerIsBetter: false))

        let cards = trafficPerformanceCardContents(
            claude: nil,
            baseten: nil)
        XCTAssertEqual(cards[0].detail, "Claude No data")
        XCTAssertEqual(cards[1].detail, "Claude No data")
    }

    func testAnalyticsDecodeToleratesMissingSectionsAndUnknownKeys() throws {
        let data = Data(
            """
            {
              "generated_at": 42,
              "unknown_future_field": {"ignored": true}
            }
            """.utf8)

        let snapshot = try JSONDecoder().decode(
            TrafficAnalyticsSnapshot.self,
            from: data)

        XCTAssertEqual(snapshot.generatedAt, 42)
        XCTAssertTrue(snapshot.isEmpty)
        XCTAssertTrue(snapshot.cost.providers.isEmpty)
        XCTAssertTrue(snapshot.performance.models.isEmpty)
    }

    func testCollectionPausedCoverageDecodesAndExplainsRetainedHistory()
        throws {
        let data = Data(
            """
            {
              "coverage": {
                "collection_enabled": false,
                "request_rows": 12
              }
            }
            """.utf8)

        let snapshot = try JSONDecoder().decode(
            TrafficAnalyticsSnapshot.self,
            from: data)

        XCTAssertFalse(snapshot.coverage.collectionEnabled)
        XCTAssertEqual(snapshot.coverage.requestRows, 12)
        XCTAssertEqual(
            trafficCollectionStatusMessage(snapshot.coverage),
            "Collection paused. Retained traffic history remains available.")
    }

    func testMissingCollectionCoverageDefaultsToEnabled() throws {
        let data = Data(#"{"coverage":{"request_rows":1}}"#.utf8)

        let snapshot = try JSONDecoder().decode(
            TrafficAnalyticsSnapshot.self,
            from: data)

        XCTAssertTrue(snapshot.coverage.collectionEnabled)
        XCTAssertNil(trafficCollectionStatusMessage(snapshot.coverage))
    }

    func testTrafficClientUsesAdminAnalyticsEndpointAndUnixRange()
        async throws {
        let fixtureData = try Data(contentsOf: approvedFixtureURL())
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [TrafficURLProtocol.self]
        let session = URLSession(configuration: configuration)
        let runtime = RuntimeProfile(
            adminBaseURL: URL(string: "http://127.0.0.1:45373")!,
            doorURLs: [],
            expectedConfigPath: nil,
            environment: [:])
        TrafficURLProtocol.handler = { request in
            let url = try XCTUnwrap(request.url)
            XCTAssertEqual(url.path, "/v1/admin/analytics")
            let items = URLComponents(
                url: url,
                resolvingAgainstBaseURL: false)?.queryItems
            XCTAssertEqual(
                items?.first(where: { $0.name == "since" })?.value,
                "100")
            XCTAssertEqual(
                items?.first(where: { $0.name == "until" })?.value,
                "700")
            return (
                HTTPURLResponse(
                    url: url,
                    statusCode: 200,
                    httpVersion: nil,
                    headerFields: nil)!,
                fixtureData)
        }

        let result = try await TrafficAPIClient(
            runtime: runtime,
            session: session)
            .fetchAnalytics(
                since: Date(timeIntervalSince1970: 100),
                until: Date(timeIntervalSince1970: 700))

        XCTAssertEqual(result.cost.summary.savedUSD, 595.1)
    }

    func testSavingsRowsRepresentNegativeSavingsWithoutFalseStack() throws {
        let group = TrafficSavingsChartGroup(
            id: "model",
            label: "Model",
            actualBasetenCostUSD: 12,
            estimatedNativeCostUSD: 9)
        XCTAssertEqual(group.estimatedAdditionalClaudeCostUSD, 0)
        XCTAssertTrue(group.hasNegativeSavings)

        let fixtureGroup = try XCTUnwrap(
            approvedFixture().cost.savings.byBasetenModel.first)
        XCTAssertEqual(
            fixtureGroup.estimatedAdditionalClaudeCostUSD,
            457.7,
            accuracy: 0.0001)
        XCTAssertFalse(
            TrafficSavingsChartGroup(
                id: fixtureGroup.id,
                label: fixtureGroup.label,
                actualBasetenCostUSD: fixtureGroup.actualBasetenCostUSD,
                estimatedNativeCostUSD: fixtureGroup.estimatedNativeCostUSD)
                .hasNegativeSavings)
        var summary = try approvedFixture().cost.summary
        summary.actualBasetenCostUSD = 999
        XCTAssertEqual(
            trafficSavingsEligibleBasetenCost(summary),
            143.5,
            accuracy: 0.0001)
    }

    func testProviderTrafficTablesAlwaysShowEveryRow() {
        XCTAssertEqual(
            trafficVisibleRowCount(
                totalCount: 12,
                isExpanded: false,
                collapsesLongLists: false),
            12)
        XCTAssertNil(
            trafficExpansionButtonTitle(
                totalCount: 12,
                isExpanded: false,
                collapsesLongLists: false))
    }

    func testModelTrafficTablesCollapseAfterEightRows() {
        XCTAssertEqual(
            trafficVisibleRowCount(
                totalCount: 8,
                isExpanded: false,
                collapsesLongLists: true),
            8)
        XCTAssertNil(
            trafficExpansionButtonTitle(
                totalCount: 8,
                isExpanded: false,
                collapsesLongLists: true))

        XCTAssertEqual(
            trafficVisibleRowCount(
                totalCount: 11,
                isExpanded: false,
                collapsesLongLists: true),
            8)
        XCTAssertEqual(
            trafficExpansionButtonTitle(
                totalCount: 11,
                isExpanded: false,
                collapsesLongLists: true),
            "Show 3 more")
    }

    func testExpandedModelTrafficTablesShowEveryRowAndCanCollapse() {
        XCTAssertEqual(
            trafficVisibleRowCount(
                totalCount: 11,
                isExpanded: true,
                collapsesLongLists: true),
            11)
        XCTAssertEqual(
            trafficExpansionButtonTitle(
                totalCount: 11,
                isExpanded: true,
                collapsesLongLists: true),
            "Show less")
    }

    @MainActor
    func testStoreKeepsLastGoodSnapshotAndMarksItStale() async throws {
        let snapshot = try approvedFixture()
        let reader = TrafficSequenceReader(outcomes: [
            .snapshot(snapshot),
            .failure,
        ])
        let store = TrafficStore(
            reader: reader,
            refreshInterval: 3_600,
            now: { Date(timeIntervalSince1970: 1_800_000_000) })

        store.start()
        await waitUntil { store.snapshot != nil }
        store.requestRefresh()
        await waitUntil { store.isStale }

        XCTAssertEqual(store.snapshot, snapshot)
        XCTAssertNotNil(store.errorMessage)
        store.stop()
    }

    @MainActor
    func testStoreCoalescesRefreshesAndNeverOverlapsRequests() async throws {
        let snapshot = try approvedFixture()
        let reader = TrafficSequenceReader(
            outcomes: [
                .snapshot(snapshot),
                .snapshot(snapshot),
            ],
            delayNanoseconds: 40_000_000)
        let store = TrafficStore(
            reader: reader,
            refreshInterval: 3_600)

        store.start()
        store.requestRefresh()
        store.requestRefresh()
        await waitUntil {
            let calls = await reader.calls
            return calls == 2 && !store.isRefreshing && !store.isLoading
        }

        let finalCalls = await reader.calls
        let maximumActive = await reader.maximumActive
        XCTAssertEqual(finalCalls, 2)
        XCTAssertEqual(maximumActive, 1)
        store.stop()
    }

    private func approvedFixture() throws -> TrafficAnalyticsSnapshot {
        let data = try Data(contentsOf: approvedFixtureURL())
        return try JSONDecoder().decode(
            TrafficAnalyticsSnapshot.self,
            from: data)
    }

    private func approvedFixtureURL() -> URL {
        var url = URL(fileURLWithPath: #filePath)
        for _ in 0..<2 {
            url.deleteLastPathComponent()
        }
        url.appendPathComponent("Fixtures/native-analytics-mock.json")
        return url
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
        XCTFail("Timed out waiting for TrafficStore state")
    }
}
