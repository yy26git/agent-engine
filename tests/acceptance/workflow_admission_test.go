package acceptance

// S3 acceptance: immutable workflow admission, through the built CLI.
//
// The unit tests prove the admission owner behaves. This file proves the deployed
// binary honours it: a first-use definition is pinned, a reference reuses it, a
// conflicting ID launches nothing, and the bundle on disk is immutable.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wfDoc builds a v2 workflow request document for the built CLI.
func wfDoc(t *testing.T, w *world, id string, def map[string]any, allowChanges bool) string {
	t.Helper()
	doc := map[string]any{
		"version":    2,
		"request_id": id,
		"origin":     map[string]any{"kind": "forge", "caller_id": "local-forge"},
		"workspace":  map[string]any{"path": w.Source},
		"input":      map[string]string{"run_id": "1842"},
		"constraints": map[string]any{
			"allow_code_changes": allowChanges,
			"allowed_paths":      []string{"**"},
			"protected_paths":    []string{"evaluation/**"},
		},
	}
	if def != nil {
		doc["workflow_definition"] = def
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal workflow request: %v", err)
	}
	path := filepath.Join(w.Root, id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write workflow request: %v", err)
	}
	return path
}

// wfDefDoc is a valid two-task definition referencing the world's checks.
func wfDefDoc() map[string]any {
	return map[string]any{
		"version": 1,
		"name":    "target-crossing",
		"tasks": []map[string]any{
			{
				"id": "investigate", "operation": "INVESTIGATE",
				"objective":        "Find why the lock is crossed late",
				"instruction":      "Investigate the crossing.",
				"executor_profile": "worker", "reviewer_profile": "reviewer",
				"check_profiles": []string{"build"},
				"input_keys":     []string{"run_id"},
			},
			{
				"id": "repair", "operation": "REPAIR",
				"objective": "Fix the crossing", "instruction": "Repair the crossing.",
				"executor_profile": "worker", "reviewer_profile": "reviewer",
				"depends_on": []string{"investigate"},
			},
		},
	}
}

// withWorkflowProfiles installs profiles the definition can name.
func (w *world) withWorkflowProfiles(t *testing.T) {
	w.t.Helper()
	tomlPath := filepath.Join(w.Store, ConfigFileForTest)
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	registry := `
[profiles.worker]
role = "worker"
tool = "mcode"

[profiles.reviewer]
role = "reviewer"
tool = "mcode"
`
	if err := os.WriteFile(tomlPath, []byte(string(data)+registry), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestAcceptance_WorkflowFirstUseAndRefReuse is the end-to-end shape S3 promises.
func TestAcceptance_WorkflowFirstUseAndRefReuse(t *testing.T) {
	w := newWorld(t, "wf-root-1")
	w.withWorkflowProfiles(t)

	// First use: a definition is submitted and pinned.
	def := wfDefDoc()
	first := w.mustRun("--json", "workflow", "admit", "--request-file", wfDoc(t, w, "wf-root-1", def, true))
	var out map[string]any
	first.JSON(t, &out)
	defSHA, _ := out["definition_sha256"].(string)
	if defSHA == "" {
		t.Fatalf("no definition digest returned: %v", out)
	}
	if launched, _ := out["launched"].(bool); launched {
		t.Error("admission launched something; admission must start no process")
	}
	children, _ := out["children"].(map[string]any)
	if len(children) != 2 {
		t.Fatalf("children = %v, want two", out["children"])
	}

	// The bundle is on disk under the measured identity.
	bundle := filepath.Join(w.Store, "WORKFLOWS", defSHA)
	if _, err := os.Stat(filepath.Join(bundle, "DEFINITION.json")); err != nil {
		t.Errorf("the bundle was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "FILES", "tasks", "investigate.md")); err != nil {
		t.Errorf("the instruction file was not published: %v", err)
	}
	// The immutable admitted record exists.
	if _, err := os.Stat(filepath.Join(w.Store, "REQUESTS", "wf-root-1", "WORKFLOW.json")); err != nil {
		t.Errorf("the admitted workflow record was not written: %v", err)
	}

	// Reuse: a second run references the same definition with a different input.
	refDoc := map[string]any{
		"version":      2,
		"request_id":   "wf-root-2",
		"origin":       map[string]any{"kind": "forge", "caller_id": "local-forge"},
		"workspace":    map[string]any{"path": w.Source},
		"workflow_ref": map[string]any{"version": 1, "sha256": defSHA},
		"input":        map[string]string{"run_id": "1843"},
		"constraints": map[string]any{
			"allow_code_changes": true, "allowed_paths": []string{"**"},
		},
	}
	data, _ := json.Marshal(refDoc)
	refPath := filepath.Join(w.Root, "wf-root-2.json")
	if err := os.WriteFile(refPath, data, 0o600); err != nil {
		t.Fatalf("write ref request: %v", err)
	}
	second := w.mustRun("--json", "workflow", "admit", "--request-file", refPath)
	var out2 map[string]any
	second.JSON(t, &out2)
	if got, _ := out2["definition_sha256"].(string); got != defSHA {
		t.Errorf("reuse produced a different definition identity: %q then %q", defSHA, got)
	}
	if out2["request_sha256"] == out["request_sha256"] {
		t.Error("two runs with different inputs share a request identity")
	}
	// Only ONE bundle exists: reuse rehashes nothing.
	entries, err := os.ReadDir(filepath.Join(w.Store, "WORKFLOWS"))
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("reuse published %d bundles, want 1", len(entries))
	}
}

// TestAcceptance_WorkflowConflictLaunchesNothing asserts a conflicting ID is
// refused and the first admission is untouched.
func TestAcceptance_WorkflowConflictLaunchesNothing(t *testing.T) {
	w := newWorld(t, "wf-conflict")
	w.withWorkflowProfiles(t)
	w.mustRun("workflow", "admit", "--request-file", wfDoc(t, w, "wf-conflict", wfDefDoc(), true))

	// The same id, different content.
	other := wfDefDoc()
	other["name"] = "a-different-graph"
	r := w.run("workflow", "admit", "--request-file", wfDoc(t, w, "wf-conflict", other, true))
	if r.Code == 0 {
		t.Fatal("a conflicting workflow reused an admitted request id")
	}
	if !strings.Contains(r.Stderr, "never changes") {
		t.Errorf("the refusal did not explain identity immutability: %s", r.Stderr)
	}
	// And only one bundle was ever written.
	entries, _ := os.ReadDir(filepath.Join(w.Store, "WORKFLOWS"))
	if len(entries) != 1 {
		t.Errorf("a refused admission published %d bundles, want 1", len(entries))
	}
}

// TestAcceptance_WorkflowMalformedRefused asserts malformed graphs are refused by
// the deployed binary, each with a specific reason.
func TestAcceptance_WorkflowMalformedRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"cycle", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["depends_on"] = []string{"repair"}
		}, "cycle"},
		{"missing dependency", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["depends_on"] = []string{"nope"}
		}, "does not contain"},
		{"unknown operation", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["operation"] = "DEPLOY"
		}, "not one of the supported operations"},
		{"same profile both roles", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["reviewer_profile"] = "worker"
			d["tasks"].([]map[string]any)[1]["reviewer_profile"] = "worker"
		}, "reviewer"},
		{"unknown check", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["check_profiles"] = []string{"no-such-check"}
		}, "registry does not contain"},
		{"missing input key", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["input_keys"] = []string{"run_id", "absent"}
		}, "missing required keys"},
		{"bad id", func(d map[string]any) {
			d["tasks"].([]map[string]any)[0]["id"] = "Bad_ID"
		}, "ids must match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, "wf-bad")
			w.withWorkflowProfiles(t)
			def := wfDefDoc()
			tc.mutate(def)
			r := w.run("workflow", "admit", "--request-file", wfDoc(t, w, "wf-bad", def, true))
			if r.Code == 0 {
				t.Fatalf("a definition with %s was admitted", tc.name)
			}
			if !strings.Contains(r.Stderr, tc.want) {
				t.Errorf("refusal did not mention %q: %s", tc.want, r.Stderr)
			}
			// Nothing was created.
			if _, err := os.Stat(filepath.Join(w.Store, "REQUESTS", "wf-bad")); err == nil {
				t.Error("a refused admission created a request directory")
			}
		})
	}
}

