import AppKit
import SwiftUI

func releaseVersionLabel(_ version: String) -> String {
    guard !version.isEmpty else { return "Unknown" }
    guard ReleaseVersion(version) != nil else { return version }
    return version.hasPrefix("v") ? version : "v\(version)"
}

struct UpdateInstallationInstructions: Equatable {
    let description: String
    let command: String?

    init(source: String) {
        switch source {
        case "homebrew":
            description = "Run this command in Terminal to install the release."
            command = "brew update && brew upgrade baseten-switch"
        case "nix":
            description = "Run this command in Terminal to upgrade your Nix profile installation."
            command = "nix profile upgrade --refresh baseten-switch"
        case "source":
            description = "Update your source checkout to the published release, then rebuild and install the CLI and Mac app."
            command = nil
        default:
            description = "Use the release notes to find the installation instructions for your setup. If you installed with a package manager, upgrade through that same package manager."
            command = nil
        }
    }
}

struct UpdateNoticeView: View {
    @ObservedObject var updates: UpdateStore
    let openUpdates: () -> Void

    var body: some View {
        if let version = updates.availableVersion {
            HStack(spacing: 12) {
                Image(systemName: "arrow.up.circle")
                    .font(.system(size: 16))
                    .foregroundStyle(.blue)
                    .accessibilityHidden(true)
                VStack(alignment: .leading, spacing: 2) {
                    Text("Baseten Switch \(releaseVersionLabel(version)) is available")
                        .font(.system(size: 12, weight: .semibold))
                    Text("You're using \(releaseVersionLabel(updates.appVersion))")
                        .font(.system(size: 11))
                        .foregroundStyle(.secondary)
                }
                Spacer(minLength: 8)
                Button("How to Update…", action: openUpdates)
                    .controlSize(.small)
                    .accessibilityIdentifier("open-software-update")
            }
            .padding(.horizontal, 20)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, minHeight: 48)
            .background(Color.blue.opacity(0.07))
            .overlay(alignment: .bottom) {
                Rectangle().fill(Color.blue.opacity(0.14)).frame(height: 1)
            }
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("update-available-notice")
        }
    }
}

@MainActor
final class UpdateWindowController: NSObject, NSWindowDelegate {
    private let updates: UpdateStore
    private let variant: AppVariant
    private let windowOpenChanged: (Bool) -> Void
    private var presentationState = RouterWindowPresentationState()
    private var window: NSWindow?

    init(updates: UpdateStore,
         variant: AppVariant = .current(),
         windowOpenChanged: @escaping (Bool) -> Void = { _ in }) {
        self.updates = updates
        self.variant = variant
        self.windowOpenChanged = windowOpenChanged
        super.init()
    }

    func show() {
        if presentationState.transition(to: true) {
            windowOpenChanged(true)
        }
        let window = window ?? makeWindow()
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
        if updates.canCheck { updates.check() }
    }

    func windowDidBecomeKey(_ notification: Notification) {
        if presentationState.transition(to: true) {
            windowOpenChanged(true)
        }
    }

    func windowWillClose(_ notification: Notification) {
        if presentationState.transition(to: false) {
            windowOpenChanged(false)
        }
    }

#if DEBUG
    func makeWindowForTesting() -> NSWindow { makeWindow() }
#endif

    private func makeWindow() -> NSWindow {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 680, height: 560),
            styleMask: [.titled, .closable],
            backing: .buffered,
            defer: false)
        window.title = "Software Update · \(variant.displayName)"
        window.isReleasedWhenClosed = false
        window.tabbingMode = .disallowed
        window.contentViewController = NSHostingController(
            rootView: UpdateDetailsView(
                updates: updates,
                done: { [weak window] in window?.performClose(nil) }))
        window.contentViewController?.view.layoutSubtreeIfNeeded()
        window.setContentSize(NSSize(width: 680, height: 560))
        window.delegate = self
        window.center()
        self.window = window
        return window
    }
}

struct UpdateDetailsView: View {
    @ObservedObject var updates: UpdateStore
    let done: () -> Void

    private var instructions: UpdateInstallationInstructions {
        UpdateInstallationInstructions(source: updates.snapshot?.installSource ?? "unknown")
    }

