package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// StepAbandoned is an append-preserving disposition for a read-only producer
// interrupted by proven retirement of its locally owned ACP process. It does
// not clear the session binding or authorize a replacement.
const StepAbandoned = "ABANDONED"

// EpisodeResumeValidation is the latest source measurement made before a
// resumed episode can recover reviews or admit transports. Pre-implementation
// drift may enter the controller-owned read-only re-investigation route after
// prior read-only work settles; drift after implementation dispatch stays
// blocked until an attributable authority path exists.
type EpisodeResumeValidation struct {
	MeasuredAt               string `json:"measured_at"`
	Workspace                string `json:"workspace"`
	SnapshotID               string `json:"snapshot_id"`
	ContentSHA256            string `json:"content_sha256"`
	FileCount                int    `json:"file_count"`
	ComparedTo               string `json:"compared_to,omitempty"`
	ComparedSHA256           string `json:"compared_sha256,omitempty"`
	DriftObserved            bool   `json:"drift_observed,omitempty"`
	Blocked                  bool   `json:"blocked,omitempty"`
	Resolution               string `json:"resolution,omitempty"`
	PlanAuthorizationRevoked bool   `json:"plan_authorization_revoked,omitempty"`
	PlanSHA256               string `json:"plan_sha256,omitempty"`
	Reason                   string `json:"reason,omitempty"`
	FirstDriftAt             string `json:"first_drift_at,omitempty"`
	FirstDriftSourceSHA256   string `json:"first_drift_source_sha256,omitempty"`
	FirstDriftAgainstSHA256  string `json:"first_drift_against_sha256,omitempty"`
	FirstDriftReason         string `json:"first_drift_reason,omitempty"`
}

