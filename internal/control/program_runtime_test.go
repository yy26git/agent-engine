package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

func programForFixture(t *testing.T, f *fixture, ep *contract.EpisodeRequest) *contract.ProgramDefinition {
	t.Helper()
	tpl, ok := f.cfg.EpisodeTemplates.Lookup(ep.Template)
	if !ok {
		t.Fatal("fixture template missing")
	}
	source, err := measureStableEpisodeSource(ep.Workspace.Path, f.cfg.StoreRoot, f.now)
	if err != nil {
		t.Fatal(err)
	}
	dependencies, err := measureDependencyBaseline(ep.Workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	templateDigest, err := programTemplateDigest(tpl)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := f.store.episodeRoleProfiles(tpl, ep.Workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	roleBindings := programRolesFromProfiles(profiles)
	capabilities := programCapabilities(ep.Constraints.AllowCodeChanges, tpl.RequiredChecks)
	budget := contract.ProgramBudget{Children: 1, RepairCycles: 2, WorkerTurns: 1, AuditAttempts: 2}
	return &contract.ProgramDefinition{
		Schema: contract.ProgramVersion, ProgramID: "program-test", Revision: 1,
		AuthorityRef: "local-test", IssuedAt: "2026-10-14T00:00:00Z",
		Budget: budget,
		Cards: []contract.ProgramCard{{
			CardID: "card-1", DependsOn: []string{}, Objective: ep.Objective, Template: ep.Template,
			TemplateDigest: templateDigest, SourceBaseline: source.ContentDigest,
			DependencyBaseline: dependencies, Grant: "read-write", Roles: roleBindings,
			Capabilities: capabilities, AllowedPaths: append([]string(nil), ep.Constraints.AllowedPaths...),
			ProtectedPaths: append([]string(nil), ep.Constraints.ProtectedPaths...),
			RequiredChecks: append([]string(nil), tpl.RequiredChecks...), AllowedChangeKinds: []string{"source"},
			Obligations:  append([]contract.WorkflowObligation(nil), ep.Obligations...),
			RepairRoutes: programRoutesFromTemplate(tpl), Resources: []contract.ResourceClaim{}, Budget: budget,
		}},
		Escalations: []contract.Escalation{{ID: "scope", When: "a mismatch", Description: "request operator review"}},
	}
}

func bindFixtureProgram(t *testing.T, f *fixture, ep *contract.EpisodeRequest) *contract.ProgramDefinition {
	t.Helper()
	program := programForFixture(t, f, ep)
	digest, err := program.ProgramDigest()
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.ProgramAuthorities = map[string]ProgramAuthorityGrant{
		program.AuthorityRef: {AuthorityRef: program.AuthorityRef, ProgramID: program.ProgramID,
			DefinitionSHA256: digest, ExpiresAt: "2026-10-15T00:00:00Z"},
	}
	return program
}

func TestAdmitProgramEpisodeBindsDefinitionAndLimitsAtomically(t *testing.T) {
	f := execFixture(t, fullTemplate())
	ep := episodeRequest(f, "EP-PROGRAM-ADMIT")
	program := bindFixtureProgram(t, f, ep)
	if _, err := f.store.AdmitProgramEpisode(program, "card-1", ep); err != nil {
		t.Fatalf("program admission: %v", err)
	}
	state, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ProgramBinding == nil || state.ProgramBinding.CardID != "card-1" || state.ProgramBinding.ProgramDigest == "" {
		t.Fatalf("program binding was not committed with episode: %+v", state.ProgramBinding)
	}
	if state.Episode.Budget.WorkerTurns != 1 || state.Episode.Budget.RepairIterations != 2 || state.Episode.Budget.CompletionAudits != 2 {
		t.Fatalf("program ceilings were not installed in episode root: %+v", state.Episode.Budget)
	}
	if _, err := f.store.AdmitEpisode(ep); err == nil {
		t.Fatal("ordinary duplicate admission detached the pinned program binding")
	}
	if err := f.store.Update(ep.EpisodeID, func(s *State) error { s.ProgramBinding.CardID = "other-card"; return nil }); err == nil {
		t.Fatal("general state update changed the admitted immutable card binding")
	}
}

func TestAdmitProgramEpisodeRefusesInvalidDefinitionWithoutPanicking(t *testing.T) {
	f := execFixture(t, fullTemplate())
	ep := episodeRequest(f, "EP-PROGRAM-INVALID")
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("invalid caller program panicked instead of returning a refusal: %v", recovered)
		}
	}()
	if _, err := f.store.AdmitProgramEpisode(&contract.ProgramDefinition{}, "missing", ep); err == nil {
		t.Fatal("admitted an invalid program definition")
	}
}

