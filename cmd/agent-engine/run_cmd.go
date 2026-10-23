package main

// Launching supervision: binding the two roles, and running the engine.
//
// These two verbs are what turn an admitted request into a supervised one. They
// are separate because they answer different questions, and a caller that
// could not state them separately would be encouraged to guess:
//
//   - `bind` says *who* is working. It is a setup step, done once per generation.
//   - `run` says *keep watching*. It is the supervision loop, and it is bounded
//     by `--once` or by cancellation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/runtime"
)

func cmdBind(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("bind", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	worker := fs.String("worker", "", "full session ID of the implementation worker")
	auditor := fs.String("auditor", "", "full session ID of the independent reviewer")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	workerRef, auditorRef := strings.TrimSpace(*worker), strings.TrimSpace(*auditor)
	if workerRef == "" && auditorRef == "" {
		return fail(stdout, stderr, asJSON, errors.New("--worker and/or --auditor are required"))
	}

	staged, err := st.UpdateCommitted(*requestID, func(s *control.State) error {
		if s.Stage.Terminal() {
			return fmt.Errorf("agent-engine: request is already terminal as %s", s.ResultStatus)
		}
		// The check is on the EFFECTIVE binding after the merge, inside the
		// state transaction, not on the two arguments above.
		//
		// Comparing the arguments only is what made a partial rebind collapse the
		// roles: binding the auditor to the already-bound worker supplies one
		// non-empty value, so the argument check is skipped entirely, and the
		// completion audit becomes self-certifying. Validating the result rather
		// than the request is the only place the collision is visible.
		if err := s.SetBinding(workerRef, auditorRef); err != nil {
			return err
		}
		// A newly bound worker starts reading events from now on. Without this a
		// late-starting supervisor would act on output the new worker produced
		// before anybody was watching it.
		s.BindingBoundary = s.BusCursor
		return s.Transition(control.StageWorking, "sessions bound", s.UpdatedAt)
	})
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id": staged.RequestID,
			"worker":     staged.WorkerSession,
			"auditor":    staged.AuditorSession,
			"stage":      string(staged.Stage),
			"generation": staged.Generation,
		})
	}
	fmt.Fprintf(stdout, "bound %s\n  worker:   %s\n  auditor:  %s\n  stage:    %s\n",
		staged.RequestID, staged.WorkerSession, staged.AuditorSession, staged.Stage)
	return 0
}

