package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

type reviewLifecycleRuntime struct {
	*fakeRuntime
	reviewer string
	status   string
}

func (r *reviewLifecycleRuntime) Inspect(ctx context.Context, id string) (agentdeck.Session, error) {
	if id == r.reviewer && r.status != "" {
		r.mu.Lock()
		stopped := false
		for _, s := range r.stopped {
			if s == id {
				stopped = true
			}
		}
		r.mu.Unlock()
		if !stopped {
			return agentdeck.Session{ID: id, Status: r.status}, nil
		}
	}
	return r.fakeRuntime.Inspect(ctx, id)
}
func liveReviewFixture(t *testing.T) (*fixture, *Engine, *reviewLifecycleRuntime, string) {
	t.Helper()
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	id := "REVIEW-LIFECYCLE"
	admitEpisode(t, f, id)
	rt := &reviewLifecycleRuntime{fakeRuntime: newFakeRuntime()}
	e, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }, Log: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	report := goodEpisodeReport()
	report.EpisodeID = id
	if _, err = e.RecordReportArtifact(id, &report); err != nil {
		t.Fatal(err)
	}
	drive(t, e, id, "standard_fix")
	s := mustState(t, f, id)
	if s.ActiveReview == nil {
		t.Fatal("fixture did not open actual review")
	}
	rt.reviewer = s.ActiveReview.ExpectedReviewer
	return f, e, rt, id
}
func TestReviewLifecycle_BlockedSendIsActionableWithoutResend(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	before := len(rt.sent)
	if _, err := f.store.UpdateCommitted(id, func(s *State) error {
		for i := range s.Effects {
			if s.Effects[i].SessionID == rt.reviewer {
				s.Effects[i].Status = EffectBlocked
				s.Effects[i].LastError = "delivery unproven"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.DriveTaskLifecycle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	s := mustState(t, f, id)
	if !s.ProgressSnapshot().RequiresAction {
		t.Fatal("blocked delivery was silently treated as waiting")
	}
	if len(rt.sent) != before {
		t.Fatal("unproven prompt was resent")
	}
}
func TestReviewLifecycle_ExitedWithoutAnswerBlocks(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.status = "error"
	if err := e.DriveTaskLifecycle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !mustState(t, f, id).ProgressSnapshot().RequiresAction {
		t.Fatal("exited reviewer was silently treated as waiting")
	}
}
func TestReviewLifecycle_DeadlineSurvivesRestart(t *testing.T) {
	f, first, rt, id := liveReviewFixture(t)
	s := mustState(t, f, id)
	if s.ActiveReview.Deadline == "" {
		t.Fatal("review has no enforced deadline")
	}
	deadline := s.ActiveReview.Deadline
	sent := len(rt.sent)
	first.Close()
	f.now = f.now.Add(time.Minute)
	restarted, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }, Log: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if err := restarted.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s = mustState(t, f, id)
	if s.ActiveReview.Deadline != deadline {
		t.Fatal("deadline changed")
	}
	at, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		t.Fatal(err)
	}
	f.now = at.Add(time.Second)
	if err := restarted.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s = mustState(t, f, id)
	if s.RuntimeBlock == nil || s.RuntimeBlock.Kind != "review-timeout" || s.Stage != StageBlocked {
		t.Fatal("restart failed to enforce the original deadline", s.RuntimeBlock, s.Stage)
	}
	if len(rt.sent) != sent {
		t.Fatal("restart resent the review")
	}
}

func retainedReviewAnswer(t *testing.T, f *fixture, rt *reviewLifecycleRuntime, id string) {
	t.Helper()
	s := mustState(t, f, id)
	body, err := json.Marshal(ReviewEnvelope{RequestID: id, Kind: s.ActiveReview.Kind, ReviewID: s.ActiveReview.ReviewID, TaskID: s.TaskID, Generation: s.ActiveReview.Generation, AnswerNonce: s.ActiveReview.AnswerNonce, Outcome: OutcomeContinue, ReportBody: "independent retained answer"})
	if err != nil {
		t.Fatal(err)
	}
	rt.outputs[rt.reviewer] = string(body)
}
func TestReviewLifecycle_ExitedWithValidAnswerIsCollected(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.status = "error"
	retainedReviewAnswer(t, f, rt, id)
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s := mustState(t, f, id)
	if s.RuntimeBlock != nil || s.ActiveReview != nil {
		t.Fatal("valid retained answer lost to death classification")
	}
	rec := s.episodeReviewFor(PhaseReportReview, 1)
	if rec == nil || !rec.Settled {
		t.Fatal("real review did not settle")
	}
}
func TestReviewLifecycle_PartialAnswerRemainsPending(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.outputs[rt.reviewer] = "still investigating"
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if mustState(t, f, id).RuntimeBlock != nil {
		t.Fatal("unfinished running answer classified as failure")
	}
}
func TestReviewLifecycle_FinalInvalidAnswerBlocks(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.status = "error"
	rt.outputs[rt.reviewer] = "PASS"
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s := mustState(t, f, id)
	if s.RuntimeBlock == nil || s.RuntimeBlock.Kind != "review-invalid" || s.ResultStatus == "PASS" {
		t.Fatal("uncorrelated final token did not block")
	}
}
func TestReviewLifecycle_UnreadableRunningReviewerHasDeadline(t *testing.T) {
	f, e, _, id := liveReviewFixture(t)
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if mustState(t, f, id).RuntimeBlock != nil {
		t.Fatal("temporary read error prematurely blocked")
	}
	deadline, err := time.Parse(time.RFC3339, mustState(t, f, id).ActiveReview.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	f.now = deadline
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s := mustState(t, f, id)
	if s.RuntimeBlock == nil {
		t.Fatal("deadline did not fire")
	}
}
func TestReviewLifecycle_RootBudgetExpiresWhileIdle(t *testing.T) {
	f, e, _, id := liveReviewFixture(t)
	s := mustState(t, f, id)
	start, _ := time.Parse(time.RFC3339, s.Episode.StartedAt)
	f.now = start.Add(time.Duration(s.Episode.Budget.WallSeconds) * time.Second)
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	s = mustState(t, f, id)
	if s.RuntimeBlock == nil || s.RuntimeBlock.Kind != "episode-timeout" {
		t.Fatal("idle root budget was not enforced")
	}
}

func TestReviewLifecycle_ExpiredContextHasFreshRecovery(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	s := mustState(t, f, id)
	start, _ := time.Parse(time.RFC3339, s.Episode.StartedAt)
	f.now = start.Add(time.Duration(s.Episode.Budget.WallSeconds) * time.Second)
	before := len(rt.sent)
	execution, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := e.Run(execution, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired execution context did not stop", err)
	}
	if handled, err := e.ReconcileExpiredEpisode(t.Context(), id); err != nil || !handled {
		t.Fatal("deadline recovery was not handled", err)
	}
	s = mustState(t, f, id)
	if s.Stage != StageBlocked || s.RuntimeBlock == nil || s.RuntimeBlock.Kind != "episode-timeout" || len(rt.stopped) == 0 || len(rt.sent) != before {
		t.Fatal("fresh deadline recovery failed to stop owned work without dispatch", s.Stage, s.RuntimeBlock)
	}
}
func TestReviewLifecycle_DryRunDoesNotMutate(t *testing.T) {
	f, e, _, id := liveReviewFixture(t)
	e.opts.DryRun = true
	before := mustState(t, f, id).Revision
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if mustState(t, f, id).Revision != before {
		t.Fatal("dry-run wrote lifecycle state")
	}
}
func TestCancellationLifecycle_SameFailureDoesNotChurn(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.aliveAfterStop = true
	if _, err := f.store.UpdateCommitted(id, func(s *State) error { s.CancellationRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	before := mustState(t, f, id)
	if before.RuntimeBlock == nil || before.RuntimeBlock.Kind != "cleanup-unresolved" {
		t.Fatal("precondition: cleanup did not block")
	}
	for i := 0; i < 12; i++ {
		f.now = f.now.Add(2 * time.Second)
		if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	after := mustState(t, f, id)
	if after.Progress.Latest != before.Progress.Latest {
		t.Fatal("unchanged failure generated notifications")
	}
	if after.CancellationReason != before.CancellationReason {
		t.Fatal("unchanged cleanup rewrote its cancellation reason")
	}
	if after.Stage == StageCancelled {
		t.Fatal("live session called cancelled")
	}
}

func TestReviewLifecycle_CleanupRecoveryRetainsOriginalFailure(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.status = "error"
	rt.aliveAfterStop = true
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	blocked := mustState(t, f, id)
	if blocked.RuntimeBlock == nil || blocked.RuntimeBlock.Kind != "cleanup-unresolved" || blocked.RuntimeBlock.CauseKind != "reviewer-unavailable" {
		t.Fatal("original reviewer failure was lost during cleanup", blocked.RuntimeBlock)
	}
	cause := blocked.RuntimeBlock.CauseReason
	rt.aliveAfterStop = false
	f.now = f.now.Add(3 * time.Second)
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	settled := mustState(t, f, id)
	if settled.Stage != StageBlocked || settled.StageReason != cause || settled.RuntimeBlock.Kind != "reviewer-unavailable" || settled.Cleanup != nil {
		t.Fatal("cleanup recovery did not retain the actual failure", settled.Stage, settled.StageReason, settled.RuntimeBlock)
	}
	if settled.ProgressSnapshot().ActionKind != "inspect-result" {
		t.Fatal("settled cleanup still requested cleanup recovery")
	}
}
func TestCancellationLifecycle_DeadlineStopsAutomaticAttempts(t *testing.T) {
	f, e, rt, id := liveReviewFixture(t)
	rt.aliveAfterStop = true
	if _, err := f.store.UpdateCommitted(id, func(s *State) error { s.CancellationRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	calls := len(rt.stopped)
	f.now = f.now.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		if err := e.DriveTaskLifecycle(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(rt.stopped) != calls || !mustState(t, f, id).Cleanup.Exhausted {
		t.Fatal("automatic cleanup exceeded its deadline")
	}
	rt.aliveAfterStop = false
	rep, err := e.ReconcileCancellation(t.Context(), id)
	if err != nil || !rep.Committed {
		t.Fatal("explicit recovery failed", rep, err)
	}
}
func TestProgress_CurrentEffectReasonBeatsAdmission(t *testing.T) {
	f, _, _, id := liveReviewFixture(t)
	s := mustState(t, f, id)
	v := s.ProgressSnapshot()
	if v.Reason == "admitted; no effects started" || v.Phase != "REPORT_REVIEW" {
		t.Fatal("stale reason", v)
	}
}
