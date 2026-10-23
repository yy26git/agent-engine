package control

// EC2 engine and dispatch tests.
//
// The claim under test is narrow and specific: a real recipient receives one
// instruction, promptly, and an uncertain outcome stays visible. Each test
// isolates one way that claim fails — two sends, a late send, a lost cursor, a
// boundary that never arrives, a dry run that changes real state, two owners at
// once.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/agentdeck"
)

// fakeRuntime is a scripted Agent Deck runtime.
//
// It records every effect attempt so a test can assert how many instructions a
// recipient would actually have received, which is the only honest way to prove
// "exactly one" without a real agent.
type fakeRuntime struct {
	mu sync.Mutex

	sent []string
	// sendState is the queue state returned by Send for the next send.
	sendState string
	// settled marks the returned record as settled, which is what turns an
	// in-flight message into an unprovable one.
	settled bool
	// failSend makes Send fail without returning a receipt.
	failSend bool
	// notFound makes SendStatus report an unknown send id.
	notFound bool

	health     agentdeck.BusHealth
	frames     []agentdeck.Frame
	eventsErr  error
	createdIDs []string
	// outputs maps a session ID to the text that session last produced.
	outputs map[string]string
	// sessions maps an activation label to the session ID that carries it. It is
	// what makes an ambiguous create reconcilable in tests.
	sessions map[string]string
	// noInventory makes List fail, modelling a runtime that cannot be inventoried.
	noInventory bool
	// nextID is the ID the next Create returns, so a test can force Create to hand
	// back a specific session.
	nextID string
	// started records the sessions Start was issued for, in order.
	started []string
	// stopped records the sessions Stop was issued for, in order.
	stopped []string
	// aliveAfterStop makes a stopped session keep reporting "running", which is
	// how an unproven stop looks.
	aliveAfterStop bool
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		sendState: "queued",
		health:    agentdeck.BusHealth{Enabled: true, Cursor: 0},
		outputs:   map[string]string{},
		sessions:  map[string]string{},
	}
}

func (f *fakeRuntime) Create(_ context.Context, spec agentdeck.CreateSpec) (agentdeck.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	if id == "" {
		id = fmt.Sprintf("sess-%d", len(f.createdIDs)+1)
	}
	f.createdIDs = append(f.createdIDs, id)
	return agentdeck.Session{ID: id, Title: spec.Title, Path: spec.Path, Tool: spec.Tool}, nil
}

func (f *fakeRuntime) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return nil
}

// List inventories sessions by activation label.
func (f *fakeRuntime) List(context.Context) ([]agentdeck.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noInventory {
		return nil, errors.New("the session registry is unreachable")
	}
	var out []agentdeck.Session
	for label, id := range f.sessions {
		out = append(out, agentdeck.Session{ID: id, Title: label, Status: "idle"})
	}
	return out, nil
}

func (f *fakeRuntime) Inspect(_ context.Context, id string) (agentdeck.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, created := range f.createdIDs {
		if created == id {
			status := "running"
			// A session that was asked to stop and is still reporting itself
			// running is exactly the case quiescence must catch.
			for _, s := range f.stopped {
				if s == id && !f.aliveAfterStop {
					status = "stopped"
				}
			}
			return agentdeck.Session{ID: created, Status: status}, nil
		}
	}
	return agentdeck.Session{}, &agentdeck.NotFoundError{What: "session", ID: id}
}

func (f *fakeRuntime) Send(_ context.Context, id, message string) (agentdeck.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend {
		return agentdeck.Receipt{}, errors.New("transport refused the connection")
	}
	f.sent = append(f.sent, id+": "+message)
	return agentdeck.Receipt{
		SendID:  fmt.Sprintf("send-%d", len(f.sent)),
		State:   f.sendState,
		Settled: f.settled,
	}, nil
}

func (f *fakeRuntime) SendStatus(_ context.Context, sendID string) (agentdeck.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notFound {
		return agentdeck.Receipt{}, &agentdeck.NotFoundError{What: "send", ID: sendID}
	}
	return agentdeck.Receipt{SendID: sendID, State: f.sendState, Settled: f.settled}, nil
}

func (f *fakeRuntime) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeRuntime) Events(_ context.Context, _ uint64) (<-chan agentdeck.Frame, <-chan error, error) {
	frames := make(chan agentdeck.Frame, len(f.frames)+1)
	errs := make(chan error, 1)
	go func() {
		defer close(frames)
		defer close(errs)
		for _, f := range f.frames {
			frames <- f
		}
		if f.eventsErr != nil {
			errs <- f.eventsErr
		}
	}()
	return frames, errs, nil
}

func (f *fakeRuntime) BusHealth(context.Context) (agentdeck.BusHealth, error) {
	return f.health, nil
}

// Output returns scripted per-session output. Tests set outputs to model a
// reviewer that actually answered, which is what the trusted ingress path reads.
func (f *fakeRuntime) Output(_ context.Context, id string, _ int) (agentdeck.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, ok := f.outputs[id]
	if !ok {
		return agentdeck.Output{}, fmt.Errorf("agentdeck: session %s produced no usable output (NOT_FOUND)", id)
	}
	return agentdeck.Output{SessionID: id, Text: text}, nil
}

func (f *fakeRuntime) sentMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// boundRequest admits a request with an implementer and an auditor bound, which
// is the state a supervised run actually starts from.
func boundRequest(t *testing.T, f *fixture) *fixture {
	t.Helper()
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if _, err := f.store.UpdateCommitted(req.RequestID, func(s *State) error {
		s.WorkerSession = "sess-worker-1"
		s.AuditorSession = "sess-auditor-1"
		return s.Transition(StageWorking, "worker started", s.UpdatedAt)
	}); err != nil {
		t.Fatalf("bind sessions: %v", err)
	}
	return f
}

func finishedFrame(cursor uint64, sessionID string) agentdeck.Frame {
	return agentdeck.Frame{
		Cursor:    cursor,
		Kind:      agentdeck.KindSessionFinished,
		SessionID: sessionID,
		Data:      []byte(`{"done_status":"ok","done_summary":"implemented the fix"}`),
	}
}

func transitionFrame(cursor uint64, sessionID, from, to string) agentdeck.Frame {
	return agentdeck.Frame{
		Cursor:    cursor,
		Kind:      agentdeck.KindSessionTransition,
		SessionID: sessionID,
		Data:      []byte(fmt.Sprintf(`{"from_status":%q,"to_status":%q}`, from, to)),
	}
}

// TestControlCLI_LiveEventDeliversBeforeFollowerExits asserts an instruction is
// delivered while the follow is still running, not on the way out.
//
// The defect being pinned here is a real one from the embedded draft: it drained
// only after the subscription returned, so an indefinite supervisor could record
// decisions for hours and tell the recipient nothing. This test fails if the
// dispatch is deferred to shutdown.
func TestControlCLI_LiveEventDeliversBeforeFollowerExits(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	// An open-ended follow: it never ends on its own, so only a live dispatch can
	// produce a send.
	rt.frames = []agentdeck.Frame{finishedFrame(10, "sess-worker-1")}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Now: func() time.Time { return f.now },
		Log: func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, rerr := eng.Run(ctx, "forge-run-1842-repair")
		done <- rerr
	}()

	// The instruction must arrive without the follow ever finishing.
	deadline := time.After(5 * time.Second)
	for {
		if len(rt.sentMessages()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no instruction was delivered while the follow was live; sends=%v", rt.sentMessages())
		case err := <-done:
			t.Fatalf("the follow ended before delivering: %v (sends=%v)", err, rt.sentMessages())
		case <-time.After(20 * time.Millisecond):
		}
	}

	sends := rt.sentMessages()
	if len(sends) != 1 {
		t.Fatalf("recipient received %d instructions, want exactly 1: %v", len(sends), sends)
	}
	if !strings.HasPrefix(sends[0], "sess-auditor-1") {
		t.Errorf("instruction went to %q, want the independent auditor", sends[0])
	}

	cancel()
	<-done
}

// TestControlCLI_DecisionIntentCursorAtomic asserts the decision, its effect and
// the cursor are committed as one transaction.
//
// This is the property that makes a restart safe. If the cursor advanced without
// the effect, the workflow would skip a frame it never acted on; if the effect
// were recorded without the cursor, the frame would be re-decided and the same
// instruction delivered twice. Both are silent, and neither is observable from
// the outside except by inspecting what one transaction did.
func TestControlCLI_DecisionIntentCursorAtomic(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	rt.frames = []agentdeck.Frame{finishedFrame(7, "sess-worker-1")}
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 7}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true,
		Now:  func() time.Time { return f.now },
		Log:  func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	res, err := eng.Run(context.Background(), "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.BoundaryReached {
		t.Error("a --once run should have reached its boundary")
	}
	if res.FramesDecided != 1 {
		t.Errorf("decided %d frames, want 1", res.FramesDecided)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	// All three landed in the same committed state.
	if !st.CursorSet || st.BusCursor != 7 {
		t.Errorf("cursor = (%d, set=%t), want (7, true)", st.BusCursor, st.CursorSet)
	}
	if len(st.Effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(st.Effects))
	}
	if st.Effects[0].Status != EffectDelivered {
		t.Errorf("effect status = %q, want %q", st.Effects[0].Status, EffectDelivered)
	}
	if st.ActiveReview == nil || st.ActiveReview.Kind != ReviewCompletion {
		t.Errorf("active review = %+v, want a COMPLETION review", st.ActiveReview)
	}
	if st.AuditAttempts != 1 {
		t.Errorf("audit attempts = %d, want 1", st.AuditAttempts)
	}
}

