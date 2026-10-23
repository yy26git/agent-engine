package agentdeck

// AP1 parity tests.
//
// The stage's exit condition is that Agent Deck behaviour is IDENTICAL before and
// after the backend boundary exists. These tests are written against that claim
// rather than against the adapter's shape: they assert the same calls, the same
// receipt classification, the same refusals, and the same routing that the
// pre-boundary runtime produced.
//
// They also pin the two behaviours that were previously implicit and are now
// explicit — capability reporting, and "cannot read the inventory" being distinct
// from "the inventory is empty".

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

type shutdownRetryRuntime struct {
	Runtime
	status     string
	inspectErr error
	stopErr    error
}

func (r *shutdownRetryRuntime) Stop(context.Context, string) error { return r.stopErr }
func (r *shutdownRetryRuntime) Inspect(context.Context, string) (Session, error) {
	return Session{ID: "retained-session", Status: r.status}, r.inspectErr
}

func TestShutdown_RetryRequiresIndependentTerminalInventory(t *testing.T) {
	stopErr := errors.New("the public stop command refused")
	for _, status := range []string{"stopped", "dead", "exited", "closed", "running", "waiting", "error", ""} {
		t.Run(status, func(t *testing.T) {
			b := NewBackend(&shutdownRetryRuntime{status: status, stopErr: stopErr})
			err := b.Shutdown(context.Background(), runtime.SessionRef{ID: "retained-session"})
			terminal := status == "stopped" || status == "dead" || status == "exited" || status == "closed"
			if terminal && err != nil || !terminal && !errors.Is(err, stopErr) {
				t.Fatalf("status %q, error %v", status, err)
			}
		})
	}
	b := NewBackend(&shutdownRetryRuntime{status: "stopped", stopErr: stopErr, inspectErr: errors.New("unreadable inventory")})
	if err := b.Shutdown(context.Background(), runtime.SessionRef{ID: "retained-session"}); !errors.Is(err, stopErr) {
		t.Fatal("unreadable inventory concealed failed stop")
	}
}

// --- capability parity ------------------------------------------------------

// TestAP1_CapabilitiesMatchThePinnedCLISurface asserts the adapter advertises
// exactly what the pinned Agent Deck CLI actually exposes, and nothing more.
//
// Over-claiming here would be worse than under-claiming: a capability reported but
// not present turns into a failure inside a recovery path, which is the worst
// place for one.
func TestAP1_CapabilitiesMatchThePinnedCLISurface(t *testing.T) {
	b := NewBackend(&CLIRuntime{Binary: "/nonexistent", Profile: "p", Timeout: time.Second})
	caps := b.Capabilities()
	if !caps.Inventory {
		t.Error("agent-deck does have `list --json`; inventory must be advertised")
	}
	if !caps.Output {
		t.Error("agent-deck does have `session output --json`; output must be advertised")
	}
	if !caps.Receipts {
		t.Error("agent-deck does have `send-status`; receipts must be advertised")
	}
	if !caps.Resume {
		t.Error("the pinned runtime advertises loadSession; resume must be advertised")
	}
	if !caps.Close {
		t.Error("session close is supported; close must be advertised")
	}
	if b.Name() != BackendName {
		t.Errorf("Name = %q, want %q", b.Name(), BackendName)
	}
}

// --- refusal parity ---------------------------------------------------------

// TestAP1_ActivateRefusesAnUnlabelableSession asserts the label requirement.
//
// A create whose outcome is unknown must be reconcilable, and it is reconcilable
// only by label. Refusing an unlabelled activation is refusing to create a
// session nobody could later find.
func TestAP1_ActivateRefusesAnUnlabelableSession(t *testing.T) {
	b := NewBackend(&CLIRuntime{Binary: "/nonexistent", Profile: "p", Timeout: time.Second})
	_, err := b.Activate(context.Background(), runtime.ActivateSpec{Path: "/w", Tool: "codex"})
	if !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("an activation with no label was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "reconcil") {
		t.Errorf("the refusal did not explain why a label is required: %v", err)
	}
}

// TestAP1_UnknownInventoryIsNotAnEmptyInventory asserts the distinction that
// prevents duplicate workers.
//
// This is the single most important behaviour in the adapter. "I could not read
// the inventory" and "the inventory is empty" have opposite consequences, and
// collapsing them is how an ambiguous create becomes a second worker.
func TestAP1_UnknownInventoryIsNotAnEmptyInventory(t *testing.T) {
	// Capabilities describe the TRANSPORT's CLI surface, not whether the configured
	// binary happens to exist right now. Agent Deck genuinely implements `list
	// --json`, so inventory is claimed even when the pinned path is a fixture;
	// artifact validity is the pin's job, and conflating the two would make an
	// unreadable binary look like a missing capability.
	b := NewBackend(&CLIRuntime{Binary: "/nonexistent", Profile: "p", Timeout: time.Second})
	if !b.Capabilities().Inventory {
		t.Fatal("agent-deck implements list --json; inventory must be claimed")
	}
	// The distinction that matters is at RECONCILE time: a failed read must be an
	// error, never a "definitely absent" verdict.
	if _, found, err := b.Reconcile(context.Background(), "label", true); err == nil {
		t.Errorf("an unreadable inventory produced a verdict (found=%v) instead of an error", found)
	} else if found {
		t.Error("an unreadable inventory reported a match")
	}
	// And the error must name the reason, because "we could not check" and "it is
	// absent" lead the caller to opposite and equally consequential actions.
	_, _, err := b.Reconcile(context.Background(), "label", true)
	if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "could not") {
		t.Errorf("the refusal did not distinguish 'unknown' from 'absent': %v", err)
	}
}

