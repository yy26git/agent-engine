package control

// Per-obligation discharge on the completion path.
//
// A completion PASS is the only thing in the system that can retire an
// obligation, so the question these tests answer is narrow and total: what does
// the store require before it lets one?
//
// The answer it used to give was nothing coherent. ApplyReview blanked every
// obligation to ADVANCED on PASS, but commitCompletion refused OPEN and UNKNOWN
// obligations BEFORE it called ApplyReview — so no episode could ever finish, and
// the only dispositions that satisfied the guard were ones no judgement produced.
// A FAILED obligation satisfied it outright.
//
// The tests below pin the replacement rule: a PASS discharges only what an
// explicit, evidence-backed judgement names, and discharge, review settlement and
// result publication are one committed decision. They reach the production path
// through Store.SubmitAnswer, and the ingress test reaches it through the engine's
// own answer collection. Nothing pre-advanced is admitted except where the test
// is specifically about a non-OPEN disposition.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// --- fixtures ----------------------------------------------------------------

// secondObligation adds a second obligation with its own required check, so the
// coverage rules are exercised against more than one entry.
func secondObligation(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Obligations = append(s.Obligations, Obligation{
			ID:             "O-2",
			Criterion:      "The fix is covered by a regression test",
			RequiredChecks: []string{"regression"},
			Disposition:    DispositionOpen,
		})
		return nil
	}); err != nil {
		t.Fatalf("add obligation: %v", err)
	}
}

// completionEvidence registers one REAL evidence item: a file that exists, whose
// bytes are measured, pinned under an ID a judgement can cite.
//
// A fixture that registers a path with a made-up digest would prove nothing about
// the rule under test, because the store re-measures the bytes a ref pins — a
// reference that resolves is only half of what "resolves" means.
func completionEvidence(t *testing.T, f *fixture, id string) string {
	t.Helper()
	path := filepath.Join(f.evRoot, id+".txt")
	body := "evidence recorded for " + id + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write evidence %s: %v", id, err)
	}
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		return s.AddEvidence(EvidenceRef{
			ID: id, Path: path, SHA256: sha256Hex([]byte(body)), Kind: "artifact",
		})
	}); err != nil {
		t.Fatalf("register evidence %s: %v", id, err)
	}
	return id
}

// judgedCompletionBody registers real evidence for every current obligation and
// returns the report body that judges each of them on that evidence.
func judgedCompletionBody(t *testing.T, f *fixture) string {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	rep := CompletionReport{Reasoning: "each obligation was checked against the executed receipts and the pinned diff"}
	for _, o := range st.Obligations {
		ref := completionEvidence(t, f, "E-"+o.ID)
		rep.Judgements = append(rep.Judgements, ObligationJudgement{
			ObligationID: o.ID,
			Outcome:      DispositionAdvanced,
			EvidenceRefs: []string{ref},
		})
	}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("encode completion report: %v", err)
	}
	return string(body)
}

// submitCompletionPass submits a correlated completion PASS carrying body.
func submitCompletionPass(t *testing.T, f *fixture, reviewID, body string) (AnswerOutcome, error) {
	t.Helper()
	return f.store.SubmitAnswer("forge-run-1842-repair", ReviewAnswer{
		Kind: ReviewCompletion, ReviewID: reviewID, Generation: 1,
		SourceSession: "sess-auditor-1",
		AnswerNonce:   issuedNonce(t, f),
		Outcome:       OutcomePass,
		Body:          body,
		ReportDigest:  sha256Hex([]byte(body)),
	})
}

// dispositions snapshots each obligation's disposition by ID, so a test can
// assert that a refusal advanced nothing rather than merely that it errored.
func dispositions(t *testing.T, f *fixture) map[string]string {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	out := map[string]string{}
	for _, o := range st.Obligations {
		out[o.ID] = o.Disposition
	}
	return out
}

