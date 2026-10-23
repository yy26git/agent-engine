package contract

// Plan conformance receipt tests.
//
// The claim under test, above everything else, is negative: a receipt is EVIDENCE
// and there is no reading of this type that yields PASS. The rest are refusals —
// a receipt that cannot be tied to the plan it judges, or that hides a
// deviation in a word this build does not know, is rejected rather than repaired.

import (
	"reflect"
	"strings"
	"testing"
)

func goodConformancePlan() *ImplementationPlan {
	return &ImplementationPlan{
		Schema: EpisodeVersion, EpisodeID: "EP-1", Version: 1,
		AcceptedReportDigest: strings.Repeat("a", 64),
		SourceBaseline:       strings.Repeat("b", 64), Grant: "read-write",
		ExecutionProfile: "worker", ReviewProfile: "auditor",
		Steps: []PlanStep{{
			StepID: "S1", Findings: []string{"F1"}, RequirementIDs: []string{"R1"},
			Repositories: []string{"agent-engine"}, AllowedChangeKinds: []string{"source"},
			AllowedPaths:       []string{"internal/**", "evaluation/**"},
			ProtectedPaths:     []string{"evaluation/**"},
			RequiredChecks:     []string{"unit"},
			AcceptanceCriteria: "the failing case now passes",
			Expected:           "exit zero",
		}},
	}
}

func TestPlanWriteScope_ConflictingStepRefusedBeforeDispatch(t *testing.T) {
	p := goodConformancePlan()
	p.Steps = append(p.Steps, PlanStep{
		StepID: "S2", Findings: []string{"F1"}, AllowedChangeKinds: []string{"source"},
		AllowedPaths:       []string{"evaluation/checker.ts"},
		PlannedWrites:      []string{"evaluation/checker.ts"},
		AcceptanceCriteria: "checker passes", Expected: "checker updated",
	})
	if err := ValidateImplementationPlan(p); err == nil || !strings.Contains(err.Error(), "evaluation/checker.ts") {
		t.Fatalf("conflicting checker write was not refused before proposal: %v", err)
	}
}

func TestPlanWriteScope_BroadAllowanceWithProtectedException(t *testing.T) {
	p := goodConformancePlan()
	p.Steps[0].AllowedPaths = []string{"src/**"}
	p.Steps[0].ProtectedPaths = []string{"src/checks/**"}
	p.Steps[0].PlannedWrites = []string{"src/main.go"}
	if err := ValidateImplementationPlan(p); err != nil {
		t.Fatalf("broad allowance with a protected exception should remain valid: %v", err)
	}
	if PlanWriteAllowed(p, "src/checks/unit.go") || !PlanWriteAllowed(p, "src/main.go") {
		t.Fatal("effective scope did not enforce the protected exception")
	}
	r := receiptFor(t, p)
	r.ChangedFiles = []string{"src/checks/unit.go"}
	got := DetectPlanDeviations(r, p)
	if len(got) != 1 || got[0].Kind != DeviationProtectedPathWrite {
		t.Fatalf("conformance used a different scope than preflight: %+v", got)
	}
}

// receiptFor builds a receipt that is already bound to the plan's MEASURED
// digest, so tests exercise one rule at a time.
func receiptFor(t *testing.T, p *ImplementationPlan) *ConformanceReceipt {
	t.Helper()
	digest, err := p.Digest()
	if err != nil {
		t.Fatalf("measuring the plan under judgement: %v", err)
	}
	return &ConformanceReceipt{
		Schema: EpisodeVersion, EpisodeID: p.EpisodeID, PlanVersion: p.Version,
		PlanDigest: digest, StepID: "S1", Iteration: 1,
		SourceDigest: strings.Repeat("c", 64),
		ChangedFiles: []string{"internal/control/engine.go"},
		Repositories: []string{"agent-engine"},
		Checks: []ConformanceCheck{{
			CheckID: "unit", Passed: true, Fresh: true,
			EvidenceRefs: []string{"run://unit/1"},
		}},
		ObservedAt: "2026-10-10T00:00:00Z",
	}
}

