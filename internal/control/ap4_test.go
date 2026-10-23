package control

// AP4 activation-routing and dispatch tests.
//
// The claim under test is that an effect reaches the transport its SESSION is
// bound to, and that every case where that binding cannot be established ends in
// a refusal rather than a plausible guess. The second half is the half that
// matters: a message delivered to the wrong worker is not a degraded outcome,
// it is a wrong engineering conclusion delivered with full confidence.
//
// Every test here is about what does NOT happen.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/emjin/agent-engine/internal/runtime"
)

// fakeBackend is a second transport, standing in for the ACP worker.
//
// It exists to prove routing by BINDING rather than by which transport the
// dispatcher happened to be built with.
type fakeBackend struct {
	name string

	mu       sync.Mutex
	submits  []string
	askedFor []string
	// receipt is what Submit and Receipt return.
	receipt runtime.Receipt
	// sendErr is returned by Submit alongside receipt, if set.
	sendErr error
	// noQueue models a transport with no durable queue, like the ACP peer.
	noQueue bool
	// pid is the owned process id, when this backend owns one.
	pid int
	// labels are the activation labels this fake has been asked to activate, so
	// Reconcile can answer the way a real owned-process backend does.
	labels map[string]string
}

func (b *fakeBackend) Name() string { return b.name }

func (b *fakeBackend) Capabilities() runtime.Capabilities {
	caps := runtime.Capabilities{Inventory: true, Receipts: true, Output: true, DurableQueue: true}
	if b.noQueue {
		// Inventory is false too: a peer that cannot list sessions is also a peer
		// that retains nothing to reconcile against.
		caps.Inventory = false
		caps.DurableQueue = false
	}
	return caps
}

func (b *fakeBackend) Activate(_ context.Context, spec runtime.ActivateSpec) (runtime.SessionRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.labels == nil {
		b.labels = map[string]string{}
	}
	id := "acp-" + b.name
	if spec.Label != "" {
		id = "acp-session"
		b.labels[spec.Label] = id
	}
	return runtime.SessionRef{Backend: b.name, ID: id, Label: spec.Label}, nil
}

// Reconcile mirrors an owned-process backend: it knows the sessions it created
// and nothing else. A label it never activated is definitively absent, not
// unknown.
func (b *fakeBackend) Reconcile(_ context.Context, label string, priorQuiesced bool) (runtime.SessionRef, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.labels[label]; ok {
		return runtime.SessionRef{Backend: b.name, ID: id, Label: label}, true, nil
	}
	if !priorQuiesced {
		return runtime.SessionRef{}, false, fmt.Errorf("%w: no session carries label %q here and an earlier "+
			"process has not been proven gone", runtime.ErrUnsupported, label)
	}
	return runtime.SessionRef{Backend: b.name}, false, nil
}

func (b *fakeBackend) Inspect(context.Context, runtime.SessionRef) (runtime.Session, error) {
	return runtime.Session{ID: "acp-" + b.name}, nil
}

func (b *fakeBackend) Submit(_ context.Context, ref runtime.SessionRef, message string) (runtime.Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.submits = append(b.submits, ref.Backend+"|"+ref.ID+"|"+message)
	return b.receipt, b.sendErr
}

func (b *fakeBackend) Receipt(_ context.Context, sendID string) (runtime.Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.askedFor = append(b.askedFor, sendID)
	return b.receipt, nil
}

func (b *fakeBackend) Output(context.Context, runtime.SessionRef, int) (string, bool, error) {
	return "", false, nil
}

func (b *fakeBackend) Shutdown(context.Context, runtime.SessionRef) error { return nil }

func (b *fakeBackend) Events(context.Context, uint64) (<-chan runtime.Event, <-chan error, error) {
	out := make(chan runtime.Event)
	errs := make(chan error, 1)
	close(out)
	return out, errs, nil
}

func (b *fakeBackend) Health(context.Context) (runtime.Health, error) {
	return runtime.Health{Available: true}, nil
}

// pid lets a test model a backend that owns a real OS process.
func (b *fakeBackend) ProcessIdentity() (int, bool) { return b.pid, b.pid != 0 }

func (b *fakeBackend) submitted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.submits...)
}

func (b *fakeBackend) reconciled() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.askedFor...)
}

// compile-time proof the fake is a real backend, not a shape that happens to fit.
var _ runtime.Backend = (*fakeBackend)(nil)

