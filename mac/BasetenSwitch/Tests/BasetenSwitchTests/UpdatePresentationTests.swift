import AppKit
import SwiftUI
import XCTest
@testable import BasetenSwitch

private actor UpdateMenuRunner: CLIRunning {
    private var outputs: [String]
    private(set) var requests: [CLIExecutionRequest] = []

    init(outputs: [String]) { self.outputs = outputs }

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        requests.append(request)
        return CLIExecutionResult(
            status: 0,
            standardOutput: outputs.removeFirst(),
            standardError: "",
            timedOut: false)
    }
}

final class UpdatePresentationTests: XCTestCase {
    func testInstallationInstructionsFollowDetectedSource() {
        XCTAssertEqual(
            UpdateInstallationInstructions(source: "homebrew").command,
            "brew update && brew upgrade baseten-switch")
        XCTAssertEqual(
            UpdateInstallationInstructions(source: "nix").command,
            "nix profile upgrade --refresh baseten-switch")
        for source in ["unknown", "source", "future-package-manager"] {
            XCTAssertNil(UpdateInstallationInstructions(source: source).command)
        }
    }

    func testReleaseMarkerRemainsAtPhysicalTopRightInBothCoordinateSystems() {
        for bounds in [
            NSRect(x: 0, y: 0, width: 24, height: 22),
            NSRect(x: 10, y: 20, width: 44, height: 36),
        ] {
            for flipped in [true, false] {
                let layout = releaseUpdateMarkerLayout(in: bounds, isFlipped: flipped)
                XCTAssertEqual(layout.frame.width, 4)
                XCTAssertEqual(layout.frame.height, 4)
                XCTAssertEqual(bounds.maxX - layout.frame.maxX, 3)
                let physicalTopInset = flipped
                    ? layout.frame.minY - bounds.minY
                    : bounds.maxY - layout.frame.maxY
                XCTAssertEqual(physicalTopInset, 3)
                // Only the opposite vertical margin may stretch on resize.
                XCTAssertEqual(layout.autoresizingMask, flipped
                    ? [.minXMargin, .maxYMargin]
                    : [.minXMargin, .minYMargin])
            }
        }
    }

    @MainActor
    func testUpdateDetailsKeepUsableContentHeightWhenHostedWithoutShowing() {
        for snapshot in [
            PopupPreviewFixture.updateAvailable.updateSnapshot,
            PopupPreviewFixture.updateCurrent.updateSnapshot,
            nil,
        ] {
            let updates = UpdateStore(
                variant: testVariant(),
                appVersion: "v0.6.0",
                snapshot: snapshot,
                enabled: false)
            let host = NSHostingController(rootView: UpdateDetailsView(
                updates: updates, done: {}))
            let window = NSWindow(
                contentRect: NSRect(x: 0, y: 0, width: 680, height: 560),
                styleMask: [.titled, .closable],
                backing: .buffered,
                defer: false)
            window.isReleasedWhenClosed = false
            window.contentViewController = host
            host.view.layoutSubtreeIfNeeded()

            // Hosting an unconstrained ScrollView can collapse a window
            // initialized with a nonzero contentRect into only a title bar.
            // Check both intrinsic layout and the resulting window content.
            XCTAssertEqual(host.view.fittingSize.height, 560, accuracy: 1)
            XCTAssertEqual(window.contentLayoutRect.height, 560, accuracy: 1)
            XCTAssertFalse(window.isVisible)
            window.close()
        }
    }

    @MainActor
    func testUpdateWindowCentersUsingItsFinalContentSizeWithoutShowing() {
        let updates = UpdateStore(
            variant: testVariant(),
            appVersion: "v0.6.0",
            snapshot: PopupPreviewFixture.updateAvailable.updateSnapshot,
            enabled: false)
        let controller = UpdateWindowController(updates: updates, variant: testVariant())
        let window = controller.makeWindowForTesting()
        window.contentViewController?.view.layoutSubtreeIfNeeded()

        XCTAssertEqual(window.contentLayoutRect.size.width, 680, accuracy: 1)
        XCTAssertEqual(window.contentLayoutRect.size.height, 560, accuracy: 1)
        if let screen = window.screen ?? NSScreen.main {
            XCTAssertEqual(window.frame.midX, screen.visibleFrame.midX, accuracy: 1)
        }
        XCTAssertFalse(window.isVisible)
        window.close()
    }

