package control

// Assessment routing: one canonical policy for "the worker says it stopped".
//
// The problem this file exists to solve is not that the feature is missing, it is
// that implementing it twice would make it wrong twice. Agent Deck already opens
// a completion review from an implementer's finished event; a second
// transport must route through the SAME rule, or the two paths drift and the
// weaker one becomes the live one.
//
// The design is deliberately the existing Agent Deck design, unchanged:
//
//	worker asserts it stopped  ->  an independent reviewer is asked  ->  reviewer decides
//
// A worker's assertion is a REQUEST to be judged. It can never be the judgement.
// That is the property that keeps the separation of duties intact, and it is
// preserved here by routing both transports through one function rather than by
// adding a special case.
//
// What is NOT eligible: cancellation, refusal, errors and limit exhaustion. Those
// have their own recovery paths, and folding them into "the worker stopped"
// would turn a failure into a request for approval.

import (
	"fmt"
	"strings"
	"time"
)

// CompletionClaim is one worker's assertion that it stopped working.
//
// It is a claim. The fields exist so the assertion can be ATTRIBUTED and
// deduplicated, not so it can be believed.
type CompletionClaim struct {
	// Backend and SessionID identify which transport and which session made the
	// assertion. They are both required: a bare session id is only unique inside
	// its backend.
	Backend   string
	SessionID string
	// Generation is the worker generation the assertion was made under. A claim
	// from a retired generation cannot judge the one that replaced it.
	Generation int
	// Epoch is the connection/launch epoch, so a result cannot be replayed into a
	// later one.
	Epoch int
	// TurnID is the worker's own identity for the unit of work that stopped. It is
	// the deduplication key: a duplicate receipt, a replayed history entry, or a
	// restart must not produce a second review.
	TurnID string
	// Summary is the worker's own words. They are quoted to the reviewer and never
	// treated as evidence.
	Summary string
}

// OwnedProcess is one transport process this supervisor started.
//
// It is recorded at STARTUP rather than at activation, so a crash between
// "process up" and "binding committed" still leaves something a later run can
// find, name and reap.
type OwnedProcess struct {
	PID        int    `json:"pid"`
	StartToken string `json:"start_token,omitempty"`
	RecordedAt string `json:"recorded_at"`
	// Pending means a spawn was COMMITTED but no pid was ever recorded.
	//
	// It is the durable statement that a process may exist which nothing can
	// identify. Recovery must BLOCK on it rather than assume the spawn never
	// happened, because assuming so admits a replacement beside a live writer.
	Pending bool `json:"pending,omitempty"`
}

// AssessmentCandidate is a durable claim awaiting an independent judgement.
//
// It is retained rather than dropped when a review is already open, because the
// alternative is silently losing the only statement that the work stopped.
type AssessmentCandidate struct {
	CandidateID string `json:"candidate_id"`
	Backend     string `json:"backend"`
	SessionID   string `json:"session_id"`
	Generation  int    `json:"generation"`
	Epoch       int    `json:"epoch,omitempty"`
	TurnID      string `json:"turn_id"`
	Summary     string `json:"summary,omitempty"`
	RaisedAt    string `json:"raised_at"`
	// Consumed records that this candidate was turned into a review.
	//
	// It is what makes deduplication survive a restart: the record of the claim
	// is the record that it was already acted on.
	Consumed bool `json:"consumed,omitempty"`
}

// MaxAssessmentCandidates bounds the retained ledger.
//
// It is backpressure, not garbage collection: consumed entries stay until the
// bound is reached, so a request that keeps producing claims cannot push the
// evidence of what it already acted on off the end.
const MaxAssessmentCandidates = 16

// candidateID derives a deterministic identity from the claim.
//
// It is derived rather than random so that re-deriving the same claim produces
// the same identity, and a duplicate receipt cannot become a second review.
func candidateID(c CompletionClaim) string {
	return fmt.Sprintf("CA-%s-%s-%d-%s",
		shortBackend(c.Backend), c.SessionID, c.Generation, shortTurn(c.TurnID))
}

func shortBackend(b string) string {
	if len(b) > 12 {
		return b[:12]
	}
	return b
}

func shortTurn(t string) string {
	if len(t) > 16 {
		return t[len(t)-16:]
	}
	return t
}

