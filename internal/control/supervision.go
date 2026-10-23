package control

import (
	"fmt"
	"strings"
	"time"
)

// Production supervision.
//
// Until this existed, the engine was purely reactive: it decided when the runtime
// produced a frame. That is sufficient for a request whose worker talks often, and
// it is silent about a request whose worker has stopped talking entirely — which
// is the case that matters, because a dead worker and a slow one look identical
// from the outside and only one of them is making progress.
//
// So the engine now OWNS a clock. The tick is not a heartbeat and it is not
// polling for activity: it is the only thing that can notice that nothing has been
// proven for too long. The rules it follows are deliberately narrow:
//
//   - Normal progress costs nothing. A tick that finds a live worker inside its
//     budget records an observation and wakes nobody. Continuous strong-model
//     polling is how supervision becomes a tax on the work it supervises.
//   - Only PROVEN events advance or satisfy a checkpoint. A file change, a commit,
//     a status line and a green command are activity facts, not evidence that an
//     obligation advanced.
//   - Debt is charged only for observed, unpaused, grant-live time, and it resets
//     only on a reviewed advance. A reviewer saying CONTINUE, a renamed hypothesis
//     and a restart all leave it alone.
//   - A wall deadline runs regardless of activity. A generous budget must not be
//     able to postpone review forever.

// Supervision reasons. They are a closed set because each is a distinct thing an
// operator would want to distinguish when reading a stuck report.
const (
	ReasonQuietStream     = "quiet-stream"
	ReasonMissingCheck    = "missing-checkpoint"
	ReasonWallDeadline    = "wall-deadline"
	ReasonObjectiveFail   = "objective-failure"
	ReasonIdleOrDead      = "unexpected-idle"
	ReasonStateInconsist  = "state-inconsistency"
	ReasonGuidanceRefresh = "refresh-decision"
)

// WakeCause is one reason a model wake was raised, with the coalescing key that
// makes identical causes a single wake.
type WakeCause struct {
	Reason string
	// Detail is bounded context for the prompt and the stuck report.
	Detail string
	// CoalesceKey identifies "the same cause". Two ticks that agree on it raise
	// one wake between them, not two.
	CoalesceKey string
}

// MinWakeIntervalFloor is the shortest gap between two wakes, whatever operator
// policy says.
//
// It exists because a zero or negative interval turns a pathological clock into a
// hot loop that spends the request's entire budget on supervision.
const MinWakeIntervalFloor = 30 * time.Second

// SupervisionState is the durable half of supervision.
//
// It is separate from Checkpoint because the two answer different questions.
// Checkpoint is "what does the worker's progress cost us"; SupervisionState is
// "what have we already woken about, and when may we wake again". Mixing them
// would make the coalescing window part of the debt arithmetic, which is exactly
// the coupling that produces a wake storm.
type SupervisionState struct {
	// LastTickAt is when the engine last evaluated deadlines.
	LastTickAt string `json:"last_tick_at,omitempty"`
	// LastWakeAt is when a model wake was last raised.
	LastWakeAt string `json:"last_wake_at,omitempty"`
	// LastWakeReason is the coalescing key of the last wake.
	LastWakeReason string `json:"last_wake_reason,omitempty"`
	// WakeCount is a lifetime counter, so a report can say how often supervision
	// has had to intervene without reconstructing it from history.
	WakeCount int `json:"wake_count"`
	// WaitingOn names a bounded external wait that pauses active debt, and
	// WaitDeadlineAt bounds it. A wait with no deadline is not a pause, because a
	// hung verifier must not be able to stop the clock forever.
	WaitingOn      string `json:"waiting_on,omitempty"`
	WaitDeadlineAt string `json:"wait_deadline_at,omitempty"`
	// ObjectiveFailures counts executed managed-check failures, distinct from the
	// worker's self-declared attempts.
	ObjectiveFailures int `json:"objective_failures,omitempty"`
	// DiagnosticsOpened counts diagnostic reviews actually opened.
	DiagnosticsOpened int `json:"diagnostics_opened,omitempty"`
}

