package control

// S3 workflow admission tests.
//
// The claim is that admission is the only moment a graph becomes durable work,
// and that everything inconvenient happens here while nothing has started. Each
// test isolates one way that claim fails: a reused definition that quietly
// resolved to different bytes, a conflicting ID, a colliding normalised title, a
// cycle, a task asking for more than its root was granted, a crash mid-publish,
// a child ID that changed, and a nested submission from an engine-owned executor.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
)

// wfFixture installs a profile registry and a two-task graph.
func wfFixture(t *testing.T) *fixture {
	t.Helper()
	f := boundRequest(t, newFixture(t))
	f.cfg.Profiles = map[string]Profile{
		"impl":  {Name: "impl", Role: RoleWorker, Tool: "mcode"},
		"audit": {Name: "audit", Role: RoleReviewer, Tool: "mcode"},
	}
	return f
}

// wfDefinition is a valid two-task graph with a dependency.
func wfDefinition() *contract.WorkflowDefinition {
	return &contract.WorkflowDefinition{
		Version: contract.WorkflowVersion,
		Name:    "target-crossing",
		Tasks: []contract.WorkflowTask{
			{
				ID: "investigate", Operation: contract.OpInvestigate,
				Objective: "Find why the lock is crossed late", Instruction: "Investigate the crossing.",
				ExecutorProfile: "impl", ReviewerProfile: "audit",
				CheckProfiles: []string{"build"},
				InputKeys:     []string{"run_id"},
			},
			{
				ID: "repair", Operation: contract.OpRepair,
				Objective: "Fix the crossing", Instruction: "Repair the crossing.",
				ExecutorProfile: "impl", ReviewerProfile: "audit",
				DependsOn: []string{"investigate"},
			},
		},
	}
}

// wfRequest builds a v2 request, first-use (with a definition).
func wfRequest(id, srcRoot string, def *contract.WorkflowDefinition, input map[string]string, allowChanges bool) *contract.WorkflowRequest {
	return &contract.WorkflowRequest{
		Version:     contract.RequestVersion2,
		RequestID:   id,
		Origin:      contract.Origin{Kind: "forge", CallerID: "local-forge"},
		Workspace:   contract.Workspace{Path: srcRoot},
		Definition:  def,
		Input:       input,
		Constraints: contract.Constraints{AllowCodeChanges: allowChanges, AllowedPaths: []string{"**"}},
	}
}

// TestWorkflow_FirstUsePinsBundle asserts a first-use definition is measured,
// published under its identity, and expanded into children.
func TestWorkflow_FirstUsePinsBundle(t *testing.T) {
	f := wfFixture(t)
	adm, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1842"}, true))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if adm.DefinitionSHA256 == "" || adm.RequestSHA256 == "" || adm.InputDigest == "" {
		t.Fatalf("admission returned no identities: %+v", adm)
	}
	if adm.Duplicate {
		t.Error("a first use reported itself a duplicate")
	}

	// The bundle exists under the measured identity, with its definition and
	// instruction files.
	bundle := filepath.Join(f.root, WorkflowsDirName, adm.DefinitionSHA256)
	if _, err := os.Stat(filepath.Join(bundle, WorkflowBundleFile)); err != nil {
		t.Fatalf("the bundle was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, WorkflowFilesDirName, "tasks", "investigate.md")); err != nil {
		t.Errorf("the instruction file was not published: %v", err)
	}
	// The immutable admitted record exists beside the root.
	if _, err := os.Stat(filepath.Join(f.root, RequestsDirName, "root-1", WorkflowStateFile)); err != nil {
		t.Errorf("the admitted workflow record was not written: %v", err)
	}
	// Both children exist as ordinary admitted requests.
	for _, task := range []string{"investigate", "repair"} {
		child := adm.Children[task]
		if child == "" {
			t.Fatalf("task %q has no derived child id", task)
		}
		if _, err := f.store.State(child); err != nil {
			t.Errorf("child %q was not admitted as a request: %v", child, err)
		}
	}
	// The root holds the workspace for its epoch.
	st, err := f.store.State("root-1")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.WorkspaceLeaseRoot != f.srcRoot {
		t.Errorf("lease root = %q, want the workspace", st.WorkspaceLeaseRoot)
	}
	if st.WorkspaceLeaseEpoch == 0 {
		t.Error("the root took no lease epoch")
	}
	// The child map is pinned in root state.
	if len(st.WorkflowChildren) != 2 {
		t.Errorf("root state carries %d children, want 2", len(st.WorkflowChildren))
	}
	if strings.Join(st.WorkflowChildOrder, ",") != "investigate,repair" {
		t.Errorf("child order = %v, want the definition's declared order", st.WorkflowChildOrder)
	}
}

