package control

// Episode phase EXECUTION tests.
//
// The claim under test has one word in it: executes. A controller that advances a
// stored phase without having asked anyone to do anything has produced an episode
// that looks like progress and is not, which is the exact failure the rest of
// this package is built to refuse. So each test here watches for the EFFECT — a
// session bound to the right role, an instruction actually sent to it, a managed
// check actually attempted — and only then for the phase move.
//
// Two properties are asserted everywhere rather than once:
//
//   - FAILURE IS VISIBLE. An unbound role, an unrunnable check, an interrupted
//     verification and an exhausted repair budget each produce a stated reason
//     and a stopped phase, never a quiet advance.
//   - NOTHING LAUNCHES. No test starts a model session, opens a socket, or runs a
//     managed command: every role runs on the scripted fake runtime, and every
//     required check names a binary that is not installed. The guard test at the
//     end asserts that second claim mechanically rather than asking you to
//     believe a comment.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// execTemplate is a complete, runnable episode shape: every role bound, a
// registered required check, and the return edges a bounded repair needs.
func execTemplate(roles map[ProfileRole]string, checks []string,
	edges map[contract.EpisodePhase][]contract.EpisodePhase) EpisodeTemplate {
	t := fullTemplate()
	t.Roles = roles
	t.RequiredChecks = checks
	// A test that asks for bounded repair gets the complete valid route set, then
	// overlays its route of interest. Production rejects partial nonempty route
	// sets because every enabled rejection phase needs an explicit disposition.
	// A nil map still selects fail-stop behavior for tests that are not about repair.
	if edges == nil {
		t.ReturnEdges = nil
	} else {
		t.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{
			contract.PhaseReportReview:     {contract.PhaseReport},
			contract.PhaseImplement:        {contract.PhaseImplement},
			contract.PhaseVerify:           {contract.PhaseImplement},
			contract.PhaseCompletionReview: {contract.PhaseImplement},
		}
		for from, to := range edges {
			t.ReturnEdges[from] = to
		}
	}
	return t
}

// unrunnableCheckID is a required check whose tool is not installed.
//
// Running it produces a real receipt with a real outcome — BLOCKED, because the
// evidence could not be produced — without starting a process. That is what lets
// the verification tests exercise the production path and the "nothing launches"
// rule at the same time.
const unrunnableCheckID = "absent-tool"

// unrunnableCheckTool is the (deliberately nonexistent) tool that check runs.
const unrunnableCheckTool = "agent-engine-no-such-check-binary"

