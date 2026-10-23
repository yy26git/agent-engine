package minimaxacp

// Cleanup: the ONE path by which an owned process stops existing.
//
// Everything here exists because a coding agent is a PROCESS, and a process does
// not stop existing because a session was closed. Three separate failures made
// that concrete:
//
//   - A handshake failure, a refused model selection and a supervisor death all
//     end with the same obligation — the child must be gone — and each used to
//     reach it through different code with different guarantees. The variant that
//     returned nil without checking whether the process had actually died was the
//     dangerous one, because it reported a clean teardown over a live writer.
//   - The previous teardown waited UNBOUNDED on the reap after SIGKILL. A child
//     wedged in an uninterruptible state therefore wedged shutdown with it, which
//     is the one thing a cancellation path must never do.
//   - Killing the process GROUP reaches the tools the agent started, but a tool
//     that called setsid has left that group, and a group probe cannot see it.
//     Claiming quiescence from a group probe alone is how a detached descendant
//     survives a "successful" cleanup.
//
// So there is one function, `terminateOwnedGroup`, that does the signalling; one
// method, `Connection.Teardown`, that every termination of a live connection goes
// through; and one result type that distinguishes closed, killed and FAILED. A
// failure is a value, never an absence: `TeardownFailed` is a distinct outcome so
// that no caller can accidentally read a failed teardown as a clean one.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// Teardown outcomes.
//
// The third one is the point of the type. `closed` and `killed` are both
// successful facts with different meanings; `failed` is the absence of a fact
// and must never be rendered as either of them.
const (
	// TeardownClosed means every session closed gracefully and the owned
	// process group was already gone.
	TeardownClosed = "closed"
	// TeardownKilled means the process group had to be signalled and was then
	// PROVEN gone.
	TeardownKilled = "killed"
	// TeardownFailed means the teardown ran and did not establish that the
	// process is gone.
	TeardownFailed = "failed"
	// TeardownNothing means there was no process to clean up, which is the one
	// non-failure that is not an achievement.
	TeardownNothing = "nothing-to-stop"
)

// TeardownResult is what a cleanup attempt actually proved.
//
// It exists so that "the child is gone" is a reportable fact rather than an
// inference from a function returning. Every field is inspectable because the
// operator's next question after any cancellation is "is something still running",
// and an answer that cannot be quoted is not an answer.
type TeardownResult struct {
	// Outcome is one of the Teardown* constants.
	Outcome string
	// Grace names why the signal path was needed: "" when the child exited on
	// TERM, "sigkill" when it did not.
	Grace string
	// Unreaped are descendant process ids that were observed before the signal
	// and were still alive after the owned group was gone.
	//
	// They are reported rather than claimed as reaped. A descendant that called
	// setsid leaves the group the signal targets, so the group probe is blind to
	// it — and the honest response to a process we cannot see is to name it, not
	// to assert quiescence we did not establish.
	Unreaped []int
	// Detail explains a failure in one line.
	Detail string
}

// Clean reports whether quiescence was established.
//
// A surviving detached descendant means quiescence was not established even
// when the direct child and its process group were released.
func (r TeardownResult) Clean() bool {
	return len(r.Unreaped) == 0 && (r.Outcome == TeardownClosed || r.Outcome == TeardownKilled)
}

// Err reports the failure as an error, so a caller that only knows how to check
// for an error still sees one.
func (r TeardownResult) Err() error {
	if r.Clean() || r.Outcome == TeardownNothing {
		return nil
	}
	if len(r.Unreaped) > 0 {
		return fmt.Errorf("minimax-acp: descendants remain alive: %v", r.Unreaped)
	}
	if r.Detail == "" {
		return errors.New("minimax-acp: the owned process could not be proven gone")
	}
	return errors.New("minimax-acp: " + r.Detail)
}

// signalFunc is the signalling seam. It exists so quiescence logic can be tested
// against an UNKNOWN probe result — "I could not tell whether it is gone" — which
// a real syscall never returns but which is exactly what a permission error or a
// racing pid reuse looks like from the outside.
type signalFunc func(pid int, sig syscall.Signal) error