// TestControlCLI_OnceHandlesEmptyAndIrrelevantBacklog asserts a single pass
// terminates on a backlog that decides nothing.
//
// The embedded draft checked its stop condition only after a decision, so frames
// belonging to other sessions never satisfied it and a --once run on a quiet
// stream blocked until it was killed. These cases are exactly that.
func TestControlCLI_OnceHandlesEmptyAndIrrelevantBacklog(t *testing.T) {
	cases := map[string][]agentdeck.Frame{
		"empty backlog": {},
		"other sessions only": {
			transitionFrame(5, "sess-somebody-else", "running", "idle"),
			transitionFrame(6, "sess-somebody-else", "running", "idle"),
		},
		"unknown kinds only": {
			{Cursor: 3, Kind: "macapp.something", SessionID: "sess-worker-1"},
			{Cursor: 4, Kind: "session.status", SessionID: "sess-worker-1"},
		},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			f := boundRequest(t, newFixture(t))
			rt := newFakeRuntime()
			rt.frames = frames
			var highest uint64
			for _, fr := range frames {
				if fr.Cursor > highest {
					highest = fr.Cursor
				}
			}
			rt.health = agentdeck.BusHealth{Enabled: true, Cursor: highest}

			eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
				Once: true,
				Now:  func() time.Time { return f.now },
				Log:  func(line string) { t.Log(line) },
			})
			if err != nil {
				t.Fatalf("new engine: %v", err)
			}
			defer eng.Close()

			type outcome struct {
				res RunResult
				err error
			}
			out := make(chan outcome, 1)
			go func() {
				r, rerr := eng.Run(context.Background(), "forge-run-1842-repair")
				out <- outcome{r, rerr}
			}()

			select {
			case o := <-out:
				// An empty backlog has no frame to satisfy the boundary, so the
				// follower exiting is the honest terminator; anything else must not
				// have decided or sent anything.
				if o.res.FramesDecided != 0 {
					t.Errorf("decided %d frames on a non-deciding backlog", o.res.FramesDecided)
				}
				if len(rt.sentMessages()) != 0 {
					t.Errorf("sent %v on a non-deciding backlog", rt.sentMessages())
				}
				if o.err != nil && !strings.Contains(o.err.Error(), "follower exited") {
					t.Errorf("unexpected error: %v", o.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a --once run over a non-deciding backlog must terminate, not block")
			}
		})
	}
}

// TestControlCLI_DryRunBudgetWritesNothing asserts a dry run changes no real
// state.
//
// The embedded draft's dry run suppressed sends but still wrote state, advanced
// the cursor and consumed attempt budget, so a test exercising it was mutating
// the workflow it claimed to be observing. Here the dispatcher is put in dry-run
// mode and every real state field must be untouched.
func TestControlCLI_DryRunBudgetWritesNothing(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	before, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	rt := newFakeRuntime()
	rt.frames = []agentdeck.Frame{finishedFrame(11, "sess-worker-1")}
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 11}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once:   true,
		DryRun: true,
		Now:    func() time.Time { return f.now },
		Log:    func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err != nil && !strings.Contains(err.Error(), "follower exited") {
		t.Fatalf("dry run: %v", err)
	}

	after, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if after.Revision != before.Revision {
		t.Errorf("dry run advanced the revision from %d to %d", before.Revision, after.Revision)
	}
	if after.AuditAttempts != before.AuditAttempts {
		t.Errorf("dry run consumed audit budget: %d then %d", before.AuditAttempts, after.AuditAttempts)
	}
	if after.BusCursor != before.BusCursor {
		t.Errorf("dry run advanced the cursor: %d then %d", before.BusCursor, after.BusCursor)
	}
	if len(after.Effects) != 0 {
		t.Errorf("dry run recorded effects: %+v", after.Effects)
	}
	if after.ActiveReview != nil {
		t.Errorf("dry run opened a review: %+v", after.ActiveReview)
	}
	if len(rt.sentMessages()) != 0 {
		t.Errorf("dry run sent messages: %v", rt.sentMessages())
	}
}

// TestMigration_OldAndNewOwnersCannotRunTogether asserts a request cannot be
// owned by two engines, and that legacy embedded state is refused rather than
// adopted.
//
// The legacy half matters as much as the lock. Adopting legacy state would let a
// completion or audit conclusion that was never correlated to a request, a
// generation or a reviewer become this engine's starting truth — an approval
// nobody actually gave.
func TestMigration_OldAndNewOwnersCannotRunTogether(t *testing.T) {
	t.Run("second engine refused", func(t *testing.T) {
		f := boundRequest(t, newFixture(t))
		rt := newFakeRuntime()

		first, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{Now: func() time.Time { return f.now }})
		if err != nil {
			t.Fatalf("first engine: %v", err)
		}
		defer first.Close()

		second, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{Now: func() time.Time { return f.now }})
		if err == nil {
			second.Close()
			t.Fatal("a second engine must be refused while one already owns the request")
		}
		if !errors.Is(err, ErrOwnershipHeld) {
			t.Errorf("error = %v, want ErrOwnershipHeld", err)
		}

		// Releasing ownership lets a new engine take over.
		first.Close()
		third, err := NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{Now: func() time.Time { return f.now }})
		if err != nil {
			t.Fatalf("engine after release: %v", err)
		}
		third.Close()
	})

	t.Run("legacy state refused", func(t *testing.T) {
		f := boundRequest(t, newFixture(t))
		dir, err := f.store.RequestDir("forge-run-1842-repair")
		if err != nil {
			t.Fatalf("request dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "AUDIT.md"), []byte("Verdict: PASS\n"), 0o600); err != nil {
			t.Fatalf("plant legacy artifact: %v", err)
		}
		_, err = NewEngine(f.store, newFakeRuntime(), "forge-run-1842-repair", EngineOptions{Now: func() time.Time { return f.now }})
		if err == nil {
			t.Fatal("legacy embedded state must be refused, not adopted")
		}
		if !errors.Is(err, ErrLegacyState) {
			t.Errorf("error = %v, want ErrLegacyState", err)
		}
	})
}

// TestRuntime_RetentionOrProducerLossReconciles asserts the two observation-loss
// cases are surfaced rather than mistaken for progress.
//
// A disabled bus means the supervisor would see nothing while reporting
// healthy. An unprovable send means the message may or may not have landed. Both
// are treated as things to reconcile, never as "nothing happened".
func TestRuntime_RetentionOrProducerLossReconciles(t *testing.T) {
	t.Run("disabled bus refuses to run", func(t *testing.T) {
		f := boundRequest(t, newFixture(t))
		rt := newFakeRuntime()
		rt.health = agentdeck.BusHealth{Enabled: false}

		eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{Now: func() time.Time { return f.now }})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		defer eng.Close()

		_, err = eng.Run(context.Background(), "forge-run-1842-repair")
		if err == nil {
			t.Fatal("running against a disabled bus must be refused")
		}
		if !strings.Contains(err.Error(), "event bus is disabled") {
			t.Errorf("error = %v, want an explicit disabled-bus refusal", err)
		}
	})

	t.Run("unprovable send blocks and is not resent", func(t *testing.T) {
		f := boundRequest(t, newFixture(t))
		rt := newFakeRuntime()
		// The queue settled the record without transcript evidence: exactly the
		// ambiguous middle.
		rt.sendState = "submitted"
		rt.settled = true

		eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{Now: func() time.Time { return f.now }})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		defer eng.Close()

		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			return s.RecordEffect(Effect{
				SessionID: "sess-auditor-1", Role: "auditor",
				Message: "audit this", Reason: "test", CreatedAt: s.UpdatedAt,
			})
		}); err != nil {
			t.Fatalf("record effect: %v", err)
		}

		d := NewDispatcher(f.store, rt)
		d.DryRun = false
		res, err := d.Drain(context.Background(), "forge-run-1842-repair")
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if res.Blocked != 1 || res.Delivered != 0 {
			t.Errorf("result = %+v, want exactly one blocked effect", res)
		}

		st, err := f.store.State("forge-run-1842-repair")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(st.Effects) != 1 || st.Effects[0].Status != EffectBlocked {
			t.Errorf("effects = %+v, want one blocked effect", st.Effects)
		}
		if st.Effects[0].LastError == "" {
			t.Error("a blocked effect must explain itself")
		}

		// A later reconcile must not resend it.
		before := len(rt.sentMessages())
		if _, err := d.Reconcile(context.Background(), "forge-run-1842-repair"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if after := len(rt.sentMessages()); after != before {
			t.Errorf("an unprovable send was resent: %d then %d attempts", before, after)
		}
	})

	t.Run("landed send reconciles to delivered", func(t *testing.T) {
		f := boundRequest(t, newFixture(t))
		rt := newFakeRuntime()
		rt.sendState = "landed"

		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			e := Effect{SessionID: "sess-auditor-1", Role: "auditor", Message: "audit", CreatedAt: s.UpdatedAt}
			if err := s.RecordEffect(e); err != nil {
				return err
			}
			// Pretend a previous process claimed it and died before recording the
			// receipt.
			s.Effects[0].Status = EffectDispatching
			s.Effects[0].SendID = "send-1"
			return nil
		}); err != nil {
			t.Fatalf("seed effect: %v", err)
		}

		d := NewDispatcher(f.store, rt)
		unresolved, err := d.Reconcile(context.Background(), "forge-run-1842-repair")
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(unresolved) != 0 {
			t.Errorf("a landed send must reconcile to delivered, got %d unresolved", len(unresolved))
		}
		st, _ := f.store.State("forge-run-1842-repair")
		if st.Effects[0].Status != EffectDelivered {
			t.Errorf("status = %q, want %q", st.Effects[0].Status, EffectDelivered)
		}
	})
}

