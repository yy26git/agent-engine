package control

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/runtime"
)

// Session activation under named profiles.
//
// Launching a session is the one place where this control layer creates something
// outside its own store, and it cannot be made atomic with state. So the order is
// fixed and every step is durable before the next one begins:
//
//  1. The profile is resolved and its identity PINNED into state. A later edit to
//     what "worker" means must not change how an in-flight request runs.
//  2. A create intent is committed, carrying a deterministic activation label.
//     The label exists so that a Create whose outcome is unknown can be
//     reconciled by looking for exactly that session, rather than by creating a
//     second one and hoping.
//  3. The session is created, and its CONFIRMED full ID replaces the intent.
//  4. Only then is Start issued, and only for the bound candidate.
//
// A crash between 2 and 3 leaves a PENDING intent, which is an explicit
// "possibly created, must be reconciled" rather than a silent loss. A crash
// between 3 and 4 leaves a CONFIRMED binding whose process has not started,
// which is safe to start. Nothing here replays an unknown effect.

// ActivationResult is what a launch attempt achieved.
type ActivationResult struct {
	// SessionID is the confirmed full session ID, when one was confirmed.
	SessionID string
	// Label is the deterministic activation label used.
	Label string
	// Profile is the pinned profile identity this session was launched under.
	Profile ProfileRef
	// Created is true when this call created the session.
	Created bool
	// Reconciled is true when an existing session was adopted rather than a new
	// one created, which is what an ambiguous Create must do.
	Reconciled bool
	// Started is true when Start was issued.
	Started bool
}

// LaunchProfile creates, binds and starts one session for a role.
//
// It refuses rather than degrades. An unknown profile, a role collision, a
// profile whose confinement is unproven, or a create intent that cannot be
// reconciled all return an error with the specific reason, and none of them
// starts a process.
func (e *Engine) LaunchProfile(ctx context.Context, requestID string, role ProfileRole, profileName, workspace, nonce string) (ActivationResult, error) {
	var out ActivationResult

	// Step 1: resolve and pin the profile identity.
	// The state is read for the task identity and generation the label is
	// derived from. Role isolation against the implementation lineage is checked
	// when the binding is committed, which is where the lineage is known.
	st, err := e.store.State(requestID)
	if err != nil {
		return out, err
	}
	var profile Profile
	if pinned, ok := st.ExecutionProfiles[role]; ok {
		if pinned.Name != profileName {
			return out, fmt.Errorf("role %s is pinned to profile %q", role, pinned.Name)
		}
		profile = pinned.Profile()
	} else {
		profile, err = e.cfg.ResolveProfile(profileName, role)
		if err != nil {
			return out, err
		}
	}
	if err := st.AuthorizeProfileExecution(profile, workspace, e.now()); err != nil {
		return out, err
	}
	if role == RoleWorker {
		if err := st.VerifyRepairBranch(); err != nil {
			return out, err
		}
	}
	if profile.ReadOnly && profile.Tool == "codex" {
		digest, err := e.cfg.VerifyConfinementReceipt(profile, workspace)
		if err != nil {
			return out, err
		}
		profile.Proven = true
		profile.ConfinementReceiptSHA256 = digest
	}
	ref := profile.Ref()
	label := ActivationLabel(requestID, st.TaskID, string(role), st.Generation, nonce)
	activationID := "AC-" + label
	// A naming change must not replace an already committed create intent.
	// The suffix identifies the same root/task/role/generation/nonce; retain
	// its recorded label and ledger identity so reconciliation finds the
	// original process instead of launching another writer.
	suffix := strings.TrimPrefix(label, "agentengine")
	found := false
	for _, recorded := range st.Activations {
		if recorded.Role != string(role) || !strings.HasSuffix(recorded.Label, suffix) {
			continue
		}
		if found && (label != recorded.Label || activationID != recorded.ActivationID) {
			return out, fmt.Errorf("agent-engine: multiple recorded intents match this activation; reconcile them before launching")
		}
		label, activationID, found = recorded.Label, recorded.ActivationID, true
	}

	// Step 2: commit the create intent with the deterministic label.
	if _, err := e.store.UpdateCommitted(requestID, func(s *State) error {
		if s.Stage.Terminal() {
			return fmt.Errorf("agent-engine: request is terminal as %s; no session is launched for a finished outcome", s.ResultStatus)
		}
		s.PinnedProfiles = appendUniqueProfile(s.PinnedProfiles, ref)
		return s.RecordActivation(Activation{
			ActivationID: activationID,
			Role:         string(role),
			Label:        label,
			Status:       ActivationPending,
			Revision:     s.Revision,
			CreatedAt:    s.UpdatedAt,
		})
	}); err != nil {
		return out, err
	}

	// Step 3: reconcile any existing session carrying this label before creating
	// a new one. Creating first and reconciling later is how a crash becomes a
	// duplicate worker.
	existing, err := e.reconcileActivation(ctx, requestID, role, profile, label)
	if err != nil {
		e.markActivation(requestID, activationID, ActivationBlocked, err.Error())
		return out, err
	}
	if existing != "" {
		if err := e.bindActivation(requestID, activationID, role, existing, label, profile, ref, 0); err != nil {
			return out, err
		}
		out.SessionID = existing
		out.Label = label
		out.Profile = ref
		out.Reconciled = true
		if e.needsExplicitStart(profile) {
			observed, ierr := e.runtime.Inspect(ctx, existing)
			if ierr != nil {
				return out, fmt.Errorf("cannot inspect reconciled session: %w", ierr)
			}
			if observed.Status == "running" || observed.Status == "waiting" {
				out.Started = true
				return out, nil
			}
			if err := e.runtime.Start(ctx, existing); err != nil {
				return out, fmt.Errorf("agent-engine: the reconciled session %s could not be started: %w", existing, err)
			}
		}
		out.Started = true
		return out, nil
	}

	created, err := e.createProfileSession(ctx, requestID, activationID, role, profile, label, workspace)
	if err != nil {
		e.markActivation(requestID, activationID, ActivationBlocked, err.Error())
		return out, err
	}
	out.SessionID = created
	out.Label = label
	out.Profile = ref
	out.Created = true

	// Step 4: start only the bound candidate.
	if e.needsExplicitStart(profile) {
		if err := e.runtime.Start(ctx, created); err != nil {
			return out, fmt.Errorf("agent-engine: session %s was created and bound but could not be started: %w", created, err)
		}
	}
	out.Started = true
	return out, nil
}

