package control

// Action dispatch and receipt reconciliation.
//
// The requirement this file exists to satisfy: a real recipient receives one
// instruction, promptly, and an uncertain outcome is visible rather than
// guessed at.
//
// Both halves of that are easy to get wrong independently. Sending twice is
// solved by having exactly one component that sends, which is what the previous
// embedded draft got wrong and what EC2 relocates. Sending late is solved by
// dispatching during the live follow rather than on the way out, which is the
// other half of the same defect. And the honest answer to "did that message
// arrive?" after a crash is not a retry — it is a receipt, which Agent Deck's
// queued send already provides.
//
// Dispatch therefore commits intent before any external call, records the
// send_id the queue returns, and reconciles that id rather than assuming
// success. An effect that is still in flight when the process dies is reconciled
// on the next start. An effect whose outcome cannot be proven is blocked, because
// resending it would tell an agent to do the same thing twice.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/runtime"
)

// EffectStatus is how far one durable effect got.
type EffectStatus string

const (
	// EffectPending means the intent is committed and no external call has been
	// attempted. This is the only state that is unconditionally safe to retry.
	EffectPending EffectStatus = "PENDING"
	// EffectDispatching means the external call has begun. From here the outcome
	// is unknown until a receipt is recorded, which is exactly what makes a crash
	// at this point ambiguous rather than merely unfinished.
	EffectDispatching EffectStatus = "DISPATCHING"
	// EffectDelivered means a receipt proved the text reached the recipient.
	EffectDelivered EffectStatus = "DELIVERED"
	// EffectFailed means the transport reported a known failure, so retrying is
	// safe.
	EffectFailed EffectStatus = "FAILED"
	// EffectBlocked means the outcome cannot be determined and a human or an
	// explicit reconcile must resolve it.
	EffectBlocked EffectStatus = "BLOCKED"
)

// MaxEffects bounds the durable intent ledger.
const MaxEffects = 32

// Effect is one durable instruction owed to a session.
type Effect struct {
	// EffectID is derived from the decision that created it, so re-deriving the
	// same decision from the same state produces the same identity and cannot
	// become a second, indistinguishable instruction.
	EffectID string `json:"effect_id"`
	// SessionID is the full recipient session ID. Titles are never used: a title
	// can be reused and a full ID cannot.
	SessionID string `json:"session_id"`
	// Role is "implementer" or "auditor", recorded so a reviewer can see which
	// side of the separation an instruction went to.
	Role string `json:"role"`
	// Message is the instruction text.
	Message string `json:"message"`
	// SelectedModel is the admission-pinned selection, not answer attribution.
	SelectedModel      string `json:"selected_model,omitempty"`
	RequestedModelEcho string `json:"requested_model_echo,omitempty"`
	// Reason explains the decision, for logs and audit trails.
	Reason string `json:"reason,omitempty"`
	// Status is how far it got.
	Status EffectStatus `json:"status"`
	// SendID is Agent Deck's queue identifier, recorded so the outcome can be
	// reconciled rather than assumed.
	SendID string `json:"send_id,omitempty"`
	// Revision is the state revision the effect was decided at.
	Revision  int    `json:"revision"`
	CreatedAt string `json:"created_at"`
	// LastError explains a failure or block.
	LastError string `json:"last_error,omitempty"`
}

// Unresolved reports whether an effect's outcome is still unknown.
func (e Effect) Unresolved() bool {
	return e.Status == EffectPending || e.Status == EffectDispatching
}

// Dispatcher performs effects through the Agent Deck runtime.
type Dispatcher struct {
	store   *Store
	runtime agentdeck.Runtime
	now     func() time.Time
	// backends maps a backend name to its implementation. An effect is routed by
	// the backend recorded for its SESSION, never by whichever transport this
	// dispatcher was constructed with.
	backends map[string]runtime.Backend
	// DryRun replaces the transport with a printer. The decision and the state
	// machine still run in full, because a dry run that skips the policy is not
	// a dry run of anything.
	DryRun bool
	// Log receives one line per performed effect.
	Log func(string)
}

// NewDispatcher builds a dispatcher bound to one request.
//
// The Agent Deck adapter is always present and is the legacy default, so a
// construction site that names no other backend behaves exactly as before.
func NewDispatcher(store *Store, rt agentdeck.Runtime) *Dispatcher {
	d := &Dispatcher{store: store, runtime: rt, now: time.Now, backends: map[string]runtime.Backend{}}
	if rt != nil {
		d.backends[agentdeck.BackendName] = agentdeck.NewBackend(rt)
	}
	return d
}

