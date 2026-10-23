import Foundation
import XCTest
@testable import BasetenSwitch

@MainActor
private final class RestartApplication: ClaudeDesktopApplicationControlling {
    var isRunning = true
    var acceptsQuit = true
    var exits = true
    var failsOpen = false
    var events: [String] = []
    var beforeOpen: (() -> Void)?
    var installationReads = 0
    func installation() -> ClaudeDesktopInstallation? {
        installationReads += 1
        return .init(url: URL(fileURLWithPath: "/Applications/Example.app"), version: "99.0")
    }
    func requestQuit() -> Bool {
        events.append("quit")
        if acceptsQuit && exits { isRunning = false }
        return acceptsQuit
    }
    func open(_ installation: ClaudeDesktopInstallation) async throws {
        events.append("open")
        beforeOpen?()
        if failsOpen { throw CocoaError(.fileReadNoSuchFile) }
        isRunning = true
    }
}

final class ClaudeDesktopRestartTests: XCTestCase {
    @MainActor
    private func status(_ revision: String?, served: String? = nil, at: Date = Date()) throws -> ClaudeDesktopStatus {
        var object: [String: Any] = [
            "client": "claude-desktop", "configured": true, "enabled": true,
            "connection": ["provider": "gateway", "base_url": "http://127.0.0.1:45274",
                           "api_key": "baseten-switch-local", "auth_scheme": "bearer",
                           "model_discovery": true, "auto_mode": false],
            "model_picker": ["enabled": true, "models": []],
            "runtime": ["state": "ready", "currently_bound": true, "desired_matches_active": true],
            "auth": ["state": "ready"],
            "request_evidence": ["state": "not_observed", "scope": "recent_500_requests"]]
        if let revision { object["model_list_revision"] = revision }
        if let served {
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            object["model_discovery"] = ["revision": served, "last_served_at": formatter.string(from: at), "model_count": 2]
        }
        let data = try JSONSerialization.data(withJSONObject: object)
        return try XCTUnwrap(ClaudeDesktopStatus(json: String(decoding: data, as: UTF8.self)))
    }

    @MainActor
    func testPendingSurvivesRelaunchAndOnlyCurrentDiscoveryClears() throws {
        let name = "restart-tests-" + UUID().uuidString
        let defaults = try XCTUnwrap(UserDefaults(suiteName: name))
        defer { defaults.removePersistentDomain(forName: name) }
        let app = RestartApplication()
        let tracker = ClaudeDesktopRestart(defaults: defaults, key: "models", application: app)
        let now = Date()
        tracker.observe(try status("a", served: "a", at: now), isCurrent: true, now: now)
        tracker.observe(try status("b"), isCurrent: true, now: now.addingTimeInterval(1))
        XCTAssertTrue(tracker.needsRestart)
        let relaunched = ClaudeDesktopRestart(defaults: defaults, key: "models", application: app)
        XCTAssertTrue(relaunched.needsRestart)
        relaunched.observe(try status(nil), isCurrent: true)
        relaunched.observe(try status("b", served: "b", at: now), isCurrent: true)
        relaunched.observe(try status("b", served: "b", at: now.addingTimeInterval(2)), isCurrent: false)
        XCTAssertTrue(relaunched.needsRestart)
        relaunched.observe(try status("c"), isCurrent: true, now: now.addingTimeInterval(3))
        relaunched.observe(try status("c", served: "b", at: now.addingTimeInterval(4)), isCurrent: true)
        XCTAssertTrue(relaunched.needsRestart)
        relaunched.observe(try status("c", served: "c", at: now.addingTimeInterval(4)), isCurrent: true, now: now.addingTimeInterval(4))
        XCTAssertFalse(relaunched.needsRestart)
        relaunched.observe(try status("d"), isCurrent: true)
        XCTAssertTrue(relaunched.needsRestart)
        relaunched.observe(try status("c"), isCurrent: true)
        XCTAssertFalse(relaunched.needsRestart)
    }

