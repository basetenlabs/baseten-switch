import Foundation
import XCTest

@testable import BasetenSwitch

@MainActor
private final class SetupApplication: ClaudeDesktopApplicationControlling {
    var isRunning = true
    var version = "3.6556.1"
    var url = URL(fileURLWithPath: "/Applications/Example Claude.app")
    var openedInstallation: ClaudeDesktopInstallation?
    var acceptsQuit = true
    var exitsOnQuit = true
    var failsToOpen = false
    var events: [String] = []
    var beforeQuit: (() -> Void)?
    var beforeOpen: (() throws -> Void)?

    func installation() -> ClaudeDesktopInstallation? {
        ClaudeDesktopInstallation(url: url, version: version)
    }

    func requestQuit() -> Bool {
        beforeQuit?()
        events.append("quit")
        if acceptsQuit && exitsOnQuit { isRunning = false }
        return acceptsQuit
    }

    func open(_ installation: ClaudeDesktopInstallation) async throws {
        events.append("open")
        openedInstallation = installation
        try beforeOpen?()
        if failsToOpen { throw CocoaError(.fileReadNoSuchFile) }
        isRunning = true
    }
}

final class ClaudeDesktopSetupTests: XCTestCase {
    private let connection = ClaudeDesktopConnectionSettings(
        baseURL: "http://127.0.0.1:45274", apiKey: "baseten-switch-local")
    private var directory: URL!
    private var support: URL { directory.appendingPathComponent("Application Support") }
    private var library: URL { support.appendingPathComponent("Claude-3p/configLibrary") }

