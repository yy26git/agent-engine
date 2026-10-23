package control

// EC3 checkpoint tests.
//
// The behaviour under test is a distinction that is easy to state and easy to
// lose: activity is not progress. A busy worker produces commits, file changes,
// green commands and status flips, and a supervisor that reads any of those as
// evidence of progress will nod along while the work goes nowhere.
//
// So the tests here mostly assert that something did *not* happen — no reviewer
// was woken, no debt was charged, no obligation advanced — and only the cases
// that genuinely constitute a reviewed advance move anything.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// checkpointFixture builds an admitted request with a controlled clock.
func checkpointFixture(t *testing.T) (*fixture, *time.Time) {
	t.Helper()
	f := boundRequest(t, newFixture(t))
	clock := f.now
	f.store.Now = func() time.Time { return clock }
	return f, &clock
}

func policy() CheckpointPolicy { return DefaultCheckpointPolicy() }

// TestCheckpoint_NormalProgressNoExtraModelWake asserts ordinary activity does
// not schedule a review.
//
// This is the single most valuable property in the stage. If every commit or
// status flip woke a strong model, a supervisor would cost more than the work it
// supervises and would still learn nothing, because none of those events says an
// obligation advanced.
func TestCheckpoint_NormalProgressNoExtraModelWake(t *testing.T) {
	f, clock := checkpointFixture(t)
	p := policy()

	// Ten minutes of ordinary, uninterrupted activity.
	from := *clock
	to := from.Add(10 * time.Minute)
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.Charge(ActiveWork{From: from, To: to})
		return nil
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Below the soft budget, so nothing is due.
	due, reason := st.Checkpoint.Due(p, to)
	if due {
		t.Errorf("a review was scheduled after ordinary activity: %s", reason)
	}
	if st.Checkpoint.ReviewPending {
		t.Error("ordinary activity must not mark a review pending")
	}

	// Even repeated observations do not accumulate into a due review while the
	// budget holds.
	for i := 0; i < 20; i++ {
		if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
			s.Checkpoint.Observe(to)
			return nil
		}); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if due, _ := st.Checkpoint.Due(p, to); due {
		t.Error("repeated status observations alone must not schedule a review")
	}
}

// TestCheckpoint_MissingWorkerSubmissionStillWakes asserts a required
// checkpoint that never arrives is caught.
//
// The worker forgetting to submit is the normal case, not an edge case. Software
// marks the requirement before handing out the instruction, so a worker cannot
// finish the work and only then discover it was never asked.
func TestCheckpoint_MissingWorkerSubmissionStillWakes(t *testing.T) {
	f, clock := checkpointFixture(t)
	p := policy()

	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.RequireCheckpoint(60, clock.Add(time.Hour), *clock)
		return nil
	}); err != nil {
		t.Fatalf("require: %v", err)
	}

	// Within grace, nothing is due.
	within := clock.Add(30 * time.Second)
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.Charge(ActiveWork{From: *clock, To: within})
		return nil
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if due, _ := st.Checkpoint.Due(p, within); due {
		t.Error("a review was scheduled before the grace expired")
	}

	// Past grace, the missing submission is due.
	past := clock.Add(2 * time.Minute)
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.Charge(ActiveWork{From: within, To: past})
		return nil
	}); err != nil {
		t.Fatalf("charge past grace: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	due, reason := st.Checkpoint.Due(p, past)
	if !due {
		t.Fatal("a required checkpoint that was never submitted must schedule a review")
	}
	if !strings.Contains(reason, "required checkpoint") {
		t.Errorf("reason = %q, want it to name the missing required checkpoint", reason)
	}

	// Scheduling marks the review pending, which suppresses a duplicate wake.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.ReviewPending = true
		s.ActiveReview = &ActiveReview{Kind: ReviewCheckpoint, ReviewID: "C-1", Generation: s.Generation}
		return nil
	}); err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if due, _ := st.Checkpoint.Due(p, past); due {
		t.Error("a pending review must suppress a second one")
	}
}

