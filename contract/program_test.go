package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func validProgram() (*ProgramDefinition, *ImplementationPlan, ProgramPlanMeasurement) {
	sha := strings.Repeat("a", 64)
	program := &ProgramDefinition{
		Schema: ProgramVersion, ProgramID: "forge-round2", Revision: 1,
		AuthorityRef: "local-grant-17", IssuedAt: "2026-10-14T01:02:03Z",
		Budget: ProgramBudget{Children: 3, RepairCycles: 2, WorkerTurns: 6, AuditAttempts: 3},
		Cards: []ProgramCard{{
			CardID: "architecture-check", DependsOn: []string{}, Objective: "Add bounded architecture checks",
			Template: "forge-repair", TemplateDigest: sha, SourceBaseline: strings.Repeat("b", 64),
			DependencyBaseline: strings.Repeat("c", 64), Grant: "repair",
			Roles:        []ProgramRoleBinding{{Role: "worker", Profile: "minimax-m27", Model: "MiniMax-M2.7"}, {Role: "reviewer", Profile: "codex-audit", Model: "gpt-6.1-sol"}},
			Capabilities: []string{"workspace-write"}, AllowedPaths: []string{"internal/**"},
			ProtectedPaths: []string{"evaluation/**"}, RequiredChecks: []string{"go-test"},
			AllowedChangeKinds: []string{"source", "test"},
			Obligations:        []WorkflowObligation{{ID: "tests", Criterion: "Run required checks", RequiredChecks: []string{"go-test"}}},
			RepairRoutes:       []RepairRoute{{From: "VERIFY_FAILED", To: "IMPLEMENT"}},
			Resources:          []ResourceClaim{{Kind: "workspace", ID: "repo-a", Exclusive: true}},
			Budget:             ProgramBudget{Children: 1, RepairCycles: 2, WorkerTurns: 4, AuditAttempts: 2},
		}},
		Escalations: []Escalation{{ID: "scope-change", When: "proposed plan widens scope", Description: "Request a local operator decision"}},
	}
	plan := &ImplementationPlan{
		Schema: EpisodeVersion, EpisodeID: "EP-child-1", Version: 1,
		AcceptedReportDigest: strings.Repeat("d", 64), SourceBaseline: strings.Repeat("b", 64),
		Grant: "repair", ExecutionProfile: "minimax-m27", ReviewProfile: "codex-audit",
		Steps: []PlanStep{{StepID: "S1", Findings: []string{"F1"}, RequirementIDs: []string{"tests"},
			AllowedPaths: []string{"internal/**"}, ProtectedPaths: []string{"evaluation/**"},
			AllowedChangeKinds: []string{"source", "test"}, RequiredChecks: []string{"go-test"},
			AcceptanceCriteria: "required test passes"}},
	}
	measured := ProgramPlanMeasurement{
		EpisodeID: "EP-child-1", Objective: "Add bounded architecture checks", ReportDigest: strings.Repeat("d", 64), Template: "forge-repair", TemplateDigest: sha,
		DependencyBaseline: strings.Repeat("c", 64),
		Roles:              []ProgramRoleBinding{{Role: "worker", Profile: "minimax-m27", Model: "MiniMax-M2.7"}, {Role: "reviewer", Profile: "codex-audit", Model: "gpt-6.1-sol"}},
		Capabilities:       []string{"workspace-write"},
		RepairRoutes:       []RepairRoute{{From: "VERIFY_FAILED", To: "IMPLEMENT"}},
		Resources:          []ResourceClaim{{Kind: "workspace", ID: "repo-a", Exclusive: true}},
		Budget:             ProgramBudget{Children: 1, RepairCycles: 2, WorkerTurns: 4, AuditAttempts: 2},
		Obligations:        []WorkflowObligation{{ID: "tests", Criterion: "Run required checks", RequiredChecks: []string{"go-test"}}},
	}
	return program, plan, measured
}

