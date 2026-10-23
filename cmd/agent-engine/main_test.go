package main

// CLI contract tests.
//
// Two properties matter at this boundary. Every operation must name its store,
// because a request is identified by store plus ID and a working-directory
// lookup would make two stores indistinguishable. And machine output must be
// separable from diagnostics, because a caller parsing stdout must never have to
// strip a human preamble.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCLIFixture writes a store with a valid CONTROL.toml and returns its root
// plus the approved source root.
func newCLIFixture(t *testing.T) (storeRoot, srcRoot string) {
	t.Helper()
	base := t.TempDir()
	storeRoot = filepath.Join(base, "store")
	srcRoot = filepath.Join(base, "src")
	evRoot := filepath.Join(base, "evidence")
	outRoot := filepath.Join(base, "out")
	for _, d := range []string{storeRoot, srcRoot, evRoot, outRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	toml := `agent_deck_binary = "agent-deck"
profile = "default"
tool = "mcode"
output_root = "` + outRoot + `"
source_roots = ["` + srcRoot + `"]
evidence_roots = ["` + evRoot + `"]

[limits]
max_repair_cycles = 3
max_worker_generations = 3
max_audit_attempts = 3
max_guidance_rounds = 2

[checks.build]
argv = ["true"]
timeout_seconds = 60
`
	if err := os.WriteFile(filepath.Join(storeRoot, "CONTROL.toml"), []byte(toml), 0o600); err != nil {
		t.Fatalf("write CONTROL.toml: %v", err)
	}
	return storeRoot, srcRoot
}

func writeRequest(t *testing.T, dir string, req map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "request.json")
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return path
}

func repairRequest(srcRoot string) map[string]any {
	return map[string]any{
		"version":    1,
		"request_id": "forge-run-1842-repair",
		"origin":     map[string]any{"kind": "forge", "caller_id": "local-forge"},
		"operation":  "REPAIR",
		"workspace":  map[string]any{"path": filepath.Join(srcRoot, "project")},
		"objective":  "Fix the attributable regression",
		"obligations": []map[string]any{
			{"id": "O-1", "criterion": "Regression fixed and verified", "required_checks": []string{"build"}},
		},
		"constraints":  map[string]any{"allow_code_changes": true, "allowed_paths": []string{"src/**"}},
		"verification": []map[string]any{{"check_id": "build"}},
		"limits":       map[string]any{"repair_cycles": 3},
	}
}

// TestCLI_AllOperationsRequireSameStore asserts no operation runs without an
// explicit absolute store, and that a request found through one store is
// invisible through another.
func TestCLI_AllOperationsRequireSameStore(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	reqPath := writeRequest(t, t.TempDir(), repairRequest(srcRoot))

	commands := [][]string{
		{"submit", "--request-file", reqPath},
		{"status", "--request-id", "forge-run-1842-repair"},
		{"result", "--request-id", "forge-run-1842-repair"},
		{"cancel", "--request-id", "forge-run-1842-repair"},
	}
	for _, cmd := range commands {
		t.Run(cmd[0]+"/no-store", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(cmd, &stdout, &stderr); code == 0 {
				t.Fatalf("%s succeeded without --store", cmd[0])
			}
			if !strings.Contains(stderr.String(), "--store is required") {
				t.Errorf("stderr = %q, want an explicit --store requirement", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("a refusal wrote to stdout: %q", stdout.String())
			}
		})

		t.Run(cmd[0]+"/relative-store", func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(append([]string{"--store", "relative/store", "--json"}, cmd...), &stdout, &stderr); code == 0 {
				t.Fatalf("%s accepted a relative --store", cmd[0])
			}
			if !strings.Contains(stderr.String(), "absolute") {
				t.Errorf("stderr = %q, want the store to be refused as relative", stderr.String())
			}
		})
	}

	// A store without operator policy is refused: a request may not supply its
	// own commands, roots or limits.
	t.Run("missing policy", func(t *testing.T) {
		bare := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := run([]string{"--store", bare, "--json", "status", "--request-id", "x"}, &stdout, &stderr)
		if code == 0 {
			t.Fatal("a store with no CONTROL.toml must be refused")
		}
		if !strings.Contains(stderr.String(), "CONTROL.toml") {
			t.Errorf("stderr = %q, want it to name the missing policy file", stderr.String())
		}
		// The refusal is still machine-readable on stdout.
		var payload map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("JSON refusal is not parseable: %v (stdout=%q)", err, stdout.String())
		}
		if payload["ok"] != false {
			t.Errorf("JSON refusal = %v, want ok:false", payload)
		}
	})

	// Admit into one store; the other store must not see the request.
	t.Run("stores are independent", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--store", storeRoot, "--json", "submit", "--request-file", reqPath}, &stdout, &stderr); code != 0 {
			t.Fatalf("submit failed: %s", stderr.String())
		}
		other, _ := newCLIFixture(t)
		var out2, err2 bytes.Buffer
		if code := run([]string{"--store", other, "--json", "status", "--request-id", "forge-run-1842-repair"}, &out2, &err2); code == 0 {
			t.Fatal("a request from one store must not be visible in another")
		}
	})
}

