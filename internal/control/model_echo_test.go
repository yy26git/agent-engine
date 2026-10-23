package control

import (
	"strings"
	"testing"
)

func TestModelEcho_PinnedSelectionSharesExistingInstruction(t *testing.T) {
	s := &State{BackendBindings: map[ProfileRole]SessionBinding{
		RoleWorker: {Backend: "minimax-acp", SessionID: "chat"},
	}, ExecutionProfiles: map[ProfileRole]ProfileRef{
		RoleWorker: {Model: "m:minimax:MiniMax-M2.7:v:thinking"},
	}}
	e := Effect{SessionID: "chat", Message: "Repair according to the accepted plan."}
	for i := 0; i < 2; i++ {
		if err := s.RecordEffect(e); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Effects) != 1 {
		t.Fatalf("echo created extra work: %+v", s.Effects)
	}
	got := s.Effects[0]
	if got.SelectedModel != s.ExecutionProfiles[RoleWorker].Model || got.RequestedModelEcho != "M2.7" || strings.Count(got.Message, `reply with "M2.7"`) != 1 {
		t.Fatalf("missing pinned selection echo: %+v", got)
	}
	if err := s.RecordEffect(got); err != nil {
		t.Fatal(err)
	}
	if len(s.Effects) != 1 {
		t.Fatal("redispatch appended the echo again")
	}
}

func TestModelEcho_ConflictingSharedSessionRefused(t *testing.T) {
	s := &State{BackendBindings: map[ProfileRole]SessionBinding{
		RoleReviewer: {SessionID: "chat"}, RoleCheckpoint: {SessionID: "chat"},
	}, ExecutionProfiles: map[ProfileRole]ProfileRef{
		RoleReviewer: {Model: "model-a"}, RoleCheckpoint: {Model: "model-b"},
	}}
	if err := s.RecordEffect(Effect{SessionID: "chat", Message: "review"}); err == nil || len(s.Effects) != 0 {
		t.Fatal("ambiguous model selection produced an instruction")
	}
}
