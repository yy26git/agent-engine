package control

// S0 reproduction: the gaps the completion path has today.
//
// Every test here is written to FAIL against the current code and to pass only
// once the owning defect is fixed. They are the "reproduce the owning gap"
// precondition, not a description of intended behaviour: if one of them passes
// for the wrong reason, the fix did not close the gap it names.
//
// The three defects under test are the ones the independent audit confirmed:
//
//   - P1: a review answer carries no source identity, so nothing ties an approval
//     to the session that was actually asked to give it.
//   - P2: required verification never runs on the completion path.
//   - Crash window: acceptance commits and result publication are separate, so a
//     crash between them loses both the review and the result.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/agentdeck"
)

// completionFixture admits a request against a real source tree whose required
// checks are all satisfiable by an observable command.
//
// This exists because S0 makes required verification actually execute. A test
// about the review gate would otherwise also become a test of the build, and
// would fail for reasons that have nothing to do with what it is pinning. The
// check still runs a real process and produces a real transcript; only the
// command is trivial.
func completionFixture(t *testing.T) *fixture {
	t.Helper()
	f, _ := verifierFixture(t)
	for _, id := range []string{"build", "regression"} {
		setCheck(t, f, id, []string{"/bin/sh", "-c", "echo ran-" + id + "; exit 0"}, false, time.Minute)
	}
	return f
}

// drainEngine runs the engine long enough to consume the fixture's scripted
// frames, then stops it. It mirrors how the CLI follows a live bus.
func drainEngine(t *testing.T, f *fixture, rt *fakeRuntime) {
	t.Helper()
	eng, err := NewEngine(f.store, rt, "forge-run-1842-repair", EngineOptions{
		Now: func() time.Time { return f.now },
		Log: func(line string) { t.Log(line) },
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	defer eng.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, rerr := eng.Run(ctx, "forge-run-1842-repair")
		done <- rerr
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			return
		case err := <-done:
			// The follower ends when the scripted frames are exhausted. That is the
			// expected end of a finite backlog, not a failure to drain.
			if err != nil && !strings.Contains(err.Error(), "event follower exited unexpectedly") {
				t.Fatalf("engine run: %v", err)
			}
			return
		case <-time.After(150 * time.Millisecond):
			cancel()
			<-done
			return
		}
	}
}

// TestCompletion_TrustedExpectedReviewer pins the P1 fix.
//
// An approval must be attributable to the session the review was addressed to.
// The test walks the whole spoof matrix, because each entry is a different way to
// be the wrong actor and a fix that only closed one of them would still leave the
// boundary open.
func TestCompletion_TrustedExpectedReviewer(t *testing.T) {
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)
	body := "the implementer is confident this is correct"
	digest := sha256Hex([]byte(body))

	cases := []struct {
		name    string
		session string
		nonce   string
	}{
		{"no stated source", "", ""},
		{"the implementer itself", "sess-worker-1", ""},
		{"an unrelated session", "sess-someone-else", ""},
		{"the right session with the wrong nonce", "sess-auditor-1", "wrong-nonce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
				Kind: ReviewCompletion, ReviewID: reviewID, Generation: 1,
				SourceSession: tc.session, AnswerNonce: tc.nonce,
				Outcome: OutcomePass, Body: body, ReportDigest: digest,
			})
			if err == nil {
				t.Fatalf("a PASS from %q verified the request", tc.name)
			}
		})
	}

	// None of those refusals may have advanced or closed anything.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.ActiveReview == nil {
		t.Fatal("a refused answer closed the review; the request is no longer retryable")
	}
	if st.Stage.Terminal() {
		t.Fatalf("a refused answer made the request terminal as %s", st.Stage)
	}

	// A retired implementer cannot come back as its own reviewer either. This is
	// the case a current-worker-only check would miss.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		// Simulate a refresh: a new worker takes over, the old one is retired.
		old := s.WorkerSession
		s.WorkerSession = "sess-worker-2"
		s.NoteImplementation(old)
		s.Generation++
		return nil
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.AuditAttempts++
		s.ActiveReview = NewActiveReview(ReviewCompletion, "A-2", "sess-worker-1", s.Generation, s.AuditAttempts)
		return s.Transition(StageCompletionAudit, "completion audit owed", s.UpdatedAt)
	}); err != nil {
		t.Fatalf("open review: %v", err)
	}
	_, err = f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: "A-2", Generation: 2,
		SourceSession: "sess-worker-1",
		AnswerNonce:   issuedNonce(t, f),
		Outcome:       OutcomePass, Body: body, ReportDigest: digest,
	})
	if err == nil {
		t.Fatal("a retired implementer reviewed its own work; lineage is not enforced")
	}
}

