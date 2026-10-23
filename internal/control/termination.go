package control

// Termination: the ONE path by which a request releases what it owns.
//
// A request can end four ways — it passed, it failed, it ran out of time, or it
// was cancelled — and all four leave the same thing behind: a worker process
// that may still be writing the workspace. Before this file, cancellation had a
// cleanup body in `cancel.go` and replacement had a second, different one in
// `refresh.go`. They disagreed about what proves quiescence, which is the one
// question a teardown exists to answer, and they disagreed in a way that made
// replacement unusable against a process-owning transport: `refresh.go` stopped a
// SESSION and then demanded the PROCESS be dead, which for ACP is never true, so
// every replacement of a real ACP worker blocked forever on a condition that
// could not occur.
//
// Four requirements shape what follows.
//
// ONE PATH. `ReconcileTermination` is the only place a request's sessions and
// processes are stopped. Cancellation and replacement both call it with a
// different SCOPE — replacement must not kill the process that also carries the
// auditor, and a terminal outcome must not stop sessions it was never asked to
// stop — but neither has a body of its own. Success, failure and timeout reach
// the same body through `ReconcileTerminalOutcome`, which is the single seam a
// committed terminal stage passes on its way out. A termination that skips
// cleanup is therefore a bug rather than a variant.
//
// BOUNDED. Every wait here has a deadline. Cleanup runs on the cancellation path,
// which is exactly the path a wedged child must not be able to stall, and a
// teardown that hangs leaves the request looking cancelled while its worker keeps
// writing — the worst state available.
//
// REPORTED, NOT SWALLOWED. A failure is a value in this report and a line in the
// operator log, never a log-and-continue. `Quiesced` is the precondition for
// committing a terminal outcome, and a caller that ignores it has chosen to lie.
//
// DISTINGUISHABLE. A session that finished cleanly and a session that should be
// running but is not are reported differently. Both are "not open" to every other
// representation, and a supervisor that cannot tell them apart either discards a
// finished session or re-dispatches to a dead one.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/emjin/agent-engine/internal/runtime"
)

// TerminationKind names which of the four terminations is running.
//
// It is recorded rather than inferred from the stage because the stage and the
// cause can disagree — a request that timed out may have been recorded as
// BLOCKED, and a cleanup report that says "the failure path released the
// worker" is only true if it knows which path that was.
type TerminationKind string

const (
	// TerminationSuccess is a request that reached VERIFIED.
	TerminationSuccess TerminationKind = "success"
	// TerminationFailure is a request that reached FAILED.
	TerminationFailure TerminationKind = "failure"
	// TerminationTimeout is a request whose budgets were exhausted.
	TerminationTimeout TerminationKind = "timeout"
	// TerminationCancelled is a request whose cancellation was reconciled.
	TerminationCancelled TerminationKind = "cancellation"
	// TerminationReplacement is the REPLACEMENT protocol retiring one worker.
	//
	// It is not a fifth request-level outcome; it is the same teardown with a
	// narrower scope, and it is named rather than folded into one of the others
	// because it stops a SESSION while the request keeps running. Recording it
	// as a cancellation would say the request ended when it did not.
	TerminationReplacement TerminationKind = "replacement"
)

// Termination outcomes. `blocked` is a real outcome and is never a failure of
// this function — it means the cleanup ran and did not prove quiescence.
const (
	TerminationClean   = "clean"
	TerminationBlocked = "blocked"
)

// TerminationReport is what a teardown proved.
//
// Quiesced is the field everything else defers to. It is false whenever anything
// is unresolved, so a caller cannot read a partial teardown as a completed one
// by forgetting to check a list.
type TerminationReport struct {
	RequestID string
	Kind      TerminationKind
	// Outcome is TerminationClean or TerminationBlocked.
	Outcome string
	// Stopped are the sessions a stop was issued for AND whose quiescence was
	// afterwards observed.
	Stopped []string
	// Stale are sessions that should have been running and were not.
	//
	// They are quiescent for this teardown — nothing is writing — but they are
	// not clean, and folding them into Stopped would hide an ACTIVE session that
	// vanished. The caller decides whether that needs recovery.
	Stale []string
	// Unresolved are effects whose quiescence could NOT be proven.
	Unresolved []string
	// Unreaped are owned descendant processes a transport reported as not proven
	// reaped.
	Unreaped []int
	// Quiesced is true only when nothing is unresolved or unreaped.
	Quiesced bool
}

