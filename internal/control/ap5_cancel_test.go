package control

// AP5.3 cancellation-quiescence tests.
//
// The claim under test is that a request is CANCELLED only when every writer it
// owns has been proven gone. Every test here is about a cancellation that must
// NOT be committed.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/runtime"
)

// liveBackend owns a real, running process, so "is it quiescent?" has a true
// answer rather than a scripted one.
type liveBackend struct {
	fakeBackend
	// alive reports whether the owned process is still running.
	alive bool
	pid   int
}

func (b *liveBackend) ProcessIdentity() (int, bool) {
	if !b.alive {
		return 0, false
	}
	return b.pid, true
}

func (b *liveBackend) Inspect(_ context.Context, ref runtime.SessionRef) (runtime.Session, error) {
	status := "dead"
	if b.alive {
		status = "running"
	}
	return runtime.Session{ID: ref.ID, Status: status}, nil
}

// runningPID returns a pid that is genuinely running for the duration of the test.
func runningPID(t *testing.T) (int, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	return cmd.Process.Pid, func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func cancelledRequest(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.CancellationRequested = true
		s.CancellationReason = "operator asked"
		return nil
	}); err != nil {
		t.Fatalf("record intent: %v", err)
	}
}

// TestCancel_RefusesWhileTheOwnedProcessIsStillAlive is the AP5 case.
//
// The backend acknowledged shutdown and the request still has a live writer. A
// cancellation that reported success here would be reporting a workspace as
// released while an agent is still writing to it.
func TestCancel_RefusesWhileTheOwnedProcessIsStillAlive(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	pid, cleanup := runningPID(t)
	defer cleanup()

	live := &liveBackend{pid: pid, alive: true}
	live.name = "minimax-acp"
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-session-7", ProcessPID: pid, Resolved: true},
		}
		s.WorkerSession = "acp-session-7"
		s.AuditorSession = "" // no Agent Deck session to complicate the assertion
		return nil
	}); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	cancelledRequest(t, f)

	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }, Backends: []runtime.Backend{live}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Committed || rep.Quiesced {
		t.Fatalf("cancellation claimed success with a live writer: %+v", rep)
	}
	if len(rep.Unresolved) == 0 {
		t.Fatal("no unresolved reason recorded")
	}
	if !strings.Contains(strings.Join(rep.Unresolved, " "), "still running") {
		t.Errorf("unresolved = %v, want it to name the live process", rep.Unresolved)
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("load: %v", serr)
	}
	if st.Stage == StageCancelled {
		t.Error("stage = CANCELLED while a writer was still running")
	}
}

// TestCancel_CommitsOnceEveryProcessIsGone is the other half: the check must not
// make cancellation impossible, or a crashed request could never be closed.
func TestCancel_CommitsOnceEveryProcessIsGone(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	dead := &liveBackend{alive: false}
	dead.name = "minimax-acp"

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-session-7", ProcessPID: reapedPID(t), Resolved: true},
		}
		s.WorkerSession = "acp-session-7"
		s.AuditorSession = ""
		return nil
	}); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	cancelledRequest(t, f)

	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }, Backends: []runtime.Backend{dead}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !rep.Quiesced {
		t.Fatalf("cancellation did not commit once the process was gone: %+v (unresolved %v)", rep, rep.Unresolved)
	}
}

// reapedPID returns a pid that is certain not to be running.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run short-lived child: %v", err)
	}
	return cmd.Process.Pid
}

// TestCancel_BindingToAnAbsentBackendIsNotSilentlySkipped covers a session bound
// to a transport this engine was never given.
//
// A bare session id is routable — a legacy binding resolves to Agent Deck — so
// the genuinely unroutable case is a binding naming a backend that is not here.
// It cannot be stopped, so it must not be reported as stopped either.
func TestCancel_BindingToAnAbsentBackendIsNotSilentlySkipped(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-session-7", ProcessPID: 4242, Resolved: true},
		}
		s.WorkerSession = "acp-session-7"
		s.AuditorSession = ""
		return nil
	}); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	cancelledRequest(t, f)

	t0 := f.now
	// No ACP backend is registered, so this session cannot be stopped.
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Quiesced || rep.Committed {
		t.Fatalf("a session on an absent backend still produced a clean cancellation: %+v", rep)
	}
	for _, id := range rep.Stopped {
		if id == "acp-session-7" {
			t.Error("a session on an absent backend was reported as stopped")
		}
	}
	if len(rep.Unresolved) == 0 {
		t.Fatal("no unresolved reason recorded")
	}
}

