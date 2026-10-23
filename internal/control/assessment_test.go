package control

// AP5 completion-routing tests.
//
// The claim under test: a worker that never says "done" and never asks for a
// review still gets assessed, and nothing short of an independent, correlated,
// fresh-checked approval can produce PASS.
//
// These drive the REAL functions the production paths call
// (`RequestCompletionAssessment`, `PromoteAssessment`, `applyPromptReceipt`),
// not a stand-in.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/runtime"
)

// receiptEvent builds the correlated prompt receipt a backend publishes.
func receiptEvent(sessionID, sendID, stopReason string) runtime.Event {
	return runtime.Event{
		Backend: "minimax-acp", Kind: runtime.KindPromptResult, SessionID: sessionID,
		Payload: []byte(`{"sessionId":"` + sessionID + `","sendId":"` + sendID + `","stopReason":"` + stopReason + `"}`),
	}
}

// acpBoundRequest admits a request whose WORKER is an ACP session.
func acpBoundRequest(t *testing.T, f *fixture, sessionID string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		s.BackendBindings[RoleWorker] = SessionBinding{
			Backend: "minimax-acp", SessionID: sessionID,
			Generation: s.Generation, Resolved: true,
		}
		s.WorkerSession = sessionID
		s.AuditorSession = "sess-auditor-1"
		return s.Transition(StageWorking, "worker started", s.UpdatedAt)
	}); err != nil {
		t.Fatalf("bind acp worker: %v", err)
	}
}

func completionReview(st *State) *ActiveReview {
	if st.ActiveReview == nil {
		return nil
	}
	return st.ActiveReview
}

// TestCompletion_MiniMaxNeverSaysDoneYetAssessmentOccurs is the headline proof.
//
// The worker emits no finished event, no checkpoint, no review request and no
// text claiming completion. It simply stops. An assessment must still happen,
// and it must be asked of the INDEPENDENT reviewer, not decided locally.
func TestCompletion_MiniMaxNeverSaysDoneYetAssessmentOccurs(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()

	// The worker says nothing at all. The only evidence is the correlated receipt.
	eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rev := completionReview(st)
	if rev == nil {
		t.Fatal("no assessment was opened for a worker that silently stopped")
	}
	if rev.Kind != ReviewCompletion {
		t.Errorf("review kind = %q, want %q", rev.Kind, ReviewCompletion)
	}
	if rev.ExpectedReviewer != "sess-auditor-1" {
		t.Errorf("reviewer = %q, want the independently bound auditor", rev.ExpectedReviewer)
	}
	// Exactly one instruction, addressed to the reviewer.
	if len(st.Effects) != 1 {
		t.Fatalf("effects = %d, want exactly 1", len(st.Effects))
	}
	if st.Effects[0].SessionID != "sess-auditor-1" {
		t.Errorf("instruction went to %q, want the auditor", st.Effects[0].SessionID)
	}
	// And it is a REQUEST for judgement, not an approval.
	if st.ActiveReview != nil && st.ResultStatus != "" {
		t.Error("a candidate produced a result status; assessment is not approval")
	}
}

// TestCompletion_DuplicateReceiptsAndRestartDoNotDuplicate is the dedup proof.
//
// The same turn reported twice, and a restart in between, must not produce a
// second review, a second audit attempt, or a second instruction.
func TestCompletion_DuplicateReceiptsAndRestartDoNotDuplicate(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	ctx := context.Background()

	apply := func() {
		eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
		defer stop()
		eng.applyPromptReceipt(ctx, "forge-run-1842-repair", b,
			receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))
	}

	apply() // first delivery
	apply() // duplicate source
	apply() // restart, same durable turn identity

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.AuditAttempts != 1 {
		t.Errorf("audit attempts = %d, want 1: a duplicate must not buy a second audit", st.AuditAttempts)
	}
	if len(st.Effects) != 1 {
		t.Errorf("effects = %d, want 1: a duplicate receipt must not send a second instruction", len(st.Effects))
	}
	if rev := completionReview(st); rev == nil || rev.ReviewID != "A1" {
		t.Errorf("active review = %+v, want the original A1 still open", rev)
	}
}

