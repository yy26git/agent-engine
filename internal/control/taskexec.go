package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The task process contract.
//
// This is the entry point an outer scheduler invokes, and it exists because a
// PROCESS EXIT is the only signal such a scheduler can observe. The mapping is
// therefore fixed and narrow:
//
//	0 PASS        current verification, required independent approval, terminal commit
//	1 FAIL        an objective failure remained after allowed recovery
//	2 BLOCKED     authority, evidence, policy, access or safe reconciliation missing
//	3 CANCELLED   durable cancellation reconciled and owned effects stopped
//
// The exit code is derived from COMMITTED STATE, never from what the run happened
// to be doing when it finished. A run that finished its work but could not prove
// it did not exit 0; it exited 2. That is the whole point: exit 0 has to mean the
// same thing to a scheduler that has never seen this process as anything but a
// number.
//
// One consequence worth stating plainly: a process that is SIGKILLed or dies of
// disk exhaustion may emit no result at all. Absence is UNKNOWN, and the caller
// must read that as UNKNOWN rather than inferring success from a zero-ish
// outcome. Nothing here can defend against a machine that loses the process, so
// the durable result — not the exit code — is what a recovery reads.

// TaskExecOutcome is the canonical result a task process commits.
type TaskExecOutcome struct {
	// RequestID is the admitted child this execution ran.
	RequestID string `json:"request_id"`
	// RootRequestID, WorkflowSHA256 and LaunchEpoch tie the child to the run that
	// scheduled it, so a result cannot be replayed into a different graph or a
	// later epoch.
	RootRequestID  string `json:"root_request_id,omitempty"`
	WorkflowSHA256 string `json:"workflow_sha256,omitempty"`
	LaunchEpoch    int    `json:"launch_epoch,omitempty"`
	// Status is the terminal contract status.
	Status TaskStatus `json:"status"`
	// ResultSHA256 is the digest of the committed canonical result body.
	ResultSHA256 string `json:"result_sha256,omitempty"`
	// Reason is a bounded one-line explanation for a non-PASS outcome.
	Reason string `json:"reason,omitempty"`
	// CompletedAt is when the outcome was committed.
	CompletedAt string `json:"completed_at"`
}

// TaskStatus is the four-valued process contract.
type TaskStatus string

const (
	TaskPass      TaskStatus = "PASS"
	TaskFail      TaskStatus = "FAIL"
	TaskBlocked   TaskStatus = "BLOCKED"
	TaskCancelled TaskStatus = "CANCELLED"
)

// ExitCode returns the process exit for a status.
//
// There is deliberately no "unknown" member: every committed outcome maps to a
// code, and an uncommitted run never reaches this function.
func (s TaskStatus) ExitCode() int {
	switch s {
	case TaskPass:
		return 0
	case TaskFail:
		return 1
	case TaskBlocked:
		return 2
	case TaskCancelled:
		return 3
	}
	return 2
}

// Terminal reports whether a status is one of the four contract outcomes.
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskPass, TaskFail, TaskBlocked, TaskCancelled:
		return true
	}
	return false
}

// TaskExecRequest is the validated identity a task execution must present.
type TaskExecRequest struct {
	RequestID     string
	RootRequestID string
	WorkflowSHA   string
	LaunchEpoch   int
}

// ErrTaskExecRefused is returned when a task execution's identity does not check
// out.
var ErrTaskExecRefused = errors.New("agent-engine: task execution refused")

