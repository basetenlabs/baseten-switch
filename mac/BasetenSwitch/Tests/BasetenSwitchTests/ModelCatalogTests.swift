import Foundation
import ServiceManagement
import XCTest
@testable import BasetenSwitch

private actor FixedModelCatalogReader: ModelCatalogReading {
    let result: Result<LiveModelCatalogSnapshot, Error>
    private(set) var calls = 0

    init(_ result: Result<LiveModelCatalogSnapshot, Error>) {
        self.result = result
    }

    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        calls += 1
        return try result.get()
    }
}

private actor SequencedModelCatalogReader: ModelCatalogReading {
    private let first: LiveModelCatalogSnapshot
    private let second: LiveModelCatalogSnapshot
    private var calls = 0

    init(first: LiveModelCatalogSnapshot,
         second: LiveModelCatalogSnapshot) {
        self.first = first
        self.second = second
    }

    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        calls += 1
        if calls == 1 {
            // Deliberately ignore cancellation. The generation guard must
            // still prevent this result from replacing the newer response.
            try? await Task.sleep(nanoseconds: 120_000_000)
            return first
        }
        return second
    }
}

private actor DelayedModelCatalogReader: ModelCatalogReading {
    let snapshot: LiveModelCatalogSnapshot

    init(snapshot: LiveModelCatalogSnapshot) {
        self.snapshot = snapshot
    }

    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        try? await Task.sleep(nanoseconds: 80_000_000)
        return snapshot
    }
}

private actor FixedModelCatalogAdminReader: AdminStatusReading {
    let snapshot: AdminStatusSnapshot

    init(snapshot: AdminStatusSnapshot) {
        self.snapshot = snapshot
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        snapshot
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }
}

private actor RecordingAuthWorkflow: AdminStatusReading, AuthReloading,
                                      ModelCatalogReading {
    private var statuses: [AdminStatusSnapshot]
    private var catalogs: [LiveModelCatalogSnapshot]
    private let reloadResult: Result<AuthStatus, Error>
    private let reloadDelayNanoseconds: UInt64
    private let suspendFirstReload: Bool
    private let suspendFirstStatus: Bool
    private let suspendFirstCatalog: Bool
    private var firstReloadStarted = false
    private var firstReloadContinuation: CheckedContinuation<Void, Never>?
    private var firstStatusStarted = false
    private var firstStatusContinuation: CheckedContinuation<Void, Never>?
    private var firstCatalogStarted = false
    private var firstCatalogContinuation: CheckedContinuation<Void, Never>?
    private(set) var events: [String] = []

    init(
        statuses: [AdminStatusSnapshot],
        catalogs: [LiveModelCatalogSnapshot],
        reloadResult: Result<AuthStatus, Error> = .success(
            AuthStatus(dict: ["signed_in": true, "health": "ok"])),
        reloadDelayNanoseconds: UInt64 = 0,
        suspendFirstReload: Bool = false,
        suspendFirstStatus: Bool = false,
        suspendFirstCatalog: Bool = false
    ) {
        self.statuses = statuses
        self.catalogs = catalogs
        self.reloadResult = reloadResult
        self.reloadDelayNanoseconds = reloadDelayNanoseconds
        self.suspendFirstReload = suspendFirstReload
        self.suspendFirstStatus = suspendFirstStatus
        self.suspendFirstCatalog = suspendFirstCatalog
    }

    func reloadAuth() async throws -> AuthStatus {
        events.append("reload")
        if suspendFirstReload, !firstReloadStarted {
            firstReloadStarted = true
            await withCheckedContinuation { continuation in
                firstReloadContinuation = continuation
            }
        }
        if reloadDelayNanoseconds > 0 {
            try await Task.sleep(nanoseconds: reloadDelayNanoseconds)
        }
        return try reloadResult.get()
    }

    func fetchStatus() async throws -> AdminStatusSnapshot {
        events.append("status")
        if suspendFirstStatus, !firstStatusStarted {
            firstStatusStarted = true
            await withCheckedContinuation { continuation in
                firstStatusContinuation = continuation
            }
        }
        guard !statuses.isEmpty else {
            throw GatewayClientError.invalidPayload
        }
        return statuses.removeFirst()
    }

    func fetchStats(windowSeconds: Int,
                    bucketSeconds: Int) async throws -> StatsSnapshot {
        StatsSnapshot(dict: [:])
    }

    func fetchModelCatalog() async throws -> LiveModelCatalogSnapshot {
        events.append("catalog")
        guard !catalogs.isEmpty else {
            throw GatewayClientError.invalidPayload
        }
        let snapshot = catalogs.removeFirst()
        if suspendFirstCatalog, !firstCatalogStarted {
            firstCatalogStarted = true
            await withCheckedContinuation { continuation in
                firstCatalogContinuation = continuation
            }
        }
        return snapshot
    }

    func waitForFirstReloadStart() async {
        while !firstReloadStarted {
            await Task.yield()
        }
    }

    func waitForFirstStatusStart() async {
        while !firstStatusStarted {
            await Task.yield()
        }
    }

    func waitForFirstCatalogStart() async {
        while !firstCatalogStarted {
            await Task.yield()
        }
    }

    func waitForEvent(_ event: String) async {
        while !events.contains(event) {
            await Task.yield()
        }
    }

    func releaseFirstStatus() {
        firstStatusContinuation?.resume()
        firstStatusContinuation = nil
    }

    func releaseFirstReload() {
        firstReloadContinuation?.resume()
        firstReloadContinuation = nil
    }

    func releaseFirstCatalog() {
        firstCatalogContinuation?.resume()
        firstCatalogContinuation = nil
    }
}

private struct ModelCatalogTransportError: Error {}

