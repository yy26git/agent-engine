package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
)

// Exercise real admission, independent report review, PLAN dispatch and artifact
// intake. Only the provider boundary is fake; no state is planted at PLAN.
func importedPlanFixture(t *testing.T, id string) (*fixture, *Engine, *fakeRuntime, contract.ImplementationPlan, EpisodeStep) {
	t.Helper()
	template := execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil)
	template.Phases = append([]contract.EpisodePhase(nil), contract.PhaseOrder...)
	f := execFixture(t, template)
	ep := episodeRequest(f, id)
	content := "# Existing implementation plan\r\nFix src/main.go. Keep evaluation/** unchanged. The original regression must pass.\r\n"
	ep.ImportedPlan = &contract.ImportedPlanDocument{Name: "existing.md", Content: content, SHA256: sha256Hex([]byte(content))}
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	// Admission must own its copy, not the caller's mutable pointer.
	ep.ImportedPlan.Content = "caller changed the original object"
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	t.Cleanup(stop)
	drive(t, e, id, "standard_fix")
	state := mustState(t, f, id)
	if state.Episode.ImportedPlan.Content != content {
		t.Fatal("admission did not preserve the exact imported document")
	}
	investigator, _ := state.BackendFor(RoleInvestigat)
	if !strings.Contains(strings.Join(sentTo(rt, investigator.SessionID), "\n"), "Do not draft a replacement plan") {
		t.Fatal("investigator did not receive existing-plan instructions")
	}
	report := goodEpisodeReport()
	report.EpisodeID = id
	report.Findings[0].RequirementID = "O-1"
	reportStep, ok := state.episodeStep("REPORT#1")
	if !ok {
		t.Fatal("report was not dispatched")
	}
	if err := importPlanAnswer(e, state, reportStep, EpisodeArtifactEnvelope{RequestID: id, StepKey: reportStep.Key, ArtifactNonce: reportStep.ArtifactNonce, Generation: reportStep.Generation, Report: &report}); err != nil {
		t.Fatal(err)
	}
	reportDigest := mustState(t, f, id).Episode.ReportDigest
	drive(t, e, id, "standard_fix")
	answerAsReviewer(t, e, f, rt, id, OutcomeContinue)
	drive(t, e, id, "standard_fix")
	state = mustState(t, f, id)
	step, ok := state.episodeStep("PLAN#1")
	if !ok || state.Episode.Phase != string(PhasePlan) {
		t.Fatal("did not dispatch the imported-plan review")
	}
	message := strings.Join(sentTo(rt, step.Session), "\n")
	if !strings.Contains(message, "Review the IMPORTED IMPLEMENTATION PLAN") || strings.Contains(message, "Draft an IMPLEMENTATION PLAN") || !strings.Contains(message, state.Episode.ImportedPlan.SHA256) {
		t.Fatal("import was sent to the free-drafting planner instead of the existing-plan reviewer")
	}
	if !strings.Contains(message, `"imported_plan_sha256":"`+state.Episode.ImportedPlan.SHA256+`"`) || !strings.Contains(message, `"imported_plan_excerpt"`) {
		t.Fatal("the artifact schema omitted required import mapping fields")
	}
	baseline, err := TakeSnapshot(state.Workspace, f.now, f.cfg.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan := contract.ImplementationPlan{Schema: contract.EpisodeVersion, EpisodeID: id, Version: 1,
		AcceptedReportDigest: reportDigest, SourceBaseline: baseline.ContentDigest, Grant: string(CapabilityScopedWrite),
		ExecutionProfile: "impl", ReviewProfile: "audit", ImportedPlanSHA256: state.Episode.ImportedPlan.SHA256,
		Steps: []contract.PlanStep{{StepID: "S1", ImportedPlanExcerpt: "Fix src/main.go.", Findings: []string{"F1"}, RequirementIDs: []string{"O-1"},
			AllowedPaths: []string{"src/**"}, PlannedWrites: []string{"src/main.go"}, ProtectedPaths: []string{"evaluation/**"}, AllowedChangeKinds: []string{"source"},
			RequiredChecks: []string{unrunnableCheckID}, AcceptanceCriteria: "The original regression must pass.", Expected: "Regression fixed"}}}
	return f, e, rt, plan, step
}

func importedPlanEnvelope(plan *contract.ImplementationPlan, step EpisodeStep) EpisodeArtifactEnvelope {
	return EpisodeArtifactEnvelope{RequestID: plan.EpisodeID, StepKey: step.Key, ArtifactNonce: step.ArtifactNonce, Generation: step.Generation, Plan: plan}
}

