package control

// Termination tests.
//
// The claim under test is narrow: ONE cleanup path, reached by every termination,
// bounded, honest about failure, safe against pid reuse, and able to tell a
// finished session from a vanished one. Each test isolates exactly one way that
// claim fails — a fifth teardown body, a cleanup that reports success over a live
// writer, a pid reused by a stranger, a restart that replays instead of blocking,
// and two different facts collapsed into one "not open".

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// terminationBackend is a process-owning transport with scriptable teardown.
//
// It exists to make the three outcomes a teardown can have reachable without
// launching a real agent: it can refuse to stop, it can report a survivor it
// could not reap, and it can tell a closed session from a stale one. A fixture
// that could only ever succeed proves nothing about a failure path.
type terminationBackend struct {
	fakeBackend
	// liveness maps a session id to one of "active", "closed", "stale".
	liveness map[string]string
	// stopErr is returned by StopOwnedProcesses, modelling a transport that
	// could not release its process.
	stopErr error
	// unreaped names processes the transport could not prove gone.
	unreaped []int
	// pid is the owned process, when it owns one.
	pid int
	// stops counts how many times the transport was asked to stop.
	stops int
}

func newTerminationBackend() *terminationBackend {
	b := &terminationBackend{liveness: map[string]string{}}
	b.name = "minimax-acp"
	return b
}

func (b *terminationBackend) ProcessIdentity() (int, bool) { return b.pid, b.pid != 0 }

func (b *terminationBackend) StopOwnedProcesses(context.Context) error {
	b.stops++
	return b.stopErr
}

func (b *terminationBackend) UnreapedProcesses() []int { return b.unreaped }

// SessionLiveness reports what the transport knows about a session, so the
// control layer never has to infer "gone" from an error.
func (b *terminationBackend) SessionLiveness(ref runtime.SessionRef) string {
	if v, ok := b.liveness[ref.ID]; ok {
		return v
	}
	return "closed"
}

// bindWorker points the request's worker at this transport and clears the other
// roles, so a test asserts about one writer and nothing else.
func bindWorker(t *testing.T, f *fixture, sessionID string, pid int, start string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: sessionID,
				ProcessPID: pid, ProcessStart: start, Resolved: true},
		}
		s.WorkerSession = sessionID
		s.AuditorSession = ""
		s.CheckpointSession = ""
		s.DiagnosticSession = ""
		return nil
	}); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
}

func terminationEngine(t *testing.T, f *fixture, backends ...runtime.Backend) *Engine {
	t.Helper()
	t0 := f.now
	eng, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair",
		EngineOptions{Once: true, Now: func() time.Time { return t0 }, Backends: backends})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(eng.Close)
	return eng
}

type episodeTerminationBackend struct {
	*terminationBackend
	shutdown []string
	survives bool
}

func (b *episodeTerminationBackend) Shutdown(_ context.Context, ref runtime.SessionRef) error {
	b.shutdown = append(b.shutdown, ref.ID)
	if !b.survives {
		b.liveness[ref.ID] = "closed"
	}
	return nil
}

func TestTermination_CancellationIncludesEpisodeOnlyBindings(t *testing.T) {
	for _, survives := range []bool{false, true} {
		t.Run(strconv.FormatBool(survives), func(t *testing.T) {
			f := boundRequest(t, newFixture(t))
			bindWorker(t, f, "investigator", 0, "")
			if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
				binding := s.BackendBindings[RoleWorker]
				s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: binding}
				s.WorkerSession = ""
				s.CancellationRequested = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			b := &episodeTerminationBackend{terminationBackend: newTerminationBackend(), survives: survives}
			b.liveness["investigator"] = "active"
			eng := terminationEngine(t, f, b)
			rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
			if err != nil {
				t.Fatal(err)
			}
			if len(b.shutdown) != 1 || b.shutdown[0] != "investigator" {
				t.Fatalf("episode investigator not stopped: %v", b.shutdown)
			}
			if rep.Committed == survives || rep.Quiesced == survives {
				t.Fatalf("cancellation incorrectly classified surviving=%v: %+v", survives, rep)
			}
		})
	}
}