// bindBackend records an explicit backend binding for a role.
func bindBackend(t *testing.T, f *fixture, role ProfileRole, backend, sessionID string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		s.BackendBindings[role] = SessionBinding{
			Backend: backend, SessionID: sessionID, Resolved: true, Generation: 1,
		}
		return nil
	}); err != nil {
		t.Fatalf("bind %s to %s: %v", role, backend, err)
	}
}

// seedEffect records one owed effect aimed at a session.
func seedEffect(t *testing.T, f *fixture, sessionID, message string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.RecordEffect(Effect{
			SessionID: sessionID, Role: "worker", Message: message,
			Reason: "test", CreatedAt: s.UpdatedAt,
		})
	}); err != nil {
		t.Fatalf("seed effect: %v", err)
	}
}

func effectState(t *testing.T, f *fixture) Effect {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(st.Effects) == 0 {
		t.Fatal("no effect recorded")
	}
	return st.Effects[0]
}

// TestDispatch_RoutesToTheBackendBoundToTheSession proves an effect is delivered
// through the transport its session is bound to, and NOT through the transport
// the dispatcher was constructed with.
func TestDispatch_RoutesToTheBackendBoundToTheSession(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{SendID: "acp-1", Outcome: runtime.OutcomeLanded}}

	// The worker moves to the ACP transport. The auditor stays on Agent Deck.
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedEffect(t, f, "acp-session-7", "implement the repair")

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (blocked=%d errors=%v)", res.Delivered, res.Blocked, res.Errors)
	}
	got := acp.submitted()
	if len(got) != 1 {
		t.Fatalf("acp submits = %v, want exactly one", got)
	}
	if !strings.HasPrefix(got[0], "minimax-acp|acp-session-7|") {
		t.Errorf("submit = %q, want it qualified by its backend and session", got[0])
	}
	if n := len(rt.sentMessages()); n != 0 {
		t.Errorf("agent deck received %d messages, want 0: routing ignored the binding", n)
	}
	if effectState(t, f).Status != EffectDelivered {
		t.Errorf("status = %q, want %q", effectState(t, f).Status, EffectDelivered)
	}
}

// TestDispatch_RefusesEffectForUnboundSession is the central refusal.
//
// An effect naming a session with no binding anywhere must NOT be delivered to
// Agent Deck merely because Agent Deck is the default. That is the exact shape of
// "route an unbound effect to a plausible default transport".
func TestDispatch_RefusesEffectForUnboundSession(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{SendID: "acp-1", Outcome: runtime.OutcomeLanded}}

	seedEffect(t, f, "session-that-was-never-bound", "implement the repair")

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Delivered != 0 || res.Failed != 0 || res.InFlight != 0 {
		t.Fatalf("an unbound effect was performed: %+v", res)
	}
	if res.Blocked != 1 {
		t.Fatalf("blocked = %d, want 1: an unrouteable effect must block", res.Blocked)
	}
	if n := len(rt.sentMessages()); n != 0 {
		t.Errorf("agent deck received %d messages, want 0", n)
	}
	if n := len(acp.submitted()); n != 0 {
		t.Errorf("acp received %d messages, want 0", n)
	}
	st := effectState(t, f)
	if st.Status != EffectBlocked {
		t.Errorf("status = %q, want %q", st.Status, EffectBlocked)
	}
	if !strings.Contains(st.LastError, "no recorded backend binding") {
		t.Errorf("LastError = %q, want it to name the missing binding", st.LastError)
	}
}

// TestDispatch_BlocksEffectForABackendTheEngineDoesNotHave proves a known
// binding is not enough: the named transport must actually be present.
//
// Sending anyway would mean the binding is documentation rather than a fact.
func TestDispatch_BlocksEffectForABackendTheEngineDoesNotHave(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"

	// Bound to a transport this engine was never given.
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedEffect(t, f, "acp-session-7", "implement the repair")

	d := NewDispatcher(f.store, rt) // no ACP backend registered
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Blocked != 1 || res.Delivered != 0 {
		t.Fatalf("result = %+v, want exactly one blocked effect", res)
	}
	if n := len(rt.sentMessages()); n != 0 {
		t.Errorf("agent deck received %d messages, want 0: a missing backend must not fall back", n)
	}
	if st := effectState(t, f); !strings.Contains(st.LastError, "does not have") {
		t.Errorf("LastError = %q, want it to name the absent backend", st.LastError)
	}
}

