package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emjin/agent-engine/internal/runtime"
	"strings"
)

// Broker-owned review ingress.
//
// This is the answer to "who actually said PASS". Everything else in the system
// records a claim; this file is where an identity is MEASURED.
//
// The flow is deliberately narrow. The engine already knows which session it
// asked for a judgement, because it opened the review against that session. So
// to establish who answered, it queries that one session's own output through the
// runtime and stamps the session it actually read:
//
//	review.ExpectedReviewer  ──query runtime──▶  Output{SessionID}
//	                     stamp ◀────────────────────┘
//
// Nothing in the submitted payload can substitute for the queried session. A
// caller may still submit the CONTENT of an answer, but the SourceSession on the
// resulting ReviewAnswer is written here, by the engine, from the session it read.
// That is what converts a self-asserted approval into an attributable one.
//
// Two properties this deliberately does not do:
//
//   - It does not parse prose into an approval. The caller supplies typed
//     fields; the broker supplies only the identity. Parsing free text would make
//     a model sentence an authority surface.
//   - It does not retry or guess. If the reviewer has not answered, the answer is
//     simply not there yet, and the review stays owed.

// ReviewEnvelope is the typed content a reviewer returns.
//
// It is intentionally narrow. The fields are exactly what a judgement needs, and
// a body — which for a completion PASS is a typed CompletionReport rather than
// prose — which is committed as evidence and never guessed at for meaning.
type ReviewEnvelope struct {
	RequestID   string        `json:"request_id"`
	Kind        ReviewKind    `json:"kind"`
	ReviewID    string        `json:"review_id"`
	TaskID      string        `json:"task_id"`
	Generation  int           `json:"generation"`
	AnswerNonce string        `json:"answer_nonce"`
	ModelEcho   string        `json:"model_echo,omitempty"`
	Outcome     ReviewOutcome `json:"outcome"`
	// ReportSHA256 must equal the digest of the body the broker commits.
	ReportSHA256 string `json:"report_sha256,omitempty"`
	// ReportBody is the reasoning. It is stored verbatim, and for a completion
	// PASS it must be one CompletionReport naming a judgement for every current
	// obligation — a PASS that carries only prose is refused.
	ReportBody string `json:"report_body,omitempty"`
}

// The prompt permits a completion report as a JSON object. Preserve that
// object's exact bytes as the report body; quoted report strings remain valid.
func (env *ReviewEnvelope) UnmarshalJSON(body []byte) error {
	type plain ReviewEnvelope
	var wire struct {
		*plain
		Report json.RawMessage `json:"report_body"`
	}
	wire.plain = (*plain)(env)
	if err := json.Unmarshal(body, &wire); err != nil {
		return err
	}
	if len(wire.Report) == 0 || string(wire.Report) == "null" {
		return nil
	}
	if wire.Report[0] == '"' {
		return json.Unmarshal(wire.Report, &env.ReportBody)
	}
	if wire.Report[0] != '{' || env.Outcome != OutcomePass || !env.Kind.Approves() {
		return fmt.Errorf("agent-engine: report_body must be a string or an approving completion report object")
	}
	env.ReportBody = string(wire.Report)
	return nil
}

// ErrReviewerSilent is returned when the expected reviewer has produced no
// answer yet.
//
// It is an ordinary non-terminal condition, not a failure: the review remains
// owed and the answer can be collected on a later pass.
var ErrReviewerSilent = errors.New("agent-engine: the expected reviewer has not answered yet")
var ErrReviewerUnreadable = errors.New("agent-engine: reviewer output is unavailable")

