package control

// Observation ingest: many streams, one durable order, and honest gaps.
//
// The engine used to follow one Agent Deck stream and advance one cursor. Two
// backends make that insufficient in a way that is easy to get wrong quietly: an
// Agent Deck cursor and an ACP connection ordinal are different UNITS. Merging
// them produces a number that looks like a position and is not one — and the
// symptom is a supervisor that skips frames or replays them, presenting as a
// timing bug when it is a correctness one.
//
// So the journal sequence is the only position anything is decided on. Each
// backend's own position is tracked per source, and is never compared across
// sources.
//
// The other half is gaps. A source whose position jumps has lost observations we
// will never see again. That is not a hiccup to ride through: a missing
// completion frame is a missing completion frame, and continuing past it means
// concluding from evidence that does not exist. A gap blocks.

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// SourceHealth is what is known about one backend's stream right now.
type SourceHealth struct {
	// Backend identifies the transport.
	Backend string
	// Last is the highest position observed from it.
	Last uint64
	// Expected is the next position this consumer will accept.
	Expected uint64
	// Seen counts observations ingested from this source.
	Seen int
	// GapFrom and GapTo describe a detected discontinuity, when there is one.
	GapFrom uint64
	GapTo   uint64
	// LastAt is when this source last produced anything.
	LastAt string
	// Healthy reports whether this source can currently be relied on.
	Healthy bool
	// Reason is a machine-readable cause code, empty when healthy.
	Reason string
}

// String renders one source for operator output.
func (h SourceHealth) String() string {
	if h.Healthy {
		return fmt.Sprintf("%s@%d (%d seen)", h.Backend, h.Last, h.Seen)
	}
	return fmt.Sprintf("%s: %s", h.Backend, h.Reason)
}

// sourceState is the ingestor's private per-backend bookkeeping.
type sourceState struct {
	last     uint64
	expected uint64
	seen     int
	lastAt   string
	// gap is set once a discontinuity is observed and is NOT cleared by later
	// healthy frames.
	//
	// It stays set on purpose. A gap means observations were lost forever;
	// subsequent frames do not bring them back, and forgetting the fact would
	// let a supervisor resume as though it had seen a complete stream.
	gapFrom uint64
	gapTo   uint64
	gapped  bool
}

// JournalIngestor appends observations to the durable journal and tracks each
// source's continuity.
//
// It owns no decision. It records what arrived, what position it came from, and
// whether anything was lost; the Engine decides what any of that means.
type JournalIngestor struct {
	journal *runtime.Journal

	mu      sync.Mutex
	sources map[string]*sourceState
	now     func() time.Time
	logf    func(string)
}

// NewJournalIngestor builds an ingestor over an opened journal.
//
// now and logf may be nil; the ingestor is usable without them.
func NewJournalIngestor(j *runtime.Journal, now func() time.Time, logf func(string)) *JournalIngestor {
	if now == nil {
		now = time.Now
	}
	return &JournalIngestor{
		journal: j, sources: map[string]*sourceState{}, now: now, logf: logf,
	}
}

// Journal exposes the underlying log, for evidence and tests.
func (i *JournalIngestor) Journal() *runtime.Journal { return i.journal }

// Observe records one event, durably, BEFORE any decision is made from it.
//
// The append happens first and unconditionally. If the decision that consumes
// this observation then fails, the record is still there for the next run, and a
// crash here loses nothing that was not already undecided.
func (i *JournalIngestor) Observe(ev runtime.Event) (runtime.JournalRecord, error) {
	if i.journal == nil {
		return runtime.JournalRecord{}, fmt.Errorf("agent-engine: the journal is not open")
	}
	if ev.Backend == "" {
		// An untagged observation cannot be attributed, so it cannot be routed
		// or replayed safely. Refusing it here keeps the journal free of records
		// whose origin nothing can prove.
		return runtime.JournalRecord{}, fmt.Errorf("agent-engine: an observation with no backend tag cannot be journaled")
	}

	rec, err := i.journal.Append(ev)
	if err != nil {
		return runtime.JournalRecord{}, err
	}

	i.mu.Lock()
	st, ok := i.sources[ev.Backend]
	if !ok {
		st = &sourceState{}
		i.sources[ev.Backend] = st
	}
	positioned := ev.Cursor != 0 || ev.Kind != runtime.KindPromptResult
	if positioned && st.expected > 0 && ev.Cursor != st.expected {
		if !st.gapped {
			st.gapped = true
			st.gapFrom = st.expected
			st.gapTo = ev.Cursor
			i.logGap(ev.Backend, st.expected, ev.Cursor)
		}
	}
	if positioned {
		st.expected = ev.Cursor + 1
		if ev.Cursor > st.last {
			st.last = ev.Cursor
		}
	}
	st.seen++
	st.lastAt = i.now().UTC().Format(time.RFC3339)
	i.mu.Unlock()

	return rec, nil
}

// logGap reports a discontinuity exactly once per source.
func (i *JournalIngestor) logGap(backend string, from, to uint64) {
	if i.logf == nil {
		return
	}
	if to < from {
		i.logf(fmt.Sprintf("source %s replayed position %d after %d; observations are being re-delivered, not lost", backend, to, from))
		return
	}
	i.logf(fmt.Sprintf("source %s jumped from %d to %d: %d observation(s) will never be seen", backend, from, to, to-from))
}

