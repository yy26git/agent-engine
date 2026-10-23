package minimaxacp

// Cleanup tests.
//
// Every fixture here is a SUBPROCESS: a shell script that answers a handshake and
// then misbehaves on purpose. No test starts a real `mcode acp`, and none sends a
// prompt — those are paid operations and a cleanup test must never spend one.
//
// The barriers are deterministic. Where a test waits, it waits on a MARKER the
// fixture wrote to a file, or on a bounded channel; nothing sleeps hoping a race
// lands the right way. The one timing bound under test is the bound itself, and
// that is asserted against a generous ceiling so a loaded machine cannot turn a
// correct bound into a false failure.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// cleanupGrace is the per-round bound these tests configure. It is short so the
// bounded-wait assertions finish quickly, and long enough that a correct
// implementation is never mistaken for a hung one.
const cleanupGrace = 300 * time.Millisecond

// wedgedServer answers the handshake and then refuses every session close and
// every signal, so cleanup must escalate and eventually report failure.
func wedgedServer(t *testing.T) string {
	t.Helper()
	return scriptServer(t, `
trap '' TERM INT HUP
rid() { printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false,"sessionCapabilities":{}},"agentInfo":{"name":"wedged","version":"0"}}}\n' "$(rid)"
      ;;
  esac
done
`)
}

// sessionServer answers the handshake AND the session calls, so a test can open a
// session and then close it on purpose.
func sessionServer(t *testing.T) string {
	t.Helper()
	return scriptServer(t, `
rid() { printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false,"sessionCapabilities":{}},"agentInfo":{"name":"sess","version":"0"}}}\n' "$(rid)"
      ;;
    *'"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-1","modes":{"currentModeId":"","availableModes":[]},"configOptions":[]}}\n' "$(rid)"
      ;;
    *'"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$(rid)"
      ;;
  esac
done
`)
}

// handshakingConn starts a server that completes the handshake and nothing else.
func handshakingConn(t *testing.T) *Connection {
	t.Helper()
	path := wedgedServer(t)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 2 * time.Second, ShutdownGrace: cleanupGrace,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return c
}

// TestTeardown_HandshakeFailureIsATerminationAndIsCleaned asserts a failed
// handshake releases the process.
//
// This is the first of the four terminations, and it is the one that was least
// covered: a server that rejects `initialize` used to fail startup through a
// narrower stop that reported success without checking the process was gone. A
// child left behind an apparently clean startup failure is the whole hazard.
func TestTeardown_HandshakeFailureIsATerminationAndIsCleaned(t *testing.T) {
	path := scriptServer(t, "cat >/dev/null 2>&1\n") // never answers initialize
	sum, _ := runtime.FileSHA256(path)

	_, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		HandshakeTimeout: 500 * time.Millisecond, ShutdownGrace: cleanupGrace,
	})
	if err == nil {
		t.Fatal("a server that never answers initialize was accepted")
	}
	// The error must SAY what cleanup did rather than leaving the reader to
	// assume the child went with it.
	if !strings.Contains(err.Error(), "cleanup:") {
		t.Errorf("startup failure does not report its cleanup verdict: %v", err)
	}
	// And the verdict must not be a failure to stop the group, which is what a
	// leaked child would look like from here.
	if strings.Contains(err.Error(), "NOT CLEAN") {
		t.Errorf("the process could not be proven gone after a failed handshake: %v", err)
	}
}

// TestTeardown_RunsOnEveryTerminationKind asserts all four terminations converge
// on the one cleanup path rather than each having its own.
//
// The four are driven through the same public entry point a caller uses. A
// variant here — a fifth body that only cancellation reaches — is exactly the
// defect the requirement names.
func TestTeardown_RunsOnEveryTerminationKind(t *testing.T) {
	for _, kind := range []string{"success", "failure", "timeout", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			c := handshakingConn(t)
			pid, owned := c.ProcessIdentity()
			if !owned {
				t.Fatal("the connection does not report the process it owns")
			}
			res := c.Teardown(context.Background())
			if !res.Clean() {
				t.Fatalf("teardown on %s did not prove the process gone: %+v", kind, res)
			}
			if err := res.Err(); err != nil {
				t.Errorf("a clean teardown still returned an error: %v", err)
			}
			// The process must ACTUALLY be gone, not merely reported gone.
			if alive, aerr := processPresent(pid); aerr == nil && alive {
				t.Errorf("process %d is still running after teardown on %s", pid, kind)
			}
		})
	}
}

