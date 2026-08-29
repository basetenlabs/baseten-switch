import Darwin
import Foundation

// MARK: - Polling

protocol RuntimeClock: Sendable {
    var now: Date { get }
    func sleep(seconds: TimeInterval) async throws
}

struct SystemRuntimeClock: RuntimeClock {
    var now: Date { Date() }

    func sleep(seconds: TimeInterval) async throws {
        try await Task.sleep(
            nanoseconds: UInt64(max(seconds, 0) * 1_000_000_000))
    }
}

enum PollEvent: Equatable, Sendable {
    case snapshot(RoutingSnapshot)
    case unavailable(observedAt: Date)
    case ignoredStaleToken
}

/// Pure ordering state used by PollCoordinator and unit tests. A boot ID that
/// has been retired can never become current again, even if a delayed response
/// arrives after a router restart.
struct RoutingTokenAcceptance: Equatable, Sendable {
    private(set) var current: RoutingToken?
    private(set) var retiredBootIDs: Set<String> = []

    mutating func accept(_ token: RoutingToken) -> Bool {
        guard token.isAuthoritative else { return false }

        guard let current else {
            self.current = token
            return true
        }
        if current.routerBootID == token.routerBootID {
            guard token.activeGeneration >= current.activeGeneration else {
                return false
            }
            self.current = token
            return true
        }
        guard !retiredBootIDs.contains(token.routerBootID) else {
            return false
        }
        retiredBootIDs.insert(current.routerBootID)
        self.current = token
        return true
    }
}

/// Accepts poll responses in request order before applying routing-token
/// ordering. Equal routing tokens cannot make a late older request current.
struct PollEventAcceptance: Equatable, Sendable {
    private(set) var latestRequestID: UInt64 = 0
    private var routingTokens = RoutingTokenAcceptance()

    mutating func accept(
        requestID: UInt64,
        token: RoutingToken?
    ) -> Bool {
        guard requestID >= latestRequestID else { return false }
        latestRequestID = requestID
        if let token, !routingTokens.accept(token) {
            return false
        }
        return true
    }
}

actor PollCoordinator {
    typealias Handler = @MainActor @Sendable (PollEvent) -> Void

    private let reader: any AdminStatusReading
    private let clock: any RuntimeClock
    private let interval: TimeInterval
    private var loop: Task<Void, Never>?
    private struct InFlightStatusRequest {
        let id: UInt64
        let task: Task<
            Result<AdminStatusSnapshot, GatewayClientError>, Never>
    }

    private struct StatusRequestResult {
        let id: UInt64
        let result: Result<AdminStatusSnapshot, GatewayClientError>
    }

    private var inFlight: InFlightStatusRequest?
    private var nextRequestID: UInt64 = 0
    private var acceptance = PollEventAcceptance()

    init(reader: any AdminStatusReading,
         clock: any RuntimeClock,
         interval: TimeInterval = 5) {
        self.reader = reader
        self.clock = clock
        self.interval = max(interval, 5)
    }

    func start(handler: @escaping Handler) {
        guard loop == nil else { return }
        loop = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                let event = await self.refresh()
                await handler(event)
                do {
                    try await self.clock.sleep(seconds: self.interval)
                } catch {
                    return
                }
            }
        }
    }

    func stop() {
        loop?.cancel()
        loop = nil
        inFlight?.task.cancel()
        inFlight = nil
    }

    func refresh() async -> PollEvent {
        let result = await fetchSingleFlight()
        return event(for: result)
    }

    /// Waits out any status request that existed at the call boundary, then
    /// returns a snapshot from a request that began afterward. This provides
    /// the read barrier needed after a local admin mutation.
    func refreshFresh() async -> PollEvent {
        if let pending = inFlight {
            _ = await pending.task.value
            if inFlight?.id == pending.id {
                inFlight = nil
            }
        }
        return event(for: await fetchSingleFlight())
    }

    private func event(
        for request: StatusRequestResult
    ) -> PollEvent {
        switch request.result {
        case .success(let status):
            guard acceptance.accept(
                requestID: request.id,
                token: status.token
            ) else {
                return .ignoredStaleToken
            }
            return .snapshot(RoutingSnapshot(
                status: status,
                observedAt: clock.now))
        case .failure:
            guard acceptance.accept(
                requestID: request.id,
                token: nil
            ) else {
                return .ignoredStaleToken
            }
            return .unavailable(observedAt: clock.now)
        }
    }

    private func fetchSingleFlight()
        async -> StatusRequestResult {
        if let inFlight {
            return StatusRequestResult(
                id: inFlight.id,
                result: await inFlight.task.value)
        }
        let reader = self.reader
        nextRequestID &+= 1
        let requestID = nextRequestID
        let task = Task<Result<AdminStatusSnapshot, GatewayClientError>, Never> {
            do {
                return .success(try await reader.fetchStatus())
            } catch let error as GatewayClientError {
                return .failure(error)
            } catch {
                return .failure(.invalidPayload)
            }
        }
        inFlight = InFlightStatusRequest(id: requestID, task: task)
        let result = await task.value
        if inFlight?.id == requestID {
            inFlight = nil
        }
        return StatusRequestResult(id: requestID, result: result)
    }
}