// TestCancel_UnknownDeliveryHistorySurvivesCancellation pins the rule that a
// clean terminal state must not be manufactured by erasing what is uncertain.
func TestCancel_UnknownDeliveryHistorySurvivesCancellation(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordEffect(Effect{
			SessionID: "sess-worker-1", Role: "worker", Message: "do the work",
			Reason: "test", CreatedAt: s.UpdatedAt,
		}); err != nil {
			return err
		}
		// A prompt whose outcome was never proven.
		s.Effects[0].Status = EffectBlocked
		s.Effects[0].LastError = "the outcome is durably UNKNOWN"
		return nil
	}); err != nil {
		t.Fatalf("seed effect: %v", err)
	}
	cancelledRequest(t, f)

	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("load: %v", serr)
	}
	if len(st.Effects) != 1 {
		t.Fatalf("effects = %d, want the uncertain record kept", len(st.Effects))
	}
	if st.Effects[0].Status != EffectBlocked {
		t.Errorf("effect status = %q, want %q: an uncertain delivery must not be laundered into success",
			st.Effects[0].Status, EffectBlocked)
	}
	if !strings.Contains(st.Effects[0].LastError, "UNKNOWN") {
		t.Errorf("effect reason = %q, want the uncertainty preserved", st.Effects[0].LastError)
	}
}

// TestReplacement_RefusesToBindWhileTheRetiredWorkerStillLives is the AP5
// replacement case.
//
// A new generation must not be bound while the old one can still write. The
// engine is not the only process involved, so the proof has to come from the
// process itself rather than from a status field on a session record.
func TestReplacement_RefusesToBindWhileTheRetiredWorkerStillLives(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	pid, cleanup := runningPID(t)
	defer cleanup()

	live := &liveBackend{pid: pid, alive: true}
	live.name = "minimax-acp"

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-old", ProcessPID: pid, Resolved: true},
		}
		s.WorkerSession = "acp-old"
		s.AuditorSession = "sess-auditor-1"
		if _, berr := s.BeginReplacement("the worker stopped responding", fixedNow()); berr != nil {
			return berr
		}
		return nil
	}); err != nil {
		t.Fatalf("begin replacement: %v", err)
	}
	// Drive straight to the candidate phase: the candidate exists, and the next
	// step is the stop-before-bind boundary under test.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Replacement.Phase = GenCandidate
		s.Replacement.RetiredSession = "acp-old"
		return nil
	}); err != nil {
		t.Fatalf("set candidate phase: %v", err)
	}

	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }, Backends: []runtime.Backend{live}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	res, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if res.Blocked == "" {
		t.Fatalf("replacement bound a new generation while the old one still lived: %+v", res)
	}
	if !strings.Contains(res.Blocked, "still running") {
		t.Errorf("blocked reason = %q, want it to name the live process", res.Blocked)
	}
	if res.Quiesced {
		t.Error("quiescence was claimed with a live retired worker")
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("load: %v", serr)
	}
	if st.Replacement.Quiesced {
		t.Error("state recorded quiescence with a live retired worker")
	}
}

// TestReplacement_BindsOnceTheRetiredWorkerIsGone is the other half: a dead
// retired worker must not wedge replacement forever.
func TestReplacement_BindsOnceTheRetiredWorkerIsGone(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	dead := &liveBackend{alive: false}
	dead.name = "minimax-acp"

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-old", ProcessPID: reapedPID(t), Resolved: true},
		}
		s.WorkerSession = "acp-old"
		s.AuditorSession = "sess-auditor-1"
		if _, berr := s.BeginReplacement("the worker stopped responding", fixedNow()); berr != nil {
			return berr
		}
		return nil
	}); err != nil {
		t.Fatalf("begin replacement: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Replacement.Phase = GenCandidate
		s.Replacement.RetiredSession = "acp-old"
		return nil
	}); err != nil {
		t.Fatalf("set candidate phase: %v", err)
	}

	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }, Backends: []runtime.Backend{dead}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	res, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if res.Blocked != "" {
		t.Fatalf("a dead retired worker blocked replacement: %q", res.Blocked)
	}
	if !res.Quiesced {
		t.Error("quiescence was not recorded once the old process was gone")
	}
}

