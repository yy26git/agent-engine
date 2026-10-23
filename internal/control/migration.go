package control

// Ownership migration from the legacy engineering draft into Agent Engine.
//
// The draft in Agent Deck's tree runs its own engineering workflow with its own
// store, its own supervisors and its own sessions. Agent Engine now owns admission,
// obligations, verification and results. Two owners of the same workspace is not a
// redundancy to be tidied up later — it is two processes able to write the same
// files, and the one that loses is whichever was not holding the lock.
//
// So a transfer is gated on three things, in this order:
//
//  1. The legacy supervisors must be stopped and their scopes isolated. If a
//     legacy process can still act on the workspace, the transfer is BLOCKED —
//     not "proceed and hope", because the failure is a concurrent writer.
//  2. Every legacy effect whose outcome is unknown must be reconciled. An
//     unresolved send, worker or approval lineage cannot be adopted, because
//     adopting it would mean asserting a history nobody can prove.
//  3. Only then is ownership recorded, with the original bytes and their digest
//     preserved so the transfer itself is auditable.
//
// The one thing this never does is promote a legacy approval into a new PASS.
// A legacy "verified" record asserts that some earlier implementation decided the
// work was correct, under a trust model that never checked who decided it. That
// history stays readable with its original assurance level; it is not evidence
// for anything under the current boundary.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrTransferBlocked is returned when ownership cannot be transferred safely.
var ErrTransferBlocked = errors.New("agent-engine: ownership transfer is blocked")

// ErrUnknownEffect is returned when a legacy effect's outcome cannot be proven.
var ErrUnknownEffect = errors.New("agent-engine: a legacy effect has an unknown outcome")

// LegacyEffect is one thing the legacy engine may have done that this layer
// cannot see the result of.
type LegacyEffect struct {
	// Kind is "send", "worker", "approval" or "job".
	Kind string `json:"kind"`
	// Identity is the effect's own identifier, for reconciliation.
	Identity string `json:"identity"`
	// Session is the full legacy session ID, when one is involved.
	Session string `json:"session,omitempty"`
	// Workspace is the workspace the effect could still write to.
	Workspace string `json:"workspace,omitempty"`
	// Settled records that the effect reached a provably terminal state.
	//
	// It is set by reconciliation, never assumed. A send that was issued with no
	// receipt is exactly the ambiguous case, and treating it as settled would
	// adopt a history that may or may not have happened.
	Settled bool `json:"settled,omitempty"`
	// Quiescent records that a worker is known stopped and holding no resources.
	Quiescent bool `json:"quiescent,omitempty"`
}

// Reconciled reports whether an effect's outcome is provably known.
//
// Only an explicitly settled state counts. "Probably finished" is the exact
// ambiguity this exists to refuse.
func (e LegacyEffect) Reconciled() bool {
	switch strings.ToLower(strings.TrimSpace(e.Kind)) {
	case "send", "job":
		// A send or a job is only adoptable once its terminal state is known.
		return e.Settled
	case "worker":
		// A worker is only adoptable once it is known stopped and quiescent.
		return e.Settled && e.Quiescent
	case "approval":
		// An approval is adoptable as HISTORY only. It can never be adopted as
		// authority, so there is no settled state that promotes it.
		return false
	}
	return false
}

// TransferRecord is the durable proof that ownership moved.
//
// It records what was transferred, from what, and on whose evidence. The
// original digest is kept so a later reader can prove the migrated state is the
// state that was inspected rather than something reconstructed from memory.
type TransferRecord struct {
	// TransferID identifies this transfer.
	TransferID string `json:"transfer_id"`
	// Epoch is the ownership epoch the transfer established.
	Epoch int `json:"epoch"`
	// Source is "legacy-agent-deck-workflow" for a draft handover.
	Source string `json:"source"`
	// OriginalStateSHA256 is the digest of the pre-transfer state bytes.
	OriginalStateSHA256 string `json:"original_state_sha256"`
	// OriginalStateExport is where those bytes were preserved, when exported.
	OriginalStateExport string `json:"original_state_export,omitempty"`
	// TransferredAt is when ownership actually moved.
	TransferredAt string `json:"transferred_at"`
	// LegacyEffects records what the legacy engine may have had outstanding.
	LegacyEffects []LegacyEffect `json:"legacy_effects,omitempty"`
	// Approved carries any legacy record that was preserved as history.
	Approved []string `json:"approved,omitempty"`
	// AssuranceLevel records how much the preserved history is worth.
	//
	// It is deliberately "unverified" for anything the legacy engine approved:
	// the bytes are real, the reasoning is preserved, and nothing about who
	// decided it was ever established.
	AssuranceLevel string `json:"assurance_level"`
}