func TestProgramDefinitionDigestAndDecodeAreStable(t *testing.T) {
	p, _, _ := validProgram()
	first, err := p.ProgramDigest()
	if err != nil {
		t.Fatalf("digest valid program: %v", err)
	}
	second, err := p.ProgramDigest()
	if err != nil || first != second {
		t.Fatalf("digest changed without mutation: %q %q, %v", first, second, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeProgramDefinition(raw)
	if err != nil {
		t.Fatalf("decode valid program: %v", err)
	}
	got, err := decoded.ProgramDigest()
	if err != nil || first != got {
		t.Fatalf("round trip changed identity: %q != %q (%v)", first, got, err)
	}

	// A future safety field cannot be silently dropped by this binary.
	withUnknown := strings.TrimSuffix(string(raw), "}") + `,"accepted_by":"worker"}`
	if _, err := DecodeProgramDefinition([]byte(withUnknown)); err == nil {
		t.Fatal("unknown caller-supplied acceptance field was accepted")
	}
	if _, err := DecodeProgramDefinition(append(raw, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestProgramCardMatchReturnsEvidenceButNotAcceptance(t *testing.T) {
	p, plan, measured := validProgram()
	match, err := MatchProgramCard(p, "architecture-check", plan, measured)
	if err != nil {
		t.Fatalf("exact card match refused: %v", err)
	}
	if match.ProgramID != p.ProgramID || match.CardID != "architecture-check" || match.PlanDigest == "" || match.CardDigest == "" || match.ProgramDigest == "" {
		t.Fatalf("match receipt is incomplete: %+v", match)
	}
	if _, err := json.Marshal(match); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustJSON(t, match)), "accepted_by") || strings.Contains(string(mustJSON(t, match)), "accepted") {
		t.Fatalf("match evidence must not claim acceptance: %s", mustJSON(t, match))
	}
}

func TestProgramCardMatchRefusesEverySecurityRelevantMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProgramDefinition, *ImplementationPlan, *ProgramPlanMeasurement)
	}{
		{"baseline", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.SourceBaseline = strings.Repeat("e", 64)
		}},
		{"grant", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) { p.Grant = "read-only" }},
		{"scope", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.Steps[0].AllowedPaths = []string{"**"}
		}},
		{"protected paths", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.Steps[0].ProtectedPaths = nil
		}},
		{"required checks", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.Steps[0].RequiredChecks = []string{"lint"}
		}},
		{"change kinds", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.Steps[0].AllowedChangeKinds = []string{"source"}
		}},
		{"template digest", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.TemplateDigest = strings.Repeat("f", 64)
		}},
		{"dependency baseline", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.DependencyBaseline = strings.Repeat("f", 64)
		}},
		{"role model", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.Roles[0].Model = "different-model"
		}},
		{"capability", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.Capabilities = []string{"unrestricted-shell"}
		}},
		{"repair route", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) { m.RepairRoutes = nil }},
		{"resource", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) { m.Resources = nil }},
		{"budget", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) { m.Budget.WorkerTurns++ }},
		{"obligations", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.Obligations[0].Criterion = "weaker criterion"
		}},
		{"episode", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.EpisodeID = "another-episode"
		}},
		{"objective", func(_ *ProgramDefinition, _ *ImplementationPlan, m *ProgramPlanMeasurement) {
			m.Objective = "different work"
		}},
		{"report digest", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.AcceptedReportDigest = strings.Repeat("f", 64)
		}},
		{"obligation not mapped", func(_ *ProgramDefinition, p *ImplementationPlan, _ *ProgramPlanMeasurement) {
			p.Steps[0].RequirementIDs = nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, plan, measured := validProgram()
			tc.mutate(p, plan, &measured)
			if _, err := MatchProgramCard(p, "architecture-check", plan, measured); err == nil {
				t.Fatal("mismatched plan or measured child was treated as a match")
			}
		})
	}
}

func TestProgramDefinitionRequiresPositiveBudgetsUniqueCardsAndEscalations(t *testing.T) {
	p, _, _ := validProgram()
	p.Budget.WorkerTurns = 0
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("zero/unbounded program budget was accepted")
	}

	p, _, _ = validProgram()
	p.Cards = append(p.Cards, p.Cards[0])
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("duplicate card id was accepted")
	}

	p, _, _ = validProgram()
	p.Escalations = nil
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("program with no explicit escalation was accepted")
	}

	p, _, _ = validProgram()
	p.Cards[0].DependsOn = []string{"missing"}
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("unknown card dependency was accepted")
	}

	p, _, _ = validProgram()
	p.Cards[0].DependsOn = []string{"architecture-check"}
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("self dependency was accepted")
	}

	p, _, _ = validProgram()
	p.Cards[0].Budget.WorkerTurns = p.Budget.WorkerTurns + 1
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("child budget above program ceiling was accepted")
	}

	p, _, _ = validProgram()
	p.Budget.Children = 0
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("program ceiling below its authorized card count was accepted")
	}
}

func TestProgramDefinitionRejectsDuplicateAndCyclicDependencies(t *testing.T) {
	p, _, _ := validProgram()
	second := p.Cards[0]
	second.CardID = "second"
	second.DependsOn = []string{"architecture-check", "architecture-check"}
	p.Cards = append(p.Cards, second)
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("duplicate dependency was accepted")
	}

	p, _, _ = validProgram()
	first := p.Cards[0]
	first.DependsOn = []string{"second"}
	second = p.Cards[0]
	second.CardID = "second"
	second.DependsOn = []string{"architecture-check"}
	p.Cards = append([]ProgramCard{first}, second)
	if err := ValidateProgramDefinition(p); err == nil {
		t.Fatal("dependency cycle was accepted")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
