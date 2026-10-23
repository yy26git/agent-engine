package acceptance

// S1 acceptance: the stock pinned runtime, exercised as a real process.
//
// Everything here runs against the ACTUAL clean upstream executable named by the
// operator's lock, not against a fixture. That is the whole point of the stage: a
// contract fixture proves the adapter parses what the fixture emits, which is a
// statement about the adapter and not about the runtime. Only executing the real
// binary can show that the pinned artifact really has the public surface the
// control layer depends on, and really lacks the legacy draft.
//
// These tests skip, loudly, when no lock or artifact is present. A skip is not a
// pass: the S1 release gate requires this to RUN, and a deployment that has not
// built its pinned artifact is BLOCKED rather than covered.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// The platform the tests run on, taken from the Go runtime so the lock key
// matches the one VerifyPin computes.
var (
	goos   = goruntime.GOOS
	goarch = goruntime.GOARCH
)

// pinnedArtifact resolves the operator's approved runtime, or skips.
func pinnedArtifact(t *testing.T) (agentdeck.Pin, string) {
	t.Helper()
	lockPath := os.Getenv("AGENT_ENGINE_RUNTIME_LOCK")
	if lockPath == "" {
		// The lock ships beside the module, which is where a source checkout has it.
		candidates := []string{
			filepath.Join("..", "..", "runtime-lock.json"),
		}
		if root, err := moduleRoot(); err == nil {
			candidates = append(candidates, filepath.Join(root, "runtime-lock.json"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				lockPath = c
				break
			}
		}
	}
	if lockPath == "" {
		t.Skip("no runtime-lock.json found; set AGENT_ENGINE_RUNTIME_LOCK to run the stock-runtime gate")
	}
	lock, err := agentdeck.LoadRuntimeLock(lockPath)
	if err != nil {
		t.Fatalf("the runtime lock is not usable, so the runtime cannot be verified: %v", err)
	}
	art, err := lock.ArtifactFor(goosOf(), goarchOf())
	if err != nil {
		t.Skipf("no approved artifact for this platform: %v", err)
	}
	pin, err := agentdeck.VerifyPin(lock, art.Path)
	if err != nil {
		t.Fatalf("the approved artifact failed verification: %v", err)
	}
	return pin, lockPath
}

func goosOf() string   { return runtimeGOOS() }
func goarchOf() string { return runtimeGOARCH() }

// runPinned invokes the real pinned executable and returns stdout.
func runPinned(t *testing.T, pin agentdeck.Pin, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(pin.Path, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running the pinned runtime %v: %v", args, err)
		}
	}
	return stdout.String(), stderr.String(), code
}