// auditBudgetExhausted reports whether the operator's audit ceiling is spent.
//
// The ceiling is checked at ADMISSION for the limits a request asks for, but it
// was never enforced at RUNTIME against the attempts actually made. A worker
// that ended a turn repeatedly — each with its own send id, so each a distinct
// candidate — could therefore open unbounded completion audits, each costing a
// reviewer turn. Spending a budget the operator set is not the same as working
// within it.
//
// A zero ceiling means "no limit configured", which is the historical meaning of
// an unset value everywhere else in this system.
func (s *State) auditBudgetExhausted() bool {
	ceiling := s.Limits.AuditAttempts
	return ceiling > 0 && s.AuditAttempts >= ceiling
}

// RequestCompletionAssessment records a worker's claim and, when nothing else is
// owed, opens the independent completion review for it.
//
// It returns the instruction to send and whether a review was opened. When a
// review is already active the claim is RETAINED and returned for later, never
// discarded and never allowed to supersede a judgement already in flight.
func (s *State) RequestCompletionAssessment(claim CompletionClaim, now time.Time) (Effect, bool, error) {
	if strings.TrimSpace(claim.Backend) == "" || strings.TrimSpace(claim.SessionID) == "" {
		return Effect{}, false, fmt.Errorf("agent-engine: a completion claim must name the backend and session it came from")
	}
	if strings.TrimSpace(claim.TurnID) == "" {
		return Effect{}, false, fmt.Errorf("agent-engine: a completion claim must name the unit of work that stopped")
	}
	if s.CancellationRequested {
		// Cancellation has priority. A worker that stops while a cancel is pending
		// is not asking to be assessed; it is being stopped.
		return Effect{}, false, nil
	}
	if s.Stage.Terminal() {
		return Effect{}, false, nil
	}
	if claim.Generation != 0 && claim.Generation != s.Generation {
		// A claim explicitly scoped to a retired generation describes work that no
		// longer exists.
		return Effect{}, false, nil
	}

	id := candidateID(claim)
	for _, existing := range s.PendingAssessments {
		if existing.CandidateID == id {
			// Already seen and already consumed. A duplicate receipt, a replayed
			// history entry, or a restart must not produce a second review.
			return Effect{}, false, nil
		}
	}

	cand := AssessmentCandidate{
		CandidateID: id, Backend: claim.Backend, SessionID: claim.SessionID,
		Generation: claim.Generation, Epoch: claim.Epoch, TurnID: claim.TurnID,
		Summary: claim.Summary, RaisedAt: now.UTC().Format(time.RFC3339),
	}

	// An owed review is never replaced. A repeated claim after the review was
	// opened would otherwise supersede a judgement already in flight, and the
	// audit that actually mattered would be silently swapped for a newer one.
	if s.ActiveReview != nil {
		s.retainAssessment(cand)
		return Effect{}, false, nil
	}
	if s.auditBudgetExhausted() {
		// The claim is RETAINED and NO error is returned. An error would roll this
		// transaction back, discarding the retention we just wrote — the same trap
		// that lost claims when no reviewer was bound. The budget stops the
		// assessment; it does not erase the evidence that one is owed.
		s.retainAssessment(cand)
		return Effect{}, false, nil
	}
	if s.AuditorSession == "" {
		// Nothing can judge this yet, so no review is opened. The claim is
		// RETAINED rather than reported as an error: an error would roll this
		// transaction back, discarding the retention, and the claim is the only
		// outstanding statement that the work stopped.
		s.retainAssessment(cand)
		return Effect{}, false, nil
	}

	s.AuditAttempts++
	reviewID := fmt.Sprintf("A%d", s.AuditAttempts)
	review := NewActiveReview(ReviewCompletion, reviewID, s.AuditorSession, s.Generation, s.AuditAttempts)
	review.RequestedAt = cand.RaisedAt

	// The review is ASSIGNED BEFORE the instruction is built, and the order is
	// load-bearing rather than stylistic.
	//
	// reviewAnswerInstruction reads s.ActiveReview to stamp the review id, the
	// generation and the correlation nonce into the outgoing envelope. Building
	// the message first therefore either stamps the PREVIOUS review's identity —
	// so Codex answers a review that is not the one being asked about — or, on a
	// first candidate where no review is active, emits no envelope at all and the
	// reviewer has nothing to correlate its answer with.
	s.ActiveReview = review
	// The model the review is asked of is taken from the PINNED profile, so an
	// operator editing configuration afterwards cannot change which model an
	// in-flight audit is judged to have come from.
	s.ActiveReview.ExpectedModel = s.pinnedModelFor(RoleReviewer)

	eff := Effect{
		SessionID: s.AuditorSession,
		Role:      "auditor",
		Message:   completionAuditPrompt(s, claim.Summary) + reviewAnswerInstruction(s),
		Reason:    "worker asserted completion on " + claim.Backend + "; an independent audit is owed",
		CreatedAt: cand.RaisedAt,
	}
	// The candidate is recorded AND consumed in the same transaction as the
	// review it justified, so a crash cannot leave a review owed for a claim
	// nobody remembers, or a claim remembered for a review that was never opened.
	s.PendingAssessments = append(s.PendingAssessments, cand)
	s.PendingAssessments[len(s.PendingAssessments)-1].Consumed = true

	if s.Stage == StageReady {
		if err := s.transition(StageWorking, "worker asserted completion", cand.RaisedAt); err != nil {
			return Effect{}, false, err
		}
	}
	return eff, true, nil
}

