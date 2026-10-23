package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// Audit gates, objective failure cycles and durable memory.
//
// Three things live here because they share one property: each is a place where
// an unearned conclusion would be invisible.
//
//   - A completion review is the only thing that can verify a request, so a PASS
//     must be correlated to an active request, a current generation, the right
//     reviewer, and a committed report whose bytes still hash to what state says.
//   - A failure cycle is counted from objective evidence — a check identity and a
//     normalized error — rather than from a worker's self-declared hypothesis, so
//     renaming a hypothesis cannot buy another attempt.
//   - Questions and decisions are durable, task-scoped memory with stable
//     identifiers, so a replacement worker reads what was already settled instead
//     of rediscovering it.

// MaxFailureRecords bounds the retained failure ledger.
const MaxFailureRecords = 64

// MaxQuestions and MaxDecisions bound the durable memory indexes.
const (
	MaxQuestions = 200
	MaxDecisions = 200
)

// FailureRecord is one objective failed attempt.
//
// Signature and OriginalText are both kept. The normalized signature is what
// makes a repeat recognisable; the original text is what lets a reviewer judge
// whether the normalization was right. Keeping only the signature would make an
// over-broad normalization invisible, and two genuinely different failures
// collapsed into one would silently consume a worker's budget.
type FailureRecord struct {
	// CycleID is the idempotency key. Re-submitting the same cycle costs nothing.
	CycleID string `json:"cycle_id"`
	// CheckID names the failing check.
	CheckID string `json:"check_id"`
	// ObligationID is the obligation the failure bears on, when known.
	ObligationID string `json:"obligation_id,omitempty"`
	// Signature is the normalized error identity.
	Signature string `json:"signature"`
	// OriginalText is the unnormalized text, preserved verbatim.
	OriginalText string `json:"original_text,omitempty"`
	// Hypothesis is the worker's self-declared reason. It is recorded and never
	// used to decide whether an attempt was distinct.
	Hypothesis string `json:"hypothesis,omitempty"`
	ObservedAt string `json:"observed_at"`
}

// NormalizeError reduces an error text to a stable identity.
//
// The normalization is deliberately conservative: it keeps the first line, drops
// volatile substrings (durations, hex addresses, line and column numbers, memory
// addresses) and collapses whitespace. It does not try to understand the error,
// because a normalizer that decides two errors are causally the same is making a
// judgement it has no grounds for. Callers may therefore still treat a repeated
// signature as a human decision rather than a proof.
func NormalizeError(text string) string {
	lines := strings.Split(text, "\n")
	head := ""
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			head = strings.TrimSpace(l)
			break
		}
	}
	if head == "" {
		return ""
	}
	fields := strings.FieldsFunc(head, func(r rune) bool {
		return r == ' ' || r == '\t'
	})
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		lower := strings.ToLower(f)
		switch {
		case strings.HasPrefix(lower, "0x"), len(f) >= 12 && isHexish(f):
			// Addresses and long hex blobs vary per run and carry no identity.
			kept = append(kept, "<hex>")
		case strings.HasPrefix(lower, "line "), strings.HasPrefix(lower, "column "),
			strings.HasPrefix(lower, "at line"), strings.HasPrefix(lower, "at column"):
			kept = append(kept, "<pos>")
		case isDurationish(lower):
			kept = append(kept, "<dur>")
		default:
			kept = append(kept, strings.ToLower(f))
		}
	}
	return strings.Join(kept, " ")
}

func isHexish(s string) bool {
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' {
			digits++
		} else {
			return false
		}
	}
	return digits == len(s)
}

func isDurationish(s string) bool {
	if !strings.HasSuffix(s, "s") || len(s) < 3 {
		return false
	}
	body := strings.TrimSuffix(s, "s")
	if !strings.ContainsAny(body, ".") && !containsDigit(body) {
		return false
	}
	return containsDigit(body)
}

func containsDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// CycleVerdict is what recording a failure concluded.
type CycleVerdict struct {
	// Duplicate is true when this cycle ID was already recorded, so nothing was
	// charged against the budget.
	Duplicate bool
	// RepeatedSignature is true when this exact failure has been seen before.
	RepeatedSignature bool
	// BudgetExhausted is true when the repair-cycle ceiling is now reached.
	BudgetExhausted bool
	// GuidanceExhausted is true when the diagnostic-round ceiling is now reached.
	GuidanceExhausted bool
	// CyclesUsed and CyclesAllowed report the objective position.
	CyclesUsed    int
	CyclesAllowed int
	// NeedsDiagnosis is true when a diagnostic review should be opened.
	NeedsDiagnosis bool
	// Reason explains the verdict for a stuck report.
	Reason string
}