// TestReconcile_AsksTheBackendThatOwnsTheSession proves recovery is routed too.
//
// A send id is minted inside one transport. Asking another whether it arrived
// is not a weaker check, it is a check of the wrong system: a transport that has
// never seen the id would report it absent and retire a message that was
// actually delivered.
func TestReconcile_AsksTheBackendThatOwnsTheSession(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{SendID: "acp-1", Outcome: runtime.OutcomeLanded}}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordEffect(Effect{
			SessionID: "acp-session-7", Role: "worker", Message: "implement the repair",
			Reason: "test", CreatedAt: s.UpdatedAt,
		}); err != nil {
			return err
		}
		// A previous process claimed it and died before recording a receipt.
		s.Effects[0].Status = EffectDispatching
		s.Effects[0].SendID = "acp-send-9"
		return nil
	}); err != nil {
		t.Fatalf("seed effect: %v", err)
	}

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	unresolved, err := d.Reconcile(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %d, want 0", len(unresolved))
	}
	if got := acp.reconciled(); len(got) != 1 || got[0] != "acp-send-9" {
		t.Errorf("acp was asked about %v, want [acp-send-9]", got)
	}
	if st := effectState(t, f); st.Status != EffectDelivered {
		t.Errorf("status = %q, want %q", st.Status, EffectDelivered)
	}
}

// TestReconcile_BlocksUnroutableEffectRatherThanRetiringIt covers the recovery
// path's version of the same refusal: no binding means no proof, and no proof
// means blocked — never delivered, and never retried elsewhere.
func TestReconcile_BlocksUnroutableEffectRatherThanRetiringIt(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordEffect(Effect{
			SessionID: "session-that-was-never-bound", Role: "worker",
			Message: "implement the repair", Reason: "test", CreatedAt: s.UpdatedAt,
		}); err != nil {
			return err
		}
		s.Effects[0].Status = EffectDispatching
		s.Effects[0].SendID = "send-1"
		return nil
	}); err != nil {
		t.Fatalf("seed effect: %v", err)
	}

	d := NewDispatcher(f.store, rt)
	if _, err := d.Reconcile(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st := effectState(t, f)
	if st.Status != EffectBlocked {
		t.Errorf("status = %q, want %q", st.Status, EffectBlocked)
	}
	if n := len(rt.sentMessages()); n != 0 {
		t.Errorf("agent deck received %d messages, want 0", n)
	}
}

// TestDispatch_UnclassifiedReceiptIsNotDelivery proves an absent classification
// is not a success.
//
// A backend that returns a receipt without saying what it means has not proven
// delivery. Reading it as delivered would turn a transport that lost the
// vocabulary into a supervisor that claims everything worked.
func TestDispatch_UnclassifiedReceiptIsNotDelivery(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  runtime.Outcome
	}{
		{"unstated", runtime.OutcomeUnstated},
		{"in flight", runtime.OutcomeInFlight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := boundRequest(t, newFixture(t))
			rt := newFakeRuntime()
			acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{
				SendID: "acp-1", State: "something-new", Outcome: tc.out,
			}}
			bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
			seedEffect(t, f, "acp-session-7", "implement the repair")

			d := NewDispatcher(f.store, rt).WithBackend(acp)
			res, err := d.Drain(context.Background(), "forge-run-1842-repair")
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if res.Delivered != 0 {
				t.Fatalf("delivered = %d, want 0: %q is not proof of delivery", res.Delivered, tc.out)
			}
			if res.InFlight != 1 {
				t.Errorf("in flight = %d, want 1, so the effect stays reconcilable", res.InFlight)
			}
			if st := effectState(t, f); st.Status != EffectDispatching {
				t.Errorf("status = %q, want %q", st.Status, EffectDispatching)
			}
		})
	}
}

// TestDispatch_AmbiguousReceiptBlocks proves the vocabulary's hardest value is
// honoured. Ambiguity is neither success nor retryable failure.
func TestDispatch_AmbiguousReceiptBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{
		SendID: "acp-1", State: "settled", Settled: true, Outcome: runtime.OutcomeAmbiguous,
	}}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedEffect(t, f, "acp-session-7", "implement the repair")

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Blocked != 1 || res.Delivered != 0 {
		t.Fatalf("result = %+v, want one blocked effect", res)
	}
	if st := effectState(t, f); st.Status != EffectBlocked {
		t.Errorf("status = %q, want %q", st.Status, EffectBlocked)
	}
}

