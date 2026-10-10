import Foundation
import XCTest
@testable import BasetenSwitch

private actor UpdateTestRunner: CLIRunning {
    nonisolated let requests: AsyncStream<CLIExecutionRequest>
    private let events: AsyncStream<CLIExecutionRequest>.Continuation
    private var pending: [CheckedContinuation<CLIExecutionResult, Never>] = []
    private(set) var callCount = 0

    init() {
        var continuation: AsyncStream<CLIExecutionRequest>.Continuation!
        requests = AsyncStream { continuation = $0 }
        events = continuation
    }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        await withCheckedContinuation { continuation in
            pending.append(continuation)
            callCount += 1
            events.yield(request)
        }
    }

    func complete(_ output: String, status: Int32 = 0) {
        pending.removeFirst().resume(returning: CLIExecutionResult(
            status: status, standardOutput: output,
            standardError: "", timedOut: false))
    }
}

private actor UpdateTestSleeper {
    nonisolated let delays: AsyncStream<TimeInterval>
    private let events: AsyncStream<TimeInterval>.Continuation
    private var pending: CheckedContinuation<Void, Never>?

    init() {
        var continuation: AsyncStream<TimeInterval>.Continuation!
        delays = AsyncStream { continuation = $0 }
        events = continuation
    }

    func sleep(seconds: TimeInterval) async {
        await withCheckedContinuation { continuation in
            pending = continuation
            events.yield(seconds)
        }
    }

    func release() {
        pending?.resume()
        pending = nil
    }
}

private struct UpdateTestClock: RuntimeClock {
    let now = Date(timeIntervalSince1970: 1_000)
    let sleeper: UpdateTestSleeper

    func sleep(seconds: TimeInterval) async throws {
        await sleeper.sleep(seconds: seconds)
        try Task.checkCancellation()
    }
}

@MainActor
final class UpdateStoreTests: XCTestCase {
    private let variant = AppVariant.resolve(infoDictionary: [:], environment: [:])
    private let availableJSON = """
        {"current_version":"v0.6.0","available_version":"v0.6.1",
         "release_url":"https://github.com/basetenlabs/baseten-switch/releases/tag/v0.6.1",
         "checked_at":"2026-10-09T20:00:00.123456Z", "automatic_check":true,
         "install_source":"homebrew", "status":"available"}
        """

    private func store(_ runner: UpdateTestRunner,
                       clock: any RuntimeClock = SystemRuntimeClock()) -> UpdateStore {
        UpdateStore(
            variant: variant, appVersion: "v0.6.0", runner: runner, clock: clock,
            binaryLocator: { URL(fileURLWithPath: "/example/baseten-switch") })
    }

    private func waitForCheck(_ store: UpdateStore) async {
        for _ in 0..<10_000 {
            if !store.isChecking { return }
            await Task.yield()
        }
        XCTFail("Update check did not complete")
    }

    func testStrictNumericReleaseOrdering() {
        XCTAssertLessThan(ReleaseVersion("v0.9.9")!, ReleaseVersion("0.10.0")!)
        XCTAssertEqual(ReleaseVersion("v0.6.1"), ReleaseVersion("0.6.1"))
        for invalid in ["dev", "v0.6.1-dirty", "0.6.1-beta", "0.6", "0.6.1.2", "0..1", "00.6.1", "+0.6.1", "0.6.1/path"] {
            XCTAssertNil(ReleaseVersion(invalid), invalid)
        }
    }

    func testUpgradedCLIStillAdvertisesUpdateForOldApp() throws {
        var snapshot = try ReleaseUpdateSnapshot.decode(availableJSON)
        snapshot = ReleaseUpdateSnapshot(
            currentVersion: "v0.6.1", availableVersion: snapshot.availableVersion,
            releaseURL: URL(string: "https://example.invalid/untrusted"),
            checkedAt: snapshot.checkedAt, automaticCheck: true,
            installSource: "homebrew", status: "current")
        let oldApp = UpdateStore(variant: variant, appVersion: "v0.6.0", snapshot: snapshot, enabled: false)
        XCTAssertEqual(oldApp.availableVersion, "v0.6.1")
        XCTAssertEqual(oldApp.releaseNotesURL?.absoluteString,
            "https://github.com/basetenlabs/baseten-switch/releases/tag/v0.6.1")
        XCTAssertNotNil(snapshot.checkedAt)
        let currentApp = UpdateStore(variant: variant, appVersion: "v0.6.1", snapshot: snapshot, enabled: false)
        XCTAssertNil(currentApp.availableVersion)
        XCTAssertTrue(currentApp.isCurrent)
        let newerApp = UpdateStore(variant: variant, appVersion: "v0.7.0", snapshot: snapshot, enabled: false)
        XCTAssertNil(newerApp.availableVersion)
        let dev = UpdateStore(variant: variant, appVersion: "dev", snapshot: snapshot, enabled: false)
        XCTAssertNil(dev.availableVersion)
        XCTAssertFalse(dev.isCurrent)
    }

