package control

// AP5.1 observation-ingest tests.
//
// The claim under test is that several streams become ONE ordered durable
// history, that the histories of different sources are never conflated, and that
// a discontinuity stops the run rather than being ridden through.
//
// Every test is about what the supervisor is prevented from concluding.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/runtime"
)

func fixedNow() time.Time { return time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC) }

func newIngestor(t *testing.T) (*JournalIngestor, *runtime.Journal) {
	t.Helper()
	j, err := runtime.OpenJournal(t.TempDir(), func() string { return fixedNow().Format(time.RFC3339) })
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	return NewJournalIngestor(j, fixedNow, nil), j
}

// TestIngest_DenseStreamNeverReportsAGap is the ordinary case, and it is the one
// that must stay quiet: a healthy stream that reports a gap every frame would
// block every healthy request.
func TestIngest_DenseStreamNeverReportsAGap(t *testing.T) {
	i, j := newIngestor(t)
	for c := uint64(1); c <= 5; c++ {
		if _, err := i.Observe(runtime.Event{Backend: "agent-deck", Cursor: c, Kind: "session.status"}); err != nil {
			t.Fatalf("observe %d: %v", c, err)
		}
	}
	if i.HasGap() {
		t.Errorf("a dense stream reported a gap: %s", i.GapReason())
	}
	res, err := j.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Records) != 5 {
		t.Errorf("records = %d, want 5", len(res.Records))
	}
}

// TestIngest_FirstObservationIsNotAGap matters at startup.
//
// A run begins mid-stream, so the first frame is necessarily not position 1.
// Treating that as a discontinuity would make every cold start block itself.
func TestIngest_FirstObservationIsNotAGap(t *testing.T) {
	i, _ := newIngestor(t)
	if _, err := i.Observe(runtime.Event{Backend: "agent-deck", Cursor: 4000}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if i.HasGap() {
		t.Errorf("the first observation was treated as a gap: %s", i.GapReason())
	}
}

// TestIngest_JumpIsAGap is the core detection.
func TestIngest_JumpIsAGap(t *testing.T) {
	i, _ := newIngestor(t)
	for c := uint64(1); c <= 3; c++ {
		if _, err := i.Observe(runtime.Event{Backend: "agent-deck", Cursor: c}); err != nil {
			t.Fatalf("observe %d: %v", c, err)
		}
	}
	if _, err := i.Observe(runtime.Event{Backend: "agent-deck", Cursor: 9}); err != nil {
		t.Fatalf("observe 9: %v", err)
	}
	if !i.HasGap() {
		t.Fatal("a jump from 4 to 9 was not detected")
	}
	h := i.Health()[0]
	if h.GapFrom != 4 || h.GapTo != 9 {
		t.Errorf("gap = %d..%d, want 4..9", h.GapFrom, h.GapTo)
	}
	// Positions 4..8 are the ones that will never arrive: five observations.
	if !strings.Contains(h.Reason, "5 observation(s) lost") {
		t.Errorf("reason = %q, want it to count what was lost", h.Reason)
	}
}

// TestIngest_GapIsNeverCleared is the part that is easy to get wrong.
//
// Frames keep arriving after a gap. They are healthy frames. Treating them as
// repairing the gap would resume a supervisor as though it had seen a complete
// stream — which is precisely the loss that matters.
func TestIngest_GapIsNeverCleared(t *testing.T) {
	i, _ := newIngestor(t)
	for _, c := range []uint64{1, 2, 5, 6, 7} {
		if _, err := i.Observe(runtime.Event{Backend: "agent-deck", Cursor: c}); err != nil {
			t.Fatalf("observe %d: %v", c, err)
		}
	}
	if !i.HasGap() {
		t.Fatal("later healthy frames cleared a real gap")
	}
}

// TestIngest_SourcePositionsAreNeverConflated is the multi-backend hazard.
//
// Agent Deck cursor 9 and an ACP ordinal 9 are different units. A consumer that
// treats one as a continuation of the other drops or repeats observations, and
// the symptom is a timing bug when it is a correctness one.
func TestIngest_SourcePositionsAreNeverConflated(t *testing.T) {
	i, _ := newIngestor(t)
	if _, err := i.Observe(runtime.Event{Backend: "agent-deck", Cursor: 9}); err != nil {
		t.Fatalf("observe agent-deck: %v", err)
	}
	// The same ordinal from a different backend is NOT a continuation.
	if _, err := i.Observe(runtime.Event{Backend: "minimax-acp", Cursor: 10}); err != nil {
		t.Fatalf("observe acp: %v", err)
	}
	if i.HasGap() {
		t.Errorf("two backends at adjacent positions were treated as one stream: %s", i.GapReason())
	}
	if n := len(i.Health()); n != 2 {
		t.Fatalf("sources = %d, want 2 tracked separately", n)
	}
	seen := map[string]bool{}
	for _, h := range i.Health() {
		if seen[h.Backend] {
			t.Errorf("source %s reported twice", h.Backend)
		}
		seen[h.Backend] = true
	}
}

// TestIngest_UntaggedObservationIsRefused keeps unattributable records out of
// the journal entirely, rather than quarantining them later.
func TestIngest_UntaggedObservationIsRefused(t *testing.T) {
	i, j := newIngestor(t)
	if _, err := i.Observe(runtime.Event{Cursor: 1, Kind: "session.status"}); err == nil {
		t.Fatal("an observation with no backend was journaled")
	}
	res, err := j.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Records) != 0 {
		t.Errorf("records = %d, want 0", len(res.Records))
	}
}

