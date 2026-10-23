package control

// This file asserts the gap fixes in one place, because each of them corrected a
// specific claim the completion path used to make.
//
// Every case here is written to fail against the pre-fix code. A test that would
// have passed before is not testing the fix.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// --- G5: code_changed is measured, not assumed ------------------------------

// TestGapG5_BaselineIsMeasuredNotAssumed asserts the baseline is measured from
// real content rather than left empty.
//
// Before the fix `State.BaselineDigest` was READ and compared but never ASSIGNED
// anywhere, so the guard `BaselineDigest != ""` always failed and every committed
// result reported `code_changed: false` — wrong for any request that changed code.
func TestGapG5_BaselineIsMeasuredNotAssumed(t *testing.T) {
	f, _ := verifierFixture(t)

	// Before any work, the baseline is genuinely unset.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.BaselineDigest != "" {
		t.Fatalf("a request already carries a baseline before working: %q", st.BaselineDigest)
	}

	measured, err := f.store.RecordBaseline("forge-run-1842-repair", func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	if measured == "" {
		t.Fatal("the baseline measurement returned no digest")
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.BaselineDigest != measured {
		t.Errorf("stored baseline %q does not match the measured %q", st.BaselineDigest, measured)
	}

	// It is idempotent: a second measurement cannot redefine "unchanged".
	second, err := f.store.RecordBaseline("forge-run-1842-repair", func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("second baseline: %v", err)
	}
	if second != measured {
		t.Errorf("a second measurement moved the baseline from %q to %q; the comparison would be redefined mid-run",
			measured, second)
	}

	// A real content change moves the measured identity, so code_changed can be
	// TRUE for a request that edited a file.
	before, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirOf(t, f), "changed.go"),
		[]byte("package demo\n\nfunc Added() {}\n"), 0o644); err != nil {
		t.Fatalf("edit source: %v", err)
	}
	after, err := TakeSnapshot(dirOf(t, f), f.now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if after.ContentDigest == before.BaselineDigest {
		t.Fatal("editing the workspace did not change the measured content identity")
	}
	// Which is exactly the comparison the result now reports.
	if after.ContentDigest == before.BaselineDigest {
		t.Fatal("code_changed would still always be false")
	}
}