// TestConformance_CleanReceiptIsEvidenceNotPass is the central negative rule.
//
// A receipt with no deviations and every check fresh must be the BEST possible
// evidence and still not a verdict. It is asserted structurally — there is no
// field on this type that could hold a result — so the property cannot be lost by
// a later field being added in a hurry.
func TestConformance_CleanReceiptIsEvidenceNotPass(t *testing.T) {
	p := goodConformancePlan()
	r := receiptFor(t, p)

	// Validating a clean receipt confirms the record; it concludes nothing.
	if err := ValidateConformanceReceiptAgainstPlan(r, p); err != nil {
		t.Fatalf("a clean receipt was refused: %v", err)
	}
	if r.Deviates() {
		t.Error("a receipt reporting no deviations claimed to deviate")
	}
	if r.HasStaleChecks() {
		t.Error("a receipt whose checks all ran on this snapshot claimed stale checks")
	}

	// Nothing on this type may be readable as a result status. If a future field
	// named pass, verdict, outcome or status appears here, this test fails and
	// the claim that a receipt cannot grant PASS has stopped being true.
	receipt := reflect.TypeOf(ConformanceReceipt{})
	for i := 0; i < receipt.NumField(); i++ {
		f := receipt.Field(i)
		tag := strings.ToLower(f.Tag.Get("json"))
		for _, forbidden := range []string{"pass", "verdict", "status", "outcome", "result"} {
			if strings.Contains(strings.ToLower(f.Name), forbidden) || strings.Contains(tag, forbidden) {
				t.Errorf("ConformanceReceipt carries a %q field (%s); a receipt is evidence, not a result", forbidden, f.Name)
			}
		}
	}
}

// TestConformance_ReceiptRefusesStructuralLapses covers the fields without which
// a receipt cannot be tied to an observation at all.
func TestConformance_ReceiptRefusesStructuralLapses(t *testing.T) {
	p := goodConformancePlan()
	if err := ValidateConformanceReceiptAgainstPlan(receiptFor(t, p), p); err != nil {
		t.Fatalf("a well-formed receipt was refused: %v", err)
	}

	// A schema this build does not understand may carry fields it would ignore.
	wrongSchema := receiptFor(t, p)
	wrongSchema.Schema = EpisodeVersion + 1
	if err := ValidateConformanceReceipt(wrongSchema); err == nil {
		t.Error("a conformance receipt of an unknown schema was accepted")
	}
	// An observation nobody can place.
	noEpisode := receiptFor(t, p)
	noEpisode.EpisodeID = "  "
	if err := ValidateConformanceReceipt(noEpisode); err == nil {
		t.Error("a receipt naming no episode was accepted")
	}
	// A receipt pinned to nothing cannot be correlated with a review.
	noPlan := receiptFor(t, p)
	noPlan.PlanDigest = ""
	if err := ValidateConformanceReceipt(noPlan); err == nil {
		t.Error("a receipt pinning no plan digest was accepted")
	}
	noStep := receiptFor(t, p)
	noStep.StepID = ""
	if err := ValidateConformanceReceipt(noStep); err == nil {
		t.Error("a receipt naming no step was accepted")
	}
	// Fresh means nothing against an unstated snapshot.
	noSource := receiptFor(t, p)
	noSource.SourceDigest = ""
	if err := ValidateConformanceReceipt(noSource); err == nil {
		t.Error("a receipt pinning no source snapshot was accepted")
	}
	// Two records under one id cannot be told apart when a review corroborates
	// them.
	dup := receiptFor(t, p)
	dup.Checks = append(dup.Checks, dup.Checks[0])
	if err := ValidateConformanceReceipt(dup); err == nil {
		t.Error("a receipt recording one check twice was accepted")
	}
}

