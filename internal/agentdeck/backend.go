package agentdeck

// The Agent Deck implementation of the backend-neutral session boundary.
//
// This is an ADAPTER and nothing else. Every method delegates to the existing
// public CLI methods, and no parsing, receipt classification, cursor handling or
// session semantics is redefined here. That restraint is the whole point of the
// stage: Agent Deck behaviour must be identical before and after the boundary
// exists, so that adding a transport later cannot quietly change the behaviour of
// the one already in production.
//
// The one genuinely new behaviour is `Capabilities`, and it is new information
// rather than new policy. Agent Deck really can list sessions, read output,
// reconcile receipts, resume and close; saying so explicitly replaces a type
// assertion that previously assumed it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emjin/agent-engine/internal/runtime"
)

// BackendName is the transport identifier recorded in provenance.
const BackendName = "agent-deck"

// CLIBackend adapts an Agent Deck runtime to runtime.Backend.
type CLIBackend struct {
	rt Runtime
}

// NewBackend wraps an Agent Deck runtime in the session boundary.
func NewBackend(rt Runtime) *CLIBackend { return &CLIBackend{rt: rt} }

// Compile-time proof that the adapter satisfies the boundary it was written for.
var _ runtime.Backend = (*CLIBackend)(nil)

// outcomeOf restates Agent Deck's own send classification in the boundary's
// vocabulary.
//
// This mapping lives in the adapter, not in runtime, for the reason runtime
// documents: only the component that knows a transport's words may decide what
// they mean. A value outside the four known answers becomes OutcomeUnstated
// rather than being optimistically rounded to a success.
func outcomeOf(o SendOutcome) runtime.Outcome {
	switch o {
	case SendLanded:
		return runtime.OutcomeLanded
	case SendFailed:
		return runtime.OutcomeFailed
	case SendInFlight, SendQueued:
		return runtime.OutcomeInFlight
	case SendAmbiguous:
		return runtime.OutcomeAmbiguous
	default:
		return runtime.OutcomeUnstated
	}
}

// receiptOf converts one Agent Deck receipt, classifying it with the runtime's
// already-tested classifier rather than with a second copy of its rules.
func receiptOf(rec Receipt) runtime.Receipt {
	return runtime.Receipt{
		SendID:    rec.SendID,
		State:     rec.State,
		Settled:   rec.Settled,
		SessionID: rec.SessionID,
		Reason:    rec.Reason,
		Outcome:   outcomeOf(Classify(rec)),
	}
}

// Name identifies the transport.
func (b *CLIBackend) Name() string { return BackendName }

// Capabilities reports what Agent Deck's public CLI actually supports.
//
// The values are drawn from what the pinned CLI exposes: `list --json` exists,
// `session output --json` exists, `send-status` exists, and `loadSession`/
// session close are advertised by the runtime itself.
func (b *CLIBackend) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{
		Inventory: true,
		Output:    true,
		Receipts:  true,
		Resume:    true,
		Close:     true,
		// The queued send IS the durable record: an accepted message is retained
		// and answerable by send id after a restart, so an unknown outcome is
		// reconcilable rather than a reason to resend.
		DurableQueue: true,
	}
}

// Activate creates and starts a session, returning its confirmed reference.
func (b *CLIBackend) Activate(ctx context.Context, spec runtime.ActivateSpec) (runtime.SessionRef, error) {
	if strings.TrimSpace(spec.Label) == "" {
		// A create whose outcome is unknown must be reconcilable. Without a
		// deterministic label there is nothing to reconcile against, so refusing
		// here is refusing to create an unmanageable session.
		return runtime.SessionRef{}, fmt.Errorf("%w: an activation label is required so an ambiguous create can be reconciled", runtime.ErrUnsupported)
	}
	sess, err := b.rt.Create(ctx, CreateSpec{Title: spec.Label, Path: spec.Path, Tool: spec.Tool})
	if err != nil {
		return runtime.SessionRef{}, err
	}
	if strings.TrimSpace(sess.ID) == "" {
		// Deliberately NOT retried upstream. The session may exist; a second
		// Create is how two workers end up on one workspace.
		return runtime.SessionRef{}, fmt.Errorf("agentdeck: the runtime reported a created session with no ID; "+
			"it may exist and must be reconciled by label %q rather than created again", spec.Label)
	}
	if err := b.rt.Start(ctx, sess.ID); err != nil {
		return runtime.SessionRef{}, err
	}
	return runtime.SessionRef{Backend: BackendName, ID: sess.ID, Label: spec.Label}, nil
}

// Reconcile looks for an existing session carrying the activation label.
func (b *CLIBackend) Reconcile(ctx context.Context, label string, _ bool) (runtime.SessionRef, bool, error) {
	if !b.Capabilities().Inventory {
		return runtime.SessionRef{}, false, fmt.Errorf("%w: agent-deck cannot list sessions, so an ambiguous "+
			"create cannot be reconciled", runtime.ErrUnsupported)
	}
	sessions, err := b.rt.List(ctx)
	if err != nil {
		// An unreadable inventory is NOT an empty inventory. Reporting "absent"
		// here is what turns an ambiguous create into a duplicate session.
		return runtime.SessionRef{}, false, fmt.Errorf("agentdeck: the session inventory could not be read, so "+
			"the create outcome is unknown and no new session may be created: %w", err)
	}
	for _, s := range sessions {
		if s.Title == label {
			return runtime.SessionRef{Backend: BackendName, ID: s.ID, Label: label}, true, nil
		}
	}
	return runtime.SessionRef{}, false, nil
}