// RevalidateEpisodeResume first re-establishes quiescence for owned child
// processes, then measures the workspace while this Engine holds request and
// workspace ownership. It does not launch a transport, rebind a role or infer
// that an external Agent Deck session has been released.
//
// A changed pre-implementation source invalidates active report/plan evidence
// and, when prior read-only work is quiescent, starts a bounded fresh REPORT
// cycle against the newly measured source. After implementation has been
// dispatched, source changes are unattributed: the plan and history are
// preserved, and resume is blocked until an authority path can distinguish
// expected writes from an external writer.
func (e *Engine) RevalidateEpisodeResume(ctx context.Context, requestID string) (EpisodeResumeValidation, error) {
	var empty EpisodeResumeValidation
	if e == nil || e.store == nil || e.release == nil || e.opts.DryRun {
		return empty, fmt.Errorf("agent-engine: episode resume validation requires live request ownership")
	}
	if requestID != e.requestID {
		return empty, fmt.Errorf("agent-engine: episode resume validation must name the owned request")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if problems := e.ReapOrphanedProcesses(ctx, requestID); len(problems) > 0 {
		return empty, fmt.Errorf("agent-engine: resume source measurement refused before owned-process quiescence: %s", strings.Join(problems, "; "))
	}
	if err := e.recoverEpisodeWorkerReleaseAfterReap(ctx, requestID); err != nil {
		return empty, fmt.Errorf("agent-engine: worker release recovery failed: %w", err)
	}

	state, err := e.store.State(requestID)
	if err != nil {
		return empty, err
	}
	if state.Episode == nil {
		return empty, nil
	}
	if state.Episode.PlanAuthorized && state.Episode.PlanBaseline == nil && !hasDispatchedImplementation(state) {
		return empty, fmt.Errorf("agent-engine: accepted plan has no persisted source baseline; resume is refused before any transport starts")
	}
	if state.Episode.PlanAuthorized && state.Episode.PlanBaseline != nil && strings.TrimSpace(state.Episode.PlanBaseline.ContentDigest) == "" && !hasDispatchedImplementation(state) {
		return empty, fmt.Errorf("agent-engine: accepted plan source baseline is empty; resume is refused before any transport starts")
	}
	snapshot, err := measureStableEpisodeSource(state.Workspace, e.store.Config().StoreRoot, e.now())
	if err != nil {
		return empty, err
	}
	validation := EpisodeResumeValidation{
		MeasuredAt: snapshot.TakenAt, Workspace: snapshot.Root,
		SnapshotID: snapshot.SnapshotID, ContentSHA256: snapshot.ContentDigest,
		FileCount: snapshot.FileCount,
	}

	anchorKind, anchorDigest, err := resumeSourceAnchor(e.store, requestID, state)
	if err != nil {
		return empty, err
	}
	validation.ComparedTo = anchorKind
	validation.ComparedSHA256 = anchorDigest
	if anchorDigest != "" && snapshot.ContentDigest != anchorDigest {
		validation.DriftObserved = true
		validation.Blocked = true
		validation.Reason = "workspace source changed since the saved evidence anchor; resume is blocked because the change has not been attributed"
		if !hasDispatchedImplementation(state) && state.Episode.PlanAuthorized && state.Episode.PlanBaseline != nil && anchorKind == "accepted_plan_baseline" {
			validation.PlanAuthorizationRevoked = true
			validation.PlanSHA256 = state.Episode.PlanDigest
			validation.Reason = "workspace source changed after plan acceptance and before implementation; the exact plan authorization was revoked and no worker was launched"
		}
	}

	previous := state.EpisodeResumeValidation
	if previous != nil {
		validation.FirstDriftAt = previous.FirstDriftAt
		validation.FirstDriftSourceSHA256 = previous.FirstDriftSourceSHA256
		validation.FirstDriftAgainstSHA256 = previous.FirstDriftAgainstSHA256
		validation.FirstDriftReason = previous.FirstDriftReason
	}
	previousDriftWaiting := previous != nil && previous.Blocked && previous.Resolution == "awaiting_readonly_quiescence"
	stickyBlock := previous != nil && previous.Blocked && !previousDriftWaiting
	if stickyBlock {
		validation.Blocked = true
		validation.DriftObserved = previous.DriftObserved
		validation.PlanAuthorizationRevoked = previous.PlanAuthorizationRevoked
		validation.PlanSHA256 = previous.PlanSHA256
		validation.Resolution = previous.Resolution
		validation.Reason = previous.Reason
	} else if validation.DriftObserved || previousDriftWaiting {
		if validation.FirstDriftAt == "" {
			validation.FirstDriftAt = e.now().UTC().Format(time.RFC3339Nano)
			validation.FirstDriftSourceSHA256 = snapshot.ContentDigest
			validation.FirstDriftAgainstSHA256 = anchorDigest
			validation.FirstDriftReason = validation.Reason
		}
		if previousDriftWaiting && !validation.DriftObserved {
			// The source returned to its prior bytes while stale read-only work was
			// settling. Its earlier drift still invalidated the accepted evidence, so
			// start a fresh investigation against the current measured bytes.
			validation.DriftObserved = true
			validation.Reason = previous.Reason
		}
		if hasDispatchedImplementation(state) {
			validation.Blocked = true
			validation.Resolution = "blocked_implementation_drift"
			validation.PlanAuthorizationRevoked = false
			validation.Reason = "workspace source changed after implementation was dispatched; attribution is unproven, so resume remains blocked and the accepted plan and execution history are preserved"
		} else {
			validation.PlanAuthorizationRevoked = state.Episode.PlanAuthorized
			validation.PlanSHA256 = state.Episode.PlanDigest
			validation.Reason = "workspace source drift invalidated the current report and plan; read-only re-investigation is required before any implementation"
			validation.Resolution = "awaiting_readonly_quiescence"
		}
	}
	if validation.DriftObserved && validation.FirstDriftAt == "" {
		validation.FirstDriftAt = e.now().UTC().Format(time.RFC3339Nano)
		validation.FirstDriftSourceSHA256 = snapshot.ContentDigest
		validation.FirstDriftAgainstSHA256 = anchorDigest
		validation.FirstDriftReason = validation.Reason
	}

	// Persist the recovery gate before examining an old producer. This keeps all
	// normal artifact intake and phase advancement blocked while the one narrowly
	// scoped recovery path attempts retained ACP receipts.
	if validation.Resolution == "awaiting_readonly_quiescence" {
		if _, err := e.store.UpdateCommitted(requestID, func(current *State) error {
			if current.Revision != state.Revision || current.Episode == nil || current.Episode.EpisodeID != state.Episode.EpisodeID {
				return fmt.Errorf("agent-engine: episode changed before read-only producer recovery; retry after reading current state")
			}
			if current.EpisodeHandoverDigest != state.EpisodeHandoverDigest || current.Episode.PlanDigest != state.Episode.PlanDigest || current.Episode.PlanAuthorized != state.Episode.PlanAuthorized {
				return fmt.Errorf("agent-engine: episode authority changed before read-only producer recovery")
			}
			current.EpisodeResumeValidation = &validation
			return nil
		}); err != nil {
			return empty, err
		}
		state, err = e.store.State(requestID)
		if err != nil {
			return empty, err
		}
		recoveryReason, recoveryErr := e.recoverPendingReadonlyProducerForDrift(ctx, requestID, state)
		if recoveryErr != nil {
			return empty, recoveryErr
		}
		state, err = e.store.State(requestID)
		if err != nil {
			return empty, err
		}
		if recoveryReason != "" {
			validation.Blocked = true
			validation.Reason = recoveryReason
			if _, err := e.store.UpdateCommitted(requestID, func(current *State) error {
				if current.Revision != state.Revision || current.Episode == nil || current.Episode.EpisodeID != state.Episode.EpisodeID {
					return fmt.Errorf("agent-engine: episode changed while read-only producer recovery was pending")
				}
				current.EpisodeResumeValidation = &validation
				return nil
			}); err != nil {
				return empty, err
			}
			return validation, nil
		}
		state, err = e.store.State(requestID)
		if err != nil {
			return empty, err
		}
		fresh, measureErr := measureStableEpisodeSource(state.Workspace, e.store.Config().StoreRoot, e.now())
		if measureErr != nil {
			return empty, measureErr
		}
		if fresh.ContentDigest != snapshot.ContentDigest {
			validation.Blocked = true
			validation.MeasuredAt = fresh.TakenAt
			validation.SnapshotID = fresh.SnapshotID
			validation.ContentSHA256 = fresh.ContentDigest
			validation.FileCount = fresh.FileCount
			validation.Reason = "workspace changed while the previous read-only producer was being recovered; no new baseline was pinned; rerun resume validation"
			if _, err := e.store.UpdateCommitted(requestID, func(current *State) error {
				if current.Revision != state.Revision || current.Episode == nil || current.Episode.EpisodeID != state.Episode.EpisodeID {
					return fmt.Errorf("agent-engine: episode changed while source was being remeasured after producer recovery")
				}
				current.EpisodeResumeValidation = &validation
				return nil
			}); err != nil {
				return empty, err
			}
			return validation, nil
		}
		snapshot = fresh
		validation.MeasuredAt = fresh.TakenAt
		validation.SnapshotID = fresh.SnapshotID
		validation.ContentSHA256 = fresh.ContentDigest
		validation.FileCount = fresh.FileCount
	}

	_, err = e.store.UpdateCommitted(requestID, func(current *State) error {
		if current.Revision != state.Revision || current.Episode == nil || current.Episode.EpisodeID != state.Episode.EpisodeID {
			return fmt.Errorf("agent-engine: episode changed during resume source measurement; retry after reading current state")
		}
		if current.EpisodeHandoverDigest != state.EpisodeHandoverDigest || current.Episode.PlanDigest != state.Episode.PlanDigest || current.Episode.PlanAuthorized != state.Episode.PlanAuthorized {
			return fmt.Errorf("agent-engine: episode authority changed during resume source measurement; refusing a stale validation commit")
		}
		if validation.Resolution == "awaiting_readonly_quiescence" {
			if canRestartEpisodeForSourceDrift(current) {
				event, restartErr := current.RestartForSourceDrift(snapshot, anchorDigest, validation.Reason, state.EpisodeHandoverDigest, e.now())
				if restartErr == nil {
					validation.Blocked = false
					validation.Resolution = "reinvestigation_started"
					validation.Reason = fmt.Sprintf("source drift invalidated report %s and plan %s; episode returned to read-only REPORT on measured snapshot %s; a new report, review, plan and local acceptance are required before implementation", brief(event.InvalidatedReportSHA256), brief(event.InvalidatedPlanSHA256), event.NewSourceSnapshotID)
				} else {
					validation.Blocked = true
					validation.Resolution = "awaiting_readonly_quiescence"
					validation.Reason = restartErr.Error()
				}
			}
		}
		current.EpisodeResumeValidation = &validation
		return nil
	})
	if err != nil {
		return empty, err
	}
	return validation, nil
}

// recoverPendingReadonlyProducerForDrift first attempts any durable, exact ACP
// turn receipt. If none can produce a valid artifact, the old ACP step may be
// abandoned only when the bound, recorded ACP process and its process group are
// positively gone. Agent Deck owns remote sessions outside this process; without
// a correlated completion artifact this function leaves them bound and blocked.
func (e *Engine) recoverPendingReadonlyProducerForDrift(ctx context.Context, requestID string, state *State) (string, error) {
	if state == nil || state.Episode == nil || state.EpisodeResumeValidation == nil ||
		state.EpisodeResumeValidation.Resolution != "awaiting_readonly_quiescence" || !state.EpisodeResumeValidation.DriftObserved {
		return "", fmt.Errorf("agent-engine: read-only producer recovery requires a persisted source-drift gate")
	}
	for _, original := range state.Episode.Execution.Steps {
		if (original.Phase != string(PhaseReport) && original.Phase != string(PhasePlan)) || original.Status != StepDispatched || original.ArtifactDigest != "" {
			continue
		}
		current, err := e.store.State(requestID)
		if err != nil {
			return "", err
		}
		step, ok := current.episodeStep(original.Key)
		if !ok || step.Status != StepDispatched || step.ArtifactDigest != "" {
			continue
		}
		binding, bound := current.BackendForID(step.Session)
		if !bound || !binding.Resolved {
			return fmt.Sprintf("source drift is waiting for read-only step %s: its original session %q has no resolved transport binding; no replacement was launched; restore access to that same producer or request an explicit, separately authorized recovery", step.Key, step.Session), nil
		}

		// A durable, correlated completion may survive either transport's
		// supervisor. Try the exact owed output before classifying the producer.
		var receiptErr error
		for _, receipt := range current.Episode.Execution.TurnReceipts {
			if receipt.Consumed || receipt.StepKey != step.Key || receipt.Backend != binding.Backend || receipt.Session != step.Session {
				continue
			}
			if receipt.Truncated {
				receiptErr = fmt.Errorf("retained producer receipt is truncated")
				continue
			}
			intakeErr := e.intakeEpisodeArtifactFromTurnForResumeRecovery(requestID, receipt.Backend, receipt.Session, receipt.SendID, receipt.Model, "", receipt.Text)
			if intakeErr == nil {
				if err := e.markEpisodeTurnReceiptConsumed(requestID, receipt); err != nil {
					return "", err
				}
				break
			}
			receiptErr = intakeErr
			latest, stateErr := e.store.State(requestID)
			if stateErr != nil {
				return "", stateErr
			}
			if latest.Stage.Terminal() || latest.Episode == nil {
				return "", fmt.Errorf("agent-engine: retained producer evidence reached a terminal refusal: %w", intakeErr)
			}
		}

		current, err = e.store.State(requestID)
		if err != nil {
			return "", err
		}
		step, ok = current.episodeStep(original.Key)
		if !ok || step.Status != StepDispatched || step.ArtifactDigest != "" {
			continue
		}
		if !isACPBackendName(binding.Backend) {
			next := fmt.Sprintf("source drift is waiting for the original Agent Deck %s producer session %s to provide its correlated completed artifact; keep that same chat available and let its existing supervisor persist the completion receipt, then rerun `agent-engine run --request-id %s --once`. The current CLI cannot safely import Agent Deck chat history during preflight, so without that receipt the episode remains blocked; Agent Engine will not start or rebind a replacement session", step.Phase, step.Session, requestID)
			if receiptErr != nil {
				next += "; its retained receipt was not accepted: " + receiptErr.Error()
			}
			return next, nil
		}
		if !reapedACPProducerProven(current, binding) {
			return fmt.Sprintf("source drift is waiting for proof that the original ACP producer session %s is quiescent; its recorded process/group identity is incomplete or still present, so no replacement or implementation can start", step.Session), nil
		}
		_, err = e.store.UpdateCommitted(requestID, func(s *State) error {
			if s.Revision != current.Revision || s.Episode == nil || s.Episode.EpisodeID != current.Episode.EpisodeID ||
				s.EpisodeResumeValidation == nil || s.EpisodeResumeValidation.Resolution != "awaiting_readonly_quiescence" ||
				!s.EpisodeResumeValidation.DriftObserved || s.Stage.Terminal() || s.CancellationRequested || hasDispatchedImplementation(s) {
				return fmt.Errorf("agent-engine: the episode changed before its interrupted ACP producer could be recorded")
			}
			fresh, exists := s.episodeStep(step.Key)
			if !exists || fresh.Session != step.Session || fresh.EffectID != step.EffectID || fresh.Status != StepDispatched || fresh.ArtifactDigest != "" {
				return fmt.Errorf("agent-engine: the ACP producer changed before interruption could be recorded")
			}
			fresh.Status = StepAbandoned
			fresh.Reason = "source drift recovery: the recorded ACP owner and process group were reaped; no valid correlated artifact survived; this read-only dispatch is retained as interrupted and will not be retried"
			s.replaceEpisodeStep(fresh)
			return nil
		})
		if err != nil {
			return "", err
		}
		state, err = e.store.State(requestID)
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

func (e *Engine) markEpisodeTurnReceiptConsumed(requestID string, receipt EpisodeTurnReceipt) error {
	_, err := e.store.UpdateCommitted(requestID, func(state *State) error {
		if state.Episode == nil {
			return fmt.Errorf("episode disappeared while consuming a retained producer receipt")
		}
		for i := range state.Episode.Execution.TurnReceipts {
			current := &state.Episode.Execution.TurnReceipts[i]
			if current.Backend == receipt.Backend && current.Session == receipt.Session && current.SendID == receipt.SendID {
				current.Consumed = true
				return nil
			}
		}
		return fmt.Errorf("retained producer receipt disappeared before it could be acknowledged")
	})
	return err
}

func isACPBackendName(name string) bool {
	return name == "minimax-acp" || strings.HasPrefix(name, "minimax-acp:")
}

func reapedACPProducerProven(state *State, binding SessionBinding) bool {
	if state == nil || binding.SessionID == "" || !binding.Resolved || !isACPBackendName(binding.Backend) || binding.ProcessPID <= 1 {
		return false
	}
	owned := false
	for name, process := range state.OwnedProcesses {
		if name == binding.Backend && !process.Pending && process.PID == binding.ProcessPID && process.StartToken == binding.ProcessStart {
			owned = true
			break
		}
	}
	if !owned {
		return false
	}
	if alive, known := recordedProcessAlive(binding.ProcessPID, binding.ProcessStart); !known || alive {
		return false
	}
	// ACP children are started in their own process group. The leader being gone
	// is insufficient if a child inherited the workspace and remains in that
	// group. EPERM and every result other than ESRCH stay unresolved.
	return errors.Is(syscall.Kill(-binding.ProcessPID, 0), syscall.ESRCH)
}

func resumeSourceAnchor(store *Store, requestID string, state *State) (string, string, error) {
	if state.Episode == nil {
		return "", "", nil
	}
	if state.Episode.ReinvestigationBaseline != nil {
		return "reinvestigation_baseline", state.Episode.ReinvestigationBaseline.ContentDigest, nil
	}
	if state.Episode.PlanBaseline != nil {
		return "accepted_plan_baseline", state.Episode.PlanBaseline.ContentDigest, nil
	}
	if state.EpisodeHandoverDigest != "" {
		handover, _, err := store.ReadEpisodeHandover(requestID)
		if err != nil {
			return "", "", fmt.Errorf("agent-engine: resume source anchor is unreadable: %w", err)
		}
		if snapshot := handover.Source.MeasuredSnapshot; snapshot != nil {
			root, err := canonicalPath(state.Workspace)
			if err != nil || snapshot.Root != root || len(snapshot.ContentSHA256) != 64 {
				return "", "", fmt.Errorf("agent-engine: handover source anchor does not match the admitted workspace identity")
			}
			return "latest_handover_source", snapshot.ContentSHA256, nil
		}
	}
	return "", "", nil
}

func canRestartEpisodeForSourceDrift(state *State) bool {
	if state == nil || state.Episode == nil || state.Stage.Terminal() || state.CancellationRequested || state.Episode.Stop != nil || state.ActiveReview != nil || hasDispatchedImplementation(state) {
		return false
	}
	for _, step := range state.Episode.Execution.Steps {
		if (step.Phase == string(PhaseReport) || step.Phase == string(PhasePlan)) && step.Status == StepDispatched && step.ArtifactDigest == "" {
			return false
		}
	}
	return state.Episode.Budget.Consumed.Reports < state.Episode.Budget.ReportAttempts && len(state.Episode.SourceInvalidations) < MaxEpisodeSourceInvalidations
}

func hasDispatchedImplementation(state *State) bool {
	if state == nil || state.Episode == nil {
		return false
	}
	for _, step := range state.Episode.Execution.Steps {
		if step.Phase == string(PhaseImplement) && step.Status == StepDispatched {
			return true
		}
	}
	return false
}

// measureStableEpisodeSource makes two content-only passes and fails if either
// pass cannot account for a source path or the tree changed during measurement.
// The workspace lease excludes cooperating Engine writers; the double read also
// catches ordinary external writers that ignore that advisory lease.
func measureStableEpisodeSource(workspace, storeRoot string, now time.Time) (Snapshot, error) {
	first, err := TakeSnapshot(workspace, now, storeRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent-engine: fresh resume source measurement failed: %w", err)
	}
	if err := requireCompleteEpisodeSnapshot(first, storeRoot); err != nil {
		return Snapshot{}, err
	}
	second, err := TakeSnapshot(workspace, now, storeRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent-engine: second resume source measurement failed: %w", err)
	}
	if err := requireCompleteEpisodeSnapshot(second, storeRoot); err != nil {
		return Snapshot{}, err
	}
	if first.ContentDigest != second.ContentDigest || strings.Join(first.Excluded, "\x00") != strings.Join(second.Excluded, "\x00") {
		return Snapshot{}, fmt.Errorf("agent-engine: workspace changed during resume source measurement; no worker or reviewer was started")
	}
	return second, nil
}

func requireCompleteEpisodeSnapshot(snapshot Snapshot, storeRoot string) error {
	storeRelative := ""
	if storeRoot != "" {
		root, rootErr := canonicalPath(snapshot.Root)
		store, storeErr := canonicalPath(storeRoot)
		if rootErr == nil && storeErr == nil {
			relative, relErr := filepath.Rel(root, store)
			if relErr == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				storeRelative = filepath.ToSlash(relative) + "/"
			}
		}
	}
	for _, excluded := range snapshot.Excluded {
		relative := filepath.ToSlash(strings.TrimSuffix(excluded, "/"))
		if excludedNames[filepath.Base(relative)] || (storeRelative != "" && filepath.ToSlash(excluded) == storeRelative) {
			continue
		}
		return fmt.Errorf("agent-engine: resume source measurement cannot prove complete workspace visibility; path %q was excluded", excluded)
	}
	return nil
}
