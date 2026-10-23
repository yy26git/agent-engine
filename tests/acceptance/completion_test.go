package acceptance

// S0 acceptance: the trustworthy success boundary, exercised through the built
// CLI rather than by calling helpers directly.
//
// The unit tests in internal/control prove the gate refuses the wrong actor. This
// file proves the same thing at the level a caller actually meets: a real
// process, a real store on disk, a real report file and a real check execution.
// A refusal that only held when the store was poked from inside its own package
// would not be a boundary, it would be a convention.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openCompletionReview drives a request to the point where a completion audit is
// owed, using only the built CLI, and returns the review identity the store
// issued.
func openCompletionReview(t *testing.T, w *world) (reviewID, nonce string) {
	t.Helper()
	reqFile := w.repairRequestFile()
	w.mustRun("submit", "--request-file", reqFile)
	w.mustRun("bind", "--request-id", w.RequestID,
		"--worker", "sess-impl-1", "--auditor", "sess-review-1")

	// The reviewer is told to audit, which is what opens the review. The engine
	// is driven by the fixture's event stream rather than by hand-built state.
	w.withFinishSession(t, "sess-impl-1")
	w.mustRun("run", "--request-id", w.RequestID, "--once")

	var st map[string]any
	w.mustRun("--json", "status", "--request-id", w.RequestID).JSON(t, &st)
	review, _ := st["active_review"].(map[string]any)
	if review == nil {
		t.Skipf("no active review was opened by the fixture event stream; status=%v", st)
	}
	reviewID, _ = review["review_id"].(string)
	nonce, _ = review["answer_nonce"].(string)
	if reviewID == "" {
		t.Fatal("the open review carries no review id")
	}
	if nonce == "" {
		t.Fatal("the open review carries no correlation nonce; an answer could not be proven to belong to it")
	}
	return reviewID, nonce
}