// execFixture admits nothing yet; it registers the profiles and one template
// against a store whose workspace is a real directory, so a snapshot exists.
func execFixture(t *testing.T, tpl EpisodeTemplate) *fixture {
	t.Helper()
	f := newFixture(t)
	f.cfg.Profiles = episodeProfiles()
	if err := f.cfg.EpisodeTemplates.Register(tpl); err != nil {
		t.Fatalf("register template: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(f.srcRoot, "project"), 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.srcRoot, "project", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	// EVERY check the base fixture knows is pointed at a tool that is not
	// installed. The shared completion path re-runs a request's own required
	// checks, so leaving `go build ./...` in the registry would let a test that
	// never meant to launch anything launch a build the moment it reached it.
	for id := range f.cfg.Checks {
		setCheck(t, f, id, []string{unrunnableCheckTool}, false, time.Minute)
	}
	setCheck(t, f, unrunnableCheckID, []string{unrunnableCheckTool}, false, time.Minute)
	return f
}

// admitEpisode admits a real episode through the real admission path, so the role
// binding under test is the one admission actually commits.
func admitEpisode(t *testing.T, f *fixture, id string) {
	t.Helper()
	if _, err := f.store.AdmitEpisode(episodeRequest(f, id)); err != nil {
		t.Fatalf("admit episode: %v", err)
	}
}

func episodeEngine(t *testing.T, f *fixture, rt *fakeRuntime, id string) (*Engine, func()) {
	t.Helper()
	eng, err := NewEngine(f.store, rt, id, EngineOptions{
		Now: func() time.Time { return f.now },
		Log: func(string) {},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng, func() { eng.Close() }
}

func drive(t *testing.T, e *Engine, id, template string) EpisodeOutcome {
	t.Helper()
	out, err := e.DriveEpisode(context.Background(), id, template)
	if err != nil {
		t.Fatalf("drive %s: %v", id, err)
	}
	return out
}

func TestEpisodeExecution_PrepareDefersToPhaseWithoutLegacyRoles(t *testing.T) {
	roles := map[ProfileRole]string{RoleInvestigat: "looksee", RoleReviewer: "audit", RoleWorker: "impl"}
	f := execFixture(t, execTemplate(roles, []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-PREPARE")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-PREPARE")
	defer stop()
	if err := eng.PrepareTaskSessions(context.Background(), "EP-PREPARE"); err != nil {
		t.Fatal(err)
	}
	before := mustState(t, f, "EP-PREPARE")
	if len(rt.createdIDs) != 0 || len(before.Effects) != 0 {
		t.Fatal("legacy preparation launched a role or instructed a worker before plan acceptance")
	}
	drive(t, eng, "EP-PREPARE", "standard_fix")
	after := mustState(t, f, "EP-PREPARE")
	if len(rt.createdIDs) != 1 || len(after.BackendBindings) != 1 || after.BackendBindings[RoleInvestigat].SessionID == "" {
		t.Fatalf("REPORT did not launch only its investigator: %+v", after.BackendBindings)
	}
}

func TestEpisodeProducerFailure_RetainedCorrelatedReportWinsOverStoppedSession(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-REPORT-RETAINED-ON-STOP"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, id)
	defer stop()
	if _, err := eng.DriveEpisode(context.Background(), id, "standard_fix"); err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	step, ok := state.episodeStep("REPORT#1")
	if !ok || step.Session == "" {
		t.Fatalf("REPORT was not dispatched: %+v", step)
	}
	report := goodEpisodeReport()
	report.EpisodeID = id
	envelope, err := json.Marshal(EpisodeArtifactEnvelope{
		RequestID: id, StepKey: step.Key, ArtifactNonce: step.ArtifactNonce,
		Generation: step.Generation, Report: &report,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	rt.outputs[step.Session] = string(envelope)
	rt.stopped = append(rt.stopped, step.Session)
	rt.mu.Unlock()
	if _, err := eng.DriveEpisode(context.Background(), id, "standard_fix"); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, f, id)
	if state.Stage.Terminal() || state.Episode.Stop != nil || state.Episode.ReportDigest == "" {
		t.Fatalf("a valid retained report lost to session termination: stage=%s stop=%+v report=%q", state.Stage, state.Episode.Stop, state.Episode.ReportDigest)
	}
}

func TestEpisodeProducerFailure_StoppedSessionWithoutCorrelatedArtifactBlocks(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-REPORT-STOPPED-NO-ARTIFACT"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, id)
	defer stop()
	if _, err := eng.DriveEpisode(context.Background(), id, "standard_fix"); err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	step, ok := state.episodeStep("REPORT#1")
	if !ok || step.Session == "" {
		t.Fatalf("REPORT was not dispatched: %+v", step)
	}
	rt.mu.Lock()
	rt.stopped = append(rt.stopped, step.Session)
	rt.mu.Unlock()
	if _, err := eng.DriveEpisode(context.Background(), id, "standard_fix"); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, f, id)
	if state.Stage != StageBlocked || state.Episode.Stop == nil || !strings.Contains(state.Episode.Stop.Reason, "no valid correlated REPORT artifact") {
		t.Fatalf("stopped producer without an artifact was not durably classified: stage=%s stop=%+v", state.Stage, state.Episode.Stop)
	}
}

// Exercise Run's ordinary empty-backlog tick, rather than manually calling
// DriveEpisode as the phase-controller fixtures do. A retained report must
// open its independent review without another caller driving the phase.
func TestEpisodeExecution_RunAdvancesRetainedReportToIndependentReview(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-RUN-REVIEW"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	e.opts.Once = true
	report := goodEpisodeReport()
	report.EpisodeID = id
	if _, err := e.RecordReportArtifact(id, &report); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	state := mustState(t, f, id)
	if state.Episode.Phase != string(PhaseReportReview) || state.ActiveReview == nil {
		t.Fatalf("ordinary supervision left a retained report unreviewed: phase=%s review=%+v", state.Episode.Phase, state.ActiveReview)
	}
	if len(sentTo(rt, state.ActiveReview.ExpectedReviewer)) != 1 || state.Episode.PlanAuthorized {
		t.Fatal("supervision must deliver exactly one independent review and cannot authorize repair")
	}
	if _, err := e.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(sentTo(rt, state.ActiveReview.ExpectedReviewer)) != 1 {
		t.Fatal("a second tick duplicated the outstanding review instruction")
	}
}

func TestEpisodeExecution_RunReportsReviewerLaunchRefusal(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-REVIEW-LAUNCH-REFUSAL"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	rt.noInventory = true
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	e.opts.Once = true
	var lines []string
	e.opts.Log = func(line string) { lines = append(lines, line) }
	report := goodEpisodeReport()
	report.EpisodeID = id
	if _, err := e.RecordReportArtifact(id, &report); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.Run(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for _, line := range lines {
		if strings.Contains(line, "no independent reviewer could be given a session") && strings.Contains(line, "session registry is unreachable") {
			count++
		}
	}
	// The first tick enters review; the next reports the unchanged waiting
	// phase. Both distinct reasons are useful, but the third must not repeat it.
	if count != 2 {
		t.Fatalf("launch refusal must be visible without repeating unchanged reasons: %q", lines)
	}
	state := mustState(t, f, id)
	if state.Episode.Phase != string(PhaseReportReview) || state.ActiveReview != nil || len(rt.createdIDs) != 0 {
		t.Fatal("diagnostics must not manufacture a review or launch a replacement")
	}
}

func TestEpisodeExecution_DryRunTickDoesNotAdvanceRetainedReport(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-DRY-RUN-TICK"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	report := goodEpisodeReport()
	report.EpisodeID = id
	if _, err := e.RecordReportArtifact(id, &report); err != nil {
		t.Fatal(err)
	}
	before := mustState(t, f, id)
	e.opts.DryRun = true
	if err := e.driveEpisodeIfPresent(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after := mustState(t, f, id)
	if after.Revision != before.Revision || after.Episode.Phase != string(PhaseReport) || len(rt.createdIDs) != 0 {
		t.Fatal("dry-run episode tick mutated the phase or started an agent")
	}
}

func TestEpisodeExecution_CancellationDoesNotSpawnReplacementTransport(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-CANCEL-STARTUP")
	if err := f.store.Update("EP-CANCEL-STARTUP", func(s *State) error { s.CancellationRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	eng, stop := episodeEngine(t, f, newFakeRuntime(), "EP-CANCEL-STARTUP")
	defer stop()
	if err := eng.InitializeTransports(context.Background(), "EP-CANCEL-STARTUP", nil); err != nil {
		t.Fatal(err)
	}
}

func mustState(t *testing.T, f *fixture, id string) *State {
	t.Helper()
	st, err := f.store.State(id)
	if err != nil {
		t.Fatalf("load state %s: %v", id, err)
	}
	return st
}

// sentTo returns the messages the fake transport actually received for a session.
func sentTo(rt *fakeRuntime, session string) []string {
	var out []string
	for _, line := range rt.sentMessages() {
		if strings.HasPrefix(line, session+": ") {
			out = append(out, strings.TrimPrefix(line, session+": "))
		}
	}
	return out
}

// answerAsReviewer scripts the reviewer's own session output and settles the open
// review through the canonical ingress and the canonical validator. Nothing here
// asserts an outcome: the answer is read back from the session the control layer
// actually queried, which is what makes it attributable.
func answerAsReviewer(t *testing.T, e *Engine, f *fixture, rt *fakeRuntime, id string, outcome ReviewOutcome) {
	t.Helper()
	st := mustState(t, f, id)
	if st.ActiveReview == nil {
		t.Fatalf("no review is open on %s to answer", id)
	}
	env := ReviewEnvelope{
		RequestID: st.RequestID, Kind: st.ActiveReview.Kind, ReviewID: st.ActiveReview.ReviewID,
		TaskID: st.TaskID, Generation: st.ActiveReview.Generation,
		AnswerNonce: st.ActiveReview.AnswerNonce, Outcome: outcome,
		ReportBody: "recorded from the reviewer's own session",
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	rt.outputs[st.ActiveReview.ExpectedReviewer] = string(body)

	ans, cerr := e.CollectReviewerAnswer(context.Background(), id, 0)
	if cerr != nil {
		t.Fatalf("collect answer: %v", cerr)
	}
	if _, serr := f.store.SubmitAnswer(id, ans); serr != nil {
		t.Fatalf("submit answer: %v", serr)
	}
}

// --- stage 1: the investigation role ---------------------------------------

// TestEpisodeExecution_ReportPhaseDispatchesToTheBoundInvestigator asserts the
// dispatch is real and is addressed to the role the template bound.
func TestEpisodeExecution_ReportPhaseDispatchesToTheBoundInvestigator(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-EXEC")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-EXEC")
	defer stop()

	out := drive(t, eng, "EP-EXEC", "standard_fix")
	if out.Advanced != "" {
		t.Fatalf("the episode advanced a phase whose work had not happened: %+v", out)
	}

	st := mustState(t, f, "EP-EXEC")
	step, ok := st.episodeStep("REPORT#1")
	if !ok {
		t.Fatalf("no dispatch was recorded for REPORT round 1: %+v", st.Episode.Execution.Steps)
	}
	if step.Status != StepDispatched || step.Role != string(RoleInvestigat) {
		t.Fatalf("step = %+v, want a DISPATCHED step on the %s role", step, RoleInvestigat)
	}
	if step.Profile != "looksee" || step.Session == "" {
		t.Fatalf("the step did not name the pinned profile and a session: %+v", step)
	}

	msgs := sentTo(rt, step.Session)
	if len(msgs) != 1 {
		t.Fatalf("the investigator received %d instructions, want exactly 1: %v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "SEMANTIC REPORT") {
		t.Errorf("the investigator was not asked for a report: %.200s", msgs[0])
	}
	// The worker must not have been used for a phase that is not its own.
	if got := sentTo(rt, st.ExecutionProfiles[RoleWorker].Name); len(got) != 0 {
		t.Errorf("the worker profile name was addressed as a session: %v", got)
	}
}

// TestEpisodeExecution_RerunningAPhaseDoesNotDispatchTwice is the idempotence
// half of the same property: a second pass over the same round adds no
// instruction and no record.
func TestEpisodeExecution_RerunningAPhaseDoesNotDispatchTwice(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-IDEMPOTENT")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-IDEMPOTENT")
	defer stop()

	drive(t, eng, "EP-IDEMPOTENT", "standard_fix")
	before := mustState(t, f, "EP-IDEMPOTENT")
	for i := 0; i < 3; i++ {
		drive(t, eng, "EP-IDEMPOTENT", "standard_fix")
	}
	after := mustState(t, f, "EP-IDEMPOTENT")

	if len(after.Episode.Execution.Steps) != len(before.Episode.Execution.Steps) {
		t.Errorf("repeated passes recorded %d extra steps: %+v",
			len(after.Episode.Execution.Steps)-len(before.Episode.Execution.Steps), after.Episode.Execution.Steps)
	}
	step, _ := after.episodeStep("REPORT#1")
	if got := len(sentTo(rt, step.Session)); got != 1 {
		t.Errorf("the investigator received %d instructions after repeated passes, want 1", got)
	}
}

// --- stage 1 refusal: an unbound role --------------------------------------

// TestEpisodeExecution_UnboundRoleBlocksRatherThanCompleting is the refusal that
// matters most, because it is invisible otherwise: the episode still exists, the
// state file is still valid, and nothing is running.
//
// The template deliberately binds a worker and a reviewer and no investigator, so
// the REPORT phase has nobody to do it. Admission succeeds — that shape is a
// legal template — and the episode must then stop rather than substitute a
// profile nobody approved.
func TestEpisodeExecution_UnboundRoleBlocksRatherThanCompleting(t *testing.T) {
	roles := map[ProfileRole]string{RoleWorker: "impl", RoleReviewer: "audit"}
	f := execFixture(t, execTemplate(roles, []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-UNBOUND")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-UNBOUND")
	defer stop()

	out := drive(t, eng, "EP-UNBOUND", "standard_fix")
	if out.Advanced != "" {
		t.Fatalf("a phase with no bound role advanced anyway: %+v", out)
	}
	if !strings.Contains(out.Blocked, "investigation role is not bound") {
		t.Fatalf("the block does not name the unbound role: %q", out.Blocked)
	}
	st := mustState(t, f, "EP-UNBOUND")
	if st.Episode.Phase != string(PhaseReport) {
		t.Errorf("phase = %q, want it still at REPORT", st.Episode.Phase)
	}
	step, ok := st.episodeStep("REPORT#1")
	if !ok || step.Status != StepRefused {
		t.Fatalf("the refusal was not recorded durably: %+v", st.Episode.Execution.Steps)
	}
	if len(rt.sentMessages()) != 0 {
		t.Errorf("an unbound role still produced a send: %v", rt.sentMessages())
	}

	// Re-running does not manufacture a second attempt or a new answer.
	out2 := drive(t, eng, "EP-UNBOUND", "standard_fix")
	if !strings.Contains(out2.Blocked, "investigation role is not bound") {
		t.Errorf("the second pass lost the reason: %q", out2.Blocked)
	}
	if n := len(mustState(t, f, "EP-UNBOUND").Episode.Execution.Steps); n != 1 {
		t.Errorf("a second pass recorded %d steps, want the single original refusal", n)
	}
}

// --- stages 2 and 3: the reviewer role --------------------------------------

// TestEpisodeExecution_ReportReviewGoesToTheReviewer asserts the judgement is
// addressed to the reviewer role, that the reviewer is a DIFFERENT session from
// the implementer, and that a report review cannot approve anything.
func TestEpisodeExecution_ReportReviewGoesToTheReviewer(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-REVIEW")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-REVIEW")
	defer stop()

	// A report has to exist before there is anything to review.
	drive(t, eng, "EP-REVIEW", "standard_fix")
	report := goodEpisodeReport()
	report.EpisodeID = "EP-REVIEW"
	report.Attempt = 1
	if _, err := eng.RecordReportArtifact("EP-REVIEW", &report); err != nil {
		t.Fatalf("record report: %v", err)
	}
	out := drive(t, eng, "EP-REVIEW", "standard_fix")
	if out.Advanced != string(PhaseReportReview) {
		t.Fatalf("the episode did not reach the report review: %+v", out)
	}

	st := mustState(t, f, "EP-REVIEW")
	review := st.ActiveReview
	if review == nil {
		t.Fatal("no judgement was opened for the report review")
	}
	if review.Kind != ReviewCheckpoint {
		t.Errorf("the report review is a %s; only a COMPLETION review may approve, and this one must not", review.Kind)
	}
	if review.Kind.Approves() {
		t.Error("a report review was opened in a mode that can approve the request")
	}
	if review.ExpectedReviewer == st.ExecutionProfiles[RoleWorker].Name {
		t.Fatal("the report review was addressed to the implementer's identity")
	}
	if st.IsImplementationSession(review.ExpectedReviewer) {
		t.Fatalf("the report review was addressed to session %s inside the implementation lineage",
			review.ExpectedReviewer)
	}
	if investigator, ok := st.BackendFor(RoleInvestigat); ok && investigator.SessionID == review.ExpectedReviewer {
		t.Fatalf("the reviewer is the investigator's own session %s", investigator.SessionID)
	}
	if st.ExecutionProfiles[RoleWorker].Name == st.ExecutionProfiles[RoleReviewer].Name {
		t.Fatal("worker and reviewer resolved to the same profile")
	}
	step, ok := st.episodeStep("REPORT_REVIEW#1")
	if !ok || step.Role != string(RoleReviewer) {
		t.Fatalf("the report review was not dispatched to the reviewer role: %+v", st.Episode.Execution.Steps)
	}
	if msgs := sentTo(rt, review.ExpectedReviewer); len(msgs) != 1 {
		t.Fatalf("the reviewer received %d instructions, want 1", len(msgs))
	}

	// The reviewer answers CONTINUE, and the episode may now plan — but only
	// because it recorded THAT verdict.
	answerAsReviewer(t, eng, f, rt, "EP-REVIEW", OutcomeContinue)
	st = mustState(t, f, "EP-REVIEW")
	rec := st.episodeReviewFor(PhaseReportReview, 1)
	if rec == nil || !rec.Settled || rec.Outcome != string(OutcomeContinue) {
		t.Fatalf("the report review's settlement was not recorded: %+v", st.Episode.Execution.Reviews)
	}
	if out = drive(t, eng, "EP-REVIEW", "standard_fix"); out.Advanced != string(PhasePlan) {
		t.Fatalf("the episode did not advance to PLAN after an accepted report: %+v", out)
	}
}

// TestEpisodeExecution_WorkerAnswerIsRefusedAsAReview is the independence
// property at the moment it would otherwise be lost: an implementer that answers
// its own review.
//
// The refusal is the existing canonical identity check, exercised through the
// real open review rather than a fabricated one.
func TestEpisodeExecution_WorkerAnswerIsRefusedAsAReview(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-SELFREVIEW")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-SELFREVIEW")
	defer stop()

	drive(t, eng, "EP-SELFREVIEW", "standard_fix")
	report := goodEpisodeReport()
	report.EpisodeID = "EP-SELFREVIEW"
	if _, err := eng.RecordReportArtifact("EP-SELFREVIEW", &report); err != nil {
		t.Fatalf("record report: %v", err)
	}
	drive(t, eng, "EP-SELFREVIEW", "standard_fix")

	st := mustState(t, f, "EP-SELFREVIEW")
	if st.ActiveReview == nil {
		t.Fatal("no report review is open to answer")
	}
	// Give the episode a real implementer session, then answer as that session.
	if _, err := eng.ensureEpisodeRoleSession(context.Background(), "EP-SELFREVIEW", RoleWorker); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	st = mustState(t, f, "EP-SELFREVIEW")
	worker, ok := st.BackendFor(RoleWorker)
	if !ok {
		t.Fatal("the worker role has no session")
	}
	_, err := f.store.SubmitAnswer("EP-SELFREVIEW", ReviewAnswer{
		Kind: st.ActiveReview.Kind, ReviewID: st.ActiveReview.ReviewID,
		TaskID: st.TaskID, Generation: st.ActiveReview.Generation,
		SourceSession: worker.SessionID, AnswerNonce: st.ActiveReview.AnswerNonce,
		Outcome: OutcomeContinue, Body: "I reviewed my own work and approve it",
	})
	if err == nil {
		t.Fatal("the implementer was allowed to answer the review of its own episode")
	}
	if !strings.Contains(err.Error(), "review answer refused") {
		t.Errorf("the refusal is not an answer refusal: %v", err)
	}
	if rec := mustState(t, f, "EP-SELFREVIEW").episodeReviewFor(PhaseReportReview, 1); rec != nil && rec.Settled {
		t.Error("a refused answer still settled the episode's judgement record")
	}
	if got := mustState(t, f, "EP-SELFREVIEW").ActiveReview; got == nil || got.ReviewID != st.ActiveReview.ReviewID {
		t.Error("a refused answer closed or replaced the review it did not answer")
	}
}

// --- stage 4: local plan acceptance ----------------------------------------

func TestEpisodeExecution_PlanInstructionCarriesAdmittedScope(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-PLAN-SCOPE"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, id)
	defer stop()
	report := goodEpisodeReport()
	report.EpisodeID = id
	if _, err := eng.RecordReportArtifact(id, &report); err != nil {
		t.Fatal(err)
	}
	drive(t, eng, id, "standard_fix")
	answerAsReviewer(t, eng, f, rt, id, OutcomeContinue)
	drive(t, eng, id, "standard_fix")
	st := mustState(t, f, id)
	step, ok := st.episodeStep("PLAN#1")
	if !ok || st.Episode.Phase != string(PhasePlan) {
		t.Fatal("planner instruction was not dispatched")
	}
	var plan contract.ImplementationPlan
	var instruction string
	for _, message := range sentTo(rt, step.Session) {
		for _, line := range strings.Split(message, "\n") {
			if strings.HasPrefix(line, `{"schema":`) && strings.Contains(line, `"accepted_report_digest"`) {
				if err := json.Unmarshal([]byte(line), &plan); err != nil {
					t.Fatal(err)
				}
				instruction = message
			}
		}
	}
	if plan.Grant != string(CapabilityScopedWrite) {
		t.Fatalf("planner received an explanatory grant instead of admitted scope: %q", plan.Grant)
	}
	if len(plan.Steps) != 1 || strings.Join(plan.Steps[0].AllowedPaths, ",") != "src/**" || strings.Join(plan.Steps[0].ProtectedPaths, ",") != "evaluation/**" {
		t.Fatalf("planner scope differs from admitted request: %+v", plan.Steps)
	}
	if !strings.Contains(instruction, "Every step must declare at least one allowed_change_kind") || !strings.Contains(instruction, "Verification and independent review remain controller-owned") {
		t.Fatal("planner was not told the contract's mutation-step and authority rules")
	}
	if st.Episode.PlanAuthorized || st.Episode.PlanDigest != "" {
		t.Fatal("draft instruction authorized or retained a plan")
	}
	baseline, err := TakeSnapshot(st.Workspace, f.now, f.cfg.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan.SourceBaseline = baseline.ContentDigest
	plan.Steps[0].Findings = []string{report.Findings[0].FindingID}
	plan.Steps[0].RequirementIDs = []string{"O-1"}
	envelope := EpisodeArtifactEnvelope{RequestID: id, StepKey: step.Key, ArtifactNonce: step.ArtifactNonce, Generation: step.Generation, Plan: &plan}
	binding, _ := st.BackendForID(step.Session)
	// A planner may ignore the instructions: the existing intake refusal must
	// still retain no plan and confer no authority.
	plan.Steps[0].AllowedChangeKinds = nil
	body, _ := json.Marshal(envelope)
	if err := eng.intakeEpisodeArtifact(id, binding.Backend, step.Session, "", "fixture-planner-model", string(body)); err == nil || !strings.Contains(err.Error(), "allowed change kinds") {
		t.Fatalf("empty mutation scope was not refused: %v", err)
	}
	if mustState(t, f, id).Episode.PlanDigest != "" {
		t.Fatal("refused plan was retained")
	}
	plan.Steps[0].AllowedChangeKinds = []string{"source"}
	body, _ = json.Marshal(envelope)
	// Regression: a valid PLAN with normal-sized native tool output used to
	// stall intake at the old 128 KiB producer evidence cap.
	if err := eng.intakeEpisodeArtifact(id, binding.Backend, step.Session, "", "fixture-planner-model", string(body), producerEvidenceOfSize(205037)); err != nil {
		t.Fatal(err)
	}
	accepted := mustState(t, f, id)
	acceptedStep, _ := accepted.episodeStep(step.Key)
	if accepted.Episode.PlanDigest == "" || acceptedStep.ArtifactEvidenceDigest == "" || accepted.Episode.PlanAuthorized {
		t.Fatal("large plan evidence was not retained independently of authorization")
	}
	eng.opts.Once = true
	for i := 0; i < 2; i++ {
		if _, err := eng.Run(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		current := mustState(t, f, id)
		if current.Episode.Phase != string(PhasePlanAccept) || current.Episode.PlanAuthorized || current.BackendBindings[RoleWorker].SessionID != "" {
			t.Fatal("ordinary supervision passed manual acceptance or launched a worker")
		}
	}
}

// TestEpisodeExecution_PlanAcceptanceRefusesAModel asserts that accepting a plan
// is a LOCAL decision and that no model bound to this episode can make it.
//
// The refusal has to survive renaming: a contract check that only compares against
// the plan's own execution profile would accept an answer naming the reviewer, or
// naming the worker under a different profile name.
func TestEpisodeExecution_PlanAcceptanceRefusesAModel(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-ACCEPT")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-ACCEPT")
	defer stop()

	drive(t, eng, "EP-ACCEPT", "standard_fix")
	report := goodEpisodeReport()
	report.EpisodeID = "EP-ACCEPT"
	measured, err := eng.RecordReportArtifact("EP-ACCEPT", &report)
	if err != nil {
		t.Fatalf("record report: %v", err)
	}
	drive(t, eng, "EP-ACCEPT", "standard_fix")
	answerAsReviewer(t, eng, f, rt, "EP-ACCEPT", OutcomeContinue)
	drive(t, eng, "EP-ACCEPT", "standard_fix")

	plan := contractPlanForTest(1)
	baseline, berr := TakeSnapshot(filepath.Join(f.srcRoot, "project"), f.now, f.cfg.StoreRoot)
	if berr != nil {
		t.Fatal(berr)
	}
	plan.SourceBaseline = baseline.ContentDigest
	plan.ExecutionProfile = "impl"
	plan.ReviewProfile = "audit"
	plan.EpisodeID = "EP-ACCEPT"
	plan.AcceptedReportDigest = measured
	if _, err := eng.ProposePlan("EP-ACCEPT", &plan); err != nil {
		t.Fatalf("propose plan: %v", err)
	}
	// A drafted plan moves the episode to plan acceptance and stops there. It is
	// a PROPOSAL: nothing about having written one confers authority to execute.
	if out := drive(t, eng, "EP-ACCEPT", "standard_fix"); out.Advanced != string(PhasePlanAccept) {
		t.Fatalf("a drafted plan did not reach plan acceptance: %+v", out)
	}
	if out := drive(t, eng, "EP-ACCEPT", "standard_fix"); out.Advanced != "" {
		t.Fatalf("an unaccepted plan advanced the episode: %+v", out)
	}
	if !strings.Contains(drive(t, eng, "EP-ACCEPT", "standard_fix").Blocked, "no model may supply it") {
		t.Error("the wait for acceptance does not say that a model cannot supply it")
	}

	for _, by := range []string{"impl", "audit", "gate", "triage", "looksee"} {
		auth := authForTest(plan)
		auth.EpisodeID = "EP-ACCEPT"
		auth.AcceptedBy = by
		if _, err := eng.AcceptEpisodePlan("EP-ACCEPT", auth, &plan); err == nil {
			t.Errorf("the model bound to this episode (%q) was allowed to accept its own plan", by)
		} else if !strings.Contains(err.Error(), "may not accept") {
			t.Errorf("the refusal for %q does not say why: %v", by, err)
		}
	}
	if st := mustState(t, f, "EP-ACCEPT"); st.Episode.PlanAuthorized {
		t.Fatal("a refused acceptance still authorised the plan")
	}

	// A local operator is accepted, and only then does implementation open.
	auth := authForTest(plan)
	auth.EpisodeID = "EP-ACCEPT"
	auth.AcceptedBy = "operator:local-forge"
	if _, err := eng.AcceptEpisodePlan("EP-ACCEPT", auth, &plan); err != nil {
		t.Fatalf("a local operator's acceptance was refused: %v", err)
	}
	st := mustState(t, f, "EP-ACCEPT")
	if !st.Episode.PlanAuthorized || st.Episode.AuthorizedBy != "operator:local-forge" {
		t.Fatalf("the acceptance was not recorded: %+v", st.Episode)
	}
	if out := drive(t, eng, "EP-ACCEPT", "standard_fix"); out.Advanced != string(PhaseImplement) {
		t.Fatalf("the episode did not reach implementation after acceptance: %+v", out)
	}
	st = mustState(t, f, "EP-ACCEPT")
	step, ok := st.episodeStep("IMPLEMENT#1")
	if !ok || step.Role != string(RoleWorker) {
		t.Fatalf("implementation was not dispatched to the worker role: %+v", st.Episode.Execution.Steps)
	}
	// The reviewer drafted the plan; implementation is a separate acknowledged
	// worker instruction that requires the operator's acceptance.
	var implement int
	for _, m := range sentTo(rt, step.Session) {
		if strings.Contains(m, "ACCEPTED plan") {
			implement++
		}
	}
	if implement != 1 {
		t.Errorf("the implementer was told to implement %d times, want exactly 1: %v",
			implement, sentTo(rt, step.Session))
	}
}

// --- stages 5 and 6: the worker and the managed checks ----------------------

// driveToVerify walks the real chain from an admitted episode to VERIFY,
// including the checkpoint assessment that ends the implementer step. Nothing is
// short-circuited: every transition is the production decision reading committed
// state.
func driveToImplement(t *testing.T, e *Engine, f *fixture, rt *fakeRuntime, id string) {
	t.Helper()
	drive(t, e, id, "standard_fix")
	report := goodEpisodeReport()
	report.EpisodeID = id
	measured, err := e.RecordReportArtifact(id, &report)
	if err != nil {
		t.Fatalf("record report: %v", err)
	}
	drive(t, e, id, "standard_fix")
	answerAsReviewer(t, e, f, rt, id, OutcomeContinue)
	drive(t, e, id, "standard_fix")

	plan := contractPlanForTest(1)
	baseline, berr := TakeSnapshot(filepath.Join(f.srcRoot, "project"), f.now, f.cfg.StoreRoot)
	if berr != nil {
		t.Fatal(berr)
	}
	plan.SourceBaseline = baseline.ContentDigest
	plan.ExecutionProfile = "impl"
	plan.ReviewProfile = "audit"
	plan.EpisodeID = id
	plan.AcceptedReportDigest = measured
	if _, err := e.ProposePlan(id, &plan); err != nil {
		t.Fatalf("propose plan: %v", err)
	}
	drive(t, e, id, "standard_fix")
	auth := authForTest(plan)
	auth.EpisodeID = id
	auth.AcceptedBy = "operator:local-forge"
	if _, err := e.AcceptEpisodePlan(id, auth, &plan); err != nil {
		t.Fatalf("accept plan: %v", err)
	}
	drive(t, e, id, "standard_fix")
	if got := mustState(t, f, id).Episode.Phase; got != string(PhaseImplement) {
		t.Fatalf("chain stalled at %s, want IMPLEMENT", got)
	}

}

func driveToVerify(t *testing.T, e *Engine, f *fixture, rt *fakeRuntime, id string) {
	t.Helper()
	driveToImplement(t, e, f, rt, id)

	// A transport-correlated turn claim is assessed independently before checks.
	current := mustState(t, f, id)
	binding, ok := current.BackendForID(current.WorkerSession)
	if !ok {
		t.Fatal("worker is unbound")
	}
	if err := e.recordEpisodeTurn(id, binding.Backend, current.WorkerSession, "fixture-terminal-turn", "", current.ExecutionProfiles[RoleWorker].Model); err != nil {
		t.Fatal(err)
	}
	drive(t, e, id, "standard_fix")
	implementationReview := mustState(t, f, id).ActiveReview
	if implementationReview == nil || implementationReview.ReviewID != "EI1" {
		t.Fatalf("implementation reused the report review identity: %+v", implementationReview)
	}
	answerAsReviewer(t, e, f, rt, id, OutcomeContinue)
	if out := drive(t, e, id, "standard_fix"); out.Advanced != string(PhaseVerify) {
		t.Fatalf("the episode did not reach VERIFY: %+v", out)
	}
	if got := mustState(t, f, id).Episode.Phase; got != string(PhaseVerify) {
		t.Fatalf("phase = %s, want VERIFY", got)
	}
}

// TestEpisodeExecution_VerifyRunsRequiredChecksAndAFailedCheckDoesNotAdvance is
// the check requirement.
//
// The template's required check is registered and named by the template, the
// verifier really is invoked, and the receipt it produces is recorded against the
// episode. A check that could not run is BLOCKED, which is not a pass, so the
// episode stays at VERIFY and says why.
func TestEpisodeExecution_VerifyRunsRequiredChecksAndAFailedCheckDoesNotAdvance(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-VERIFY")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-VERIFY")
	defer stop()

	jobsBefore := len(mustState(t, f, "EP-VERIFY").Jobs)
	driveToVerify(t, eng, f, rt, "EP-VERIFY")

	out := drive(t, eng, "EP-VERIFY", "standard_fix")
	if out.Advanced == string(PhaseCompletionReview) {
		t.Fatalf("verification that did not pass advanced the episode: %+v", out)
	}
	if !strings.Contains(out.Blocked, "did not pass") {
		t.Errorf("the block does not say the checks did not pass: %q", out.Blocked)
	}

	st := mustState(t, f, "EP-VERIFY")
	v := st.Episode.Execution.Verification
	if v == nil || !v.Complete {
		t.Fatalf("the verification run was not recorded as complete: %+v", v)
	}
	if v.Satisfied {
		t.Fatal("a check that could not run was recorded as satisfying the template")
	}
	if len(v.Receipts) != 1 || v.Receipts[0].CheckID != unrunnableCheckID {
		t.Fatalf("the template's required check produced no recorded receipt: %+v", v.Receipts)
	}
	if v.Receipts[0].Outcome == JobPass {
		t.Errorf("a check whose tool is absent was recorded as PASS: %+v", v.Receipts[0])
	}
	if len(st.Jobs) <= jobsBefore {
		t.Errorf("no managed job was recorded: %d before, %d after", jobsBefore, len(st.Jobs))
	}

	// A second pass must not run the same round again. The template declares no
	// return edge out of VERIFY, so the episode stays put and the only thing that
	// could add a job is a re-run.
	jobsAfterFirst := len(st.Jobs)
	for i := 0; i < 3; i++ {
		drive(t, eng, "EP-VERIFY", "standard_fix")
	}
	if got := len(mustState(t, f, "EP-VERIFY").Jobs); got != jobsAfterFirst {
		t.Errorf("a repeated pass ran the checks again: %d jobs, want %d", got, jobsAfterFirst)
	}
	if got := mustState(t, f, "EP-VERIFY").Episode.Phase; got != string(PhaseVerify) {
		t.Errorf("phase = %s, want it still at VERIFY", got)
	}
}

// TestEpisodeExecution_VerifyAdvancesOnlyOnSatisfiedChecks asserts the other
// side of the same gate, and that the independent audit is opened by the REVIEWER
// role once verification stands.
//
// The receipts are a fixture because producing a PASS receipt requires running a
// real command, which this suite does not do; everything downstream of the
// receipts — the round, the completeness check, the phase decision and the
// reviewer dispatch — is the production path.
func TestEpisodeExecution_VerifyAdvancesOnlyOnSatisfiedChecks(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-AUDIT")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-AUDIT")
	defer stop()

	driveToVerify(t, eng, f, rt, "EP-AUDIT")
	if err := f.store.RecordEpisodeVerification("EP-AUDIT", 1, []string{unrunnableCheckID},
		[]CheckReceipt{{
			ReceiptID: "V-fixture", CheckID: unrunnableCheckID, Outcome: JobPass,
			ContentDigest: strings.Repeat("a", 64), SnapshotID: "S-fixture", ExitCode: 0,
		}}, strings.Repeat("a", 64), true, "", f.now.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("record verification: %v", err)
	}

	out := drive(t, eng, "EP-AUDIT", "standard_fix")
	if out.Advanced != string(PhaseCompletionReview) {
		t.Fatalf("satisfied verification did not open the completion audit: %+v", out)
	}
	st := mustState(t, f, "EP-AUDIT")
	review := st.ActiveReview
	if review == nil || review.Kind != ReviewCompletion {
		t.Fatalf("no independent completion audit was opened: %+v", review)
	}
	worker, _ := st.BackendFor(RoleWorker)
	if review.ExpectedReviewer == worker.SessionID {
		t.Fatal("the completion audit was addressed to the implementer")
	}
	step, ok := st.episodeStep("COMPLETION_REVIEW#1")
	if !ok || step.Role != string(RoleReviewer) {
		t.Fatalf("the completion audit was not dispatched to the reviewer role: %+v", st.Episode.Execution.Steps)
	}
	msgs := sentTo(rt, review.ExpectedReviewer)
	if len(msgs) < 1 {
		t.Fatalf("the reviewer was never actually asked for the audit: %v", rt.sentMessages())
	}
	if last := msgs[len(msgs)-1]; !strings.Contains(last, "Managed verification record:") || !strings.Contains(last, "V-fixture") {
		t.Fatalf("completion audit did not receive the measured check receipt: %s", last)
	}
	// Entering the audit costs an audit attempt; it is not free.
	if st.Episode.Budget.Consumed.CompletionAudit != 1 {
		t.Errorf("completion audits consumed = %d, want 1", st.Episode.Budget.Consumed.CompletionAudit)
	}
}

// --- stage 8: bounded repair ------------------------------------------------

// TestEpisodeExecution_RepairBudgetExhaustionStopsTheEpisode asserts the episode
// does not loop while still calling itself progress.
//
// Each failed completion audit buys exactly one repair, charged against the root
// allowance the template declared. When that allowance is gone the episode STOPS,
// states why, and stops dispatching — which is the whole difference between a
// bounded correction and an episode that returns to implementation forever.
func TestEpisodeExecution_RepairBudgetExhaustionStopsTheEpisode(t *testing.T) {
	edges := map[contract.EpisodePhase][]contract.EpisodePhase{
		contract.PhaseVerify:           {contract.PhaseImplement},
		contract.PhaseCompletionReview: {contract.PhaseImplement},
	}
	tpl := execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, edges)
	tpl.MaxRepairIterations = 2
	f := execFixture(t, tpl)
	admitEpisode(t, f, "EP-REPAIR")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-REPAIR")
	defer stop()

	driveToVerify(t, eng, f, rt, "EP-REPAIR")
	if err := f.store.RecordEpisodeVerification("EP-REPAIR", 1, []string{unrunnableCheckID},
		[]CheckReceipt{{
			ReceiptID: "V-fixture", CheckID: unrunnableCheckID, Outcome: JobPass,
			ContentDigest: strings.Repeat("b", 64), SnapshotID: "S-fixture", ExitCode: 0,
		}}, strings.Repeat("b", 64), true, "", f.now.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("record verification: %v", err)
	}

	// Drive the audit/repair cycle until the episode refuses to continue. The
	// loop is bounded by construction: each pass either spends budget or stops.
	var last EpisodeOutcome
	stopped := false
	for i := 0; i < 12; i++ {
		last = drive(t, eng, "EP-REPAIR", "standard_fix")
		if last.Blocked != "" && strings.Contains(last.Blocked, "root budget is exhausted") {
			stopped = true
			break
		}
		st := mustState(t, f, "EP-REPAIR")
		if st.Episode.Phase == string(PhaseImplement) {
			key := episodeStepKey(PhaseImplement, st.episodePhaseRound(PhaseImplement))
			if step, ok := st.episodeStep(key); ok && step.TurnID == "" {
				binding, bound := st.BackendForID(step.Session)
				if !bound {
					t.Fatal("repair worker unbound")
				}
				if err := eng.recordEpisodeTurn("EP-REPAIR", binding.Backend, step.Session, key, "", st.ExecutionProfiles[RoleWorker].Model); err != nil {
					t.Fatal(err)
				}
			}
		}
		if st.ActiveReview != nil {
			if st.ActiveReview.Kind == ReviewCompletion {
				answerAsReviewer(t, eng, f, rt, "EP-REPAIR", OutcomeFail)
			} else {
				answerAsReviewer(t, eng, f, rt, "EP-REPAIR", OutcomeContinue)
			}
		}
	}
	if !stopped {
		t.Fatalf("the episode never stopped repairing; last outcome %+v", last)
	}

	st := mustState(t, f, "EP-REPAIR")
	if got := st.Episode.Budget.Consumed.Repairs; got > tpl.MaxRepairIterations {
		t.Errorf("repairs consumed = %d, above the declared ceiling of %d", got, tpl.MaxRepairIterations)
	}
	if !strings.Contains(last.Blocked, "repair iterations") {
		t.Errorf("the terminal block does not name the exhausted budget: %q", last.Blocked)
	}

	// And it stays stopped: more passes add no progress and no new instruction.
	phase := st.Episode.Phase
	steps := len(st.Episode.Execution.Steps)
	for i := 0; i < 3; i++ {
		drive(t, eng, "EP-REPAIR", "standard_fix")
	}
	final := mustState(t, f, "EP-REPAIR")
	if final.Episode.Phase != phase {
		t.Errorf("the episode moved again after its repair budget was spent: %s -> %s", phase, final.Episode.Phase)
	}
	if len(final.Episode.Execution.Steps) != steps {
		t.Errorf("the episode recorded more steps after it stopped: %d -> %d", steps, len(final.Episode.Execution.Steps))
	}
}

// TestEpisodeExecution_InterruptedVerificationIsNotReadAsPass asserts the crash
// window is a refusal rather than a retry.
//
// The intent is written before the checks run, so a run that never reported is
// visibly incomplete. Repeating it would append a second set of jobs whose
// relationship to the first nobody can later establish — which is the situation
// this whole store treats as BLOCKED everywhere else.
func TestEpisodeExecution_InterruptedVerificationIsNotReadAsPass(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-INTERRUPTED")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-INTERRUPTED")
	defer stop()

	driveToVerify(t, eng, f, rt, "EP-INTERRUPTED")
	// Clear the round driveToVerify's own pass produced, then write exactly what
	// the production path writes before it launches any check.
	if _, err := f.store.UpdateCommitted("EP-INTERRUPTED", func(s *State) error {
		s.Episode.Execution.Verification = nil
		return nil
	}); err != nil {
		t.Fatalf("clear verification: %v", err)
	}
	jobsBefore := len(mustState(t, f, "EP-INTERRUPTED").Jobs)
	if err := beginEpisodeVerification(f.store, "EP-INTERRUPTED", 1, []string{unrunnableCheckID}, func() time.Time { return f.now }); err != nil {
		t.Fatalf("begin verification: %v", err)
	}

	out := drive(t, eng, "EP-INTERRUPTED", "standard_fix")
	if out.Advanced != "" {
		t.Fatalf("an interrupted verification advanced the episode: %+v", out)
	}
	if !strings.Contains(out.Blocked, "interrupted") {
		t.Errorf("the block does not name the interruption: %q", out.Blocked)
	}
	if !strings.Contains(out.Blocked, "not read as a pass") {
		t.Errorf("the block does not say the outcome is not a pass: %q", out.Blocked)
	}
	st := mustState(t, f, "EP-INTERRUPTED")
	if v := st.Episode.Execution.Verification; v == nil || v.Complete || v.Satisfied {
		t.Fatalf("the interrupted run was completed or satisfied: %+v", v)
	}
	if got := len(st.Jobs); got != jobsBefore {
		t.Errorf("the interrupted round was re-run: %d jobs, want the %d from before", got, jobsBefore)
	}
	if out2 := drive(t, eng, "EP-INTERRUPTED", "standard_fix"); out2.Advanced != "" {
		t.Errorf("a second pass advanced the interrupted episode: %+v", out2)
	}
}

// --- the "nothing launches" rule -------------------------------------------

// TestEpisodeExecution_NoRequiredCheckCanStartAProcess asserts, mechanically,
// that no check these tests drive could spawn anything.
//
// It is written as a check rather than a promise because the whole point of the
// constraint is that a later edit to a fixture must not break it silently: if a
// real argv is ever added to a template used here, this fails.
func TestEpisodeExecution_NoRequiredCheckCanStartAProcess(t *testing.T) {
	if _, err := exec.LookPath(unrunnableCheckTool); err == nil {
		t.Fatalf("%s is installed; the required-check tests would now launch a real process",
			unrunnableCheckTool)
	}
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	// EVERY registered check, not only the one the template names: the shared
	// completion path runs the request's own checks, so a real argv left anywhere
	// in this fixture would be reachable from a test that did not mean to exec.
	for id := range f.cfg.Checks {
		ck, err := f.cfg.Check(id)
		if err != nil {
			t.Fatalf("check %q is registered but unreadable: %v", id, err)
		}
		if ck.Argv[0] != unrunnableCheckTool {
			t.Errorf("check %q runs %q; only the absent %q may be used here",
				id, ck.Argv[0], unrunnableCheckTool)
		}
	}

	// Every role in these tests runs on the Agent Deck transport, so the engine
	// routes them through the scripted fake runtime and through nothing else.
	for name, p := range episodeProfiles() {
		if got := p.TransportFor(); got != TransportAgentDeck {
			t.Errorf("profile %q declares transport %q; a non-fake transport would be launched", name, got)
		}
		if len(p.LaunchCommand) != 0 {
			t.Errorf("profile %q carries its own launch command; nothing here should exec anything", name)
		}
	}
}

// TestEpisodeExecution_CompletionPassIsRefusedWhileNothingIsJudgedOrExecuted is a
// refusal CONTROL, not an isolation of one rule, and it is named for what it
// actually pins.
//
// An episode that reaches a completion audit has obligations nobody has judged
// and a required check that cannot run. This test drives that exact state and
// asserts only the two things that hold regardless of which gate refuses first:
// the PASS is refused, and the refusal closes nothing — the episode is still
// non-terminal with its audit still owed.
//
// It deliberately does NOT claim to prove why the PASS is refused. The report
// body here is prose, which the store refuses as a missing judgement payload, and
// the fixture's check is unrunnable, which the verification gate refuses. Both
// are correct and either may be reached first; pinning one of them would make
// this test about the episode controller's fixture rather than about the episode
// staying owed. The reachable completion, and each individual refusal, are pinned
// in completion_discharge_test.go.
func TestEpisodeExecution_CompletionPassIsRefusedWhileNothingIsJudgedOrExecuted(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-PASS")
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, "EP-PASS")
	defer stop()

	driveToVerify(t, eng, f, rt, "EP-PASS")
	if err := f.store.RecordEpisodeVerification("EP-PASS", 1, []string{unrunnableCheckID},
		[]CheckReceipt{{
			ReceiptID: "V-fixture", CheckID: unrunnableCheckID, Outcome: JobPass,
			ContentDigest: strings.Repeat("c", 64), SnapshotID: "S-fixture", ExitCode: 0,
		}}, strings.Repeat("c", 64), true, "", f.now.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("record verification: %v", err)
	}
	if out := drive(t, eng, "EP-PASS", "standard_fix"); out.Advanced != string(PhaseCompletionReview) {
		t.Fatalf("the completion audit was not opened: %+v", out)
	}

	st := mustState(t, f, "EP-PASS")
	review := st.ActiveReview
	if review == nil || review.Kind != ReviewCompletion {
		t.Fatalf("no completion audit is open: %+v", review)
	}
	body := "every obligation is discharged and the executed checks pass"
	env := ReviewEnvelope{
		RequestID: st.RequestID, Kind: review.Kind, ReviewID: review.ReviewID,
		TaskID: st.TaskID, Generation: review.Generation, AnswerNonce: review.AnswerNonce,
		Outcome: OutcomePass, ReportBody: body, ReportSHA256: sha256Hex([]byte(body)),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	rt.outputs[review.ExpectedReviewer] = string(raw)

	ans, cerr := eng.CollectReviewerAnswer(context.Background(), "EP-PASS", 0)
	if cerr != nil {
		t.Fatalf("collect answer: %v", cerr)
	}
	if _, serr := f.store.SubmitAnswer("EP-PASS", ans); serr == nil {
		t.Fatal("a completion PASS was accepted for an episode with nothing executed behind it")
	}

	after := mustState(t, f, "EP-PASS")
	if after.Stage.Terminal() {
		t.Fatalf("the refused PASS still made the request terminal as %s", after.ResultStatus)
	}
	if after.ActiveReview == nil || after.ActiveReview.ReviewID != review.ReviewID {
		t.Error("the refused PASS closed the audit it did not satisfy")
	}
	for _, o := range after.Obligations {
		if o.Disposition == DispositionAdvanced {
			t.Errorf("the refused PASS advanced obligation %s", o.ID)
		}
	}
}

func TestEpisodeExecution_ReportRevisionReceivesCorrelatedReviewFeedback(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, map[contract.EpisodePhase][]contract.EpisodePhase{contract.PhaseReportReview: {contract.PhaseReport}}))
	id := "EP-REPORT-FEEDBACK"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, id)
	defer stop()
	drive(t, eng, id, "standard_fix")
	report := goodEpisodeReport()
	report.EpisodeID = id
	report.Attempt = 1
	if _, err := eng.RecordReportArtifact(id, &report); err != nil {
		t.Fatal(err)
	}
	drive(t, eng, id, "standard_fix")
	answerAsReviewer(t, eng, f, rt, id, OutcomeCollectEvidence)
	st := mustState(t, f, id)
	review := st.episodeReviewFor(PhaseReportReview, 1)
	if review == nil || review.ReportBody != "recorded from the reviewer's own session" {
		t.Fatalf("correlated review lost its detailed feedback: %+v", review)
	}
	if out := drive(t, eng, id, "standard_fix"); out.Advanced != string(PhaseReport) {
		t.Fatalf("revision edge did not return: %+v", out)
	}
	drive(t, eng, id, "standard_fix")
	st = mustState(t, f, id)
	step, ok := st.episodeStep("REPORT#2")
	if !ok {
		t.Fatal("no second report dispatch")
	}
	messages := sentTo(rt, step.Session)
	if len(messages) < 2 || !strings.Contains(messages[len(messages)-1], review.ReportBody) || !strings.Contains(messages[len(messages)-1], "preserve admitted obligations and scope") {
		t.Fatalf("revision did not receive bounded feedback: %v", messages)
	}
	if st.Episode.PlanAuthorized || st.Stage.Terminal() {
		t.Fatal("report feedback granted implementation or completion")
	}
}

func TestEpisodeExecution_ImplementationCheckpointRetryIsSameSessionAndBounded(t *testing.T) {
	tpl := execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, map[contract.EpisodePhase][]contract.EpisodePhase{contract.PhaseImplement: {contract.PhaseImplement}})
	tpl.MaxRepairIterations = 1
	f := execFixture(t, tpl)
	id := "EP-IN-PLAN-RETRY"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	eng, stop := episodeEngine(t, f, rt, id)
	defer stop()
	driveToImplement(t, eng, f, rt, id)
	original := mustState(t, f, id)
	first, _ := original.episodeStep("IMPLEMENT#1")
	digest := original.Episode.PlanDigest
	binding, _ := original.BackendForID(first.Session)
	if err := eng.recordEpisodeTurn(id, binding.Backend, first.Session, "turn1", "", original.ExecutionProfiles[RoleWorker].Model); err != nil {
		t.Fatal(err)
	}
	drive(t, eng, id, "standard_fix")
	answerAsReviewer(t, eng, f, rt, id, OutcomeRedirect)
	out := drive(t, eng, id, "standard_fix")
	if out.Advanced != string(PhaseImplement) {
		t.Fatalf("checkpoint rejection did not retry: %+v", out)
	}
	st := mustState(t, f, id)
	second, ok := st.episodeStep("IMPLEMENT#2")
	if !ok || second.Session != first.Session || second.EffectID == first.EffectID || st.Episode.PlanDigest != digest || !st.Episode.PlanAuthorized || st.Episode.Budget.Consumed.Repairs != 1 {
		t.Fatalf("retry lost identity, authority or budget: %+v", st.Episode)
	}
	messages := sentTo(rt, second.Session)
	if len(messages) != 2 || !strings.Contains(messages[1], "recorded from the reviewer's own session") || !strings.Contains(messages[1], "Do not delegate implementation") {
		t.Fatalf("retry lost direct-tool instruction or feedback: %v", messages)
	}
	for i := 0; i < 3; i++ {
		drive(t, eng, id, "standard_fix")
	}
	if len(sentTo(rt, second.Session)) != 2 {
		t.Fatal("pending retry was resent")
	}
	if err := eng.recordEpisodeTurn(id, binding.Backend, second.Session, "turn2", "", st.ExecutionProfiles[RoleWorker].Model); err != nil {
		t.Fatal(err)
	}
	drive(t, eng, id, "standard_fix")
	answerAsReviewer(t, eng, f, rt, id, OutcomeRedirect)
	for i := 0; i < 2; i++ {
		if got := drive(t, eng, id, "standard_fix"); got.Advanced != "" || !strings.Contains(got.Blocked, "spent") {
			t.Fatalf("exhausted retry was not stopped: %+v", got)
		}
	}
	st = mustState(t, f, id)
	if len(sentTo(rt, second.Session)) != 2 || st.Episode.Budget.Consumed.Repairs != 1 || st.episodePhaseRound(PhaseImplement) != 2 {
		t.Fatal("retry exhaustion created another turn or lost history")
	}
}
