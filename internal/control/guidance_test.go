package control

// LF1.2 turn-boundary guidance tests.
//
// The claim: an instruction asked for while a turn is live is neither lost nor
// sent concurrently. It is committed durably and delivered when a prompt becomes
// legal again.

import (
	"context"
	"testing"
)

func guidanceFixture(t *testing.T) *fixture {
	t.Helper()
	f := boundRequest(t, newFixture(t))
	return f
}

// TestGuidance_QueuedWhileATurnIsLiveIsNotLost is the core property.
//
// A reviewer answering mid-turn must not have to wait for the worker, and the
// instruction must survive the wait.
func TestGuidance_QueuedWhileATurnIsLiveIsNotLost(t *testing.T) {
	f := guidanceFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.QueueGuidance(Guidance{
			Message:   "Stop widening the change; fix the failing test first.",
			Reason:    "reviewer redirected",
			SessionID: "sess-worker-1",
			QueuedAt:  s.UpdatedAt,
		})
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := len(st.UndeliveredGuidance()); n != 1 {
		t.Fatalf("undelivered guidance = %d, want 1: an instruction asked for mid-turn was lost", n)
	}
	// Nothing was sent: the worker is mid-turn.
	if len(st.Effects) != 0 {
		t.Errorf("guidance was sent while a turn was live: %+v", st.Effects)
	}
}

// TestGuidance_IsNotPromotedWhileAReviewIsActive is the boundary that matters.
//
// Guidance resumes work. Sending it while the worker owes a review answer would
// put two instructions in flight around the same turn.
func TestGuidance_IsNotPromotedWhileAReviewIsActive(t *testing.T) {
	f := guidanceFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.QueueGuidance(Guidance{Message: "narrow it", SessionID: "sess-worker-1", QueuedAt: s.UpdatedAt}); err != nil {
			return err
		}
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C1", "sess-auditor-1", s.Generation, 1)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if _, ok := st.PromoteGuidanceAtTurnBoundary(st.UpdatedAt); ok {
		t.Error("guidance was promoted while a review was active")
	}
}

// TestGuidance_IsNotPromotedAfterACompletionClaim is the rule that keeps a
// finished request finished.
func TestGuidance_IsNotPromotedAfterACompletionClaim(t *testing.T) {
	f := guidanceFixture(t)
	b := newEmittingBackend("minimax-acp")
	acpBoundRequest(t, f, "acp-1")
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		if err := s.QueueGuidance(Guidance{Message: "keep going", SessionID: "acp-1", QueuedAt: s.UpdatedAt}); err != nil {
			return err
		}
		s.ActiveReview = NewActiveReview(ReviewCompletion, "A1", "sess-auditor-1", s.Generation, 1)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now}, b)
	defer stop()
	eng.applyPromptReceipt(context.Background(), "forge-run-1842-repair", b,
		receiptEvent("acp-1", "acp:acp-1:1", "end_turn"))

	st, _ := f.store.State("forge-run-1842-repair")
	for _, e := range st.Effects {
		if e.Role == "worker" && e.Message == "keep going" {
			t.Fatal("more work was sent to a worker that had just claimed completion")
		}
	}
}

// TestGuidance_DuplicateRequestsCollapse is the idempotence property.
func TestGuidance_DuplicateRequestsCollapse(t *testing.T) {
	f := guidanceFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
			return s.QueueGuidance(Guidance{Message: "same instruction", SessionID: "sess-worker-1", QueuedAt: s.UpdatedAt})
		}); err != nil {
			t.Fatalf("queue %d: %v", i, err)
		}
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if n := len(st.UndeliveredGuidance()); n != 1 {
		t.Errorf("queued guidance = %d, want 1: asking twice queued it twice", n)
	}
}

// TestGuidance_CancellationRefusesQueueing keeps cancellation authoritative.
func TestGuidance_CancellationRefusesQueueing(t *testing.T) {
	f := guidanceFixture(t)
	cancelledRequest(t, f)
	_, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.QueueGuidance(Guidance{Message: "one more thing", SessionID: "sess-worker-1", QueuedAt: s.UpdatedAt})
	})
	if err == nil {
		t.Error("guidance was queued for a request being cancelled")
	}
}

// TestGuidance_DeliveryIsAcknowledgedNotAssumed is the property that stops an
// instruction being believed sent when nothing was sent.
func TestGuidance_DeliveryIsAcknowledgedNotAssumed(t *testing.T) {
	f := guidanceFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.QueueGuidance(Guidance{Message: "do this next", SessionID: "sess-worker-1", QueuedAt: s.UpdatedAt})
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	g := st.UndeliveredGuidance()[0]

	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.AcknowledgeGuidance(g.GuidanceID)
	}); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if n := len(st.UndeliveredGuidance()); n != 0 {
		t.Errorf("undelivered guidance = %d after acknowledgement, want 0", n)
	}
	// Acknowledging an unknown item is an error, not a silent success.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.AcknowledgeGuidance("G-nope-deadbeef")
	}); err == nil {
		t.Error("acknowledging guidance that was never queued succeeded")
	}
}