// TestDispatch_FailedReceiptKeepsTheBackendReason proves a failure an operator
// cannot read does not get retried on a guess.
func TestDispatch_FailedReceiptKeepsTheBackendReason(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{
		SendID: "acp-1", State: "refused", Outcome: runtime.OutcomeFailed,
		Reason: "the session rejected the prompt: model not permitted",
	}}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedEffect(t, f, "acp-session-7", "implement the repair")

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Failed != 1 || res.Delivered != 0 {
		t.Fatalf("result = %+v, want one failure", res)
	}
	st := effectState(t, f)
	if st.Status != EffectFailed {
		t.Errorf("status = %q, want %q", st.Status, EffectFailed)
	}
	if !strings.Contains(st.LastError, "model not permitted") {
		t.Errorf("LastError = %q, want the backend's own reason preserved", st.LastError)
	}
}

// TestDispatcher_EngineRegistersDeclaredBackends proves EngineOptions actually
// reaches the registry, so a declared transport is routable and an undeclared
// one is not.
func TestDispatcher_EngineRegistersDeclaredBackends(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	acp := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{SendID: "acp-1", Outcome: runtime.OutcomeLanded}}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedEffect(t, f, "acp-session-7", "implement the repair")

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Backends: []runtime.Backend{acp},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.dispatch.Drain(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := len(acp.submitted()); n != 1 {
		t.Errorf("acp submits = %d, want 1: a declared backend must be routable", n)
	}
	if st := effectState(t, f); st.Status != EffectDelivered {
		t.Errorf("status = %q, want %q", st.Status, EffectDelivered)
	}
}

// TestDispatcher_UnsupportedBackendIsNotSilentlyIgnored proves a nil or unusable
// backend cannot quietly shrink the routing table.
//
// A backend that does not implement the boundary is a programming error, and it
// must not be swallowed into "no backends registered", which would look
// identical to an unbound session.
func TestDispatcher_UnsupportedBackendIsNotSilentlyIgnored(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	d := NewDispatcher(f.store, newFakeRuntime())

	if d.WithBackend(nil) != d {
		t.Error("WithBackend(nil) must return the dispatcher unchanged")
	}
	if _, ok := d.backends[""]; ok {
		t.Error("a nil backend registered under an empty name")
	}
	if _, ok := d.backends["agent-deck"]; !ok {
		t.Error("agent deck must always be registered as the legacy default")
	}

	var _ runtime.Backend = (*errBackend)(nil)
	if d.WithBackend(&errBackend{}) == nil {
		t.Error("WithBackend must return the dispatcher for chaining")
	}
}

// errBackend is a backend whose capabilities are explicitly false, standing in
// for a transport that cannot do what is asked of it.
type errBackend struct{}

func (e *errBackend) Name() string { return "empty" }
func (e *errBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{}
}
func (e *errBackend) Activate(context.Context, runtime.ActivateSpec) (runtime.SessionRef, error) {
	return runtime.SessionRef{}, runtime.ErrUnsupported
}
func (e *errBackend) Reconcile(context.Context, string, bool) (runtime.SessionRef, bool, error) {
	return runtime.SessionRef{}, false, runtime.ErrUnsupported
}
func (e *errBackend) Inspect(context.Context, runtime.SessionRef) (runtime.Session, error) {
	return runtime.Session{}, runtime.ErrUnsupported
}
func (e *errBackend) Submit(context.Context, runtime.SessionRef, string) (runtime.Receipt, error) {
	return runtime.Receipt{}, runtime.ErrUnsupported
}
func (e *errBackend) Receipt(context.Context, string) (runtime.Receipt, error) {
	return runtime.Receipt{}, runtime.ErrUnsupported
}
func (e *errBackend) Output(context.Context, runtime.SessionRef, int) (string, bool, error) {
	return "", false, runtime.ErrUnsupported
}
func (e *errBackend) Shutdown(context.Context, runtime.SessionRef) error {
	return runtime.ErrUnsupported
}
func (e *errBackend) Events(context.Context, uint64) (<-chan runtime.Event, <-chan error, error) {
	return nil, nil, runtime.ErrUnsupported
}
func (e *errBackend) Health(context.Context) (runtime.Health, error) {
	return runtime.Health{Available: false, Reasons: []string{"empty"}}, nil
}