// retainAssessment records a candidate for later reconsideration, bounded.
func (s *State) retainAssessment(cand AssessmentCandidate) {
	s.PendingAssessments = append(s.PendingAssessments, cand)
	if len(s.PendingAssessments) > MaxAssessmentCandidates {
		// Only CONSUMED entries are eligible for removal. An unconsumed candidate
		// is the only outstanding statement that the work stopped, and dropping it
		// to make room is how a request quietly loses its completion.
		kept := make([]AssessmentCandidate, 0, MaxAssessmentCandidates)
		drop := len(s.PendingAssessments) - MaxAssessmentCandidates
		for _, c := range s.PendingAssessments {
			if drop > 0 && c.Consumed {
				drop--
				continue
			}
			kept = append(kept, c)
		}
		s.PendingAssessments = kept
	}
}

// PromoteAssessment opens the review owed by the oldest retained candidate.
//
// It is called after a review settles, so a claim that arrived while another
// judgement was in flight is still acted on **even if no further worker event
// ever arrives**. That is the whole point of retaining it: the alternative is a
// candidate that waits forever for a notification that may never come.
func (s *State) PromoteAssessment(now time.Time) (Effect, bool, error) {
	if s.ActiveReview != nil || s.CancellationRequested || s.Stage.Terminal() {
		return Effect{}, false, nil
	}
	if s.AuditorSession == "" || s.auditBudgetExhausted() {
		// Exhausted budget stops promotion exactly as it stops a fresh claim.
		// Promoting a retained candidate is still an audit attempt, and an audit
		// attempt is what the budget bounds.
		return Effect{}, false, nil
	}
	for i, cand := range s.PendingAssessments {
		if cand.Consumed {
			continue
		}
		// Freshness is revalidated before dispatch. A candidate raised for a
		// generation that has since been replaced describes work that no longer
		// exists, and judging it would be judging a fiction.
		if cand.Generation != s.Generation {
			s.PendingAssessments[i].Consumed = true
			continue
		}
		s.AuditAttempts++
		reviewID := fmt.Sprintf("A%d", s.AuditAttempts)
		review := NewActiveReview(ReviewCompletion, reviewID, s.AuditorSession, s.Generation, s.AuditAttempts)
		review.RequestedAt = now.UTC().Format(time.RFC3339)
		// Assigned before the message is built, for the same reason as above.
		s.ActiveReview = review
		s.ActiveReview.ExpectedModel = s.pinnedModelFor(RoleReviewer)
		eff := Effect{
			SessionID: s.AuditorSession,
			Role:      "auditor",
			Message:   completionAuditPrompt(s, cand.Summary) + reviewAnswerInstruction(s),
			Reason:    "retained completion candidate promoted after the previous review settled",
			CreatedAt: review.RequestedAt,
		}
		s.PendingAssessments[i].Consumed = true
		if s.Stage == StageReady {
			if err := s.transition(StageWorking, "a retained completion candidate is now owed", review.RequestedAt); err != nil {
				return Effect{}, false, err
			}
		}
		return eff, true, nil
	}
	return Effect{}, false, nil
}

// pinnedModelFor reports the model pinned for a role, or "" when none was named.
//
// It reads the PINNED profiles rather than the live configuration on purpose: an
// in-flight review was opened against a specific identity, and re-reading
// configuration at judgement time is how an audit ends up evaluated against a
// model it was never asked of.
func (s *State) pinnedModelFor(role ProfileRole) string {
	if ref, ok := s.ExecutionProfiles[role]; ok {
		return ref.Model
	}
	return ""
}

// UnconsumedAssessmentCandidates reports claims still waiting for a judgement.
func (s *State) UnconsumedAssessmentCandidates() []AssessmentCandidate {
	var out []AssessmentCandidate
	for _, c := range s.PendingAssessments {
		if !c.Consumed {
			out = append(out, c)
		}
	}
	return out
}
