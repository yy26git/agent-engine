import Foundation

@main
enum ObserverPolicyFixtureRunner {
    static func main() throws {
        let loopback = URL(string: "http://127.0.0.1:8787")!
        let token = String(repeating: "t", count: 32)
        let request = ObserverRequestPolicy.discoveryRequest(base: loopback, token: token, lastEventID: "cursor-1")!
        precondition(request.httpMethod == "GET")
        precondition(request.httpBody == nil)
        precondition(request.value(forHTTPHeaderField: "Last-Event-ID") == "cursor-1")
        for address in ["https://127.0.0.1:8787", "http://example.com", "http://127.0.0.1:8787/mutate", "http://user@127.0.0.1"] {
            precondition(ObserverRequestPolicy.discoveryRequest(base: URL(string: address)!, token: token, lastEventID: nil) == nil)
        }

        var budget = ObserverStreamBudget()
        for _ in 0..<ObserverStreamBudget.maximumBytes { try budget.append(97) }
        do {
            try budget.append(97)
            preconditionFailure("oversized line was accepted")
        } catch ObserverStreamError.lineTooLarge {}
        var frameBudget = ObserverStreamBudget()
        do {
            try frameBudget.appendDataLine(String(repeating: "x", count: ObserverStreamBudget.maximumBytes))
            preconditionFailure("oversized frame was accepted")
        } catch ObserverStreamError.frameTooLarge {}

        let old = DiscoveryCursorCheckpoint(deploymentID: "deployment-a", eventID: "old")
        let unsafeTransition = DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "new", payloadID: "new", eventType: "update",
            deploymentID: "deployment-b", resyncRequired: false
        )
        precondition(unsafeTransition == .rejectAndReconnect)
        let resynced = DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "snapshot-2", payloadID: "snapshot-2", eventType: "resync-required",
            deploymentID: "deployment-b", resyncRequired: true
        )
        precondition(resynced == .accept(.init(deploymentID: "deployment-b", eventID: "snapshot-2")))
        let mismatched = DiscoveryCursorPolicy.decide(
            previous: old, envelopeID: "header", payloadID: "body", eventType: "update",
            deploymentID: "deployment-a", resyncRequired: false
        )
        precondition(mismatched == .rejectAndReconnect)
        print("HUD observer fixtures passed: GET-only policy, loopback restrictions, stream bounds, cursor correlation and deployment resync")
    }
}
