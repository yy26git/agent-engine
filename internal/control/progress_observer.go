package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

type progressCursor struct {
	Deployment    string `json:"deployment"`
	RequestDigest string `json:"request_sha256"`
	Sequence      uint64 `json:"sequence"`
}

type ProgressFrame struct {
	Type           string `json:"type"`
	DeploymentID   string `json:"deployment_id"`
	EventID        string `json:"event_id"`
	ResyncRequired bool   `json:"resync_required,omitempty"`
	ProgressEvent
}

func cursorFor(d Deployment, s *State, sequence uint64) string {
	b, _ := json.Marshal(progressCursor{Deployment: d.ID, RequestDigest: s.RequestDigest, Sequence: sequence})
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) ProgressFrames(id, cursor string) ([]ProgressFrame, *State, error) {
	d, err := s.Deployment()
	if err != nil {
		return nil, nil, fmt.Errorf("initialize deployment identity with deployment-init before subscribing: %w", err)
	}
	state, err := s.State(id)
	if err != nil {
		return nil, nil, err
	}
	if state.Episode == nil {
		return nil, nil, fmt.Errorf("request is not an episode")
	}
	latest := uint64(0)
	if state.Progress != nil {
		latest = state.Progress.Latest
	}
	snapshot := func(kind string, gap bool) ProgressFrame {
		return ProgressFrame{Type: kind, DeploymentID: d.ID, EventID: cursorFor(d, state, latest), ResyncRequired: gap, ProgressEvent: ProgressEvent{Schema: 1, Sequence: latest, StateRevision: state.Revision, Kind: "snapshot", ObservedAt: state.UpdatedAt, ProgressView: state.ProgressSnapshot()}}
	}
	if cursor == "" {
		return []ProgressFrame{snapshot("snapshot", false)}, state, nil
	}
	if len(cursor) > 1024 {
		return nil, nil, fmt.Errorf("invalid progress cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid progress cursor")
	}
	var c progressCursor
	if err = json.Unmarshal(b, &c); err != nil || c.Deployment != d.ID || c.RequestDigest != state.RequestDigest {
		return nil, nil, fmt.Errorf("progress cursor belongs to another deployment or episode")
	}
	events, gap, err := state.ProgressReplay(c.Sequence)
	if err != nil {
		return nil, nil, err
	}
	if gap {
		return []ProgressFrame{snapshot("resync-required", true)}, state, nil
	}
	frames := make([]ProgressFrame, 0, len(events))
	for _, event := range events {
		frames = append(frames, ProgressFrame{Type: "update", DeploymentID: d.ID, EventID: cursorFor(d, state, event.Sequence), ProgressEvent: event})
	}
	// A terminal reconnect must still let its caller reconcile the durable result.
	if len(frames) == 0 && state.Stage.Terminal() {
		frames = append(frames, snapshot("snapshot", false))
	}
	return frames, state, nil
}

// ObserveProgress owns only the observer. Closing it never cancels the episode.
// Reads may see writes by a separate supervisor; delivery never holds store locks.
func (s *Store) ObserveProgress(ctx context.Context, id, cursor string, interval time.Duration, stopAtDecision bool, send func(ProgressFrame) error) error {
	if interval < 100*time.Millisecond {
		return fmt.Errorf("observation interval must be at least 100ms")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		frames, state, err := s.ProgressFrames(id, cursor)
		if err != nil {
			return err
		}
		for _, frame := range frames {
			if err = send(frame); err != nil {
				return err
			}
			cursor = frame.EventID
		}
		if state.Stage.Terminal() || (stopAtDecision && state.ProgressSnapshot().RequiresAction) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