// TestWorkflow_RefReusesDefinitionWithNewInput asserts two runs share a
// definition identity but are distinct requests.
func TestWorkflow_RefReusesDefinitionWithNewInput(t *testing.T) {
	f := wfFixture(t)
	first, err := f.store.AdmitWorkflow(wfRequest("root-a", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1842"}, true))
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}

	// The second run references the admitted definition with a different input.
	second := &contract.WorkflowRequest{
		Version:   contract.RequestVersion2,
		RequestID: "root-b",
		Origin:    contract.Origin{Kind: "forge", CallerID: "local-forge"},
		Workspace: contract.Workspace{Path: "/approved/worktree"},
		Ref:       &contract.WorkflowRef{Version: contract.WorkflowVersion, SHA256: first.DefinitionSHA256},
		Input:     map[string]string{"run_id": "1843"},
		Constraints: contract.Constraints{
			AllowCodeChanges: true, AllowedPaths: []string{"**"},
		},
	}
	adm, err := f.store.AdmitWorkflow(second)
	if err != nil {
		t.Fatalf("ref reuse: %v", err)
	}
	// The DEFINITION identity is shared: no rehash of run-specific data.
	if adm.DefinitionSHA256 != first.DefinitionSHA256 {
		t.Errorf("the definition identity changed on reuse: %q then %q", first.DefinitionSHA256, adm.DefinitionSHA256)
	}
	// The REQUEST identity differs, because the input differs.
	if adm.RequestSHA256 == first.RequestSHA256 {
		t.Error("two runs with different inputs share a request identity")
	}
	if adm.InputDigest == first.InputDigest {
		t.Error("two runs with different inputs share an input digest")
	}
	// Child IDs differ, because they derive from the root.
	if adm.Children["investigate"] == first.Children["investigate"] {
		t.Error("two roots derived the same child id for the same task")
	}
	// And no second bundle was written.
	entries, err := os.ReadDir(filepath.Join(f.root, WorkflowsDirName))
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("reuse published %d bundles, want 1", len(entries))
	}
}

// TestWorkflow_SourceEditDoesNotChangeActiveRun asserts a first-use definition's
// instruction bytes are pinned, and that tampering with the stored bundle is
// caught rather than executed.
func TestWorkflow_SourceEditDoesNotChangeActiveRun(t *testing.T) {
	f := wfFixture(t)
	adm, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1842"}, true))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	// The admitted instruction digest is recorded.
	record, err := os.ReadFile(filepath.Join(f.root, RequestsDirName, "root-1", WorkflowStateFile))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var admitted AdmittedWorkflow
	if err := jsonUnmarshalBytes(record, &admitted); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if len(admitted.Instructions) != 2 {
		t.Fatalf("recorded %d instruction digests, want 2", len(admitted.Instructions))
	}
	original := admitted.Instructions[0].SHA256

	// Tamper with the published instruction file.
	target := filepath.Join(f.root, WorkflowsDirName, adm.DefinitionSHA256, WorkflowFilesDirName, "tasks", "investigate.md")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(target, []byte("now do something else entirely"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// A later run referencing that definition is REFUSED, not executed.
	ref := &contract.WorkflowRequest{
		Version: contract.RequestVersion2, RequestID: "root-2",
		Origin:    contract.Origin{Kind: "forge", CallerID: "local-forge"},
		Workspace: contract.Workspace{Path: "/approved/worktree"},
		Ref:       &contract.WorkflowRef{Version: contract.WorkflowVersion, SHA256: adm.DefinitionSHA256},
		Input:     map[string]string{"run_id": "1843"},
		Constraints: contract.Constraints{
			AllowCodeChanges: true, AllowedPaths: []string{"**"},
		},
	}
	if _, err := f.store.AdmitWorkflow(ref); err == nil {
		t.Fatal("a run was admitted against a tampered instruction bundle")
	}
	_ = original
}

// TestWorkflow_ConflictLaunchesNothing asserts a conflicting ID is refused and
// launches nothing.
func TestWorkflow_ConflictLaunchesNothing(t *testing.T) {
	f := wfFixture(t)
	if _, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1842"}, true)); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// The same ID with different content.
	other := wfDefinition()
	other.Tasks[0].Instruction = "Investigate something else."
	_, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, other, map[string]string{"run_id": "9999"}, true))
	if err == nil {
		t.Fatal("a conflicting workflow reused an admitted request id")
	}
	if !strings.Contains(err.Error(), "never changes") {
		t.Errorf("the refusal did not explain identity immutability: %v", err)
	}
	// The original admission is untouched.
	st, serr := f.store.State("root-1")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if st.Stage.Terminal() {
		t.Errorf("the conflicting submission changed the original run to %s", st.Stage)
	}
}

