package control

// Episode template registry tests.
//
// The claim under test is almost entirely about REFUSALS. A happy-path test
// proves the happy path works; it says nothing about the case that matters, which
// is a template that looks locally approved but could never produce an
// independent audit — the failure that silently turns a checked episode into a
// self-certified one. So most of what follows asserts that something is REJECTED.

import (
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
)

// fullTemplate is a valid baseline that individual tests break in one way.
func fullTemplate() EpisodeTemplate {
	return EpisodeTemplate{
		Name: "standard_fix",
		Phases: []contract.EpisodePhase{
			contract.PhaseReport, contract.PhaseReportReview, contract.PhasePlan,
			contract.PhaseImplement, contract.PhaseVerify, contract.PhaseCompletionReview,
		},
		Roles: map[ProfileRole]string{
			RoleWorker:   "impl",
			RoleReviewer: "audit",
		},
		RequiredChecks: []string{"build", "test"},
		ReturnEdges: map[contract.EpisodePhase][]contract.EpisodePhase{
			contract.PhaseReportReview:     {contract.PhaseReport},
			contract.PhaseImplement:        {contract.PhaseImplement},
			contract.PhaseVerify:           {contract.PhaseImplement},
			contract.PhaseCompletionReview: {contract.PhaseImplement},
		},
		MaxRepairIterations: 2,
	}
}

// mustRegister registers into the registry under test, failing on any refusal.
func mustRegister(t *testing.T, reg *EpisodeTemplateRegistry, tpl EpisodeTemplate) {
	t.Helper()
	if err := reg.Register(tpl); err != nil {
		t.Fatalf("register %q: unexpected refusal: %v", tpl.Name, err)
	}
}

// TestEpisodeTemplate_NoReviewerIsRefused is the important one.
//
// A template with no reviewer cannot be audited by anything, ever. Admitting it
// would mean running an episode to completion with no independent judge — the
// episode still produces a COMPLETION_REVIEW phase, it just has no session that
// could fill it, so the audit is decorative from the first phase.
func TestEpisodeTemplate_NoReviewerIsRefused(t *testing.T) {
	tpl := fullTemplate()
	delete(tpl.Roles, RoleReviewer)

	var reg EpisodeTemplateRegistry
	err := reg.Register(tpl)
	if err == nil {
		t.Fatal("a template with no reviewer was admitted; nothing could ever audit it")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("refusal does not name the missing reviewer: %v", err)
	}
	if len(reg.Names()) != 0 {
		t.Fatalf("a refused template was registered anyway: %v", reg.Names())
	}
}

// TestEpisodeTemplate_WorkerAsReviewerIsRefused covers the subtler version of the
// same defect: the reviewer role IS named, but it resolves to the implementer.
func TestEpisodeTemplate_WorkerAsReviewerIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Roles[RoleReviewer] = tpl.Roles[RoleWorker]

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a template whose reviewer is the implementer was admitted; the review is not independent")
	}
}

// TestEpisodeTemplate_DuplicateNameIsRefused asserts overwrite is not allowed.
//
// Two templates sharing a name is how an approved policy silently becomes a
// different one: a later registration with a laxer shape wins, and every request
// that referenced the name by trust is now running under policy nobody approved
// for it.
func TestEpisodeTemplate_DuplicateNameIsRefused(t *testing.T) {
	var reg EpisodeTemplateRegistry
	mustRegister(t, &reg, fullTemplate())

	// Weakened in exactly one way: a far larger repair budget. Everything else
	// is valid, so the ONLY reason this can be refused is the duplicate name —
	// otherwise this test would pass for the wrong reason.
	weakened := fullTemplate()
	weakened.MaxRepairIterations = 99
	if err := reg.Register(weakened); err == nil {
		t.Fatal("a duplicate template name was admitted; overwrite must not be allowed")
	}

	got, ok := reg.Lookup("standard_fix")
	if !ok {
		t.Fatal("the original template disappeared")
	}
	if got.MaxRepairIterations != 2 {
		t.Fatalf("the refused duplicate overwrote the approved template: MaxRepairIterations=%d, want 2", got.MaxRepairIterations)
	}
}