// WithBackend registers an additional transport for routing.
func (d *Dispatcher) WithBackend(b runtime.Backend) *Dispatcher {
	if b != nil {
		if d.backends == nil {
			d.backends = map[string]runtime.Backend{}
		}
		d.backends[b.Name()] = b
	}
	return d
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(fmt.Sprintf(format, args...))
	}
}

// RecordEffect commits a new effect inside a decision transaction.
//
// It is called from within the same state transaction as the decision that
// justified it, so an effect and its justification are committed together. An
// effect without a decision is a phantom; a decision without its effect is a
// lost instruction, and the worker is never told what to do.
func (s *State) RecordEffect(e Effect) error {
	// Include the inspection request in the durable instruction before dispatch.
	// It shares the task's turn and never creates another session or audit.
	model := ""
	for role, binding := range s.BackendBindings {
		if binding.SessionID != e.SessionID {
			continue
		}
		candidate := s.pinnedModelFor(role)
		if model != "" && candidate != "" && model != candidate {
			return fmt.Errorf("session %s has conflicting pinned models", e.SessionID)
		}
		if candidate != "" {
			model = candidate
		}
	}
	if model != "" && e.RequestedModelEcho == "" {
		e.SelectedModel = model
		e.RequestedModelEcho = modelEchoLabel(model)
		e.Message += fmt.Sprintf("\nModel selection check: reply with %q. For a JSON answer, put this literal in the optional model_echo field of the outer response envelope; for a prose answer, put it on the first line. This echoes the operator's selection, not independently measured model identity. Keep the required answer format and task correlation.\n", e.RequestedModelEcho)
	}
	// A re-decided event must not become a second instruction.
	for _, existing := range s.Effects {
		if existing.Unresolved() && existing.SessionID == e.SessionID && existing.Message == e.Message {
			return nil
		}
	}
	if e.EffectID == "" {
		e.EffectID = EffectID(s.Revision, e.SessionID, e.Role, e.Message)
	}
	e.Status = EffectPending
	e.Revision = s.Revision
	s.Effects = append(s.Effects, e)
	// The bound is backpressure, not garbage collection: only resolved entries
	// are eligible for removal, so a request that is failing to confirm
	// deliveries keeps every record of what it is unsure about.
	if len(s.Effects) > MaxEffects {
		kept := make([]Effect, 0, MaxEffects)
		drop := len(s.Effects) - MaxEffects
		for _, existing := range s.Effects {
			if drop > 0 && !existing.Unresolved() {
				drop--
				continue
			}
			kept = append(kept, existing)
		}
		s.Effects = kept
	}
	return nil
}

func modelEchoLabel(model string) string {
	if strings.HasPrefix(model, "m:minimax:") {
		model = strings.TrimPrefix(model, "m:minimax:")
		model, _, _ = strings.Cut(model, ":v:")
	} else if _, last, ok := strings.Cut(model, "/"); ok {
		model = last
	}
	model, _, _ = strings.Cut(model, "#")
	return strings.TrimPrefix(model, "MiniMax-")
}

// EffectID derives a deterministic effect identity.
func EffectID(revision int, sessionID, role, message string) string {
	// FNV-1a over the message keeps the label short while still changing when
	// the instruction changes. This is an identity label, not a security
	// boundary; the revision is what separates decisions.
	var h uint32 = 2166136261
	for i := 0; i < len(message); i++ {
		h ^= uint32(message[i])
		h *= 16777619
	}
	return fmt.Sprintf("E%d-%s-%s-%08x", revision, role, sessionID, h)
}

// OwedEffects returns effects that may still be performed, oldest first.
//
// Order matters: an older instruction describes an earlier obligation, and
// performing a later one first can tell an agent to proceed while the message
// that would have stopped it is still queued behind it.
func (s *State) OwedEffects() []Effect {
	var out []Effect
	for _, e := range s.Effects {
		if e.Unresolved() {
			out = append(out, e)
		}
	}
	return out
}

// UnresolvedEffects returns every effect whose outcome is unknown.
func (s *State) UnresolvedEffects() []Effect {
	var out []Effect
	for _, e := range s.Effects {
		if e.Unresolved() {
			out = append(out, e)
		}
	}
	return out
}

// EffectByID returns a recorded effect.
func (s *State) EffectByID(id string) (Effect, bool) {
	for _, e := range s.Effects {
		if e.EffectID == id {
			return e, true
		}
	}
	return Effect{}, false
}