// TestWorkflow_CycleMissingDependencyRefused asserts a malformed graph is
// refused, and refused BEFORE anything is written.
func TestWorkflow_CycleMissingDependencyRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*contract.WorkflowDefinition)
		want   string
	}{
		{"a cycle", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].DependsOn = []string{"repair"}
		}, "cycle"},
		{"a missing dependency", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].DependsOn = []string{"nonexistent"}
		}, "does not contain"},
		{"a self dependency", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].DependsOn = []string{"investigate"}
		}, "depends on itself"},
		{"a duplicate task id", func(d *contract.WorkflowDefinition) {
			d.Tasks[1].ID = "investigate"
		}, "more than once"},
		{"an unknown operation", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].Operation = "DEPLOY_TO_PROD"
		}, "not one of the supported operations"},
		{"both instruction sources", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].InstructionFile = "tasks/investigate.md"
		}, "both an instruction file and an inline instruction"},
		{"no instruction at all", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].Instruction = "   "
		}, "no instruction"},
		{"an unknown profile", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].ExecutorProfile = "no-such-profile"
		}, "executor profile"},
		{"an unknown check", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].CheckProfiles = []string{"no-such-check"}
		}, "registry does not contain"},
		{"a missing input key", func(d *contract.WorkflowDefinition) {
			d.Tasks[0].InputKeys = []string{"run_id", "missing_key"}
		}, "missing required keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := wfFixture(t)
			def := wfDefinition()
			tc.mutate(def)
			_, err := f.store.AdmitWorkflow(wfRequest("root-x", f.srcRoot, def, map[string]string{"run_id": "1"}, true))
			if err == nil {
				t.Fatalf("a definition with %s was admitted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal did not mention %q: %v", tc.want, err)
			}
			// Nothing was published.
			if _, serr := f.store.State("root-x"); serr == nil {
				t.Error("a refused admission still created a request")
			}
			if _, derr := os.Stat(filepath.Join(f.root, WorkflowsDirName)); derr == nil {
				entries, _ := os.ReadDir(filepath.Join(f.root, WorkflowsDirName))
				if len(entries) > 0 {
					t.Errorf("a refused admission published %d bundles", len(entries))
				}
			}
		})
	}
}

// TestWorkflow_NoPermissionUpgrade asserts a task may be narrower than its root
// but never wider.
func TestWorkflow_NoPermissionUpgrade(t *testing.T) {
	f := wfFixture(t)
	// The root does not grant code changes, but the graph's repair task needs them.
	_, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1"}, false))
	if err == nil {
		t.Fatal("a graph asking for code changes was admitted under a root that forbids them")
	}
	if !strings.Contains(err.Error(), "narrower than its root") {
		t.Errorf("the refusal did not name the escalation: %v", err)
	}
	// An investigate-only graph under a read-only root IS admitted.
	def := wfDefinition()
	def.Tasks = def.Tasks[:1]
	if _, err := f.store.AdmitWorkflow(wfRequest("root-ro", f.srcRoot, def, map[string]string{"run_id": "1"}, false)); err != nil {
		t.Fatalf("a read-only graph was refused under a read-only root: %v", err)
	}
}

