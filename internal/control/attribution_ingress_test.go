package control

import (
	"context"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

type attributedRuntime struct {
	*fakeRuntime
	model string
}

func (r *attributedRuntime) Output(ctx context.Context, id string, budget int) (agentdeck.Output, error) {
	out, err := r.fakeRuntime.Output(ctx, id, budget)
	out.Model = r.model
	return out, err
}

func TestAttribution_CollectionAllowsMissingModelWithoutInventingAttribution(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	pinReviewerModel(t, f, "pinned-model")
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCompletion, "A1", s.AuditorSession, s.Generation, 1)
		s.ActiveReview.ExpectedModel = "pinned-model"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	rt := &attributedRuntime{fakeRuntime: newFakeRuntime()}
	// A model-authored identity must not fill the missing transport evidence.
	rt.outputs[st.AuditorSession] = strings.TrimSuffix(testReviewResponse(st, OutcomeFail, "findings"), "}") + `,"source_model":"pinned-model"}`
	e, err := NewEngine(f.store, rt, st.RequestID, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	answer, err := e.CollectReviewerAnswer(context.Background(), st.RequestID, 1000)
	if err != nil || answer.SourceModel != "" {
		t.Fatalf("missing model should stay unknown: %+v %v", answer, err)
	}
	rt.model = "transport-model"
	answer, err = e.CollectReviewerAnswer(context.Background(), st.RequestID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if answer.SourceModel != "transport-model" {
		t.Fatalf("model identity came from a pin or the answer text: %q", answer.SourceModel)
	}
	if _, err := f.store.SubmitAnswer(st.RequestID, answer); err == nil || !strings.Contains(err.Error(), "attributed to model") {
		t.Fatalf("wrong measured model reached acceptance: %v", err)
	}
	answer.SourceModel = ""
	if _, err := f.store.SubmitAnswer(st.RequestID, answer); err != nil {
		t.Fatalf("correlated missing-model answer refused: %v", err)
	}
}