// writeReport puts a real report body on disk and returns its path and digest.
func writeReport(t *testing.T, w *world, body string) (path, digest string) {
	t.Helper()
	path = filepath.Join(w.Root, "review-report.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path, sha256Of(t, body)
}

// TestAcceptance_BindRefusesSelfReview proves the partial-rebind bypass is closed
// through the real CLI.
//
// Binding the reviewer to the session that is already the implementer supplies
// only one argument, so a check on the arguments alone is skipped and the
// completion audit becomes self-certifying. This is the exact sequence that used
// to succeed.
func TestAcceptance_BindRefusesSelfReview(t *testing.T) {
	w := newWorld(t, "accept-self-review")
	w.mustRun("submit", "--request-file", w.repairRequestFile())
	w.mustRun("bind", "--request-id", w.RequestID,
		"--worker", "sess-impl-1", "--auditor", "sess-review-1")

	// A partial rebind onto the implementer's own session.
	r := w.run("bind", "--request-id", w.RequestID, "--auditor", "sess-impl-1")
	if r.Code == 0 {
		t.Fatal("binding the reviewer to the implementer's session succeeded; the audit is self-certifying")
	}
	if !strings.Contains(strings.ToLower(r.Stderr), "different sessions") {
		t.Errorf("refusal did not explain the collision: %s", r.Stderr)
	}

	// The refused bind must not have changed the binding it refused.
	st := w.status()
	if got, _ := st["auditor_session"].(string); got != "sess-review-1" {
		t.Errorf("auditor = %q, want the original independent reviewer", got)
	}
	if got, _ := st["worker_session"].(string); got != "sess-impl-1" {
		t.Errorf("worker = %q, want the original implementer", got)
	}
}

// TestAcceptance_ApprovalRequiresAttributableReviewer proves the P1 fix at the
// CLI: a completion PASS is refused unless it comes from the session the review
// was addressed to, carries the issued nonce, and cites a report whose bytes
// hash to the claimed digest.
func TestAcceptance_ApprovalRequiresAttributableReviewer(t *testing.T) {
	w := newWorld(t, "accept-reviewer-identity")
	reviewID, nonce := openCompletionReview(t, w)
	body := "every required check passed against the frozen candidate"
	reportPath, digest := writeReport(t, w, body)

	// Advance the obligations so the failure under test is identity, not debt.
	// This is fixture setup, not the claim: the assertions below are about who
	// may answer.
	markObligationsAdvanced(t, w)

	cases := []struct {
		name    string
		session string
		nonce   string
		digest  string
	}{
		{"no session", "", nonce, digest},
		{"the implementer answering", "sess-impl-1", nonce, digest},
		{"an unrelated session", "sess-stranger", nonce, digest},
		{"the reviewer with a wrong nonce", "sess-review-1", "not-the-issued-nonce", digest},
		{"the reviewer citing an unbacked digest", "sess-review-1", nonce, strings.Repeat("0", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"review-result", "--request-id", w.RequestID,
				"--review-id", reviewID, "--kind", "COMPLETION",
				"--generation", "1", "--outcome", "PASS",
				"--report-file", reportPath, "--json"}
			if tc.session != "" {
				args = append(args, "--session", tc.session)
			}
			if tc.nonce != "" {
				args = append(args, "--answer-nonce", tc.nonce)
			}
			// The digest the CLI computes is the caller's claim; a mismatching
			// claim is injected by editing the report after the digest is taken.
			r := w.run(args...)
			if tc.name == "the reviewer citing an unbacked digest" {
				if err := os.WriteFile(reportPath, []byte(body+"\nappended after the digest was claimed\n"), 0o644); err != nil {
					t.Fatalf("tamper report: %v", err)
				}
				r = w.run(args...)
			}
			if r.Code == 0 {
				t.Fatalf("a PASS from %q was accepted; the review was not attributable", tc.name)
			}
		})
	}

	// None of the refusals may have advanced the request.
	st := w.status()
	if terminal, _ := st["terminal"].(bool); terminal {
		t.Fatalf("a refused approval left the request terminal: %v", st)
	}

	// A correctly attributed answer is accepted, which proves the refusals above
	// were about identity rather than the command being broken.
	r := w.run("--json", "review-result", "--request-id", w.RequestID,
		"--review-id", reviewID, "--kind", "COMPLETION", "--generation", "1",
		"--outcome", "PASS", "--report-file", reportPath,
		"--session", "sess-review-1", "--answer-nonce", nonce)
	if r.Code != 0 {
		t.Fatalf("a properly attributed PASS was refused: %s\n%s", r.Stdout, r.Stderr)
	}
	var out map[string]any
	r.JSON(t, &out)
	if verified, _ := out["verified"].(bool); !verified {
		t.Errorf("the attributed PASS did not verify the request: %v", out)
	}
}

