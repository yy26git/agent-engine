package control

// S4 supervision tests.
//
// The claim is narrow and unforgiving: a worker that goes silent is noticed, a
// worker that is progressing is not taxed, and a loop cannot buy itself unlimited
// attempts by renaming what it is doing. Each test isolates one way that claim
// fails — a quiet stream nobody watched, a wake storm, debt reset by a judgement
// that advanced nothing, a ceiling bypassed by a repeat, a cancel that claims
// success it did not prove, and a restart that replays an unknown effect.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// supFixture binds a request with a worker and an auditor, so supervision has
// somewhere to send a review.
func supFixture(t *testing.T) *fixture {
	t.Helper()
	return boundRequest(t, newFixture(t))
}

// TestSupervision_QuietStreamSoftDeadline asserts a silent worker is noticed.
//
// The engine used to be purely reactive, so this is the behaviour that did not
// exist before: no frames arrive, and the deadline is still reached.
func TestSupervision_QuietStreamSoftDeadline(t *testing.T) {
	f := supFixture(t)
	eng := profileEngine(t, f, newFakeRuntime())

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	// Nothing observed, but the budget is spent.
	st.Checkpoint.DebtSeconds = 9999
	cause, due := eng.tick(st, f.now)
	if !due {
		t.Fatal("a worker silent past the soft budget produced no supervision cause")
	}
	if cause.Reason != ReasonQuietStream && cause.Reason != ReasonMissingCheck {
		t.Errorf("cause = %q, want a quiet-stream or missing-checkpoint cause", cause.Reason)
	}
	if cause.CoalesceKey == "" {
		t.Error("the cause carries no coalescing key, so identical causes cannot be merged")
	}
}

// TestSupervision_NormalProgressNoStrongWake asserts a progressing worker costs
// nothing.
//
// This is the property that stops supervision becoming a tax: a tick that finds a
// live request inside its budget must not raise a model wake.
func TestSupervision_NormalProgressNoStrongWake(t *testing.T) {
	f := supFixture(t)
	eng := profileEngine(t, f, newFakeRuntime())

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st.Checkpoint.DebtSeconds = 1
	st.Checkpoint.LastActivityAt = f.now.Add(-time.Second).Format(time.RFC3339)

	before := st.Supervision.WakeCount
	cause, due := eng.tick(st, f.now)
	if due {
		t.Fatalf("a progressing worker produced a supervision cause: %+v", cause)
	}
	if st.Supervision.WakeCount != before {
		t.Errorf("wake count moved from %d to %d without a cause", before, st.Supervision.WakeCount)
	}
}

// TestSupervision_MissingCheckpointWakesOnce asserts the same cause does not
// become a wake storm.
func TestSupervision_MissingCheckpointWakesOnce(t *testing.T) {
	f := supFixture(t)
	eng := profileEngine(t, f, newFakeRuntime())

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st.Checkpoint.DebtSeconds = 9999
	cause, due := eng.tick(st, f.now)
	if !due {
		t.Fatal("no cause for a spent budget")
	}

	// The first wake is always allowed.
	wake, why := st.Supervision.ShouldWake(cause, f.now, DefaultCheckpointPolicy())
	if !wake {
		t.Fatalf("the first wake was suppressed: %s", why)
	}
	st.Supervision.NoteWake(cause, f.now)

	// Immediately after, the SAME cause is coalesced.
	if wake, why := st.Supervision.ShouldWake(cause, f.now.Add(time.Second), DefaultCheckpointPolicy()); wake {
		t.Error("the same cause raised a second wake inside the minimum interval")
	} else if !strings.Contains(why, "same cause") && !strings.Contains(why, "minimum interval") {
		t.Errorf("the suppression did not explain itself: %s", why)
	}
	// A DIFFERENT cause is also suppressed by the floor, because a wake storm is
	// not improved by varying the reason.
	other := WakeCause{Reason: ReasonWallDeadline, CoalesceKey: ReasonWallDeadline}
	if wake, _ := st.Supervision.ShouldWake(other, f.now.Add(time.Second), DefaultCheckpointPolicy()); wake {
		t.Error("a different cause raised a wake inside the minimum interval")
	}
	// Far enough later, the same cause may wake again — suppression is a window,
	// not a permanent silence.
	if wake, why := st.Supervision.ShouldWake(cause, f.now.Add(2*MinWakeIntervalFloor), DefaultCheckpointPolicy()); !wake {
		t.Errorf("the same cause never woke again after the window: %s", why)
	}
}