// TestConformance_ReceiptMustPinThePlanItJudges is what stops a receipt being
// reviewed against a plan it was never compared with.
func TestConformance_ReceiptMustPinThePlanItJudges(t *testing.T) {
	p := goodConformancePlan()
	r := receiptFor(t, p)

	// A declared digest nobody measured is exactly the failure this refuses.
	asserted := *r
	asserted.PlanDigest = strings.Repeat("d", 64)
	if err := ValidateConformanceReceiptAgainstPlan(&asserted, p); err == nil {
		t.Error("a receipt was accepted against a plan it does not measure to")
	}
	// The plan itself moved after the receipt was taken.
	moved := *p
	moved.Steps[0].AcceptanceCriteria = "the failing case still fails elsewhere"
	if err := ValidateConformanceReceiptAgainstPlan(r, &moved); err == nil {
		t.Error("a receipt stayed valid after the plan under judgement changed")
	}
	// A step the plan does not contain has nothing to conform to.
	ghost := *r
	ghost.StepID = "S9"
	if err := ValidateConformanceReceiptAgainstPlan(&ghost, p); err == nil {
		t.Error("a receipt naming a step outside the plan was accepted")
	}
	// Measuring against nothing is a refusal, not a vacuous pass.
	if err := ValidateConformanceReceiptAgainstPlan(r, nil); err == nil {
		t.Error("a receipt was accepted with no plan to judge it against")
	}
}

// TestConformance_StaleChecksBlockAnyPassReading pins the Fresh field.
//
// One stale check is enough: an old Forge PASS cannot approve new code, and a
// receipt that cannot distinguish the two cannot be read as current.
func TestConformance_StaleChecksBlockAnyPassReading(t *testing.T) {
	p := goodConformancePlan()

	// An explicit fresh flag is what makes staleness readable at all.
	stale := receiptFor(t, p)
	stale.Checks[0].Fresh = false
	if !stale.HasStaleChecks() {
		t.Error("a check that did not run on this snapshot was not reported stale")
	}
	// A stale check is still a legitimate record, so it is not a refusal: the
	// remedy is to re-run, not to reject the evidence of what did happen.
	if err := ValidateConformanceReceiptAgainstPlan(stale, p); err != nil {
		t.Errorf("a receipt recording a stale check was refused: %v", err)
	}
	// Carrying the old PASS forward is the failure mode, so restating it is what
	// must be impossible.
	oneFreshOneStale := receiptFor(t, p)
	oneFreshOneStale.Checks = append(oneFreshOneStale.Checks, ConformanceCheck{
		CheckID: "lint", Passed: true, Fresh: false,
		EvidenceRefs: []string{"run://lint/earlier"},
	})
	if !oneFreshOneStale.HasStaleChecks() {
		t.Error("a stale check among fresh ones was not reported stale")
	}
}

// TestConformance_APassMustCiteEvidence is the anti-narrative rule.
//
// A failure needs no evidence to be a fact. A pass with nothing behind it is the
// one thing a checkpoint must never be able to accept.
func TestConformance_APassMustCiteEvidence(t *testing.T) {
	p := goodConformancePlan()
	r := receiptFor(t, p)
	r.Checks[0].EvidenceRefs = nil
	if err := ValidateConformanceReceipt(r); err == nil {
		t.Error("a check reporting a pass with no evidence was accepted")
	}
	// A failure without evidence is a real observation and must survive.
	failed := receiptFor(t, p)
	failed.Checks[0] = ConformanceCheck{CheckID: "unit", Passed: false, Fresh: true}
	if err := ValidateConformanceReceipt(failed); err != nil {
		t.Errorf("a recorded check failure was refused: %v", err)
	}
}