// Clean reports whether every owned effect was proven released.
func (r TerminationReport) Clean() bool { return r.Quiesced }

// Reason renders a refusal in one bounded line.
func (r TerminationReport) Reason() string {
	if r.Quiesced {
		return ""
	}
	parts := append([]string(nil), r.Unresolved...)
	if len(r.Unreaped) > 0 {
		parts = append(parts, fmt.Sprintf("descendant process(es) %v survived and are not proven reaped", r.Unreaped))
	}
	return strings.Join(parts, "; ")
}

// unreapReporter is a transport that can name processes it could not prove reaped.
//
// It is asked rather than inferred. The control layer cannot enumerate a
// transport's process tree, and a transport that CAN report a survivor is the
// only place that fact exists; having the control layer guess would mean either
// hiding a live tool or failing a teardown that actually succeeded.
type unreapReporter interface {
	UnreapedProcesses() []int
}

// livenessReporter is a transport that distinguishes a finished session from a
// vanished one.
//
// Optional, because most transports cannot tell. Where one can, its answer beats
// an inferred one: inferring "gone" from an error is how a stale session gets
// mistaken for a completed one.
type livenessReporter interface {
	SessionLiveness(ref runtime.SessionRef) string
}

// terminationScope names WHICH owned effects a teardown may touch.
//
// Replacement exists because it cannot use the full scope. One ACP process
// carries the worker, the auditor and the checker at once, so stopping the
// retired worker's PROCESS would stop the reviewer that is about to judge it. The
// scope is therefore explicit rather than implicit in the call site, because the
// difference between the two is a live-writer hazard and not a style choice.
type terminationScope struct {
	kind TerminationKind
	// sessions restricts the teardown to these ids. Empty means every session
	// the committed state records.
	sessions []string
	// stopSessions permits ending owned SESSIONS.
	//
	// It is separate from stopProcesses because a terminal outcome and a
	// cancellation need different things, and the difference is not a style
	// choice. The residue every termination leaves is an owned PROCESS that may
	// still be writing the workspace — that is what has to be released. A
	// session is not that residue: a transport that owns a process takes its
	// sessions down with it, and Agent Deck's sessions are explicitly
	// "supervised elsewhere" (`ProcessIdentity` returns owned=false precisely to
	// say so). Stopping them anyway would be a side effect on a request that
	// asked to be VERIFIED, and a plain Agent Deck request would stop its own
	// worker and auditor on the way out for no established reason.
	//
	// Cancellation is the exception because AP5 requires it: a cancelled request
	// must stop ALL Codex role sessions, not merely release the workspace.
	stopSessions bool
	// stopProcesses permits ending owned OS processes.
	stopProcesses bool
	// verifyProcesses requires owned OS processes to be PROVEN gone even when
	// they are not being stopped.
	//
	// The two must be separable. Cancellation ends a request, so nothing needs
	// the process afterwards and it is terminated. Replacement must NOT
	// terminate it: one ACP process carries the worker, the auditor and the
	// checker together, and killing it to retire the worker kills the reviewer
	// that is about to judge the replacement. It must still be CHECKED, because
	// a retired worker that is still writing is two writers on one workspace —
	// so the check is real and the kill is not.
	verifyProcesses bool
}

// ReconcileTermination stops everything a request owns and reports what it proved.
// This is the single cleanup path, and it is safe to call more than once: the
// work is idempotent and a repeated call re-observes rather than re-decides.
//
// CALLERS. Cancellation reaches it through `ReconcileCancellation`, the
// replacement protocol through `teardown` with a narrower scope, and the three
// terminal outcomes through `ReconcileTerminalOutcome`, which is the one place a
// committed terminal stage is observed on the way out. All four run this body;
// none of them has one of its own.
//
// SCOPE IS DERIVED FROM THE KIND, not chosen by the caller. A terminal outcome
// stops owned PROCESSES but not sessions; cancellation stops both; replacement
// stops neither the shared process nor anyone else's. The reason is the residue,
// and the residue is named above: all four leave a worker process that may still
// be writing. Sessions are not that residue — a transport owning a process takes
// its sessions with it, and Agent Deck owns no process at all (`ProcessIdentity`
// returns owned=false precisely to say its sessions are supervised elsewhere).
// Stopping a verified request's sessions anyway would be a side effect the
// request never asked for, and it would make reconciliation a gate on ordinary
// Agent Deck work that previously passed untouched.
//
// The scope is decided here, once, so it cannot drift between call sites. A
// caller that needs a different scope is asking for a different decision and
// belongs in `teardown`.
func (e *Engine) ReconcileTermination(ctx context.Context, requestID string, kind TerminationKind) (TerminationReport, error) {
	switch kind {
	case TerminationCancelled:
		// AP5 requires a cancelled request to stop ALL Codex role sessions as well
		// as release its workspace, so cancellation is the one kind with the wider
		// scope. A cancelled request is not going to be read again by anybody.
		return e.teardown(ctx, requestID, terminationScope{kind: kind, stopSessions: true, stopProcesses: true})
	default:
		return e.teardown(ctx, requestID, terminationScope{kind: kind, stopProcesses: true})
	}
}

