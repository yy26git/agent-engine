package oma

// Receipt tests.
//
// The load-bearing case here parses a receipt CAPTURED FROM THE REAL PINNED CLI,
// not one written to suit the parser. That distinction is the whole point: a
// fixture invented alongside a parser proves only that the two agree with each
// other, and a schema written from reading source proves only that the author read
// the source correctly. This fixture was produced by running
// `@open-multi-agent/core` 1.21.1 at commit 75a67581 with three process agents, one
// of which failed and one of which was blocked behind it.

import (
	"os"
	"strings"
	"testing"
)

func realReceipt(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/real-receipt-dep-failure.json")
	if err != nil {
		t.Fatalf("read the captured receipt: %v", err)
	}
	return raw
}

// TestReceipt_RealPinnedCLIOutput asserts the parser understands what the pinned
// binary actually emits.
func TestReceipt_RealPinnedCLIOutput(t *testing.T) {
	titles := map[string]bool{"task-a": true, "task-b": true, "task-c": true}
	r, err := ParseReceipt(realReceipt(t), titles)
	if err != nil {
		t.Fatalf("the real receipt did not parse: %v", err)
	}
	if r.Command != "task" {
		t.Errorf("command = %q, want task", r.Command)
	}
	if r.Success {
		t.Error("the real receipt reports success=true although a task failed")
	}
	by := r.ByTitle()
	if len(by) != 3 {
		t.Fatalf("indexed %d tasks, want 3", len(by))
	}

	// A task that ran and succeeded carries its STDOUT.
	a := by["task-a"]
	if !a.Ran || !a.Success {
		t.Errorf("task-a = %+v, want ran and succeeded", a)
	}
	if !strings.Contains(a.Output, "OUT-A") {
		t.Errorf("task-a output = %q, want its stdout", a.Output)
	}

	// A task that failed carries a STDERR-derived error and NO stdout. This is the
	// pinned CLI's actual behaviour and the reason stdout is not a result transport.
	b := by["task-b"]
	if !b.Ran || b.Success {
		t.Errorf("task-b = %+v, want ran and failed", b)
	}
	if !strings.Contains(b.Output, "exited with code 7") {
		t.Errorf("task-b output = %q, want the exit-code error", b.Output)
	}
	if strings.Contains(b.Output, "OUT-B") {
		t.Errorf("task-b output contains its stdout %q; the pinned CLI discards it on failure", b.Output)
	}

	// A task BLOCKED behind a failed dependency did not run at all: no metrics and
	// no agent result. Treating it as anything else would turn a blocked task into
	// a passed one.
	c := by["task-c"]
	if c.Ran {
		t.Errorf("task-c = %+v, want did-not-run behind a failed dependency", c)
	}
	if c.Success {
		t.Error("a task that never ran reported success")
	}
}

// TestReceipt_RetriesWereHonoured asserts maxRetries:0 was actually obeyed.
func TestReceipt_RetriesWereHonoured(t *testing.T) {
	r, err := ParseReceipt(realReceipt(t), map[string]bool{"task-a": true, "task-b": true, "task-c": true})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := r.ObservedRetries(); got != 0 {
		t.Errorf("observed retries = %d, want 0: an outer retry would re-run supervised work outside its budget", got)
	}
}

// TestReceipt_RefusesUnknownAndMissingTasks asserts the identity check both ways.
func TestReceipt_RefusesUnknownAndMissingTasks(t *testing.T) {
	// A receipt naming a task this layer did not generate.
	extra := `{"command":"task","success":true,"tasks":[
	  {"id":"1","title":"task-a","status":"completed"},
	  {"id":"2","title":"smuggled","status":"completed"}]}`
	if _, err := ParseReceipt([]byte(extra), map[string]bool{"task-a": true}); err == nil {
		t.Error("a receipt naming a task outside the generated graph was accepted")
	}
	// A receipt missing a task we generated.
	missing := `{"command":"task","success":true,"tasks":[
	  {"id":"1","title":"task-a","status":"completed"}]}`
	_, err := ParseReceipt([]byte(missing), map[string]bool{"task-a": true, "task-b": true})
	if err == nil {
		t.Fatal("a receipt missing a generated task was accepted; its outcome is unknown")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("the refusal did not name the unknown outcome: %v", err)
	}
}

// TestReceipt_RefusesMalformedAndEmpty asserts the failure modes that must never
// be read as success.
func TestReceipt_RefusesMalformedAndEmpty(t *testing.T) {
	cases := []struct{ name, body string }{
		{"empty output", "   "},
		{"not JSON", "oma exploded"},
		{"wrong command", `{"command":"run","success":true,"tasks":[]}`},
		{"scheduler error", `{"command":"task","success":false,"error":{"kind":"validation","message":"team.name required"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReceipt([]byte(tc.body), nil); err == nil {
				t.Fatalf("%s was accepted as a receipt", tc.name)
			}
		})
	}
}

// TestReceipt_IsBounded asserts an unbounded external payload is refused.
func TestReceipt_IsBounded(t *testing.T) {
	huge := make([]byte, MaxReceiptBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	if _, err := ParseReceipt(huge, nil); err == nil {
		t.Error("an oversized receipt was accepted")
	}
}
