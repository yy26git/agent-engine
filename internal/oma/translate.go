// Package oma is the isolated adapter to an external OMA CLI.
//
// OMA is an external dependency reached only through its public command line, and
// this package exists because that command line is a versioned protocol with its
// own serialisation rules and its own failure semantics. Coupling those rules
// directly into engineering state would mean an upstream schema change could
// silently alter what a request is allowed to do — so the protocol stops here.
//
// Three rules govern everything in this package:
//
//   - OMA schedules the OUTER graph and nothing else. It cannot approve
//     engineering work, it does not decide what a task's obligations are, and it
//     never sees a check receipt. Approval authority stays in control.
//   - Only FIELDS THE PINNED CLI ACTUALLY READS are emitted. The pinned CLI
//     forwards a specific set and ignores the rest, so anything not in that set
//     would be a field that LOOKS like enforcement and is not. Emitting such a
//     field is worse than not emitting it, because a reader would trust it.
//   - Nothing a caller supplies reaches an argv. Tasks carry fixed arguments
//     pointing at an absolute EX binary, so a workflow cannot smuggle in a command.
package oma

import (
	"fmt"
	"sort"
	"strings"

	"github.com/emjin/agent-engine/contract"
)

// SupportedVersion is the @open-multi-agent/core release whose CLI protocol this
// adapter was written against.
//
// It is a claim about the PROTOCOL, verified against a real binary by contract
// tests, not a claim that a particular string is printed. The pinned CLI has no
// `--version` command at all, so version identity is established by pinning the
// package manifest and the launcher artifact separately.
const SupportedVersion = "1.21.1"

// TaskSpec is one admitted task as OMA will receive it.
//
// The field set is exactly what the pinned `asTaskSpecs` mapper reads:
// assignee, dependsOn, maxRetries, plus the descriptive fields it accepts. There
// is deliberately no `requires` and no `verify`: the pinned CLI ignores both, so
// emitting them would create the appearance of a dependency gate that does not
// exist.
type TaskSpec struct {
	// Title is the stable task ID. OMA derives its dependency graph from titles,
	// so the title IS the engineering identity on its side.
	Title string `json:"title"`
	// Description carries the objective. It is descriptive only; the authoritative
	// instruction lives in the pinned store, not here.
	Description string `json:"description"`
	// Assignee is the team agent that will run this task.
	Assignee string `json:"assignee"`
	// DependsOn maps to titles, never to OMA's generated task IDs.
	DependsOn []string `json:"dependsOn,omitempty"`
	// MaxRetries is always 0.
	//
	// An outer retry would re-run engineering work without its supervisor. Retry
	// policy for a task belongs to the control layer's repair budget, which is
	// lifetime-scoped and evidence-backed; an outer retry is neither.
	MaxRetries int `json:"maxRetries"`
	// Input is always "none": the authoritative instruction and the upstream result
	// references come from pinned store records, never from prompt text.
	Input string `json:"input"`
}

// TeamAgentSpec is one generated process agent.
//
// Every agent is a fixed process invocation of an absolute EX binary. OMA's job is
// to decide what order to run these in, and the only thing each one can do is hand
// work to Agent Engine.
type TeamAgentSpec struct {
	Name string `json:"name"`
	// Model is REQUIRED by the pinned CLI's team validator even for a process
	// backend, and it is NOT used to execute one. It is a declared placeholder,
	// never consulted for a real call: a process backend that fails without
	// credentials proves no model is invoked.
	//
	// Omitting it fails validation with "agent.model required" and the agent never
	// runs, so it is emitted rather than left out — but it is not a licence to
	// configure a provider.
	Model   string             `json:"model"`
	Backend ProcessBackendSpec `json:"backend"`
}

// ProcessBackendSpec is the pinned CLI's process-backend discriminator.
//
// The discriminant is `backend.kind == "process"`. Declaring a bare top-level
// "process" key instead leaves `agent.backend` undefined, and OMA then DEFAULTS
// TO THE LLM BACKEND — which builds an LLM client and attempts a real model call
// before any process is spawned. That failure mode looks like "OMA needs
// credentials for process agents" and is purely a configuration error.
type ProcessBackendSpec struct {
	Kind string `json:"kind"`
	// Command is the absolute EX binary. Nothing a caller supplies reaches it.
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Input is "none": the instruction comes from the pinned store, never from
	// prompt text passed through the scheduler.
	Input string `json:"input"`
}

// ProcessBackendKind is the discriminator the pinned CLI dispatches on.
const ProcessBackendKind = "process"

// TasksFile is the generated per-epoch input.
type TasksFile struct {
	Tasks []TaskSpec `json:"tasks"`
}

// TeamFile is the generated team configuration.
type TeamFile struct {
	// Name is REQUIRED by the pinned CLI's team validator and must be non-empty.
	// Omitting it fails validation with "team.name: non-empty string required".
	Name         string           `json:"name"`
	Orchestrator OrchestratorSpec `json:"orchestrator"`
	Agents       []TeamAgentSpec  `json:"agents"`
}

// OrchestratorSpec is the outer scheduler's settings.
type OrchestratorSpec struct {
	// MaxConcurrency is always 1.
	//
	// Every task in a run shares one workspace and one single-writer lease, so
	// two tasks running at once would be two writers on one workspace regardless
	// of what their dependency edges say.
	MaxConcurrency int `json:"maxConcurrency"`
}

// Translation is the complete generated input for one launch epoch.
type Translation struct {
	Tasks TaskSpecList `json:"tasks"`
	Team  TeamFile     `json:"team"`
}

