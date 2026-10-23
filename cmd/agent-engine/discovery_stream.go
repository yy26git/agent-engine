package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/emjin/agent-engine/internal/control"
)

const maxDiscoveryObservers = 64

type episodeDiscoveryEntry struct {
	EpisodeID       string               `json:"episode_id"`
	Template        string               `json:"template"`
	CallerID        string               `json:"caller_id"`
	Stage           control.Stage        `json:"stage"`
	Phase           string               `json:"phase"`
	UpdatedAt       string               `json:"updated_at"`
	StartedAt       string               `json:"started_at"`
	Revision        int                  `json:"revision"`
	Terminal        bool                 `json:"terminal"`
	Progress        control.ProgressView `json:"progress"`
	ExecutionHealth string               `json:"execution_health"`
	SupervisorExit  string               `json:"supervisor_exit_classification,omitempty"`
	RequiresAction  bool                 `json:"requires_action"`
	NextAction      string               `json:"next_action"`
	Phases          []string             `json:"phases,omitempty"`
	VisitedPhases   []string             `json:"visited_phases,omitempty"`
	Profiles        []displayProfile     `json:"profiles,omitempty"`
}

type displayProfile struct {
	Role          string `json:"role"`
	Profile       string `json:"profile"`
	Transport     string `json:"transport"`
	Tool          string `json:"tool,omitempty"`
	Model         string `json:"model,omitempty"`
	ObservedModel string `json:"observed_model,omitempty"`
	Effort        string `json:"effort,omitempty"`
}

type executionHealthView struct {
	health         string
	requiresAction bool
	nextAction     string
	supervisorExit string
}

type episodeDiscoveryFrame struct {
	ID              string                  `json:"id"`
	Type            string                  `json:"type"`
	DeploymentID    string                  `json:"deployment_id"`
	ObservedAt      string                  `json:"observed_at"`
	SourceAvailable bool                    `json:"source_available"`
	SourceError     string                  `json:"source_error,omitempty"`
	ResyncRequired  bool                    `json:"resync_required,omitempty"`
	Episodes        []episodeDiscoveryEntry `json:"episodes"`
}

// startDiscovery runs one sampler while at least one observer is connected.
// Individual observers never scan the store and never own episode execution.
func (api *episodeAPI) startDiscovery() {
	api.discoveryMu.Lock()
	if api.discoveryCancel != nil {
		api.discoveryMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	api.discoveryCancel = cancel
	if api.discoverySubs == nil {
		api.discoverySubs = map[chan episodeDiscoveryFrame]struct{}{}
	}
	api.discoveryMu.Unlock()
	// Refresh now, then keep retrying even if the first read fails. A temporary
	// store or deployment read failure must not permanently disable discovery.
	api.refreshDiscovery()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				api.refreshDiscovery()
			}
		}
	}()
}

func (api *episodeAPI) refreshDiscovery() {
	deployment, err := api.store.Deployment()
	if err != nil {
		api.markDiscoveryUnavailable("deployment identity is unavailable")
		return
	}
	entries, err := api.episodeSnapshot(100)
	if err != nil {
		api.markDiscoveryUnavailable("episode state could not be read")
		return
	}
	api.publishDiscovery(deployment.ID, entries, true, "")
}

func (api *episodeAPI) markDiscoveryUnavailable(reason string) {
	api.discoveryMu.Lock()
	var deployment string
	var entries []episodeDiscoveryEntry
	if api.discoveryLatest != nil {
		deployment = api.discoveryLatest.DeploymentID
		entries = api.discoveryLatest.Episodes
	}
	api.discoveryMu.Unlock()
	api.publishDiscovery(deployment, entries, false, reason)
}

func (api *episodeAPI) publishDiscovery(deployment string, entries []episodeDiscoveryEntry, available bool, reason string) {
	canonical, err := json.Marshal(struct {
		DeploymentID    string                  `json:"deployment_id"`
		SourceAvailable bool                    `json:"source_available"`
		SourceError     string                  `json:"source_error,omitempty"`
		Episodes        []episodeDiscoveryEntry `json:"episodes"`
	}{DeploymentID: deployment, SourceAvailable: available, SourceError: reason, Episodes: entries})
	if err != nil {
		return
	}
	sum := sha256.Sum256(canonical)
	frame := episodeDiscoveryFrame{
		ID: hex.EncodeToString(sum[:]), Type: "update", DeploymentID: deployment,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Episodes: entries,
		SourceAvailable: available, SourceError: reason,
	}
	api.discoveryMu.Lock()
	defer api.discoveryMu.Unlock()
	if api.discoveryLatest != nil && api.discoveryLatest.ID == frame.ID {
		return
	}
	api.discoveryLatest = &frame
	for subscriber := range api.discoverySubs {
		select {
		case subscriber <- frame:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- frame:
			default:
			}
		}
	}
}

