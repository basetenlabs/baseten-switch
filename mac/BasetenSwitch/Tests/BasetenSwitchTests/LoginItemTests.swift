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

    // The headless --unregister-login-item mode exits 0 when
    // .notRegistered confirms removal, whatever the prior status. Live,
    // pending, and unknown future end states fail closed.
    func testLoginItemUnregisterAccepted() throws {
        XCTAssertTrue(loginItemUnregisterAccepted(before: .enabled, after: .notRegistered))
        XCTAssertTrue(loginItemUnregisterAccepted(before: .notFound, after: .notRegistered))
        XCTAssertFalse(loginItemUnregisterAccepted(before: .enabled, after: .enabled))
        XCTAssertFalse(loginItemUnregisterAccepted(before: .enabled, after: .requiresApproval))
        let future = try XCTUnwrap(SMAppService.Status(rawValue: 999))
        XCTAssertFalse(loginItemUnregisterAccepted(before: .enabled, after: future))
    }

    // .notFound is a lookup error, not confirmation of unregistration,
    // so a registration that was live or pending before the call and is
    // .notFound after it fails closed. .notFound going in is the
    // distinct never-resolvable case (an ad-hoc signed bundle whose code
    // identity changed across an upgrade): no unregister() this bundle
    // can make will move that status, so refusing would strand the app
    // forever rather than protect a login item we could still reach.
    func testLoginItemUnregisterNotFoundDependsOnPriorStatus() throws {
        XCTAssertTrue(loginItemUnregisterAccepted(before: .notFound, after: .notFound))
        XCTAssertTrue(loginItemUnregisterAccepted(before: .notRegistered, after: .notFound))
        XCTAssertFalse(loginItemUnregisterAccepted(before: .enabled, after: .notFound))
        XCTAssertFalse(loginItemUnregisterAccepted(before: .requiresApproval, after: .notFound))
        let future = try XCTUnwrap(SMAppService.Status(rawValue: 999))
        XCTAssertFalse(loginItemUnregisterAccepted(before: future, after: .notFound))
    }

    // The prior-status gate is the "no live registration going in" test
    // on its own; an unknown future status fails closed.
    func testLoginItemRegistrationAbsent() throws {
        XCTAssertTrue(loginItemRegistrationAbsent(.notRegistered))
        XCTAssertTrue(loginItemRegistrationAbsent(.notFound))
        XCTAssertFalse(loginItemRegistrationAbsent(.enabled))
        XCTAssertFalse(loginItemRegistrationAbsent(.requiresApproval))
        let future = try XCTUnwrap(SMAppService.Status(rawValue: 999))
        XCTAssertFalse(loginItemRegistrationAbsent(future))
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