// needsExplicitStart reports whether a Start call is still owed after Activate.
//
// Agent Deck's Start begins a process that Create only registered. A native ACP
// session is live from the moment session/new returns, so issuing Start there
// would be a second lifecycle action against a session that is already running.
func (e *Engine) needsExplicitStart(profile Profile) bool {
	return profile.TransportFor() == TransportAgentDeck
}

// backendForProfile resolves the transport a profile declares.
//
// A profile naming a transport this engine was not given is REFUSED. Falling back
// to the default transport would run admitted work on a backend the operator did
// not approve for it, which is a worse failure than refusing to start.
func (e *Engine) backendForProfile(profile Profile) (runtime.Backend, error) {
	name := string(profile.TransportFor())
	if profile.TransportFor() == TransportAgentDeck {
		return agentdeck.NewBackend(e.runtime), nil
	}
	// ACP permissions are process-scoped. Production gives every profile its
	// own process; legacy single-backend fixtures remain supported.
	if b, ok := e.backends[name+":"+profile.Name]; ok {
		return b, nil
	}
	b, ok := e.backends[name]
	if !ok {
		return nil, fmt.Errorf("agent-engine: profile %q declares transport %q, which this engine does not have; "+
			"refusing to run it on a transport that was not approved for it", profile.Name, name)
	}
	return b, nil
}

// reconcileActivation resolves whether an activation already happened.
//
// Agent Deck reconciles by inventory: a session carrying the deterministic label
// is found and adopted. ACP has no inventory — this layer owns exactly the
// process it spawned — so it reconciles from COMMITTED ACTIVATION INTENT instead.
// Inventing an inventory for it would be pretending a capability that does not
// exist, and "cannot find it" must never be read as "it is absent".
func (e *Engine) reconcileActivation(ctx context.Context, requestID string, role ProfileRole, profile Profile, label string) (string, error) {
	if profile.TransportFor() == TransportAgentDeck {
		return e.findSessionByLabel(ctx, label)
	}

	// Quiescence of a previous run is established BEFORE the backend is asked
	// whether the label is free, because a backend that owns one process cannot
	// see an orphan from an earlier one. Asking first and checking afterwards
	// would mean acting on an answer the backend was never entitled to give.
	st, err := e.store.State(requestID)
	if err != nil {
		return "", err
	}
	priorQuiesced := true
	if prev, ok := st.BackendBindings[role]; ok && prev.ProcessPID > 0 {
		alive, known := recordedProcessAlive(prev.ProcessPID, prev.ProcessStart)
		if !known {
			return "", fmt.Errorf("agent-engine: the previous %s process %d for role %s could not be identified, "+
				"so it is not proven gone; refusing to create a second session", prev.Backend, prev.ProcessPID, role)
		}
		if alive {
			return "", fmt.Errorf("agent-engine: the previous %s process %d for role %s is still running, so this "+
				"activation is not quiesced; creating another session would put two writers on one workspace. "+
				"Stop process %d or resolve it deliberately", prev.Backend, prev.ProcessPID, role, prev.ProcessPID)
		}
	}

	// A live session on THIS process is adopted directly: there is nothing to
	// reload and no earlier process to reason about.
	backend, err := e.backendForProfile(profile)
	if err != nil {
		return "", err
	}
	ref, found, rerr := backend.Reconcile(ctx, label, priorQuiesced)
	if rerr != nil {
		return "", rerr
	}
	if found {
		return ref.ID, nil
	}
	return "", nil
}