func (e *errBackend) ProcessIdentity() (int, bool) { return 0, false }

// TestDispatcher_SubmitErrorWithoutReceiptBlocksIsNotAFailure proves a transport
// error with no id is an ambiguity.
//
// Retrying is safe only for a KNOWN failure. This one has no id to reconcile, so
// the outcome is unknown and the effect blocks.
func TestDispatcher_SubmitErrorWithoutReceiptBlocksIsNotAFailure(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &fakeBackend{name: "minimax-acp", sendErr: errors.New("connection reset")}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedEffect(t, f, "acp-session-7", "implement the repair")

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0: an error with no receipt is not a known failure", res.Failed)
	}
	if res.Blocked != 1 {
		t.Errorf("blocked = %d, want 1", res.Blocked)
	}
	if st := effectState(t, f); st.Status != EffectBlocked {
		t.Errorf("status = %q, want %q", st.Status, EffectBlocked)
	}
}

// seedClaimedEffect records an effect a previous process claimed and died on,
// before any receipt existed. This is the interrupted turn.
func seedClaimedEffect(t *testing.T, f *fixture, sessionID, message string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordEffect(Effect{
			SessionID: sessionID, Role: "worker", Message: message,
			Reason: "test", CreatedAt: s.UpdatedAt,
		}); err != nil {
			return err
		}
		s.Effects[0].Status = EffectDispatching
		return nil
	}); err != nil {
		t.Fatalf("seed claimed effect: %v", err)
	}
}

// TestReconcile_InterruptedTurnOnQueueLessTransportBlocks is the interrupted
// unknown turn, and it is the case this whole stage exists for.
//
// A prompt was committed and the write may have reached the peer before the
// process died. Nothing can say which. Retrying is the one thing guaranteed to
// be wrong if it landed, so the effect blocks as durably UNKNOWN.
func TestReconcile_InterruptedTurnOnQueueLessTransportBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	acp := &fakeBackend{name: "minimax-acp", noQueue: true}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")
	seedClaimedEffect(t, f, "acp-session-7", "implement the repair")

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	if _, err := d.Reconcile(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st := effectState(t, f)
	if st.Status != EffectBlocked {
		t.Fatalf("status = %q, want %q", st.Status, EffectBlocked)
	}
	if !strings.Contains(st.LastError, "durably UNKNOWN") {
		t.Errorf("LastError = %q, want it to record the outcome as durably unknown", st.LastError)
	}

	// And it must not be resent by the next drain.
	res, err := d.Drain(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(acp.submitted()) != 0 {
		t.Errorf("acp received %d prompts, want 0: an uncertain turn must never be replayed", len(acp.submitted()))
	}
	if len(rt.sentMessages()) != 0 {
		t.Errorf("agent deck received %d messages, want 0", len(rt.sentMessages()))
	}
	if res.Delivered != 0 || res.InFlight != 0 || res.Failed != 0 || res.Blocked != 0 {
		t.Errorf("drain reported activity for a blocked effect: %+v", res)
	}
}

// Retention alone cannot identify a submission whose receipt was never saved.
func TestReconcile_InterruptedTurnOnDurableQueueBlocksWithoutReceipt(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	seedClaimedEffect(t, f, "sess-worker-1", "implement the repair")

	d := NewDispatcher(f.store, rt)
	unresolved, err := d.Reconcile(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %d, want 1", len(unresolved))
	}
	if st := effectState(t, f); st.Status != EffectBlocked {
		t.Errorf("status = %q, want UNKNOWN send blocked", st.Status)
	}
	if _, err := d.Drain(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatal(err)
	}
	if len(rt.sentMessages()) != 0 {
		t.Fatal("unknown send was replayed")
	}
}

func TestDispatch_RepeatedDrainReconcilesWithoutResubmitting(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	b := &fakeBackend{name: "minimax-acp", receipt: runtime.Receipt{SendID: "queued-1", Outcome: runtime.OutcomeInFlight}}
	bindBackend(t, f, RoleWorker, b.name, "acp-worker")
	seedEffect(t, f, "acp-worker", "perform exactly once")
	d := NewDispatcher(f.store, rt).WithBackend(b)
	for i := 0; i < 3; i++ {
		if _, err := d.Drain(context.Background(), "forge-run-1842-repair"); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.submitted()) != 1 || effectState(t, f).SendID != "queued-1" {
		t.Fatal("in-flight send was replaced or submitted twice")
	}
	b.receipt.Outcome = runtime.OutcomeLanded
	if _, err := d.Drain(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatal(err)
	}
	if len(b.submitted()) != 1 || effectState(t, f).Status != EffectDelivered {
		t.Fatal("landed receipt was not reconciled without another submit")
	}
}

func TestDispatch_CancellationPreventsPendingSubmission(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	seedEffect(t, f, "sess-worker-1", "must not execute")
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error { s.CancellationRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDispatcher(f.store, rt).Drain(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatal(err)
	}
	if len(rt.sentMessages()) != 0 {
		t.Fatal("cancelled request submitted new work")
	}
}

// TestReconcile_QueueLessTransportStillReconcilesCompletedWork proves the
// durable-queue rule is narrow: it restricts RETRY, not reconciliation.
//
// A completed ACP turn carries a receipt, so it resolves normally. Blocking it
// would mean a transport with no queue could never confirm anything.
func TestReconcile_QueueLessTransportStillReconcilesCompletedWork(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &fakeBackend{name: "minimax-acp", noQueue: true,
		receipt: runtime.Receipt{SendID: "acp:acp-session-7:1", Outcome: runtime.OutcomeLanded}}
	bindBackend(t, f, RoleWorker, "minimax-acp", "acp-session-7")

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordEffect(Effect{
			SessionID: "acp-session-7", Role: "worker", Message: "implement the repair",
			Reason: "test", CreatedAt: s.UpdatedAt,
		}); err != nil {
			return err
		}
		s.Effects[0].Status = EffectDispatching
		s.Effects[0].SendID = "acp:acp-session-7:1"
		return nil
	}); err != nil {
		t.Fatalf("seed effect: %v", err)
	}

	d := NewDispatcher(f.store, rt).WithBackend(acp)
	unresolved, err := d.Reconcile(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %d, want 0", len(unresolved))
	}
	if st := effectState(t, f); st.Status != EffectDelivered {
		t.Errorf("status = %q, want %q", st.Status, EffectDelivered)
	}
	if got := acp.reconciled(); len(got) != 1 || got[0] != "acp:acp-session-7:1" {
		t.Errorf("reconciled %v, want the acp receipt asked about", got)
	}
}

