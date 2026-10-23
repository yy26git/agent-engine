package control

// LF2 phase and budget tests.
//
// Two claims. A phase never moves forward without authority, and a budget
// belongs to the EPISODE — so an episode that oscillates between phases still
// runs out.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newEpisode(t *testing.T) *State {
	t.Helper()
	s := &State{Revision: 1, UpdatedAt: "2026-10-10T00:00:00Z"}
	if err := s.StartEpisode("EP-1", DefaultEpisodeBudget(), time.Unix(1791500000, 0).UTC()); err != nil {
		t.Fatalf("start episode: %v", err)
	}
	return s
}

func acceptPlan(t *testing.T, s *State, version int) {
	t.Helper()
	plan := contractPlanForTest(version)
	auth := authForTest(plan)
	if err := s.AuthorizePlan(auth, &plan, time.Unix(1791500000, 0).UTC()); err != nil {
		t.Fatalf("authorize plan: %v", err)
	}
}

// TestPhase_ImplementationRequiresAnAcceptedPlan is the central guard.
//
// Implementing on the strength of a plan having been DRAFTED is how an
// unauthorised change reaches the workspace.
func TestPhase_ImplementationRequiresAnAcceptedPlan(t *testing.T) {
	s := newEpisode(t)
	now := time.Unix(1791500000, 0).UTC()
	for _, to := range []EpisodePhase{PhaseReportReview, PhasePlan, PhasePlanAccept} {
		if err := s.AdvancePhase(to, "progress", now, nil); err != nil {
			t.Fatalf("advance to %s: %v", to, err)
		}
	}
	err := s.AdvancePhase(PhaseImplement, "work", now, nil)
	if !errors.Is(err, ErrPhaseNotAllowed) {
		t.Fatalf("implementation was permitted without an accepted plan: %v", err)
	}
	acceptPlan(t, s, 1)
	if err := s.AdvancePhase(PhaseImplement, "work", now, nil); err != nil {
		t.Fatalf("implementation refused after a plan was accepted: %v", err)
	}
}

// TestPhase_BackwardsMovesNeedATemplateReturnEdge proves a rejection cannot be
// walked past silently.
func TestPhase_BackwardsMovesNeedATemplateReturnEdge(t *testing.T) {
	s := newEpisode(t)
	now := time.Unix(1791500000, 0).UTC()
	_ = s.AdvancePhase(PhaseReportReview, "r", now, nil)
	_ = s.AdvancePhase(PhasePlan, "p", now, nil)
	// Back to the report: a real possibility, but only with a declared edge.
	if err := s.AdvancePhase(PhaseReport, "rejected", now, nil); !errors.Is(err, ErrPhaseNotAllowed) {
		t.Fatalf("a backwards move was permitted with no template: %v", err)
	}
}

// TestPhase_AcceptanceNeverGrantsPass is the invariant the whole authority model
// rests on: authorising a plan is not approving work.
func TestPhase_AcceptanceNeverGrantsPass(t *testing.T) {
	s := newEpisode(t)
	acceptPlan(t, s, 1)
	if s.Episode.PlanDigest == "" {
		t.Fatal("the accepted plan digest was not recorded")
	}
	if s.Stage == StageVerified {
		t.Fatal("accepting a plan verified the request")
	}
	// Only a COMPLETION review may approve, and none is open.
	if s.ActiveReview != nil && s.ActiveReview.Kind.Approves() {
		t.Fatal("plan acceptance opened an approving review")
	}
	if s.ResultStatus != "" {
		t.Fatalf("plan acceptance produced a result status: %q", s.ResultStatus)
	}
}

// TestPhase_BudgetSurvivesPhaseChanges is the anti-oscillation property.
func TestPhase_BudgetSurvivesPhaseChanges(t *testing.T) {
	s := &State{Revision: 1}
	if err := s.StartEpisode("EP-1", EpisodeBudget{
		ReportAttempts: 2, PlanAttempts: 2, RepairIterations: 1,
		CompletionAudits: 1, WallSeconds: 3600,
	}, time.Unix(1791500000, 0).UTC()); err != nil {
		t.Fatalf("start: %v", err)
	}
	now := time.Unix(1791500000, 0).UTC()
	if err := s.ConsumeEpisodeBudget("reports", now); err != nil {
		t.Fatalf("first report: %v", err)
	}
	// Oscillate through phases; the ROOT allowance must not reset.
	for i := 0; i < 3; i++ {
		_ = s.AdvancePhase(PhaseReportReview, "r", now, nil)
		_ = s.AdvancePhase(PhasePlan, "p", now, nil)
	}
	if err := s.ConsumeEpisodeBudget("reports", now); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if err := s.ConsumeEpisodeBudget("reports", now); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("a third report was allowed after %d attempts: %v", s.Episode.Budget.ReportAttempts, err)
	}
}

