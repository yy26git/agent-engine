package control

// Obligations, checkpoints and the active-time clock.
//
// The problem this file exists to solve is that a supervising agent produces a
// great deal of signal that is not progress. Files change, commands go green,
// tokens are spent, statuses flip. None of that means an obligation advanced. A
// supervisor that treats activity as progress will nod along while the work goes
// nowhere, and the failure is invisible precisely because everything looks busy.
//
// So progress here means one thing: a reviewer, looking at attributable evidence,
// judged that a named criterion actually advanced. Everything else is an input to
// that judgement, never a substitute for it.
//
// The clock is deliberately split. "Active" time is the time a worker is believed
// to be executing toward the request; wall time includes the verifier, an external
// simulator and a human. Charging a worker for time a test spent compiling would
// manufacture a divergence that never happened, and not charging during unknown
// downtime would let a request that was dead for a week look like it was
// thinking. Unobserved downtime is therefore never charged.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxCheckpointSubmissions bounds the retained checkpoint receipts.
const MaxCheckpointSubmissions = 32

// MaxEvidenceRefs bounds the retained evidence index.
const MaxEvidenceRefs = 64

// ErrCheckpointDuplicate is returned when an identical checkpoint is submitted
// again. It is a success with a distinct name, because a retrying worker must be
// able to tell "already recorded" from "recorded now".
var ErrCheckpointDuplicate = errors.New("agent-engine: checkpoint already recorded with identical content")

// ErrCheckpointStale is returned when a checkpoint refers to a superseded
// checkpoint ID, generation or snapshot.
var ErrCheckpointStale = errors.New("agent-engine: checkpoint is stale and was refused without mutation")

// ErrCheckpointEmpty is returned when a submission carries no claim or action.
var ErrCheckpointEmpty = errors.New("agent-engine: a checkpoint must state a claim or a next action")

// EvidenceRef points at one immutable, digest-pinned evidence item.
type EvidenceRef struct {
	// ID is the request-local evidence identifier.
	ID string `json:"id"`
	// Path is the artifact location.
	Path string `json:"path,omitempty"`
	// SHA256 pins the bytes. An evidence ref without a digest is a pointer to
	// something that may since have changed, which is not evidence.
	SHA256 string `json:"sha256"`
	// Kind classifies how it was produced.
	Kind string `json:"kind,omitempty"`
}

// NegativeClaim is a worker's assertion that something is absent.
//
// The structure forces the discipline the plan requires: a claim carries its
// scope, what was actually searched, and the evidence behind each search. A claim
// with no query record is not a negative claim, it is an assumption — and an
// unchecked assumption is how a worker convinces a reviewer that a bug does not
// exist.
type NegativeClaim struct {
	ClaimID string `json:"claim_id"`
	// Subject is what is claimed absent.
	Subject string `json:"subject"`
	// Scope is the exact bounded region searched, ideally with a revision.
	Scope string `json:"scope"`
	// Queries are the actual searches performed.
	Queries []string `json:"queries,omitempty"`
	// Alternatives are the other names/interfaces that were considered.
	Alternatives []string `json:"alternatives,omitempty"`
	// Locations are the files, packages and call sites inspected.
	Locations []string `json:"locations,omitempty"`
	// Conclusion is UNKNOWN unless every field above is populated. Software
	// enforces this rather than trusting the worker to self-assess.
	Conclusion string `json:"conclusion"`
}

// Negative-claim conclusions.
const (
	// ClaimAbsent is a supported conclusion: the scope was searched and the
	// subject really was not found.
	ClaimAbsent = "ABSENT"
	// ClaimPresent is a supported conclusion: the subject was found.
	ClaimPresent = "PRESENT"
	// ClaimUnknown is the only permitted conclusion for an incomplete search.
	//
	// This is the important one. An incomplete search is UNKNOWN, and software
	// refuses to record anything else, because "I did not look properly" and "it
	// is not there" are very different statements with the same words.
	ClaimUnknown = "UNKNOWN"
)