// assertUnapprovedAndRetryable asserts the shape a refusal must leave behind:
// the review is still owed, no terminal result exists, and nothing advanced.
func assertUnapprovedAndRetryable(t *testing.T, f *fixture, reviewID string) {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.ActiveReview == nil || st.ActiveReview.ReviewID != reviewID {
		t.Errorf("the refused answer closed review %s; the request is no longer retryable", reviewID)
	}
	if st.Stage.Terminal() {
		t.Errorf("a refused answer made the request terminal as %s", st.Stage)
	}
	if st.ResultDigest != "" {
		t.Errorf("a refused answer published result digest %s", st.ResultDigest)
	}
	if st.ResultStatus != "" {
		t.Errorf("a refused answer recorded result status %q", st.ResultStatus)
	}
	if st.Completion != nil {
		t.Error("a refused answer recorded completion evidence")
	}
	if st.PendingResult != "" {
		t.Errorf("a refused answer left a pending result pointer %q", st.PendingResult)
	}
}

// --- 1. reachable completion --------------------------------------------------

// TestCompletionDischarge_OpenObligationsReachPassOnJudgedEvidence is the case
// that used to be unreachable: obligations OPEN, a review owed, and a PASS that
// carries a judgement for every one of them with evidence that resolves.
//
// The obligations are NOT pre-advanced. Nothing in this path bypasses
// admission, ingress or the store: the answer is correlated, the report digest is
// measured against committed bytes, the required checks execute, and then the
// obligations, the review settlement and the result publication commit together.
func TestCompletionDischarge_OpenObligationsReachPassOnJudgedEvidence(t *testing.T) {
	f := completionFixture(t)
	secondObligation(t, f)
	reviewID := auditedRequest(t, f)

	before := mustState(t, f, "forge-run-1842-repair")
	for _, o := range before.Obligations {
		if o.Disposition != DispositionOpen {
			t.Fatalf("obligation %s is %s before the answer; the fixture must start OPEN", o.ID, o.Disposition)
		}
	}

	body := judgedCompletionBody(t, f)
	out, err := submitCompletionPass(t, f, reviewID, body)
	if err != nil {
		t.Fatalf("a fully judged completion PASS was refused: %v", err)
	}
	if !out.Verified || out.ResultStatus != string(contract.StatusPass) || out.ResultDigest == "" {
		t.Fatalf("outcome = %+v, want a verified PASS with a committed digest", out)
	}

	st := mustState(t, f, "forge-run-1842-repair")
	for _, o := range st.Obligations {
		if o.Disposition != DispositionAdvanced {
			t.Errorf("obligation %s is %s after a judged PASS", o.ID, o.Disposition)
		}
		if len(o.EvidenceRefs) == 0 {
			t.Errorf("obligation %s is ADVANCED with no evidence behind it", o.ID)
		}
		for _, ref := range o.EvidenceRefs {
			if ref != "E-"+o.ID {
				t.Errorf("obligation %s carries evidence %q, which is not the evidence its judgement named", o.ID, ref)
			}
		}
	}
	// The review is settled, and settled by the same commit that published the
	// result: a review still owed beside a terminal result would mean the
	// approval and its obligations had been decided separately.
	if st.ActiveReview != nil {
		t.Errorf("a completed PASS left review %s open", st.ActiveReview.ReviewID)
	}
	if st.Checkpoint.ReviewPending {
		t.Error("a completed PASS left the review marked pending")
	}
	if st.Stage != StageVerified || st.ResultStatus != string(contract.StatusPass) {
		t.Errorf("stage/status = %s/%s, want %s/PASS", st.Stage, st.ResultStatus, StageVerified)
	}
	if st.Completion == nil || st.Completion.ReportDigest != sha256Hex([]byte(body)) {
		t.Errorf("completion evidence does not name the measured report digest: %+v", st.Completion)
	}
	if st.PendingResult != "" {
		t.Errorf("a committed PASS left PendingResult %q", st.PendingResult)
	}

	res, err := f.store.Result("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !res.Ready || res.Status != contract.StatusPass {
		t.Errorf("result = %+v, want a ready PASS", res)
	}
	if res.CompletionAudit == nil || res.CompletionAudit.ReviewerSession != "sess-auditor-1" {
		t.Errorf("the committed result does not attribute the judgement: %+v", res.CompletionAudit)
	}
}

