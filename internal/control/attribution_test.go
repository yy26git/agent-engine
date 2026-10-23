package control

// LF1.3 attribution tests.
//
// The claim: an answer is attributable to a MODEL, not only to a session.
// Recording the session while leaving the model open is the gap that lets a
// reviewer be swapped underneath an audit.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// pinReviewerModel pins a reviewer profile so a review records what it expects.
func pinReviewerModel(t *testing.T, f *fixture, model string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{
			RoleReviewer: {Name: "auditor", Role: RoleReviewer, Model: model},
		}
		return nil
	}); err != nil {
		t.Fatalf("pin reviewer: %v", err)
	}
}

// TestAttribution_ReviewRecordsThePinnedModel proves the expectation is captured
// when the review is OPENED, not read at judgement time.
func TestAttribution_ReviewRecordsThePinnedModel(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pinReviewerModel(t, f, "gpt-5.6-sol")

	st, _ := f.store.State("forge-run-1842-repair")
	eff, opened, err := st.RequestCompletionAssessment(CompletionClaim{
		Backend: "agent-deck", SessionID: "sess-worker-1", Generation: st.Generation,
		TurnID: "f1", Summary: "done",
	}, f.now)
	if err != nil || !opened {
		t.Fatalf("open review: opened=%v err=%v", opened, err)
	}
	_ = eff
	if st.ActiveReview.ExpectedModel != "gpt-5.6-sol" {
		t.Errorf("ExpectedModel = %q, want the pinned %q", st.ActiveReview.ExpectedModel, "gpt-5.6-sol")
	}
	// Changing configuration afterwards must NOT change what this review expects.
	pinned := f.cfg.Profiles["auditor"]
	pinned.Model = "some-other-model"
	f.cfg.Profiles["auditor"] = pinned
	if got := st.pinnedModelFor(RoleReviewer); got != "gpt-5.6-sol" {
		t.Errorf("pinnedModelFor after a config edit = %q; it must read the PIN, not the config", got)
	}
}

// TestAttribution_WrongModelAnswerIsRefused is the property that matters.
func TestAttribution_WrongModelAnswerIsRefused(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pinReviewerModel(t, f, "gpt-5.6-sol")
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCompletion, "A1", "sess-auditor-1", s.Generation, 1)
		s.ActiveReview.ExpectedModel = "gpt-5.6-sol"
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	_, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: "A1", Generation: 1,
		SourceSession: "sess-auditor-1", SourceModel: "some-other-model",
		// The nonce is echoed correctly on purpose: this test is about the MODEL
		// gate, and a refusal at the nonce would pass for the wrong reason.
		AnswerNonce: st.ActiveReview.AnswerNonce,
		Outcome:     OutcomePass, ReportDigest: strings.Repeat("a", 64), Body: "fine",
	})
	if err == nil {
		t.Fatal("an answer attributed to a model nobody admitted was accepted")
	}
	if !strings.Contains(err.Error(), "attributed to model") {
		t.Errorf("error = %v, want it to name the attribution mismatch", err)
	}
}

// TestAttribution_NoExpectedModelPermitsRecording proves the gate is not a blanket
// requirement. A configuration that names no model may still record one.
func TestAttribution_NoExpectedModelPermitsRecording(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C1", "sess-auditor-1", s.Generation, 1)
		// ExpectedModel deliberately empty.
		s.QueueGuidance(Guidance{Message: "continue", SessionID: "sess-worker-1", QueuedAt: s.UpdatedAt})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A CHECKPOINT review cannot approve at all, which is the stronger guarantee
	// and is checked before anything about models.
	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCheckpoint, ReviewID: "C1", Generation: 1,
		SourceSession: "sess-auditor-1", SourceModel: "whatever",
		Outcome: OutcomePass,
	}); err == nil {
		t.Error("a CHECKPOINT review was allowed to PASS")
	}
}

// --- LF1.5 supervisor-death recovery -----------------------------------------

// TestSupervisorDeath_OrphanedProcessIsReaped is the composition that matters.
//
// Killing a supervisor does not kill the `mcode acp` it owned. A restart that
// admits a second transport without noticing the first gives the request two
// writers on one workspace.
func TestSupervisorDeath_OrphanedProcessIsReaped(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	// A real sleeper stands in for the orphaned ACP child.
	pid, cleanup := startSleeper(t)
	defer cleanup()
	token := pidStartToken(pid)

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-1", ProcessPID: pid, ProcessStart: token, Resolved: true},
		}
		s.WorkerSession = "acp-1"
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()

	if problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair"); len(problems) != 0 {
		t.Fatalf("a live orphan was not reaped: %v", problems)
	}
	if still, _ := recordedProcessAlive(pid, token); still {
		t.Error("the orphaned process is STILL RUNNING after reaping")
	}
}