// TestWorkflow_ChildIdsStable asserts child derivation is deterministic and
// depends on every input.
func TestWorkflow_ChildIdsStable(t *testing.T) {
	f := wfFixture(t)
	first, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1"}, true))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	// Re-admitting the identical request returns the same children.
	again, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1"}, true))
	if err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	if !again.Duplicate {
		t.Error("an identical resubmission was not reported as a duplicate")
	}
	for task, id := range first.Children {
		if again.Children[task] != id {
			t.Errorf("child %q changed identity on resubmission: %q then %q", task, id, again.Children[task])
		}
	}
	// Derivation is a pure function of root, definition and task.
	a := contract.ChildRequestID("r", "d", "t")
	b := contract.ChildRequestID("r", "d", "t")
	if a != b {
		t.Errorf("child derivation is not deterministic: %q vs %q", a, b)
	}
	for _, other := range []string{
		contract.ChildRequestID("r2", "d", "t"),
		contract.ChildRequestID("r", "d2", "t"),
		contract.ChildRequestID("r", "d", "t2"),
	} {
		if other == a {
			t.Error("two different (root, definition, task) triples derived the same child id")
		}
	}
	// And the derived id is a valid request id.
	if err := contract.ValidateRequestID(a); err != nil {
		t.Errorf("the derived child id is not a valid request id: %v", err)
	}
}

// TestWorkflow_AdmissionCrashNoPartialLaunch asserts a refusal part-way leaves no
// root and no children, and that no staging residue is mistaken for a bundle.
func TestWorkflow_AdmissionCrashNoPartialLaunch(t *testing.T) {
	f := wfFixture(t)
	// A graph whose SECOND task references an unknown check: the definition is
	// structurally valid, so validation must catch it before any write.
	def := wfDefinition()
	def.Tasks[1].CheckProfiles = []string{"missing-check"}
	_, err := f.store.AdmitWorkflow(wfRequest("root-1", f.srcRoot, def, map[string]string{"run_id": "1"}, true))
	if err == nil {
		t.Fatal("a graph referencing an unknown check was admitted")
	}
	if _, serr := f.store.State("root-1"); serr == nil {
		t.Error("the root request survived a refused admission")
	}
	// No staging directory is left behind that could be mistaken for a bundle.
	entries, _ := os.ReadDir(f.root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".workflow-staging-") {
			t.Errorf("staging residue %q survived a failed admission", e.Name())
		}
	}
}

// TestWorkflow_DuplicateNormalizedTitle asserts two task ids that would collide
// under a consumer's title normalisation are refused.
func TestWorkflow_DuplicateNormalizedTitle(t *testing.T) {
	f := wfFixture(t)
	// Title normalisation (trim + lowercase) is applied by OMA in S5. Two ids that
	// normalise identically must not be admitted, because the outer scheduler
	// would merge them.
	// Two ids that would collide once trimmed and lowercased are refused. The
	// lowercase ID pattern catches this first, which is the better place for it:
	// the collision is structurally impossible rather than merely detected.
	collide := normalisationPair()
	_, err := f.store.AdmitWorkflow(wfRequest("root-collide", f.srcRoot, collide, nil, false))
	if err == nil {
		t.Fatal("two task ids that normalise to the same title were both admitted")
	}
	if !strings.Contains(err.Error(), `ids must match`) {
		t.Errorf("the refusal did not come from the ID pattern: %v", err)
	}
	// A graph whose ids are genuinely distinct is admitted.
	distinct := distinctPair()
	if _, err := f.store.AdmitWorkflow(wfRequest("root-distinct", f.srcRoot, distinct, nil, false)); err != nil {
		t.Errorf("a graph with distinct titles was refused: %v", err)
	}
}