// MARK: - Serialized, bounded child processes

struct CLIExecutionRequest: Equatable, Sendable {
    let binary: URL
    let arguments: [String]
    let environment: [String: String]
    let timeout: TimeInterval
}

struct CLIExecutionResult: Equatable, Sendable {
    let status: Int32
    let standardOutput: String
    let standardError: String
    let timedOut: Bool

    var succeeded: Bool {
        status == 0 && !timedOut
    }
}

protocol CLIRunning: Sendable {
    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult
}

private final class BoundedProcessBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var data = Data()
    private let limit: Int

    init(limit: Int) {
        self.limit = max(limit, 1)
    }

    func append(_ newData: Data) {
        guard !newData.isEmpty else { return }
        lock.lock()
        defer { lock.unlock() }
        let remaining = limit - data.count
        guard remaining > 0 else { return }
        data.append(newData.prefix(remaining))
    }

    func string() -> String {
        lock.lock()
        defer { lock.unlock() }
        return String(decoding: data, as: UTF8.self)
    }
}

private final class ProcessCompletion: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<(Int32, Bool), Never>?
    private var completed: (Int32, Bool)?
    private var timeoutTriggered = false

    func install(_ continuation: CheckedContinuation<(Int32, Bool), Never>) {
        lock.lock()
        if let completed {
            lock.unlock()
            continuation.resume(returning: completed)
            return
        }
        self.continuation = continuation
        lock.unlock()
    }

    func complete(status: Int32, timedOut: Bool) {
        lock.lock()
        guard completed == nil else {
            lock.unlock()
            return
        }
        completed = (status, timedOut)
        let continuation = self.continuation
        self.continuation = nil
        lock.unlock()
        continuation?.resume(returning: (status, timedOut))
    }

    func markTimedOut() {
        lock.lock()
        timeoutTriggered = true
        lock.unlock()
    }

    func completeFromProcess(status: Int32) {
        lock.lock()
        let timedOut = timeoutTriggered
        lock.unlock()
        complete(status: status, timedOut: timedOut)
    }
}

struct SystemCLIRunner: CLIRunning {
    private let outputLimit = 32 * 1024

    func run(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        let process = Process()
        process.executableURL = request.binary
        process.arguments = request.arguments
        process.environment = request.environment

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        let stdout = BoundedProcessBuffer(limit: outputLimit)
        let stderr = BoundedProcessBuffer(limit: outputLimit)
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe
        stdoutPipe.fileHandleForReading.readabilityHandler = { handle in
            stdout.append(handle.availableData)
        }
        stderrPipe.fileHandleForReading.readabilityHandler = { handle in
            stderr.append(handle.availableData)
        }

        let completion = ProcessCompletion()
        process.terminationHandler = { terminated in
            completion.completeFromProcess(status: terminated.terminationStatus)
        }

        do {
            try process.run()
        } catch {
            stdoutPipe.fileHandleForReading.readabilityHandler = nil
            stderrPipe.fileHandleForReading.readabilityHandler = nil
            return CLIExecutionResult(
                status: -1,
                standardOutput: "",
                standardError: "Unable to launch command.",
                timedOut: false)
        }

        let timeout = max(request.timeout, 0.1)
        let timeoutTask = Task.detached {
            try? await Task.sleep(
                nanoseconds: UInt64(timeout * 1_000_000_000))
            guard !Task.isCancelled else { return }
            guard process.isRunning else { return }
            // Mark before sending SIGTERM. Process.terminationHandler may run
            // immediately, and must not turn a timeout into a normal failure.
            completion.markTimedOut()
            process.terminate()
            try? await Task.sleep(nanoseconds: 1_000_000_000)
            if process.isRunning {
                kill(process.processIdentifier, SIGKILL)
            }
            completion.complete(status: -1, timedOut: true)
        }

        let (status, timedOut) = await withCheckedContinuation {
            completion.install($0)
        }
        timeoutTask.cancel()
        stdoutPipe.fileHandleForReading.readabilityHandler = nil
        stderrPipe.fileHandleForReading.readabilityHandler = nil
        if timedOut {
            try? stdoutPipe.fileHandleForReading.close()
            try? stderrPipe.fileHandleForReading.close()
        } else {
            stdout.append(stdoutPipe.fileHandleForReading.readDataToEndOfFile())
            stderr.append(stderrPipe.fileHandleForReading.readDataToEndOfFile())
        }

        return CLIExecutionResult(
            status: status,
            standardOutput: stdout.string(),
            standardError: redactDiagnosticText(stderr.string()),
            timedOut: timedOut)
    }
}

