package oma

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
)

func testDef() *contract.WorkflowDefinition {
	return &contract.WorkflowDefinition{
		Version: contract.WorkflowVersion,
		Name:    "crossing",
		Tasks: []contract.WorkflowTask{
			{ID: "investigate", Operation: contract.OpInvestigate, Objective: "Investigate",
				Instruction: "Investigate.", ExecutorProfile: "impl", ReviewerProfile: "audit"},
			{ID: "repair", Operation: contract.OpRepair, Objective: "Repair",
				Instruction: "Repair.", ExecutorProfile: "impl", ReviewerProfile: "audit",
				DependsOn: []string{"investigate"}},
		},
	}
}

func testOpts() TranslateOptions {
	return TranslateOptions{
		ExBinary:      "/usr/local/bin/agent-engine",
		StoreRoot:     "/approved/store",
		RootRequestID: "root-1",
		WorkflowSHA:   strings.Repeat("a", 64),
		Epoch:         1,
		Definition:    testDef(),
		Children:      map[string]string{"investigate": "root-1--investigate--aa", "repair": "root-1--repair--bb"},
		Objectives:    map[string]string{},
	}
}

// TestTranslate_ExactCliGoldenMapping asserts the generated documents match the
// shape the PINNED CLI actually validates.
//
// Each of these requirements was discovered by running the real binary, not by
// reading a schema: team.name and agent.model are both required even for a process
// backend, and the backend discriminator is backend.kind == "process".
func TestTranslate_ExactCliGoldenMapping(t *testing.T) {
	tr, err := Translate(testOpts())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if tr.Team.Name == "" {
		t.Error("team.name is empty; the pinned CLI refuses the team")
	}
	if len(tr.Team.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(tr.Team.Agents))
	}
	for _, a := range tr.Team.Agents {
		if a.Model == "" {
			t.Errorf("agent %q has no model; the pinned CLI refuses the team", a.Name)
		}
		if a.Backend.Kind != ProcessBackendKind {
			t.Errorf("agent %q backend kind = %q, want %q", a.Name, a.Backend.Kind, ProcessBackendKind)
		}
		if a.Backend.Input != "none" {
			t.Errorf("agent %q input = %q, want none", a.Name, a.Backend.Input)
		}
		if !strings.HasPrefix(a.Backend.Command, "/") {
			t.Errorf("agent %q command is not absolute: %q", a.Name, a.Backend.Command)
		}
	}
	if tr.Team.Orchestrator.MaxConcurrency != 1 {
		t.Errorf("maxConcurrency = %d, want 1", tr.Team.Orchestrator.MaxConcurrency)
	}
	// Outer retries are disabled unconditionally.
	for _, task := range tr.Tasks.Tasks {
		if task.MaxRetries != 0 {
			t.Errorf("task %q maxRetries = %d, want 0", task.Title, task.MaxRetries)
		}
		if task.Input != "none" {
			t.Errorf("task %q input = %q, want none", task.Title, task.Input)
		}
	}
	// Dependencies are TITLES, never OMA's generated ids.
	for _, task := range tr.Tasks.Tasks {
		for _, dep := range task.DependsOn {
			if dep == "" || strings.Contains(dep, "-") && len(dep) > 20 {
				t.Errorf("task %q depends on %q, which looks like an OMA generated id rather than a title", task.Title, dep)
			}
		}
	}
}

// TestTranslate_UnsupportedFieldsNotEmitted asserts no field the pinned CLI
// ignores is emitted.
//
// `requires` and `verify` exist in OMA's task model but the pinned asTaskSpecs
// mapper does not forward them, so emitting them would create the appearance of a
// dependency gate that does not exist.
func TestTranslate_UnsupportedFieldsNotEmitted(t *testing.T) {
	tr, err := Translate(testOpts())
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{`"requires"`, `"verify"`, `"systemPrompt"`, `"coordinator"`} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("the generated input contains %s, which the pinned CLI does not enforce", banned)
		}
	}
}

// TestTranslate_DuplicateNormalizedTitleRefused asserts two titles that normalise
// identically never reach the CLI, which would merge them.
func TestTranslate_DuplicateNormalizedTitleRefused(t *testing.T) {
	opts := testOpts()
	opts.Definition = &contract.WorkflowDefinition{
		Version: contract.WorkflowVersion,
		Tasks: []contract.WorkflowTask{
			{ID: "review", Operation: contract.OpInvestigate, Instruction: "a",
				ExecutorProfile: "impl", ReviewerProfile: "audit"},
			{ID: "review ", Operation: contract.OpInvestigate, Instruction: "b",
				ExecutorProfile: "impl", ReviewerProfile: "audit"},
		},
	}
	opts.Children = map[string]string{"review": "c1", "review ": "c2"}
	if _, err := Translate(opts); err == nil {
		t.Fatal("two titles that normalise identically were translated")
	}
}

// TestTranslate_RefusesUnschedulableGraph asserts every refusal happens before a
// process could be spawned.
func TestTranslate_RefusesUnschedulableGraph(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*TranslateOptions)
	}{
		{"a relative EX binary", func(o *TranslateOptions) { o.ExBinary = "agent-engine" }},
		{"a relative store root", func(o *TranslateOptions) { o.StoreRoot = "store" }},
		{"a zero epoch", func(o *TranslateOptions) { o.Epoch = 0 }},
		{"a task with no admitted child", func(o *TranslateOptions) {
			o.Children = map[string]string{"investigate": "c1"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOpts()
			tc.mut(&opts)
			if _, err := Translate(opts); err == nil {
				t.Fatalf("translation accepted %s", tc.name)
			}
		})
	}
	// A dependency outside the graph would be waited on forever.
	opts := testOpts()
	opts.Definition.Tasks[0].DependsOn = []string{"not-in-graph"}
	if _, err := Translate(opts); err == nil {
		t.Error("a dependency outside the graph was translated")
	}
}
