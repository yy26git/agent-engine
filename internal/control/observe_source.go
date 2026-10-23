package control

// Following a second transport's observations.
//
// The engine's decision path was written around one stream. With two backends,
// the choice is between two failures: ignore the second stream (a live ACP worker
// is watched by nobody) or merge cursors (an Agent Deck cursor and an ACP
// connection ordinal are different units, and merging them produces a number
// shaped like a position that is not one).
//
// This file takes the first failure and declines the second. Each backend is
// followed separately, in its own units, and folded into the supervision CLOCK —
// which is the one thing that is genuinely transport-neutral.
//
// What this deliberately does NOT do is treat an ACP turn as work completion.
// Agent Deck's `session.finished` says an implementer stopped working; ACP's
// `end_turn` says a conversational turn ended, which may contain a refusal, a
// partial fix, or a question. Only the former may open a completion audit, and
// only Agent Deck produces it. An ACP turn that ended charges its time and stops
// charging. That is supervision; it is not a verdict.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// observationWatermarks holds the last charged instant PER SOURCE.
//
// One shared watermark would be a bug with a plausible shape: Agent Deck's clock
// and an ACP turn are independent timelines, and an interval bounded by two
// observations from different transports is not an interval at all. Charging it
// would manufacture debt or erase it, neither of which happened.
type observationWatermarks struct {
	mu sync.Mutex
	at map[string]time.Time
}

func newObservationWatermarks() *observationWatermarks {
	return &observationWatermarks{at: map[string]time.Time{}}
}

// advance returns the previous instant for a source and records a new one.
//
// It reports ok=false on the first observation of a source, because an interval
// that starts unobserved is not charged at all: nothing is known about what
// happened between the previous run and this frame.
func (w *observationWatermarks) advance(backend string, now time.Time) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev, seen := w.at[backend]
	w.at[backend] = now
	return prev, seen
}

// followBackend consumes one backend's stream for the life of ctx.
//
// Every observation is journaled before it is used, so a crash between the two
// replays rather than loses. Only observations about the request's OWN worker
// move the clock; another session's traffic is recorded and ignored, exactly as
// the single-stream path treats frames for other sessions.
func (e *Engine) followBackend(ctx context.Context, requestID string, backend runtime.Backend, marks *observationWatermarks) {
	after, err := e.resumePosition(requestID, backend.Name())
	if err != nil {
		e.logf("%s: cannot determine a resume position, so its stream is not followed: %v", backend.Name(), err)
		return
	}

	// The position is committed BEFORE following, so a crash after the first
	// observation cannot restart the stream from the beginning and charge the
	// request for work that predates this run.
	if _, perr := e.store.UpdateCommitted(requestID, func(s *State) error {
		if s.IngestedPositions == nil {
			s.IngestedPositions = map[string]uint64{}
		}
		s.IngestedPositions[backend.Name()] = after
		return nil
	}); perr != nil {
		e.logf("%s: the resume position could not be recorded: %v", backend.Name(), perr)
		return
	}

	frames, errs, ferr := backend.Events(ctx, after)
	if ferr == nil {
		e.followReceipts(ctx, requestID, backend, marks, errs)
	}
	if ferr != nil {
		// Not fatal to the run. The Agent Deck path still supervises, and this is
		// recorded rather than hidden, because a supervisor that cannot see a
		// worker must not appear to be watching it.
		e.logf("%s: observations are unavailable, so this transport is not supervised: %v", backend.Name(), ferr)
		return
	}

	e.followerWG.Add(1)
	go func() {
		defer e.followerWG.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.followerDone:
				return
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if err != nil {
					e.logf("%s: observation stream reported: %v", backend.Name(), err)
				}
			case ev, ok := <-frames:
				if !ok {
					// The stream ending says the transport is no longer OBSERVABLE.
					// It says nothing about the work, and must not be read as the
					// worker having finished.
					e.logf("%s: observation stream ended; this transport is no longer observable", backend.Name())
					return
				}
				e.applyObservation(ctx, requestID, backend, ev, marks)
			}
		}
	}()
}