// terminalKindFor names the termination a committed terminal stage represents.
//
// It is derived rather than passed, because the stage is the only thing a caller
// observes after the commit: by then it no longer knows which route committed it.
// The mapping is one-to-one, and each stage's meaning is already fixed in
// `state.go` — VERIFIED is the successful outcome, FAILED the disproved one,
// BLOCKED the exhausted-budget one, CANCELLED the reconciled cancellation. There
// is no stage that means two of them, and no fifth stage to map.
func terminalKindFor(stage Stage) (TerminationKind, bool) {
	switch stage {
	case StageVerified:
		return TerminationSuccess, true
	case StageFailed:
		return TerminationFailure, true
	case StageBlocked:
		return TerminationTimeout, true
	case StageCancelled:
		return TerminationCancelled, true
	}
	return "", false
}

// ReconcileTerminalOutcome releases what a request owns after its terminal stage
// is committed, and is the ONE place the three terminal outcomes reach cleanup.
//
// It exists as a separate entry from `ReconcileTermination` for one reason: a
// terminal stage is committed INSIDE a store transaction, and reconciliation
// cannot run there. Holding a store lock across a process signal is the rule this
// layer exists not to break. So success, failure and timeout are observed here,
// after the commit and before the caller is told the request finished.
//
// IDEMPOTENT BY OUTCOME, NOT BY CALL COUNT. A teardown that PROVED quiescence
// needs no second run: the work is done, the fact is durable, and repeating it
// would append a second `cleanup/<kind>` record that makes the history misreport
// how many teardowns a request had. That is also what stops CANCELLATION from
// reconciling twice — it proves quiescence before committing CANCELLED, so it
// arrives here having already recorded a clean teardown of its own.
//
// A teardown that did NOT prove quiescence is the other case, and it is the one
// this must never launder. An unresolved effect is still unresolved on the next
// tick: the process is still running, or the owner is still unknown. Skipping it
// because "cleanup already ran" would convert a reported failure into a clean
// result on the retry — turning a request whose writer is still live into one that
// reports as finished, which is the exact lie this path exists to prevent. So an
// unresolved cleanup is re-attempted and keeps being reported for as long as it is
// true.
//
// NOT A GATE. A request that owns nothing reconciles to a clean report with no
// effect and no refusal, which is what keeps ordinary Agent Deck work that used
// to pass untouched passing untouched. What this refuses is the opposite case:
// reporting clean over an owned process that could not be proven gone.
func (e *Engine) ReconcileTerminalOutcome(ctx context.Context, requestID string) (TerminationReport, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return TerminationReport{RequestID: requestID}, err
	}
	kind, terminal := terminalKindFor(st.Stage)
	if !terminal {
		// Not a terminal outcome; nothing to release. A caller that reaches this
		// before the commit is mid-request, and reconciling there would stop a
		// worker that is still supposed to be working.
		return TerminationReport{RequestID: requestID, Outcome: TerminationClean, Quiesced: true}, nil
	}
	// A prior CLEAN teardown is final: re-observing it would decide nothing and
	// would double the durable record.
	if cleanupProvedQuiescence(st, kind) {
		return TerminationReport{
			RequestID: requestID, Kind: kind,
			Outcome: TerminationClean, Quiesced: true,
		}, nil
	}
	return e.ReconcileTermination(ctx, requestID, kind)
}

// cleanupProvedQuiescence reports whether this request already recorded a
// teardown of the named kind that PROVED quiescence.
//
// It reads the durable history rather than an in-memory flag, because the thing
// it protects against is a CRASH between the commit and the next tick: an
// in-memory flag would be empty after a restart and the request would tear down
// a second time, and a state field would be a second place that has to agree with
// the stage it describes.
//
// It reads the recorded VERDICT, not merely the record's existence, because a
// recorded failure is not a finished teardown. `terminationNote` is the one place
// that renders the verdict, so its two forms are distinguishable without parsing
// prose for a pid.
// cleanupEvent names the durable record a teardown leaves.
func cleanupEvent(kind TerminationKind) string { return "cleanup/" + string(kind) }