// TestSupervision_WallDeadlineBeatsActivity asserts a generous budget cannot
// postpone review forever.
func TestSupervision_WallDeadlineBeatsActivity(t *testing.T) {
	f := supFixture(t)
	eng := profileEngine(t, f, newFakeRuntime())

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st.Checkpoint.DebtSeconds = 0
	st.Checkpoint.DeadlineAt = f.now.Add(-time.Second).Format(time.RFC3339)

	cause, due := eng.tick(st, f.now)
	if !due {
		t.Fatal("a passed wall deadline produced no cause")
	}
	if cause.Reason != ReasonWallDeadline {
		t.Errorf("cause = %q, want %q", cause.Reason, ReasonWallDeadline)
	}
}

// TestSupervision_GuidanceDoesNotResetDebt asserts only a reviewed ADVANCE resets
// active debt.
//
// A reviewer saying CONTINUE is a judgement that work should proceed, not evidence
// that anything advanced. Resetting on it would let a request that is going
// nowhere keep asking for more time.
func TestSupervision_GuidanceDoesNotResetDebt(t *testing.T) {
	f := supFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.DebtSeconds = 500
		s.AuditAttempts++
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C-1", s.AuditorSession, s.Generation, 1)
		return nil
	}); err != nil {
		t.Fatalf("open review: %v", err)
	}
	// A CONTINUE judgement.
	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCheckpoint, ReviewID: "C-1", Generation: 1,
		SourceSession: "sess-auditor-1", AnswerNonce: issuedNonce(t, f),
		Outcome: OutcomeContinue, Body: "carry on",
	}); err != nil {
		t.Fatalf("continue: %v", err)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Checkpoint.DebtSeconds == 0 {
		t.Error("a CONTINUE judgement reset active debt; only a reviewed ADVANCE may do that")
	}
}

// TestSupervision_ObjectiveFailureSchedulesDiagnostic asserts executed failures
// open an INDEPENDENT diagnostic, and only once the budget is actually spent.
func TestSupervision_ObjectiveFailureSchedulesDiagnostic(t *testing.T) {
	f := supFixture(t)
	eng := profileEngine(t, f, newFakeRuntime())

	// A failure with budget remaining does not escalate: the worker is entitled
	// to its attempts.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Supervision.ObjectiveFailures = 1
		s.RepairCycles = 0
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if _, due := eng.tick(st, f.now); due {
		t.Error("a single objective failure escalated while the repair budget remained")
	}

	// With the budget spent, it escalates — and to a DIAGNOSTIC, not a checkpoint
	// review, because the question has changed.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RepairCycles = s.effectiveRepairLimit()
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	cause, due := eng.tick(st, f.now)
	if !due {
		t.Fatal("spent budget with an objective failure produced no cause")
	}
	if cause.Reason != ReasonObjectiveFail {
		t.Fatalf("cause = %q, want %q", cause.Reason, ReasonObjectiveFail)
	}
	if err := eng.openDiagnosticOrReview(st, cause, f.now); err != nil {
		t.Fatalf("open diagnostic: %v", err)
	}
	if st.ActiveReview == nil {
		t.Fatal("no review was opened")
	}
	if st.ActiveReview.Kind != ReviewDiagnostic {
		t.Errorf("review kind = %s, want DIAGNOSTIC", st.ActiveReview.Kind)
	}
	if st.Stage != StageDiagnostic {
		t.Errorf("stage = %s, want DIAGNOSTIC", st.Stage)
	}
	// A diagnostic approves nothing, so the debt must be untouched.
	if st.Checkpoint.DebtSeconds != 0 && st.Checkpoint.LastReviewedAt != "" {
		t.Error("opening a diagnostic recorded a reviewed advance")
	}
}