// terminateOwnedGroup is the single implementation of owned-process termination.
//
// Three properties are load-bearing and all three were once missing:
//
//   - it signals the PROCESS GROUP, because a coding agent spawns tools and
//     killing only the parent orphans precisely what must not be orphaned;
//   - every wait is BOUNDED, so a wedged child delays cleanup by the grace and
//     then produces a failure instead of hanging the caller forever;
//   - it VERIFIES absence by probing rather than by trusting that a signal was
//     delivered, because delivery is a request and absence is the fact.
//
// Absence has TWO independent witnesses, and it keeps them apart. The DIRECT
// process is ours and its absence is unambiguous, but killing it orphans the
// tools it started; the PROCESS GROUP reaches those tools, but its probe can be
// declined — a sandbox or a raced pid reuse answers EPERM, which is the absence
// of evidence and never evidence of absence. Treating one probe as the whole
// proof is how this went wrong before: a group probe that transiently answered
// "cannot tell" turned a completed teardown into a reported failure.
//
// It is the body used both by `Connection.Teardown` and by the unrecorded-spawn
// abort, because "the process exists and nothing can name it" and "the process
// exists and is named" are the same termination problem. Two implementations of
// it would be two places for the escalation to be wrong.
func terminateOwnedGroup(cmd *exec.Cmd, grace time.Duration, signal signalFunc) (groupWitness, error) {
	if cmd == nil || cmd.Process == nil {
		return groupWitness{}, errors.New("no process to clean up")
	}
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { _, err := cmd.Process.Wait(); done <- err }()
	reaped := false
	var wit groupWitness

	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := signal(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			if perr := signal(pid, sig); perr != nil && !errors.Is(perr, syscall.ESRCH) {
				return wit, fmt.Errorf("process group signal failed: %v; direct process signal failed: %w", err, perr)
			}
		}
		deadline := time.NewTimer(grace)
		tick := time.NewTicker(20 * time.Millisecond)
		waitDone := (<-chan error)(done)
		if reaped {
			waitDone = nil
		}
		for {
			// The DIRECT process is the primary witness: it is ours, its absence
			// is unambiguous, and it is the thing cleanup was asked to release.
			directErr := signal(pid, 0)
			if directErr != nil && !errors.Is(directErr, syscall.ESRCH) {
				deadline.Stop()
				tick.Stop()
				return wit, fmt.Errorf("cannot establish process %d absence: %w", pid, directErr)
			}
			directGone := errors.Is(directErr, syscall.ESRCH)

			// The GROUP is a second witness, and the only one that reaches tools.
			// Its three answers stay distinct because collapsing them is what makes
			// a teardown lie: ESRCH is empty, nil is still populated, and anything
			// else means the kernel or a sandbox declined to say — which is not
			// evidence of emptiness and must not be recorded as one.
			groupErr := signal(-pid, 0)
			wit.groupInconclusive = groupErr != nil && !errors.Is(groupErr, syscall.ESRCH)
			wit.groupEstablished = errors.Is(groupErr, syscall.ESRCH)
			wit.groupStillAlive = groupErr == nil

			if !reaped {
				select {
				case waitErr := <-waitDone:
					if waitErr != nil {
						deadline.Stop()
						tick.Stop()
						return wit, fmt.Errorf("cannot reap spawned process %d: %w", pid, waitErr)
					}
					reaped = true
					continue
				case <-tick.C:
				case <-deadline.C:
					tick.Stop()
					goto nextSignal
				}
				continue
			}

			if directGone && wit.groupEstablished {
				deadline.Stop()
				tick.Stop()
				wit.directGone = true
				return wit, nil
			}
			if wit.groupEstablished && !directGone {
				// The group is empty but the direct child is not in it, which is
				// not a shape this cleanup may assert anything about.
				deadline.Stop()
				tick.Stop()
				return wit, fmt.Errorf("process group %d is empty but process %d was not found in it", pid, pid)
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				tick.Stop()
				goto nextSignal
			}
		}
	nextSignal:
	}
	if !wit.groupEstablished {
		// Both escalation rounds finished and the group never answered with
		// absence. The caller decides whether its own second witness can carry
		// the conclusion; falling out of the loop as success would not.
		return wit, nil
	}
	return wit, fmt.Errorf("process group %d quiescence not established after TERM/KILL cleanup", pid)
}

