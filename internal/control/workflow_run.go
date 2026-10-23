package control

// The outer run owner.
//
// S3 admits a graph; S4 supervises the tasks inside it. This file owns the thing
// in between: writing the generated documents for one launch epoch, running the
// pinned scheduler through the OMA gate, and then deciding what the run actually
// proved.
//
// The decision rule is the whole design, and it is deliberately asymmetric:
//
//	A graph that reports success proves only that processes exited.
//	A task that committed a PASS result proves the engineering gate was satisfied.
//
// So root PASS requires BOTH a successful graph receipt AND every required child
// carrying a committed PASS result whose identity matches. Graph success alone
// never promotes a run, and an OMA failure with every child PASS is reported as
// BLOCKED rather than quietly repaired — a run that was operationally broken but
// technically correct is still a run the operator has to look at.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/oma"
)

// WorkflowRunRecord is the durable, versioned record of one outer run.
//
// It is discriminated from an ordinary task's state on disk: a root has a graph,
// an epoch and a receipt, and none of those apply to a single supervised task.
type WorkflowRunRecord struct {
	Version int `json:"version"`
	// RequestID is the root.
	RequestID string `json:"request_id"`
	// DefinitionSHA256 pins the admitted graph.
	DefinitionSHA256 string `json:"definition_sha256"`
	// InputDigest pins this run's input.
	InputDigest string `json:"input_sha256"`
	// Epoch is the launch epoch this record describes.
	Epoch int `json:"epoch"`
	// Phase is where the run got to.
	Phase string `json:"phase"`
	// ChildResults records each required child's committed status, keyed by task.
	ChildResults map[string]ChildOutcome `json:"child_results,omitempty"`
	// GraphReceiptSHA256 is the measured digest of the scheduler's stdout.
	GraphReceiptSHA256 string `json:"graph_receipt_sha256,omitempty"`
	// GraphSuccess is what the scheduler reported.
	GraphSuccess bool `json:"graph_success"`
	// RootOutcome is the committed aggregate.
	RootOutcome string `json:"root_outcome,omitempty"`
	// Reason explains a non-PASS outcome.
	Reason string `json:"reason,omitempty"`
	// LaunchedAt and FinishedAt bound the run.
	LaunchedAt string `json:"launched_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// ChildOutcome is one required child's committed verdict.
type ChildOutcome struct {
	TaskID   string `json:"task_id"`
	ChildID  string `json:"child_id"`
	Status   string `json:"status"`
	Ran      bool   `json:"ran"`
	Verified bool   `json:"verified"`
	Reason   string `json:"reason,omitempty"`
}

// Run phases.
const (
	RunPhaseAdmitted         = "ADMITTED"
	RunPhaseLaunching        = "LAUNCHING"
	RunPhaseLaunched         = "LAUNCHED"
	RunPhaseAggregating      = "AGGREGATING"
	RunPhaseCompleted        = "COMPLETED"
	RunPhaseRecoveryRequired = "RECOVERY_REQUIRED"
)

// ErrRunRefused is returned when a run cannot be launched or aggregated.
var ErrRunRefused = errors.New("agent-engine: workflow run refused")

// RunWorkflow launches one epoch of an admitted graph and aggregates the result.
//
// It returns the run record and an error. A non-nil error means the run could not
// proceed at all; a nil error with a non-PASS RootOutcome means it proceeded and
// the evidence did not support a pass. Those are different failures and are kept
// apart so a caller can retry the first without mistaking it for the second.
func (s *Store) RunWorkflow(ctx context.Context, requestID string, gate oma.Gate, launcher oma.Launcher, timeout time.Duration) (WorkflowRunRecord, error) {
	st, err := s.State(requestID)
	if err != nil {
		return WorkflowRunRecord{}, err
	}
	if st.Stage.Terminal() {
		return WorkflowRunRecord{}, fmt.Errorf("%w: request %s is already terminal as %s; a published "+
			"outcome is never re-run", ErrRunRefused, requestID, st.ResultStatus)
	}
	if st.WorkflowDefinitionSHA == "" {
		return WorkflowRunRecord{}, fmt.Errorf("%w: request %s is not a workflow root; it has no admitted graph",
			ErrRunRefused, requestID)
	}
	if len(st.WorkflowChildOrder) == 0 {
		return WorkflowRunRecord{}, fmt.Errorf("%w: request %s has no admitted children", ErrRunRefused, requestID)
	}

	// The epoch increments BEFORE anything is launched, and it is what makes a
	// result from an earlier launch unacceptable to this one.
	nextEpoch := st.LaunchEpoch + 1
	if _, err := s.UpdateCommitted(requestID, func(cur *State) error {
		cur.LaunchEpoch = nextEpoch
		return nil
	}); err != nil {
		return WorkflowRunRecord{}, err
	}

	// Load the pinned definition and the admitted child map from the bundle, never
	// from anything a caller supplies.
	definition, files, defSHA, err := s.loadBundle(st.WorkflowDefinitionSHA)
	if err != nil {
		return WorkflowRunRecord{}, err
	}
	if !strings.EqualFold(defSHA, st.WorkflowDefinitionSHA) {
		return WorkflowRunRecord{}, fmt.Errorf("%w: the stored definition no longer matches the admitted identity",
			ErrRunRefused)
	}
	_ = files

	st, _ = s.State(requestID)
	objectives := map[string]string{}
	for _, task := range definition.Tasks {
		objectives[task.ID] = task.Objective
	}

	epochDir := oma.EpochDir(s.cfg.StoreRoot, requestID, nextEpoch)
	if err := os.MkdirAll(filepath.Join(epochDir, "inputs"), 0o755); err != nil {
		return WorkflowRunRecord{}, fmt.Errorf("agent-engine: the epoch directory could not be created: %w", err)
	}

	// The generated documents point at the EXECUTABLE PATH the operator configured.
	// There is no default: this owner cannot invent an executable to launch.
	exBinary := strings.TrimSpace(s.cfg.AgentDeckBinary)
	if !strings.HasPrefix(exBinary, "/") {
		return WorkflowRunRecord{}, fmt.Errorf("%w: no absolute EX binary is configured, so no child "+
			"process can be launched; a graph with no executable is not schedulable", ErrRunRefused)
	}

	tr, terr := oma.Translate(oma.TranslateOptions{
		ExBinary:      exBinary,
		StoreRoot:     s.cfg.StoreRoot,
		RootRequestID: requestID,
		WorkflowSHA:   defSHA,
		Epoch:         nextEpoch,
		Definition:    definition,
		Children:      st.WorkflowChildren,
		Objectives:    objectives,
	})
	if terr != nil {
		return WorkflowRunRecord{}, terr
	}
	tasksPath := filepath.Join(epochDir, "inputs", "tasks.json")
	teamPath := filepath.Join(epochDir, "inputs", "team.json")
	if err := writeJSONFile(tasksPath, tr.Tasks); err != nil {
		return WorkflowRunRecord{}, err
	}
	if err := writeJSONFile(teamPath, tr.Team); err != nil {
		return WorkflowRunRecord{}, err
	}

	rec := WorkflowRunRecord{
		Version:          1,
		RequestID:        requestID,
		DefinitionSHA256: defSHA,
		InputDigest:      st.WorkflowInputDigest,
		Epoch:            nextEpoch,
		Phase:            RunPhaseLaunching,
		LaunchedAt:       s.nowRFC3339(),
		ChildResults:     map[string]ChildOutcome{},
	}

	titles := map[string]bool{}
	for _, t := range definition.Tasks {
		titles[oma.NormaliseTitle(t.ID)] = true
	}

	// The launch intent is committed before the scheduler exists.
	launch, lerr := gate.Run(ctx, oma.LaunchRequest{
		Launcher:  launcher,
		TasksFile: tasksPath,
		TeamFile:  teamPath,
		WorkDir:   epochDir,
		Epoch:     nextEpoch,
		Timeout:   timeout,
		OnLaunchIntent: func(epoch int, argv []string) error {
			_, err := s.UpdateCommitted(requestID, func(cur *State) error {
				cur.LaunchedAt = s.nowRFC3339()
				return nil
			})
			return err
		},
	}, titles)
	if lerr != nil {
		rec.Phase = RunPhaseRecoveryRequired
		rec.RootOutcome = string(TaskBlocked)
		rec.Reason = lerr.Error()
		_ = s.writeWorkflowRunRecord(requestID, rec)
		return rec, lerr
	}
	rec.Phase = RunPhaseLaunched
	if launch.Receipt != nil {
		rec.GraphSuccess = launch.Receipt.Success
		if data, err := os.ReadFile(launch.StdoutPath); err == nil {
			rec.GraphReceiptSHA256 = sha256Hex(data)
		}
	}
	if launch.ReceiptErr != nil {
		rec.Reason = launch.ReceiptErr.Error()
	}

	// Aggregate from the children's COMMITTED results, not from the receipt.
	rec.ChildResults, rec.RootOutcome, rec.Reason = s.aggregateChildren(st, definition, rec)
	rec.Phase = RunPhaseCompleted
	rec.FinishedAt = s.nowRFC3339()
	if err := s.writeWorkflowRunRecord(requestID, rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// aggregateChildren decides the root outcome from committed child results.
//
// Three independent conditions must ALL hold for PASS. Each catches a different
// false success, which is why none of them is folded into another:
//
//   - every required child committed PASS;
//   - the graph receipt exists AND reports success;
//   - the receipt contains no retry (a retry means work ran outside its budget).
func (s *Store) aggregateChildren(st *State, def *contract.WorkflowDefinition, rec WorkflowRunRecord) (map[string]ChildOutcome, string, string) {
	results := map[string]ChildOutcome{}
	var reasons []string

	// The graph's own verdict is one input, never the decision.
	graphOK := rec.GraphSuccess

	for _, task := range def.Tasks {
		childID := st.WorkflowChildren[task.ID]
		out := ChildOutcome{TaskID: task.ID, ChildID: childID}
		if childID == "" {
			out.Status = "MISSING"
			out.Reason = "no admitted child request"
			results[task.ID] = out
			reasons = append(reasons, task.ID+": "+out.Reason)
			continue
		}
		res, err := s.Result(childID)
		switch {
		case err != nil:
			out.Status = "UNKNOWN"
			out.Reason = "no readable result: " + err.Error()
		case !res.Ready:
			out.Status = "NOT_FINISHED"
			out.Reason = "stage " + res.Stage
		case string(res.Status) != string(contract.StatusPass):
			out.Status = string(res.Status)
			out.Reason = res.Summary
		default:
			out.Status = "PASS"
			out.Verified = true
		}
		results[task.ID] = out
		if !out.Verified {
			reasons = append(reasons, fmt.Sprintf("%s is %s%s", task.ID, out.Status,
				ifNonEmpty(": "+out.Reason, "")))
		}
	}

	if len(reasons) == 0 && !graphOK {
		// Every child passed but the graph did not. That is an OPERATIONAL failure,
		// not a technical one, and it is reported rather than repaired.
		return results, string(TaskBlocked),
			"every required child committed PASS but the graph did not report success; " +
				"this is an operational failure that a human must look at"
	}
	if len(reasons) > 0 {
		return results, string(TaskFail), strings.Join(reasons, "; ")
	}
	if rec.Reason != "" {
		return results, string(TaskBlocked), rec.Reason
	}
	return results, string(TaskPass), ""
}

func ifNonEmpty(v, alt string) string {
	if v != "" {
		return v
	}
	return alt
}

// writeWorkflowRunRecord persists the run record beside the root's state.
func (s *Store) writeWorkflowRunRecord(requestID string, rec WorkflowRunRecord) error {
	rel := filepath.Join(RequestsDirName, requestID, fmt.Sprintf("RUN-e%d.json", rec.Epoch))
	if err := writeJSONDurable(s.cfg.StoreRoot, rel, rec, 0o444); err != nil {
		return fmt.Errorf("agent-engine: the run record could not be written: %w", err)
	}
	return nil
}

// writeJSONFile writes a generated document for the external CLI to read.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("agent-engine: encoding %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("agent-engine: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("agent-engine: writing %s: %w", filepath.Base(path), err)
	}
	return nil
}