// TestEpisodeTemplate_ReturnEdgeToUndeclaredPhaseIsRefused asserts a return edge
// cannot name a phase the template does not declare.
func TestEpisodeTemplate_ReturnEdgeToUndeclaredPhaseIsRefused(t *testing.T) {
	tpl := fullTemplate()
	// PLAN_ACCEPT is a real phase, but this template does not declare it.
	tpl.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{
		contract.PhaseVerify: {contract.PhasePlanAccept},
	}

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a return edge to an undeclared phase was admitted")
	}
}

// TestEpisodeTemplate_ReturnEdgeFromUndeclaredPhaseIsRefused covers the other end
// of the same edge.
func TestEpisodeTemplate_ReturnEdgeFromUndeclaredPhaseIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{
		contract.PhasePlanAccept: {contract.PhaseReport},
	}

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a return edge from an undeclared phase was admitted")
	}
}

// TestEpisodeTemplate_ForwardEdgeAsReturnIsRefused asserts a "return" that moves
// forward is a modelling error rather than a lenient permission.
//
// VERIFY -> IMPLEMENT in this template happens to be a real backwards edge, so
// the forward case is spelled explicitly: REPORT -> PLAN advances normally, so
// declaring it as a return is restating an ordinary step in a map that exists to
// record revisions.
func TestEpisodeTemplate_ForwardEdgeAsReturnIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{
		contract.PhaseReport: {contract.PhasePlan},
	}

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a forward move declared as a return edge was admitted")
	}
}

// Report/planning self edges still do not create a bounded implementation retry.
func TestEpisodeTemplate_SelfReturnEdgeIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{
		contract.PhaseReport: {contract.PhaseReport},
	}

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a self return edge was admitted")
	}
}

// TestEpisodeTemplate_DuplicatePhaseIsRefused asserts a repeated phase is refused.
//
// Judgement call, stated explicitly because the question was left open: this is
// NOT a duplicate-NAME case. Phases are documented as "in the order they occur",
// so listing one twice leaves the shape with no single reading — there is no
// longer one answer to which occurrence is meant. It is refused for the same
// reason plan step ids and finding ids are in the contract: an ambiguous
// identifier is a defect, not a harmless repetition.
func TestEpisodeTemplate_DuplicatePhaseIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Phases = append(tpl.Phases, contract.PhasePlan)

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a phase listed twice was admitted")
	}
}

// TestEpisodeTemplate_UnknownPhaseIsRefused asserts a phase outside
// contract.PhaseOrder is refused, since Advances can never reach it.
func TestEpisodeTemplate_UnknownPhaseIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Phases = append(tpl.Phases, contract.EpisodePhase("SHIP"))

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a phase outside contract.PhaseOrder was admitted; it could never be entered")
	}
}

// TestEpisodeTemplate_EmptyNameIsRefused asserts an unnamed template is refused,
// since a request references a template by name.
func TestEpisodeTemplate_EmptyNameIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Name = "   "

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a template with an empty name was admitted")
	}
}

// TestEpisodeTemplate_EmptyPhaseListIsRefused asserts a template with no phases is
// refused: it admits an episode that cannot begin.
func TestEpisodeTemplate_EmptyPhaseListIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Phases = nil

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a template with no phases was admitted")
	}
}

// TestEpisodeTemplate_NonPositiveRepairIterationsIsRefused covers zero and
// negative, the boundary either side of the only legal value.
func TestEpisodeTemplate_NonPositiveRepairIterationsIsRefused(t *testing.T) {
	for _, v := range []int{0, -1} {
		tpl := fullTemplate()
		tpl.MaxRepairIterations = v
		var reg EpisodeTemplateRegistry
		if err := reg.Register(tpl); err == nil {
			t.Fatalf("MaxRepairIterations=%d was admitted; the repair budget must be positive", v)
		}
	}
	// One is the boundary that must still be admitted.
	var reg EpisodeTemplateRegistry
	tpl := fullTemplate()
	tpl.MaxRepairIterations = 1
	mustRegister(t, &reg, tpl)
}

