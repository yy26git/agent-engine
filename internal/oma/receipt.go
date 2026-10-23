package oma

// Receipt parsing.
//
// The outer scheduler's final JSON is parsed here and NOWHERE else. It is bounded,
// it is identity-checked, and it is never allowed to decide anything about
// engineering work — because the pinned CLI's failure path proves why:
//
//	"Process backend \"probe-fail\" exited with code 3: ERR-FAIL"
//
// That string is built from STDERR. A failing task's stdout is not in it. So a
// receipt can tell you that a task failed and roughly why, and cannot tell you
// what the task produced. Anything that needs the task's actual output has to read
// the child's durable result, which is why every aggregation rule in this package
// treats the receipt as a SCHEDULER observation and the child results as the
// evidence.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxReceiptBytes bounds how much of a receipt this package will parse.
//
// The bound exists because the receipt is external output written by a process
// this layer does not control. An unbounded read of external output into the
// control layer is a way to exhaust memory on the component that is supposed to be
// watching for trouble.
const MaxReceiptBytes = 8 << 20 // 8 MiB

// Receipt is the parsed `oma task` output.
//
// Every field is optional except the ones that identify the run, because the CLI
// genuinely omits fields in different paths: a task whose dependency failed
// carries no metrics and no agent result at all.
type Receipt struct {
	Command      string                 `json:"command"`
	Success      bool                   `json:"success"`
	Tasks        []Task                 `json:"tasks"`
	AgentResults map[string]AgentResult `json:"agentResults"`
	Error        *ReceiptError          `json:"error"`
}

// ReceiptError is the CLI's own error envelope.
type ReceiptError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Task is one task as the scheduler reports it.
//
// ID is OMA-GENERATED and is recorded but never used as engineering identity:
// OMA normalises and re-identifies tasks, so a stable task title plus the
// admitted child-ID map is what maps back to this layer.
type Task struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Assignee    string   `json:"assignee"`
	Status      string   `json:"status"`
	DependsOn   []string `json:"dependsOn"`
	Description string   `json:"description"`
	MaxRetries  int      `json:"maxRetries"`
	Metrics     *Metrics `json:"metrics"`
}

// Metrics is per-task timing and retry evidence.
type Metrics struct {
	StartMS    int64 `json:"startMs"`
	EndMS      int64 `json:"endMs"`
	DurationMS int64 `json:"durationMs"`
	Retries    int   `json:"retries"`
}

// AgentResult is one agent's observed execution.
type AgentResult struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
}

// Task statuses the pinned CLI reports.
const (
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
	TaskStatusPending   = "pending"
	TaskStatusSkipped   = "skipped"
	TaskStatusRunning   = "running"
)

// ParseReceipt decodes and identity-checks a receipt.
//
// expectedTitles is the set of titles this layer generated. A receipt naming a
// task outside that set is refused rather than merged, because a task the
// scheduler invented is a task whose instructions nobody approved.
func ParseReceipt(raw []byte, expectedTitles map[string]bool) (*Receipt, error) {
	if len(raw) > MaxReceiptBytes {
		return nil, fmt.Errorf("oma: the receipt is %d bytes, over the %d byte limit", len(raw), MaxReceiptBytes)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("oma: the scheduler produced no receipt; absence is unknown, not success")
	}
	var r Receipt
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return nil, fmt.Errorf("oma: the receipt is not readable JSON: %w", err)
	}
	if r.Command != "task" {
		return nil, fmt.Errorf("oma: the receipt reports command %q, expected \"task\"", r.Command)
	}
	if r.Error != nil && r.Error.Message != "" {
		return &r, fmt.Errorf("oma: the scheduler refused the run (%s): %s", r.Error.Kind, r.Error.Message)
	}
	// Identity check. A receipt covering a task set other than the one generated
	// cannot be aggregated: it would attribute one graph's outcomes to another.
	if len(expectedTitles) > 0 {
		seen := map[string]bool{}
		for _, t := range r.Tasks {
			seen[NormaliseTitle(t.Title)] = true
			if !expectedTitles[NormaliseTitle(t.Title)] {
				return &r, fmt.Errorf("oma: the receipt names task %q, which was not in the generated graph",
					t.Title)
			}
		}
		// Every generated task must appear. A task missing from the receipt has an
		// UNKNOWN outcome, and an unknown outcome is never a pass.
		for want := range expectedTitles {
			if !seen[want] {
				return &r, fmt.Errorf("oma: the receipt has no entry for task %q; its outcome is unknown", want)
			}
		}
	}
	return &r, nil
}

// TitleStatus is a receipt's verdict for one task title.
type TitleStatus struct {
	Title string
	// Status is the CLI's reported status, empty when the task has none.
	Status string
	// Ran is true when the agent actually produced a result.
	//
	// A task whose dependency failed appears in the receipt with no metrics and no
	// agent result: it did not run. Treating that as anything other than "did not
	// run" is how a blocked task becomes a passed one.
	Ran bool
	// Success is the agent's own reported success.
	Success bool
	// Retries is the observed retry count, which must be zero.
	Retries int
	// Output is whatever the agent emitted, which for a FAILURE is stderr-derived
	// and for a SUCCESS is stdout. It is evidence about the process, never about
	// the engineering outcome.
	Output string
}

// ByTitle indexes a receipt by normalised title.
func (r *Receipt) ByTitle() map[string]TitleStatus {
	out := make(map[string]TitleStatus, len(r.Tasks))
	for _, t := range r.Tasks {
		key := NormaliseTitle(t.Title)
		ts := TitleStatus{Title: t.Title, Status: t.Status}
		if t.Metrics != nil {
			ts.Retries = t.Metrics.Retries
			// Metrics are only present for a task that actually ran.
			ts.Ran = true
		}
		if ar, ok := r.AgentResults[NormaliseTitle(t.Title)]; ok {
			ts.Ran = true
			ts.Success = ar.Success
			ts.Output = ar.Output
		}
		out[key] = ts
	}
	return out
}

// ObservedRetries returns the largest retry count any task reported.
//
// It exists so a caller can assert `maxRetries: 0` was HONOURED rather than
// merely requested. A retry would re-run supervised engineering work outside its
// repair budget, so a non-zero value is a contract violation, not a metric.
func (r *Receipt) ObservedRetries() int {
	max := 0
	for _, t := range r.Tasks {
		if t.Metrics != nil && t.Metrics.Retries > max {
			max = t.Metrics.Retries
		}
	}
	return max
}