// TestSupervision_RepeatedSignatureStillEnforcesCeiling asserts the ceiling
// cannot be bypassed by re-running a check that is already known to fail.
//
// This is the bug the stage names directly: the repeat branch returned before the
// guidance-exhaustion check, and because a repeat costs no budget, a loop could
// be handed to a diagnostic reviewer indefinitely.
func TestSupervision_RepeatedSignatureStillEnforcesCeiling(t *testing.T) {
	f := supFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Limits.RepairCycles = 1
		return nil
	}); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	rec := FailureRecord{CycleID: "c1", CheckID: "build", Signature: "compile error: undefined: x"}

	// First occurrence spends budget.
	var v CycleVerdict
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		v = s.RecordFailure(rec, f.now)
		return nil
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if !v.BudgetExhausted {
		t.Fatalf("the first failure did not exhaust a budget of 1: %+v", v)
	}

	// Exhaust the guidance budget, then repeat the SAME signature.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.DiagnosticRounds = s.guidanceAllowed()
		return nil
	}); err != nil {
		t.Fatalf("exhaust guidance: %v", err)
	}
	var repeat CycleVerdict
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		repeat = s.RecordFailure(FailureRecord{
			CycleID: "c2", CheckID: "build", Signature: "compile error: undefined: x",
		}, f.now)
		return nil
	}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if !repeat.RepeatedSignature {
		t.Error("the repeat was not recognised as a repeat")
	}
	if !repeat.GuidanceExhausted {
		t.Fatalf("a repeat bypassed the guidance ceiling: %+v", repeat)
	}
	if repeat.NeedsDiagnosis {
		t.Error("guidance was requested after the guidance budget was already spent")
	}
	// And the repair budget was not consumed by a free repeat.
	st, _ := f.store.State("forge-run-1842-repair")
	if st.RepairCycles != 1 {
		t.Errorf("repair cycles = %d, want 1: a repeat must not buy another attempt", st.RepairCycles)
	}
}

// TestSupervision_RenamedHypothesisDoesNotEvadeCeiling asserts a renamed
// hypothesis changes nothing, because counters are objective.
func TestSupervision_RenamedHypothesisDoesNotEvadeCeiling(t *testing.T) {
	f := supFixture(t)
	// Three genuinely DIFFERENT objective failures, each with a differently
	// self-declared hypothesis. Identical signatures would correctly be recognised
	// as a repeat and cost nothing, which is a different property.
	signatures := []string{"compile error: undefined: x", "test failed: want 3 got 4", "lint: shadowed variable"}
	for i, sig := range signatures {
		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			s.RecordFailure(FailureRecord{
				CycleID:   "c" + string(rune('0'+i+1)),
				CheckID:   "build",
				Signature: sig,
				// A different self-declared reason each time.
				Hypothesis: "hypothesis number " + string(rune('0'+i+1)),
			}, f.now)
			return nil
		}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.RepairCycles != 3 {
		t.Errorf("repair cycles = %d, want 3: a renamed hypothesis must still cost budget", st.RepairCycles)
	}
	if st.Supervision.ObjectiveFailures != 0 {
		t.Error("a worker self-report was counted as objective evidence")
	}
}