actor MutationCoordinator {
    private struct Waiter {
        let id: UUID
        let continuation: CheckedContinuation<Bool, Never>
    }

    private let runner: any CLIRunning
    private var busy = false
    private var waiters: [Waiter] = []

    init(runner: any CLIRunning) {
        self.runner = runner
    }

    func perform(_ request: CLIExecutionRequest) async -> CLIExecutionResult {
        guard await acquire() else {
            return canceledCLIExecutionResult
        }
        guard !Task.isCancelled else {
            release()
            return canceledCLIExecutionResult
        }
        defer { release() }
        return await runner.run(request)
    }

    private func acquire() async -> Bool {
        guard !Task.isCancelled else { return false }
        if !busy {
            busy = true
            return true
        }
        let id = UUID()
        return await withTaskCancellationHandler {
            await withCheckedContinuation { continuation in
                if Task.isCancelled {
                    continuation.resume(returning: false)
                } else {
                    waiters.append(Waiter(
                        id: id,
                        continuation: continuation))
                }
            }
        } onCancel: {
            Task { await self.cancelWaiter(id) }
        }
    }

    private func cancelWaiter(_ id: UUID) {
        if let index = waiters.firstIndex(where: { $0.id == id }) {
            let waiter = waiters.remove(at: index)
            waiter.continuation.resume(returning: false)
        }
    }

    private func release() {
        if waiters.isEmpty {
            busy = false
        } else {
            waiters.removeFirst().continuation.resume(returning: true)
        }
    }

    private var canceledCLIExecutionResult: CLIExecutionResult {
        CLIExecutionResult(
            status: -1,
            standardOutput: "",
            standardError: "",
            timedOut: false)
    }
}

struct GlobalMutationReceipt: Equatable, Sendable {
    var ok: Bool
    var operationID: String
    var operation: String
    var client: String
    var key: String
    var requested: Bool?
    var requestedTarget: String
    var desiredConfigHash: String
    var activeToken: String
    var activeConfigHash: String
    var applied: Bool
    var reconciliationRequired: Bool
    var reconciliationAction: String
    var blockingOperationID: String
    var outcome: String
    var cleanupPending: Bool
    var requestFingerprint: String
    var identityStrength: String
    var warnings: [String]
    var errorCode: String
    var errorMessage: String
    var errorRetryable: Bool

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any] else {
            return nil
        }
        ok = dict["ok"] as? Bool ?? false
        operationID = dict["operation_id"] as? String ?? ""
        operation = dict["operation"] as? String ?? ""
        client = dict["client"] as? String ?? ""
        key = dict["key"] as? String ?? ""
        requested = dict["requested"] as? Bool
        requestedTarget = dict["requested_target"] as? String ?? ""
        desiredConfigHash = dict["desired_config_hash"] as? String ?? ""
        activeToken = dict["active_token"] as? String ?? ""
        activeConfigHash = dict["active_config_hash"] as? String ?? ""
        applied = dict["applied"] as? Bool ?? false
        reconciliationRequired =
            dict["reconciliation_required"] as? Bool ?? false
        reconciliationAction =
            dict["reconciliation_action"] as? String ?? ""
        blockingOperationID = dict["blocking_operation_id"] as? String ?? ""
        outcome = dict["outcome"] as? String ?? ""
        cleanupPending = dict["cleanup_pending"] as? Bool ?? false
        requestFingerprint = dict["request_fingerprint"] as? String ?? ""
        identityStrength = dict["identity_strength"] as? String ?? ""
        warnings = (dict["warnings"] as? [String] ?? [])
            .filter { !$0.isEmpty }
        let error = dict["error"] as? [String: Any]
        errorCode = error?["code"] as? String ?? ""
        errorMessage = error?["message"] as? String ?? ""
        errorRetryable = error?["retryable"] as? Bool ?? false
    }
}