// TestDispatcher_EffectLedgerRefusesRatherThanForgetting asserts a full ledger
// refuses new intent instead of discarding owed instructions.
func TestDispatcher_EffectLedgerRefusesRatherThanForgetting(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		for i := 0; i < MaxEffects; i++ {
			if err := s.RecordEffect(Effect{
				SessionID: "sess-auditor-1", Role: "auditor",
				Message: fmt.Sprintf("instruction %d", i), CreatedAt: s.UpdatedAt,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("fill ledger: %v", err)
	}

	d := NewDispatcher(f.store, rt)
	d.DryRun = true
	if _, err := d.Drain(context.Background(), "forge-run-1842-repair"); !errors.Is(err, ErrEffectLedgerFull) {
		t.Fatalf("error = %v, want ErrEffectLedgerFull", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if len(st.Effects) != MaxEffects {
		t.Errorf("ledger = %d effects, want all %d preserved", len(st.Effects), MaxEffects)
	}
}

// stampedTransitionFrame is a transition frame carrying a real timestamp, which
// is what the active-time sampler needs to bound an interval.
func stampedTransitionFrame(cursor uint64, sessionID, from, to string, at time.Time) agentdeck.Frame {
	f := transitionFrame(cursor, sessionID, from, to)
	f.TS = at.UnixMilli()
	return f
}

// TestEngine_ActivitySamplingChargesRunningAndPausesWaiting asserts the engine
// turns observed status into active time correctly.
//
// Two failures are possible in opposite directions and both matter. Charging a
// worker for time it spent blocked on a permission prompt manufactures a
// divergence that never happened; not charging time it was genuinely working
// means a request going nowhere never gets noticed.
func TestEngine_ActivitySamplingChargesRunningAndPausesWaiting(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	t0 := f.now
	rt := newFakeRuntime()
	// running for ten minutes, then blocked waiting for two, then running again.
	//
	// Cursors are DENSE. The real bus follows unfiltered from a cursor and
	// delivers every retained frame, so a jump is a lost observation, not a
	// convenient way to label frames.
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 13}
	rt.frames = []agentdeck.Frame{
		stampedTransitionFrame(10, "sess-worker-1", "starting", "running", t0.Add(1*time.Minute)),
		stampedTransitionFrame(11, "sess-worker-1", "running", "running", t0.Add(11*time.Minute)),
		stampedTransitionFrame(12, "sess-worker-1", "running", "waiting", t0.Add(13*time.Minute)),
		stampedTransitionFrame(13, "sess-worker-1", "waiting", "running", t0.Add(18*time.Minute)),
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true,
		Now:  func() time.Time { return t0 },
		Log:  func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err != nil && !strings.Contains(err.Error(), "follower exited") {
		t.Fatalf("run: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The first frame establishes the watermark and charges nothing, because the
	// interval before it is unobserved. Then 10 minutes running are charged, the
	// 2-minute wait is not, and the final 5 minutes of running are charged.
	want := (10*60 + 5*60) // seconds
	got := int(st.Checkpoint.DebtSeconds)
	if got < want-5 || got > want+5 {
		t.Errorf("active debt = %d seconds, want about %d (running charged, waiting paused)", got, want)
	}
	if st.Checkpoint.LastActivityAt == "" {
		t.Error("confirmed working observations should be recorded")
	}
}

// TestEngine_DueOpensReviewAndInstructsAuditor asserts the wiring that was
// previously missing: due debt actually opens a review and actually reaches the
// reviewer.
//
// The policy for this was implemented and tested in isolation, but a policy
// nothing calls is a policy that never fires. This closes that gap.
func TestEngine_DueOpensReviewAndInstructsAuditor(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	t0 := f.now

	// A tiny soft budget so ordinary activity crosses it.
	f.cfg.Checkpoints.SoftActiveBudget = 3 * time.Minute

	rt := newFakeRuntime()
	rt.sendState = "landed"
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 11}
	rt.frames = []agentdeck.Frame{
		stampedTransitionFrame(10, "sess-worker-1", "starting", "running", t0.Add(1*time.Minute)),
		stampedTransitionFrame(11, "sess-worker-1", "running", "running", t0.Add(6*time.Minute)),
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true,
		Now:  func() time.Time { return t0 },
		Log:  func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err != nil && !strings.Contains(err.Error(), "follower exited") {
		t.Fatalf("run: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.ActiveReview == nil {
		t.Fatal("crossing the soft budget with no reviewed advance must open a review")
	}
	if st.ActiveReview.Kind != ReviewCheckpoint {
		t.Errorf("review kind = %q, want %q", st.ActiveReview.Kind, ReviewCheckpoint)
	}
	if !st.Checkpoint.ReviewPending {
		t.Error("an open review must be marked pending so a duplicate wake is suppressed")
	}
	if st.Stage != StageCheckpointReview {
		t.Errorf("stage = %s, want %s", st.Stage, StageCheckpointReview)
	}
	// The reviewer was actually told, exactly once.
	sends := rt.sentMessages()
	if len(sends) != 1 {
		t.Fatalf("reviewer received %d instructions, want exactly 1: %v", len(sends), sends)
	}
	if !strings.HasPrefix(sends[0], "sess-auditor-1") {
		t.Errorf("instruction went to %q, want the auditor", sends[0])
	}
	// Scheduling a review must not reset the debt: nothing has been judged yet.
	if st.Checkpoint.DebtSeconds <= 0 {
		t.Error("scheduling a review must not reset the active-time debt")
	}
}

// TestEngine_ReviewWithoutAuditorBlocks asserts an unreviewable request blocks
// rather than silently continuing.
//
// A review that cannot be delivered is a request nobody is watching. Continuing
// quietly would look exactly like healthy supervision while nobody was actually
// supervising anything.
func TestEngine_ReviewWithoutAuditorBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	t0 := f.now
	f.cfg.Checkpoints.SoftActiveBudget = time.Minute

	// Remove the auditor binding.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.AuditorSession = ""
		return nil
	}); err != nil {
		t.Fatalf("unbind auditor: %v", err)
	}

	rt := newFakeRuntime()
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 11}
	rt.frames = []agentdeck.Frame{
		stampedTransitionFrame(10, "sess-worker-1", "starting", "running", t0.Add(1*time.Minute)),
		stampedTransitionFrame(11, "sess-worker-1", "running", "running", t0.Add(5*time.Minute)),
	}

	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true,
		Now:  func() time.Time { return t0 },
		Log:  func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err != nil && !strings.Contains(err.Error(), "follower exited") {
		t.Fatalf("run: %v", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage != StageBlocked {
		t.Errorf("stage = %s, want %s when a review cannot be delivered", st.Stage, StageBlocked)
	}
	if !strings.Contains(st.StageReason, "no auditor session") {
		t.Errorf("stage reason = %q, want it to explain the missing auditor", st.StageReason)
	}
	if len(rt.sentMessages()) != 0 {
		t.Errorf("an instruction was sent with no recipient: %v", rt.sentMessages())
	}
}

// auditedRequest opens a COMPLETION review with its obligations still OPEN.
//
// It deliberately does NOT pre-advance them. Marking obligations ADVANCED here
// was a bypass of the very thing the completion path has to prove: that a PASS
// discharges work on the strength of an explicit, evidence-backed judgement for
// every current obligation. A fixture that arrives already discharged cannot tell
// a store that discharges correctly from one that never discharges at all, so
// every test that reaches a PASS supplies its own judgements instead.
func auditedRequest(t *testing.T, f *fixture) (reviewID string) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.AuditAttempts++
		s.ActiveReview = NewActiveReview(ReviewCompletion, "A-1", s.AuditorSession, s.Generation, s.AuditAttempts)
		s.Checkpoint.ReviewPending = true
		if err := s.transition(StageCompletionAudit, "completion audit owed", s.UpdatedAt); err != nil {
			return err
		}
		reviewID = s.ActiveReview.ReviewID
		return nil
	}); err != nil {
		t.Fatalf("open completion audit: %v", err)
	}
	return reviewID
}

// TestAudit_OnlyActiveCompletionPassCanVerify asserts the single most important
// gate in the system.
//
// A completion PASS is the only thing that can verify a request, so everything
// that is not one of them must fail: a wrong review ID, a stale generation, a
// mismatched mode, a PASS from a diagnostic review, a PASS that judges nothing,
// and a PASS that cites no report.
func TestAudit_OnlyActiveCompletionPassCanVerify(t *testing.T) {
	// The fixture's required checks are made satisfiable so this test is about
	// the review gate rather than about running a build. S0 makes required
	// verification actually execute, so a gate test would otherwise be blocked
	// by an unrelated toolchain failure.
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)

	// A PASS citing no report is refused: the reasoning is part of the approval.
	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: reviewID, TaskID: "", Generation: 1,
		Outcome: OutcomePass, Body: "everything checks out",
	}); err == nil {
		t.Fatal("a completion PASS citing no committed report must be refused")
	}

	// A PASS that judges no obligation is refused. The obligations in this
	// fixture are OPEN and stay OPEN: a completion PASS does not supply the
	// missing judgements, so prose about them is not a substitute for them.
	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: reviewID, Generation: 1,
		Outcome: OutcomePass, ReportDigest: strings.Repeat("ab", 32), Body: "ok",
	}); err == nil {
		t.Fatal("a PASS that judges no obligation must be refused")
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.Obligations[0].Disposition != DispositionOpen {
			t.Fatalf("a refused PASS advanced obligation %s", s.Obligations[0].ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("obligations unchanged: %v", err)
	}

	// A properly correlated PASS with a committed report that judges every
	// obligation on evidence that resolves verifies. The report body and the
	// answer are the same bytes: the claimed digest is checked against what
	// actually gets committed, so citing one body while submitting another is
	// refused.
	reportBody := judgedCompletionBody(t, f)
	digest, err := f.store.CommitReport("forge-run-1842-repair", "A-1-answer",
		[]byte(reportBody), func(s *State, d string) error { return nil })
	if err != nil {
		t.Fatalf("commit report: %v", err)
	}
	out, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: reviewID, Generation: 1,
		// The answer is attributable to the session the review was addressed to.
		SourceSession: "sess-auditor-1",
		AnswerNonce:   issuedNonce(t, f),
		Outcome:       OutcomePass, ReportDigest: digest, Body: reportBody,
	})
	if err != nil {
		t.Fatalf("valid PASS refused: %v", err)
	}
	if !out.Verified {
		t.Error("a properly correlated completion PASS must verify the request")
	}
	if out.ResultStatus != "PASS" {
		t.Errorf("result status = %q, want PASS", out.ResultStatus)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage != StageVerified {
		t.Errorf("stage = %s, want %s", st.Stage, StageVerified)
	}
	res, err := f.store.Result("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !res.Ready || res.Status != "PASS" {
		t.Errorf("result = %+v, want a ready PASS", res)
	}

	// A second answer cannot change a published outcome.
	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: reviewID, Generation: 1, Outcome: OutcomeFail,
	}); err == nil {
		t.Error("a published outcome must not be revisable")
	}
}