    func testManualConfirmationRemainsCurrentWhenAutomaticChecksAreOff() {
        let snapshot = ReleaseUpdateSnapshot(
            currentVersion: "v0.6.1", availableVersion: "v0.6.1",
            checkedAt: Date(), automaticCheck: false,
            installSource: "unknown", status: "disabled")
        let updates = UpdateStore(variant: variant, appVersion: "v0.6.1", snapshot: snapshot, enabled: false)
        XCTAssertTrue(updates.isCurrent)
        XCTAssertFalse(updates.automaticCheck)
        var failed = snapshot
        failed.error = "Unable to reach release metadata"
        let unknown = UpdateStore(variant: variant, appVersion: "v0.6.1", snapshot: failed, enabled: false)
        XCTAssertFalse(unknown.isCurrent)
    }

    func testCoalescedChecksAndFailureKeepConfirmedUpdate() async {
        let runner = UpdateTestRunner()
        let updates = store(runner)
        var requests = runner.requests.makeAsyncIterator()
        updates.check(force: true)
        updates.check(force: true)
        let request = await requests.next()!
        XCTAssertEqual(request.arguments, ["update", "check", "--json", "--refresh"])
        XCTAssertEqual(request.timeout, 6)
        XCTAssertNil(request.environment["GITHUB_TOKEN"])
        XCTAssertNil(request.environment["BASETEN_API_KEY"])
        let calls = await runner.callCount
        XCTAssertEqual(calls, 1)
        await runner.complete(availableJSON)
        await waitForCheck(updates)
        XCTAssertEqual(updates.availableVersion, "v0.6.1")
        updates.check(force: true)
        _ = await requests.next()
        await runner.complete("old CLI help output", status: 1)
        await waitForCheck(updates)
        XCTAssertEqual(updates.availableVersion, "v0.6.1")
        XCTAssertNotNil(updates.error)
        XCTAssertNotNil(updates.snapshot?.checkedAt)
    }

    func testStopRejectsAnInFlightResult() async {
        let runner = UpdateTestRunner()
        let updates = store(runner)
        var requests = runner.requests.makeAsyncIterator()
        updates.check()
        _ = await requests.next()
        updates.stop()
        await runner.complete(availableJSON)
        for _ in 0..<20 { await Task.yield() }
        XCTAssertNil(updates.snapshot)
        XCTAssertFalse(updates.isChecking)
    }

    func testSchedulerUsesSharedBackoffAndPreferenceDoesNotInstall() async {
        let runner = UpdateTestRunner()
        let sleeper = UpdateTestSleeper()
        let updates = store(runner, clock: UpdateTestClock(sleeper: sleeper))
        var requests = runner.requests.makeAsyncIterator()
        var delays = sleeper.delays.makeAsyncIterator()
        updates.start()
        updates.start()
        let initialRequest = await requests.next()
        XCTAssertEqual(initialRequest?.arguments, ["update", "check", "--json"])
        await runner.complete("""
            {"current_version":"v0.6.0","next_check_at":"1970-01-01T01:16:40Z",
             "automatic_check":true,"install_source":"unknown","status":"unknown",
             "error":"Unable to check"}
            """, status: 1)
        let delay = await delays.next()
        XCTAssertEqual(delay, 3_600)
        XCTAssertNotNil(updates.error)
        updates.setAutomaticCheck(false)
        let preferenceRequest = await requests.next()
        XCTAssertEqual(preferenceRequest?.arguments, ["update", "automatic", "off", "--json"])
        await runner.complete("""
            {"current_version":"v0.6.0","automatic_check":false,
             "install_source":"unknown","status":"disabled"}
            """)
        await waitForCheck(updates)
        XCTAssertFalse(updates.automaticCheck)
        updates.stop()
        await sleeper.release()
    }

    func testFixturesAndSourceBuildsNeverSpawnChecks() async {
        let runner = UpdateTestRunner()
        for (version, enabled) in [("v0.6.0", false), ("dev", true), ("v0.0.0", true)] {
            let updates = UpdateStore(
                variant: variant, appVersion: version, runner: runner,
                binaryLocator: { URL(fileURLWithPath: "/example/baseten-switch") }, enabled: enabled)
            updates.start()
            updates.check(force: true)
            updates.setAutomaticCheck(false)
            updates.stop()
        }
        let calls = await runner.callCount
        XCTAssertEqual(calls, 0)
    }
}