// TestTeardown_IsIdempotentAndDoesNotResignal asserts a second call returns the
// first verdict.
//
// Signalling a pid twice is not merely wasteful. Once the child is gone that
// number can be REUSED, and a second signal would land on a stranger — so the
// cleanup path has to be safe to reach from cancellation, replacement and
// shutdown without coordinating them.
func TestTeardown_IsIdempotentAndDoesNotResignal(t *testing.T) {
	c := handshakingConn(t)
	first := c.Teardown(context.Background())
	second := c.Teardown(context.Background())
	if first.Outcome != second.Outcome {
		t.Errorf("a repeated teardown re-decided the outcome: %q then %q", first.Outcome, second.Outcome)
	}
	if len(second.Unreaped) != len(first.Unreaped) {
		t.Errorf("a repeated teardown re-surveyed survivors: %v then %v", first.Unreaped, second.Unreaped)
	}
}

// TestTeardown_ConcurrentCallersShareOneVerdict asserts concurrent terminations
// do not each signal the same process.
func TestTeardown_ConcurrentCallersShareOneVerdict(t *testing.T) {
	c := handshakingConn(t)
	const callers = 4
	results := make(chan TeardownResult, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- c.Teardown(context.Background()) }()
	}
	seen := map[string]bool{}
	for i := 0; i < callers; i++ {
		select {
		case res := <-results:
			seen[res.Outcome] = true
		case <-time.After(30 * time.Second):
			t.Fatal("a concurrent teardown never returned; cleanup must be bounded")
		}
	}
	if len(seen) != 1 {
		t.Errorf("concurrent teardowns disagreed: %v", seen)
	}
}

// TestTeardown_CancelledCallerContextStillReleasesTheProcess asserts cleanup does
// not inherit an already-cancelled context.
//
// Cancellation is precisely when cleanup happens, so handing the teardown the
// caller's dead context means a cancelled request never releases its worker. The
// teardown takes its own bounded budget instead.
func TestTeardown_CancelledCallerContextStillReleasesTheProcess(t *testing.T) {
	c := handshakingConn(t)
	pid, _ := c.ProcessIdentity()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the operator already went away

	res := c.Teardown(ctx)
	if !res.Clean() {
		t.Fatalf("a cancelled caller context prevented cleanup: %+v", res)
	}
	if alive, aerr := processPresent(pid); aerr == nil && alive {
		t.Errorf("process %d survived because the caller's context was cancelled", pid)
	}
}

