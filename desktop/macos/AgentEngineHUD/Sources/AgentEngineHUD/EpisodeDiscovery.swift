import Foundation
import Combine
#if canImport(AgentEngineHUDCore)
import AgentEngineHUDCore
#endif

struct EpisodeDiscoveryFrame: Decodable {
    let id: String
    let type: String
    let deploymentID: String
    let observedAt: String
    let sourceAvailable: Bool
    let sourceError: String?
    let resyncRequired: Bool?
    let episodes: [EpisodeCard]

    enum CodingKeys: String, CodingKey {
        case id, type, episodes
        case deploymentID = "deployment_id"
        case observedAt = "observed_at"
        case sourceAvailable = "source_available"
        case sourceError = "source_error"
        case resyncRequired = "resync_required"
    }
}

struct EpisodeCard: Decodable, Identifiable {
    let episodeID: String
    let template: String
    let callerID: String
    let stage: String
    let phase: String
    let updatedAt: String
    let startedAt: String
    let terminal: Bool
    let progress: EpisodeProgress
    let executionHealth: String
    let supervisorExitClassification: String?
    let requiresAction: Bool
    let nextAction: String
    let phases: [String]?
    let visitedPhases: [String]?
    let profiles: [ProfileSelection]?

    var id: String { episodeID }

    enum CodingKeys: String, CodingKey {
        case template, stage, phase, terminal, progress, phases, profiles
        case episodeID = "episode_id"
        case callerID = "caller_id"
        case updatedAt = "updated_at"
        case startedAt = "started_at"
        case executionHealth = "execution_health"
        case supervisorExitClassification = "supervisor_exit_classification"
        case requiresAction = "requires_action"
        case nextAction = "next_action"
        case visitedPhases = "visited_phases"
    }
}

struct EpisodeProgress: Decodable {
    let reason: String?
    let waitingForOperator: Bool
    let actionKind: String?
    let role: String?
    let profile: String?
    let sessionID: String?
    let planDigest: String?

    enum CodingKeys: String, CodingKey {
        case reason, profile, role
        case waitingForOperator = "waiting_for_operator"
        case actionKind = "action_kind"
        case sessionID = "session_id"
        case planDigest = "plan_digest"
    }
}

struct ProfileSelection: Decodable, Identifiable {
    let role: String
    let profile: String
    let transport: String
    let tool: String?
    let model: String?
    let observedModel: String?
    let effort: String?

    enum CodingKeys: String, CodingKey {
        case role, profile, transport, tool, model, effort
        case observedModel = "observed_model"
    }

    var id: String { role }
}

/// Creates copy-only inspection destinations for episodes in the current
/// authenticated discovery snapshot. The HUD never executes this command.
enum EpisodeInspectionNavigation {
    static func episodeID(for candidate: EpisodeCard, in registeredEpisodes: [EpisodeCard]) -> String? {
        let matches = registeredEpisodes.filter { $0.episodeID == candidate.episodeID }
        guard matches.count == 1,
              !candidate.episodeID.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              candidate.episodeID.utf8.count <= 512,
              !candidate.episodeID.unicodeScalars.contains(where: CharacterSet.controlCharacters.contains) else {
            return nil
        }
        return candidate.episodeID
    }

    static func statusCommand(for candidate: EpisodeCard, in registeredEpisodes: [EpisodeCard]) -> String? {
        guard let episodeID = episodeID(for: candidate, in: registeredEpisodes) else { return nil }
        return "agent-engine --store /absolute/store episode status --episode-id \(shellQuote(episodeID)) --json"
    }

    private static func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

@MainActor
final class EpisodeDiscovery: ObservableObject {
    @Published private(set) var episodes: [EpisodeCard] = []
    @Published private(set) var connectionText = "Not connected"
    @Published private(set) var isConnected = false
    @Published private(set) var lastError = ""

    private var task: Task<Void, Never>?
    private var lastEventID = ""
    private var cursorCheckpoint: DiscoveryCursorCheckpoint?
    private let decoder = JSONDecoder()