// MigrationRequest describes a proposed transfer.
type MigrationRequest struct {
	// Source names the legacy owner being retired.
	Source string
	// Workspace is the workspace ownership is moving for.
	Workspace string
	// Effects are the outstanding legacy effects that must be reconciled.
	Effects []LegacyEffect
	// LegacySupervisorsStopped is the operator's attestation that the legacy
	// supervisors and their auto-launchers are stopped.
	//
	// It is an attestation rather than something this layer can prove, because
	// the legacy processes are outside it. That is exactly why it is recorded
	// explicitly: an unstated assumption here is how two owners end up writing
	// the same workspace.
	LegacySupervisorsStopped bool
	// Approved lists legacy approval identities to preserve as history.
	Approved []string
	// Now is the clock, injected so the record is deterministic under test.
	Now time.Time
}

// MigrateOwnership transfers ownership, or explains precisely why it cannot.
//
// The refusal messages are the point. "Blocked" with no reason would leave an
// operator to guess, and the guess they are most likely to make is to proceed.
func (s *Store) MigrateOwnership(requestID string, req MigrationRequest) (*TransferRecord, error) {
	if strings.TrimSpace(req.Source) == "" {
		return nil, fmt.Errorf("%w: the transfer names no legacy source", ErrTransferBlocked)
	}
	if !req.LegacySupervisorsStopped {
		return nil, fmt.Errorf("%w: the legacy supervisors for %s are not attested stopped; "+
			"two owners able to write the same workspace is a concurrent-writer race, not a redundancy",
			ErrTransferBlocked, req.Source)
	}
	// Every outstanding effect must be provably settled. One unknown effect
	// blocks the whole transfer, because a partially adopted history is a history
	// with a hole in it that nobody can later detect.
	var unresolved []string
	for _, e := range req.Effects {
		if !e.Reconciled() {
			unresolved = append(unresolved, fmt.Sprintf("%s %q", e.Kind, e.Identity))
		}
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("%w: these legacy effects have outcomes nobody can prove: %s; "+
			"reconcile them before transferring ownership",
			ErrUnknownEffect, strings.Join(unresolved, ", "))
	}

	// The pre-transfer bytes are read and preserved before anything is written,
	// so the record describes the state that was actually inspected.
	dir, err := s.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(dir, StateFileName)
	original, rerr := os.ReadFile(statePath)
	if rerr != nil {
		return nil, fmt.Errorf("%w: the pre-transfer state cannot be read, so the transfer cannot be "+
			"recorded against known bytes: %v", ErrTransferBlocked, rerr)
	}
	sum := sha256.Sum256(original)
	originalDigest := hex.EncodeToString(sum[:])

	exportPath, err := s.exportOriginalState(requestID, original, originalDigest)
	if err != nil {
		// An export failure leaves ownership exactly where it was.
		return nil, err
	}

	now := req.Now
	if now.IsZero() {
		now = s.now()
	}
	rec := TransferRecord{
		TransferID:          "T-" + requestID,
		Epoch:               1,
		Source:              req.Source,
		OriginalStateSHA256: originalDigest,
		OriginalStateExport: exportPath,
		TransferredAt:       now.UTC().Format(time.RFC3339),
		LegacyEffects:       req.Effects,
		Approved:            req.Approved,
		AssuranceLevel:      "unverified",
	}

	staged, err := s.UpdateCommitted(requestID, func(cur *State) error {
		if cur.Stage.Terminal() {
			return fmt.Errorf("%w: request %s is already terminal as %s; a finished outcome is history, "+
				"not something to take ownership of", ErrTransferBlocked, requestID, cur.ResultStatus)
		}
		cur.Transfers = append(cur.Transfers, rec)
		// Legacy ownership is recorded as retired. It is never treated as the
		// current owner, and a retired owner cannot be reactivated by a later
		// write: activation is what the engine does, and only the engine does it.
		cur.RetiredOwners = append(cur.RetiredOwners, req.Source)
		// A legacy approval is preserved as history and is explicitly NOT
		// authority. The request is left owing a fresh independent review.
		cur.LegacyAssurance = "unverified"
		cur.ActiveReview = nil
		cur.Checkpoint.ReviewPending = false
		return cur.transition(StageWorking, "ownership transferred from "+req.Source+
			"; a fresh independent completion review is owed", rec.TransferredAt)
	})
	if err != nil {
		return nil, err
	}
	_ = staged
	return &rec, nil
}

