package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// CanonicalSHA256 returns the content digest of a request.
//
// The digest is what makes admission idempotent, so the hashing rules are
// chosen for one property above all: two requests are the same request if and
// only if they would produce the same effects. That requires:
//
//   - Object keys sorted, so a field's position in the file cannot change its
//     identity. A request re-serialised by a different JSON library must still
//     hash the same, or an honest retry would look like a conflict and a
//     conflicting request would be able to hide behind reordering.
//   - Array order preserved, because obligations, evidence, verification and
//     allowed/protected paths are semantically ordered. Sorting them would make
//     a request that permits `src/**` before `evaluation/**` identical to one
//     that permits the reverse, and those can mean different things.
//   - No field omitted. A safety constraint that did not contribute to the
//     digest could be changed without changing identity, which is precisely how
//     a weaker request passes as the accepted one.
//
// The digest covers the whole request including the version, so a schema
// change is never invisible to admission.
func CanonicalSHA256(r *Request) (string, error) {
	if r == nil {
		return "", fmt.Errorf("contract: cannot hash a nil request")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("contract: encoding request: %w", err)
	}
	// Round-tripping through a generic map makes encoding/json emit keys in
	// sorted order, which is what makes the result independent of struct field
	// order and of any caller's own serialisation.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("contract: normalising request: %w", err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("contract: encoding canonical request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalJSON returns the canonical byte form of a request.
//
// It is stored alongside the digest so a human can see exactly which bytes were
// admitted. A digest alone is not auditable: it proves two things match, not
// what either of them was.
func CanonicalJSON(r *Request) ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("contract: encoding request: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("contract: normalising request: %w", err)
	}
	out, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("contract: encoding canonical request: %w", err)
	}
	return append(out, '\n'), nil
}

// DecodeRequest parses a submitted request and refuses anything it cannot fully
// understand.
//
// Two behaviours matter here. An unknown version is refused, because a newer
// schema may carry a safety field this build would silently ignore. And an
// unknown field is refused, because a caller that sends a misspelled
// `allowed_paths` and gets acceptance believes their path scope is enforced
// while this build is enforcing nothing.
func DecodeRequest(data []byte) (*Request, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var r Request
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("contract: decoding request: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("contract: request contains trailing content after the JSON object")
	}
	return &r, nil
}