// TestCompletionDischarge_PassCarriedThroughIngressReachesPass proves the typed
// payload survives the real ingress, not just a direct store call.
//
// The reviewer writes a correlated envelope into its own session; the engine
// queries that session, stamps the identity it read, and the store decides. If
// the envelope could only carry prose to the store, the rule would be
// unreachable by any actual reviewer.
func TestCompletionDischarge_PassCarriedThroughIngressReachesPass(t *testing.T) {
	f := completionFixture(t)
	auditedRequest(t, f)

	st := mustState(t, f, "forge-run-1842-repair")
	body := judgedCompletionBody(t, f)
	rt := newFakeRuntime()
	rt.outputs[st.AuditorSession] = testReviewResponse(st, OutcomePass, body)
	eng := profileEngine(t, f, rt)

	ans, err := eng.CollectReviewerAnswer(context.Background(), "forge-run-1842-repair", 0)
	if err != nil {
		t.Fatalf("collect answer: %v", err)
	}
	if ans.SourceSession != "sess-auditor-1" {
		t.Fatalf("collected source = %q, want the session the engine queried", ans.SourceSession)
	}
	if ans.Body != body {
		t.Errorf("the collected body is not the reviewer's report; the payload would have been rewritten")
	}
	// The digest travels in the envelope, so the reviewer states it; the store
	// measures it back off the committed bytes either way.
	ans.ReportDigest = sha256Hex([]byte(body))
	out, err := f.store.SubmitAnswer("forge-run-1842-repair", ans)
	if err != nil {
		t.Fatalf("a judged PASS collected from the reviewer's own session was refused: %v", err)
	}
	if !out.Verified {
		t.Error("the ingress path did not verify the request")
	}
	after := mustState(t, f, "forge-run-1842-repair")
	for _, o := range after.Obligations {
		if o.Disposition != DispositionAdvanced {
			t.Errorf("obligation %s is %s after an ingressed judged PASS", o.ID, o.Disposition)
		}
	}
}

// --- 2. the refusal matrix ---------------------------------------------------