// groupWitness records what the probes were able to establish.
//
// It is returned rather than a bare error because two callers have genuinely
// different evidence available. The spawn abort has no descendant survey, so an
// INCONCLUSIVE group probe is the end of its proof and must fail. A live
// connection does have one, snapshotted before any signal, and may accept
// inconclusive-plus-empty-survey while SAYING so.
type groupWitness struct {
	// directGone means the owned process itself is provably absent and reaped.
	directGone bool
	// groupEstablished means the process GROUP positively reported empty.
	groupEstablished bool
	// groupInconclusive means the group probe could not answer. It is the
	// absence of evidence and is never counted as evidence of absence.
	groupInconclusive bool
	// groupStillAlive means the group positively reported that it still has
	// members. That is evidence of SURVIVORS, and it is a failure on its own:
	// a snapshot taken before the signal cannot prove a member that the snapshot
	// never saw.
	groupStillAlive bool
}

// proven reports whether the OWNED PROCESS is provably gone, which is a strictly
// weaker and more certain fact than an empty group.
func (w groupWitness) proven() bool { return w.directGone }

// provenWithoutGroup reports termination that established the direct process is
// gone while the group probe could not answer.
func (w groupWitness) provenWithoutGroup() bool {
	return w.directGone && !w.groupEstablished && w.groupInconclusive
}

// abortSpawn terminates a process that was created but could not be recorded.
//
// It is a thin adapter over the shared terminator. The distinction from
// `Teardown` is only in what the caller can do with the failure — here there is no
// Connection and no report, so the error is the whole answer.
func abortSpawn(cmd *exec.Cmd, grace time.Duration) error {
	wit, err := terminateOwnedGroup(cmd, grace, syscall.Kill)
	if err != nil {
		return err
	}
	if wit.groupEstablished {
		return nil
	}
	// With no descendant survey there is no second witness, so a group that
	// could not be shown empty is the end of the proof. Reporting success here
	// is exactly the claim this function exists to refuse.
	return fmt.Errorf("process group quiescence not established after TERM/KILL cleanup (inconclusive)")
}

// abortSpawnWithSignal is abortSpawn with the signalling seam exposed.
func abortSpawnWithSignal(cmd *exec.Cmd, grace time.Duration, signal signalFunc) error {
	wit, err := terminateOwnedGroup(cmd, grace, signal)
	if err != nil {
		return err
	}
	if wit.groupEstablished {
		return nil
	}
	return fmt.Errorf("process group quiescence not established after TERM/KILL cleanup (inconclusive)")
}

// Teardown stops everything this connection owns and reports what it proved.
//
// It is the ONE cleanup path for a live connection. A handshake failure, a
// refused selection, an operator cancellation and a supervisor shutdown all reach
// it, and reaching it more than once is safe: the second call returns the FIRST
// call's verdict rather than signalling a pid that may since have been reused.
//
// It is bounded by construction. The caller's context bounds the graceful phase,
// the grace bounds each escalation round, and nothing here waits on an unbounded
// channel receive — the previous implementation did, on the reap after SIGKILL,
// which let a wedged child wedge shutdown.
func (c *Connection) Teardown(ctx context.Context) TeardownResult {
	c.teardownOnce.Do(func() {
		c.teardownRes = c.runTeardown(ctx)
		close(c.teardownDone)
	})
	// A concurrent caller waits for the first teardown, but not forever: a
	// teardown in progress is bounded, so a bounded wait can only be a bug in
	// the teardown, and an unbounded one would reintroduce the hang.
	select {
	case <-c.teardownDone:
	case <-time.After(2 * DefaultShutdownGrace):
		return TeardownResult{
			Outcome: TeardownFailed,
			Detail:  "a concurrent teardown did not finish within the bound; its result is unknown, not clean",
		}
	}
	return c.teardownRes
}