// TestCheckpoint_SoftActiveNoAdvanceReview asserts the soft budget fires when
// active time passes without a reviewed advance.
//
// This is the periodic sanity check, and it is deliberately not a poll. It is
// scheduled by elapsed active time, not by a timer that wakes a model on every
// tick.
func TestCheckpoint_SoftActiveNoAdvanceReview(t *testing.T) {
	f, clock := checkpointFixture(t)
	p := policy()
	p.SoftActiveBudget = 20 * time.Minute

	// Active time passes the budget with nothing reviewed.
	at := clock.Add(25 * time.Minute)
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.Charge(ActiveWork{From: *clock, To: at})
		return nil
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	due, reason := st.Checkpoint.Due(p, at)
	if !due {
		t.Fatal("exceeding the soft active budget with no reviewed advance must schedule a review")
	}
	if !strings.Contains(reason, "soft budget") {
		t.Errorf("reason = %q, want it to name the soft budget", reason)
	}

	// A reviewed CONTINUE does NOT reset the debt. "Keep going" is a judgement
	// that work should proceed; it is not evidence that anything advanced, and
	// treating it as such let a stalled request reset its own clock every cycle.
	// The test's own name is the property: no ADVANCE, no reset.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = &ActiveReview{Kind: ReviewCheckpoint, ReviewID: "C-2", Generation: s.Generation}
		return nil
	}); err != nil {
		t.Fatalf("open review: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("C-2", OutcomeContinue, "progressing", at)
	}); err != nil {
		t.Fatalf("apply review: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.Checkpoint.DebtSeconds == 0 {
		t.Error("a CONTINUE judgement reset the debt; only a reviewed ADVANCE may do that")
	}
	if due, _ := st.Checkpoint.Due(p, at); !due {
		t.Error("the budget must still be due: a judgement that advanced nothing bought no time")
	}
	// The judgement itself is still recorded, because knowing a review happened is
	// useful even when it bought nothing.
	if st.Checkpoint.LastReviewedAt == "" {
		t.Error("the review was not recorded at all")
	}

	// An EVIDENCED advance does reset it.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = &ActiveReview{Kind: ReviewCheckpoint, ReviewID: "C-3", Generation: s.Generation}
		return nil
	}); err != nil {
		t.Fatalf("open review: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if len(s.Obligations) == 0 {
			s.Obligations = []Obligation{{ID: "O-1", Criterion: "c"}}
		}
		s.Obligations[0].Disposition = DispositionAdvanced
		s.Obligations[0].EvidenceRefs = []string{"ev-1"}
		return s.ApplyReview("C-3", OutcomeContinue, "advanced with evidence", at)
	}); err != nil {
		t.Fatalf("apply evidenced review: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.Checkpoint.DebtSeconds != 0 {
		t.Errorf("an evidenced advance did not reset the debt, got %v", st.Checkpoint.DebtSeconds)
	}
	if due, _ := st.Checkpoint.Due(p, at); due {
		t.Error("the budget must not be due immediately after a reviewed advance")
	}
}

// TestCheckpoint_VerifierWaitPausesDebtButHasDeadline asserts a verifier run is
// not charged to the worker, while a hung verifier is still bounded.
//
// Both halves are load-bearing. Charging a worker for a test's compile time
// manufactures a divergence that never happened. But pausing the clock with no
// escape would let a hung verifier hide behind a worker that is not running, so
// the pause is bounded by the verifier's own deadline and by the request's
// absolute deadline.
func TestCheckpoint_VerifierWaitPausesDebtButHasDeadline(t *testing.T) {
	f, clock := checkpointFixture(t)
	p := policy()
	p.SoftActiveBudget = 10 * time.Minute

	start := *clock
	verifierEnd := start.Add(time.Hour) // a long, legitimate build
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		// Two hours of verifier time in one go.
		s.Checkpoint.Charge(ActiveWork{From: start, To: verifierEnd, Paused: true})
		return nil
	}); err != nil {
		t.Fatalf("charge paused: %v", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	if st.Checkpoint.DebtSeconds != 0 {
		t.Errorf("a paused verifier run was charged to the worker: %v seconds of debt", st.Checkpoint.DebtSeconds)
	}
	if due, _ := st.Checkpoint.Due(p, verifierEnd); due {
		t.Error("a paused verifier run must not trigger a worker review")
	}

	// But the request still has an absolute deadline, so a hung verifier is
	// eventually caught.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.RequireCheckpoint(0, start.Add(45*time.Minute), start)
		return nil
	}); err != nil {
		t.Fatalf("require with deadline: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if due, reason := st.Checkpoint.Due(p, start.Add(46*time.Minute)); !due {
		t.Error("the absolute deadline must fire even while worker debt is paused")
	} else if !strings.Contains(reason, "deadline") {
		t.Errorf("reason = %q, want it to name the absolute deadline", reason)
	}
}

