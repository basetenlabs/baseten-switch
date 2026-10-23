import Foundation
import Darwin

struct ClaudeDesktopConnectionSettings: Equatable, Sendable {
    let baseURL: String
    let apiKey: String
    let authScheme: String

    init(baseURL: String, apiKey: String, authScheme: String = "bearer") {
        self.baseURL = baseURL
        self.apiKey = apiKey
        self.authScheme = authScheme
    }

    func configurationData() throws -> Data {
        guard let url = URLComponents(string: baseURL), url.scheme == "http",
              ["127.0.0.1", "localhost", "[::1]", "::1"].contains(url.host ?? ""),
              let port = url.port, (1...65535).contains(port), url.user == nil,
              url.password == nil, url.path.isEmpty, url.query == nil, url.fragment == nil,
              apiKey == "baseten-switch-local", authScheme == "bearer" else {
            throw ClaudeDesktopConfigurationError.invalidConnection
        }
        return try JSONSerialization.data(withJSONObject: [
            "inferenceProvider": "gateway",
            "inferenceGatewayBaseUrl": baseURL,
            "inferenceCredentialKind": "static",
            "inferenceGatewayApiKey": apiKey,
            "inferenceGatewayAuthScheme": authScheme,
            "chatTabEnabled": true,
            "autoModeEnabled": false,
        ], options: [.prettyPrinted, .sortedKeys])
    }
}

struct ClaudeDesktopConfigurationReceipt {
    let backupDirectoryURL: URL
}

enum ClaudeDesktopConfigurationError: LocalizedError {
    case unsupportedVersion, invalidConnection, managedPolicy, unsafeFiles, invalidLibrary
    case running, changedFiles, recoveryRequired, fileAccess

    var errorDescription: String? {
        switch self {
        case .unsupportedVersion:
            return "This Claude Desktop version has not been verified for automatic setup. Use manual setup."
        case .invalidConnection:
            return "Automatic setup requires Switch's local HTTP address and local placeholder key. Refresh Switch and try again."
        case .managedPolicy:
            return "Claude Desktop has managed preferences. Contact your administrator or use your organization's setup instructions."
        case .unsafeFiles:
            return "Claude Desktop configuration contains a symbolic link or an unexpected file type. Use manual setup."
        case .invalidLibrary:
            return "Claude Desktop's configuration library is not recognized. Use manual setup to preserve your existing configuration."
        case .running:
            return "Quit Claude Desktop completely before connecting it to Switch."
        case .changedFiles:
            return "Claude Desktop configuration changed during setup. Retry after closing Claude Desktop and other configuration editors."
        case .recoveryRequired:
            return "An earlier setup needs recovery, but its files have changed. Your backup is retained in Application Support/Baseten Switch/Claude Desktop Setup. Restore it manually before retrying."
        case .fileAccess:
            return "Switch could not read or save Claude Desktop's configuration. Check file permissions and try again."
        }
    }
}

struct ClaudeDesktopConfigurationStore {
    private let support: URL
    private let managedPreferences: URL
    private let username: String
    private let configurationName: String
    private let fm = FileManager.default
    private var root: URL { support.appendingPathComponent("Claude-3p") }
    private var library: URL { root.appendingPathComponent("configLibrary") }
    private var state: URL { support.appendingPathComponent("Baseten Switch/Claude Desktop Setup") }
    private var pending: URL { state.appendingPathComponent("pending.json") }
    private var ownership: URL { state.appendingPathComponent("ownership.json") }

    init(applicationSupportURL: URL = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support"),
         managedPreferencesURL: URL = URL(fileURLWithPath: "/Library/Managed Preferences"),
         username: String = NSUserName(), configurationName: String = "Baseten Switch") {
        support = applicationSupportURL
        managedPreferences = managedPreferencesURL
        self.username = username
        self.configurationName = configurationName
    }

    func validate(appVersion: String, connection: ClaudeDesktopConnectionSettings) throws {
        try access {
            try validateInputs(appVersion, connection)
            let recovery = try recoveryRecord()
            if let recovery { try checkRecovery(recovery) }
            _ = try prepare(connection, recovery: recovery)
        }
    }

