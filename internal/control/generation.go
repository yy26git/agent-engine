package control

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Generation replacement: retiring a worker without losing the task.
//
// The failure this exists to prevent is a "fresh worker" that is not fresh. The
// embedded draft's refresh incremented a generation counter and could point the
// binding at the same session again, producing a new generation number
// describing an old conversation. A generation number is supposed to mean
// "there is a real, reconstructable starting point for this generation", so the
// counter is advanced only after three things exist: a validated handoff, a
// distinct session identity, and a committed binding.
//
// Ordering is the design, because each crash boundary has a different correct
// recovery and a single "generation++" cannot distinguish them:
//
//	INTENT -> HANDOFF -> CANDIDATE -> BINDING -> ORIENTATION -> ACTIVE
//
// A crash before BINDING has a candidate that may or may not exist and must be
// inspected rather than created again. A crash after BINDING has a committed
// identity whose orientation may or may not have been accepted. In neither case
// is the right answer "create another session".

// GenerationPhase is how far a replacement has progressed.
type GenerationPhase string

const (
	// GenIntent means the supervisor decided to replace the worker. No new
	// session exists.
	GenIntent GenerationPhase = "INTENT"
	// GenHandoff means a validated handoff exists for the outgoing generation.
	GenHandoff GenerationPhase = "HANDOFF"
	// GenCandidate means a replacement session identity has been recorded but the
	// binding has not been committed. The candidate is quarantined: it exists,
	// but it must not be able to advance the request.
	GenCandidate GenerationPhase = "CANDIDATE"
	// GenBinding means the new session is the committed worker identity and the
	// generation has advanced.
	GenBinding GenerationPhase = "BINDING"
	// GenOrientation means the replacement has been told what it is working on
	// and that instruction has been accepted.
	GenOrientation GenerationPhase = "ORIENTATION"
	// GenActive means the replacement is running normally.
	GenActive GenerationPhase = "ACTIVE"
)

// Handoff is a validated, generation-linked orientation document.
type Handoff struct {
	// TaskID must match the request, so a handoff from another task is refused.
	TaskID string
	// Generation is the outgoing generation this handoff describes.
	Generation int
	// Sections must all be present and non-empty. Explicit "none" is allowed;
	// silence is not, because a heading with no body is indistinguishable from a
	// section the author forgot.
	Objective   string
	Status      string
	OpenWork    string
	NextAction  string
	DoneSoFar   string
	FailedPaths string
}

// Validate refuses a handoff that cannot orient a fresh worker.
func (h Handoff) Validate(taskID string, generation int) error {
	if h.TaskID != taskID {
		return fmt.Errorf("%w: handoff is for task %s, not %s", ErrStaleHandoff, h.TaskID, taskID)
	}
	if h.Generation != generation {
		return fmt.Errorf("%w: handoff is for generation %d, current is %d", ErrStaleHandoff, h.Generation, generation)
	}
	for name, body := range map[string]string{
		"objective":   h.Objective,
		"status":      h.Status,
		"open work":   h.OpenWork,
		"next action": h.NextAction,
	} {
		if strings.TrimSpace(body) == "" {
			return fmt.Errorf("%w: handoff section %q is empty; write \"none\" if there is genuinely nothing",
				ErrStaleHandoff, name)
		}
	}
	return nil
}

// ErrStaleHandoff is returned when a handoff does not belong to the current
// task or generation.
var ErrStaleHandoff = errors.New("agent-engine: handoff does not belong to this task and generation")

// ErrEmptyReplacement is returned when a replacement is the session being
// replaced.
//
// "Fresh context" that is the same conversation is not fresh. Accepting the old
// session here is what produced a generation counter that advanced while the
// context stayed exactly as degraded as before.
var ErrEmptyReplacement = errors.New("agent-engine: the replacement session must be distinct from the retired worker")

