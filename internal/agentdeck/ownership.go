package agentdeck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/emjin/agent-engine/internal/runtime"
)

// OwnershipProof uses the pinned public command, never private upstream storage.
type OwnershipProof struct {
	InstanceID   string          `json:"instance_id"`
	Admissible   *bool           `json:"admissible"`
	PaneAttached *bool           `json:"pane_attached"`
	Verdict      string          `json:"verdict"`
	Survivors    json.RawMessage `json:"survivors"`
	ReceiptError string          `json:"receipt_error,omitempty"`
	noReceipt    bool
}
type ownershipRuntime interface {
	InspectOwnership(context.Context, string) (OwnershipProof, error)
	ReconcileOwnership(context.Context, string) error
}

func decodeOwnership(raw, id string) (OwnershipProof, error) {
	var p OwnershipProof
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	if p.InstanceID != id || p.Admissible == nil || p.PaneAttached == nil || len(p.Survivors) == 0 || strings.TrimSpace(string(p.Survivors)) == "null" {
		return p, fmt.Errorf("ownership proof missing or mismatched")
	}
	var survivors []json.RawMessage
	if err := json.Unmarshal(p.Survivors, &survivors); err != nil {
		return p, err
	}
	if p.ReceiptError != "" {
		return p, fmt.Errorf("ownership receipt unreadable")
	}
	if p.Verdict == "" {
		// The pinned ownership CLI omits receipt metadata and verdict when there
		// is no receipt. Its explicit admissibility and pane/survivor observations
		// distinguish that response from a malformed or unreadable receipt.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return p, err
		}
		for _, key := range []string{"generation", "state", "provider", "leader", "members"} {
			if _, exists := fields[key]; exists {
				return p, fmt.Errorf("ownership receipt metadata has no verdict")
			}
		}
		if !*p.Admissible || *p.PaneAttached || len(survivors) != 0 {
			return p, fmt.Errorf("no-receipt ownership response does not prove release")
		}
		p.noReceipt = true
		return p, nil
	}
	switch p.Verdict {
	case "clear", "owned", "unknown":
	default:
		return p, fmt.Errorf("unknown ownership verdict")
	}
	return p, nil
}
func (p OwnershipProof) clear() bool {
	var survivors []json.RawMessage
	if json.Unmarshal(p.Survivors, &survivors) != nil {
		return false
	}
	return p.Admissible != nil && *p.Admissible && p.PaneAttached != nil && !*p.PaneAttached &&
		(p.Verdict == "clear" || p.noReceipt) && len(survivors) == 0 && p.ReceiptError == ""
}
func (r *CLIRuntime) InspectOwnership(ctx context.Context, id string) (OwnershipProof, error) {
	raw, err := r.run(ctx, "session", "ownership", "inspect", id, "--json")
	// Even a nonzero command can carry a useful negative receipt, never success.
	p, decodeErr := decodeOwnership(raw, id)
	if decodeErr != nil {
		return p, decodeErr
	}
	if err != nil && p.clear() {
		return p, err
	}
	return p, nil
}
func (r *CLIRuntime) ReconcileOwnership(ctx context.Context, id string) error {
	_, err := r.run(ctx, "session", "ownership", "reconcile", id, "--json")
	return err
}
func (b *CLIBackend) ProveQuiescence(ctx context.Context, ref runtime.SessionRef) (bool, error) {
	sess, err := b.rt.Inspect(ctx, ref.ID)
	if err != nil {
		return false, err
	}
	if sess.ID != ref.ID {
		return false, fmt.Errorf("inventory identity mismatch")
	}
	status := strings.ToLower(strings.TrimSpace(sess.Status))
	switch status {
	case "stopped", "dead", "exited", "closed", "error", "errored", "stale":
	default:
		return false, nil
	}
	if rt, ok := b.rt.(ownershipRuntime); ok {
		proof, err := rt.InspectOwnership(ctx, ref.ID)
		if err != nil {
			return false, err
		}
		return proof.clear(), nil
	}
	// Scripted/non-process runtimes may define stopped inventory as their complete
	// lifecycle contract. Real CLIRuntime always takes the ownership proof above.
	switch status {
	case "stopped", "dead", "exited", "closed":
		return true, nil
	}
	return false, fmt.Errorf("transport cannot prove release of an error session")
}