// runTeardown is the single body of an owned connection's cleanup.
func (c *Connection) runTeardown(ctx context.Context) TeardownResult {
	grace := c.opts.ShutdownGrace
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	// The caller may already be cancelled — that is precisely when teardown
	// happens — so cleanup gets its OWN bounded budget rather than inheriting a
	// context that is already dead. Inheriting it is how a cancelled request ends
	// up never releasing its worker.
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	// Sessions are closed first, while the pipe still works. A graceful close
	// lets the server persist its own state; a process signal cannot.
	//
	// Snapshot under the lock, then release it. Holding c.mu across c.call would
	// self-deadlock — call registers its waiter under the same non-reentrant
	// mutex — and a shutdown that cannot complete is the worst possible place for
	// that bug to live.
	c.mu.Lock()
	ids := make([]string, 0, len(c.openSessions))
	for id := range c.openSessions {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	sort.Strings(ids)

	var sessionFailures []string
	for _, id := range ids {
		if _, err := c.call(gctx, "session/close", map[string]any{"sessionId": id}, grace); err != nil {
			// Fall through to the process signal. A server that will not close a
			// session must still not survive, and the session is recorded as
			// closed-by-force so liveness does not keep claiming it is open.
			sessionFailures = append(sessionFailures,
				fmt.Sprintf("session %s refused to close (%v) and was terminated with the process", id, err))
			continue
		}
		c.markSessionClosed(id)
	}

	c.mu.Lock()
	pid := 0
	if c.cmd != nil && c.cmd.Process != nil {
		pid = c.cmd.Process.Pid
	}
	c.mu.Unlock()
	if pid == 0 {
		// Nothing was ever spawned, so there is nothing that could still be
		// running. This is not an achievement and does not claim to be.
		return TeardownResult{Outcome: TeardownNothing, Detail: "no process was started on this connection"}
	}

	// Descendants are enumerated BEFORE any signal. After the group is gone the
	// tree is unresolvable: the parent is a zombie, its children are reparented,
	// and a process that called setsid is not in the group at all. A snapshot
	// taken afterwards would prove nothing, which is why the ordering is not
	// negotiable.
	desc := descendantsOf(pid)

	needKill := len(sessionFailures) > 0
	if !needKill {
		if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
			// Anything other than a positive "the group is gone" means the process
			// cannot be claimed released, so it is terminated.
			//
			// An INCONCLUSIVE probe resolves in the safe direction rather than
			// failing. Signalling a process that already exited is a no-op; not
			// signalling one that is still alive leaves a writer holding the
			// workspace, which is the outcome this whole path exists to prevent.
			// Refusing to act on "I could not tell" would be the safe-looking
			// choice and the dangerous one.
			needKill = true
		}
	}

	res := TeardownResult{Outcome: TeardownClosed}
	if len(sessionFailures) > 0 {
		// Not a failure — the process termination below ends those sessions too —
		// but it is operator-relevant, so it is said rather than dropped.
		res.Detail = strings.Join(sessionFailures, "; ")
	}
	if needKill {
		wit, err := terminateOwnedGroup(c.cmd, grace, c.signal())
		// The survey is taken BEFORE any signal and is therefore evidence about
		// processes the group signal could never have reached.
		left := waitForDescendants(desc, grace)
		res.Unreaped = left
		if err != nil {
			res.Outcome = TeardownFailed
			res.Detail = err.Error()
			return res
		}
		if !wit.proven() {
			res.Outcome = TeardownFailed
			res.Detail = "the owned process could not be proven gone after signalling its group"
			return res
		}
		if wit.groupStillAlive {
			// A group that positively answered "still populated" after both
			// escalation rounds is a member that survived. The descendant survey
			// cannot excuse it: the survey is a SNAPSHOT, and something that was
			// not in it can certainly be in the group.
			res.Outcome = TeardownFailed
			res.Detail = "the owned process group still has members after SIGKILL"
			return res
		}
		if wit.groupInconclusive && len(left) > 0 {
			// The group probe could not answer AND something survived. Two
			// witnesses failing is a fact, not a rounding error.
			res.Outcome = TeardownFailed
			res.Detail = fmt.Sprintf(
				"the process group could not be shown empty and descendant process(es) %v survived", left)
			return res
		}
		res.Outcome = TeardownKilled
		if wit.groupEstablished {
			res.Grace = "the process group was signalled and proven empty"
		} else {
			// SAID, not hidden: the group probe was inconclusive here, and the
			// conclusion rests on the direct process being gone plus a survivor
			// survey taken before any signal. An operator reading "clean" must
			// be able to see which witness carried it.
			res.Grace = "the process was proven gone, but the group could not be enumerated; " +
				"quiescence rests on a descendant survey taken before any signal"
		}
	}

	// The group is gone. Anything still alive from the pre-signal snapshot left
	// it — setsid, or a daemonise — and the group signal could not reach it.
	//
	// This is reported and NOT folded into the outcome: the owned process IS
	// gone, which is what the caller asked to prove, and something else on the
	// machine is not. Conflating them would either hide a surviving tool or
	// report a terminated process as a failed teardown; naming them separately
	// lets the operator see exactly which is which.
	if !needKill {
		res.Unreaped = waitForDescendants(desc, grace)
	}
	return res
}