    override func setUpWithError() throws {
        directory = URL(fileURLWithPath: "/private/tmp/\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        guard UUID(uuidString: directory.lastPathComponent) != nil,
            directory.deletingLastPathComponent().path == "/private/tmp"
        else {
            return XCTFail("Refusing cleanup outside the unique test directory")
        }
        try FileManager.default.removeItem(at: directory)
    }

    private func store() -> ClaudeDesktopConfigurationStore {
        ClaudeDesktopConfigurationStore(
            applicationSupportURL: support,
            managedPreferencesURL: directory.appendingPathComponent("Managed Preferences"),
            username: "test-user")
    }

    private func status(discovery: [String: Any]? = nil, runtime: String = "ready") throws
        -> ClaudeDesktopStatus
    {
        var object: [String: Any] = [
            "client": "claude-desktop", "configured": true, "enabled": true,
            "connection": [
                "provider": "gateway", "base_url": connection.baseURL,
                "api_key": "baseten-switch-local", "auth_scheme": "bearer",
                "model_discovery": true, "auto_mode": false,
            ],
            "model_picker": ["enabled": true, "models": []],
            "runtime": [
                "state": runtime, "currently_bound": runtime == "ready",
                "desired_matches_active": runtime == "ready",
            ],
            "auth": ["state": "ready"],
            "request_evidence": [
                "state": "not_observed", "scope": "recent_500_requests",
                "last_request": NSNull(),
            ],
        ]
        if let discovery { object["model_discovery"] = discovery }
        let data = try JSONSerialization.data(withJSONObject: object)
        return try XCTUnwrap(ClaudeDesktopStatus(json: String(decoding: data, as: UTF8.self)))
    }

    private func discovery(at date: Date) -> [String: Any] {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return ["last_served_at": formatter.string(from: date), "model_count": 2]
    }

    private func assertNoFiles(file: StaticString = #filePath, line: UInt = #line) throws {
        XCTAssertEqual(
            try FileManager.default.contentsOfDirectory(atPath: directory.path), [], file: file, line: line)
    }

    @MainActor
    func testPrepareAndCancelHaveNoFileOrApplicationEffects() async throws {
        let app = SetupApplication()
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        setup.prepare(connection: connection)
        guard case .confirming(let request) = setup.phase else {
            return XCTFail("Expected restart confirmation")
        }
        XCTAssertEqual(request.connection, connection)
        XCTAssertEqual(request.installation.version, "3.6556.1")
        try assertNoFiles()
        setup.cancelRestart()
        await setup.connect()
        XCTAssertEqual(setup.phase, .ready)
        XCTAssertTrue(app.events.isEmpty)
        try assertNoFiles()
    }

    @MainActor
    func testUnsupportedVersionNeverQuitsOrWrites() async throws {
        let app = SetupApplication()
        app.version = "9.0.0"
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        setup.prepare(connection: connection)
        guard case .unavailable(let message) = setup.phase else {
            return XCTFail("Expected unsupported version recovery")
        }
        XCTAssertTrue(message.contains("manual setup"))
        await setup.connect()
        XCTAssertTrue(app.events.isEmpty)
        try assertNoFiles()
    }

    @MainActor
    func testRefusedGracefulQuitLeavesSettingsUntouched() async throws {
        let app = SetupApplication()
        app.acceptsQuit = false
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        setup.prepare(connection: connection)
        await setup.connect()
        guard case .unavailable(let message) = setup.phase else { return XCTFail("Expected quit recovery") }
        XCTAssertTrue(message.contains("could not quit"))
        XCTAssertEqual(app.events, ["quit"])
        try assertNoFiles()
    }

    @MainActor
    func testStillRunningAtQuitDeadlineLeavesSettingsUntouched() async throws {
        let app = SetupApplication()
        app.exitsOnQuit = false
        let setup = ClaudeDesktopSetup(store: store(), application: app, quitTimeout: 0)
        setup.prepare(connection: connection)
        await setup.connect()
        guard case .unavailable(let message) = setup.phase else {
            return XCTFail("Expected running application recovery")
        }
        XCTAssertTrue(message.contains("still running"))
        XCTAssertEqual(app.events, ["quit"])
        try assertNoFiles()
    }

    @MainActor
    func testApplySavesLocalConfigurationBeforeReopeningAndWaitsForDiscovery() async throws {
        let app = SetupApplication()
        let configurationStore = store()
        app.beforeQuit = {
            XCTAssertFalse(FileManager.default.fileExists(atPath: self.support.path))
        }
        app.beforeOpen = {
            XCTAssertTrue(configurationStore.isConfigured(connection: self.connection))
            let meta = try XCTUnwrap(
                JSONSerialization.jsonObject(
                    with:
                        Data(contentsOf: self.library.appendingPathComponent("_meta.json"))) as? [String: Any]
            )
            let id = try XCTUnwrap(meta["appliedId"] as? String)
            let config = try XCTUnwrap(
                JSONSerialization.jsonObject(
                    with:
                        Data(contentsOf: self.library.appendingPathComponent("\(id).json"))) as? [String: Any]
            )
            XCTAssertEqual(config["inferenceGatewayBaseUrl"] as? String, "http://127.0.0.1:45274")
            XCTAssertEqual(config["inferenceGatewayApiKey"] as? String, "baseten-switch-local")
            XCTAssertEqual(config["inferenceGatewayAuthScheme"] as? String, "bearer")
            XCTAssertEqual(config["autoModeEnabled"] as? Bool, false)
            XCTAssertNil(config["models"])
        }
        let setup = ClaudeDesktopSetup(store: configurationStore, application: app)
        setup.prepare(connection: connection)
        await setup.connect()
        XCTAssertEqual(app.events, ["quit", "open"])
        guard case .waiting(let since) = setup.phase else {
            return XCTFail("Opening the app must not confirm connection")
        }
        setup.observe(try status(), isCurrent: true, now: since.addingTimeInterval(1))
        XCTAssertEqual(setup.phase, .waiting(since: since))
        setup.observe(
            try status(discovery: discovery(at: since.addingTimeInterval(1))),
            isCurrent: true, now: since.addingTimeInterval(2))
        XCTAssertEqual(setup.phase, .connected(since: since))
    }

    @MainActor
    func testOldStaleUnreadyFutureAndStoppedAppEvidenceCannotConfirm() throws {
        let app = SetupApplication()
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        let since = Date(timeIntervalSince1970: 1_800_000_000)
        let now = since.addingTimeInterval(2)
        setup.beginManualVerification(since: since)
        setup.observe(
            try status(discovery: discovery(at: since.addingTimeInterval(-1))), isCurrent: true, now: now)
        XCTAssertEqual(setup.phase, .waiting(since: since))
        let fresh = try status(discovery: discovery(at: since.addingTimeInterval(1)))
        setup.observe(fresh, isCurrent: false, now: now)
        XCTAssertEqual(setup.phase, .waiting(since: since))
        setup.observe(
            try status(discovery: discovery(at: since.addingTimeInterval(1)), runtime: "pending"),
            isCurrent: true, now: now)
        XCTAssertEqual(setup.phase, .waiting(since: since))
        setup.observe(
            try status(discovery: discovery(at: now.addingTimeInterval(6))), isCurrent: true, now: now)
        XCTAssertEqual(setup.phase, .waiting(since: since))
        app.isRunning = false
        setup.observe(fresh, isCurrent: true, now: now)
        XCTAssertEqual(setup.phase, .waiting(since: since))
    }

    @MainActor
    func testDiscoveryTimeoutCanRecoverOnLaterFreshObservation() throws {
        let app = SetupApplication()
        let setup = ClaudeDesktopSetup(store: store(), application: app, discoveryTimeout: 30)
        let since = Date(timeIntervalSince1970: 1_800_000_000)
        setup.beginManualVerification(since: since)
        setup.observe(nil, isCurrent: false, now: since.addingTimeInterval(30))
        XCTAssertEqual(setup.phase, .notObserved(since: since))
        setup.observe(
            try status(discovery: discovery(at: since.addingTimeInterval(31))),
            isCurrent: true, now: since.addingTimeInterval(32))
        XCTAssertEqual(setup.phase, .connected(since: since))
    }

    @MainActor
    func testReopenFailureKeepsSavedConfigurationAndCanRecover() async throws {
        let app = SetupApplication()
        app.failsToOpen = true
        let configurationStore = store()
        let setup = ClaudeDesktopSetup(store: configurationStore, application: app)
        setup.prepare(connection: connection)
        await setup.connect()
        XCTAssertEqual(app.events, ["quit", "open"])
        XCTAssertTrue(configurationStore.isConfigured(connection: connection))
        guard case .notObserved(let since) = setup.phase else { return XCTFail("Expected reopen recovery") }
        app.isRunning = true
        setup.observe(
            try status(discovery: discovery(at: since.addingTimeInterval(1))),
            isCurrent: true, now: since.addingTimeInterval(2))
        XCTAssertEqual(setup.phase, .connected(since: since))
    }

    @MainActor
    func testManualVerificationUsesGuideOpeningBaselineWithoutApplyingFiles() throws {
        let app = SetupApplication()
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        let guideOpened = Date(timeIntervalSince1970: 1_800_000_000)
        setup.beginManualVerification(since: guideOpened)
        setup.observe(
            try status(discovery: discovery(at: guideOpened.addingTimeInterval(10))),
            isCurrent: true, now: guideOpened.addingTimeInterval(20))
        XCTAssertEqual(setup.phase, .connected(since: guideOpened))
        XCTAssertTrue(app.events.isEmpty)
        try assertNoFiles()
    }

    @MainActor
    func testVersionChangeDuringQuitNeverWritesOrReopens() async throws {
        let app = SetupApplication()
        app.beforeQuit = {
            app.version = "9.0.0"
            app.url = URL(fileURLWithPath: "/Applications/Updated Claude.app")
        }
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        setup.prepare(connection: connection)
        await setup.connect()
        guard case .unavailable(let message) = setup.phase else {
            return XCTFail("Expected changed application recovery")
        }
        XCTAssertTrue(message.contains("changed during setup"))
        XCTAssertEqual(app.events, ["quit"])
        try assertNoFiles()
    }

    @MainActor
    func testSameVersionAppTranslocationReopensResolvedInstallationAfterQuit() async throws {
        let app = SetupApplication()
        let translocatedURL = URL(fileURLWithPath: "/private/var/AppTranslocation/Example/d/Claude.app")
        let installedURL = URL(fileURLWithPath: "/Applications/Claude.app")
        app.url = translocatedURL
        let configurationStore = store()
        app.beforeQuit = {
            XCTAssertFalse(FileManager.default.fileExists(atPath: self.support.path))
            app.url = installedURL
        }
        app.beforeOpen = {
            XCTAssertTrue(configurationStore.isConfigured(connection: self.connection))
        }
        let setup = ClaudeDesktopSetup(store: configurationStore, application: app)
        setup.prepare(connection: connection)
        guard case .confirming(let request) = setup.phase else { return XCTFail("Expected confirmation") }
        XCTAssertEqual(request.installation.url, translocatedURL)
        await setup.connect()
        XCTAssertEqual(app.events, ["quit", "open"])
        XCTAssertEqual(
            app.openedInstallation, ClaudeDesktopInstallation(url: installedURL, version: app.version))
        guard case .waiting(let since) = setup.phase else { return XCTFail("Expected fresh discovery wait") }
        setup.observe(try status(), isCurrent: true, now: since.addingTimeInterval(1))
        XCTAssertEqual(setup.phase, .waiting(since: since))
    }

    @MainActor
    func testConfirmedConnectionClearsWhenCurrentDiscoveryDisappearsAndCanRecover() throws {
        let app = SetupApplication()
        let setup = ClaudeDesktopSetup(store: store(), application: app)
        let since = Date(timeIntervalSince1970: 1_800_000_000)
        let fresh = try status(discovery: discovery(at: since.addingTimeInterval(1)))
        let now = since.addingTimeInterval(2)
        setup.beginManualVerification(since: since)
        setup.observe(fresh, isCurrent: true, now: now)
        XCTAssertEqual(setup.phase, .connected(since: since))
        XCTAssertTrue(setup.connectionIsConfirmed(by: fresh, isCurrent: true, now: now))
        XCTAssertFalse(setup.connectionIsConfirmed(by: try status(), isCurrent: true, now: now))
        XCTAssertFalse(setup.connectionIsConfirmed(by: fresh, isCurrent: false, now: now))
        setup.observe(try status(), isCurrent: true, now: now)
        XCTAssertEqual(setup.phase, .notObserved(since: since))
        setup.observe(
            try status(discovery: discovery(at: since.addingTimeInterval(3))),
            isCurrent: true, now: since.addingTimeInterval(4))
        XCTAssertEqual(setup.phase, .connected(since: since))
        app.isRunning = false
        let stoppedAt = since.addingTimeInterval(5)
        XCTAssertFalse(setup.connectionIsConfirmed(by: fresh, isCurrent: true, now: stoppedAt))
        setup.observe(fresh, isCurrent: true, now: stoppedAt)
        XCTAssertEqual(setup.phase, .notObserved(since: stoppedAt))
        app.isRunning = true
        setup.observe(fresh, isCurrent: true, now: stoppedAt.addingTimeInterval(1))
        XCTAssertEqual(setup.phase, .notObserved(since: stoppedAt))
        setup.observe(
            try status(discovery: discovery(at: stoppedAt.addingTimeInterval(2))),
            isCurrent: true, now: stoppedAt.addingTimeInterval(3))
        XCTAssertEqual(setup.phase, .connected(since: stoppedAt))
    }

    func testDiscoveryParserHandlesNanoDatesAndRejectsMalformedEvidence() throws {
        let valid = try status(discovery: [
            "last_served_at": "2026-10-22T12:34:56.123456789Z", "model_count": 2,
        ])
        XCTAssertEqual(valid.modelDiscovery?.modelCount, 2)
        XCTAssertNotNil(valid.modelDiscovery?.lastServedAt)
        XCTAssertNil(try status().modelDiscovery)
        for malformed: [String: Any] in [
            ["last_served_at": "not-a-date", "model_count": 2],
            ["last_served_at": "2026-10-22T12:34:56Z", "model_count": -1],
            ["last_served_at": "2026-10-22T12:34:56Z", "model_count": "2"],
            ["last_served_at": "2026-10-22T12:34:56Z", "model_count": 1.5],
        ] {
            XCTAssertNil(try status(discovery: malformed).modelDiscovery)
        }
    }
}