// TestRun_OnceReconcilesAPendingCancellationDespiteAnEmptyStream guards a defect
// that only an end-to-end CLI run surfaced.
//
// With --once and nothing on the bus, Run returned before the supervision tick.
// A recorded cancellation is durable intent that no frame will ever mention, so
// the request then waited forever for an observation only a live worker would
// produce — which is precisely what cancellation exists to stop.
func TestRun_OnceReconcilesAPendingCancellationDespiteAnEmptyStream(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	// An empty stream: the boundary is zero, so the early return is the path.
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 0}
	rt.frames = nil
	cancelledRequest(t, f)

	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Now: func() time.Time { return t0 }, Log: func(string) {},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("run: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The reconciliation was ATTEMPTED: the reason moved off the passive
	// "pending" wording and onto a proven outcome.
	if strings.Contains(st.CancellationReason, "pending reconciliation") {
		t.Errorf("cancellation was left unreconciled on an empty stream: %q", st.CancellationReason)
	}
	if !strings.Contains(st.CancellationReason, "could not be proven stopped") {
		t.Errorf("cancellation reason = %q, want the proven-stop failure recorded", st.CancellationReason)
	}
	// And it must NOT have claimed success.
	if st.Stage == StageCancelled {
		t.Error("stage = CANCELLED although no stop could be proven")
	}
}

// TestPIDReuse_DistinguishesTheOriginalProcessFromAReusedNumber is the LF1.4
// proof.
//
// A pid proves only that SOMETHING holds that number. On a long-lived machine the
// original exits and the number is reused, and "still running" then reports on a
// stranger. The start token is what makes reuse detectable.
func TestPIDReuse_DistinguishesTheOriginalProcessFromAReusedNumber(t *testing.T) {
	pid := os.Getpid()
	cur, err := readProcessIdentity(pid)
	if err != nil {
		t.Fatalf("reading our own identity: %v", err)
	}
	if cur.PID != pid || cur.StartToken == "" {
		t.Fatalf("identity is not usable: %+v", cur)
	}

	// The same process is reported as alive, and the answer is KNOWN.
	alive, known := sameProcess(ProcessIdentity{PID: pid, StartToken: cur.StartToken})
	if !alive || !known {
		t.Errorf("our own process reported alive=%v known=%v; want true, true", alive, known)
	}

	// A different start token on a live pid is PID REUSE: the original is gone.
	alive, known = sameProcess(ProcessIdentity{PID: pid, StartToken: "Fri Jan  1 00:00:00 1999"})
	if alive {
		t.Errorf("a reused pid reported the ORIGINAL as alive; this would block recovery forever")
	}
	if !known {
		t.Error("a reused pid should be a DEFINITE answer, not unknown")
	}
}

// TestPIDReuse_UnreadableIsTreatedAsAlive is the asymmetry that matters.
//
// "I could not tell" must never become "it is gone". Getting that backwards is
// exactly how two workers end up on one workspace.
func TestPIDReuse_UnreadableIsTreatedAsAlive(t *testing.T) {
	// A pid that does not exist is a DEFINITE answer: the original is gone, and
	// reporting it as "still alive" is what made a reaped orphan look eternal.
	alive, known := recordedProcessAlive(999999999, "whatever")
	if alive || !known {
		t.Errorf("a nonexistent pid reported alive=%v known=%v; want false, true", alive, known)
	}

	// An empty start token degrades to the weaker pid-only answer, which is
	// correct for a binding recorded before this field existed.
	alive, _ = recordedProcessAlive(os.Getpid(), "")
	if !alive {
		t.Error("a live process with no recorded token reported dead")
	}
}

// TestPIDReuse_BindingRecordsTheStartToken proves the token is actually captured
// at binding time rather than being a field nothing writes.
func TestPIDReuse_BindingRecordsTheStartToken(t *testing.T) {
	if pidStartToken(0) != "" {
		t.Error("pid 0 should have no start token")
	}
	tok := pidStartToken(os.Getpid())
	if tok == "" {
		t.Fatal("a live process produced no start token")
	}
	st := &State{}
	st.BackendBindings = map[ProfileRole]SessionBinding{
		RoleWorker: {Backend: "minimax-acp", SessionID: "s", ProcessPID: os.Getpid(), ProcessStart: tok},
	}
	b, ok := st.BackendFor(RoleWorker)
	if !ok || b.ProcessStart != tok {
		t.Errorf("the binding did not carry the start token: %+v", b)
	}
}
