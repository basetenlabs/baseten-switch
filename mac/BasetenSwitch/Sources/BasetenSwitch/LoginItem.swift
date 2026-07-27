import Foundation
import ServiceManagement

// MARK: - Pure decision logic (unit-tested; no SMAppService calls)

/// What launch-time reconciliation should do for a given
/// (status, first-launch flag) pair. Pure value so the decision table
/// is unit-testable; the SMAppService calls stay in LoginItem's thin
/// wrappers (the Display.swift pattern).
enum LoginItemLaunchAction: Equatable {
    /// Nothing to do: healthy, pending approval (only the user can
    /// approve; the popup surfaces it), or the user's explicit
    /// toggle-off.
    case leaveAlone
    /// Call register(). persistFlag is true when this launch is also
    /// the first-launch default, so success should persist the
    /// one-time flag; a stale-registration repair (flag already set)
    /// leaves the flag untouched.
    case register(persistFlag: Bool)
    /// Status already satisfies the first-launch default; just persist
    /// the flag.
    case persistFlagOnly
}

/// Launch-time reconciliation decision, evaluated on every launch so
/// registrations invalidated by an app replacement or signing change are
/// repaired:
/// - .notFound always re-registers, flag or no flag. macOS flips a
///   registered item to .notFound when the registration goes stale
///   (bundle replaced by a refresh, signature identity changed); that
///   is not the user opting out.
/// - .notRegistered with the flag set is the user's explicit
///   toggle-off; leave it alone.
/// - .notRegistered without the flag is the first launch; register
///   with the default enabled.
/// - .enabled / .requiresApproval only need the flag persisted on
///   first launch. requiresApproval is not repaired here (register
///   cannot approve); the popup caption points at System Settings.
func loginItemLaunchAction(status: SMAppService.Status,
                           firstLaunchFlagSet: Bool) -> LoginItemLaunchAction {
    switch status {
    case .notFound:
        return .register(persistFlag: !firstLaunchFlagSet)
    case .notRegistered:
        return firstLaunchFlagSet ? .leaveAlone : .register(persistFlag: true)
    case .enabled, .requiresApproval:
        return firstLaunchFlagSet ? .leaveAlone : .persistFlagOnly
    @unknown default:
        return .leaveAlone
    }
}

/// What the user's toggle click should do: registered (or pending
/// approval) turns off; anything else turns on.
enum LoginItemToggleAction: Equatable {
    case register
    case unregister
}

func loginItemToggleAction(status: SMAppService.Status) -> LoginItemToggleAction {
    switch status {
    case .enabled, .requiresApproval:
        return .unregister
    default:
        return .register
    }
}

/// Stable human name for a status, for logs and the reconciliation
/// transition line.
func loginItemStatusName(_ status: SMAppService.Status) -> String {
    switch status {
    case .notRegistered: return "notRegistered"
    case .enabled: return "enabled"
    case .requiresApproval: return "requiresApproval"
    case .notFound: return "notFound"
    @unknown default: return "unknown(\(status.rawValue))"
    }
}

// MARK: - Thin SMAppService wrappers

/// Start-at-Login via SMAppService.mainApp (macOS 13+). The app owns
/// its own login item; launchd owns the router and door
/// (the lifecycle contract, "Menubar app packaging"). Registration only
/// works when the process runs from a real .app bundle
/// (scripts/build-menubar.sh); from a bare SwiftPM binary the calls
/// throw and we log instead of crashing.
enum LoginItem {
    private static let firstLaunchDefaultKey = "baseten-switch.didDefaultStartAtLogin"

    static var status: SMAppService.Status {
        SMAppService.mainApp.status
    }

    /// Launch-time reconciliation, run on every launch. The decision
    /// is loginItemLaunchAction (pure, tested); this wrapper reads the
    /// inputs, performs the SMAppService calls, and logs the status
    /// transition. The first-launch flag is only persisted once
    /// registration succeeds (or the item is already registered), so a
    /// failed attempt from a bare dev binary does not burn the
    /// one-time default for the bundled app.
    ///
    /// Development signing caveat: a locally rebuilt, ad-hoc-signed bundle
    /// presents a new signing identity and macOS flips the previous
    /// registration to .notFound; on the refresh cadence this repair runs
    /// after every update. Each successful register() posts a system
    /// "Login Item Added" notification, and the superseded identity's
    /// entry can linger as a duplicate row in System Settings > Login
    /// Items (the accumulation is documented for unsigned/ad-hoc apps
    /// in sindresorhus/LaunchAtLogin-Legacy issue #100). The
    /// best-effort unregister below drops the stale entry when macOS
    /// can still associate it with this bundle; the per-update
    /// notification is the accepted cost of repairing the registration.
    static func reconcileAtLaunch() {
        let defaults = UserDefaults.standard
        let flagSet = defaults.bool(forKey: firstLaunchDefaultKey)
        let before = status
        switch loginItemLaunchAction(status: before, firstLaunchFlagSet: flagSet) {
        case .leaveAlone:
            break
        case .persistFlagOnly:
            defaults.set(true, forKey: firstLaunchDefaultKey)
            log("Start at Login already \(loginItemStatusName(before)); first-launch default recorded")
        case .register(let persistFlag):
            do {
                if before == .notFound {
                    // Stale-registration repair: clear whatever entry
                    // macOS can still tie to this bundle before
                    // re-registering, so duplicate Login Items rows do
                    // not accumulate across ad-hoc-signed updates.
                    // Best-effort by design; with no matching entry
                    // the call just throws.
                    try? SMAppService.mainApp.unregister()
                }
                try SMAppService.mainApp.register()
                if persistFlag {
                    defaults.set(true, forKey: firstLaunchDefaultKey)
                }
                log("Start at Login reconciled: \(loginItemStatusName(before)) -> \(loginItemStatusName(status))")
            } catch {
                log("Start at Login registration failed (still \(loginItemStatusName(status))): \(error)")
            }
        }
    }

    /// Flip the login item. The decision is loginItemToggleAction
    /// (pure, tested): registered or pending approval turns off;
    /// anything else (including a stale .notFound) turns on, so the
    /// toggle doubles as the retry when launch-time re-registration
    /// failed.
    static func toggle() {
        do {
            switch loginItemToggleAction(status: status) {
            case .unregister:
                try SMAppService.mainApp.unregister()
                log("unregistered Start at Login")
            case .register:
                try SMAppService.mainApp.register()
                log("registered Start at Login")
            }
        } catch {
            log("Start at Login toggle failed: \(error)")
        }
    }

    /// Open System Settings > General > Login Items: the one-click
    /// path for the requiresApproval state, where only the user's
    /// approval in that pane can enable the item.
    static func openSystemSettings() {
        SMAppService.openSystemSettingsLoginItems()
    }

    private static func log(_ msg: String) {
        FileHandle.standardError.write(Data("[BasetenSwitch] \(msg)\n".utf8))
    }
}