func importPlanAnswer(e *Engine, state *State, step EpisodeStep, env EpisodeArtifactEnvelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	binding, _ := state.BackendForID(step.Session)
	return e.intakeEpisodeArtifact(state.RequestID, binding.Backend, step.Session, "", "", string(body))
}

func TestImportedPlan_ReviewMappingKeepsAcceptanceAndOriginalForWorker(t *testing.T) {
	const id = "EP-IMPORTED-ACCEPT"
	f, e, rt, plan, step := importedPlanFixture(t, id)
	state := mustState(t, f, id)
	if err := importPlanAnswer(e, state, step, importedPlanEnvelope(&plan, step)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		drive(t, e, id, "standard_fix")
	}
	state = mustState(t, f, id)
	if state.Episode.Phase != string(PhasePlanAccept) || state.Episode.PlanAuthorized || state.WorkerSession != "" {
		t.Fatal("import skipped operator acceptance or started a worker")
	}
	for _, effect := range state.Effects {
		if effect.Role == string(RoleWorker) {
			t.Fatal("import dispatched implementation before acceptance")
		}
	}
	if state.ProgressSnapshot().ImportedPlanSHA256 != plan.ImportedPlanSHA256 {
		t.Fatal("observer lost imported-plan identity")
	}
	original := *state.Episode.ImportedPlan
	artifact, _ := json.Marshal(original)
	retained, err := e.ReadEpisodeArtifact(id, sha256Hex(artifact))
	if err != nil || string(retained) != string(artifact) {
		t.Fatalf("complete original not retained by digest: %v", err)
	}
	auth := authForTest(plan)
	auth.EpisodeID = plan.EpisodeID
	auth.AcceptedBy = "operator:import-test"
	if _, err := e.AcceptEpisodePlan(id, auth, &plan); err != nil {
		t.Fatal(err)
	}
	drive(t, e, id, "standard_fix")
	state = mustState(t, f, id)
	worker, ok := state.episodeStep("IMPLEMENT#1")
	if !ok {
		t.Fatal("accepted imported plan did not dispatch implementation")
	}
	message := strings.Join(sentTo(rt, worker.Session), "\n")
	if !strings.Contains(message, "Follow both") || !strings.Contains(message, original.SHA256) || !strings.Contains(message, "The original regression must pass") {
		t.Fatal("worker did not receive the original plan alongside the accepted mapping")
	}
}

func TestImportedPlan_RefusesDroppedIdentityRewrittenQuoteAndStaleCorrelation(t *testing.T) {
	f, e, _, plan, step := importedPlanFixture(t, "EP-IMPORTED-REFUSE")
	state := mustState(t, f, plan.EpisodeID)
	for name, mutate := range map[string]func(*contract.ImplementationPlan, *EpisodeArtifactEnvelope){
		"dropped source": func(p *contract.ImplementationPlan, _ *EpisodeArtifactEnvelope) { p.ImportedPlanSHA256 = "" },
		"different source": func(p *contract.ImplementationPlan, _ *EpisodeArtifactEnvelope) {
			p.ImportedPlanSHA256 = strings.Repeat("c", 64)
		},
		"rewritten quote": func(p *contract.ImplementationPlan, _ *EpisodeArtifactEnvelope) {
			p.Steps[0].ImportedPlanExcerpt = "Replace the architecture and tests"
		},
		"missing quote": func(p *contract.ImplementationPlan, _ *EpisodeArtifactEnvelope) { p.Steps[0].ImportedPlanExcerpt = "" },
		"protected write": func(p *contract.ImplementationPlan, _ *EpisodeArtifactEnvelope) {
			p.Steps[0].PlannedWrites = []string{"evaluation/test.go"}
		},
		"stale nonce": func(_ *contract.ImplementationPlan, env *EpisodeArtifactEnvelope) { env.ArtifactNonce = "wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := plan
			candidate.Steps = append([]contract.PlanStep(nil), plan.Steps...)
			env := importedPlanEnvelope(&candidate, step)
			mutate(&candidate, &env)
			if err := importPlanAnswer(e, state, step, env); err == nil {
				t.Fatal("invalid imported-plan mapping was accepted")
			}
			if got := mustState(t, f, plan.EpisodeID); got.Episode.PlanDigest != "" || got.Episode.PlanAuthorized {
				t.Fatal("refused plan was retained or granted authority")
			}
		})
	}
}