// activatingBackend records what a transport was asked to do.
type activatingBackend struct {
	fakeBackend
	activates []runtime.ActivateSpec
}

func (b *activatingBackend) Activate(_ context.Context, spec runtime.ActivateSpec) (runtime.SessionRef, error) {
	b.mu.Lock()
	b.activates = append(b.activates, spec)
	b.mu.Unlock()
	return b.fakeBackend.Activate(context.Background(), spec)
}

func (b *activatingBackend) activated() []runtime.ActivateSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runtime.ActivateSpec(nil), b.activates...)
}

// TestActivation_BranchesOnTheAdmittedTransport proves activation follows the
// profile's declared transport rather than the runtime the engine holds.
//
// Agent Deck is left untouched: if the ACP path leaked into it, a worker would
// be created on the wrong transport and the routing table would name a backend
// that never made the session.
func TestActivation_BranchesOnTheAdmittedTransport(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &activatingBackend{}
	acp.name = "minimax-acp"

	worker := Profile{Name: "acp-worker", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode"}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: worker.Ref()}
		grantActivationFixture(t, s, f.srcRoot, worker)
		return nil
	}); err != nil {
		t.Fatalf("pin worker profile: %v", err)
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Backends: []runtime.Backend{acp}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	res, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker,
		"acp-worker", f.srcRoot, "n1")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if !res.Created || res.Reconciled {
		t.Errorf("activation = %+v, want a fresh creation", res)
	}
	if n := len(rt.createdIDs); n != 0 {
		t.Errorf("agent deck created %d sessions, want 0: activation ignored the declared transport", n)
	}
	specs := acp.activated()
	if len(specs) != 1 {
		t.Fatalf("acp activations = %d, want 1", len(specs))
	}
	if specs[0].Label == "" || specs[0].Path != f.srcRoot {
		t.Errorf("activate spec = %+v, want the deterministic label and the workspace", specs[0])
	}

	// The binding must be backend-qualified, because that is what dispatch reads.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b, ok := st.BackendFor(RoleWorker)
	if !ok {
		t.Fatal("the worker has no backend binding, so its effects cannot be routed")
	}
	if b.Backend != "minimax-acp" {
		t.Errorf("binding backend = %q, want %q", b.Backend, "minimax-acp")
	}
	if b.SessionID != res.SessionID {
		t.Errorf("binding session = %q, want %q", b.SessionID, res.SessionID)
	}
	if !b.Resolved {
		t.Error("a binding that was never confirmed is a claim, not a binding")
	}
}