func (api *episodeAPI) episodeSnapshot(limit int) ([]episodeDiscoveryEntry, error) {
	states, err := api.store.EpisodeStates(api.callerPrefix, api.templates, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]episodeDiscoveryEntry, 0, len(states))
	for _, state := range states {
		request, err := api.store.Request(state.RequestID)
		if err != nil {
			return nil, err
		}
		progress := state.ProgressSnapshot()
		entry := episodeDiscoveryEntry{
			EpisodeID: state.Episode.EpisodeID, Template: state.EpisodeTemplate,
			CallerID: request.Origin.CallerID, Stage: state.Stage, Phase: state.Episode.Phase,
			UpdatedAt: state.UpdatedAt, Revision: state.Revision, Terminal: state.Stage.Terminal(),
			StartedAt: state.Episode.StartedAt,
			Progress:  progress,
		}
		health := api.executionHealth(state)
		entry.ExecutionHealth, entry.SupervisorExit, entry.RequiresAction, entry.NextAction = health.health, health.supervisorExit, health.requiresAction, health.nextAction
		if template, ok := api.store.Config().EpisodeTemplates.Lookup(state.EpisodeTemplate); ok {
			for _, phase := range template.Phases {
				entry.Phases = append(entry.Phases, string(phase))
			}
		}
		visited := map[string]bool{}
		for _, move := range state.Episode.PhaseHistory {
			if !visited[move.To] {
				entry.VisitedPhases = append(entry.VisitedPhases, move.To)
				visited[move.To] = true
			}
		}
		for role, profile := range state.ExecutionProfiles {
			observedModel := observedEpisodeModel(state, role)
			entry.Profiles = append(entry.Profiles, displayProfile{
				Role: string(role), Profile: profile.Name, Transport: string(profile.Transport),
				Tool: profile.Tool, Model: profile.Model, ObservedModel: observedModel, Effort: profile.Effort,
			})
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// observedEpisodeModel returns only a model identity measured on a completed
// turn or correlated reviewer receipt. The configured profile remains separate
// so presentation never makes a requested model look observed.
func observedEpisodeModel(state *control.State, role control.ProfileRole) string {
	if state == nil || state.Episode == nil {
		return ""
	}
	for i := len(state.Episode.Execution.Steps) - 1; i >= 0; i-- {
		step := state.Episode.Execution.Steps[i]
		if step.Role != string(role) {
			continue
		}
		if step.SourceModel != "" {
			return step.SourceModel
		}
		if step.ModelEcho != "" {
			return step.ModelEcho
		}
	}
	if role == control.RoleReviewer {
		for i := len(state.Episode.Execution.Reviews) - 1; i >= 0; i-- {
			if state.Episode.Execution.Reviews[i].ModelEcho != "" {
				return state.Episode.Execution.Reviews[i].ModelEcho
			}
		}
	}
	return ""
}

func (api *episodeAPI) executionHealth(state *control.State) executionHealthView {
	progress := state.ProgressSnapshot()
	view := executionHealthView{health: "unknown", nextAction: progress.NextAction}
	if state.Stage.Terminal() {
		view.health = "terminal"
		return view
	}
	if state.RuntimeBlock != nil {
		view.health, view.requiresAction = "blocked", true
		return view
	}
	if progress.WaitingForOperator {
		view.health, view.requiresAction = "awaiting_operator", true
		return view
	}
	if state.CancellationRequested {
		view.health, view.requiresAction = "recovering", true
		view.nextAction = "cancellation is pending; let the existing supervisor reconcile owned effects before starting replacement work"
		return view
	}
	held, err := api.store.OwnershipHeld(state.RequestID)
	if err != nil {
		view.requiresAction = true
		view.nextAction = "supervisor ownership could not be established; inspect the local Engine state"
		return view
	}
	if held || (api.supervisorActive != nil && api.supervisorActive(state.RequestID)) {
		view.health = "running"
		return view
	}
	view.health, view.requiresAction = "stopped", true
	view.nextAction = "inspect retained progress, then resume the existing episode supervisor"
	if state.Supervisor != nil {
		switch {
		case state.Supervisor.State != control.SupervisorRunning:
			view.supervisorExit = state.Supervisor.LastExitKind
		case state.Supervisor.StopRequestedAt != "":
			view.supervisorExit = control.SupervisorIntentionalStop
		default:
			// The durable record said this process was running, but the exclusive
			// request owner is now absent. Until the next owner writes its measured
			// reconciliation, do not guess which signal or crash ended it.
			view.supervisorExit = "unclassified_exit"
		}
	}
	return view
}

func (api *episodeAPI) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	if api.observers.Add(1) > maxDiscoveryObservers {
		api.observers.Add(-1)
		http.Error(w, "too many observers", http.StatusTooManyRequests)
		return
	}
	defer api.observers.Add(-1)
	subscriber := make(chan episodeDiscoveryFrame, 1)
	api.discoveryMu.Lock()
	if api.discoverySubs == nil {
		api.discoverySubs = map[chan episodeDiscoveryFrame]struct{}{}
	}
	api.discoverySubs[subscriber] = struct{}{}
	api.discoveryMu.Unlock()
	api.startDiscovery()
	defer func() {
		api.discoveryMu.Lock()
		delete(api.discoverySubs, subscriber)
		var cancel context.CancelFunc
		if len(api.discoverySubs) == 0 {
			cancel = api.discoveryCancel
			api.discoveryCancel = nil
		}
		api.discoveryMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	api.discoveryMu.Lock()
	latest := api.discoveryLatest
	api.discoveryMu.Unlock()
	if latest == nil {
		http.Error(w, "episode discovery is unavailable", http.StatusServiceUnavailable)
		return
	}
	frame := *latest
	frame.Type = "snapshot"
	last := r.Header.Get("Last-Event-ID")
	if last != "" && last != latest.ID {
		frame.Type = "resync-required"
		frame.ResyncRequired = true
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(item episodeDiscoveryFrame) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
			return err
		}
		body, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", item.ID, item.Type, body); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write(frame); err != nil {
		return
	}
	last = frame.ID
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-subscriber:
			if frame.ID == last {
				continue
			}
			frame.Type = "update"
			if err := write(frame); err != nil {
				return
			}
			last = frame.ID
		case <-heartbeat.C:
			if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || controller.Flush() != nil {
				return
			}
		}
	}
}