// TaskSpecList is the top-level `tasks` array wrapper the CLI reads.
type TaskSpecList struct {
	Tasks []TaskSpec `json:"tasks"`
}

// TranslateOptions is everything translation needs, and nothing a caller may
// influence beyond choosing among already-admitted tasks.
type TranslateOptions struct {
	// ExBinary is the ABSOLUTE path of the built agent-engine executable. It is
	// operator-resolved and never comes from a request.
	ExBinary string
	// StoreRoot is the absolute store path.
	StoreRoot string
	// RootRequestID, WorkflowSHA and Epoch identify the admitted run.
	RootRequestID string
	WorkflowSHA   string
	Epoch         int
	// Definition is the admitted graph, read from the pinned bundle.
	Definition *contract.WorkflowDefinition
	// Children is the immutable admitted child map: task ID to derived request ID.
	Children map[string]string
	// Objectives carries each task's objective, taken from the admitted definition.
	Objectives map[string]string
}

// Translate converts an admitted graph into OMA CLI input.
//
// Every refusal happens here, BEFORE anything is spawned, because a graph that
// OMA cannot schedule must never reach OMA.
func Translate(opts TranslateOptions) (*Translation, error) {
	if opts.Definition == nil {
		return nil, fmt.Errorf("oma: no admitted definition supplied")
	}
	if strings.TrimSpace(opts.ExBinary) == "" || !strings.HasPrefix(opts.ExBinary, "/") {
		return nil, fmt.Errorf("oma: the EX binary must be an absolute path; got %q", opts.ExBinary)
	}
	if strings.TrimSpace(opts.StoreRoot) == "" || !strings.HasPrefix(opts.StoreRoot, "/") {
		return nil, fmt.Errorf("oma: the store root must be an absolute path; got %q", opts.StoreRoot)
	}
	if opts.Epoch <= 0 {
		return nil, fmt.Errorf("oma: a launch epoch must be positive; got %d", opts.Epoch)
	}

	titles, err := uniqueTitles(opts.Definition)
	if err != nil {
		return nil, err
	}

	tr := &Translation{}
	for _, t := range opts.Definition.Tasks {
		title := titles[t.ID]
		depends := make([]string, 0, len(t.DependsOn))
		for _, dep := range t.DependsOn {
			dt, ok := titles[dep]
			if !ok {
				// A dependency on a task outside this graph would be satisfied by
				// nothing, and OMA would wait for a task that does not exist.
				return nil, fmt.Errorf("oma: task %q depends on %q, which is not in this graph", t.ID, dep)
			}
			depends = append(depends, dt)
		}
		desc := t.Objective
		if o, ok := opts.Objectives[t.ID]; ok && strings.TrimSpace(o) != "" {
			desc = o
		}
		child, ok := opts.Children[t.ID]
		if !ok {
			return nil, fmt.Errorf("oma: task %q has no admitted child request; only admitted tasks may be scheduled", t.ID)
		}
		tr.Tasks.Tasks = append(tr.Tasks.Tasks, TaskSpec{
			Title:       title,
			Description: desc,
			// The assignee is the task's own stable name, so a receipt can be
			// mapped back to an admitted child without a lookup table.
			Assignee:  title,
			DependsOn: depends,
			// Outer retries are disabled unconditionally, not configured. A retry
			// here would re-run supervised work outside its budget.
			MaxRetries: 0,
			Input:      "none",
		})

		tr.Team.Agents = append(tr.Team.Agents, TeamAgentSpec{
			Name:  title,
			Model: "process-backend-placeholder",
			Backend: ProcessBackendSpec{
				Kind:    ProcessBackendKind,
				Command: opts.ExBinary,
				Args: []string{
					"--store", opts.StoreRoot,
					"task-exec",
					"--request-id", child,
					"--root-request-id", opts.RootRequestID,
					"--workflow-sha256", opts.WorkflowSHA,
					"--launch-epoch", itoa(opts.Epoch),
				},
				Input: "none",
			},
		})
	}
	tr.Team.Name = "agentengine-executor-team"
	tr.Team.Orchestrator = OrchestratorSpec{MaxConcurrency: 1}
	return tr, nil
}

// uniqueTitles assigns each task its title, refusing collisions.
//
// OMA trims and lowercases titles before comparing them. Two task IDs that differ
// only by case or surrounding whitespace would therefore become ONE task at
// runtime while this layer counts them as two — so the collision is refused here,
// where it can still be reported.
func uniqueTitles(def *contract.WorkflowDefinition) (map[string]string, error) {
	out := make(map[string]string, len(def.Tasks))
	seen := make(map[string]string, len(def.Tasks))
	for _, t := range def.Tasks {
		normalised := NormaliseTitle(t.ID)
		if prev, dup := seen[normalised]; dup {
			return nil, fmt.Errorf("oma: tasks %q and %q normalise to the same title %q; "+
				"an outer scheduler would merge them into one task", prev, t.ID, normalised)
		}
		seen[normalised] = t.ID
		out[t.ID] = t.ID
	}
	return out, nil
}

// NormaliseTitle mirrors the trim/lowercase normalisation an outer scheduler
// applies to titles.
func NormaliseTitle(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func itoa(v int) string { return fmt.Sprintf("%d", v) }

// SortedTitles returns the graph's titles in deterministic order, for receipts.
func SortedTitles(def *contract.WorkflowDefinition) []string {
	out := make([]string, 0, len(def.Tasks))
	for _, t := range def.Tasks {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}