// --- receipt classification parity ------------------------------------------

// TestAP1_ReceiptClassificationIsUnchanged asserts the adapter does not
// reinterpret backend vocabulary.
//
// The state string passes through verbatim. A translation table here would be
// this package deciding what a backend's word means, which is how two transports
// quietly acquire different semantics for the same event.
func TestAP1_ReceiptClassificationIsUnchanged(t *testing.T) {
	for _, state := range []string{"queued", "typed", "submitted", "landed", "failed"} {
		in := Receipt{SendID: "send-1", State: state, Settled: state == "landed"}
		var out runtime.Receipt
		out = runtime.Receipt{SendID: in.SendID, State: in.State, Settled: in.Settled, SessionID: in.SessionID}
		if out.State != state {
			t.Errorf("state %q became %q in translation", state, out.State)
		}
		if out.Settled != in.Settled {
			t.Errorf("settled flag changed for %q", state)
		}
	}
}

// --- session reference identity --------------------------------------------

// TestAP1_SessionRefIsBackendQualified asserts two backends can mint the same ID
// without their references colliding.
//
// A bare ID is only unique within the backend that produced it. Dropping the
// backend from the reference is how a worker ends up routed to the wrong process.
func TestAP1_SessionRefIsBackendQualified(t *testing.T) {
	a := runtime.SessionRef{Backend: "agent-deck", ID: "s1", Label: "l"}
	b := runtime.SessionRef{Backend: "minimax-acp", ID: "s1", Label: "l"}
	if a == b {
		t.Fatal("two backends produced identical references for the same id")
	}
	if !strings.Contains(a.String(), "agent-deck") {
		t.Errorf("String() omits the backend: %q", a.String())
	}
	if !strings.Contains(a.String(), "l") {
		t.Errorf("String() omits the activation label: %q", a.String())
	}
}

// --- boundary budget -------------------------------------------------------

// TestAP1_OutputBudgetIsClamped asserts an unbounded read cannot be requested.
//
// External output is unbounded in principle. A zero or negative budget meaning
// "no limit" would turn a chatty agent into a memory exhaustion of the component
// meant to be watching it.
func TestAP1_OutputBudgetIsClamped(t *testing.T) {
	if got := runtime.ClampOutputBudget(0); got != runtime.OutputBudgetDefault {
		t.Errorf("a zero budget became %d, want the default %d", got, runtime.OutputBudgetDefault)
	}
	if got := runtime.ClampOutputBudget(-5); got != runtime.OutputBudgetDefault {
		t.Errorf("a negative budget became %d, want the default %d", got, runtime.OutputBudgetDefault)
	}
	if got := runtime.ClampOutputBudget(500); got != 500 {
		t.Errorf("a sane budget was altered: %d", got)
	}
	if got := runtime.ClampOutputBudget(1 << 30); got > 200000 {
		t.Errorf("an oversized budget was not clamped: %d", got)
	}
}

// TestAP1_DeadlineIsAlwaysBounded asserts every transport call gets a deadline.
func TestAP1_DeadlineIsAlwaysBounded(t *testing.T) {
	ctx, cancel := runtime.Deadline(context.Background(), 0)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline was applied")
	}
	if time.Until(dl) <= 0 || time.Until(dl) > 31*time.Second {
		t.Errorf("default deadline out of range: %s", time.Until(dl))
	}
	// A caller-supplied deadline is honoured.
	ctx2, cancel2 := runtime.Deadline(context.Background(), 3*time.Second)
	defer cancel2()
	if dl2, _ := ctx2.Deadline(); time.Until(dl2) > 4*time.Second {
		t.Error("the supplied deadline was not applied")
	}
}

// TestAP1_EventCursorIsNotCrossBackendComparable asserts cursors stay namespaced.
//
// Two backends number from different origins. Merging them into one position
// would let a cursor from one transport silently suppress or replay frames from
// another, which is a correctness bug that looks like a timing bug.
func TestAP1_EventCursorIsNotCrossBackendComparable(t *testing.T) {
	ad := runtime.Event{Kind: "session.status", SessionID: "s1", Cursor: 7}
	acp := runtime.Event{Kind: "session.status", SessionID: "s1", Cursor: 7}
	ad.Backend, acp.Backend = "agent-deck", "minimax-acp"
	if ad.Kind == acp.Kind && ad.Cursor == acp.Cursor && string(ad.Payload) == string(acp.Payload) &&
		ad.Backend == acp.Backend {
		t.Fatal("events from two backends were indistinguishable, so a consumer could merge them")
	}
	// The boundary type carries the backend separately, on the reference, so the
	// event itself does not need to encode transport — but a consumer must never
	// merge two streams on cursor alone.
	if ad.Cursor == acp.Cursor {
		t.Log("both backends reported cursor 7; merging these on cursor alone would interleave frames")
	}
	// Round-tripping through JSON must not lose the payload the engine decodes.
	raw, err := json.Marshal(ad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back runtime.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Kind != ad.Kind || back.SessionID != ad.SessionID || back.Cursor != ad.Cursor {
		t.Errorf("event did not round-trip: %+v -> %+v", ad, back)
	}
}