// exportOriginalState preserves the pre-transfer bytes next to the state.
//
// It is written under an immutable name and never edited, so a later reader can
// hash it and confirm the transfer was recorded against exactly these bytes.
func (s *Store) exportOriginalState(requestID string, original []byte, digest string) (string, error) {
	rel := filepath.Join(RequestsDirName, requestID, "MIGRATION-ORIGINAL-STATE.json")
	if err := writeDurable(s.cfg.StoreRoot, rel, original, 0o444); err != nil {
		return "", fmt.Errorf("%w: the original state could not be preserved, so ownership is unchanged: %v",
			ErrTransferBlocked, err)
	}
	// The export is verified by reading it back, not by trusting the write.
	written, err := os.ReadFile(filepath.Join(s.cfg.StoreRoot, rel))
	if err != nil {
		return "", fmt.Errorf("%w: the preserved state cannot be read back: %v", ErrTransferBlocked, err)
	}
	back := sha256.Sum256(written)
	if got := hex.EncodeToString(back[:]); got != digest {
		return "", fmt.Errorf("%w: the preserved state hashes to %s but the inspected bytes hashed to %s; "+
			"ownership is unchanged", ErrTransferBlocked, got, digest)
	}
	return rel, nil
}

// PromoteLegacyPass refuses to treat a legacy approval as a new PASS.
//
// This exists as a function rather than as a comment because a rule nobody can
// call is a rule that will eventually be inlined by someone in a hurry. The
// refusal is explicit, permanent, and testable.
func PromoteLegacyPass(legacyApproval string) error {
	return fmt.Errorf("%w: legacy approval %q is preserved history with unverified assurance; "+
		"it cannot become a PASS under the current boundary, because the legacy engine never established "+
		"who approved or what was verified. A fresh independent completion review is required",
		ErrTransferBlocked, legacyApproval)
}

// migrateStateV2ToV3 adds backend-qualified bindings and observation positions.
//
// Two rules matter here. First, legacy bare IDs become Agent Deck bindings rather
// than being left ambiguous, because an unqualified ID cannot be routed safely and
// every pre-v3 state in existence was produced through Agent Deck. That is a
// statement about HISTORY, not an inference about capability, which is why it is
// safe to apply automatically.
//
// Second, an effect that was in flight when the state was written is NOT resolved.
// Its delivery outcome carries forward as unknown, because assuming it is how a
// duplicate send becomes a duplicate write.
func migrateStateV2ToV3(st *State) {
	if st.BackendBindings == nil {
		st.BackendBindings = map[ProfileRole]SessionBinding{}
	}
	legacy := func(role ProfileRole, id string) {
		if id == "" {
			return
		}
		// Only fill an empty slot. A state already carrying a qualified binding
		// knows more than this migration does.
		if _, exists := st.BackendBindings[role]; exists {
			return
		}
		st.BackendBindings[role] = SessionBinding{
			Backend: LegacyBackendAgentDeck, SessionID: id, Resolved: true,
		}
	}
	legacy(RoleWorker, st.WorkerSession)
	legacy(RoleReviewer, st.AuditorSession)
	legacy(RoleCheckpoint, st.CheckpointSession)

	// The consumed position starts at zero on purpose. Nothing in a v2 state
	// proves an observation was consumed, so claiming otherwise would silently
	// discard whatever the first v3 pass observes.
	st.ConsumedJournalSeq = 0
	if st.IngestedPositions == nil {
		st.IngestedPositions = map[string]uint64{}
	}
	// Unresolved activations keep their uncertainty: their ActivationID is the
	// only identity they ever had, and it is deliberately left unqualified rather
	// than assigned a backend that was never recorded.
	// Name this migration's target so later migrations still run, and never
	// reapply the v2 journal reset to a newer state when another version is added.
	st.SchemaVersion = 3
}