// TestEpisodeTemplate_EmptyRoleProfileIsRefused asserts a role bound to nothing is
// refused.
func TestEpisodeTemplate_EmptyRoleProfileIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Roles[RoleReviewer] = ""

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("a reviewer role naming no profile was admitted")
	}
}

// TestEpisodeTemplate_UnknownRoleIsRefused asserts a typo'd role key is refused
// rather than ignored.
func TestEpisodeTemplate_UnknownRoleIsRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.Roles[ProfileRole("reviewer_typo")] = "audit"

	var reg EpisodeTemplateRegistry
	if err := reg.Register(tpl); err == nil {
		t.Fatal("an unrecognised role key was admitted")
	}
}

// TestEpisodeTemplate_AllowsMove asserts the move rule: forward moves and
// declared return edges are permitted, everything else is refused.
func TestEpisodeTemplate_AllowsMove(t *testing.T) {
	tpl := fullTemplate()

	cases := []struct {
		from, to contract.EpisodePhase
		want     bool
		why      string
	}{
		{contract.PhaseReport, contract.PhasePlan, true, "forward move"},
		{contract.PhaseVerify, contract.PhaseCompletionReview, true, "forward move"},
		{contract.PhaseImplement, contract.PhaseVerify, true, "forward move"},
		{contract.PhaseVerify, contract.PhaseImplement, true, "declared return edge"},
		{contract.PhaseVerify, contract.PhasePlanAccept, false, "backwards move nobody declared"},
		{contract.PhaseCompletionReview, contract.PhaseReport, false, "backwards move nobody declared"},
		{contract.PhasePlan, contract.PhasePlan, false, "self move is not a forward step"},
		{contract.PhaseImplement, contract.PhaseReportReview, false, "backwards move nobody declared"},
		{contract.PhaseReport, "UNKNOWN", false, "move to a phase that is not a phase"},
	}
	for _, c := range cases {
		if got := tpl.AllowsMove(c.from, c.to); got != c.want {
			t.Errorf("AllowsMove(%s -> %s) = %v, want %v (%s)", c.from, c.to, got, c.want, c.why)
		}
	}
}

// TestEpisodeTemplate_AllowsMoveUndeclaredReturnEdge asserts a return edge that
// exists in the map for a DIFFERENT source phase does not permit this move.
func TestEpisodeTemplate_AllowsMoveUndeclaredReturnEdge(t *testing.T) {
	tpl := fullTemplate()
	tpl.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{
		contract.PhaseVerify: {contract.PhaseImplement},
	}
	if tpl.AllowsMove(contract.PhaseCompletionReview, contract.PhaseImplement) {
		t.Fatal("a return edge declared from VERIFY permitted an unrelated move")
	}
}

// TestEpisodeTemplate_AllowsPhase asserts phase admission follows the declared
// list exactly.
func TestEpisodeTemplate_AllowsPhase(t *testing.T) {
	tpl := fullTemplate()
	for _, p := range tpl.Phases {
		if !tpl.AllowsPhase(p) {
			t.Errorf("AllowsPhase(%s) = false for a declared phase", p)
		}
	}
	if tpl.AllowsPhase(contract.PhasePlanAccept) {
		t.Error("AllowsPhase(PLAN_ACCEPT) = true for a phase this template does not declare")
	}
	if tpl.AllowsPhase("SHIP") {
		t.Error("AllowsPhase(SHIP) = true for a phase that is not a phase at all")
	}
}