    func apply(appVersion: String, connection: ClaudeDesktopConnectionSettings,
               isClaudeRunning: () -> Bool) throws -> ClaudeDesktopConfigurationReceipt {
        try access {
            try validateInputs(appVersion, connection)
            guard !isClaudeRunning() else { throw ClaudeDesktopConfigurationError.running }
            if let recovery = try recoveryRecord() {
                try restore(recovery, isClaudeRunning: isClaudeRunning)
            }
            let plan = try prepare(connection)
            guard !isClaudeRunning() else { throw ClaudeDesktopConfigurationError.running }
            try makeDirectory(state)
            let backup = state.appendingPathComponent(UUID().uuidString)
            try makeDirectory(backup)
            var items: [Change] = []
            for (index, write) in plan.enumerated() {
                if let original = write.before {
                    try atomic(original.data, at: backup.appendingPathComponent("\(index).data"), mode: 0o600)
                }
                items.append(Change(path: write.path, originalMode: write.before?.mode, replacement: write.data))
            }
            let createdDirectories = try ["Claude-3p", "Claude-3p/configLibrary"].filter {
                try attributes(support.appendingPathComponent($0)) == nil
            }
            let record = Recovery(backup: backup.lastPathComponent, changes: items, createdDirectories: createdDirectories)
            try atomic(JSONEncoder().encode(record), at: pending, mode: 0o600)
            do {
                for write in plan {
                    guard !isClaudeRunning() else { throw ClaudeDesktopConfigurationError.running }
                    let destination = try target(write.path)
                    guard try snapshot(destination) == write.before else { throw ClaudeDesktopConfigurationError.changedFiles }
                    try makeDirectory(destination.deletingLastPathComponent())
                    try atomic(write.data, at: destination, mode: write.before?.mode ?? 0o600)
                }
                guard !isClaudeRunning() else { throw ClaudeDesktopConfigurationError.running }
                for write in plan {
                    guard try snapshot(target(write.path)) == Snapshot(data: write.data, mode: write.before?.mode ?? 0o600)
                    else { throw ClaudeDesktopConfigurationError.changedFiles }
                }
                try fm.removeItem(at: pending)
                try synchronizeDirectory(state)
                return ClaudeDesktopConfigurationReceipt(backupDirectoryURL: backup)
            } catch {
                do { try restore(record, isClaudeRunning: isClaudeRunning) }
                catch { throw ClaudeDesktopConfigurationError.recoveryRequired }
                throw error
            }
        }
    }

    func isConfigured(connection: ClaudeDesktopConnectionSettings) -> Bool {
        do {
            _ = try connection.configurationData()
            try checkPolicy()
            guard try snapshot(pending) == nil else { return false }
            let meta = try metadata()
            guard let id = meta["appliedId"] as? String,
                  let entries = meta["entries"] as? [[String: Any]],
                  entries.contains(where: { $0["id"] as? String == id }),
                  try object(at: root.appendingPathComponent("claude_desktop_config.json"))["deploymentMode"] as? String == "3p",
                  let file = try snapshot(library.appendingPathComponent("\(id).json")) else { return false }
            return try NSDictionary(dictionary: JSONSerialization.jsonObject(with: file.data) as? [String: Any] ?? [:])
                .isEqual(to: JSONSerialization.jsonObject(with: connection.configurationData()) as? [String: Any] ?? [:])
        } catch { return false }
    }

    private struct Snapshot: Equatable {
        let data: Data
        let mode: Int
    }
    private struct Write {
        let path: String
        let before: Snapshot?
        let data: Data
    }
    private struct Change: Codable {
        let path: String
        let originalMode: Int?
        let replacement: Data
    }
    private struct Recovery: Codable {
        let backup: String
        let changes: [Change]
        let createdDirectories: [String]
    }

    private func validateInputs(_ version: String, _ connection: ClaudeDesktopConnectionSettings) throws {
        guard ["2.42304.0", "3.6556.1", "3.6556.2"].contains(version) else { throw ClaudeDesktopConfigurationError.unsupportedVersion }
        _ = try connection.configurationData()
        guard !configurationName.isEmpty, configurationName.count <= 100 else { throw ClaudeDesktopConfigurationError.invalidLibrary }
        try checkPolicy()
        try checkPath(root, directory: true)
        try checkPath(library, directory: true)
        try checkPath(state, directory: true)
    }