// ProcessIdentity is what makes a recorded pid meaningful across time.
//
// A pid alone proves only that SOMETHING holds that number. On a long-lived
// machine the original process exits and the number is reused, and a check that
// reads "still running" is then reporting on an unrelated process. That is
// conservative in the safe direction — it blocks work forever — but it is still
// wrong, and a caller who later relaxes it in order to make progress gets two
// writers on one workspace.
type ProcessIdentity struct {
	PID int
	// StartToken is the process's own start time. A reused pid has a different
	// one, which is what makes reuse DETECTABLE rather than merely possible.
	StartToken string
	// Executable is a second, independent witness.
	Executable string
}

// readProcessIdentity reads the identity of a live process.
//
// `ps` is used rather than /proc so the same code works on both supported
// platforms. An unreadable identity is an ERROR, never an empty success: a
// caller that treated "I could not tell" as "it is different" would allow a
// duplicate worker on exactly the evidence that was missing.
func readProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("agent-engine: %d is not a process id", pid)
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=,comm=").Output()
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("agent-engine: the identity of process %d could not be read: %w", pid, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ProcessIdentity{}, fmt.Errorf("agent-engine: process %d has no identity, which means it is gone", pid)
	}
	id := ProcessIdentity{PID: pid, Executable: strings.TrimSpace(line)}
	// lstart is fixed-width ("Fri Oct 10 09:12:33 2025"), so the executable is
	// whatever follows the trailing year.
	if i := strings.LastIndex(line, " "); i >= 0 {
		id.StartToken = strings.TrimSpace(line[:i])
	}
	return id, nil
}

// sameProcess reports whether a recorded identity still describes a live process.
//
// The three answers are deliberately distinct:
//   - same token  -> the original process is still running
//   - different   -> the pid was reused, so the original is provably GONE
//   - unreadable  -> unknown, and unknown is treated as alive
func sameProcess(rec ProcessIdentity) (alive bool, known bool) {
	// Existence is checked FIRST, and the order is load-bearing.
	//
	// `ps` cannot read the identity of a process that is gone, so consulting the
	// identity first maps a definitively dead process to "cannot tell", which the
	// conservative rule then reports as ALIVE. A successfully reaped orphan would
	// be reported as still running forever.
	//
	// A dead pid is a DEFINITE answer — the original is gone, whatever now holds
	// the number.
	if exists, err := processAlive(rec.PID); err == nil && !exists {
		return false, true
	}
	cur, err := readProcessIdentity(rec.PID)
	if err != nil {
		// It exists but its identity is unreadable. Treating that as "gone" is
		// the one answer that could produce two workers on one workspace.
		return true, false
	}
	if rec.StartToken != "" && cur.StartToken != "" && rec.StartToken != cur.StartToken {
		return false, true
	}
	if rec.Executable != "" && cur.Executable != "" &&
		rec.Executable != cur.Executable && rec.StartToken == "" {
		return false, true
	}
	return true, true
}

// processAlive reports whether a process id still exists.
//
// Signal 0 performs the existence and permission checks without delivering
// anything. EPERM means "it exists and is not ours", which is still alive: a
// process we may not signal is a process still holding whatever it holds, and
// reading it as dead is exactly the error that produces a duplicate worker.
func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, err
	}
}

// backendPid reports the owned process id of a backend, or 0 when it owns none.
func backendPid(b runtime.Backend) int {
	pid, owned := b.ProcessIdentity()
	if !owned {
		return 0
	}
	return pid
}