// TestTermination_AllFourKindsReachTheOnePath asserts success, failure, timeout
// and cancellation all release what the request owns.
//
// A teardown reached by only some of the four is the defect this requirement
// names: the terminations that are rare are exactly the ones that leave a worker
// behind, and they leave it behind because nobody thought to stop it.
//
// The SCOPE differs by kind and that difference is asserted, not incidental. All
// four terminate the owned PROCESS, because that is the residue all four leave.
// Only cancellation also stops SESSIONS: AP5 requires a cancelled request to stop
// every Codex role session, and on Agent Deck those sessions have no process of
// ours to kill, so an explicit stop is the only thing that releases them.
func TestTermination_AllFourKindsReachTheOnePath(t *testing.T) {
	for _, kind := range []TerminationKind{
		TerminationSuccess, TerminationFailure, TerminationTimeout, TerminationCancelled,
	} {
		t.Run(string(kind), func(t *testing.T) {
			f := boundRequest(t, newFixture(t))
			pid := reapedPID(t)
			b := newTerminationBackend()
			b.pid = pid
			b.liveness["acp-w"] = "closed"
			bindWorker(t, f, "acp-w", pid, "")
			eng := terminationEngine(t, f, b)

			rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", kind)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if rep.Kind != kind {
				t.Errorf("kind = %q, want %q", rep.Kind, kind)
			}
			if !rep.Quiesced {
				t.Fatalf("%s did not prove quiescence: %+v", kind, rep)
			}
			// The process is released on every kind, and its release is named in
			// the report rather than inferred from a session having gone quiet.
			if !containsSubstring(rep.Stopped, "minimax-acp process") {
				t.Errorf("stopped = %v, want the owned process named", rep.Stopped)
			}
			// Sessions are stopped only where the scope permits it.
			stoppedSession := containsString(rep.Stopped, "acp-w")
			if wantSession := kind == TerminationCancelled; stoppedSession != wantSession {
				t.Errorf("session stopped = %v for %s, want %v (stopped=%v)",
					stoppedSession, kind, wantSession, rep.Stopped)
			}
			// The transport was actually asked, and the verdict is DURABLE. A
			// teardown that only exists in a return value is invisible to the next
			// operator, and a request that reached VERIFIED looks identical whether
			// or not its worker was ever stopped.
			if b.stops != 1 {
				t.Errorf("owned processes were stopped %d times, want 1", b.stops)
			}
			st, serr := f.store.State("forge-run-1842-repair")
			if serr != nil {
				t.Fatalf("load: %v", serr)
			}
			found := false
			for _, h := range st.History {
				if h.Event == "cleanup/"+string(kind) {
					found = true
				}
			}
			if !found {
				t.Errorf("no durable cleanup record for %s; history = %+v", kind, st.History)
			}
		})
	}
}

// TestTermination_CleanupFailureIsReportedNotSuccess is requirement 3.
//
// A transport that could not release its process must leave the request BLOCKED
// and say which process. Reporting a failed teardown as clean is worse than
// crashing: the operator then believes the workspace is free.
func TestTermination_CleanupFailureIsReportedNotSuccess(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	b.stopErr = errors.New("the peer refused to release its process group")
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", TerminationFailure)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Clean() {
		t.Fatalf("a teardown that could not stop the process reported clean: %+v", rep)
	}
	if rep.Outcome != TerminationBlocked {
		t.Errorf("outcome = %q, want %q", rep.Outcome, TerminationBlocked)
	}
	if !strings.Contains(strings.Join(rep.Unresolved, " "), "refused to release") {
		t.Errorf("unresolved = %v, want the transport's refusal named", rep.Unresolved)
	}
	// And the failure is RECORDED, not merely returned.
	st, _ := f.store.State("forge-run-1842-repair")
	note := ""
	for _, h := range st.History {
		if strings.HasPrefix(h.Event, "cleanup/") {
			note = h.Note
		}
	}
	if !strings.Contains(note, "did NOT prove quiescence") {
		t.Errorf("durable cleanup note = %q, want it to state the failure", note)
	}
}