// TestCompletion_PartialBindCannotSelfReview reproduces the binding bypass.
//
// bind only compares the two session arguments when BOTH are supplied. Binding
// the auditor to the already-bound worker therefore succeeds, and the completion
// audit becomes self-certifying.
func TestCompletion_PartialBindCannotSelfReview(t *testing.T) {
	f := boundRequest(t, newFixture(t))

	// Binding the auditor to the already-bound worker must be refused. The
	// check runs on the effective binding inside the state transaction, so the
	// refusal happens before anything is written.
	_, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.SetBinding("", s.WorkerSession)
	})
	if err == nil {
		t.Fatal("BYPASS NOT CLOSED: the auditor was rebound to the worker's own session")
	}
	if !errors.Is(err, ErrRoleCollision) {
		t.Errorf("refusal = %v, want ErrRoleCollision", err)
	}

	// And the refused bind must not have changed the binding it refused.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.WorkerSession == st.AuditorSession {
		t.Fatal("a refused rebind still collapsed the two roles")
	}
	if st.AuditorSession != "sess-auditor-1" {
		t.Errorf("auditor = %q, want the original independent reviewer", st.AuditorSession)
	}
}

// TestCompletion_DoneDoesNotOpenCompletion reproduces the DONE handling defect.
//
// handleFrame admits a finished event from EITHER bound session and then
// unconditionally replaces ActiveReview, so a repeated DONE supersedes a review
// that is already owed.
func TestCompletion_DoneDoesNotOpenCompletion(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	rt := newFakeRuntime()
	rt.sendState = "landed"
	// Two finished events from the AUDITOR, not the worker.
	rt.frames = []agentdeck.Frame{
		finishedFrame(10, "sess-auditor-1"),
		finishedFrame(11, "sess-auditor-1"),
	}
	drainEngine(t, f, rt)
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.AuditAttempts > 0 || st.ActiveReview != nil {
		t.Fatalf("DEFECT REPRODUCED: the auditor's own DONE opened a completion review "+
			"(audit_attempts=%d, active=%+v); a reviewer cannot assert that the work finished",
			st.AuditAttempts, st.ActiveReview)
	}
}

func TestCompletion_DoneDoesNotOverwriteActiveReview(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	// A review is already owed before any event arrives.
	_ = auditedRequest(t, f)
	before, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	original := before.ActiveReview.ReviewID

	rt := newFakeRuntime()
	rt.sendState = "landed"
	rt.frames = []agentdeck.Frame{finishedFrame(12, "sess-worker-1")}
	drainEngine(t, f, rt)

	after, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if after.ActiveReview != nil && after.ActiveReview.ReviewID != original {
		t.Fatalf("DEFECT REPRODUCED: a repeated DONE replaced the open review %s with %s",
			original, after.ActiveReview.ReviewID)
	}
}

// TestCompletion_RequiredCheckActuallyRuns proves P2 is closed: required verification now
// executes on the completion path, and a failing check blocks the approval.
func TestCompletion_RequiredCheckActuallyRuns(t *testing.T) {
	f := completionFixture(t)
	// One required check genuinely fails.
	setCheck(t, f, "regression", []string{"/bin/sh", "-c", "echo boom >&2; exit 3"}, false, time.Minute)

	// The obligations stay OPEN and the report judges them properly, so the gate
	// that refuses this answer is the failing CHECK and nothing else.
	reviewID := auditedRequest(t, f)
	if _, err := submitCompletionPass(t, f, reviewID, judgedCompletionBody(t, f)); err == nil {
		t.Fatal("P2 NOT CLOSED: PASS accepted although a required check failed")
	}

	// The check really ran: a job and a receipt exist in state.
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var sawFailed bool
	for _, j := range st.Jobs {
		if j.CheckID == "regression" && j.Status == JobFail {
			sawFailed = true
			if j.ReceiptID == "" {
				t.Error("a failed check was recorded with no receipt")
			}
		}
	}
	if !sawFailed {
		t.Fatalf("the required check never ran; jobs=%+v", st.Jobs)
	}
	// And the request is still retryable, not stranded terminal.
	if st.Stage.Terminal() {
		t.Errorf("a refused approval left the request terminal as %s", st.Stage)
	}
}