// Inspect returns a session's observed state.
func (b *CLIBackend) Inspect(ctx context.Context, ref runtime.SessionRef) (runtime.Session, error) {
	s, err := b.rt.Inspect(ctx, ref.ID)
	if err != nil {
		return runtime.Session{}, err
	}
	return runtime.Session{ID: s.ID, Title: s.Title, Path: s.Path, Tool: s.Tool, Status: s.Status}, nil
}

// Submit hands work to a session.
func (b *CLIBackend) Submit(ctx context.Context, ref runtime.SessionRef, message string) (runtime.Receipt, error) {
	rec, err := b.rt.Send(ctx, ref.ID, message)
	if err != nil {
		return runtime.Receipt{}, err
	}
	return receiptOf(rec), nil
}

// Receipt reconciles a queued send.
func (b *CLIBackend) Receipt(ctx context.Context, sendID string) (runtime.Receipt, error) {
	if !b.Capabilities().Receipts {
		return runtime.Receipt{}, fmt.Errorf("%w: agent-deck cannot reconcile send receipts", runtime.ErrUnsupported)
	}
	rec, err := b.rt.SendStatus(ctx, sendID)
	if err != nil {
		return runtime.Receipt{}, err
	}
	return receiptOf(rec), nil
}

// Output reads a bounded session response.
//
// truncation is reported as false because the pinned CLI's JSON envelope carries
// no truncation flag. That is an honest "not observable" rather than a claim that
// nothing was cut: a caller needing the distinction must compare against the
// budget it passed.
func (b *CLIBackend) Output(ctx context.Context, ref runtime.SessionRef, maxTokens int) (string, bool, error) {
	if !b.Capabilities().Output {
		return "", false, fmt.Errorf("%w: agent-deck cannot read session output", runtime.ErrUnsupported)
	}
	out, err := b.rt.Output(ctx, ref.ID, runtime.ClampOutputBudget(maxTokens))
	if err != nil {
		return "", false, err
	}
	return out.Text, false, nil
}

// Shutdown ends a session.
//
// It delegates to the runtime's Stop and adds no claim that the session actually
// stopped. Proving quiescence remains the caller's job, via Inspect.
func (b *CLIBackend) Shutdown(ctx context.Context, ref runtime.SessionRef) error {
	stopErr := b.rt.Stop(ctx, ref.ID)
	clear, proofErr := b.ProveQuiescence(ctx, ref)
	if clear && proofErr == nil {
		return nil
	}
	if owner, ok := b.rt.(ownershipRuntime); ok && proofErr == nil {
		proof, err := owner.InspectOwnership(ctx, ref.ID)
		if err == nil && proof.PaneAttached != nil && !*proof.PaneAttached && proof.Verdict != "unknown" {
			if err = owner.ReconcileOwnership(ctx, ref.ID); err == nil {
				clear, proofErr = b.ProveQuiescence(ctx, ref)
				if clear && proofErr == nil {
					return nil
				}
			} else {
				proofErr = err
			}
		}
	}
	if stopErr != nil || proofErr != nil {
		return errors.Join(stopErr, proofErr)
	}
	return fmt.Errorf("session %s has not proven owned-process release", ref.ID)
}

// Events follows the Agent Deck observation stream.
func (b *CLIBackend) Events(ctx context.Context, after uint64) (<-chan runtime.Event, <-chan error, error) {
	frames, errs, err := b.rt.Events(ctx, after)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan runtime.Event, 256)
	go func() {
		defer close(out)
		for f := range frames {
			select {
			case out <- runtime.Event{
				Backend: BackendName, Kind: f.Kind, SessionID: f.SessionID,
				Cursor: f.Cursor, Payload: f.Data,
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errs, nil
}

// Health reports observation availability.
func (b *CLIBackend) Health(ctx context.Context) (runtime.Health, error) {
	h, err := b.rt.BusHealth(ctx)
	if err != nil {
		return runtime.Health{Available: false, Reasons: []string{"bus-health-error"}}, err
	}
	var reasons []string
	if !h.OK() {
		reasons = append(reasons, "bus-disabled")
	}
	return runtime.Health{Available: h.OK(), Reasons: reasons, Cursor: h.Cursor}, nil
}

// ProcessIdentity reports that this backend owns no child process.
//
// Agent Deck's sessions are supervised by Agent Deck, not by this process, so
// there is no pid here to record. Returning false rather than zero keeps "no
// process of mine" distinguishable from "process 0", which would be a claim.
func (b *CLIBackend) ProcessIdentity() (int, bool) { return 0, false }

// EnsureSessionRunning starts a bound session that is not up.
//
// It exists because the control layer must be able to RESUME, and a resume that
// stops a non-running session destroys the very thing it was asked to recover.
// A backend that owns its process has no such step and must not pretend to.
func (b *CLIBackend) EnsureSessionRunning(ctx context.Context, ref runtime.SessionRef) error {
	sess, err := b.rt.Inspect(ctx, ref.ID)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(sess.Status)) {
	case "running", "waiting":
		return nil
	}
	return b.rt.Start(ctx, ref.ID)
}

func (b *CLIBackend) ReadAttributedOutput(ctx context.Context, ref runtime.SessionRef, budget int) (runtime.SessionOutput, error) {
	out, err := b.rt.Output(ctx, ref.ID, runtime.ClampOutputBudget(budget))
	return runtime.SessionOutput{SessionID: out.SessionID, Text: out.Text, Model: out.Model}, err
}
