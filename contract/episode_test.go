package contract

// Episode contract tests.
//
// Two claims under test. First, a report or plan is EVIDENCE and never an
// instruction: nothing here grants capability. Second, the refusals are
// structural — a malformed artifact is rejected, never quietly repaired.

import (
	"strings"
	"testing"
)

func goodReport() *SemanticReport {
	return &SemanticReport{
		Schema: EpisodeVersion, EpisodeID: "EP-1", Attempt: 1,
		Objective: "Make the build pass", EvidenceDigest: strings.Repeat("a", 64),
		SourceDigest: strings.Repeat("b", 64), ObservedProfile: "investigator",
		Findings: []ConfirmedFinding{{
			FindingID: "F1", RequirementID: "R1",
			Expected: "exit zero", Observed: "exit 2",
			EvidenceRefs: []string{"E1"}, Unknowns: []string{"why in CI only"},
			AlternativesChecked: []string{"dependency drift"},
		}},
	}
}

func TestEpisode_ReportMustStateEvidenceAndUnknowns(t *testing.T) {
	if err := ValidateSemanticReport(goodReport()); err != nil {
		t.Fatalf("a well-formed report was refused: %v", err)
	}
	// No evidence: an unsupported finding is worse than an absent one.
	r := goodReport()
	r.Findings[0].EvidenceRefs = nil
	if err := ValidateSemanticReport(r); err == nil {
		t.Error("a finding citing no evidence was accepted")
	}
	// No unknowns: a report that cannot say what it does not know claims too much.
	r = goodReport()
	r.Findings[0].Unknowns = nil
	if err := ValidateSemanticReport(r); err == nil {
		t.Error("a finding that does not state its unknowns was accepted")
	}
	// Expected and observed cannot be conflated.
	r = goodReport()
	r.Findings[0].Expected = ""
	if err := ValidateSemanticReport(r); err == nil {
		t.Error("a finding stating only one side of expected/observed was accepted")
	}
}

// TestEpisode_ReportCannotBeReusedAcrossEpisodes pins the evidence binding.
func TestEpisode_ReportCannotBeReusedAcrossEpisodes(t *testing.T) {
	r := goodReport()
	r.EvidenceDigest = ""
	if err := ValidateSemanticReport(r); err == nil {
		t.Error("a report pinning no evidence digest was accepted")
	}
}

// TestEpisode_PlanStepsMustBeBoundedAndLinked is the anti-grant-widening check.
func TestEpisode_PlanStepsMustBeBoundedAndLinked(t *testing.T) {
	p := &ImplementationPlan{
		Schema: EpisodeVersion, EpisodeID: "EP-1", Version: 1,
		AcceptedReportDigest: strings.Repeat("a", 64),
		SourceBaseline:       strings.Repeat("b", 64), Grant: "read-write",
		ExecutionProfile: "worker", ReviewProfile: "auditor",
		Steps: []PlanStep{{
			StepID: "S1", Findings: []string{"F1"}, AllowedChangeKinds: []string{"source"},
			AcceptanceCriteria: "build passes",
		}},
	}
	if err := ValidateImplementationPlan(p); err != nil {
		t.Fatalf("a well-formed plan was refused: %v", err)
	}
	// An unbounded step is the grant problem in miniature.
	q := *p
	q.Steps = []PlanStep{{StepID: "S1", Findings: []string{"F1"}, AcceptanceCriteria: "x"}}
	if err := ValidateImplementationPlan(&q); err == nil {
		t.Error("a step declaring no allowed change kinds was accepted")
	}
	// A step not linked to any finding has no reason to exist.
	q = *p
	q.Steps = []PlanStep{{StepID: "S1", AllowedChangeKinds: []string{"source"}, AcceptanceCriteria: "x"}}
	if err := ValidateImplementationPlan(&q); err == nil {
		t.Error("a step linked to no report finding was accepted")
	}
	// A dangling dependency is not an ordering the system can execute.
	q = *p
	q.Steps = []PlanStep{{
		StepID: "S1", Findings: []string{"F1"}, AllowedChangeKinds: []string{"source"},
		AcceptanceCriteria: "x", DependsOn: []string{"S9"},
	}}
	if err := ValidateImplementationPlan(&q); err == nil {
		t.Error("a step depending on a nonexistent step was accepted")
	}
	// A step with no dependencies must not panic validation.
	q = *p
	q.Steps = []PlanStep{{
		StepID: "S1", Findings: []string{"F1"}, AllowedChangeKinds: []string{"source"},
		AcceptanceCriteria: "x",
	}}
	if err := ValidateImplementationPlan(&q); err != nil {
		t.Errorf("a step with no dependencies was refused: %v", err)
	}
}