// CheckpointReceipt is one accepted worker submission.
type CheckpointReceipt struct {
	// CheckpointID is the request-local identifier. A duplicate ID with different
	// content is a conflict, not a duplicate.
	CheckpointID string `json:"checkpoint_id"`
	Sequence     int    `json:"sequence"`
	Generation   int    `json:"generation"`
	// Claim is what the worker says it is working on.
	Claim string `json:"claim"`
	// NextAction is what it proposes to do next.
	NextAction string `json:"next_action"`
	// Blocker is what prevents progress, if anything.
	Blocker string `json:"blocker,omitempty"`
	// EvidenceRefs are the immutable items this rests on.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	// NegativeClaims are absence assertions requiring review.
	NegativeClaims []NegativeClaim `json:"negative_claims,omitempty"`
	// Digest is the canonical hash of the submission, used to tell a genuine
	// duplicate from a conflicting reuse of the same ID.
	Digest     string `json:"digest"`
	AcceptedAt string `json:"accepted_at"`
	// Reviewed is true once a reviewer has judged it. Until then the submission
	// has advanced nothing.
	Reviewed bool   `json:"reviewed"`
	Outcome  string `json:"outcome,omitempty"`
}

// Checkpoint is the obligation-review bookkeeping for one request.
type Checkpoint struct {
	// Sequence counts accepted submissions.
	Sequence int `json:"sequence"`
	// Required is set by software before handing out a mutation instruction or
	// after a required managed command. A worker cannot clear it.
	Required bool `json:"required,omitempty"`
	// GraceSeconds is the active-time budget a required submission gets before a
	// review is scheduled. It comes from operator policy, never from the worker.
	GraceSeconds int `json:"grace_seconds,omitempty"`
	// DeadlineAt is the absolute wall deadline after which grace no longer helps.
	DeadlineAt string `json:"deadline_at,omitempty"`
	// DebtSeconds is accumulated active time since the last reviewed advance.
	//
	// It only advances while the worker is believed to be executing. Pauses
	// during verifier runs, external waits and review are recorded explicitly,
	// because a managed check has its own finite timeout and pausing forever
	// would hide a hung verifier behind a worker that is not actually running.
	DebtSeconds float64 `json:"debt_seconds"`
	// LastActivityAt is the last observation that confirmed the worker working.
	LastActivityAt string `json:"last_activity_at,omitempty"`
	// LastReviewedAt and LastReviewedEvidenceIDs record the previous reviewed
	// point, so a reviewer can be told what is new rather than re-reading history.
	LastReviewedAt          string   `json:"last_reviewed_at,omitempty"`
	LastReviewedEvidenceIDs []string `json:"last_reviewed_evidence_ids,omitempty"`
	// ReviewPending suppresses duplicate wakes while a review is outstanding.
	ReviewPending bool `json:"review_pending,omitempty"`
	// MutationGrantsPaused is set when a high-risk gate withholds the next
	// mutation grant until its checkpoint or lease releases.
	MutationGrantsPaused bool `json:"mutation_grants_paused,omitempty"`
	// Submissions is the bounded receipt log.
	Submissions []CheckpointReceipt `json:"submissions,omitempty"`
	// Evidence is the bounded immutable evidence index.
	Evidence []EvidenceRef `json:"evidence,omitempty"`
}

func (c Checkpoint) clone() Checkpoint {
	out := c
	if c.Submissions != nil {
		out.Submissions = make([]CheckpointReceipt, len(c.Submissions))
		for i, r := range c.Submissions {
			out.Submissions[i] = r
			if r.EvidenceRefs != nil {
				out.Submissions[i].EvidenceRefs = append([]string(nil), r.EvidenceRefs...)
			}
			if r.NegativeClaims != nil {
				out.Submissions[i].NegativeClaims = make([]NegativeClaim, len(r.NegativeClaims))
				for j, n := range r.NegativeClaims {
					out.Submissions[i].NegativeClaims[j] = n
					out.Submissions[i].NegativeClaims[j].Queries = append([]string(nil), n.Queries...)
					out.Submissions[i].NegativeClaims[j].Alternatives = append([]string(nil), n.Alternatives...)
					out.Submissions[i].NegativeClaims[j].Locations = append([]string(nil), n.Locations...)
				}
			}
		}
	}
	if c.Evidence != nil {
		out.Evidence = append([]EvidenceRef(nil), c.Evidence...)
	}
	if c.LastReviewedEvidenceIDs != nil {
		out.LastReviewedEvidenceIDs = append([]string(nil), c.LastReviewedEvidenceIDs...)
	}
	return out
}

