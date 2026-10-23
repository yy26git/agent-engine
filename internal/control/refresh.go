package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// The production replacement driver.
//
// `generation.go` declares the six phases and refuses each one out of order. What
// it could not do was RUN them: every phase transition existed as a helper that
// tests called directly, and nothing in the engine ever reached them. This file is
// that driver, and the difference matters — a helper proves the transition is
// legal, while a driver proves the transition actually happens.
//
// The protocol, in the order it is executed:
//
//	INTENT → HANDOFF → CANDIDATE → (prove quiescence) → BINDING → ORIENTATION → ACTIVE
//
// Two rules carry the weight:
//
//   - The retired worker's write grant is revoked BEFORE the candidate is bound,
//     and quiescence is PROVED before the binding commits. Committing a binding
//     over a worker that may still be writing leaves two mutators on one
//     workspace, which is the failure this whole mechanism exists to prevent.
//   - The lifetime repair and guidance counters survive the rotation. A refresh
//     changes who is working, not what has already been spent.

// ReplaceResult is what a replacement attempt achieved.
type ReplaceResult struct {
	// Phase is how far the replacement got.
	Phase GenerationPhase
	// RetiredSession and CandidateSession are the two identities involved.
	RetiredSession   string
	CandidateSession string
	// HandoffDigest is the committed handoff body.
	HandoffDigest string
	// OrientationEffectID is the durable instruction that oriented the new worker.
	OrientationEffectID string
	// Quiesced is true only when the retired session was PROVEN stopped.
	Quiesced bool
	// Active is true when the new generation is running.
	Active bool
	// Blocked explains a refusal, with the reason.
	Blocked string
}

