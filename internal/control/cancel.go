package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/runtime"
)

// Graceful cancellation.
//
// Persisting "cancel this" is trivial and is not cancellation. A request is
// CANCELLED only when everything it owns has been accounted for, and the ordering
// is what makes the difference:
//
//	persist intent → stop new dispatch → cancel owned jobs → stop owned sessions
//	→ PROVE quiescence → commit CANCELLED → publish the result
//
// Each step is durable before the next, so a crash leaves a request that is
// visibly mid-cancellation and can be resumed, rather than one that is cancelled
// on paper while a worker keeps writing the workspace.
//
// The step most worth naming is PROVE quiescence. `Stop` is a request to a
// runtime, and the runtime acknowledging it is not the same as the session having
// released the workspace. A cancelled request whose worker is still writing is
// worse than one that is honestly still reconciling, so an unproved stop records
// BLOCKED instead of claiming success.

// CancelReport is what a cancellation attempt achieved.
type CancelReport struct {
	// RequestID is the cancelled request.
	RequestID string
	// Stopped lists the sessions a Stop was issued for.
	Stopped []string
	// JobsCancelled lists the managed check jobs that were signalled.
	JobsCancelled []string
	// Quiesced is true only when every owned session was inspected and found
	// stopped. It is the precondition for committing CANCELLED.
	Quiesced bool
	// Stale are sessions that should have been running and were not. They are
	// quiescent for this teardown, but they are not the same fact as ones that
	// finished, and reporting them separately is what keeps a supervisor from
	// treating a vanished worker as a completed one.
	Stale []string
	// Unresolved lists effects whose outcome could not be proven.
	Unresolved []string
	// Committed is true when the terminal CANCELLED outcome was written.
	Committed bool
	// Reason explains a refusal or a BLOCKED result.
	Reason string
}

// processOwner is a backend that owns OS processes of its own.
//
// It is an optional capability, asked for rather than inferred: a backend that
// owns nothing simply does not implement it, and that is an answer rather than a
// gap.
type processOwner interface {
	StopOwnedProcesses(ctx context.Context) error
}