// TestCompletion_QuietFailedCancelledAndStaleTurnsCannotProducePASS covers the
// eligibility boundary.
//
// Every one of these is a way a turn ends that is NOT a claim that the work
// finished. Each must leave the request with no completion review at all.
func TestCompletion_QuietFailedCancelledAndStaleTurnsCannotProducePASS(t *testing.T) {
	cases := map[string]struct {
		stopReason string
		sendID     string
		mutate     func(*State) error
	}{
		"a failed turn":           {stopReason: "error", sendID: "s1"},
		"a refusal":               {stopReason: "refusal", sendID: "s2"},
		"a cancelled turn":        {stopReason: "cancelled", sendID: "s3"},
		"an unknown stop reason":  {stopReason: "who_knows", sendID: "s4"},
		"an uncorrelated receipt": {stopReason: "end_turn", sendID: ""},
		"a stale generation":      {stopReason: "end_turn", sendID: "s6", mutate: func(s *State) error { s.Generation++; return nil }},
		"another session's turn": {stopReason: "end_turn", sendID: "s7", mutate: func(s *State) error {
			s.WorkerSession = "acp-other"
			return nil
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := boundRequest(t, newFixture(t))
			b := newEmittingBackend("minimax-acp")
			acpBoundRequest(t, f, "acp-1")
			if tc.mutate != nil {
				if _, err := f.store.UpdateCommitted("forge-run-1842-repair", tc.mutate); err != nil {
					t.Fatalf("mutate: %v", err)
				}
			}
			eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
			defer stop()

			session := "acp-1"
			if tc.sendID == "s7" {
				session = "acp-other"
			}
			eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
				receiptEvent(session, tc.sendID, tc.stopReason))

			st, err := f.store.State("forge-run-1842-repair")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if rev := completionReview(st); rev != nil && rev.Kind == ReviewCompletion {
				t.Errorf("a completion review was opened for %s: %+v", name, rev)
			}
			if st.ResultStatus != "" {
				t.Errorf("a result status was produced: %q", st.ResultStatus)
			}
			if st.Stage == StageVerified {
				t.Error("stage = VERIFIED without an independent approval")
			}
		})
	}
}

// TestCompletion_CandidateIsRetainedAndPromotedAfterAReviewSettles is the
// stranding proof.
//
// A completion candidate arrives while a checkpoint review is in flight. No
// further worker event ever arrives. The candidate must still be assessed.
func TestCompletion_CandidateIsRetainedAndPromotedAfterAReviewSettles(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	ctx := context.Background()

	// Something else is already owed.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C1", "sess-auditor-1", s.Generation, 1)
		s.ActiveReview.RequestedAt = s.UpdatedAt
		return nil
	}); err != nil {
		t.Fatalf("open checkpoint review: %v", err)
	}

	eng.applyPromptReceipt(ctx, "forge-run-1842-repair", b, receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rev := completionReview(st); rev == nil || rev.Kind != ReviewCheckpoint {
		t.Fatalf("the active review was replaced: %+v", rev)
	}
	if len(st.UnconsumedAssessmentCandidates()) != 1 {
		t.Fatalf("the candidate was not retained: %+v", st.PendingAssessments)
	}
	attemptsBefore := st.AuditAttempts

	// The checkpoint settles. NO further worker event arrives.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("C1", OutcomeContinue, "keep going", f.now)
	}); err != nil {
		t.Fatalf("settle checkpoint: %v", err)
	}
	if err := eng.PromoteRetainedAssessment(ctx, "forge-run-1842-repair"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	st, err = f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rev := completionReview(st)
	if rev == nil || rev.Kind != ReviewCompletion {
		t.Fatalf("the retained candidate was stranded: active review = %+v", rev)
	}
	if st.AuditAttempts != attemptsBefore+1 {
		t.Errorf("audit attempts = %d, want %d", st.AuditAttempts, attemptsBefore+1)
	}
	if len(st.UnconsumedAssessmentCandidates()) != 0 {
		t.Error("the candidate was promoted but still shows as unconsumed")
	}
}

// TestCompletion_CancellationPriorityBeatsAClaim is the ordering guarantee.
//
// A worker that stops while a cancellation is pending is being stopped, not
// asking to be assessed.
func TestCompletion_CancellationPriorityBeatsAClaim(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	cancelledRequest(t, f)

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rev := completionReview(st); rev != nil && rev.Kind == ReviewCompletion {
		t.Errorf("a completion review opened while a cancellation was pending: %+v", rev)
	}
}

// TestCompletion_NoReviewerMeansRetainedNotDiscarded.
//
// With nobody to judge, the claim must survive. Dropping it would silently lose
// the only statement that the work finished.
func TestCompletion_NoReviewerMeansRetainedNotDiscarded(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.AuditorSession = ""
		return nil
	}); err != nil {
		t.Fatalf("unbind auditor: %v", err)
	}

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(st.UnconsumedAssessmentCandidates()) != 1 {
		t.Errorf("the claim was discarded with no reviewer bound: %+v", st.PendingAssessments)
	}
	if len(st.Effects) != 0 {
		t.Errorf("an instruction was sent with no reviewer: %+v", st.Effects)
	}
}