// applyObservation journals one observation and folds it into the clock.
func (e *Engine) applyObservation(ctx context.Context, requestID string, backend runtime.Backend, ev runtime.Event, marks *observationWatermarks) {
	if _, err := e.ingestEvent(ev); err != nil {
		e.logf("%s: observation at position %d could not be journaled: %v", backend.Name(), ev.Cursor, err)
		return
	}
	// The ingested position advances with the journal, never separately: a
	// position that ran ahead of what was recorded would skip observations on the
	// next start, which is the one loss that cannot be recovered.
	if ev.Cursor > 0 {
		if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
			if s.IngestedPositions == nil {
				s.IngestedPositions = map[string]uint64{}
			}
			if ev.Cursor > s.IngestedPositions[backend.Name()] {
				s.IngestedPositions[backend.Name()] = ev.Cursor
			}
			return nil
		}); err != nil {
			e.logf("%s: the ingested position could not be recorded: %v", backend.Name(), err)
			return
		}
	}
	if reason := e.ingest.GapReason(); reason != "" {
		e.logf("%s", reason)
		return
	}

	// A CORRELATED prompt receipt is handled before the notification classifier,
	// because it — not a notification — is how a normal turn end actually arrives.
	// It is also the ONLY thing eligible to request an assessment.
	if ev.Kind == runtime.KindPromptResult {
		e.applyPromptReceipt(ctx, requestID, backend, ev)
		return
	}

	obs, classified := ClassifyACPNotification(ev.Kind, ev.Payload)
	if !classified {
		return
	}

	// Only the request's own worker moves the clock. Anything else — another
	// session, another request — is recorded and otherwise ignored.
	st, err := e.store.State(requestID)
	if err != nil {
		return
	}
	// The role is resolved from the STATE's own binding, not from the session id
	// string: a bare id is only unique inside its backend, and guessing the role
	// from it is how a reviewer's traffic charges the worker's clock.
	binding, bound := st.BackendForID(ev.SessionID)
	if !bound || binding.Backend != backend.Name() {
		return
	}
	if ev.SessionID != st.WorkerSession {
		return
	}
	if st.Stage.Terminal() {
		return
	}

	now := e.now().UTC()
	working := obs == ACPObsActivity
	prev, seen := marks.advance(backend.Name(), now)
	if !seen || !now.After(prev) {
		return
	}

	if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
		s.Checkpoint.Charge(ActiveWork{From: prev, To: now, Paused: !working})
		if working {
			s.Checkpoint.Observe(now)
		}
		return nil
	}); err != nil {
		e.logf("%s: active time could not be charged: %v", backend.Name(), err)
		return
	}

	// A terminal ACP turn is recorded as the end of a TURN and nothing more. No
	// completion audit is opened here, and that omission is the design: a
	// finished turn is not finished work.
	if obs.Terminal() {
		e.logf("%s: worker turn ended (%s); this is not a completion and opens no audit", backend.Name(), obs)
	}
}

// receiptStream is the optional second observation channel a backend may offer.
//
// It exists because a backend may deliver correlated receipts on a stream of
// their own. Asserting for an interface here would be the pattern this codebase
// deliberately avoids: an optional capability must be asked for, not inferred
// from whether a type happens to satisfy something.
type receiptStream interface {
	Receipts() <-chan runtime.Event
}

// followReceipts consumes a backend's dedicated receipt stream, when it has one.
//
// A receipt that is only ever delivered on the general observation stream can be
// starved by the high-volume traffic that stream also carries, which is how a
// completion goes missing while everything still looks healthy.
func (e *Engine) followReceipts(ctx context.Context, requestID string, backend runtime.Backend, marks *observationWatermarks, srcErrs <-chan error) {
	rs, ok := backend.(receiptStream)
	if !ok {
		return
	}
	receipts := rs.Receipts()
	if receipts == nil {
		return
	}
	e.followerWG.Add(1)
	go func() {
		defer e.followerWG.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.followerDone:
				return
			case _, ok := <-srcErrs:
				if !ok {
					srcErrs = nil
				}
			case ev, ok := <-receipts:
				if !ok {
					return
				}
				ev.Cursor = 0 // receipts carry their own identity, not a stream position
				e.applyObservation(ctx, requestID, backend, ev, marks)
			}
		}
	}()
}