// Replacement is the durable record of a generation change.
type Replacement struct {
	// Phase is how far the replacement has progressed.
	Phase GenerationPhase `json:"phase"`
	// Reason is why the worker is being replaced.
	Reason string `json:"reason,omitempty"`
	// OutgoingGeneration is the generation being retired.
	OutgoingGeneration int `json:"outgoing_generation"`
	// IncomingGeneration is the generation being prepared. It is the outgoing
	// generation plus one, and it is not counted until the binding commits.
	IncomingGeneration int `json:"incoming_generation"`
	// RetiredSession is the session being replaced.
	RetiredSession string `json:"retired_session"`
	// CandidateSession is a confirmed, distinct session identity, once one
	// exists.
	CandidateSession string `json:"candidate_session,omitempty"`
	// HandoffDigest references the committed handoff body.
	HandoffDigest string `json:"handoff_digest,omitempty"`
	// OrientationEffectID is the durable instruction that oriented the new worker.
	OrientationEffectID string `json:"orientation_effect_id,omitempty"`
	// Quiesced records whether the old worker was confirmed stopped.
	Quiesced bool `json:"quiesced"`
	// RequestedAt is when the replacement began.
	RequestedAt string `json:"requested_at"`
}

// NeedsQuiescence reports whether the retired session must be stopped first.
//
// It does: a session that can still write while a replacement is being prepared
// is a writer nobody is supervising, and its late output would race the new
// generation. Stopping is asynchronous in Agent Deck, so the confirmation is
// recorded separately rather than inferred from having issued the stop.
func (r Replacement) NeedsQuiescence() bool {
	return r.Phase == GenIntent || r.Phase == GenHandoff || r.Phase == GenCandidate
}

// BeginReplacement records the intent to replace the worker.
//
// It commits intent before any external call, so a crash immediately after
// leaves a recorded decision rather than an unexplained new session.
func (s *State) BeginReplacement(reason string, now time.Time) (Replacement, error) {
	if s.Stage == StageRefreshing {
		if s.Replacement.Phase != "" {
			// Already replacing. Returning the existing record rather than starting
			// a second one is what makes a retry after a crash safe.
			return s.Replacement, nil
		}
	}
	if s.Stage.Terminal() {
		return Replacement{}, fmt.Errorf("agent-engine: %s is terminal; a verified request cannot rotate its worker", s.Stage)
	}
	if s.Generation >= s.Limits.WorkerGens {
		return Replacement{}, fmt.Errorf("agent-engine: the worker-generation limit of %d is exhausted", s.Limits.WorkerGens)
	}
	if s.ActiveReview != nil {
		return Replacement{}, fmt.Errorf("agent-engine: a %s review is open; finish or settle it before replacing the worker", s.ActiveReview.Kind)
	}
	r := Replacement{
		Phase:              GenIntent,
		Reason:             reason,
		OutgoingGeneration: s.Generation,
		IncomingGeneration: s.Generation + 1,
		RetiredSession:     s.WorkerSession,
		RequestedAt:        now.UTC().Format(time.RFC3339),
	}
	s.Replacement = r
	if err := s.transition(StageRefreshing, "replacing the worker: "+reason, now.UTC().Format(time.RFC3339)); err != nil {
		return Replacement{}, err
	}
	return r, nil
}

// RecordHandoff records a validated handoff for the outgoing generation.
func (s *State) RecordHandoff(h Handoff, digest string, now time.Time) error {
	if s.Replacement.Phase == "" {
		return errors.New("agent-engine: no replacement is in progress; a handoff with nothing to hand off to is a stray artifact")
	}
	if err := h.Validate(s.TaskID, s.Replacement.OutgoingGeneration); err != nil {
		return err
	}
	s.Replacement.Phase = GenHandoff
	s.Replacement.HandoffDigest = digest
	s.HandoffGeneration = h.Generation
	_ = now
	return nil
}