// TestConformance_UndeclaredRepositoryIsADeviation is the strongest
// undeclared-edit detection the plan contract currently supports.
//
// A PlanStep declares repositories and protected paths but no allowed path
// scope, so an undeclared edit is only observable at REPOSITORY granularity: a
// changed file inside a declared repository cannot be distinguished here from one
// the plan never authorised. DetectPlanDeviations therefore reports the
// repository, and ChangedFiles is carried in full for the reviewer who can judge
// the rest by hand. Closing the gap needs an allowed-scope field on PlanStep,
// which lives in episode.go and is out of scope for this file.
func TestConformance_UndeclaredRepositoryIsADeviation(t *testing.T) {
	p := goodConformancePlan()
	r := receiptFor(t, p)
	if got := DetectPlanDeviations(r, p); len(got) != 0 {
		t.Fatalf("a receipt matching the declared scope reported deviations: %+v", got)
	}

	// A repository no step declares is work the plan never authorised.
	// The plan declares only "agent-engine".
	r.Repositories = []string{"agent-engine", "agent-deck"}
	got := DetectPlanDeviations(r, p)
	if len(got) != 1 {
		t.Fatalf("expected one deviation for the undeclared repository, got %+v", got)
	}
	if got[0].Kind != DeviationUndeclaredEdit || got[0].Path != "agent-deck" {
		t.Errorf("the undeclared repository was not reported as an undeclared edit: %+v", got[0])
	}
	if err := ValidateDeviationKinds(got); err != nil {
		t.Errorf("a measured deviation was refused by the kind set: %v", err)
	}
	// A measured deviation is recorded, not repaired: the receipt the supervisor
	// observed stands as evidence, and the reviewer decides what it means.
	r.Deviations = got
	if !r.Deviates() {
		t.Error("a receipt with a measured deviation did not report deviating")
	}
}

// TestConformance_ProtectedPathWriteIsADeviation covers the scope the plan DOES
// declare exactly.
func TestConformance_ProtectedPathWriteIsADeviation(t *testing.T) {
	p := goodConformancePlan()
	r := receiptFor(t, p)

	r.ChangedFiles = []string{"internal/control/engine.go", "evaluation/weights.bin"}
	got := DetectPlanDeviations(r, p)
	if len(got) != 1 {
		t.Fatalf("expected one protected path deviation, got %+v", got)
	}
	d := got[0]
	if d.Kind != DeviationProtectedPathWrite || d.Path != "evaluation/weights.bin" {
		t.Errorf("the protected path write was not reported as such: %+v", d)
	}
	if len(d.EvidenceRefs) == 0 || d.Detail == "" {
		t.Errorf("a protected path deviation must be explicable and evidenced: %+v", d)
	}
	// The scope is a subtree, so a nested file is still protected.
	if coversPath("evaluation/**", "evaluation/sets/holdout.json") != true {
		t.Error("a nested file under a protected subtree was treated as writable")
	}
	// And an unrelated file that merely shares a prefix is not.
	if coversPath("evaluation", "evaluations/report.json") {
		t.Error("a sibling directory was treated as inside the protected scope")
	}
}

// TestConformance_UnknownDeviationKindIsRefused keeps the kind set closed.
//
// A deviation nobody can classify is one nobody is obliged to judge, so a receipt
// cannot hide a problem behind a word this build does not know.
func TestConformance_UnknownDeviationKindIsRefused(t *testing.T) {
	p := goodConformancePlan()
	r := receiptFor(t, p)

	r.Deviations = []ConformanceDeviation{{Kind: "vibes", Detail: "something felt off"}}
	if err := ValidateConformanceReceipt(r); err == nil {
		t.Error("a receipt recording an unknown deviation kind was accepted")
	}
	if err := ValidateConformanceReceiptAgainstPlan(r, p); err == nil {
		t.Error("an unknown deviation kind survived validation against the plan")
	}
	// Every advertised kind is recognised, so the set cannot drift from its use.
	for _, k := range ConformanceDeviationKinds {
		known := []ConformanceDeviation{{Kind: k, Detail: "d"}}
		if err := ValidateDeviationKinds(known); err != nil {
			t.Errorf("advertised deviation kind %q was not recognised: %v", k, err)
		}
	}
	// "Something deviated" is not a reportable observation.
	vague := []ConformanceDeviation{{Kind: DeviationRequirementChange}}
	if err := ValidateDeviationKinds(vague); err == nil {
		t.Error("a deviation explaining nothing was accepted")
	}
}
