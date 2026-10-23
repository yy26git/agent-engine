package control

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEpisodeConformance_TurnMeasuresUndeclaredEditWithoutApproving(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	id := "forge-run-1842-repair"
	st, _ := f.store.State(id)
	if err := os.MkdirAll(st.Workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Workspace, "main.go"), []byte("before"), 0644); err != nil {
		t.Fatal(err)
	}
	baseline, err := TakeSnapshot(st.Workspace, f.now, f.cfg.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan := contractPlanForTest(1)
	plan.SourceBaseline = baseline.ContentDigest
	for i := range plan.Steps {
		plan.Steps[i].AllowedPaths = []string{"main.go"}
	}
	digest, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.retainEpisodeArtifact(id, digest, &plan); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.UpdateCommitted(id, func(s *State) error {
		s.Episode.Phase = string(PhaseImplement)
		s.Episode.PhaseHistory = append(s.Episode.PhaseHistory, EpisodePhaseMove{To: string(PhaseImplement), At: f.now.Format(time.RFC3339)})
		s.Episode.PlanDigest = digest
		s.Episode.PlanAuthorized = true
		s.Episode.PlanBaseline = &baseline
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleWorker: {Backend: "minimax-acp:worker", SessionID: "worker"}}
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: {Name: "worker", Role: RoleWorker, Model: "actual-worker-model"}}
		s.recordEpisodeStep(EpisodeStep{Key: "IMPLEMENT#1", Phase: "IMPLEMENT", Round: 1, Role: string(RoleWorker), Session: "worker", Generation: s.Generation, EffectID: "effect", Status: StepDispatched})
		s.Effects = append(s.Effects, Effect{EffectID: "effect", SessionID: "worker", SendID: "send"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Workspace, "unplanned.go"), []byte("after"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"different-model"} {
		if err := eng.recordEpisodeTurn(id, "minimax-acp:worker", "worker", "send", "send", model); err == nil {
			t.Fatalf("unattributed or mismatched worker model %q advanced implementation", model)
		}
		unchanged, err := f.store.State(id)
		if err != nil {
			t.Fatal(err)
		}
		if step, _ := unchanged.episodeStep("IMPLEMENT#1"); step.TurnID != "" {
			t.Fatal("refused worker model committed a turn")
		}
	}
	if err := eng.recordEpisodeTurn(id, "minimax-acp:worker", "worker", "send", "send", ""); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if st.Stage.Terminal() || st.ActiveReview != nil {
		t.Fatal("turn claim approved implementation")
	}
	if len(st.Episode.Execution.Conformance) == 0 || len(st.Episode.Execution.Conformance[0].Deviations) == 0 {
		t.Fatal("undeclared change was not measured")
	}
	if st.Episode.Execution.Conformance[0].ChangedFiles[0] != "unplanned.go" {
		t.Fatal(st.Episode.Execution.Conformance)
	}
}