// TestCompletionDischarge_RefusesIncompleteOrUnsupportedJudgement walks every way
// a completion report can fail to say what it means.
//
// Each case is a separate fixture, because the property under test is not merely
// that the answer is refused but that NOTHING moved: the obligations keep their
// dispositions, the review stays owed, and no result exists. A refusal that
// half-discharged the work would be worse than the defect it replaced.
func TestCompletionDischarge_RefusesIncompleteOrUnsupportedJudgement(t *testing.T) {
	// The fixture the payload is built from, so each case differs in exactly one
	// way from a report that would be accepted.
	build := func(t *testing.T, f *fixture) CompletionReport {
		t.Helper()
		st, err := f.store.State("forge-run-1842-repair")
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		rep := CompletionReport{Reasoning: "judged against the executed receipts"}
		for _, o := range st.Obligations {
			rep.Judgements = append(rep.Judgements, ObligationJudgement{
				ObligationID: o.ID,
				Outcome:      DispositionAdvanced,
				EvidenceRefs: []string{completionEvidence(t, f, "E-"+o.ID)},
			})
		}
		return rep
	}

	cases := []struct {
		name string
		// tamper mutates a valid report into the case under test.
		tamper func(t *testing.T, f *fixture, rep CompletionReport) string
		// seed changes state before the answer, when the case is about state.
		seed func(t *testing.T, f *fixture)
		// want is a substring the refusal must contain, so the test cannot pass
		// on an unrelated refusal.
		want string
	}{
		{
			name: "prose instead of judgements",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				return "every obligation is discharged and the executed checks pass"
			},
			want: "judgement payload",
		},
		{
			name: "no judgement for an obligation",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements = nil
				return mustJSON(t, rep)
			},
			want: "no judgement for obligation",
		},
		{
			name: "the same obligation judged twice",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements = append(rep.Judgements, rep.Judgements[0])
				return mustJSON(t, rep)
			},
			want: "judged twice",
		},
		{
			name: "an obligation this request does not owe",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements = append(rep.Judgements, ObligationJudgement{
					ObligationID: "O-99", Outcome: DispositionAdvanced,
					EvidenceRefs: []string{completionEvidence(t, f, "E-O-99")},
				})
				return mustJSON(t, rep)
			},
			want: "does not owe",
		},
		{
			name: "a judgement with no outcome",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements[0].Outcome = ""
				return mustJSON(t, rep)
			},
			want: "judged",
		},
		{
			name: "a negative judgement inside a PASS",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements[0].Outcome = DispositionFailed
				return mustJSON(t, rep)
			},
			want: "positive judgement",
		},
		{
			name: "a judgement citing nothing",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements[0].EvidenceRefs = nil
				return mustJSON(t, rep)
			},
			want: "no evidence reference",
		},
		{
			name: "a judgement citing a reference that names nothing",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements[0].EvidenceRefs = []string{"E-does-not-exist"}
				return mustJSON(t, rep)
			},
			want: "neither a registered evidence ref nor a managed verification receipt",
		},
		{
			name: "evidence whose bytes moved after it was pinned",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				// The ref is registered and its digest is real. The file is then
				// rewritten, which is the only way real evidence ever becomes
				// evidence of something else.
				path := filepath.Join(f.evRoot, rep.Judgements[0].EvidenceRefs[0]+".txt")
				if err := os.WriteFile(path, []byte("rewritten after the digest was pinned\n"), 0o644); err != nil {
					t.Fatalf("tamper evidence: %v", err)
				}
				return mustJSON(t, rep)
			},
			want: "changed after it was recorded",
		},
		{
			name: "a receipt for a check that did not pass",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				setCheck(t, f, "regressed", []string{"/bin/sh", "-c", "exit 1"}, false, time.Minute)
				receipt, err := f.store.RunCheck(t.Context(), "forge-run-1842-repair", "regressed", "J-regressed",
					func() time.Time { return f.now })
				if err != nil {
					t.Fatalf("run the regressed check: %v", err)
				}
				if receipt.Outcome == JobPass {
					t.Fatal("the fixture check passed, so it cannot back a refusal about failing receipts")
				}
				rep.Judgements[0].EvidenceRefs = []string{receipt.ReceiptID}
				return mustJSON(t, rep)
			},
			want: "only a passing receipt",
		},
		{
			name: "a FAILED obligation judged FAILED",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string {
				rep.Judgements[0].Outcome = DispositionFailed
				return mustJSON(t, rep)
			},
			// The reviewer says the work did not happen. A PASS carrying that
			// judgement is asserting completion and a failure at once, and the
			// store refuses rather than resolving the contradiction.
			want: "positive judgement",
		},
		{
			name:   "a FAILED obligation nobody judged",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string { return mustJSON(t, dropFirst(rep)) },
			seed: func(t *testing.T, f *fixture) {
				if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
					s.Obligations[0].Disposition = DispositionFailed
					return nil
				}); err != nil {
					t.Fatalf("fail obligation: %v", err)
				}
			},
			// THIS is the defect the old guard had. It rejected only OPEN and
			// UNKNOWN, so a FAILED obligation satisfied completion on its own,
			// with no judgement and no evidence behind it.
			want: "no judgement for obligation",
		},
		{
			name:   "an UNKNOWN obligation nobody judged",
			tamper: func(t *testing.T, f *fixture, rep CompletionReport) string { return mustJSON(t, dropFirst(rep)) },
			seed: func(t *testing.T, f *fixture) {
				if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
					s.Obligations[0].Disposition = DispositionUnknown
					return nil
				}); err != nil {
					t.Fatalf("unresolve obligation: %v", err)
				}
			},
			want: "no judgement for obligation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := completionFixture(t)
			if tc.seed != nil {
				tc.seed(t, f)
			}
			reviewID := auditedRequest(t, f)
			before := dispositions(t, f)

			body := tc.tamper(t, f, build(t, f))
			if _, err := submitCompletionPass(t, f, reviewID, body); err == nil {
				t.Fatal("the report was accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v, want it to name %q", err, tc.want)
			}

			after := dispositions(t, f)
			for id, d := range before {
				if after[id] != d {
					t.Errorf("obligation %s moved from %s to %s on a refusal", id, d, after[id])
				}
			}
			assertUnapprovedAndRetryable(t, f, reviewID)
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(data)
}

