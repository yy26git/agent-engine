package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/minimaxacp"
	"github.com/emjin/agent-engine/internal/runtime"
)

func TestACPStream_NotificationsAndReceiptsReachAssessmentWithoutFalseGaps(t *testing.T) {
	// Actual stdio Connection, actual journal and actual assessment ingress.
	// This reproduces the live defect without vendor sessions or model charges.
	server := filepath.Join(t.TempDir(), "acp")
	body := `#!/bin/sh
while IFS= read -r line; do
 id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
 case "$line" in
 *'"initialize"'*) result='{"protocolVersion":1,"agentCapabilities":{}}';;
 *'"session/new"'*) result='{"sessionId":"script-worker","configOptions":[]}';;
 *'"session/prompt"'*)
   echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"script-worker","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"working"}}}}'
   echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"script-worker","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" finished"}}}}'
   result='{"stopReason":"end_turn"}';;
 *) result='{}';;
 esac
 printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$result"
done
`
	if err := os.WriteFile(server, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	sum, err := runtime.FileSHA256(server)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := minimaxacp.Start(ctx, minimaxacp.Options{Launcher: server, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: t.TempDir(), CallTimeout: 3 * time.Second, ShutdownGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(context.Background())
	ref, err := c.Activate(ctx, runtime.ActivateSpec{Label: "composition"})
	if err != nil {
		t.Fatal(err)
	}
	f := boundRequest(t, newFixture(t))
	acpBoundRequest(t, f, ref.ID)
	e, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, c)
	defer stop()
	frames, _, err := c.Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	marks := newObservationWatermarks()
	for turn := 0; turn < 2; turn++ {
		sent, err := c.Submit(ctx, ref, "work")
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			select {
			case ev := <-frames:
				if ev.Cursor != uint64(turn*2+n+1) || ev.SessionID != ref.ID {
					t.Fatalf("unattributed/nonsequential notification: %+v", ev)
				}
				e.applyObservation(ctx, "forge-run-1842-repair", c, ev, marks)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		select {
		case ev := <-c.Receipts():
			var body struct {
				SendID string `json:"sendId"`
			}
			if json.Unmarshal(ev.Payload, &body) != nil || body.SendID != sent.SendID || ev.Cursor != 0 {
				t.Fatal("receipt lost its independent identity")
			}
			e.applyObservation(ctx, "forge-run-1842-repair", c, ev, marks)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if e.ingest.HasGap() {
		t.Fatal(e.ingest.GapReason())
	}
	s, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatal(err)
	}
	if completionReview(s) == nil || s.ResultStatus != "" {
		t.Fatal("receipt failed to open an independent assessment, or fabricated completion")
	}
	// Receipts must not hide a real lost notification, including after restart.
	e.applyObservation(ctx, "forge-run-1842-repair", c, runtime.Event{Backend: c.Name(), Kind: "session/update", Cursor: 6}, marks)
	if !e.ingest.HasGap() {
		t.Fatal("real missing notification was ignored")
	}
	replayed, err := e.ingest.Journal().Replay()
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewJournalIngestor(e.ingest.Journal(), nil, nil)
	restarted.Rehydrate(replayed.Records)
	if !restarted.HasGap() {
		t.Fatal("restart erased a genuine gap")
	}
}