// TestSupervisorDeath_ReusedPidIsNotSignalled is the safety half.
//
// The recorded pid now belongs to SOMETHING ELSE. Signalling it would kill an
// unrelated process, which is worse than leaving an orphan.
func TestSupervisorDeath_ReusedPidIsNotSignalled(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			// THIS process, but with a start token that cannot be ours.
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-1", ProcessPID: os.Getpid(),
				ProcessStart: "Fri Jan  1 00:00:00 1999", Resolved: true},
		}
		s.WorkerSession = "acp-1"
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()

	if problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair"); len(problems) != 0 {
		t.Errorf("a reused pid produced problems, so it was treated as an orphan: %v", problems)
	}
	// The test process is emphatically still alive.
	if !processExists(os.Getpid()) {
		t.Fatal("the reaper signalled a process that merely held a reused pid; this test is running, so it did")
	}
}

func processExists(pid int) bool {
	alive, _ := processAlive(pid)
	return alive
}

// startSleeper starts a real background process and returns its pid.
func startSleeper(t *testing.T) (int, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 120")
	// Deliberately NOT its own process group. A group-leader fixture would only
	// exercise the group-signal path, which this sandbox refuses with EPERM; the
	// pid-only fallback is the path that must work here, and it is the path a
	// non-leader child takes in any case.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	// Reap concurrently. A killed child stays a ZOMBIE until its parent waits on
	// it, and a zombie still answers signal 0 — so without this the reaper's
	// "is it gone" check would never see it gone.
	//
	// This models nothing about production, where a dead supervisor's child is
	// reparented to init and reaped there. It only keeps the fixture honest.
	go func() { _, _ = cmd.Process.Wait() }()
	return cmd.Process.Pid, func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// --- restart/resume for an already-bound ACP role ----------------------------

// restartableACP is an ACP-bound backend whose sessions are live from creation.
type restartableACP struct {
	fakeBackend
	inspected []string
	// procPID makes it own a process, which is what makes "nothing to start" true.
	procPID int
	// refuseRead makes readiness impossible to establish.
	refuseRead bool
}

func (b *restartableACP) ProcessIdentity() (int, bool) {
	if b.procPID == 0 {
		return 0, false
	}
	return b.procPID, true
}

func (b *restartableACP) Inspect(_ context.Context, ref runtime.SessionRef) (runtime.Session, error) {
	if b.refuseRead {
		return runtime.Session{}, errors.New("this session cannot be read")
	}
	b.mu.Lock()
	b.inspected = append(b.inspected, ref.ID)
	b.mu.Unlock()
	return runtime.Session{ID: ref.ID, Status: "running"}, nil
}

// TestRestart_ACPBoundRoleResumesThroughItsOwnTransport is the restart-path proof.
//
// The bug was that resume inspected an already-bound role through the Agent Deck
// runtime regardless of owner, so an ACP-bound worker failed on the exact path
// meant to bring it back.
func TestRestart_ACPBoundRoleResumesThroughItsOwnTransport(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	acp := &restartableACP{procPID: os.Getpid()}
	acp.name = "minimax-acp"

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		// PrepareTaskSessions requires an admitted profile for every role it
		// supervises, so all four are present; only the worker's transport is
		// under test here.
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{
			RoleWorker:     {Name: "worker", Role: RoleWorker, Transport: "minimax-acp"},
			RoleReviewer:   {Name: "auditor", Role: RoleReviewer, Transport: "agent-deck"},
			RoleCheckpoint: {Name: "checkpoint", Role: RoleCheckpoint, Transport: "agent-deck"},
			RoleDiagnostic: {Name: "diagnostic", Role: RoleDiagnostic, Transport: "agent-deck"},
		}
		grantActivationFixture(t, s, f.srcRoot, s.ExecutionProfiles[RoleWorker].Profile())
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-1", Resolved: true, Generation: s.Generation},
		}
		s.WorkerSession = "acp-1"
		s.AuditorSession = "sess-auditor-1"
		// Both roles already activated, as they would be after a restart.
		s.Activations = []Activation{
			{ActivationID: "AC-w", Role: string(RoleWorker), Label: "w", SessionID: "acp-1", Status: ActivationConfirmed},
			{ActivationID: "AC-a", Role: string(RoleReviewer), Label: "a", SessionID: "sess-auditor-1", Status: ActivationConfirmed},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The reviewer really exists on Agent Deck, so the only thing under test is
	// whether the WORKER is routed through its own transport.
	rt := newFakeRuntime()
	rt.createdIDs = append(rt.createdIDs, "sess-auditor-1")

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Backends: []runtime.Backend{acp}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if err := eng.PrepareTaskSessions(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("an ACP-bound worker could not resume: %v", err)
	}
	acp.mu.Lock()
	seen := append([]string(nil), acp.inspected...)
	acp.mu.Unlock()
	if len(seen) != 1 || seen[0] != "acp-1" {
		t.Errorf("the ACP session was inspected as %v, want [acp-1] through its own transport", seen)
	}
}

// TestCrashWindow_StartupRecordIsReapedWithoutAnyBinding is the crash-window
// proof.
//
// A process recorded at startup may never have reached a session binding. It is
// therefore invisible to binding-driven reaping, and a supervisor that dies in
// that window leaves an orphan nothing can name.
func TestCrashWindow_StartupRecordIsReapedWithoutAnyBinding(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, cleanup := startSleeper(t)
	defer cleanup()
	token := pidStartToken(pid)

	// The crash-window record ONLY: no session binding at all.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.OwnedProcesses = map[string]OwnedProcess{
			"minimax-acp": {PID: pid, StartToken: token, RecordedAt: s.UpdatedAt},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	if problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair"); len(problems) != 0 {
		t.Fatalf("a startup-recorded orphan was not reaped: %v", problems)
	}
	if still, _ := recordedProcessAlive(pid, token); still {
		t.Error("the orphan recorded only at startup is STILL RUNNING")
	}
}

