// Package agentdeck is the control layer's only door to Agent Deck.
//
// It shells out to the `agent-deck` CLI and decodes its public JSON. It does not
// import Agent Deck's internal packages, and it must not: those are unversioned
// implementation details, and a control plane that reaches into them stops being
// a control plane and becomes a second, invisible copy of session management.
// Going through the documented command surface is what makes the two products
// independently upgradable.
//
// Every method here returns a typed outcome rather than a bare error, because
// "the call failed" and "the call may or may not have taken effect" are very
// different situations for a supervisor. Only the first is safe to retry.
package agentdeck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds one CLI invocation.
const DefaultTimeout = 2 * time.Minute

// CreateSpec describes a session to create.
type CreateSpec struct {
	// Path is the absolute project directory.
	Path string
	// Title is the session title. It is display metadata only; routing uses the
	// returned full ID.
	Title string
	// Tool is an existing Agent Deck tool name from operator config. The
	// control layer never builds provider command lines itself.
	Tool           string
	Model          string
	Effort         string
	Sandbox        bool
	WorktreeBranch string
	NewBranch      bool
	NoParent       bool
	// Wrapper uses Agent Deck's persisted public --wrapper seam. Only an
	// operator-approved profile may populate it.
	Wrapper string
}

// Session is the confirmed identity of an Agent Deck session.
type Session struct {
	// ID is the full session ID. Routing, generation binding and stale-event
	// exclusion all use this, never the title: a title can be reused and a full
	// ID cannot.
	ID string `json:"id"`
	// Title is display metadata.
	Title string `json:"title,omitempty"`
	// Path is the project directory the session was created in.
	Path string `json:"path,omitempty"`
	// Status is Agent Deck's own status word.
	Status string `json:"status,omitempty"`
	// Tool is the tool the session runs.
	Tool string `json:"tool,omitempty"`
}

// SendOutcome classifies what is known about a message's delivery.
//
// The distinctions come straight from Agent Deck's queued-send semantics, and
// they are what make retry safe or unsafe:
//
//   - Landed: the text is in the transcript. Proof of delivery.
//   - Failed: nothing was typed. Retrying is safe.
//   - InFlight: the queue still owns it. Do not also send directly; that is how
//     an agent receives the same instruction twice.
//   - Ambiguous: the queue settled the record without transcript evidence and
//     will never type it again. The message may or may not have been typed. This
//     is the one case where a supervisor must block rather than decide.
type SendOutcome string

const (
	SendLanded    SendOutcome = "landed"
	SendFailed    SendOutcome = "failed"
	SendInFlight  SendOutcome = "in_flight"
	SendAmbiguous SendOutcome = "ambiguous"
	SendQueued    SendOutcome = "queued"
)

// Retryable reports whether the same message may be sent again.
//
// Only a known failure qualifies. Ambiguity is not a failure: it is the absence
// of one, and treating it as a failure is how a repair instruction gets applied
// twice.
func (o SendOutcome) Retryable() bool { return o == SendFailed }

// Receipt is the durable record of a queued send.
type Receipt struct {
	SendID       string `json:"send_id"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Settled      bool   `json:"settled,omitempty"`
	Attempts     int    `json:"attempts,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	TargetStatus string `json:"target_status,omitempty"`
	LandedRowID  string `json:"landed_row_id,omitempty"`
	LandedAt     string `json:"landed_at,omitempty"`
}