// TestCompletion_CandidateSurvivesStaleGenerationOnPromotion proves freshness is
// revalidated before dispatch, not just at intake.
func TestCompletion_CandidateSurvivesStaleGenerationOnPromotion(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")

	// Retain a candidate directly, then move the generation underneath it.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C1", "sess-auditor-1", s.Generation, 1)
		s.PendingAssessments = append(s.PendingAssessments, AssessmentCandidate{
			CandidateID: "CA-stale", Backend: "minimax-acp", SessionID: "acp-1",
			Generation: s.Generation, TurnID: "old", RaisedAt: s.UpdatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.ApplyReview("C1", OutcomeContinue, "keep going", f.now); err != nil {
			return err
		}
		s.Generation++
		return nil
	}); err != nil {
		t.Fatalf("advance generation: %v", err)
	}

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	if err := eng.PromoteRetainedAssessment(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rev := completionReview(st); rev != nil && rev.Kind == ReviewCompletion {
		t.Errorf("a candidate from a retired generation was promoted: %+v", rev)
	}
}

// TestCompletion_PassStillRequiresACorrelatedIndependentApproval guards the
// invariant that this whole feature sits behind.
//
// Adding an assessment route must not have weakened what a PASS requires.
func TestCompletion_PassStillRequiresACorrelatedIndependentApproval(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rev := completionReview(st)
	if rev == nil {
		t.Fatal("no review was opened")
	}
	if rev.AnswerNonce == "" {
		t.Error("the review carries no correlation nonce, so its answer could never be proven to belong to it")
	}
	if rev.Kind.Approves() == false {
		t.Error("a COMPLETION review must be able to approve; otherwise the loop cannot terminate")
	}

	// An answer carrying the WRONG nonce must be refused.
	_, err = f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: rev.ReviewID,
		Generation: rev.Generation, SourceSession: "sess-auditor-1",
		AnswerNonce: strings.Repeat("0", 64), Outcome: OutcomePass,
		ReportDigest: strings.Repeat("a", 64), Body: "looks fine to me",
	})
	if err == nil {
		t.Error("an uncorrelated PASS was accepted")
	}
}

// TestCompletion_AuditBudgetIsEnforcedAtRuntime is the exhaustion proof.
//
// The operator's ceiling is checked at admission for the limits a request ASKS
// for, but it was never checked against the attempts actually made. A worker
// that ended a turn repeatedly — each with its own send id, so each a distinct
// candidate — could open unbounded audits, each costing a reviewer turn.
func TestCompletion_AuditBudgetIsEnforcedAtRuntime(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Limits.AuditAttempts = 2
		return nil
	}); err != nil {
		t.Fatalf("set limit: %v", err)
	}

	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	ctx := context.Background()

	// Distinct send ids are distinct claims, so deduplication does not stop them.
	for i := 1; i <= 2; i++ {
		eng.applyPromptReceipt(ctx, "forge-run-1842-repair", b,
			receiptEvent("acp-1", "acp:acp-1:"+string(rune('0'+i)), "end_turn"))
		// Settle whatever opened, so the next one is not blocked by an active review.
		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			if s.ActiveReview == nil {
				return nil
			}
			// A COMPLETION review may only answer PASS, FAIL or BLOCKED — the
			// separate-authority rule. BLOCKED settles it without approving.
			return s.ApplyReview(s.ActiveReview.ReviewID, OutcomeBlocked, "needs another look", f.now)
		}); err != nil {
			t.Fatalf("settle: %v", err)
		}
	}

	before := reviewCount(t, f)
	eng.applyPromptReceipt(ctx, "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:9", "end_turn"))
	if got := reviewCount(t, f); got != before {
		t.Errorf("reviews = %d after the budget was spent, want %d", got, before)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.AuditAttempts > st.Limits.AuditAttempts {
		t.Errorf("audit attempts = %d exceeds the operator limit %d", st.AuditAttempts, st.Limits.AuditAttempts)
	}
	// The claim is RETAINED, not discarded: an exhausted budget means nobody may
	// judge it now, and dropping it would erase the only statement the work
	// stopped.
	if len(st.UnconsumedAssessmentCandidates()) == 0 {
		t.Error("the claim was discarded instead of retained when the budget ran out")
	}
	if st.ResultStatus == "PASS" {
		t.Error("a PASS was produced with the audit budget exhausted")
	}
}

