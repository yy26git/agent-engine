package control

import (
	"fmt"
	"time"
)

func (e *Engine) beginCleanupAttempt(id string) (bool, error) {
	ready := false
	_, err := e.store.UpdateCommitted(id, func(s *State) error {
		now := e.now()
		if s.Cleanup == nil {
			start := now
			if at, err := time.Parse(time.RFC3339, s.CancellationRequestedAt); err == nil {
				start = at
			}
			if s.CancellationRequested {
				for _, h := range s.History {
					if h.Event == "cancellation_requested" {
						if t, err := time.Parse(time.RFC3339, h.At); err == nil {
							start = t
						}
						break
					}
				}
			}
			deadline := start.Add(time.Minute)
			if s.Episode != nil {
				if root, err := time.Parse(time.RFC3339, s.Episode.StartedAt); err == nil {
					cap := root.Add(time.Duration(s.Episode.Budget.WallSeconds) * time.Second)
					if s.Episode.Budget.WallSeconds > 0 && cap.After(start) && cap.Before(deadline) {
						deadline = cap
					}
				}
			}
			s.Cleanup = &CleanupAttempt{Deadline: deadline.UTC().Format(time.RFC3339)}
		}
		if s.Cleanup.Exhausted {
			return nil
		}
		deadline, err := time.Parse(time.RFC3339, s.Cleanup.Deadline)
		if err != nil {
			return fmt.Errorf("invalid cleanup deadline")
		}
		if !now.Before(deadline) {
			s.Cleanup.Exhausted = true
			return nil
		}
		if next, err := time.Parse(time.RFC3339, s.Cleanup.NextAttemptAt); err == nil && now.Before(next) {
			return nil
		}
		s.Cleanup.Attempts++
		delay := time.Duration(2) * time.Second
		for i := 1; i < s.Cleanup.Attempts && delay < 30*time.Second; i++ {
			delay *= 2
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		s.Cleanup.NextAttemptAt = now.Add(delay).UTC().Format(time.RFC3339)
		ready = true
		return nil
	})
	return ready, err
}
func (e *Engine) recordCleanupBlock(id, reason string) error {
	_, err := e.store.UpdateCommitted(id, func(s *State) error {
		if s.RuntimeBlock == nil {
			s.RuntimeBlock = &RuntimeBlock{Kind: "cleanup-unresolved", Generation: s.Generation, FirstObservedAt: e.stamp(), IntendedOutcome: "BLOCKED"}
			if s.CancellationRequested {
				s.RuntimeBlock.IntendedOutcome = "CANCELLED"
			}
		}
		if s.RuntimeBlock.Kind != "cleanup-unresolved" && s.RuntimeBlock.CauseKind == "" {
			s.RuntimeBlock.CauseKind = s.RuntimeBlock.Kind
			s.RuntimeBlock.CauseReason = s.RuntimeBlock.Reason
		}
		s.RuntimeBlock.Kind = "cleanup-unresolved"
		s.RuntimeBlock.Reason = displayText(reason)
		s.StageReason = s.RuntimeBlock.Reason
		if s.CancellationRequested {
			s.CancellationReason = s.RuntimeBlock.Reason
		}
		s.Checkpoint.MutationGrantsPaused = true
		return nil
	})
	return err
}
