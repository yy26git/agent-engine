package control

// Terminal wiring tests.
//
// The claim under test is that the three terminations which previously bypassed
// cleanup now reach the single reconciliation path, and that reaching it did not
// turn into a new gate on ordinary work. Every test here drives
// `DriveTaskLifecycle` — the real seam `Run` and the task-exec verb both call —
// rather than calling `ReconcileTermination` directly, because a test that calls
// the teardown itself proves only that the teardown works, not that anything
// reaches it.
//
// Nothing here launches a model session or opens a socket: the process-owning
// transport is a fake, and the only real processes are `sleep` children used to
// make a pid genuinely live or genuinely gone.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// driveLifecycle runs the production seam once, which is what a supervisor tick
// and the task-exec verb each do.
func driveLifecycle(t *testing.T, e *Engine, requestID string) error {
	t.Helper()
	return e.DriveTaskLifecycle(context.Background(), requestID)
}

// commitTerminal moves a bound request to a terminal stage the way the real
// commit paths do — inside a store transaction, before any cleanup runs. That
// ordering is the whole point: it reproduces the production window in which a
// terminal request still owns a live process.
func commitTerminal(t *testing.T, f *fixture, stage Stage) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.Stage.Terminal() {
			return errors.New("the fixture request is already terminal")
		}
		s.StageReason = "committed by the production terminal path"
		return s.transition(stage, s.StageReason, f.now.UTC().Format(time.RFC3339))
	}); err != nil {
		t.Fatalf("commit %s: %v", stage, err)
	}
}

// TestTerminalWiring_AllFourTerminationsReachTheOnePath is the central claim.
//
// Success, failure and timeout each commit their terminal stage somewhere
// different — the completion audit, the blocked review, the supervision clock —
// and each used to return without releasing anything. The stage does not matter
// to cleanup: what matters is that the request's residue is released on every
// route out, so a FAILED request never keeps a process alive that a CANCELLED one
// releases.
func TestTerminalWiring_AllFourTerminationsReachTheOnePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage Stage
		kind  TerminationKind
	}{
		{"success", StageVerified, TerminationSuccess},
		{"failure", StageFailed, TerminationFailure},
		{"timeout", StageBlocked, TerminationTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := boundRequest(t, newFixture(t))
			pid := reapedPID(t)
			b := newTerminationBackend()
			b.pid = pid
			b.liveness["acp-w"] = "closed"
			bindWorker(t, f, "acp-w", pid, "")
			eng := terminationEngine(t, f, b)

			commitTerminal(t, f, tc.stage)
			if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err != nil {
				t.Fatalf("drive: %v", err)
			}

			// The transport was actually asked to release the process, and the
			// verdict is durable. A teardown that exists only in a return value is
			// invisible to the next operator, and a request that reached VERIFIED
			// looks identical whether or not its worker was ever stopped.
			if b.stops != 1 {
				t.Errorf("owned processes were stopped %d times, want 1", b.stops)
			}
			st, serr := f.store.State("forge-run-1842-repair")
			if serr != nil {
				t.Fatalf("load: %v", serr)
			}
			found := false
			for _, h := range st.History {
				if h.Event == "cleanup/"+string(tc.kind) {
					found = true
				}
			}
			if !found {
				t.Errorf("no durable cleanup record for %s; history = %+v", tc.kind, st.History)
			}
		})
	}
}

// TestTerminalWiring_CancellationIsNotReconciledTwice asserts the terminal seam
// does not re-run a teardown that already ran.
//
// Cancellation reconciles BEFORE it commits CANCELLED, because a cancelled
// request must prove quiescence before claiming to be cancelled. It then arrives
// at the terminal seam like every other terminal request. If that seam treated
// CANCELLED as new work it would tear down a second time and record a second
// `cleanup/cancellation` the history would present as two teardowns.
func TestTerminalWiring_CancellationIsNotReconciledTwice(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "")
	cancelledRequest(t, f)
	eng := terminationEngine(t, f, b)

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile cancellation: %v", err)
	}
	if !rep.Committed || !rep.Quiesced {
		t.Fatalf("cancellation did not commit cleanly: %+v", rep)
	}
	stoppedAfterCancellation := b.stops

	// The terminal seam now runs, on an already-cancelled request.
	if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err != nil {
		t.Fatalf("drive after cancellation: %v", err)
	}
	if b.stops != stoppedAfterCancellation {
		t.Errorf("the terminal seam tore down again: stops went %d -> %d",
			stoppedAfterCancellation, b.stops)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if got := countCleanupRecords(st, string(TerminationCancelled)); got != 1 {
		t.Errorf("cleanup records for cancellation = %d, want exactly 1; history = %+v", got, st.History)
	}
}

