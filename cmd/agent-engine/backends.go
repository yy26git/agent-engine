package main

// Production backend composition.
//
// The control layer reaches more than one transport, and the set it can reach is
// decided by what the admitted profiles actually declare — not by what happens
// to be installed on the host. A transport nobody admitted is not started, so a
// stray binary or a stale login cannot become a way to run work.
//
// Two properties are worth stating, because both are ways this goes wrong:
//
//   - A transport is PINNED before it runs. The launcher path is resolved and its
//     digest measured; an unpinned executable is refused outright, because
//     everything a session produces is attributed to the binary that ran.
//   - A failure to bring up a REQUIRED transport is fatal for the run. An engine
//     that silently lost its ACP backend would block every ACP-bound effect as
//     unrouteable, which is safe but looks like a broken configuration. Failing
//     here names the real cause instead.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/minimaxacp"
	"github.com/emjin/agent-engine/internal/runtime"
)

// DefaultMCodeLauncher is where the pinned launcher lives when the operator has
// not overridden it.
const DefaultMCodeLauncher = ".minimax-code/releases/0.6.2/.mcode-launcher"

// acpLauncherPath resolves the pinned ACP launcher.
//
// The override exists so an operator can point at a specific reviewed release
// without editing this file. It must be absolute: a relative launcher is a
// launcher whose identity depends on the directory the process happened to start
// in.
func acpLauncherPath() (string, error) {
	p := os.Getenv("AGENT_ENGINE_MCODE_LAUNCHER")
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("agent-engine: the home directory is unknown, so the pinned ACP launcher cannot be located: %w", err)
		}
		p = filepath.Join(home, DefaultMCodeLauncher)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("agent-engine: the ACP launcher path %q cannot be resolved: %w", p, err)
	}
	if !filepath.IsAbs(abs) {
		return "", fmt.Errorf("agent-engine: the ACP launcher path %q must be absolute", p)
	}
	return abs, nil
}

// startACP launches and pins the ACP transport.
//
// It returns the connection so the caller can stop it deterministically. A
// session is never admitted onto a transport whose identity was not measured.
//
// Admitting ACP is an explicit operator decision rather than something inferred
// from what is installed: the rollback for this transport is to stop asking for
// it, and that has to be something an operator can actually do.
func startACP(ctx context.Context, st *control.Store, requestID, workspace, model, permissionMode string) (*minimaxacp.Connection, error) {
	if err := control.RequireQualifiedMiniMaxBoundary(control.Profile{
		Name: "legacy --acp-model", Transport: control.TransportMiniMaxACP,
	}); err != nil {
		return nil, err
	}
	if err := verifyACPRelease(st); err != nil {
		return nil, err
	}
	return startACPWithStarter(ctx, st, requestID, workspace, model, permissionMode, minimaxacp.Start)
}

// Both production and composition tests use this factory; only the process
// boundary varies. Intent and callback policy have a single implementation.
func startACPWithStarter(ctx context.Context, st *control.Store, requestID, workspace, model, permissionMode string, start func(context.Context, minimaxacp.Options) (*minimaxacp.Connection, error)) (*minimaxacp.Connection, error) {
	return startACPInstance(ctx, st, requestID, workspace, model, permissionMode, minimaxacp.BackendName, start)
}

func startACPInstance(ctx context.Context, st *control.Store, requestID, workspace, model, permissionMode, backendID string, start func(context.Context, minimaxacp.Options) (*minimaxacp.Connection, error)) (*minimaxacp.Connection, error) {
	return startACPInstanceWithEffort(ctx, st, requestID, workspace, model, "", permissionMode, backendID, start)
}

