import AppKit
import SwiftUI

struct HUDRootView: View {
    @StateObject private var observer = EpisodeDiscovery()
    @AppStorage("AgentEngineHUD.endpoint") private var endpoint = "http://127.0.0.1:8787"
    @AppStorage("AgentEngineHUD.observerTokenFile") private var tokenFile = ""
    @AppStorage("AgentEngineHUD.selectedEpisode") private var selectedEpisode = ""
    @AppStorage("AgentEngineHUD.expanded") private var expanded = false
    @State private var inspectionMessage = ""

    let onExpand: (Bool) -> Void

    private var selected: EpisodeCard? {
        observer.episodes.first(where: { $0.episodeID == selectedEpisode })
            ?? observer.episodes.first(where: { $0.requiresAction })
            ?? observer.episodes.first(where: { $0.executionHealth == "running" })
            ?? observer.episodes.first
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            if expanded {
                connectionSettings
                if !observer.lastError.isEmpty {
                    Text(observer.lastError)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .fixedSize(horizontal: false, vertical: true)
                }
                workflowList
                if let selected {
                    Divider()
                    details(selected)
                }
            } else if let selected {
                HStack(spacing: 8) {
                    Circle()
                        .fill(selected.requiresAction ? .orange : color(for: selected.executionHealth))
                        .frame(width: 8, height: 8)
                    Text(selected.template)
                        .font(.subheadline.weight(.medium))
                        .lineLimit(1)
                    Text("· \(selected.phase)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                    if let role = selected.progress.role, !role.isEmpty {
                        Text("· \(role)")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                    }
                    Spacer(minLength: 2)
                    Text(selected.executionHealth.replacingOccurrences(of: "_", with: " "))
                        .font(.caption2)
                        .foregroundStyle(selected.requiresAction ? .orange : .secondary)
                        .lineLimit(1)
                }
            } else {
                Text(observer.connectionText)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(14)
        .frame(minWidth: expanded ? 380 : 280, idealWidth: expanded ? 420 : 310, maxWidth: 460, alignment: .leading)
        .background(.regularMaterial)
        .onChange(of: observer.episodes.map(\.episodeID)) { ids in
            if !ids.contains(selectedEpisode) {
                selectedEpisode = observer.episodes.first?.episodeID ?? ""
            }
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            Image(systemName: "waveform.path")
                .foregroundColor(.accentColor)
            Text("Agent Engine")
                .font(.headline)
            Spacer()
            Text(observer.connectionText)
                .font(.caption2)
                .foregroundStyle(observer.isConnected ? .green : .secondary)
                .lineLimit(1)
            Button(expanded ? "Collapse" : "Details") {
                expanded.toggle()
                onExpand(expanded)
            }
            .buttonStyle(.borderless)
        }
    }

    private var connectionSettings: some View {
        VStack(alignment: .leading, spacing: 6) {
            TextField("Local API URL", text: $endpoint)
                .textFieldStyle(.roundedBorder)
            HStack(spacing: 6) {
                TextField("Private observer token file", text: $tokenFile)
                    .textFieldStyle(.roundedBorder)
                Button(observer.isConnected ? "Disconnect" : "Connect") {
                    if observer.isConnected {
                        observer.disconnect()
                    } else {
                        observer.connect(endpoint: endpoint, tokenFile: tokenFile)
                    }
                }
                .disabled(!observer.isConnected && tokenFile.isEmpty)
            }
            Text("Local endpoints only. Token values stay in the private file.")
                .font(.caption2)
                .foregroundStyle(.tertiary)
        }
    }

    private var workflowList: some View {
        Group {
            if observer.episodes.isEmpty {
                VStack(spacing: 6) {
                    Image(systemName: "rectangle.stack")
                    Text("No workflows")
                        .font(.subheadline)
                    Text("The observer shows workflows admitted by this local service.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                }
                .frame(maxWidth: .infinity)
                .frame(height: 150)
            } else {
                List(observer.episodes) { episode in
                    Button {
                        selectedEpisode = episode.episodeID
                    } label: {
                        HStack(alignment: .top, spacing: 8) {
                            Circle()
                                .fill(episode.requiresAction ? .orange : color(for: episode.executionHealth))
                                .frame(width: 7, height: 7)
                                .padding(.top, 5)
                            VStack(alignment: .leading, spacing: 3) {
                                Text(episode.template)
                                    .font(.subheadline.weight(.medium))
                                    .lineLimit(1)
                                Text("\(episode.phase) · \(episode.executionHealth.replacingOccurrences(of: "_", with: " "))")
                                    .font(.caption)
                                    .foregroundStyle(episode.requiresAction ? .orange : .secondary)
                                    .lineLimit(1)
                            }
                            Spacer()
                            if episode.episodeID == selectedEpisode {
                                Image(systemName: "checkmark")
                                    .foregroundColor(.accentColor)
                            }
                        }
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                }
                .listStyle(.plain)
                .frame(height: 190)
            }
        }
    }

    private func details(_ episode: EpisodeCard) -> some View {
        VStack(alignment: .leading, spacing: 7) {
            Text("\(episode.template) · \(episode.episodeID)")
                .font(.subheadline.weight(.semibold))
            Text("\(episode.stage) / \(episode.phase) · \(episode.executionHealth.replacingOccurrences(of: "_", with: " "))")
                .font(.caption)
                .foregroundStyle(.secondary)
            if let exit = episode.supervisorExitClassification, !exit.isEmpty {
                Text("Last supervisor state: \(exit.replacingOccurrences(of: "_", with: " "))")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            if !episode.callerID.isEmpty {
                Text("Caller: \(episode.callerID)")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            if let role = episode.progress.role, !role.isEmpty {
                let profile = episode.progress.profile.map { " · \($0)" } ?? ""
                Text("Active role: \(role)\(profile)")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            if let started = ISO8601DateFormatter().date(from: episode.startedAt) {
                HStack(spacing: 4) {
                    Text("Elapsed wall time:")
                    Text(started, style: .timer)
                }
                .font(.caption2)
                .foregroundStyle(.secondary)
            }
            if let reason = episode.progress.reason, !reason.isEmpty {
                Text(reason)
                    .font(.caption)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if episode.requiresAction {
                Label(episode.nextAction, systemImage: "exclamationmark.circle")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            HStack(spacing: 8) {
                Button("Copy episode ID") {
                    copyInspectionText(EpisodeInspectionNavigation.episodeID(for: episode, in: observer.episodes),
                                       success: "Episode ID copied.")
                }
                .disabled(EpisodeInspectionNavigation.episodeID(for: episode, in: observer.episodes) == nil)
                Button("Copy read-only status command") {
                    copyInspectionText(EpisodeInspectionNavigation.statusCommand(for: episode, in: observer.episodes),
                                       success: "Status command copied. Replace /absolute/store before running it.")
                }
                .disabled(EpisodeInspectionNavigation.statusCommand(for: episode, in: observer.episodes) == nil)
            }
            .buttonStyle(.borderless)
            if !inspectionMessage.isEmpty {
                Text(inspectionMessage)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let phases = episode.phases, !phases.isEmpty {
                Text("Configured flow: \(phases.joined(separator: " → "))")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let visited = episode.visitedPhases, !visited.isEmpty {
                Text("Phases entered: \(visited.joined(separator: " → "))")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            Text("Token usage: unavailable from the current provider receipt")
                .font(.caption2)
                .foregroundStyle(.tertiary)
            ForEach(episode.profiles ?? []) { profile in
                let selection = [profile.tool, profile.model.map { "requested \($0)" }, profile.effort]
                    .compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
                if !selection.isEmpty {
                    Text("\(profile.role): \(profile.profile) · \(selection)")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
                if let observed = profile.observedModel, !observed.isEmpty {
                    Text("\(profile.role) measured model: \(observed)")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
        }
    }

    private func copyInspectionText(_ value: String?, success: String) {
        guard let value else {
            inspectionMessage = "This episode is no longer in the local discovery snapshot. Refresh the observer before copying its destination."
            return
        }
        NSPasteboard.general.clearContents()
        guard NSPasteboard.general.setString(value, forType: .string) else {
            inspectionMessage = "Could not copy inspection text."
            return
        }
        inspectionMessage = success
    }

    private func color(for health: String) -> Color {
        switch health {
        case "running": return .green
        case "awaiting_operator": return .orange
        case "blocked", "stopped": return .red
        case "terminal": return .secondary
        default: return .gray
        }
    }
}