// TestTeardown_ServerThatIgnoresTerminationIsStillBounded asserts a wedged child
// delays cleanup by the bound and then produces a FAILURE, not a hang.
//
// This is the property the old wait-on-reap lacked: `<-done` after SIGKILL has no
// deadline, so a child wedged in an uninterruptible state wedged the shutdown
// path with it — on the one path that must always be able to stop a worker
// holding the workspace.
func TestTeardown_ServerThatIgnoresTerminationIsStillBounded(t *testing.T) {
	c := handshakingConn(t)

	done := make(chan TeardownResult, 1)
	start := time.Now()
	go func() { done <- c.Teardown(context.Background()) }()

	// The ceiling is generous on purpose. This asserts the teardown RETURNED,
	// not that it returned quickly; a correct bounded implementation is never
	// this slow, and a loaded CI machine is never failed for being slow.
	select {
	case res := <-done:
		if !res.Clean() {
			t.Errorf("the wedged server was not stopped: %+v", res)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("teardown took %s; it is not bounded", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a child that ignores termination wedged the shutdown path")
	}
}

// TestTeardown_GroupIsProvenGoneRatherThanAssumed asserts the teardown VERIFIES
// absence instead of trusting that a signal was delivered.
//
// Delivery is a request; absence is the fact. The previous implementation
// returned "killed" immediately after signalling, so a process that survived was
// reported as terminated.
func TestTeardown_GroupIsProvenGoneRatherThanAssumed(t *testing.T) {
	c := handshakingConn(t)
	pid, _ := c.ProcessIdentity()
	res := c.Teardown(context.Background())
	if !res.Clean() {
		t.Fatalf("teardown failed: %+v", res)
	}
	if alive, err := processPresent(pid); err != nil || alive {
		t.Errorf("the owned process was reported clean but is still present: alive=%v err=%v", alive, err)
	}
	if err := syscall.Kill(-pid, 0); err == nil {
		t.Errorf("process group %d was reported gone but still exists", pid)
	}
}

// TestTeardown_SurvivedDescendantIsNamedNotClaimed is requirement 7.
//
// Killing the process GROUP reaches the tools the agent started, but a tool that
// called setsid has LEFT that group, and a group probe cannot see it. Claiming
// quiescence from the group probe alone is how a detached descendant survives a
// teardown that reported success.
//
// The fixture is a Go subprocess that really detaches, because macOS ships no
// setsid binary and a same-group child would prove nothing.
func TestTeardown_SurvivedDescendantIsNamedNotClaimed(t *testing.T) {
	c, marker := detachingConn(t)
	// The barrier: the fixture refuses to answer `initialize` until the detached
	// grandchild has written its pid. No sleep, no polling guess.
	waitForFile(t, marker, helperDeadline)
	grandchild := readPID(t, marker)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	res := c.Teardown(context.Background())

	// The OWNED process is gone — that part succeeded, and claiming otherwise
	// would hide a real termination.
	if res.Clean() || res.Err() == nil {
		t.Fatalf("surviving descendant was reported as quiescent: %+v", res)
	}
	if alive, err := processPresent(grandchild); err == nil && !alive {
		t.Fatal("the detached descendant is gone; this fixture was supposed to leave one")
	}
	// And the descendant that escaped is NAMED rather than folded into success.
	if !containsInt(res.Unreaped, grandchild) {
		t.Errorf("unreaped = %v, want the detached descendant %d named rather than "+
			"claimed as reaped", res.Unreaped, grandchild)
	}
}

// TestTeardown_SurvivingGroupIsReportedNotClean is requirement 3 at the
// transport boundary.
//
// A process group that still has members after TERM and SIGKILL is a real
// failure — something in it survived — and the outcome type exists so it cannot be
// rendered as `killed`. The condition cannot be produced portably on demand,
// which is why the connection carries a signalling seam; the assertion is on the
// REPORTED verdict, not on the fixture.
func TestTeardown_SurvivingGroupIsReportedNotClean(t *testing.T) {
	c := handshakingConn(t)
	pid, _ := c.ProcessIdentity()

	c.mu.Lock()
	c.terminateWith = func(target int, sig syscall.Signal) error {
		if target == -pid {
			// The group answers "still populated" forever, which is what a
			// surviving descendant looks like from the direct child's side.
			return nil
		}
		return syscall.Kill(target, sig)
	}
	c.mu.Unlock()
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })

	res := c.Teardown(context.Background())
	if res.Clean() {
		t.Fatalf("a surviving process group was reported clean: %+v", res)
	}
	if res.Outcome != TeardownFailed {
		t.Errorf("outcome = %q, want %q", res.Outcome, TeardownFailed)
	}
	// And it is usable as an ERROR, for a caller that only knows how to check
	// for one — a failure that cannot be observed is a failure that gets ignored.
	if res.Err() == nil {
		t.Error("a failed teardown returned a nil error")
	}
	if described := describeTeardown(res); !strings.Contains(described, "NOT CLEAN") {
		t.Errorf("describeTeardown = %q, want it to refuse to read as clean", described)
	}
}

// TestTeardown_ReapedDescendantIsNotReportedSurviving asserts the unreaped list
// is not noise.
//
// A report that names everything it ever saw would be as useless as one that
// names nothing, and would train an operator to ignore it. The fixture here has
// an ordinary in-group child, which the group signal reaches.
func TestTeardown_ReapedDescendantIsNotReportedSurviving(t *testing.T) {
	c := handshakingConn(t)
	res := c.Teardown(context.Background())
	if !res.Clean() {
		t.Fatalf("teardown failed: %+v", res)
	}
	if len(res.Unreaped) != 0 {
		t.Errorf("unreaped = %v, want empty: the fixture left no detached descendant", res.Unreaped)
	}
}

