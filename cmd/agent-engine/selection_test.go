package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func TestProfilesCLI_ShowsSelectionsAndRefusesUnknownRole(t *testing.T) {
	root, _ := newCLIFixture(t)
	file := filepath.Join(root, "CONTROL.toml")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	config := string(original) + `
[profiles.checkpoint]
role = "checkpoint"
tool = "codex"
model = "checkpoint-model"
effort = "high"

[profiles.worker]
role = "worker"
tool = "claude"
model = "worker-model"
`
	if err := os.WriteFile(file, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--store", root, "profiles", "--role", "checkpoint", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	var out struct {
		Profiles []struct {
			Selection  struct{ Tool, Model, Effort string }
			Launchable bool
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Profiles) != 1 || out.Profiles[0].Selection.Tool != "codex" || out.Profiles[0].Selection.Model != "checkpoint-model" || out.Profiles[0].Selection.Effort != "high" || !out.Profiles[0].Launchable {
		t.Fatalf("selection list lost settings: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--store", root, "profiles", "--role", "unapproved"}, &stdout, &stderr); code != 1 {
		t.Fatalf("unknown role accepted: %d", code)
	}
}

func TestTaskExec_TerminalResultUsesCommittedDigestWithoutRuntime(t *testing.T) {
	root, src := newCLIFixture(t)
	if err := os.MkdirAll(filepath.Join(src, "project"), 0755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	requestFile := writeRequest(t, root, repairRequest(src))
	if code := run([]string{"--store", root, "submit", "--request-file", requestFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("submit: %s", stderr.String())
	}
	cfg, err := control.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	store := control.NewStore(cfg)
	id := "forge-run-1842-repair"
	digest, err := store.PublishResult(id, contract.Result{Status: contract.StatusFail, Summary: "failed verification"})
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--store", root, "task-exec", "--request-id", id, "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("terminal exit: %d %s", code, stderr.String())
	}
	var result control.TaskExecOutcome
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ResultSHA256 != digest || len(result.ResultSHA256) != 64 {
		t.Fatalf("digest is prose or missing: %+v", result)
	}
}