// RecordCandidate records a confirmed, distinct replacement session.
//
// The candidate is quarantined here. It is not the worker, it cannot receive
// instructions, and it cannot advance the request — a candidate that started
// working before the binding committed would be doing work nobody supervised,
// which is the fake-fresh-context failure in its purest form.
func (s *State) RecordCandidate(sessionID string, now time.Time) error {
	if s.Replacement.Phase != GenHandoff {
		return fmt.Errorf("agent-engine: a candidate needs a validated handoff first; the replacement is at %s", s.Replacement.Phase)
	}
	if strings.TrimSpace(sessionID) == "" {
		return ErrEmptyReplacement
	}
	if sessionID == s.Replacement.RetiredSession {
		// The specific defect this protocol exists to prevent.
		return fmt.Errorf("%w: %s is the session being replaced", ErrEmptyReplacement, sessionID)
	}
	if sessionID == s.WorkerSession {
		return fmt.Errorf("%w: %s is still the bound worker", ErrEmptyReplacement, sessionID)
	}
	s.Replacement.CandidateSession = sessionID
	s.Replacement.Phase = GenCandidate
	_ = now
	return nil
}

// CommitBinding advances the generation and rebinds the worker.
//
// This is the single point at which the generation counts up, and it happens
// only once a distinct session is recorded. Committing binding and generation
// together means a state file can never show generation 3 pointing at
// generation 2's session.
func (s *State) CommitBinding(now time.Time) error {
	if s.Replacement.Phase != GenCandidate {
		return fmt.Errorf("agent-engine: the replacement is at %s; a binding needs a confirmed candidate", s.Replacement.Phase)
	}
	if !s.Replacement.Quiesced {
		// Committing while the old session can still write leaves two mutators.
		return errors.New("agent-engine: the retired worker has not been confirmed quiesced; refusing to rotate")
	}
	s.WorkerSession = s.Replacement.CandidateSession
	s.Generation = s.Replacement.IncomingGeneration
	s.Replacement.Phase = GenBinding
	s.HandoffGeneration = 0
	// A fresh worker starts with a clean active-time clock, but the lifetime
	// counters stay: a request that already burned three repair cycles has still
	// burned them, whoever is doing the repairing.
	s.Checkpoint.DebtSeconds = 0
	s.Checkpoint.ReviewPending = false
	s.Checkpoint.Required = false
	// Memory is task-scoped and deliberately survives the rotation.
	s.transition(StageWorking, fmt.Sprintf("generation %d bound to a distinct session", s.Generation), now.UTC().Format(time.RFC3339))
	return nil
}

// RecordOrientation records that the replacement was oriented.
func (s *State) RecordOrientation(effectID string, now time.Time) error {
	if s.Replacement.Phase != GenBinding {
		return fmt.Errorf("agent-engine: orientation follows a committed binding; the replacement is at %s", s.Replacement.Phase)
	}
	s.Replacement.OrientationEffectID = effectID
	s.Replacement.Phase = GenOrientation
	_ = now
	return nil
}

// FinishReplacement marks the new generation active.
func (s *State) FinishReplacement(now time.Time) error {
	if s.Replacement.Phase != GenOrientation && s.Replacement.Phase != GenBinding {
		return fmt.Errorf("agent-engine: the replacement is at %s", s.Replacement.Phase)
	}
	s.Replacement.Phase = GenActive
	_ = now
	return nil
}

// Quiesce records that the retired worker was confirmed stopped.
func (s *State) Quiesce(now time.Time) error {
	if s.Replacement.Phase == "" {
		return errors.New("agent-engine: nothing is being replaced")
	}
	s.Replacement.Quiesced = true
	_ = now
	return nil
}

// ErrUnsupportedReplacement is returned when a state carries a replacement phase
// this build cannot interpret.
var ErrUnsupportedReplacement = errors.New("agent-engine: the request has a replacement this build does not understand")

