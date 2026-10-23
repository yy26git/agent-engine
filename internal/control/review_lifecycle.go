package control

import (
	"context"
	"errors"
	"fmt"
	"github.com/emjin/agent-engine/contract"
	"path/filepath"
	"strings"
	"time"
)

// RuntimeBlock records a lifecycle failure; it never supplies an audit verdict.
type RuntimeBlock struct {
	Kind              string `json:"kind"`
	ReviewID          string `json:"review_id,omitempty"`
	EffectID          string `json:"effect_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
	Generation        int    `json:"generation"`
	FirstObservedAt   string `json:"first_observed_at"`
	Reason            string `json:"reason"`
	CauseKind         string `json:"cause_kind,omitempty"`
	CauseReason       string `json:"cause_reason,omitempty"`
	IntendedOutcome   string `json:"intended_outcome"`
	EvidenceReference string `json:"evidence_reference,omitempty"`
}

// CleanupAttempt bounds automatic cleanup without producing progress per tick.
type CleanupAttempt struct {
	Deadline      string `json:"deadline"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	Attempts      int    `json:"attempts"`
	Exhausted     bool   `json:"exhausted"`
}

func (s *State) reviewDeadline(cfg *Config) string {
	if s.ActiveReview == nil || s.ActiveReview.RequestedAt == "" || s.PolicyDigest != cfg.Digest {
		return ""
	}
	start, err := time.Parse(time.RFC3339, s.ActiveReview.RequestedAt)
	if err != nil {
		return ""
	}
	duration := cfg.Checkpoints.ReviewTimeout
	if duration <= 0 {
		duration = DefaultCheckpointPolicy().ReviewTimeout
	}
	deadline := start.Add(duration)
	if s.Episode != nil && s.Episode.Budget.WallSeconds > 0 {
		if root, err := time.Parse(time.RFC3339, s.Episode.StartedAt); err == nil {
			cap := root.Add(time.Duration(s.Episode.Budget.WallSeconds) * time.Second)
			if cap.Before(deadline) {
				deadline = cap
			}
		}
	}
	return deadline.UTC().Format(time.RFC3339)
}

func (e *Engine) latchRuntimeBlock(id string, observed *State, kind, reason string) error {
	_, err := e.store.UpdateCommitted(id, func(s *State) error {
		if s.Stage.Terminal() || s.CancellationRequested || s.RuntimeBlock != nil {
			return nil
		}
		if s.Generation != observed.Generation {
			return nil
		}
		reviewID, session, effect := "", "", ""
		if observed.ActiveReview != nil {
			if s.ActiveReview == nil || s.ActiveReview.ReviewID != observed.ActiveReview.ReviewID ||
				s.ActiveReview.AnswerNonce != observed.ActiveReview.AnswerNonce {
				return nil
			}
			reviewID = observed.ActiveReview.ReviewID
			session = observed.ActiveReview.ExpectedReviewer
			if s.Episode != nil {
				for _, step := range s.Episode.Execution.Steps {
					if step.Session == session && step.Phase == s.Episode.Phase {
						effect = step.EffectID
					}
				}
			}
		}
		s.RuntimeBlock = &RuntimeBlock{Kind: kind, ReviewID: reviewID, SessionID: session, EffectID: effect,
			Generation: s.Generation, FirstObservedAt: e.stamp(), Reason: displayText(reason), IntendedOutcome: string(contract.StatusBlocked)}
		diagnostic := map[string]any{"request_id": id, "kind": kind, "review_id": reviewID, "session_id": session, "effect_id": effect, "generation": s.Generation, "reason": s.RuntimeBlock.Reason, "observed_at": s.RuntimeBlock.FirstObservedAt}
		rel := filepath.Join(RequestsDirName, id, "diagnostics", "runtime-block-"+kind+".json")
		if err := writeJSONDurable(e.cfg.StoreRoot, rel, diagnostic, 0600); err != nil {
			return err
		}
		s.RuntimeBlock.EvidenceReference = rel
		s.Checkpoint.MutationGrantsPaused = true
		s.StageReason = s.RuntimeBlock.Reason
		return nil
	})
	return err
}

// reconcileRuntimeBlock uses the same teardown body, with all role sessions in
// scope: an unavailable reviewer must not leave other task agents running.
func (e *Engine) reconcileRuntimeBlock(ctx context.Context, id string) error {
	s, err := e.store.State(id)
	if err != nil {
		return err
	}
	if s.RuntimeBlock == nil || s.Stage.Terminal() {
		return nil
	}
	ready, err := e.beginCleanupAttempt(id)
	if err != nil || !ready {
		return err
	}
	rep, err := e.teardown(ctx, id, terminationScope{kind: TerminationFailure, stopSessions: true, stopProcesses: true})
	if err != nil {
		return err
	}
	if !rep.Quiesced {
		return e.recordCleanupBlock(id, rep.Reason())
	}
	// Re-read under the commit lock: cancellation wins a race with failure.
	_, err = e.store.UpdateCommitted(id, func(cur *State) error {
		if cur.Stage.Terminal() || cur.CancellationRequested {
			return nil
		}
		reason := cur.RuntimeBlock.Reason
		if cur.RuntimeBlock.CauseKind != "" {
			cur.RuntimeBlock.Kind = cur.RuntimeBlock.CauseKind
			cur.RuntimeBlock.Reason = cur.RuntimeBlock.CauseReason
			reason = cur.RuntimeBlock.CauseReason
		} else if cur.RuntimeBlock.Kind == "cleanup-unresolved" {
			// Release is now proven. Do not keep projecting unresolved cleanup.
			reason = "execution stopped after a cleanup failure; owned effects are now quiescent"
			cur.RuntimeBlock = nil
		}
		if err := e.store.writeResultBody(id, contract.Result{Status: contract.StatusBlocked, Summary: reason, Reason: reason}); err != nil {
			return err
		}
		digest, err := e.store.digestResultBody(id)
		if err != nil {
			return err
		}
		cur.ResultDigest = digest
		cur.ResultStatus = string(contract.StatusBlocked)
		cur.Cleanup = nil
		return cur.transition(StageBlocked, reason, e.stamp())
	})
	return err
}