// CollectReviewerAnswer reads the expected reviewer's own output and returns an
// answer whose SourceSession the engine stamped.
//
// The envelope is decoded from what the reviewer actually wrote. The identity is
// NOT taken from that content: it is the session this method queried, so a
// reviewer cannot answer as somebody else by writing somebody else's name.
func (e *Engine) CollectReviewerAnswer(ctx context.Context, requestID string, maxTokens int) (ReviewAnswer, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return ReviewAnswer{}, err
	}
	if st.ActiveReview == nil {
		return ReviewAnswer{}, fmt.Errorf("agent-engine: no review is open for %s", requestID)
	}
	active := st.ActiveReview
	reviewer := strings.TrimSpace(active.ExpectedReviewer)
	if reviewer == "" {
		return ReviewAnswer{}, fmt.Errorf("agent-engine: review %s names no independent reviewer, so no answer can be attributed to it",
			active.ReviewID)
	}
	// A session that did the work can never be its own reviewer, whatever the
	// state says about the expected reviewer.
	if st.IsImplementationSession(reviewer) {
		return ReviewAnswer{}, fmt.Errorf("%w: review %s is addressed to session %s, which is inside the "+
			"implementation lineage", ErrProfileNotIsolated, active.ReviewID, reviewer)
	}

	backend, ref, err := e.dispatch.backendFor(requestID, reviewer)
	if err != nil {
		return ReviewAnswer{}, fmt.Errorf("%w: %v", ErrReviewerUnreadable, err)
	}
	out := runtime.SessionOutput{}
	if reader, ok := backend.(runtime.AttributedOutputReader); ok {
		out, err = reader.ReadAttributedOutput(ctx, ref, maxTokens)
	} else {
		var truncated bool
		out.Text, truncated, err = backend.Output(ctx, ref, maxTokens)
		out.SessionID = ref.ID
		if truncated {
			return ReviewAnswer{}, fmt.Errorf("%w: response was truncated", ErrReviewerUnreadable)
		}
	}
	if err != nil {
		// An unreadable reviewer is silence, not a verdict. Treating a read error
		// as "no answer, therefore fine" would be the exact failure this whole
		// stage exists to prevent.
		return ReviewAnswer{}, fmt.Errorf("%w: %v", ErrReviewerUnreadable, err)
	}
	// The identity is the session the runtime reported for the query, cross-checked
	// against the one we asked. A runtime that answers about a different session is
	// refused rather than believed.
	if out.SessionID != reviewer {
		return ReviewAnswer{}, fmt.Errorf("agent-engine: the runtime reported output for session %s when %s was queried; "+
			"refusing to attribute an answer to a session nobody asked", out.SessionID, reviewer)
	}

	env, err := decodeReviewEnvelope(out.Text)
	if err != nil {
		return ReviewAnswer{}, err
	}
	if env.RequestID != st.RequestID || env.Kind != active.Kind || env.ReviewID != active.ReviewID || env.TaskID != st.TaskID || env.Generation != active.Generation || env.AnswerNonce != active.AnswerNonce {
		return ReviewAnswer{}, fmt.Errorf("agent-engine: reviewer answer does not echo the current request, review, task, generation and nonce")
	}
	if env.Outcome == OutcomePass {
		measured := sha256Hex([]byte(env.ReportBody))
		if env.ReportSHA256 != "" && env.ReportSHA256 != measured {
			return ReviewAnswer{}, fmt.Errorf("agent-engine: reviewer supplied a report digest that does not describe its body")
		}
		// The broker measures content received from the independently queried
		// session. The model need not compute a cryptographic hash; Store still
		// commits these bytes and remeasures before any approving transaction.
		env.ReportSHA256 = measured
	}
	// Correlation is cross-checked before canonical fields are stamped. Otherwise
	// an old terminal answer would acquire the nonce of a later open review.
	return ReviewAnswer{
		Kind:          active.Kind,
		ReviewID:      active.ReviewID,
		TaskID:        st.TaskID,
		Generation:    active.Generation,
		SourceSession: reviewer,
		SourceModel:   out.Model,
		ModelEcho:     env.ModelEcho,
		AnswerNonce:   active.AnswerNonce,
		Outcome:       env.Outcome,
		Body:          env.ReportBody,
		ReportDigest:  env.ReportSHA256,
		// Trusted is true because the engine read this from the reviewer's own
		// session. It is the one place that may be set.
		Trusted: true,
	}, nil
}