// MaxDebtBeforeReview is the operator default active-time budget for a required
// checkpoint before a review is scheduled.
//
// It is a default, not a contract: local policy supplies it, and the plan is
// explicit that no universal ten-or-fifteen-minute threshold belongs in the
// contract. It is named here only so an operator policy that omits it still has
// a defined, documented behaviour.
const MaxDebtBeforeReview = 15 * 60

// CheckpointPolicy is the operator-supplied timing policy for checkpoints.
type CheckpointPolicy struct {
	// SoftActiveBudget is how much active time may accumulate without a reviewed
	// obligation advance before a review is scheduled.
	SoftActiveBudget time.Duration
	// Grace is the active-time allowance a required submission gets.
	Grace time.Duration
	// MaxWallDuration is the absolute ceiling on a request.
	MaxWallDuration time.Duration
	// ReviewTimeout bounds how long a review may remain owed.
	ReviewTimeout time.Duration
	// MinWakeInterval is the smallest gap between two reviewer wakes, so a busy
	// worker cannot cause a wake storm.
	MinWakeInterval time.Duration
}

// DefaultCheckpointPolicy returns a documented default policy.
func DefaultCheckpointPolicy() CheckpointPolicy {
	return CheckpointPolicy{
		SoftActiveBudget: MaxDebtBeforeReview * time.Second,
		Grace:            5 * time.Minute,
		MaxWallDuration:  4 * time.Hour,
		ReviewTimeout:    30 * time.Minute,
		MinWakeInterval:  time.Minute,
	}
}

// ActiveWork records one interval the worker was believed to be executing.
type ActiveWork struct {
	// From and To bound the interval. Zero-length intervals are ignored, so a
	// clock read that did not move cannot manufacture debt.
	From, To time.Time
	// Paused marks an interval that is explicitly not charged: a verifier run, an
	// external wait, a review, or a permission prompt.
	Paused bool
}

// Charge applies an observed interval to the active-time debt.
//
// Only unpaused intervals count, and only while mutation grants are live. The
// distinction is the whole point: charging a worker for a test's compile time
// manufactures a divergence that never happened, while charging it for a weekend
// during which the request was dead manufactures progress that never happened
// either.
func (c *Checkpoint) Charge(work ActiveWork) {
	if c.MutationGrantsPaused {
		return
	}
	d := work.To.Sub(work.From)
	if d <= 0 {
		return
	}
	if work.Paused {
		return
	}
	c.DebtSeconds += d.Seconds()
	c.LastActivityAt = work.To.UTC().Format(time.RFC3339)
}

// Observe records a confirmed working observation. It does not advance the debt
// on its own; it only marks the worker as seen.
func (c *Checkpoint) Observe(at time.Time) {
	c.LastActivityAt = at.UTC().Format(time.RFC3339)
}

