// Package runtime is the narrow, backend-neutral session boundary.
//
// It exists because there are now two real transports — the pinned Agent Deck CLI
// and, later, a native ACP process — and because the alternative was for the
// control layer to name Agent Deck types directly. That alternative is what this
// package removes. The consequence worth stating plainly: `control` stops
// depending on `agentdeck`, so adding a transport becomes an addition here rather
// than an edit through the engineering authority.
//
// Three properties are deliberate and are the reason the interface is this
// small.
//
// Capabilities are returned as DATA, not discovered by interface assertion. A
// backend that cannot list its sessions says so, and the caller reconciles
// accordingly. Asserting for an optional interface turns "this backend cannot do
// that" into a nil-pointer at the moment it matters, which is usually inside a
// recovery path.
//
// Sessions are referred to by a Backend-qualified reference, not by a bare ID.
// The same ID string could be produced by two different backends, and a
// reference that cannot say which backend minted it is a reference that can be
// routed to the wrong process.
//
// This package owns no decisions. It cannot accept an obligation, judge an
// outcome, or publish a result. Store remains the single decision owner and the
// Engine remains the review policy; a backend that could approve work would make
// every boundary in this system decorative.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"
)

// ErrUnsupported is returned when a backend is asked for a capability it does not
// advertise.
//
// It is a distinct type on purpose: a caller can tell "this transport cannot do
// that" apart from "the transport tried and failed", and the first is a design
// fact while the second is an incident.
var ErrUnsupported = errors.New("runtime: the backend does not support this operation")

// ErrUnknownSession is returned when a reference names no live session.
//
// It is never interchangeable with success. A backend that cannot find a session
// must say so, because "I could not find it" and "it is fine" are the two
// answers whose confusion produces duplicate work.
var ErrUnknownSession = errors.New("runtime: no live session matches the reference")

// Capabilities is what a backend can actually do.
//
// Every field is an explicit answer. There is no "assume yes" default, because
// every such default is a guess made on the recovery path, where guesses are
// least affordable.
type Capabilities struct {
	// Inventory reports whether Activate-created sessions can be LISTED, which is
	// what makes reconciliation-by-label possible.
	Inventory bool `json:"inventory"`
	// Output reports whether bounded session output can be read.
	Output bool `json:"output"`
	// Receipts reports whether a queued send can be reconciled by send ID.
	Receipts bool `json:"receipts"`
	// Resume reports whether a prior session can be reloaded.
	Resume bool `json:"resume"`
	// Close reports whether a session can be closed gracefully.
	Close bool `json:"close"`
	// DurableQueue reports whether a submission this backend accepted is held
	// durably somewhere that survives this process, so that a later run can
	// ask about it instead of resending it.
	//
	// This decides whether an UNKNOWN outcome is safe to retry, and it is a
	// property of the transport rather than of any one effect. A backend
	// without a durable queue (a direct JSON-RPC peer, say) has no record of a
	// prompt once the process that wrote it dies, so resending after a crash can
	// duplicate work. That case must block as UNKNOWN instead.
	//
	// It is false by default on purpose: a backend that forgets to declare it
	// gets the conservative answer.
	DurableQueue bool `json:"durable_queue"`
}

// SessionRef identifies one session unambiguously.
//
// Backend is part of the identity rather than a debugging label, because a bare
// ID is only unique within the backend that minted it.
type SessionRef struct {
	Backend string `json:"backend"`
	// ID is the backend's own session identity.
	ID string `json:"id"`
	// Label is the deterministic activation label, when the backend supports one.
	// It is what makes an ambiguous create reconcilable.
	Label string `json:"label,omitempty"`
	// Generation is the worker generation this session belongs to.
	Generation int `json:"generation,omitempty"`
	// Epoch is the launch epoch, so a result cannot be replayed into a later one.
	Epoch int `json:"epoch,omitempty"`
}

// String renders a reference for operator-facing output.
func (r SessionRef) String() string {
	if r.Label != "" {
		return r.Backend + ":" + r.Label + " (" + r.ID + ")"
	}
	return r.Backend + ":" + r.ID
}

