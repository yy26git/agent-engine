package control

// Exact, local release of an episode's current worker. This is deliberately a
// release primitive only: it cannot choose a replacement or resume IMPLEMENT.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"syscall"
	"time"
)

var ErrWorkerReleaseRefused = errors.New("agent-engine: worker release request refused")

type workerReleaseIdentity struct {
	EpisodeID     string         `json:"episode_id"`
	Phase         string         `json:"phase"`
	Generation    int            `json:"generation"`
	Binding       SessionBinding `json:"binding"`
	ProfileSHA256 string         `json:"profile_sha256"`
	PlanSHA256    string         `json:"plan_sha256"`
	SourceSHA256  string         `json:"source_sha256"`
	ProgramSHA256 string         `json:"program_sha256,omitempty"`
	CardSHA256    string         `json:"card_sha256,omitempty"`
	Process       OwnedProcess   `json:"process"`
}

// WorkerReleaseBindingDigest returns the opaque digest an operator must pass
// back to bind a release request to the exact current worker identity.
func WorkerReleaseBindingDigest(st *State) (string, error) {
	identity, err := currentWorkerReleaseIdentity(st)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func currentWorkerReleaseIdentity(st *State) (workerReleaseIdentity, error) {
	var identity workerReleaseIdentity
	if st == nil || st.Episode == nil || st.Episode.Phase != string(PhaseImplement) ||
		!st.Episode.PlanAuthorized || st.Stage.Terminal() || st.Episode.Stop != nil {
		return identity, fmt.Errorf("%w: episode must be active at IMPLEMENT with its plan authorized", ErrWorkerReleaseRefused)
	}
	if st.ActiveReview != nil {
		return identity, fmt.Errorf("%w: an independent review is still open", ErrWorkerReleaseRefused)
	}
	if len(st.Episode.PlanDigest) != 64 || st.Episode.PlanBaseline == nil || len(st.Episode.PlanBaseline.ContentDigest) != 64 {
		return identity, fmt.Errorf("%w: the accepted plan or its measured source baseline is missing or malformed", ErrWorkerReleaseRefused)
	}
	binding, ok := st.BackendFor(RoleWorker)
	if !ok || !binding.Resolved {
		return identity, fmt.Errorf("%w: the current worker binding is missing or unresolved", ErrWorkerReleaseRefused)
	}
	if binding.Generation != 0 && binding.Generation != st.Generation {
		return identity, fmt.Errorf("%w: worker binding generation %d does not match current generation %d", ErrWorkerReleaseRefused, binding.Generation, st.Generation)
	}
	profile, ok := st.ExecutionProfiles[RoleWorker]
	if !ok || profile.Name == "" {
		return identity, fmt.Errorf("%w: pinned worker profile is missing", ErrWorkerReleaseRefused)
	}
	process := st.OwnedProcesses[binding.Backend]
	if process.Pending {
		return identity, fmt.Errorf("%w: worker process has an unresolved pre-spawn intent", ErrWorkerReleaseRefused)
	}
	if binding.ProcessPID != 0 {
		if process.PID != binding.ProcessPID || process.StartToken == "" || binding.ProcessStart == "" || process.StartToken != binding.ProcessStart {
			return identity, fmt.Errorf("%w: recorded worker process identity is incomplete or inconsistent", ErrWorkerReleaseRefused)
		}
	}
	identity = workerReleaseIdentity{
		EpisodeID: st.Episode.EpisodeID, Phase: st.Episode.Phase,
		Generation: st.Generation, Binding: binding,
		ProfileSHA256: profileDigest(profile.Profile()), PlanSHA256: st.Episode.PlanDigest,
		SourceSHA256: st.Episode.PlanBaseline.ContentDigest,
		Process:      process,
	}
	if st.ProgramBinding != nil {
		identity.ProgramSHA256 = st.ProgramBinding.ProgramDigest
		identity.CardSHA256 = st.ProgramBinding.CardDigest
	}
	return identity, nil
}

// RequestEpisodeWorkerRelease commits a release intent after checking the
// operator's digest against the current exact binding. It is idempotent for the
// same outstanding request and blocks only never-attempted worker effects.
func (s *Store) RequestEpisodeWorkerRelease(requestID, expectedDigest string) (*EpisodeWorkerRelease, error) {
	if expectedDigest == "" {
		return nil, fmt.Errorf("%w: --expected-binding-sha256 is required", ErrWorkerReleaseRefused)
	}
	var result *EpisodeWorkerRelease
	_, err := s.UpdateCommitted(requestID, func(st *State) error {
		identity, err := currentWorkerReleaseIdentity(st)
		if err != nil {
			return err
		}
		body, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		measured := hex.EncodeToString(sum[:])
		if measured != expectedDigest {
			return fmt.Errorf("%w: supplied digest does not match the current worker binding (current %s)", ErrWorkerReleaseRefused, measured)
		}
		if existing := st.EpisodeWorkerRelease; existing != nil {
			if existing.BindingSHA256 != measured || existing.EpisodeID != identity.EpisodeID {
				return fmt.Errorf("%w: a different release request is already recorded", ErrWorkerReleaseRefused)
			}
			result = existing
			return nil
		}
		release := &EpisodeWorkerRelease{
			Status: WorkerReleaseRequested, RequestedAt: s.now().UTC().Format(time.RFC3339Nano),
			BindingSHA256: measured, EpisodeID: identity.EpisodeID, Phase: identity.Phase,
			Generation: identity.Generation, Backend: identity.Binding.Backend,
			SessionID: identity.Binding.SessionID, Profile: st.ExecutionProfiles[RoleWorker].Name,
			ProfileSHA256: identity.ProfileSHA256, PlanSHA256: identity.PlanSHA256,
			SourceSHA256: identity.SourceSHA256, ProgramSHA256: identity.ProgramSHA256, CardSHA256: identity.CardSHA256,
			ProcessPID: identity.Process.PID, ProcessStart: identity.Process.StartToken,
		}
		st.EpisodeWorkerRelease = release
		for i := range st.Effects {
			effect := &st.Effects[i]
			if effect.Role != string(RoleWorker) || effect.Status != EffectPending {
				continue
			}
			effect.Status = EffectBlocked
			effect.LastError = "not sent: exact worker release was requested before dispatch"
		}
		result = release
		return nil
	})
	if err != nil {
		return nil, err
	}
	copy := *result
	return &copy, nil
}

// consumeEpisodeWorkerRelease runs only in the active Engine, which owns the
// bound transport. It stops one exact session and, for an exclusive
// process-owning backend, proves that exact recorded process is gone.
func (e *Engine) consumeEpisodeWorkerRelease(ctx context.Context, requestID string) (bool, string, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return false, "", err
	}
	release := st.EpisodeWorkerRelease
	if release == nil || release.Status != WorkerReleaseRequested {
		return false, "", nil
	}
	identity, err := currentWorkerReleaseIdentity(st)
	if err != nil {
		reason, berr := e.blockWorkerRelease(requestID, err.Error())
		return true, reason, berr
	}
	body, _ := json.Marshal(identity)
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != release.BindingSHA256 {
		reason, berr := e.blockWorkerRelease(requestID, "worker binding changed after release intent; no transport was touched")
		return true, reason, berr
	}
	for role, binding := range st.BackendBindings {
		if role != RoleWorker && binding.Backend == release.Backend && binding.SessionID != "" {
			reason, berr := e.blockWorkerRelease(requestID, fmt.Sprintf("backend %s is also bound to role %s; shared process release is refused", release.Backend, role))
			return true, reason, berr
		}
	}
	backend, ref, err := e.dispatch.backendFor(requestID, release.SessionID)
	if err != nil || backend.Name() != release.Backend {
		if err == nil {
			err = fmt.Errorf("resolved backend %s differs from pinned backend %s", backend.Name(), release.Backend)
		}
		reason, berr := e.blockWorkerRelease(requestID, "bound worker transport is unavailable: "+err.Error())
		return true, reason, berr
	}
	owner, canStopProcess := backend.(processOwner)
	pid, ownsProcess := backend.ProcessIdentity()
	if release.ProcessPID != 0 {
		if !ownsProcess || pid != release.ProcessPID || !canStopProcess {
			reason, berr := e.blockWorkerRelease(requestID, "the live backend process identity no longer matches the recorded worker process")
			return true, reason, berr
		}
		if observedStart := pidStartToken(pid); observedStart == "" || observedStart != release.ProcessStart {
			reason, berr := e.blockWorkerRelease(requestID, "the backend pid does not carry the recorded process start token; refusing to signal a possibly reused pid")
			return true, reason, berr
		}
	} else if ownsProcess {
		reason, berr := e.blockWorkerRelease(requestID, "the worker transport owns a process whose identity was not pinned in the release request")
		return true, reason, berr
	}
	if err := backend.Shutdown(ctx, ref); err != nil {
		reason, berr := e.blockWorkerRelease(requestID, "worker session shutdown was not confirmed: "+err.Error())
		return true, reason, berr
	}
	if verdict := e.classifySession(ctx, backend, ref); verdict != verdictClosed {
		reason, berr := e.blockWorkerRelease(requestID, fmt.Sprintf("worker session did not report closed after shutdown (state %s)", verdict))
		return true, reason, berr
	}
	proof := "bound session reported closed"
	if release.ProcessPID != 0 {
		if err := owner.StopOwnedProcesses(ctx); err != nil {
			reason, berr := e.blockWorkerRelease(requestID, "owned worker process stop failed: "+err.Error())
			return true, reason, berr
		}
		if reporter, ok := backend.(unreapReporter); ok && len(reporter.UnreapedProcesses()) != 0 {
			reason, berr := e.blockWorkerRelease(requestID, "the transport reported surviving descendant processes after worker shutdown")
			return true, reason, berr
		}
		alive, known := recordedProcessAlive(release.ProcessPID, release.ProcessStart)
		if alive || !known {
			reason, berr := e.blockWorkerRelease(requestID, "recorded worker process is not proven gone after shutdown")
			return true, reason, berr
		}
		if isACPBackendName(release.Backend) && !errors.Is(syscall.Kill(-release.ProcessPID, 0), syscall.ESRCH) {
			reason, berr := e.blockWorkerRelease(requestID, "the recorded ACP process group is not proven empty after shutdown")
			return true, reason, berr
		}
		proof = fmt.Sprintf("session closed; backend process %d with the recorded start token is gone", release.ProcessPID)
	}
	_, err = e.store.UpdateCommitted(requestID, func(cur *State) error {
		if cur.EpisodeWorkerRelease == nil || cur.EpisodeWorkerRelease.BindingSHA256 != release.BindingSHA256 || cur.EpisodeWorkerRelease.Status != WorkerReleaseRequested {
			return fmt.Errorf("worker release intent changed while shutdown was in progress")
		}
		cur.EpisodeWorkerRelease.Status = WorkerReleaseReleased
		cur.EpisodeWorkerRelease.CompletedAt = e.stamp()
		cur.EpisodeWorkerRelease.QuiescenceProof = proof
		delete(cur.OwnedProcesses, release.Backend)
		return nil
	})
	if err != nil {
		return true, "", err
	}
	e.logf("episode %s worker session released; replacement remains blocked", release.EpisodeID)
	return true, "worker released; pinned profile and binding are retained, and replacement remains unavailable", nil
}