// Due reports whether a review should be scheduled now.
//
// The rules, in order:
//
//   - A pending review suppresses a second one. Two outstanding reviews of the
//     same work produce two answers, and one of them is wasted or contradictory.
//   - A required submission past its grace, or past its absolute deadline, is due.
//     The deadline is an escape hatch: a generous grace must not be able to
//     postpone review forever.
//   - Active time past the soft budget, with nothing reviewed since, is due.
//
// Note what is deliberately absent: a mere file change, a new commit, a green
// command or a changed status never makes a review due on its own, because none
// of those is evidence that an obligation advanced.
func (c *Checkpoint) Due(policy CheckpointPolicy, now time.Time) (bool, string) {
	if c.ReviewPending {
		return false, ""
	}
	if c.DeadlineAt != "" {
		if deadline, err := time.Parse(time.RFC3339, c.DeadlineAt); err == nil && !now.Before(deadline) {
			return true, "the absolute request deadline has passed"
		}
	}
	if c.Required {
		grace := time.Duration(c.GraceSeconds) * time.Second
		if grace <= 0 {
			grace = policy.Grace
		}
		if grace > 0 && c.DebtSeconds >= grace.Seconds() {
			return true, "a required checkpoint is missing after its grace expired"
		}
	}
	if policy.SoftActiveBudget > 0 && c.DebtSeconds >= policy.SoftActiveBudget.Seconds() {
		return true, "active time passed the soft budget with no reviewed obligation advance"
	}
	return false, ""
}

// SubmitCheckpoint accepts a typed worker submission.
//
// It is idempotent by content: the same checkpoint ID carrying the same bytes is
// recorded once and reported as a duplicate, so a worker retrying after a
// timeout does not manufacture a second receipt. The same ID carrying different
// bytes is a conflict and is refused without mutation, because a worker reusing
// an identifier to mean something else has made the ID meaningless.
//
// Accepting a submission advances nothing. It records a claim for a reviewer to
// judge; only a reviewed outcome moves an obligation.
func (s *State) SubmitCheckpoint(rec CheckpointReceipt, digest string, now time.Time) (CheckpointReceipt, error) {
	id := strings.TrimSpace(rec.CheckpointID)
	if id == "" {
		return CheckpointReceipt{}, errors.New("agent-engine: checkpoint_id is required")
	}
	if strings.TrimSpace(rec.Claim) == "" && strings.TrimSpace(rec.NextAction) == "" {
		return CheckpointReceipt{}, ErrCheckpointEmpty
	}
	rec.CheckpointID = id
	rec.Digest = digest
	rec.Generation = s.Generation
	rec.AcceptedAt = now.UTC().Format(time.RFC3339)

	for _, existing := range s.Checkpoint.Submissions {
		if existing.CheckpointID != id {
			continue
		}
		if existing.Digest == digest {
			return existing, ErrCheckpointDuplicate
		}
		return CheckpointReceipt{}, fmt.Errorf("%w: %s was already recorded with different content",
			ErrCheckpointStale, id)
	}

	// A negative claim may only be recorded as a supported conclusion when the
	// search behind it is actually documented. Software decides this rather than
	// trusting the worker to self-assess: a claim that grades its own rigour is
	// exactly the claim that needs checking.
	//
	// A single query with no alternatives is not a search. It is one guess, and
	// absence established by one guess is not absence.
	for i := range rec.NegativeClaims {
		c := &rec.NegativeClaims[i]
		if strings.TrimSpace(c.ClaimID) == "" || strings.TrimSpace(c.Subject) == "" {
			return CheckpointReceipt{}, errors.New("agent-engine: a negative claim needs a claim_id and a subject")
		}
		documented := strings.TrimSpace(c.Scope) != "" &&
			len(c.Locations) > 0 &&
			(len(c.Alternatives) > 0 || len(c.Queries) > 1)
		if !documented {
			c.Conclusion = ClaimUnknown
		}
	}

	s.Checkpoint.Sequence++
	rec.Sequence = s.Checkpoint.Sequence
	s.Checkpoint.Submissions = append(s.Checkpoint.Submissions, rec)
	if len(s.Checkpoint.Submissions) > MaxCheckpointSubmissions {
		s.Checkpoint.Submissions = s.Checkpoint.Submissions[len(s.Checkpoint.Submissions)-MaxCheckpointSubmissions:]
	}
	// A submission satisfies a pending requirement. It does not reset the debt:
	// only a reviewed advance does, because a submission that is never judged has
	// advanced nothing.
	if s.Checkpoint.Required {
		s.Checkpoint.Required = false
	}
	return rec, nil
}