@MainActor
private final class ModelCatalogLoginItemService: LoginItemServicing {
    var status: SMAppService.Status = .notRegistered
    func reconcileAtLaunch() {}
    func toggle() {}
    func openSystemSettings() {}
}

private final class ModelCatalogURLProtocol: URLProtocol {
    static var responseData = Data()
    static var statusCode = 200
    static var responseDelay: TimeInterval = 0
    static var observedURL: URL?
    static var observedMethod: String?
    static var observedBody: Data?
    static var observedAdminHeader: String?
    static var observedTimeoutInterval: TimeInterval?

    private var responseWorkItem: DispatchWorkItem?

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest)
        -> URLRequest {
        request
    }

    override func startLoading() {
        Self.observedURL = request.url
        Self.observedMethod = request.httpMethod
        Self.observedBody = request.httpBody
        if Self.observedBody == nil, let stream = request.httpBodyStream {
            stream.open()
            defer { stream.close() }
            var data = Data()
            var buffer = [UInt8](repeating: 0, count: 1024)
            while stream.hasBytesAvailable {
                let count = stream.read(&buffer, maxLength: buffer.count)
                guard count > 0 else { break }
                data.append(contentsOf: buffer.prefix(count))
            }
            Self.observedBody = data
        }
        Self.observedAdminHeader = request.value(
            forHTTPHeaderField: "X-Baseten-Switch-Admin")
        Self.observedTimeoutInterval = request.timeoutInterval
        let workItem = DispatchWorkItem { [weak self] in
            guard let self else { return }
            let response = HTTPURLResponse(
                url: request.url!,
                statusCode: Self.statusCode,
                httpVersion: nil,
                headerFields: ["Content-Type": "application/json"])!
            client?.urlProtocol(
                self,
                didReceive: response,
                cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: Self.responseData)
            client?.urlProtocolDidFinishLoading(self)
        }
        responseWorkItem = workItem
        if Self.responseDelay > 0 {
            DispatchQueue.global().asyncAfter(
                deadline: .now() + Self.responseDelay,
                execute: workItem)
        } else {
            workItem.perform()
        }
    }

    override func stopLoading() {
        responseWorkItem?.cancel()
        responseWorkItem = nil
    }
}

private struct LostAPIKeyResponse: SavedAPIKeyManaging {
    func updateSavedAPIKey(_ key: String?) async throws -> AuthStatus {
        throw URLError(.networkConnectionLost)
    }
}