// unprovenMarker is the phrase `terminationNote` puts in a teardown record that
// did NOT prove quiescence, and the ONLY phrase `cleanupProvedQuiescence` looks
// for.
//
// It is one constant used by both writer and reader because the reader's whole
// job is to tell a finished teardown from a recorded failure. A reader that
// stopped recognising it would begin treating a recorded failure as a finished
// teardown — the one mistake this idempotency guard must not be able to make. The
// phrase is part of a human-readable sentence, so it is deliberately more than a
// bare token: an operator reading the history must be able to tell the two apart
// without a parser.
const unprovenMarker = "did NOT prove quiescence"

// cleanupProvedQuiescence reports whether the named kind's teardown is recorded
// AND recorded as having proved quiescence.
//
// The LATEST record wins, because a teardown is re-attempted after a failure. A
// first-match read would let a long-standing failure be masked by a later success,
// or a success be overwritten in the reader's view by an earlier failure.
func cleanupProvedQuiescence(st *State, kind TerminationKind) bool {
	event := cleanupEvent(kind)
	found := false
	clean := false
	for _, h := range st.History {
		if h.Event != event {
			continue
		}
		found = true
		clean = !strings.Contains(h.Note, unprovenMarker)
	}
	return found && clean
}

// teardown is the one body of owned-effect cleanup.
func (e *Engine) teardown(ctx context.Context, requestID string, scope terminationScope) (TerminationReport, error) {
	rep := TerminationReport{RequestID: requestID, Kind: scope.kind, Outcome: TerminationClean}

	// State is read and the lock is RELEASED before any effect. A store lock held
	// across a process signal is a lock held across external I/O, which is the
	// rule this whole layer is built not to break: it would let a wedged child
	// block every writer, including the ones trying to record that it is wedged.
	st, err := e.store.State(requestID)
	if err != nil {
		return rep, err
	}

	// The scope decides WHICH sessions. An empty list means every session the
	// committed state records, and that default is deliberate: a teardown that
	// silently stopped nothing because a list was empty would be indistinguishable
	// from a request that owned nothing.
	//
	// The scope may also permit no sessions at all. A terminal outcome still has
	// to release what it owns, and a request with nothing owned reconciles to a
	// clean report with no effect — which is what keeps this from becoming a new
	// gate on work that used to be allowed.
	var sessions []string
	if scope.stopSessions {
		sessions = scope.sessions
		if len(sessions) == 0 {
			sessions = ownedSessions(st)
		}
	}
	for _, id := range sessions {
		e.stopSession(ctx, requestID, id, &rep)
	}

	if scope.stopProcesses || scope.verifyProcesses {
		e.reconcileOwnedProcesses(ctx, st, &rep, scope.stopProcesses)
	}

	rep.Quiesced = len(rep.Unresolved) == 0 && len(rep.Unreaped) == 0
	if rep.Quiesced {
		rep.Outcome = TerminationClean
	} else {
		rep.Outcome = TerminationBlocked
	}

	// The verdict is durable. Cleanup is not an event a reader can reconstruct
	// from the request's stage — a request that reached VERIFIED looks identical
	// whether or not its worker was ever stopped — so the fact is recorded rather
	// than returned to a caller that may be a one-shot operator command.
	if _, herr := e.store.UpdateCommitted(requestID, func(cur *State) error {
		cur.History = appendHistory(cur, cur.Stage, cleanupEvent(scope.kind),
			terminationNote(rep))
		return nil
	}); herr != nil {
		return rep, herr
	}
	if !rep.Quiesced {
		e.logf("cleanup %s for %s did not prove quiescence: %s", scope.kind, requestID, rep.Reason())
	}
	return rep, nil
}

// ownedSessions lists the distinct sessions a request committed to.
func ownedSessions(st *State) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	// Worker first: it is the session that writes, so it is stopped before the
	// readers that would want to keep running.
	add(st.WorkerSession)
	add(st.AuditorSession)
	add(st.CheckpointSession)
	add(st.DiagnosticSession)
	roles := make([]string, 0, len(st.BackendBindings))
	for role := range st.BackendBindings {
		roles = append(roles, string(role))
	}
	sort.Strings(roles)
	for _, role := range roles {
		add(st.BackendBindings[ProfileRole(role)].SessionID)
	}
	return out
}