// ownedBackendsFor lists the distinct transports this request's sessions belong
// to. A process is stopped once regardless of how many sessions it carried.
//
// A session that cannot be routed is REPORTED, not skipped. Dropping it would be
// the most expensive line in this file: a transport this engine does not hold may
// still be the one running the process, and a teardown that never asked would then
// report the workspace free while a writer is live in it. The engine may legitimately
// have no transport for a session whose binding it never admitted — that is a fact
// about ownership, and an unestablished owner blocks rather than passes.
func (e *Engine) ownedBackendsFor(requestID string) ([]runtime.Backend, []string) {
	st, err := e.store.State(requestID)
	if err != nil {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []runtime.Backend
	var unrouted []string
	ids := []string{st.WorkerSession, st.AuditorSession, st.CheckpointSession, st.DiagnosticSession}
	for _, binding := range st.BackendBindings {
		ids = append(ids, binding.SessionID)
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		backend, _, berr := e.dispatch.backendFor(requestID, id)
		if berr != nil {
			if e.retiredLocalACPSession(requestID, id) {
				continue
			}
			unrouted = append(unrouted, fmt.Sprintf("session %s could not be routed to a transport, so whether it owns a running process is unestablished: %v", id, berr))
			continue
		}
		if seen[backend.Name()] {
			continue
		}
		seen[backend.Name()] = true
		out = append(out, backend)
	}
	return out, unrouted
}

// ReconcileCancellation stops a request's owned effects and commits CANCELLED only
// once they are accounted for.
//
// It is safe to call repeatedly. A second call on an already-cancelled request
// reports the existing outcome rather than failing, because an operator retrying
// a cancellation is doing the right thing and should not have to guess why it
// failed.
func (e *Engine) ReconcileCancellation(ctx context.Context, requestID string) (CancelReport, error) {
	return e.reconcileCancellation(ctx, requestID, false)
}
func (e *Engine) reconcileCancellation(ctx context.Context, requestID string, automatic bool) (CancelReport, error) {
	var rep CancelReport
	rep.RequestID = requestID

	st, err := e.store.State(requestID)
	if err != nil {
		return rep, err
	}
	if st.Stage == StageCancelled {
		rep.Committed = true
		rep.Quiesced = true
		rep.Reason = "the request was already reconciled as CANCELLED"
		return rep, nil
	}
	if st.Stage.Terminal() {
		return rep, fmt.Errorf("agent-engine: request %s is already terminal as %s; its effects were never "+
			"reconciled, so it cannot be re-cancelled into a different outcome", requestID, st.ResultStatus)
	}
	if !st.CancellationRequested {
		return rep, fmt.Errorf("agent-engine: request %s has no recorded cancellation intent; "+
			"reconcile only follows a persisted intent, so nothing is stopped on a guess", requestID)
	}

	// Intent is durable before any effect. It is re-committed here because a
	// caller may have set it through a path that did not record the reason.
	if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
		cur.CancellationRequested = true
		if cur.RuntimeBlock == nil {
			cur.StageReason = "cancellation requested; reconciling owned effects"
		}
		if !automatic {
			cur.Cleanup = &CleanupAttempt{Deadline: e.now().Add(time.Minute).UTC().Format(time.RFC3339)}
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Stop new dispatch first, so nothing new is handed to a worker we are about
	// to stop.
	if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
		cur.Checkpoint.MutationGrantsPaused = true
		if cur.RuntimeBlock == nil {
			cur.CancellationReason = "dispatch paused pending reconciliation"
		}
		return nil
	}); err != nil {
		return rep, err
	}

	// Managed check jobs are signalled. Their transcripts are already durable, so
	// a killed job leaves a receipt, not a gap.
	for _, job := range st.Jobs {
		if job.Status == JobRunning || job.Status == JobPending {
			rep.JobsCancelled = append(rep.JobsCancelled, job.JobID)
		}
	}
	if len(rep.JobsCancelled) > 0 {
		e.logf("cancellation: %d managed job(s) outstanding: %v", len(rep.JobsCancelled), rep.JobsCancelled)
	}

	// Every owned effect is released through the ONE teardown path, which the
	// success, failure and timeout terminations also use.
	//
	// This used to be an inline copy of the stop-and-prove sequence. Keeping two
	// was not duplication for its own sake: they disagreed about what counts as
	// proof, and the disagreement was worth two writers on one workspace. There
	// is now one implementation of "stop it and then prove it", and this function
	// decides only what the outcome MEANS for a cancellation.
	ready, rerr := e.beginCleanupAttempt(requestID)
	if rerr != nil {
		return rep, rerr
	}
	if !ready {
		fresh, err := e.store.State(requestID)
		if err != nil {
			return rep, err
		}
		rep.Reason = fresh.StageReason
		if fresh.RuntimeBlock != nil {
			rep.Unresolved = []string{fresh.RuntimeBlock.Reason}
		}
		return rep, nil
	}
	term, terr := e.ReconcileTermination(ctx, requestID, TerminationCancelled)
	if terr != nil {
		return rep, terr
	}
	rep.Stopped = append(rep.Stopped, term.Stopped...)
	rep.Stale = append(rep.Stale, term.Stale...)
	rep.Unresolved = append(rep.Unresolved, term.Unresolved...)
	for _, pid := range term.Unreaped {
		rep.Unresolved = append(rep.Unresolved, fmt.Sprintf(
			"descendant process %d survived the transport teardown and is not proven reaped", pid))
	}
	rep.Quiesced = term.Quiesced

	if len(rep.Unresolved) > 0 {
		// The honest outcome is BLOCKED, not CANCELLED. A request that cannot
		// prove its writers stopped must not claim they did.
		rep.Reason = "owned effects could not be proven stopped: " + strings.Join(rep.Unresolved, "; ")
		if err := e.recordCleanupBlock(requestID, rep.Reason); err != nil {
			return rep, err
		}
		return rep, nil
	}

	// The result BODY becomes durable before the outcome claims it. Writing it
	// first is what makes the terminal commit atomic.
	//
	// The alternative — committing CANCELLED and publishing afterwards — is a trap:
	// PublishResult refuses a terminal request whose status is still empty, because
	// that is indistinguishable from an already-published outcome. So the body goes
	// first, its measured digest goes in the same transaction as the stage, and the
	// projection is derived afterwards.
	res := contract.Result{
		Status:  contract.StatusCancelled,
		Summary: "cancelled after every owned session was stopped and proven quiescent",
		Findings: []contract.Finding{{
			Severity: "info",
			Summary: fmt.Sprintf("stopped %d session(s); %d managed job(s) were outstanding at cancellation",
				len(rep.Stopped), len(rep.JobsCancelled)),
		}},
	}
	if err := e.store.writeResultBody(requestID, res); err != nil {
		return rep, err
	}
	bodyDigest, err := e.store.digestResultBody(requestID)
	if err != nil {
		return rep, err
	}

	// ONE transaction makes the whole outcome authoritative: the review that
	// granted cancellation, the terminal stage, and the digest of the body that
	// justifies it.
	committed, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
		if cur.Stage.Terminal() {
			return nil
		}
		cur.RuntimeBlock = nil
		cur.Cleanup = nil
		cur.ResultDigest = bodyDigest
		cur.ResultStatus = string(contract.StatusCancelled)
		cur.StageReason = "cancelled; all owned effects stopped and proven quiescent"
		return cur.transition(StageCancelled, cur.StageReason, e.now().UTC().Format(time.RFC3339))
	})
	if err != nil {
		return rep, err
	}
	rep.Committed = committed.Stage == StageCancelled
	if rep.Committed {
		// The projection is derived, never authoritative; a failure here leaves a
		// finished request whose result re-derives on the next read.
		_ = e.store.regenerateProjection(committed)
	}
	return rep, nil
}