    private var title: String {
        if let version = updates.availableVersion {
            return "Baseten Switch \(releaseVersionLabel(version)) is available"
        }
        if updates.isChecking { return "Checking for updates…" }
        if updates.error != nil { return "Unable to check for updates" }
        if updates.isCurrent {
            return "Baseten Switch is up to date"
        }
        return "Software Update"
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                HStack(alignment: .top, spacing: 12) {
                    Image(nsImage: BasetenSwitchApp.menubarIcon)
                        .resizable().frame(width: 34, height: 34)
                        .foregroundStyle(.secondary)
                        .accessibilityHidden(true)
                    VStack(alignment: .leading, spacing: 5) {
                        Text(title).font(.system(size: 18, weight: .semibold))
                        Text("Mac app \(releaseVersionLabel(updates.appVersion)) · CLI \(cliVersion)")
                            .font(.system(size: 12)).foregroundStyle(.secondary)
                        if let releaseURL = updates.releaseNotesURL {
                            Link("Release Notes ↗", destination: releaseURL)
                                .font(.system(size: 12))
                        }
                    }
                }
                Divider()
                if updates.availableVersion != nil {
                    VStack(alignment: .leading, spacing: 8) {
                        Text("1. Install the update")
                            .font(.system(size: 13, weight: .semibold))
                        Text(instructions.description)
                            .font(.system(size: 12)).foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                        if let command = instructions.command {
                            UpdateCommandView(command: command)
                        }
                    }
                    VStack(alignment: .leading, spacing: 8) {
                        Text("2. Apply the update when you're ready")
                            .font(.system(size: 13, weight: .semibold))
                        Text("Finish active coding requests, then run this command to restart the local gateway and open the updated Mac app.")
                            .font(.system(size: 12)).foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                        UpdateCommandView(command: "baseten-switch up")
                    }
                } else if !updates.isChecking, updates.error == nil {
                    Text(updates.isCurrent
                        ? "You're using the latest release available for your installation."
                        : (updates.canCheck
                            ? "Check for updates to find the latest release available for your installation."
                            : "Public release checks are available in packaged releases. This build does not check for public updates."))
                        .font(.system(size: 12)).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Divider()
                Toggle("Automatically check for updates", isOn: Binding(
                    get: { updates.automaticCheck },
                    set: { updates.setAutomaticCheck($0) }))
                    .toggleStyle(.checkbox)
                    .disabled(updates.isChecking || !updates.canCheck)
                    .font(.system(size: 12))
                    .accessibilityIdentifier("automatic-update-checks")
                Text("Checks once a day. Updates are installed manually.")
                    .font(.system(size: 11)).foregroundStyle(.secondary)
                    .padding(.top, -14)
                if let error = updates.error {
                    Text(error)
                        .font(.system(size: 12)).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("update-check-error")
                }
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        if updates.isChecking {
                            Text("Checking for updates…")
                        } else if let checkedAt = updates.snapshot?.checkedAt {
                            Text("Last checked \(checkedAt.formatted(date: .abbreviated, time: .shortened))")
                        } else {
                            Text("No successful check yet")
                        }
                    }
                    .font(.system(size: 11)).foregroundStyle(.secondary)
                    Spacer()
                    Button("Check Again") { updates.check(force: true) }
                        .disabled(updates.isChecking || !updates.canCheck)
                        .accessibilityIdentifier("check-for-updates")
                    Button("Done", action: done)
                        .keyboardShortcut(.defaultAction)
                }
            }
            .padding(32)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .frame(width: 680, height: 560)
        .accessibilityIdentifier("software-update-details")
    }

    private var cliVersion: String {
        guard let version = updates.snapshot?.currentVersion,
              !version.isEmpty else { return "Unknown" }
        return releaseVersionLabel(version)
    }
}

private struct UpdateCommandView: View {
    let command: String
    @State private var copied = false

    var body: some View {
        HStack(spacing: 12) {
            Text(command)
                .font(.system(size: 12, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button(copied ? "Copied" : "Copy") {
                NSPasteboard.general.clearContents()
                copied = NSPasteboard.general.setString(command, forType: .string)
            }
            .controlSize(.small)
            .accessibilityLabel("Copy \(command)")
        }
        .padding(10)
        .background(Color(nsColor: .controlBackgroundColor))
        .clipShape(RoundedRectangle(cornerRadius: 6))
        .overlay {
            RoundedRectangle(cornerRadius: 6)
                .stroke(Color.primary.opacity(0.1), lineWidth: 1)
        }
    }
}
