package control

import (
	"context"
	"encoding/json"
	"github.com/emjin/agent-engine/internal/agentdeck"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEpisodeArtifact_RetainedBytesAreMeasuredAndCorruptionRefused(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	report := testReport()
	digest, err := eng.RecordReportArtifact("forge-run-1842-repair", report)
	if err != nil {
		t.Fatal(err)
	}
	body, err := eng.ReadEpisodeArtifact("forge-run-1842-repair", digest)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(report)
	if string(body) != string(want) {
		t.Fatal("digest did not resolve to exact report bytes")
	}
	dir, _ := f.store.RequestDir("forge-run-1842-repair")
	path := filepath.Join(dir, "episode-artifacts", digest+".json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ReadEpisodeArtifact("forge-run-1842-repair", digest); err == nil {
		t.Fatal("changed bytes accepted")
	}
}

func TestEpisodeArtifact_IntakeUsesTransportIdentityAndRejectsStaleNonce(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	id := "forge-run-1842-repair"
	_, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee", Model: "measured-model"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "minimax-acp:looksee", SessionID: "reporter"}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "nonce", Generation: s.Generation, EffectID: "effect", Status: StepDispatched})
		s.Effects = append(s.Effects, Effect{EffectID: "effect", SessionID: "reporter", SendID: "send"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	env := EpisodeArtifactEnvelope{RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "stale", Generation: st.Generation, Report: testReport()}
	body, _ := json.Marshal(env)
	if err := eng.intakeEpisodeArtifact(id, "minimax-acp:looksee", "reporter", "send", "measured-model", string(body)); err == nil {
		t.Fatal("stale nonce accepted")
	}
	env.ArtifactNonce = "nonce"
	env.Report.ObservedModel = "model-authored-lie"
	body, _ = json.Marshal(env)
	if err := eng.intakeEpisodeArtifact(id, "minimax-acp:looksee", "reporter", "send", "wrong-model", string(body)); err == nil {
		t.Fatal("mismatched model accepted")
	}
	if err := eng.intakeEpisodeArtifact(id, "minimax-acp:looksee", "reporter", "send", "", string(body)); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if st.Episode.ReportModel != "" || st.Episode.ReportProducer != "looksee" {
		t.Fatal("producer identity came from artifact content")
	}
	if err := eng.intakeEpisodeArtifact(id, "minimax-acp:looksee", "reporter", "send", "", string(body)); err != nil {
		t.Fatal("duplicate refused:", err)
	}
}

func TestEpisodeArtifact_ReceiptBeforeDispatchCommitRemainsOwed(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	id := "forge-run-1842-repair"
	_, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee", Model: "actual"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "minimax-acp:looksee", SessionID: "reporter"}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "nonce", Generation: s.Generation, EffectID: "effect", Status: StepDispatched})
		s.Effects = append(s.Effects, Effect{EffectID: "effect", SessionID: "reporter"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	env := EpisodeArtifactEnvelope{RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "nonce", Generation: st.Generation, Report: testReport()}
	body, _ := json.Marshal(env)
	if err := eng.retainEpisodeTurnReceipt(id, "minimax-acp:looksee", "reporter", "send", "actual", string(body), false); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if err := eng.consumeEpisodeReceipts(id, st); err == nil {
		t.Fatal("receipt was admitted before dispatch correlation existed")
	}
	st, _ = f.store.State(id)
	if st.Episode.Execution.TurnReceipts[0].Consumed {
		t.Fatal("unmatched receipt was discarded")
	}
	_, err = f.store.UpdateCommitted(id, func(s *State) error {
		for i := range s.Effects {
			if s.Effects[i].EffectID == "effect" {
				s.Effects[i].SendID = "send"
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if err := eng.consumeEpisodeReceipts(id, st); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if st.Episode.ReportDigest == "" || !st.Episode.Execution.TurnReceipts[0].Consumed {
		t.Fatal("owed artifact was lost")
	}
}

func TestEpisodeArtifact_ProducerEvidenceTravelsWithDigestAndIsChecked(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	id := "forge-run-1842-repair"
	_, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "agent-deck", SessionID: "reporter"}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "nonce", Generation: s.Generation, Status: StepDispatched})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	body, _ := json.Marshal(EpisodeArtifactEnvelope{RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "nonce", Generation: st.Generation, Report: testReport()})
	evidence := json.RawMessage(`{"native_turn_generation":"thread:turn","native_tool_records":[{"type":"response_item","payload":{"type":"function_call_output","output":"Chunk ID: abc123\nexit=1"}}]}`)
	if err := eng.intakeEpisodeArtifact(id, "agent-deck", "reporter", "", "", string(body), evidence); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	if step.ArtifactEvidenceDigest == "" {
		t.Fatal("tool records were not retained")
	}
	packet, err := eng.episodeArtifactContext(id, st)
	if err != nil || !strings.Contains(packet, "abc123") || !strings.Contains(packet, step.ArtifactEvidenceDigest) || !strings.Contains(packet, "not managed verification or permission") {
		t.Fatalf("packet=%s err=%v", packet, err)
	}
	st.Episode.Execution.Steps[0].Session = "another producer"
	if _, err := eng.episodeArtifactContext(id, st); err == nil {
		t.Fatal("sidecar followed an unrelated producer")
	}
	// Corrupting retained bytes must refuse review, even if the state's digest is unchanged.
	st, _ = f.store.State(id)
	dir, _ := f.store.RequestDir(id)
	path := filepath.Join(dir, "episode-artifacts", step.ArtifactEvidenceDigest+".json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.episodeArtifactContext(id, st); err == nil {
		t.Fatal("corrupt producer evidence accepted")
	}
}

func TestEpisodeArtifact_OversizedProducerEvidenceDoesNotCommitArtifact(t *testing.T) {
	f, _ := episodeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	id := "forge-run-1842-repair"
	_, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "agent-deck", SessionID: "reporter"}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "nonce", Generation: s.Generation, Status: StepDispatched})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	body, _ := json.Marshal(EpisodeArtifactEnvelope{RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "nonce", Generation: st.Generation, Report: testReport()})
	evidence, _ := json.Marshal(strings.Repeat("x", maxEpisodeProducerEvidenceBytes))
	if err := eng.intakeEpisodeArtifact(id, "agent-deck", "reporter", "", "", string(body), evidence); err == nil {
		t.Fatal("oversized records silently truncated")
	}
	st, _ = f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	if st.Episode.ReportDigest != "" || step.ArtifactDigest != "" || step.ArtifactEvidenceDigest != "" {
		t.Fatal("failed evidence retention committed an artifact")
	}
}

type episodeOutputRuntime struct {
	*fakeRuntime
	turn string
}

func (r *episodeOutputRuntime) Output(ctx context.Context, id string, budget int) (agentdeck.Output, error) {
	out, err := r.fakeRuntime.Output(ctx, id, budget)
	out.NativeTurn = r.turn
	return out, err
}

func TestEpisodeArtifact_NonceCorrectionIsDurableSameSessionAndBounded(t *testing.T) {
	f, _ := episodeFixture(t)
	rt := &episodeOutputRuntime{fakeRuntime: newFakeRuntime(), turn: "thread:bad-turn"}
	id := "forge-run-1842-repair"
	eng, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	stop := func() { eng.Close() }
	defer stop()
	_, err = f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee", Tool: "codex"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "agent-deck", SessionID: "reporter", Generation: s.Generation}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "exact-nonce", Generation: s.Generation, EffectID: "original", Status: StepDispatched})
		s.Effects = append(s.Effects, Effect{EffectID: "original", SessionID: "reporter", Status: EffectDelivered})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	env := EpisodeArtifactEnvelope{RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "typo", Generation: st.Generation, Report: testReport()}
	body, _ := json.Marshal(env)
	rt.outputs["reporter"] = string(body)
	step, _ := st.episodeStep("REPORT#1")
	for i := 0; i < 3; i++ {
		if err := eng.collectEpisodeOutput(context.Background(), id, st, step); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = f.store.State(id)
	current, _ := st.episodeStep(step.Key)
	if current.ArtifactCorrectionEffectID == "" || current.ArtifactCorrectionTurn != "thread:bad-turn" || st.Episode.ReportDigest != "" || len(rt.sentMessages()) != 1 {
		t.Fatalf("nonce refusal was accepted or resent: %+v sends=%v", current, rt.sentMessages())
	}
	if !strings.Contains(rt.sentMessages()[0], `artifact_nonce="exact-nonce"`) {
		t.Fatal("correction omitted exact nonce")
	}
	// Reopening the engine retains the same owed/delivered correction identity.
	stop()
	eng2, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	if err := eng2.collectEpisodeOutput(context.Background(), id, st, current); err != nil {
		t.Fatal(err)
	}
	if len(rt.sentMessages()) != 1 {
		t.Fatal("restart duplicated the correction")
	}
	// A second completed malformed answer stops. Nothing accepts or patches it.
	rt.turn = "thread:second-bad-turn"
	if err := eng2.collectEpisodeOutput(context.Background(), id, st, current); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if st.Stage != StageBlocked || st.Episode.ReportDigest != "" || len(rt.sentMessages()) != 1 {
		t.Fatalf("correction budget did not stop: stage=%s messages=%v", st.Stage, rt.sentMessages())
	}
	result, err := f.store.Result(id)
	if err != nil || !result.Ready || result.Status != "BLOCKED" || !strings.Contains(result.Reason, "correction budget exhausted") {
		t.Fatalf("terminal refusal has no correlated result: %+v %v", result, err)
	}
}

func TestEpisodeArtifact_CorrectedNonceIsStillValidatedAndAccepted(t *testing.T) {
	f, _ := episodeFixture(t)
	rt := &episodeOutputRuntime{fakeRuntime: newFakeRuntime(), turn: "thread:bad"}
	id := "forge-run-1842-repair"
	eng, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	stop := func() { eng.Close() }
	defer stop()
	_, err = f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee", Tool: "codex"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "agent-deck", SessionID: "reporter", Generation: s.Generation}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "exact", Generation: s.Generation, EffectID: "original", Status: StepDispatched})
		s.Effects = append(s.Effects, Effect{EffectID: "original", SessionID: "reporter", Status: EffectDelivered})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	env := EpisodeArtifactEnvelope{RequestID: id, StepKey: step.Key, ArtifactNonce: "typo", Generation: st.Generation, Report: testReport()}
	set := func() { b, _ := json.Marshal(env); rt.outputs["reporter"] = string(b) }
	set()
	// An old phase answer is not eligible for correction, even with a bad nonce.
	env.StepKey = "REPORT#2"
	set()
	if err := eng.collectEpisodeOutput(context.Background(), id, st, step); err == nil {
		t.Fatal("unrelated phase answer was not refused")
	}
	if len(rt.sentMessages()) != 0 {
		t.Fatal("unrelated phase answer spent a correction")
	}
	env.StepKey = step.Key
	set()
	rt.turn = ""
	if err := eng.collectEpisodeOutput(context.Background(), id, st, step); err != nil {
		t.Fatal(err)
	}
	if len(rt.sentMessages()) != 0 {
		t.Fatal("working pane spent a correction")
	}
	rt.turn = "thread:bad"
	if err := eng.collectEpisodeOutput(context.Background(), id, st, step); err != nil {
		t.Fatal(err)
	}
	env.ArtifactNonce = "exact"
	rt.turn = "thread:corrected"
	set()
	if err := eng.collectEpisodeOutput(context.Background(), id, st, step); err != nil {
		t.Fatal(err)
	}
	st, _ = f.store.State(id)
	if st.Episode.ReportDigest == "" || st.Stage.Terminal() || len(rt.sentMessages()) != 1 {
		t.Fatal("correct model regeneration was not accepted through normal validation")
	}
}