// dropFirst removes the first judgement, so a case can be about coverage rather
// than about the content of the judgements it no longer carries.
func dropFirst(rep CompletionReport) CompletionReport {
	return CompletionReport{Reasoning: rep.Reasoning, Judgements: rep.Judgements[1:]}
}

// TestCompletionDischarge_PositiveJudgementSupersedesAFailedDisposition pins what
// the FAILED defect's fix actually means, because "FAILED no longer satisfies
// completion" is only half a rule.
//
// The half that would be easy to get wrong is the other direction: a FAILED
// disposition is prior state, not a permanent verdict. A reviewer who judges the
// obligation ADVANCED, with evidence that resolves, has discharged it — and the
// committed state then says ADVANCED with that evidence, which is exactly what
// the post-discharge assertion requires. What is forbidden is FAILED satisfying a
// PASS with no judgement behind it, which the refusal matrix pins.
func TestCompletionDischarge_PositiveJudgementSupersedesAFailedDisposition(t *testing.T) {
	f := completionFixture(t)
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.Obligations[0].Disposition = DispositionFailed
		return nil
	}); err != nil {
		t.Fatalf("fail obligation: %v", err)
	}
	reviewID := auditedRequest(t, f)

	out, err := submitCompletionPass(t, f, reviewID, judgedCompletionBody(t, f))
	if err != nil {
		t.Fatalf("a positive judgement over a FAILED disposition was refused: %v", err)
	}
	if !out.Verified {
		t.Fatal("the request was not verified")
	}
	st := mustState(t, f, "forge-run-1842-repair")
	for _, o := range st.Obligations {
		if o.Disposition != DispositionAdvanced {
			t.Errorf("obligation %s is %s behind a committed PASS; no FAILED disposition may survive one", o.ID, o.Disposition)
		}
		if len(o.EvidenceRefs) == 0 {
			t.Errorf("obligation %s is ADVANCED with no evidence", o.ID)
		}
	}
}

// --- 3. the authority boundary ----------------------------------------------

// TestCompletionDischarge_OnlyTheCompletionTransactionAdvances asserts where the
// discharge authority lives.
//
// ApplyReview is bookkeeping. It used to blank every obligation on PASS, which
// meant the function that records "a review happened" also decided "the work is
// done" — and it did so for whichever review kind reached it. Callers now have to
// go through the completion transaction to retire an obligation, and the only
// way that path can be reached with a real judgement is by supplying one.
func TestCompletionDischarge_OnlyTheCompletionTransactionAdvances(t *testing.T) {
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)

	// A direct PASS settles the review and advances nothing.
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview(reviewID, OutcomePass, "", f.now)
	}); err != nil {
		t.Fatalf("ApplyReview(PASS): %v", err)
	}
	st := mustState(t, f, "forge-run-1842-repair")
	for _, o := range st.Obligations {
		if o.Disposition == DispositionAdvanced {
			t.Errorf("ApplyReview(PASS) advanced obligation %s; it has no discharge authority", o.ID)
		}
	}
	if st.ActiveReview != nil {
		t.Error("ApplyReview(PASS) did not settle the review it was given")
	}

	// And a checkpoint review cannot reach the case at all.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.ActiveReview = NewActiveReview(ReviewCheckpoint, "C-9", s.AuditorSession, s.Generation, 1)
		return nil
	}); err != nil {
		t.Fatalf("open checkpoint review: %v", err)
	}
	if err := f.store.Update("forge-run-1842-repair", func(s *State) error {
		return s.ApplyReview("C-9", OutcomePass, "", f.now)
	}); err == nil {
		t.Fatal("a CHECKPOINT review answered PASS")
	}
	after := mustState(t, f, "forge-run-1842-repair")
	for _, o := range after.Obligations {
		if o.Disposition == DispositionAdvanced {
			t.Errorf("a refused checkpoint PASS advanced obligation %s", o.ID)
		}
	}
}