final class ModelCatalogTests: XCTestCase {
    @MainActor
    func testAmbiguousSavedKeyMutationDiscardsPreviousCatalog() async {
        var status = adminSnapshot(signedIn: true, health: "ok")
        status.capabilities.append("saved_api_key")
        let old = catalogSnapshot(state: .ready, models: [liveModel("vendor/old")])
        let new = catalogSnapshot(state: .ready, models: [liveModel("vendor/new")])
        let workflow = RecordingAuthWorkflow(statuses: [status, status], catalogs: [old, new])
        let state = BasetenSwitchState(
            variant: .resolve(infoDictionary: [:], environment: [:]),
            reader: workflow, authReloader: workflow,
            savedAPIKeyManager: LostAPIKeyResponse(), modelCatalogReader: workflow,
            loginItemService: ModelCatalogLoginItemService(), startPolling: false)
        await state.refresh()
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(old.models))
        let error = await state.updateSavedAPIKey("synthetic-new-key")
        XCTAssertNotNil(error)
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(new.models))
    }

    func testSavedAPIKeyUsesProtectedMutationAndSecretFreeReceipt() async throws {
        ModelCatalogURLProtocol.responseData = Data("""
        {"signed_in":true,"auth_type":"api_key","source":"saved_api_key","saved_api_key":true,"health":"ok"}
        """.utf8)
        ModelCatalogURLProtocol.statusCode = 200
        ModelCatalogURLProtocol.responseDelay = 0
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(runtime: .stable(), session: URLSession(configuration: configuration))
        let status = try await client.updateSavedAPIKey("synthetic-switch-key")
        XCTAssertTrue(status.savedAPIKey)
        XCTAssertEqual(status.source, "saved_api_key")
        XCTAssertEqual(authLineLabel(auth: status), "Auth: saved Switch API key")
        XCTAssertEqual(ModelCatalogURLProtocol.observedURL?.path, "/v1/admin/auth/api-key")
        XCTAssertNil(ModelCatalogURLProtocol.observedURL?.query)
        XCTAssertEqual(ModelCatalogURLProtocol.observedMethod, "PUT")
        XCTAssertEqual(ModelCatalogURLProtocol.observedAdminHeader, "1")
        XCTAssertEqual(ModelCatalogURLProtocol.observedTimeoutInterval, 30)
        let body = try XCTUnwrap(ModelCatalogURLProtocol.observedBody)
        let payload = try JSONSerialization.jsonObject(with: body) as? [String: String]
        XCTAssertEqual(payload, ["api_key": "synthetic-switch-key"])

        ModelCatalogURLProtocol.responseData = Data("{\"saved_api_key\":false}".utf8)
        let removed = try await client.updateSavedAPIKey(nil)
        XCTAssertFalse(removed.savedAPIKey)
        XCTAssertEqual(ModelCatalogURLProtocol.observedMethod, "DELETE")
        XCTAssertEqual(ModelCatalogURLProtocol.observedAdminHeader, "1")
        XCTAssertEqual(ModelCatalogURLProtocol.observedTimeoutInterval, 30)
        XCTAssertEqual(ModelCatalogURLProtocol.observedBody?.count ?? 0, 0)
    }

    func testSavedAPIKeyMutationOutlivesOrdinaryResourceTimeout() async throws {
        ModelCatalogURLProtocol.responseData = Data("{\"saved_api_key\":true}".utf8)
        ModelCatalogURLProtocol.statusCode = 200
        ModelCatalogURLProtocol.responseDelay = 5.2
        defer { ModelCatalogURLProtocol.responseDelay = 0 }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = 2
        configuration.timeoutIntervalForResource = 5
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(), session: URLSession(configuration: configuration))

        let status = try await client.updateSavedAPIKey("synthetic-switch-key")

        XCTAssertTrue(status.savedAPIKey)
        XCTAssertEqual(ModelCatalogURLProtocol.observedTimeoutInterval, 30)
    }

    @MainActor
    func testSavedAPIKeyShowsSafeServerFailureAfterRefreshingState() async throws {
        let message = "could not save API key in macOS Keychain; unlock Keychain and allow access, then retry"
        ModelCatalogURLProtocol.responseData = try JSONSerialization.data(
            withJSONObject: ["error": message])
        ModelCatalogURLProtocol.statusCode = 500
        ModelCatalogURLProtocol.responseDelay = 0
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(), session: URLSession(configuration: configuration))
        var status = adminSnapshot(signedIn: true, health: "ok")
        status.capabilities.append("saved_api_key")
        let catalog = catalogSnapshot(state: .ready, models: [liveModel("vendor/new")])
        let workflow = RecordingAuthWorkflow(statuses: [status, status], catalogs: [catalog])
        let state = BasetenSwitchState(
            variant: .resolve(infoDictionary: [:], environment: [:]),
            reader: workflow, authReloader: workflow,
            savedAPIKeyManager: client, modelCatalogReader: workflow,
            loginItemService: ModelCatalogLoginItemService(), startPolling: false)
        await state.refresh()

        let error = await state.updateSavedAPIKey("synthetic-switch-key")
        await state.waitForModelCatalogRefresh()

        XCTAssertEqual(error, message)
        XCTAssertEqual(state.liveModelCatalogState, .ready(catalog.models))
        XCTAssertFalse(state.savingAPIKey)
    }

    func testSavedAPIKeyDoesNotExposeInvalidServerErrors() async throws {
        ModelCatalogURLProtocol.statusCode = 500
        ModelCatalogURLProtocol.responseDelay = 0
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(), session: URLSession(configuration: configuration))
        for message in ["synthetic-switch-key", "unsafe\nmessage", String(repeating: "x", count: 513)] {
            ModelCatalogURLProtocol.responseData = try JSONSerialization.data(
                withJSONObject: ["error": message])
            do {
                _ = try await client.updateSavedAPIKey("synthetic-switch-key")
                XCTFail("Expected the mutation to fail")
            } catch {
                XCTAssertEqual(error as? GatewayClientError, .badResponse(500))
            }
        }
    }

    func testModelCatalogRequestOutlivesOrdinaryAdminTimeout() async throws {
        ModelCatalogURLProtocol.responseData = Data("""
        {
          "state": "ready",
          "models": [],
          "signed_out_reason": "",
          "fetched_at": "2026-08-17T05:00:00Z",
          "error": ""
        }
        """.utf8)
        ModelCatalogURLProtocol.statusCode = 200
        ModelCatalogURLProtocol.responseDelay = 2.2
        ModelCatalogURLProtocol.observedTimeoutInterval = nil
        defer { ModelCatalogURLProtocol.responseDelay = 0 }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = 2
        configuration.timeoutIntervalForResource = 5
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(),
            session: URLSession(configuration: configuration))

        let snapshot = try await client.fetchModelCatalog()

        XCTAssertEqual(snapshot.state, .ready)
        XCTAssertEqual(
            ModelCatalogURLProtocol.observedTimeoutInterval ?? -1,
            4,
            accuracy: 0.001)
    }

    func testOrdinaryAdminReadRetainsTwoSecondTimeout() async throws {
        ModelCatalogURLProtocol.responseData = Data("""
        {
          "router_boot_id": "boot-a",
          "active_generation": 1,
          "active_config_hash": "sha256:active",
          "desired_config_hash": "sha256:active",
          "capabilities": [],
          "global_routing_enabled": false,
          "clients": []
        }
        """.utf8)
        ModelCatalogURLProtocol.statusCode = 200
        ModelCatalogURLProtocol.responseDelay = 0
        ModelCatalogURLProtocol.observedTimeoutInterval = nil
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = 2
        configuration.timeoutIntervalForResource = 5
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(),
            session: URLSession(configuration: configuration))

        _ = try await client.fetchStatus()

        XCTAssertEqual(
            ModelCatalogURLProtocol.observedTimeoutInterval ?? -1,
            2,
            accuracy: 0.001)
    }

    func testGatewayClientReloadsAuthWithEmptyPOST() async throws {
        ModelCatalogURLProtocol.responseData = Data("""
        {
          "signed_in": true,
          "auth_type": "api_key",
          "profile": "",
          "fallback_enabled": false,
          "fallback_in_use": false,
          "health": "ok",
          "last_refresh_error": ""
        }
        """.utf8)
        ModelCatalogURLProtocol.statusCode = 200
        ModelCatalogURLProtocol.observedURL = nil
        ModelCatalogURLProtocol.observedMethod = nil
        ModelCatalogURLProtocol.observedBody = nil
        ModelCatalogURLProtocol.observedAdminHeader = nil
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(),
            session: URLSession(configuration: configuration))

        let auth = try await client.reloadAuth()

        XCTAssertTrue(auth.signedIn)
        XCTAssertEqual(auth.authType, "api_key")
        XCTAssertEqual(auth.health, "ok")
        XCTAssertEqual(
            ModelCatalogURLProtocol.observedURL?.path,
            "/v1/admin/auth/reload")
        XCTAssertEqual(ModelCatalogURLProtocol.observedMethod, "POST")
        XCTAssertEqual(ModelCatalogURLProtocol.observedAdminHeader, "1")
        XCTAssertEqual(ModelCatalogURLProtocol.observedBody?.count ?? 0, 0)
    }

    func testGatewayClientDecodesNarrowModelCatalogContract() async throws {
        ModelCatalogURLProtocol.responseData = Data("""
        {
          "state": "ready",
          "models": [
            {
              "slug": "zai-org/GLM-5.2",
              "display_name": "GLM 5.2"
            }
          ],
          "signed_out_reason": "",
          "fetched_at": "2026-07-24T18:00:00Z",
          "error": ""
        }
        """.utf8)
        ModelCatalogURLProtocol.statusCode = 200
        ModelCatalogURLProtocol.observedURL = nil
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(),
            session: URLSession(configuration: configuration))

        let snapshot = try await client.fetchModelCatalog()

        XCTAssertEqual(snapshot.state, .ready)
        XCTAssertNil(snapshot.signedOutReason)
        XCTAssertEqual(snapshot.models.count, 1)
        XCTAssertEqual(snapshot.models[0].slug, "zai-org/GLM-5.2")
        XCTAssertEqual(snapshot.models[0].displayName, "GLM 5.2")
        XCTAssertEqual(
            ModelCatalogURLProtocol.observedURL?.path,
            "/v1/admin/model-catalog")
    }

    func testModelCatalogEntryRejectsMissingOrInvalidRequiredFields() {
        let valid: [String: Any] = [
            "slug": "vendor/model",
            "display_name": "Model",
        ]
        XCTAssertNotNil(LiveModelCatalogEntry(dict: valid))

        for invalid in [
            [
                "display_name": "Model",
            ] as [String: Any],
            [
                "slug": "   ",
                "display_name": "Model",
            ],
            [
                "slug": "vendor/model",
            ],
            [
                "slug": "vendor/model",
                "display_name": 42,
            ],
        ] {
            XCTAssertNil(LiveModelCatalogEntry(dict: invalid))
        }
    }

    func testLiveModelCatalogUsesRawSlugAsFinalDisplayFallback() {
        let model = LiveModelCatalogEntry(dict: [
            "slug": "vendor/model-with-hyphens",
            "display_name": "",
        ])

        XCTAssertEqual(model?.displayLabel, "vendor/model-with-hyphens")
    }

    func testModelCatalogSnapshotRejectsMalformedEnvelopeOrAnyRow() {
        let validModel: [String: Any] = [
            "slug": "vendor/model",
            "display_name": "Model",
        ]
        let valid: [String: Any] = [
            "state": "ready",
            "signed_out_reason": "",
            "models": [validModel],
            "fetched_at": "2026-07-24T18:00:00Z",
            "error": "",
        ]
        XCTAssertNotNil(LiveModelCatalogSnapshot(dict: valid))

        var invalidEnvelopes: [[String: Any]] = [
            [
                "models": [validModel],
                "fetched_at": "",
                "error": "",
            ],
            [
                "state": "unknown",
                "signed_out_reason": "",
                "models": [validModel],
                "fetched_at": "",
                "error": "",
            ],
            [
                "state": "ready",
                "signed_out_reason": "",
                "models": "not-an-array",
                "fetched_at": "",
                "error": "",
            ],
            [
                "state": "ready",
                "signed_out_reason": "",
                "models": [validModel],
                "error": "",
            ],
            [
                "state": "ready",
                "signed_out_reason": "",
                "models": [validModel],
                "fetched_at": "",
            ],
            [
                "state": "ready",
                "models": [validModel],
                "fetched_at": "",
                "error": "",
            ],
            [
                "state": "ready",
                "signed_out_reason": "session_expired",
                "models": [validModel],
                "fetched_at": "",
                "error": "",
            ],
            [
                "state": "signed_out",
                "signed_out_reason": "unknown",
                "models": [],
                "fetched_at": "",
                "error": "",
            ],
        ]
        var malformedRow = valid
        malformedRow["models"] = [
            validModel,
            [
                "slug": "vendor/bad",
                "display_name": 1,
            ],
        ]
        invalidEnvelopes.append(malformedRow)

        for invalid in invalidEnvelopes {
            XCTAssertNil(LiveModelCatalogSnapshot(dict: invalid))
        }
    }

    func testGatewayClientRejectsMalformedCatalogRow() async {
        ModelCatalogURLProtocol.responseData = Data("""
        {
          "state": "ready",
          "models": [
            {
              "slug": "vendor/model"
            }
          ],
          "signed_out_reason": "",
          "fetched_at": "2026-07-24T18:00:00Z",
          "error": ""
        }
        """.utf8)
        ModelCatalogURLProtocol.statusCode = 200
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [ModelCatalogURLProtocol.self]
        let client = GatewayAPIClient(
            runtime: .stable(),
            session: URLSession(configuration: configuration))

        do {
            _ = try await client.fetchModelCatalog()
            XCTFail("Expected the malformed row to reject the payload")
        } catch {
            XCTAssertEqual(
                error as? GatewayClientError,
                GatewayClientError.invalidPayload)
        }
    }

    func testProjectionUsesOneLiveBackedListAndPreservesAliasMatching() {
        let configured = [
            configuredModel(
                label: "Configured A",
                target: "claude-baseten-a",
                slug: "vendor/a",
                available: false),
            configuredModel(
                label: "Configured B",
                target: "claude-baseten-b",
                slug: "vendor/b",
                available: true),
            configuredModel(
                label: "Private C",
                target: "private-c",
                slug: "private/c",
                available: false),
        ]
        let live = [
            liveModel("vendor/a", label: "Live A"),
            liveModel("vendor/b", label: "Live B"),
            liveModel("vendor/d", label: "Live D"),
            liveModel("vendor/e", label: "Live E"),
            liveModel("vendor/d", label: "Duplicate D"),
        ]

        let projection = projectModelCatalog(
            configured: configured,
            liveState: .ready(live))

        XCTAssertEqual(
            projection.selectable.map(\.target),
            [
                "vendor/a",
                "vendor/b",
                "vendor/d",
                "vendor/e",
            ])
        XCTAssertEqual(projection.selectable[0].label, "Live A")
        XCTAssertEqual(projection.selectable[0].slug, "vendor/a")
        XCTAssertEqual(projection.selectable[0].alias, "claude-baseten-a")
        XCTAssertFalse(
            projection.selectable.contains(where: {
                $0.slug == "private/c"
            }))
    }

    func testNonReadyStateDoesNotPresentConfiguredModelsAsAvailable() {
        let configured = [
            configuredModel(
                label: "Configured",
                target: "configured",
                slug: "vendor/configured",
                available: true),
        ]

        for state in [
            LiveModelCatalogLoadState.idle,
            .loading,
            .signedOut(.notSignedIn),
            .signedOut(.sessionExpired),
            .signedOut(.credentialRejected),
            .error("unavailable"),
        ] {
            let projection = projectModelCatalog(
                configured: configured,
                liveState: state)
            XCTAssertTrue(projection.selectable.isEmpty)
        }
    }

    func testLiveModelAPIDispatchesItsRawSlug() {
        let projection = projectModelCatalog(
            configured: [],
            liveState: .ready([
                liveModel("zai-org/GLM-5.2", label: "GLM 5.2"),
            ]))
        let model = projection.selectable[0]

        XCTAssertEqual(model.target, "zai-org/GLM-5.2")
        XCTAssertEqual(
            familyDispatchArgs(
                client: "claude-code",
                family: "opus",
                choice: .catalog(model)),
            ["claude", "route", "opus", "zai-org/GLM-5.2"])
        XCTAssertEqual(
            subagentDispatchArgs(
                client: "claude-code",
                choice: .catalog(model)),
            ["claude", "subagents", "zai-org/GLM-5.2"])
    }

    @MainActor
    func testCatalogRefreshAppliesReadySignedOutAndErrorStates() async {
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/ready")])
        let reader = FixedModelCatalogReader(.success(ready))
        let state = makeState(modelCatalogReader: reader)

        state.ensureModelCatalogLoaded()
        XCTAssertEqual(state.liveModelCatalogState, .loading)
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
        state.ensureModelCatalogLoaded()
        await state.waitForModelCatalogRefresh()
        let calls = await reader.calls
        XCTAssertEqual(calls, 1)

        let notSignedInState = makeState(modelCatalogReader:
            FixedModelCatalogReader(.success(catalogSnapshot(
                state: .signedOut,
                signedOutReason: .notSignedIn))))
        notSignedInState.requestModelCatalogRefresh()
        await notSignedInState.waitForModelCatalogRefresh()
        XCTAssertEqual(
            notSignedInState.liveModelCatalogState,
            .signedOut(.notSignedIn))

        let expiredState = makeState(modelCatalogReader:
            FixedModelCatalogReader(.success(catalogSnapshot(
                state: .signedOut,
                signedOutReason: .sessionExpired))))
        expiredState.requestModelCatalogRefresh()
        await expiredState.waitForModelCatalogRefresh()
        XCTAssertEqual(
            expiredState.liveModelCatalogState,
            .signedOut(.sessionExpired))
        XCTAssertEqual(
            liveModelCatalogSignedOutMessage(.notSignedIn),
            "Sign in to Baseten to load Model APIs.")
        XCTAssertEqual(
            liveModelCatalogSignedOutMessage(.sessionExpired),
            "Your Baseten session expired. Sign in again to load Model APIs.")
        XCTAssertEqual(
            liveModelCatalogSignedOutMessage(.credentialRejected),
            "Your Baseten API key was rejected. Update it and try again.")

        let backendErrorState = makeState(modelCatalogReader:
            FixedModelCatalogReader(.success(catalogSnapshot(
                state: .error,
                error: "Catalog is temporarily unavailable."))))
        backendErrorState.requestModelCatalogRefresh()
        await backendErrorState.waitForModelCatalogRefresh()
        XCTAssertEqual(
            backendErrorState.liveModelCatalogState,
            .error("Catalog is temporarily unavailable."))
    }

    @MainActor
    func testClientPageRefreshCoalescesBurstIntoOnePendingRerun() async {
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/ready")])
        let signedOut = catalogSnapshot(
            state: .signedOut,
            signedOutReason: .notSignedIn)
        let workflow = RecordingAuthWorkflow(
            statuses: [
                adminSnapshot(signedIn: false, health: "signed_out"),
                adminSnapshot(signedIn: true, health: "ok"),
            ],
            catalogs: [signedOut, ready],
            suspendFirstReload: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)

        state.requestClientPageRefresh()
        await workflow.waitForFirstReloadStart()
        for _ in 0..<25 {
            state.requestClientPageRefresh()
        }
        await workflow.releaseFirstReload()
        await state.waitForClientPageRefresh()

        let events = await workflow.events
        XCTAssertEqual(
            events,
            [
                "reload", "status", "catalog",
                "reload", "status", "catalog",
            ])
        XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
    }

    @MainActor
    func testClientPageRefreshPreservesSavedPickerSelectionsWhileLoading() async {
        let old = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/old", label: "Old Model")])
        let newest = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/new", label: "New Model")])
        let workflow = RecordingAuthWorkflow(
            statuses: [adminSnapshot(signedIn: true, health: "ok")],
            catalogs: [old, newest],
            suspendFirstReload: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)
        let claude = routingClient(name: "claude-code")
        let codex = routingClient(name: "codex")

        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        state.requestClientPageRefresh()
        await workflow.waitForFirstReloadStart()

        XCTAssertEqual(state.liveModelCatalogState, .loading)
        XCTAssertFalse(state.modelCatalogAllowsMutation)
        let claudeCatalog = state.modelCatalogProjection(for: claude).selectable
        let codexCatalog = state.modelCatalogProjection(for: codex).selectable
        XCTAssertEqual(claudeCatalog.map(\.slug), ["vendor/old"])
        XCTAssertEqual(
            familyPickerSelection(
                claude.families[0],
                catalog: claudeCatalog),
            .catalog("vendor/old"))
        XCTAssertEqual(
            subagentPickerSelection(claude, catalog: claudeCatalog),
            .catalog("vendor/old"))
        XCTAssertEqual(
            codexRoutePickerSelection(codex, catalog: codexCatalog),
            .catalog("vendor/old"))

        await workflow.releaseFirstReload()
        await state.waitForClientPageRefresh()

        XCTAssertTrue(state.modelCatalogAllowsMutation)
        XCTAssertEqual(
            state.modelCatalogProjection(for: claude).selectable.map(\.slug),
            ["vendor/new"])
    }

    @MainActor
    func testClientPageRefreshClearsPreservedCatalogOnTerminalResult() async {
        let old = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/old")])
        let terminalResults = [
            catalogSnapshot(
                state: .signedOut,
                signedOutReason: .notSignedIn),
            catalogSnapshot(
                state: .error,
                error: "Catalog unavailable."),
        ]

        for terminal in terminalResults {
            let workflow = RecordingAuthWorkflow(
                statuses: [adminSnapshot(signedIn: true, health: "ok")],
                catalogs: [old, terminal])
            let state = makeState(
                adminReader: workflow,
                authReloader: workflow,
                modelCatalogReader: workflow)
            let claude = routingClient(name: "claude-code")

            state.requestModelCatalogRefresh()
            await state.waitForModelCatalogRefresh()
            state.requestClientPageRefresh()
            await state.waitForClientPageRefresh()

            XCTAssertFalse(state.modelCatalogAllowsMutation)
            XCTAssertTrue(
                state.modelCatalogProjection(for: claude).selectable.isEmpty)
        }
    }

    @MainActor
    func testWindowCloseClearsCatalogPreservedDuringRefresh() async {
        let old = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/old")])
        let workflow = RecordingAuthWorkflow(
            statuses: [adminSnapshot(signedIn: true, health: "ok")],
            catalogs: [old],
            suspendFirstReload: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)
        let claude = routingClient(name: "claude-code")

        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        state.requestClientPageRefresh()
        await workflow.waitForFirstReloadStart()
        XCTAssertFalse(
            state.modelCatalogProjection(for: claude).selectable.isEmpty)

        state.routerWindowDidClose()
        XCTAssertEqual(state.liveModelCatalogState, .idle)
        XCTAssertTrue(
            state.modelCatalogProjection(for: claude).selectable.isEmpty)
        await workflow.releaseFirstReload()
    }

    @MainActor
    func testClientPageRefreshBoundsReloadFailuresAndContinues() async {
        let failures: [Error] = [
            GatewayClientError.badResponse(405),
            GatewayClientError.invalidPayload,
            GatewayClientError.badResponse(500),
            ModelCatalogTransportError(),
        ]
        for failure in failures {
            let ready = catalogSnapshot(
                state: .ready,
                models: [liveModel("vendor/ready")])
            let workflow = RecordingAuthWorkflow(
                statuses: [adminSnapshot(signedIn: true, health: "ok")],
                catalogs: [ready],
                reloadResult: .failure(failure))
            let state = makeState(
                adminReader: workflow,
                authReloader: workflow,
                modelCatalogReader: workflow)

            state.requestClientPageRefresh()
            await state.waitForClientPageRefresh()

            let events = await workflow.events
            XCTAssertEqual(events, ["reload", "status", "catalog"])
            XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
        }
    }

    @MainActor
    func testClientPageRefreshWaitsOutPreReloadStatusRequest() async {
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/ready")])
        let workflow = RecordingAuthWorkflow(
            statuses: [
                adminSnapshot(signedIn: false, health: "signed_out"),
                adminSnapshot(signedIn: true, health: "ok"),
            ],
            catalogs: [ready],
            suspendFirstStatus: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)

        let preReloadPoll = Task { await state.refresh() }
        await workflow.waitForFirstStatusStart()
        state.requestClientPageRefresh()
        await workflow.waitForEvent("reload")
        await workflow.releaseFirstStatus()
        await preReloadPoll.value
        await state.waitForClientPageRefresh()

        let events = await workflow.events
        XCTAssertEqual(
            events,
            ["status", "reload", "status", "catalog"])
        XCTAssertTrue(state.auth?.signedIn == true)
        XCTAssertEqual(state.auth?.health, "ok")
        XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
    }

    @MainActor
    func testClientPageRefreshInvalidatesPreReloadCatalogResult() async {
        let signedOut = catalogSnapshot(
            state: .signedOut,
            signedOutReason: .notSignedIn)
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/ready")])
        let workflow = RecordingAuthWorkflow(
            statuses: [adminSnapshot(signedIn: true, health: "ok")],
            catalogs: [signedOut, ready],
            suspendFirstCatalog: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)

        state.requestModelCatalogRefresh()
        await workflow.waitForFirstCatalogStart()
        state.requestClientPageRefresh()
        await state.waitForClientPageRefresh()
        await workflow.releaseFirstCatalog()
        await Task.yield()

        let events = await workflow.events
        XCTAssertEqual(events, ["catalog", "reload", "status", "catalog"])
        XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
    }

    @MainActor
    func testClosingWindowCancelsOrderedRefreshBeforeLateReads() async {
        let workflow = RecordingAuthWorkflow(
            statuses: [adminSnapshot(signedIn: true, health: "ok")],
            catalogs: [catalogSnapshot(state: .ready)],
            suspendFirstReload: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)

        state.requestClientPageRefresh()
        await workflow.waitForFirstReloadStart()
        state.routerWindowDidClose()
        await workflow.releaseFirstReload()
        await Task.yield()

        let events = await workflow.events
        XCTAssertEqual(events, ["reload"])
        XCTAssertEqual(state.liveModelCatalogState, .idle)
    }

    @MainActor
    func testHealthyAuthTransitionRetriesNotSignedInCatalogOnce() async {
        let signedOut = catalogSnapshot(
            state: .signedOut,
            signedOutReason: .notSignedIn)
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/recovered")])
        let workflow = RecordingAuthWorkflow(
            statuses: [
                adminSnapshot(signedIn: false, health: "signed_out"),
                adminSnapshot(signedIn: true, health: "ok"),
                adminSnapshot(signedIn: true, health: "ok"),
            ],
            catalogs: [signedOut, ready])
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)

        await state.refresh()
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        await state.refresh()
        await state.waitForModelCatalogRefresh()
        await state.refresh()
        await state.waitForModelCatalogRefresh()

        let events = await workflow.events
        XCTAssertEqual(
            events,
            ["status", "catalog", "status", "catalog", "status"])
        XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
    }

    @MainActor
    func testSavedKeyCatalogRefreshesAfterRouterRestartWithSameAuthRevision() async {
        func status(bootID: String) -> AdminStatusSnapshot {
            var snapshot = adminSnapshot(signedIn: true, health: "ok")
            snapshot.token = RoutingToken(routerBootID: bootID, activeGeneration: 1)
            snapshot.auth = AuthStatus(dict: [
                "signed_in": true, "health": "ok", "saved_api_key": true,
                "source": "saved_api_key", "revision": 1,
            ])
            return snapshot
        }
        let old = catalogSnapshot(state: .ready, models: [liveModel("vendor/old")])
        let new = catalogSnapshot(state: .ready, models: [liveModel("vendor/new")])
        let workflow = RecordingAuthWorkflow(
            statuses: [status(bootID: "boot-a"), status(bootID: "boot-b"), status(bootID: "boot-b")],
            catalogs: [old, new])
        let state = makeState(adminReader: workflow, authReloader: workflow, modelCatalogReader: workflow)

        await state.refresh()
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(old.models))

        await state.refresh()
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(new.models))

        await state.refresh()
        let events = await workflow.events
        XCTAssertEqual(events, ["status", "catalog", "status", "catalog", "status"])
    }

    @MainActor
    func testSavedKeyReplacementRefreshesHealthyAccountCatalog() async {
        func status(_ revision: Int) -> AdminStatusSnapshot {
            var snapshot = adminSnapshot(signedIn: true, health: "ok")
            snapshot.auth = AuthStatus(dict: [
                "signed_in": true, "health": "ok", "saved_api_key": true,
                "source": "saved_api_key", "revision": revision,
            ])
            return snapshot
        }
        let old = catalogSnapshot(state: .ready, models: [liveModel("vendor/old")])
        let new = catalogSnapshot(state: .ready, models: [liveModel("vendor/new")])
        let workflow = RecordingAuthWorkflow(
            statuses: [status(1), status(2), status(2)], catalogs: [old, new])
        let state = makeState(adminReader: workflow, authReloader: workflow, modelCatalogReader: workflow)
        await state.refresh()
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(old.models))
        await state.refresh()
        await state.waitForModelCatalogRefresh()
        XCTAssertEqual(state.liveModelCatalogState, .ready(new.models))
        await state.refresh()
        let events = await workflow.events
        XCTAssertEqual(events, ["status", "catalog", "status", "catalog", "status"])
    }

    @MainActor
    func testHealthyTransitionRetriesLateNotSignedInCatalogOnce() async {
        let signedOut = catalogSnapshot(
            state: .signedOut,
            signedOutReason: .notSignedIn)
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/recovered")])
        let workflow = RecordingAuthWorkflow(
            statuses: [
                adminSnapshot(signedIn: false, health: "signed_out"),
                adminSnapshot(signedIn: true, health: "ok"),
            ],
            catalogs: [signedOut, ready],
            suspendFirstCatalog: true)
        let state = makeState(
            adminReader: workflow,
            authReloader: workflow,
            modelCatalogReader: workflow)

        await state.refresh()
        state.requestModelCatalogRefresh()
        await workflow.waitForFirstCatalogStart()
        await state.refresh()
        await workflow.releaseFirstCatalog()
        await state.waitForModelCatalogRefresh()

        let events = await workflow.events
        XCTAssertEqual(
            events,
            ["status", "catalog", "status", "catalog"])
        XCTAssertEqual(state.liveModelCatalogState, .ready(ready.models))
    }

    @MainActor
    func testHealthyAuthTransitionDoesNotRetryTerminalSignedOutCatalogs() async {
        for reason in [
            LiveModelCatalogSignedOutReason.sessionExpired,
            .credentialRejected,
        ] {
            let terminal = catalogSnapshot(
                state: .signedOut,
                signedOutReason: reason)
            let workflow = RecordingAuthWorkflow(
                statuses: [
                    adminSnapshot(signedIn: false, health: "signed_out"),
                    adminSnapshot(signedIn: true, health: "ok"),
                ],
                catalogs: [terminal])
            let state = makeState(
                adminReader: workflow,
                authReloader: workflow,
                modelCatalogReader: workflow)

            await state.refresh()
            state.requestModelCatalogRefresh()
            await state.waitForModelCatalogRefresh()
            await state.refresh()
            await state.waitForModelCatalogRefresh()

            let events = await workflow.events
            XCTAssertEqual(events, ["status", "catalog", "status"])
            XCTAssertEqual(state.liveModelCatalogState, .signedOut(reason))
        }
    }

    @MainActor
    func testNewerCatalogRefreshWinsWhenCancelledReaderReturnsLate() async {
        let old = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/old")])
        let newest = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/new")])
        let state = makeState(modelCatalogReader:
            SequencedModelCatalogReader(first: old, second: newest))

        state.requestModelCatalogRefresh()
        try? await Task.sleep(nanoseconds: 10_000_000)
        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()
        try? await Task.sleep(nanoseconds: 140_000_000)

        XCTAssertEqual(state.liveModelCatalogState, .ready(newest.models))
    }

    @MainActor
    func testWindowCloseInvalidatesLateCatalogResult() async {
        let ready = catalogSnapshot(
            state: .ready,
            models: [liveModel("vendor/late")])
        let state = makeState(modelCatalogReader:
            DelayedModelCatalogReader(snapshot: ready))

        state.ensureModelCatalogLoaded()
        state.routerWindowDidClose()
        try? await Task.sleep(nanoseconds: 100_000_000)

        XCTAssertEqual(state.liveModelCatalogState, .idle)
    }

    @MainActor
    func testCatalogFailureDoesNotStaleConfirmedRoutingSnapshot() async {
        let catalogReader = FixedModelCatalogReader(
            .failure(GatewayClientError.badResponse(503)))
        let state = makeState(
            adminReader: FixedModelCatalogAdminReader(
                snapshot: adminSnapshot()),
            modelCatalogReader: catalogReader)
        await state.refresh()
        XCTAssertNotNil(state.routingSnapshot)
        XCTAssertFalse(state.snapshotIsStale)

        state.requestModelCatalogRefresh()
        await state.waitForModelCatalogRefresh()

        XCTAssertNotNil(state.routingSnapshot)
        XCTAssertFalse(state.snapshotIsStale)
        XCTAssertEqual(
            state.liveModelCatalogState,
            .error("Live model availability could not be loaded."))
        XCTAssertNil(state.lastError)
    }

    private func configuredModel(
        label: String,
        target: String,
        slug: String,
        available: Bool
    ) -> ModelCatalogEntry {
        ModelCatalogEntry(dict: [
            "label": label,
            "storage_target": target,
            "slug": slug,
            "alias": target,
            "available": available,
        ])!
    }

    private func liveModel(
        _ slug: String,
        label: String = ""
    ) -> LiveModelCatalogEntry {
        LiveModelCatalogEntry(dict: [
            "slug": slug,
            "display_name": label,
        ])!
    }

    private func catalogSnapshot(
        state: LiveModelCatalogResponseState,
        models: [LiveModelCatalogEntry] = [],
        signedOutReason: LiveModelCatalogSignedOutReason? = nil,
        error: String = ""
    ) -> LiveModelCatalogSnapshot {
        LiveModelCatalogSnapshot(dict: [
            "state": state.rawValue,
            "signed_out_reason": signedOutReason?.rawValue ?? "",
            "models": models.map {
                [
                    "slug": $0.slug,
                    "display_name": $0.displayName,
                ] as [String: Any]
            },
            "fetched_at": "2026-07-24T18:00:00Z",
            "error": error,
        ])!
    }

    private func adminSnapshot(
        signedIn: Bool? = nil,
        health: String = ""
    ) -> AdminStatusSnapshot {
        var dict: [String: Any] = [
            "router_boot_id": "boot-a",
            "active_generation": 1,
            "active_config_hash": "sha256:active",
            "desired_config_hash": "sha256:active",
            "capabilities": ["global_routing"],
            "global_routing_enabled": true,
            "clients": [],
        ]
        if let signedIn {
            dict["auth"] = [
                "signed_in": signedIn,
                "health": health,
            ]
        }
        return AdminStatusSnapshot(dict: dict)
    }

    private func routingClient(name: String) -> ClientStatus {
        ClientStatus(dict: [
            "name": name,
            "enabled": true,
            "subagent_model": "old-alias",
            "subagent_routing": "baseten",
            "unmatched_native_model": [
                "configured_target": "old-alias",
                "effective_route": "baseten",
                "effective_model": "vendor/old",
            ],
            "families": [[
                "family": "fable",
                "configured_target": "old-alias",
                "configured_source": "explicit",
                "effective_route": "baseten",
                "effective_model": "vendor/old",
            ]],
            "model_catalog": [[
                "label": "Configured Old",
                "storage_target": "old-alias",
                "slug": "vendor/old",
                "alias": "old-alias",
                "available": true,
            ]],
        ])!
    }

    @MainActor
    private func makeState(
        adminReader: (any AdminStatusReading)? = nil,
        authReloader: (any AuthReloading)? = nil,
        modelCatalogReader: any ModelCatalogReading
    ) -> BasetenSwitchState {
        BasetenSwitchState(
            variant: .resolve(
                infoDictionary: [:],
                environment: [:]),
            reader: adminReader ?? FixedModelCatalogAdminReader(
                snapshot: adminSnapshot()),
            authReloader: authReloader,
            modelCatalogReader: modelCatalogReader,
            loginItemService: ModelCatalogLoginItemService(),
            startPolling: false)
    }
}
