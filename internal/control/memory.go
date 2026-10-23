package control

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Durable question and decision memory.
//
// Two failures motivate this file, and both are silent.
//
// The first is a replacement worker rediscovering settled ground. A worker that
// has lost its context to a refresh will re-ask a question that was already
// answered unless something durable says otherwise, and the cost is not the
// question — it is the risk that it re-litigates a decision under different
// conditions and neither party notices the disagreement.
//
// The second is an identifier that quietly stops identifying. If a stable ID is
// derived from "how many are in the list right now", then trimming the list
// recycles IDs, and a reference to a resolved question becomes indistinguishable
// from a reference to a new one. So identifiers come from dedicated monotonic
// counters that are never rewound by compaction, and a resolved question records
// an explicit DecisionID rather than having its title edited to point at one.

// Question and decision lifecycle states.
const (
	QuestionOpenStatus     = "OPEN"
	QuestionResolvedStatus = "RESOLVED"

	DecisionProposedStatus   = "PROPOSED"
	DecisionAcceptedStatus   = "ACCEPTED"
	DecisionSupersededStatus = "SUPERSEDED"
)

// MaxMemoryBodies bounds the retained bodies in each index.
//
// Bodies are trimmed before identities are: a body is reasoning that a future
// worker benefits from reading, and an ID is what makes a reference mean
// something. Keeping the identities and dropping the prose is the safe order.
const MaxMemoryBodies = 100

// QuestionRecord is one durable open question.
type QuestionRecord struct {
	// ID is allocated from a monotonic counter and never reused.
	ID string `json:"id"`
	// Title is a one-line summary.
	Title string `json:"title"`
	// Status is OPEN or RESOLVED.
	Status string `json:"status"`
	// AskedBy names the role that raised it.
	AskedBy string `json:"asked_by,omitempty"`
	// OriginGeneration records the generation that raised it.
	//
	// It is never re-stamped when the worker is replaced. A question asked of
	// generation 2 and answered for generation 3 is still a question about the
	// second worker, and pretending otherwise loses the only record of the
	// conditions it was asked under.
	OriginGeneration int    `json:"origin_generation"`
	CreatedAt        string `json:"created_at"`
	// ResolvedAt and DecisionID record the resolution. DecisionID is explicit
	// because editing the title to say "-> D-004" makes the link prose, and
	// prose is not queryable.
	ResolvedAt string `json:"resolved_at,omitempty"`
	DecisionID string `json:"decision_id,omitempty"`
	// Body is the reasoning, retained while the index holds room.
	Body string `json:"body,omitempty"`
}