// WaitSatisfied reports whether a recorded external wait is over.
//
// A wait with no deadline never satisfies: pausing indefinitely is the failure
// mode this whole mechanism exists to prevent, so the absence of a deadline is
// treated as "not a pause" rather than "an infinite pause".
func (s SupervisionState) WaitSatisfied(now time.Time) (bool, string) {
	if s.WaitingOn == "" {
		return true, ""
	}
	if s.WaitDeadlineAt == "" {
		return false, "the request is waiting on " + s.WaitingOn + " with no deadline, so the clock keeps running"
	}
	deadline, err := time.Parse(time.RFC3339, s.WaitDeadlineAt)
	if err != nil {
		return false, "the recorded wait deadline is unreadable, so the clock keeps running"
	}
	if now.Before(deadline) {
		return false, "the request is waiting on " + s.WaitingOn + " until " + s.WaitDeadlineAt
	}
	return true, "the bounded wait on " + s.WaitingOn + " expired"
}

// RecordWait records a bounded external wait that pauses active debt.
func (s *SupervisionState) RecordWait(on string, deadline time.Time) {
	s.WaitingOn = on
	if deadline.IsZero() {
		s.WaitDeadlineAt = ""
		return
	}
	s.WaitDeadlineAt = deadline.UTC().Format(time.RFC3339)
}

// ClearWait records that a bounded external wait has ended.
func (s *SupervisionState) ClearWait() {
	s.WaitingOn = ""
	s.WaitDeadlineAt = ""
}

// ShouldWake reports whether a cause warrants a model wake right now.
//
// The rule is: never the same cause twice inside the minimum interval, and never
// any cause twice inside it either. Normal progress therefore costs nothing,
// because a tick that finds nothing due does not come here at all.
func (s SupervisionState) ShouldWake(c WakeCause, now time.Time, policy CheckpointPolicy) (bool, string) {
	if s.LastWakeAt == "" {
		return true, ""
	}
	last, err := time.Parse(time.RFC3339, s.LastWakeAt)
	if err != nil {
		// An unreadable watermark wakes. Refusing to wake because we cannot read
		// our own history is the wrong failure direction.
		return true, "the last wake time is unreadable, so supervision wakes rather than trusting it"
	}
	gap := policy.MinWakeInterval
	if gap < MinWakeIntervalFloor {
		gap = MinWakeIntervalFloor
	}
	since := now.Sub(last)
	if since < gap {
		if c.CoalesceKey != "" && s.LastWakeReason == c.CoalesceKey {
			return false, "the same cause was raised " + since.Round(time.Second).String() + " ago"
		}
		return false, "a wake was raised " + since.Round(time.Second).String() +
			" ago, inside the minimum interval of " + gap.String()
	}
	return true, ""
}

// NoteWake records that a wake was raised, and coalesces on its cause.
func (s *SupervisionState) NoteWake(c WakeCause, now time.Time) {
	s.LastWakeAt = now.UTC().Format(time.RFC3339)
	s.LastWakeReason = c.CoalesceKey
	s.WakeCount++
}

// TICK is the engine's supervised interval. It is deliberately short enough to
// notice a stalled worker in reasonable time and long enough that the tick itself
// is never the reason a request runs slowly.
const supervisionTick = 2 * time.Second