// TestPhase_CompletionAuditsAreBoundedSeparately stops a request buying more
// audits by repairing more.
func TestPhase_CompletionAuditsAreBoundedSeparately(t *testing.T) {
	s := newEpisode(t)
	now := time.Unix(1791500000, 0).UTC()
	if err := s.ConsumeEpisodeBudget("completion_audits", now); err != nil {
		t.Fatalf("first audit: %v", err)
	}
	if err := s.ConsumeEpisodeBudget("repairs", now); err != nil {
		t.Fatalf("repair: %v", err)
	}
	// Repairs must not buy extra audits: the allowance is its own.
	if err := s.ConsumeEpisodeBudget("completion_audits", now); err != nil {
		t.Fatalf("second audit: %v", err)
	}
	if err := s.ConsumeEpisodeBudget("completion_audits", now); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("a third completion audit was allowed despite repairs: %v", err)
	}
}

// TestPhase_PlanEditIsNewVersionRequiringAcceptance keeps every issued effect
// attributable to the plan it was issued under.
func TestPhase_PlanEditIsNewVersionRequiringAcceptance(t *testing.T) {
	s := newEpisode(t)
	acceptPlan(t, s, 1)
	first := s.Episode.PlanDigest
	revised := contractPlanForTest(2)
	s.Episode.PlanDigest, _ = revised.Digest()
	s.InvalidatePlan("revised proposal")
	acceptPlan(t, s, 2)
	if s.Episode.PlanDigest == first {
		t.Error("a revised plan did not change the pinned digest")
	}
	if s.Episode.PlanVersion != 2 {
		t.Errorf("plan version = %d, want 2", s.Episode.PlanVersion)
	}
}

// TestPhase_InvalidatedPlanCannotBeReentered closes the moved-baseline hole.
func TestPhase_InvalidatedPlanCannotBeReentered(t *testing.T) {
	s := newEpisode(t)
	acceptPlan(t, s, 1)
	now := time.Unix(1791500000, 0).UTC()
	_ = s.AdvancePhase(PhaseReportReview, "r", now, nil)
	_ = s.AdvancePhase(PhasePlan, "p", now, nil)
	_ = s.AdvancePhase(PhasePlanAccept, "a", now, nil)

	s.InvalidatePlan("the source baseline moved")
	if s.Episode.PlanAuthorized {
		t.Fatal("an invalidated plan is still authorised")
	}
	if !strings.Contains(s.Episode.InvalidatedReason, "baseline") {
		t.Errorf("reason = %q, want it to explain the invalidation", s.Episode.InvalidatedReason)
	}
	if err := s.AdvancePhase(PhaseImplement, "work", now, nil); !errors.Is(err, ErrPhaseNotAllowed) {
		t.Errorf("implementation proceeded on an invalidated plan: %v", err)
	}
}

// TestPhase_AuthorizationNamesTheDigestNotTrustsIt refuses a digest that does not
// match the plan's own bytes.
func TestPhase_AuthorizationNamesTheDigestNotTrustsIt(t *testing.T) {
	s := newEpisode(t)
	plan := contractPlanForTest(1)
	auth := authForTest(plan)
	auth.PlanDigest = strings.Repeat("9", 64)
	if err := s.AuthorizePlan(auth, &plan, time.Unix(1791500000, 0).UTC()); err == nil {
		t.Fatal("a plan was authorized against a digest that is not its own")
	}
}

// TestPhase_HistoryRecordsReturns proves the episode can explain itself.
func TestPhase_HistoryRecordsReturns(t *testing.T) {
	s := newEpisode(t)
	now := time.Unix(1791500000, 0).UTC()
	_ = s.AdvancePhase(PhaseReportReview, "reviewed", now, nil)
	_ = s.AdvancePhase(PhasePlan, "planned", now, nil)
	if len(s.Episode.PhaseHistory) < 3 {
		t.Fatalf("history = %d entries, want the start plus two moves", len(s.Episode.PhaseHistory))
	}
	if s.Episode.PhaseHistory[1].From != string(PhaseReport) {
		t.Errorf("first move recorded from %q", s.Episode.PhaseHistory[1].From)
	}
}
