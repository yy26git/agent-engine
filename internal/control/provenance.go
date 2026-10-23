package control

import (
	"fmt"
	"strings"
)

// Provenance: recording what a request was admitted against.
//
// Two things belong on every admitted request and were previously recorded
// nowhere: the operator POLICY digest (already present) and the RUNTIME identity
// it was admitted against. The second matters because the runtime is an external
// executable that produces every supervised session: a request audited without
// naming it is audited against a moving target.

// RecordRuntimePin stores the verified runtime identity on a request.
//
// It is set once, at admission. Refusing to overwrite it is deliberate: a
// deployment upgrade must never retroactively change what a completed run was
// admitted under, or an old result would appear to have run on a newer binary.
func (s *Store) RecordRuntimePin(requestID string, pin RuntimePin) error {
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		if st.RuntimePin.Pinned() {
			return nil
		}
		pin.VerifiedAt = s.nowRFC3339()
		st.RuntimePin = pin
		return nil
	})
	return err
}

// VerifyRuntimePin re-checks the recorded identity against the lock.
//
// It is called before any runtime effect. A request admitted against one artifact
// and about to be driven by another is refused, because the supervision decisions
// that follow would be attributed to a binary that was never reviewed for them.
func (s *Store) VerifyRuntimePin(requestID string, commit, sha string) error {
	st, err := s.State(requestID)
	if err != nil {
		return err
	}
	if !st.RuntimePin.Pinned() {
		// No recorded pin means no claim to violate; the launch-time pin is the
		// authority and is enforced by the caller.
		return nil
	}
	if strings.TrimSpace(commit) != "" && !strings.EqualFold(strings.TrimSpace(commit), st.RuntimePin.SourceCommit) {
		return fmt.Errorf("agent-engine: request %s was admitted against runtime commit %s but is about to "+
			"run on %s; a run may not change the binary it was admitted under",
			requestID, st.RuntimePin.SourceCommit, commit)
	}
	if strings.TrimSpace(sha) != "" && !strings.EqualFold(strings.TrimSpace(sha), st.RuntimePin.BinarySHA256) {
		return fmt.Errorf("agent-engine: request %s was admitted against artifact %s but is about to run %s",
			requestID, st.RuntimePin.BinarySHA256, sha)
	}
	return nil
}