// ErrEffectLedgerFull is returned when new intent is requested while unresolved
// effects already fill the ledger.
var ErrEffectLedgerFull = errors.New("agent-engine: effect ledger is full of unresolved instructions")

// DispatchResult summarises one drain.
type DispatchResult struct {
	Delivered int
	Failed    int
	InFlight  int
	Blocked   int
	Errors    []string
}

// Drain performs every owed effect, oldest first, reconciling as it goes.
//
// Per effect: claim it (committed before any external call), send through the
// durable queue, then record the receipt's outcome. A claim is never blindly
// retried, because after a crash the send may have happened.
func (d *Dispatcher) Drain(ctx context.Context, requestID string) (DispatchResult, error) {
	var res DispatchResult
	if _, err := d.Reconcile(ctx, requestID); err != nil {
		return res, err
	}
	st, err := d.store.State(requestID)
	if err != nil {
		return res, err
	}
	if st.Episode != nil && st.Episode.Stop != nil {
		// A policy-stop marker is committed before the terminal result is
		// published. It already withdraws authority to deliver any new episode
		// instruction, including an effect that was queued earlier.
		return res, nil
	}
	owed := st.OwedEffects()
	if len(owed) >= MaxEffects {
		return res, fmt.Errorf("%w: %d unresolved instructions; reconcile them before dispatching more",
			ErrEffectLedgerFull, len(owed))
	}

	for _, owed0 := range owed {
		if owed0.Status == EffectDispatching {
			res.InFlight++
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		claimed, err := d.claim(requestID, owed0.EffectID)
		if err != nil {
			if errors.Is(err, errEffectGone) {
				continue
			}
			return res, err
		}
		if claimed.Role == string(RoleWorker) || claimed.Role == "implementer" {
			current, err := d.store.State(requestID)
			if err != nil {
				return res, err
			}
			if err := current.VerifyRepairBranch(); err != nil {
				_ = d.acknowledge(requestID, claimed, EffectBlocked, err.Error())
				res.Blocked++
				continue
			}
		}
		if d.DryRun {
			d.logf("[dry-run] would send %s to %s: %.80s", claimed.Role, claimed.SessionID, claimed.Message)
			_ = d.acknowledge(requestID, claimed, EffectFailed, "dry run: no transport attempted")
			res.Failed++
			continue
		}

		d.logf("dispatch %s (%s -> %s)", claimed.EffectID, claimed.Role, claimed.SessionID)

		// Route through the BACKEND that owns this session, not through whichever
		// transport the engine happens to hold. An effect carries a session id,
		// and that id is only unique inside its backend; matching by string alone
		// would send a stale effect to a transport that merely shares the name.
		backend, sref, berr := d.backendFor(requestID, claimed.SessionID)
		if berr != nil {
			// Not a routing failure. The outcome is unknown, so the effect blocks
			// rather than being retried into the wrong transport.
			_ = d.acknowledge(requestID, claimed, EffectBlocked, berr.Error())
			res.Blocked++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", claimed.EffectID, berr))
			continue
		}
		latest, err := d.store.State(requestID)
		if err != nil {
			return res, err
		}
		if latest.Episode != nil && latest.Episode.Stop != nil {
			_ = d.acknowledge(requestID, claimed, EffectBlocked, "dispatch withheld by the episode policy stop: "+latest.Episode.Stop.Reason)
			res.Blocked++
			continue
		}
		if claimed.Role == string(RoleWorker) && latest.EpisodeWorkerRelease != nil {
			_ = d.acknowledge(requestID, claimed, EffectBlocked, "delivery was withheld before transport submission because a worker release is recorded")
			res.Blocked++
			continue
		}
		rec, sendErr := backend.Submit(ctx, sref, claimed.Message)
		switch {
		case sendErr != nil && rec.SendID == "":
			// No id to reconcile against, so the outcome is unknown even though
			// the call reported a problem. That is an ambiguity, not a failure.
			_ = d.acknowledge(requestID, claimed, EffectBlocked, "send failed without a receipt: "+sendErr.Error())
			res.Blocked++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", claimed.EffectID, sendErr))
		case sendErr != nil:
			// The queue accepted it and gave an id, so the effect can be
			// reconciled later. It is in flight, not failed.
			_ = d.recordSendID(requestID, claimed, rec.SendID)
			res.InFlight++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", claimed.EffectID, sendErr))
		default:
			_ = d.recordSendID(requestID, claimed, rec.SendID)
			switch rec.Outcome {
			case runtime.OutcomeLanded:
				_ = d.acknowledge(requestID, claimed, EffectDelivered, "")
				res.Delivered++
			case runtime.OutcomeFailed:
				_ = d.acknowledge(requestID, claimed, EffectFailed, receiptReason(rec))
				res.Failed++
			case runtime.OutcomeAmbiguous:
				_ = d.acknowledge(requestID, claimed, EffectBlocked,
					"the queue settled the message without transcript evidence; delivery cannot be proven")
				res.Blocked++
			default:
				// In flight, or a backend that classified nothing. Neither is a
				// delivery, so the effect stays unresolved and is reconcilable.
				// An unclassified receipt is deliberately not read as success.
				res.InFlight++
			}
		}
	}
	return res, nil
}

var errEffectGone = errors.New("agent-engine: effect is no longer recorded")

// backendFor resolves the transport that owns a session.
//
// The request is passed in rather than remembered, because it is already an
// argument to every caller and a stored copy is a second source of truth that
// can disagree with the request actually being served. A disagreement there does
// not fail loudly; it routes against some other request's bindings.
//
// An unbound session is REFUSED rather than sent to a default transport. Sending
// it somewhere plausible is how a message meant for one worker reaches another.
func (d *Dispatcher) backendFor(requestID, sessionID string) (runtime.Backend, runtime.SessionRef, error) {
	st, err := d.store.State(requestID)
	if err != nil {
		return nil, runtime.SessionRef{}, err
	}
	binding, ok := st.BackendForID(sessionID)
	if !ok {
		return nil, runtime.SessionRef{}, fmt.Errorf(
			"agent-engine: session %s has no recorded backend binding, so the effect cannot be routed", sessionID)
	}
	backend, ok := d.backends[binding.Backend]
	if !ok {
		return nil, runtime.SessionRef{}, fmt.Errorf(
			"agent-engine: session %s belongs to backend %q, which this engine does not have",
			sessionID, binding.Backend)
	}
	return backend, runtime.SessionRef{
		Backend: binding.Backend, ID: binding.SessionID, Label: binding.Label,
		Generation: binding.Generation, Epoch: binding.Epoch,
	}, nil
}

// receiptReason renders a failure reason an operator can act on.
//
// A backend that reports a failure without explaining it still gets the raw
// state recorded, because a durable record of "failed" with no evidence is a
// record that invites a retry on a guess.
func receiptReason(rec runtime.Receipt) string {
	if rec.Reason != "" {
		return rec.Reason
	}
	if rec.State != "" {
		return "the transport reported state " + rec.State
	}
	return "the transport reported a failure with no state and no reason"
}

// claim marks an effect DISPATCHING under the state lock.
func (d *Dispatcher) claim(requestID, id string) (Effect, error) {
	var out Effect
	_, err := d.store.UpdateCommitted(requestID, func(s *State) error {
		if s.Stage.Terminal() || s.CancellationRequested || s.RuntimeBlock != nil || (s.Episode != nil && s.Episode.Stop != nil) {
			return errEffectGone
		}
		e, ok := s.EffectByID(id)
		if !ok {
			return errEffectGone
		}
		if e.Status != EffectPending {
			return errEffectGone
		}
		if e.Role == string(RoleWorker) && s.EpisodeWorkerRelease != nil {
			return errEffectGone
		}
		for i := range s.Effects {
			if s.Effects[i].EffectID != id {
				continue
			}
			s.Effects[i].Status = EffectDispatching
			s.Effects[i].LastError = ""
			out = s.Effects[i]
			return nil
		}
		return errEffectGone
	})
	if err != nil {
		return Effect{}, err
	}
	return out, nil
}

func (d *Dispatcher) recordSendID(requestID string, claimed Effect, sendID string) error {
	_, err := d.store.UpdateCommitted(requestID, func(s *State) error {
		for i := range s.Effects {
			if s.Effects[i].EffectID != claimed.EffectID {
				continue
			}
			s.Effects[i].SendID = sendID
			return nil
		}
		return errEffectGone
	})
	return err
}

func (d *Dispatcher) acknowledge(requestID string, claimed Effect, status EffectStatus, msg string) error {
	_, err := d.store.UpdateCommitted(requestID, func(s *State) error {
		for i := range s.Effects {
			if s.Effects[i].EffectID != claimed.EffectID {
				continue
			}
			if !s.Effects[i].Unresolved() && s.Effects[i].Status != status {
				// Somebody already resolved it. Rewriting a settled outcome would
				// destroy the only record of what was known.
				return fmt.Errorf("agent-engine: effect %s is already %s", claimed.EffectID, s.Effects[i].Status)
			}
			s.Effects[i].Status = status
			s.Effects[i].LastError = msg
			return nil
		}
		return errEffectGone
	})
	return err
}

// Reconcile resolves effects left in flight by a previous process.
//
// It never resends. For each unresolved effect with a recorded send id, the
// queue's own status interface is asked what happened. A landed message becomes
// delivered; a known failure becomes failed so it may be retried deliberately; a
// settled-but-unproven message becomes blocked, because the queue itself has
// given up proving it and resending would risk delivering the instruction twice.
func (d *Dispatcher) Reconcile(ctx context.Context, requestID string) ([]Effect, error) {
	st, err := d.store.State(requestID)
	if err != nil {
		return nil, err
	}
	var stillUnresolved []Effect
	for _, e := range st.Effects {
		if !e.Unresolved() {
			continue
		}
		if e.Status == EffectPending {
			// Intent committed without an external attempt is still safe to send.
			continue
		}
		if e.SendID == "" {
			// No receipt was recorded, so nothing is known about the outcome.
			// A durable queue cannot recover an unknown submission without a
			// correlation key. Both transport kinds must refuse blind replay.
			backend, _, berr := d.backendFor(requestID, e.SessionID)
			if berr != nil {
				_ = d.acknowledge(requestID, e, EffectBlocked, berr.Error())
				stillUnresolved = append(stillUnresolved, e)
				continue
			}
			if !backend.Capabilities().DurableQueue {
				_ = d.acknowledge(requestID, e, EffectBlocked,
					"the instruction was committed to "+backend.Name()+" but no receipt was recorded, and "+
						"that transport retains nothing to reconcile against; the outcome is durably UNKNOWN "+
						"and the instruction must not be sent again automatically")
				d.logf("reconciled %s: blocked (no durable queue; outcome unknown)", e.EffectID)
				continue
			}
			// A durable queue is not an idempotent submit API. Without a send
			// identity it cannot tell us which earlier attempt it retained.
			_ = d.acknowledge(requestID, e, EffectBlocked,
				"the external send was attempted without a recorded receipt; a durable queue alone cannot prove delivery or make resubmission safe")
			stillUnresolved = append(stillUnresolved, e)
			continue
		}
		if d.DryRun {
			continue
		}
		// Reconciliation asks the OWNING backend, for the same reason dispatch
		// does. A send id is only meaningful inside the backend that minted it, so
		// asking a different transport whether it arrived is not a weaker check —
		// it is a check of the wrong system, and a backend that has never heard
		// of the id would answer "absent" and retire a message that was
		// delivered.
		backend, _, berr := d.backendFor(requestID, e.SessionID)
		if berr != nil {
			// Without a routing decision the outcome stays unknown. It is not
			// evidence of failure and must not be retried into another transport.
			_ = d.acknowledge(requestID, e, EffectBlocked, berr.Error())
			stillUnresolved = append(stillUnresolved, e)
			continue
		}
		rec, serr := backend.Receipt(ctx, e.SendID)
		if serr != nil {
			if agentdeck.IsNotFound(serr) {
				// The record is gone. The queue does not retain it, so the
				// outcome is unknowable rather than failed.
				_ = d.acknowledge(requestID, e, EffectBlocked, "the send record no longer exists; delivery cannot be proven")
				stillUnresolved = append(stillUnresolved, e)
				continue
			}
			return nil, fmt.Errorf("agent-engine: reconciling send %s: %w", e.SendID, serr)
		}
		switch rec.Outcome {
		case runtime.OutcomeLanded:
			_ = d.acknowledge(requestID, e, EffectDelivered, "")
			d.logf("reconciled %s: landed", e.EffectID)
		case runtime.OutcomeFailed:
			_ = d.acknowledge(requestID, e, EffectFailed, receiptReason(rec))
			d.logf("reconciled %s: failed (%s)", e.EffectID, receiptReason(rec))
		case runtime.OutcomeInFlight:
			// Still owned by the queue. Leave it in flight rather than sending
			// it ourselves; that is how an agent receives one instruction twice.
			d.logf("reconciled %s: still in flight", e.EffectID)
			stillUnresolved = append(stillUnresolved, e)
		default:
			// Ambiguous, or a backend that classified nothing. Both mean the same
			// thing to a supervisor: delivery cannot be proven, so block.
			_ = d.acknowledge(requestID, e, EffectBlocked,
				"the queue settled the message without transcript evidence; delivery cannot be proven")
			d.logf("reconciled %s: blocked (unproven)", e.EffectID)
		}
	}
	return stillUnresolved, nil
}