// TestEpisodeTemplate_RegistryLookupAndNames asserts lookup and sorted names.
func TestEpisodeTemplate_RegistryLookupAndNames(t *testing.T) {
	var reg EpisodeTemplateRegistry
	a := fullTemplate()
	a.Name = "zulu"
	b := fullTemplate()
	b.Name = "alpha"

	mustRegister(t, &reg, a)
	mustRegister(t, &reg, b)

	if _, ok := reg.Lookup("  alpha  "); !ok {
		t.Error("Lookup did not tolerate surrounding whitespace on a registered name")
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Error("Lookup found a template that was never registered")
	}
	names := reg.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zulu" {
		t.Errorf("Names() = %v, want sorted [alpha zulu]", names)
	}
}

// TestEpisodeTemplate_LookupReturnsCopy asserts the registry's approved policy
// cannot be edited by whoever holds the copy it returned.
func TestEpisodeTemplate_LookupReturnsCopy(t *testing.T) {
	var reg EpisodeTemplateRegistry
	original := fullTemplate()
	mustRegister(t, &reg, original)

	got, ok := reg.Lookup(original.Name)
	if !ok {
		t.Fatal("registered template not found")
	}
	got.Phases[0] = contract.PhaseCompletionReview
	got.ReturnEdges[contract.PhaseVerify][0] = contract.PhaseReport
	got.Roles[RoleReviewer] = "attacker"

	fresh, _ := reg.Lookup(original.Name)
	if fresh.Phases[0] != contract.PhaseReport {
		t.Error("mutating the returned phases changed the registered template")
	}
	if fresh.ReturnEdges[contract.PhaseVerify][0] != contract.PhaseImplement {
		t.Error("mutating the returned return edges changed the registered template")
	}
	if fresh.Roles[RoleReviewer] != "audit" {
		t.Error("mutating the returned roles changed the registered template")
	}
}

// TestEpisodeTemplate_ProfileFor asserts role resolution, including that an
// unbound role reports absent rather than an empty string.
func TestEpisodeTemplate_ProfileFor(t *testing.T) {
	tpl := fullTemplate()
	if name, ok := tpl.ProfileFor(RoleWorker); !ok || name != "impl" {
		t.Errorf("ProfileFor(worker) = %q,%v; want impl,true", name, ok)
	}
	if name, ok := tpl.ProfileFor(RoleReviewer); !ok || name != "audit" {
		t.Errorf("ProfileFor(reviewer) = %q,%v; want audit,true", name, ok)
	}
	if name, ok := tpl.ProfileFor(RoleCheckpoint); ok || name != "" {
		t.Errorf("ProfileFor(checkpoint) = %q,%v; want \"\",false", name, ok)
	}
	if !tpl.RequiresReviewer() {
		t.Error("RequiresReviewer() = false for a template that names a reviewer")
	}
	var zero EpisodeTemplate
	if zero.RequiresReviewer() {
		t.Error("a zero-value template claimed a reviewer")
	}
}

// TestLoadEpisodeTemplates_ParsesWellFormedTable asserts the loader reads the
// documented shape.
func TestLoadEpisodeTemplates_ParsesWellFormedTable(t *testing.T) {
	doc := map[string]any{
		"episode_templates": map[string]any{
			"standard_fix": map[string]any{
				"phases":                []string{"REPORT", "report_review", "PLAN", "IMPLEMENT", "VERIFY", "COMPLETION_REVIEW"},
				"roles":                 []string{"worker=impl", "reviewer=audit"},
				"required_checks":       []string{"build", "test"},
				"return_edges":          []string{"REPORT_REVIEW=REPORT", "IMPLEMENT=IMPLEMENT", "VERIFY=IMPLEMENT", "COMPLETION_REVIEW=IMPLEMENT"},
				"max_repair_iterations": 2,
				"max_wall_seconds":      10800,
				"read_only":             "true",
			},
		},
	}
	reg, err := loadEpisodeTemplates(doc)
	if err != nil {
		t.Fatalf("loadEpisodeTemplates: %v", err)
	}
	got, ok := reg.Lookup("standard_fix")
	if !ok {
		t.Fatal("standard_fix was not registered")
	}
	if len(got.Phases) != 6 || got.Phases[1] != contract.PhaseReportReview {
		t.Errorf("phases = %v; want the six declared phases, case-normalised", got.Phases)
	}
	if got.Roles[RoleWorker] != "impl" || got.Roles[RoleReviewer] != "audit" {
		t.Errorf("roles = %v; want worker=impl reviewer=audit", got.Roles)
	}
	if len(got.RequiredChecks) != 2 {
		t.Errorf("required_checks = %v; want [build test]", got.RequiredChecks)
	}
	if !got.AllowsMove(contract.PhaseVerify, contract.PhaseImplement) {
		t.Error("the parsed return edge VERIFY->IMPLEMENT is not permitted")
	}
	if got.MaxWallSeconds != 10800 {
		t.Fatalf("max_wall_seconds=%d", got.MaxWallSeconds)
	}
	if got.MaxRepairIterations != 2 {
		t.Errorf("max_repair_iterations = %d, want 2", got.MaxRepairIterations)
	}
	if !got.ReadOnly {
		t.Error("read_only = false, want true")
	}
}

