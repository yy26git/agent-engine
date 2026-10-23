package acceptance

// S5 acceptance against the REAL pinned OMA artifact.
//
// This is the mandatory installed-CLI contract test. Everything else in the OMA
// suite proves the adapter behaves; this proves the pinned binary ACCEPTS what the
// adapter generates, runs it, and produces a receipt the adapter can read.
//
// It SKIPS, loudly, when no pinned artifact is present. A skip is not a pass:
// S5's release gate requires this to run.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/oma"
)

// pinnedOMA resolves the operator-approved scheduler, or skips.
func pinnedOMA(t *testing.T) oma.Launcher {
	t.Helper()
	root := os.Getenv("AGENT_ENGINE_OMA_RUNTIME")
	if root == "" {
		base, err := filepath.Abs(filepath.Join("..", "..", "..", ".oma-runtime", "oma", "1.21.1"))
		if err != nil {
			t.Skipf("cannot resolve the pinned OMA runtime: %v", err)
		}
		root = base
	}
	path := filepath.Join(root, "oma")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the pinned OMA artifact is not present at %s; build it to run this gate", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the pinned OMA artifact cannot be read: %v", err)
	}
	sum := sha256.Sum256(data)
	return oma.Launcher{
		Path:           path,
		SHA256:         hex.EncodeToString(sum[:]),
		PackageVersion: oma.SupportedVersion,
	}
}

