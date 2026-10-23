package control

// LF3 episode controller tests.
//
// The claim: an episode progresses WITHOUT the worker ever asking to progress,
// and no phase is entered because the party it judges said so.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

func episodeFixture(t *testing.T) (*fixture, time.Time) {
	t.Helper()
	f := boundRequest(t, newFixture(t))
	now := f.now
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.StartEpisode("EP-1", DefaultEpisodeBudget(), now)
	}); err != nil {
		t.Fatalf("start episode: %v", err)
	}
	return f, now
}

func testReport() *contract.SemanticReport {
	r := goodEpisodeReport()
	return &r
}

func goodEpisodeReport() contract.SemanticReport {
	return contract.SemanticReport{
		Schema: contract.EpisodeVersion, EpisodeID: "EP-1", Attempt: 1,
		Objective: "fix the build", EvidenceDigest: strings.Repeat("a", 64),
		SourceDigest: strings.Repeat("b", 64), ObservedProfile: "investigator",
		Findings: []contract.ConfirmedFinding{{
			FindingID: "F1", RequirementID: "R1",
			Expected: "exit zero", Observed: "exit 2",
			EvidenceRefs: []string{"E1"}, Unknowns: []string{"CI only"},
		}},
	}
}

// TestEpisode_ProgressionNeedsNoRequestFromTheWorker is the central claim.
//
// The worker contributes only artifacts. Every phase move is driven by the
// controller observing committed state.
func TestEpisode_ProgressionNeedsNoRequestFromTheWorker(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	ctx := context.Background()

	// REPORT with nothing recorded: legitimately still investigating.
	out, err := eng.DriveEpisode(ctx, "forge-run-1842-repair", "")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if out.Advanced != "" || !strings.Contains(out.Blocked, "awaiting an investigation report") {
		t.Fatalf("moved with no report: %+v", out)
	}

	// The investigator RECORDS; it does not advance.
	rep := testReport()
	if _, err := eng.RecordReportArtifact("forge-run-1842-repair", rep); err != nil {
		t.Fatalf("record report: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Episode.Phase != string(PhaseReport) {
		t.Fatalf("recording a report advanced the phase on its own: %q", st.Episode.Phase)
	}

	// Now the controller moves it.
	out, err = eng.DriveEpisode(ctx, "forge-run-1842-repair", "")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if out.Advanced != string(PhaseReportReview) {
		t.Fatalf("the controller did not advance to review: %+v", out)
	}
}

// TestEpisode_ProposingAPlanNeverAuthorisesIt is the separation that matters.
func TestEpisode_ProposingAPlanNeverAuthorisesIt(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()

	rep := testReport()
	measured, err := eng.RecordReportArtifact("forge-run-1842-repair", rep)
	if err != nil {
		t.Fatalf("record report: %v", err)
	}
	plan := contractPlanForTest(1)
	// The plan must answer the report's MEASURED digest, not one it invents.
	plan.AcceptedReportDigest = measured
	if _, err := eng.ProposePlan("forge-run-1842-repair", &plan); err != nil {
		t.Fatalf("propose: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Episode.PlanAuthorized {
		t.Fatal("proposing a plan authorised it")
	}
	if st.Episode.PlanDigest == "" {
		t.Error("the proposed plan was not pinned")
	}
	// Implementation must still be refused.
	if _, err := eng.DriveEpisode(context.Background(), "forge-run-1842-repair", ""); err != nil {
		t.Fatalf("drive: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.Episode.Phase == string(PhaseImplement) {
		t.Fatal("the episode reached implementation on a merely drafted plan")
	}
}

// TestEpisode_PlanMustAnswerTheEpisodeOwnsReport stops cross-report reuse.
func TestEpisode_PlanMustAnswerTheEpisodeOwnsReport(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	rep := testReport()
	if _, err := eng.RecordReportArtifact("forge-run-1842-repair", rep); err != nil {
		t.Fatalf("record report: %v", err)
	}
	plan := contractPlanForTest(1)
	plan.AcceptedReportDigest = strings.Repeat("c", 64) // some OTHER report
	if _, err := eng.ProposePlan("forge-run-1842-repair", &plan); err == nil {
		t.Fatal("a plan answering a different report was accepted")
	}
}

// TestEpisode_ReportForAnotherEpisodeIsRefused.
func TestEpisode_ReportForAnotherEpisodeIsRefused(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	rep := testReport()
	rep.EpisodeID = "EP-OTHER"
	if _, err := eng.RecordReportArtifact("forge-run-1842-repair", rep); err == nil {
		t.Fatal("a report for another episode was recorded against this one")
	}
}

// TestEpisode_UnknownTemplateBlocksRatherThanFallingBack is the admission rule.
func TestEpisode_UnknownTemplateBlocksRatherThanFallingBack(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	out, err := eng.DriveEpisode(context.Background(), "forge-run-1842-repair", "no-such-template")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !strings.Contains(out.Blocked, "not approved") {
		t.Fatalf("an unknown template did not block: %+v", out)
	}
}

// TestEpisode_CancellationOutranksProgress.
func TestEpisode_CancellationOutranksProgress(t *testing.T) {
	f, _ := episodeFixture(t)
	rep := testReport()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordEpisodeReport(rep); err != nil {
			return err
		}
		s.CancellationRequested = true
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	out, err := eng.DriveEpisode(context.Background(), "forge-run-1842-repair", "")
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if out.Advanced != "" {
		t.Fatalf("the episode advanced while cancellation was pending: %+v", out)
	}
}

// RecordEpisodeReport is a test helper that stores a validated report digest.
func (s *State) RecordEpisodeReport(rep *contract.SemanticReport) error {
	if err := contract.ValidateSemanticReport(rep); err != nil {
		return err
	}
	d, err := rep.Digest()
	if err != nil {
		return err
	}
	if s.Episode == nil {
		return errNoEpisode
	}
	s.Episode.ReportDigest = d
	return nil
}

var errNoEpisode = errors.New("no episode")

// TestEpisode_NonEpisodeRequestIsUntouched keeps the old path working.
func TestEpisode_NonEpisodeRequestIsUntouched(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	out, err := eng.DriveEpisode(context.Background(), "forge-run-1842-repair", "")
	if err != nil {
		t.Fatalf("a non-episode request failed the controller: %v", err)
	}
	if out.EpisodeID != "" || out.Advanced != "" {
		t.Fatalf("the controller acted on a request with no episode: %+v", out)
	}
}