// stopSession stops one session through the transport that OWNS it and then
// classifies what it can observe.
//
// Sending a shutdown to a backend that never made this session is how a live ACP
// worker keeps writing while the request reports itself stopped.
func (e *Engine) stopSession(ctx context.Context, requestID, id string, rep *TerminationReport) {
	backend, sref, err := e.dispatch.backendFor(requestID, id)
	if err != nil {
		if e.retiredLocalACPSession(requestID, id) {
			rep.Stale = append(rep.Stale, id)
			return
		}
		rep.Unresolved = append(rep.Unresolved,
			fmt.Sprintf("session %s could not be routed to a transport, so it was not stopped: %v", id, err))
		return
	}
	if serr := backend.Shutdown(ctx, sref); serr != nil {
		// A refused stop is unresolved, never success. Recording it as stopped
		// would let a live worker be counted as released.
		rep.Unresolved = append(rep.Unresolved,
			fmt.Sprintf("stop %s on %s: %v", id, backend.Name(), serr))
		return
	}

	switch e.classifySession(ctx, backend, sref) {
	case verdictClosed:
		rep.Stopped = append(rep.Stopped, id)
	case verdictStale:
		// Nothing is writing, so quiescence holds for this teardown — but a
		// session that should be running and is not is an ACTIVE session that
		// vanished, which is not the same fact as one that finished. It gets its
		// own list so a reader cannot mistake one for the other.
		rep.Stale = append(rep.Stale, id)
	case verdictActive:
		rep.Unresolved = append(rep.Unresolved,
			fmt.Sprintf("session %s still reports running on %s after stop", id, backend.Name()))
	default:
		// An Inspect that FAILS is unknown, not stopped. Treating an unreadable
		// session as a quiescent one would let an unreachable runtime convert "we
		// could not check" into "the worker is gone".
		rep.Unresolved = append(rep.Unresolved,
			fmt.Sprintf("session %s could not be inspected after stop, so it is not proven quiescent", id))
	}
}

// sessionVerdict is the four-way answer about one session after a stop.
type sessionVerdict string

const (
	// verdictClosed means it finished and nothing is owed to it.
	verdictClosed sessionVerdict = "closed"
	// verdictStale means it should be running and is not.
	verdictStale sessionVerdict = "stale"
	// verdictActive means it is still running.
	verdictActive sessionVerdict = "active"
	// verdictUnknown means it could not be established.
	verdictUnknown sessionVerdict = "unknown"
)

// classifySession decides which of those four a stopped session is.
//
// A transport that can tell CLOSED from STALE is believed over an inferred
// answer, because the inference — "Inspect returned an error, so it is gone" —
// is the exact reasoning this requirement forbids.
func (e *Engine) classifySession(ctx context.Context, backend runtime.Backend, ref runtime.SessionRef) sessionVerdict {
	if prover, ok := backend.(runtime.QuiescenceProver); ok {
		clear, err := prover.ProveQuiescence(ctx, ref)
		if err != nil {
			return verdictUnknown
		}
		if clear {
			return verdictClosed
		}
		return verdictActive
	}

	if r, ok := backend.(livenessReporter); ok {
		switch strings.ToLower(strings.TrimSpace(r.SessionLiveness(ref))) {
		case "active", "running":
			return verdictActive
		case "closed":
			return verdictClosed
		case "stale":
			return verdictStale
		}
	}
	sess, err := backend.Inspect(ctx, ref)
	if err != nil {
		return verdictUnknown
	}
	switch status := strings.ToLower(strings.TrimSpace(sess.Status)); status {
	case "":
		return verdictUnknown
	case "stopped", "dead", "exited", "error", "errored":
		// Terminal statuses, and an empty one from a runtime that does not report
		// status at all.
		return verdictClosed
	case "closed":
		return verdictClosed
	case "stale":
		return verdictStale
	default:
		return verdictActive
	}
}

