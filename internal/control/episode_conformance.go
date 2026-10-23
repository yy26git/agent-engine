package control

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/emjin/agent-engine/contract"
)

// recordEpisodeTurn records a claim and its measured diff, never completion.
// Managed verification and an independent completion audit remain owed.
func (e *Engine) recordEpisodeTurn(id, backend, session, turnID, sendID, model string) error {
	st, err := e.store.State(id)
	if err != nil {
		return err
	}
	if st.Episode == nil || st.Episode.Phase != string(PhaseImplement) || !st.Episode.PlanAuthorized {
		return fmt.Errorf("implementation turn is outside an authorized phase")
	}
	key := episodeStepKey(PhaseImplement, st.episodePhaseRound(PhaseImplement))
	step, ok := st.episodeStep(key)
	if !ok || step.Session != session {
		return fmt.Errorf("implementation turn has no matching phase dispatch")
	}
	if step.TurnID != "" {
		return nil
	}
	binding, ok := st.BackendForID(session)
	if !ok || binding.Backend != backend || step.Generation != st.Generation {
		return fmt.Errorf("implementation turn binding is stale")
	}
	if turnID == "" {
		return fmt.Errorf("implementation turn has no identity")
	}
	profile, admittedProfile := st.ExecutionProfiles[RoleWorker]
	if !admittedProfile || (profile.Model != "" && model != "" && profile.Model != model) {
		return fmt.Errorf("implementation turn lacks matching answer-turn model attribution (expected %q, observed %q)", profile.Model, model)
	}
	if sendID != "" {
		matched := false
		for _, effect := range st.Effects {
			if effect.EffectID == step.EffectID && effect.SendID == sendID {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("implementation receipt does not match the phase dispatch")
		}
	}
	if st.Episode.PlanBaseline == nil {
		return fmt.Errorf("accepted plan has no measured baseline; re-acceptance is required")
	}
	body, err := e.ReadEpisodeArtifact(id, st.Episode.PlanDigest)
	if err != nil {
		return err
	}
	var plan contract.ImplementationPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		return err
	}
	admitted, err := e.store.loadRequestDir(id)
	if err != nil {
		return err
	}
	current, err := TakeSnapshot(st.Workspace, e.nowFn(), e.store.Config().StoreRoot)
	if err != nil {
		return err
	}
	changed := map[string]bool{}
	for path, hash := range current.Files {
		if st.Episode.PlanBaseline.Files[path] != hash {
			changed[path] = true
		}
	}
	for path, hash := range st.Episode.PlanBaseline.Files {
		if current.Files[path] != hash {
			changed[path] = true
		}
	}
	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	receipts := make([]contract.ConformanceReceipt, 0, len(plan.Steps))
	for _, planned := range plan.Steps {
		receipt := contract.ConformanceReceipt{Schema: contract.EpisodeVersion, EpisodeID: st.Episode.EpisodeID, PlanVersion: plan.Version, PlanDigest: st.Episode.PlanDigest, StepID: planned.StepID, Iteration: st.episodePhaseRound(PhaseImplement), SourceDigest: current.ContentDigest, ChangedFiles: paths, ObservedAt: e.stamp()}
		receipt.Deviations = contract.DetectPlanDeviations(&receipt, &plan)
		// DetectPlanDeviations uses the same effective plan scope as proposal
		// preflight; the admitted request's independent boundary is checked below.
		for _, changedPath := range paths {
			protected := false
			for _, pattern := range admitted.Request.Constraints.ProtectedPaths {
				if pathCovered(pattern, changedPath) {
					protected = true
				}
			}
			if protected {
				receipt.Deviations = append(receipt.Deviations, contract.ConformanceDeviation{Kind: contract.DeviationProtectedPathWrite, Path: changedPath, Detail: "changed path is protected by the admitted local request"})
			}
			if len(admitted.Request.Constraints.AllowedPaths) > 0 {
				allowed := false
				for _, pattern := range admitted.Request.Constraints.AllowedPaths {
					if pathCovered(pattern, changedPath) {
						allowed = true
					}
				}
				if !allowed {
					receipt.Deviations = append(receipt.Deviations, contract.ConformanceDeviation{Kind: contract.DeviationUndeclaredEdit, Path: changedPath, Detail: "changed path exceeds the admitted local scope"})
				}
			}
		}
		if err := contract.ValidateConformanceReceiptAgainstPlan(&receipt, &plan); err != nil {
			return err
		}
		receipts = append(receipts, receipt)
	}
	_, err = e.store.UpdateCommitted(id, func(s *State) error {
		fresh, ok := s.episodeStep(key)
		b, bound := s.BackendForID(session)
		currentProfile, profileBound := s.ExecutionProfiles[RoleWorker]
		if !ok || !bound || !profileBound || currentProfile.Name != profile.Name || currentProfile.Model != profile.Model || b.Backend != backend || fresh.Session != session || s.Generation != step.Generation || s.Episode.Phase != string(PhaseImplement) || s.Episode.PlanDigest != st.Episode.PlanDigest || !s.Episode.PlanAuthorized {
			return fmt.Errorf("implementation attribution changed during conformance measurement")
		}
		if fresh.TurnID != "" {
			return nil
		}
		for i := range s.Episode.Execution.Steps {
			if s.Episode.Execution.Steps[i].Key == key {
				s.Episode.Execution.Steps[i].TurnID = turnID
				s.Episode.Execution.Steps[i].SourceModel = model
			}
		}
		s.Episode.Execution.Conformance = receipts
		return nil
	})
	return err
}

func pathCovered(pattern, file string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	file = strings.TrimPrefix(file, "./")
	if strings.HasSuffix(pattern, "/**") {
		root := strings.TrimSuffix(pattern, "/**")
		return file == root || strings.HasPrefix(file, root+"/")
	}
	ok, _ := path.Match(pattern, file)
	return ok
}
