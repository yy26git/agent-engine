package control

import (
	"context"
	"fmt"
)

// RecoverCompletedEpisodeReview consumes the existing independent answer before
// recreating transports on resume. The owning supervisor must establish owned
// process quiescence before canonical completion can run managed checks. It
// queries the bound reviewer; no imported answer, new model turn or worker is
// created. An unanswered/refused review never permits replacement worker spawn.
func (e *Engine) RecoverCompletedEpisodeReview(ctx context.Context, id string) (bool, error) {
	s, err := e.store.State(id)
	if err != nil {
		return false, err
	}
	if s.Episode == nil || s.Episode.Phase != string(PhaseCompletionReview) || s.ActiveReview == nil || !s.ActiveReview.Kind.Approves() {
		return false, nil
	}
	binding, ok := s.BackendForID(s.ActiveReview.ExpectedReviewer)
	if !ok || binding.Backend != "agent-deck" {
		return true, fmt.Errorf("agent-engine: pending completion reviewer cannot be recovered through its bound transport")
	}
	answer, err := e.CollectReviewerAnswer(ctx, id, 0)
	if err != nil {
		return true, err
	}
	if problems := e.ReapOrphanedProcesses(ctx, id); len(problems) != 0 {
		return true, fmt.Errorf("agent-engine: completion recovery cannot establish quiescence: %v", problems)
	}
	if _, err := e.store.SubmitAnswer(id, answer); err != nil {
		return true, err
	}
	fresh, err := e.store.State(id)
	if err != nil {
		return true, err
	}
	if !fresh.Stage.Terminal() {
		return true, fmt.Errorf("agent-engine: completion review settled; further work remains and no worker was restarted")
	}
	return true, nil
}