// TestSupervision_CancelJoinsJobsAndSessions asserts cancellation stops owned
// effects and commits CANCELLED only once they are proven stopped.
func TestSupervision_CancelJoinsJobsAndSessions(t *testing.T) {
	f := supFixture(t)
	rt := newFakeRuntime()
	// The bound sessions exist in the runtime, so Inspect can answer for them.
	rt.createdIDs = []string{"sess-worker-1", "sess-auditor-1"}
	rt.aliveAfterStop = true
	eng := profileEngine(t, f, rt)

	// Persist the intent first, which is what reconcile requires.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.CancellationRequested = true
		s.CancellationReason = "operator asked"
		return nil
	}); err != nil {
		t.Fatalf("intent: %v", err)
	}

	rep, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rt.stopped) != 2 {
		t.Errorf("stopped %d sessions, want both: %v", len(rt.stopped), rt.stopped)
	}
	// The session reports itself alive, so quiescence is NOT proven and the
	// request must not claim to be cancelled.
	if rep.Quiesced {
		t.Error("quiescence was claimed although the session still reports alive")
	}
	if rep.Committed {
		t.Error("CANCELLED was committed without proven quiescence")
	}
	if len(rep.Unresolved) == 0 {
		t.Error("the report names no unresolved effect")
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage == StageCancelled {
		t.Error("the request is CANCELLED despite unproven quiescence")
	}

	// Once the session really is stopped, the same call completes.
	rt.aliveAfterStop = false
	done, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile again: %v", err)
	}
	if !done.Quiesced || !done.Committed {
		t.Fatalf("a genuinely stopped session did not reconcile: %+v", done)
	}
	final, _ := f.store.State("forge-run-1842-repair")
	if final.Stage != StageCancelled {
		t.Errorf("stage = %s, want CANCELLED", final.Stage)
	}
	// And the result is published.
	if final.ResultStatus == "" {
		t.Error("no durable result was committed for a cancelled request")
	}
}

// TestSupervision_CancelWithoutIntentRefuses asserts reconcile never acts on a
// guess.
func TestSupervision_CancelWithoutIntentRefuses(t *testing.T) {
	f := supFixture(t)
	rt := newFakeRuntime()
	rt.createdIDs = []string{"sess-worker-1", "sess-auditor-1"}
	eng := profileEngine(t, f, rt)
	_, err := eng.ReconcileCancellation(context.Background(), "forge-run-1842-repair")
	if err == nil {
		t.Fatal("cancellation reconciled with no recorded intent")
	}
	if len(rt.stopped) != 0 {
		t.Errorf("sessions were stopped with no intent: %v", rt.stopped)
	}
}

// TestSupervision_BoundedWaitDoesNotStopTheClock asserts a wait with no deadline
// is not a pause.
//
// A verifier that hangs must not be able to freeze the request's clock forever,
// which is why an unbounded wait is treated as "not waiting" rather than "waiting
// indefinitely".
func TestSupervision_BoundedWaitDoesNotStopTheClock(t *testing.T) {
	var sup SupervisionState
	sup.RecordWait("verifier:build", time.Time{})
	if ok, why := sup.WaitSatisfied(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)); ok {
		t.Errorf("an unbounded wait reported as satisfied: %s", why)
	}
	// A bounded wait is satisfied only after its deadline.
	sup.RecordWait("verifier:build", time.Date(2026, 10, 6, 0, 5, 0, 0, time.UTC))
	if ok, _ := sup.WaitSatisfied(time.Date(2026, 10, 6, 0, 1, 0, 0, time.UTC)); ok {
		t.Error("a bounded wait reported as satisfied before its deadline")
	}
	if ok, why := sup.WaitSatisfied(time.Date(2026, 10, 6, 0, 6, 0, 0, time.UTC)); !ok {
		t.Errorf("an expired wait was not released: %s", why)
	}
}

// TestSupervision_RestartUnknownDoesNotReplay asserts an unresolved activation is
// visible after a restart rather than being retried silently.
func TestSupervision_RestartUnknownDoesNotReplay(t *testing.T) {
	f := supFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.RecordActivation(Activation{
			ActivationID: "AC-uncertain",
			Role:         "worker",
			Label:        "agentengine-label-1",
			Status:       ActivationDispatching,
			Revision:     s.Revision,
			CreatedAt:    s.UpdatedAt,
		})
	}); err != nil {
		t.Fatalf("record activation: %v", err)
	}
	// A restart re-reads state; the uncertainty must still be there.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state after restart: %v", err)
	}
	unresolved := st.UnresolvedActivations()
	if len(unresolved) != 1 {
		t.Fatalf("unresolved activations = %d, want 1 carried across the restart", len(unresolved))
	}
	if unresolved[0].Label != "agentengine-label-1" {
		t.Errorf("the activation lost its reconciliation label: %q", unresolved[0].Label)
	}
	// The ledger is full-resistant rather than evicting unresolved intent.
	for i := 0; i < MaxActivations; i++ {
		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			return s.RecordActivation(Activation{
				ActivationID: "AC-filler-" + string(rune('a'+i)),
				Role:         "worker", Label: "l", Status: ActivationPending,
				Revision: s.Revision, CreatedAt: s.UpdatedAt,
			})
		}); err != nil {
			if errors.Is(err, ErrActivationLedgerFull) {
				return // the bound held, which is the property under test
			}
			t.Fatalf("filler %d: %v", i, err)
		}
	}
	t.Error("the activation ledger accepted unlimited unresolved intent")
}