// pidStartToken captures a process's start token, or "" when it cannot be read.
//
// An empty token records missing identity evidence. A later existence check may
// prove the pid gone, but a live pid alone cannot authorize signalling it.
// StartTokenFor exposes the measured start token for a process.
//
// It exists so a CALLER that has just spawned something can record its identity
// at the moment it exists, rather than later and outside the window in which a
// crash would orphan it.
func StartTokenFor(pid int) string { return pidStartToken(pid) }

func pidStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	id, err := readProcessIdentity(pid)
	if err != nil {
		return ""
	}
	return id.StartToken
}

// recordedProcessAlive reports whether a RECORDED binding's process is still the
// one that was bound.
//
// It answers "is the original worker still running", which is not the same
// question as processAlive. A reused pid means the original is provably gone and
// work may proceed; an unreadable identity means unknown, and unknown is alive.
func recordedProcessAlive(pid int, startToken string) (alive bool, known bool) {
	if startToken == "" {
		a, err := processAlive(pid)
		if err != nil {
			return true, false
		}
		if a {
			return true, false
		}
		return false, true
	}
	return sameProcess(ProcessIdentity{PID: pid, StartToken: startToken})
}

// PrepareTaskSessions activates the admitted selections before supervision.
// Legacy requests without selections keep the explicit manual binding path.
func (e *Engine) PrepareTaskSessions(ctx context.Context, requestID string) error {
	st, err := e.store.State(requestID)
	if err != nil {
		return err
	}
	if st.Stage.Terminal() || st.CancellationRequested || len(st.ExecutionProfiles) == 0 {
		return nil
	}
	// Episodes activate their current phase's role through ensureEpisodeSession.
	// The legacy workflow launcher would require unadmitted roles and send the
	// worker an instruction before a plan has been accepted.
	if st.Episode != nil {
		// Episodes launch unbound roles lazily after their phase permits it, but
		// a retained binding must still establish readiness on resume.
		for _, bound := range boundEpisodeProfiles(st) {
			if err := st.AuthorizeProfileExecution(bound.ref.Profile(), st.Workspace, e.now()); err != nil {
				return err
			}
			binding, ok := st.BackendFor(bound.role)
			if !ok {
				continue
			}
			backend, ref, err := e.dispatch.backendFor(requestID, binding.SessionID)
			if err != nil {
				return err
			}
			observed, err := backend.Inspect(ctx, ref)
			if err != nil {
				return fmt.Errorf("episode %s readiness: %w", bound.role, err)
			}
			if status := strings.ToLower(observed.Status); status != "running" && status != "waiting" {
				if err := ensureSessionRunning(ctx, backend, ref); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if e.opts.DryRun {
		return fmt.Errorf("profile activation is unavailable in dry-run")
	}
	cap, err := e.store.PreflightCapability(requestID)
	if err != nil {
		return err
	}
	if !cap.Allowed {
		return fmt.Errorf("capability preflight refused: %s", cap.Reason)
	}
	if cap.Capability == CapabilityReadOnly {
		p := st.ExecutionProfiles[RoleWorker]
		if !p.ReadOnly || !p.Proven {
			return ErrConfinementUnproven
		}
	}
	health, err := e.runtime.BusHealth(ctx)
	if err != nil {
		return err
	}
	if !health.OK() {
		return fmt.Errorf("event bus is disabled; no profiles activated")
	}
	for _, role := range []ProfileRole{RoleReviewer, RoleCheckpoint, RoleDiagnostic, RoleWorker} {
		p, ok := st.ExecutionProfiles[role]
		if ok && p.ReadOnly && p.Tool == "codex" {
			if _, err := e.cfg.VerifyConfinementReceipt(p.Profile(), st.Workspace); err != nil {
				return err
			}
		}
		if !ok {
			return fmt.Errorf("no admitted profile for %s", role)
		}
		if err := st.AuthorizeProfileExecution(p.Profile(), st.Workspace, e.now()); err != nil {
			return err
		}
		bound := st.AuditorSession
		switch role {
		case RoleWorker:
			bound = st.WorkerSession
		case RoleCheckpoint:
			bound = st.CheckpointSession
		case RoleDiagnostic:
			bound = st.DiagnosticSession
		}
		if bound != "" {
			owned := false
			for _, a := range st.Activations {
				if a.Role == string(role) && a.SessionID == bound && a.Status == ActivationConfirmed {
					owned = true
				}
			}
			if role == RoleWorker && st.Generation > 1 && st.IsImplementationSession(bound) {
				owned = true
			}
			if !owned {
				return fmt.Errorf("%s session is manually bound; its admitted selection is unproven", role)
			}
			// An already-bound session is inspected through the transport that
			// OWNS it.
			//
			// Asking the Agent Deck runtime about an ACP-bound session queries a
			// system that never had it, so a resume of an ACP worker failed on the
			// one path that is supposed to bring it back. Start is likewise only
			// meaningful for a backend whose sessions need starting at all.
			backend, sref, berr := e.dispatch.backendFor(requestID, bound)
			if berr != nil {
				return fmt.Errorf("the %s session %s could not be routed to a transport: %w", role, bound, berr)
			}
			observed, err := backend.Inspect(ctx, sref)
			if err != nil {
				return fmt.Errorf("the %s session %s could not be inspected on %s: %w",
					role, bound, backend.Name(), err)
			}
			switch strings.ToLower(strings.TrimSpace(observed.Status)) {
			case "running", "waiting":
			default:
				// A session that is bound but not running has to be BROUGHT UP,
				// not shut down. Stopping it would make resume destroy the very
				// session it was asked to recover.
				//
				// Readiness is asked of the owning backend rather than assumed
				// from an Inspect that merely did not error.
				if err := ensureSessionRunning(ctx, backend, sref); err != nil {
					return fmt.Errorf("the %s session %s is bound but not running on %s: %w",
						role, bound, backend.Name(), err)
				}
			}
			continue
		}
		if _, err := e.LaunchProfile(ctx, requestID, role, p.Name, st.Workspace, "initial"); err != nil {
			return err
		}
	}
	return e.store.Update(requestID, func(s *State) error {
		for _, eff := range s.Effects {
			if eff.Reason == "initial task instruction" {
				return nil
			}
		}
		dir, err := e.store.RequestDir(requestID)
		if err != nil {
			return err
		}
		if err := s.RecordEffect(Effect{SessionID: s.WorkerSession, Role: "worker", Reason: "initial task instruction", Message: fmt.Sprintf("Execute the admitted task in %s. Read the immutable REQUEST.json, fulfil its objective and obligations, and report evidence through Agent Engine. Do not edit control state. Request ID: %s.", dir, requestID), CreatedAt: s.UpdatedAt}); err != nil {
			return err
		}
		if s.Stage == StageReady {
			return s.transition(StageWorking, "admitted profiles activated", s.UpdatedAt)
		}
		return nil
	})
}

// sessionStarter is a backend that can bring one of its sessions back up.
//
// It is an optional capability, asked for rather than inferred. A backend that
// owns its process has no separate start step, and claiming otherwise is what
// made a resume stop a session instead of recovering it.
type sessionStarter interface {
	EnsureSessionRunning(ctx context.Context, ref runtime.SessionRef) error
}

// ensureSessionRunning proves a bound session is ready, starting it if the
// backend supports that.
//
// For a backend that owns its own process there is nothing to start, so the only
// meaningful check is that the session is genuinely open. Skipping that check
// would let a resume pass on a session that was never there.
func ensureSessionRunning(ctx context.Context, backend runtime.Backend, ref runtime.SessionRef) error {
	if st, ok := backend.(sessionStarter); ok {
		return st.EnsureSessionRunning(ctx, ref)
	}
	if backendOwnsLiveProcesses(backend) {
		sess, err := backend.Inspect(ctx, ref)
		if err != nil {
			return fmt.Errorf("readiness could not be established: %w", err)
		}
		if s := strings.ToLower(strings.TrimSpace(sess.Status)); s != "running" && s != "waiting" {
			return fmt.Errorf("the session reports %q and cannot be started on this transport", sess.Status)
		}
		return nil
	}
	return fmt.Errorf("%s offers no way to start a bound session", backend.Name())
}

// backendOwnsLiveProcesses reports whether a backend runs the session itself.
//
// Such a session is live from creation, so there is nothing to "start": issuing
// a start against it would be a second lifecycle action against a session that
// is already running.
func backendOwnsLiveProcesses(b runtime.Backend) bool {
	_, owned := b.ProcessIdentity()
	return owned
}

// createProfileSession issues the runtime Create and commits the confirmed ID.
func (e *Engine) createProfileSession(ctx context.Context, requestID, activationID string, role ProfileRole, profile Profile, label, workspace string) (string, error) {
	// The intent moves to DISPATCHING before the process is created, so a crash
	// during Create is visible as an unresolved intent.
	e.markActivation(requestID, activationID, ActivationDispatching, "")

	// The transport is taken from the profile, never from whichever runtime the
	// engine happens to hold. Agent Deck keeps its existing Create/Start path so
	// its behaviour is unchanged; every other transport goes through the boundary.
	if profile.TransportFor() != TransportAgentDeck {
		return e.createProfileSessionOnBackend(ctx, requestID, activationID, role, profile, label, workspace)
	}

	spec, err := profile.CreateSpec(workspace, label)
	if err != nil {
		return "", err
	}
	sess, err := e.runtime.Create(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("agent-engine: the %s session could not be created: %w", role, err)
	}
	if strings.TrimSpace(sess.ID) == "" {
		// A Create that returned no ID has an unknown outcome. It is NOT retried:
		// the session may exist, and a second Create is how a duplicate worker
		// appears on a workspace with a single-writer lease.
		return "", fmt.Errorf("%w: the runtime reported a created %s session with no ID; "+
			"it may exist and must be reconciled by label %q rather than created again",
			ErrUnknownEffect, role, label)
	}
	if err := e.bindActivation(requestID, activationID, role, sess.ID, label, profile, profile.Ref(), 0); err != nil {
		return "", err
	}
	return sess.ID, nil
}

// createProfileSessionOnBackend activates a session on a non-default transport.
func (e *Engine) createProfileSessionOnBackend(ctx context.Context, requestID, activationID string, role ProfileRole, profile Profile, label, workspace string) (string, error) {
	backend, err := e.backendForProfile(profile)
	if err != nil {
		return "", err
	}
	ref, aerr := backend.Activate(ctx, runtime.ActivateSpec{
		Label: label, Path: workspace, Tool: profile.Tool, Title: label,
	})
	if aerr != nil {
		return "", fmt.Errorf("agent-engine: the %s session could not be activated on %s: %w", role, backend.Name(), aerr)
	}
	if strings.TrimSpace(ref.ID) == "" {
		// The backend confirmed a create with no identity. The session may exist,
		// so this is an unknown outcome and NOT a retry.
		return "", fmt.Errorf("%w: %s reported an activated %s session with no ID; it may exist and must "+
			"be reconciled by label %q rather than activated again", ErrUnknownEffect, backend.Name(), role, label)
	}
	if err := e.bindActivation(requestID, activationID, role, ref.ID, label, profile, profile.Ref(), backendPid(backend)); err != nil {
		return "", err
	}
	return ref.ID, nil
}

// bindActivation commits the confirmed session and binds it to its role.
//
// The binding is committed BEFORE Start, so a crash after Start cannot leave a
// running process that no state claims.
//
// The backend-qualified binding is written in the SAME transaction as the legacy
// bare ID. They cannot diverge because there is no second write to fall out of
// step: an effect routed by a bare id would have no transport to route through.
func (e *Engine) bindActivation(requestID, activationID string, role ProfileRole, sessionID, label string, profile Profile, ref ProfileRef, pid int) error {
	backend, err := e.backendForProfile(profile)
	if err != nil {
		return err
	}
	startToken := pidStartToken(pid)
	_, err = e.store.UpdateCommitted(requestID, func(s *State) error {
		// A reviewer may not be bound to a session that implemented this request.
		if role == RoleReviewer || role == RoleDiagnostic || role == RoleCheckpoint {
			if s.IsImplementationSession(sessionID) {
				return fmt.Errorf("%w: session %s is inside this request's implementation lineage",
					ErrProfileNotIsolated, sessionID)
			}
		}
		if role == RoleWorker {
			s.NoteImplementation(sessionID)
		}
		switch role {
		case RoleWorker:
			s.WorkerSession = sessionID
		case RoleReviewer:
			s.AuditorSession = sessionID
		case RoleCheckpoint:
			s.CheckpointSession = sessionID
		case RoleDiagnostic:
			s.DiagnosticSession = sessionID
		}
		if s.BackendBindings == nil {
			s.BackendBindings = map[ProfileRole]SessionBinding{}
		}
		s.BackendBindings[role] = SessionBinding{
			ProcessPID:   pid,
			ProcessStart: startToken,
			Backend:      backend.Name(),
			SessionID:    sessionID,
			Label:        label,
			Generation:   s.Generation,
			Epoch:        s.LaunchEpoch,
			Resolved:     true,
		}
		for i := range s.Activations {
			if s.Activations[i].ActivationID == activationID {
				s.Activations[i].Status = ActivationConfirmed
				s.Activations[i].SessionID = sessionID
				s.Activations[i].LastError = ""
				return nil
			}
		}
		return fmt.Errorf("agent-engine: activation %s is not in the ledger", activationID)
	})
	return err
}

// markActivation records a terminal status or a blocking reason for an intent.
func (e *Engine) markActivation(requestID, activationID, status, reason string) {
	_ = e.store.Update(requestID, func(s *State) error {
		for i := range s.Activations {
			if s.Activations[i].ActivationID == activationID {
				s.Activations[i].Status = status
				if reason != "" {
					s.Activations[i].LastError = reason
				}
				return nil
			}
		}
		return nil
	})
}

// findSessionByLabel looks for an existing session carrying an activation label.
//
// The capability is ASKED FOR rather than discovered by asserting for an optional
// interface. The difference is not stylistic: an assertion silently produces
// "this runtime has no inventory" for a backend that simply lacks the method,
// and that answer is indistinguishable from a genuine capability gap at the one
// moment the caller is deciding whether to create a second worker.
//
// A nil id with a nil error means "definitely absent", which is the only safe
// basis for creating. An error means the outcome is unknown, and the caller must
// not create.
func (e *Engine) findSessionByLabel(ctx context.Context, label string) (string, error) {
	inv, ok := e.runtime.(SessionInventory)
	if !ok {
		return "", fmt.Errorf("agent-engine: this runtime exposes no session inventory, so a create with an unknown "+
			"outcome cannot be reconciled by label %q; refusing to create a second session", label)
	}
	sessions, err := inv.List(ctx)
	if err != nil {
		return "", fmt.Errorf("agent-engine: the session inventory could not be read, so an ambiguous create "+
			"cannot be reconciled and no new session may be created: %w", err)
	}
	for _, s := range sessions {
		if s.Title == label {
			return s.ID, nil
		}
	}
	return "", nil
}

// SessionInventory is the optional capability a runtime provides when it can list
// its sessions.
//
// It remains separate from Runtime because most operations never need it, and
// adding it to the mandatory interface would force every implementation —
// including the test fake — to support an inventory it does not need. The
// difference from a bare type assertion is that callers can now ASK a backend
// what it supports through runtime.Capabilities and get an answer for every
// backend, not only for those that happen to satisfy an interface.
type SessionInventory interface {
	List(ctx context.Context) ([]agentdeck.Session, error)
}

// appendUniqueProfile pins a profile identity once.
func appendUniqueProfile(existing []ProfileRef, ref ProfileRef) []ProfileRef {
	for _, p := range existing {
		if p.Name == ref.Name && p.Role == ref.Role {
			return existing
		}
	}
	return append(existing, ref)
}

// ReapOrphanedProcesses stops transport processes left behind by a dead
// supervisor.
//
// This is the specific hazard of supervisor death on a transport that OWNS a
// child. Killing the supervisor does not kill `mcode acp`: it keeps running,
// still holding the workspace, with nothing supervising it. A restart then
// starts a SECOND one, and the request has two writers on one tree.
//
// Identity is checked before signalling, because a recorded pid may since have
// been reused by something unrelated — and signalling a stranger's process is a
// worse outcome than leaving an orphan behind.
//
// It runs before any new connection is admitted. An orphan that cannot be
// reaped is reported rather than worked around: proceeding would mean silently
// admitting a request whose workspace already has an unattributed writer.
func (e *Engine) ReapOrphanedProcesses(ctx context.Context, requestID string) []string {
	st, err := e.store.State(requestID)
	if err != nil {
		return []string{fmt.Sprintf("state unreadable, so no orphan could be reaped: %v", err)}
	}
	var problems []string
	seen := map[int]bool{}

	// STARTUP RECORDS FIRST. A process recorded here may never have reached a
	// session binding at all, which is exactly the crash window the record
	// exists to cover.
	for name, rec := range st.OwnedProcesses {
		if rec.Pending {
			// A committed spawn whose pid was never recorded. The process may be
			// running and unidentifiable, so quiescence CANNOT be established and a
			// replacement must not be admitted.
			problems = append(problems, fmt.Sprintf(
				"transport %s: a spawn was recorded at %s but its process was never identified, so it may "+
					"still be running; recovery is required before a replacement is admitted", name, rec.RecordedAt))
			continue
		}
		if rec.PID <= 0 || seen[rec.PID] {
			continue
		}
		seen[rec.PID] = true
		alive, known := recordedProcessAlive(rec.PID, rec.StartToken)
		if !known {
			problems = append(problems, fmt.Sprintf(
				"transport %s: recorded process %d could not be identified, so it is not proven gone",
				name, rec.PID))
			continue
		}
		if !alive {
			continue
		}
		if err := reapProcessGroup(ctx, rec.PID); err != nil {
			problems = append(problems, fmt.Sprintf(
				"transport %s: orphaned process %d could not be stopped: %v", name, rec.PID, err))
			continue
		}
		// Termination is a request; being gone is the fact. The helper's own
		// wait expiring is not evidence, so liveness is confirmed here.
		if still, _ := recordedProcessAlive(rec.PID, rec.StartToken); still {
			problems = append(problems, fmt.Sprintf(
				"transport %s: orphaned process %d is STILL RUNNING after being signalled", name, rec.PID))
			continue
		}
		e.logf("reaped orphaned %s process %d recorded at startup", name, rec.PID)
	}

	for role, b := range st.BackendBindings {
		if b.ProcessPID <= 0 || seen[b.ProcessPID] {
			continue
		}
		seen[b.ProcessPID] = true

		alive, known := recordedProcessAlive(b.ProcessPID, b.ProcessStart)
		if !known {
			problems = append(problems, fmt.Sprintf(
				"role %s: process %d could not be identified, so it is not proven gone and was not signalled",
				role, b.ProcessPID))
			continue
		}
		if !alive {
			// Gone, or the pid was reused by something else. Either way the
			// ORIGINAL is provably not running.
			continue
		}
		if err := reapProcessGroup(ctx, b.ProcessPID); err != nil {
			problems = append(problems, fmt.Sprintf(
				"role %s: orphaned process %d could not be stopped: %v", role, b.ProcessPID, err))
			continue
		}
		// Termination is a request; the process being gone is the fact.
		deadline := time.Now().Add(orphanReapGrace)
		for time.Now().Before(deadline) {
			if still, _ := recordedProcessAlive(b.ProcessPID, b.ProcessStart); !still {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if still, _ := recordedProcessAlive(b.ProcessPID, b.ProcessStart); still {
			problems = append(problems, fmt.Sprintf(
				"role %s: orphaned process %d is STILL RUNNING after being signalled", role, b.ProcessPID))
			continue
		}
		e.logf("reaped orphaned %s process %d left by a previous supervisor (role %s)",
			b.Backend, b.ProcessPID, role)
	}
	return problems
}

// orphanReapGrace bounds how long a signalled orphan is waited for.
const orphanReapGrace = 5 * time.Second

// reapProcessGroup signals a process group, escalating from TERM to KILL.
//
// The negative pid targets the GROUP, because a coding agent spawns tools as
// children and killing only the parent orphans exactly the children this whole
// mechanism exists to prevent.
func reapProcessGroup(ctx context.Context, pid int) error {
	// Zero signals our own process group; negative values can target other
	// groups or all permitted processes. Neither is an owned process identity.
	if pid <= 0 {
		return fmt.Errorf("cannot reap invalid process pid %d", pid)
	}
	// The negative form targets a PROCESS GROUP, and it is only correct when the
	// pid really is that group's leader. Assuming so is how a reaper ends up
	// signalling somebody else's group — including, in the worst case, its own.
	//
	// Agent Engine starts ACP children with Setpgid, so they are leaders; anything
	// else is signalled by pid alone, which reaches the process and never a
	// stranger's group.
	group := pid
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		group = 0
	}
	signal := func(sig syscall.Signal) error {
		if group == pid {
			if err := syscall.Kill(-group, sig); err == nil || errors.Is(err, syscall.ESRCH) {
				return err
			}
			// The group form can be refused even for a group we lead — the
			// sandbox may permit the process but not its group. Falling back to
			// the process reaches the orphan, which is the point; it cannot
			// reach anything else.
			return syscall.Kill(pid, sig)
		}
		return syscall.Kill(pid, sig)
	}
	aliveProbe := func() error {
		if group == pid {
			return syscall.Kill(-group, 0)
		}
		return syscall.Kill(pid, 0)
	}

	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := signal(sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(orphanReapGrace / 2)
		for time.Now().Before(deadline) {
			if err := aliveProbe(); errors.Is(err, syscall.ESRCH) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	// Falling out of both escalation rounds is NOT success. Returning nil here
	// told the caller the orphan was reaped while it was still running, and the
	// startup-record path had no further check — so a replacement transport could
	// be admitted on top of a live writer.
	return fmt.Errorf("the process survived SIGTERM and SIGKILL")
}