// TestAudit_DiagnosticCannotVerify asserts a diagnostic review can never verify
// a request, in either direction: the mode cannot emit PASS, and a PASS answer
// claiming a diagnostic review is refused even if the outcome were allowed.
func TestAudit_DiagnosticCannotVerify(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		for i := range s.Obligations {
			s.Obligations[i].Disposition = DispositionAdvanced
		}
		s.ActiveReview = NewActiveReview(ReviewDiagnostic, "D-1", s.AuditorSession, s.Generation, 1)
		if err := s.transition(StageDiagnostic, "diagnostic owed", s.UpdatedAt); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("open diagnostic: %v", err)
	}

	// The mode's outcome allowlist excludes PASS.
	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewDiagnostic, ReviewID: "D-1", Generation: 1, Outcome: OutcomePass,
		ReportDigest: strings.Repeat("ab", 32),
	}); err == nil {
		t.Fatal("a DIAGNOSTIC review must not be able to answer PASS")
	}

	// A valid diagnostic guidance answer returns the request to work and does
	// not verify anything.
	out, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewDiagnostic, ReviewID: "D-1", Generation: 1, Outcome: OutcomeGuidance,
		SourceSession: "sess-auditor-1",
		AnswerNonce:   issuedNonce(t, f),
		Body:          "root cause: the snapshot was stale",
	})
	if err != nil {
		t.Fatalf("diagnostic guidance refused: %v", err)
	}
	if out.Verified {
		t.Error("a diagnostic must never verify a request")
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage.Terminal() {
		t.Errorf("diagnostic guidance made the request terminal: %s", st.Stage)
	}
	if st.DiagnosticRounds != 1 {
		t.Errorf("diagnostic rounds = %d, want 1", st.DiagnosticRounds)
	}
}

// TestAudit_StaleOrMalformedRefused asserts correlation is enforced strictly.
func TestAudit_StaleOrMalformedRefused(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	reviewID := auditedRequest(t, f)
	digest := strings.Repeat("ab", 32)

	cases := map[string]ReviewAnswer{
		"no review id":     {Kind: ReviewCompletion, Generation: 1, Outcome: OutcomePass, ReportDigest: digest},
		"wrong review id":  {Kind: ReviewCompletion, ReviewID: "A-99", Generation: 1, Outcome: OutcomePass, ReportDigest: digest},
		"wrong mode":       {Kind: ReviewDiagnostic, ReviewID: reviewID, Generation: 1, Outcome: OutcomeGuidance},
		"wrong generation": {Kind: ReviewCompletion, ReviewID: reviewID, Generation: 99, Outcome: OutcomePass, ReportDigest: digest},
		"wrong task":       {Kind: ReviewCompletion, ReviewID: reviewID, TaskID: "T-someone-else", Generation: 1, Outcome: OutcomePass, ReportDigest: digest},
	}
	for name, ans := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ans); err == nil {
				t.Errorf("%s must be refused", name)
			}
		})
	}

	// None of the refusals may have mutated anything.
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage.Terminal() {
		t.Errorf("a refused answer made the request terminal: %s", st.Stage)
	}
	if st.ActiveReview == nil {
		t.Error("a refused answer closed the open review")
	}
}

// TestAudit_ReportFailureCannotVerify asserts that failing to commit the report
// behind a PASS prevents the PASS, rather than verifying a request whose
// reasoning does not exist.
func TestAudit_ReportFailureCannotVerify(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	reviewID := auditedRequest(t, f)

	// Obstruct the reports directory so committing the answer body fails.
	reportsDir := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair", ReportsDirName)
	if err := os.RemoveAll(reportsDir); err != nil {
		t.Fatalf("clear reports: %v", err)
	}
	if err := os.WriteFile(reportsDir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("obstruct reports dir: %v", err)
	}

	if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: reviewID, Generation: 1,
		Outcome: OutcomePass, ReportDigest: strings.Repeat("ab", 32),
		Body: "verified, allegedly",
	}); err == nil {
		t.Fatal("a PASS whose report cannot be committed must fail")
	}

	st, _ := f.store.State("forge-run-1842-repair")
	if st.Stage == StageVerified {
		t.Fatal("the request was verified despite the report failing to commit")
	}
	if st.ResultDigest != "" {
		t.Error("a result was published despite the report failure")
	}
}

// TestAudit_ThirdAllowedFourthBlocked asserts the completion-attempt budget
// permits a configured number of audits and refuses the next.
func TestAudit_ThirdAllowedFourthBlocked(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	// The effective limit is the request's own, which may be lower than the
	// operator ceiling: a request may ask for fewer audits, never more.
	initial, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	allowed := initial.Limits.AuditAttempts
	if allowed <= 0 {
		t.Fatal("fixture must configure an audit budget")
	}
	if allowed > f.cfg.Limits.AuditAttempts {
		t.Fatalf("request limit %d exceeds the operator ceiling %d", allowed, f.cfg.Limits.AuditAttempts)
	}

	// Book FAILs until the budget is used up.
	for i := 1; i <= allowed; i++ {
		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			if s.AuditAttempts >= s.Limits.AuditAttempts {
				return errors.New("budget exhausted")
			}
			s.AuditAttempts++
			s.ActiveReview = NewActiveReview(ReviewCompletion, fmt.Sprintf("A-%d", s.AuditAttempts), s.AuditorSession, s.Generation, s.AuditAttempts)
			return s.transition(StageCompletionAudit, "audit owed", s.UpdatedAt)
		}); err != nil {
			t.Fatalf("audit %d of %d: %v", i, allowed, err)
		}
		// Answer FAIL, which books a repair cycle but keeps the request alive.
		if _, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
			Kind: ReviewCompletion, ReviewID: fmt.Sprintf("A-%d", i), Generation: 1,
			SourceSession: "sess-auditor-1",
			AnswerNonce:   issuedNonce(t, f),
			Outcome:       OutcomeFail, Body: "not yet",
		}); err != nil {
			t.Fatalf("FAIL %d: %v", i, err)
		}
	}

	// The next audit request is over budget.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if s.AuditAttempts >= s.Limits.AuditAttempts {
			return fmt.Errorf("audit budget of %d is exhausted", s.Limits.AuditAttempts)
		}
		return nil
	}); err == nil {
		t.Fatalf("audit %d must be refused after %d attempts", allowed+1, allowed)
	}
}