    @MainActor
    func testAvailableMenuItemDispatchesNativeActionToUpdateWindowCallback() {
        let variant = testVariant()
        let updates = UpdateStore(
            variant: variant,
            appVersion: "v0.6.0",
            snapshot: PopupPreviewFixture.updateAvailable.updateSnapshot,
            enabled: false)
        let state = BasetenSwitchState(preview: .healthy, variant: variant, updates: updates)
        var openedUpdates = 0
        let controller = StatusItemController(
            state: state,
            variant: variant,
            isPreview: true,
            openUpdates: { openedUpdates += 1 })

        XCTAssertTrue(controller.performMenuItemForTesting(titled: "Update Available: v0.6.1…"))
        XCTAssertEqual(openedUpdates, 1)
    }

    @MainActor
    func testCurrentMenuKeepsManualCheckBeforeOpenAppAndNoReleaseMarker() {
        let variant = testVariant()
        let updates = UpdateStore(
            variant: variant,
            appVersion: "v0.6.1",
            snapshot: ReleaseUpdateSnapshot(
                currentVersion: "v0.6.1",
                availableVersion: "v0.6.1",
                automaticCheck: true,
                installSource: "homebrew",
                status: "current"),
            enabled: false)
        let state = BasetenSwitchState(preview: .healthy, variant: variant, updates: updates)
        let controller = StatusItemController(
            state: state,
            variant: variant,
            isPreview: true)
        let titles = controller.menuItemTitlesForTesting
        XCTAssertFalse(controller.releaseMarkerVisibleForTesting)
        XCTAssertFalse(titles.contains { $0.hasPrefix("Update Available:") })
        guard let check = titles.firstIndex(of: "Check for Updates…"),
              let open = titles.firstIndex(of: "Open Baseten Switch") else {
            return XCTFail("Manual check and Open app actions must be present")
        }
        XCTAssertLessThan(check, open)
    }

    @MainActor
    func testReleaseChangesDoNotRebuildTrackedMenuOrChangeRoutingHealth() async {
        let variant = testVariant()
        let runner = UpdateMenuRunner(outputs: [
            Self.output(target: "v0.6.1", status: "available"),
            Self.output(target: "v0.6.0", status: "current"),
        ])
        let updates = UpdateStore(
            variant: variant,
            appVersion: "v0.6.0",
            runner: runner,
            binaryLocator: { URL(fileURLWithPath: "/usr/bin/true") })
        let state = BasetenSwitchState(preview: .healthy, variant: variant, updates: updates)
        let controller = StatusItemController(
            state: state,
            variant: variant,
            isPreview: true)
        let currentTitles = controller.menuItemTitlesForTesting
        let health = controller.displayedIconStateForTesting
        controller.menuWillOpenForTesting()
        updates.check(force: true)
        await waitForProjection(updates)

        XCTAssertTrue(controller.releaseMarkerVisibleForTesting)
        XCTAssertEqual(controller.displayedIconStateForTesting, health)
        XCTAssertEqual(controller.menuItemTitlesForTesting, currentTitles)
        controller.menuDidCloseForTesting()
        controller.menuNeedsUpdateForTesting()
        let availableTitles = controller.menuItemTitlesForTesting
        XCTAssertTrue(availableTitles.contains("Update Available: v0.6.1…"))

        controller.menuWillOpenForTesting()
        updates.check(force: true)
        await waitForProjection(updates)
        XCTAssertFalse(controller.releaseMarkerVisibleForTesting)
        XCTAssertEqual(controller.displayedIconStateForTesting, health)
        XCTAssertEqual(controller.menuItemTitlesForTesting, availableTitles)
        controller.menuDidCloseForTesting()
        controller.menuNeedsUpdateForTesting()
        XCTAssertFalse(controller.menuItemTitlesForTesting.contains {
            $0.hasPrefix("Update Available:")
        })
        let requests = await runner.requests
        XCTAssertEqual(requests.map(\.arguments), [
            ["update", "check", "--json", "--refresh"],
            ["update", "check", "--json", "--refresh"],
        ])
        updates.stop()
    }

    @MainActor
    private func waitForProjection(_ updates: UpdateStore) async {
        while updates.isChecking { await Task.yield() }
        // Combine delivery and the controller's post-mutation projection each
        // occupy one main-queue turn.
        for _ in 0..<3 {
            await withCheckedContinuation { continuation in
                DispatchQueue.main.async { continuation.resume() }
            }
        }
    }

    private func testVariant() -> AppVariant {
        AppVariant.resolve(
            infoDictionary: [:],
            homeDirectory: "/tmp/baseten-switch-update-test-home",
            environment: [:])
    }

    private static func output(target: String, status: String) -> String {
        """
        {"current_version":"v0.6.0","available_version":"\(target)",
         "automatic_check":true,"install_source":"homebrew","status":"\(status)"}
        """
    }
}