// RecordFailure books one failed cycle against the objective budget.
//
// The two escalation triggers are deliberately different in kind. A *new* failure
// costs one unit of budget, and the budget running out is the evidence that the
// approach itself is the problem. A *repeat* of an already-seen signature costs
// nothing extra and diagnoses immediately: re-running an approach already known
// to fail is not an attempt, it is a loop.
//
// A renamed hypothesis changes neither conclusion. That is the whole point of
// counting objective signatures instead of self-declared approaches.
func (s *State) RecordFailure(rec FailureRecord, now time.Time) CycleVerdict {
	if strings.TrimSpace(rec.CycleID) == "" {
		rec.CycleID = rec.CheckID + ":" + rec.Signature
	}
	if rec.Signature == "" {
		rec.Signature = NormalizeError(rec.OriginalText)
	}
	rec.ObservedAt = now.UTC().Format(time.RFC3339)

	used := s.RepairCycles
	allowed := s.Limits.RepairCycles
	if allowed <= 0 {
		allowed = contract.DefaultRepairCycles
	}
	verdict := CycleVerdict{CyclesUsed: used, CyclesAllowed: allowed}

	// Idempotency first: the same cycle submitted twice is one cycle.
	for _, existing := range s.Failures {
		if existing.CycleID == rec.CycleID {
			verdict.Duplicate = true
			verdict.CyclesUsed = s.RepairCycles
			verdict.CyclesAllowed = allowed
			verdict.Reason = "this cycle was already recorded; no budget was consumed"
			return verdict
		}
	}
	// A repeat of the same objective failure is not a new attempt at all.
	//
	// The guidance ceiling is checked HERE as well, not only on the path below.
	// Returning early on a repeat is exactly the bypass that lets a loop run
	// forever: re-running a known-failing check consumed no budget, so without
	// this check a request could be handed to a diagnostic reviewer indefinitely
	// while every individual "attempt" was free.
	for _, existing := range s.Failures {
		if existing.Signature == rec.Signature && existing.CheckID == rec.CheckID {
			verdict.RepeatedSignature = true
			s.Failures = append(s.Failures, rec)
			s.trimFailures()
			verdict.CyclesUsed = s.RepairCycles
			verdict.CyclesAllowed = allowed
			verdict.Reason = "the same failure was already observed; this is a repeat, not progress"
			verdict.NeedsDiagnosis = true
			if s.DiagnosticRounds >= s.guidanceAllowed() {
				verdict.GuidanceExhausted = true
				verdict.NeedsDiagnosis = false
				verdict.Reason = "the same failure keeps recurring and the diagnostic guidance budget is exhausted; a human must decide"
				return verdict
			}
			if s.RepairCycles >= allowed {
				verdict.BudgetExhausted = true
				verdict.NeedsDiagnosis = true
				verdict.Reason = "the same failure keeps recurring and the repair-cycle budget is exhausted"
			}
			return verdict
		}
	}

	s.Failures = append(s.Failures, rec)
	s.trimFailures()
	s.RepairCycles++
	verdict.CyclesUsed = s.RepairCycles

	// Exhaustion is checked before another diagnostic, including on a repeat, so
	// a request cannot be handed to a reviewer indefinitely.
	if s.DiagnosticRounds >= s.guidanceAllowed() {
		verdict.GuidanceExhausted = true
		verdict.Reason = "the diagnostic guidance budget is exhausted; a human must decide"
		return verdict
	}
	if s.RepairCycles >= allowed {
		verdict.BudgetExhausted = true
		verdict.NeedsDiagnosis = true
		verdict.Reason = "the repair-cycle budget is exhausted"
		return verdict
	}
	verdict.Reason = "failure recorded within budget"
	return verdict
}

func (s *State) guidanceAllowed() int {
	if s.Limits.GuidanceRounds > 0 {
		return s.Limits.GuidanceRounds
	}
	return MaxGuidanceRounds
}

// MaxGuidanceRounds is the default diagnostic-round ceiling.
const MaxGuidanceRounds = 2

func (s *State) trimFailures() {
	if len(s.Failures) > MaxFailureRecords {
		s.Failures = s.Failures[len(s.Failures)-MaxFailureRecords:]
	}
}

// RenderStuckReport produces a stuck report from objective evidence.
//
// It is rendered from the failure ledger and the request's own records, so a
// worker that forgets to write one cannot prevent escalation. The worker's own
// explanation is included when present and is clearly attributed as the
// worker's, because it is reasoning rather than fact.
func (s *State) RenderStuckReport(reason string, workerExplanation string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\nGeneration: %d\nStage: %s\nWritten: %s\n",
		s.TaskID, s.Generation, s.Stage, s.UpdatedAt)
	fmt.Fprintf(&b, "Escalation: %s\n", reason)

	b.WriteString("\n# Objective\n")
	if strings.TrimSpace(s.StageReason) != "" {
		b.WriteString(s.StageReason + "\n")
	} else {
		b.WriteString("(not recorded)\n")
	}

	b.WriteString("\n# Open obligations\n")
	for _, o := range s.Obligations {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", o.Disposition, o.ID, o.Criterion)
	}

	b.WriteString("\n# Failure history (objective)\n")
	if len(s.Failures) == 0 {
		b.WriteString("No failures recorded.\n")
	}
	for i, f := range s.Failures {
		fmt.Fprintf(&b, "\n## Cycle %d — %s\n", i+1, f.CheckID)
		fmt.Fprintf(&b, "signature:  %s\n", f.Signature)
		if f.ObligationID != "" {
			fmt.Fprintf(&b, "obligation: %s\n", f.ObligationID)
		}
		if f.Hypothesis != "" {
			// Attributed, because this is the worker's claim, not a finding.
			fmt.Fprintf(&b, "worker hypothesis (claim, unverified): %s\n", f.Hypothesis)
		}
		if f.OriginalText != "" {
			b.WriteString("original output:\n")
			for _, l := range strings.Split(f.OriginalText, "\n") {
				if strings.TrimSpace(l) == "" {
					continue
				}
				b.WriteString("  " + l + "\n")
			}
		}
	}

	b.WriteString("\n# Question for the reviewer\n")
	if strings.TrimSpace(workerExplanation) != "" {
		b.WriteString("Worker states: " + workerExplanation + "\n")
	} else {
		b.WriteString("Why is implementation not progressing against the open obligations above?\n")
	}
	b.WriteString("\n<!-- Rendered from objective receipts. The worker's own report is attributed, not trusted. -->\n")
	return b.String()
}

// --- correlated review requests and answers ---------------------------------