// TestCancellation_DoesNotCommitWhenCleanupFails asserts the failure reaches the
// one caller that publishes an outcome.
//
// A cancellation that commits CANCELLED over a worker nobody released is the
// specific lie this whole path exists to prevent.
func TestCancellation_DoesNotCommitWhenCleanupFails(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	b.stopErr = errors.New("the process group survived")
	bindWorker(t, f, "acp-w", pid, "")
	cancelledRequest(t, f)
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Committed || rep.Quiesced {
		t.Fatalf("cancellation claimed success despite a failed teardown: %+v", rep)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage == StageCancelled {
		t.Error("the request is CANCELLED although its process was never released")
	}
}

// TestTermination_LiveProcessWithNoStopCapabilityBlocks asserts a transport that
// cannot stop its own process still blocks the request.
//
// The dangerous case is not the one that errors — it is the one that has no way
// to act and therefore looks like nothing to do.
func TestTermination_LiveProcessWithNoStopCapabilityBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, stop := runningPID(t)
	defer stop()

	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", TerminationSuccess)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Clean() {
		t.Fatalf("a live process reported clean: %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.Unresolved, " "), "still running") {
		t.Errorf("unresolved = %v, want it to name the live process", rep.Unresolved)
	}
}

// TestTermination_SurvivedDescendantBlocksQuiescence is requirement 7 at the
// control layer.
//
// A transport that reports a survivor it could not prove gone must not be
// reported clean. The alternative is a cancellation that releases a workspace
// whose tool is still running.
func TestTermination_SurvivedDescendantBlocksQuiescence(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	b.unreaped = []int{5150}
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", TerminationTimeout)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Clean() {
		t.Fatalf("an unreaped descendant reported clean: %+v", rep)
	}
	if !containsInt(rep.Unreaped, 5150) {
		t.Errorf("unreaped = %v, want 5150 named", rep.Unreaped)
	}
	if !strings.Contains(rep.Reason(), "not proven reaped") {
		t.Errorf("reason = %q, want it to say the descendant is unproven", rep.Reason())
	}
}

// TestTermination_ClosedAndStaleSessionsAreDistinguishable is requirement 6.
//
// A session that finished cleanly and one that should be running but is not are
// both "not open" to every other representation. A supervisor that cannot tell
// them apart either discards a finished session or re-dispatches to a dead one.
func TestTermination_ClosedAndStaleSessionsAreDistinguishable(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newTerminationBackend()
	b.liveness["acp-finished"] = "closed"
	b.liveness["acp-vanished"] = "stale"
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker:   {Backend: "minimax-acp", SessionID: "acp-finished", Resolved: true},
			RoleReviewer: {Backend: "minimax-acp", SessionID: "acp-vanished", Resolved: true},
		}
		s.WorkerSession = "acp-finished"
		s.AuditorSession = "acp-vanished"
		return nil
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	eng := terminationEngine(t, f, b)

	// CANCELLATION is the kind whose scope stops sessions, so this is the kind that
	// can distinguish a finished session from a vanished one. Asserting it on a
	// kind that never stops sessions would pass vacuously.
	rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", TerminationCancelled)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !containsString(rep.Stopped, "acp-finished") {
		t.Errorf("stopped = %v, want the closed session", rep.Stopped)
	}
	if !containsString(rep.Stale, "acp-vanished") {
		t.Errorf("stale = %v, want the vanished session reported separately", rep.Stale)
	}
	if containsString(rep.Stopped, "acp-vanished") {
		t.Error("a stale session was reported as cleanly stopped; the two are different facts")
	}
	// Neither is unresolved: nothing is writing. But they must not be conflated.
	if !rep.Quiesced {
		t.Errorf("quiescence = false: a released workspace is still released (%v)", rep.Unresolved)
	}
}

// TestTermination_ActiveSessionAfterStopBlocks asserts a stop that did not take
// is unresolved rather than success.
//
// CANCELLATION is the kind whose scope stops sessions; on a kind that never stops
// them there is no stop to ignore, and this would pass vacuously.
func TestTermination_ActiveSessionAfterStopBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "active" // the transport ignored the stop
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", TerminationCancelled)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Clean() {
		t.Fatalf("a session still running reported clean: %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.Unresolved, " "), "still reports running") {
		t.Errorf("unresolved = %v, want it to name the live session", rep.Unresolved)
	}
}