// RequireCheckpoint marks a checkpoint as owed, with its grace budget.
//
// The requirement is recorded before the instruction or command that causes it is
// handed out, so a worker cannot finish the work and then discover the
// requirement was never set.
func (s *State) RequireCheckpoint(graceSeconds int, deadline time.Time, now time.Time) {
	s.Checkpoint.Required = true
	s.Checkpoint.GraceSeconds = graceSeconds
	if !deadline.IsZero() {
		s.Checkpoint.DeadlineAt = deadline.UTC().Format(time.RFC3339)
	}
}

// AddEvidence records an immutable evidence item.
//
// A ref without a digest is refused: a path alone points at bytes that may since
// have changed, and a conclusion resting on it is not attributable to anything.
func (s *State) AddEvidence(ref EvidenceRef) error {
	if strings.TrimSpace(ref.ID) == "" {
		return errors.New("agent-engine: evidence needs an id")
	}
	if err := contractValidateDigest(ref.SHA256); err != nil {
		return fmt.Errorf("agent-engine: evidence %s: %w", ref.ID, err)
	}
	for _, existing := range s.Checkpoint.Evidence {
		if existing.ID == ref.ID {
			if existing.SHA256 == ref.SHA256 {
				return nil
			}
			return fmt.Errorf("agent-engine: evidence %s already recorded with a different digest", ref.ID)
		}
	}
	s.Checkpoint.Evidence = append(s.Checkpoint.Evidence, ref)
	if len(s.Checkpoint.Evidence) > MaxEvidenceRefs {
		s.Checkpoint.Evidence = s.Checkpoint.Evidence[len(s.Checkpoint.Evidence)-MaxEvidenceRefs:]
	}
	return nil
}

// ApplyReview records a reviewer's judgement against the review state itself
// holds.
//
// IT DOES NOT ADVANCE AN OBLIGATION. That is deliberate and it is the whole
// reason a completion PASS can now discharge work at all. This function used to
// blank every obligation to ADVANCED when a completion review answered PASS,
// which meant the store's own bookkeeping loop decided which obligations were
// satisfied without ever asking what the reviewer judged. A checkpoint or
// diagnostic review, routed through the same switch, could have done the same
// had its outcome allowlist ever moved.
//
// The single authority for discharge is Store.commitCompletion, which validates
// an explicit, evidence-backed judgement for every current obligation and
// commits the discharge, the review settlement and the result publication
// together. ApplyReview settles the review and records that it happened. What it
// no longer does is decide what the review MEANT.
//
// The caller supplies the review ID it is answering, not a copy of the review.
// The stored record is the authority for the review's kind and generation: if the
// caller had to echo those back, a caller that constructed a partial review
// object would either be silently validating against its own fiction or would
// fail on an unrelated field. A stale or wrong ID is still refused, and a
// generation that has since moved on is still caught — because the stored review
// is what is compared against the current state.
//
// Only a checkpoint review with a positive outcome and evidenced advancement
// resets the active-time debt. Nothing else does: a schema-valid submission, a
// new commit and a changed file are all still just activity.
func (s *State) ApplyReview(reviewID string, outcome ReviewOutcome, note string, now time.Time) error {
	if strings.TrimSpace(reviewID) == "" {
		return fmt.Errorf("%w: no review id supplied", ErrCheckpointStale)
	}
	if s.ActiveReview == nil {
		return fmt.Errorf("%w: no review is active", ErrCheckpointStale)
	}
	if s.ActiveReview.ReviewID != reviewID {
		return fmt.Errorf("%w: review %s is active, not %s", ErrCheckpointStale, s.ActiveReview.ReviewID, reviewID)
	}
	// The stored review is the authority, and it must still describe the current
	// generation: a review opened for a worker that has since been replaced
	// cannot judge the replacement.
	if s.ActiveReview.Generation != s.Generation {
		return fmt.Errorf("%w: review is for generation %d, current is %d",
			ErrCheckpointStale, s.ActiveReview.Generation, s.Generation)
	}
	kind := s.ActiveReview.Kind
	if !kind.Allows(outcome) {
		return fmt.Errorf("agent-engine: a %s review cannot answer %s", kind, outcome)
	}
	s.ActiveReview = nil
	s.Checkpoint.ReviewPending = false
	stamp := now.UTC().Format(time.RFC3339)

	// The debt is reset ONLY when the review evidenced that an obligation actually
	// advanced. It is not reset merely because a judgement happened.
	//
	// This is the rule that stops a request going nowhere from asking for more time
	// indefinitely: a reviewer saying CONTINUE means "keep going", not "something
	// advanced", and treating the first as the second lets a stalled request reset
	// its own clock every cycle. The judgement is still recorded — LastReviewedAt
	// advances — because knowing that a review occurred is itself useful; what it
	// does not do is buy budget.
	advanced := len(s.OpenObligationIDs()) == 0 && obligationAdvancedByReview(s)
	switch outcome {
	case OutcomeContinue, OutcomeCollectEvidence, OutcomeRedirect, OutcomeDiagnose, OutcomeRefresh:
		s.Checkpoint.LastReviewedAt = stamp
		if advanced {
			s.Checkpoint.DebtSeconds = 0
		}
	case OutcomePass:
		if !kind.Approves() {
			return fmt.Errorf("agent-engine: %s cannot approve the request", kind)
		}
		// No obligation is touched here. By the time this case runs on the
		// completion path, Store.commitCompletion has already discharged every
		// obligation against its judgement, inside the transaction that
		// publishes the result — so reaching here means the work really was
		// discharged, and the debt reset below is earned rather than assumed.
		s.Checkpoint.DebtSeconds = 0
		s.Checkpoint.LastReviewedAt = stamp
	case OutcomeFail, OutcomeBlocked:
		// The debt deliberately does not reset. Work that was judged not to have
		// advanced has not advanced, and clearing the clock would let a request
		// that is going nowhere keep asking for more time.
		s.Checkpoint.LastReviewedAt = stamp
	}
	s.Checkpoint.LastReviewedEvidenceIDs = s.CurrentEvidenceIDs()

	for i := len(s.Checkpoint.Submissions) - 1; i >= 0; i-- {
		if s.Checkpoint.Submissions[i].Generation == s.Generation {
			s.Checkpoint.Submissions[i].Reviewed = true
			s.Checkpoint.Submissions[i].Outcome = string(outcome)
			break
		}
	}
	_ = note
	return nil
}

