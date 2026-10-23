import XCTest
@testable import AgentEngineHUDCore

final class ObserverStreamPolicyTests: XCTestCase {
    func testRequestPolicyAllowsOnlyLoopbackGETWithoutBody() throws {
        let endpoint = try XCTUnwrap(URL(string: "http://127.0.0.1:8787"))
        let request = try XCTUnwrap(ObserverRequestPolicy.discoveryRequest(
            base: endpoint, token: String(repeating: "t", count: 32), lastEventID: "cursor-1"
        ))
        XCTAssertEqual(request.httpMethod, "GET")
        XCTAssertNil(request.httpBody)
        XCTAssertEqual(request.value(forHTTPHeaderField: "Last-Event-ID"), "cursor-1")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer \(String(repeating: "t", count: 32))")
    }

    func testRequestPolicyRejectsRemoteAndMalformedDestinations() throws {
        for raw in ["https://127.0.0.1:8787", "http://example.com", "http://127.0.0.1:8787/mutate", "http://user@127.0.0.1"] {
            let url = try XCTUnwrap(URL(string: raw))
            XCTAssertNil(ObserverRequestPolicy.discoveryRequest(base: url, token: String(repeating: "t", count: 32), lastEventID: nil), raw)
        }
        let local = try XCTUnwrap(URL(string: "http://localhost:8787"))
        XCTAssertNil(ObserverRequestPolicy.discoveryRequest(base: local, token: "short", lastEventID: nil))
    }

    func testStreamBudgetAcceptsLimitAndResetsAtFrameBoundary() throws {
        var budget = ObserverStreamBudget()
        for _ in 0..<ObserverStreamBudget.maximumBytes { try budget.append(97) }
        XCTAssertThrowsError(try budget.append(97)) { XCTAssertEqual($0 as? ObserverStreamError, .lineTooLarge) }

        var frame = ObserverStreamBudget()
        try frame.appendDataLine(String(repeating: "x", count: ObserverStreamBudget.maximumBytes - 1))
        frame.finishFrame()
        try frame.appendDataLine("x")
    }

    func testStreamBudgetRejectsOversizedDataFrame() {
        var budget = ObserverStreamBudget()
        XCTAssertThrowsError(try budget.appendDataLine(String(repeating: "x", count: ObserverStreamBudget.maximumBytes))) {
            XCTAssertEqual($0 as? ObserverStreamError, .frameTooLarge)
        }
    }

    func testCursorAdvancesOnlyForCorrelatedFramesAndResyncsDeploymentChanges() {
        let old = DiscoveryCursorCheckpoint(deploymentID: "deployment-a", eventID: "old")
        XCTAssertEqual(DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "new", payloadID: "new", eventType: "update",
            deploymentID: "deployment-b", resyncRequired: false
        ), .rejectAndReconnect)

        let snapshot = DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "snapshot-2", payloadID: "snapshot-2", eventType: "resync-required",
            deploymentID: "deployment-b", resyncRequired: true
        )
        XCTAssertEqual(snapshot, .accept(.init(deploymentID: "deployment-b", eventID: "snapshot-2")))

        XCTAssertEqual(DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "header", payloadID: "different", eventType: "update",
            deploymentID: "deployment-a", resyncRequired: false
        ), .rejectAndReconnect)
        XCTAssertEqual(DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "same-deployment", payloadID: "same-deployment", eventType: "update",
            deploymentID: "deployment-a", resyncRequired: false
        ), .accept(.init(deploymentID: "deployment-a", eventID: "same-deployment")))
    }
}