// TestIngest_HealthIsStableOrdered keeps operator output diffable between runs.
func TestIngest_HealthIsStableOrdered(t *testing.T) {
	i, _ := newIngestor(t)
	for _, b := range []string{"minimax-acp", "agent-deck"} {
		if _, err := i.Observe(runtime.Event{Backend: b, Cursor: 1}); err != nil {
			t.Fatalf("observe %s: %v", b, err)
		}
	}
	got := i.Health()
	if len(got) != 2 || got[0].Backend != "agent-deck" || got[1].Backend != "minimax-acp" {
		t.Errorf("health order = %+v, want agent-deck then minimax-acp", got)
	}
}

// TestIngest_JournalSurvivesReopen is the durability claim: a restart continues
// the sequence rather than restarting it, so pre- and post-restart records
// cannot collide.
func TestIngest_JournalSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := runtime.OpenJournal(dir, func() string { return fixedNow().Format(time.RFC3339) })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	i1 := NewJournalIngestor(first, fixedNow, nil)
	for c := uint64(1); c <= 3; c++ {
		if _, oerr := i1.Observe(runtime.Event{Backend: "agent-deck", Cursor: c}); oerr != nil {
			t.Fatalf("observe: %v", oerr)
		}
	}

	second, err := runtime.OpenJournal(dir, func() string { return fixedNow().Format(time.RFC3339) })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	i2 := NewJournalIngestor(second, fixedNow, nil)
	// Rehydrating is what stops the restart looking like a stream that lost
	// everything: without it the first post-restart observation is a gap.
	res, err := second.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	i2.Rehydrate(res.Records)
	if _, oerr := i2.Observe(runtime.Event{Backend: "agent-deck", Cursor: 4}); oerr != nil {
		t.Fatalf("observe after restart: %v", oerr)
	}
	if i2.HasGap() {
		t.Errorf("a clean restart reported a gap: %s", i2.GapReason())
	}
	if i2.lastSequence() != 4 {
		t.Errorf("sequence after restart = %d, want 4", i2.lastSequence())
	}
}

// TestEngine_JournalsObservationsBeforeDeciding asserts the wiring, not the unit:
// a supervised run leaves a durable record of what it saw.
func TestEngine_JournalsObservationsBeforeDeciding(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 3}
	rt.frames = []agentdeck.Frame{
		transitionFrame(1, "sess-worker-1", "starting", "running"),
		transitionFrame(2, "sess-worker-1", "running", "running"),
		transitionFrame(3, "sess-somebody-else", "running", "idle"),
	}
	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Now: func() time.Time { return t0 },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()
	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatalf("run: %v", err)
	}

	dir, err := f.store.RequestDir("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("request dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journal.ndjson")); err != nil {
		t.Fatalf("no journal was written: %v", err)
	}
	j, err := runtime.OpenJournal(dir, func() string { return t0.Format(time.RFC3339) })
	if err != nil {
		t.Fatalf("reopen journal: %v", err)
	}
	res, err := j.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Records) != 3 {
		t.Errorf("journal holds %d records, want 3", len(res.Records))
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.ConsumedJournalSeq != 3 {
		t.Errorf("consumed journal position = %d, want 3", st.ConsumedJournalSeq)
	}
}

// TestEngine_GapBlocksTheRequest is the end-to-end refusal.
//
// A lost completion frame is a lost completion frame. Continuing would conclude
// the request from evidence that does not exist.
func TestEngine_GapBlocksTheRequest(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 7}
	rt.frames = []agentdeck.Frame{
		transitionFrame(1, "sess-worker-1", "starting", "running"),
		transitionFrame(2, "sess-worker-1", "running", "running"),
		// 3..6 never arrive.
		transitionFrame(7, "sess-worker-1", "running", "running"),
	}
	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Now: func() time.Time { return t0 },
		Log: func(string) {},
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	_, err = eng.Run(context.Background(), "forge-run-1842-repair")
	if err == nil {
		t.Fatal("run succeeded across an observation gap")
	}
	if !strings.Contains(err.Error(), "gap") {
		t.Errorf("error = %v, want it to name the gap", err)
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("load: %v", serr)
	}
	if st.Stage != StageBlocked {
		t.Errorf("stage = %q, want %q", st.Stage, StageBlocked)
	}
	if !strings.Contains(st.StageReason, "lost") {
		t.Errorf("stage reason = %q, want it to record what was lost", st.StageReason)
	}
}

// TestEngine_RefusesToRunWithoutAJournal pins the precondition.
//
// A supervisor that cannot read its own history would decide from whatever is on
// the stream right now, which is the evidence-free operation this layer exists to
// prevent.
func TestEngine_RefusesToRunWithoutAJournal(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.health = agentdeck.BusHealth{Enabled: true, Cursor: 1}
	rt.frames = []agentdeck.Frame{transitionFrame(1, "sess-worker-1", "starting", "running")}
	t0 := f.now
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Once: true, Now: func() time.Time { return t0 },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()

	// Break the journal the way a damaged store would.
	eng.journalErr = os.ErrPermission
	if _, err := eng.Run(context.Background(), "forge-run-1842-repair"); err == nil {
		t.Fatal("run succeeded with an unopenable journal")
	} else if !strings.Contains(err.Error(), "journal") {
		t.Errorf("error = %v, want it to name the journal", err)
	}
}