// ReviewRequest is a correlated request for a judgement.
//
// Correlation is the entire point. An answer that names a review the state does
// not hold open, a different generation, or a different reviewer is refused,
// because the failure this prevents is a confident judgement about the wrong
// work — which is worse than no judgement, since it looks like supervision.
type ReviewRequest struct {
	Kind             ReviewKind `json:"kind"`
	ReviewID         string     `json:"review_id"`
	TaskID           string     `json:"task_id"`
	Generation       int        `json:"generation"`
	SnapshotID       string     `json:"snapshot_id,omitempty"`
	ExpectedReviewer string     `json:"expected_reviewer,omitempty"`
	RequestedAt      string     `json:"requested_at"`
	Deadline         string     `json:"deadline,omitempty"`
	// RequiredFields names what a DIAGNOSTIC answer must contain. Software
	// requires them rather than accepting a diagnosis that names no cause.
	RequiredFields []string `json:"required_fields,omitempty"`
}

// ReviewAnswer is a reviewer's typed response.
//
// SourceSession and AnswerNonce are the two fields that make the answer
// attributable. Without them the answer is a claim about a review, not a reply
// to it: any caller able to run the CLI could name the open review and assert
// PASS, and nothing in the store would distinguish that from the session that was
// actually asked. A caller-supplied reviewer name would not help either — that
// would be the answerer vouching for itself.
type ReviewAnswer struct {
	Kind     ReviewKind `json:"kind"`
	ReviewID string     `json:"review_id"`
	TaskID   string     `json:"task_id"`
	// Generation is the implementer generation the answer is scoped to.
	Generation int `json:"generation"`
	// SourceSession is the full session ID that produced this answer.
	//
	// On the trusted ingress path the engine stamps this from the session it
	// actually queried, so it is a measured identity rather than a declared one.
	// It is compared against the review's ExpectedReviewer and against the
	// request's whole implementation lineage.
	SourceSession string `json:"source_session,omitempty"`
	// SourceModel is the model identity that actually produced this answer.
	//
	// It is recorded rather than trusted: it is compared against the review's
	// ExpectedModel, so an answer attributed to a model nobody admitted is
	// refused instead of quietly accepted. Empty remains unknown and is allowed
	// by the operator model-selection policy. Model echoes never populate it.
	SourceModel string `json:"source_model,omitempty"`
	// ModelEcho is model-authored text for operator inspection, never attribution.
	ModelEcho string `json:"model_echo,omitempty"`
	// AnswerNonce must echo the nonce issued with the review, so an answer to a
	// closed-then-reopened review is refused rather than replayed.
	AnswerNonce string        `json:"answer_nonce,omitempty"`
	Outcome     ReviewOutcome `json:"outcome"`
	// ReportDigest must reference a committed report. A PASS without one is
	// refused: the reasoning behind an approval is part of the approval.
	ReportDigest string `json:"report_sha256,omitempty"`
	// Body carries a diagnostic's required fields, and a completion PASS's
	// typed CompletionReport.
	//
	// For a completion PASS it is no longer free text. The store parses it as
	// one judgement per obligation and refuses anything it cannot read that
	// way, because a sentence that says "all obligations are discharged" says
	// nothing about WHICH obligation was judged or on what evidence.
	Body string `json:"body,omitempty"`
	// Trusted marks an answer the broker collected from the expected session's
	// own output, as opposed to one imported by hand.
	//
	// A manual import remains accepted for operator workflow, but it is recorded
	// as an import rather than an independent judgement, and it can never
	// impersonate a model reviewer.
	Trusted bool `json:"trusted,omitempty"`
}

// AnswerError explains why an answer was refused.
type AnswerError struct{ Reason string }

func (e *AnswerError) Error() string { return "agent-engine: review answer refused: " + e.Reason }

func refuse(format string, args ...any) error {
	return &AnswerError{Reason: fmt.Sprintf(format, args...)}
}

// ErrNotApprovable is returned when an outcome would approve something its mode
// cannot approve.
var ErrNotApprovable = errors.New("agent-engine: this review mode cannot approve the request")

// AnswerOutcome is the effect of an accepted review answer.
type AnswerOutcome struct {
	// Verified is true when this answer verified the request.
	Verified bool
	// ResultStatus is the committed terminal status, when one was produced.
	ResultStatus string
	// ResultDigest is the committed result's digest.
	ResultDigest string
	// NeedsRepair is true when a FAIL must be returned to the worker.
	NeedsRepair bool
	// FailureCycleID is the repair cycle a FAIL booked.
	FailureCycleID string
}

// ReviewTimeout is how long a review may remain owed before it is due.
const ReviewTimeout = 30 * time.Minute

