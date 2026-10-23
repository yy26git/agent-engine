package main

// Typed submissions from outside the engine: worker checkpoints and evidence.
//
// These are the two things a worker can tell the control layer without being
// asked. Both are typed commands rather than free text, because a paragraph of
// prose cannot be correlated to a generation, deduped, or checked for the fields
// a reviewer actually needs — and a submission that cannot be correlated is a
// submission nobody may act on.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emjin/agent-engine/internal/control"
)

// checkpointPayload is the typed body of `checkpoint --payload-file`.
type checkpointPayload struct {
	CheckpointID string `json:"checkpoint_id"`
	Claim        string `json:"claim"`
	NextAction   string `json:"next_action"`
	Blocker      string `json:"blocker,omitempty"`
	// EvidenceRefs name evidence already added with `evidence add`. A ref to
	// evidence that was never recorded is refused at review time, not here,
	// because a worker may legitimately reference an item in the same breath.
	EvidenceRefs   []string                `json:"evidence_refs,omitempty"`
	NegativeClaims []control.NegativeClaim `json:"negative_claims,omitempty"`
}

func cmdCheckpoint(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("checkpoint", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	payloadFile := fs.String("payload-file", "", "typed checkpoint payload JSON, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	if strings.TrimSpace(*payloadFile) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--payload-file is required (use - for stdin)"))
	}
	raw, err := readInput(*payloadFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	var payload checkpointPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}

	// The digest covers the submitted content, so an identical retry is a
	// duplicate and a reused ID with different bytes is a conflict.
	digest, err := digestBytes(raw)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}

	var accepted control.CheckpointReceipt
	var duplicate bool
	if _, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
		rec, err := s.SubmitCheckpoint(control.CheckpointReceipt{
			CheckpointID:   payload.CheckpointID,
			Claim:          payload.Claim,
			NextAction:     payload.NextAction,
			Blocker:        payload.Blocker,
			EvidenceRefs:   payload.EvidenceRefs,
			NegativeClaims: payload.NegativeClaims,
		}, digest, stNow())
		if errors.Is(err, control.ErrCheckpointDuplicate) {
			duplicate = true
			accepted = rec
			return nil
		}
		if err != nil {
			return err
		}
		accepted = rec
		return nil
	}); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}

	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":    *requestID,
			"checkpoint_id": accepted.CheckpointID,
			"sequence":      accepted.Sequence,
			"duplicate":     duplicate,
			"accepted_at":   accepted.AcceptedAt,
			// Explicitly restated to the caller: accepting a checkpoint advances
			// nothing. Only a reviewed outcome moves an obligation, and a worker
			// that believes otherwise will stop working believing it is done.
			"advanced": false,
		})
	}
	if duplicate {
		fmt.Fprintf(stdout, "checkpoint %s was already recorded (sequence %d); nothing changed\n",
			accepted.CheckpointID, accepted.Sequence)
		return 0
	}
	fmt.Fprintf(stdout, "recorded checkpoint %s (sequence %d)\n", accepted.CheckpointID, accepted.Sequence)
	fmt.Fprintln(stdout, "  this advances nothing: a reviewer must judge it before any obligation moves")
	return 0
}

func cmdEvidence(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("evidence", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	id := fs.String("id", "", "request-local evidence identifier")
	path := fs.String("path", "", "artifact path")
	sha := fs.String("sha256", "", "SHA-256 of the artifact bytes")
	kind := fs.String("kind", "artifact", "artifact, managed-command or external-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--id is required"))
	}
	// A digest is mandatory rather than optional. A path alone points at bytes
	// that may since have changed, so a conclusion resting on it attributes to
	// nothing at all.
	if strings.TrimSpace(*sha) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--sha256 is required: evidence that is not digest-pinned is not evidence"))
	}

	if _, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
		return s.AddEvidence(control.EvidenceRef{
			ID: *id, Path: *path, SHA256: strings.ToLower(*sha), Kind: *kind,
		})
	}); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":  *requestID,
			"evidence_id": *id,
			"sha256":      strings.ToLower(*sha),
		})
	}
	fmt.Fprintf(stdout, "recorded evidence %s (%s)\n", *id, strings.ToLower(*sha))
	return 0
}

// decodeStrict refuses unknown fields, so a misspelled safety field is an error
// rather than an unenforced intention.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("agent-engine: decoding payload: %w", err)
	}
	return nil
}

// stNow is the clock used to stamp an accepted submission.
func stNow() time.Time { return time.Now().UTC() }

// digestBytes is the content digest of a submitted payload.
//
// It is taken over the bytes the caller actually sent, not over a re-encoding of
// the parsed value, so a retry that differs only in insignificant whitespace is
// still recognised as a duplicate while a changed field is not.
func digestBytes(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