// reviewCount counts completion reviews the ledger has ever opened.
func reviewCount(t *testing.T, f *fixture) int {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	n := 0
	for _, c := range st.PendingAssessments {
		if c.Consumed {
			n++
		}
	}
	return n
}

// auditEnvelope extracts the ReviewEnvelope the outgoing instruction carries.
//
// The instruction is the ONLY thing the reviewer receives, so if it does not
// carry the right review id, generation and nonce, the reviewer cannot answer
// correctly no matter how well the control layer behaves.
//
// The envelope is the FIRST JSON value in the message and not the whole of it: a
// completion instruction continues past the envelope with the report shape the
// reviewer must fill in, and prose after a JSON object is not a parse error in
// the object. Decoding one value keeps this helper answering its actual question
// — is the correlated envelope there? — instead of failing on whatever the
// instruction says after it.
func auditEnvelope(t *testing.T, st *State) (ReviewEnvelope, bool) {
	t.Helper()
	for _, eff := range st.Effects {
		i := strings.Index(eff.Message, "{")
		if i < 0 {
			continue
		}
		var env ReviewEnvelope
		if err := json.NewDecoder(strings.NewReader(eff.Message[i:])).Decode(&env); err != nil {
			continue
		}
		if env.Kind == ReviewCompletion && env.AnswerNonce != "" {
			return env, true
		}
	}
	return ReviewEnvelope{}, false
}

// TestCompletion_AuditInstructionCarriesTheRightReviewIdentity is the ordering
// proof for the immediate path.
//
// reviewAnswerInstruction reads s.ActiveReview to stamp the review id, generation
// and nonce. Building the message before assigning the review either stamps the
// PREVIOUS review's identity or emits no envelope at all — and a reviewer with
// no nonce cannot answer, so the request can never complete.
func TestCompletion_AuditInstructionCarriesTheRightReviewIdentity(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()

	eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rev := completionReview(st)
	if rev == nil {
		t.Fatal("no review was opened")
	}
	env, ok := auditEnvelope(t, st)
	if !ok {
		t.Fatalf("the audit instruction carries no review envelope; the reviewer cannot correlate "+
			"an answer to it. message starts: %.200s", firstEffectMessage(st))
	}
	if env.ReviewID != rev.ReviewID {
		t.Errorf("instruction review id = %q, active review = %q", env.ReviewID, rev.ReviewID)
	}
	if env.Generation != rev.Generation {
		t.Errorf("instruction generation = %d, active review = %d", env.Generation, rev.Generation)
	}
	if env.AnswerNonce != rev.AnswerNonce {
		t.Errorf("instruction nonce = %q, active review = %q", env.AnswerNonce, rev.AnswerNonce)
	}
	if env.Kind != ReviewCompletion {
		t.Errorf("instruction kind = %q, want %q", env.Kind, ReviewCompletion)
	}
	if env.RequestID != "forge-run-1842-repair" {
		t.Errorf("instruction request id = %q", env.RequestID)
	}
}

// TestCompletion_PromotedAuditInstructionCarriesTheRightIdentity is the same
// proof for the PROMOTION path, which builds its message separately and so can
// drift independently.
func TestCompletion_PromotedAuditInstructionCarriesTheRightIdentity(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	ctx := context.Background()

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C1", "sess-auditor-1", s.Generation, 1)
		s.PendingAssessments = append(s.PendingAssessments, AssessmentCandidate{
			CandidateID: "CA-held", Backend: "minimax-acp", SessionID: "acp-1",
			Generation: s.Generation, TurnID: "held", RaisedAt: s.UpdatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("C1", OutcomeContinue, "keep going", f.now)
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := eng.PromoteRetainedAssessment(ctx, "forge-run-1842-repair"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rev := completionReview(st)
	if rev == nil || rev.Kind != ReviewCompletion {
		t.Fatalf("no completion review was promoted: %+v", rev)
	}
	env, ok := auditEnvelope(t, st)
	if !ok {
		t.Fatalf("the PROMOTED instruction carries no envelope: %.200s", firstEffectMessage(st))
	}
	if env.ReviewID != rev.ReviewID || env.AnswerNonce != rev.AnswerNonce || env.Generation != rev.Generation {
		t.Errorf("promoted instruction identity does not match the active review: envelope=%+v review=%+v", env, rev)
	}
}

func firstEffectMessage(st *State) string {
	for _, e := range st.Effects {
		if strings.Contains(e.Message, "completion audit") {
			return e.Message
		}
	}
	if len(st.Effects) > 0 {
		return st.Effects[0].Message
	}
	return "(no effects)"
}