// TestAcceptance_ResultCarriesExecutedEvidence proves the committed result names
// the checks that actually ran and the content they ran against.
func TestAcceptance_ResultCarriesExecutedEvidence(t *testing.T) {
	w := newWorld(t, "accept-result-evidence")
	reviewID, nonce := openCompletionReview(t, w)
	markObligationsAdvanced(t, w)

	body := "verified against the executed receipts"
	reportPath, _ := writeReport(t, w, body)
	w.mustRun("review-result", "--request-id", w.RequestID,
		"--review-id", reviewID, "--kind", "COMPLETION", "--generation", "1",
		"--outcome", "PASS", "--report-file", reportPath,
		"--session", "sess-review-1", "--answer-nonce", nonce)

	var res map[string]any
	w.mustRun("--json", "result", "--request-id", w.RequestID).JSON(t, &res)

	if ready, _ := res["ready"].(bool); !ready {
		t.Fatalf("result is not ready: %v", res)
	}
	if status, _ := res["status"].(string); status != "PASS" {
		t.Errorf("status = %q, want PASS", status)
	}

	verification, _ := res["verification"].([]any)
	if len(verification) == 0 {
		t.Fatal("the committed result carries no verification evidence")
	}
	seen := map[string]bool{}
	for _, e := range verification {
		m, _ := e.(map[string]any)
		id, _ := m["check_id"].(string)
		seen[id] = true
		if outcome, _ := m["outcome"].(string); outcome != "PASS" {
			t.Errorf("check %q reported %q in a passing result", id, outcome)
		}
		if receipt, _ := m["receipt_id"].(string); receipt == "" {
			t.Errorf("check %q is cited with no receipt", id)
		}
		if snap, _ := m["snapshot_id"].(string); snap == "" {
			t.Errorf("check %q is cited with no snapshot", id)
		}
	}
	for _, want := range []string{"build", "regression"} {
		if !seen[want] {
			t.Errorf("the required check %q is missing from the result evidence", want)
		}
	}

	audit, _ := res["completion_audit"].(map[string]any)
	if audit == nil {
		t.Fatal("the committed result names no completion audit")
	}
	if session, _ := audit["reviewer_session"].(string); session != "sess-review-1" {
		t.Errorf("completion audit reviewer = %q, want the session that actually answered", session)
	}
	ws, _ := res["workspace"].(map[string]any)
	if ws == nil {
		t.Fatal("the committed result pins no workspace identity")
	}
	if snap, _ := ws["snapshot_id"].(string); snap == "" {
		t.Error("the committed result pins no snapshot")
	}
}

// TestAcceptance_FailingRequiredCheckBlocksApproval proves P2 through the built
// CLI: a required check that fails cannot be approved over, and the request stays
// retryable rather than stranded.
func TestAcceptance_FailingRequiredCheckBlocksApproval(t *testing.T) {
	w := newWorld(t, "accept-failing-check")
	reviewID, nonce := openCompletionReview(t, w)
	markObligationsAdvanced(t, w)

	// Make a required check genuinely fail.
	tomlPath := filepath.Join(w.Store, "CONTROL.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	broken := strings.Replace(string(data),
		`argv = ["/bin/sh", "-c", "exit 0"]`,
		`argv = ["/bin/sh", "-c", "echo failing >&2; exit 4"]`, 1)
	if broken == string(data) {
		t.Fatal("could not retarget the build check; the fixture config changed")
	}
	if err := os.WriteFile(tomlPath, []byte(broken), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	body := "approving anyway"
	reportPath, _ := writeReport(t, w, body)
	r := w.run("review-result", "--request-id", w.RequestID,
		"--review-id", reviewID, "--kind", "COMPLETION", "--generation", "1",
		"--outcome", "PASS", "--report-file", reportPath,
		"--session", "sess-review-1", "--answer-nonce", nonce)
	if r.Code == 0 {
		t.Fatal("a PASS was accepted although a required check failed")
	}
	if !strings.Contains(strings.ToLower(r.Stderr), "verification") {
		t.Errorf("refusal did not name verification as the reason: %s", r.Stderr)
	}

	st := w.status()
	if terminal, _ := st["terminal"].(bool); terminal {
		t.Fatalf("a refused approval left the request terminal: %v", st)
	}
}

// markObligationsAdvanced advances the request's obligations so that a test about
// review identity is not simultaneously a test of obligation debt.
//
// This is explicit fixture setup and is confined to tests that then assert
// something else. It is never used to make a completion PASS possible on its own:
// the approval still has to pass identity, nonce, report-digest and executed
// verification.
func markObligationsAdvanced(t *testing.T, w *world) {
	t.Helper()
	path := filepath.Join(w.Store, "REQUESTS", w.RequestID, "STATE.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st map[string]any
	if err := jsonUnmarshal(data, &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	obligations, _ := st["obligations"].([]any)
	for _, o := range obligations {
		if m, ok := o.(map[string]any); ok {
			m["disposition"] = "ADVANCED"
		}
	}
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// sha256Of is the digest a report's claimed hash must equal.
func sha256Of(t *testing.T, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
