import XCTest
import ServiceManagement
@testable import BasetenSwitch

// Start-at-Login decision logic and labels. The SMAppService calls
// live in LoginItem's thin wrappers; everything decidable is pure and
// pinned here. SMAppService.Status values are plain enum cases, so the
// tests construct them directly without a real .app bundle.

final class LoginItemTests: XCTestCase {

    // MARK: - Launch reconciliation decision table (status x flag)

    // .notFound means macOS invalidated a registration (bundle
    // replaced, signing identity changed), not that the user opted
    // out: re-register on every launch, regardless of the flag.
    func testNotFoundReRegistersRegardlessOfFlag() {
        XCTAssertEqual(loginItemLaunchAction(status: .notFound,
                                             firstLaunchFlagSet: true),
                       .register(persistFlag: false))
        XCTAssertEqual(loginItemLaunchAction(status: .notFound,
                                             firstLaunchFlagSet: false),
                       .register(persistFlag: true))
    }

    // .notRegistered with the flag set is the user's explicit
    // toggle-off; reconciliation must never override it.
    func testNotRegisteredWithFlagIsUserOptOut() {
        XCTAssertEqual(loginItemLaunchAction(status: .notRegistered,
                                             firstLaunchFlagSet: true),
                       .leaveAlone)
    }

    // .notRegistered without the flag is the first launch: register by
    // default and persist the flag on success.
    func testNotRegisteredFirstLaunchRegisters() {
        XCTAssertEqual(loginItemLaunchAction(status: .notRegistered,
                                             firstLaunchFlagSet: false),
                       .register(persistFlag: true))
    }

    // Already enabled or pending approval: nothing to repair. On the
    // first launch only the flag needs persisting; register cannot
    // approve a pending item, so requiresApproval is surfaced by the
    // popup caption instead of retried here.
    func testEnabledAndRequiresApprovalOnlyPersistFlagOnFirstLaunch() {
        for status: SMAppService.Status in [.enabled, .requiresApproval] {
            XCTAssertEqual(loginItemLaunchAction(status: status,
                                                 firstLaunchFlagSet: false),
                           .persistFlagOnly,
                           "status \(loginItemStatusName(status))")
            XCTAssertEqual(loginItemLaunchAction(status: status,
                                                 firstLaunchFlagSet: true),
                           .leaveAlone,
                           "status \(loginItemStatusName(status))")
        }
    }

    // A status case this build does not know must never trigger a
    // mutation. (Raw-value init on the ObjC-backed enum lets us forge
    // a future case.)
    func testUnknownStatusLeavesAlone() throws {
        let future = try XCTUnwrap(SMAppService.Status(rawValue: 999))
        XCTAssertEqual(loginItemLaunchAction(status: future,
                                             firstLaunchFlagSet: false),
                       .leaveAlone)
        XCTAssertEqual(loginItemLaunchAction(status: future,
                                             firstLaunchFlagSet: true),
                       .leaveAlone)
    }

    // MARK: - Toggle decision

    // Registered or pending approval turns off; anything else turns
    // on, so the toggle doubles as the retry for a stale .notFound
    // whose launch-time re-registration failed.
    func testToggleAction() {
        XCTAssertEqual(loginItemToggleAction(status: .enabled), .unregister)
        XCTAssertEqual(loginItemToggleAction(status: .requiresApproval), .unregister)
        XCTAssertEqual(loginItemToggleAction(status: .notRegistered), .register)
        XCTAssertEqual(loginItemToggleAction(status: .notFound), .register)
    }

    // MARK: - Popup labels

    // The caption is non-nil exactly in the states where the login
    // item will NOT fire at next login, so a broken registration is
    // visible without opening System Settings. The .notFound wording
    // must not assert a failed attempt (the status can flip mid-session
    // with no register() call, and it is the expected state on a bare
    // dev binary) and must name the retry, which is the toggle itself.
    func testStartAtLoginCaption() {
        XCTAssertEqual(startAtLoginCaption(status: .requiresApproval),
                       "needs approval in System Settings")
        XCTAssertEqual(startAtLoginCaption(status: .notFound),
                       "not registered, will not start at login; toggle to register")
        XCTAssertNil(startAtLoginCaption(status: .enabled))
        XCTAssertNil(startAtLoginCaption(status: .notRegistered))
    }

    // Only requiresApproval redirects the row's click to System
    // Settings; .notFound keeps the toggle as the retry path.
    func testStartAtLoginOpensSystemSettings() {
        XCTAssertTrue(startAtLoginOpensSystemSettings(status: .requiresApproval))
        XCTAssertFalse(startAtLoginOpensSystemSettings(status: .enabled))
        XCTAssertFalse(startAtLoginOpensSystemSettings(status: .notRegistered))
        XCTAssertFalse(startAtLoginOpensSystemSettings(status: .notFound))
    }

    // MARK: - CLI uninstall acceptance

    // The headless --unregister-login-item mode exits 0 exactly when no
    // live registration can remain: never-registered and the stale
    // .notFound both qualify (nothing points at the about-to-be-deleted
    // bundle), while a live or pending registration fails the run so
    // the CLI keeps the bundle and prints the manual path. An unknown
    // future status fails closed.
    func testLoginItemUnregisterAccepted() throws {
        XCTAssertTrue(loginItemUnregisterAccepted(.notRegistered))
        XCTAssertTrue(loginItemUnregisterAccepted(.notFound))
        XCTAssertFalse(loginItemUnregisterAccepted(.enabled))
        XCTAssertFalse(loginItemUnregisterAccepted(.requiresApproval))
        let future = try XCTUnwrap(SMAppService.Status(rawValue: 999))
        XCTAssertFalse(loginItemUnregisterAccepted(future))
    }

    // Log names stay stable so the reconciliation transition line is
    // greppable across releases.
    func testLoginItemStatusName() throws {
        XCTAssertEqual(loginItemStatusName(.notRegistered), "notRegistered")
        XCTAssertEqual(loginItemStatusName(.enabled), "enabled")
        XCTAssertEqual(loginItemStatusName(.requiresApproval), "requiresApproval")
        XCTAssertEqual(loginItemStatusName(.notFound), "notFound")
        let future = try XCTUnwrap(SMAppService.Status(rawValue: 999))
        XCTAssertEqual(loginItemStatusName(future), "unknown(999)")
    }
}