// TestEpisode_PlanCannotWidenItsGrant is the central authorization property.
func TestEpisode_PlanCannotWidenItsGrant(t *testing.T) {
	p := &ImplementationPlan{
		Schema: EpisodeVersion, EpisodeID: "EP-1", Version: 1,
		AcceptedReportDigest: strings.Repeat("a", 64), Grant: "read-only",
		SourceBaseline:   strings.Repeat("b", 64),
		ExecutionProfile: "worker", ReviewProfile: "auditor",
		Steps: []PlanStep{{StepID: "S1", Findings: []string{"F1"},
			AllowedChangeKinds: []string{"source"}, AcceptanceCriteria: "x"}},
	}
	good := &PlanAuthorization{
		Schema: EpisodeVersion, EpisodeID: "EP-1", PlanVersion: 1,
		PlanDigest: strings.Repeat("c", 64), ReportDigest: p.AcceptedReportDigest,
		SourceBaseline: p.SourceBaseline, Grant: p.Grant,
		AcceptedBy: "operator", AcceptedAt: "2026-10-10T00:00:00Z",
	}
	if err := ValidatePlanAuthorization(good, p); err != nil {
		t.Fatalf("a well-formed authorization was refused: %v", err)
	}
	// A WIDER grant than the plan was written under.
	wide := *good
	wide.Grant = "read-write"
	if err := ValidatePlanAuthorization(&wide, p); err == nil {
		t.Error("an authorization granted more capability than the plan was written under")
	}
	// The implementer approving its own plan makes the separation decorative.
	selfApproved := *good
	selfApproved.AcceptedBy = p.ExecutionProfile
	if err := ValidatePlanAuthorization(&selfApproved, p); err == nil {
		t.Error("the execution profile was allowed to accept its own plan")
	}
	// An authorization for a different report.
	other := *good
	other.ReportDigest = strings.Repeat("d", 64)
	if err := ValidatePlanAuthorization(&other, p); err == nil {
		t.Error("a plan was authorized against a report it does not answer")
	}
}

// TestEpisode_AdmissionRefusesAMutableTemplate is the no-program rule.
func TestEpisode_AdmissionRefusesAMutableTemplate(t *testing.T) {
	ok := `{"version":1,"episode_id":"EP-1","origin":{"kind":"forge","caller_id":"f"},
	       "workspace":{"path":"/tmp"},"objective":"investigate","template":"forge-investigation",
	       "obligations":[{"id":"O1","criterion":"The build exits zero."}],
	       "constraints":{"allow_code_changes":false}}`
	if _, err := DecodeEpisodeRequest([]byte(ok)); err != nil {
		t.Fatalf("a well-formed episode request was refused: %v", err)
	}
	// An episode with no obligations has nothing to be judged against, so every
	// completion verdict would pass vacuously.
	noObligations := strings.Replace(ok,
		`"obligations":[{"id":"O1","criterion":"The build exits zero."}],`, `"obligations":[],`, 1)
	if _, err := DecodeEpisodeRequest([]byte(noObligations)); err == nil {
		t.Error("an episode stating no obligations was admitted")
	}
	// A caller may not smuggle control configuration through the request.
	bad := strings.Replace(ok, `"template":"forge-investigation"`,
		`"template":"forge","checks":[{"argv":["rm","-rf","/"]}]`, 1)
	if _, err := DecodeEpisodeRequest([]byte(bad)); err == nil {
		t.Error("a caller-supplied check command was accepted into an episode request")
	}
	// Version is separate from the task contract version.
	wrong := strings.Replace(ok, `"version":1`, `"version":99`, 1)
	if _, err := DecodeEpisodeRequest([]byte(wrong)); err == nil {
		t.Error("an unsupported episode version was accepted")
	}
}

// TestEpisode_DigestChangesWithContent is what makes acceptance binding.
func TestEpisode_DigestChangesWithContent(t *testing.T) {
	a := goodReport()
	b := goodReport()
	d1, _ := a.Digest()
	d2, _ := b.Digest()
	if d1 != d2 {
		t.Error("identical reports produced different digests")
	}
	b.Findings[0].Observed = "exit 0"
	d3, _ := b.Digest()
	if d1 == d3 {
		t.Error("changing a finding's observed behaviour did not change the digest")
	}
}
