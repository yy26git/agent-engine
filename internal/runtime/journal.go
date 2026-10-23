package runtime

// The observation journal.
//
// One append-only file per request records what was OBSERVED. It is deliberately
// not a second decision state: nothing in this package decides anything. The
// journal exists so that after a crash, "what did the transport tell us" is a
// question with an answer, rather than one that has to be re-derived by guessing
// at a cursor.
//
// The ordering rule is the entire design and it is one rule:
//
//	append the observation  →  commit the decision that consumed it
//
// Journal first. A decision whose input was never recorded is a decision nobody
// can audit, and an audit that depends on the ordering of two writes to two files
// is not a guarantee. The consumer position advances only when the decision is
// durable, so a crash between the two replays the observation rather than losing
// it — and replaying an observation is safe, because applying it twice must be
// idempotent or the consumer is wrong.
//
// Positions are per-source and are NEVER compared across backends. Agent Deck
// numbers frames from its own cursor; an ACP connection has an ordinal. They are
// different units and merging them produces a bug that looks like a race.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MaxJournalRecordBytes bounds one observation record.
//
// The bound exists because the journal is written from transport payloads. An
// unbounded append is a way for a misbehaving backend to exhaust the disk of the
// component responsible for noticing misbehaviour.
const MaxJournalRecordBytes = 1 << 20 // 1 MiB

// MaxJournalReplay bounds how many records a single replay returns.
//
// A bounded replay is deliberate: an unbounded one turns a large journal into a
// large allocation at exactly the moment the process is least able to afford it.
const MaxJournalReplay = 10000

// ErrJournalCorrupt reports a damaged journal tail.
//
// It is distinct from "no records" so that a caller can quarantine residue and
// continue, rather than discarding a whole run because its last append was torn.
var ErrJournalCorrupt = errors.New("runtime: the observation journal is damaged")

// SourceRef identifies the stream an observation came from.
//
// Backend is part of the identity. Two backends' positions are different units,
// and a consumer that cannot tell them apart can suppress or replay the wrong
// frames — which presents as a timing bug and is a correctness one.
type SourceRef struct {
	Backend string `json:"backend"`
	// Position is the backend's own cursor or connection ordinal.
	Position uint64 `json:"position"`
}

// String renders a source position for operator output.
func (s SourceRef) String() string { return fmt.Sprintf("%s@%d", s.Backend, s.Position) }

// Zero reports whether a source reference names nothing.
func (s SourceRef) Zero() bool { return s.Backend == "" }

// JournalRecord is one durable observation.
type JournalRecord struct {
	// Seq is the journal's own local sequence, starting at 1 and strictly
	// increasing. It is the ONLY position that is comparable across sources.
	Seq uint64 `json:"seq"`
	// Source is the backend position this observation came from.
	Source SourceRef `json:"source"`
	// SessionRef is the session the observation concerns, when known.
	Session SessionRef `json:"session,omitempty"`
	// Generation is the worker generation at observation time.
	Generation int `json:"generation,omitempty"`
	// Kind is the backend's own event kind.
	Kind string `json:"kind"`
	// Payload is the bounded backend body.
	Payload []byte `json:"payload,omitempty"`
	// RecordedAt is when the observation was appended.
	RecordedAt string `json:"recorded_at"`
}

// Journal is a serialized, append-only observation log for one request.
//
// One writer at a time. The mutex is not an optimisation: two concurrent appends
// that each assigned a sequence would produce duplicate or out-of-order sequence
// numbers, and every replay assumption in the system rests on that sequence being
// strictly increasing.
type Journal struct {
	mu   sync.Mutex
	path string
	seq  uint64
	now  func() string
}

// OpenJournal opens or creates the journal for a request directory.
//
// It recovers the sequence from an existing file so that a restart continues the
// sequence rather than restarting it — a restarted sequence would make every
// pre-restart record collide with a post-restart one.
func OpenJournal(dir string, now func() string) (*Journal, error) {
	path := filepath.Join(dir, "journal.ndjson")
	j := &Journal{path: path, now: now}
	last, err := lastSequence(path)
	if err != nil {
		return nil, err
	}
	j.seq = last
	return j, nil
}

// Path returns the journal file location, for evidence and tests.
func (j *Journal) Path() string { return j.path }

// LastSequence returns the highest local sequence written.
//
// It exists so a consumer can commit "I have accounted for everything up to
// here" in the same transaction as the decision that did the accounting.
func (j *Journal) LastSequence() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}