// --- 4. the preparation race --------------------------------------------------

// TestCompletionDischarge_RefusesWhenTheScopeMovesDuringPreparation asserts that
// preparation is not a licence.
//
// The checks take real time, and state keeps moving while they run. An obligation
// added in that window was never judged by anybody, and a required check added in
// that window never ran — so the approval must be refused even though every
// judgement in the report is valid and every reference resolves.
//
// The barrier is deterministic: the store's clock is the seam the production path
// already exposes, and the mutation fires at the first clock read AFTER every
// required check has recorded a receipt — which is the boundary between preparing
// an approval and committing it. No sleeps, no timing assumptions.
func TestCompletionDischarge_RefusesWhenTheScopeMovesDuringPreparation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*State) error
		want   string
	}{
		{
			name: "an obligation added while the checks ran",
			mutate: func(s *State) error {
				s.Obligations = append(s.Obligations, Obligation{
					ID: "O-LATE", Criterion: "raised after the reviewer answered",
					Disposition: DispositionOpen,
				})
				return nil
			},
			want: "changed while this approval was being prepared",
		},
		{
			name: "a required check added while the checks ran",
			mutate: func(s *State) error {
				s.Checks = append(s.Checks, "late-check")
				return nil
			},
			want: "changed while this approval was being prepared",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := completionFixture(t)
			reviewID := auditedRequest(t, f)
			body := judgedCompletionBody(t, f)

			fired := false
			f.store.Now = func() time.Time {
				if !fired && everyRequiredCheckRecorded(f, "forge-run-1842-repair") {
					fired = true
					if _, err := f.store.UpdateCommitted("forge-run-1842-repair", tc.mutate); err != nil {
						t.Errorf("prepare-race mutation: %v", err)
					}
				}
				return f.now
			}

			_, err := submitCompletionPass(t, f, reviewID, body)
			if !fired {
				t.Fatal("the preparation barrier never fired; the race under test did not happen")
			}
			if err == nil {
				t.Fatal("an approval was published against a scope that had moved")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v, want it to name %q", err, tc.want)
			}

			st := mustState(t, f, "forge-run-1842-repair")
			for _, o := range st.Obligations {
				if o.Disposition == DispositionAdvanced {
					t.Errorf("obligation %s was advanced by a refused approval", o.ID)
				}
			}
			assertUnapprovedAndRetryable(t, f, reviewID)
		})
	}
}

