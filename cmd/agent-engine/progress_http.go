package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/emjin/agent-engine/internal/control"
)

func (api *episodeAPI) serveProgress(w http.ResponseWriter, r *http.Request, id string) {
	if api.observers.Add(1) > 64 {
		api.observers.Add(-1)
		http.Error(w, "too many observers", 429)
		return
	}
	defer api.observers.Add(-1)
	cursor := r.Header.Get("Last-Event-ID")
	if _, _, err := api.store.ProgressFrames(id, cursor); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	write := func(body string) error {
		// Reset per delivery: the ordinary API's 30s deadline must not kill
		// a healthy long-lived stream, while blocked writes remain bounded.
		if err := rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
			return err
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			return err
		}
		return rc.Flush()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := write(": connected\n\n"); err != nil {
		return
	}
	lastHeartbeat := time.Now()
	// One writer owns ResponseWriter. Sampling bounds cross-process latency;
	// heartbeats are comments and are never delivered as agent updates.
	for {
		frames, state, err := api.store.ProgressFrames(id, cursor)
		if err != nil {
			_ = write("event: observation-error\ndata: {\"error\":\"episode observation unavailable\"}\n\n")
			return
		}
		for _, frame := range frames {
			b, _ := json.Marshal(frame)
			if err = write("id: " + frame.EventID + "\nevent: " + frame.Type + "\ndata: " + string(b) + "\n\n"); err != nil {
				return
			}
			cursor = frame.EventID
		}
		if state.Stage.Terminal() {
			return
		}
		if time.Since(lastHeartbeat) >= 15*time.Second {
			if err = write(": heartbeat\n\n"); err != nil {
				return
			}
			lastHeartbeat = time.Now()
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// CLI follow uses the identical cursor/replay owner, not a second progress rule.
func observeCLI(st *control.Store, ctx context.Context, id, cursor string, interval time.Duration, follow bool, send func(control.ProgressFrame) error) error {
	return st.ObserveProgress(ctx, id, cursor, interval, !follow, send)
}