func cmdRun(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("run", stderr)
	requestID := fs.String("request-id", "", "admitted request identifier")
	once := fs.Bool("once", false, "process the current backlog through a captured boundary, then exit")
	dryRun := fs.Bool("dry-run", false, "decide without sending, writing state, or touching the request")
	acpModel := fs.String("acp-model", "", "admit the native ACP transport for the worker, pinned to this model")
	acpPermission := fs.String("acp-permission-mode", "", "permission mode frozen into every ACP session")
	watchdog := fs.Duration("watchdog", 0, "safety-net poll interval; 0 disables")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*requestID) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}

	cfg := st.Config()
	state, err := st.State(*requestID)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if state.Stage.Terminal() {
		if asJSON {
			return emit(stdout, stderr, map[string]any{
				"request_id": state.RequestID,
				"stage":      string(state.Stage),
				"status":     state.ResultStatus,
				"terminal":   true,
			})
		}
		fmt.Fprintf(stdout, "%s is already terminal as %s; nothing to supervise\n", state.RequestID, state.ResultStatus)
		return 0
	}
	if state.Episode != nil && state.Episode.ReportOnly {
		return cmdRunReport(st, *requestID, *dryRun, stdout, stderr, asJSON)
	}
	if strings.TrimSpace(state.WorkerSession) == "" && len(state.ExecutionProfiles) == 0 {
		return fail(stdout, stderr, asJSON, fmt.Errorf(
			"no worker is bound; run `agent-engine --store %s bind --request-id %s --worker <full-session-id> --auditor <full-session-id>` first",
			cfg.StoreRoot, state.RequestID))
	}

	// Supervision requires both sides. A worker with no reviewer would accumulate
	// audit debt nothing could ever discharge, which looks exactly like healthy
	// progress from the outside.
	if strings.TrimSpace(state.AuditorSession) == "" && !*dryRun && len(state.ExecutionProfiles) == 0 {
		return fail(stdout, stderr, asJSON, errors.New(
			"no auditor is bound; a worker with no independent reviewer can accumulate audit debt nothing can discharge"))
	}
	if !*dryRun && !state.CancellationRequested {
		var profiles []control.ProfileRef
		for _, ref := range state.ExecutionProfiles {
			profiles = append(profiles, ref)
		}
		if err := requireCodexRuntimeHome(profiles); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
	}

	cap, err := st.PreflightCapability(*requestID)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if !cap.Allowed {
		return fail(stdout, stderr, asJSON, fmt.Errorf("capability preflight refused: %s", cap.Reason))
	}

	// The capability preflight must run against the same workspace the engine
	// will touch, and a dry run is refused outright here rather than later,
	// because a dry run that cannot prove confinement would silently look clean.
	if !*dryRun && cap.Capability == control.CapabilityReadOnly {
		if _, err := os.Stat(cfg.OutputRoot); err != nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf(
				"the bounded output area %q is not usable, so a read-only run has nowhere to record findings: %w",
				cfg.OutputRoot, err))
		}
	}

	// The runtime is verified BEFORE it is constructed for use. A supervisor that
	// cannot prove what executable it is about to drive must not start, because
	// everything it decides afterwards is attributed to a binary nobody has
	// identified.
	rt, err := pinnedRuntime(cfg, stderr, !asJSON)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}

	eng, err := control.NewEngine(st, rt, *requestID, control.EngineOptions{
		Once:   *once,
		DryRun: *dryRun,
		Log:    func(line string) { fmt.Fprintln(stderr, "  "+line) },
	})
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	defer eng.Close()

	if *dryRun && strings.TrimSpace(*acpModel) != "" {
		return fail(stdout, stderr, asJSON, errors.New("--acp-model cannot be combined with --dry-run"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	supervisorPID := os.Getpid()
	supervisorStart := control.StartTokenFor(supervisorPID)
	supervisorStartedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := st.BeginSupervisor(*requestID, supervisorPID, supervisorStart, supervisorStartedAt); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	defer func() {
		exitKind := control.SupervisorNormalExit
		if errors.Is(ctx.Err(), context.Canceled) {
			exitKind = control.SupervisorSignalStop
		}
		if err := st.EndSupervisor(*requestID, supervisorPID, supervisorStart, exitKind, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			fmt.Fprintf(stderr, "supervisor lifecycle record could not be closed: %v\n", err)
		}
	}()
	// A persisted stop must not start a replacement ACP process just to clean
	// up. Recovery is independent of grant expiry and the execution deadline.
	if state.CancellationRequested && !*dryRun {
		recoveryCtx, cancelRecovery := context.WithTimeout(ctx, 30*time.Second)
		defer cancelRecovery()
		if problems := eng.ReapOrphanedProcesses(recoveryCtx, *requestID); len(problems) > 0 {
			return fail(stdout, stderr, asJSON, fmt.Errorf("cancellation requires ownership recovery: %s", strings.Join(problems, "; ")))
		}
		rep, err := eng.ReconcileCancellation(recoveryCtx, *requestID)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		if !rep.Quiesced {
			return fail(stdout, stderr, asJSON, fmt.Errorf("cancellation remains unresolved: %s", rep.Reason))
		}
		if asJSON {
			return emit(stdout, stderr, map[string]any{"request_id": *requestID, "stage": control.StageCancelled, "cancelled": true})
		}
		fmt.Fprintf(stdout, "%s cancelled; owned effects reconciled\n", *requestID)
		return 0
	}
	if state.Episode != nil && state.Episode.Budget.WallSeconds > 0 {
		until, err := state.ExecutionDeadline()
		if err != nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("episode deadline cannot be established: %w", err))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, until)
		defer cancel()
		if ctx.Err() != nil {
			recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancelRecovery()
			if problems := eng.ReapOrphanedProcesses(recoveryCtx, *requestID); len(problems) > 0 {
				return fail(stdout, stderr, asJSON, fmt.Errorf("expired episode requires ownership recovery: %s", strings.Join(problems, "; ")))
			}
			if _, err := eng.ReconcileExpiredEpisode(recoveryCtx, *requestID); err != nil {
				return fail(stdout, stderr, asJSON, err)
			}
			return fail(stdout, stderr, asJSON, fmt.Errorf("episode wall-clock budget is exhausted"))
		}
	}
	if !*dryRun {
		if state.Episode != nil {
			// Re-establish owned-process quiescence and measure source before any
			// completed-review recovery or new transport admission. Drift is a
			// durable refusal; this handover path never binds a replacement worker.
			validation, err := eng.RevalidateEpisodeResume(ctx, *requestID)
			if err != nil {
				return fail(stdout, stderr, asJSON, err)
			}
			if validation.Blocked {
				return fail(stdout, stderr, asJSON, fmt.Errorf("episode resume blocked: %s", validation.Reason))
			}
			state, err = st.State(*requestID)
			if err != nil {
				return fail(stdout, stderr, asJSON, err)
			}
		}
		if handled, err := eng.RecoverCompletedEpisodeReview(ctx, *requestID); handled {
			if err != nil {
				return fail(stdout, stderr, asJSON, err)
			}
			fresh, err := st.State(*requestID)
			if err != nil {
				return fail(stdout, stderr, asJSON, err)
			}
			return emit(stdout, stderr, map[string]any{"request_id": *requestID, "stage": fresh.Stage, "result_status": fresh.ResultStatus, "completion_recovered": true})
		}
		if err := eng.InitializeTransports(ctx, *requestID, func() ([]runtime.Backend, func(), error) {
			if strings.TrimSpace(*acpModel) == "" {
				return admitPinnedTransports(ctx, st, *requestID, state.Workspace, false)
			}
			if len(state.ExecutionProfiles) > 0 {
				return nil, nil, errors.New("--acp-model cannot override admitted execution profiles")
			}
			if state.Workspace == "" {
				return nil, nil, errors.New("the request records no workspace for an ACP session")
			}
			conn, err := startACP(ctx, st, *requestID, state.Workspace, strings.TrimSpace(*acpModel), strings.TrimSpace(*acpPermission))
			if err != nil {
				return nil, nil, err
			}
			return []runtime.Backend{conn}, func() { _, _ = conn.Stop(context.Background()) }, nil
		}); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
	}

	if len(state.ExecutionProfiles) > 0 && !*dryRun {
		if err := eng.PrepareTaskSessions(ctx, *requestID); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		state, err = st.State(*requestID)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
	}

	if *watchdog > 0 {
		// The watchdog is a safety net, not the primary path. The event stream is
		// the primary path; this exists to catch a session that changed without a
		// frame being published, which the plan records as a real possibility
		// because producer queues can drop events.
		//
		// It is deliberately cheap: one bounded status read per tick, and no model
		// call. A supervisor that polls a strong model every tick costs more than
		// the work it supervises and still learns nothing an event would not say.
		go func() {
			ticker := time.NewTicker(*watchdog)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sess, ierr := rt.Inspect(ctx, state.WorkerSession)
					if ierr != nil {
						fmt.Fprintf(stderr, "  watchdog: cannot read worker status: %v\n", ierr)
						continue
					}
					fmt.Fprintf(stderr, "  watchdog: worker %s is %s (frames may still be pending)\n",
						state.WorkerSession, sess.Status)
				}
			}
		}()
	}

	res, runErr := eng.Run(ctx, *requestID)
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) && !*dryRun {
			recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancelRecovery()
			if _, err := eng.ReconcileExpiredEpisode(recoveryCtx, *requestID); err != nil {
				return fail(stdout, stderr, asJSON, errors.Join(runErr, err))
			}
		}
		if errors.Is(runErr, context.Canceled) {
			// Cancellation is a legitimate way to stop, and the state is durable.
			if asJSON {
				return emit(stdout, stderr, map[string]any{
					"request_id": state.RequestID,
					"cancelled":  true,
					"stage":      string(res.Stage),
					"frames":     res.FramesObserved,
				})
			}
			fmt.Fprintf(stdout, "stopped after %d frame(s); state is durable and the request is not finished\n",
				res.FramesObserved)
			return 0
		}
		return fail(stdout, stderr, asJSON, runErr)
	}

	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":        state.RequestID,
			"stage":             string(res.Stage),
			"frames_observed":   res.FramesObserved,
			"frames_decided":    res.FramesDecided,
			"effects_delivered": res.EffectsDispatched,
			"effects_blocked":   res.BlockedEffects,
			"boundary_reached":  res.BoundaryReached,
			"dry_run":           *dryRun,
		})
	}
	fmt.Fprintf(stdout, "run finished for %s\n", state.RequestID)
	fmt.Fprintf(stdout, "  stage:     %s\n", res.Stage)
	fmt.Fprintf(stdout, "  frames:    %d observed, %d decided\n", res.FramesObserved, res.FramesDecided)
	fmt.Fprintf(stdout, "  delivered: %d, blocked: %d\n", res.EffectsDispatched, res.BlockedEffects)
	if !res.BoundaryReached && !*once {
		fmt.Fprintln(stdout, "  note: the event follower exited; the request is not finished and must be reconciled")
	}
	return 0
}

