package control

import "fmt"

// A receipt is retained before intake. Submit publishes before the dispatcher
// commits its send id; retaining this join avoids losing output in that window.
type EpisodeTurnReceipt struct {
	StepKey   string `json:"step_key"`
	Backend   string `json:"backend"`
	Session   string `json:"session"`
	SendID    string `json:"send_id"`
	Model     string `json:"model,omitempty"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Consumed  bool   `json:"consumed,omitempty"`
}

func (e *Engine) retainEpisodeTurnReceipt(id, backend, session, sendID, model, text string, truncated bool) error {
	if len(text) > 1<<20 {
		return fmt.Errorf("episode output exceeds retained limit")
	}
	_, err := e.store.UpdateCommitted(id, func(s *State) error {
		if s.Episode == nil || s.CancellationRequested || s.Stage.Terminal() {
			return nil
		}
		phase := EpisodePhase(s.Episode.Phase)
		if phase != PhaseReport && phase != PhasePlan && phase != PhaseImplement {
			return nil
		}
		key := episodeStepKey(phase, s.episodePhaseRound(phase))
		step, ok := s.episodeStep(key)
		binding, bound := s.BackendForID(session)
		if !ok || !bound || step.Session != session || binding.Backend != backend || step.Generation != s.Generation {
			return fmt.Errorf("receipt producer does not own the current phase")
		}
		for _, effect := range s.Effects {
			if effect.SessionID == session && effect.SendID == sendID && effect.EffectID != step.EffectID {
				return fmt.Errorf("receipt belongs to a prior phase dispatch")
			}
		}
		for _, receipt := range s.Episode.Execution.TurnReceipts {
			if receipt.Backend == backend && receipt.Session == session && receipt.SendID == sendID {
				return nil
			}
		}
		if len(s.Episode.Execution.TurnReceipts) >= MaxEpisodeSteps {
			return fmt.Errorf("episode receipt ledger is full; owed output is retained")
		}
		s.Episode.Execution.TurnReceipts = append(s.Episode.Execution.TurnReceipts, EpisodeTurnReceipt{StepKey: key, Backend: backend, Session: session, SendID: sendID, Model: model, Text: text, Truncated: truncated})
		return nil
	})
	return err
}

func (e *Engine) consumeEpisodeReceipts(id string, s *State) error {
	key := episodeStepKey(EpisodePhase(s.Episode.Phase), s.episodePhaseRound(EpisodePhase(s.Episode.Phase)))
	for _, receipt := range s.Episode.Execution.TurnReceipts {
		if receipt.Consumed || receipt.StepKey != key {
			continue
		}
		if receipt.Truncated {
			return fmt.Errorf("phase output was truncated; artifact intake is blocked")
		}
		var err error
		if s.Episode.Phase == string(PhaseImplement) {
			err = e.recordEpisodeTurn(id, receipt.Backend, receipt.Session, receipt.SendID, receipt.SendID, receipt.Model)
		} else {
			err = e.intakeEpisodeArtifact(id, receipt.Backend, receipt.Session, receipt.SendID, receipt.Model, receipt.Text)
		}
		if err != nil {
			return err
		}
		_, err = e.store.UpdateCommitted(id, func(current *State) error {
			for i := range current.Episode.Execution.TurnReceipts {
				r := &current.Episode.Execution.TurnReceipts[i]
				if r.Backend == receipt.Backend && r.Session == receipt.Session && r.SendID == receipt.SendID {
					r.Consumed = true
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