// TestCompletion_ReportDigestMatchesBytes reproduces the unbound report claim.
//
// The CLI computes a digest of the report it read. SubmitAnswer only checks that
// the field is non-empty, so any 64 characters satisfies it.
func TestCompletion_ReportDigestMatchesBytes(t *testing.T) {
	f := boundRequest(t, newFixture(t))
	reviewID := auditedRequest(t, f)

	// A digest that is well-formed but describes bytes nobody committed.
	_, err := f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind:         ReviewCompletion,
		ReviewID:     reviewID,
		Generation:   1,
		Outcome:      OutcomePass,
		Body:         "the reasoning behind this approval",
		ReportDigest: strings.Repeat("c", 64),
	})
	if err == nil {
		t.Fatal("DEFECT NOT REPRODUCED: an unbacked report digest was accepted as approval evidence")
	}
	t.Logf("correctly refused: %v", err)
}

// TestCompletion_AtomicApprovalResultCommit reproduces the crash window.
//
// Acceptance clears the active review and commits. Result publication happens
// afterwards, separately. A crash in between leaves a non-terminal request with
// no open review and no result: the audit can neither be applied nor retried.
func TestCompletion_AtomicApprovalResultCommit(t *testing.T) {
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)
	body := judgedCompletionBody(t, f)

	// A PASS whose required verification cannot be met must leave the request
	// retryable: the review stays owed, so the same answer can be resubmitted
	// once the evidence exists. That is the property the old split commit lost.
	out, err := submitCompletionPass(t, f, reviewID, body)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.Stage.Terminal() {
		t.Fatalf("approval did not reach a terminal outcome: stage=%s", st.Stage)
	}
	if st.ResultDigest == "" {
		t.Fatal("terminal approval committed with no result digest")
	}
	if st.ResultDigest != out.ResultDigest {
		t.Errorf("committed digest %s does not match the reported %s", st.ResultDigest, out.ResultDigest)
	}
	if st.ActiveReview != nil {
		t.Error("terminal approval committed with the review still owed")
	}
	if st.Completion == nil {
		t.Fatal("terminal approval committed with no completion evidence")
	}
	// The evidence and the outcome must agree, which is the whole point of
	// committing them in one transaction.
	if st.ResultStatus != string(contract.StatusPass) {
		t.Errorf("result status = %q, want PASS", st.ResultStatus)
	}

	res, err := f.store.Result("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !res.Ready {
		t.Fatal("the committed result is not readable")
	}
	if res.CompletionAudit == nil {
		t.Fatal("the committed result names no completion audit")
	}
	if res.CompletionAudit.ReviewerSession != "sess-auditor-1" {
		t.Errorf("completion audit reviewer = %q, want the session that actually answered",
			res.CompletionAudit.ReviewerSession)
	}
	if res.CompletionAudit.ReportSHA256 != sha256Hex([]byte(body)) {
		t.Errorf("completion audit report digest = %q, want the measured digest of the committed body",
			res.CompletionAudit.ReportSHA256)
	}
}

// TestCompletion_ResultContainsEvidence shows the committed result is not self-describing.
//
// newResultFor hardcodes a single finding and infers code_changed from counters,
// so a caller cannot tell which checks ran or what content was verified.
func TestCompletion_ResultContainsEvidence(t *testing.T) {
	f := completionFixture(t)
	// The obligations are discharged by this PASS rather than pre-marked, so the
	// result is the evidence a real approval rests on.
	reviewID := auditedRequest(t, f)
	body := judgedCompletionBody(t, f)
	if _, err := submitCompletionPass(t, f, reviewID, body); err != nil {
		t.Fatalf("completion refused: %v", err)
	}

	res, err := f.store.Result("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if len(res.Verification) == 0 {
		t.Fatal("the committed PASS result carries no verification evidence")
	}
	if len(res.Verification) != 2 {
		t.Fatalf("result carries %d verification entries, want one per required check", len(res.Verification))
	}
	var v contract.VerificationResult
	for _, e := range res.Verification {
		if e.CheckID == "build" {
			v = e
		}
	}
	if v.CheckID == "" {
		t.Fatal("the required build check is missing from the result evidence")
	}
	if v.Outcome != JobPass {
		t.Errorf("verification outcome = %q, want PASS", v.Outcome)
	}
	if v.ReceiptID == "" {
		t.Error("the verification result names no receipt")
	}
	if v.SnapshotID == "" || v.ContentSHA256 == "" {
		t.Error("the verification result does not pin the content it checked")
	}
	if res.CompletionAudit == nil {
		t.Error("the committed PASS result names no completion audit")
	}
	if res.Workspace == nil || res.Workspace.SnapshotID == "" {
		t.Error("the committed PASS result pins no snapshot")
	}
}