// SubmitAnswer validates and applies a correlated review answer.
//
// The ordering is deliberate and is the whole safety property of this function:
// correlation is checked first, then the report is committed, and only then is
// the outcome applied. A PASS therefore cannot be recorded unless a report
// exists whose bytes still hash to the digest the answer named — which is what
// stops a bare "PASS" from verifying a request.
func (s *Store) SubmitAnswer(requestID string, ans ReviewAnswer) (AnswerOutcome, error) {
	var out AnswerOutcome

	st, err := s.State(requestID)
	if err != nil {
		return out, err
	}
	if st.Stage.Terminal() {
		return out, refuse("the request is already terminal as %s; a published outcome never changes", st.ResultStatus)
	}
	if st.ActiveReview == nil {
		return out, refuse("no review is open; an answer to a review nobody asked for is not evidence")
	}
	active := st.ActiveReview
	if ans.ReviewID == "" {
		return out, refuse("the answer does not name the review it is answering")
	}
	if ans.ReviewID != active.ReviewID {
		return out, refuse("review %s is open, not %s", active.ReviewID, ans.ReviewID)
	}
	if ans.Kind != active.Kind {
		return out, refuse("the answer claims to be %s but the open review is %s", ans.Kind, active.Kind)
	}
	if ans.TaskID != "" && ans.TaskID != st.TaskID {
		return out, refuse("the answer names task %s, not %s", ans.TaskID, st.TaskID)
	}
	if ans.Generation != active.Generation || ans.Generation != st.Generation {
		return out, refuse("the answer is for generation %d; the request is at %d", ans.Generation, st.Generation)
	}
	if !active.Kind.Allows(ans.Outcome) {
		return out, refuse("a %s review cannot answer %s", active.Kind, ans.Outcome)
	}
	// The nonce binds this answer to this specific request for a judgement, so a
	// response captured for an earlier review of the same ID cannot be replayed
	// into a reopened one.
	//
	// An approving answer additionally REQUIRES a nonce to have been issued. A
	// review opened without one has no replay protection, and the safe reading of
	// "this review cannot be correlated" is "this review cannot approve", not
	// "the identity check is probably enough".
	if active.AnswerNonce != "" && ans.AnswerNonce != active.AnswerNonce {
		return out, refuse("the answer does not carry the nonce issued for review %s", active.ReviewID)
	}
	if active.Kind.Approves() && ans.Outcome == OutcomePass && active.AnswerNonce == "" {
		return out, refuse("review %s carries no correlation nonce, so its answer cannot be proven to belong to it; it cannot approve",
			active.ReviewID)
	}
	// Identity is checked against the expected session and the whole
	// implementation lineage, because a session that did any part of this work
	// cannot be the independent judge of it.
	if err := st.checkAnswerIdentity(active, ans); err != nil {
		return out, err
	}

	// Observed model conflicts are refused; missing attribution is allowed.
	//
	// Recording only the session says "this session said PASS" while leaving
	// open what it was running, which is the gap that lets a reviewer be swapped
	// to a different model underneath an audit. The comparison is against what
	// was PINNED when the review was opened, not against whatever is current.
	if active.ExpectedModel != "" && ans.SourceModel != "" && ans.SourceModel != active.ExpectedModel {
		return out, refuse("the answer is attributed to model %q but review %s was opened against model %q; "+
			"the reviewer is not the one this review was asked of",
			ans.SourceModel, active.ReviewID, active.ExpectedModel)
	}

	// An approving answer must cite a report whose bytes are committed and whose
	// measured digest is the one the answer claims.
	//
	// The digest is verified, not merely required to be present. A non-empty
	// check would accept any 64 characters, which is indistinguishable from
	// having checked the reasoning at all.
	var reportDigest string
	if ans.Outcome == OutcomePass {
		if err := st.VerifyRepairBranch(); err != nil {
			return out, refuse("repair branch changed: %v", err)
		}
		if strings.TrimSpace(ans.ReportDigest) == "" {
			return out, refuse("a completion PASS must cite a committed report; the reasoning is part of the approval")
		}
		if err := contract.ValidateDigest(ans.ReportDigest); err != nil {
			return out, refuse("the cited report digest is not a valid digest: %v", err)
		}
		body := []byte(ans.Body)
		if len(strings.TrimSpace(string(body))) == 0 {
			return out, refuse("a completion PASS must carry the report body; a digest with no bytes proves nothing")
		}
		// Commit the body first, then read the digest back from the committed
		// bytes. Comparing the measured value against the claimed one is the
		// whole check: it cannot pass unless the bytes on disk are the ones the
		// answer vouched for.
		committed, err := s.CommitReport(requestID, active.ReviewID+"-answer", body,
			func(st *State, digest string) error { return nil })
		if err != nil {
			return out, err
		}
		if committed != ans.ReportDigest {
			return out, refuse("the committed report hashes to %s, not the cited %s; the approval does not describe the reasoning it claims",
				committed, ans.ReportDigest)
		}
		reportDigest = committed
	}

	// An approving answer takes a different route entirely. It must not be applied
	// by the generic transaction below, because acceptance and publication are one
	// decision: committing the review and then publishing separately is what left
	// a crash window in which neither existed.
	if ans.Outcome == OutcomePass && active.Kind.Approves() {
		return s.commitCompletion(requestID, st, active, ans, reportDigest)
	}

	if _, err := s.UpdateCommitted(requestID, func(cur *State) error {
		if cur.ActiveReview == nil || cur.ActiveReview.ReviewID != active.ReviewID {
			return refuse("the review was closed by another writer")
		}
		switch ans.Outcome {
		case OutcomeGuidance:
			// A diagnostic consumes a round and returns the worker to work.
			cur.DiagnosticRounds++
			if err := cur.ApplyReview(active.ReviewID, ans.Outcome, ans.Body, s.now()); err != nil {
				return err
			}
			if cur.Stage == StageDiagnostic || cur.Stage == StageCheckpointReview {
				if err := cur.transition(StageWorking, "diagnostic guidance accepted", s.now().UTC().Format(time.RFC3339)); err != nil {
					return err
				}
			}
		case OutcomeRefresh:
			if err := cur.ApplyReview(active.ReviewID, ans.Outcome, ans.Body, s.now()); err != nil {
				return err
			}
		case OutcomeFail:
			// A completion FAIL books a repair cycle and is bound for the same
			// worker. A FAIL that resets the lifetime budget would let a request
			// loop forever.
			cycleID := fmt.Sprintf("audit-%s", active.ReviewID)
			verdict := cur.RecordFailure(FailureRecord{
				CycleID:      cycleID,
				CheckID:      "completion-audit",
				Signature:    "audit:" + active.ReviewID,
				OriginalText: ans.Body,
			}, s.now())
			out.NeedsRepair = true
			out.FailureCycleID = cycleID
			_ = verdict
			if err := cur.ApplyReview(active.ReviewID, ans.Outcome, ans.Body, s.now()); err != nil {
				return err
			}
		default:
			if err := cur.ApplyReview(active.ReviewID, ans.Outcome, ans.Body, s.now()); err != nil {
				return err
			}
			if ans.Outcome == OutcomeBlocked {
				if err := cur.transition(StageBlocked, "review blocked: "+ans.Body, s.now().UTC().Format(time.RFC3339)); err != nil {
					return err
				}
			}
		}
		// Closing a review clears the active slot, so after this transaction the
		// state no longer says WHICH verdict landed. An episode that owed this
		// judgement cannot advance on "a review was answered", so its own ledger
		// records the settlement here — in the ONE place an answer is applied, so
		// there is no second path that could record it and no second verdict to
		// disagree with this one. A PASS never reaches this closure: it commits
		// terminally through commitCompletion, which is its own durable record.
		cur.recordEpisodeReviewSettlement(active, ans, s.now().UTC().Format(time.RFC3339))
		return nil
	}); err != nil {
		return AnswerOutcome{}, err
	}
	return out, nil
}