func startACPInstanceWithEffort(ctx context.Context, st *control.Store, requestID, workspace, model, effort, permissionMode, backendID string, start func(context.Context, minimaxacp.Options) (*minimaxacp.Connection, error)) (*minimaxacp.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turnTimeout := minimaxacp.DefaultTurnTimeout
	if deadline, ok := ctx.Deadline(); ok {
		turnTimeout = time.Until(deadline)
		if turnTimeout <= 0 {
			return nil, context.DeadlineExceeded
		}
	}
	launcher, err := acpLauncherPath()
	if err != nil {
		return nil, err
	}
	sum, err := runtime.FileSHA256(launcher)
	if err != nil {
		return nil, fmt.Errorf("agent-engine: the pinned ACP launcher %s could not be measured, so the "+
			"transport cannot be admitted: %w", launcher, err)
	}
	dir, err := st.RequestDir(requestID)
	if err != nil {
		return nil, err
	}
	// A PRE-SPAWN intent is committed before anything can create a process.
	//
	// Without it there is an unavoidable window between "the process exists" and
	// "we recorded which process it is". A supervisor dying inside that window
	// leaves a live child with no identity, and recovery then cannot prove
	// quiescence — so it must BLOCK rather than assume.
	//
	// The intent carries no pid because none exists yet. Its meaning on recovery
	// is precisely "we may have started something we cannot name".
	if err := recordSpawnIntentFor(st, requestID, backendID); err != nil {
		return nil, fmt.Errorf("agent-engine: the pre-spawn intent could not be recorded, so no ACP process may "+
			"be started: %w", err)
	}

	stateDir := filepath.Join(dir, "minimax-acp")
	if backendID != minimaxacp.BackendName {
		stateDir = filepath.Join(stateDir, runtimeDigest(backendID))
	}
	conn, err := start(ctx, minimaxacp.Options{
		BackendID:      backendID,
		StateDir:       stateDir,
		Launcher:       launcher,
		LauncherSHA256: sum,
		Cwd:            workspace,
		Model:          model,
		Effort:         effort,
		PermissionMode: permissionMode,
		// Native release initialization has exceeded the protocol's 30-second
		// default on this host. Keep startup bounded while the caller's episode
		// context remains the tighter wall-clock limit when less time is left.
		HandshakeTimeout: 90 * time.Second,
		// The supervisor already bounds this context by episode budget and pilot
		// expiry. Its deadline must also bound coding turns; the standalone
		// 15-minute default can otherwise abandon a still-authorized prompt.
		// Submit continues to observe ctx cancellation and never invents receipts.
		TurnTimeout: turnTimeout,
		// Runs between cmd.Start() and the handshake. It is FALLIBLE on purpose:
		// a spawn this supervisor cannot record is a spawn it must not keep, so
		// the error aborts startup and the child is reaped below.
		OnSpawned: func(pid int) error {
			return recordSpawnedProcessFor(st, requestID, backendID, pid)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("agent-engine: the ACP transport could not be started from the pinned launcher "+
			"%s (digest %s): %w", launcher, sum[:16], err)
	}
	return conn, nil
}

// admitPinnedTransports starts the transports a request's PINNED PROFILES demand.
//
// This is the one place that decides which transports a run may reach, so `run`
// and `task-exec` cannot disagree about it. Admission is driven by the profile
// the request pinned, not by what happens to be installed: a deployment with an
// ACP launcher and no profile naming it never starts one, and a profile naming
// ACP is never quietly served by Agent Deck.
//
// The model comes from the PINNED profile rather than a flag. A flag supplied at
// run time could differ from the model the request was admitted against, and the
// result would be work attributed to a model nobody approved.
func admitPinnedTransports(ctx context.Context, st *control.Store, requestID, workspace string, dryRun bool) ([]runtime.Backend, func(), error) {
	return admitPinnedTransportsWithStarter(ctx, st, requestID, workspace, dryRun, minimaxacp.Start)
}

func admitPinnedTransportsWithStarter(ctx context.Context, st *control.Store, requestID, workspace string, dryRun bool, start func(context.Context, minimaxacp.Options) (*minimaxacp.Connection, error)) ([]runtime.Backend, func(), error) {
	noop := func() {}
	subset, err := st.State(requestID)
	if err != nil {
		return nil, noop, err
	}
	profiles := map[string]control.ProfileRef{}
	for _, ref := range subset.ExecutionProfiles {
		if ref.Transport == control.TransportMiniMaxACP {
			key := minimaxacp.BackendName + ":" + ref.Name
			if prior, ok := profiles[key]; ok && (prior.Model != ref.Model || prior.Permission != ref.Permission || prior.Effort != ref.Effort) {
				return nil, noop, fmt.Errorf("agent-engine: profile %q has incompatible process selections", ref.Name)
			}
			profiles[key] = ref
		}
	}
	if len(profiles) == 0 {
		return nil, noop, nil
	}
	if dryRun {
		return nil, noop, fmt.Errorf("agent-engine: an ACP dry run must not launch a real process")
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Check every pinned profile before measuring a release or committing any
	// spawn intent. Old admissions cannot bypass the current boundary gate, and
	// failure of a later profile must not leave an earlier process running.
	for _, id := range ids {
		if err := subset.AuthorizeProfileExecution(profiles[id].Profile(), workspace, time.Now()); err != nil {
			return nil, noop, err
		}
	}
	if err := verifyACPRelease(st); err != nil {
		return nil, noop, err
	}
	var connections []*minimaxacp.Connection
	var backends []runtime.Backend
	stop := func() {
		for _, conn := range connections {
			_, _ = conn.Stop(context.Background())
		}
	}
	for _, id := range ids {
		ref := profiles[id]
		conn, err := startACPInstanceWithEffort(ctx, st, requestID, workspace, ref.Model, ref.Effort, ref.Permission, id, start)
		if err != nil {
			stop()
			return nil, noop, err
		}
		connections = append(connections, conn)
		backends = append(backends, conn)
	}
	return backends, stop, nil
}

// workspaceOf reports the workspace a request operates on, for admission.
func workspaceOf(st *control.Store, requestID string) string {
	s, err := st.State(requestID)
	if err != nil {
		return ""
	}
	return s.Workspace
}

// recordSpawnIntent commits the durable "we are about to start something"
// marker, before any process exists.
//
// It is deliberately pid-less. A pid cannot be known yet, and inventing one would
// be worse than admitting the gap: on recovery this record means a process may
// exist that nothing can identify, which is the one condition under which a
// replacement must be refused.
func recordSpawnIntent(st *control.Store, requestID string) error {
	return recordSpawnIntentFor(st, requestID, minimaxacp.BackendName)
}

func recordSpawnIntentFor(st *control.Store, requestID, backendID string) error {
	if strings.TrimSpace(requestID) == "" {
		return nil
	}
	_, err := st.UpdateCommitted(requestID, func(s *control.State) error {
		if s.OwnedProcesses == nil {
			s.OwnedProcesses = map[string]control.OwnedProcess{}
		}
		s.OwnedProcesses[backendID] = control.OwnedProcess{
			Pending:    true,
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		}
		return nil
	})
	return err
}

// recordSpawnedProcess durably records the process that now exists, with its
// measured start token.
//
// It runs inside the spawn callback, so it must not take the engine's ownership
// lock — the caller may already hold it. The store's own transaction lock is
// enough for this single write.
//
// It clears the pre-spawn intent in the SAME transaction as the pid, so there is
// no state in which a live process is recorded as merely pending.
func recordSpawnedProcess(st *control.Store, requestID string, pid int) error {
	return recordSpawnedProcessFor(st, requestID, minimaxacp.BackendName, pid)
}

func recordSpawnedProcessFor(st *control.Store, requestID, backendID string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("agent-engine: a spawned process reported no pid")
	}
	if strings.TrimSpace(requestID) == "" {
		return nil
	}
	_, err := st.UpdateCommitted(requestID, func(s *control.State) error {
		if s.OwnedProcesses == nil {
			s.OwnedProcesses = map[string]control.OwnedProcess{}
		}
		s.OwnedProcesses[backendID] = control.OwnedProcess{
			PID: pid, StartToken: control.StartTokenFor(pid), Pending: false,
			RecordedAt: time.Now().UTC().Format(time.RFC3339),
		}
		return nil
	})
	return err
}

func runtimeDigest(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func verifyACPRelease(st *control.Store) error {
	launcher, err := acpLauncherPath()
	if err != nil {
		return err
	}
	receipt := os.Getenv("AGENT_ENGINE_MCODE_RECEIPT")
	if receipt == "" {
		receipt = filepath.Join(st.Config().StoreRoot, "MINIMAX-RELEASE.json")
	}
	if !filepath.IsAbs(receipt) {
		return fmt.Errorf("MiniMax release receipt must be an absolute path")
	}
	if err := minimaxacp.VerifyReleaseReceipt(launcher, receipt); err != nil {
		return fmt.Errorf("MiniMax release admission refused: %w", err)
	}
	return nil
}