// TestCLI_SubmitStatusResultFlow exercises the real command path end to end.
func TestCLI_SubmitStatusResultFlow(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	reqPath := writeRequest(t, t.TempDir(), repairRequest(srcRoot))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--store", storeRoot, "--json", "submit", "--request-file", reqPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("submit: %s", stderr.String())
	}
	var adm map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &adm); err != nil {
		t.Fatalf("submit output is not JSON: %v (%q)", err, stdout.String())
	}
	if adm["accepted"] != true {
		t.Errorf("first submit accepted = %v, want true", adm["accepted"])
	}
	if adm["stage"] != "READY" {
		t.Errorf("stage = %v, want READY", adm["stage"])
	}
	if adm["store"] != storeRoot {
		t.Errorf("store = %v, want %s", adm["store"], storeRoot)
	}

	// A duplicate is reported as such, not as new work.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "submit", "--request-file", reqPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("duplicate submit: %s", stderr.String())
	}
	var dup map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &dup); err != nil {
		t.Fatalf("duplicate output is not JSON: %v", err)
	}
	if dup["duplicate"] != true || dup["accepted"] != false {
		t.Errorf("duplicate submit = %v, want duplicate:true accepted:false", dup)
	}

	// Status is readable and machine-parseable.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "status", "--request-id", "forge-run-1842-repair"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status: %s", stderr.String())
	}
	var st map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		t.Fatalf("status output is not JSON: %v", err)
	}
	if st["operation"] != "REPAIR" {
		t.Errorf("operation = %v, want REPAIR", st["operation"])
	}
	if st["terminal"] != false {
		t.Errorf("terminal = %v, want false before a result is published", st["terminal"])
	}

	// A nonterminal request reports not-ready rather than a fabricated result.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "result", "--request-id", "forge-run-1842-repair"}, &stdout, &stderr); code != 0 {
		t.Fatalf("result: %s", stderr.String())
	}
	var res map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("result output is not JSON: %v", err)
	}
	if res["ready"] != false {
		t.Errorf("ready = %v, want false for an unrun request", res["ready"])
	}
	if res["status"] != nil && res["status"] != "" {
		t.Errorf("status = %v, want no terminal status before one is published", res["status"])
	}

	// Cancellation records intent and leaves the request nonterminal, because
	// effects still have to be reconciled.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "cancel", "--request-id", "forge-run-1842-repair", "--reason", "operator changed their mind"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cancel: %s", stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "status", "--request-id", "forge-run-1842-repair"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status after cancel: %s", stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		t.Fatalf("status after cancel is not JSON: %v", err)
	}
	if st["terminal"] != false {
		t.Error("cancelling must not by itself make a request terminal; effects are still unreconciled")
	}
}

// TestCLI_UnknownCommandIsRefused asserts typos fail loudly.
func TestCLI_UnknownCommandIsRefused(t *testing.T) {
	storeRoot, _ := newCLIFixture(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--store", storeRoot, "admit"}, &stdout, &stderr); code == 0 {
		t.Fatal("an unknown command must be refused")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("stderr = %q, want an explicit unknown-command refusal", stderr.String())
	}
}