// tick evaluates supervision deadlines and opens at most one review.
//
// It is called from the engine's select loop on a timer AND after each frame, so
// a busy worker and a silent one are both supervised. Every decision it makes goes
// through the state transaction; the tick itself never mutates state directly,
// because two owners of the same decision is the failure this whole file exists to
// avoid.
func (e *Engine) tick(s *State, now time.Time) (WakeCause, bool) {
	if s.Stage.Terminal() {
		return WakeCause{}, false
	}
	// A bounded wait pauses active debt but never the wall clock. A hung verifier
	// must not be able to stop the request forever, so the wait is checked and,
	// if it has expired, cleared so the ordinary rules resume.
	if s.Supervision.WaitingOn != "" {
		done, why := s.Supervision.WaitSatisfied(now)
		if done {
			s.Supervision.ClearWait()
			e.logf("wait ended: %s", why)
		}
	}

	// A wall deadline fires regardless of what the worker is doing. It is checked
	// first because it is the only rule that survives every other condition.
	if due, reason := s.wallDeadlineDue(now); due {
		return WakeCause{
			Reason:      ReasonWallDeadline,
			Detail:      reason,
			CoalesceKey: ReasonWallDeadline,
		}, true
	}
	// A recorded objective failure is objective evidence, and it escalates on its
	// own schedule rather than waiting for a soft budget to fill.
	if cause, ok := s.objectiveFailureCause(now); ok {
		return cause, true
	}
	// Finally the ordinary checkpoint rules. A request that is simply progressing
	// reaches none of these, and so costs nothing.
	policy := e.cfg.Checkpoints
	if policy.SoftActiveBudget <= 0 {
		policy = DefaultCheckpointPolicy()
	}
	if due, reason := s.Checkpoint.Due(policy, now); due {
		key := ReasonQuietStream
		if s.Checkpoint.Required {
			key = ReasonMissingCheck
		}
		return WakeCause{Reason: key, Detail: reason, CoalesceKey: key + "|" + s.Stage.String()}, true
	}
	return WakeCause{}, false
}

// wallDeadlineDue reports whether the request's absolute deadline has passed.
func (s *State) wallDeadlineDue(now time.Time) (bool, string) {
	if s.Checkpoint.DeadlineAt == "" {
		return false, ""
	}
	deadline, err := time.Parse(time.RFC3339, s.Checkpoint.DeadlineAt)
	if err != nil {
		return false, ""
	}
	if now.Before(deadline) {
		return false, ""
	}
	return true, "the absolute request deadline " + s.Checkpoint.DeadlineAt + " has passed"
}

// objectiveFailureCause escalates a recorded managed-check failure.
//
// The counters are LIFETIME. A renamed hypothesis, a new generation and a restart
// all leave them alone, which is the entire point of counting objective evidence
// rather than attempts.
func (s *State) objectiveFailureCause(now time.Time) (WakeCause, bool) {
	if s.Supervision.ObjectiveFailures == 0 {
		return WakeCause{}, false
	}
	// A failure only escalates once the repair budget is actually gone. Before
	// that, the worker still has attempts it is entitled to use, and a diagnostic
	// review would be spending a round on a problem the budget would have solved.
	if s.RepairCycles < s.effectiveRepairLimit() {
		return WakeCause{}, false
	}
	// And not while a review is already owed.
	if s.ActiveReview != nil || s.Checkpoint.ReviewPending {
		return WakeCause{}, false
	}
	_ = now
	return WakeCause{
		Reason:      ReasonObjectiveFail,
		Detail:      fmt.Sprintf("%d objective failure(s) recorded and the repair budget of %d is spent", s.Supervision.ObjectiveFailures, s.effectiveRepairLimit()),
		CoalesceKey: ReasonObjectiveFail,
	}, true
}

// effectiveRepairLimit is the request's repair ceiling, or the default.
func (s *State) effectiveRepairLimit() int {
	if s.Limits.RepairCycles > 0 {
		return s.Limits.RepairCycles
	}
	return defaultRepairCycles
}

// defaultRepairCycles mirrors the contract default without importing a cycle.
const defaultRepairCycles = 3