// CurrentEvidenceIDs returns the evidence currently in the index.
func (s *State) CurrentEvidenceIDs() []string {
	out := make([]string, 0, len(s.Checkpoint.Evidence))
	for _, e := range s.Checkpoint.Evidence {
		out = append(out, e.ID)
	}
	return out
}

// contractValidateDigest is a thin indirection so this file does not import the
// contract package purely for one check, keeping the dependency edge visible.
func contractValidateDigest(d string) error {
	if len(d) != 64 {
		return fmt.Errorf("digest must be 64 hex characters, got %d", len(d))
	}
	for _, r := range strings.ToLower(d) {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("digest contains non-hex character %q", r)
		}
	}
	return nil
}

// OpenObligationIDs returns the obligations still awaiting disposition.
func (s *State) OpenObligationIDs() []string {
	var out []string
	for _, o := range s.Obligations {
		if o.Disposition == DispositionOpen || o.Disposition == DispositionUnknown {
			out = append(out, o.ID)
		}
	}
	return out
}

// obligationAdvancedByReview reports whether the reviewer's judgement came with
// evidence that an obligation moved.
//
// A judgement advances work only when it carries evidence backing an ADVANCED
// disposition, and the only writer of that disposition is the completion
// transaction, which sets it from a validated per-obligation judgement. A body of
// prose, a changed direction, or a refreshed worker are all reasoning; none of
// them is a demonstrated advance, and none of them may reset the active-time
// debt.
func obligationAdvancedByReview(s *State) bool {
	for _, o := range s.Obligations {
		if o.Disposition == DispositionAdvanced && len(o.EvidenceRefs) > 0 {
			return true
		}
	}
	return false
}