// DecisionRecord is one durable decision and who accepted it.
type DecisionRecord struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Status is PROPOSED, ACCEPTED or SUPERSEDED.
	Status string `json:"status"`
	// Authority names who accepted it.
	//
	// This is recorded because "a decision was made" is not the same fact as
	// "this person decided it", and only the second one can be challenged later.
	Authority string `json:"authority,omitempty"`
	// QuestionID links to the question this answers, when there was one.
	QuestionID string `json:"question_id,omitempty"`
	// OriginGeneration is never re-stamped across a refresh.
	OriginGeneration int      `json:"origin_generation"`
	CreatedAt        string   `json:"created_at"`
	SupersededBy     string   `json:"superseded_by,omitempty"`
	Body             string   `json:"body,omitempty"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
}

// ErrMemoryUnknown is returned when a referenced question or decision does not
// exist. A typo must not invent a resolution.
var ErrMemoryUnknown = errors.New("agent-engine: referenced question or decision does not exist")

// ErrNoDecisionAuthority is returned when a decision is accepted by a role that
// is not configured to accept decisions.
//
// A worker proposing and a worker accepting are different authorities. Letting
// the proposer accept its own proposal would make the accepted-decision record
// meaningless, because the record exists precisely to show that somebody other
// than the proposer signed off.
var ErrNoDecisionAuthority = errors.New("agent-engine: this role may not accept decisions")

// DecisionAuthority reports whether a role may accept a decision.
func (s *State) DecisionAuthority(role string) bool {
	if strings.TrimSpace(s.DecisionAuthorityRole) == "" {
		// No configured authority means nothing may be accepted. Defaulting to
		// "anyone may" would make the authority field decorative.
		return false
	}
	return role == s.DecisionAuthorityRole
}

// OpenQuestion records a newly raised question.
func (s *State) OpenQuestion(title, body, askedBy string, now time.Time) (QuestionRecord, error) {
	if strings.TrimSpace(title) == "" {
		return QuestionRecord{}, errors.New("agent-engine: a question needs a title")
	}
	// The ID comes from a dedicated counter, not from the length of the list, so
	// compaction can never recycle it.
	s.NextQuestionSeq++
	q := QuestionRecord{
		ID:               fmt.Sprintf("Q-%04d", s.NextQuestionSeq),
		Title:            title,
		Status:           QuestionOpenStatus,
		AskedBy:          askedBy,
		OriginGeneration: s.Generation,
		CreatedAt:        now.UTC().Format(time.RFC3339),
		Body:             body,
	}
	s.Questions = append(s.Questions, q)
	s.trimQuestions()
	return q, nil
}

func (s *State) trimQuestions() {
	// Bodies go first; identities stay.
	for i := range s.Questions {
		if len(s.Questions) > MaxMemoryBodies {
			s.Questions[i].Body = ""
		}
	}
	if len(s.Questions) > MaxQuestions {
		// Drop the oldest resolved questions. An open question is never trimmed,
		// because dropping one would silently forget that something is
		// unresolved.
		kept := make([]QuestionRecord, 0, MaxQuestions)
		drop := len(s.Questions) - MaxQuestions
		for _, q := range s.Questions {
			if drop > 0 && q.Status == QuestionResolvedStatus {
				drop--
				continue
			}
			kept = append(kept, q)
		}
		s.Questions = kept
	}
}

// ResolveQuestion links a question to the decision that answered it.
func (s *State) ResolveQuestion(questionID, decisionID string, now time.Time) error {
	for i := range s.Questions {
		if s.Questions[i].ID != questionID {
			continue
		}
		if decisionID != "" {
			if !s.hasDecision(decisionID) {
				return fmt.Errorf("%w: %s", ErrMemoryUnknown, decisionID)
			}
		}
		s.Questions[i].Status = QuestionResolvedStatus
		s.Questions[i].ResolvedAt = now.UTC().Format(time.RFC3339)
		s.Questions[i].DecisionID = decisionID
		return nil
	}
	return fmt.Errorf("%w: question %s", ErrMemoryUnknown, questionID)
}

func (s *State) hasDecision(id string) bool {
	for _, d := range s.Decisions {
		if d.ID == id {
			return true
		}
	}
	return false
}

// RecordDecision appends a proposed or accepted decision.
//
// acceptedBy is required for an ACCEPTED decision and is checked against the
// configured authority, so a proposer cannot sign off its own proposal.
func (s *State) RecordDecision(title, body, questionID, authority string, evidenceRefs []string, now time.Time) (DecisionRecord, error) {
	if strings.TrimSpace(title) == "" {
		return DecisionRecord{}, errors.New("agent-engine: a decision needs a title")
	}
	status := DecisionProposedStatus
	if strings.TrimSpace(authority) != "" {
		if !s.DecisionAuthority(authority) {
			return DecisionRecord{}, fmt.Errorf("%w: %q is not %q", ErrNoDecisionAuthority, authority, s.DecisionAuthorityRole)
		}
		status = DecisionAcceptedStatus
	}
	if questionID != "" {
		if !s.hasQuestion(questionID) {
			return DecisionRecord{}, fmt.Errorf("%w: question %s", ErrMemoryUnknown, questionID)
		}
	}
	s.NextDecisionSeq++
	d := DecisionRecord{
		ID:               fmt.Sprintf("D-%04d", s.NextDecisionSeq),
		Title:            title,
		Status:           status,
		Authority:        authority,
		QuestionID:       questionID,
		OriginGeneration: s.Generation,
		CreatedAt:        now.UTC().Format(time.RFC3339),
		Body:             body,
		EvidenceRefs:     append([]string(nil), evidenceRefs...),
	}
	s.Decisions = append(s.Decisions, d)
	if questionID != "" {
		// A decision that answers a question resolves it, with the link recorded
		// explicitly rather than appended to the question's title.
		_ = s.ResolveQuestion(questionID, d.ID, now)
	}
	s.trimDecisions()
	return d, nil
}

func (s *State) hasQuestion(id string) bool {
	for _, q := range s.Questions {
		if q.ID == id {
			return true
		}
	}
	return false
}

func (s *State) trimDecisions() {
	for i := range s.Decisions {
		if len(s.Decisions) > MaxMemoryBodies {
			s.Decisions[i].Body = ""
		}
	}
	if len(s.Decisions) > MaxDecisions {
		// Superseded decisions go first; an accepted, un-superseded decision is
		// the whole value of this file and is never trimmed.
		kept := make([]DecisionRecord, 0, MaxDecisions)
		drop := len(s.Decisions) - MaxDecisions
		for _, d := range s.Decisions {
			if drop > 0 && d.Status == DecisionSupersededStatus {
				drop--
				continue
			}
			kept = append(kept, d)
		}
		s.Decisions = kept
	}
}

// SupersedeDecision marks a decision replaced by a newer one.
func (s *State) SupersedeDecision(decisionID, supersededBy string) error {
	for i := range s.Decisions {
		if s.Decisions[i].ID == decisionID {
			s.Decisions[i].Status = DecisionSupersededStatus
			s.Decisions[i].SupersededBy = supersededBy
			return nil
		}
	}
	return fmt.Errorf("%w: decision %s", ErrMemoryUnknown, decisionID)
}

// OpenQuestions returns the currently open questions in order.
func (s *State) OpenQuestions() []QuestionRecord {
	var out []QuestionRecord
	for _, q := range s.Questions {
		if q.Status == QuestionOpenStatus {
			out = append(out, q)
		}
	}
	return out
}

// AcceptedDecisions returns the accepted, un-superseded decisions.
func (s *State) AcceptedDecisions() []DecisionRecord {
	var out []DecisionRecord
	for _, d := range s.Decisions {
		if d.Status == DecisionAcceptedStatus {
			out = append(out, d)
		}
	}
	return out
}

// RenderMemory produces the human-readable memory document.
//
// It is a projection of the structured indexes, not the authority. A reader that
// parsed it to decide something would be doing exactly what the structured
// index exists to prevent.
func (s *State) RenderMemory() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\nGeneration: %d\nStage: %s\nWritten: %s\n",
		s.TaskID, s.Generation, s.Stage, s.UpdatedAt)

	b.WriteString("\n<!-- A projection of the structured indexes in STATE.json. Do not decide anything from this file. -->\n")

	b.WriteString("\n# Open questions\n")
	open := s.OpenQuestions()
	if len(open) == 0 {
		b.WriteString("None open.\n")
	}
	for _, q := range open {
		fmt.Fprintf(&b, "\n## %s — %s\n", q.ID, q.Title)
		fmt.Fprintf(&b, "asked by %s in generation %d at %s\n", q.AskedBy, q.OriginGeneration, q.CreatedAt)
		if q.Body != "" {
			b.WriteString(q.Body + "\n")
		}
	}

	b.WriteString("\n# Accepted decisions\n")
	accepted := s.AcceptedDecisions()
	if len(accepted) == 0 {
		b.WriteString("None recorded.\n")
	}
	for _, d := range accepted {
		fmt.Fprintf(&b, "\n## %s — %s\n", d.ID, d.Title)
		fmt.Fprintf(&b, "accepted by %s in generation %d at %s\n", d.Authority, d.OriginGeneration, d.CreatedAt)
		if d.QuestionID != "" {
			fmt.Fprintf(&b, "answers: %s\n", d.QuestionID)
		}
		if len(d.EvidenceRefs) > 0 {
			fmt.Fprintf(&b, "evidence: %s\n", strings.Join(d.EvidenceRefs, ", "))
		}
		if d.Body != "" {
			b.WriteString(d.Body + "\n")
		}
	}
	return b.String()
}