// Runtime is the capability surface the control layer needs from Agent Deck.
//
// It is deliberately small and behaviour-named. A future direct backend — a
// different harness, a remote host — can satisfy it without the control layer
// knowing anything about that backend.
type Runtime interface {
	// List returns every known session.
	//
	// It is here because an activation whose outcome is unknown can only be
	// resolved by finding the session its label already created. Without it the
	// only available answer to "did that create land?" is to create again, which
	// is how two workers end up on one workspace.
	List(ctx context.Context) ([]Session, error)
	// Create adds a session and returns its confirmed full ID.
	Create(ctx context.Context, spec CreateSpec) (Session, error)
	// Start begins a session's process.
	Start(ctx context.Context, id string) error
	// Inspect returns the current status of a session.
	Inspect(ctx context.Context, id string) (Session, error)
	// Send queues a message durably and returns its receipt.
	Send(ctx context.Context, id, message string) (Receipt, error)
	// SendStatus reconciles a queued send.
	SendStatus(ctx context.Context, sendID string) (Receipt, error)
	// Stop ends a session's process. It does not wait for full retirement; see
	// the note on Stop below.
	Stop(ctx context.Context, id string) error
	// Events follows the public event stream.
	Events(ctx context.Context, after uint64) (<-chan Frame, <-chan error, error)
	// BusHealth reports whether event production is live.
	BusHealth(ctx context.Context) (BusHealth, error)
	// Output collects a bounded response from one session.
	//
	// It exists so a review answer can be attributed to the session that actually
	// produced it. Without it the only source of an answer is a caller asserting
	// which session it is, and the trust boundary degrades to that assertion.
	Output(ctx context.Context, id string, maxTokens int) (Output, error)
}

// Output is one bounded response read from a session's own output.
type Output struct {
	// SessionID is the session the runtime actually queried. The control layer
	// stamps this onto an answer rather than trusting the caller to name it.
	SessionID string `json:"session_id"`
	// Model is transport-reported answer attribution, when available. Never
	// populate it from launch arguments, configuration or model-authored text.
	Model string `json:"model,omitempty"`
	// Text is the bounded response body.
	Text string `json:"text"`
	// Truncated records that the response exceeded the requested budget.
	Truncated bool `json:"truncated,omitempty"`
	// NativeTurn identifies a completed native response, never a working pane.
	NativeTurn string `json:"native_turn,omitempty"`
	// Evidence contains source tool records joined to that response's final bytes.
	// Tool text is untrusted; absence means unavailable, not verified work.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// DefaultOutputTokens bounds a collected response when the caller names none.
const DefaultOutputTokens = 8000

// BusHealth is the producer/enablement check.
//
// A disabled bus or a producer that is not running means the control layer will
// see nothing and will look perfectly healthy while doing nothing. That is the
// worst possible failure for a component whose entire job is to notice, so it is
// checked explicitly rather than inferred from an empty stream.
type BusHealth struct {
	Enabled bool              `json:"enabled"`
	Dir     string            `json:"dir,omitempty"`
	Cursor  uint64            `json:"cursor"`
	Frames  uint64            `json:"frames"`
	Kinds   map[string]uint64 `json:"kinds,omitempty"`
	Reasons []string          `json:"reasons,omitempty"`
}

// OK reports whether the bus can be relied on for observation.
func (h BusHealth) OK() bool { return h.Enabled }

// CLIRuntime is the production Runtime, backed by the agent-deck binary.
type CLIRuntime struct {
	// Binary is the agent-deck executable. Empty resolves it from PATH.
	Binary string
	// Profile scopes every session operation.
	Profile string
	// Timeout bounds one invocation.
	Timeout time.Duration
	// Log receives one line per invocation, for diagnostics.
	Log func(string)
}

// NewCLIRuntime builds a runtime for a profile.
func NewCLIRuntime(binary, profile string) *CLIRuntime {
	return &CLIRuntime{Binary: binary, Profile: profile, Timeout: DefaultTimeout}
}

func (r *CLIRuntime) binary() (string, error) {
	if strings.TrimSpace(r.Binary) != "" {
		return r.Binary, nil
	}
	path, err := exec.LookPath("agent-deck")
	if err != nil {
		return "", fmt.Errorf("agentdeck: agent-deck binary not found: %w", err)
	}
	return path, nil
}

// CommandError is a failed CLI invocation with its real exit status preserved.
type CommandError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "no stderr"
	}
	return fmt.Sprintf("agentdeck: %s failed (exit %d): %s", strings.Join(e.Args, " "), e.ExitCode, msg)
}

// NotFoundError reports that a named session or send does not exist.
//
// It is distinct from a generic failure because the correct response differs: a
// missing session is a state fact to reconcile, not a command to retry.
type NotFoundError struct{ What, ID string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("agentdeck: %s %q not found", e.What, e.ID)
}

// IsNotFound reports whether an error is a not-found fact.
func IsNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}

