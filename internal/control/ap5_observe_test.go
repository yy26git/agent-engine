package control

// AP5.6 multi-source supervision tests.
//
// The claim under test is that a second transport is now SUPERVISED — not merely
// recorded — while remaining structurally incapable of declaring an outcome.
//
// The tests below are written so that the dangerous regression fails loudly: one
// where a finished ACP turn is mistaken for finished work would look exactly like
// a system that is working correctly.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// emittingBackend is a backend whose observation stream a test drives directly.
type emittingBackend struct {
	fakeBackend
	mu       sync.Mutex
	stream   chan runtime.Event
	events   []runtime.Event
	followAt uint64
}

func newEmittingBackend(name string) *emittingBackend {
	return &emittingBackend{
		fakeBackend: fakeBackend{name: name},
		stream:      make(chan runtime.Event, 64),
	}
}

// Events serves whatever the test pushed, then holds the stream open.
//
// It records the position it was asked to resume from, which is how a test proves
// a restart continues rather than replaying.
func (b *emittingBackend) Events(_ context.Context, after uint64) (<-chan runtime.Event, <-chan error, error) {
	b.mu.Lock()
	b.followAt = after
	b.mu.Unlock()
	// The live stream is handed over directly. Serving a pre-seeded snapshot
	// instead would let a test "emit" into a channel nobody was reading, which
	// looks like a working follower and silently asserts nothing.
	errs := make(chan error, 1)
	return b.stream, errs, nil
}

// emit delivers one observation to the running follower.
func (b *emittingBackend) emit(ev runtime.Event) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.mu.Unlock()
	b.stream <- ev
}

// ev builds an ACP observation at a position.
func acpEv(cursor uint64, sessionID, sessionUpdate string) runtime.Event {
	return runtime.Event{
		Backend: "minimax-acp", Cursor: cursor, SessionID: sessionID,
		Kind: "session/update", Payload: acpUpdate(sessionUpdate),
	}
}

func (b *emittingBackend) resumedFrom() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.followAt
}

// acpWorker binds the request's worker to an emitting ACP transport.
func acpWorker(t *testing.T, f *fixture, b runtime.Backend, sessionID string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		s.BackendBindings[RoleWorker] = SessionBinding{
			Backend: b.Name(), SessionID: sessionID, ProcessPID: 0, Resolved: true,
		}
		s.WorkerSession = sessionID
		return nil
	}); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
}

func acpUpdate(sessionUpdate string) []byte {
	return []byte(`{"sessionId":"acp-1","update":{"sessionUpdate":"` + sessionUpdate + `"}}`)
}

// startEngineWithBackends builds an engine and starts following its backends,
// returning a stop function.
func startEngineWithBackends(t *testing.T, f *fixture, rt *fakeRuntime, backends ...runtime.Backend) (*Engine, func()) {
	t.Helper()
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once:     true,
		Now:      func() time.Time { return f.now },
		Backends: backends,
		Log:      func(string) {},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	marks := newObservationWatermarks()
	for _, b := range eng.followedBackends() {
		eng.followBackend(ctx, "forge-run-1842-repair", b, marks)
	}
	return eng, func() { cancel(); eng.Close() }
}

// TestSupervision_ACPActivityIsJournalled proves the second transport's
// observations reach durable storage at all.
//
// Before this, a live ACP worker produced nothing this layer could read.
func TestSupervision_ACPActivityIsJournalled(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	b := newEmittingBackend("minimax-acp")
	acpWorker(t, f, b, "acp-1")
	b.emit(runtime.Event{Backend: "minimax-acp", Cursor: 1, SessionID: "acp-1",
		Kind: "session/update", Payload: acpUpdate("tool_call")})

	eng, stop := startEngineWithBackends(t, f, rt, b)
	defer stop()
	waitFor(t, func() bool { return len(eng.ingest.Health()) > 0 })

	res, err := eng.journal.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Records) == 0 {
		t.Fatal("no ACP observation reached the journal")
	}
	if res.Records[0].Source.Backend != "minimax-acp" {
		t.Errorf("journalled source = %q, want minimax-acp", res.Records[0].Source.Backend)
	}
}