// openDiagnosticOrReview turns a supervision cause into the right kind of owed
// review.
//
// The distinction is not cosmetic. A DIAGNOSTIC review is an independent
// investigation of a stuck cycle; a CHECKPOINT review judges whether work is
// advancing. Only the latter resets active debt, so escalating to the wrong one
// would either stall a healthy request or let a stuck one keep its budget.
func (e *Engine) openDiagnosticOrReview(s *State, cause WakeCause, now time.Time) error {
	// A cause that already has a review owed changes nothing. A second review of
	// the same work produces a second answer, and one of them is wasted or
	// contradictory.
	if s.ActiveReview != nil {
		return nil
	}
	kind := ReviewCheckpoint
	if cause.Reason == ReasonObjectiveFail {
		kind = ReviewDiagnostic
	}
	reviewer := s.ReviewSession(kind)
	if reviewer == "" {
		// Nothing can review this. That is a block, not a reason to keep ticking:
		// a request that owes a judgement nobody can give will never make one.
		s.StageReason = "supervision owes a review (" + cause.Reason + ") but no auditor session is bound"
		return s.transition(StageBlocked, s.StageReason, now.UTC().Format(time.RFC3339))
	}
	stamp := now.UTC().Format(time.RFC3339)

	// An objective failure with the repair budget spent is a DIAGNOSTIC: the
	// question is no longer "is it progressing" but "why can it not progress, and
	// what should happen instead".
	if cause.Reason == ReasonObjectiveFail {
		if s.DiagnosticRounds >= s.guidanceAllowed() {
			s.StageReason = "objective failures persist and the diagnostic guidance budget is exhausted; a human must decide"
			return s.transition(StageBlocked, s.StageReason, stamp)
		}
		reviewID := fmt.Sprintf("D%d", s.DiagnosticRounds+1)
		s.ActiveReview = NewActiveReview(ReviewDiagnostic, reviewID, reviewer, s.Generation, s.DiagnosticRounds+1)
		s.ActiveReview.RequestedAt = stamp
		s.Checkpoint.ReviewPending = true
		s.Supervision.DiagnosticsOpened++
		if err := s.RecordEffect(Effect{
			SessionID: reviewer,
			Role:      "auditor",
			Message:   diagnosticPrompt(s, cause),
			Reason:    "diagnostic opened: " + cause.Detail,
			CreatedAt: stamp,
		}); err != nil {
			return err
		}
		if s.Stage == StageWorking {
			return s.transition(StageDiagnostic, cause.Detail, stamp)
		}
		return nil
	}

	// Everything else is an ordinary checkpoint review, which is what resets debt.
	reviewID := fmt.Sprintf("C%d", s.Checkpoint.Sequence+1)
	s.ActiveReview = NewActiveReview(ReviewCheckpoint, reviewID, reviewer, s.Generation, s.Checkpoint.Sequence+1)
	s.ActiveReview.RequestedAt = stamp
	s.Checkpoint.ReviewPending = true
	// The debt is NOT reset by scheduling. Only a reviewed advance resets it,
	// because opening a review has established nothing about progress.
	if err := s.RecordEffect(Effect{
		SessionID: reviewer,
		Role:      "auditor",
		Message:   checkpointReviewPrompt(s, cause.Detail),
		Reason:    "checkpoint review scheduled by supervision: " + cause.Detail,
		CreatedAt: stamp,
	}); err != nil {
		return err
	}
	if s.Stage == StageWorking {
		return s.transition(StageCheckpointReview, cause.Detail, stamp)
	}
	return nil
}

// diagnosticPrompt builds the instruction for an independent investigation.
func diagnosticPrompt(s *State, cause WakeCause) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An independent diagnosis is owed for task %s.\n\n", s.TaskID)
	fmt.Fprintf(&b, "Escalation: %s\n", cause.Detail)
	fmt.Fprintf(&b, "Objective failures recorded: %d\n", s.Supervision.ObjectiveFailures)
	fmt.Fprintf(&b, "Repair cycles used: %d of %d\n", s.RepairCycles, s.effectiveRepairLimit())
	fmt.Fprintf(&b, "Diagnostic rounds used: %d of %d\n", s.DiagnosticRounds, s.guidanceAllowed())

	b.WriteString("\n# Failure history (objective, not self-declared)\n")
	if len(s.Failures) == 0 {
		b.WriteString("No failures recorded.\n")
	}
	for i, f := range s.Failures {
		fmt.Fprintf(&b, "%d. %s — signature: %s\n", i+1, f.CheckID, f.Signature)
		if f.Hypothesis != "" {
			fmt.Fprintf(&b, "   worker hypothesis (claim, unverified): %s\n", f.Hypothesis)
		}
	}

	b.WriteString("\n# What is wanted\n")
	b.WriteString("Select exactly one of GUIDANCE, REFRESH or BLOCKED for the JSON outcome.\n")
	b.WriteString("Name the most likely cause and the next concrete action. A diagnostic may not approve the\n")
	b.WriteString("request: it cannot publish a PASS, and it cannot reset the repair budget.\n")
	return b.String() + reviewAnswerInstruction(s)
}