// run invokes the CLI and returns stdout.
//
// Output is captured with a bounded buffer rather than streamed, because every
// caller wants a parsed document. The buffer is bounded so a misbehaving command
// cannot exhaust memory; overflow is reported rather than silently truncated,
// because a truncated JSON document would decode into a plausible-looking empty
// result.
func (r *CLIRuntime) run(ctx context.Context, args ...string) (string, error) {
	bin, err := r.binary()
	if err != nil {
		return "", err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := args
	if strings.TrimSpace(r.Profile) != "" {
		full = append([]string{"-p", r.Profile}, args...)
	}
	if r.Log != nil {
		r.Log("run: " + strings.Join(full, " "))
	}
	cmd := exec.CommandContext(ctx, bin, full...)
	stdout := cappedCommandBuffer{limit: 16 << 20}
	stderr := cappedCommandBuffer{limit: 64 << 10}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A cancelled context must reach the child's process group, or an abandoned
	// session process keeps running after the supervisor has decided it stopped.
	cmd.WaitDelay = 5 * time.Second

	err = cmd.Run()
	if stdout.overflow || stderr.overflow {
		return "", fmt.Errorf("agentdeck: %s exceeded the CLI output bound", strings.Join(full, " "))
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), &CommandError{
				Args: full, ExitCode: ee.ExitCode(), Stderr: stderr.String(),
			}
		}
		return stdout.String(), fmt.Errorf("agentdeck: %s: %w", strings.Join(full, " "), err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return "", fmt.Errorf("agentdeck: %s produced no output", strings.Join(full, " "))
	}
	return stdout.String(), nil
}

// Create adds a session and returns its confirmed identity.
func (r *CLIRuntime) Create(ctx context.Context, spec CreateSpec) (Session, error) {
	if spec.NewBranch && spec.WorktreeBranch == "" {
		return Session{}, errors.New("agentdeck: new branch requires a worktree branch")
	}
	if strings.TrimSpace(spec.Path) == "" {
		return Session{}, errors.New("agentdeck: create requires an absolute project path")
	}
	if !strings.HasPrefix(spec.Path, "/") {
		return Session{}, fmt.Errorf("agentdeck: project path must be absolute, got %q", spec.Path)
	}
	args := spec.Args()
	out, err := r.run(ctx, args...)
	if err != nil {
		return Session{}, err
	}
	s, err := decodeSession(out)
	if err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(s.ID) == "" {
		// Creation that cannot be confirmed is worse than a reported failure: a
		// session may exist whose ID nobody recorded.
		return Session{}, fmt.Errorf("agentdeck: create returned no session id; the session may exist unconfirmed")
	}
	return s, nil
}

// Start begins a session's process.
func (r *CLIRuntime) Start(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("agentdeck: start requires a session id")
	}
	_, err := r.run(ctx, "session", "start", id)
	return err
}

// Inspect returns a session's current status.
//
// It uses the registry listing rather than re-parsing a pane, because Agent Deck
// owns session status and a second status parser is a second source of truth that
// can disagree with the first.
func (r *CLIRuntime) Inspect(ctx context.Context, id string) (Session, error) {
	out, err := r.run(ctx, "list", "--json")
	if err != nil {
		return Session{}, err
	}
	for _, s := range decodeSessionList(out) {
		if s.ID == id {
			return s, nil
		}
	}
	return Session{}, &NotFoundError{What: "session", ID: id}
}

// Send queues a message durably and returns its receipt.
//
// The queue is used rather than a bare send because the plan requires an
// interpretable receipt. `session send --queue` returns a server-generated
// send_id whose state can be reconciled through `session send-status`, so a
// supervisor can distinguish delivered from never-typed from unprovable. A bare
// send is immediate submission, not a durable queue, and offers none of that.
func (r *CLIRuntime) Send(ctx context.Context, id, message string) (Receipt, error) {
	if strings.TrimSpace(id) == "" {
		return Receipt{}, errors.New("agentdeck: send requires a session id")
	}
	if strings.TrimSpace(message) == "" {
		return Receipt{}, errors.New("agentdeck: send requires a message")
	}
	out, err := r.run(ctx, "session", "send", id, message, "--queue", "--json")
	if err != nil {
		return Receipt{}, err
	}
	rec, err := decodeReceipt(out)
	if err != nil {
		return Receipt{}, err
	}
	if strings.TrimSpace(rec.SendID) == "" {
		// Accepted into the command, but no id to reconcile against. The send may
		// still be in flight, so this is an ambiguity rather than a failure.
		return rec, fmt.Errorf("agentdeck: send returned no send_id; delivery cannot be reconciled")
	}
	return rec, nil
}

