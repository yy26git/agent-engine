package control

// ACP observation classification.
//
// The mapping from an ACP notification to "what does this mean for supervision"
// has one job, and the job is mostly about what it must NOT do.
//
// The temptation is to treat a successful `session/prompt` as good news. It is
// not. A completed turn proves the agent processed a prompt; it says nothing
// about whether the work passed, and a supervisor that reads it as an outcome is
// the same defect as a reviewer approving itself. Tool activity is weaker still:
// a tool call proves the agent is doing something, and "something" is not
// "finished", "correct", or even "intended".
//
// So the vocabulary below separates three questions that are constantly
// collapsed into one: is the agent busy, did the turn end, and did the work pass.
// Only the first two are answerable here. The third is answered by an independent
// reviewer, and this file cannot produce it even if asked.
//
// UNKNOWN is a real value rather than an error to be resolved later. An
// unrecognised notification is recorded and routed nowhere, because a vocabulary
// this layer has not observed is a vocabulary it must not guess at.

import (
	"encoding/json"
	"strings"
)

// ACPObservation is what one ACP notification means for supervision.
type ACPObservation string

const (
	// ACPObsUnknown is a notification this layer cannot classify.
	//
	// It is journalled like any other observation and drives NOTHING. Mapping an
	// unrecognised value to something convenient is how an unobserved protocol
	// becomes an invented one.
	ACPObsUnknown ACPObservation = "unknown"
	// ACPObsTurnStarted means a turn has begun.
	ACPObsTurnStarted ACPObservation = "turn_started"
	// ACPObsActivity means the agent is doing something.
	//
	// Tool calls and message chunks land here. It is deliberately the largest
	// bucket in this vocabulary, because it is the one that proves least.
	ACPObsActivity ACPObservation = "activity"
	// ACPObsTurnCompleted means the agent's turn ended.
	//
	// It is NOT a completion of the work. It is the end of a conversational
	// turn, which may contain a refusal, a partial fix, or a question.
	ACPObsTurnCompleted ACPObservation = "turn_completed"
	// ACPObsTurnFailed means the turn ended in an error.
	ACPObsTurnFailed ACPObservation = "turn_failed"
	// ACPObsCancelled means the turn was cancelled.
	ACPObsCancelled ACPObservation = "cancelled"
)

// Terminal reports whether this observation ended the turn.
//
// This is deliberately NOT named "Complete". "Complete" invites exactly the
// reading this file exists to prevent.
func (o ACPObservation) Terminal() bool {
	switch o {
	case ACPObsTurnCompleted, ACPObsTurnFailed, ACPObsCancelled:
		return true
	default:
		return false
	}
}

// EngineeringPass is not answerable from an observation, and this method exists
// to say so.
//
// It always returns false. That is not a stub: it is the encoding of the rule
// that transport evidence can never stand in for a reviewer's judgement. A
// caller that wants a PASS must go and get one.
func (o ACPObservation) EngineeringPass() bool { return false }

// String renders the observation for logs.
func (o ACPObservation) String() string { return string(o) }

// acpUpdateEnvelope is the shape this layer reads out of an ACP notification.
//
// It is intentionally permissive about unknown fields and strict about the one
// it depends on. A newer server that adds fields still classifies; a server that
// renames the one field this reads produces ACPObsUnknown rather than a guess.
type acpUpdateEnvelope struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
	} `json:"update"`
	// StopReason appears on some servers at the top level rather than inside
	// update; both are read because guessing which one a server uses is how a
	// real signal gets dropped.
	StopReason string `json:"stopReason"`
}

// ClassifyACPNotification maps one ACP notification onto an observation.
//
// ok is false when the payload is not a notification this layer can read at all,
// which is different from ok with ACPObsUnknown — the first means "I could not
// parse this", the second means "I parsed it and it means nothing to me".
//
// NOTE ON THE TABLE: the sessionUpdate values below are the ACP vocabulary, not
// values captured from a live `mcode acp` turn. AP0 established the method names
// and the session/prompt shape but never observed a real update stream, because
// that needs an authenticated prompt. The table is therefore written to fail
// closed: anything not listed is ACPObsUnknown, so an unobserved value costs
// visibility and cannot cost correctness. It must be re-checked against a real
// capture before this mapping is trusted for anything load-bearing.
func ClassifyACPNotification(method string, payload []byte) (ACPObservation, bool) {
	if payload == nil {
		return ACPObsUnknown, false
	}

	switch {
	case strings.HasSuffix(method, "current_session_update"):
		// A vendor "what is happening now" signal. Useful for liveness, not an
		// outcome.
		return ACPObsActivity, true

	case method == "session/cancel" || strings.HasSuffix(method, "queue_update"):
		return ACPObsUnknown, true

	case method == "session/update":
		var env acpUpdateEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return ACPObsUnknown, false
		}
		return classifyUpdateValue(env), true

	default:
		return ACPObsUnknown, true
	}
}

// classifyUpdateValue maps the inner sessionUpdate discriminator.
//
// A stop reason is checked FIRST, and the ordering is load-bearing. The two
// signals routinely arrive together — a message chunk carrying `end_turn` is the
// normal shape of a finished turn — so whichever is examined first wins. With the
// activity kind first, a turn could never be marked terminal, and a supervisor
// would sit watching an agent that has already stopped.
//
// A terminal signal must therefore mask an activity signal. The reverse is not
// true: seeing that a tool ran never un-finishes a turn.
//
// The fallthrough is ACPObsUnknown, not a best guess. Every value below is
// checked against what it PROVES: tool activity proves activity and nothing
// more; a stop reason proves the turn ended and nothing more.
func classifyUpdateValue(env acpUpdateEnvelope) ACPObservation {
	// A stop reason may arrive as an update or alongside one.
	switch normalizeStopReason(env.StopReason) {
	case "end_turn", "endturn":
		// PROVES: the turn ended. The work is exactly as verified as it was
		// before the prompt.
		return ACPObsTurnCompleted
	case "cancelled", "canceled":
		return ACPObsCancelled
	case "refusal", "error":
		return ACPObsTurnFailed
	}

	switch strings.ToLower(strings.TrimSpace(env.Update.SessionUpdate)) {
	case "tool_call", "tool_call_update":
		// PROVES: the agent invoked or updated a tool. It does not prove the tool
		// succeeded, that it was permitted, or that it did anything useful.
		return ACPObsActivity

	case "agent_message_chunk", "agent_thought_chunk", "user_message_chunk":
		// PROVES: output was produced. Nothing about its content is verified here.
		return ACPObsActivity

	case "plan":
		// A plan is an intention, and an intention is the furthest thing from a
		// result.
		return ACPObsActivity

	case "current_mode_update", "available_commands_update":
		return ACPObsActivity
	}

	return ACPObsUnknown
}

// normalizeStopReason folds the spellings seen into one comparison.
func normalizeStopReason(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", ""))
}

// ObservationFromReceipt derives an observation from a completed prompt.
//
// This exists because a `session/prompt` answer is the strongest signal the ACP
// transport offers, and it is still not a result. The function is here so that
// every caller maps it the same way rather than each inventing a reading.
func ObservationFromReceipt(stopReason string) ACPObservation {
	switch normalizeStopReason(stopReason) {
	case "end_turn", "endturn":
		return ACPObsTurnCompleted
	case "cancelled", "canceled":
		return ACPObsCancelled
	case "refusal", "error":
		return ACPObsTurnFailed
	default:
		// A stop reason this layer has never seen is not a success.
		return ACPObsUnknown
	}
}