// Detached stock guardians learn of parent death through IPC disconnect. Give
// them a bounded opportunity to finish before declaring a survivor. This only
// observes the pre-signal snapshot: it never signals a PID that may be reused.
func waitForDescendants(snapshot []int, grace time.Duration) []int {
	left := survivors(snapshot)
	if len(left) == 0 || grace <= 0 {
		return left
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			return survivors(left)
		case <-ticker.C:
			left = survivors(left)
			if len(left) == 0 {
				return nil
			}
		}
	}
}

// signal is how this connection terminates its process group.
//
// It is a method rather than a direct call to syscall.Kill for one reason: a
// group that survives SIGKILL is the failure this type exists to report, and it
// cannot be produced portably on demand. Keeping the seam per-connection rather
// than as a package variable means a test that installs one cannot affect a
// connection running alongside it.
func (c *Connection) signal() signalFunc {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminateWith != nil {
		return c.terminateWith
	}
	return syscall.Kill
}

// markSessionClosed records a session this connection ended on purpose.
func (c *Connection) markSessionClosed(id string) {
	c.mu.Lock()
	delete(c.openSessions, id)
	for label, sid := range c.openLabels {
		if sid == id {
			delete(c.openLabels, label)
		}
	}
	c.closedSessions[id] = true
	c.mu.Unlock()
}

// survivors filters a pre-signal descendant snapshot down to what is still alive.
func survivors(snapshot []int) []int {
	var out []int
	for _, pid := range snapshot {
		alive, err := processPresent(pid)
		if err != nil {
			// Unreadable is NOT absent. A process we cannot check has not been
			// proven reaped, so it is named rather than dropped.
			out = append(out, pid)
			continue
		}
		if alive {
			out = append(out, pid)
		}
	}
	sort.Ints(out)
	return out
}

// processPresent reports whether a process id still exists.
//
// EPERM means it exists and is not ours. Reading that as absent would let a
// cleanup claim quiescence over a process it never actually reached.
func processPresent(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, err
	}
}