// SendStatus reconciles a queued send.
func (r *CLIRuntime) SendStatus(ctx context.Context, sendID string) (Receipt, error) {
	if strings.TrimSpace(sendID) == "" {
		return Receipt{}, errors.New("agentdeck: send-status requires a send id")
	}
	out, err := r.run(ctx, "session", "send-status", sendID, "--json")
	if err != nil {
		if ce := (*CommandError)(nil); errors.As(err, &ce) && ce.ExitCode == 2 {
			return Receipt{}, &NotFoundError{What: "send", ID: sendID}
		}
		return Receipt{}, err
	}
	return decodeReceipt(out)
}

// Stop ends a session's process.
//
// This is Agent Deck's public stop, which is asynchronous: it triggers tracked
// child cleanup but does not by itself acknowledge that the session has finished
// retiring. A supervisor that needs confirmed quiescence must observe it
// separately rather than assuming this call guarantees it.
func (r *CLIRuntime) Stop(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("agentdeck: stop requires a session id")
	}
	_, err := r.run(ctx, "session", "stop", id)
	return err
}

// RemoveRetired removes only an already-stopped qualification session. It uses
// the public guard and never --force; callers must first observe retirement.
func (r *CLIRuntime) RemoveRetired(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("agentdeck: remove requires a session id")
	}
	_, err := r.run(ctx, "session", "remove", id)
	return err
}

// Output collects a bounded response from a session's own output.
//
// This is the seam that makes a review answer attributable. The engine asks the
// RUNTIME which session produced a response, and stamps that identity itself, so
// the answer's source is a measured fact rather than something the submitting
// caller claimed.
//
// The read is bounded by an explicit token budget because the response is
// unbounded in principle: an agent can emit a very large last response, and an
// unbounded read into the control layer is a way to exhaust memory on the
// component that is supposed to be watching for trouble.
func (r *CLIRuntime) Output(ctx context.Context, id string, maxTokens int) (Output, error) {
	if strings.TrimSpace(id) == "" {
		return Output{}, errors.New("agentdeck: output requires a session id")
	}
	if maxTokens <= 0 {
		maxTokens = DefaultOutputTokens
	}
	out, err := r.run(ctx, "session", "output", id, "--json",
		"--max-tokens", strconv.Itoa(maxTokens))
	if err != nil {
		return Output{}, err
	}
	answer, err := decodeOutput(id, out, maxTokens)
	if err != nil {
		return Output{}, err
	}
	var native struct {
		SessionID      string `json:"session_id"`
		Tool           string `json:"tool"`
		Role           string `json:"role"`
		ConversationID string `json:"conversation_id"`
		Generation     string `json:"codex_turn_generation"`
	}
	if err := json.Unmarshal([]byte(out), &native); err != nil {
		return Output{}, err
	}
	if native.Tool != "codex" || native.Generation == "" {
		return answer, nil // A working pane is not a completed native turn.
	}
	// Selection is pinned separately. Recall is optional enrichment, not a gate
	// on an otherwise correlated answer; never substitute a session default.
	reportedModel := answer.Model
	answer.Model = ""
	thread, _, ok := strings.Cut(native.Generation, ":")
	if !ok || native.SessionID != id || native.Role != "assistant" || native.ConversationID != thread {
		return Output{}, errors.New("agentdeck: completed Codex output has inconsistent native identity")
	}
	answer.NativeTurn = native.Generation
	timeline, err := r.run(ctx, "recall", "timeline", thread, "--json", "--v1")
	var refusal struct {
		Code string `json:"code"`
	}
	// The pinned CLI can encode NOT_FOUND in a successful process exit.
	// Inspect its public result envelope as well as the exit status.
	if json.Unmarshal([]byte(timeline), &refusal) == nil && refusal.Code == "NOT_FOUND" {
		// v1 needs an index address. Use the public interactive search's bounded
		// refresh once for a new conversation; never enable Recall, backfill all
		// history or start another model turn as a side effect of reading output.
		if _, refreshErr := r.run(ctx, "recall", "search", thread, "--limit", "1", "--json"); refreshErr != nil {
			return answer, nil
		}
		timeline, err = r.run(ctx, "recall", "timeline", thread, "--json", "--v1")
	}
	if err != nil {
		return answer, nil
	}
	if evidence, err := codexTimelineEvidence(timeline, native.Generation, answer.Text); err == nil {
		answer.Evidence = evidence
	}
	model, err := codexTimelineModel(timeline, native.Generation, answer.Text)
	if err != nil {
		return answer, nil
	}
	if reportedModel != "" && reportedModel != model {
		return Output{}, errors.New("agentdeck: output model conflicts with its exact Codex turn context")
	}
	answer.Model = model
	return answer, nil
}