// probeChild writes a stand-in for `agent-engine task-exec`.
//
// It is not a mock of the control layer. It is the child process the scheduler
// spawns, and the point of the test is that the pinned CLI launches it, observes
// its exit, and reports it correctly.
func probeChild(t *testing.T, dir, name string, exitCode int, stdout string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %q\nexit %d\n", stdout, exitCode)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func writeDoc(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestAcceptance_RealPinnedOMARunsAGeneratedGraph is the load-bearing test: the
// adapter's generated documents, executed by the real pinned binary.
func TestAcceptance_RealPinnedOMARunsAGeneratedGraph(t *testing.T) {
	launcher := pinnedOMA(t)
	if err := oma.VerifyLauncher(launcher); err != nil {
		t.Fatalf("the pinned launcher failed verification: %v", err)
	}

	w := newWorld(t, "oma-real-1")
	w.withWorkflowProfiles(t)
	// The real CLI must never see a provider credential: this stage's whole claim is
	// that a process backend needs no model.
	os.Unsetenv("OPENAI_API_KEY")
	os.Unsetenv("ANTHROPIC_API_KEY")

	// Admit the graph through the built CLI so the bundle is genuinely on disk.
	w.mustRun("workflow", "admit", "--request-file", wfDoc(t, w, "oma-real-1", wfDefDoc(), true))
	var admitOut map[string]any
	w.mustRun("--json", "workflow", "admit", "--request-file", wfDoc(t, w, "oma-real-1", wfDefDoc(), true)).JSON(t, &admitOut)
	defSHA, _ := admitOut["definition_sha256"].(string)
	if defSHA == "" {
		t.Fatalf("admission reported no definition digest: %v", admitOut)
	}
	children, _ := admitOut["children"].(map[string]any)
	if len(children) != 2 {
		t.Fatalf("admission reported %d children, want 2: %v", len(children), admitOut)
	}
	childIDs := map[string]string{}
	for k, v := range children {
		childIDs[k], _ = v.(string)
	}

	// The definition as the store pinned it, decoded from the bundle on disk.
	definition, err := contract.DecodeWorkflowDefinition(mustReadFile(t, filepath.Join(
		w.Store, "WORKFLOWS", defSHA, "DEFINITION.json")))
	if err != nil {
		t.Fatalf("decode the admitted definition: %v", err)
	}

	childDir := filepath.Join(w.Root, "children")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	okChild := probeChild(t, childDir, "child-ok", 0, "OUT-OK")
	failChild := probeChild(t, childDir, "child-fail", 4, "OUT-FAIL")

	tr, err := oma.Translate(oma.TranslateOptions{
		ExBinary: okChild, StoreRoot: w.Store,
		RootRequestID: "oma-real-1", WorkflowSHA: defSHA, Epoch: 1,
		Definition: definition, Children: childIDs, Objectives: map[string]string{},
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// Only the SECOND task's agent fails; every other field stays as generated.
	tr.Team.Agents[1].Backend.Command = failChild

	workDir := filepath.Join(w.Root, "oma-epoch-1")
	tasksPath := filepath.Join(workDir, "inputs", "tasks.json")
	teamPath := filepath.Join(workDir, "inputs", "team.json")
	writeDoc(t, tasksPath, tr.Tasks)
	writeDoc(t, teamPath, tr.Team)

	titles := map[string]bool{}
	for _, task := range definition.Tasks {
		titles[oma.NormaliseTitle(task.ID)] = true
	}

	var intentEpoch int
	var intentArgs []string
	res, err := (oma.Gate{}).Run(context.Background(), oma.LaunchRequest{
		Launcher: launcher, TasksFile: tasksPath, TeamFile: teamPath,
		WorkDir: workDir, Epoch: 1, Timeout: 2 * time.Minute,
		OnLaunchIntent: func(epoch int, argv []string) error {
			intentEpoch, intentArgs = epoch, argv
			return nil
		},
	}, titles)
	if err != nil {
		t.Fatalf("the pinned scheduler could not be run: %v", err)
	}
	if intentEpoch != 1 || len(intentArgs) == 0 {
		t.Errorf("the durable launch intent recorded epoch=%d argv=%v", intentEpoch, intentArgs)
	}
	if !res.Started {
		t.Fatal("the scheduler was not started")
	}
	if res.ReceiptErr != nil {
		t.Fatalf("the real receipt could not be read: %v", res.ReceiptErr)
	}
	if res.Receipt == nil {
		t.Fatal("no receipt was parsed from the real scheduler's stdout")
	}

	by := res.Receipt.ByTitle()
	firstKey := oma.NormaliseTitle(definition.Tasks[0].ID)
	first, ok := by[firstKey]
	if !ok {
		t.Fatalf("the receipt has no entry for %s: %v", firstKey, by)
	}
	if !first.Ran || !first.Success {
		t.Errorf("the first task = %+v, want ran and succeeded", first)
	}
	if !strings.Contains(first.Output, "OUT-OK") {
		t.Errorf("the first task's stdout is missing: %q", first.Output)
	}

	secondKey := oma.NormaliseTitle(definition.Tasks[1].ID)
	second, ok := by[secondKey]
	if !ok {
		t.Fatalf("the receipt has no entry for %s: %v", secondKey, by)
	}
	if second.Success {
		t.Error("the failing child was reported as a success")
	}
	if !strings.Contains(second.Output, "exited with code 4") {
		t.Errorf("the failure did not carry the exit code: %q", second.Output)
	}
	// The pinned CLI discards a failed task's stdout. This is the EMPIRICAL proof
	// that stdout cannot be a result transport, observed rather than assumed.
	if strings.Contains(second.Output, "OUT-FAIL") {
		t.Errorf("the failed task's stdout survived (%q); the pinned CLI is expected to discard it", second.Output)
	}

	if got := res.Receipt.ObservedRetries(); got != 0 {
		t.Errorf("observed retries = %d, want 0", got)
	}
	if res.Receipt.Success {
		t.Error("the graph reported success although a child failed")
	}

	// The generated documents are on disk for an operator to audit.
	for _, p := range []string{tasksPath, teamPath, res.StdoutPath, res.StderrPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected artifact %s is missing: %v", p, err)
		}
	}
}

// TestAcceptance_RealPinnedOMARejectsSubstitutedArtifact asserts the pin refuses
// anything that is not the approved scheduler, BEFORE spawning it.
func TestAcceptance_RealPinnedOMARejectsSubstitutedArtifact(t *testing.T) {
	launcher := pinnedOMA(t)
	work := t.TempDir()

	decoy := filepath.Join(work, "decoy-oma")
	if err := os.WriteFile(decoy, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	forged := launcher
	forged.Path = decoy
	if err := oma.VerifyLauncher(forged); err == nil {
		t.Error("a substituted scheduler path was accepted")
	}

	bad := launcher
	bad.SHA256 = strings.Repeat("0", 64)
	if err := oma.VerifyLauncher(bad); err == nil {
		t.Error("a scheduler with a mismatched digest was accepted")
	}

	relative := launcher
	relative.Path = "oma"
	if err := oma.VerifyLauncher(relative); err == nil {
		t.Error("a non-absolute scheduler path was accepted; it could resolve through PATH")
	}

	wrongVersion := launcher
	wrongVersion.PackageVersion = "0.0.1"
	if err := oma.VerifyLauncher(wrongVersion); err == nil {
		t.Error("an unsupported pinned package version was accepted")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