// requestIDFromFile reads the request_id out of a request file so `start` can
// admit and then run the same request without the caller restating the ID.
//
// The ID is read from the file rather than trusted from a flag, because the file
// is what admission will actually accept; deriving the run target from anything
// else would let the two steps disagree about which request is being started.
func requestIDFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var payload struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return payload.RequestID
}

// runtimeLockName is the reviewed dependency manifest shipped beside the binary.
const runtimeLockName = "runtime-lock.json"

// pinnedRuntime verifies the configured Agent Deck executable against the
// operator's approved artifact identity and returns a runtime for it.
//
// Verification happens here, before the engine exists, so there is no window in
// which a supervisor is running against an unverified executable. The order is
// deliberate:
//
//  1. A lock manifest must be locatable and readable.
//  2. The configured binary must be an absolute path and must resolve to the
//     approved one — a bare name could resolve to anything on PATH.
//  3. Its measured digest must equal the approved digest.
//
// Any failure is a refusal with the specific reason, because "the runtime is
// wrong" is not actionable and an operator who cannot act will work around it.
func pinnedRuntime(cfg *control.Config, stderr io.Writer, verbose bool) (*agentdeck.CLIRuntime, error) {
	lockPath, err := locateRuntimeLock()
	if err != nil {
		return nil, err
	}
	lock, err := agentdeck.LoadRuntimeLock(lockPath)
	if err != nil {
		return nil, err
	}
	// An operator may also pin inline. When both are present they must agree:
	// a config that names a different commit than the lock is a contradiction,
	// and choosing one silently would be picking a side in a disagreement about
	// what is approved.
	if cfg.AgentDeckSourceCommit != "" && cfg.AgentDeckSourceCommit != lock.SourceCommit {
		return nil, fmt.Errorf("agent-engine: the configuration pins source commit %s but %s approves %s; "+
			"reconcile the operator config with the lock rather than choosing one",
			cfg.AgentDeckSourceCommit, runtimeLockName, lock.SourceCommit)
	}
	pin, err := agentdeck.VerifyPin(lock, cfg.AgentDeckBinary)
	if err != nil {
		return nil, err
	}
	if cfg.AgentDeckBinarySHA256 != "" && !strings.EqualFold(cfg.AgentDeckBinarySHA256, pin.SHA256) {
		return nil, fmt.Errorf("agent-engine: the configuration pins artifact %s but the executable measures %s; "+
			"it has been replaced since it was reviewed", cfg.AgentDeckBinarySHA256, pin.SHA256)
	}
	if verbose {
		fmt.Fprintf(stderr, "  runtime pinned: %s\n", pin.String())
	}
	rt := agentdeck.NewCLIRuntime(pin.Path, cfg.Profile)
	if verbose {
		// Diagnostics stay on stderr so a JSON caller can parse stdout cleanly.
		rt.Log = func(line string) { fmt.Fprintln(stderr, "  "+line) }
	}
	return rt, nil
}

