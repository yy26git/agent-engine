package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

func acceptedPlanResumeFixture(t *testing.T, id string) (*fixture, *Engine, *fakeRuntime) {
	t.Helper()
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	e, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(e.Close)

	report := goodEpisodeReport()
	report.EpisodeID = id
	reportDigest, err := e.RecordReportArtifact(id, &report)
	if err != nil {
		t.Fatalf("record report: %v", err)
	}
	baseline, err := TakeSnapshot(filepath.Join(f.srcRoot, "project"), f.now, f.cfg.StoreRoot)
	if err != nil {
		t.Fatalf("measure plan baseline: %v", err)
	}
	plan := contractPlanForTest(1)
	plan.EpisodeID = id
	plan.AcceptedReportDigest = reportDigest
	plan.SourceBaseline = baseline.ContentDigest
	plan.ExecutionProfile = "impl"
	plan.ReviewProfile = "audit"
	if _, err := e.ProposePlan(id, &plan); err != nil {
		t.Fatalf("propose plan: %v", err)
	}
	auth := authForTest(plan)
	auth.EpisodeID = id
	auth.AcceptedBy = "operator:resume-test"
	if _, err := e.AcceptEpisodePlan(id, auth, &plan); err != nil {
		t.Fatalf("accept plan: %v", err)
	}
	return f, e, rt
}

func TestEpisodeResume_ReapsBeforeMeasuringAndDoesNotAdmitTransport(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-RESUME-PENDING-SPAWN"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	if _, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.OwnedProcesses = map[string]OwnedProcess{"minimax-acp": {Pending: true, RecordedAt: "2026-10-14T00:00:00Z"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "before owned-process quiescence") {
		t.Fatalf("resume measured source without first proving process quiescence: %v", err)
	}
	state := mustState(t, f, id)
	if state.EpisodeResumeValidation != nil {
		t.Fatal("source measurement was committed despite unresolved process ownership")
	}
	if len(rt.createdIDs) != 0 {
		t.Fatalf("resume admitted a transport before recovery: %v", rt.createdIDs)
	}
}

func TestEpisodeResume_UnchangedAcceptedPlanBaselineKeepsAuthorization(t *testing.T) {
	f, e, _ := acceptedPlanResumeFixture(t, "EP-RESUME-UNCHANGED")
	validation, err := e.RevalidateEpisodeResume(context.Background(), "EP-RESUME-UNCHANGED")
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, "EP-RESUME-UNCHANGED")
	if validation.Blocked || validation.DriftObserved || validation.ComparedTo != "accepted_plan_baseline" {
		t.Fatalf("unchanged plan baseline did not pass revalidation: %+v", validation)
	}
	if validation.ContentSHA256 != state.Episode.PlanBaseline.ContentDigest || !state.Episode.PlanAuthorized {
		t.Fatalf("unchanged source moved or revoked the accepted plan: validation=%+v episode=%+v", validation, state.Episode)
	}
	if state.EpisodeResumeValidation == nil || state.EpisodeResumeValidation.SnapshotID != validation.SnapshotID {
		t.Fatal("fresh source measurement was not durably recorded")
	}
}

