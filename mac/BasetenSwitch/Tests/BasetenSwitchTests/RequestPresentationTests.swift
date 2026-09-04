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
            "Image input unsupported")
    }

    func testSuppressedFallbackUsesSettingAndTargetCopy() {
        let disabled429 = RequestFallback(
            attempted: false,
            suppressedReason: "policy_disabled_http_429")
        XCTAssertEqual(
            requestFallbackReason(disabled429),
            "Disabled by setting")
        XCTAssertEqual(
            requestFallbackHelp(disabled429),
            "Automatic fallback for HTTP 429 is disabled by setting.")

        let disabled5xx = RequestFallback(
            attempted: false,
            suppressedReason: "policy_disabled_http_5xx")
        XCTAssertEqual(
            requestFallbackReason(disabled5xx),
            "Disabled by setting")
        XCTAssertEqual(
            requestFallbackHelp(disabled5xx),
            "Automatic fallback for HTTP 5xx is disabled by setting.")

        let missingTarget = RequestFallback(
            attempted: false,
            suppressedReason: "native_target_unconfigured")
        XCTAssertEqual(
            requestFallbackReason(missingTarget),
            "Fallback target not configured")
        XCTAssertFalse(RequestItem(
            eventID: "suppressed",
            completedAt: Date(),
            client: "claude-code",
            configuredRoute: "baseten",
            effectiveProvider: "baseten",
            requestedModel: "claude-baseten-glm",
            servedModel: "zai-org/GLM",
            status: 429,
            durationMs: 1,
            terminationReason: "completed",
            subagent: false,
            fallback: disabled429).isFallback)
    }

    func testRouteAndModelLabelsShowRequestedToServedPath() throws {
        let item = requestItem()
        let primary = try XCTUnwrap(item.primary)

        XCTAssertEqual(
            requestModelLabel(item),
            "claude-opus-4-8 → claude-opus-4-8-20260701")
        XCTAssertEqual(
            requestRouteLabel(item),
            "Baseten → Anthropic")
        XCTAssertEqual(
            requestServedProviderLabel(item),
            "claude-opus-4-8-20260701 · Anthropic")
        XCTAssertEqual(
            requestPrimaryProviderLabel(primary),
            "zai-org/GLM-5.2 · Baseten")
        XCTAssertEqual(
            requestPrimaryRoutingLabel(primary),
            "zai-org/GLM-5.2 · Baseten")
        XCTAssertEqual(
            requestPrimaryStateLabel(primary),
            "HTTP 503")
        XCTAssertEqual(
            requestRoutingAccessibilityLabel(item),
            "Primary zai-org/GLM-5.2 · Baseten, HTTP 503. "
                + "Served by claude-opus-4-8-20260701 · Anthropic")
        XCTAssertEqual(requestResultLabel(item), "HTTP 200")
    }

    func testBypassedPrimaryIsExplicitlyNotAttempted() throws {
        var item = requestItem()
        item.primary = RequestPrimaryAttempt(
            provider: "baseten",
            model: "zai-org/GLM-5.2",
            attempted: false,
            outcome: "cooldown")
        let primary = try XCTUnwrap(item.primary)

        XCTAssertEqual(
            requestPrimaryStateLabel(primary),
            "Not attempted")
        XCTAssertEqual(
            requestPrimaryRoutingLabel(primary),
            "zai-org/GLM-5.2 · Baseten · Not attempted")
        XCTAssertTrue(
            requestRoutingAccessibilityLabel(item)
                .contains("Not attempted"))
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

    func testAutoCheckTypeUsesServerKindAndRoutingAction() {
        var item = requestItem()
        XCTAssertNil(requestTypeLabel(item))
        XCTAssertNil(requestTypeDescription(item))

        item.requestClassification = RequestClassification(
            kind: "claude_auto_permission_check",
            detector: "claude_auto_v1",
            routingAction: "native_anthropic")

        XCTAssertEqual(requestTypeLabel(item), "Auto check")
        XCTAssertEqual(
            requestTypeDescription(item),
            "Switch identified this as a Claude Code Auto permission check "
                + "and routed it to Anthropic.")

        item.requestClassification?.detector = "future_detector"
        XCTAssertEqual(requestTypeLabel(item), "Auto check")
        XCTAssertNotNil(requestTypeDescription(item))

        item.requestClassification?.routingAction = "future_action"
        XCTAssertNil(requestTypeLabel(item))
        XCTAssertNil(requestTypeDescription(item))
    }

    func testAutoCheckTypeExplainsAnthropicAuthFailures() {
        var item = requestItem()
        item.requestClassification = RequestClassification(
            kind: "claude_auto_permission_check",
            detector: "claude_auto_v1",
            routingAction: "native_anthropic")
        let unauthorized = "Switch identified this as a Claude Code Auto "
            + "permission check and routed it to Anthropic. Anthropic "
            + "rejected the request credentials. Auto checks require a "
            + "Claude login or Anthropic API key accepted by Anthropic."
        let forbidden = "Switch identified this as a Claude Code Auto "
            + "permission check and routed it to Anthropic. Anthropic denied "
            + "access to the request. Check the Claude login or Anthropic API "
            + "key, plus the account and model permissions."

        item.status = 401
        XCTAssertEqual(requestTypeDescription(item), unauthorized)

        item.status = 403
        XCTAssertEqual(requestTypeDescription(item), forbidden)

        item.status = 429
        XCTAssertEqual(
            requestTypeDescription(item),
            "Switch identified this as a Claude Code Auto permission check "
                + "and routed it to Anthropic.")
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
                trigger: "http_503"),
            primary: RequestPrimaryAttempt(
                provider: "baseten",
                model: "zai-org/GLM-5.2",
                attempted: true,
                outcome: "http_error",
                status: 503))
    }
}
