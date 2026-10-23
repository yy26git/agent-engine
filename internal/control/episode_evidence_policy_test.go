package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

func producerEvidenceOfSize(n int) json.RawMessage {
	prefix := `{"native_turn_generation":"thread:turn","native_tool_records":[{"output":"`
	suffix := `TAIL-EVIDENCE-MARKER"}]}`
	return json.RawMessage(prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix)
}

func evidenceIntakeFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	f, _ := episodeFixture(t)
	id := "forge-run-1842-repair"
	st, err := f.store.UpdateCommitted(id, func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleInvestigat: {Name: "looksee", Tool: "codex"}}
		s.BackendBindings = map[ProfileRole]SessionBinding{RoleInvestigat: {Backend: "agent-deck", SessionID: "reporter", Generation: s.Generation}}
		s.recordEpisodeStep(EpisodeStep{Key: "REPORT#1", Phase: "REPORT", Round: 1, Role: string(RoleInvestigat), Session: "reporter", ArtifactNonce: "nonce", Generation: s.Generation, Status: StepDispatched})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(EpisodeArtifactEnvelope{RequestID: id, StepKey: "REPORT#1", ArtifactNonce: "nonce", Generation: st.Generation, Report: testReport()})
	if err != nil {
		t.Fatal(err)
	}
	return f, id, string(body)
}

func TestEpisodeEvidence_LargeRecordsRetainedExactlyAndPromptBounded(t *testing.T) {
	f, id, body := evidenceIntakeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	evidence := producerEvidenceOfSize(205037)
	if err := eng.intakeEpisodeArtifact(id, "agent-deck", "reporter", "", "", body, evidence); err != nil {
		t.Fatal(err)
	}
	st, _ := f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	if step.ArtifactEvidenceDigest == "" || st.Episode.ReportDigest == "" || st.Stage.Terminal() {
		t.Fatal("valid large evidence was not committed")
	}
	retained, err := eng.ReadEpisodeArtifact(id, step.ArtifactEvidenceDigest)
	if err != nil {
		t.Fatal(err)
	}
	var sidecar episodeProducerEvidence
	if err := json.Unmarshal(retained, &sidecar); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sidecar.NativeEvidence, evidence) {
		t.Fatal("full evidence was changed")
	}
	prompt, err := eng.episodeArtifactContext(id, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > defaultEpisodeEvidencePromptBytes+4096 || !strings.Contains(prompt, "PARTIAL PROMPT EXCERPT") || strings.Contains(prompt, "TAIL-EVIDENCE-MARKER") {
		t.Fatal("large evidence bypassed the prompt budget")
	}
	dir, _ := f.store.RequestDir(id)
	path := filepath.Join(dir, "episode-artifacts", step.ArtifactEvidenceDigest+".json")
	escaped, _ := json.Marshal(path)
	if !strings.Contains(prompt, string(escaped)) || !strings.Contains(prompt, step.ArtifactEvidenceDigest) {
		t.Fatal("reviewer cannot locate/measure complete evidence")
	}
	read, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(read, retained) || sha256Hex(read) != step.ArtifactEvidenceDigest || !bytes.Contains(read, []byte("TAIL-EVIDENCE-MARKER")) {
		t.Fatal("manifest does not resolve to complete evidence")
	}
}

type evidenceOutputRuntime struct {
	*fakeRuntime
	evidence json.RawMessage
	reads    int
}

func (r *evidenceOutputRuntime) Output(ctx context.Context, id string, budget int) (agentdeck.Output, error) {
	r.reads++
	out, err := r.fakeRuntime.Output(ctx, id, budget)
	out.NativeTurn, out.Evidence = "thread:turn", r.evidence
	return out, err
}

