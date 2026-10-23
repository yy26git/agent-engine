package control

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

type selectionRuntime struct {
	*fakeRuntime
	specs []agentdeck.CreateSpec
}

type idleSelectionRuntime struct{ *selectionRuntime }

func (r *idleSelectionRuntime) Inspect(context.Context, string) (agentdeck.Session, error) {
	return agentdeck.Session{ID: "created-not-started", Status: "idle"}, nil
}

func TestSelection_IdleCreateRecoveryStillStartsSession(t *testing.T) {
	f := profileFixture(t)
	base := &selectionRuntime{fakeRuntime: newFakeRuntime()}
	rt := &idleSelectionRuntime{selectionRuntime: base}
	st, _ := f.store.State("forge-run-1842-repair")
	label := ActivationLabel(st.RequestID, st.TaskID, string(RoleWorker), st.Generation, "idle-recovery")
	rt.sessions[label] = "created-not-started"
	e, err := NewEngine(f.store, rt, st.RequestID, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.LaunchProfile(context.Background(), st.RequestID, RoleWorker, "impl", f.srcRoot, "idle-recovery"); err != nil {
		t.Fatal(err)
	}
	if len(rt.started) != 1 || len(rt.specs) != 0 {
		t.Fatal("idle create was assumed live or recreated")
	}
}

func testReviewResponse(s *State, outcome ReviewOutcome, body string) string {
	r := s.ActiveReview
	data, _ := json.Marshal(ReviewEnvelope{RequestID: s.RequestID, Kind: r.Kind, ReviewID: r.ReviewID, TaskID: s.TaskID, Generation: r.Generation, AnswerNonce: r.AnswerNonce, Outcome: outcome, ReportBody: body})
	return string(data)
}

func TestSelection_ReviewCollectionRejectsOldAndUncorrelatedOutput(t *testing.T) {
	f := profileFixture(t)
	auditedRequest(t, f)
	st, _ := f.store.State("forge-run-1842-repair")
	rt := newFakeRuntime()
	e := profileEngine(t, f, rt)
	for _, answer := range []string{"PASS", `{"outcome":"PASS"}`, strings.Replace(testReviewResponse(st, OutcomeFail, "old answer"), st.ActiveReview.AnswerNonce, "old-nonce", 1)} {
		rt.outputs[st.AuditorSession] = answer
		if _, err := e.CollectReviewerAnswer(context.Background(), st.RequestID, 0); err == nil {
			t.Fatalf("uncorrelated answer accepted: %s", answer)
		}
	}
	rt.outputs[st.AuditorSession] = "```json\n" + testReviewResponse(st, OutcomeFail, "current answer") + "\n```"
	answer, err := e.CollectReviewerAnswer(context.Background(), st.RequestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Trusted || answer.AnswerNonce != st.ActiveReview.AnswerNonce {
		t.Fatal("current correlated answer not trusted")
	}
}

func (r *selectionRuntime) Create(ctx context.Context, s agentdeck.CreateSpec) (agentdeck.Session, error) {
	r.specs = append(r.specs, s)
	session, err := r.fakeRuntime.Create(ctx, s)
	if err == nil {
		r.sessions[s.Title] = session.ID
	}
	return session, err
}

func TestSelection_LaunchPassesActualNativeOptions(t *testing.T) {
	f := profileFixture(t)
	p := f.cfg.Profiles["impl"]
	p.Tool = "codex"
	p.Sandbox = true
	f.cfg.Profiles["impl"] = p
	rt := &selectionRuntime{fakeRuntime: newFakeRuntime()}
	e, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker, "impl", f.srcRoot, "native"); err != nil {
		t.Fatal(err)
	}
	got := rt.specs[0]
	if got.Model != "m" || got.Effort != "high" || !got.Sandbox {
		t.Fatalf("launch dropped selections: %+v", got)
	}
}

func TestSelection_WorktreeWithoutVerificationBindingRefused(t *testing.T) {
	p := Profile{Name: "unbound-worktree", Role: RoleWorker, Tool: "codex", Worktree: true}
	if _, err := p.CreateSpec("/source", "worker"); err == nil || !strings.Contains(err.Error(), "verification-workspace") {
		t.Fatalf("worktree could verify a different workspace: %v", err)
	}
}

func TestSelection_CustomCommandQuotesValuesAndIsolatesEnvironment(t *testing.T) {
	model := "model ' ; $(touch should-not-exist)"
	p := Profile{Name: "minimax", Tool: "mcode", Provider: "minimax", Model: model, Effort: "high", LaunchCommand: []string{"/usr/bin/printf", "%s\\n", "{model}", "{effort}"}, LaunchEnv: map[string]string{"EX_PROFILE_HOME": "/isolated/config"}}
	spec, err := p.CreateSpec("/tmp", "test")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Model != "" || spec.Effort != "" {
		t.Fatal("custom CLI received unsupported native flags")
	}
	cmd := exec.Command("/bin/sh", "-c", spec.Wrapper)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != model+"\nhigh\n" {
		t.Fatalf("argument changed: %q", out)
	}
	if _, err := os.Stat(filepath.Join(cmd.Dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("model was executed as shell syntax")
	}
	if !strings.Contains(spec.Wrapper, "EX_PROFILE_HOME=/isolated/config") {
		t.Fatal("isolated environment missing")
	}
	p.LaunchCommand = []string{"/usr/bin/printf", "fixed-model"}
	if _, err := p.CreateSpec("/tmp", ""); err == nil {
		t.Fatal("ignored model was accepted")
	}
	p.LaunchCommand = []string{"/usr/bin/printf", "{model}"}
	if _, err := p.CreateSpec("/tmp", ""); err == nil {
		t.Fatal("ignored effort was accepted")
	}
}

func TestSelection_AdmissionPinsEveryRoleAndActivationUsesPin(t *testing.T) {
	f := wfFixture(t)
	f.cfg.Profiles["checkpoint"] = Profile{Name: "checkpoint", Role: RoleCheckpoint, Tool: "codex", Model: "checkpoint-model"}
	f.cfg.Profiles["diagnostic"] = Profile{Name: "diagnostic", Role: RoleDiagnostic, Tool: "claude", Model: "diagnostic-model"}
	impl := f.cfg.Profiles["impl"]
	impl.Tool = "codex"
	impl.Model = "original-worker"
	f.cfg.Profiles["impl"] = impl
	def := wfDefinition()
	def.Tasks[1].CheckpointProfile = "checkpoint"
	def.Tasks[1].DiagnosticProfile = "diagnostic"
	adm, err := f.store.AdmitWorkflow(wfRequest("selection-root", f.srcRoot, def, map[string]string{"run_id": "1842"}, true))
	if err != nil {
		t.Fatal(err)
	}
	child := adm.Children["repair"]
	st, err := f.store.State(child)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ExecutionProfiles) != 4 {
		t.Fatal("not every role was pinned")
	}
	impl.Model = "changed-worker"
	f.cfg.Profiles["impl"] = impl
	rt := &selectionRuntime{fakeRuntime: newFakeRuntime()}
	e, err := NewEngine(f.store, rt, child, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.PrepareTaskSessions(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if len(rt.specs) != 4 || rt.specs[3].Model != "original-worker" || rt.specs[1].Model != "checkpoint-model" || rt.specs[2].Model != "diagnostic-model" {
		t.Fatalf("selection lost or drifted: %+v", rt.specs)
	}
	st, _ = f.store.State(child)
	if st.ReviewSession(ReviewCheckpoint) == st.AuditorSession || st.ReviewSession(ReviewDiagnostic) == st.AuditorSession || st.WorkerSession == st.DiagnosticSession {
		t.Fatal("review roles share a session")
	}
	if err := e.PrepareTaskSessions(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if len(rt.specs) != 4 {
		t.Fatal("recovery created duplicate sessions")
	}
	if len(st.Effects) != 1 {
		t.Fatal("initial task instruction missing")
	}
	if err := f.store.Update(child, func(s *State) error { s.CancellationRequested = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReconcileCancellation(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if len(rt.stopped) != 4 {
		t.Fatalf("cancellation omitted roles: %v", rt.stopped)
	}
}

func TestSelection_ReviewRoutingAndMissingExplicitRole(t *testing.T) {
	f := profileFixture(t)
	e := profileEngine(t, f, newFakeRuntime())
	st, _ := f.store.State("forge-run-1842-repair")
	st.DiagnosticSession = "diagnostic-session"
	st.CheckpointSession = "checkpoint-session"
	cause := WakeCause{Reason: ReasonObjectiveFail, Detail: "failed"}
	if err := e.openDiagnosticOrReview(st, cause, f.now); err != nil {
		t.Fatal(err)
	}
	if st.ActiveReview == nil || st.ActiveReview.ExpectedReviewer != "diagnostic-session" || st.Effects[len(st.Effects)-1].SessionID != "diagnostic-session" {
		t.Fatal("diagnostic sent to completion reviewer")
	}
	st.ActiveReview = nil
	st.Effects = nil
	if err := e.openDiagnosticOrReview(st, WakeCause{Detail: "checkpoint"}, f.now); err != nil {
		t.Fatal(err)
	}
	if st.ActiveReview == nil || st.ActiveReview.ExpectedReviewer != "checkpoint-session" {
		t.Fatal("checkpoint sent to completion reviewer")
	}
	st.CheckpointSession = ""
	st.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleCheckpoint: {Name: "checkpoint"}}
	if st.ReviewSession(ReviewCheckpoint) != "" {
		t.Fatal("missing explicit role silently used legacy auditor")
	}
}

func TestSelection_CloneDoesNotShareCommandOrEnvironment(t *testing.T) {
	p := Profile{Name: "worker", Role: RoleWorker, LaunchCommand: []string{"/bin/echo", "{model}"}, LaunchEnv: map[string]string{"HOME": "isolated"}}
	s := &State{ExecutionProfiles: map[ProfileRole]ProfileRef{RoleWorker: p.Ref()}, PinnedProfiles: []ProfileRef{p.Ref()}}
	c := s.Clone()
	ref := c.ExecutionProfiles[RoleWorker]
	ref.LaunchCommand[0] = "changed"
	ref.LaunchEnv["HOME"] = "changed"
	c.PinnedProfiles[0].LaunchCommand[0] = "changed"
	if s.ExecutionProfiles[RoleWorker].LaunchCommand[0] != "/bin/echo" || s.ExecutionProfiles[RoleWorker].LaunchEnv["HOME"] != "isolated" || s.PinnedProfiles[0].LaunchCommand[0] != "/bin/echo" {
		t.Fatal("clone mutated admitted profile")
	}
}

func TestSelection_ConfigLoadsCustomPreset(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(f.root, ConfigFileName)
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(original) + `\n[profiles.minimax-review]
role = "checkpoint"
tool = "mcode"
provider = "minimax"
model = "selected-model"
effort = "high"
launch_command = ["/approved/bin/mcode-launch", "--model", "{model}", "--effort", "{effort}"]
launch_env = ["EX_PROFILE_HOME=/isolated/minimax"]
`
	text = strings.Replace(text, `\n[profiles`, "\n[profiles", 1)
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.ResolveProfile("minimax-review", RoleCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := p.CreateSpec(f.srcRoot, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.Wrapper, "selected-model") || !strings.Contains(spec.Wrapper, "EX_PROFILE_HOME=/isolated/minimax") {
		t.Fatalf("preset was not applied: %+v", spec)
	}
}

func TestSelection_RefreshRetainsWorkerSelection(t *testing.T) {
	f := supFixture(t)
	pinned := Profile{Name: "original", Role: RoleWorker, Tool: "codex", Model: "original-model", Effort: "high"}.Ref()
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		s.ExecutionProfiles = map[ProfileRole]ProfileRef{RoleWorker: pinned}
		_, err := s.BeginReplacement("refresh", f.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rt := &selectionRuntime{fakeRuntime: newFakeRuntime()}
	e, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.cfg.Tool = "changed-default"
	if _, err := e.DriveReplacement(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DriveReplacement(context.Background(), "forge-run-1842-repair"); err != nil {
		t.Fatal(err)
	}
	if len(rt.specs) != 1 || rt.specs[0].Tool != "codex" || rt.specs[0].Model != "original-model" || rt.specs[0].Effort != "high" {
		t.Fatalf("refresh reverted to default: %+v", rt.specs)
	}
}