// TestCLI_CheckpointAndEvidenceFlow exercises the two typed submissions a worker
// can make without being asked.
//
// The property that matters most is the last one: accepting a checkpoint must not
// be reported as progress. A worker that believes its submission advanced the
// request will stop working, believing it is done.
func TestCLI_CheckpointAndEvidenceFlow(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	reqPath := writeRequest(t, t.TempDir(), repairRequest(srcRoot))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--store", storeRoot, "--json", "submit", "--request-file", reqPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("submit: %s", stderr.String())
	}

	// Evidence must be digest-pinned.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "evidence", "--request-id", "forge-run-1842-repair",
		"--id", "E-1", "--path", "EVIDENCE/E-1/diff.patch"}, &stdout, &stderr); code == 0 {
		t.Error("evidence without a digest must be refused")
	}

	digest := strings.Repeat("ab", 32)
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "evidence", "--request-id", "forge-run-1842-repair",
		"--id", "E-1", "--path", "EVIDENCE/E-1/diff.patch", "--sha256", digest}, &stdout, &stderr); code != 0 {
		t.Fatalf("evidence: %s", stderr.String())
	}

	// A typed checkpoint.
	payload := filepath.Join(t.TempDir(), "cp.json")
	cp := map[string]any{
		"checkpoint_id": "CP-1",
		"claim":         "the regression now reproduces under `go test ./...`",
		"next_action":   "fix the guard in the parser",
		"evidence_refs": []string{"E-1"},
	}
	data, _ := json.Marshal(cp)
	if err := os.WriteFile(payload, data, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "checkpoint", "--request-id", "forge-run-1842-repair",
		"--payload-file", payload}, &stdout, &stderr); code != 0 {
		t.Fatalf("checkpoint: %s", stderr.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rec); err != nil {
		t.Fatalf("checkpoint output is not JSON: %v (%q)", err, stdout.String())
	}
	if rec["checkpoint_id"] != "CP-1" {
		t.Errorf("checkpoint_id = %v", rec["checkpoint_id"])
	}
	if rec["advanced"] != false {
		t.Error("accepting a checkpoint must report that it advanced nothing")
	}

	// An identical retry is a duplicate, not a failure.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "checkpoint", "--request-id", "forge-run-1842-repair",
		"--payload-file", payload}, &stdout, &stderr); code != 0 {
		t.Fatalf("duplicate checkpoint: %s", stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &rec); err != nil {
		t.Fatalf("duplicate output is not JSON: %v", err)
	}
	if rec["duplicate"] != true {
		t.Error("an identical retry must be reported as a duplicate")
	}

	// The same ID with different content is refused.
	cp["claim"] = "something else entirely"
	other, _ := json.Marshal(cp)
	otherPath := filepath.Join(t.TempDir(), "cp2.json")
	if err := os.WriteFile(otherPath, other, 0o600); err != nil {
		t.Fatalf("write conflicting payload: %v", err)
	}
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "checkpoint", "--request-id", "forge-run-1842-repair",
		"--payload-file", otherPath}, &stdout, &stderr); code == 0 {
		t.Error("reusing a checkpoint ID with different content must be refused")
	}

	// A payload with an unknown field is refused rather than partially honoured.
	typo := `{"checkpoint_id":"CP-9","claim":"x","next_action":"y","evidnce_refs":["E-1"]}`
	typoPath := filepath.Join(t.TempDir(), "cp3.json")
	if err := os.WriteFile(typoPath, []byte(typo), 0o600); err != nil {
		t.Fatalf("write typo payload: %v", err)
	}
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "checkpoint", "--request-id", "forge-run-1842-repair",
		"--payload-file", typoPath}, &stdout, &stderr); code == 0 {
		t.Error("a misspelled field must be refused, not silently ignored")
	}

	// The request is still not advanced and still nonterminal.
	stdout.Reset()
	if code := run([]string{"--store", storeRoot, "--json", "status", "--request-id", "forge-run-1842-repair"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status: %s", stderr.String())
	}
	var st map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		t.Fatalf("status is not JSON: %v", err)
	}
	if st["terminal"] != false {
		t.Error("a submitted checkpoint must not make a request terminal")
	}
	obligations, _ := st["obligations"].([]any)
	if len(obligations) == 0 {
		t.Fatal("status reported no obligations")
	}
	first, _ := obligations[0].(map[string]any)
	if first["disposition"] != "OPEN" {
		t.Errorf("obligation disposition = %v, want OPEN: a submission is not a review", first["disposition"])
	}
}
