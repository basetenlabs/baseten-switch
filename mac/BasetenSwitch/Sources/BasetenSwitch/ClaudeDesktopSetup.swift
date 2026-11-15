import AppKit
import Foundation

struct ClaudeDesktopInstallation: Equatable {
    let url: URL
    let version: String
}

@MainActor
protocol ClaudeDesktopApplicationControlling {
    var isRunning: Bool { get }
    func installation() -> ClaudeDesktopInstallation?
    func requestQuit() -> Bool
    func open(_ installation: ClaudeDesktopInstallation) async throws
}

@MainActor
struct SystemClaudeDesktopApplication: ClaudeDesktopApplicationControlling {
    private var applications: [NSRunningApplication] {
        NSRunningApplication.runningApplications(
            withBundleIdentifier: "com.anthropic.claudefordesktop")
    }

    var isRunning: Bool { applications.contains { !$0.isTerminated } }

    func installation() -> ClaudeDesktopInstallation? {
        guard
            let url = applications.first?.bundleURL
                ?? NSWorkspace.shared.urlForApplication(
                    withBundleIdentifier: "com.anthropic.claudefordesktop"),
            let data = try? Data(contentsOf: url.appendingPathComponent("Contents/Info.plist")),
            let info = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
            info["CFBundleIdentifier"] as? String == "com.anthropic.claudefordesktop",
            let version = info["CFBundleShortVersionString"] as? String
        else { return nil }
        return ClaudeDesktopInstallation(url: url, version: version)
    }

    func requestQuit() -> Bool {
        applications.filter { !$0.isTerminated }.allSatisfy { $0.terminate() }
    }

    func open(_ installation: ClaudeDesktopInstallation) async throws {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = true
        try await withCheckedThrowingContinuation {
            (continuation: CheckedContinuation<Void, Error>) in
            NSWorkspace.shared.openApplication(
                at: installation.url, configuration: configuration
            ) { _, error in
                if let error { continuation.resume(throwing: error) } else { continuation.resume() }
            }
        }
    }
}

struct ClaudeDesktopSetupRequest: Equatable {
    let installation: ClaudeDesktopInstallation
    let connection: ClaudeDesktopConnectionSettings
}

enum ClaudeDesktopSetupPhase: Equatable {
    case ready
    case configured
    case confirming(ClaudeDesktopSetupRequest)
    case applying
    case waiting(since: Date)
    case notObserved(since: Date)
    case connected(since: Date)
    case unavailable(String)

    var isWorking: Bool {
        switch self {
        case .applying, .waiting: return true
        default: return false
        }
    }
}

@MainActor
final class ClaudeDesktopSetup: ObservableObject {
    @Published private(set) var phase: ClaudeDesktopSetupPhase = .ready

    private let store: ClaudeDesktopConfigurationStore
    private let application: any ClaudeDesktopApplicationControlling
    private let quitTimeout: TimeInterval
    private let discoveryTimeout: TimeInterval

    init(
        store: ClaudeDesktopConfigurationStore = .init(),
        application: (any ClaudeDesktopApplicationControlling)? = nil,
        quitTimeout: TimeInterval = 20,
        discoveryTimeout: TimeInterval = 30
    ) {
        self.store = store
        self.application = application ?? SystemClaudeDesktopApplication()
        self.quitTimeout = quitTimeout
        self.discoveryTimeout = discoveryTimeout
    }

    func load(connection: ClaudeDesktopConnectionSettings) {
        guard case .ready = phase else { return }
        if store.isConfigured(connection: connection) { phase = .configured }
    }

    func prepare(connection: ClaudeDesktopConnectionSettings) {
        guard !phase.isWorking else { return }
        guard let installation = application.installation() else {
            phase = .unavailable("Claude Desktop is not installed. Install it, then try again.")
            return
        }
        do {
            try store.validate(appVersion: installation.version, connection: connection)
            phase = .confirming(
                ClaudeDesktopSetupRequest(
                    installation: installation, connection: connection))
        } catch {
            phase = .unavailable(error.localizedDescription)
        }
    }

    func cancelRestart() {
        guard case .confirming(let request) = phase else { return }
        phase = store.isConfigured(connection: request.connection) ? .configured : .ready
    }

    func connect() async {
        guard case .confirming(let request) = phase else { return }
        phase = .applying
        if application.isRunning && !application.requestQuit() {
            phase = .unavailable(
                "Claude Desktop could not quit. Save your work and try again. Its settings have not been changed."
            )
            return
        }
        let deadline = Date().addingTimeInterval(quitTimeout)
        while application.isRunning && Date() < deadline {
            do { try await Task.sleep(nanoseconds: 200_000_000) } catch {
                phase = .unavailable("Setup stopped before changing Claude Desktop’s settings.")
                return
            }
        }
        guard !application.isRunning else {
            phase = .unavailable(
                "Claude Desktop is still running. Finish any open prompts, quit Claude, then try again. Its settings have not been changed."
            )
            return
        }
        guard let installation = application.installation(),
            installation.version == request.installation.version
        else {
            phase = .unavailable(
                "Claude Desktop changed during setup. Try connecting again. Its settings have not been changed."
            )
            return
        }
        do {
            try Task.checkCancellation()
            _ = try store.apply(
                appVersion: installation.version,
                connection: request.connection,
                isClaudeRunning: { application.isRunning })
        } catch {
            phase = .unavailable(error.localizedDescription)
            return
        }
        let startedAt = Date()
        phase = .waiting(since: startedAt)
        do { try await application.open(installation) } catch {
            phase = .notObserved(since: startedAt)
        }
    }

    func beginManualVerification(since: Date) {
        guard !phase.isWorking else { return }
        phase = .waiting(since: since)
    }

    func connectionIsConfirmed(
        by status: ClaudeDesktopStatus?, isCurrent: Bool, now: Date = Date()
    ) -> Bool {
        guard case .connected(let since) = phase else { return false }
        return hasFreshDiscovery(status, isCurrent: isCurrent, since: since, now: now)
    }

    func observe(_ status: ClaudeDesktopStatus?, isCurrent: Bool, now: Date = Date()) {
        let since: Date
        let wasConnected: Bool
        switch phase {
        case .waiting(let started), .notObserved(let started):
            since = started
            wasConnected = false
        case .connected(let started):
            since = started
            wasConnected = true
        default: return
        }
        if hasFreshDiscovery(status, isCurrent: isCurrent, since: since, now: now) {
            phase = .connected(since: since)
        } else if wasConnected || now.timeIntervalSince(since) >= discoveryTimeout {
            phase = .notObserved(since: wasConnected && !application.isRunning ? now : since)
        }
    }

    private func hasFreshDiscovery(
        _ status: ClaudeDesktopStatus?, isCurrent: Bool, since: Date, now: Date
    ) -> Bool {
        isCurrent && status?.gatewayReady == true
            && status?.modelDiscovery.map {
                $0.lastServedAt >= since && $0.lastServedAt <= now.addingTimeInterval(5)
            } == true && application.isRunning
    }
}