func TestProgramPlanExactMatchRecordsPolicyReceiptAtPlanAccept(t *testing.T) {
	f := execFixture(t, fullTemplate())
	ep := episodeRequest(f, "EP-PROGRAM-ACCEPT")
	program := bindFixtureProgram(t, f, ep)
	if _, err := f.store.AdmitProgramEpisode(program, "card-1", ep); err != nil {
		t.Fatal(err)
	}
	state, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := strings.Repeat("a", 64)
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error {
		s.Episode.ReportDigest = reportDigest
		s.Episode.Phase = string(PhasePlan)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan := contract.ImplementationPlan{
		Schema: contract.EpisodeVersion, EpisodeID: ep.EpisodeID, Version: 1,
		AcceptedReportDigest: reportDigest, SourceBaseline: program.Cards[0].SourceBaseline,
		Grant: "read-write", ExecutionProfile: "impl", ReviewProfile: "audit",
		Steps: []contract.PlanStep{{StepID: "S1", Findings: []string{"F1"}, RequirementIDs: []string{"O-1"},
			AllowedPaths: []string{"src/**"}, ProtectedPaths: []string{"evaluation/**"},
			AllowedChangeKinds: []string{"source"}, RequiredChecks: []string{"build", "test"},
			AcceptanceCriteria: "The regression is fixed", Expected: "The regression check passes"}},
	}
	if _, err := (&Engine{store: f.store, now: func() time.Time { return f.now }}).ProposePlan(ep.EpisodeID, &plan); err != nil {
		t.Fatalf("propose plan: %v", err)
	}
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error { s.Episode.Phase = string(PhasePlanAccept); return nil }); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{store: f.store, now: func() time.Time { return f.now }, opts: EngineOptions{DryRun: true}}
	if _, err := engine.DriveEpisode(context.Background(), ep.EpisodeID, ep.Template); err != nil {
		t.Fatalf("drive exact program plan through PLAN_ACCEPT: %v", err)
	}
	state, err = f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Episode.PlanAuthorized || len(state.ProgramAcceptances) != 1 || state.ProgramAcceptances[0].PlanDigest != state.Episode.PlanDigest {
		t.Fatalf("plan acceptance lacks exact durable local receipt: authorized=%t receipts=%+v", state.Episode.PlanAuthorized, state.ProgramAcceptances)
	}
	view := state.ProgressSnapshot()
	if view.PlanAcceptanceSource != "policy:program:local-test" || view.ProgramID != "program-test" || view.ProgramCardID != "card-1" || view.ProgramAcceptancePlanDigest != state.Episode.PlanDigest || view.ProgramAcceptanceAuthorityRef != "local-test" || view.ProgramAcceptanceAt == "" {
		t.Fatalf("progress projection omits attributable policy acceptance: %+v", view)
	}
	handover, _, err := f.store.CreateEpisodeHandover(ep.EpisodeID)
	if err != nil {
		t.Fatalf("create policy handover: %v", err)
	}
	if handover.Episode.AuthorizedBy != "policy:program:local-test" || handover.Episode.ProgramID != "program-test" || handover.Episode.ProgramCardID != "card-1" || handover.Episode.ProgramAcceptance == nil || handover.Episode.ProgramAcceptance.PlanDigest != state.Episode.PlanDigest {
		t.Fatalf("handover omits policy receipt and binding identity: %+v", handover.Episode)
	}
}

func TestProgramPlanMismatchStaysAtOperatorDecision(t *testing.T) {
	f := execFixture(t, fullTemplate())
	ep := episodeRequest(f, "EP-PROGRAM-MISMATCH")
	program := bindFixtureProgram(t, f, ep)
	if _, err := f.store.AdmitProgramEpisode(program, "card-1", ep); err != nil {
		t.Fatal(err)
	}
	reportDigest := strings.Repeat("a", 64)
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error { s.Episode.ReportDigest = reportDigest; return nil }); err != nil {
		t.Fatal(err)
	}
	plan := contract.ImplementationPlan{
		Schema: contract.EpisodeVersion, EpisodeID: ep.EpisodeID, Version: 1,
		AcceptedReportDigest: reportDigest, SourceBaseline: program.Cards[0].SourceBaseline,
		Grant: "read-write", ExecutionProfile: "impl", ReviewProfile: "audit",
		Steps: []contract.PlanStep{{StepID: "S1", Findings: []string{"F1"}, RequirementIDs: []string{"O-1"},
			AllowedPaths: []string{"src/**"}, ProtectedPaths: []string{"evaluation/**"},
			AllowedChangeKinds: []string{"source", "documentation"}, RequiredChecks: []string{"build", "test"},
			AcceptanceCriteria: "The regression is fixed", Expected: "The regression check passes"}},
	}
	if _, err := (&Engine{store: f.store, now: func() time.Time { return f.now }}).ProposePlan(ep.EpisodeID, &plan); err != nil {
		t.Fatalf("propose mismatching plan: %v", err)
	}
	if _, err := f.store.UpdateCommitted(ep.EpisodeID, func(s *State) error { s.Episode.Phase = string(PhasePlanAccept); return nil }); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{store: f.store, now: func() time.Time { return f.now }, opts: EngineOptions{DryRun: true}}
	if _, err := engine.DriveEpisode(context.Background(), ep.EpisodeID, ep.Template); err != nil {
		t.Fatalf("drive mismatching plan through PLAN_ACCEPT: %v", err)
	}
	state, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Episode.PlanAuthorized || len(state.ProgramAcceptances) != 0 {
		t.Fatalf("mismatch issued a policy receipt: %+v", state.ProgramAcceptances)
	}
}