// CompletionReport is the body a completion PASS must commit.
//
// It is a fixed typed payload rather than prose because a completion PASS is the
// only thing in the system that can discharge an obligation, and prose cannot
// say WHICH obligation was judged, with what verdict, or on what evidence.
// Inferring that from a reviewer's sentences would make a model sentence the
// authority for an obligation's disposition — which is exactly the authority
// surface this package refuses everywhere else. The reviewer is measured, never
// parsed.
//
// The payload lives INSIDE the report body rather than beside it, so the
// existing measured report digest binds the judgements and the reasoning
// together. A side list could disagree with the prose it was written to support;
// these bytes cannot, because there are no other bytes.
type CompletionReport struct {
	// Reasoning is the reviewer's narrative. It is required: an approval that
	// states no reasoning is not a judgement, however complete its table.
	Reasoning string `json:"reasoning"`
	// Judgements is one entry per obligation, judged by ID. Coverage is decided
	// against the obligations as they stand when the answer is applied, never
	// against whatever list the reviewer was shown earlier.
	Judgements []ObligationJudgement `json:"judgements"`
}

// ObligationJudgement is one reviewer's verdict about one obligation.
type ObligationJudgement struct {
	// ObligationID names the obligation. An ID this request does not owe
	// advances nothing and is refused rather than ignored.
	ObligationID string `json:"obligation_id"`
	// Outcome is the disposition the reviewer judged, in the obligation's own
	// vocabulary. DispositionAdvanced is the only positive judgement: a
	// completion PASS that judges an obligation anything else is asserting PASS
	// and outstanding work in the same breath, and is refused rather than
	// resolved. That includes FAILED — the reviewer's own statement that the
	// work did not happen.
	Outcome string `json:"outcome"`
	// EvidenceRefs names request-scoped immutable evidence backing this
	// judgement. Every reference must resolve; a judgement citing nothing is
	// not a judgement.
	EvidenceRefs []string `json:"evidence_refs"`
}

// parseCompletionReport reads a committed report body as a completion report.
//
// The decode is strict in both directions. An unknown field is refused, because
// a reviewer who writes a field this store ignores has been told something
// untrue about what was recorded. Content after the payload is refused, because
// a body that is a valid payload followed by something else is not a payload and
// its digest would bind only half of what it appears to. Prose is refused for
// the same reason: it carries no judgement the store could check.
func parseCompletionReport(body string) (CompletionReport, error) {
	var rep CompletionReport
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return CompletionReport{}, refuse("the committed completion report is not a typed judgement payload: %v; "+
			"a completion PASS must carry reasoning and one judgement per obligation", err)
	}
	if dec.More() {
		return CompletionReport{}, refuse("the committed completion report carries content after the payload; " +
			"the body must be exactly one payload so the report digest binds all of it")
	}
	if strings.TrimSpace(rep.Reasoning) == "" {
		return CompletionReport{}, refuse("the committed completion report states no reasoning; " +
			"an approval that does not say why is not a judgement")
	}
	return rep, nil
}

// completionScopeDigest measures the work an approval is being prepared for.
//
// It covers the request's own required checks and every obligation definition,
// because those are what the prepared evidence was gathered for. Comparing the
// measurement taken before preparation with the one taken inside the final
// transaction is what catches an obligation added, removed or re-scoped while
// the checks were running. Approving against a scope that has since moved would
// publish a PASS for work nobody judged, which is the same defect as approving
// work that was never judged at all.
func completionScopeDigest(st *State) string {
	var b strings.Builder
	for _, c := range st.Checks {
		b.WriteString("check\x00" + c + "\x01")
	}
	for _, o := range st.Obligations {
		b.WriteString("obligation\x00" + o.ID + "\x00" + o.Criterion + "\x01")
		for _, rc := range o.RequiredChecks {
			b.WriteString("required\x00" + rc + "\x01")
		}
	}
	return sha256Hex([]byte(b.String()))
}