// reconcileOwnedProcesses proves the OS processes this request owns are gone,
// terminating them first when the scope permits it.
//
// Sessions are not processes. A transport that owns a child may have failed to
// close a session — a bad id, a peer that lost it — while that process keeps
// writing the workspace. Closing sessions alone is therefore not enough to prove
// quiescence.
func (e *Engine) reconcileOwnedProcesses(ctx context.Context, st *State, rep *TerminationReport, stop bool) {
	backends, unrouted := e.ownedBackendsFor(st.RequestID)
	// Unroutable sessions are unresolved BEFORE any process is touched, so a
	// teardown can never report clean while an owner it could not name might
	// still be writing. Ordering matters: reporting them last would let a reader
	// see a stopped process and conclude the workspace is free.
	rep.Unresolved = append(rep.Unresolved, unrouted...)
	for _, backend := range backends {
		pid, owns := backend.ProcessIdentity()
		if !owns {
			continue
		}
		// Only a transport that CAN stop itself is asked, and only when the
		// scope permits stopping. A failure to do so is reported, never swallowed
		// — but it does not skip the liveness check below, because "I could not
		// stop it" and "it is gone" are different facts and only the second one
		// releases a workspace.
		if owner, ok := backend.(processOwner); ok && stop {
			if err := owner.StopOwnedProcesses(ctx); err != nil {
				rep.Unresolved = append(rep.Unresolved,
					fmt.Sprintf("owned process %d of %s could not be stopped: %v", pid, backend.Name(), err))
				continue
			}
			if r, has := backend.(unreapReporter); has {
				rep.Unreaped = append(rep.Unreaped, r.UnreapedProcesses()...)
			}
		}
		// Termination is a request; being gone is the fact. The wait inside the
		// transport expiring is not evidence, so liveness is confirmed here — and
		// by START TOKEN, not by pid, because a bare pid check on a number that
		// may since have been reused reads "still running" about a stranger.
		//
		// A transport with no stop capability of its own still reaches this line.
		// That is deliberate: the case where nothing could be stopped is exactly
		// the case where the request must stay BLOCKED rather than claim a
		// workspace nobody released.
		e.verifyOwnedProcessGone(st.RequestID, backend.Name(), pid, rep)
	}
	sort.Ints(rep.Unreaped)
}

// verifyOwnedProcessGone confirms an owned process is really gone.
//
// The start token is what makes the check safe against pid reuse: without it a
// bare liveness probe on a recycled number reports "still running" about a
// stranger forever, and a caller who relaxes that in order to make progress gets
// two writers on one workspace.
func (e *Engine) verifyOwnedProcessGone(requestID, backendName string, pid int, rep *TerminationReport) {
	token := e.startTokenFor(requestID, pid)
	alive, known := recordedProcessAlive(pid, token)
	switch {
	case alive:
		// Tested BEFORE `known` on purpose. A live pid without a start token is
		// still a process we can see occupying the number, and "still running" is
		// the stronger and more useful statement; demoting it to "could not be
		// identified" would understate a live writer.
		rep.Unresolved = append(rep.Unresolved, fmt.Sprintf(
			"owned process %d of %s is still running after being stopped", pid, backendName))
	case !known:
		rep.Unresolved = append(rep.Unresolved, fmt.Sprintf(
			"owned process %d of %s could not be identified after stop, so it is not proven gone",
			pid, backendName))
	default:
		rep.Stopped = append(rep.Stopped, fmt.Sprintf("%s process %d", backendName, pid))
	}
}

// startTokenFor finds the measured start token recorded for an owned process.
//
// It REUSES the identity captured when the process existed. A token measured
// now describes whatever holds the number today, which is exactly the process we
// must not signal — so re-measuring would defeat the purpose of recording one.
// An empty token is missing evidence, and is passed through as such.
func (e *Engine) startTokenFor(requestID string, pid int) string {
	st, err := e.store.State(requestID)
	if err != nil {
		return ""
	}
	for _, b := range st.BackendBindings {
		if b.ProcessPID == pid && b.ProcessStart != "" {
			return b.ProcessStart
		}
	}
	for _, rec := range st.OwnedProcesses {
		if rec.PID == pid && rec.StartToken != "" {
			return rec.StartToken
		}
	}
	return ""
}

// terminationNote renders a report as the bounded history line it is stored as.
//
// The two forms are distinguishable by `unprovenMarker`, because
// `cleanupProvedQuiescence` reads this to tell a finished teardown from a
// recorded failure — and getting that backwards turns an unreleased workspace
// into a clean-looking terminal state.
func terminationNote(rep TerminationReport) string {
	if rep.Quiesced {
		return fmt.Sprintf("cleanup on %s released %d effect(s); every owned effect was proven stopped",
			rep.Kind, len(rep.Stopped))
	}
	return fmt.Sprintf("cleanup on %s %s: %s", rep.Kind, unprovenMarker, rep.Reason())
}