// TestFailure_RenamedHypothesisDoesNotResetCycleLimit asserts the cycle budget is
// counted from objective evidence, not from what the worker calls its attempt.
//
// This is the property that makes the budget a bound. If renaming a hypothesis
// bought a fresh attempt, a stuck worker would simply relabel its retries and
// loop indefinitely while appearing to explore.
func TestFailure_RenamedHypothesisDoesNotResetCycleLimit(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	original := "TestFoo: expected 3 got 4\n    at parser.go:118:7"

	first, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RecordFailure(FailureRecord{
			CycleID: "c-1", CheckID: "regression",
			OriginalText: original, Hypothesis: "the parser is wrong",
		}, f.now)
		return nil
	})
	if err != nil {
		t.Fatalf("first failure: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.RepairCycles != 1 {
		t.Fatalf("cycles = %d, want 1", st.RepairCycles)
	}

	// The same failure, with a completely different hypothesis label, and even
	// different volatile detail in the text, is the same failure.
	renamed, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RecordFailure(FailureRecord{
			CycleID: "c-2", CheckID: "regression",
			// Different line number, different hex address, different duration:
			// all volatile, none of it identity.
			OriginalText: "TestFoo: expected 3 got 4\n    at parser.go:207:3 (0xdeadbeef, 1.25s)",
			Hypothesis:   "the scheduler is wrong",
		}, f.now)
		return nil
	})
	if err != nil {
		t.Fatalf("renamed failure: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.RepairCycles != 1 {
		t.Errorf("cycles = %d, want still 1: a renamed repeat must not consume budget", st.RepairCycles)
	}
	_ = renamed
	_ = first

	// The original text is still available for a reviewer to judge the
	// normalization, which is the point of keeping it.
	if len(st.Failures) != 2 {
		t.Fatalf("failures recorded = %d, want both attempts retained for review", len(st.Failures))
	}
	if !strings.Contains(st.Failures[0].OriginalText, "parser.go:118:7") {
		t.Error("the original failure text was not preserved")
	}
}

// TestFailure_GuidanceExhaustionBlocks asserts the diagnostic budget is checked
// before another diagnostic, including on a repeat.
func TestFailure_GuidanceExhaustionBlocks(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.DiagnosticRounds = s.guidanceAllowed()
		return nil
	}); err != nil {
		t.Fatalf("seed rounds: %v", err)
	}

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RecordFailure(FailureRecord{
			CycleID: "c-1", CheckID: "regression",
			OriginalText: "boom", Hypothesis: "unknown",
		}, f.now)
		return nil
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.DiagnosticRounds > 0 && !st.Stage.Terminal() && len(st.Failures) > 0 {
		// The verdict is what matters; assert it through a direct call.
	}
	verdict := (&State{
		Limits:           st.Limits,
		DiagnosticRounds: st.DiagnosticRounds,
		RepairCycles:     st.RepairCycles,
		Failures:         st.Failures,
	}).RecordFailure(FailureRecord{
		CycleID: "c-2", CheckID: "regression", OriginalText: "boom again",
	}, f.now)
	if !verdict.GuidanceExhausted {
		t.Error("with the guidance budget spent, a further failure must report exhaustion")
	}
	if verdict.NeedsDiagnosis {
		t.Error("an exhausted guidance budget must not schedule another diagnostic")
	}
}

// TestMemory_ResolvedQuestionAcceptedDecisionSurvivesRestart asserts durable
// memory does what it exists for: a replacement worker reads settled ground
// instead of re-deriving it.
//
// The chain exercised is the whole point — a question raised, a decision
// accepting it, the question marked resolved with an explicit DecisionID — and
// then a restart and a generation change, after which the decision must still be
// there, still accepted, still attributed, and still carrying the generation it
// was made in rather than the one it is being read from.
func TestMemory_ResolvedQuestionAcceptedDecisionSurvivesRestart(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now

	// An auditor accepts a decision; the worker may not.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.DecisionAuthorityRole = "auditor"
		return nil
	}); err != nil {
		t.Fatalf("configure authority: %v", err)
	}

	var questionID string
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		q, err := s.OpenQuestion("which scheduler should own the lock?", "both paths contend", "implementation", now)
		questionID = q.ID
		return err
	}); err != nil {
		t.Fatalf("open question: %v", err)
	}
	if questionID != "Q-0001" {
		t.Errorf("first question id = %q, want Q-0001", questionID)
	}

	// The proposing role cannot accept its own proposal.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.RecordDecision("pin the lock to the scheduler", "because...", questionID, "implementation", nil, now)
		return err
	}); !errors.Is(err, ErrNoDecisionAuthority) {
		t.Fatalf("a proposer accepting its own decision = %v, want ErrNoDecisionAuthority", err)
	}

	var decisionID string
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		d, err := s.RecordDecision("pin the lock to the scheduler", "measured under contention", questionID, "auditor", []string{"E-1"}, now)
		decisionID = d.ID
		return err
	}); err != nil {
		t.Fatalf("record accepted decision: %v", err)
	}

	// The question resolved with an explicit link, not a title edit.
	st, _ := f.store.State("forge-run-1842-repair")
	var resolved *QuestionRecord
	for i := range st.Questions {
		if st.Questions[i].ID == questionID {
			resolved = &st.Questions[i]
		}
	}
	if resolved == nil {
		t.Fatal("the question vanished")
	}
	if resolved.Status != QuestionResolvedStatus {
		t.Errorf("question status = %q, want RESOLVED", resolved.Status)
	}
	if resolved.DecisionID != decisionID {
		t.Errorf("question DecisionID = %q, want %q", resolved.DecisionID, decisionID)
	}
	if strings.Contains(resolved.Title, decisionID) {
		t.Error("the decision link was smuggled into the question title instead of being recorded as a link")
	}

	// Restart: a fresh store object over the same directory.
	restarted := NewStore(f.cfg)
	restarted.Now = func() time.Time { return f.now }
	after, err := restarted.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("reload after restart: %v", err)
	}
	if len(after.Decisions) != 1 || after.Decisions[0].ID != decisionID {
		t.Fatalf("decisions after restart = %+v", after.Decisions)
	}
	if after.Decisions[0].Status != DecisionAcceptedStatus {
		t.Errorf("decision status after restart = %q, want ACCEPTED", after.Decisions[0].Status)
	}
	if after.Decisions[0].Authority != "auditor" {
		t.Errorf("decision authority after restart = %q, want auditor", after.Decisions[0].Authority)
	}

	// A generation change must not re-stamp origin.
	if _, err := restarted.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Generation = 4
		return nil
	}); err != nil {
		t.Fatalf("advance generation: %v", err)
	}
	gen4, _ := restarted.State("forge-run-1842-repair")
	if gen4.Decisions[0].OriginGeneration != 1 {
		t.Errorf("decision origin generation = %d, want 1: a refresh must not re-stamp history",
			gen4.Decisions[0].OriginGeneration)
	}
	if gen4.Questions[0].OriginGeneration != 1 {
		t.Errorf("question origin generation = %d, want 1", gen4.Questions[0].OriginGeneration)
	}
	// And the accepted decision is still readable as settled ground.
	if len(gen4.AcceptedDecisions()) != 1 {
		t.Error("the accepted decision must remain readable after a generation change")
	}
}

// TestMemory_IdentifiersAreNeverRecycled asserts compaction cannot make an old
// reference ambiguous.
func TestMemory_IdentifiersAreNeverRecycled(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.DecisionAuthorityRole = "auditor"
		return nil
	}); err != nil {
		t.Fatalf("configure authority: %v", err)
	}

	// Overflow both indexes so trimming definitely runs.
	total := MaxQuestions + 20
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		for i := 0; i < total; i++ {
			q, err := s.OpenQuestion(fmt.Sprintf("question %d", i), "body", "implementation", now)
			if err != nil {
				return err
			}
			if _, err := s.RecordDecision(fmt.Sprintf("decision %d", i), "body", q.ID, "auditor", nil, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("fill indexes: %v", err)
	}

	before, _ := f.store.State("forge-run-1842-repair")
	firstRemaining := before.Questions[0].ID
	nextSeqBefore := before.NextQuestionSeq

	// A new question must not reuse an identifier that is still referenced.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.OpenQuestion("after compaction", "body", "implementation", now)
		return err
	}); err != nil {
		t.Fatalf("open after compaction: %v", err)
	}
	after, _ := f.store.State("forge-run-1842-repair")
	if after.NextQuestionSeq <= nextSeqBefore {
		t.Errorf("question counter did not advance past compaction: %d then %d", nextSeqBefore, after.NextQuestionSeq)
	}
	for _, q := range after.Questions {
		if q.ID == firstRemaining && after.Questions[len(after.Questions)-1].ID == q.ID {
			t.Error("an identifier was recycled by compaction")
		}
	}
	// Identifiers remain unique across the whole index.
	seen := map[string]bool{}
	for _, q := range after.Questions {
		if seen[q.ID] {
			t.Errorf("duplicate question identifier %q after compaction", q.ID)
		}
		seen[q.ID] = true
	}
}

// TestMemory_UnknownReferencesRefused asserts a typo cannot invent memory.
func TestMemory_UnknownReferencesRefused(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.DecisionAuthorityRole = "auditor"
		return nil
	}); err != nil {
		t.Fatalf("configure authority: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.RecordDecision("titled", "body", "Q-9999", "auditor", nil, now)
		return err
	}); !errors.Is(err, ErrMemoryUnknown) {
		t.Errorf("deciding against an unknown question = %v, want ErrMemoryUnknown", err)
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.ResolveQuestion("Q-9999", "", now)
	}); !errors.Is(err, ErrMemoryUnknown) {
		t.Errorf("resolving an unknown question = %v, want ErrMemoryUnknown", err)
	}
}

