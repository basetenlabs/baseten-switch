import Foundation
import XCTest
@testable import BasetenSwitch

final class ClaudeDesktopConfigurationTests: XCTestCase {
    private let connection = ClaudeDesktopConnectionSettings(baseURL: "http://127.0.0.1:45274", apiKey: "baseten-switch-local")
    private let version = "3.6556.1"
    private var directory: URL!
    private var support: URL { directory.appendingPathComponent("Application Support") }
    private var policy: URL { directory.appendingPathComponent("Managed Preferences") }
    private var root: URL { support.appendingPathComponent("Claude-3p") }
    private var library: URL { root.appendingPathComponent("configLibrary") }
    private var state: URL { support.appendingPathComponent("Baseten Switch/Claude Desktop Setup") }
    private let defaultID = "11111111-1111-4111-8111-111111111111"
    private let fm = FileManager.default

    override func setUpWithError() throws {
        directory = URL(fileURLWithPath: "/private/tmp/\(UUID().uuidString)", isDirectory: true)
        try fm.createDirectory(at: directory, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        guard UUID(uuidString: directory.lastPathComponent) != nil,
              directory.deletingLastPathComponent().path == "/private/tmp" else {
            return XCTFail("Refusing cleanup outside the unique test directory")
        }
        try fm.removeItem(at: directory)
    }

    private func store(name: String = "Baseten Switch") -> ClaudeDesktopConfigurationStore {
        ClaudeDesktopConfigurationStore(applicationSupportURL: support, managedPreferencesURL: policy,
                                        username: "test-user", configurationName: name)
    }

    private func write(_ object: Any, to url: URL, mode: Int = 0o600) throws {
        try fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys]).write(to: url)
        try fm.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
    }

    private func read(_ url: URL) throws -> [String: Any] {
        try XCTUnwrap(JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any])
    }

    private func seed() throws {
        try write(["inferenceProvider": "firstParty", "extra": ["keep": true]], to: library.appendingPathComponent("\(defaultID).json"))
        try write(["appliedId": defaultID, "entries": [["id": defaultID, "name": "Default"]], "extra": "preserved"],
                  to: library.appendingPathComponent("_meta.json"), mode: 0o640)
        try write(["deploymentMode": "consumer", "preferences": ["theme": "dark"]],
                  to: root.appendingPathComponent("claude_desktop_config.json"), mode: 0o640)
    }

    func testGeneratedConfigurationUsesOnlyLocalPlaceholderAndDiscovery() throws {
        let data = try connection.configurationData()
        let config = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        XCTAssertEqual(config.count, 7)
        XCTAssertEqual(config["inferenceProvider"] as? String, "gateway")
        XCTAssertEqual(config["inferenceCredentialKind"] as? String, "static")
        XCTAssertEqual(config["inferenceGatewayBaseUrl"] as? String, "http://127.0.0.1:45274")
        XCTAssertEqual(config["inferenceGatewayApiKey"] as? String, "baseten-switch-local")
        XCTAssertEqual(config["inferenceGatewayAuthScheme"] as? String, "bearer")
        XCTAssertEqual(config["chatTabEnabled"] as? Bool, true)
        XCTAssertEqual(config["autoModeEnabled"] as? Bool, false)
        XCTAssertNil(config["models"])
        for url in ["https://127.0.0.1:45274", "http://example.com:45274", "http://127.0.0.1", "http://127.0.0.1:0",
                    "http://127.0.0.1:65536", "http://user@127.0.0.1:45274", "http://127.0.0.1:45274/", "http://127.0.0.1:45274?x=1", "http://127.0.0.1:45274#x"] {
            XCTAssertThrowsError(try ClaudeDesktopConnectionSettings(baseURL: url, apiKey: "baseten-switch-local").configurationData(), url)
        }
        XCTAssertThrowsError(try ClaudeDesktopConnectionSettings(baseURL: connection.baseURL, apiKey: "synthetic-provider-credential").configurationData())
    }

    func testSetupPreservesOriginalsAndRepeatedSetupUsesSameEntry() throws {
        try seed()
        let metaURL = library.appendingPathComponent("_meta.json")
        let rootURL = root.appendingPathComponent("claude_desktop_config.json")
        let originalMeta = try Data(contentsOf: metaURL)
        let originalRoot = try Data(contentsOf: rootURL)
        let originalDefault = try Data(contentsOf: library.appendingPathComponent("\(defaultID).json"))
        try store().validate(appVersion: version, connection: connection)
        XCTAssertFalse(fm.fileExists(atPath: state.path))
        let receipt = try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false })
        XCTAssertEqual(try Data(contentsOf: receipt.backupDirectoryURL.appendingPathComponent("1.data")), originalMeta)
        XCTAssertEqual(try Data(contentsOf: receipt.backupDirectoryURL.appendingPathComponent("2.data")), originalRoot)
        XCTAssertEqual(try fm.attributesOfItem(atPath: receipt.backupDirectoryURL.path)[.posixPermissions] as? Int, 0o700)
        XCTAssertEqual(try fm.attributesOfItem(atPath: receipt.backupDirectoryURL.appendingPathComponent("1.data").path)[.posixPermissions] as? Int, 0o600)
        XCTAssertEqual(try fm.attributesOfItem(atPath: metaURL.path)[.posixPermissions] as? Int, 0o640)
        let first = try read(metaURL)
        let appliedID = try XCTUnwrap(first["appliedId"] as? String)
        XCTAssertNotNil(appliedID.range(of: "^[a-f0-9-]{36}$", options: .regularExpression))
        let switchEntry = try XCTUnwrap((first["entries"] as? [[String: Any]])?.first(where: { $0["name"] as? String == "Baseten Switch" }))
        XCTAssertEqual(switchEntry["id"] as? String, appliedID)
        XCTAssertTrue(try fm.contentsOfDirectory(atPath: library.path).contains("\(appliedID).json"))
        XCTAssertEqual(first["extra"] as? String, "preserved")
        XCTAssertEqual((try read(rootURL)["preferences"] as? [String: String])?["theme"], "dark")
        XCTAssertEqual(try Data(contentsOf: library.appendingPathComponent("\(defaultID).json")), originalDefault)
        XCTAssertTrue(store().isConfigured(connection: connection))
        for appVersion in ["2.42304.0", "3.6556.2"] {
            _ = try store().apply(appVersion: appVersion, connection: connection, isClaudeRunning: { false })
            let second = try read(metaURL)
            XCTAssertEqual(first["appliedId"] as? String, second["appliedId"] as? String)
            XCTAssertEqual((second["entries"] as? [[String: Any]])?.count, 2)
            XCTAssertTrue(store().isConfigured(connection: connection))
        }
        let preview = ClaudeDesktopConnectionSettings(baseURL: "http://127.0.0.1:45374", apiKey: "baseten-switch-local")
        _ = try store(name: "Baseten Switch Preview").apply(appVersion: version, connection: preview, isClaudeRunning: { false })
        XCTAssertEqual((try read(metaURL)["entries"] as? [[String: Any]])?.count, 3)
        XCTAssertTrue(store(name: "Baseten Switch Preview").isConfigured(connection: preview))
        XCTAssertFalse(store().isConfigured(connection: connection))
    }

    func testFreshSetupAndManualEntryRecognition() throws {
        _ = try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false })
        XCTAssertTrue(store().isConfigured(connection: connection))
        try fm.removeItem(at: state.appendingPathComponent("ownership.json"))
        XCTAssertTrue(store().isConfigured(connection: connection))
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false }))
    }

    func testUppercaseLibraryAndOwnershipIDsAreRefusedBeforeWrites() throws {
        let uppercaseID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
        let metaURL = library.appendingPathComponent("_meta.json")
        let entryURL = library.appendingPathComponent("\(uppercaseID).json")
        try write(["inferenceProvider": "firstParty"], to: entryURL)
        try write(["appliedId": uppercaseID, "entries": [["id": uppercaseID, "name": "Default"]]], to: metaURL)
        let beforeMeta = try Data(contentsOf: metaURL)
        let beforeEntry = try Data(contentsOf: entryURL)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false }))
        XCTAssertEqual(try Data(contentsOf: metaURL), beforeMeta)
        XCTAssertEqual(try Data(contentsOf: entryURL), beforeEntry)
        XCTAssertFalse(fm.fileExists(atPath: state.path))

        try fm.removeItem(at: entryURL)
        try seed()
        let ownershipURL = state.appendingPathComponent("ownership.json")
        try write(["Baseten Switch": uppercaseID], to: ownershipURL)
        let beforeOwnership = try Data(contentsOf: ownershipURL)
        let validMeta = try Data(contentsOf: metaURL)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false }))
        XCTAssertEqual(try Data(contentsOf: ownershipURL), beforeOwnership)
        XCTAssertEqual(try Data(contentsOf: metaURL), validMeta)
        XCTAssertEqual(try fm.contentsOfDirectory(atPath: state.path), ["ownership.json"])
    }

    func testUnsupportedManagedMalformedAndRunningStatesDoNotWrite() throws {
        try seed()
        let original = try Data(contentsOf: library.appendingPathComponent("_meta.json"))
        let rootURL = root.appendingPathComponent("claude_desktop_config.json")
        let defaultURL = library.appendingPathComponent("\(defaultID).json")
        let originalRoot = try Data(contentsOf: rootURL)
        let originalDefault = try Data(contentsOf: defaultURL)
        for appVersion in ["3.6556.3", "3.6557.0", "9.0.0"] {
            XCTAssertThrowsError(try store().validate(appVersion: appVersion, connection: connection))
            XCTAssertThrowsError(try store().apply(appVersion: appVersion, connection: connection, isClaudeRunning: { false }))
            XCTAssertEqual(try Data(contentsOf: library.appendingPathComponent("_meta.json")), original)
            XCTAssertEqual(try Data(contentsOf: rootURL), originalRoot)
            XCTAssertEqual(try Data(contentsOf: defaultURL), originalDefault)
            XCTAssertFalse(fm.fileExists(atPath: state.path))
        }
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: { true }))
        let policyURL = policy.appendingPathComponent("test-user/com.anthropic.claudefordesktop.plist")
        try write(["inferenceProvider": "gateway"], to: policyURL)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        try fm.removeItem(at: policyURL)
        for metadata: [String: Any] in [
            ["entries": "invalid"],
            ["entries": [["id": "../escape", "name": "Default"]]],
            ["entries": [["id": defaultID, "name": "A"], ["id": defaultID, "name": "B"]]],
            ["entries": [["id": defaultID, "name": "Baseten Switch"]]],
            ["entries": [["id": defaultID, "name": "Default"]], "hybridPointer": "external"],
        ] {
            try write(metadata, to: library.appendingPathComponent("_meta.json"))
            XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        }
        try Data("{".utf8).write(to: library.appendingPathComponent("_meta.json"))
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        try original.write(to: library.appendingPathComponent("_meta.json"))
        XCTAssertFalse(fm.fileExists(atPath: state.path))
    }

    func testSymlinkedFilesAndAncestorsAreRejected() throws {
        try seed()
        let meta = library.appendingPathComponent("_meta.json")
        let foreign = directory.appendingPathComponent("foreign.json")
        try fm.moveItem(at: meta, to: foreign)
        try fm.createSymbolicLink(at: meta, withDestinationURL: foreign)
        let bytes = try Data(contentsOf: foreign)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        XCTAssertEqual(try Data(contentsOf: foreign), bytes)
        try fm.removeItem(at: meta)
        try fm.moveItem(at: foreign, to: meta)
        let moved = directory.appendingPathComponent("other-support")
        try fm.moveItem(at: support, to: moved)
        try fm.createSymbolicLink(at: support, withDestinationURL: moved)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
    }

    func testUnreadableAndNonregularManagedPolicyFailClosed() throws {
        let policyURL = policy.appendingPathComponent("com.anthropic.claudefordesktop.plist")
        try write(["managed": true], to: policyURL, mode: 0)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        try fm.removeItem(at: policyURL)
        try fm.createDirectory(at: policyURL, withIntermediateDirectories: true)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        XCTAssertFalse(fm.fileExists(atPath: root.path))
    }

    func testFailedFreshSetupRemovesItsEmptyClaudeDirectories() throws {
        var calls = 0
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: {
            calls += 1
            return calls == 4
        }))
        XCTAssertFalse(fm.fileExists(atPath: root.path))
        XCTAssertFalse(fm.fileExists(atPath: state.appendingPathComponent("pending.json").path))
    }

    func testPartialFailureRollsBackExactBytesAndPermissions() throws {
        try seed()
        let original = try Data(contentsOf: library.appendingPathComponent("_meta.json"))
        var calls = 0
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: {
            calls += 1
            return calls == 5
        }))
        XCTAssertEqual(try Data(contentsOf: library.appendingPathComponent("_meta.json")), original)
        XCTAssertEqual(try fm.attributesOfItem(atPath: library.appendingPathComponent("_meta.json").path)[.posixPermissions] as? Int, 0o640)
        XCTAssertEqual(Set(try fm.contentsOfDirectory(atPath: library.path)), Set(["_meta.json", "\(defaultID).json"]))
        XCTAssertFalse(fm.fileExists(atPath: state.appendingPathComponent("pending.json").path))
    }

    func testInterruptedSetupCanBeValidatedAndRecoveredBeforeRetry() throws {
        try seed()
        var calls = 0
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: {
            calls += 1
            return calls >= 5
        }))
        XCTAssertTrue(fm.fileExists(atPath: state.appendingPathComponent("pending.json").path))
        XCTAssertFalse(store().isConfigured(connection: connection))
        let beforeValidation = try Data(contentsOf: library.appendingPathComponent("_meta.json"))
        try store().validate(appVersion: version, connection: connection)
        XCTAssertEqual(try Data(contentsOf: library.appendingPathComponent("_meta.json")), beforeValidation)
        _ = try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false })
        XCTAssertTrue(store().isConfigured(connection: connection))
        XCTAssertEqual((try read(library.appendingPathComponent("_meta.json"))["entries"] as? [[String: Any]])?.count, 2)
    }

    func testRecoveryRefusesToOverwriteSubsequentEdits() throws {
        try seed()
        var calls = 0
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: {
            calls += 1
            return calls >= 5
        }))
        let meta = library.appendingPathComponent("_meta.json")
        let edited = Data("{\"editedElsewhere\":true}".utf8)
        try edited.write(to: meta)
        XCTAssertThrowsError(try store().validate(appVersion: version, connection: connection))
        XCTAssertThrowsError(try store().apply(appVersion: version, connection: connection, isClaudeRunning: { false }))
        XCTAssertEqual(try Data(contentsOf: meta), edited)
        XCTAssertTrue(fm.fileExists(atPath: state.appendingPathComponent("pending.json").path))
    }
}