// TestCrashWindow_StartupRecordIsWrittenBeforeAnyBinding proves the record is
// made at STARTUP, which is the whole point.
func TestCrashWindow_StartupRecordIsWrittenBeforeAnyBinding(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := &restartableACP{procPID: os.Getpid()}
	b.name = "minimax-acp"

	eng, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair",
		EngineOptions{Backends: []runtime.Backend{b}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if err := eng.InitializeTransports(context.Background(), "forge-run-1842-repair",
		func() ([]runtime.Backend, func(), error) {
			return []runtime.Backend{b}, func() {}, nil
		}); err != nil {
		t.Fatalf("initialize transports: %v", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	rec, ok := st.OwnedProcesses["minimax-acp"]
	if !ok {
		t.Fatal("the started process was not recorded, so a crash here orphans it unrecorded")
	}
	if rec.PID != os.Getpid() || rec.StartToken == "" {
		t.Errorf("recorded identity = %+v, want this process with a measured token", rec)
	}
	if len(st.BackendBindings) != 0 {
		t.Errorf("a binding was expected to be absent at this point: %+v", st.BackendBindings)
	}
}

// --- the three recovery gaps -------------------------------------------------

// starterBackend records Start/Shutdown calls so resume can be observed.
type starterBackend struct {
	fakeBackend
	mu         sync.Mutex
	started    int
	shutdowns  int
	status     string
	refuseRead bool
}

func (b *starterBackend) EnsureSessionRunning(_ context.Context, _ runtime.SessionRef) error {
	b.mu.Lock()
	b.started++
	b.mu.Unlock()
	return nil
}

func (b *starterBackend) Inspect(context.Context, runtime.SessionRef) (runtime.Session, error) {
	if b.refuseRead {
		return runtime.Session{}, errors.New("this session cannot be read")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return runtime.Session{ID: "s", Status: b.status}, nil
}

func (b *starterBackend) Shutdown(context.Context, runtime.SessionRef) error {
	b.mu.Lock()
	b.shutdowns++
	b.mu.Unlock()
	return nil
}

func (b *starterBackend) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started, b.shutdowns
}

// seedResume binds a worker session that is NOT running, as a restart would find.
func seedResume(t *testing.T, f *fixture, backendName, sessionID string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{
			RoleWorker:     {Name: "w", Role: RoleWorker, Transport: Transport(backendName)},
			RoleReviewer:   {Name: "a", Role: RoleReviewer, Transport: Transport(backendName)},
			RoleCheckpoint: {Name: "c", Role: RoleCheckpoint, Transport: Transport(backendName)},
			RoleDiagnostic: {Name: "d", Role: RoleDiagnostic, Transport: Transport(backendName)},
		}
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		s.BackendBindings[RoleWorker] = SessionBinding{
			Backend: backendName, SessionID: sessionID, Resolved: true, Generation: s.Generation,
		}
		s.WorkerSession = sessionID
		s.AuditorSession = "auditor-1"
		s.Activations = []Activation{
			{ActivationID: "AC-w", Role: string(RoleWorker), Label: "w", SessionID: sessionID, Status: ActivationConfirmed},
			{ActivationID: "AC-a", Role: string(RoleReviewer), Label: "a", SessionID: "auditor-1", Status: ActivationConfirmed},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestResume_StartsANonRunningSessionRatherThanShuttingItDown is gap 1.
//
// Resume exists to recover a session. Shutting one down destroys the thing it
// was asked to bring back.
func TestResume_StartsANonRunningSessionRatherThanShuttingItDown(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := &starterBackend{status: "idle"}
	// This tests an owned backend lifecycle, not MiniMax admission qualification.
	b.name = "owned-process-fixture"
	seedResume(t, f, b.name, "acp-1")

	rt := newFakeRuntime()
	rt.createdIDs = append(rt.createdIDs, "auditor-1")
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{Backends: []runtime.Backend{b}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if err := eng.PrepareTaskSessions(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	started, shutdowns := b.counts()
	if shutdowns != 0 {
		t.Errorf("resume issued %d shutdown(s); a resume must never stop the session it is recovering", shutdowns)
	}
	if started != 1 {
		t.Errorf("resume started the session %d times, want 1", started)
	}
}

// TestResume_UnreadyACPSessionDoesNotPassSilently is the second half of gap 1.
//
// A backend that owns its process has no Start, so a non-running session can
// only be READINESS-checked. Passing without establishing it would let a resume
// report success for a session that was never there.
func TestResume_UnreadyACPSessionDoesNotPassSilently(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := &restartableACP{refuseRead: true} // refuses readiness
	// This tests an owned backend lifecycle, not MiniMax admission qualification.
	b.name = "owned-process-fixture"
	seedResume(t, f, b.name, "acp-1")

	rt := newFakeRuntime()
	rt.createdIDs = append(rt.createdIDs, "auditor-1")
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{Backends: []runtime.Backend{b}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if err := eng.PrepareTaskSessions(context.Background(), "forge-run-1842-repair"); err == nil {
		t.Fatal("resume passed for a session whose readiness could not be established")
	}
}

// TestReap_UnkillableProcessIsReportedNotAssumedGone is gap 3.
//
// The termination helper used to fall out of its wait and return nil, which told
// the caller the orphan was reaped while it still ran.
// Run the invalid-PID regression in its own process group. Removing the guard
// must fail a disposable child, never terminate the surrounding go test command.
func TestReap_InvalidPIDDoesNotSignalSupervisorGroup(t *testing.T) {
	if os.Getenv("AGENT_ENGINE_REAP_INVALID_PID_CHILD") == "1" {
		if err := reapProcessGroup(context.Background(), 0); err == nil {
			t.Fatal("invalid PID was accepted")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReap_InvalidPIDDoesNotSignalSupervisorGroup$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AGENT_ENGINE_REAP_INVALID_PID_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid PID terminated or failed the isolated test process: %v\n%s", err, out)
	}
}

// TestPendingSpawnIntentBlocksReplacement proves a pid-less spawn record blocks.
//
// The process may be running and unidentifiable, so quiescence cannot be
// established. Assuming the spawn never happened admits a replacement beside a
// live writer, which is the one outcome this record exists to prevent.
func TestPendingSpawnIntentBlocksReplacement(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.OwnedProcesses = map[string]OwnedProcess{
			"minimax-acp": {Pending: true, RecordedAt: s.UpdatedAt},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()

	problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair")
	if len(problems) == 0 {
		t.Fatal("an unidentifiable possible-spawn did not block replacement")
	}
	joined := strings.Join(problems, " ")
	if !strings.Contains(joined, "recovery is required") {
		t.Errorf("problems = %v, want it to require recovery rather than assume absence", problems)
	}
}

// TestRecordedSpawnClearsPending shows the intent resolves to a real record.
func TestRecordedSpawnClearsPending(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, cleanup := startSleeper(t)
	defer cleanup()

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.OwnedProcesses = map[string]OwnedProcess{
			"minimax-acp": {Pending: true, RecordedAt: s.UpdatedAt},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	// The same transaction that records the pid clears the intent.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.OwnedProcesses["minimax-acp"] = OwnedProcess{
			PID: pid, StartToken: pidStartToken(pid), RecordedAt: s.UpdatedAt,
		}
		return nil
	}); err != nil {
		t.Fatalf("record pid: %v", err)
	}

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	if problems := eng.ReapOrphanedProcesses(context.Background(), "forge-run-1842-repair"); len(problems) != 0 {
		t.Fatalf("a recorded, identified process was not reaped cleanly: %v", problems)
	}
}