    private func checkPolicy() throws {
        guard !username.isEmpty, !username.contains("/"), username != ".", username != ".." else {
            throw ClaudeDesktopConfigurationError.managedPolicy
        }
        for directory in [managedPreferences, managedPreferences.appendingPathComponent(username)] {
            try checkPath(directory, directory: true)
            let policy = directory.appendingPathComponent("com.anthropic.claudefordesktop.plist")
            if try snapshot(policy) != nil { throw ClaudeDesktopConfigurationError.managedPolicy }
        }
    }

    private func prepare(_ connection: ClaudeDesktopConnectionSettings, recovery: Recovery? = nil) throws -> [Write] {
        var meta = try metadata(recovery: recovery)
        var entries = meta["entries"] as? [[String: Any]] ?? []
        var owned = try owners(recovery: recovery)
        let id: String
        if let existing = owned[configurationName] {
            guard entries.contains(where: { $0["id"] as? String == existing && $0["name"] as? String == configurationName }) else {
                throw ClaudeDesktopConfigurationError.invalidLibrary
            }
            id = existing
        } else {
            guard !entries.contains(where: { $0["name"] as? String == configurationName }) else {
                throw ClaudeDesktopConfigurationError.invalidLibrary
            }
            id = UUID().uuidString.lowercased()
            guard try original("Claude-3p/configLibrary/\(id).json", recovery) == nil else { throw ClaudeDesktopConfigurationError.invalidLibrary }
            entries.append(["id": id, "name": configurationName])
            owned[configurationName] = id
        }
        meta["entries"] = entries
        meta["appliedId"] = id
        let configPath = "Claude-3p/claude_desktop_config.json"
        var config = try object(original(configPath, recovery)?.data)
        guard config["hybridPointer"] == nil || config["hybridPointer"] is NSNull else { throw ClaudeDesktopConfigurationError.invalidLibrary }
        config["deploymentMode"] = "3p"
        let values: [(String, Data)] = [
            ("Claude-3p/configLibrary/\(id).json", try connection.configurationData()),
            ("Claude-3p/configLibrary/_meta.json", try json(meta)),
            (configPath, try json(config)),
            ("Baseten Switch/Claude Desktop Setup/ownership.json", try json(owned)),
        ]
        return try values.map { Write(path: $0.0, before: try original($0.0, recovery), data: $0.1) }
    }

    private func metadata(recovery: Recovery? = nil) throws -> [String: Any] {
        try checkPath(library, directory: true)
        if try attributes(library) != nil {
            for child in try fm.contentsOfDirectory(at: library, includingPropertiesForKeys: nil) {
                try checkPath(child, directory: false)
            }
        }
        let saved = try original("Claude-3p/configLibrary/_meta.json", recovery)
        if saved == nil {
            let recoveringFiles = Set(recovery?.changes.filter { $0.originalMode == nil }.map { URL(fileURLWithPath: $0.path).lastPathComponent } ?? [])
            if fm.fileExists(atPath: library.path), !(try fm.contentsOfDirectory(atPath: library.path)).filter({ !recoveringFiles.contains($0) }).isEmpty {
                throw ClaudeDesktopConfigurationError.invalidLibrary
            }
            return ["entries": [[String: Any]]()]
        }
        let meta = try object(saved?.data)
        guard let entries = meta["entries"] as? [[String: Any]], meta["appliedId"] != nil,
              meta["hybridPointer"] == nil || meta["hybridPointer"] is NSNull else { throw ClaudeDesktopConfigurationError.invalidLibrary }
        var ids = Set<String>()
        var names = Set<String>()
        for entry in entries {
            guard let id = entry["id"] as? String, isLibraryID(id),
                  ids.insert(id.lowercased()).inserted, let name = entry["name"] as? String, !name.isEmpty,
                  names.insert(name).inserted,
                  entry["hybridPointer"] == nil || entry["hybridPointer"] is NSNull,
                  let data = try original("Claude-3p/configLibrary/\(id).json", recovery)?.data else {
                throw ClaudeDesktopConfigurationError.invalidLibrary
            }
            let config = try object(data)
            guard config["hybridPointer"] == nil || config["hybridPointer"] is NSNull else { throw ClaudeDesktopConfigurationError.invalidLibrary }
        }
        if let applied = meta["appliedId"], !(applied is NSNull) {
            guard let id = applied as? String, entries.contains(where: { $0["id"] as? String == id }) else {
                throw ClaudeDesktopConfigurationError.invalidLibrary
            }
        }
        return meta
    }