    func connect(endpoint: String, tokenFile: String) {
        disconnect()
        task = Task { [weak self] in
            guard let self else { return }
            await self.observe(endpoint: endpoint, tokenFile: tokenFile)
        }
    }

    func disconnect() {
        task?.cancel()
        task = nil
        isConnected = false
        connectionText = episodes.isEmpty ? "Not connected" : "Disconnected · showing last update"
    }

    private func observe(endpoint: String, tokenFile: String) async {
        do {
            let base = try validateEndpoint(endpoint)
            let cursorKey = cursorPreferenceKey(for: base)
            if let data = UserDefaults.standard.data(forKey: cursorKey),
               let checkpoint = try? JSONDecoder().decode(DiscoveryCursorCheckpoint.self, from: data) {
                cursorCheckpoint = checkpoint
                lastEventID = checkpoint.eventID
            } else {
                cursorCheckpoint = nil
                lastEventID = ""
            }
            let session = makeSession()
            var delay: UInt64 = 1_000_000_000
            while !Task.isCancelled {
                do {
                    // Re-read on reconnect so an operator can rotate the local
                    // observer credential without restarting the HUD.
                    let token = try loadToken(tokenFile)
                    try await stream(base: base, token: token, session: session)
                    if Task.isCancelled { return }
                    throw ObserverError.streamEnded
                } catch {
                    if Task.isCancelled { return }
                    isConnected = false
                    connectionText = episodes.isEmpty ? "Reconnecting" : "Disconnected · showing last update"
                    lastError = error.localizedDescription
                    try? await Task.sleep(nanoseconds: delay)
                    delay = min(delay * 2, 30_000_000_000)
                }
            }
        } catch {
            isConnected = false
            connectionText = "Connection unavailable"
            lastError = error.localizedDescription
        }
    }