// TestTermination_ReusedPidIsNotSignalled is requirement 4.
//
// A recorded pid may have been recycled by something unrelated. Signalling it
// would kill a stranger, which is worse than leaving an orphan behind — so the
// start token must be compared before any signal, and a MISMATCH must mean the
// original is provably gone rather than "still running".
func TestTermination_ReusedPidIsNotSignalled(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	// A process that is genuinely running and is NOT the one we recorded.
	pid, stop := runningPID(t)
	defer stop()

	// A start token that cannot belong to this process. It was measured at the
	// moment the ORIGINAL process existed and the pid has since been reused.
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "Mon Jan  1 00:00:00 1990")
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileTermination(context.Background(), "forge-run-1842-repair", TerminationFailure)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The stranger is treated as GONE, which is the correct reading of a
	// mismatched token — and the decisive assertion is below.
	if !containsString(rep.Stopped, "minimax-acp process") && !rep.Clean() {
		t.Fatalf("a reused pid was not recognised as gone: %+v", rep)
	}
	alive, aerr := processAlive(pid)
	if aerr != nil {
		t.Fatalf("probe: %v", aerr)
	}
	if !alive {
		t.Fatal("the mismatched-identity process was killed; it must never be signalled")
	}
}

// TestReap_ReusedPidIsNotKilledOnSupervisorDeath is the same rule on the restart
// path, where signalling actually happens.
func TestReap_ReusedPidIsNotKilledOnSupervisorDeath(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, stop := runningPID(t)
	defer stop()

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.OwnedProcesses = map[string]OwnedProcess{
			// A token that cannot match the live process: the pid was reused.
			"minimax-acp": {PID: pid, StartToken: "Mon Jan  1 00:00:00 1990", RecordedAt: s.UpdatedAt},
		}
		return nil
	}); err != nil {
		t.Fatalf("record owned process: %v", err)
	}
	eng := terminationEngine(t, f)

	problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair")
	if len(problems) != 0 {
		t.Errorf("a reused pid was treated as an orphan: %v", problems)
	}
	alive, aerr := processAlive(pid)
	if aerr != nil {
		t.Fatalf("probe: %v", aerr)
	}
	if !alive {
		t.Fatal("restart signalling killed a process whose identity did not match")
	}
}

// TestReap_UncertainSpawnBlocksRatherThanReplaying is requirement 5.
//
// A spawn whose pid was never recorded may still be running and unidentifiable.
// Quietly retrying, reconnecting or replaying on that evidence produces a second
// worker on one workspace, so the correct outcome is to BLOCK.
func TestReap_UncertainSpawnBlocksRatherThanReplaying(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.OwnedProcesses = map[string]OwnedProcess{
			// The crash window: a spawn was committed, its pid never recorded.
			"minimax-acp": {Pending: true, RecordedAt: s.UpdatedAt},
		}
		return nil
	}); err != nil {
		t.Fatalf("record pending spawn: %v", err)
	}
	eng := terminationEngine(t, f)

	problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair")
	if len(problems) == 0 {
		t.Fatal("an unidentified spawn was reported as reconciled")
	}
	if !strings.Contains(strings.Join(problems, " "), "never identified") {
		t.Errorf("problems = %v, want the unidentified spawn named", problems)
	}
	// And the transport must REFUSE to answer for a label rather than assume the
	// label is free, which is what would let a replacement be admitted beside a
	// writer nobody can name.
	b := newTerminationBackend()
	if _, _, rerr := b.Reconcile(context.Background(), "some-label", false); rerr == nil {
		t.Error("a transport answered 'label absent' while an earlier process is unproven")
	}
	if _, found, _ := b.Reconcile(context.Background(), "some-label", true); found {
		t.Error("reconcile reported a label found that was never activated")
	}
}

// TestTermination_CancelledContextStillReturns asserts cleanup is bounded from
// the caller's side too.
//
// Cancellation is precisely when teardown runs, so inheriting an already-cancelled
// context would mean a cancelled request never releases its worker at all.
func TestTermination_CancelledContextStillReturns(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan TerminationReport, 1)
	go func() {
		rep, _ := eng.ReconcileTermination(ctx, "forge-run-1842-repair", TerminationCancelled)
		done <- rep
	}()
	select {
	case rep := <-done:
		if !containsString(rep.Stopped, "acp-w") {
			t.Errorf("stopped = %v, want the worker released despite a cancelled caller", rep.Stopped)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cleanup never returned; a cancelled context must not wedge the teardown")
	}
}

func containsString(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

// containsSubstring matches a rendered entry. A released process is reported as
// "<backend> process <pid>", so its presence cannot be asserted by exact id
// without hard-coding a pid the test did not choose.
func containsSubstring(hay []string, needle string) bool {
	for _, v := range hay {
		if strings.Contains(v, needle) {
			return true
		}
	}
	return false
}

func containsInt(hay []int, needle int) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