func TestEpisodeResume_PlanBaselineDriftStartsReadOnlyReinvestigation(t *testing.T) {
	f, e, rt := acceptedPlanResumeFixture(t, "EP-RESUME-DRIFT")
	id := "EP-RESUME-DRIFT"
	workspace := filepath.Join(f.srcRoot, "project")
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n// changed after plan acceptance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := mustState(t, f, "EP-RESUME-DRIFT")
	createdBefore, effectsBefore := len(rt.createdIDs), len(before.Effects)
	validation, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	if validation.Blocked || !validation.DriftObserved || !validation.PlanAuthorizationRevoked || validation.Resolution != "reinvestigation_started" {
		t.Fatalf("changed pre-implementation source did not restart read-only investigation: %+v", validation)
	}
	if state.Episode.Phase != string(PhaseReport) || state.Episode.PlanAuthorized || state.Episode.PlanDigest != "" || state.Episode.ReportDigest != "" || state.Episode.PlanBaseline != nil {
		t.Fatalf("source drift did not clear active source-dependent artifacts and return to REPORT: %+v", state.Episode)
	}
	if !strings.Contains(state.Episode.InvalidatedReason, "source drift") || len(state.Episode.SourceInvalidations) != 1 {
		t.Fatalf("source invalidation was not durably recorded: %+v", state.Episode.SourceInvalidations)
	}
	invalidated := state.Episode.SourceInvalidations[0]
	if invalidated.InvalidatedReportSHA256 != before.Episode.ReportDigest || invalidated.InvalidatedPlanSHA256 != before.Episode.PlanDigest || !invalidated.PlanWasAuthorized {
		t.Fatalf("source invalidation does not preserve prior report/plan authority identities: %+v", invalidated)
	}
	if state.Episode.ReinvestigationBaseline == nil || state.Episode.ReinvestigationBaseline.ContentDigest != validation.ContentSHA256 {
		t.Fatalf("new measured source was not pinned for re-investigation: %+v", state.Episode.ReinvestigationBaseline)
	}
	if len(state.Episode.Execution.Steps) != len(before.Episode.Execution.Steps) {
		t.Fatal("drift erased or rewrote execution-step history")
	}
	for _, move := range state.Episode.PhaseHistory {
		if move.Cause == "SOURCE_DRIFT_REINVESTIGATION" && (move.To != string(PhaseReport) || !move.Return) {
			t.Fatalf("source-drift phase move is not an explicit return to read-only REPORT: %+v", move)
		}
	}
	if len(state.Episode.PhaseHistory) == 0 || state.Episode.PhaseHistory[len(state.Episode.PhaseHistory)-1].Cause != "SOURCE_DRIFT_REINVESTIGATION" {
		t.Fatal("phase history does not end with explicit source-drift reinvestigation")
	}
	for _, oldDigest := range []string{before.Episode.ReportDigest, before.Episode.PlanDigest} {
		if _, err := e.ReadEpisodeArtifact(id, oldDigest); err != nil {
			t.Fatalf("invalidated artifact %s was not retained: %v", brief(oldDigest), err)
		}
	}
	oldPlanBytes, err := e.ReadEpisodeArtifact(id, before.Episode.PlanDigest)
	if err != nil {
		t.Fatal(err)
	}
	var oldPlan contract.ImplementationPlan
	if err := json.Unmarshal(oldPlanBytes, &oldPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ProposePlan(id, &oldPlan); err == nil {
		t.Fatal("old report/plan pair was reusable after source drift")
	}
	if len(rt.createdIDs) != createdBefore || len(state.Effects) != effectsBefore {
		t.Fatal("drift revalidation dispatched work or opened a replacement session")
	}

	wrongReport := goodEpisodeReport()
	wrongReport.EpisodeID = id
	if _, err := e.RecordReportArtifact(id, &wrongReport); err == nil {
		t.Fatal("report not bound to the newly measured source was accepted")
	}
	newReport := goodEpisodeReport()
	newReport.EpisodeID = id
	newReport.Attempt = 2
	newReport.SourceDigest = state.Episode.ReinvestigationBaseline.ContentDigest
	newReportDigest, err := e.RecordReportArtifact(id, &newReport)
	if err != nil {
		t.Fatalf("new report against re-investigation baseline was refused: %v", err)
	}
	out, err := e.DriveEpisode(context.Background(), id, "")
	if err != nil || out.Advanced != string(PhaseReportReview) {
		t.Fatalf("fresh report did not require its own independent review: outcome=%+v err=%v", out, err)
	}
	state = mustState(t, f, id)
	if state.ActiveReview == nil || state.ActiveReview.Kind != ReviewCheckpoint {
		t.Fatalf("new report review was not opened: %+v", state.ActiveReview)
	}
	newReportReviewID := state.ActiveReview.ReviewID
	if _, err := f.store.UpdateCommitted(id, func(s *State) error {
		if s.ActiveReview == nil || s.ActiveReview.ReviewID != newReportReviewID {
			return fmt.Errorf("fresh report review changed before fixture settlement")
		}
		s.ActiveReview = nil
		settled := false
		for i := range s.Episode.Execution.Reviews {
			if s.Episode.Execution.Reviews[i].ReviewID == newReportReviewID {
				s.Episode.Execution.Reviews[i].Settled = true
				s.Episode.Execution.Reviews[i].Outcome = string(OutcomeContinue)
				s.Episode.Execution.Reviews[i].SettledAt = f.now.UTC().Format(time.RFC3339Nano)
				settled = true
			}
		}
		if !settled {
			return fmt.Errorf("fresh report review record is missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err = e.DriveEpisode(context.Background(), id, "")
	if err != nil || out.Advanced != string(PhasePlan) {
		t.Fatalf("episode did not wait for and then pass its new review before planning: outcome=%+v err=%v", out, err)
	}
	newPlan := contractPlanForTest(before.Episode.PlanVersion + 1)
	newPlan.EpisodeID = id
	newPlan.AcceptedReportDigest = newReportDigest
	newPlan.SourceBaseline = state.Episode.ReinvestigationBaseline.ContentDigest
	newPlan.ExecutionProfile, newPlan.ReviewProfile = "impl", "audit"
	newPlanDigest, err := e.ProposePlan(id, &newPlan)
	if err != nil {
		t.Fatalf("new plan against new report/source was refused: %v", err)
	}
	state = mustState(t, f, id)
	if state.Episode.PlanAuthorized || state.Episode.PlanDigest != newPlanDigest || state.Episode.ReportDigest != newReportDigest {
		t.Fatalf("new artifacts did not remain a proposed, unaccepted pair: %+v", state.Episode)
	}

	// Exercise the normal phase gate up to local acceptance. No worker-phase
	// instruction/effect may be emitted while acceptance is absent.
	out, err = e.DriveEpisode(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != string(PhasePlanAccept) {
		t.Fatalf("new plan did not reach local acceptance: %+v", out)
	}
	out, err = e.DriveEpisode(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != string(PhasePlanAccept) || !strings.Contains(out.Blocked, "awaiting local plan acceptance") {
		t.Fatalf("unaccepted successor plan did not wait at PLAN_ACCEPT: %+v", out)
	}
	state = mustState(t, f, id)
	if state.Episode.PlanAuthorized {
		t.Fatal("plan became authorized without local acceptance")
	}
	for _, step := range state.Episode.Execution.Steps {
		if step.Phase == string(PhaseImplement) {
			t.Fatalf("implementation step began before exact local plan acceptance: %+v", step)
		}
	}
	for _, effect := range state.Effects {
		if strings.Contains(effect.Reason, string(PhaseImplement)) || effect.Role == string(RoleWorker) {
			t.Fatalf("implementation instruction/effect was emitted before exact local plan acceptance: %+v", effect)
		}
	}
	if _, err := f.store.UpdateCommitted(id, func(s *State) error {
		return s.AdvancePhase(PhaseImplement, "test must not bypass acceptance", f.now, nil)
	}); err == nil {
		t.Fatal("phase machine allowed implementation before local acceptance")
	}
	auth := authForTest(newPlan)
	auth.EpisodeID = id
	auth.AcceptedBy = "operator:resume-test-successor"
	if _, err := e.AcceptEpisodePlan(id, auth, &newPlan); err != nil {
		t.Fatalf("exact successor plan acceptance was refused: %v", err)
	}
	state = mustState(t, f, id)
	if !state.Episode.PlanAuthorized || state.Episode.PlanBaseline == nil || state.Episode.PlanBaseline.ContentDigest != newPlan.SourceBaseline || state.Episode.ReinvestigationBaseline != nil {
		t.Fatalf("accepted successor did not bind to fresh source baseline: %+v", state.Episode)
	}
}

func TestEpisodeResume_SecondPreImplementationDriftRestartsAndPreservesInvalidationHistory(t *testing.T) {
	f, e, rt := acceptedPlanResumeFixture(t, "EP-RESUME-DRIFT-TWICE")
	id := "EP-RESUME-DRIFT-TWICE"
	workspace := filepath.Join(f.srcRoot, "project")
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n// drift one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil || first.Blocked {
		t.Fatalf("first source drift did not start investigation: validation=%+v err=%v", first, err)
	}
	firstState := mustState(t, f, id)
	firstBaseline := firstState.Episode.ReinvestigationBaseline.ContentDigest
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n// drift two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := e.DriveEpisode(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(out.Blocked, "changed again") {
		t.Fatalf("REPORT dispatched against bytes newer than its pinned reinvestigation baseline: %+v", out)
	}
	if len(rt.createdIDs) != 0 {
		t.Fatalf("stale re-investigation source opened a role session: %v", rt.createdIDs)
	}
	second, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil || second.Blocked || second.Resolution != "reinvestigation_started" {
		t.Fatalf("second source drift did not restart the safe route: validation=%+v err=%v", second, err)
	}
	state := mustState(t, f, id)
	if len(state.Episode.SourceInvalidations) != 2 || state.Episode.ReinvestigationGeneration != 2 || state.Episode.ReinvestigationBaseline.ContentDigest == firstBaseline {
		t.Fatalf("second drift did not append a fresh invalidation/baseline: %+v", state.Episode)
	}
	if state.Episode.Budget.Consumed.Reports != 2 || state.Episode.Phase != string(PhaseReport) || len(rt.createdIDs) != 0 {
		t.Fatalf("repeated drift escaped report budget or launched a role: episode=%+v sessions=%v", state.Episode, rt.createdIDs)
	}
}

func TestEpisodeResume_ImplementationDriftBlocksWithoutRevokingPlanOrHistory(t *testing.T) {
	f, e, rt := acceptedPlanResumeFixture(t, "EP-RESUME-IMPLEMENTATION-DRIFT")
	id := "EP-RESUME-IMPLEMENTATION-DRIFT"
	if _, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.Episode.Execution.Steps = append(s.Episode.Execution.Steps, EpisodeStep{
			Key: "IMPLEMENT#1", Phase: string(PhaseImplement), Status: StepDispatched,
			EffectID: "effect-impl-1", At: f.now.UTC().Format(time.RFC3339Nano),
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.srcRoot, "project", "main.go"), []byte("package main\n// changed during implementation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := mustState(t, f, id)
	createdBefore, effectsBefore := len(rt.createdIDs), len(before.Effects)
	validation, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	if !validation.Blocked || validation.PlanAuthorizationRevoked || !state.Episode.PlanAuthorized {
		t.Fatalf("implementation drift should block without guessing attribution or revoking plan: %+v %+v", validation, state.Episode)
	}
	if len(state.Episode.Execution.Steps) != len(before.Episode.Execution.Steps) || len(state.Effects) != effectsBefore || len(rt.createdIDs) != createdBefore {
		t.Fatal("implementation drift changed history or started another worker")
	}
}

func TestEpisodeResume_DriftReapsAndAbandonsUnrecoverableACPProducer(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-RESUME-ACP-ABANDON"
	admitEpisode(t, f, id)
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	})
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("fixture ACP process group did not retire: %v", err)
	}
	installPendingDriftProducer(t, f, id, "minimax-acp:looksee", "acp-old", pid, "", nil)
	if _, _, err := f.store.CreateEpisodeHandover(id); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(f.srcRoot, "project")
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n// source changed during old ACP report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	validation, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	step, ok := state.episodeStep("REPORT#1")
	if !ok || step.Status != StepAbandoned || step.ArtifactDigest != "" || !strings.Contains(step.Reason, "process group were reaped") {
		t.Fatalf("unrecoverable ACP step was not retained as an interrupted read-only step: %+v", step)
	}
	if validation.Blocked || validation.Resolution != "reinvestigation_started" || state.Episode.Phase != string(PhaseReport) || len(state.Episode.SourceInvalidations) != 1 {
		t.Fatalf("proven ACP retirement did not unblock only the bounded REPORT restart: validation=%+v episode=%+v", validation, state.Episode)
	}
	if len(rt.createdIDs) != 0 || len(state.Effects) != 1 {
		t.Fatalf("resume launched or instructed a replacement before report dispatch: sessions=%v effects=%+v", rt.createdIDs, state.Effects)
	}
}

func TestEpisodeResume_DriftRetainsCorrelatedACPReceiptBeforeRestart(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-RESUME-ACP-RECEIPT"
	admitEpisode(t, f, id)
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	})
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
	}
	report := goodEpisodeReport()
	report.EpisodeID = id
	envelope, err := json.Marshal(EpisodeArtifactEnvelope{
		RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "old-report-nonce", Generation: 1, Report: &report,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := &EpisodeTurnReceipt{StepKey: "REPORT#1", Backend: "minimax-acp:looksee", Session: "acp-old", SendID: "acp-send-1", Text: string(envelope)}
	installPendingDriftProducer(t, f, id, receipt.Backend, receipt.Session, pid, "", receipt)
	if _, _, err := f.store.CreateEpisodeHandover(id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.srcRoot, "project", "main.go"), []byte("package main\n// source changed after ACP report dispatch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	validation, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	step, ok := state.episodeStep("REPORT#1")
	if !ok || step.Status != StepDispatched || step.ArtifactDigest == "" {
		t.Fatalf("valid receipt did not remain as retained old evidence: %+v", step)
	}
	if len(state.Episode.Execution.TurnReceipts) != 1 || !state.Episode.Execution.TurnReceipts[0].Consumed {
		t.Fatalf("correlated ACP receipt was not acknowledged after retention: %+v", state.Episode.Execution.TurnReceipts)
	}
	if validation.Blocked || state.Episode.Phase != string(PhaseReport) || len(state.Episode.SourceInvalidations) != 1 || state.Episode.SourceInvalidations[0].InvalidatedReportSHA256 != step.ArtifactDigest {
		t.Fatalf("old receipt was not archived by the source-drift restart: validation=%+v episode=%+v step=%+v", validation, state.Episode, step)
	}
	if len(rt.createdIDs) != 0 || len(state.Effects) != 1 {
		t.Fatalf("receipt recovery launched a replacement or implementation instruction: sessions=%v effects=%+v", rt.createdIDs, state.Effects)
	}
}

func TestEpisodeResume_DriftKeepsUncorrelatedAgentDeckProducerBlocked(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-RESUME-AGENT-DECK-PENDING"
	admitEpisode(t, f, id)
	installPendingDriftProducer(t, f, id, "agent-deck", "deck-report-old", 0, "", nil)
	if _, _, err := f.store.CreateEpisodeHandover(id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.srcRoot, "project", "main.go"), []byte("package main\n// source changed during Agent Deck report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	validation, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	step, ok := state.episodeStep("REPORT#1")
	if !ok || step.Status != StepDispatched || step.ArtifactDigest != "" {
		t.Fatalf("Agent Deck producer was abandoned without correlated completion evidence: %+v", step)
	}
	if !validation.Blocked || validation.Resolution != "awaiting_readonly_quiescence" || !strings.Contains(validation.Reason, "keep that same chat available") || !strings.Contains(validation.Reason, "cannot safely import Agent Deck chat history") || !strings.Contains(validation.Reason, "without that receipt the episode remains blocked") || !strings.Contains(validation.Reason, "will not start or rebind a replacement") {
		t.Fatalf("Agent Deck recovery did not give a concrete same-session next action: %+v", validation)
	}
	if state.Episode.Phase != string(PhaseReport) || len(state.Episode.SourceInvalidations) != 0 || len(rt.createdIDs) != 0 || len(state.Effects) != 1 {
		t.Fatalf("blocked Agent Deck recovery mutated phase/history or launched work: phase=%s invalidations=%d sessions=%v effects=%d", state.Episode.Phase, len(state.Episode.SourceInvalidations), rt.createdIDs, len(state.Effects))
	}
}

func TestEpisodeResume_DriftConsumesCorrelatedAgentDeckReceiptBeforeRestart(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-RESUME-AGENT-DECK-RECEIPT"
	admitEpisode(t, f, id)
	report := goodEpisodeReport()
	report.EpisodeID = id
	envelope, err := json.Marshal(EpisodeArtifactEnvelope{
		RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "old-report-nonce", Generation: 1, Report: &report,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := &EpisodeTurnReceipt{StepKey: "REPORT#1", Backend: "agent-deck", Session: "deck-report-old", SendID: "acp-send-1", Text: string(envelope)}
	installPendingDriftProducer(t, f, id, receipt.Backend, receipt.Session, 0, "", receipt)
	if _, _, err := f.store.CreateEpisodeHandover(id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.srcRoot, "project", "main.go"), []byte("package main\\n// source changed after Agent Deck report dispatch\\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	validation, err := e.RevalidateEpisodeResume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	step, ok := state.episodeStep("REPORT#1")
	if !ok || step.Status != StepDispatched || step.ArtifactDigest == "" {
		t.Fatalf("valid Agent Deck receipt was not retained before drift restart: %+v", step)
	}
	if len(state.Episode.Execution.TurnReceipts) != 1 || !state.Episode.Execution.TurnReceipts[0].Consumed {
		t.Fatalf("correlated Agent Deck receipt was not acknowledged: %+v", state.Episode.Execution.TurnReceipts)
	}
	if validation.Blocked || validation.Resolution != "reinvestigation_started" || state.Episode.Phase != string(PhaseReport) || len(state.Episode.SourceInvalidations) != 1 || state.Episode.SourceInvalidations[0].InvalidatedReportSHA256 != step.ArtifactDigest {
		t.Fatalf("Agent Deck receipt was not archived before bounded restart: validation=%+v episode=%+v step=%+v", validation, state.Episode, step)
	}
	if len(rt.createdIDs) != 0 {
		t.Fatalf("receipt recovery launched a replacement session: %v", rt.createdIDs)
	}
}

func installPendingDriftProducer(t *testing.T, f *fixture, id, backend, session string, pid int, startToken string, receipt *EpisodeTurnReceipt) {
	t.Helper()
	_, err := f.store.UpdateCommitted(id, func(state *State) error {
		if state.BackendBindings == nil {
			state.BackendBindings = make(map[ProfileRole]SessionBinding)
		}
		state.BackendBindings[RoleInvestigat] = SessionBinding{
			Backend: backend, SessionID: session, ProcessPID: pid, ProcessStart: startToken,
			Generation: state.Generation, Resolved: true,
		}
		if pid > 0 {
			state.OwnedProcesses = map[string]OwnedProcess{backend: {PID: pid, StartToken: startToken}}
		}
		step := EpisodeStep{
			Key: "REPORT#1", Phase: string(PhaseReport), Round: 1, Role: string(RoleInvestigat),
			Profile: "looksee", Session: session, Generation: state.Generation,
			EffectID: "effect-old-report", ArtifactNonce: "old-report-nonce", Status: StepDispatched,
			At: f.now.UTC().Format(time.RFC3339Nano),
		}
		state.recordEpisodeStep(step)
		state.Effects = append(state.Effects, Effect{
			EffectID: step.EffectID, Role: string(RoleInvestigat), SessionID: session,
			SendID: "acp-send-1", Status: EffectDelivered, Reason: "REPORT#1",
		})
		if receipt != nil {
			state.Episode.Execution.TurnReceipts = append(state.Episode.Execution.TurnReceipts, *receipt)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEpisodeHandover_CapturesMeasuredSourceAndRefusesSilentRebase(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-HANDOVER-SOURCE"
	admitEpisode(t, f, id)
	workspace := filepath.Join(f.srcRoot, "project")
	measured, err := measureStableEpisodeSource(workspace, f.cfg.StoreRoot, f.now)
	if err != nil {
		t.Fatal(err)
	}
	handover, digest, err := f.store.CreateEpisodeHandover(id)
	if err != nil {
		t.Fatal(err)
	}
	if handover.Source.MeasuredSnapshot == nil || handover.Source.MeasuredSnapshot.ContentSHA256 != measured.ContentDigest || len(digest) != 64 {
		t.Fatalf("handover lacks the stable measured source identity: %+v", handover.Source)
	}
	state := mustState(t, f, id)
	if state.EpisodeHandoverDigest != digest {
		t.Fatalf("handover pointer was not committed: %q != %q", state.EpisodeHandoverDigest, digest)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n// source moved after handover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.CreateEpisodeHandover(id); err == nil || !strings.Contains(err.Error(), "cannot bless new bytes") {
		t.Fatalf("new handover silently rebased drifted source: %v", err)
	}
}