// locateRuntimeLock finds the lock manifest next to the executable, in the
// working directory, or at an operator-configured absolute path.
func locateRuntimeLock() (string, error) {
	var tried []string
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), runtimeLockName)
		tried = append(tried, p)
		if _, serr := os.Stat(p); serr == nil {
			return p, nil
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		p := filepath.Join(cwd, runtimeLockName)
		tried = append(tried, p)
		if _, serr := os.Stat(p); serr == nil {
			return p, nil
		}
	}
	if env := strings.TrimSpace(os.Getenv("AGENT_ENGINE_RUNTIME_LOCK")); env != "" {
		tried = append(tried, env)
		if _, serr := os.Stat(env); serr == nil {
			return env, nil
		}
	}
	return "", fmt.Errorf("agent-engine: no %s was found (looked in %s); a runtime with no approved artifact "+
		"identity cannot be launched, because nothing would be able to say what it drove",
		runtimeLockName, strings.Join(tried, ", "))
}

// requestFileArg extracts the request file from a start invocation.
//
// It reads the value rather than removing the pair, because the submission still
// needs it. A missing or repeated value is an error with a specific reason: the
// alternative is forwarding an empty path and letting a later stage fail with a
// message about the wrong thing.
func requestFileArg(args []string) (string, error) {
	var found string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--request-file" || args[i] == "-request-file":
			if i+1 >= len(args) {
				return "", errors.New("--request-file needs a path")
			}
			if found != "" {
				return "", errors.New("--request-file was given more than once; start admits exactly one request")
			}
			found = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--request-file="):
			if found != "" {
				return "", errors.New("--request-file was given more than once; start admits exactly one request")
			}
			found = strings.TrimPrefix(args[i], "--request-file=")
		}
	}
	if strings.TrimSpace(found) == "" {
		return "", errors.New("start needs --request-file so the request can be admitted before it runs")
	}
	return found, nil
}

// requestIDFromFileStrict reads the request ID, refusing rather than returning "".
//
// An empty ID used to flow straight into a --request-id argument, which fails
// later with a message about a missing request. Naming the actual problem — the
// file could not be read, or carried no request_id — is the difference between a
// diagnosable failure and a confusing one.
func requestIDFromFileStrict(path string) (string, error) {
	if path == "-" {
		// Submission can read the request from stdin, but the ID has to be known
		// before the run starts, so a piped request cannot be started in one step.
		return "", errors.New("start cannot read a request from stdin; the request id must be known " +
			"before the run begins, so pass --request-file with a path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("agent-engine: the request file cannot be read: %w", err)
	}
	var payload struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("agent-engine: the request file is not readable JSON: %w", err)
	}
	if strings.TrimSpace(payload.RequestID) == "" {
		return "", fmt.Errorf("agent-engine: the request file %s carries no request_id", path)
	}
	return payload.RequestID, nil
}