// DriveReplacement runs the replacement protocol as far as it can, one call at a
// time.
//
// It is deliberately incremental rather than a single blocking sequence. Each call
// performs one phase's durable work and returns; a caller retries. That is what
// makes a crash recoverable, because the phase recorded in state is the only thing
// that has to survive for the next call to know where it was.
func (e *Engine) DriveReplacement(ctx context.Context, requestID string) (ReplaceResult, error) {
	var res ReplaceResult

	st, err := e.store.State(requestID)
	if err != nil {
		return res, err
	}
	res.Phase = st.Replacement.Phase
	res.RetiredSession = st.Replacement.RetiredSession
	res.CandidateSession = st.Replacement.CandidateSession
	res.HandoffDigest = st.Replacement.HandoffDigest
	res.OrientationEffectID = st.Replacement.OrientationEffectID
	res.Quiesced = st.Replacement.Quiesced

	// Nothing to drive.
	if st.Replacement.Phase == "" {
		res.Blocked = "no replacement is in progress"
		return res, nil
	}
	// Already done.
	if st.Replacement.Phase == GenActive {
		res.Active = true
		return res, nil
	}

	now := e.now()

	switch st.Replacement.Phase {

	case GenIntent:
		// A handoff must exist and validate before anything else happens. The
		// handoff body is committed, not constructed in memory, so the bytes a new
		// worker reads are the bytes that were reviewed.
		h := e.buildHandoff(st)
		if err := h.Validate(st.TaskID, st.Generation); err != nil {
			res.Blocked = err.Error()
			return res, nil
		}
		body := renderHandoff(h)
		digest, err := e.store.CommitReport(requestID, st.Replacement.HandoffReportID(), []byte(body),
			func(cur *State, dg string) error { return nil })
		if err != nil {
			return res, err
		}
		if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
			return cur.RecordHandoff(h, digest, now)
		}); err != nil {
			return res, err
		}
		res.Phase = GenHandoff
		res.HandoffDigest = digest

	case GenHandoff:
		// Create the candidate. Its identity is recorded before anything starts it,
		// so a crash here leaves a candidate to reconcile rather than an anonymous
		// process.
		label := ActivationLabel(requestID, st.TaskID, "worker", st.Replacement.IncomingGeneration, "refresh")
		spec := agentdeck.CreateSpec{Title: label, Path: st.Workspace, Tool: e.cfg.Tool}
		if pinned, ok := st.ExecutionProfiles[RoleWorker]; ok {
			var err error
			spec, err = pinned.Profile().CreateSpec(st.Workspace, label)
			if err != nil {
				return res, err
			}
		} else {
			for _, pinned := range st.PinnedProfiles {
				if pinned.Role == RoleWorker {
					var err error
					spec, err = pinned.Profile().CreateSpec(st.Workspace, label)
					if err != nil {
						return res, err
					}
					break
				}
			}
		}
		sess, cerr := e.runtime.Create(ctx, spec)
		if cerr != nil {
			return res, fmt.Errorf("agent-engine: the replacement session could not be created: %w", cerr)
		}
		if strings.TrimSpace(sess.ID) == "" {
			// An unknown create outcome is never retried. It may exist, and a
			// second create is how two workers end up on one workspace.
			res.Blocked = "the runtime reported a created session with no ID; it may exist and must be " +
				"reconciled by label " + label + " rather than created again"
			return res, nil
		}
		if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
			// The old worker's write grant is revoked FIRST. A candidate is not
			// granted anything until the old writer is gone.
			cur.Checkpoint.MutationGrantsPaused = true
			return cur.RecordCandidate(sess.ID, now)
		}); err != nil {
			return res, err
		}
		res.Phase = GenCandidate
		res.CandidateSession = sess.ID

	case GenCandidate:
		// Quiescence is proved, not prompted. A Stop that returns is a request; an
		// Inspect that still finds the session alive is the fact that matters.
		retired := st.Replacement.RetiredSession
		if retired != "" {
			// The retired worker is released through the ONE teardown path, scoped
			// to its own session.
			//
			// Two things are deliberately different here. The PROCESS is CHECKED but
			// NOT TERMINATED: one ACP process carries the worker, the auditor and
			// the checker together, so killing it to retire the worker would kill
			// the reviewer about to judge the replacement. It must still be
			// checked, because a retired worker that is still writing is two
			// writers on one workspace — so "verify, do not stop" is the scoped
			// form, and the same code proves it either way.
			term, terr := e.teardown(ctx, requestID, terminationScope{
				kind:            TerminationReplacement,
				sessions:        []string{retired},
				stopSessions:    true,
				verifyProcesses: true,
			})
			if terr != nil {
				res.Blocked = fmt.Sprintf("the retired worker %s could not be reconciled: %v", retired, terr)
				return res, nil
			}
			if !term.Quiesced {
				// Quiescence is proved, not prompted. A stop that returns is a
				// request; an observation that still finds the retired worker
				// alive is the fact that decides whether a new generation may be
				// bound over it.
				res.Blocked = fmt.Sprintf("the retired worker %s is not proven stopped, so the new "+
					"generation must not be bound: %s", retired, term.Reason())
				return res, nil
			}
			if len(term.Stale) > 0 {
				// Released, but it vanished rather than finished. That is worth
				// saying out loud, because a stale worker and a retired one are
				// different facts about the same session id.
				e.logf("replacement: the retired worker %s was stale rather than closed cleanly", retired)
			}
		}
		if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
			if err := cur.Quiesce(now); err != nil {
				return err
			}
			// The binding boundary is the event watermark AT the moment of the swap.
			// Everything at or before it belongs to the retired worker, and the frame
			// handler refuses it — so a retired worker's late output cannot be
			// mistaken for the new generation's, which is the whole reason the
			// boundary is captured rather than left at whatever it happened to be.
			cur.BindingBoundary = cur.BusCursor
			cur.CursorSet = true
			return cur.CommitBinding(now)
		}); err != nil {
			return res, err
		}
		res.Phase = GenBinding
		res.Quiesced = true

	case GenBinding:
		// Orient the new worker. The instruction is recorded as a durable effect
		// BEFORE the session starts, so a crash cannot lose the orientation.
		msg := e.orientationMessage(requestID, st)
		var effectID string
		if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
			if err := cur.RecordEffect(Effect{
				SessionID: cur.WorkerSession,
				Role:      "worker",
				Message:   msg,
				Reason:    "orientation for a fresh generation; the handoff is a claim, not evidence",
				CreatedAt: now.UTC().Format(time.RFC3339),
			}); err != nil {
				return err
			}
			for _, eff := range cur.Effects {
				if eff.SessionID == cur.WorkerSession && eff.Message == msg {
					effectID = eff.EffectID
				}
			}
			return cur.RecordOrientation(effectID, now)
		}); err != nil {
			return res, err
		}
		res.Phase = GenOrientation
		res.OrientationEffectID = effectID
		// The new generation starts only now — after orientation, never before.
		if serr := e.runtime.Start(ctx, st.Replacement.CandidateSession); serr != nil {
			return res, fmt.Errorf("agent-engine: the new generation could not be started: %w", serr)
		}

	case GenOrientation:
		if _, err := e.store.UpdateCommitted(requestID, func(cur *State) error {
			cur.Checkpoint.MutationGrantsPaused = false
			return cur.FinishReplacement(now)
		}); err != nil {
			return res, err
		}
		res.Phase = GenActive
		res.Active = true
	}

	return res, nil
}