// decodeReviewEnvelope reads a reviewer's typed answer.
//
// Legacy outcome tokens are recognised by the decoder, but collection requires
// the full correlation envelope. An uncorrelated token cannot become a verdict.
func decodeReviewEnvelope(text string) (ReviewEnvelope, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ReviewEnvelope{}, fmt.Errorf("%w: the reviewer produced no readable answer", ErrReviewerSilent)
	}
	// A fenced JSON block is the common shape when a model answers in a terminal.
	candidate := trimmed
	if strings.HasPrefix(candidate, "```") {
		candidate = strings.TrimPrefix(candidate, "```")
		candidate = strings.TrimPrefix(candidate, "json")
		candidate = strings.TrimSpace(candidate)
		if end := strings.Index(candidate, "```"); end >= 0 {
			candidate = strings.TrimSpace(candidate[:end])
		}
	}
	if strings.HasPrefix(candidate, "{") {
		var env ReviewEnvelope
		if err := json.Unmarshal([]byte(candidate), &env); err == nil && env.Outcome != "" {
			return env, nil
		}
	}
	// Fall back to a single bare outcome token, which is what a reviewer told to
	// "answer with one of: …" naturally produces.
	upper := strings.ToUpper(trimmed)
	for _, o := range []ReviewOutcome{
		OutcomePass, OutcomeFail, OutcomeBlocked, OutcomeGuidance,
		OutcomeRefresh, OutcomeContinue, OutcomeRedirect, OutcomeCollectEvidence, OutcomeDiagnose,
	} {
		if upper == string(o) {
			return ReviewEnvelope{Outcome: o, ReportBody: trimmed}, nil
		}
	}
	return ReviewEnvelope{}, fmt.Errorf("agent-engine: the reviewer's answer contains no recognised outcome; " +
		"expected one of PASS, FAIL, BLOCKED, GUIDANCE, REFRESH, CONTINUE, REDIRECT, COLLECT_EVIDENCE, DIAGNOSE")
}

func reviewAnswerInstruction(s *State) string {
	if s.ActiveReview == nil {
		return ""
	}
	r := s.ActiveReview
	body, _ := json.Marshal(ReviewEnvelope{RequestID: s.RequestID, Kind: r.Kind, ReviewID: r.ReviewID, TaskID: s.TaskID, Generation: r.Generation, AnswerNonce: r.AnswerNonce, Outcome: OutcomeBlocked, ReportBody: "replace with evidence and reasoning"})
	instruction := "\nReturn one JSON answer using this correlation exactly. Replace outcome with the permitted verdict and report_body with your findings:\n" + string(body) + "\n"
	if !r.Kind.Approves() {
		return instruction
	}
	// A completion review has to say what it judged, and the store cannot infer
	// that from prose. The obligations are named here — with their current
	// disposition — so the reviewer is answering the actual work rather than a
	// remembered list.
	//
	// The shape is described rather than shown as a second JSON document on
	// purpose: this message already carries the correlation envelope the
	// reviewer must echo, and a reader that takes the first object in the
	// message as that envelope must not find a second one ahead of it.
	var b strings.Builder
	b.WriteString(instruction)
	b.WriteString("\nFor PASS, report_body must be ONE JSON object with exactly these fields and no others:\n")
	b.WriteString("  reasoning: a string saying why each judgement follows from the evidence\n")
	b.WriteString("The controller measures the exact supplied report_body digest; do not invent a hash. A supplied mismatching report_sha256 is refused.\n")
	b.WriteString("  judgements: an array with one entry per CURRENT obligation, where each entry is an object with\n")
	b.WriteString("    obligation_id: the id listed below\n")
	b.WriteString("    outcome: the string " + DispositionAdvanced + "\n")
	b.WriteString("    evidence_refs: a non-empty array of ids already registered on this request — an evidence ref id,\n")
	b.WriteString("      or the receipt id of a passing managed check\n")
	b.WriteString("A missing, duplicate or unknown obligation id, any outcome other than " + DispositionAdvanced +
		", and evidence that does not resolve are all refused.\n")
	if len(s.Checkpoint.Evidence) > 0 {
		b.WriteString("Registered evidence reference ids (registration pins bytes, not truth; judge their supplied content):\n")
		for _, ev := range s.Checkpoint.Evidence {
			fmt.Fprintf(&b, "- %s sha256=%s kind=%s path=%s\n", ev.ID, ev.SHA256, ev.Kind, ev.Path)
		}
	}
	if len(s.Obligations) > 0 {
		b.WriteString("Current obligations:\n")
		for _, o := range s.Obligations {
			fmt.Fprintf(&b, "- %s [%s] %s\n", o.ID, o.Disposition, o.Criterion)
		}
	}
	return b.String()
}