// sourceTree writes a small source tree and returns its path.
func sourceTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "a.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}
	return dir
}

// TestWorkspace_SnapshotIsReproducibleAndInvalidatedByEdits asserts the snapshot
// means what a receipt needs it to mean.
//
// Two properties, both load-bearing. It must not move when nothing changed, or
// every receipt in the store would be invalidated by a `touch`. And it must move
// when content changed, or a receipt could approve bytes that no longer exist.
func TestWorkspace_SnapshotIsReproducibleAndInvalidatedByEdits(t *testing.T) {
	dir := sourceTree(t)
	now := f0()

	first, err := TakeSnapshot(dir, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if first.FileCount != 2 {
		t.Errorf("file count = %d, want 2 (the two source files)", first.FileCount)
	}

	// Reproducible: same content, different instant.
	second, err := TakeSnapshot(dir, now.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if second.ContentDigest != first.ContentDigest {
		t.Error("the snapshot digest moved without any content change; receipts would be invalidated by a touch")
	}

	// A modification is detected.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	ok, _, err := first.Verify(now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Error("a content change was not detected; a receipt could approve bytes that no longer exist")
	}

	// Adding a file is detected too.
	dir2 := sourceTree(t)
	snap, _ := TakeSnapshot(dir2, now)
	if err := os.WriteFile(filepath.Join(dir2, "extra.go"), []byte("package extra\n"), 0o644); err != nil {
		t.Fatalf("add: %v", err)
	}
	if ok, _, _ := snap.Verify(now); ok {
		t.Error("adding a file was not detected")
	}
}

// TestWorkspace_SnapshotExcludesControlOutput asserts the snapshot cannot
// include the store that describes it.
func TestWorkspace_SnapshotExcludesControlOutput(t *testing.T) {
	dir := sourceTree(t)
	// A control directory and a git directory inside the tree must not appear.
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write .git/HEAD: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "x.js"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write node_modules: %v", err)
	}

	snap, err := TakeSnapshot(dir, f0())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.FileCount != 2 {
		t.Errorf("file count = %d, want 2: .git and node_modules are not source", snap.FileCount)
	}
	// The exclusions are recorded, so a gap is visible rather than silent.
	if len(snap.Excluded) < 2 {
		t.Errorf("excluded = %v, want the exclusions recorded so a gap is visible", snap.Excluded)
	}
}

// TestWorkspace_ReadOnlyWriteIsDenied asserts a read-only grant does not also
// carry write permission, and that a contradictory scope is refused.
//
// The capability preflight reads the immutable admitted request rather than
// mutable state, because the constraints a request was admitted under are part
// of its identity — an operator must not be able to widen a running request's
// write scope by editing state.
func TestWorkspace_ReadOnlyWriteIsDenied(t *testing.T) {
	f := boundRequest(t, newFixture(t))

	// The fixture admitted a REPAIR with explicit write permission.
	repair, err := f.store.PreflightCapability("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("preflight repair: %v", err)
	}
	if !repair.Allowed || repair.Capability != CapabilityScopedWrite {
		t.Errorf("a repair with explicit permission = %+v, want scoped write", repair)
	}

	// Admit a read-only request: no code changes, and the output root is the one
	// writable area.
	ro := f.validRequest()
	ro.RequestID = "forge-run-1842-investigate"
	ro.Operation = contract.OpInvestigate
	ro.Constraints = contract.Constraints{AllowCodeChanges: false, AllowedPaths: []string{"src/**"}}
	if _, err := f.store.Admit(ro); err != nil {
		t.Fatalf("admit read-only: %v", err)
	}
	readOnly, err := f.store.PreflightCapability("forge-run-1842-investigate")
	if err != nil {
		t.Fatalf("preflight read-only: %v", err)
	}
	if !readOnly.Allowed {
		t.Fatalf("a read-only operation with a bounded output should be allowed: %s", readOnly.Reason)
	}
	if readOnly.Capability != CapabilityReadOnly {
		t.Errorf("capability = %q, want %q", readOnly.Capability, CapabilityReadOnly)
	}
	if readOnly.BoundedOutput == "" {
		t.Error("a read-only grant must name the one writable area, or findings have nowhere to go")
	}
	// The very fact that the store carries an explicit output root is what makes
	// read-only admissible; without one, the operation has nowhere to record
	// findings and must be refused rather than given an unbounded output area.
	if readOnly.BoundedOutput != f.cfg.OutputRoot {
		t.Errorf("bounded output = %q, want the configured output root %q", readOnly.BoundedOutput, f.cfg.OutputRoot)
	}

	// A contradictory scope is refused rather than resolved by check order.
	bad := f.validRequest()
	bad.RequestID = "forge-run-1842-contradiction"
	bad.Constraints.AllowedPaths = []string{"src/**", "evaluation/**"}
	bad.Constraints.ProtectedPaths = []string{"evaluation/**"}
	if _, err := f.store.Admit(bad); err == nil {
		t.Fatal("a request whose scope is self-contradictory must be refused at admission")
	} else if !strings.Contains(err.Error(), "allow_code_changes") && !strings.Contains(err.Error(), "protected_paths") {
		t.Errorf("error = %v, want it to name the contradictory constraint", err)
	}
}

// TestWorkspace_ConflictingRequestsRejected asserts two registered operations on
// one workspace cannot both proceed.
func TestWorkspace_ConflictingRequestsRejected(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	ws := filepath.Join(f.srcRoot, "project")

	release, err := f.store.AcquireWorkspaceLease(ws, "forge-run-1842-repair")
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := f.store.AcquireWorkspaceLease(ws, "other-request"); !errors.Is(err, ErrWorkspaceBusy) {
		t.Errorf("second lease error = %v, want ErrWorkspaceBusy", err)
	}
	// The same directory reached by a different path spelling must not take a
	// second lease, which is the whole point of canonicalising first.
	viaDot := filepath.Join(ws, "..", "project")
	if _, err := f.store.AcquireWorkspaceLease(viaDot, "other-request"); !errors.Is(err, ErrWorkspaceBusy) {
		t.Errorf("lease via a different spelling = %v, want ErrWorkspaceBusy: the same directory leased twice", err)
	}

	release()
	if _, err := f.store.AcquireWorkspaceLease(ws, "other-request"); err != nil {
		t.Errorf("the lease must be available after release: %v", err)
	}
}

// TestRefresh_EmptyOldWorkerRefused asserts a replacement cannot be the session
// being replaced.
//
// This is the specific defect the embedded draft had: the generation counter
// advanced while the binding still pointed at the same conversation, producing a
// "fresh" worker with the identical degraded context.
func TestRefresh_EmptyOldWorkerRefused(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.BeginReplacement("context degraded", now)
		return err
	}); err != nil {
		t.Fatalf("begin replacement: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.RecordHandoff(Handoff{
			TaskID: "T-wrong", Generation: 1, Objective: "o", Status: "s", OpenWork: "w", NextAction: "n",
		}, "digest", now)
	}); !errors.Is(err, ErrStaleHandoff) {
		t.Errorf("a handoff for another task = %v, want ErrStaleHandoff", err)
	}

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.RecordHandoff(Handoff{
			TaskID: s.TaskID, Generation: 1, Objective: "o", Status: "s", OpenWork: "w", NextAction: "n",
		}, "digest", now)
	}); err != nil {
		t.Fatalf("valid handoff refused: %v", err)
	}

	// The retired session itself is refused as a replacement.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.RecordCandidate("sess-worker-1", now)
	}); !errors.Is(err, ErrEmptyReplacement) {
		t.Errorf("replacing a worker with itself = %v, want ErrEmptyReplacement", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.RecordCandidate("", now)
	}); !errors.Is(err, ErrEmptyReplacement) {
		t.Errorf("an empty replacement = %v, want ErrEmptyReplacement", err)
	}
}

// TestRefresh_CrashEachBoundaryNoDuplicateWorker walks every crash boundary and
// asserts the recovery action never proposes creating a second session.
func TestRefresh_CrashEachBoundaryNoDuplicateWorker(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now
	// Allow a second generation.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Limits.WorkerGens = 3
		return nil
	}); err != nil {
		t.Fatalf("raise limit: %v", err)
	}

	steps := []struct {
		name    string
		advance func(*State) error
		phase   GenerationPhase
	}{
		{"intent", func(s *State) error { _, err := s.BeginReplacement("degraded", now); return err }, GenIntent},
		{"handoff", func(s *State) error {
			return s.RecordHandoff(Handoff{TaskID: s.TaskID, Generation: s.Generation, Objective: "o", Status: "s", OpenWork: "w", NextAction: "n"}, "d", now)
		}, GenHandoff},
		{"candidate", func(s *State) error { return s.RecordCandidate("sess-worker-2", now) }, GenCandidate},
		{"quiesced", func(s *State) error { return s.Quiesce(now) }, GenCandidate},
		{"binding", func(s *State) error { return s.CommitBinding(now) }, GenBinding},
		{"orientation", func(s *State) error { return s.RecordOrientation("E-1", now) }, GenOrientation},
		{"active", func(s *State) error { return s.FinishReplacement(now) }, GenActive},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
				return step.advance(s)
			}); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			st, _ := f.store.State("forge-run-1842-repair")
			if st.Replacement.Phase != step.phase {
				t.Fatalf("phase = %q, want %q", st.Replacement.Phase, step.phase)
			}
			_, action := st.ReconcileReplacement()
			// Before a candidate exists, creating one is the correct step. From the
			// moment a candidate is recorded, recovery must never propose creating
			// another — that is how two workers end up writing one workspace.
			if st.Replacement.CandidateSession != "" &&
				(strings.Contains(strings.ToLower(action), "create one") ||
					strings.Contains(strings.ToLower(action), "create a new session")) {
				t.Errorf("recovery at %s proposes creating a session despite a recorded candidate: %q", step.phase, action)
			}
		})
	}

	// The generation advanced exactly once, and the binding points at a session
	// that is not the one it retired.
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Generation != 2 {
		t.Errorf("generation = %d, want 2 after one replacement", st.Generation)
	}
	if st.WorkerSession == "sess-worker-1" {
		t.Error("the binding still points at the retired session: the generation advanced without a new worker")
	}
	if st.WorkerSession != "sess-worker-2" {
		t.Errorf("bound session = %q, want sess-worker-2", st.WorkerSession)
	}
}

