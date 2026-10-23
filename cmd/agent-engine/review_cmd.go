package main

// The remaining typed verbs: attempts, review answers, questions and decisions.
//
// All four are correlated submissions rather than free text, because each of them
// has to survive a restart and be attributable afterwards. An answer that says
// "PASS" in a pane of text cannot be checked against a generation, cannot be
// deduped, and cannot be distinguished from an answer to a review nobody asked
// for — which is precisely the defect that let the embedded draft accept
// uncorrelated audit results.

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emjin/agent-engine/internal/control"
)

func cmdAttempt(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("attempt", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	cycleID := fs.String("cycle-id", "", "idempotency key for this attempt")
	checkID := fs.String("check", "", "failing check id")
	obligation := fs.String("obligation", "", "obligation this failure bears on")
	output := fs.String("output", "", "the failing command's output")
	hypothesis := fs.String("hypothesis", "", "the worker's own explanation (recorded, never decisive)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	if strings.TrimSpace(*checkID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--check is required: a failure with no check identity cannot be counted objectively"))
	}

	var verdict control.CycleVerdict
	if _, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
		verdict = s.RecordFailure(control.FailureRecord{
			CycleID:      *cycleID,
			CheckID:      *checkID,
			ObligationID: *obligation,
			OriginalText: *output,
			Hypothesis:   *hypothesis,
		}, stNow())
		return nil
	}); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}

	// A repeat of a known failure, or an exhausted budget, means the request
	// needs a human or a diagnosis. That is surfaced here rather than being left
	// for the worker to work around.
	if verdict.GuidanceExhausted {
		if _, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
			return s.Transition(control.StageBlocked, verdict.Reason, stNow().Format(timeLayout))
		}); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
	}

	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":         *requestID,
			"cycle_id":           *cycleID,
			"duplicate":          verdict.Duplicate,
			"repeated_signature": verdict.RepeatedSignature,
			"budget_exhausted":   verdict.BudgetExhausted,
			"guidance_exhausted": verdict.GuidanceExhausted,
			"cycles_used":        verdict.CyclesUsed,
			"cycles_allowed":     verdict.CyclesAllowed,
			"needs_diagnosis":    verdict.NeedsDiagnosis,
			"reason":             verdict.Reason,
		})
	}
	fmt.Fprintf(stdout, "%s\n", verdict.Reason)
	fmt.Fprintf(stdout, "  cycles: %d/%d\n", verdict.CyclesUsed, verdict.CyclesAllowed)
	if verdict.NeedsDiagnosis {
		fmt.Fprintln(stdout, "  this failure needs an independent diagnosis, not another attempt")
	}
	return 0
}

// timeLayout is the stamp format used by the control state.
const timeLayout = "2006-01-02T15:04:05Z07:00"

func cmdReviewResult(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("review-result", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	reviewID := fs.String("review-id", "", "the review being answered")
	kind := fs.String("kind", "", "CHECKPOINT, DIAGNOSTIC or COMPLETION")
	generation := fs.Int("generation", 0, "generation the answer is scoped to")
	outcome := fs.String("outcome", "", "the outcome for this review mode")
	body := fs.String("body", "", "reasoning; required for a diagnostic")
	reportFile := fs.String("report-file", "", "report to commit before accepting an approving outcome")
	session := fs.String("session", "", "full session ID of the reviewer; required for an approving outcome")
	nonce := fs.String("answer-nonce", "", "nonce issued with the review")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" || strings.TrimSpace(*reviewID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id and --review-id are required"))
	}
	if strings.TrimSpace(*kind) == "" || strings.TrimSpace(*outcome) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--kind and --outcome are required"))
	}

	ans := control.ReviewAnswer{
		Kind:     control.ReviewKind(strings.ToUpper(*kind)),
		ReviewID: *reviewID,
		// The submitting session is attributed, not trusted: this command cannot
		// prove who typed it. It records the claim, and the store checks it
		// against the review the judgement was actually opened for. A caller
		// still cannot approve by naming a reviewer, because the name has to
		// match that review and must not be a session that did the work.
		SourceSession: strings.TrimSpace(*session),
		AnswerNonce:   strings.TrimSpace(*nonce),
		Generation:    *generation,
		Outcome:       control.ReviewOutcome(strings.ToUpper(*outcome)),
		Body:          *body,
		// A hand-submitted answer is recorded as an import, not as an
		// independent model judgement.
		Trusted: false,
	}
	if strings.TrimSpace(*reportFile) != "" {
		data, err := readInput(*reportFile)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		ans.Body = string(data)
		// The digest is the caller's claim. SubmitAnswer commits these exact
		// bytes, measures the result and refuses the answer unless the two
		// agree, so a digest that does not describe the file cannot buy an
		// approval.
		ans.ReportDigest = digestOf(data)
	}

	res, err := st.SubmitAnswer(*requestID, ans)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":    *requestID,
			"review_id":     *reviewID,
			"verified":      res.Verified,
			"result_status": res.ResultStatus,
			"result_sha256": res.ResultDigest,
			"needs_repair":  res.NeedsRepair,
			"failure_cycle": res.FailureCycleID,
		})
	}
	if res.Verified {
		fmt.Fprintf(stdout, "%s verified; result committed\n", *requestID)
		return 0
	}
	if res.NeedsRepair {
		fmt.Fprintf(stdout, "audit failed; repair cycle %s booked\n", res.FailureCycleID)
		return 0
	}
	fmt.Fprintf(stdout, "answer accepted: %s\n", *outcome)
	return 0
}

func cmdQuestion(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("question", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	title := fs.String("title", "", "one-line question")
	body := fs.String("body", "", "the question and why it matters")
	askedBy := fs.String("asked-by", "implementation", "role raising the question")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" || strings.TrimSpace(*title) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id and --title are required"))
	}
	var q control.QuestionRecord
	if _, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
		rec, err := s.OpenQuestion(*title, *body, *askedBy, stNow())
		q = rec
		return err
	}); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":        *requestID,
			"question_id":       q.ID,
			"status":            q.Status,
			"origin_generation": q.OriginGeneration,
		})
	}
	fmt.Fprintf(stdout, "opened %s: %s\n", q.ID, q.Title)
	return 0
}

func cmdDecision(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("decision", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	title := fs.String("title", "", "one-line decision")
	body := fs.String("body", "", "the reasoning and the evidence behind it")
	questionID := fs.String("question-id", "", "question this decision answers")
	authority := fs.String("accept-as", "", "role accepting this decision; empty records a proposal only")
	evidence := fs.String("evidence", "", "comma-separated evidence ids")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" || strings.TrimSpace(*title) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id and --title are required"))
	}
	var refs []string
	if strings.TrimSpace(*evidence) != "" {
		for _, r := range strings.Split(*evidence, ",") {
			if r = strings.TrimSpace(r); r != "" {
				refs = append(refs, r)
			}
		}
	}
	var d control.DecisionRecord
	if _, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
		rec, err := s.RecordDecision(*title, *body, *questionID, *authority, refs, stNow())
		d = rec
		return err
	}); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":        *requestID,
			"decision_id":       d.ID,
			"status":            d.Status,
			"authority":         d.Authority,
			"question_id":       d.QuestionID,
			"origin_generation": d.OriginGeneration,
		})
	}
	fmt.Fprintf(stdout, "recorded %s [%s]: %s\n", d.ID, d.Status, d.Title)
	if d.Status == control.DecisionProposedStatus {
		fmt.Fprintln(stdout, "  this is a proposal; only the configured decision authority may accept it")
	}
	return 0
}
