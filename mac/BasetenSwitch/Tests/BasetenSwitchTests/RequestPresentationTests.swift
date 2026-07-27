import Foundation
import XCTest
@testable import BasetenSwitch

final class RequestPresentationTests: XCTestCase {
    @MainActor
    func testRequestsDestinationAndToolbarRefreshRouting() {
        let navigation = RouterWindowNavigation()
        navigation.prepareForShow(destination: .requests)

        XCTAssertEqual(navigation.selection, .requests)
        XCTAssertTrue(
            toolbarRefreshesRequests(selection: navigation.selection))
        XCTAssertFalse(toolbarRefreshesRequests(selection: .traffic))
        XCTAssertFalse(toolbarRefreshesTraffic(selection: .requests))
        XCTAssertFalse(toolbarRefreshesModelCatalog(selection: .requests))
    }

    func testImageFallbackUsesHumanReason() {
        let fallback = RequestFallback(
            attempted: true,
            count: 1,
            trigger: "image_input_unsupported")

        XCTAssertEqual(
            requestFallbackReason(fallback),
            "Baseten could not accept the image, native provider used")
    }

    func testRouteAndModelLabelsShowRequestedToServedPath() {
        let item = requestItem()

        XCTAssertEqual(
            requestModelLabel(item),
            "claude-opus-4-8 → claude-opus-4-8-20260701")
        XCTAssertEqual(
            requestRouteLabel(item),
            "Baseten → Anthropic")
        XCTAssertEqual(
            requestServedProviderLabel(item),
            "claude-opus-4-8-20260701 · Anthropic")
        XCTAssertEqual(requestResultLabel(item), "HTTP 200")
    }

    func testCoverageAndFilterSpecificEmptyStates() {
        XCTAssertNil(
            requestCoverageMessage(
                RequestCoverage(complete: true)))
        XCTAssertEqual(
            requestCoverageMessage(
                RequestCoverage(
                    complete: false,
                    reason: "retention boundary")),
            "Partial request history: retention boundary")
        XCTAssertEqual(
            requestsEmptyTitle(filter: .fallbacks),
            "No fallback requests")
        XCTAssertEqual(
            requestsEmptyDetail(filter: .errors),
            "Requests that end in an error will appear here.")
    }

    private func requestItem() -> RequestItem {
        RequestItem(
            eventID: "0123456789abcdef0123456789abcdef",
            completedAt: Date(timeIntervalSince1970: 1_800_000_000),
            client: "claude-code",
            configuredRoute: "baseten",
            effectiveProvider: "anthropic",
            requestedModel: "claude-opus-4-8",
            servedModel: "claude-opus-4-8-20260701",
            status: 200,
            durationMs: 1_234,
            terminationReason: "completed",
            subagent: false,
            fallback: RequestFallback(
                attempted: true,
                count: 1,
                trigger: "image_input_unsupported"))
    }
}