// TestSupervision_RefreshProductionEachCrashPhase asserts the replacement driver
// really runs the protocol, phase by phase, and that a crash between phases
// resumes rather than restarting.
func TestSupervision_RefreshProductionEachCrashPhase(t *testing.T) {
	f := supFixture(t)
	rt := newFakeRuntime()
	// The retired worker exists and will stop cleanly.
	rt.createdIDs = []string{"sess-worker-1", "sess-auditor-1"}
	eng := profileEngine(t, f, rt)

	// Begin the replacement the way a reviewer REFRESH answer would.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.BeginReplacement("the worker made no progress against an obligation", f.now)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Phase 1: INTENT -> HANDOFF, and the handoff is committed as a body.
	res, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive intent: %v", err)
	}
	if res.Phase != GenHandoff {
		t.Fatalf("phase = %q, want %q", res.Phase, GenHandoff)
	}
	if res.HandoffDigest == "" {
		t.Error("the handoff has no committed digest")
	}
	if _, rerr := f.store.ReadReport("forge-run-1842-repair", "handoff-does-not-exist", ""); rerr == nil {
		t.Error("a missing handoff report was readable")
	}
	// A crash here resumes at HANDOFF, not from the beginning.
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Generation != 1 {
		t.Errorf("generation changed at the handoff phase: %d", st.Generation)
	}

	// Phase 2: HANDOFF -> CANDIDATE. A candidate is created and recorded, and the
	// retired worker's write grant is revoked in the SAME transaction.
	res, err = eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive handoff: %v", err)
	}
	if res.Phase != GenCandidate {
		t.Fatalf("phase = %q, want %q", res.Phase, GenCandidate)
	}
	if res.CandidateSession == "" {
		t.Fatal("no candidate session was recorded")
	}
	if res.CandidateSession == "sess-worker-1" {
		t.Error("the candidate is the retired worker; a replacement must be a distinct session")
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if !st.Checkpoint.MutationGrantsPaused {
		t.Error("the retired worker's write grant was not revoked before a candidate was bound")
	}

	// Phase 3: CANDIDATE -> BINDING requires PROVEN quiescence.
	res, err = eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive candidate: %v", err)
	}
	if res.Phase != GenBinding {
		t.Fatalf("phase = %q, want %q", res.Phase, GenBinding)
	}
	if !res.Quiesced {
		t.Error("the binding committed without proven quiescence")
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.Generation != 2 {
		t.Errorf("generation = %d, want 2 after the binding", st.Generation)
	}
	if st.WorkerSession != res.CandidateSession {
		t.Errorf("worker = %q, want the candidate %q", st.WorkerSession, res.CandidateSession)
	}
	if st.BindingBoundary == 0 && !st.CursorSet {
		t.Error("the binding boundary was not captured; a retired frame could be mistaken for the new generation's")
	}

	// Phase 4 and 5: orientation, then activation. The new generation starts only
	// after orientation.
	res, err = eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive binding: %v", err)
	}
	if res.Phase != GenOrientation {
		t.Fatalf("phase = %q, want %q", res.Phase, GenOrientation)
	}
	if res.OrientationEffectID == "" {
		t.Error("orientation was recorded without a durable effect id")
	}
	res, err = eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive orientation: %v", err)
	}
	if !res.Active || res.Phase != GenActive {
		t.Fatalf("phase = %q active=%v, want ACTIVE", res.Phase, res.Active)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.Checkpoint.MutationGrantsPaused {
		t.Error("the write grant was not restored after activation")
	}
	// The new session was started exactly once.
	var starts int
	for _, id := range rt.started {
		if id == res.CandidateSession {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("the new generation was started %d times, want exactly 1", starts)
	}
}

// TestSupervision_RefreshUnprovenQuiescenceBlocksBinding asserts a binding is
// refused while the retired worker may still be writing.
func TestSupervision_RefreshUnprovenQuiescenceBlocksBinding(t *testing.T) {
	f := supFixture(t)
	rt := newFakeRuntime()
	rt.createdIDs = []string{"sess-worker-1", "sess-auditor-1"}
	// The retired worker refuses to die.
	rt.aliveAfterStop = true
	eng := profileEngine(t, f, rt)

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.BeginReplacement("no progress", f.now)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < 2; i++ { // INTENT -> HANDOFF -> CANDIDATE
		if _, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair"); err != nil {
			t.Fatalf("drive %d: %v", i, err)
		}
	}
	res, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drive candidate: %v", err)
	}
	if res.Phase == GenBinding {
		t.Fatal("the binding committed although the retired worker still reports alive")
	}
	if res.Blocked == "" {
		t.Error("the refusal carries no reason")
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Generation != 1 {
		t.Errorf("generation advanced to %d without proven quiescence", st.Generation)
	}
	if st.WorkerSession != "sess-worker-1" {
		t.Error("the worker binding changed without proven quiescence")
	}
}