// TestLoadEpisodeTemplates_Malformed is a table of malformed inputs that must all
// be refused, each for a different reason.
func TestLoadEpisodeTemplates_Malformed(t *testing.T) {
	cases := []struct {
		name string
		doc  map[string]any
	}{
		{"present but empty", map[string]any{"episode_templates": map[string]any{}}},
		{"not a table", map[string]any{"episode_templates": "nope"}},
		{"entry not a table", map[string]any{"episode_templates": map[string]any{"t": "nope"}}},
		{"no phases", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{}, "roles": []string{"reviewer=audit"}, "max_repair_iterations": 1}}}},
		{"no reviewer", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT"}, "roles": []string{"worker=impl"}, "max_repair_iterations": 1}}}},
		{"zero repair iterations", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT"}, "roles": []string{"reviewer=audit"}, "max_repair_iterations": 0}}}},
		{"roles not an array", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT"}, "roles": "reviewer=audit", "max_repair_iterations": 1}}}},
		{"role entry without =", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT"}, "roles": []string{"reviewer"}, "max_repair_iterations": 1}}}},
		{"duplicate role key", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT"}, "roles": []string{"reviewer=audit", "reviewer=other"}, "max_repair_iterations": 1}}}},
		{"return edge not an array", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT", "VERIFY"}, "roles": []string{"reviewer=audit"},
				"return_edges": "VERIFY=REPORT", "max_repair_iterations": 1}}}},
		{"return edge to undeclared phase", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT", "VERIFY"}, "roles": []string{"reviewer=audit"},
				"return_edges": []string{"VERIFY=IMPLEMENT"}, "max_repair_iterations": 1}}}},
		{"forward edge as return", map[string]any{"episode_templates": map[string]any{
			"t": map[string]any{"phases": []string{"REPORT", "PLAN"}, "roles": []string{"reviewer=audit"},
				"return_edges": []string{"REPORT=PLAN"}, "max_repair_iterations": 1}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := loadEpisodeTemplates(c.doc); err == nil {
				t.Fatalf("%s was accepted; a malformed episode_templates table must be refused", c.name)
			}
		})
	}
}

// TestLoadEpisodeTemplates_AbsentIsOptional asserts a deployment with no approved
// episode shape still loads an empty registry, mirroring `profiles`.
func TestLoadEpisodeTemplates_AbsentIsOptional(t *testing.T) {
	reg, err := loadEpisodeTemplates(map[string]any{})
	if err != nil {
		t.Fatalf("an absent episode_templates table was refused: %v", err)
	}
	if len(reg.Names()) != 0 {
		t.Errorf("Names() = %v, want empty", reg.Names())
	}
}

func TestEpisodeTemplate_NegativeWallAllowanceRefused(t *testing.T) {
	tpl := fullTemplate()
	tpl.MaxWallSeconds = -1
	reg := NewEpisodeTemplateRegistry()
	if err := reg.Register(tpl); err == nil {
		t.Fatal("negative wall allowance accepted")
	}
}