// dirOf returns the fixture's workspace directory.
func dirOf(t *testing.T, f *fixture) string {
	t.Helper()
	adm, err := f.store.loadRequestDir("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	return adm.Request.Workspace.Path
}

// --- G6: discovery counts executed test cases -------------------------------

// TestGapG6_DiscoveryRequiresMachineEvents is covered by
// TestVerify_DiscoveryCountsExecutedTestCases in engine_test.go. What this adds
// is the end-to-end claim: a discovery-REQUIRED check whose output is not machine
// events cannot satisfy the requirement.
func TestGapG6_DiscoveryRequiresMachineEvents(t *testing.T) {
	f := completionFixture(t)
	// A check that prints a plausible summary line but runs nothing.
	setCheck(t, f, "regression", []string{"/bin/sh", "-c", "printf 'ok  \\tstub\\t0.01s\\n'; exit 0"}, true, time.Minute)

	receipt, err := f.store.RunCheck(t.Context(), "forge-run-1842-repair", "regression", "J-stub",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if receipt.Outcome != JobFail {
		t.Errorf("outcome = %q, want %q: a run that executed no test case must not satisfy a "+
			"discovery-required check, even though it printed a summary line", receipt.Outcome, JobFail)
	}
}

// --- G8: the runtime identity is recorded on the request --------------------

// TestGapG8_RuntimePinIsRecordedAndEnforced asserts the approved runtime identity
// is stored on the request, not only checked at launch.
func TestGapG8_RuntimePinIsRecordedAndEnforced(t *testing.T) {
	f := completionFixture(t)
	commit := strings.Repeat("a", 40) + strings.Repeat("0", 24)
	digest := strings.Repeat("b", 64)

	if err := f.store.RecordRuntimePin("forge-run-1842-repair", RuntimePin{
		SourceCommit: commit, BinarySHA256: digest, LockFile: "runtime-lock.json",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.RuntimePin.Pinned() {
		t.Fatalf("the request records no runtime pin: %+v", st.RuntimePin)
	}
	if st.RuntimePin.SourceCommit != commit || st.RuntimePin.BinarySHA256 != digest {
		t.Errorf("the recorded pin does not match what was supplied: %+v", st.RuntimePin)
	}

	// The matching identity passes.
	if err := f.store.VerifyRuntimePin("forge-run-1842-repair", commit, digest); err != nil {
		t.Errorf("the recorded pin refused its own identity: %v", err)
	}
	// A DIFFERENT artifact is refused: a run may not change the binary it was
	// admitted under.
	err = f.store.VerifyRuntimePin("forge-run-1842-repair", commit, strings.Repeat("c", 64))
	if err == nil {
		t.Error("a substituted artifact was accepted for a pinned request")
	}
	if !strings.Contains(err.Error(), "admitted against") {
		t.Errorf("the refusal did not explain the provenance change: %v", err)
	}

	// The pin is written once: a later attempt cannot rewrite history.
	if err := f.store.RecordRuntimePin("forge-run-1842-repair", RuntimePin{
		SourceCommit: strings.Repeat("9", 40) + strings.Repeat("0", 24),
		BinarySHA256: strings.Repeat("d", 64),
	}); err != nil {
		t.Fatalf("second record: %v", err)
	}
	st, _ = f.store.State("forge-run-1842-repair")
	if st.RuntimePin.SourceCommit != commit {
		t.Error("a deployment upgrade retroactively changed what the request was admitted under")
	}
}

// --- G7: a schema migration preserves its source ---------------------------

// TestGapG7_MigrationPreservesOriginalBytes asserts a v1 state is exported
// before it is migrated, so the migration itself is auditable.
func TestGapG7_MigrationPreservesOriginalBytes(t *testing.T) {
	f := completionFixture(t)
	statePath := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair", StateFileName)

	// Write a genuine v1 state.
	raw := []byte(`{"schema_version":1,"revision":3,"request_id":"forge-run-1842-repair",` +
		`"request_sha256":"` + strings.Repeat("c", 64) + `","stage":"WORKING"}`)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatalf("write v1 state: %v", err)
	}

	// Loading migrates it.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load migrated state: %v", err)
	}
	if st.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", st.SchemaVersion, CurrentSchemaVersion)
	}

	// And the ORIGINAL bytes were preserved, unchanged, under an immutable name.
	sidecar := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair", "MIGRATION-STATE-v1.json")
	preserved, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("the pre-migration state was not preserved: %v", err)
	}
	if string(preserved) != string(raw) {
		t.Errorf("the preserved bytes differ from what was on disk:\n got: %s\nwant: %s", preserved, raw)
	}
}

// --- G11: a paused replacement is not read as a healthy request -------------

// TestGapG11_PartialReplacementIsRefusedByAnOlderBuild asserts a replacement
// paused mid-protocol is not silently readable as an ordinary working request.
func TestGapG11_PartialReplacementIsRefusedByAnOlderBuild(t *testing.T) {
	f := completionFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		_, err := s.BeginReplacement("stalled mid-protocol", f.now)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// This build knows every phase, so it reads and reconciles it rather than
	// refusing — the guard is about a build that does NOT know the phase.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Replacement.Phase != GenIntent {
		t.Fatalf("the paused phase was lost: %q", st.Replacement.Phase)
	}
	// The guard fires for a phase outside the set this build implements.
	saved := knownReplacementPhases
	knownReplacementPhases = map[GenerationPhase]bool{}
	defer func() { knownReplacementPhases = saved }()
	if err := st.validate(); err == nil {
		t.Fatal("a build that understands no replacement phases accepted a paused replacement")
	} else if !strings.Contains(err.Error(), "does not understand") {
		t.Errorf("the refusal did not explain itself: %v", err)
	}
}

// --- G10: the child capability is unforgeable ------------------------------