    private func owners(recovery: Recovery? = nil) throws -> [String: String] {
        let value = try object(original("Baseten Switch/Claude Desktop Setup/ownership.json", recovery)?.data)
        guard let owners = value as? [String: String], owners.values.allSatisfy(isLibraryID),
              Set(owners.values.map { $0.lowercased() }).count == owners.count else { throw ClaudeDesktopConfigurationError.invalidLibrary }
        return owners
    }

    private func isLibraryID(_ id: String) -> Bool {
        UUID(uuidString: id) != nil && id == id.lowercased()
    }

    private func target(_ path: String) throws -> URL {
        let fixed = ["Claude-3p/configLibrary/_meta.json", "Claude-3p/claude_desktop_config.json", "Baseten Switch/Claude Desktop Setup/ownership.json"]
        let prefix = "Claude-3p/configLibrary/"
        let validID = path.hasPrefix(prefix) && path.hasSuffix(".json") && UUID(uuidString: String(path.dropFirst(prefix.count).dropLast(5))) != nil
        guard fixed.contains(path) || validID else { throw ClaudeDesktopConfigurationError.recoveryRequired }
        return support.appendingPathComponent(path)
    }

    private func original(_ path: String, _ recovery: Recovery?) throws -> Snapshot? {
        if let recovery, let index = recovery.changes.firstIndex(where: { $0.path == path }) {
            guard let mode = recovery.changes[index].originalMode else { return nil }
            guard let saved = try snapshot(state.appendingPathComponent(recovery.backup).appendingPathComponent("\(index).data")) else {
                throw ClaudeDesktopConfigurationError.recoveryRequired
            }
            return Snapshot(data: saved.data, mode: mode)
        }
        return try snapshot(target(path))
    }

    private func recoveryRecord() throws -> Recovery? {
        guard let saved = try snapshot(pending) else { return nil }
        guard let record = try? JSONDecoder().decode(Recovery.self, from: saved.data),
              UUID(uuidString: record.backup) != nil, record.changes.count == 4,
              Set(record.changes.map(\.path)).count == 4,
              Set(record.changes.map(\.path)).isSuperset(of: ["Claude-3p/configLibrary/_meta.json", "Claude-3p/claude_desktop_config.json", "Baseten Switch/Claude Desktop Setup/ownership.json"]),
              Set(record.createdDirectories).count == record.createdDirectories.count,
              record.createdDirectories.allSatisfy({ ["Claude-3p", "Claude-3p/configLibrary"].contains($0) }),
              record.changes.allSatisfy({ $0.originalMode == nil || (0...0o777).contains($0.originalMode!) }) else {
            throw ClaudeDesktopConfigurationError.recoveryRequired
        }
        for change in record.changes { _ = try target(change.path) }
        return record
    }

    private func checkRecovery(_ record: Recovery) throws {
        for change in record.changes {
            let before = try original(change.path, record)
            let current = try snapshot(target(change.path))
            let after = Snapshot(data: change.replacement, mode: before?.mode ?? 0o600)
            guard current == before || current == after else { throw ClaudeDesktopConfigurationError.recoveryRequired }
        }
    }