// knownReplacementPhases is every phase this build can interpret.
var knownReplacementPhases = map[GenerationPhase]bool{
	GenIntent: true, GenHandoff: true, GenCandidate: true,
	GenBinding: true, GenOrientation: true, GenActive: true,
}

// understandsReplacementPhase reports whether this build can drive a phase.
//
// It exists so the refusal above is about a GENUINELY newer protocol rather than
// about a phase that was merely added. A build that grows a new phase must add it
// to knownReplacementPhases in the same change that teaches it to drive that
// phase; until it does, a state carrying it is refused rather than half-read.
func understandsReplacementPhase(p GenerationPhase) bool { return knownReplacementPhases[p] }

// ReconcileReplacement reports the recovery action for the current phase.
//
// It never chooses to create a session. An ambiguous candidate is inspected by
// its recorded identity, because creating another one is how two workers end up
// writing the same workspace.
func (s *State) ReconcileReplacement() (Replacement, string) {
	r := s.Replacement
	if r.Phase == "" {
		return r, "no replacement is in progress"
	}
	switch r.Phase {
	case GenIntent:
		return r, "the handoff was not yet committed; validate and record it before anything else"
	case GenHandoff:
		return r, "a handoff is durable but no replacement session exists; create one, then record the candidate"
	case GenCandidate:
		if r.CandidateSession == "" {
			return r, "a candidate was started but its identity was never recorded; inspect for it rather than creating another"
		}
		return r, "a candidate exists but was never bound; confirm quiescence of " + r.RetiredSession + ", then commit the binding"
	case GenBinding:
		return r, "the binding is committed; the orientation instruction may or may not have been accepted, so reconcile its effect rather than resending blindly"
	case GenOrientation:
		return r, "the orientation was accepted; the new generation can be marked active"
	case GenActive:
		return r, "the replacement is complete"
	}
	return r, "unknown replacement phase"
}

// OrientationPrompt is the instruction that tells a replacement what it is doing.
//
// It points at durable references rather than restating content, because the
// handoff's value is telling a fresh worker *where to inspect*, and paraphrasing
// that here would create a second copy that can drift from the first.
func (s *State) OrientationPrompt(workspaceRoot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the implementation worker for task %s, generation %d.\n", s.TaskID, s.Generation)
	fmt.Fprintf(&b, "You have no prior conversation. Everything you need is on disk.\n\n")

	b.WriteString("Read these before touching anything:\n")
	for _, o := range s.Obligations {
		fmt.Fprintf(&b, "- obligation %s: %s\n", o.ID, o.Criterion)
	}
	for _, d := range s.AcceptedDecisions() {
		fmt.Fprintf(&b, "- decision %s (accepted by %s): %s\n", d.ID, d.Authority, d.Title)
	}
	if open := s.OpenQuestions(); len(open) > 0 {
		b.WriteString("\nOpen questions, each of which is a decision somebody has not made yet:\n")
		for _, q := range open {
			fmt.Fprintf(&b, "- %s: %s\n", q.ID, q.Title)
		}
	}
	if len(s.Failures) > 0 {
		b.WriteString("\nThese approaches already failed. Repeating one is not progress:\n")
		for _, fl := range s.Failures {
			fmt.Fprintf(&b, "- %s on %s: %s\n", fl.CheckID, fl.Signature, fl.Hypothesis)
		}
	}
	if len(s.Checks) > 0 {
		fmt.Fprintf(&b, "\nRequired checks: %s. They are the definition of done, not a formality.\n",
			strings.Join(s.Checks, ", "))
	}
	fmt.Fprintf(&b, "\nWorkspace: %s\n", workspaceRoot)
	b.WriteString("\nYour context is empty by design. Verify claims against the source rather than trusting\n")
	b.WriteString("this message: it tells you where to look, not what is true.\n")
	b.WriteString("Submit a checkpoint and a completion assertion through the control layer when you finish.\n")
	return b.String()
}