    private func stream(base: URL, token: String, session: URLSession) async throws {
        guard let request = ObserverRequestPolicy.discoveryRequest(
            base: base, token: token, lastEventID: lastEventID
        ) else { throw ObserverError.endpointMustBeLoopback }
        let (bytes, response) = try await session.bytes(for: request)
        guard let response = response as? HTTPURLResponse else { throw ObserverError.invalidResponse }
        guard response.statusCode == 200 else { throw ObserverError.httpStatus(response.statusCode) }

        var buffer: [UInt8] = []
        var dataLines: [String] = []
        var budget = ObserverStreamBudget()
        var envelopeID = ""
        var eventType = ""
        for try await byte in bytes {
            if Task.isCancelled { return }
            try budget.append(byte)
            if byte == 10 {
                if buffer.last == 13 { buffer.removeLast() }
                let line = String(decoding: buffer, as: UTF8.self)
                buffer.removeAll(keepingCapacity: true)
                if line.isEmpty {
                    if !dataLines.isEmpty {
                        let payload = Data(dataLines.joined(separator: "\n").utf8)
                        let frame = try decoder.decode(EpisodeDiscoveryFrame.self, from: payload)
                        switch DiscoveryCursorPolicy.decide(
                            previous: cursorCheckpoint,
                            envelopeID: envelopeID,
                            payloadID: frame.id,
                            eventType: eventType,
                            deploymentID: frame.deploymentID,
                            resyncRequired: frame.resyncRequired ?? false
                        ) {
                        case .accept(let checkpoint):
                            cursorCheckpoint = checkpoint
                            lastEventID = checkpoint.eventID
                            if let saved = try? JSONEncoder().encode(checkpoint) {
                                UserDefaults.standard.set(saved, forKey: cursorPreferenceKey(for: base))
                            }
                        case .rejectAndReconnect:
                            cursorCheckpoint = nil
                            lastEventID = ""
                            UserDefaults.standard.removeObject(forKey: cursorPreferenceKey(for: base))
                            throw ObserverError.cursorResyncRequired
                        }
                        episodes = frame.episodes
                        isConnected = true
                        if frame.sourceAvailable {
                            connectionText = "Connected · \(episodes.count) workflows"
                            lastError = ""
                        } else {
                            connectionText = "Connected · showing last snapshot"
                            lastError = frame.sourceError ?? "Agent Engine could not read current state."
                        }
                    }
                    dataLines.removeAll(keepingCapacity: true)
                    budget.finishFrame()
                    envelopeID = ""
                    eventType = ""
                } else if line.hasPrefix("data:") {
                    let data = String(line.dropFirst(5)).trimmingCharacters(in: .whitespaces)
                    try budget.appendDataLine(data)
                    dataLines.append(data)
                } else if line.hasPrefix("id:") {
                    envelopeID = String(line.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                } else if line.hasPrefix("event:") {
                    eventType = String(line.dropFirst(6)).trimmingCharacters(in: .whitespaces)
                }
            } else {
                buffer.append(byte)
            }
        }
    }

    private func validateEndpoint(_ value: String) throws -> URL {
        guard let url = URL(string: value.trimmingCharacters(in: .whitespacesAndNewlines)),
              url.scheme == "http",
              let host = url.host?.lowercased(),
              ["127.0.0.1", "localhost", "::1"].contains(host),
              url.user == nil, url.password == nil, url.query == nil, url.fragment == nil,
              url.path.isEmpty || url.path == "/" else {
            throw ObserverError.endpointMustBeLoopback
        }
        return url
    }

    private func cursorPreferenceKey(for url: URL) -> String {
        let port = url.port.map(String.init) ?? "default"
        return "AgentEngineHUD.discovery.\(url.host ?? "local"):\(port)"
    }

    private func loadToken(_ path: String) throws -> String {
        guard path.hasPrefix("/") else { throw ObserverError.tokenPathMustBeAbsolute }
        let tokenURL = URL(fileURLWithPath: path)
        let attributes = try FileManager.default.attributesOfItem(atPath: path)
        let linkValues = try tokenURL.resourceValues(forKeys: [.isSymbolicLinkKey])
        guard attributes[.type] as? FileAttributeType == .typeRegular,
              linkValues.isSymbolicLink != true,
              let permissions = (attributes[.posixPermissions] as? NSNumber)?.intValue,
              permissions & 0o177 == 0 else { throw ObserverError.tokenFileMustBePrivate }
        let data = try Data(contentsOf: URL(fileURLWithPath: path), options: .mappedIfSafe)
        guard let token = String(data: data, encoding: .utf8)?
            .trimmingCharacters(in: .whitespacesAndNewlines), token.utf8.count >= 32 else {
            throw ObserverError.invalidToken
        }
        return token
    }

    private func makeSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = 45
        return URLSession(configuration: configuration, delegate: NoRedirectDelegate(), delegateQueue: nil)
    }
}

private final class NoRedirectDelegate: NSObject, URLSessionTaskDelegate {
    func urlSession(
        _ session: URLSession,
        task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse,
        newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        completionHandler(nil)
    }
}

private enum ObserverError: LocalizedError {
    case endpointMustBeLoopback
    case tokenPathMustBeAbsolute
    case tokenFileMustBePrivate
    case invalidToken
    case invalidResponse
    case httpStatus(Int)
    case eventTooLarge
    case cursorResyncRequired
    case streamEnded

    var errorDescription: String? {
        switch self {
        case .endpointMustBeLoopback: return "Use a local http://127.0.0.1 endpoint."
        case .tokenPathMustBeAbsolute: return "Observer token file path must be absolute."
        case .tokenFileMustBePrivate: return "Observer token file permissions must be 0600 or stricter."
        case .invalidToken: return "Observer token file must contain at least 32 UTF-8 bytes."
        case .invalidResponse: return "Agent Engine returned an invalid response."
        case .httpStatus(let code): return "Agent Engine observer returned HTTP \(code)."
        case .eventTooLarge: return "Agent Engine discovery event exceeded 1 MiB."
        case .cursorResyncRequired: return "Agent Engine discovery cursor changed; reconnecting from a fresh snapshot."
        case .streamEnded: return "Agent Engine discovery stream ended."
        }
    }
}