// TestActivation_RefusesTransportTheEngineDoesNotHave proves admission is not
// inferred from what is installed.
//
// Silently falling back to Agent Deck would run approved-for-ACP work on a
// transport nobody chose for it, and record a binding naming a backend that
// never created the session.
func TestActivation_RefusesTransportTheEngineDoesNotHave(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()

	worker := Profile{Name: "acp-worker", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode"}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: worker.Ref()}
		grantActivationFixture(t, s, f.srcRoot, worker)
		return nil
	}); err != nil {
		t.Fatalf("pin worker profile: %v", err)
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	_, err = eng.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker,
		"acp-worker", f.srcRoot, "n1")
	if err == nil {
		t.Fatal("launch succeeded without the declared transport")
	}
	if !strings.Contains(err.Error(), "does not have") {
		t.Errorf("error = %v, want it to name the absent transport", err)
	}
	if n := len(rt.createdIDs); n != 0 {
		t.Errorf("agent deck created %d sessions, want 0: refusal must not fall back", n)
	}
}

// TestActivation_ACPIsReconciledFromCommittedIntentNotInventory proves the ACP
// path does not borrow Agent Deck's inventory.
//
// A second activation for the same label adopts the committed binding instead of
// creating another session, and it does so without ever listing anything.
func TestActivation_ACPIsReconciledFromCommittedIntentNotInventory(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &activatingBackend{}
	acp.name = "minimax-acp"

	worker := Profile{Name: "acp-worker", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode"}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: worker.Ref()}
		grantActivationFixture(t, s, f.srcRoot, worker)
		return nil
	}); err != nil {
		t.Fatalf("pin worker profile: %v", err)
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Backends: []runtime.Backend{acp}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ctx := context.Background()
	first, err := eng.LaunchProfile(ctx, "forge-run-1842-repair", RoleWorker, "acp-worker", f.srcRoot, "same")
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	second, err := eng.LaunchProfile(ctx, "forge-run-1842-repair", RoleWorker, "acp-worker", f.srcRoot, "same")
	if err != nil {
		t.Fatalf("second launch: %v", err)
	}
	if !second.Reconciled {
		t.Error("the second activation created rather than reconciled")
	}
	if second.SessionID != first.SessionID {
		t.Errorf("second session = %q, want the reconciled %q", second.SessionID, first.SessionID)
	}
	if n := len(acp.activated()); n != 1 {
		t.Errorf("acp activations = %d, want 1: a duplicate session was created", n)
	}
}

// TestProfileRef_RoundTripPreservesTransportAndConfinement guards the pinned
// identity itself.
//
// Ref() pins the transport and the declared confinement. If reconstructing a
// Profile from that pinned identity drops either one, every session activated
// from a pin silently runs on Agent Deck with a weaker confinement than the
// operator approved — and nothing anywhere reports that, because both the
// binding and the activation would look successful.
func TestProfileRef_RoundTripPreservesTransportAndConfinement(t *testing.T) {
	original := Profile{
		Name: "acp-worker", Role: RoleWorker,
		Transport: TransportMiniMaxACP, Confinement: string(ConfinementWorktree),
		Tool: "mcode", Model: "m", Proven: true,
	}
	back := original.Ref().Profile()
	if back.Transport != TransportMiniMaxACP {
		t.Errorf("transport = %q, want %q: a pinned transport that does not survive the round trip is not pinned",
			back.Transport, TransportMiniMaxACP)
	}
	if back.Confinement != string(ConfinementWorktree) {
		t.Errorf("confinement = %q, want %q", back.Confinement, ConfinementWorktree)
	}
	if back.TransportFor() != TransportMiniMaxACP {
		t.Errorf("TransportFor = %q, want %q", back.TransportFor(), TransportMiniMaxACP)
	}

	// An unset transport must still default, or every pre-transport profile
	// would become invalid.
	if got := (Profile{Name: "legacy"}).Ref().Profile().TransportFor(); got != TransportAgentDeck {
		t.Errorf("legacy default = %q, want %q", got, TransportAgentDeck)
	}
}