func TestImportedPlan_IncompatiblePlanStopsWithoutReplacement(t *testing.T) {
	f, e, rt, plan, step := importedPlanFixture(t, "EP-IMPORTED-CONFLICT")
	state := mustState(t, f, plan.EpisodeID)
	env := importedPlanEnvelope(&plan, step)
	env.Plan = nil
	env.ImportedPlanRefusal = "The original plan requires changing a protected evaluation file. Revise the source plan."
	if err := importPlanAnswer(e, state, step, env); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, f, plan.EpisodeID)
	if state.Stage != StageBlocked || state.ResultStatus != string(contract.StatusBlocked) || state.Episode.PlanDigest != "" || state.Episode.Stop == nil || !strings.Contains(state.Episode.Stop.Reason, "protected evaluation file") {
		t.Fatalf("incompatible source plan did not stop durably: %+v", state.Episode.Stop)
	}
	count := len(state.Effects)
	created := len(rt.createdIDs)
	for i := 0; i < 2; i++ {
		drive(t, e, plan.EpisodeID, "standard_fix")
	}
	if len(mustState(t, f, plan.EpisodeID).Effects) != count || len(rt.createdIDs) != created {
		t.Fatal("refused import launched another planner or worker")
	}
}

func TestImportedPlan_AdmissionIdentityAndSourceDriftPreserveOriginal(t *testing.T) {
	f, e, _, plan, step := importedPlanFixture(t, "EP-IMPORTED-DRIFT")
	state := mustState(t, f, plan.EpisodeID)
	if err := importPlanAnswer(e, state, step, importedPlanEnvelope(&plan, step)); err != nil {
		t.Fatal(err)
	}
	drive(t, e, plan.EpisodeID, "standard_fix")
	auth := authForTest(plan)
	auth.EpisodeID = plan.EpisodeID
	auth.AcceptedBy = "operator:import-test"
	if _, err := e.AcceptEpisodePlan(plan.EpisodeID, auth, &plan); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, f, plan.EpisodeID)
	original := *state.Episode.ImportedPlan
	ep := episodeRequest(f, plan.EpisodeID)
	ep.ImportedPlan = &original
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatalf("exact imported-plan admission could not be retried: %v", err)
	}
	changed := original
	changed.Content += "\nNew task"
	changed.SHA256 = sha256Hex([]byte(changed.Content))
	ep.ImportedPlan = &changed
	if _, err := f.store.AdmitEpisode(ep); err == nil {
		t.Fatal("admitted episode accepted replacement source-plan bytes")
	}
	if err := os.WriteFile(filepath.Join(state.Workspace, "main.go"), []byte("package main\n// drift\n"), 0600); err != nil {
		t.Fatal(err)
	}
	validation, err := e.RevalidateEpisodeResume(context.Background(), plan.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	state = mustState(t, f, plan.EpisodeID)
	if !validation.DriftObserved || state.Episode.PlanAuthorized || state.Episode.PlanDigest != "" || *state.Episode.ImportedPlan != original {
		t.Fatalf("source re-investigation dropped original or kept stale authority: validation=%+v, phase=%s, authorized=%t, plan=%s, original_preserved=%t", validation, state.Episode.Phase, state.Episode.PlanAuthorized, state.Episode.PlanDigest, *state.Episode.ImportedPlan == original)
	}
	// Existing states migrate without inventing an imported plan, and older
	// executables cannot read new schema-6 imported-plan states.
	legacy := state.Clone()
	legacy.SchemaVersion, legacy.Episode.ImportedPlan = 5, nil
	if err := f.store.migrateAndValidate(legacy); err != nil || legacy.SchemaVersion != CurrentSchemaVersion || legacy.Episode.ImportedPlan != nil {
		t.Fatalf("legacy migration invented a plan or failed: %v", err)
	}
}

