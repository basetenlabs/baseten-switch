import Combine
import Foundation

struct ReleaseVersion: Equatable, Comparable, Sendable {
    private let components: [Int]

    init?(_ value: String) {
        let version = value.hasPrefix("v") ? String(value.dropFirst()) : value
        let parts = version.split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 3,
              parts.allSatisfy({ !$0.isEmpty && $0.count <= 9
                  && ($0.count == 1 || $0.first != "0")
                  && $0.allSatisfy({ $0 >= "0" && $0 <= "9" }) }) else {
            return nil
        }
        components = parts.compactMap { Int($0) }
        guard components.count == 3 else { return nil }
    }

    static func < (lhs: Self, rhs: Self) -> Bool {
        lhs.components.lexicographicallyPrecedes(rhs.components)
    }
}

struct ReleaseUpdateSnapshot: Decodable, Equatable, Sendable {
    let currentVersion: String
    var availableVersion: String? = nil
    var releaseURL: URL? = nil
    var checkedAt: Date? = nil
    var lastAttemptAt: Date? = nil
    var nextCheckAt: Date? = nil
    let automaticCheck: Bool
    let installSource: String
    let status: String
    var error: String? = nil

    private enum CodingKeys: String, CodingKey {
        case currentVersion = "current_version"
        case availableVersion = "available_version"
        case releaseURL = "release_url"
        case checkedAt = "checked_at"
        case lastAttemptAt = "last_attempt_at"
        case nextCheckAt = "next_check_at"
        case automaticCheck = "automatic_check"
        case installSource = "install_source"
        case status, error
    }

    static func decode(_ output: String) throws -> Self {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let raw = try decoder.singleValueContainer().decode(String.self)
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = formatter.date(from: raw) { return date }
            formatter.formatOptions = [.withInternetDateTime]
            guard let date = formatter.date(from: raw) else {
                throw DecodingError.dataCorrupted(.init(
                    codingPath: decoder.codingPath,
                    debugDescription: "Invalid update check timestamp"))
            }
            return date
        }
        return try decoder.decode(Self.self, from: Data(output.utf8))
    }
}

/// One app-level projection of the CLI's shared public-release cache. Router
/// polling and routing mutations have a separate cadence and lifecycle.
@MainActor
final class UpdateStore: ObservableObject {
    @Published private(set) var snapshot: ReleaseUpdateSnapshot?
    @Published private(set) var isChecking = false
    @Published private(set) var error: String?

    let appVersion: String
    private let runner: any CLIRunning
    private let clock: any RuntimeClock
    private let binaryLocator: @MainActor () -> URL?
    private let environment: [String: String]
    private let enabled: Bool
    private var checkTask: Task<Void, Never>?
    private var loopTask: Task<Void, Never>?
    private var generation: UInt64 = 0

    init(variant: AppVariant = .current(),
         appVersion: String? = nil,
         snapshot: ReleaseUpdateSnapshot? = nil,
         runner: any CLIRunning = SystemCLIRunner(),
         clock: any RuntimeClock = SystemRuntimeClock(),
         binaryLocator: (@MainActor () -> URL?)? = nil,
         enabled: Bool = true) {
        let bundled = Bundle.main.object(
            forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        let version = appVersion ?? bundled.map { "v\($0)" } ?? "dev"
        self.appVersion = version
        self.snapshot = snapshot
        self.error = snapshot?.error
        self.runner = runner
        self.clock = clock
        self.binaryLocator = binaryLocator ?? {
            BasetenSwitchState.locateBasetenSwitchBinary(variant: variant)
        }
        self.environment = allowlistedCLIEnvironment(
            overrides: variant.runtime.environment)
        self.enabled = enabled && variant.channel == .stable
            && variant.identityError == nil
            && ReleaseVersion(version) != nil
            && ReleaseVersion(version) != ReleaseVersion("0.0.0")
        if enabled && !self.enabled && snapshot == nil {
            self.error = "Public release checks are unavailable for development and Preview builds."
        }
    }

    var automaticCheck: Bool { snapshot?.automaticCheck ?? true }
    var canCheck: Bool { enabled }

    var availableVersion: String? {
        guard let snapshot, let candidate = snapshot.availableVersion,
              let target = ReleaseVersion(candidate),
              let app = ReleaseVersion(appVersion),
              app != ReleaseVersion("0.0.0") else { return nil }
        let cliIsOlder = ReleaseVersion(snapshot.currentVersion).map { $0 < target } ?? false
        return app < target || cliIsOlder ? candidate : nil
    }

    var isCurrent: Bool {
        guard error == nil, let snapshot, snapshot.checkedAt != nil,
              let target = snapshot.availableVersion.flatMap(ReleaseVersion.init),
              let app = ReleaseVersion(appVersion),
              app != ReleaseVersion("0.0.0"),
              let cli = ReleaseVersion(snapshot.currentVersion) else { return false }
        return app >= target && cli >= target
    }

    /// Construct the destination from the validated numeric target rather
    /// than allowing a cached URL to open an arbitrary site.
    var releaseNotesURL: URL? {
        guard let target = snapshot?.availableVersion,
              ReleaseVersion(target) != nil else { return nil }
        let tag = target.hasPrefix("v") ? target : "v\(target)"
        return URL(string: "https://github.com/basetenlabs/baseten-switch/releases/tag/\(tag)")
    }

    func start() {
        guard enabled, loopTask == nil else { return }
        loopTask = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                self.check()
                await self.checkTask?.value
                guard !Task.isCancelled else { return }
                let delay = self.snapshot?.nextCheckAt.map {
                    max(60, $0.timeIntervalSince(self.clock.now))
                } ?? 86_400
                do {
                    try await self.clock.sleep(seconds: delay)
                } catch {
                    return
                }
            }
        }
    }

    func stop() {
        generation &+= 1
        loopTask?.cancel()
        loopTask = nil
        checkTask?.cancel()
        checkTask = nil
        isChecking = false
    }

    func check(force: Bool = false) {
        run(arguments: ["update", "check", "--json"] + (force ? ["--refresh"] : []))
    }

    func setAutomaticCheck(_ enabled: Bool) {
        run(arguments: ["update", "automatic", enabled ? "on" : "off", "--json"])
    }

    private func run(arguments: [String]) {
        guard enabled else {
            error = "Public release checks are unavailable for development and Preview builds."
            return
        }
        guard checkTask == nil else { return }
        guard let binary = binaryLocator() else {
            error = "The Switch CLI was not found. Install Switch to check for updates."
            return
        }
        isChecking = true
        error = nil
        generation &+= 1
        let requestGeneration = generation
        checkTask = Task { [weak self, runner] in
            guard let self else { return }
            let result = await runner.run(CLIExecutionRequest(
                binary: binary,
                arguments: arguments,
                environment: self.environment,
                timeout: 6))
            guard !Task.isCancelled, self.generation == requestGeneration else { return }
            if let snapshot = try? ReleaseUpdateSnapshot.decode(result.standardOutput) {
                self.snapshot = snapshot
                self.error = snapshot.error
                if !result.succeeded && self.error == nil {
                    self.error = "Unable to check for updates. Try again later."
                }
            } else {
                self.error = result.timedOut
                    ? "The update check timed out. Try again later."
                    : "The installed CLI could not check for updates. Update Switch and try again."
            }
            self.isChecking = false
            self.checkTask = nil
        }
    }
}