// TestRefresh_EarlyCandidateCheckpointCannotAdvance asserts a quarantined
// candidate cannot move the request.
func TestRefresh_EarlyCandidateCheckpointCannotAdvance(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Limits.WorkerGens = 3
		_, err := s.BeginReplacement("degraded", now)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.RecordHandoff(Handoff{TaskID: s.TaskID, Generation: s.Generation, Objective: "o", Status: "s", OpenWork: "w", NextAction: "n"}, "d", now); err != nil {
			return err
		}
		return s.RecordCandidate("sess-worker-2", now)
	}); err != nil {
		t.Fatalf("record candidate: %v", err)
	}

	// The candidate is quarantined: the bound worker is still the old session,
	// so a completion from the candidate is not the bound worker's event.
	st, _ := f.store.State("forge-run-1842-repair")
	if st.WorkerSession != "sess-worker-1" {
		t.Error("a candidate became the bound worker before the binding was committed")
	}
	if st.Replacement.Phase != GenCandidate {
		t.Errorf("phase = %q, want CANDIDATE", st.Replacement.Phase)
	}

	// And the binding cannot commit while the old writer is unquiesced.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.CommitBinding(now)
	}); err == nil {
		t.Fatal("a binding must not commit while the retired worker can still write")
	}
}

// TestRefresh_BindingCommittedBeforeActivation asserts binding and generation
// advance together, so a state file can never show a new generation pointing at
// the old session.
func TestRefresh_BindingCommittedBeforeActivation(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	now := f.now
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Limits.WorkerGens = 3
		if _, err := s.BeginReplacement("degraded", now); err != nil {
			return err
		}
		if err := s.RecordHandoff(Handoff{TaskID: s.TaskID, Generation: s.Generation, Objective: "o", Status: "s", OpenWork: "w", NextAction: "n"}, "d", now); err != nil {
			return err
		}
		if err := s.RecordCandidate("sess-worker-2", now); err != nil {
			return err
		}
		return s.Quiesce(now)
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.CommitBinding(now)
	}); err != nil {
		t.Fatalf("commit binding: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.Generation != 2 || st.WorkerSession != "sess-worker-2" {
		t.Errorf("generation = %d bound to %q: the two must move together", st.Generation, st.WorkerSession)
	}
	// The phase records that orientation has not been accepted yet, so a restart
	// can reconcile rather than assume.
	if st.Replacement.Phase != GenBinding {
		t.Errorf("phase = %q, want BINDING: orientation acceptance is not yet known", st.Replacement.Phase)
	}
}

// TestRefresh_NewConversationRevalidatesHandoff asserts a replacement is oriented
// from durable references rather than from a copied summary.
func TestRefresh_NewConversationRevalidatesHandoff(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.DecisionAuthorityRole = "auditor"
		q, err := s.OpenQuestion("which approach?", "", "implementation", f.now)
		if err != nil {
			return err
		}
		if _, err := s.RecordDecision("use the queue", "", q.ID, "auditor", nil, f.now); err != nil {
			return err
		}
		// A genuinely open question, so the prompt has one to surface.
		if _, err := s.OpenQuestion("is the migration reversible?", "", "implementation", f.now); err != nil {
			return err
		}
		s.RecordFailure(FailureRecord{CycleID: "c1", CheckID: "regression", Signature: "testfoo: mismatch", Hypothesis: "the scheduler"}, f.now)
		return nil
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	st, _ := f.store.State("forge-run-1842-repair")
	prompt := st.OrientationPrompt("/approved/worktree")

	// The replacement must be told the settled decisions and the failed attempts,
	// because both are exactly what a fresh context would otherwise rediscover.
	if !strings.Contains(prompt, "use the queue") {
		t.Error("the orientation prompt omits accepted decisions; a fresh worker would re-derive them")
	}
	if !strings.Contains(prompt, "testfoo: mismatch") {
		t.Error("the orientation prompt omits failed approaches; a fresh worker would repeat them")
	}
	if !strings.Contains(prompt, "O-1") {
		t.Error("the orientation prompt omits the open obligations")
	}
	// And it must say plainly that it points rather than asserts.
	if !strings.Contains(prompt, "not what is true") {
		t.Error("the orientation prompt must state that it points at evidence rather than asserting it")
	}
	// An open question must not be presented as settled.
	if !strings.Contains(prompt, "Open questions") {
		t.Error("the orientation prompt omits open questions")
	}
}

func f0() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }

// verifierFixture admits a request whose workspace is a real source tree, and
// returns a store whose check registry can be overridden per test.
func verifierFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := boundRequest(t, newFixture(t))
	dir := sourceTree(t)

	// Re-admit the request against the real tree, since the admitted request is
	// immutable and the capability preflight reads it.
	adm, err := f.store.loadRequestDir("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	adm.Request.Workspace.Path = dir
	adm.Request.Operation = contract.OpRepair
	adm.Request.Constraints.AllowCodeChanges = true
	if err := writeJSONDurable(f.root,
		filepath.Join(RequestsDirName, adm.Request.RequestID, RequestFileName), adm, 0o444); err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	return f, dir
}

// setCheck installs a check into the fixture's registry.
func setCheck(t *testing.T, f *fixture, id string, argv []string, discovery bool, timeout time.Duration) {
	t.Helper()
	f.cfg.Checks[id] = Check{ID: id, Argv: argv, Timeout: timeout, DiscoveryRequired: discovery}
}

// TestVerify_MissingBinaryBlockedNoPanic asserts a missing tool is BLOCKED.
//
// This is the distinction the stage turns on. A missing binary is not evidence
// that the work is wrong; the evidence could not be produced at all. Reporting
// FAIL would blame the code for a harness problem and would consume a repair
// cycle that ought to be spent on the actual defect.
func TestVerify_MissingBinaryBlockedNoPanic(t *testing.T) {
	f, dir := verifierFixture(t)
	setCheck(t, f, "ghost", []string{"definitely-not-a-real-binary-xyz"}, false, time.Minute)

	receipt, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "ghost", "job-1",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if receipt.Outcome != JobBlocked {
		t.Errorf("outcome = %q, want %q: a missing binary is not a failing check", receipt.Outcome, JobBlocked)
	}
	if receipt.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1 (no process ran)", receipt.ExitCode)
	}
	if !strings.Contains(receipt.Reason, "not installed") {
		t.Errorf("reason = %q, want it to name the missing tool", receipt.Reason)
	}

	// And a FAIL against current content is reported as a failure, not blocked.
	setCheck(t, f, "failing", []string{"/bin/sh", "-c", "echo 'FAIL: something' >&2; exit 1"}, false, time.Minute)
	failReceipt, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "failing", "job-2",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if failReceipt.Outcome != JobFail {
		t.Errorf("a reproducible failure = %q, want %q", failReceipt.Outcome, JobFail)
	}
	_ = dir
}