// TestSupervision_ACPActivityChargesTheClock is the point of the whole stage.
//
// A worker that is busy must accumulate active time, or a long turn produces no
// debt and no checkpoint review is ever scheduled — which is how a request going
// nowhere stays unnoticed.
func TestSupervision_ACPActivityChargesTheClock(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpWorker(t, f, b, "acp-1")

	t0 := f.now
	clock := &shiftingClock{at: t0}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), clock, b)
	defer stop()
	marks := newObservationWatermarks()
	ctx := context.Background()

	// The FIRST observation only establishes a watermark: an interval that starts
	// unobserved is not charged, because nothing is known about what happened
	// between the previous run and this frame.
	clock.set(t0.Add(10 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(1, "acp-1", "tool_call"), marks)
	if got := chargedSeconds(f); got != 0 {
		t.Fatalf("the first observation charged %ds; an interval that starts unobserved must not be billed", got)
	}

	// Now a real interval, bracketed by two observations.
	clock.set(t0.Add(40 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(2, "acp-1", "tool_call"), marks)
	if got := chargedSeconds(f); got < 30 {
		t.Fatalf("charged %ds after a 30s working interval, want at least 30", got)
	}
	clock.set(t0.Add(100 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(3, "acp-1", "tool_call"), marks)
	if got := chargedSeconds(f); got < 90 {
		t.Fatalf("charged %ds in total, want at least 90", got)
	}
}

// journalLen counts durable records, used as a "the follower ran" probe.
func journalLen(t *testing.T, eng *Engine) int {
	t.Helper()
	res, err := eng.journal.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return len(res.Records)
}

func chargedSeconds(f *fixture) int {
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		return 0
	}
	return int(st.Checkpoint.DebtSeconds)
}

// TestSupervision_AFinishedACPTurnOpensNoAudit is the most important assertion
// in this file.
//
// A completed turn means a conversation ended. Treating it as completed WORK is
// how a supervisor declares victory over an agent that only finished talking.
func TestSupervision_AFinishedACPTurnOpensNoAudit(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpWorker(t, f, b, "acp-1")

	clock := &shiftingClock{at: f.now}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), clock, b)
	defer stop()
	marks := newObservationWatermarks()
	ctx := context.Background()

	clock.set(f.now.Add(10 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(1, "acp-1", "agent_message_chunk"), marks)
	clock.set(f.now.Add(20 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, runtime.Event{
		Backend: "minimax-acp", Cursor: 2, SessionID: "acp-1", Kind: "session/update",
		Payload: []byte(`{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk"},"stopReason":"end_turn"}`),
	}, marks)

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.ActiveReview != nil {
		t.Errorf("a finished ACP turn opened a review: %+v", st.ActiveReview)
	}
	if len(st.Effects) != 0 {
		t.Errorf("a finished ACP turn recorded effects: %+v", st.Effects)
	}
	if st.AuditAttempts != 0 {
		t.Errorf("audit attempts = %d, want 0", st.AuditAttempts)
	}
	if st.Stage != StageWorking {
		t.Errorf("stage = %q, want %q: a finished turn is not a stage transition", st.Stage, StageWorking)
	}
}

// TestSupervision_OtherSessionsTrafficIsNotCharged keeps a reviewer's chatter out
// of the worker's clock.
//
// Inferring the role from a session id is how one session's activity becomes
// another's debt.
func TestSupervision_OtherSessionsTrafficIsNotCharged(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	b := newEmittingBackend("minimax-acp")
	acpWorker(t, f, b, "acp-1")

	clock := &shiftingClock{at: f.now}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), clock, b)
	defer stop()
	marks := newObservationWatermarks()
	ctx := context.Background()

	// The worker's own 80 seconds of work, bracketed by its own observations.
	clock.set(f.now.Add(10 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(1, "acp-1", "tool_call"), marks)
	clock.set(f.now.Add(90 * time.Second))
	eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(2, "acp-1", "tool_call"), marks)
	want := chargedSeconds(f)
	if want < 80 {
		t.Fatalf("the worker's own interval charged %ds, want at least 80", want)
	}

	// A great deal of traffic from sessions this request does not own.
	for c := uint64(3); c <= 8; c++ {
		clock.set(f.now.Add(time.Duration(100+c*30) * time.Second))
		eng.applyObservation(ctx, "forge-run-1842-repair", b, acpEv(c, "somebody-else", "tool_call"), marks)
	}

	if got := chargedSeconds(f); got != want {
		t.Errorf("another session's traffic moved the worker's debt from %d to %d", want, got)
	}
}

// engineWithClock builds an engine with a movable clock and no live followers.
func engineWithClock(t *testing.T, f *fixture, rt *fakeRuntime, clock *shiftingClock, backends ...runtime.Backend) (*Engine, func()) {
	t.Helper()
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Now: clock.now, Backends: backends, Log: func(string) {},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng, func() { eng.Close() }
}

// TestSupervision_ResumePositionContinuesRatherThanReplays proves a restart does
// not re-charge history.
//
// Replaying a whole stream into the clock would bill the request for work that
// happened before this run existed.
func TestSupervision_ResumePositionContinuesRatherThanReplays(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	b := newEmittingBackend("minimax-acp")
	acpWorker(t, f, b, "acp-1")

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.IngestedPositions = map[string]uint64{"minimax-acp": 77}
		return nil
	}); err != nil {
		t.Fatalf("seed position: %v", err)
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{Once: true})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.followBackend(ctx, "forge-run-1842-repair", b, newObservationWatermarks())

	if got := b.resumedFrom(); got != 77 {
		t.Errorf("resumed from %d, want 77", got)
	}
}

func startEngineWithClock(t *testing.T, f *fixture, rt *fakeRuntime, clock *shiftingClock, backends ...runtime.Backend) (*Engine, func()) {
	t.Helper()
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Now: clock.now, Backends: backends, Log: func(string) {},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	marks := newObservationWatermarks()
	for _, b := range eng.followedBackends() {
		eng.followBackend(ctx, "forge-run-1842-repair", b, marks)
	}
	return eng, func() { cancel(); eng.Close() }
}

// shiftingClock is a movable clock, so an interval of "work" can be observed
// without the test sleeping.
type shiftingClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *shiftingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *shiftingClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

// waitFor polls a condition, because the follower runs on its own goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the observation to be applied")
}