struct MutationRecoveryReceipt: Equatable, Sendable {
    var ok: Bool
    var classification: String
    var operationID: String
    var cleanupPending: Bool
    var errorCode: String
    var errorRetryable: Bool

    init?(json: String) {
        guard let data = json.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data),
              let dict = object as? [String: Any] else {
            return nil
        }
        ok = dict["ok"] as? Bool ?? false
        classification = dict["classification"] as? String
            ?? dict["status"] as? String
            ?? ""
        operationID = dict["operation_id"] as? String ?? ""
        cleanupPending = dict["cleanup_pending"] as? Bool ?? false
        let error = dict["error"] as? [String: Any]
        errorCode = error?["code"] as? String ?? ""
        errorRetryable = error?["retryable"] as? Bool ?? false
    }
}

func isValidMutationRequestFingerprint(_ value: String) -> Bool {
    guard value.hasPrefix("sha256:") else { return false }
    let digest = value.dropFirst("sha256:".count)
    return digest.count == 64
        && digest.allSatisfy {
            $0.isNumber || ("a"..."f").contains($0)
        }
}

func allowlistedCLIEnvironment(
    ambient: [String: String] = ProcessInfo.processInfo.environment,
    overrides: [String: String]
) -> [String: String] {
    var executableSearchPaths = [
        "/opt/homebrew/bin",
        "/usr/local/bin",
        "/usr/bin",
        "/bin",
        "/usr/sbin",
        "/sbin",
    ]
    if let localBin = currentUserLocalBin(home: ambient["HOME"]) {
        // Keep system locations first while supporting Claude Code's standard
        // user-local installer location without inheriting an ambient PATH.
        executableSearchPaths.append(localBin)
    }
    let inherited = [
        "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
        "LANG", "LC_ALL",
    ]
    var result: [String: String] = [
        "PATH": executableSearchPaths.joined(separator: ":"),
    ]
    for key in inherited {
        if let value = ambient[key], !value.isEmpty {
            result[key] = value
        }
    }
    for (key, value) in overrides {
        result[key] = value
    }
    return result
}

private func currentUserLocalBin(home: String?) -> String? {
    guard let home,
          !home.isEmpty,
          home.hasPrefix("/"),
          !home.contains(":"),
          !home.contains("\0") else { return nil }
    return URL(fileURLWithPath: home, isDirectory: true)
        .appendingPathComponent(".local/bin", isDirectory: true)
        .standardizedFileURL.path
}

/// Returns a user-facing refusal when any Preview runtime entry can escape the
/// canonical private root. The app runs this before every CLI operation,
/// including `up`, because Finder/Spotlight launches do not pass through the
/// shell helper's filesystem preflight.
func previewRuntimeFilesystemError(
    runtime: RuntimeProfile,
    fileManager: FileManager = .default
) -> String? {
    guard let configPath = runtime.expectedConfigPath, !configPath.isEmpty else {
        return "Preview runtime has no expected configuration path."
    }
    let rootURL = URL(fileURLWithPath: configPath)
        .deletingLastPathComponent()
        .standardizedFileURL
    let rootPath = rootURL.path
    guard rootPath.hasPrefix("/") else {
        return "Preview runtime root must be absolute."
    }

    let requiredDirectories = [
        rootPath,
        rootURL.appendingPathComponent("logs", isDirectory: true).path,
        rootURL.appendingPathComponent("backups", isDirectory: true).path,
        rootURL.appendingPathComponent("claude", isDirectory: true).path,
        rootURL.appendingPathComponent("baseten", isDirectory: true).path,
    ]
    let requiredFiles = [
        configPath,
        runtime.environment["BASETEN_SWITCH_ENV_FILE"] ?? "",
        runtime.environment["BASETEN_SWITCH_AUTH_FILE"] ?? "",
    ]
    for path in requiredDirectories {
        if let error = privateRuntimeEntryError(
            path: path,
            expectedDirectory: true,
            required: true,
            rootPath: rootPath) {
            return error
        }
    }
    for path in requiredFiles {
        if let error = privateRuntimeEntryError(
            path: path,
            expectedDirectory: false,
            required: true,
            rootPath: rootPath) {
            return error
        }
    }

    var enumerationError: String?
    guard let enumerator = fileManager.enumerator(
        at: rootURL,
        includingPropertiesForKeys: nil,
        options: [],
        errorHandler: { url, error in
            enumerationError = "Could not inspect Preview runtime entry \(url.path): \(error.localizedDescription)"
            return false
        }) else {
        return "Could not inspect Preview runtime root \(rootPath)."
    }
    while let url = enumerator.nextObject() as? URL {
        if let error = privateRuntimeEntryError(
            path: url.path,
            expectedDirectory: nil,
            required: true,
            rootPath: rootPath) {
            return error
        }
    }
    return enumerationError
}