// descendantsOf snapshots the process tree rooted at pid.
//
// It is bounded on purpose. An unbounded walk under a hostile or merely busy
// process tree is a hang, and this runs on the path that must never hang. A tree
// deeper than the bound is reported as truncated by omitting the deeper levels —
// which narrows the guarantee honestly instead of pretending to a complete one.
func descendantsOf(pid int) []int {
	type node struct{ self, parent int }
	out, err := exec.Command("ps", "-o", "pid=,ppid=", "-A").Output()
	if err != nil {
		// No snapshot means no claim about descendants. The teardown still proves
		// the group; it simply cannot name anything that left it.
		return nil
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		self, e1 := strconv.Atoi(fields[0])
		parent, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil {
			continue
		}
		children[parent] = append(children[parent], self)
	}

	const maxDescendantDepth = 8
	seen := map[int]bool{pid: true}
	var found []int
	frontier := []int{pid}
	for depth := 0; depth < maxDescendantDepth && len(frontier) > 0; depth++ {
		var next []int
		for _, parent := range frontier {
			for _, child := range children[parent] {
				if seen[child] {
					continue
				}
				seen[child] = true
				found = append(found, child)
				next = append(next, child)
			}
		}
		frontier = next
	}
	sort.Ints(found)
	return found
}

// SessionLiveness distinguishes the three states a session reference can be in.
//
// The conflation this exists to prevent is the expensive one. A session that
// finished cleanly and a session that should be running but is not both end as
// "not found" in every other representation, and a supervisor that cannot tell
// them apart either deletes a finished session or re-dispatches to a dead one.
type SessionLiveness string

const (
	// SessionActive means the session is open on a live connection.
	SessionActive SessionLiveness = "active"
	// SessionClosed means this connection closed the session on purpose and it
	// is in the closed record. It finished; nothing is owed to it.
	SessionClosed SessionLiveness = "closed"
	// SessionStale means the session is not open, it was never closed on
	// purpose, and the connection carrying it has ended. It SHOULD be running.
	// This is the state that needs recovery, and it is not the same as closed.
	SessionStale SessionLiveness = "stale"
	// SessionUnknown means this connection has no record of the session at all,
	// which is an absence of evidence rather than a finding.
	SessionUnknown SessionLiveness = "unknown"
)

// SessionLiveness classifies a session reference against this connection.
//
// The ORDER of these checks is the safety property, not an implementation detail.
// A session map entry outliving the connection is the stale case exactly — that
// is what a vanished peer leaves behind — so a live-looking entry must never
// outrank the fact that the connection carrying it has ended. Checking openness
// first reports a dead worker's session as running, which is the "looks healthy
// while nothing is there" failure this whole classification exists to remove.
func (c *Connection) SessionLiveness(ref runtime.SessionRef) SessionLiveness {
	c.mu.Lock()
	_, open := c.openSessions[ref.ID]
	closed := c.closedSessions[ref.ID]
	c.mu.Unlock()

	// Closed on purpose is a fact about the SESSION and outranks the connection's
	// fate: a session that finished is finished, whatever happened to the process
	// afterwards.
	if closed {
		return SessionClosed
	}
	select {
	case <-c.closed:
		if open {
			// It was open and the connection ended without closing it. That is the
			// definition of stale: something that should be running is not.
			return SessionStale
		}
		// Never opened here. The connection's death is not evidence about a
		// session it never made, so this stays an absence of evidence.
		return SessionUnknown
	default:
	}
	if open {
		return SessionActive
	}
	return SessionUnknown
}

// TerminateProbeProcess lets the capability probe use the canonical bounded
// owned-process termination instead of a second kill-and-wait implementation.
// The caller must have started cmd in its own process group.
func TerminateProbeProcess(cmd *exec.Cmd, grace time.Duration) (string, error) {
	if cmd == nil || cmd.Process == nil {
		return TeardownNothing, nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return TeardownFailed, err
	}
	if err == nil && pgid != cmd.Process.Pid {
		return TeardownFailed, errors.New("probe does not own its process group")
	}
	desc := descendantsOf(cmd.Process.Pid)
	witness, err := terminateOwnedGroup(cmd, grace, syscall.Kill)
	if err != nil {
		return TeardownFailed, err
	}
	if !witness.groupEstablished {
		return TeardownFailed, fmt.Errorf("probe process group absence is unproven")
	}
	if left := survivors(desc); len(left) > 0 {
		return TeardownFailed, fmt.Errorf("probe descendants remain alive: %v", left)
	}
	return TeardownKilled, nil
}