// Health reports every known source, ordered by name so operator output is
// stable between runs.
func (i *JournalIngestor) Health() []SourceHealth {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]SourceHealth, 0, len(i.sources))
	for name, st := range i.sources {
		h := SourceHealth{
			Backend: name, Last: st.last, Expected: st.expected,
			Seen: st.seen, LastAt: st.lastAt, Healthy: true,
		}
		if st.gapped {
			h.Healthy = false
			h.GapFrom, h.GapTo = st.gapFrom, st.gapTo
			if st.gapTo < st.gapFrom {
				h.Reason = fmt.Sprintf("replayed positions after %d", st.gapFrom)
			} else {
				h.Reason = fmt.Sprintf("gap %d..%d: %d observation(s) lost", st.gapFrom, st.gapTo, st.gapTo-st.gapFrom)
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Backend < out[b].Backend })
	return out
}

// GappedSources returns the backends with a known discontinuity.
func (i *JournalIngestor) GappedSources() []SourceHealth {
	var out []SourceHealth
	for _, h := range i.Health() {
		if !h.Healthy {
			out = append(out, h)
		}
	}
	return out
}

// HasGap reports whether any source has lost observations.
func (i *JournalIngestor) HasGap() bool { return len(i.GappedSources()) > 0 }

// GapReason renders a block reason, or "" when there is nothing to report.
func (i *JournalIngestor) GapReason() string {
	gapped := i.GappedSources()
	if len(gapped) == 0 {
		return ""
	}
	parts := make([]string, 0, len(gapped))
	for _, h := range gapped {
		parts = append(parts, h.Backend+" "+h.Reason)
	}
	return "observation gaps block further decisions: " + joinParts(parts)
}

// Rehydrate restores per-source continuity from already-journaled records.
//
// It runs once, on start. Without it every source looks brand new, its first
// observation would register as a gap, and a clean restart would block itself.
func (i *JournalIngestor) Rehydrate(recs []runtime.JournalRecord) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, rec := range recs {
		st, ok := i.sources[rec.Source.Backend]
		if !ok {
			st = &sourceState{}
			i.sources[rec.Source.Backend] = st
		}
		positioned := rec.Source.Position != 0 || rec.Kind != runtime.KindPromptResult
		if positioned {
			if st.expected > 0 && rec.Source.Position != st.expected && !st.gapped {
				st.gapped = true
				st.gapFrom = st.expected
				st.gapTo = rec.Source.Position
			}
			st.expected = rec.Source.Position + 1
			if rec.Source.Position > st.last {
				st.last = rec.Source.Position
			}
		}
		st.seen++
	}
}

// joinParts renders a list without importing strings into this file's logic.
func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

// openJournal attaches a durable observation log to this request.
//
// It is opened, not created-on-demand, and an unreadable journal is fatal to the
// run. A supervisor that starts without its log would decide from whatever
// happens to be on the stream, which is exactly the evidence-free operation this
// control layer exists to prevent.
func (e *Engine) openJournal(requestID string) {
	dir, err := e.store.RequestDir(requestID)
	if err != nil {
		e.journalErr = err
		return
	}
	j, err := runtime.OpenJournal(dir, func() string { return e.now().UTC().Format(time.RFC3339) })
	if err != nil {
		e.journalErr = err
		return
	}
	e.journal = j
	e.journalDir = dir
	e.ingest = NewJournalIngestor(j, e.now, func(line string) { e.logf("%s", line) })
}

// ingestEvent journals one observation.
//
// The append happens BEFORE the decision reads it. A crash between the two
// leaves a record with no committed decision, which the next run replays; the
// reverse — a decision with no record — cannot happen here at all.
func (e *Engine) ingestEvent(ev runtime.Event) (runtime.JournalRecord, error) {
	if e.ingest == nil {
		return runtime.JournalRecord{}, fmt.Errorf("agent-engine: the observation journal is not open for this request")
	}
	return e.ingest.Observe(ev)
}

// replayUnconsumed returns journal records no committed decision has accounted
// for, and rehydrates source continuity from them.
//
// This is what makes the crash window survivable: an observation appended before
// the process died is replayed rather than lost, and per-source positions are
// restored so a clean restart does not look like a stream that lost everything.
func (e *Engine) replayUnconsumed(requestID string) ([]runtime.JournalRecord, error) {
	if e.journal == nil {
		return nil, fmt.Errorf("agent-engine: the observation journal is not open for this request")
	}
	res, err := e.journal.Replay()
	if err != nil {
		return nil, err
	}
	if err := res.VerifyChain(); err != nil {
		// A broken chain means the log cannot be trusted to say what was seen.
		// Continuing would decide from an unknown history.
		return nil, fmt.Errorf("agent-engine: the observation journal chain is broken, so this request cannot "+
			"resume: %w", err)
	}
	e.ingest.Rehydrate(res.Records)
	if res.Quarantined > 0 {
		e.logf("journal: %d record(s) were quarantined as unattributable and will not be replayed", res.Quarantined)
	}
	if res.Truncated {
		e.logf("journal: replay stopped at its bound; later records exist and were not examined")
	}
	return res.Unconsumed(), nil
}

// lastSequence reports the highest journal position written.
//
// It is read inside the decision transaction so the consumed position and the
// decision that accounts for it commit together.
func (i *JournalIngestor) lastSequence() uint64 {
	if i == nil || i.journal == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.journal.LastSequence()
}