func TestEpisodeEvidence_OversizeBlocksOnceAndSurvivesRestart(t *testing.T) {
	f, id, body := evidenceIntakeFixture(t)
	rt := &evidenceOutputRuntime{fakeRuntime: newFakeRuntime(), evidence: producerEvidenceOfSize(maxEpisodeProducerEvidenceBytes + 1)}
	rt.outputs["reporter"] = body
	eng, err := NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	// Use the controller's real production path, not just the retention helper.
	for i := 0; i < 4; i++ {
		if _, err := eng.executeEpisodePhase(context.Background(), id, ""); err != nil {
			t.Fatal(err)
		}
	}
	eng.Close()
	st, _ := f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	if st.Stage != StageBlocked || step.Status != StepRefused || step.ArtifactDigest != "" || st.Episode.ReportDigest != "" || step.EvidenceRefusal == nil {
		t.Fatal("oversize response did not persist a refusal without accepting the artifact")
	}
	r := step.EvidenceRefusal
	if r.NativeTurn != "thread:turn" || r.Actual != int64(len(rt.evidence)) || r.Limit != maxEpisodeProducerEvidenceBytes || r.Digest != sha256Hex(rt.evidence) {
		t.Fatalf("wrong refusal attribution: %+v", r)
	}
	result, err := f.store.Result(id)
	if err != nil || !result.Ready || result.Status != "BLOCKED" || !strings.Contains(result.Reason, "4194304") {
		t.Fatalf("missing actionable terminal result: %+v %v", result, err)
	}
	eng, err = NewEngine(f.store, rt, id, EngineOptions{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if _, err := eng.executeEpisodePhase(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if rt.reads != 1 || len(rt.sentMessages()) != 0 {
		t.Fatalf("permanent refusal was retried: reads=%d sends=%d", rt.reads, len(rt.sentMessages()))
	}
}

func TestEpisodeEvidence_EpisodeBudgetCountsUncommittedBytesAndConcurrentWrites(t *testing.T) {
	f, _ := episodeFixture(t)
	f.cfg.Evidence = EpisodeEvidencePolicy{MaxProducerBytes: 800, MaxEpisodeBytes: 1000, MaxPromptBytes: 128}
	eng := &Engine{store: f.store}
	id := "forge-run-1842-repair"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, ch := range []string{"a", "b"} {
		wg.Add(1)
		go func(ch string) {
			defer wg.Done()
			artifact := strings.Repeat(ch, 600)
			body, _ := json.Marshal(artifact)
			errs <- eng.retainEpisodeArtifact(id, sha256Hex(body), artifact)
		}(ch)
	}
	wg.Wait()
	close(errs)
	success, refused := 0, 0
	for err := range errs {
		var refusal *episodeEvidenceRefusal
		switch {
		case err == nil:
			success++
		case errors.As(err, &refusal) && refusal.Scope == "episode":
			refused++
		default:
			t.Fatalf("unexpected retention error: %v", err)
		}
	}
	if success != 1 || refused != 1 {
		t.Fatalf("budget overspent: success=%d refused=%d", success, refused)
	}
	dir, _ := f.store.RequestDir(id)
	entries, _ := os.ReadDir(filepath.Join(dir, "episode-artifacts"))
	for _, entry := range entries {
		body, _ := os.ReadFile(filepath.Join(dir, "episode-artifacts", entry.Name()))
		var artifact string
		_ = json.Unmarshal(body, &artifact)
		if err := eng.retainEpisodeArtifact(id, sha256Hex(body), artifact); err != nil {
			t.Fatalf("identical replay charged budget again: %v", err)
		}
	}
}

func TestEpisodeEvidence_ConfigDefaultsOverridesAndInvalidPolicy(t *testing.T) {
	f := newFixture(t)
	if f.cfg.Evidence != defaultEpisodeEvidencePolicy() {
		t.Fatal("wrong default evidence policy")
	}
	raw, err := os.ReadFile(filepath.Join(f.root, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		section string
		valid   bool
	}{
		{"[evidence]\nmax_producer_bytes = 2097152\nmax_episode_bytes = 8388608\nmax_prompt_bytes = 65536\n", true},
		{"[evidence]\nmax_producer_bytes = 0\n", false},
		{"[evidence]\nmax_episode_bytes = 1024\n", false},
		{"[evidence]\nmax_prompt_bytes = 8388608\n", false},
	} {
		cfg, err := parseConfig(append(append([]byte(nil), raw...), []byte("\n"+tc.section)...))
		if (err == nil) != tc.valid {
			t.Fatalf("policy %q: %v", tc.section, err)
		}
		if tc.valid && cfg.Evidence != (EpisodeEvidencePolicy{2097152, 8388608, 65536}) {
			t.Fatal("operator overrides lost")
		}
	}
}

func TestEpisodeEvidence_WrapperCountsTowardTurnLimit(t *testing.T) {
	f, id, _ := evidenceIntakeFixture(t)
	f.cfg.Evidence = EpisodeEvidencePolicy{4096, 16384, 128}
	eng := &Engine{store: f.store}
	st, _ := f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	digest := strings.Repeat("a", 64)
	probe := producerEvidenceOfSize(200)
	wrapped, _ := json.Marshal(episodeProducerEvidence{ArtifactDigest: digest, StepKey: step.Key, Backend: "agent-deck", Session: step.Session, Generation: step.Generation, NativeEvidence: probe})
	overhead := len(wrapped) - len(probe)
	evidence := producerEvidenceOfSize(4096 - overhead)
	ref, err := eng.retainEpisodeProducerEvidence(id, digest, "agent-deck", step, []json.RawMessage{evidence})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := eng.ReadEpisodeArtifact(id, ref)
	if err != nil || len(retained) != 4096 {
		t.Fatalf("wrong boundary: size=%d err=%v", len(retained), err)
	}
	var refusal *episodeEvidenceRefusal
	_, err = eng.retainEpisodeProducerEvidence(id, digest, "agent-deck", step, []json.RawMessage{producerEvidenceOfSize(len(evidence) + 1)})
	if !errors.As(err, &refusal) || refusal.Actual != 4097 {
		t.Fatalf("wrapper overhead escaped the cap: %v", err)
	}
}

func TestEpisodeEvidence_StorageFailureIsNotPermanentPolicyRefusal(t *testing.T) {
	f, id, body := evidenceIntakeFixture(t)
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	dir, _ := f.store.RequestDir(id)
	if err := os.WriteFile(filepath.Join(dir, "episode-artifacts"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := eng.intakeEpisodeArtifact(id, "agent-deck", "reporter", "", "", body, producerEvidenceOfSize(200)); err == nil {
		t.Fatal("storage failure ignored")
	}
	st, _ := f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	if st.Stage.Terminal() || step.Status != StepDispatched || step.EvidenceRefusal != nil {
		t.Fatal("storage error turned into an irreversible policy refusal")
	}
}

func TestEpisodeEvidence_EpisodeBudgetRefusalIsDurableAtIntake(t *testing.T) {
	f, id, body := evidenceIntakeFixture(t)
	f.cfg.Evidence = EpisodeEvidencePolicy{4096, 4096, 128}
	eng, stop := engineWithClock(t, f, newFakeRuntime(), &shiftingClock{at: f.now})
	defer stop()
	// Orphaned content counts even though no committed state references it.
	prior := strings.Repeat("x", 3000)
	priorBody, _ := json.Marshal(prior)
	if err := eng.retainEpisodeArtifact(id, sha256Hex(priorBody), prior); err != nil {
		t.Fatal(err)
	}
	if err := eng.intakeEpisodeArtifact(id, "agent-deck", "reporter", "", "", body, producerEvidenceOfSize(1500)); err == nil {
		t.Fatal("episode storage budget bypassed")
	}
	st, _ := f.store.State(id)
	step, _ := st.episodeStep("REPORT#1")
	if st.Stage != StageBlocked || step.EvidenceRefusal == nil || step.EvidenceRefusal.Scope != "episode" || st.Episode.ReportDigest != "" {
		t.Fatal("episode budget did not block durably without accepting evidence")
	}
}