func TestImportedPlan_LargeSourceIsRetainedWithoutTruncation(t *testing.T) {
	template := execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil)
	template.Phases = append([]contract.EpisodePhase(nil), contract.PhaseOrder...)
	f := execFixture(t, template)
	f.cfg.Evidence = EpisodeEvidencePolicy{MaxProducerBytes: 65536, MaxEpisodeBytes: 1 << 20, MaxPromptBytes: 128}
	ep := episodeRequest(f, "EP-IMPORTED-LARGE")
	content := strings.Repeat("Existing source-plan requirement.\n", 1000) + "FINAL REQUIREMENT MUST NOT BE LOST\n"
	ep.ImportedPlan = &contract.ImportedPlanDocument{Content: content, SHA256: sha256Hex([]byte(content))}
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e, stop := episodeEngine(t, f, newFakeRuntime(), ep.EpisodeID)
	defer stop()
	state := mustState(t, f, ep.EpisodeID)
	prompt, _, err := e.episodeArtifactContextWithBudget(ep.EpisodeID, state)
	if err != nil || !strings.Contains(prompt, "PARTIAL EXCERPT") || strings.Contains(prompt, "FINAL REQUIREMENT MUST NOT BE LOST") {
		t.Fatalf("prompt did not respect excerpt budget: %v", err)
	}
	body, _ := json.Marshal(ep.ImportedPlan)
	retained, err := e.ReadEpisodeArtifact(ep.EpisodeID, sha256Hex(body))
	if err != nil || string(retained) != string(body) {
		t.Fatalf("complete imported document was truncated: %v", err)
	}
}

func TestImportedPlan_AdmissionAndUnadmittedSourceRefusals(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	ep := episodeRequest(f, "EP-IMPORTED-BAD")
	ep.ImportedPlan = &contract.ImportedPlanDocument{Content: "plan", SHA256: strings.Repeat("a", 64)}
	if _, err := f.store.AdmitEpisode(ep); err == nil {
		t.Fatal("admission accepted a fabricated source digest")
	}
	if _, err := os.Stat(filepath.Join(f.root, RequestsDirName, ep.EpisodeID)); !os.IsNotExist(err) {
		t.Fatalf("invalid source left a partial admission: %v", err)
	}
	ep.ImportedPlan.SHA256 = sha256Hex([]byte(ep.ImportedPlan.Content))
	if _, err := f.store.AdmitEpisode(ep); err == nil || !strings.Contains(err.Error(), "PLAN_ACCEPT") {
		t.Fatalf("template that omits import review/acceptance was accepted: %v", err)
	}
	admitEpisode(t, f, "EP-NO-IMPORTED-SOURCE")
	e, stop := episodeEngine(t, f, newFakeRuntime(), "EP-NO-IMPORTED-SOURCE")
	defer stop()
	plan := contractPlanForTest(1)
	plan.ImportedPlanSHA256 = strings.Repeat("b", 64)
	plan.Steps[0].ImportedPlanExcerpt = "unadmitted text"
	if _, err := e.ProposePlan("EP-NO-IMPORTED-SOURCE", &plan); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("generated plan invented an imported source: %v", err)
	}
}

func TestImportedPlan_Schema5UpgradePreservesExistingJournalAndProgress(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-EXISTING-SCHEMA5")
	state := mustState(t, f, "EP-EXISTING-SCHEMA5")
	state.SchemaVersion = 5
	state.ConsumedJournalSeq = 42
	state.IngestedPositions = map[string]uint64{"retained-stream": 91}
	progressBefore, _ := json.Marshal(state.Progress)
	if err := f.store.migrateAndValidate(state); err != nil {
		t.Fatal(err)
	}
	progressAfter, _ := json.Marshal(state.Progress)
	if state.SchemaVersion != CurrentSchemaVersion || state.ConsumedJournalSeq != 42 || state.IngestedPositions["retained-stream"] != 91 || string(progressBefore) != string(progressAfter) || state.Episode.ImportedPlan != nil {
		t.Fatalf("schema upgrade rewound existing observations or invented an import: schema=%d cursor=%d", state.SchemaVersion, state.ConsumedJournalSeq)
	}
}

func TestImportedPlan_UpgradeRunsIntermediateMigrations(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	admitEpisode(t, f, "EP-OLDER-SCHEMA")
	for _, version := range []int{1, 2, 3, 4} {
		state := mustState(t, f, "EP-OLDER-SCHEMA")
		state.SchemaVersion = version
		state.CancellationRequested, state.CancellationRequestedAt = true, ""
		state.ConsumedJournalSeq = 42
		if err := f.store.migrateAndValidate(state); err != nil {
			t.Fatal(err)
		}
		if state.SchemaVersion != CurrentSchemaVersion || state.CancellationRequestedAt != state.UpdatedAt {
			t.Fatalf("schema %d skipped a required intermediate migration: schema=%d cancelled_at=%q", version, state.SchemaVersion, state.CancellationRequestedAt)
		}
		if version >= 3 && state.ConsumedJournalSeq != 42 {
			t.Fatalf("schema %d reran an older journal reset", version)
		}
	}
}