// TestCheckpoint_RestartPreservesDebtWithoutChargingDowntime asserts a restart
// neither loses accrued debt nor charges the machine for time nobody was
// watching.
//
// Both are failure modes. Losing the debt would let a diverged request ask for
// time forever from a clean slate. Charging downtime would mean a request that
// was dead overnight looks like it was thinking hard, which is the opposite of
// the property the active clock exists to provide.
func TestCheckpoint_RestartPreservesDebtWithoutChargingDowntime(t *testing.T) {
	f, clock := checkpointFixture(t)
	p := policy()
	p.SoftActiveBudget = 2 * time.Minute

	// Accrue three minutes of debt.
	accrued := clock.Add(3 * time.Minute)
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.Charge(ActiveWork{From: *clock, To: accrued})
		return nil
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}

	// Simulate a restart: a brand new store object over the same directory, and a
	// clock that has jumped six hours forward.
	restarted := NewStore(f.cfg)
	restarted.Now = func() time.Time { return f.now }
	later := accrued.Add(6 * time.Hour)
	restarted.Now = func() time.Time { return later }

	st, err := restarted.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if st.Checkpoint.DebtSeconds < 179 || st.Checkpoint.DebtSeconds > 181 {
		t.Errorf("debt after restart = %v, want the accrued ~180 seconds preserved", st.Checkpoint.DebtSeconds)
	}

	// The six unobserved hours must not have been charged.
	if st.Checkpoint.DebtSeconds > 200 {
		t.Errorf("downtime was charged: debt = %v", st.Checkpoint.DebtSeconds)
	}

	// And the preserved debt still makes a review due.
	if due, _ := st.Checkpoint.Due(p, later); !due {
		t.Error("preserved debt must still make a review due after a restart")
	}
}

// TestCheckpoint_DuplicateOrStaleRefused asserts submission identity is strict.
func TestCheckpoint_DuplicateOrStaleRefused(t *testing.T) {
	f, clock := checkpointFixture(t)
	now := *clock

	base := CheckpointReceipt{
		CheckpointID: "CP-1",
		Claim:        "the failing test now reproduces locally",
		NextAction:   "fix the guard in the parser",
		EvidenceRefs: []string{"E-1"},
	}

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.SubmitCheckpoint(base, "digest-1", now)
		return err
	}); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// An identical retry is a duplicate, not an error the worker must handle.
	var dup CheckpointReceipt
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		r, err := s.SubmitCheckpoint(base, "digest-1", now)
		dup = r
		return err
	}); !errors.Is(err, ErrCheckpointDuplicate) {
		t.Fatalf("identical retry error = %v, want ErrCheckpointDuplicate", err)
	}
	if dup.Sequence == 0 {
		t.Error("a duplicate should report the original receipt, not a new one")
	}

	// The same ID with different content is a conflict, refused without mutation.
	before, _ := f.store.State("forge-run-1842-repair")
	conflicting := base
	conflicting.Claim = "something else entirely"
	err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		_, err := s.SubmitCheckpoint(conflicting, "digest-2", now)
		return err
	})
	if !errors.Is(err, ErrCheckpointStale) {
		t.Fatalf("conflicting submit error = %v, want ErrCheckpointStale", err)
	}
	after, _ := f.store.State("forge-run-1842-repair")
	if len(after.Checkpoint.Submissions) != len(before.Checkpoint.Submissions) {
		t.Errorf("a refused submission mutated the ledger: %d then %d",
			len(before.Checkpoint.Submissions), len(after.Checkpoint.Submissions))
	}

	// A submission with neither a claim nor a next action is refused.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		_, err := s.SubmitCheckpoint(CheckpointReceipt{CheckpointID: "CP-2"}, "digest-3", now)
		return err
	}); !errors.Is(err, ErrCheckpointEmpty) {
		t.Errorf("empty submit error = %v, want ErrCheckpointEmpty", err)
	}
}