// resolveJudgementEvidence reports whether ref names evidence this request can
// still stand behind.
//
// A non-empty string is not enough, and that is the entire purpose of this
// function. The two vocabularies that already exist are registered evidence refs
// and managed verification receipts; a reference outside both names nothing, so
// a judgement resting on it attributes to nothing whatsoever.
//
// A registered ref is re-measured rather than trusted. The digest it was pinned
// with is the whole claim that the bytes it names have not moved, so a ref whose
// bytes no longer hash to that digest is evidence of something other than what
// was recorded when it was recorded.
func (s *State) resolveJudgementEvidence(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return refuse("names no evidence; a judgement with no evidence is not a judgement")
	}
	for _, ev := range s.Checkpoint.Evidence {
		if ev.ID != ref {
			continue
		}
		if err := contractValidateDigest(ev.SHA256); err != nil {
			return refuse("evidence %s is pinned to %q, which is not a digest: %v", ref, ev.SHA256, err)
		}
		path := strings.TrimSpace(ev.Path)
		if path == "" {
			return refuse("evidence %s has no retained bytes to measure", ref)
		}
		got, err := DigestFile(path)
		if err != nil {
			return refuse("evidence %s is recorded at %s but its bytes can no longer be measured: %v", ref, path, err)
		}
		if !strings.EqualFold(got, ev.SHA256) {
			return refuse("evidence %s now hashes to %s, not the %s it was pinned with; the judgement would rest on bytes that changed after it was recorded",
				ref, got, strings.ToLower(ev.SHA256))
		}
		return nil
	}
	for _, j := range s.Jobs {
		if j.ReceiptID != ref {
			continue
		}
		// A receipt that did not pass is evidence that a check was run. It is
		// not evidence that an obligation advanced, and citing it as backing
		// would let a recorded failure stand behind an advance.
		if j.Status != JobPass {
			return refuse("receipt %s records check %s as %s; only a passing receipt can back an advance",
				ref, j.CheckID, j.Status)
		}
		return nil
	}
	return refuse("names %q, which is neither a registered evidence ref nor a managed verification receipt of this request", ref)
}

// dischargeObligations validates a committed report against the CURRENT
// obligations and, only once every judgement holds, advances them.
//
// Validation completes before any mutation, on purpose. A report that judges
// three of four obligations, or cites evidence that does not resolve, has
// advanced nothing at all: a partially discharged request is one whose remaining
// work nobody is tracking, which is a worse state to be in than one that is
// still honestly owed.
//
// ADVANCED is the only positive judgement. FAILED is not a weaker pass — it is
// the reviewer stating that the work did not happen, and treating it as one is
// how a FAILED obligation came to satisfy completion.
func dischargeObligations(cur *State, rep CompletionReport) error {
	current := make(map[string]*Obligation, len(cur.Obligations))
	for i := range cur.Obligations {
		o := &cur.Obligations[i]
		id := strings.TrimSpace(o.ID)
		if id == "" {
			return refuse("the request owes an obligation with no ID, which no judgement can name")
		}
		if _, dup := current[id]; dup {
			return refuse("obligation %s appears more than once in state; coverage cannot be decided against it", id)
		}
		current[id] = o
	}

	// First pass: validate every judgement. Nothing below this point mutates.
	approved := make(map[string][]string, len(rep.Judgements))
	for _, j := range rep.Judgements {
		id := strings.TrimSpace(j.ObligationID)
		if id == "" {
			return refuse("the report carries a judgement that names no obligation")
		}
		if _, ok := current[id]; !ok {
			return refuse("the report judges obligation %s, which this request does not owe", id)
		}
		if _, dup := approved[id]; dup {
			return refuse("obligation %s is judged twice; two verdicts about one criterion is not a judgement", id)
		}
		if strings.TrimSpace(j.Outcome) != DispositionAdvanced {
			return refuse("obligation %s is judged %q; a completion PASS requires a positive judgement (%s) for every obligation it covers",
				id, j.Outcome, DispositionAdvanced)
		}
		refs := dedupeStrings(j.EvidenceRefs)
		if len(refs) == 0 {
			return refuse("obligation %s is judged %s with no evidence reference", id, DispositionAdvanced)
		}
		for _, ref := range refs {
			if err := cur.resolveJudgementEvidence(ref); err != nil {
				return refuse("obligation %s: %v", id, err)
			}
		}
		approved[id] = refs
	}
	// Coverage is decided against the obligations as they stand now. A missing
	// judgement leaves that obligation exactly as it was, whatever it was.
	for id, o := range current {
		if _, judged := approved[id]; !judged {
			return refuse("the report carries no judgement for obligation %s, which is still %s; "+
				"a completion PASS must judge every current obligation", id, o.Disposition)
		}
	}

	// Second pass: mutate. Every judgement has already been validated, so this
	// cannot leave a half-discharged request behind.
	for i := range cur.Obligations {
		o := &cur.Obligations[i]
		o.Disposition = DispositionAdvanced
		o.EvidenceRefs = approved[strings.TrimSpace(o.ID)]
	}
	// Asserted after the fact rather than assumed: the assertion is what makes a
	// future edit to the loop above unable to publish an approval that left an
	// obligation FAILED, UNKNOWN or unbacked. It aborts the transaction, so a
	// failure here changes nothing.
	for _, o := range cur.Obligations {
		if o.Disposition != DispositionAdvanced || len(o.EvidenceRefs) == 0 {
			return fmt.Errorf("agent-engine: obligation %s is %s with %d evidence refs after a validated discharge; "+
				"refusing to publish an approval that does not discharge every obligation",
				o.ID, o.Disposition, len(o.EvidenceRefs))
		}
	}
	return nil
}