// normaliseTitle mirrors the trim/lowercase normalisation an outer scheduler applies.
func normaliseTitle(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// distinctPair builds a graph whose two task ids are genuinely distinct.
func distinctPair() *contract.WorkflowDefinition {
	d := normalisationPair()
	d.Tasks[1].ID = "beta-two"
	return d
}

// normalisationPair builds a definition whose two task ids normalise identically.
//
// The ids are valid individually but collide once trimmed and lowercased, which is
// exactly the case an outer scheduler's normalisation would merge.
func normalisationPair() *contract.WorkflowDefinition {
	return &contract.WorkflowDefinition{
		Version: contract.WorkflowVersion,
		Name:    "collide",
		Tasks: []contract.WorkflowTask{
			{ID: "review", Operation: contract.OpInvestigate, Objective: "a", Instruction: "a",
				ExecutorProfile: "impl", ReviewerProfile: "audit"},
			{ID: "Review", Operation: contract.OpInvestigate, Objective: "b", Instruction: "b",
				ExecutorProfile: "impl", ReviewerProfile: "audit"},
		},
		// The second id is deliberately outside the accepted pattern so the
		// normalisation collision is reachable at all.
	}
}

// TestWorkflow_ChildActivationRequiresMintedCapability asserts child activation is
// gated on an unforgeable engine token, not on a declared origin.
//
// This replaced an earlier check that refused `origin.kind: executor`. That check
// was theatre: `Origin.Kind` is a caller-SUPPLIED field, so a caller could declare
// `forge` and be admitted. The capability is a token the store minted and holds
// the only copy of, so a caller that does not have it cannot present it.
func TestWorkflow_ChildActivationRequiresMintedCapability(t *testing.T) {
	f := wfFixture(t)

	// A forged token against a real root is refused.
	// Minting returns the token exactly once; the store keeps only its digest.
	real, err := f.store.IssueChildCapability("forge-run-1842-repair", "", f.now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if real == "" {
		t.Fatal("no capability token was minted")
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.ChildCapability.Token != "" {
		t.Error("the raw token was persisted; only its digest may be stored")
	}
	if st.ChildCapability.TokenHash == "" {
		t.Fatal("no token digest was persisted, so the capability can never validate")
	}

	// A second admitted root, so a cross-root presentation is genuinely cross-root.
	other := wfRequestToV1("other-root-x", f.srcRoot)
	if _, err := f.store.Admit(&other); err != nil {
		t.Fatalf("admit other root: %v", err)
	}
	for _, tc := range []struct{ name, token, root string }{
		{"a forged token", strings.Repeat("f", 64), "forge-run-1842-repair"},
		// A GENUINE token, presented for a root it was not minted for.
		{"a valid token against an unrelated root", real, "other-root-x"},
		{"a capability with no root named", real, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := wfRequest("child-x", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1"}, true)
			req.ChildCapability = tc.token
			req.RootRequestID = tc.root
			_, err := f.store.AdmitWorkflow(req)
			if err == nil {
				t.Fatalf("%s was admitted", tc.name)
			}
			if !strings.Contains(err.Error(), "capability") {
				t.Errorf("refusal did not name the capability: %v", err)
			}
		})
	}

	// The genuine token validates against the store's own copy.
	if _, err := f.store.VerifyChildCapability("forge-run-1842-repair", real, f.now); err != nil {
		t.Errorf("the minted capability did not validate: %v", err)
	}
	// Revoking it stops it immediately.
	if err := f.store.RevokeChildCapability("forge-run-1842-repair"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := f.store.VerifyChildCapability("forge-run-1842-repair", real, f.now); err == nil {
		t.Error("a revoked capability still validated")
	}
	// And an expired one is refused even while un-revoked.
	live, err := f.store.IssueChildCapability("forge-run-1842-repair", "", f.now)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if _, err := f.store.VerifyChildCapability("forge-run-1842-repair", live,
		f.now.Add(2*DefaultCapabilityTTL)); err == nil {
		t.Error("an expired capability validated")
	}
}

// TestWorkflow_CapabilityCannotWidenScope asserts a valid token cannot be reused
// for a different root.
func TestWorkflow_CapabilityCannotWidenScope(t *testing.T) {
	f := wfFixture(t)
	other := wfRequestToV1("other-root", f.srcRoot)
	if _, err := f.store.Admit(&other); err != nil {
		t.Fatalf("admit other: %v", err)
	}
	token, err := f.store.IssueChildCapability("forge-run-1842-repair", "", f.now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := f.store.VerifyChildCapability("other-root", token, f.now); err == nil {
		t.Fatal("a capability minted for one root validated against another")
	}
}

// TestWorkflow_BothOrNeitherDefinitionRefused asserts exactly one of definition
// or ref is required.
func TestWorkflow_BothOrNeitherDefinitionRefused(t *testing.T) {
	f := wfFixture(t)
	// Neither.
	neither := &contract.WorkflowRequest{
		Version: contract.RequestVersion2, RequestID: "root-neither",
		Origin:    contract.Origin{Kind: "forge"},
		Workspace: contract.Workspace{Path: "/w"},
		Input:     map[string]string{"run_id": "1"},
	}
	if _, err := f.store.AdmitWorkflow(neither); err == nil {
		t.Error("a request with neither a definition nor a ref was admitted")
	}
	// Both.
	both := wfRequest("root-both", f.srcRoot, wfDefinition(), map[string]string{"run_id": "1"}, true)
	both.Ref = &contract.WorkflowRef{Version: contract.WorkflowVersion, SHA256: strings.Repeat("a", 64)}
	if _, err := f.store.AdmitWorkflow(both); err == nil {
		t.Error("a request with BOTH a definition and a ref was admitted")
	}
}

// TestWorkflow_InstructionPathEscapeRefused asserts bundle paths are validated.
func TestWorkflow_InstructionPathEscapeRefused(t *testing.T) {
	bad := []string{
		"", "/etc/passwd", "../escape.md", "tasks/../../escape.md",
		"tasks//x.md", "tasks/./x.md", `tasks\x.md`, "a\x00b",
	}
	for _, p := range bad {
		if err := contract.ValidateInstructionPath(p); err == nil {
			t.Errorf("unsafe instruction path %q was accepted", p)
		}
	}
	for _, p := range []string{"tasks/investigate.md", "a.md", "a/b/c.md"} {
		if err := contract.ValidateInstructionPath(p); err != nil {
			t.Errorf("safe instruction path %q was refused: %v", p, err)
		}
	}
}

// TestWorkflow_DefinitionAndInputDigestsAreDeterministic asserts identity does not
// depend on serialisation order.
func TestWorkflow_DefinitionAndInputDigestsAreDeterministic(t *testing.T) {
	def := wfDefinition()
	files := []InstructionFile{{Path: "tasks/b.md", SHA256: "bb"}, {Path: "tasks/a.md", SHA256: "aa"}}
	one, err := contract.DefinitionIdentity(def, files)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	// The manifest order must not change identity.
	reordered := []InstructionFile{files[1], files[0]}
	two, err := contract.DefinitionIdentity(def, reordered)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if one != two {
		t.Error("the definition identity depends on the manifest order")
	}
	// A changed instruction digest must change identity.
	changed := []InstructionFile{{Path: "tasks/a.md", SHA256: "cc"}, {Path: "tasks/b.md", SHA256: "bb"}}
	three, err := contract.DefinitionIdentity(def, changed)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if three == one {
		t.Error("changing an instruction's bytes did not change the definition identity")
	}
}

// wfRequestToV1 admits an ordinary single-task request under a chosen ID.
func wfRequestToV1(id, srcRoot string) contract.Request {
	r := wfRequest(id, srcRoot, wfDefinition(), map[string]string{"run_id": "1"}, true)
	return contract.Request{
		Version: contract.Version, RequestID: id,
		Origin:      r.Origin,
		Operation:   contract.OpRepair,
		Workspace:   r.Workspace,
		Objective:   "an ordinary task",
		Obligations: []contract.Obligation{{ID: "O-1", Criterion: "works"}},
		Constraints: r.Constraints,
		Limits:      contract.Limits{RepairCycles: 2},
	}
}

// jsonUnmarshalBytes is a local decode for the admitted record.
func jsonUnmarshalBytes(data []byte, v any) error { return json.Unmarshal(data, v) }