// TestCheckpoint_SubmissionAdvancesNothing asserts accepting a checkpoint does
// not move the request.
//
// This is the property that separates a checkpoint from an approval. The worker
// may submit a perfectly well-formed checkpoint claiming enormous progress, and
// the obligation is still open, because nothing has reviewed it.
func TestCheckpoint_SubmissionAdvancesNothing(t *testing.T) {
	f, clock := checkpointFixture(t)
	now := *clock

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.SubmitCheckpoint(CheckpointReceipt{
			CheckpointID: "CP-1",
			Claim:        "all tests pass and every obligation is complete",
			NextAction:   "declare done",
		}, "digest-a", now)
		return err
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage.Terminal() {
		t.Errorf("a submission made the request terminal: %s", st.Stage)
	}
	for _, o := range st.Obligations {
		if o.Disposition != DispositionOpen {
			t.Errorf("obligation %s advanced to %s from a submission alone", o.ID, o.Disposition)
		}
	}
	if st.AuditAttempts != 0 {
		t.Errorf("a submission consumed audit budget: %d", st.AuditAttempts)
	}
	if len(st.Checkpoint.Submissions) != 1 || st.Checkpoint.Submissions[0].Reviewed {
		t.Error("an unreviewed submission must not be marked reviewed")
	}
}

// TestCheckpoint_StatusReportContinuesOpenWork asserts a status-only event
// continues open work rather than being treated as a stall or a completion.
//
// The embedded draft conflated three things: going idle, finishing, and being
// stuck. A status report is none of them. It is the most ordinary event in the
// system, and treating it as a divergence would manufacture a supervision
// problem out of normal operation.
func TestCheckpoint_StatusReportContinuesOpenWork(t *testing.T) {
	f, _ := checkpointFixture(t)
	rt := newFakeRuntime()
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 6}
	// A worker that simply reports a status: running -> waiting. No DONE, no
	// completion assertion, nothing owed.
	rt.frames = []agentdeck.Frame{
		transitionFrame(6, "sess-worker-1", "running", "waiting"),
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true,
		Now:  func() time.Time { return f.now },
		Log:  func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	res, err := eng.Run(context.Background(), "forge-run-1842-repair")
	if err != nil && !strings.Contains(err.Error(), "follower exited") {
		t.Fatalf("run: %v", err)
	}
	if res.FramesDecided != 0 {
		t.Errorf("a status-only frame produced %d decisions, want 0", res.FramesDecided)
	}
	if len(rt.sentMessages()) != 0 {
		t.Errorf("a status-only frame sent instructions: %v", rt.sentMessages())
	}

	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage.Terminal() {
		t.Errorf("a status report made the request terminal: %s", st.Stage)
	}
	if st.AuditAttempts != 0 {
		t.Errorf("a status report opened an audit: %d attempts", st.AuditAttempts)
	}
	for _, o := range st.Obligations {
		if o.Disposition != DispositionOpen {
			t.Errorf("obligation %s changed to %s from a status report", o.ID, o.Disposition)
		}
	}
	// The checkpoint is still owed and no review was scheduled, which is what
	// "continues open work" means concretely.
	if st.Checkpoint.ReviewPending {
		t.Error("a status report scheduled a review")
	}
	if st.Checkpoint.DebtSeconds != 0 {
		t.Errorf("a status frame charged active time without a working observation: %v", st.Checkpoint.DebtSeconds)
	}
}

// TestNegativeClaim_SingleQueryIsUnknown asserts an incomplete negative search
// can only be recorded as UNKNOWN.
//
// A worker that greps once and concludes a bug does not exist has not established
// absence. Software decides the conclusion rather than trusting the worker to
// self-assess, because a claim that grades its own rigour is exactly the claim
// that needs checking.
func TestNegativeClaim_SingleQueryIsUnknown(t *testing.T) {
	f, clock := checkpointFixture(t)
	now := *clock

	rec := CheckpointReceipt{
		CheckpointID: "CP-1",
		Claim:        "the timeout is not caused by the parser",
		NextAction:   "instrument the scheduler",
		NegativeClaims: []NegativeClaim{{
			ClaimID:    "NC-1",
			Subject:    "a call to ParseState in the hot path",
			Scope:      "src/parser at e19e23d",
			Queries:    []string{"rg ParseState src/"},
			Locations:  []string{"src/parser/parser.go"},
			Conclusion: ClaimAbsent,
		}},
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.SubmitCheckpoint(rec, "digest-nc", now)
		return err
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	got := st.Checkpoint.Submissions[0].NegativeClaims[0]
	// A single query with no alternatives is an incomplete search, so the
	// recorded conclusion must be UNKNOWN regardless of what was claimed.
	if got.Conclusion != ClaimUnknown {
		t.Errorf("conclusion = %q, want %q: a single query is not a completed search", got.Conclusion, ClaimUnknown)
	}

	// A fully documented search keeps its own conclusion.
	full := rec
	full.CheckpointID = "CP-2"
	full.NegativeClaims = []NegativeClaim{{
		ClaimID:      "NC-2",
		Subject:      "a call to ParseState in the hot path",
		Scope:        "src/parser at e19e23d",
		Queries:      []string{"rg ParseState src/", "rg parse_state src/"},
		Alternatives: []string{"ParseState", "parse_state", "parseState"},
		Locations:    []string{"src/parser/parser.go", "src/parser/lexer.go"},
		Conclusion:   ClaimAbsent,
	}}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.SubmitCheckpoint(full, "digest-nc-2", now)
		return err
	}); err != nil {
		t.Fatalf("submit full: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if c := st.Checkpoint.Submissions[1].NegativeClaims[0].Conclusion; c != ClaimAbsent {
		t.Errorf("a documented search had its conclusion downgraded to %q", c)
	}
}