    @MainActor
    func testDiscoveryBetweenSaveAndStatusCompletionIsFresh() throws {
        let name = "restart-tests-" + UUID().uuidString
        let defaults = try XCTUnwrap(UserDefaults(suiteName: name))
        defer { defaults.removePersistentDomain(forName: name) }
        let tracker = ClaudeDesktopRestart(defaults: defaults, key: "models", application: RestartApplication())
        let start = Date()
        tracker.observe(try status("a", served: "a", at: start), isCurrent: true, now: start)
        tracker.observe(try status("b"), isCurrent: true, now: start.addingTimeInterval(3), changedSince: start.addingTimeInterval(1))
        tracker.observe(try status("b", served: "b", at: start.addingTimeInterval(2)), isCurrent: true, now: start.addingTimeInterval(4))
        XCTAssertFalse(tracker.needsRestart)
    }

    @MainActor
    func testInitialConnectionAndManualDiscovery() throws {
        let name = "restart-tests-" + UUID().uuidString
        let defaults = try XCTUnwrap(UserDefaults(suiteName: name))
        defer { defaults.removePersistentDomain(forName: name) }
        let tracker = ClaudeDesktopRestart(defaults: defaults, key: "models", application: RestartApplication())
        tracker.observe(try status("a"), isCurrent: true)
        tracker.observe(try status("b"), isCurrent: true)
        XCTAssertFalse(tracker.needsRestart)
        tracker.observe(try status("b", served: "b"), isCurrent: true)
        tracker.observe(try status("c"), isCurrent: true)
        XCTAssertTrue(tracker.needsRestart)
    }

    @MainActor
    func testEditsDuringRestartStayPendingUntilThatListLoads() async throws {
        let name = "restart-tests-" + UUID().uuidString
        let defaults = try XCTUnwrap(UserDefaults(suiteName: name))
        defer { defaults.removePersistentDomain(forName: name) }
        let app = RestartApplication()
        let tracker = ClaudeDesktopRestart(defaults: defaults, key: "models", application: app)
        tracker.observe(try status("a", served: "a"), isCurrent: true)
        tracker.observe(try status("b"), isCurrent: true)
        let newer = try status("c")
        app.beforeOpen = { tracker.observe(newer, isCurrent: true) }
        await tracker.prepare()
        await tracker.restart()
        XCTAssertEqual(app.events, ["quit", "open"])
        XCTAssertEqual(app.installationReads, 2)
        tracker.observe(try status("c", served: "b", at: Date().addingTimeInterval(1)), isCurrent: true)
        XCTAssertTrue(tracker.needsRestart)
        app.isRunning = false
        tracker.observe(try status("c", served: "c", at: Date().addingTimeInterval(1)), isCurrent: true)
        XCTAssertTrue(tracker.needsRestart)
        app.isRunning = true
        tracker.observe(try status("c", served: "c", at: Date().addingTimeInterval(1)), isCurrent: true)
        XCTAssertFalse(tracker.needsRestart)
    }

    @MainActor
    func testCancelRefusalTimeoutStoppedAndLaunchFailure() async throws {
        let name = "restart-tests-" + UUID().uuidString
        let defaults = try XCTUnwrap(UserDefaults(suiteName: name))
        defer { defaults.removePersistentDomain(forName: name) }
        let app = RestartApplication()
        let tracker = ClaudeDesktopRestart(defaults: defaults, key: "models", application: app, quitTimeout: 0)
        tracker.setConfigured()
        tracker.observe(try status("a"), isCurrent: true)
        tracker.observe(try status("b"), isCurrent: true)
        await tracker.prepare()
        XCTAssertEqual(tracker.phase, .confirming)
        tracker.cancel()
        XCTAssertTrue(app.events.isEmpty)
        app.acceptsQuit = false
        await tracker.restart()
        XCTAssertEqual(app.events, ["quit"])
        app.acceptsQuit = true
        app.exits = false
        await tracker.restart()
        XCTAssertEqual(app.events, ["quit", "quit"])
        app.isRunning = false
        app.failsOpen = true
        await tracker.prepare()
        XCTAssertEqual(app.events, ["quit", "quit", "open"])
        XCTAssertTrue(tracker.needsRestart)
        app.failsOpen = false
        await tracker.prepare()
        XCTAssertEqual(app.events, ["quit", "quit", "open", "open"])
        XCTAssertEqual(tracker.phase, .waiting)
        XCTAssertTrue(tracker.needsRestart)
        tracker.observe(try status("b", served: "b", at: Date().addingTimeInterval(1)), isCurrent: true)
        XCTAssertFalse(tracker.needsRestart)
    }
}