func (e *Engine) handleReviewCollection(ctx context.Context, id string, s *State, collectionErr error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	kind, reason := "", ""
	// Actual answer collection happens first. No prompt is repeated.
	if s.Episode != nil {
		for _, step := range s.Episode.Execution.Steps {
			if step.Phase == s.Episode.Phase && step.Session == s.ActiveReview.ExpectedReviewer {
				if eff, ok := s.EffectByID(step.EffectID); ok && (eff.Status == EffectBlocked || eff.Status == EffectFailed) {
					kind = "review-delivery"
					reason = "review prompt delivery is blocked: " + eff.LastError
					break
				}
			}
		}
	}
	deadline := s.ActiveReview.Deadline
	if deadline == "" {
		deadline = s.reviewDeadline(e.cfg)
		if deadline == "" {
			kind = "review-timeout"
			reason = "review deadline cannot be reconstructed from the admitted policy"
		}
	}
	if kind == "" && deadline != "" {
		at, err := time.Parse(time.RFC3339, deadline)
		if err != nil || !e.now().Before(at) {
			kind = "review-timeout"
			reason = "the independent review deadline expired without a valid answer"
		}
	}
	if kind == "" {
		backend, ref, err := e.dispatch.backendFor(id, s.ActiveReview.ExpectedReviewer)
		if err == nil {
			sess, err := backend.Inspect(ctx, ref)
			if err == nil {
				switch strings.ToLower(strings.TrimSpace(sess.Status)) {
				case "stopped", "dead", "exited", "closed", "error", "errored", "stale":
					kind = "reviewer-unavailable"
					reason = "the independent reviewer reports " + displayText(sess.Status) + " and has no valid retained answer"
					if !errors.Is(collectionErr, ErrReviewerSilent) && !errors.Is(collectionErr, ErrReviewerUnreadable) {
						kind = "review-invalid"
						reason = "the independent reviewer's final answer was refused: " + collectionErr.Error()
					}
				}
			}
		}
	}
	if kind == "" {
		return nil
	} // Running partial/unreadable output is bounded pending.
	if err := e.latchRuntimeBlock(id, s, kind, reason); err != nil {
		return err
	}
	return e.reconcileRuntimeBlock(ctx, id)
}

func (e *Engine) checkRootDeadline(ctx context.Context, id string, s *State) (bool, error) {
	return e.reconcileEpisodeDeadline(ctx, id, s)
}

// ExecutionDeadline is the earliest admitted wall or pilot deadline. Recovery
// is allowed after it expires, but no execution grant is renewed here.
func (s *State) ExecutionDeadline() (time.Time, error) {
	if s.Episode == nil || s.Episode.Budget.WallSeconds <= 0 {
		return time.Time{}, nil
	}
	start, err := time.Parse(time.RFC3339, s.Episode.StartedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("episode start time is unreadable: %w", err)
	}
	deadline := start.Add(time.Duration(s.Episode.Budget.WallSeconds) * time.Second)
	if s.LocalPilot != nil {
		expires, err := time.Parse(time.RFC3339, s.LocalPilot.ExpiresAt)
		if err != nil {
			return time.Time{}, fmt.Errorf("pilot expiry is unreadable: %w", err)
		}
		if expires.Before(deadline) {
			deadline = expires
		}
	}
	return deadline, nil
}

// ReconcileExpiredEpisode runs with a fresh, bounded recovery context after
// the execution context has expired. It owns the durable failure and teardown.
func (e *Engine) ReconcileExpiredEpisode(ctx context.Context, id string) (bool, error) {
	s, err := e.store.State(id)
	if err != nil {
		return false, err
	}
	if s.Stage.Terminal() {
		return true, nil
	}
	return e.reconcileEpisodeDeadline(ctx, id, s)
}

func (e *Engine) reconcileEpisodeDeadline(ctx context.Context, id string, s *State) (bool, error) {
	deadline, err := s.ExecutionDeadline()
	if err != nil {
		return true, err
	}
	if deadline.IsZero() || e.now().Before(deadline) {
		return false, nil
	}
	if err := e.latchRuntimeBlock(id, s, "episode-timeout", "the admitted episode execution deadline expired"); err != nil {
		return true, err
	}
	return true, e.reconcileRuntimeBlock(ctx, id)
}