// TestGapG10_RawTokenIsNeverPersisted asserts the store keeps only a digest, so
// a readable state file is not a usable capability.
func TestGapG10_RawTokenIsNeverPersisted(t *testing.T) {
	f := completionFixture(t)
	token, err := f.store.IssueChildCapability("forge-run-1842-repair", "", f.now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64 hex characters", len(token))
	}
	statePath := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair", StateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Error("the raw capability token is present in state.json; a readable store would be a usable capability")
	}
	if !strings.Contains(string(raw), "token_sha256") {
		t.Error("no token digest was persisted")
	}
	// The token the mint returned is the one that validates.
	if _, err := f.store.VerifyChildCapability("forge-run-1842-repair", token, f.now); err != nil {
		t.Errorf("the minted token did not validate: %v", err)
	}
	// A token of the right shape but the wrong value does not.
	if _, err := f.store.VerifyChildCapability("forge-run-1842-repair", strings.Repeat("0", 64), f.now); err == nil {
		t.Error("a same-shape wrong token validated")
	}
}

// --- G12: a diagnostic round is collectable through the broker -------------

// TestGapG12_DiagnosticRoundIsCollectable asserts a DIAGNOSTIC review issued by
// supervision can actually be answered and applied through the same ingress a
// completion review uses.
func TestGapG12_DiagnosticRoundIsCollectable(t *testing.T) {
	f := completionFixture(t)
	rt := newFakeRuntime()
	eng := profileEngine(t, f, rt)

	// Spend the budget with objective failures so supervision escalates.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Supervision.ObjectiveFailures = 2
		s.RepairCycles = s.effectiveRepairLimit()
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The cause is evaluated against a snapshot, and the review is opened inside
	// the state transaction — opening it on an in-memory copy would prove nothing.
	var cause WakeCause
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		c, due := eng.tick(s, f.now)
		if !due {
			return errNoCause
		}
		cause = c
		return eng.openDiagnosticOrReview(s, c, f.now)
	}); err != nil {
		t.Fatalf("open diagnostic: %v", err)
	}
	if cause.Reason != ReasonObjectiveFail {
		t.Fatalf("cause = %q, want %q", cause.Reason, ReasonObjectiveFail)
	}
	st, _ := f.store.State("forge-run-1842-repair")
	if st.ActiveReview == nil || st.ActiveReview.Kind != ReviewDiagnostic {
		t.Fatal("no diagnostic review was opened or persisted")
	}
	// The reviewer answers in its own session, and the broker collects it. The
	// correlation fields must echo the open review before canonical stamping.
	rt.outputs["sess-auditor-1"] = testReviewResponse(st, OutcomeGuidance, "the module boundary is wrong")
	collected, err := eng.CollectReviewerAnswer(t.Context(), "forge-run-1842-repair", 0)
	if err != nil {
		t.Fatalf("the broker could not collect the diagnostic answer: %v", err)
	}
	if collected.SourceSession != "sess-auditor-1" {
		t.Errorf("collected source = %q, want the session the broker queried", collected.SourceSession)
	}
	if collected.Outcome != OutcomeGuidance {
		t.Errorf("collected outcome = %q, want GUIDANCE", collected.Outcome)
	}
	ans := collected
	out, err := f.store.SubmitAnswer("forge-run-1842-repair", ans)
	if err != nil {
		t.Fatalf("diagnostic answer refused: %v", err)
	}
	if out.Verified {
		t.Error("a diagnostic verified the request; it must approve nothing")
	}
	after, _ := f.store.State("forge-run-1842-repair")
	if after.DiagnosticRounds != 1 {
		t.Errorf("diagnostic rounds = %d, want 1: the guidance round was not consumed", after.DiagnosticRounds)
	}
	if after.Stage.Terminal() {
		t.Errorf("a diagnostic round made the request terminal as %s", after.Stage)
	}
}

// errNoCause is the sentinel used when a tick produces nothing to escalate.
var errNoCause = errors.New("no supervision cause")

// contractUnused keeps the contract import meaningful if the file is trimmed.
var _ = contract.Version