// decodeOutput reads the public `session output --json` envelope.
//
// A failure is returned as an error rather than as empty text, because "the
// session produced no readable response" and "the session said nothing" are
// different facts and only one of them is evidence.
func decodeOutput(id, out string, maxTokens int) (Output, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return Output{}, fmt.Errorf("agentdeck: session %s returned no output envelope", id)
	}
	var env struct {
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
		Success   *bool  `json:"success"`
		Code      string `json:"code"`
		Error     string `json:"error"`
		Text      string `json:"text"`
		Output    string `json:"output"`
		Content   string `json:"content"`
		Data      struct {
			Text    string `json:"text"`
			Output  string `json:"output"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return Output{}, fmt.Errorf("agentdeck: decoding output for session %s: %w", id, err)
	}
	if env.Success != nil && !*env.Success {
		code := env.Code
		if code == "" {
			code = "ERROR"
		}
		detail := env.Error
		if detail == "" {
			detail = "the runtime reported no output for this session"
		}
		return Output{}, fmt.Errorf("agentdeck: session %s produced no usable output (%s): %s", id, code, detail)
	}
	// The body is accepted from any of the shapes the public CLI has used, so a
	// future envelope change does not silently yield an empty-but-successful read.
	body := env.Text
	if body == "" {
		body = env.Output
	}
	if body == "" {
		body = env.Content
	}
	if body == "" {
		body = env.Data.Text
	}
	if body == "" {
		body = env.Data.Output
	}
	if body == "" {
		body = env.Data.Content
	}
	if env.SessionID != "" && env.SessionID != id {
		return Output{}, fmt.Errorf("agentdeck: output identified session %s when %s was queried", env.SessionID, id)
	}
	return Output{SessionID: id, Text: body, Model: env.Model}, nil
}

// List inventories the runtime's sessions.
//
// It exists so an ambiguous Create can be reconciled by activation label. Without
// it, a create whose outcome is unknown has two equally bad answers — assume it
// failed and create a duplicate, or assume it succeeded and proceed without an ID
// — and the first one produces two workers on a single-writer workspace.
func (r *CLIRuntime) List(ctx context.Context) ([]Session, error) {
	out, err := r.run(ctx, "list", "--json")
	if err != nil {
		return nil, err
	}
	sessions := decodeSessionList(out)
	if sessions == nil {
		// An inventory that could not be read is an ERROR, not an empty inventory.
		// Treating them the same is how an ambiguous create becomes a duplicate
		// worker, because "I could not see it" and "it is not there" have opposite
		// consequences.
		return nil, fmt.Errorf("agentdeck: the session inventory was not readable JSON: %s", out)
	}
	return sessions, nil
}

// BusHealth reports whether event production is live.
func (r *CLIRuntime) BusHealth(ctx context.Context) (BusHealth, error) {
	out, err := r.run(ctx, "events", "stats", "--json")
	if err != nil {
		return BusHealth{}, err
	}
	return decodeBusHealth(out)
}

// --- decoding -------------------------------------------------------------

// decodeSession reads a session object, tolerating a wrapped or bare array.
func decodeSession(out string) (Session, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return Session{}, errors.New("agentdeck: empty session payload")
	}
	if strings.HasPrefix(trimmed, "[") {
		list := decodeSessionList(trimmed)
		if len(list) == 0 {
			return Session{}, errors.New("agentdeck: session array was empty or undecodable")
		}
		return list[0], nil
	}
	var wrapper struct {
		Session  *Session  `json:"session"`
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil {
		if wrapper.Session != nil && wrapper.Session.ID != "" {
			return *wrapper.Session, nil
		}
		if len(wrapper.Sessions) > 0 {
			return wrapper.Sessions[0], nil
		}
	}
	var s Session
	if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
		return Session{}, fmt.Errorf("agentdeck: decoding session: %w", err)
	}
	return s, nil
}

func decodeSessionList(out string) []Session {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var wrapper struct {
			Sessions []Session `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && len(wrapper.Sessions) > 0 {
			return wrapper.Sessions
		}
	}
	var list []Session
	if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
		return nil
	}
	return list
}

