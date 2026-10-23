package control

// Disposable LIVE pilot. Gated; it is not part of the ordinary suite.
//
// It exists because every other ACP test in this repository runs against a
// scripted server, and a scripted server cannot tell you whether the real
// transport produces a receipt this control layer can actually route. This one
// drives the real `mcode acp` process end to end:
//
//	real prompt -> real receipt -> journal -> assessment candidate -> audit owed
//
// It is deliberately the SMALLEST thing that proves the chain. One short prompt,
// one disposable workspace, no checks that could spend anything, and no attempt
// to approve anything.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/minimaxacp"
	"github.com/emjin/agent-engine/internal/runtime"
)

func liveLauncher(t *testing.T) (string, string) {
	t.Helper()
	p := os.Getenv("AGENT_ENGINE_MCODE_LAUNCHER")
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home directory to locate the pinned launcher")
		}
		p = filepath.Join(home, ".minimax-code/releases/0.6.2/.mcode-launcher")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("the pinned launcher is not present at %s", p)
	}
	sum, err := runtime.FileSHA256(p)
	if err != nil {
		t.Skipf("the launcher cannot be measured: %v", err)
	}
	return p, sum
}

// TestLivePilot_MiniMaxTurnRoutesToAnAudit is the live proof.
//
// The worker never says "done" and never requests a review. It simply answers a
// prompt. That receipt must still become a durable assessment candidate and an
// independent audit owed — against the REAL transport.
func TestLivePilot_MiniMaxTurnRoutesToAnAudit(t *testing.T) {
	if os.Getenv("AGENT_ENGINE_LIVE_PILOT") != "1" {
		t.Skip("set AGENT_ENGINE_LIVE_PILOT=1 to run the disposable live pilot")
	}
	launcher, sum := liveLauncher(t)

	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	conn, err := minimaxacp.Start(context.Background(), minimaxacp.Options{
		Launcher: launcher, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 120 * time.Second, ShutdownGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("the live ACP transport could not be started: %v", err)
	}
	defer func() { _, _ = conn.Stop(context.Background()) }()

	ref, err := conn.Activate(context.Background(), runtime.ActivateSpec{Label: "pilot"})
	if err != nil {
		t.Fatalf("live activate: %v", err)
	}
	t.Logf("live session = %s", ref.ID)

	// Drain the receipt stream for the whole pilot.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receipts := conn.Receipts()
	go func() {
		for ev := range conn.Updates() {
			_ = ev // notifications are not under test here
		}
	}()

	// ONE minimal prompt. This is the only paid operation in the repository.
	rec, err := conn.Submit(ctx, ref, "Reply with exactly: OK")
	if err != nil {
		t.Fatalf("live submit: %v", err)
	}
	t.Logf("live receipt: send_id=%s stop_reason=%q", rec.SendID, rec.State)

	// The receipt must also reach the OBSERVATION stream the supervisor follows.
	seen := make(chan runtime.Event, 8)
	go func() {
		for ev := range receipts {
			select {
			case seen <- ev:
			default:
			}
		}
	}()
	select {
	case ev := <-seen:
		if ev.Kind != runtime.KindPromptResult {
			t.Fatalf("observed kind = %q, want %q", ev.Kind, runtime.KindPromptResult)
		}
		t.Logf("observed receipt for session %s", ev.SessionID)
	case <-time.After(5 * time.Second):
		t.Fatal("the live completion never reached the observation stream")
	}

	// And it must become an audit owed, through the real production path.
	acpBoundRequest(t, f, ref.ID)
	eng, stop := engineWithClock(t, f, rt, &shiftingClock{at: f.now}, conn)
	defer stop()
	eng.applyPromptReceipt(ctx, "forge-run-1842-repair", conn, runtime.Event{
		Backend: minimaxacp.BackendName, Kind: runtime.KindPromptResult, SessionID: ref.ID,
		Payload: []byte(`{"sessionId":"` + ref.ID + `","sendId":"` + rec.SendID +
			`","stopReason":"` + rec.State + `"}`),
	})

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rev := completionReview(st)
	if rev == nil || rev.Kind != ReviewCompletion {
		t.Fatalf("a real MiniMax turn produced no completion review: %+v", rev)
	}
	if rev.ExpectedReviewer != "sess-auditor-1" {
		t.Errorf("audit addressed to %q, want the independently bound reviewer", rev.ExpectedReviewer)
	}
	if rev.AnswerNonce == "" {
		t.Error("the live audit carries no correlation nonce")
	}
	if st.ResultStatus != "" {
		t.Errorf("a live turn produced a result status %q; an audit is not an approval", st.ResultStatus)
	}
	if !strings.Contains(strings.Join(effectMessages(st), " "), rev.ReviewID) {
		t.Error("the audit instruction does not name the review it is answering")
	}
	t.Logf("live audit owed: %s (nonce %s…)", rev.ReviewID, rev.AnswerNonce[:12])
}

// TestLivePilot_CancellationProvesOwnedProcessesGone is the second live proof.
//
// A real ACP child process is started and then cancelled. The claim is that the
// process is actually gone afterwards — not merely that a stop was requested.
func TestLivePilot_CancellationProvesOwnedProcessesGone(t *testing.T) {
	if os.Getenv("AGENT_ENGINE_LIVE_PILOT") != "1" {
		t.Skip("set AGENT_ENGINE_LIVE_PILOT=1 to run the disposable live pilot")
	}
	launcher, sum := liveLauncher(t)

	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	conn, err := minimaxacp.Start(context.Background(), minimaxacp.Options{
		Launcher: launcher, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 60 * time.Second, ShutdownGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("live ACP transport could not be started: %v", err)
	}
	pid, owned := conn.ProcessIdentity()
	if !owned {
		t.Fatal("the live connection owns no process, so there is nothing to prove")
	}
	alive, aerr := processAlive(pid)
	if aerr != nil || !alive {
		t.Fatalf("the owned process %d is not running before cancellation (alive=%v err=%v)", pid, alive, aerr)
	}
	t.Logf("owned process before cancellation: pid=%d", pid)

	liveRef, err := conn.Activate(context.Background(), runtime.ActivateSpec{Label: "pilot-cancel"})
	if err != nil {
		t.Fatalf("live activate: %v", err)
	}
	t.Logf("live session = %s", liveRef.ID)
	// Bind the worker to a REAL ACP session id carrying the REAL pid.
	//
	// It is deliberately distinct from the reviewer: binding the implementer to
	// the reviewer's session is refused by the isolation check, and it should be.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		s.BackendBindings[RoleWorker] = SessionBinding{
			Backend: minimaxacp.BackendName, SessionID: liveRef.ID,
			ProcessPID: pid, Generation: s.Generation, Resolved: true,
		}
		s.WorkerSession = liveRef.ID
		return nil
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	cancelledRequest(t, f)

	eng, stop := engineWithClock(t, f, rt, &shiftingClock{at: f.now}, conn)
	defer stop()

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile cancellation: %v", err)
	}
	// Give the OS a moment to reap what was signalled.
	deadline := time.Now().Add(5 * time.Second)
	var stillAlive bool
	for time.Now().Before(deadline) {
		stillAlive, aerr = processAlive(pid)
		if aerr != nil || !stillAlive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if stillAlive {
		t.Errorf("owned process %d is STILL RUNNING after cancellation; report: %+v", pid, rep)
	}
	if rep.Quiesced {
		t.Logf("cancellation proved quiescent: %v", rep.Stopped)
	} else {
		t.Logf("cancellation did NOT claim quiescence: %v", rep.Unresolved)
	}
	t.Logf("owned process after cancellation: pid=%d alive=%v", pid, stillAlive)
}

func effectMessages(st *State) []string {
	var out []string
	for _, e := range st.Effects {
		out = append(out, e.Message)
	}
	return out
}
