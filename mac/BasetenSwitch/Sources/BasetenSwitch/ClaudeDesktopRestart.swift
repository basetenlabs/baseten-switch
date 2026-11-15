import Foundation

@MainActor
final class ClaudeDesktopRestart: ObservableObject {
    enum Phase: Equatable {
        case ready, confirming, quitting, opening, waiting
        case failed(String)
        var isWorking: Bool { self == .quitting || self == .opening }
    }

    private struct Record: Codable {
        var baseline: String?
        var pending: String?
        var changedAt: Date?
        var connected = false
    }

    @Published private(set) var phase: Phase = .ready
    @Published private(set) var needsRestart = false
    @Published private(set) var isRunning = false
    private var record: Record
    private let defaults: UserDefaults
    private let key: String
    private let application: any ClaudeDesktopApplicationControlling
    private let quitTimeout: TimeInterval
    private let persists: Bool

    init(defaults: UserDefaults = .standard, key: String,
         application: (any ClaudeDesktopApplicationControlling)? = nil,
         quitTimeout: TimeInterval = 20, persists: Bool = true) {
        self.defaults = defaults
        self.key = key
        self.application = application ?? SystemClaudeDesktopApplication()
        self.quitTimeout = quitTimeout
        self.persists = persists
        record = persists ? defaults.data(forKey: key).flatMap {
            try? JSONDecoder().decode(Record.self, from: $0)
        } ?? Record() : Record()
        needsRestart = record.connected && record.pending != nil
    }

    private func save() {
        needsRestart = record.connected && record.pending != nil
        if persists, let data = try? JSONEncoder().encode(record) {
            defaults.set(data, forKey: key)
        }
    }

    func setConfigured() {
        record.connected = true
        save()
    }

    func observe(_ status: ClaudeDesktopStatus?, isCurrent: Bool, now: Date = Date(), changedSince: Date? = nil) {
        isRunning = application.isRunning
        guard isCurrent, let status, status.gatewayReady,
              let desired = status.modelListRevision, !desired.isEmpty else { return }
        let discovery = status.modelDiscovery
        let fresh = isRunning && discovery.map {
            $0.revision == desired && $0.lastServedAt <= now.addingTimeInterval(5)
                && $0.lastServedAt >= (record.changedAt ?? .distantPast)
        } == true
        if fresh {
            record.connected = true
            record.baseline = desired
            record.pending = nil
            record.changedAt = nil
            if phase == .waiting { phase = .ready }
        } else if record.baseline == nil && record.pending == nil {
            record.baseline = desired
        } else if record.connected && desired != record.baseline {
            if record.pending != desired { record.changedAt = changedSince ?? now }
            record.pending = desired
        } else if desired == record.baseline {
            record.pending = nil
            record.changedAt = nil
        }
        save()
    }

    func prepare() async {
        guard !phase.isWorking else { return }
        isRunning = application.isRunning
        if isRunning { phase = .confirming } else { await restart() }
    }

    func cancel() { if phase == .confirming { phase = .ready } }

    func restart() async {
        guard !phase.isWorking else { return }
        guard application.installation() != nil else {
            phase = .failed("Claude Desktop is not installed. Install it, then try again.")
            return
        }
        phase = .quitting
        if application.isRunning && !application.requestQuit() {
            phase = .failed("Claude Desktop could not quit. Save your work and try again.")
            return
        }
        let deadline = Date().addingTimeInterval(quitTimeout)
        while application.isRunning && Date() < deadline {
            do { try await Task.sleep(nanoseconds: 200_000_000) } catch {
                phase = .failed("Restart canceled. Your model changes are still saved.")
                return
            }
        }
        guard !application.isRunning else {
            phase = .failed("Claude Desktop is still running. Finish any open prompts, then try again.")
            return
        }
        guard let installation = application.installation() else {
            phase = .failed("Claude Desktop could not be found. Open it manually, then try again.")
            return
        }
        phase = .opening
        do {
            try Task.checkCancellation()
            try await application.open(installation)
            isRunning = application.isRunning
            phase = .waiting
        } catch {
            phase = .failed("Claude Desktop could not be opened. Your model changes are still saved. Try again or open Claude manually.")
        }
    }
}