// commitCompletion performs the terminal approval as one recoverable decision.
//
// "A completion PASS does not supply missing judgments. Store validates an
// explicit, evidence-backed judgment for every current obligation before
// advancing any of them. Discharge, review settlement and result publication
// commit together; incomplete coverage is refused without changing
// authoritative state."
//
// The preparation order is the whole safety property, and no obligation advances
// during any of it:
//
//  1. Correlation, reviewer identity and the report digest have already been
//     validated by SubmitAnswer, and the committed report's judgements are
//     parsed here — before anything is executed, so a body that carries no
//     judgement costs no build.
//  2. Required checks are executed (or their existing receipts revalidated)
//     against the current content, and their receipts are persisted.
//  3. The result body is written to disk under an immutable name and its
//     measured digest is prepared. It is not yet authoritative — nothing reads a
//     result except through state.
//  4. ONE state transaction revalidates the review, re-measures the scope it was
//     prepared against, validates every judgement against the obligations as
//     they stand NOW, discharges them, applies the review, and publishes.
//
// A crash after 1 or 2 leaves checks recorded and the review still owed, so the
// answer can be resubmitted. A crash after 3 leaves a body plus a pointer to
// it, so the outcome is still decidable rather than lost. A crash after 4 is a
// finished request whose only remaining work is regenerating a projection, which
// Result already does. There is no boundary at which a review is settled and its
// result is missing, and none at which an obligation is discharged without the
// judgement that discharged it.
func (s *Store) commitCompletion(requestID string, snap *State, active *ActiveReview, ans ReviewAnswer, reportDigest string) (AnswerOutcome, error) {
	var out AnswerOutcome

	// The judgements are read before the checks run. A report the store cannot
	// read as one judgement per obligation is refused here, and an unrunnable
	// gate that refused first would have cost a real build to reach the same
	// answer.
	report, err := parseCompletionReport(ans.Body)
	if err != nil {
		return out, err
	}
	// The scope this approval is prepared against, measured once here and
	// re-measured inside the transaction. Anything that moved in between is
	// refused rather than silently approved.
	scope := completionScopeDigest(snap)

	// Required checks are the union of the request's own set and any check an
	// obligation names, so a caller cannot satisfy the request by omitting an
	// obligation's check from the request.
	required := append([]string(nil), snap.Checks...)
	for _, o := range snap.Obligations {
		required = append(required, o.RequiredChecks...)
	}
	required = dedupeStrings(required)

	nowFn := s.now
	// The baseline is established before the checks run, so the comparison is
	// "content when work started" against "content now". Without it every result
	// reports code_changed=false, which is wrong for any request that changed
	// code and is exactly the kind of unearned claim this layer exists to prevent.
	if berr := s.ensureBaseline(requestID); berr != nil {
		return out, fmt.Errorf("agent-engine: the baseline content identity could not be measured, so this "+
			"approval cannot report whether code changed: %w", berr)
	}
	receipts, err := s.runRequiredChecks(requestID, required, nowFn)
	if err != nil {
		return out, err
	}
	gate := EvaluateVerification(required, receipts, Snapshot{})
	if !gate.Satisfied {
		return out, refuse("required verification is not met: %s", gate.Reason)
	}

	// The evidence the approval rests on, assembled from what actually ran.
	evidence := CompletionEvidence{
		ReviewID:        active.ReviewID,
		ReviewerSession: ans.SourceSession,
		Generation:      active.Generation,
		ReportDigest:    reportDigest,
		ApprovedAt:      nowFn().UTC().Format(time.RFC3339),
		BaselineDigest:  snap.BaselineDigest,
		CheckCount:      len(receipts),
	}
	for _, r := range receipts {
		evidence.ReceiptIDs = append(evidence.ReceiptIDs, r.ReceiptID)
		evidence.SnapshotID = r.SnapshotID
		evidence.ContentDigest = r.ContentDigest
	}
	if len(receipts) > 0 {
		evidence.FinalDigest = receipts[len(receipts)-1].ContentDigest
	}

	// The result body is built from that evidence, so what a caller reads names
	// the checks that ran and the content they ran against.
	res := newResultFor(requestID, snap, contractStatusPass)
	res.Verification = verificationResultsFor(receipts)
	res.CompletionAudit = &contract.CompletionAuditRef{
		ReviewID:        active.ReviewID,
		Outcome:         string(ans.Outcome),
		ReviewerSession: ans.SourceSession,
		ReviewedAt:      evidence.ApprovedAt,
		ReportSHA256:    reportDigest,
		Trusted:         ans.Trusted,
	}
	res.Workspace = &contract.ResultWorkspace{
		Branch:         snap.RepairBranch,
		BaseHead:       snap.RepairBaseHead,
		SnapshotID:     evidence.SnapshotID,
		ContentSHA256:  evidence.ContentDigest,
		BaselineSHA256: evidence.BaselineDigest,
		FinalSHA256:    evidence.FinalDigest,
	}
	// code_changed is measured: same digest means the content did not change, and
	// an absent baseline is no longer possible because ensureBaseline ran above.
	res.CodeChanged = evidence.BaselineDigest != "" && evidence.FinalDigest != "" &&
		evidence.BaselineDigest != evidence.FinalDigest

	// The body becomes durable before anything claims it.
	if err := s.writeResultBody(requestID, res); err != nil {
		return out, err
	}
	bodyDigest, err := s.digestResultBody(requestID)
	if err != nil {
		return out, err
	}

	// One transaction makes the whole outcome authoritative: the judgements are
	// discharged, the review is settled and the result is published together, or
	// none of it happens.
	if _, err := s.UpdateCommitted(requestID, func(cur *State) error {
		if cur.ActiveReview == nil || cur.ActiveReview.ReviewID != active.ReviewID {
			return refuse("the review was closed by another writer")
		}
		// The nonce is re-checked as well as the ID. A review closed and reopened
		// under the same ID carries a different nonce, and an answer prepared
		// for the previous one must not be applied to this one however well it
		// correlates by name.
		if cur.ActiveReview.AnswerNonce != active.AnswerNonce {
			return refuse("review %s was closed and reopened, and its answer is no longer the one that was asked for", active.ReviewID)
		}
		if cur.Generation != active.Generation {
			return refuse("the request advanced to generation %d while this answer was in flight", cur.Generation)
		}
		// Preparation measured the work this approval is for. If that work moved
		// while the checks were running, the judgements were written against a
		// scope that no longer exists and are refused — including an obligation
		// ADDED since, which no reviewer has judged at all.
		if got := completionScopeDigest(cur); got != scope {
			return refuse("the obligations or required checks changed while this approval was being prepared (%s, now %s); "+
				"an approval cannot be granted against work that has since moved", shortDigest(scope), shortDigest(got))
		}
		// Discharge and publication are one decision. dischargeObligations
		// validates every judgement before it advances any of them, so a refusal
		// here leaves the obligations exactly as they were and the review owed.
		if err := cur.VerifyRepairBranch(); err != nil {
			return err
		}
		if err := dischargeObligations(cur, report); err != nil {
			return err
		}
		// Only now is the review applied. ApplyReview settles the review and its
		// bookkeeping; it does not decide which obligations are satisfied, so by
		// this point there is nothing left for it to decide.
		if err := cur.ApplyReview(active.ReviewID, ans.Outcome, ans.Body, s.now()); err != nil {
			return err
		}
		cur.Completion = &evidence
		cur.ResultDigest = bodyDigest
		cur.ResultStatus = string(contractStatusPass)
		cur.PendingResult = ""
		return cur.transition(StageVerified, "independent completion audit passed with current executed evidence", evidence.ApprovedAt)
	}); err != nil {
		// The prewritten body is now unreferenced. It is reported as an orphan
		// rather than deleted, and the review is still owed because the
		// transaction that would have closed it did not commit.
		return AnswerOutcome{}, err
	}

	// The projection is derived, never authoritative. A failure here leaves a
	// finished request whose result is re-derivable on the next read.
	if st2, serr := s.State(requestID); serr == nil {
		_ = s.regenerateProjection(st2)
	}
	out.Verified = true
	out.ResultStatus = string(contractStatusPass)
	out.ResultDigest = bodyDigest
	return out, nil
}