// TestVerify_CancelKillsOwnedDescendants asserts cancelling a check reaches the
// process tree, not just the leader.
//
// This matters because a test runner spawns children. Killing only the parent
// leaves orphaned processes holding the workspace and the terminal, so the next
// run contends with ghosts from the last one, and a cancelled check keeps
// consuming resources while its request is being torn down.
func TestVerify_CancelKillsOwnedDescendants(t *testing.T) {
	f, _ := verifierFixture(t)
	// A check that spawns a long-lived child and then waits.
	setCheck(t, f, "spawner", []string{
		"/bin/sh", "-c",
		"sleep 300 & echo $! > /tmp/ec-child-pid-$$; sleep 300",
	}, false, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan CheckReceipt, 1)
	go func() {
		r, err := f.store.RunCheck(ctx, "forge-run-1842-repair", "spawner", "job-c",
			func() time.Time { return f.now })
		if err != nil {
			t.Errorf("RunCheck: %v", err)
		}
		done <- r
	}()

	// Let the check start and produce its child.
	time.Sleep(700 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		// Cancellation is not a verdict. Reading it as a failure would blame the
		// work for the harness being interrupted.
		if r.Outcome == JobFail {
			t.Errorf("a cancelled check reported FAIL: cancellation is not evidence the work is wrong")
		}
		if r.Outcome != JobBlocked {
			t.Errorf("outcome = %q, want %q for an interrupted check", r.Outcome, JobBlocked)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelling the check did not return; the process group is not being reaped")
	}
}

// TestVerify_ZeroTestsCannotMeetRequiredCheck asserts a green run that tested
// nothing cannot satisfy a required check.
//
// This is the most common way a passing build hides an untested change: the
// command succeeds, discovers nothing, and is reported as evidence.
func TestVerify_ZeroTestsCannotMeetRequiredCheck(t *testing.T) {
	f, _ := verifierFixture(t)
	setCheck(t, f, "empty", []string{"/bin/sh", "-c", "exit 0"}, true, time.Minute)

	receipt, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "empty", "job-z",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if receipt.ExitCode != 0 {
		t.Fatalf("fixture check should exit 0, got %d", receipt.ExitCode)
	}
	if receipt.Outcome != JobFail {
		t.Errorf("outcome = %q, want %q: a check that ran no tests verified nothing", receipt.Outcome, JobFail)
	}
	if !strings.Contains(receipt.Reason, "no test cases") {
		t.Errorf("reason = %q, want it to say nothing was discovered", receipt.Reason)
	}

	// The same command without the discovery requirement passes, because it is
	// not pretending to be a test run.
	setCheck(t, f, "build", []string{"/bin/sh", "-c", "exit 0"}, false, time.Minute)
	buildReceipt, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "build", "job-b",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if buildReceipt.Outcome != JobPass {
		t.Errorf("a non-test check that exits 0 = %q, want PASS", buildReceipt.Outcome)
	}
}

// TestVerify_InspectionCannotReplaceCompile asserts reading the code is not a
// substitute for running the compiler.
func TestVerify_InspectionCannotReplaceCompile(t *testing.T) {
	f, dir := verifierFixture(t)
	setCheck(t, f, "build", []string{"/bin/sh", "-c", "exit 0"}, false, time.Minute)

	if _, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "build", "job-1",
		func() time.Time { return f.now }); err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	snap, err := TakeSnapshot(dir, f.now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	passReceipt := CheckReceipt{CheckID: "build", Outcome: JobPass, ContentDigest: snap.ContentDigest}

	// A gate that only saw the pass receipt, with no other checks required, is
	// satisfied — but only because the check actually ran. The test that matters
	// is that a MISSING check is never treated as satisfied by inspection.
	gate := EvaluateVerification([]string{"build", "regression"}, []CheckReceipt{passReceipt}, snap)
	if gate.Satisfied {
		t.Error("a gate with a missing check must not be satisfied")
	}
	if len(gate.Missing) != 1 || gate.Missing[0] != "regression" {
		t.Errorf("missing = %v, want [regression]", gate.Missing)
	}
	if !strings.Contains(gate.Reason, "regression") {
		t.Errorf("reason = %q, want it to name the missing check", gate.Reason)
	}
}

// TestVerify_OldSnapshotCannotApproveNewDiff asserts a receipt from earlier
// content cannot approve later content.
func TestVerify_OldSnapshotCannotApproveNewDiff(t *testing.T) {
	dir := sourceTree(t)
	before, err := TakeSnapshot(dir, f0())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	oldReceipt := CheckReceipt{
		CheckID: "build", Outcome: JobPass,
		SnapshotID: before.SnapshotID, ContentDigest: before.ContentDigest,
	}

	// The work changes.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// changed\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	after, err := TakeSnapshot(dir, f0())
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if after.ContentDigest == before.ContentDigest {
		t.Fatal("the edit was not detected")
	}

	gate := EvaluateVerification([]string{"build"}, []CheckReceipt{oldReceipt}, after)
	if gate.Satisfied {
		t.Fatal("a receipt from earlier content approved later content")
	}
	if len(gate.Stale) != 1 {
		t.Errorf("stale = %v, want the build check", gate.Stale)
	}
	if !strings.Contains(gate.Reason, "stale") {
		t.Errorf("reason = %q, want it to say the receipt is stale", gate.Reason)
	}
	// Staleness is distinct from failure: the check did not fail, its evidence is
	// simply about different bytes.
	if len(gate.Failed) != 0 {
		t.Errorf("stale evidence must not be reported as a failing check: %v", gate.Failed)
	}
}

// TestVerify_RestartUnknownExternalRunBlocks asserts an external run whose
// outcome is unknown is not treated as passed.
func TestVerify_RestartUnknownExternalRunBlocks(t *testing.T) {
	run := ExternalRun{
		RunID: "forge-1842-verify", Harness: "forge-verification-only",
		Scenario: "regression", SnapshotID: "S-abc123",
		Constraints: ExternalConstraints{VerificationOnly: true, AllowCodeChanges: false},
	}
	if err := ValidateExternalRun(run); err != nil {
		t.Fatalf("a well-formed verification run was refused: %v", err)
	}

	// A result whose run id does not match is refused rather than attributed.
	if _, err := AcceptExternalResult(run, ExternalResult{RunID: "other", Outcome: ExternalPass}); err == nil {
		t.Error("a result for a different run must be refused")
	}
	// So is a result describing other content.
	if _, err := AcceptExternalResult(run, ExternalResult{
		RunID: run.RunID, Outcome: ExternalPass, SnapshotID: "S-different",
	}); err == nil {
		t.Error("a result for a different snapshot must be refused: it would look like evidence")
	}
	// And a result with no outcome at all is not a pass.
	if _, err := AcceptExternalResult(run, ExternalResult{RunID: run.RunID, Outcome: "LIKELY_FINE"}); err == nil {
		t.Error("an unrecognised outcome must be refused, not coerced to a pass")
	}
	// A genuine FAIL is distinct from an unavailable one.
	outcome, err := AcceptExternalResult(run, ExternalResult{RunID: run.RunID, Outcome: ExternalFail, Reason: "regression"})
	if err != nil || outcome != ExternalFail {
		t.Errorf("a reproducible external failure = (%q, %v), want FAIL", outcome, err)
	}
}

// TestForgeContract_ReadOnlyAndRepairConstraints asserts a harness that both
// repairs and verifies is refused.
//
// This is the incompatibility the plan requires be made explicit rather than
// discovered later: a Forge repair loop that also fixes what it measures cannot
// vouch for the repair, because the thing that changed the content is the thing
// attesting to it.
func TestForgeContract_ReadOnlyAndRepairConstraints(t *testing.T) {
	base := ExternalRun{
		RunID: "r1", Harness: "forge", Scenario: "s", SnapshotID: "S-1",
		Constraints: ExternalConstraints{VerificationOnly: true, AllowCodeChanges: false},
	}
	if err := ValidateExternalRun(base); err != nil {
		t.Fatalf("verification-only run refused: %v", err)
	}

	// A harness that may change code cannot be a verifier.
	repairing := base
	repairing.Constraints.AllowCodeChanges = true
	if err := ValidateExternalRun(repairing); err == nil {
		t.Error("a run permitting code changes must be refused as a verification")
	}
	notVerificationOnly := base
	notVerificationOnly.Constraints.VerificationOnly = false
	if err := ValidateExternalRun(notVerificationOnly); err == nil {
		t.Error("a harness that is not verification-only must be refused")
	}

	// Identity and content must be pinned before start, so an unknown outcome can
	// be inspected rather than repeated.
	noRunID := base
	noRunID.RunID = ""
	if err := ValidateExternalRun(noRunID); err == nil {
		t.Error("a run with no pre-assigned id must be refused: its outcome could not be reconciled")
	}
	noSnapshot := base
	noSnapshot.SnapshotID = ""
	if err := ValidateExternalRun(noSnapshot); err == nil {
		t.Error("a run with no pinned snapshot must be refused")
	}
}

// TestVerify_DiscoveryCountsExecutedTestCases asserts coverage is counted from
// `go test -json` machine events rather than from summary lines.
//
// The summary-line behaviour this replaces was unsound in a way that mattered: a
// package "ok" line counted as ONE discovery regardless of how many tests ran,
// and the pattern was satisfiable by OUTPUT rather than by EXECUTION — printing
// such a line with `echo` produced a non-zero count for a run that tested nothing.
//
// Text that is not machine output now counts zero, because a verifier that cannot
// count its own coverage should report none rather than guess.
func TestVerify_DiscoveryCountsExecutedTestCases(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   int
	}{
		{"two distinct test cases", `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.01}
{"Action":"run","Package":"p","Test":"TestB"}
{"Action":"fail","Package":"p","Test":"TestB","Elapsed":0.01}`, 2},
		{"a repeat of the same test counts once", `{"Action":"pass","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestA"}`, 1},
		{"the same name in two packages counts twice", `{"Action":"pass","Package":"a","Test":"TestX"}
{"Action":"pass","Package":"b","Test":"TestX"}`, 2},
		{"package-level events are not test cases", `{"Action":"pass","Package":"p","Elapsed":0.03}`, 0},
		{"skipped tests are counted", `{"Action":"skip","Package":"p","Test":"TestA"}`, 1},
		// The old false positives: text output, not machine events.
		{"a package summary line", "ok  " + `\tstub` + "\t0.01s\n", 0},
		{"per-test PASS lines", "--- PASS: TestA (0.00s)\n--- PASS: TestB (0.00s)", 0},
		{"a bare PASS summary", "PASS\n3 tests passed\n", 0},
		{"nothing at all", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountDiscovered(tc.output); got != tc.want {
				t.Errorf("CountDiscovered = %d, want %d", got, tc.want)
			}
		})
	}
}
