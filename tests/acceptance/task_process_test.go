package acceptance

// S5 task-process acceptance.
//
// The load-bearing property is that a PROCESS EXIT means the same thing every
// time, because a scheduler that reads anything else has no way to tell a passed
// task from a run that merely finished. Each case below drives the built binary
// and asserts the exit code the contract promises.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

// runTaskExec invokes the built task-exec verb and returns code plus parsed JSON.
func runTaskExec(t *testing.T, w *world, args ...string) (int, map[string]any, string) {
	t.Helper()
	full := append([]string{"--json", "task-exec"}, args...)
	r := w.run(full...)
	out := map[string]any{}
	_ = json.Unmarshal([]byte(strings.TrimSpace(r.Stdout)), &out)
	return r.Code, out, r.Stderr
}

// TestAcceptance_TaskExecRefusesAnUnknownRequest asserts the BLOCKED exit.
//
// This is the exit that matters most, because it is the one an unsatisfied
// requirement must produce. A scheduler that saw 0 here would treat an
// unexecutable task as a success.
func TestAcceptance_TaskExecRefusesAnUnknownRequest(t *testing.T) {
	w := newWorld(t, "taskexec-unknown")
	w.withWorkflowProfiles(t)

	code, out, stderr := runTaskExec(t, w, "--request-id", "no-such-request")
	if code != control.TaskBlocked.ExitCode() {
		t.Fatalf("exit = %d, want %d (BLOCKED)\nstderr: %s", code, control.TaskBlocked.ExitCode(), stderr)
	}
	if status, _ := out["status"].(string); status != string(control.TaskBlocked) {
		t.Errorf("status = %q, want BLOCKED", status)
	}
	if reason, _ := out["reason"].(string); reason == "" {
		t.Error("the refusal carries no reason on stdout, so a scheduler cannot diagnose it")
	}
}

// TestAcceptance_TaskExecRefusesAChildOfAnotherRun asserts provenance is checked.
func TestAcceptance_TaskExecRefusesAChildOfAnotherRun(t *testing.T) {
	w := newWorld(t, "taskexec-provenance")
	w.withWorkflowProfiles(t)

	// Two admitted graphs, so the child id is REAL and only the root is wrong.
	var a, b map[string]any
	w.mustRun("--json", "workflow", "admit", "--request-file", wfDoc(t, w, "root-a", wfDefDoc(), true)).JSON(t, &a)
	w.mustRun("--json", "workflow", "admit", "--request-file", wfDoc(t, w, "root-b", wfDefDoc(), true)).JSON(t, &b)
	childrenA, _ := a["children"].(map[string]any)
	childID, _ := childrenA["investigate"].(string)
	if childID == "" {
		t.Fatalf("no child id in the admission output: %v", a)
	}

	// A genuine child of root-a, presented as belonging to root-b.
	code, out, stderr := runTaskExec(t, w,
		"--request-id", childID,
		"--root-request-id", "root-b")
	if code != control.TaskBlocked.ExitCode() {
		t.Fatalf("exit = %d, want BLOCKED %d\nstderr: %s", code, control.TaskBlocked.ExitCode(), stderr)
	}
	reason, _ := out["reason"].(string)
	if !strings.Contains(reason, "not a child of") {
		t.Errorf("the refusal did not name the provenance violation %q: %q", "not a child of", reason)
	}

	// The SAME child presented against its OWN root is not refused for provenance.
	code, out, _ = runTaskExec(t, w, "--request-id", childID, "--root-request-id", "root-a")
	if reason, _ := out["reason"].(string); strings.Contains(reason, "not a child of") {
		t.Errorf("a child was refused against its own root: %q", reason)
	}
	_ = code
}

// TestAcceptance_TaskExecBlocksWhenNothingHasReviewed asserts a nonterminal task
// never exits zero.
//
// This is the "helper coverage overstates production" case in its rawest form: a
// task that did nothing at all has no review, no receipts and no result, and must
// therefore not look like a pass to a scheduler.
func TestAcceptance_TaskExecBlocksWhenNothingHasReviewed(t *testing.T) {
	w := newWorld(t, "taskexec-nothing")
	w.withWorkflowProfiles(t)
	w.mustRun("submit", "--request-file", w.repairRequestFile())

	code, out, _ := runTaskExec(t, w, "--request-id", w.RequestID)
	if code != control.TaskBlocked.ExitCode() {
		t.Fatalf("exit = %d, want %d (BLOCKED): a task with no review and no result must not exit 0",
			code, control.TaskBlocked.ExitCode())
	}
	if status, _ := out["status"].(string); status != string(control.TaskBlocked) {
		t.Errorf("status = %q, want BLOCKED", status)
	}
}

// TestAcceptance_TaskProcessExitContract asserts the four exits are the ones the
// design promises, and that they are distinct.
func TestAcceptance_TaskProcessExitContract(t *testing.T) {
	want := map[control.TaskStatus]int{
		control.TaskPass:      0,
		control.TaskFail:      1,
		control.TaskBlocked:   2,
		control.TaskCancelled: 3,
	}
	seen := map[int]control.TaskStatus{}
	for status, code := range want {
		if status.ExitCode() != code {
			t.Errorf("%s exits %d, want %d", status, status.ExitCode(), code)
		}
		if status.Terminal() != true {
			t.Errorf("%s is not reported as terminal", status)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("%s and %s share exit code %d", prev, status, code)
		}
		seen[code] = status
	}
	// An unrecognised status must not fall through to success.
	if got := control.TaskStatus("WHATEVER").ExitCode(); got == 0 {
		t.Error("an unknown status exited 0")
	}
}

// TestAcceptance_TaskExecWritesNothingBeforeItMayAct asserts a refused execution
// creates no request directory.
//
// The refusal has to be a refusal and not a side effect: a scheduler that retries
// must not find a half-created request on the second attempt.
func TestAcceptance_TaskExecWritesNothingBeforeItMayAct(t *testing.T) {
	w := newWorld(t, "taskexec-nowrite")
	w.withWorkflowProfiles(t)

	before := countEntries(t, filepath.Join(w.Store, "REQUESTS"))
	code, _, _ := runTaskExec(t, w, "--request-id", "definitely-not-admitted")
	if code != control.TaskBlocked.ExitCode() {
		t.Fatalf("exit = %d, want BLOCKED", code)
	}
	after := countEntries(t, filepath.Join(w.Store, "REQUESTS"))
	if after != before {
		t.Errorf("a refused task-exec changed the store: %d request dirs became %d", before, after)
	}
}

func countEntries(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}