private func privateRuntimeEntryError(
    path: String,
    expectedDirectory: Bool?,
    required: Bool,
    rootPath: String
) -> String? {
    guard !path.isEmpty else {
        return "Preview runtime contains an empty path."
    }
    let lexical = URL(fileURLWithPath: path).standardizedFileURL.path
    let rootLexical = URL(fileURLWithPath: rootPath).standardizedFileURL.path
    guard lexical == rootLexical || lexical.hasPrefix(rootLexical + "/") else {
        return "Preview runtime path is outside its private root: \(path)"
    }

    var info = stat()
    if lstat(lexical, &info) != 0 {
        if !required && errno == ENOENT {
            return nil
        }
        return "Preview runtime path is missing or unreadable: \(path)"
    }
    let kind = info.st_mode & mode_t(S_IFMT)
    if kind == mode_t(S_IFLNK) {
        return "Preview runtime paths must not be symlinks: \(path)"
    }
    if let expectedDirectory {
        let expectedKind = expectedDirectory ? mode_t(S_IFDIR) : mode_t(S_IFREG)
        if kind != expectedKind {
            return expectedDirectory
                ? "Preview runtime path must be a directory: \(path)"
                : "Preview runtime path must be a regular file: \(path)"
        }
    } else if kind != mode_t(S_IFDIR) && kind != mode_t(S_IFREG) {
        return "Preview runtime entries must be regular files or directories: \(path)"
    }
    if info.st_mode & mode_t(0o077) != 0 {
        return "Preview runtime permissions are unsafe: \(path)"
    }

    let canonicalRoot = URL(fileURLWithPath: rootLexical)
        .resolvingSymlinksInPath()
        .standardizedFileURL.path
    let canonicalPath = URL(fileURLWithPath: lexical)
        .resolvingSymlinksInPath()
        .standardizedFileURL.path
    guard canonicalPath == canonicalRoot ||
            canonicalPath.hasPrefix(canonicalRoot + "/") else {
        return "Preview runtime path resolves outside its private root: \(path)"
    }
    return nil
}

/// Defensive redaction for bounded child-process diagnostics. The UI reports
/// typed failures and exit codes; this text is retained only for local,
/// credential-safe diagnostics.
func redactDiagnosticText(_ raw: String) -> String {
    var redacted = raw
    let secretKeys = [
        "BASETEN_API_KEY", "BASETEN_SWITCH_API_KEY", "BASETEN_SWITCH_API_KEY_FALLBACK",
        "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY",
        "CODEX_AUTH_TOKEN", "BASETEN_SWITCH_ANTHROPIC_KEY", "Authorization",
    ]
    for key in secretKeys {
        // Redact the complete value through the end of the diagnostic line.
        // In particular, an Authorization value normally contains both a
        // scheme and credential (`Bearer secret`); token-only matching leaks
        // the credential after replacing just the scheme.
        let escaped = NSRegularExpression.escapedPattern(for: key)
        let pattern = #"(?im)(["']?\b\#(escaped)["']?\s*[:=]\s*)[^\r\n]*"#
        if let regex = try? NSRegularExpression(pattern: pattern) {
            let range = NSRange(redacted.startIndex..., in: redacted)
            redacted = regex.stringByReplacingMatches(
                in: redacted,
                range: range,
                withTemplate: "$1<redacted>")
        }
    }
    let secretFlagPattern =
        #"(?i)(--(?:api-key|auth-token|access-token|token)(?:=|\s+))([^\s]+)"#
    if let regex = try? NSRegularExpression(pattern: secretFlagPattern) {
        let range = NSRange(redacted.startIndex..., in: redacted)
        redacted = regex.stringByReplacingMatches(
            in: redacted,
            range: range,
            withTemplate: "$1<redacted>")
    }
    return menuErrorLabel(redacted, limit: 2_000)
}