// everyRequiredCheckRecorded reports whether every required check of the request
// has a job carrying a receipt. It is the preparation barrier's condition: a
// durable fact, never a duration.
func everyRequiredCheckRecorded(f *fixture, requestID string) bool {
	st, err := f.store.State(requestID)
	if err != nil {
		return false
	}
	required := append([]string(nil), st.Checks...)
	for _, o := range st.Obligations {
		required = append(required, o.RequiredChecks...)
	}
	required = dedupeStrings(required)
	for _, id := range required {
		var found bool
		for _, j := range st.Jobs {
			if j.CheckID == id && j.ReceiptID != "" {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// --- 5. atomic recovery ------------------------------------------------------

// TestCompletionDischarge_RefusedApprovalLeavesNothingAuthoritative asserts the
// two ends of the crash boundary, because the property is not "it refused" but
// "what it refused with".
//
// Before the authoritative commit, a refusal must leave a prewritten result body
// that nothing references — an orphan an operator can see — and the request must
// still be retryable, which it proves by accepting the same review again with a
// complete report. After the commit, a projection failure must leave the decision
// intact and re-derivable. There is no state in which a review is settled and
// its result is missing, or an obligation is discharged without the result.
func TestCompletionDischarge_RefusedApprovalLeavesNothingAuthoritative(t *testing.T) {
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)

	// A report that judges nothing is refused after the body has been written.
	empty := mustJSON(t, CompletionReport{Reasoning: "no judgement supplied"})
	if _, err := submitCompletionPass(t, f, reviewID, empty); err == nil {
		t.Fatal("a report judging no obligation was accepted")
	}
	assertUnapprovedAndRetryable(t, f, reviewID)

	orphans, err := f.store.OrphanResults()
	if err != nil {
		t.Fatalf("orphan results: %v", err)
	}
	if len(orphans) == 0 {
		t.Error("the prewritten body is not reported as an orphan; a decision in flight must be visible")
	}

	// The same review, answered properly, still completes. Refusing left the
	// request retryable rather than half-finished.
	body := judgedCompletionBody(t, f)
	out, err := submitCompletionPass(t, f, reviewID, body)
	if err != nil {
		t.Fatalf("retry after a refusal: %v", err)
	}
	if !out.Verified {
		t.Fatal("the retry did not verify the request")
	}
	st := mustState(t, f, "forge-run-1842-repair")
	if st.ActiveReview != nil || !st.Stage.Terminal() {
		t.Errorf("after a successful retry: review=%+v stage=%s", st.ActiveReview, st.Stage)
	}
}

// TestCompletionDischarge_ProjectionFailureKeepsTheDecision asserts the other end
// of the boundary: a projection that cannot be written leaves a COMPLETE and
// authoritative request, not a lost result.
//
// The projection is derived from the committed body, so obstructing it changes
// nothing about the decision — and removing the obstruction lets Result re-derive
// the file. What must never happen is the inverse: a result nobody can read
// described as settled.
func TestCompletionDischarge_ProjectionFailureKeepsTheDecision(t *testing.T) {
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)
	body := judgedCompletionBody(t, f)

	// RESULT.json is a directory, so the projection's rename cannot land.
	proj := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair", ResultFileName)
	if err := os.MkdirAll(filepath.Join(proj, "blocker"), 0o755); err != nil {
		t.Fatalf("obstruct projection: %v", err)
	}

	out, err := submitCompletionPass(t, f, reviewID, body)
	if err != nil {
		t.Fatalf("a failed projection changed the decision: %v", err)
	}
	if !out.Verified || out.ResultDigest == "" {
		t.Fatalf("outcome = %+v, want a committed decision", out)
	}
	st := mustState(t, f, "forge-run-1842-repair")
	if st.Stage != StageVerified || st.ResultDigest != out.ResultDigest || st.Completion == nil {
		t.Fatalf("state is not authoritative and complete: stage=%s digest=%s completion=%v",
			st.Stage, st.ResultDigest, st.Completion != nil)
	}
	for _, o := range st.Obligations {
		if o.Disposition != DispositionAdvanced {
			t.Errorf("obligation %s is %s beside a committed PASS", o.ID, o.Disposition)
		}
	}

	// Once the obstruction is gone the projection is re-derived, not re-decided.
	if err := os.RemoveAll(proj); err != nil {
		t.Fatalf("clear obstruction: %v", err)
	}
	res, err := f.store.Result("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("result after the projection was re-derived: %v", err)
	}
	if !res.Ready || res.Status != contract.StatusPass {
		t.Errorf("result = %+v, want the re-derived PASS", res)
	}
}