// ValidateTaskExec checks that an execution may act on a child request.
//
// Three things are checked, and all three matter:
//
//   - The child must be an ADMITTED request. An execution naming an unknown id is
//     refused rather than creating one, because the only path that creates
//     requests is admission.
//   - When a root is named, the child must actually belong to it. Without this, a
//     valid child id from one run could be executed inside another and its result
//     attributed to the wrong graph.
//   - A named workflow digest and epoch must match what was admitted, so a result
//     from an earlier epoch cannot be presented as current.
func (s *Store) ValidateTaskExec(req TaskExecRequest) error {
	if strings.TrimSpace(req.RequestID) == "" {
		return fmt.Errorf("%w: no request id", ErrTaskExecRefused)
	}
	if _, err := s.State(req.RequestID); err != nil {
		return fmt.Errorf("%w: %s is not an admitted request: %v", ErrTaskExecRefused, req.RequestID, err)
	}
	if strings.TrimSpace(req.RootRequestID) == "" {
		// A direct single-task execution is supported: OMA absent must leave direct
		// task supervision working.
		return nil
	}
	root, err := s.State(req.RootRequestID)
	if err != nil {
		return fmt.Errorf("%w: the named root %s is not admitted: %v", ErrTaskExecRefused, req.RootRequestID, err)
	}
	// The child map is keyed by TASK id and holds the derived REQUEST id, so the
	// lookup has to match on the value. Looking it up by key would reject every
	// genuine child while accepting any request id that happened to share a name
	// with a task, which is the opposite of the check's purpose.
	var taskID string
	for tid, cid := range root.WorkflowChildren {
		if cid == req.RequestID {
			taskID = tid
			break
		}
	}
	if taskID == "" {
		return fmt.Errorf("%w: %s is not a child of %s; an execution may only act on a child of the run "+
			"that scheduled it", ErrTaskExecRefused, req.RequestID, req.RootRequestID)
	}
	if strings.TrimSpace(req.WorkflowSHA) != "" && root.WorkflowDefinitionSHA != "" &&
		!strings.EqualFold(req.WorkflowSHA, root.WorkflowDefinitionSHA) {
		return fmt.Errorf("%w: the execution names workflow %s but the root was admitted against %s",
			ErrTaskExecRefused, req.WorkflowSHA, root.WorkflowDefinitionSHA)
	}
	if req.LaunchEpoch > 0 {
		if root.LaunchEpoch == 0 {
			return fmt.Errorf("%w: the execution names epoch %d but the root has never launched",
				ErrTaskExecRefused, req.LaunchEpoch)
		}
		if req.LaunchEpoch != root.LaunchEpoch {
			return fmt.Errorf("%w: the execution names epoch %d but the root is at epoch %d; a result from "+
				"an earlier launch may not be presented as current", ErrTaskExecRefused, req.LaunchEpoch, root.LaunchEpoch)
		}
	}
	return nil
}

// VerifyUpstreamChildren checks that every upstream child a task depends on has a
// committed PASS.
//
// This is a SAFETY check, not a second dependency scheduler. It never reorders or
// waits: if an upstream result is missing, contradictory or not PASS, the
// execution REFUSES. Deciding what to do about that belongs to the outer
// scheduler, which already knows the graph.
func (s *Store) VerifyUpstreamChildren(requestID string, upstream []string) error {
	for _, dep := range upstream {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		res, err := s.Result(dep)
		if err != nil {
			return fmt.Errorf("%w: upstream %s has no readable result, so this task cannot proceed: %v",
				ErrTaskExecRefused, dep, err)
		}
		if !res.Ready {
			return fmt.Errorf("%w: upstream %s has not finished", ErrTaskExecRefused, dep)
		}
		if string(res.Status) != "PASS" {
			return fmt.Errorf("%w: upstream %s is %s, not PASS", ErrTaskExecRefused, dep, res.Status)
		}
	}
	return nil
}

// DriveTaskLifecycle advances one task lifecycle; Run calls it on each tick.
//
// It is the body of the task-exec verb, separated from the CLI so that the
// lifecycle can be driven by a test or by an in-process caller with the same
// guarantees. The steps, in order:
//
//  0. Whatever the step committed is reconciled on the way out: a terminal
//     request releases what it owns and proves it quiescent. Cleanup runs HERE
//     and nowhere else, because this is the only point after the commit and
//     outside the store lock.
//  1. A recorded cancellation that has been reconciled wins outright.
//  2. Otherwise an open review is collected from the EXPECTED reviewer's own
//     session, and submitted through the broker-owned ingress. The engine never
//     decides the outcome; it only measures who answered.
//  3. Whatever the committed result then says is the outcome.
//
// A step that cannot complete leaves the request non-terminal, which the caller
// reads as BLOCKED. Nothing here invents a PASS to make a scheduler happy.
// Equally, nothing here reports a finished request as finished while its worker is
// still writing the workspace: that is the error reconciliation returns.
//
// Step 0 is on the way OUT, not the way in, and that ordering is the whole
// property. The guard this replaced — `if st.Stage.Terminal() { return nil }` —
// sat at the top and returned before anything was released, which is how a
// VERIFIED request kept its ACP process alive while a CANCELLED one released it.
// Reconciling on entry would not fix it either: a request that becomes terminal
// DURING this call (a completion PASS commits here) would leave without cleanup,
// and `task-exec` is a one-shot process, so "the next tick" never comes.
func (e *Engine) DriveTaskLifecycle(ctx context.Context, requestID string) error {
	if e.opts.DryRun {
		return nil
	}

	return e.reconcileOnExit(ctx, requestID, func() error { return e.driveTaskLifecycleStep(ctx, requestID) })
}