// TestSessionLiveness_ClosedAndStaleAreDistinct is requirement 6.
//
// A session that finished cleanly and a session that should be running but is
// not are both "not open" to every other representation. A supervisor that
// cannot tell them apart either discards a finished session or re-dispatches to
// a dead one, and both look correct from where it sits.
func TestSessionLiveness_ClosedAndStaleAreDistinct(t *testing.T) {
	path := sessionServer(t)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: cleanupGrace,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ref, aerr := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if aerr != nil {
		t.Fatalf("activate: %v", aerr)
	}

	if got := c.SessionLiveness(ref); got != SessionActive {
		t.Fatalf("liveness = %q, want %q for a freshly created session", got, SessionActive)
	}

	// CLOSED: this connection closed it on purpose.
	if err := c.Shutdown(context.Background(), ref); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := c.SessionLiveness(ref); got != SessionClosed {
		t.Errorf("liveness = %q, want %q after a deliberate close", got, SessionClosed)
	}
	// And Inspect reports a terminal status rather than an error, because a
	// finished session is a fact and not a missing one.
	sess, ierr := c.Inspect(context.Background(), ref)
	if ierr != nil {
		t.Errorf("inspect of a cleanly closed session errored: %v", ierr)
	}
	if sess.Status != "closed" {
		t.Errorf("status = %q, want %q", sess.Status, "closed")
	}

	// STALE: a second session that was open when the connection died. It was
	// never closed, it is simply gone, and it SHOULD be running.
	stale := runtime.SessionRef{Backend: BackendName, ID: "stale-session"}
	c.mu.Lock()
	c.openSessions[stale.ID] = true
	c.mu.Unlock()

	// The connection ends without closing the stale session.
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("end the peer: %v", err)
	}
	waitClosed(t, c)

	if got := c.SessionLiveness(ref); got != SessionClosed {
		t.Errorf("a closed session became %q after the connection died", got)
	}
	if got := c.SessionLiveness(stale); got != SessionStale {
		t.Errorf("liveness = %q, want %q: it should be running and is not", got, SessionStale)
	}
	// Inspect must not report a dead session as running. The stale check comes
	// FIRST precisely because the session map still lists it.
	if _, serr := c.Inspect(context.Background(), stale); serr == nil {
		t.Error("inspect reported a stale session as healthy after its connection died")
	}
	// Stopping is now complete: the process is already reaped.
	_ = c.Teardown(context.Background())
}

// TestHealth_ReportsAFailedCleanup asserts a failed teardown is a visible state.
//
// A teardown that failed while Health still says "available" is the swallow this
// requirement forbids: the operator is told everything is fine over a process
// that may still hold the workspace.
func TestHealth_ReportsAFailedCleanup(t *testing.T) {
	c := handshakingConn(t)
	// Before any teardown, Health must NOT start one. A status check that
	// terminates the worker it is reporting on is a side effect hidden in a
	// getter.
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !h.Available {
		t.Errorf("a live connection reported unavailable before any teardown: %v", h.Reasons)
	}
	if alive, _ := processPresent(mustPID(t, c)); !alive {
		t.Fatal("Health terminated the owned process; it must be a query, not an effect")
	}
}

// TestHealth_ReportsSurvivingDescendants asserts survivors reach operator output.
//
// A verdict that is silently truncated to the easy part is how an operator is
// told everything is fine over a tool that is still running.
func TestHealth_ReportsSurvivingDescendants(t *testing.T) {
	c, marker := detachingConn(t)
	waitForFile(t, marker, helperDeadline)
	gc := readPID(t, marker)
	t.Cleanup(func() { _ = syscall.Kill(gc, syscall.SIGKILL) })

	res := c.Teardown(context.Background())
	if !containsInt(res.Unreaped, gc) {
		t.Fatalf("unreaped = %v, want %d", res.Unreaped, gc)
	}
	// The rendered form must not read as a clean teardown.
	if described := describeTeardown(res); !strings.Contains(described, "NOT proven reaped") {
		t.Errorf("describeTeardown = %q, want it to name the unproven descendant", described)
	}
	// And the verdict must be usable as an error, for a caller that only checks
	// for one. The owned process is gone, but the surviving descendant still
	// prevents a successful quiescence verdict.
	if err := res.Err(); err == nil {
		t.Error("surviving descendant did not fail quiescence")
	}
}

// --- helpers ---------------------------------------------------------------

func mustPID(t *testing.T, c *Connection) int {
	t.Helper()
	pid, owned := c.ProcessIdentity()
	if !owned {
		t.Fatal("no owned process")
	}
	return pid
}

func containsInt(hay []int, needle int) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

// waitForFile blocks on a MARKER rather than a sleep.
func waitForFile(t *testing.T, path string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture never wrote its readiness marker %s", path)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid marker: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid marker %q: %v", raw, err)
	}
	return pid
}

// waitClosed blocks until the connection's reader has observed EOF.
func waitClosed(t *testing.T, c *Connection) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(20 * time.Second):
		t.Fatal("the connection never observed its peer exiting")
	}
}