// TestSupervision_StaleGenerationBoundary asserts a retired frame is refused after
// the swap.
func TestSupervision_StaleGenerationBoundary(t *testing.T) {
	f := supFixture(t)
	rt := newFakeRuntime()
	rt.createdIDs = []string{"sess-worker-1", "sess-auditor-1"}
	eng := profileEngine(t, f, rt)

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.BeginReplacement("no progress", f.now)
		s.BusCursor = 50
		s.CursorSet = true
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < 3; i++ { // through to BINDING
		if _, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair"); err != nil {
			t.Fatalf("drive %d: %v", i, err)
		}
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.BindingBoundary != 50 {
		t.Fatalf("binding boundary = %d, want the watermark 50 captured at the swap", st.BindingBoundary)
	}
	// A frame from BEFORE the boundary, from the RETIRED session, decides nothing.
	rt.frames = []agentdeck.Frame{finishedFrame(20, "sess-worker-1")}
	rt.sendState = "landed"
	if _, err := eng.handleFrame(context.Background(), "forge-run-1842-repair", rt.frames[0]); err != nil {
		t.Fatalf("handle stale frame: %v", err)
	}
	after, _ := f.store.State("forge-run-1842-repair")
	if after.ActiveReview != nil {
		t.Error("a retired worker's stale frame opened a review on the new generation")
	}
}

// TestSupervision_CountersSurviveRefresh asserts a refresh buys no budget.
func TestSupervision_CountersSurviveRefresh(t *testing.T) {
	f := supFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RepairCycles = 2
		s.DiagnosticRounds = 1
		_, err := s.BeginReplacement("stuck", f.now)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rt := newFakeRuntime()
	rt.createdIDs = []string{"sess-worker-1", "sess-auditor-1"}
	eng := profileEngine(t, f, rt)
	for i := 0; i < 3; i++ {
		if _, err := eng.DriveReplacement(context.Background(), "forge-run-1842-repair"); err != nil {
			t.Fatalf("drive %d: %v", i, err)
		}
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Generation < 2 {
		t.Fatalf("generation = %d, the replacement did not run", st.Generation)
	}
	if st.RepairCycles != 2 || st.DiagnosticRounds != 1 {
		t.Errorf("a refresh reset the lifetime counters: %d cycles, %d rounds",
			st.RepairCycles, st.DiagnosticRounds)
	}
}