// HandoffReportID is the immutable report name for a replacement's handoff.
func (r Replacement) HandoffReportID() string {
	return "handoff-g" + itoa(r.OutgoingGeneration) + "-to-g" + itoa(r.IncomingGeneration)
}

// buildHandoff renders the durable state a fresh worker needs.
//
// It is built from committed facts, never from the outgoing worker's own account
// of what it did. The handoff tells the new worker where to look; it does not
// vouch for what it finds there.
func (e *Engine) buildHandoff(s *State) Handoff {
	var open []string
	for _, o := range s.Obligations {
		open = append(open, fmt.Sprintf("%s (%s): %s", o.ID, o.Disposition, o.Criterion))
	}
	if len(open) == 0 {
		open = []string{"none recorded"}
	}
	var failed []string
	for _, f := range s.Failures {
		failed = append(failed, f.CheckID+": "+f.Signature)
	}
	if len(failed) == 0 {
		failed = []string{"none recorded"}
	}
	return Handoff{
		TaskID:      s.TaskID,
		Generation:  s.Generation,
		Objective:   s.StageReason,
		Status:      fmt.Sprintf("generation %d is being replaced at phase %s", s.Generation, s.Replacement.Phase),
		OpenWork:    strings.Join(open, "; "),
		NextAction:  "re-inspect the repository, verify the handoff's claims against it, then continue from the first unmet obligation",
		DoneSoFar:   fmt.Sprintf("%d repair cycle(s) and %d diagnostic round(s) have been spent", s.RepairCycles, s.DiagnosticRounds),
		FailedPaths: strings.Join(failed, "; "),
	}
}

// renderHandoff renders the immutable handoff body.
func renderHandoff(h Handoff) string {
	var b strings.Builder
	b.WriteString("# Handoff\n\n")
	fmt.Fprintf(&b, "Task: %s\nGeneration: %d\n\n", h.TaskID, h.Generation)
	for _, sec := range []struct{ name, body string }{
		{"Objective", h.Objective},
		{"Status", h.Status},
		{"Open work", h.OpenWork},
		{"Next action", h.NextAction},
		{"Done so far", h.DoneSoFar},
		{"Failed paths", h.FailedPaths},
	} {
		fmt.Fprintf(&b, "## %s\n%s\n\n", sec.name, sec.body)
	}
	b.WriteString("<!-- This handoff is a claim written by the control layer. A fresh worker must " +
		"verify it against the repository; nothing here is evidence on its own. -->\n")
	return b.String()
}

// orientationMessage is the instruction that orients a fresh generation.
func (e *Engine) orientationMessage(requestID string, st *State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the new implementer for task %s, generation %d.\n\n", st.TaskID, st.Generation)
	fmt.Fprintf(&b, "The previous worker was retired after: %s\n\n", st.Replacement.Reason)
	fmt.Fprintf(&b, "Read the committed handoff report %s.\n\n", st.Replacement.HandoffReportID())
	b.WriteString("The handoff is a claim, not evidence. Before acting:\n")
	b.WriteString("  1. inspect the repository and confirm the current content yourself;\n")
	b.WriteString("  2. re-validate every claim in the handoff against what you find;\n")
	b.WriteString("  3. discard any claim that does not survive that check, and say so.\n\n")
	fmt.Fprintf(&b, "Lifetime counters carry over: %d repair cycle(s), %d diagnostic round(s).\n",
		st.RepairCycles, st.DiagnosticRounds)
	b.WriteString("A refresh does not buy more budget.\n")
	return b.String()
}