// TestActivation_RefusesWhilePreviousProcessIsAlive is the orphaned-child case.
//
// A supervisor can die and leave its ACP child running. Nothing in the new
// process knows that except the recorded pid, and creating a session without
// checking it is how two agents end up writing one tree.
func TestActivation_RefusesWhilePreviousProcessIsAlive(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	// A live, foreign process that is not ours and must never be signalled.
	live, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	acp := &fakeBackend{name: "minimax-acp", pid: live.Pid}
	startToken := pidStartToken(live.Pid)
	if startToken == "" {
		t.Fatal("could not establish the live process identity required by this test")
	}

	worker := Profile{Name: "acp-worker", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode"}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: worker.Ref()}
		grantActivationFixture(t, s, f.srcRoot, worker)
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "old-session", Label: "stale-label", ProcessPID: live.Pid, ProcessStart: startToken, Resolved: true},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed prior binding: %v", err)
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Backends: []runtime.Backend{acp}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	_, err = eng.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker,
		"acp-worker", f.srcRoot, "n2")
	if err == nil {
		t.Fatal("launch succeeded while a previous process was still running")
	}
	if !strings.Contains(err.Error(), "not quiesced") {
		t.Errorf("error = %v, want it to refuse on quiescence", err)
	}
	if n := len(acp.labels); n != 0 {
		t.Errorf("a session was created despite the live previous process (%d labels)", n)
	}
}

// TestActivation_CreatesOncePreviousProcessIsGone is the other half: a dead pid
// must NOT block forever, or a crashed run could never be recovered.
func TestActivation_CreatesOncePreviousProcessIsGone(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	acp := &fakeBackend{name: "minimax-acp"}

	worker := Profile{Name: "acp-worker", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode"}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: worker.Ref()}
		grantActivationFixture(t, s, f.srcRoot, worker)
		s.BackendBindings = map[ProfileRole]SessionBinding{
			RoleWorker: {Backend: "minimax-acp", SessionID: "old-session", Label: "stale-label", ProcessPID: deadPID(t), Resolved: true},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed prior binding: %v", err)
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair",
		EngineOptions{Backends: []runtime.Backend{acp}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	res, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker,
		"acp-worker", f.srcRoot, "n3")
	if err != nil {
		t.Fatalf("a dead previous process blocked recovery: %v", err)
	}
	if !res.Created {
		t.Errorf("activation = %+v, want a fresh creation once the old process was gone", res)
	}
}

// deadPID returns a pid that is certain not to be running: a child of this
// process that has already been waited for.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run short-lived child: %v", err)
	}
	// Run waits for the child, so it is reaped and a later signal 0 reports
	// ESRCH. The pid is not reused within a test process in any way this depends
	// on: the check is "this pid is gone", and any other short-lived pid would
	// answer the same.
	return cmd.Process.Pid
}

// TestProcessAlive_TreatsPermissionDeniedAsAlive pins the asymmetry that matters.
//
// A process we may not signal is a process that is still holding whatever it
// holds. Reading EPERM as "gone" is the specific error that produces a duplicate
// worker on a workspace someone else owns.
func TestProcessAlive_TreatsPermissionDeniedAsAlive(t *testing.T) {
	if pid := os.Getpid(); pid <= 0 {
		t.Skip("no usable pid")
	} else if alive, err := processAlive(pid); !alive || err != nil {
		t.Errorf("processAlive(self) = %v, %v; want true, nil", alive, err)
	}
	if alive, err := processAlive(0); alive || err != nil {
		t.Errorf("processAlive(0) = %v, %v; want false, nil", alive, err)
	}
	if alive, err := processAlive(-1); alive || err != nil {
		t.Errorf("processAlive(-1) = %v, %v; want false, nil", alive, err)
	}
	if alive, err := processAlive(deadPID(t)); alive || err != nil {
		t.Errorf("processAlive(reaped) = %v, %v; want false, nil", alive, err)
	}
}