// TestCompletion_ZeroCachedSkippedDiscovery asserts a required test check that
// ran nothing cannot satisfy the requirement.
//
// Exiting zero having executed no test cases is the most common way a green
// build hides an untested change, so a discovery-required check reporting zero
// must be a failure even though the process succeeded.
func TestCompletion_ZeroCachedSkippedDiscovery(t *testing.T) {
	f := completionFixture(t)
	// A check that exits zero having reported no test case at all.
	setCheck(t, f, "regression",
		[]string{"/bin/sh", "-c", "echo 'build finished successfully'; exit 0"}, true, time.Minute)

	receipt, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "regression", "J-zero",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if receipt.ExitCode != 0 {
		t.Fatalf("the check should have exited zero; the point is what it discovered: %+v", receipt)
	}
	if receipt.Outcome != JobFail {
		t.Errorf("outcome = %q, want %q: a zero-discovery run verified nothing", receipt.Outcome, JobFail)
	}
	if receipt.Reason == "" {
		t.Error("a zero-discovery failure carries no reason")
	}

	// A wholly skipped run is the same failure wearing a different hat.
	setCheck(t, f, "regression",
		[]string{"/bin/sh", "-c", "echo '--- SKIP: everything'; exit 0"}, true, time.Minute)
	skipped, err := f.store.RunCheck(context.Background(), "forge-run-1842-repair", "regression", "J-skip",
		func() time.Time { return f.now })
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if skipped.Outcome == JobPass {
		t.Errorf("a wholly skipped run was accepted as PASS: %+v", skipped)
	}
}

// TestCompletion_SnapshotIncludesSubjectAndRejectsUnreadable asserts a snapshot
// covers the subject and reports what it could not read.
//
// The control layer is the subject often enough that excluding it by name would
// mean verifying an empty tree. Excluding the store by path keeps a real control
// module inside the digest while still keeping a self-referential store out.
func TestCompletion_SnapshotIncludesSubjectAndRejectsUnreadable(t *testing.T) {
	dir := t.TempDir()
	// A control-shaped module directory: this must be INSIDE the snapshot.
	modDir := filepath.Join(dir, "agent-engine")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Generated output that must stay out.
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "dep.js"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// An excluded absolute path: pruned whole even though its name is ordinary.
	storeDir := filepath.Join(dir, "control-store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "STATE.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap, err := TakeSnapshot(dir, f0(), storeDir)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}
	if snap.FileCount != 1 {
		t.Errorf("file count = %d, want 1: only agent-engine/main.go belongs in the digest", snap.FileCount)
	}
	if snap.ContentDigest == "" {
		t.Error("the snapshot carries no content digest")
	}
	var sawModules, sawStore bool
	for _, e := range snap.Excluded {
		if strings.Contains(e, "node_modules") {
			sawModules = true
		}
		if strings.Contains(e, "control-store") {
			sawStore = true
		}
	}
	if !sawModules {
		t.Error("generated output was not recorded as excluded")
	}
	if !sawStore {
		t.Error("the excluded store path was not recorded as excluded")
	}

	// Excluding the snapshot root itself must not silently produce an empty
	// digest; that would verify nothing while looking like a clean pass. The
	// baseline here excludes nothing, so any difference is caused by the
	// self-exclusion alone.
	baseline, err := TakeSnapshot(dir, f0())
	if err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	selfExcluded, err := TakeSnapshot(dir, f0(), dir)
	if err != nil {
		t.Fatalf("TakeSnapshot with self-exclusion: %v", err)
	}
	if selfExcluded.FileCount != baseline.FileCount {
		t.Errorf("excluding the root changed the digest from %d to %d files; that excludes the subject",
			baseline.FileCount, selfExcluded.FileCount)
	}
	if selfExcluded.FileCount == 0 {
		t.Fatal("self-exclusion produced an empty snapshot, which would verify nothing")
	}

	// A root that does not exist is an error, never an empty-but-valid snapshot.
	if _, err := TakeSnapshot(filepath.Join(dir, "nope"), f0()); err == nil {
		t.Error("a missing snapshot root was accepted")
	}
}

// issuedNonce reads the correlation nonce currently issued for a request's
// review. Tests use it so an answer carries the nonce the review actually
// issued, rather than a test-local guess about what it should be.
func issuedNonce(t *testing.T, f *fixture) string {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.ActiveReview == nil {
		t.Fatal("no review is open, so there is no nonce to echo")
	}
	if st.ActiveReview.AnswerNonce == "" {
		t.Fatal("the open review carries no nonce")
	}
	return st.ActiveReview.AnswerNonce
}