// TestAcceptance_WorkflowNoPermissionUpgrade asserts a mutating graph is refused
// under a read-only root.
func TestAcceptance_WorkflowNoPermissionUpgrade(t *testing.T) {
	w := newWorld(t, "wf-escalation")
	w.withWorkflowProfiles(t)
	r := w.run("workflow", "admit", "--request-file", wfDoc(t, w, "wf-escalation", wfDefDoc(), false))
	if r.Code == 0 {
		t.Fatal("a graph with a REPAIR task was admitted under a root that forbids code changes")
	}
	if !strings.Contains(r.Stderr, "narrower than its root") {
		t.Errorf("the refusal did not name the escalation: %s", r.Stderr)
	}
}

// TestAcceptance_WorkflowForgedChildCapabilityRefused asserts a caller cannot
// admit a child by inventing a capability.
//
// The declared `origin` is deliberately NOT the gate: it is a caller-supplied
// field, so refusing on it was theatre. The gate is a token only the engine can
// mint, and this asserts a hand-written one buys nothing.
func TestAcceptance_WorkflowForgedChildCapabilityRefused(t *testing.T) {
	w := newWorld(t, "wf-forged-cap")
	w.withWorkflowProfiles(t)
	// Admit a real root to present the capability against.
	w.mustRun("workflow", "admit", "--request-file", wfDoc(t, w, "wf-root-x", wfDefDoc(), true))

	doc := map[string]any{
		"version":             2,
		"request_id":          "wf-child-x",
		"origin":              map[string]any{"kind": "forge", "caller_id": "local-forge"},
		"workspace":           map[string]any{"path": w.Source},
		"workflow_definition": wfDefDoc(),
		"input":               map[string]string{"run_id": "1"},
		"child_capability":    strings.Repeat("a", 64),
		"root_request_id":     "wf-root-x",
		"constraints":         map[string]any{"allow_code_changes": true, "allowed_paths": []string{"**"}},
	}
	data, _ := json.Marshal(doc)
	path := filepath.Join(w.Root, "child.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := w.run("workflow", "admit", "--request-file", path)
	if r.Code == 0 {
		t.Fatal("a forged child capability admitted a nested workflow")
	}
	if !strings.Contains(strings.ToLower(r.Stderr), "capability") {
		t.Errorf("the refusal did not name the capability: %s", r.Stderr)
	}
	// And nothing was created for the refused child.
	if _, err := os.Stat(filepath.Join(w.Store, "REQUESTS", "wf-child-x")); err == nil {
		t.Error("a refused child capability still created a request")
	}
}
