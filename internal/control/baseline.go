package control

import "time"

// The baseline measurement.
//
// `code_changed` is supposed to be a MEASUREMENT: what the content was when the
// work started, compared with what it is when the work finished. It cannot be
// inferred from how many repair cycles a request burned, because a request can
// spend five cycles and change nothing, and a request can change a great deal on
// its first attempt.
//
// The baseline is taken at the moment the request begins WORKING, not at
// admission. That distinction matters: a request may sit READY while an operator
// prepares evidence, and content written during that window is not work the
// implementer did. Measuring at admission would charge the request for someone
// else's edits, and the resulting `code_changed` would be wrong in the opposite
// direction — blaming an implementer for pre-existing changes.

// RecordBaseline measures the request's workspace and pins that identity.
//
// It is deliberately idempotent: the baseline is the FIRST measurement, so a
// later call cannot move it. A refresh, a second bind, or a restart must not
// silently redefine what "unchanged" means, because that would let a generation
// boundary erase the comparison the whole result rests on.
func (s *Store) RecordBaseline(requestID string, now func() time.Time) (string, error) {
	adm, err := s.loadRequestDir(requestID)
	if err != nil {
		return "", err
	}
	snap, err := TakeSnapshot(adm.Request.Workspace.Path, now())
	if err != nil {
		return "", err
	}
	var measured string
	if _, err := s.UpdateCommitted(requestID, func(st *State) error {
		if st.BaselineDigest != "" {
			measured = st.BaselineDigest
			return nil
		}
		st.BaselineDigest = snap.ContentDigest
		measured = snap.ContentDigest
		return nil
	}); err != nil {
		return "", err
	}
	return measured, nil
}

// ensureBaseline measures the baseline if it has never been taken.
//
// It is called from the WORKING transitions. A failure is returned rather than
// swallowed, because a request that cannot establish what "unchanged" means
// cannot report `code_changed` honestly, and quietly defaulting it to false is
// the failure mode this exists to prevent.
func (s *Store) ensureBaseline(requestID string) error {
	st, err := s.State(requestID)
	if err != nil {
		return err
	}
	if st.BaselineDigest != "" {
		return nil
	}
	_, err = s.RecordBaseline(requestID, s.now)
	return err
}

// RecordBaselineOnFirstWork measures the baseline when the request first becomes
// WORKING and has never been measured.
//
// It is a no-op on every later call. The first WORKING transition is the only
// moment at which "the content before the implementer's changes" is knowable
// without having to guess when the work happened.
func (s *Store) RecordBaselineOnFirstWork(requestID string) (string, error) {
	st, err := s.State(requestID)
	if err != nil {
		return "", err
	}
	if st.BaselineDigest != "" {
		return st.BaselineDigest, nil
	}
	if st.Stage != StageWorking {
		// Not working yet: there is nothing to compare against.
		return "", nil
	}
	return s.RecordBaseline(requestID, s.now)
}