// Session is the observed state of one session.
type Session struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Path   string `json:"path,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Status string `json:"status,omitempty"`
}

// Outcome is a backend's own verdict on one submitted unit of work, restated in
// the only four answers every transport is able to give.
//
// It is NOT an engineering result. It reports what the TRANSPORT observed while
// delivering a message; what that message accomplished is decided elsewhere and
// never inferred from this.
//
// The vocabulary lives here rather than in each adapter so a consumer can switch
// on it, but the MAPPING does not: each adapter classifies its own transport's
// words, because only the adapter knows what those words mean. Runtime defines
// the shape of the answer and refuses to supply any of them.
type Outcome string

const (
	// OutcomeUnstated means the backend returned a receipt without classifying
	// it.
	//
	// It is a distinct value rather than a zero that could alias a real answer,
	// because "nobody said" and "it landed" are the two statements a supervisor
	// must never confuse.
	OutcomeUnstated Outcome = ""
	// OutcomeLanded is the only positive proof: the text reached its recipient.
	OutcomeLanded Outcome = "landed"
	// OutcomeFailed is a KNOWN failure, so retrying is safe.
	OutcomeFailed Outcome = "failed"
	// OutcomeInFlight means the transport still owns the message. It must not
	// also be sent directly; that is how an agent receives one instruction twice.
	OutcomeInFlight Outcome = "in_flight"
	// OutcomeAmbiguous means the transport settled the record without evidence
	// that the message was received and will not report again.
	//
	// This is the case that must block. It is not a failure and not a success,
	// and it is the reason this vocabulary has a value for it at all.
	OutcomeAmbiguous Outcome = "ambiguous"
)

// Receipt is the outcome of submitting work.
//
// State is deliberately a string rather than an enum: backends publish their own
// vocabulary, and mapping it here would mean this package decides what a
// particular backend's word means.
type Receipt struct {
	SendID    string `json:"send_id"`
	State     string `json:"state"`
	Settled   bool   `json:"settled"`
	SessionID string `json:"session_id,omitempty"`
	// Outcome is the adapter's classification of this receipt.
	//
	// The adapter fills it, because the adapter is the only component that knows
	// its transport's vocabulary. An unset Outcome means the receipt was not
	// classified, which is NOT the same as delivered.
	Outcome Outcome `json:"outcome,omitempty"`
	// Reason is the transport's own explanation when it reports a failure.
	//
	// It is preserved rather than discarded because a failure an operator cannot
	// read is a failure that gets retried on a guess.
	Reason string `json:"reason,omitempty"`
}

// Event is one observation from a backend's stream.
type Event struct {
	// Backend identifies which transport produced this observation.
	//
	// It is here rather than inferred, because two backends number cursors from
	// different origins. Without the tag, a consumer CAN merge an Agent Deck
	// cursor with an ACP connection ordinal and suppress or replay the wrong
	// frames — a correctness bug that presents as a timing bug.
	Backend string `json:"backend"`
	// WorkerGeneration is the generation the observation was made under.
	//
	// It travels on the event rather than being inferred from the session,
	// because a record can outlive the binding it was observed under, and a
	// generation reconstructed after the fact would be the wrong one.
	WorkerGeneration int `json:"worker_generation,omitempty"`
	// Kind is the backend's own event kind.
	Kind string `json:"kind"`
	// SessionID is the session the observation belongs to, when known.
	SessionID string `json:"session_id,omitempty"`
	// Cursor is the backend's own position. It is deliberately NOT comparable
	// across backends: two backends numbering from different origins must never
	// be compared or merged into one position.
	// Zero on KindPromptResult denotes a separately correlated receipt with no
	// notification position; it must not advance notification continuity.
	Cursor uint64 `json:"cursor"`
	// Payload is bounded, backend-specific JSON.
	Payload []byte `json:"payload,omitempty"`
}

// KindPromptResult is the observation kind for a CORRELATED prompt receipt.
//
// It exists because a completed turn does not arrive as a notification. A
// JSON-RPC response is delivered to the caller that asked for it, so a backend
// that returned only notifications to its observation stream would never publish
// the one event that means "the turn ended" — and the supervisor would have no
// production route to completion at all.
const KindPromptResult = "prompt/result"

// Health reports whether a backend can currently be relied on for observation.
type Health struct {
	// Available is false when the backend cannot be observed at all.
	Available bool `json:"available"`
	// Reasons are machine-readable cause codes, not prose.
	Reasons []string `json:"reasons,omitempty"`
	// Cursor is the backend's latest observed position, when meaningful.
	Cursor uint64 `json:"cursor"`
}

// ActivateSpec is what a caller asks a backend to create.
type ActivateSpec struct {
	// Label is the deterministic activation label. It is REQUIRED: a create whose
	// outcome is unknown must be reconcilable, and a random label makes it
	// unreconcilable.
	Label string
	// Path is the workspace the session runs in.
	Path string
	// Tool is the backend's own tool identifier.
	Tool string
	// Title is display metadata and is never used for routing.
	Title string
}

// Backend is the narrow session boundary.
//
// Every method is one capability, and a backend that cannot provide one says so in
// Capabilities rather than failing at the call site.
type Backend interface {
	// Name identifies the transport for operator output and provenance.
	Name() string

	// Capabilities reports what this backend can do. It must be cheap and must not
	// perform effects.
	Capabilities() Capabilities

	// Activate creates a session and returns its confirmed reference.
	Activate(ctx context.Context, spec ActivateSpec) (SessionRef, error)

	// Reconcile looks for an existing session carrying the activation label.
	//
	// It reports found=false only when the label is DEFINITELY absent, and an
	// ERROR when it cannot tell — because "cannot tell" must never be treated as
	// "absent", which is how a second worker gets created.
	//
	// priorQuiesced carries the one fact a backend cannot establish for itself:
	// whether a process from an EARLIER run has been proven gone. A backend that
	// owns one process can only see its own sessions, so without this it must
	// refuse rather than answer "absent" — an orphaned sibling may still hold the
	// label. With it, absence is a real answer and recovery becomes possible.
	Reconcile(ctx context.Context, label string, priorQuiesced bool) (ref SessionRef, found bool, err error)

	// Inspect returns a session's observed state.
	Inspect(ctx context.Context, ref SessionRef) (Session, error)

	// Submit hands work to a session and returns its receipt.
	Submit(ctx context.Context, ref SessionRef, message string) (Receipt, error)

	// Receipt reconciles a previously submitted unit of work by send ID.
	//
	// An implementation that can classify its transport's response MUST set
	// Outcome. One that cannot leaves it OutcomeUnstated, which consumers treat
	// as "not proven" — never as success.
	Receipt(ctx context.Context, sendID string) (Receipt, error)

	// Output reads a bounded response from a session.
	//
	// It returns truncated=true when the response exceeded the bound, so a caller
	// never mistakes a cut-off transcript for a complete one.
	Output(ctx context.Context, ref SessionRef, maxTokens int) (text string, truncated bool, err error)

	// Shutdown ends a session. It is a REQUEST; a backend that cannot prove the
	// session stopped must not report success.
	Shutdown(ctx context.Context, ref SessionRef) error

	// Events follows the backend's observation stream from a position.
	//
	// The returned channels are closed when the stream ends. A caller must treat
	// that as "no longer observable", never as "finished".
	Events(ctx context.Context, after uint64) (<-chan Event, <-chan error, error)

	// Health reports observation availability.
	Health(ctx context.Context) (Health, error)

	// ProcessIdentity reports the OS process this backend owns, when it owns one.
	//
	// owned=false means the backend runs no child of its own — Agent Deck's
	// sessions are supervised elsewhere — and is an explicit answer, not a gap.
	// The caller uses it to prove an earlier run's process is gone before it
	// creates anything, because a session id cannot say whether the thing that
	// made it is still running.
	ProcessIdentity() (pid int, owned bool)
}

// OutputBudgetDefault bounds a collected response when the caller names none.
const OutputBudgetDefault = 8000

// FileSHA256 measures a file's digest for a transport pin.
//
// It lives here because every transport needs to prove the executable it is about
// to run is the one that was reviewed, and two copies of a digest check is two
// places for them to drift.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ClampOutputBudget keeps a caller-supplied bound inside positive limits.
//
// A zero or negative budget would mean "unbounded" in a naive implementation, and
// unbounded external output read into a supervisor is a way to exhaust the
// component that is supposed to be watching for trouble.
func ClampOutputBudget(n int) int {
	if n <= 0 {
		return OutputBudgetDefault
	}
	const max = 200000
	if n > max {
		return max
	}
	return n
}

// Deadline returns a bounded deadline for one backend call.
//
// It exists so every transport call is bounded the same way; an unbounded call in
// a supervision path is an outage waiting for a slow runtime.
func Deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 30 * time.Second
	}
	return context.WithTimeout(parent, d)
}
