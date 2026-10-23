package agentdeck

import (
	"context"
	"errors"
	"github.com/emjin/agent-engine/internal/runtime"
	"testing"
)

type ownedShutdownRuntime struct {
	shutdownRetryRuntime
	proof      OwnershipProof
	proofErr   error
	reconciled int
}

func (r *ownedShutdownRuntime) InspectOwnership(context.Context, string) (OwnershipProof, error) {
	return r.proof, r.proofErr
}
func (r *ownedShutdownRuntime) ReconcileOwnership(context.Context, string) error {
	r.reconciled++
	return errors.New("survivor still alive")
}
func TestShutdown_ErrorStatusRequiresClearOwnership(t *testing.T) {
	proof, err := decodeOwnership("{\"instance_id\":\"retained-session\",\"admissible\":true,\"pane_attached\":false,\"verdict\":\"clear\",\"survivors\":[]}", "retained-session")
	if err != nil {
		t.Fatal(err)
	}
	r := &ownedShutdownRuntime{shutdownRetryRuntime: shutdownRetryRuntime{status: "error", stopErr: errors.New("not running")}, proof: proof}
	b := NewBackend(r)
	if err = b.Shutdown(t.Context(), runtime.SessionRef{ID: "retained-session"}); err != nil {
		t.Fatal(err)
	}
	if r.reconciled != 0 {
		t.Fatal("clear proof triggered reaping")
	}
}
func TestShutdown_UnknownOwnershipNeverPasses(t *testing.T) {
	for _, raw := range []string{
		"{\"instance_id\":\"other\",\"admissible\":true,\"pane_attached\":false,\"verdict\":\"clear\",\"survivors\":[]}",
		"{\"instance_id\":\"retained-session\",\"pane_attached\":false,\"survivors\":[]}",
		"{\"instance_id\":\"retained-session\",\"admissible\":true,\"pane_attached\":false,\"verdict\":\"clear\",\"survivors\":[],\"receipt_error\":\"corrupt\"}",
	} {
		if _, err := decodeOwnership(raw, "retained-session"); err == nil {
			t.Fatal("invalid proof accepted", raw)
		}
	}
	proof, _ := decodeOwnership("{\"instance_id\":\"retained-session\",\"admissible\":false,\"pane_attached\":false,\"verdict\":\"unknown\",\"survivors\":[]}", "retained-session")
	r := &ownedShutdownRuntime{shutdownRetryRuntime: shutdownRetryRuntime{status: "error", stopErr: errors.New("not running")}, proof: proof}
	if err := NewBackend(r).Shutdown(t.Context(), runtime.SessionRef{ID: "retained-session"}); err == nil {
		t.Fatal("unknown proof passed")
	}
}
func TestShutdown_StoppedWithSurvivorIsNotClean(t *testing.T) {
	proof, err := decodeOwnership("{\"instance_id\":\"retained-session\",\"admissible\":false,\"pane_attached\":false,\"verdict\":\"owned\",\"survivors\":[{\"pid\":42}]}", "retained-session")
	if err != nil {
		t.Fatal(err)
	}
	r := &ownedShutdownRuntime{shutdownRetryRuntime: shutdownRetryRuntime{status: "stopped"}, proof: proof}
	if err := NewBackend(r).Shutdown(t.Context(), runtime.SessionRef{ID: "retained-session"}); err == nil {
		t.Fatal("survivor passed")
	}
	if r.reconciled != 1 {
		t.Fatal("owned survivor not reconciled through owner")
	}
}

// This is the exact public shape observed from the pinned CLI after cleanup.
// A missing verdict is meaningful only with explicit no-receipt evidence.
func TestShutdown_NoReceiptAfterStopIsClean(t *testing.T) {
	proof, err := decodeOwnership(`{"admissible":true,"instance_id":"retained-session","pane_attached":false,"reason":"no ownership receipt","survivors":[]}`, "retained-session")
	if err != nil {
		t.Fatal(err)
	}
	r := &ownedShutdownRuntime{shutdownRetryRuntime: shutdownRetryRuntime{status: "stopped", stopErr: errors.New("not running")}, proof: proof}
	if err := NewBackend(r).Shutdown(t.Context(), runtime.SessionRef{ID: "retained-session"}); err != nil {
		t.Fatal(err)
	}
	if r.reconciled != 0 {
		t.Fatal("no receipt should not trigger reaping")
	}
}

func TestOwnership_PartialReceiptWithoutVerdictIsRefused(t *testing.T) {
	for _, raw := range []string{
		`{"admissible":true,"instance_id":"retained-session","pane_attached":false,"generation":1,"survivors":[]}`,
		`{"admissible":true,"instance_id":"retained-session","pane_attached":false,"members":[],"survivors":[]}`,
		`{"admissible":false,"instance_id":"retained-session","pane_attached":false,"survivors":[]}`,
		`{"admissible":true,"instance_id":"retained-session","pane_attached":true,"survivors":[]}`,
	} {
		if _, err := decodeOwnership(raw, "retained-session"); err == nil {
			t.Fatal("partial/blocked receipt accepted", raw)
		}
	}
}

func TestOwnership_EmptySurvivorWhitespaceIsEquivalent(t *testing.T) {
	proof, err := decodeOwnership(`{"admissible":true,"instance_id":"retained-session","pane_attached":false,"verdict":"clear","survivors":[ ]}`, "retained-session")
	if err != nil || !proof.clear() {
		t.Fatal("valid empty survivor list refused", err)
	}
}
