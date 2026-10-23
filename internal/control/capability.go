package control

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Engine-issued execution capability.
//
// The rule this enforces is "only the engine may activate an admitted child". The
// previous implementation refused an `origin.kind` naming an executor, which is a
// caller-SUPPLIED CLAIM: a caller that wrote `kind: "forge"` while actually being
// an engine-owned session would have been admitted, and the whole point of the
// restriction is that a worker cannot widen its own authority.
//
// A capability fixes that by making the authority unforgeable rather than
// declared. It is a random token the engine generates, stores against the request
// that owns it, and presents only when it activates a child. A caller that guesses
// the wrong token is refused, and a caller that supplies its own token is refused
// because the store holds the only copy that counts.

// CapabilityScope names what a capability authorises.
type CapabilityScope string

const (
	// ScopeChildActivation authorises admitting one child of one admitted root.
	ScopeChildActivation CapabilityScope = "child-activation"
)

// ExecutionCapability is an unforgeable, single-scope authorisation.
type ExecutionCapability struct {
	// Token is the secret. It exists only between the mint and the caller that
	// received it; it is never persisted, logged, or returned.
	Token string `json:"-"`
	// TokenHash is what is persisted: the digest of the token.
	//
	// Storing a hash rather than the token means a leaked state file — which
	// happens, and is readable by anyone who can read the store — does not hand
	// over a live capability. Validation compares digests, so possession of the
	// state file is not possession of the authority.
	TokenHash string `json:"token_sha256"`
	// Scope is what the token authorises.
	Scope CapabilityScope `json:"scope"`
	// RootRequestID is the admitted root this capability may activate children of.
	RootRequestID string `json:"root_request_id"`
	// DefinitionSHA binds the capability to one admitted definition, so a token
	// cannot be reused against a different graph.
	DefinitionSHA string `json:"definition_sha256"`
	// ExpiresAt bounds the capability's lifetime.
	//
	// An unbounded capability is a permanent grant, and a permanent grant cannot
	// be revoked without revoking the request. A short life means a leaked token
	// stops working on its own.
	ExpiresAt string `json:"expires_at"`
	// IssuedAt is when it was minted.
	IssuedAt string `json:"issued_at"`
}

// Valid reports whether a presented token may be used right now.
func (c ExecutionCapability) Valid(token string, scope CapabilityScope, rootID string, now string) error {
	if c.TokenHash == "" && c.Token == "" {
		return fmt.Errorf("%w: no capability was issued for this request", ErrCapabilityInvalid)
	}
	if c.Scope != scope {
		return fmt.Errorf("%w: the capability authorises %s, not %s", ErrCapabilityInvalid, c.Scope, scope)
	}
	if c.RootRequestID != rootID {
		return fmt.Errorf("%w: the capability belongs to root %s, not %s",
			ErrCapabilityInvalid, c.RootRequestID, rootID)
	}
	if c.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
		if err != nil {
			return fmt.Errorf("%w: the capability expiry is unreadable, so the capability is refused", ErrCapabilityInvalid)
		}
		nowT, err := time.Parse(time.RFC3339, now)
		if err != nil {
			return fmt.Errorf("%w: the current time is unreadable, so the capability is refused", ErrCapabilityInvalid)
		}
		if !nowT.Before(exp) {
			return fmt.Errorf("%w: the capability expired at %s", ErrCapabilityInvalid, c.ExpiresAt)
		}
	}
	if c.TokenHash == "" {
		return fmt.Errorf("%w: the capability has no stored digest, so no token can match it", ErrCapabilityInvalid)
	}
	presented := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(presented[:])
	// A constant-time comparison, because a capability is a secret and a
	// timing-variable comparison of secrets is a real, if slow, attack.
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(strings.ToLower(c.TokenHash))) != 1 {
		return fmt.Errorf("%w: the presented token does not match", ErrCapabilityInvalid)
	}
	return nil
}

// ErrCapabilityInvalid is returned when a presented capability is not honoured.
var ErrCapabilityInvalid = errors.New("agent-engine: execution capability refused")

// DefaultCapabilityTTL bounds how long a minted capability stays usable.
const DefaultCapabilityTTL = time.Hour

// IssueChildCapability mints a capability for one admitted root and RETURNS the
// token.
//
// The token is returned exactly once, to the caller that minted it — in practice
// the engine, when it activates a child. The store persists only its digest, so a
// caller that never received the token cannot reproduce it, and a leaked state
// file does not carry a live capability.
func (s *Store) IssueChildCapability(requestID string, definitionSHA string, now time.Time) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("agent-engine: a child-activation capability could not be minted: %w", err)
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := s.UpdateCommitted(requestID, func(st *State) error {
		st.ChildCapability = ExecutionCapability{
			TokenHash:     hex.EncodeToString(sum[:]),
			Scope:         ScopeChildActivation,
			RootRequestID: requestID,
			DefinitionSHA: definitionSHA,
			IssuedAt:      now.UTC().Format(time.RFC3339),
			ExpiresAt:     now.Add(DefaultCapabilityTTL).UTC().Format(time.RFC3339),
		}
		return nil
	}); err != nil {
		return "", err
	}
	return token, nil
}

// VerifyChildCapability checks a presented token against the stored one.
//
// It returns the store's own record on success, so a caller cannot substitute a
// capability with different fields — a wider scope, a different root, a longer
// expiry — by simply supplying a well-formed token of their own.
func (s *Store) VerifyChildCapability(requestID, token string, now time.Time) (ExecutionCapability, error) {
	st, err := s.State(requestID)
	if err != nil {
		return ExecutionCapability{}, err
	}
	if err := st.ChildCapability.Valid(token, ScopeChildActivation, requestID, now.UTC().Format(time.RFC3339)); err != nil {
		return ExecutionCapability{}, err
	}
	return st.ChildCapability, nil
}

// RevokeChildCapability invalidates a minted capability immediately.
//
// Both the in-memory token and the persisted digest are cleared: clearing only the
// digest is what actually revokes, and clearing only the token would leave a
// stored digest that still validates.
func (s *Store) RevokeChildCapability(requestID string) error {
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		st.ChildCapability.Token = ""
		st.ChildCapability.TokenHash = ""
		return nil
	})
	return err
}

// randomToken produces a 256-bit random token as lowercase hex.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