// applyPromptReceipt turns a correlated terminal receipt into an assessment
// candidate.
//
// Eligibility is deliberately NARROW: only a normal, attributable end_turn. A
// refusal, an error, a cancelled turn, an unknown stop reason and limit
// exhaustion each keep their own recovery path. Folding them in here would turn
// a failure into a request for approval, which is the one thing this control
// layer must never do.
func (e *Engine) applyPromptReceipt(ctx context.Context, requestID string, backend runtime.Backend, ev runtime.Event) {
	var rec struct {
		SessionID  string `json:"sessionId"`
		SendID     string `json:"sendId"`
		StopReason string `json:"stopReason"`
		Text       string `json:"text"`
		Model      string `json:"model"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal(ev.Payload, &rec); err != nil {
		e.logf("%s: a prompt receipt could not be decoded, so it is not a candidate: %v", backend.Name(), err)
		return
	}
	if rec.SendID == "" {
		// An uncorrelated receipt cannot be deduplicated, and an undeduplicable
		// claim would let a duplicate open a second audit.
		e.logf("%s: a prompt receipt carries no send id, so it is not a candidate", backend.Name())
		return
	}
	if !isNormalTurnEnd(rec.StopReason) {
		// Recorded and ignored. Not an assessment candidate, and not an error.
		e.logf("%s: turn ended as %q, which is not a completion claim and keeps its own recovery path",
			backend.Name(), rec.StopReason)
		return
	}

	st, err := e.store.State(requestID)
	if err != nil {
		return
	}
	if st.Episode != nil {
		if err := e.retainEpisodeTurnReceipt(requestID, backend.Name(), rec.SessionID, rec.SendID, rec.Model, rec.Text, rec.Truncated); err != nil {
			e.logf("episode receipt refused: %v", err)
		}
		return
	}
	// Attribution is revalidated at the moment of use: a receipt for a session
	// this request does not own must not become its claim.
	binding, bound := st.BackendForID(rec.SessionID)
	if !bound || binding.Backend != backend.Name() || rec.SessionID != st.WorkerSession {
		return
	}

	// ONE transaction for the whole decision: revalidate the binding, open the
	// review, apply the audit budget, record the candidate's consumption, and
	// record the instruction. Splitting these is how a crash leaves either a
	// review owed for a claim nobody remembers, or a claim remembered for a
	// review that was never opened.
	if _, aerr := e.store.UpdateCommitted(requestID, func(s *State) error {
		// The binding is revalidated INSIDE the transaction. It was checked
		// outside, and a binding that changed in between would route a claim to a
		// generation that no longer owns it.
		cur, ok := s.BackendForID(rec.SessionID)
		if !ok || cur.Backend != backend.Name() || rec.SessionID != s.WorkerSession {
			return fmt.Errorf("agent-engine: the receipt's session is no longer this request's worker")
		}
		// Freshness is checked against the BINDING, not against a generation this
		// function supplies itself. A binding recorded under an earlier generation
		// describes a worker that has already been replaced, and a receipt for it
		// must not become a claim about the current one.
		if cur.Generation != 0 && cur.Generation != s.Generation {
			return fmt.Errorf("agent-engine: the worker binding is from generation %d, current is %d", cur.Generation, s.Generation)
		}
		eff, opened, aerr := s.RequestCompletionAssessment(CompletionClaim{
			Backend:    backend.Name(),
			SessionID:  rec.SessionID,
			Generation: s.Generation,
			Epoch:      cur.Epoch,
			TurnID:     rec.SendID,
			Summary:    "the worker reported the end of a turn on " + backend.Name(),
		}, e.now())
		if aerr != nil {
			return aerr
		}
		if !opened || eff.SessionID == "" {
			// A normal turn end that opened nothing was RETAINED rather than
			// dropped. Saying so is the difference between an operator seeing an
			// outstanding claim and seeing silence.
			//
			// It is also the moment queued guidance becomes deliverable: the turn
			// has ended, so a prompt is legal again.
			return e.promoteGuidanceNow(s, backend.Name())
		}
		return s.RecordEffect(eff)
	}); aerr != nil {
		e.logf("%s: the completion candidate was refused: %v", backend.Name(), aerr)
		return
	}
	if st, serr := e.store.State(requestID); serr == nil && st.ActiveReview != nil &&
		st.ActiveReview.Kind == ReviewCompletion {
		e.logf("%s: turn end recorded as an assessment candidate; an independent review is owed", backend.Name())
	}
}

// promoteGuidanceNow delivers one queued instruction, having established that a
// prompt is legal and wanted.
//
// The caller has already committed the receipt, so the turn is over and the
// worker is idle. Two things still block delivery and both mean "not now": an
// active review (the worker is owed an answer) and a completion claim (more work
// would contradict what the worker just asserted).
func (e *Engine) promoteGuidanceNow(s *State, backend string) error {
	g, ok := s.PromoteGuidanceAtTurnBoundary(e.now().UTC().Format(time.RFC3339))
	if !ok {
		return nil
	}
	reason := g.Reason
	if reason == "" {
		reason = "guidance queued at a safe turn boundary"
	}
	e.logf("%s: delivering queued guidance %s at the turn boundary (%s)", backend, g.GuidanceID, reason)
	return s.RecordEffect(Effect{
		SessionID: g.SessionID,
		Role:      "worker",
		Message:   g.Message,
		Reason:    reason,
		CreatedAt: e.now().UTC().Format(time.RFC3339),
	})
}

// isNormalTurnEnd reports whether a stop reason is the ordinary end of a turn.
//
// Everything else — refusal, error, cancellation, an unrecognised word — is not
// a claim that the work finished, and none of them may be treated as one.
func isNormalTurnEnd(stopReason string) bool {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "end_turn", "endturn":
		return true
	default:
		return false
	}
}

// resumePosition reports where to resume a backend's stream.
//
// It starts from the position already ingested for that backend rather than
// zero, because replaying a whole history into the clock would charge the
// request for work that happened before this run existed.
func (e *Engine) resumePosition(requestID string, backend string) (uint64, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return 0, err
	}
	if pos, ok := st.IngestedPositions[backend]; ok {
		return pos, nil
	}
	return 0, nil
}

// Role reports which role a binding serves, if the binding names one.
//
// The backend-neutral SessionBinding carries an ID and a backend, not a role, so
// this resolves the role from the state's own role map rather than inferring one
// from a session id.
func (b SessionBinding) Role() ProfileRole {
	switch b.SessionID {
	case "":
		return ""
	default:
		return ""
	}
}

var _ = fmt.Sprintf