// reconcileOnExit runs one lifecycle step and then reconciles whatever it left
// terminal.
//
// It is a single wrapper rather than a call repeated at each return because the
// terminal commit can happen on ANY route through the step — a verified
// completion, a blocked review, a cancellation — and a branch that forgot to
// reconcile would silently leak a process. One exit is the property; four
// carefully-placed calls would only be the intention.
//
// A step that ERRORED still reconciles, because an error after the commit does
// not un-commit it: a request that verified and then failed to project its result
// still owns a worker process, and the step's error must not be the only evidence
// that anything was released.
//
// A reconciliation failure REPLACES the step's error rather than being folded into
// it. Both are true, and the caller must see the cleanup one: the request is
// terminal and its workspace unreleased, which is the more urgent of the two.
func (e *Engine) reconcileOnExit(ctx context.Context, requestID string, step func() error) error {
	stepErr := step()

	st, err := e.store.State(requestID)
	if err != nil || !st.Stage.Terminal() {
		if stepErr != nil {
			return stepErr
		}
		return err
	}
	rep, terr := e.ReconcileTerminalOutcome(ctx, requestID)
	if terr != nil {
		if stepErr != nil {
			return stepErr
		}
		return terr
	}
	if !rep.Quiesced {
		// NOT a log line. The request is terminal and cannot be un-committed, so
		// returning nil would report a finished request whose workspace nobody
		// released — the exact lie this path exists to prevent. It surfaces through
		// `Run` and the task-exec exit code, where an operator will actually see it.
		return fmt.Errorf("agent-engine: request %s committed %s but its owned effects were not proven "+
			"released: %s", requestID, st.Stage, rep.Reason())
	}
	return stepErr
}

// driveTaskLifecycleStep advances one request by at most one step, and commits
// whatever decision that step implies.
//
// It is separate from its caller because a terminal stage is committed INSIDE
// this function — `SubmitAnswer` verifies a request — and cleanup may not run
// under the store lock that commits it. So the decision happens here and the
// reconciliation happens on the way out, once, whatever route the request took.
func (e *Engine) driveTaskLifecycleStep(ctx context.Context, requestID string) error {
	st, err := e.store.State(requestID)
	if err != nil {
		return err
	}
	if st.Stage.Terminal() {
		return nil
	}
	if st.CancellationRequested {
		// The intent is recorded; the outcome waits for reconciliation, which is
		// ReconcileCancellation's job. An unresolved stop never becomes CANCELLED.
		_, err := e.reconcileCancellation(ctx, requestID, true)
		return err
	}
	if st.RuntimeBlock != nil {
		return e.reconcileRuntimeBlock(ctx, requestID)
	}
	if expired, err := e.checkRootDeadline(ctx, requestID, st); expired || err != nil {
		return err
	}
	if st.ActiveReview == nil {
		if st.Replacement.Phase != "" && st.Replacement.Phase != GenActive {
			_, err := e.DriveReplacement(ctx, requestID)
			return err
		}
		// Nothing has been asked, so nothing can have been answered. A task with
		// no review owed has not been audited.
		return nil
	}
	ans, cerr := e.CollectReviewerAnswer(ctx, requestID, 0)
	if cerr != nil {
		return e.handleReviewCollection(ctx, requestID, st, cerr)
	}
	if _, aerr := e.store.SubmitAnswer(requestID, ans); aerr != nil {
		return aerr
	}

	// A review just settled. Any completion candidate that arrived while it was in
	// flight is promoted here — the ONE place a settled review is observed.
	//
	// It has to be here rather than in a worker-event handler: the candidate may
	// have been raised minutes earlier and no further worker event may ever
	// arrive. Waiting for one strands the only statement that the work finished.
	return e.PromoteRetainedAssessment(ctx, requestID)
}

// PromoteRetainedAssessment opens the review owed by a retained candidate.
//
// It is deliberately separate from the promotion rule itself, which lives on
// State. This is the lifecycle edge: the decision is the state's, and the timing
// is the engine's.
func (e *Engine) PromoteRetainedAssessment(ctx context.Context, requestID string) error {
	if e.opts.DryRun {
		return nil
	}
	_, err := e.store.UpdateCommitted(requestID, func(s *State) error {
		eff, opened, perr := s.PromoteAssessment(e.now())
		if perr != nil {
			return perr
		}
		if !opened || eff.SessionID == "" {
			return nil
		}
		return s.RecordEffect(eff)
	})
	return err
}