// TestCheckpoint_ReviewGateKeepsModesApart asserts a checkpoint review cannot
// approve, and that only an active, current review is applied.
func TestCheckpoint_ReviewGateKeepsModesApart(t *testing.T) {
	f, clock := checkpointFixture(t)
	now := clock.Add(time.Minute)

	// A stale review ID is refused without mutation.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("nope", OutcomeContinue, "", now)
	}); !errors.Is(err, ErrCheckpointStale) {
		t.Errorf("stale review error = %v, want ErrCheckpointStale", err)
	}

	// A checkpoint review cannot answer PASS.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = &ActiveReview{Kind: ReviewCheckpoint, ReviewID: "C-1", Generation: s.Generation}
		return nil
	}); err != nil {
		t.Fatalf("open review: %v", err)
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("C-1", OutcomePass, "", now)
	}); err == nil {
		t.Fatal("a CHECKPOINT review must not be able to answer PASS")
	}

	// A review opened for a worker that has since been replaced is refused: the
	// stored review's generation is compared with the current one, so moving the
	// generation is what invalidates it.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = &ActiveReview{Kind: ReviewCheckpoint, ReviewID: "C-2", Generation: s.Generation}
		return nil
	}); err != nil {
		t.Fatalf("open review 2: %v", err)
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Generation++
		return nil
	}); err != nil {
		t.Fatalf("advance generation: %v", err)
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("C-2", OutcomeContinue, "", now)
	}); !errors.Is(err, ErrCheckpointStale) {
		t.Errorf("cross-generation review error = %v, want ErrCheckpointStale", err)
	}
}

// TestCheckpoint_EvidenceRequiresDigest asserts evidence is pinned.
//
// A path with no digest points at bytes that may since have changed, so a
// conclusion resting on it attributes to nothing.
func TestCheckpoint_EvidenceRequiresDigest(t *testing.T) {
	f, _ := checkpointFixture(t)

	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.AddEvidence(EvidenceRef{ID: "E-1", Path: "EVIDENCE/E-1/diff.patch"})
	}); err == nil {
		t.Error("evidence without a digest must be refused")
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.AddEvidence(EvidenceRef{ID: "E-1", Path: "x", SHA256: "not-a-digest"})
	}); err == nil {
		t.Error("a malformed digest must be refused")
	}
	digest := strings.Repeat("ab", 32)
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.AddEvidence(EvidenceRef{ID: "E-1", Path: "x", SHA256: digest, Kind: "artifact"})
	}); err != nil {
		t.Fatalf("valid evidence refused: %v", err)
	}
	// The same ID with a different digest is a conflict, not a replacement.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.AddEvidence(EvidenceRef{ID: "E-1", Path: "x", SHA256: strings.Repeat("cd", 32)})
	}); err == nil {
		t.Error("reusing an evidence ID with a different digest must be refused")
	}
	// The same ID with the same digest is a no-op.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.AddEvidence(EvidenceRef{ID: "E-1", Path: "x", SHA256: digest})
	}); err != nil {
		t.Errorf("identical evidence should be a no-op, got %v", err)
	}
}

// TestCheckpoint_GrantsPausedStopsCharging asserts pausing mutation grants stops
// the active clock.
func TestCheckpoint_GrantsPausedStopsCharging(t *testing.T) {
	f, clock := checkpointFixture(t)
	from := *clock
	to := from.Add(30 * time.Minute)

	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.Checkpoint.MutationGrantsPaused = true
		s.Checkpoint.Charge(ActiveWork{From: from, To: to})
		return nil
	}); err != nil {
		t.Fatalf("charge: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Checkpoint.DebtSeconds != 0 {
		t.Errorf("time accrued while grants were paused was charged: %v", st.Checkpoint.DebtSeconds)
	}
}