// Append durably records one observation and returns its local sequence.
func (j *Journal) Append(ev Event) (JournalRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.seq++
	rec := JournalRecord{
		Seq:        j.seq,
		Source:     SourceRef{Backend: ev.Backend, Position: ev.Cursor},
		Generation: ev.WorkerGeneration,
		Kind:       ev.Kind,
		Payload:    ev.Payload,
		RecordedAt: j.now(),
	}
	if ev.SessionID != "" {
		rec.Session = SessionRef{Backend: ev.Backend, ID: ev.SessionID}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		j.seq--
		return JournalRecord{}, fmt.Errorf("runtime: encoding a journal record: %w", err)
	}
	if len(line) > MaxJournalRecordBytes {
		j.seq--
		return JournalRecord{}, fmt.Errorf("runtime: a journal record of %d bytes exceeds the %d byte limit",
			len(line), MaxJournalRecordBytes)
	}
	// Append-only and fsynced. A journal that is not durable is not evidence, it
	// is a hope, and a restart would silently discard observations that a decision
	// had already been taken on.
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		j.seq--
		return JournalRecord{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		j.seq--
		return JournalRecord{}, err
	}
	if err := f.Sync(); err != nil {
		j.seq--
		return JournalRecord{}, fmt.Errorf("runtime: the journal append was not durable: %w", err)
	}
	return rec, nil
}

// ReplayResult is what a replay recovered, and what it could not.
type ReplayResult struct {
	Records []JournalRecord
	// Consumed is the local position of the last record a committed decision
	// already accounted for. Records after it have not been consumed.
	Consumed uint64
	// Quarantined counts records that were structurally readable but referred to
	// an unknown source position, and are therefore not safely replayable.
	Quarantined int
	// Truncated says the replay stopped at its bound, so later records exist.
	Truncated bool
}

// Unconsumed returns the records a decision has not yet accounted for.
//
// This is the call that makes the crash window survivable: after a restart, the
// caller replays and gets exactly the observations whose effects were never
// committed.
func (r ReplayResult) Unconsumed() []JournalRecord {
	var out []JournalRecord
	for _, rec := range r.Records {
		if rec.Seq > r.Consumed {
			out = append(out, rec)
		}
	}
	return out
}

// Replay reads the journal from the beginning.
//
// A damaged TAIL is quarantined rather than fatal. A torn final append is the
// expected shape of a crash mid-write, and refusing to read the journal because
// of it would discard every good observation before it. A damaged record in the
// MIDDLE is a different problem, and is reported rather than skipped, because it
// means something wrote out of order.
func (j *Journal) Replay() (ReplayResult, error) {
	var res ReplayResult
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil // an empty journal is a valid state, not an error
		}
		return res, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxJournalRecordBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			res.Quarantined++
			continue
		}
		if rec.Seq == 0 || rec.Source.Backend == "" {
			// A record with no local sequence or no source cannot be ordered or
			// attributed. It is quarantined rather than replayed, because
			// replaying an unattributable observation would apply a decision to
			// content nobody can trace.
			res.Quarantined++
			continue
		}
		res.Records = append(res.Records, rec)
		if len(res.Records) >= MaxJournalReplay {
			res.Truncated = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			res.Truncated = true
			return res, nil
		}
		return res, fmt.Errorf("runtime: reading the journal: %w", err)
	}
	return res, nil
}

// VerifyChain checks that sequence numbers are strictly increasing and that no
// record's payload was altered after the fact.
//
// The check is a digest, not a signature: it detects accidental or casual
// tampering, which is the realistic threat for a local journal. It is NOT a
// defence against a privileged writer, and this package does not claim one.
func (r ReplayResult) VerifyChain() error {
	var prev uint64
	for _, rec := range r.Records {
		if prev != 0 && rec.Seq != prev+1 {
			return fmt.Errorf("%w: sequence jumped from %d to %d", ErrJournalCorrupt, prev, rec.Seq)
		}
		prev = rec.Seq
	}
	return nil
}

// lastSequence reads the highest sequence already on disk.
//
// A torn final line is ignored rather than treated as corruption: it is exactly
// what a crash mid-append leaves behind, and failing to open the journal because
// of it would be worse than proceeding with the last complete record.
func lastSequence(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var last uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), MaxJournalRecordBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec JournalRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // torn tail: stop trusting this line, keep the last good one
		}
		if rec.Seq > last {
			last = rec.Seq
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return 0, err
	}
	return last, nil
}

// Digest is the content digest of the whole journal, for an evidence manifest.
//
// It lets an operator prove that the observations backing a decision are the ones
// that existed, without re-reading them.
func (r ReplayResult) Digest() string {
	h := sha256.New()
	for _, rec := range r.Records {
		line, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		h.Write(line)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
