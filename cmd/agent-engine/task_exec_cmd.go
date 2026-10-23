package main

// The task-exec entry point.
//
// An outer scheduler can only see a process exit, so this command exists to turn
// COMMITTED STATE into that exit. It deliberately does not interpret the run: it
// validates that it is allowed to act, drives the ordinary lifecycle, and then
// reports whatever the durable result says.
//
// The three rules that keep the contract honest:
//
//   - The exit code comes from the committed result, never from the run's mood at
//     the moment it finished. A run that finished its work but could not prove it
//     did exits 2, not 0.
//   - Retrieval semantics are untouched. `status` and `result` still exit 0 when
//     they successfully return a FAIL; only this verb reports failure through its
//     status.
//   - Diagnostics go to stderr and one bounded JSON result goes to stdout AFTER
//     the durable commit. A scheduler may read stdout; it must never need it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/runtime"
)

func cmdTaskExec(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("task-exec", stderr)
	requestID := fs.String("request-id", "", "admitted child request to execute")
	rootID := fs.String("root-request-id", "", "admitted root that scheduled this child")
	workflowSHA := fs.String("workflow-sha256", "", "admitted workflow definition digest")
	epoch := fs.Int("launch-epoch", 0, "outer launch epoch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}

	exec := control.TaskExecRequest{
		RequestID:     strings.TrimSpace(*requestID),
		RootRequestID: strings.TrimSpace(*rootID),
		WorkflowSHA:   strings.TrimSpace(*workflowSHA),
		LaunchEpoch:   *epoch,
	}
	// Identity first: an execution that may not act must say so without touching
	// anything, so a scheduler's first attempt is never a side effect.
	if err := st.ValidateTaskExec(exec); err != nil {
		fmt.Fprintf(stderr, "task-exec: %v\n", err)
		emitTaskOutcome(stdout, control.TaskExecOutcome{
			RequestID:      exec.RequestID,
			RootRequestID:  exec.RootRequestID,
			WorkflowSHA256: exec.WorkflowSHA,
			LaunchEpoch:    exec.LaunchEpoch,
			Status:         control.TaskBlocked,
			Reason:         err.Error(),
			CompletedAt:    stNow().Format(timeLayout),
		}, asJSON)
		return control.TaskBlocked.ExitCode()
	}

	state, serr := st.State(exec.RequestID)
	if serr == nil && state.Stage.Terminal() {
		outcome := readTaskOutcome(st, exec)
		emitTaskOutcome(stdout, outcome, asJSON)
		return outcome.Status.ExitCode()
	}
	rt, rerr := pinnedRuntime(st.Config(), stderr, !asJSON)
	if rerr != nil {
		fmt.Fprintf(stderr, "task-exec: runtime: %v\n", rerr)
		emitTaskOutcome(stdout, control.TaskExecOutcome{RequestID: exec.RequestID, Status: control.TaskBlocked, Reason: rerr.Error(), CompletedAt: stNow().Format(timeLayout)}, asJSON)
		return 2
	}

	// Drive the ordinary lifecycle. Everything that decides PASS already exists on
	// the completion path; this verb adds no authority of its own.
	eng, err := control.NewEngine(st, rt, exec.RequestID, control.EngineOptions{
		Now: stNow,
	})
	if err != nil {
		fmt.Fprintf(stderr, "task-exec: engine: %v\n", err)
		emitTaskOutcome(stdout, control.TaskExecOutcome{
			RequestID: exec.RequestID, Status: control.TaskBlocked,
			Reason: err.Error(), CompletedAt: stNow().Format(timeLayout),
		}, asJSON)
		return control.TaskBlocked.ExitCode()
	}
	defer eng.Close()

	if err := eng.InitializeTransports(context.Background(), exec.RequestID, func() ([]runtime.Backend, func(), error) {
		return admitPinnedTransports(context.Background(), st, exec.RequestID, workspaceOf(st, exec.RequestID), false)
	}); err != nil {
		fmt.Fprintf(stderr, "task-exec: transport: %v\n", err)
		emitTaskOutcome(stdout, control.TaskExecOutcome{RequestID: exec.RequestID, Status: control.TaskBlocked, Reason: err.Error(), CompletedAt: stNow().Format(timeLayout)}, asJSON)
		return control.TaskBlocked.ExitCode()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := eng.PrepareTaskSessions(ctx, exec.RequestID); err != nil {
		fmt.Fprintf(stderr, "task-exec: activation: %v\n", err)
		outcome := readTaskOutcome(st, exec)
		if outcome.Status == control.TaskBlocked {
			outcome.Reason = err.Error()
		}
		emitTaskOutcome(stdout, outcome, asJSON)
		return outcome.Status.ExitCode()
	}
	if len(state.ExecutionProfiles) > 0 {
		if _, err := eng.Run(ctx, exec.RequestID); err != nil {
			fmt.Fprintf(stderr, "task-exec: supervision: %v\n", err)
		}
	}
	if err := eng.DriveTaskLifecycle(ctx, exec.RequestID); err != nil {
		fmt.Fprintf(stderr, "task-exec: lifecycle: %v\n", err)
	}

	// The exit comes from the committed result, or from the absence of one.
	outcome := readTaskOutcome(st, exec)
	emitTaskOutcome(stdout, outcome, asJSON)
	return outcome.Status.ExitCode()
}

// readTaskOutcome derives the process contract from committed state.
//
// Absence is BLOCKED, never PASS. A run whose result cannot be read has not
// proved anything, and reporting success there is precisely the false-PASS this
// contract exists to prevent.
func readTaskOutcome(st *control.Store, exec control.TaskExecRequest) control.TaskExecOutcome {
	out := control.TaskExecOutcome{
		RequestID:      exec.RequestID,
		RootRequestID:  exec.RootRequestID,
		WorkflowSHA256: exec.WorkflowSHA,
		LaunchEpoch:    exec.LaunchEpoch,
		CompletedAt:    stNow().Format(timeLayout),
	}
	res, err := st.Result(exec.RequestID)
	if err != nil {
		out.Status = control.TaskBlocked
		out.Reason = "no committed result: " + err.Error()
		return out
	}
	if !res.Ready {
		out.Status = control.TaskBlocked
		out.Reason = "the request is not finished: stage " + res.Stage
		return out
	}
	state, err := st.State(exec.RequestID)
	if err != nil {
		out.Status = control.TaskBlocked
		out.Reason = "cannot read committed result identity: " + err.Error()
		return out
	}
	out.ResultSHA256 = state.ResultDigest
	switch res.Status {
	case "PASS":
		out.Status = control.TaskPass
	case "FAIL":
		out.Status = control.TaskFail
		out.Reason = res.Summary
	case "CANCELLED":
		out.Status = control.TaskCancelled
	default:
		out.Status = control.TaskBlocked
		out.Reason = "committed status " + string(res.Status) + " is not a task-process outcome"
	}
	return out
}

// emitTaskOutcome writes exactly one bounded JSON result to stdout.
//
// It is emitted AFTER the durable commit, and its failure is not fatal: the
// authoritative record is on disk, and a scheduler that could not read stdout is
// better served by a diagnostic than by a second, differently-shaped result.
func emitTaskOutcome(stdout io.Writer, out control.TaskExecOutcome, asJSON bool) {
	if !asJSON {
		fmt.Fprintf(stdout, "%s\n", out.Status)
		if out.Reason != "" {
			fmt.Fprintf(stdout, "  %s\n", out.Reason)
		}
		return
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	stdout.Write(append(data, '\n'))
}