// decodeReceipt reads a send record.
//
// The queue's output nests its record under a "data" envelope, but the field set
// is also accepted flat, because the envelope is a CLI presentation detail and a
// control plane should not break if that presentation changes.
func decodeReceipt(out string) (Receipt, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return Receipt{}, errors.New("agentdeck: empty send payload")
	}
	var envelope struct {
		Data  *Receipt `json:"data"`
		Send  *Receipt `json:"send"`
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err == nil {
		if envelope.Data != nil && envelope.Data.SendID != "" {
			return *envelope.Data, nil
		}
		if envelope.Send != nil && envelope.Send.SendID != "" {
			return *envelope.Send, nil
		}
	}
	var rec Receipt
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		return Receipt{}, fmt.Errorf("agentdeck: decoding send receipt: %w", err)
	}
	return rec, nil
}

func decodeBusHealth(out string) (BusHealth, error) {
	var h BusHealth
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &h); err != nil {
		return BusHealth{}, fmt.Errorf("agentdeck: decoding bus stats: %w", err)
	}
	return h, nil
}

// Classify converts a receipt into an outcome the dispatcher can act on.
//
// The mapping is the whole point of using a queued send: `landed` is proof,
// `failed` means nothing was typed, and a settled-but-not-landed record is the
// ambiguous middle that a supervisor must refuse to resolve on its own.
func Classify(rec Receipt) SendOutcome {
	switch strings.ToLower(strings.TrimSpace(rec.State)) {
	case "landed":
		return SendLanded
	case "failed":
		return SendFailed
	case "queued", "typing", "typed", "submitted":
		if rec.Settled {
			// The queue gave up proving it and will never type again. The message
			// may or may not have reached the pane.
			return SendAmbiguous
		}
		return SendInFlight
	case "":
		if rec.SendID == "" {
			return SendFailed
		}
		return SendInFlight
	default:
		// An unrecognised state is not a success. Treating it as one would let a
		// changed queue vocabulary silently become a delivery guarantee.
		return SendAmbiguous
	}
}

// Ensure the runtime satisfies the interface at compile time.
var _ Runtime = (*CLIRuntime)(nil)

// ensure os is referenced for environments where Log is unused.
var _ = os.Getenv

// Args builds only the public Agent Deck add command.
func (s CreateSpec) Args() []string {
	args := []string{"add", s.Path, "--json"}
	if s.Title != "" {
		args = append(args, "--title", s.Title)
	}
	if s.Tool != "" {
		// The tool is an operator-chosen Agent Deck tool name. The control layer
		// never assembles a provider command line.
		args = append(args, "-c", s.Tool)
	}
	if s.NoParent {
		args = append(args, "--no-parent")
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}
	if s.Sandbox {
		args = append(args, "--sandbox")
	}
	if s.WorktreeBranch != "" {
		args = append(args, "--worktree", s.WorktreeBranch)
		if s.NewBranch {
			args = append(args, "--new-branch")
		}
	}
	if s.Wrapper != "" {
		args = append(args, "--wrapper", s.Wrapper)
	}
	return args
}