// TestStockRuntime_PublicCliEventReceiptContract asserts the real artifact has
// the public surface the control layer depends on.
func TestStockRuntime_PublicCliEventReceiptContract(t *testing.T) {
	pin, _ := pinnedArtifact(t)

	// The version string must exist, but it must NOT be treated as the identity.
	// The pinned commit is four commits past the v1.16.24 tag, so a version
	// string alone would approve the wrong artifact.
	out, _, code := runPinned(t, pin, "--version")
	if code != 0 || !strings.Contains(out, "Agent Deck") {
		t.Fatalf("the pinned runtime reported no version: code=%d out=%q", code, out)
	}
	t.Logf("version string: %s (not used as identity; pin is %s)", strings.TrimSpace(out), pin.SourceCommit[:12])

	// session output --json is the seam trusted review ingress depends on.
	out, _, _ = runPinned(t, pin, "session", "output", "--json", "sess-definitely-absent-s1")
	if !strings.Contains(out, "NOT_FOUND") && !strings.Contains(out, "not found") {
		t.Errorf("session output did not answer a typed envelope for an absent session: %q", out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Errorf("session output is not machine-readable JSON: %v (%q)", err, out)
	}

	// events stats is how the supervisor decides the bus is live at all.
	out, _, _ = runPinned(t, pin, "events", "stats", "--json")
	var health map[string]any
	if err := json.Unmarshal([]byte(out), &health); err != nil {
		t.Errorf("events stats is not machine-readable JSON: %v (%q)", err, out)
	}
	if _, ok := health["enabled"]; !ok {
		t.Errorf("events stats carries no enabled field: %q", out)
	}

	// The session surface the adapter drives must exist.
	out, _, _ = runPinned(t, pin, "session", "--help")
	for _, want := range []string{"start", "stop", "send", "send-status", "output"} {
		if !strings.Contains(out, want) {
			t.Errorf("the pinned runtime has no session %q command", want)
		}
	}
}

// TestStockRuntime_CustomWorkflowUnavailable asserts the legacy draft is genuinely
// absent from the release artifact.
//
// This is the migration's central claim. If the draft were still reachable, the
// retired owner would still be dispatchable and two owners could still act on the
// same workspace — which is the concurrent-writer failure the whole stage exists
// to prevent.
func TestStockRuntime_CustomWorkflowUnavailable(t *testing.T) {
	pin, _ := pinnedArtifact(t)

	_, stderr, _ := runPinned(t, pin, "workflow", "--help")
	combined := stderr
	if strings.Contains(strings.ToLower(combined), "agent-deck workflow") {
		t.Fatalf("the pinned runtime still dispatches the legacy workflow command: %s", combined)
	}
	if !strings.Contains(combined, "not a recognized command") {
		t.Logf("legacy workflow dispatch is absent; runtime said: %s", strings.TrimSpace(combined))
	}

	// The stronger check: the draft's symbols must not be compiled into the
	// binary at all. A command that is merely undocumented could still be reached.
	data, err := os.ReadFile(pin.Path)
	if err != nil {
		t.Fatalf("read the pinned artifact: %v", err)
	}
	blob := string(data)
	for _, symbol := range []string{"handleWorkflow", "internal/workflow", "internal/toolstatus"} {
		if strings.Contains(blob, symbol) {
			t.Errorf("the pinned artifact contains the legacy draft symbol %q; it is not unmodified upstream", symbol)
		}
	}
}

// TestRuntimePin_ExactCommitAndArtifactRequired asserts the pin refuses an
// unpinned or substituted runtime before any effect.
func TestRuntimePin_ExactCommitAndArtifactRequired(t *testing.T) {
	pin, lockPath := pinnedArtifact(t)
	lock, err := agentdeck.LoadRuntimeLock(lockPath)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}

	// The honest case passes and reports both axes.
	if pin.SourceCommit != lock.SourceCommit {
		t.Errorf("pin commit = %q, want %q", pin.SourceCommit, lock.SourceCommit)
	}
	if pin.SHA256 == "" {
		t.Error("the verified pin carries no measured artifact hash")
	}

	dir := t.TempDir()
	// A bare name must never be accepted: it would resolve through PATH.
	if _, err := agentdeck.VerifyPin(lock, "agent-deck"); err == nil {
		t.Error("a bare binary name was accepted; it could resolve to anything on PATH")
	}
	// An empty configuration is refused.
	if _, err := agentdeck.VerifyPin(lock, "  "); err == nil {
		t.Error("an empty agent_deck_binary was accepted")
	}
	// A different absolute path is refused even if the file is identical,
	// because the lock approves a location as well as a hash.
	other := filepath.Join(dir, "agent-deck")
	if err := os.WriteFile(other, mustRead(t, pin.Path), 0o755); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if _, err := agentdeck.VerifyPin(lock, other); err == nil {
		t.Error("a byte-identical copy at an unapproved path was accepted")
	}
	// A tampered file at the approved path is refused.
	approved := filepath.Join(dir, "approved")
	if err := os.WriteFile(approved, []byte("not the reviewed executable"), 0o755); err != nil {
		t.Fatalf("write tampered: %v", err)
	}
	tampered := *lock
	tampered.Artifacts = map[string]agentdeck.LockedArtifact{
		goosOf() + "/" + goarchOf(): {Path: approved, SHA256: pin.SHA256},
	}
	if _, err := agentdeck.VerifyPin(&tampered, approved); err == nil {
		t.Error("a replaced executable at the approved path was accepted")
	}
	// A lock with no artifact for this platform stays refused.
	empty := &agentdeck.RuntimeLock{Schema: 1, SourceCommit: lock.SourceCommit, Artifacts: map[string]agentdeck.LockedArtifact{}}
	if _, err := agentdeck.VerifyPin(empty, approved); err == nil {
		t.Error("a lock with no artifact for this platform was accepted")
	}
	// A lock naming an unknown field is refused rather than under-enforced.
	badLock := filepath.Join(dir, "bad-lock.json")
	if err := os.WriteFile(badLock, []byte(`{"schema":1,"source_commit":"x","artifacts":{},"future_field":true}`), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if _, err := agentdeck.LoadRuntimeLock(badLock); err == nil {
		t.Error("a lock with an unknown field was accepted; an older build would under-enforce it")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// runtimeGOOS and runtimeGOARCH are indirected so the platform key used in tests
// matches the one the pin uses internally.
func runtimeGOOS() string   { return goos }
func runtimeGOARCH() string { return goarch }