// TestTerminalWiring_ReplacementIsNotReconciledTwice asserts the narrower
// replacement teardown is not repeated as a full one at the terminal seam.
//
// Replacement deliberately does NOT stop the shared process: one ACP process
// carries the worker, the auditor and the checker, so killing it to retire a
// worker would kill the reviewer about to judge the replacement. That narrower
// teardown must stand as its own record, and the terminal seam must not replay it
// as though the request had already been released.
func TestTerminalWiring_ReplacementIsNotReconciledTwice(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	// The replacement teardown, driven directly because this asserts what the
	// terminal seam does with the record it leaves, not the replacement protocol.
	if _, err := eng.teardown(context.Background(), "forge-run-1842-repair", terminationScope{
		kind:            TerminationReplacement,
		sessions:        []string{"acp-w"},
		stopSessions:    true,
		verifyProcesses: true,
	}); err != nil {
		t.Fatalf("replacement teardown: %v", err)
	}
	// Verify-only: the process was checked, not terminated.
	if b.stops != 0 {
		t.Errorf("replacement stopped the shared process %d time(s); it must verify, not terminate", b.stops)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if got := countCleanupRecords(st, string(TerminationReplacement)); got != 1 {
		t.Fatalf("replacement cleanup records = %d, want 1; history = %+v", got, st.History)
	}

	// Now the request finishes successfully. This is a DIFFERENT termination and
	// it legitimately releases the process the replacement left running — but it
	// must not be recorded as a second replacement teardown.
	commitTerminal(t, f, StageVerified)
	if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err != nil {
		t.Fatalf("drive: %v", err)
	}
	final, _ := f.store.State("forge-run-1842-repair")
	if got := countCleanupRecords(final, string(TerminationReplacement)); got != 1 {
		t.Errorf("replacement cleanup records = %d after a later success, want 1; history = %+v",
			got, final.History)
	}
	if b.stops != 1 {
		t.Errorf("the terminal success did not release the process the replacement left running: stops = %d", b.stops)
	}
}

// TestTerminalWiring_UnownedRequestIsUnchanged is requirement 3, and the one
// that decides whether this change is a gate or a cleanup.
//
// A plain Agent Deck request owns no process, so there is nothing to release. It
// must produce the same result it produced before any of this existed: no error,
// no refusal, the same committed outcome. A teardown that turned "nothing owned"
// into "blocked" would stop every ordinary request in the system.
func TestTerminalWiring_UnownedRequestIsUnchanged(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	// No ACP backend at all: this is the ordinary Agent Deck request.
	eng := terminationEngine(t, f)

	before, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	commitTerminal(t, f, StageVerified)
	if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err != nil {
		t.Fatalf("an unowned request was refused by reconciliation: %v", err)
	}

	after, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Same result, and specifically still VERIFIED rather than downgraded.
	if after.Stage != StageVerified {
		t.Errorf("stage = %s, want %s; reconciliation changed a result it had no business changing",
			after.Stage, StageVerified)
	}
	if after.ResultStatus != before.ResultStatus {
		t.Errorf("result status = %q, want it untouched at %q", after.ResultStatus, before.ResultStatus)
	}
	// The one thing it may add is its own durable verdict, which is a record and
	// not a gate: nothing was stopped, so nothing is claimed to have been.
	rep, terr := eng.ReconcileTerminalOutcome(context.Background(), "forge-run-1842-repair")
	if terr != nil {
		t.Fatalf("reconcile: %v", terr)
	}
	if !rep.Quiesced || len(rep.Unresolved) != 0 {
		t.Errorf("an unowned request did not reconcile clean: %+v", rep)
	}
}

// TestTerminalWiring_CleanupFailureIsReportedNotClean is requirement 5.
//
// The request is already terminal and cannot be un-committed, so the only honest
// outcome is to REFUSE to report the run as finished. Returning nil here would
// hand a scheduler exit 0 for a workspace nobody released — the specific lie this
// whole path exists to prevent. The failure is also named, not merely non-nil.
func TestTerminalWiring_CleanupFailureIsReportedNotClean(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	b.stopErr = errors.New("the process group survived")
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	commitTerminal(t, f, StageVerified)
	err := driveLifecycle(t, eng, "forge-run-1842-repair")
	if err == nil {
		t.Fatal("a terminal request whose process was never released reported success")
	}
	if !strings.Contains(err.Error(), "could not be stopped") {
		t.Errorf("error = %q, want the transport's refusal named", err)
	}

	// And it is durable, so the next operator sees it rather than a clean
	// terminal stage with nothing behind it.
	st, _ := f.store.State("forge-run-1842-repair")
	if got := countCleanupRecords(st, string(TerminationSuccess)); got != 1 {
		t.Errorf("cleanup records = %d, want the failed teardown recorded once", got)
	}
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

// TestTerminalWiring_ReportedFailureIsNotTurnedIntoSuccess is the other half of
// requirement 5, and it runs the opposite direction from the test above.
//
// A teardown that reported blocked must not be laundered into a clean outcome by
// a later tick. The request is already committed and cannot be un-committed, so
// the guarantee is that the failure keeps being reported for as long as it is
// true: an operator who retries sees the same refusal rather than a fresh pass.
func TestTerminalWiring_ReportedFailureIsNotTurnedIntoSuccess(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	b.stopErr = errors.New("the process group survived")
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	commitTerminal(t, f, StageVerified)
	first := driveLifecycle(t, eng, "forge-run-1842-repair")
	if first == nil {
		t.Fatal("a teardown that could not release the process reported success")
	}

	// The operator retries. The failure has not gone away, so neither has the
	// report: it must not be swallowed into a clean second attempt.
	second := driveLifecycle(t, eng, "forge-run-1842-repair")
	if second == nil {
		t.Fatal("a retried tick laundered a reported teardown failure into success")
	}
	if !strings.Contains(second.Error(), "could not be stopped") {
		t.Errorf("retry error = %q, want the same refusal reported again", second)
	}
}

// TestTerminalWiring_UncertainOwnershipBlocks is requirement 6.
//
// A session bound to a transport this engine does not hold may still be the thing
// running the process. A teardown that never asked would report the workspace
// free while a writer is live in it, so uncertainty blocks instead of proceeding.
// The counterpart — a request that owns nothing at all — is the no-op asserted
// above; the two must not be confused, because conflating them is exactly how an
// unreleased workspace gets reported as free.
func TestTerminalWiring_UncertainOwnershipBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	// The session claims a transport that was never admitted to this engine.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "acp-w", Resolved: true},
		}
		s.WorkerSession = "acp-w"
		s.AuditorSession = ""
		s.CheckpointSession = ""
		s.DiagnosticSession = ""
		return nil
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// No backend is registered, so ownership of whatever runs this session cannot
	// be established from here.
	eng := terminationEngine(t, f)

	commitTerminal(t, f, StageVerified)
	err := driveLifecycle(t, eng, "forge-run-1842-repair")
	if err == nil {
		t.Fatal("unestablished process ownership reported clean; it must block")
	}
	if !strings.Contains(err.Error(), "unestablished") {
		t.Errorf("error = %q, want it to name the unestablished ownership", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	note := ""
	for _, h := range st.History {
		if strings.HasPrefix(h.Event, "cleanup/") {
			note = h.Note
		}
	}
	if !strings.Contains(note, "did NOT prove quiescence") {
		t.Errorf("durable cleanup note = %q, want the uncertainty recorded", note)
	}
}

// TestTerminalWiring_LiveProcessWithNoStopCapabilityBlocks asserts a transport
// that owns a process but cannot stop it still blocks.
//
// The absence of a capability is not the absence of an effect: a backend with no
// StopOwnedProcesses looks exactly like a backend with nothing to do, and that
// resemblance is how a live writer survives a teardown.
func TestTerminalWiring_LiveProcessWithNoStopCapabilityBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pid, stop := runningPID(t)
	defer stop()

	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)

	commitTerminal(t, f, StageVerified)
	if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err == nil {
		t.Fatal("a request whose process is still running reported a clean finish")
	}

	st, _ := f.store.State("forge-run-1842-repair")
	note := ""
	for _, h := range st.History {
		if strings.HasPrefix(h.Event, "cleanup/") {
			note = h.Note
		}
	}
	if !strings.Contains(note, "did NOT prove quiescence") {
		t.Errorf("durable cleanup note = %q, want the survivor recorded", note)
	}
}