    private func restore(_ record: Recovery, isClaudeRunning: () -> Bool) throws {
        try checkRecovery(record)
        for change in record.changes.reversed() {
            guard !isClaudeRunning() else { throw ClaudeDesktopConfigurationError.recoveryRequired }
            try checkRecovery(record)
            let destination = try target(change.path)
            if let before = try original(change.path, record) {
                try atomic(before.data, at: destination, mode: before.mode)
            } else if try snapshot(destination) != nil {
                try fm.removeItem(at: destination)
                try synchronizeDirectory(destination.deletingLastPathComponent())
            }
        }
        for path in record.createdDirectories.sorted(by: { $0.count > $1.count }) {
            let directory = support.appendingPathComponent(path)
            try checkPath(directory, directory: true)
            if try attributes(directory) != nil, try fm.contentsOfDirectory(atPath: directory.path).isEmpty {
                try fm.removeItem(at: directory)
                try synchronizeDirectory(directory.deletingLastPathComponent())
            }
        }
        try fm.removeItem(at: pending)
        try synchronizeDirectory(state)
    }

    private func object(at url: URL) throws -> [String: Any] { try object(snapshot(url)?.data) }
    private func object(_ data: Data?) throws -> [String: Any] {
        guard let data else { return [:] }
        guard let value = try? JSONSerialization.jsonObject(with: data), let object = value as? [String: Any] else {
            throw ClaudeDesktopConfigurationError.invalidLibrary
        }
        return object
    }
    private func json(_ value: Any) throws -> Data { try JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys]) }

    private func attributes(_ url: URL) throws -> [FileAttributeKey: Any]? {
        do { return try fm.attributesOfItem(atPath: url.path) }
        catch let error as NSError where error.domain == NSCocoaErrorDomain && error.code == NSFileReadNoSuchFileError { return nil }
    }

    private func checkPath(_ url: URL, directory: Bool) throws {
        var current = URL(fileURLWithPath: "/", isDirectory: true)
        let components = url.pathComponents.dropFirst()
        guard url.isFileURL, !components.contains("."), !components.contains("..") else {
            throw ClaudeDesktopConfigurationError.unsafeFiles
        }
        for (index, part) in components.enumerated() {
            current.appendPathComponent(part)
            guard let attrs = try attributes(current) else { continue }
            let expected: FileAttributeType = (index < components.count - 1 || directory) ? .typeDirectory : .typeRegular
            guard attrs[.type] as? FileAttributeType == expected else { throw ClaudeDesktopConfigurationError.unsafeFiles }
        }
    }

    private func snapshot(_ url: URL) throws -> Snapshot? {
        try checkPath(url, directory: false)
        guard let attrs = try attributes(url), let mode = attrs[.posixPermissions] as? NSNumber else { return nil }
        return Snapshot(data: try Data(contentsOf: url), mode: mode.intValue)
    }

    private func makeDirectory(_ url: URL) throws {
        try checkPath(url, directory: true)
        try fm.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    }

    private func atomic(_ data: Data, at url: URL, mode: Int) throws {
        try checkPath(url, directory: false)
        let temporary = url.deletingLastPathComponent().appendingPathComponent(".switch-\(UUID().uuidString).tmp")
        guard fm.createFile(atPath: temporary.path, contents: nil, attributes: [.posixPermissions: 0o600]) else {
            throw ClaudeDesktopConfigurationError.fileAccess
        }
        defer { try? fm.removeItem(at: temporary) }
        let handle = try FileHandle(forWritingTo: temporary)
        do {
            try handle.write(contentsOf: data)
            try handle.synchronize()
            try handle.close()
        } catch { try? handle.close(); throw error }
        try fm.setAttributes([.posixPermissions: mode], ofItemAtPath: temporary.path)
        try checkPath(url, directory: false)
        if try attributes(url) == nil {
            try fm.moveItem(at: temporary, to: url)
        } else {
            _ = try fm.replaceItemAt(url, withItemAt: temporary, options: .usingNewMetadataOnly)
        }
        try synchronizeDirectory(url.deletingLastPathComponent())
    }

    private func synchronizeDirectory(_ url: URL) throws {
        try checkPath(url, directory: true)
        let descriptor = open(url.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
        guard descriptor >= 0 else { throw ClaudeDesktopConfigurationError.fileAccess }
        defer { close(descriptor) }
        guard fsync(descriptor) == 0 else { throw ClaudeDesktopConfigurationError.fileAccess }
    }

    private func access<T>(_ work: () throws -> T) throws -> T {
        do { return try work() }
        catch let error as ClaudeDesktopConfigurationError { throw error }
        catch { throw ClaudeDesktopConfigurationError.fileAccess }
    }
}