// shortDigest abbreviates a digest for a refusal message. A refusal has to be
// able to say WHICH measurement it compared, and a reader does not need 64
// characters to tell two of them apart.
func shortDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}

// dedupeStrings removes repeats while preserving first-seen order.
func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// verificationResultsFor projects receipts into the public result contract.
func verificationResultsFor(receipts []CheckReceipt) []contract.VerificationResult {
	var out []contract.VerificationResult
	for _, r := range receipts {
		out = append(out, contract.VerificationResult{
			CheckID:       r.CheckID,
			Outcome:       r.Outcome,
			Reason:        r.Reason,
			ExitCode:      r.ExitCode,
			ReceiptID:     r.ReceiptID,
			SnapshotID:    r.SnapshotID,
			ContentSHA256: r.ContentDigest,
			Discovered:    r.Discovered,
			StdoutSHA256:  r.StdoutDigest,
			StderrSHA256:  r.StderrDigest,
		})
	}
	return out
}

// contractStatusPass is the terminal status a completed audit produces.
const contractStatusPass = contract.StatusPass

// checkAnswerIdentity refuses an answer that did not come from the session the
// review was addressed to.
//
// Three distinct violations are separated, because they are different failures
// with different fixes: an answer with no stated source, an answer from the wrong
// session, and an answer from a session that did the work. A reviewer that
// shares a CLI with the implementer is fine; a reviewer that IS the implementer,
// or a replaced one, never is.
func (s *State) checkAnswerIdentity(active *ActiveReview, ans ReviewAnswer) error {
	source := strings.TrimSpace(ans.SourceSession)

	// A review with no expected reviewer is not an independent review. Refusing
	// here is what stops a manually bound single-session request from
	// self-certifying.
	if strings.TrimSpace(active.ExpectedReviewer) == "" {
		return refuse("review %s names no independent reviewer; an unbound review cannot approve anything",
			active.ReviewID)
	}
	if source == "" {
		return refuse("the answer does not identify the session it came from; an approval must be attributable to the reviewer that gave it")
	}
	if source != active.ExpectedReviewer {
		return refuse("review %s was addressed to session %s, not %s",
			active.ReviewID, active.ExpectedReviewer, source)
	}
	if s.IsImplementationSession(source) {
		return refuse("session %s is inside this request's implementation lineage and cannot review its own work", source)
	}
	// The same check again against the raw answer, so a caller cannot clear it by
	// leaving Generation at zero.
	if s.WorkerSession != "" && source == s.WorkerSession {
		return refuse("session %s is the bound implementer and cannot review this request", source)
	}
	return nil
}

// newResultFor builds the durable result for a verified request.
//
// It reports what is actually true rather than restating the verdict: which
// checks were required, whether any repair cycle ran, and that the disposition
// came from an independent review. A result that simply echoed "PASS" would be
// indistinguishable from one produced without any evidence at all.
func newResultFor(requestID string, st *State, status contract.Status) contract.Result {
	findings := []contract.Finding{{
		Summary: "every obligation was reviewed and advanced by an independent completion audit",
	}}
	if st.RepairCycles > 0 {
		findings = append(findings, contract.Finding{
			Severity: "info",
			Summary:  fmt.Sprintf("%d repair cycle(s) were required before the audit passed", st.RepairCycles),
		})
	}
	return contract.Result{
		Status:      status,
		Summary:     "obligations met and an independent completion audit passed",
		Findings:    findings,
		CodeChanged: st.RepairCycles > 0 || st.Generation > 1,
	}
}
