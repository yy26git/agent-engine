package control

// Turn-boundary guidance.
//
// A coding agent's turn does not tolerate a second prompt arriving mid-turn, and
// Agent Engine will not pretend otherwise. What it CAN do is stop losing the
// instruction: guidance asked for while a turn is live is committed durably and
// delivered at the next turn boundary, when the worker is idle and a prompt is
// legal.
//
// This is the same shape as a retained assessment candidate — durable while
// blocked, promoted when the blocker clears — and it is deliberately built the
// same way. Two mechanisms with the same discipline are easier to reason about
// than one mechanism with two behaviours.
//
// The delivery rule is narrow: guidance is promoted only when the turn has
// genuinely ended AND the turn did not claim completion. Sending more work to a
// worker that just said "I finished" is how a request stops being finished.

import (
	"fmt"
	"strings"
)

// Guidance is one instruction addressed to the worker at a safe turn boundary.
type Guidance struct {
	// GuidanceID is a derived identity, so re-asking for the same guidance does
	// not queue it twice.
	GuidanceID string `json:"guidance_id"`
	// Message is the instruction text.
	Message string `json:"message"`
	// Reason explains why it was raised.
	Reason string `json:"reason,omitempty"`
	// SessionID is the worker it is addressed to.
	SessionID string `json:"session_id,omitempty"`
	// QueuedAt is when it became durable.
	QueuedAt string `json:"queued_at"`
	// Delivered records that the worker actually received it.
	//
	// Delivery is acknowledged rather than assumed: a prompt sent at a turn
	// boundary can still fail, and an instruction believed delivered but never
	// received is the one failure mode this whole mechanism exists to prevent.
	Delivered bool `json:"delivered,omitempty"`
}

// MaxGuidanceQueue bounds the durable guidance ledger.
//
// It is backpressure, not garbage collection: delivered entries are only
// eligible for removal once the bound is reached, so a request that keeps asking
// for guidance cannot evict the record of what it has already delivered.
const MaxGuidanceQueue = 16

// GuidanceID derives a stable identity from an instruction and its recipient.
func GuidanceID(sessionID, message string) string {
	return fmt.Sprintf("G-%s-%08x", shortBackend(sessionID), fnv1a(message))
}

// QueueGuidance records an instruction for the worker without sending it.
//
// It is the only way guidance enters the queue, and it never performs I/O. The
// caller may be a reviewer answering mid-turn, and a turn-boundary queue exists
// precisely because that must not block on the worker.
func (s *State) QueueGuidance(g Guidance) error {
	if strings.TrimSpace(g.Message) == "" {
		return fmt.Errorf("agent-engine: guidance with no message cannot be queued")
	}
	if s.CancellationRequested {
		// Cancellation outranks guidance. Queuing work for a request being
		// stopped is how a cancelled request keeps writing.
		return fmt.Errorf("agent-engine: cancellation is pending, so guidance is refused rather than queued")
	}
	if s.Stage.Terminal() {
		return fmt.Errorf("agent-engine: %s is terminal; there is no turn to guide", s.Stage)
	}
	if g.GuidanceID == "" {
		g.GuidanceID = GuidanceID(g.SessionID, g.Message)
	}
	for _, existing := range s.PendingGuidance {
		if existing.GuidanceID == g.GuidanceID {
			// Already queued. Asking twice is not asking harder.
			return nil
		}
	}
	s.PendingGuidance = append(s.PendingGuidance, g)
	if len(s.PendingGuidance) > MaxGuidanceQueue {
		kept := make([]Guidance, 0, MaxGuidanceQueue)
		drop := len(s.PendingGuidance) - MaxGuidanceQueue
		for _, existing := range s.PendingGuidance {
			if drop > 0 && existing.Delivered {
				drop--
				continue
			}
			kept = append(kept, existing)
		}
		s.PendingGuidance = kept
	}
	return nil
}

// UndeliveredGuidance returns instructions queued but not yet confirmed received.
func (s *State) UndeliveredGuidance() []Guidance {
	var out []Guidance
	for _, g := range s.PendingGuidance {
		if !g.Delivered {
			out = append(out, g)
		}
	}
	return out
}

// PromoteGuidanceAtTurnBoundary returns the next instruction that is legal to
// send now.
//
// It refuses when anything makes a prompt unsafe or pointless:
//   - a turn is not over, because the transport refuses concurrent prompts;
//   - a review is active, because the worker is owed an answer;
//   - the worker just claimed completion, because more work would contradict it;
//   - cancellation is pending.
//
// The caller records the returned instruction as an effect; delivery itself is
// confirmed later by AcknowledgeGuidance.
func (s *State) PromoteGuidanceAtTurnBoundary(now string) (Guidance, bool) {
	if s.ActiveReview != nil || s.CancellationRequested || s.Stage.Terminal() {
		return Guidance{}, false
	}
	for _, g := range s.PendingGuidance {
		if g.Delivered {
			continue
		}
		return g, true
	}
	return Guidance{}, false
}

// AcknowledgeGuidance records that the worker actually received one instruction.
//
// Without it the queue would either resend forever or forget an instruction that
// was genuinely never delivered. A failed send leaves the item undelivered, which
// is the answer that lets it be retried at the next boundary.
func (s *State) AcknowledgeGuidance(id string) error {
	for i := range s.PendingGuidance {
		if s.PendingGuidance[i].GuidanceID == id {
			s.PendingGuidance[i].Delivered = true
			return nil
		}
	}
	return fmt.Errorf("agent-engine: guidance %s is not queued", id)
}

// fnv1a is the same short, stable hash the effect ledger uses.
//
// It is an identity label, not a security boundary; the guidance ID only has to
// change when the instruction changes, so re-asking for the same text collapses
// to one queued item.
func fnv1a(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
