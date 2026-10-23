package control

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

func TestEpisodeReview_HandsOffOnlyCorrelatedWorkerEvidence(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-EVIDENCE-HANDOFF"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	const backend, session, send = "minimax-acp:impl", "native-worker", "send-one"
	if _, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.Episode.Phase = string(PhaseImplement)
		s.Episode.PhaseHistory = append(s.Episode.PhaseHistory, EpisodePhaseMove{From: string(PhaseVerify), To: string(PhaseImplement), Return: true, Reason: "managed rejection retained"})
		s.Jobs = append(s.Jobs, CheckJob{JobID: "first-check", CheckID: "original-check", Status: JobFail})
		s.WorkerSession = session
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleWorker: {Backend: backend, SessionID: session, Generation: s.Generation, Resolved: true}}
		s.Episode.PlanBaseline = &Snapshot{ContentDigest: "accepted-baseline", Files: map[string]string{"protected_test.go": "original-test-hash"}}
		s.recordEpisodeStep(EpisodeStep{Key: "IMPLEMENT#1", Phase: string(PhaseImplement), Role: string(RoleWorker), Session: session, EffectID: "worker-effect", TurnID: send, Generation: s.Generation})
		s.Effects = append(s.Effects, Effect{EffectID: "worker-effect", SessionID: session, SendID: send, Status: EffectDelivered})
		s.Episode.Execution.TurnReceipts = []EpisodeTurnReceipt{
			{StepKey: "IMPLEMENT#1", Backend: backend, Session: session, SendID: send, Text: "native worker test output", Consumed: true},
			{StepKey: "IMPLEMENT#1", Backend: backend, Session: session, SendID: "other-send", Text: "UNCORRELATED", Consumed: true},
			{StepKey: "IMPLEMENT#1", Backend: backend, Session: session, SendID: send, Text: "TRUNCATED", Consumed: true, Truncated: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []runtime.Event{
		{Backend: backend, SessionID: session, Kind: "session/update", Payload: []byte(`{"sessionId":"native-worker","update":{"sessionUpdate":"tool_call","name":"bash","toolCallId":"actual-tool"}}`)},
		{Backend: "different-backend", SessionID: session, Kind: "session/update", Payload: []byte(`{"sessionId":"native-worker","update":{"sessionUpdate":"tool_call","name":"WRONG-BACKEND"}}`)},
		{Backend: backend, SessionID: session, Kind: "session/update", Payload: []byte(`{"sessionId":"different-session","update":{"sessionUpdate":"tool_call","name":"WRONG-SESSION"}}`)},
	} {
		if _, err := e.journal.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	state := mustState(t, f, id)
	if _, err := e.openEpisodeJudgement(context.Background(), id, PhaseImplement, 1, ReviewCheckpoint, func(*State) string { return "Assess implementation.\n" }); err != nil {
		t.Fatal(err)
	}
	state = mustState(t, f, id)
	message := state.Effects[len(state.Effects)-1].Message
	for _, wanted := range []string{"native worker test output", "original-test-hash", "actual-tool", "managed rejection retained", "first-check", "Missing tool detail remains UNKNOWN", "not independent operating-system attestation", "evidence_ref="} {
		if !strings.Contains(message, wanted) {
			t.Fatalf("review lacks retained evidence %q", wanted)
		}
	}
	for _, forbidden := range []string{"UNCORRELATED", "TRUNCATED", "WRONG-BACKEND", "WRONG-SESSION"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("review included unattributable evidence %q", forbidden)
		}
	}
	if state.Episode.Execution.Verification != nil || state.Stage.Terminal() {
		t.Fatal("handing off worker evidence manufactured verification or completion")
	}
	if len(state.Checkpoint.Evidence) != 1 {
		t.Fatalf("review observations are not registered: %+v", state.Checkpoint.Evidence)
	}
	ref := state.Checkpoint.Evidence[0]
	if err := state.resolveJudgementEvidence(ref.ID); err != nil {
		t.Fatalf("review cannot cite its retained observations: %v", err)
	}
	if state.Obligations[0].Disposition == DispositionAdvanced {
		t.Fatal("registering observations advanced an obligation")
	}
	if err := os.Chmod(ref.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := state.resolveJudgementEvidence(ref.ID); err == nil {
		t.Fatal("changed review observations still resolve")
	}
	e.store.Config().Evidence = EpisodeEvidencePolicy{MaxProducerBytes: 4096, MaxEpisodeBytes: 4096, MaxPromptBytes: 1024}
	state.Episode.Execution.TurnReceipts[0].Text = strings.Repeat("x", 4096)
	if _, err := e.episodeImplementationEvidence(state); err == nil {
		t.Fatal("oversized evidence was silently truncated")
	}
}

func TestEpisodeVerification_RepeatedChecksHaveDistinctCitableReceipts(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	const id = "EP-DISTINCT-CHECK-RECEIPTS"
	admitEpisode(t, f, id)
	f.cfg.Checks[unrunnableCheckID] = Check{ID: unrunnableCheckID, Argv: []string{"/bin/sh", "-c", "exit 1"}, Timeout: time.Second}
	first, err := f.store.runRequiredChecks(id, []string{unrunnableCheckID}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Checks[unrunnableCheckID] = Check{ID: unrunnableCheckID, Argv: []string{"/bin/sh", "-c", "exit 0"}, Timeout: time.Second}
	second, err := f.store.runRequiredChecks(id, []string{unrunnableCheckID}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Outcome != JobFail || second[0].Outcome != JobPass || first[0].ReceiptID == second[0].ReceiptID {
		t.Fatalf("round identities collide or outcomes wrong: %+v %+v", first, second)
	}
	s := mustState(t, f, id)
	if err := s.resolveJudgementEvidence(first[0].ReceiptID); err == nil {
		t.Fatal("earlier failed receipt supports an advance")
	}
	if err := s.resolveJudgementEvidence(second[0].ReceiptID); err != nil {
		t.Fatalf("fresh passing receipt resolved to earlier failure: %v", err)
	}
}