// recoverEpisodeWorkerReleaseAfterReap closes the crash window between stopping
// a process-owning worker and writing its release receipt. It is called only
// after ReapOrphanedProcesses has established old-process quiescence.
func (e *Engine) recoverEpisodeWorkerReleaseAfterReap(ctx context.Context, requestID string) error {
	st, err := e.store.State(requestID)
	if err != nil {
		return err
	}
	release := st.EpisodeWorkerRelease
	if release == nil || release.Status != WorkerReleaseRequested {
		return nil
	}
	identity, err := currentWorkerReleaseIdentity(st)
	if err != nil {
		_, berr := e.blockWorkerRelease(requestID, err.Error())
		return berr
	}
	body, _ := json.Marshal(identity)
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != release.BindingSHA256 {
		_, berr := e.blockWorkerRelease(requestID, "worker binding changed before release recovery")
		return berr
	}
	if release.ProcessPID == 0 {
		// Agent Deck does not own a local process; inspect/shutdown the exact
		// session through the backend already held by this Engine.
		handled, reason, err := e.consumeEpisodeWorkerRelease(ctx, requestID)
		if err != nil || !handled {
			return err
		}
		latest, err := e.store.State(requestID)
		if err != nil {
			return err
		}
		if latest.EpisodeWorkerRelease == nil || latest.EpisodeWorkerRelease.Status != WorkerReleaseReleased {
			return fmt.Errorf("worker release recovery did not prove session closure: %s", reason)
		}
		return nil
	}
	alive, known := recordedProcessAlive(release.ProcessPID, release.ProcessStart)
	if !known || alive {
		_, berr := e.blockWorkerRelease(requestID, "the recorded worker process is not proven quiescent after supervisor restart")
		return berr
	}
	if isACPBackendName(release.Backend) && !errors.Is(syscall.Kill(-release.ProcessPID, 0), syscall.ESRCH) {
		_, berr := e.blockWorkerRelease(requestID, "the recorded ACP process group is not proven empty after supervisor restart")
		return berr
	}
	_, err = e.store.UpdateCommitted(requestID, func(cur *State) error {
		if cur.EpisodeWorkerRelease == nil || cur.EpisodeWorkerRelease.Status != WorkerReleaseRequested || cur.EpisodeWorkerRelease.BindingSHA256 != release.BindingSHA256 {
			return fmt.Errorf("worker release intent changed during restart recovery")
		}
		cur.EpisodeWorkerRelease.Status = WorkerReleaseReleased
		cur.EpisodeWorkerRelease.CompletedAt = e.stamp()
		cur.EpisodeWorkerRelease.QuiescenceProof = "after supervisor restart, the recorded worker pid/start token is gone"
		if isACPBackendName(release.Backend) {
			cur.EpisodeWorkerRelease.QuiescenceProof += " and the recorded ACP process group is empty"
		}
		delete(cur.OwnedProcesses, release.Backend)
		return nil
	})
	return err
}

func (e *Engine) blockWorkerRelease(requestID, reason string) (string, error) {
	_, err := e.store.UpdateCommitted(requestID, func(st *State) error {
		if st.EpisodeWorkerRelease != nil && st.EpisodeWorkerRelease.Status == WorkerReleaseRequested {
			st.EpisodeWorkerRelease.Status = WorkerReleaseBlocked
			st.EpisodeWorkerRelease.CompletedAt = e.stamp()
			st.EpisodeWorkerRelease.Reason = reason
		}
		return nil
	})
	return reason, err
}