// TestTerminalWiring_NothingLaunches pins the constraint the whole suite rests
// on: no test here may reach a real model session or a socket. It is asserted
// rather than trusted because a fixture that quietly grows a live launcher would
// invalidate every other claim in this file while still passing them.
func TestTerminalWiring_NothingLaunches(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.liveness["acp-w"] = "closed"
	bindWorker(t, f, "acp-w", pid, "")
	eng := terminationEngine(t, f, b)
	eng.runtime = rt

	commitTerminal(t, f, StageVerified)
	if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err != nil {
		t.Fatalf("drive: %v", err)
	}

	// Cleanup released a process through the transport; it did not start a
	// session, send a prompt, or open a connection to do it.
	if len(rt.sentMessages()) != 0 {
		t.Errorf("reconciliation sent %d message(s): %v; cleanup must not dispatch anything",
			len(rt.sentMessages()), rt.sentMessages())
	}
	if len(rt.createdIDs) != 0 {
		t.Errorf("reconciliation created %d session(s): %v; cleanup must not activate anything",
			len(rt.createdIDs), rt.createdIDs)
	}
}

// TestTerminalWiring_RequestTerminatingInsideTheCallIsReconciled is the case that
// decides WHERE the seam goes.
//
// A request that is already terminal on entry is the easy half: any top-of-function
// guard would release it. The half that leaks is a request that becomes terminal
// DURING the lifecycle call — a completion PASS is committed inside
// `SubmitAnswer` — and it is the common one, because that is how a request
// actually finishes.
//
// Reconciling on entry rather than on exit leaves this request owning a live
// process, and `task-exec` is a one-shot process: it reads the committed result
// and exits, so the deferred "next tick" never arrives. The process would then stay
// alive until some later, unrelated invocation happened to drive this request.
func TestTerminalWiring_RequestTerminatingInsideTheCallIsReconciled(t *testing.T) {
	f := completionFixture(t)
	auditedRequest(t, f)
	rt := newFakeRuntime()

	// A process-owning transport stands in for the ACP worker the request would
	// be running. It is bound to the worker role ALREADY recorded, keeping the
	// auditor binding intact: the auditor is the session that has to answer for
	// this request to finish, and clearing it would mean the test never reached a
	// terminal commit at all.
	st := mustState(t, f, "forge-run-1842-repair")
	pid := reapedPID(t)
	b := newTerminationBackend()
	b.pid = pid
	b.name = "minimax-acp"
	b.liveness[st.WorkerSession] = "closed"
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		worker, ok := s.BackendBindings[RoleWorker]
		if !ok {
			worker = SessionBinding{Resolved: true}
		}
		// The worker moves onto the process-owning transport. The auditor stays on
		// Agent Deck, because it is the session that has to answer for this request
		// to finish, and moving it would stop the very reviewer being asked.
		worker.Backend = b.Name()
		worker.SessionID = s.WorkerSession
		worker.Resolved = true
		worker.ProcessPID = pid
		s.BackendBindings[RoleWorker] = worker
		return nil
	}); err != nil {
		t.Fatalf("bind worker process: %v", err)
	}

	// The reviewer writes a correlated PASS into its own session, digest included,
	// so the answer this call collects is one the store will actually accept. The
	// terminal stage is therefore committed INSIDE this call.
	body := judgedCompletionBody(t, f)
	rt.outputs[st.AuditorSession] = passEnvelope(t, st, body)

	eng := profileEngine(t, f, rt)
	eng.dispatch.WithBackend(b)

	if err := driveLifecycle(t, eng, "forge-run-1842-repair"); err != nil {
		t.Fatalf("drive: %v", err)
	}

	final := mustState(t, f, "forge-run-1842-repair")
	if final.Stage != StageVerified {
		t.Fatalf("stage = %s, want %s; this test needs the call to have committed the outcome",
			final.Stage, StageVerified)
	}
	// The request reached VERIFIED inside the call, so the process it owned must
	// have been released before the caller was told the request finished.
	if b.stops != 1 {
		t.Errorf("a request that finished inside the call left its process running: stops = %d", b.stops)
	}
	if got := countCleanupRecords(final, string(TerminationSuccess)); got != 1 {
		t.Errorf("cleanup records = %d, want the same call to record one; history = %+v", got, final.History)
	}
}

// passEnvelope builds the correlated answer a reviewer writes into its own
// session, digest included.
//
// The digest must be IN the envelope rather than stamped afterwards, because this
// test needs the production call to collect and submit the answer itself. A test
// that submitted the answer by hand would commit the terminal stage BEFORE the
// lifecycle ran, which is the opposite of the window under test.
func passEnvelope(t *testing.T, st *State, body string) string {
	t.Helper()
	env := ReviewEnvelope{
		RequestID: st.RequestID, Kind: st.ActiveReview.Kind, ReviewID: st.ActiveReview.ReviewID,
		TaskID: st.TaskID, Generation: st.ActiveReview.Generation,
		AnswerNonce: st.ActiveReview.AnswerNonce, Outcome: OutcomePass,
		ReportBody: body, ReportSHA256: sha256Hex([]byte(body)),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return string(data)
}

// countCleanupRecords counts the durable teardown records of one kind.
func countCleanupRecords(st *State, kind string) int {
	n := 0
	for _, h := range st.History {
		if h.Event == "cleanup/"+kind {
			n++
		}
	}
	return n
}
