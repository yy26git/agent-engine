import Foundation

public enum ObserverStreamError: Error, Equatable {
    case lineTooLarge
    case frameTooLarge
}

/// Shared bounds for an untrusted local HTTP event stream.
public struct ObserverStreamBudget {
    public static let maximumBytes = 1_048_576

    private var lineBytes = 0
    private var dataBytes = 0

    public init() {}

    public mutating func append(_ byte: UInt8) throws {
        if byte == 10 {
            lineBytes = 0
            return
        }
        lineBytes += 1
        guard lineBytes <= Self.maximumBytes else { throw ObserverStreamError.lineTooLarge }
    }

    public mutating func appendDataLine(_ value: String) throws {
        dataBytes += value.utf8.count + 1
        guard dataBytes <= Self.maximumBytes else { throw ObserverStreamError.frameTooLarge }
    }

    public mutating func finishFrame() {
        dataBytes = 0
    }
}

public struct DiscoveryCursorCheckpoint: Codable, Equatable {
    public let deploymentID: String
    public let eventID: String

    public init(deploymentID: String, eventID: String) {
        self.deploymentID = deploymentID
        self.eventID = eventID
    }
}

public enum DiscoveryCursorDecision: Equatable {
    case accept(DiscoveryCursorCheckpoint)
    case rejectAndReconnect
}

/// Accepts a cursor only when the payload and SSE envelope agree and any
/// deployment transition is represented by a full snapshot/resync frame.
public enum DiscoveryCursorPolicy {
    public static func decide(
        previous: DiscoveryCursorCheckpoint?,
        envelopeID: String,
        payloadID: String,
        eventType: String,
        deploymentID: String,
        resyncRequired: Bool
    ) -> DiscoveryCursorDecision {
        guard !envelopeID.isEmpty,
              envelopeID == payloadID,
              !deploymentID.isEmpty else { return .rejectAndReconnect }

        if let previous, previous.deploymentID != deploymentID,
           eventType != "snapshot", !resyncRequired {
            return .rejectAndReconnect
        }

        guard ["snapshot", "resync-required", "update"].contains(eventType) else {
            return .rejectAndReconnect
        }

        return .accept(.init(deploymentID: deploymentID, eventID: envelopeID))
    }
}

/// Constructs the sole HTTP request shape available to the observer. Keeping
/// this builder pure makes the read-only boundary directly testable.
public enum ObserverRequestPolicy {
    public static func discoveryRequest(base: URL, token: String, lastEventID: String?) -> URLRequest? {
        guard base.scheme == "http",
              let host = base.host?.lowercased(),
              ["127.0.0.1", "localhost", "::1"].contains(host),
              base.user == nil, base.password == nil, base.query == nil, base.fragment == nil,
              base.path.isEmpty || base.path == "/",
              token.utf8.count >= 32 else { return nil }
        var request = URLRequest(url: base.appendingPathComponent("episodes/events"))
        request.httpMethod = "GET"
        request.timeoutInterval = 60
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let lastEventID, !lastEventID.isEmpty {
            request.setValue(lastEventID, forHTTPHeaderField: "Last-Event-ID")
        }
        return request
    }
}
