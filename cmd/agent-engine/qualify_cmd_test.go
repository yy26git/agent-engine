package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/control"
)

type qualificationRuntime struct {
	agentdeck.Runtime
	specs            []agentdeck.CreateSpec
	stopped          map[string]bool
	broken           bool
	failCreate       bool
	removed          map[string]bool
	cleanupDeadlines []time.Time
}

func (q *qualificationRuntime) Create(_ context.Context, s agentdeck.CreateSpec) (agentdeck.Session, error) {
	q.specs = append(q.specs, s)
	if q.failCreate {
		return agentdeck.Session{}, fmt.Errorf("ambiguous create")
	}
	return agentdeck.Session{ID: fmt.Sprintf("s%d", len(q.specs))}, nil
}
func (q *qualificationRuntime) Start(context.Context, string) error { return nil }
func (q *qualificationRuntime) Stop(ctx context.Context, id string) error {
	deadline, _ := ctx.Deadline()
	q.cleanupDeadlines = append(q.cleanupDeadlines, deadline)
	q.stopped[id] = true
	return nil
}
func (q *qualificationRuntime) RemoveRetired(_ context.Context, id string) error {
	if !q.stopped[id] {
		return fmt.Errorf("session still running")
	}
	if q.removed == nil {
		q.removed = map[string]bool{}
	}
	q.removed[id] = true
	return nil
}
func (q *qualificationRuntime) Inspect(_ context.Context, id string) (agentdeck.Session, error) {
	if q.stopped[id] {
		return agentdeck.Session{ID: id, Status: "stopped"}, nil
	}
	return agentdeck.Session{ID: id, Status: "running"}, nil
}
func (q *qualificationRuntime) Output(_ context.Context, id string, _ int) (agentdeck.Output, error) {
	var n int
	fmt.Sscanf(id, "s%d", &n)
	command := q.specs[n-1].Wrapper
	at := strings.Index(command, "EXQUAL_")
	marker := strings.Split(command[at:], ":")[0]
	code, text := 1, "Permission denied\n"
	if n == 1 {
		code = 0
		text = "qualification sentinel\n"
	}
	if q.broken {
		code = 65
		text = "sandbox-exec: unbound variable: TIOCSTI\n"
	}
	return agentdeck.Output{SessionID: id, Text: text + marker + fmt.Sprintf(":%d\n", code)}, nil
}

func TestQualification_ProductionSeamRunsEveryProbeAndCleansUp(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			workspace := t.TempDir()
			q := &qualificationRuntime{stopped: map[string]bool{}, broken: broken}
			p := control.Profile{Name: "read", Tool: "codex", ReadOnly: true, Model: "selected", LaunchCommand: []string{"/fixture/codex", "--model", "{model}", "--sandbox", "read-only", "--ask-for-approval", "never"}}
			r := control.ConfinementReceipt{}
			saves := 0
			err := runConfinementQualification(context.Background(), q, p, workspace, &r, func() error { saves++; return nil })
			if broken {
				if err == nil || !strings.Contains(err.Error(), "read-control") {
					t.Fatal(err)
				}
				if len(q.specs) != 1 {
					t.Fatal("runtime failure was treated as a denial and more probes ran")
				}
			} else if err != nil || len(q.specs) != 5 {
				t.Fatal(err, len(q.specs))
			}
			if !r.CleanupProven || len(q.stopped) != len(q.specs) || len(q.removed) != len(q.specs) || saves < 3 {
				t.Fatal("qualification did not join cleanup or retain evidence", r)
			}
			for i, deadline := range q.cleanupDeadlines {
				if deadline.IsZero() || (i > 0 && !deadline.After(q.cleanupDeadlines[i-1])) {
					t.Fatal("an owned probe inherited a prior cleanup deadline", q.cleanupDeadlines)
				}
			}
			for _, spec := range q.specs {
				if !strings.Contains(spec.Wrapper, "'--sandbox' 'read-only' '--ask-for-approval' 'never' 'sandbox'") {
					t.Fatal("qualification changed selected policy", spec.Wrapper)
				}
			}
			files, err := os.ReadDir(workspace)
			if err != nil || len(files) != 0 {
				t.Fatal("sentinel directory survived", err, files)
			}
		})
	}
}

func TestQualification_FailureReceiptPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.json")
	if err := retainQualification(path, control.ConfinementReceipt{Status: "FAILED", Failure: "TIOCSTI"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "TIOCSTI") {
		t.Fatal(err, string(b))
	}
}

func TestQualification_RealShellRetainsFastFailureAndQuotesPaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sentinel with ' quote")
	if err := os.WriteFile(target, []byte("qualification sentinel\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []bool{false, true} {
		log := filepath.Join(dir, fmt.Sprintf("output-%t", failure))
		invocation := "/bin/sh -c " + qualifyWord(`cat "$1"`) + " qualification " + qualifyWord(target)
		want := "EXQUAL_REAL:0"
		if failure {
			invocation = "/bin/sh -c " + qualifyWord("printf 'sandbox-exec: unbound variable: TIOCSTI\\n'; exit 65")
			want = "EXQUAL_REAL:65"
		}
		cmd := exec.Command("/bin/sh", "-c", qualificationWrapper(invocation, "EXQUAL_REAL", log))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		var body []byte
		for time.Now().Before(deadline) {
			body, _ = os.ReadFile(log)
			if strings.Contains(string(body), want) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if !strings.Contains(string(body), want) {
			t.Fatalf("lost exit evidence: %s", body)
		}
		if failure && !strings.Contains(string(body), "TIOCSTI") {
			t.Fatal(string(body))
		}
		if !failure && !strings.Contains(string(body), "qualification sentinel") {
			t.Fatal(string(body))
		}
	}
}

func TestQualification_UnknownCreateCannotClaimCleanup(t *testing.T) {
	q := &qualificationRuntime{stopped: map[string]bool{}, failCreate: true}
	p := control.Profile{Name: "read", Tool: "codex", ReadOnly: true, Model: "selected", LaunchCommand: []string{"/fixture/codex", "--model", "{model}", "--sandbox", "read-only", "--ask-for-approval", "never"}}
	r := control.ConfinementReceipt{}
	err := runConfinementQualification(context.Background(), q, p, t.TempDir(), &r, func() error { return nil })
	if err == nil || r.CleanupProven || !strings.Contains(err.Error(), "reconcile label") {
		t.Fatal(err, r)
	}
}
