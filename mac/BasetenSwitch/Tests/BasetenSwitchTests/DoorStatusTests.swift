import Foundation
import XCTest
@testable import BasetenSwitch

private enum DoorTestError: Error {
    case unavailable
}

private final class DoorURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest)
        -> URLRequest {
        request
    }

    override func startLoading() {
        do {
            guard let handler = Self.handler else {
                throw DoorTestError.unavailable
            }
            let (response, data) = try handler(request)
            client?.urlProtocol(
                self,
                didReceive: response,
                cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

private actor DoorMappedReader: DoorStatusReading {
    private let statuses: [URL: DoorStatusPayload]
    private(set) var requestedURLs: [URL] = []

    init(statuses: [URL: DoorStatusPayload]) {
        self.statuses = statuses
    }

    func fetchDoorStatus(at url: URL) async throws -> DoorStatusPayload {
        requestedURLs.append(url)
        guard let status = statuses[url] else { throw DoorTestError.unavailable }
        return status
    }
}

final class DoorStatusTests: XCTestCase {
    override func tearDown() {
        DoorURLProtocol.handler = nil
        super.tearDown()
    }

    func testRuntimeProfilesUseChannelIsolatedDoorEndpoints() {
        let stable = RuntimeProfile.stable(environment: [:])
        XCTAssertEqual(
            stable.doorURLs,
            [URL(string: "http://127.0.0.1:45271/doorz")!])

        let customStable = RuntimeProfile.stable(environment: [
            "BASETEN_SWITCH_DOOR_PORTS": "8087,127.0.0.1:8088",
        ])
        XCTAssertEqual(customStable.doorURLs, [
            URL(string: "http://127.0.0.1:8087/doorz")!,
            URL(string: "http://127.0.0.1:8088/doorz")!,
        ])

        let preview = RuntimeProfile.preview(
            homeDirectory: "/tmp/baseten-switch-home",
            environment: [
                "BASETEN_SWITCH_DOOR_URLS": "http://127.0.0.1:8081/doorz",
                "BASETEN_SWITCH_DOOR_PORTS": "8081",
            ])
        XCTAssertEqual(
            preview.doorURLs,
            [URL(string: "http://127.0.0.1:45371/doorz")!])
    }

    func testStableDoorEndpointsRejectNonLoopbackURLs() {
        let runtime = RuntimeProfile.stable(environment: [
            "BASETEN_SWITCH_DOOR_URLS":
                "https://example.com:8081/doorz,http://127.0.0.1:8082/anything",
        ])
        XCTAssertEqual(
            runtime.doorURLs,
            [URL(string: "http://127.0.0.1:8082/doorz")!])
    }

    func testDoorClientDecodesExactDoorzPayload() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [DoorURLProtocol.self]
        let session = URLSession(configuration: configuration)
        let url = URL(string: "http://127.0.0.1:45371/doorz")!
        DoorURLProtocol.handler = { request in
            XCTAssertEqual(request.url, url)
            let data = Data(
                """
                {
                  "port": 45371,
                  "shape": "anthropic",
                  "router": "127.0.0.1:45372",
                  "fallback_base": "https://api.anthropic.com",
                  "tripped": true,
                  "cooldown_remaining_ms": 12500,
                  "last_transition_unix": 1800000000,
                  "version": "v0.2.0",
                  "future_field": true
                }
                """.utf8)
            return (
                HTTPURLResponse(
                    url: url,
                    statusCode: 200,
                    httpVersion: nil,
                    headerFields: nil)!,
                data)
        }

        let status = try await DoorStatusAPIClient(session: session)
            .fetchDoorStatus(at: url)

        XCTAssertEqual(status.port, 45_371)
        XCTAssertEqual(status.shape, "anthropic")
        XCTAssertEqual(status.router, "127.0.0.1:45372")
        XCTAssertTrue(status.tripped)
        XCTAssertEqual(status.cooldownRemainingMilliseconds, 12_500)
        XCTAssertEqual(status.lastTransitionUnix, 1_800_000_000)
    }

    @MainActor
    func testDoorStorePublishesReachableAndUnreachableEndpoints()
        async throws {
        let urls = [
            URL(string: "http://127.0.0.1:8081/doorz")!,
            URL(string: "http://127.0.0.1:8082/doorz")!,
        ]
        let reader = DoorMappedReader(statuses: [
            urls[0]: doorStatus(tripped: false),
        ])
        let store = DoorStatusStore(
            urls: urls,
            reader: reader,
            refreshInterval: 3_600)

        store.start()
        await waitForDoorStore { store.probes.count == 2 }

        XCTAssertEqual(store.probes.map(\.url), urls)
        XCTAssertEqual(store.probes.map(\.isReachable), [true, false])
        XCTAssertEqual(
            store.probes.last?.errorMessage,
            "Door is unreachable")
        store.stop()
    }

    func testDoorAndClientPresentationLabels() {
        let url = URL(string: "http://127.0.0.1:8081/doorz")!
        let tripped = DoorProbeSnapshot(
            url: url,
            status: doorStatus(tripped: true),
            errorMessage: nil)
        XCTAssertEqual(doorStateLabel(tripped), "Native fallback active")
        XCTAssertEqual(
            doorCooldownLabel(milliseconds: 12_500),
            "13s remaining")
        XCTAssertEqual(
            doorTransitionLabel(
                unix: 1_000,
                now: Date(timeIntervalSince1970: 4_700)),
            "1h ago")

        let client = clientStatus()
        XCTAssertEqual(
            livePathConfiguredRoute(
                client: client,
                globalRoutingEnabled: true),
            "Model mappings · GLM 5.2")
        XCTAssertEqual(
            livePathConfiguredRoute(
                client: client,
                globalRoutingEnabled: false),
            "Native · Anthropic")
        XCTAssertEqual(livePathGatewayHealth(nil), "Running")
        XCTAssertEqual(livePathEffectiveRoute(client), "Baseten · GLM 5.2")
        XCTAssertEqual(livePathModel(client), "GLM 5.2")
        XCTAssertEqual(livePathFallback(client), "Ready · anthropic")
    }

    private func doorStatus(tripped: Bool) -> DoorStatusPayload {
        DoorStatusPayload(
            port: 8_081,
            shape: "anthropic",
            router: "127.0.0.1:18081",
            fallbackBase: "https://api.anthropic.com",
            tripped: tripped,
            cooldownRemainingMilliseconds: tripped ? 12_500 : 0,
            lastTransitionUnix: tripped ? 1_800_000_000 : 0,
            version: "test")
    }

    private func clientStatus() -> ClientStatus {
        ClientStatus(dict: [
            "name": "claude-code",
            "enabled": true,
            "bind_addr": "127.0.0.1:18081",
            "protocol_shape": "anthropic",
            "effective_route": "baseten",
            "native_route": "anthropic",
            "currently_bound": true,
            "effective_summary": "Baseten · GLM-5.2",
            "model_catalog": [[
                "label": "GLM 5.2",
                "storage_target": "zai-org/GLM-5.2",
                "slug": "zai-org/GLM-5.2",
                "alias": "claude-baseten-glm-5-2",
                "available": true,
            ]],
            "fallback": [
                "active": false,
                "served_route": "",
                "cause": "",
            ],
            "families": [
                [
                    "family": "opus",
                    "configured_target": "zai-org/GLM-5.2",
                ],
                [
                    "family": "sonnet",
                    "configured_target": "zai-org/GLM-5.2",
                ],
            ],
            "unmatched_native_model": [
                "configured_target": "zai-org/GLM-5.2",
                "effective_route": "baseten",
                "effective_model": "zai-org/GLM-5.2",
                "effective_source": "default_baseten",
            ],
        ])!
    }
}

@MainActor
private func waitForDoorStore(
    timeout: TimeInterval = 2,
    predicate: @escaping @MainActor () -> Bool
) async {
    let deadline = Date().addingTimeInterval(timeout)
    while !predicate(), Date() < deadline {
        try? await Task.sleep(nanoseconds: 10_000_000)
    }
}
