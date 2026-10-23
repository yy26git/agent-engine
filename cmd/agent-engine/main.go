// Command agent-engine admits engineering requests and publishes their
// durable results.
//
// Two rules shape every command here. First, --store is mandatory and absolute:
// a request is located by its store and its own ID, never by the current
// working directory, so two stores holding the same request ID can never be
// confused for each other and no hidden index has to exist to disambiguate them.
// Second, machine-readable output goes to stdout and everything else goes to
// stderr, so a caller can pipe this command's output into a JSON parser without
// having to strip a human preamble.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `agent-engine — engineering control layer

Usage:
  agent-engine --store /absolute/store <command> [flags]

Commands:
  version     identify this binary without a store or provider calls
  capabilities describe supported integration protocols without a store
  deployment-init explicitly initialize stable operator-deployment identity
  discover    inspect scoped approved flows and CLI/model/effort selections
  submit    admit a request exactly once from a file or stdin
  status    print the current stage and counters for a request
  flows     inspect approved episode templates and their role selections
  profiles  list approved CLI/provider/model selections for each role
  chats     show retained role chats and inspection paths without starting agents
  qualify   retain real Codex sandbox qualification through pinned Agent Deck
  result    print the durable result for a request
  cancel      record cancellation intent for a request
  checkpoint  submit a typed worker checkpoint
  evidence    record one immutable, digest-pinned evidence item
  attempt     report a failed attempt against the objective cycle budget
  review-result  answer an open review with a correlated, typed outcome
  question    raise a durable question
  decision    record a proposed or accepted durable decision
  bind        attach the worker and auditor session IDs for a request
  run         supervise an admitted request until cancelled or --once completes
  serve       expose authenticated episode routes on literal loopback
  hud         start, inspect or stop the optional macOS floating companion
  episode     admit, inspect artifacts, accept plans and progress episodes
  program     digest an immutable work order or admit one locally authorized root card
  prepare-repair  create a separate worktree on a new per-episode repair branch
  prepare-repair-bundle create a durably journaled multi-repository worktree bundle
  start       submit a request and then run it

ACP starts from the models and permissions pinned in admitted profiles. Each
profile owns an isolated process and an operator-retained MINIMAX-RELEASE.json
locks the release payload and bundled Node. Dry runs cannot start ACP processes.

Execution and configuration commands require --store. Version/capabilities do not.
There is no implicit working-directory lookup.
`

// flagSet returns a subcommand flag set that reports errors to stderr.
func flagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func run(args []string, stdout, stderr io.Writer) int {
	// The global flags are accepted in any position, before or after the
	// subcommand. Both orderings are natural to write and the plan's own
	// documented syntax puts --json last, so requiring one fixed order would
	// reject correct invocations.
	globals, rest := extractGlobals(args)

	fs := flag.NewFlagSet("agent-engine", flag.ContinueOnError)
	fs.SetOutput(stderr)
	store := fs.String("store", "", "absolute path to the operator-approved request store (required)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON on stdout")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	if err := fs.Parse(globals); err != nil {
		return 2
	}
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	if rest[0] == "version" || rest[0] == "capabilities" {
		return cmdIdentity(rest[0], rest[1:], stdout, stderr, *asJSON)
	}
	// The optional HUD lifecycle is independent of any operator store. It only
	// manages the native observation companion; it never starts the Engine
	// service or a workflow.
	if rest[0] == "hud" {
		return cmdHUD(rest[1:], stdout, stderr, *asJSON)
	}
	if strings.TrimSpace(*store) == "" {
		return fail(stdout, stderr, *asJSON, errors.New("--store is required: a request is identified by its store and its own ID, never by the working directory"))
	}
	if !strings.HasPrefix(*store, "/") {
		return fail(stdout, stderr, *asJSON, fmt.Errorf("--store must be an absolute path, got %q", *store))
	}

	cfg, err := control.LoadConfig(*store)
	if err != nil {
		return fail(stdout, stderr, *asJSON, err)
	}
	st := control.NewStore(cfg)

	command, rest := rest[0], rest[1:]
	switch command {
	case "deployment-init":
		d, err := st.InitializeDeployment()
		if err != nil {
			return fail(stdout, stderr, *asJSON, err)
		}
		return emit(stdout, stderr, d)
	case "discover":
		c, err := st.Discover(nil)
		if err != nil {
			return fail(stdout, stderr, *asJSON, err)
		}
		return emit(stdout, stderr, c)
	case "flows":
		return cmdFlows(st, rest, stdout, stderr, *asJSON)
	case "prepare-repair":
		return cmdPrepareRepair(st, rest, stdout, stderr, *asJSON)
	case "prepare-repair-bundle":
		return cmdPrepareRepairBundle(st, rest, stdout, stderr, *asJSON)
	case "qualify":
		return cmdQualify(st, rest, stdout, stderr, *asJSON)
	case "chats":
		return cmdChats(st, rest, stdout, stderr, *asJSON)
	case "serve":
		return cmdServe(st, rest, stdout, stderr, *asJSON)
	case "profiles":
		return cmdProfiles(st, rest, stdout, stderr, *asJSON)
	case "submit":
		return cmdSubmit(st, rest, stdout, stderr, *asJSON)
	case "start":
		// Convenience only. It must go through the same admission path, so the
		// idempotency guarantee cannot be bypassed by starting a request.
		//
		// The two invocations need DIFFERENT arguments: submission needs the
		// request file, and the run needs the admitted request's ID. This
		// previously stripped --request-file from the submission arguments, which
		// left submit with nothing to read and made `start` unable to admit
		// anything at all. It also aliased the caller's slice while doing so, so
		// any trailing flag was shifted into the wrong position. Both argument
		// lists are now built independently and neither mutates `rest`.
		requestFile, err := requestFileArg(rest)
		if err != nil {
			return fail(stdout, stderr, *asJSON, err)
		}
		// A start that cannot name the request it just admitted would run an
		// unknown one, so the ID is read from the file and a failure is fatal
		// rather than being turned into an empty --request-id.
		requestID, rerr := requestIDFromFileStrict(requestFile)
		if rerr != nil {
			return fail(stdout, stderr, *asJSON, rerr)
		}
		if code := cmdSubmit(st, []string{"--request-file", requestFile}, stdout, stderr, *asJSON); code != 0 {
			return code
		}
		return cmdRun(st, []string{"--request-id", requestID}, stdout, stderr, *asJSON)
	case "task-exec":
		return cmdTaskExec(st, rest, stdout, stderr, *asJSON)
	case "episode":
		return cmdEpisode(st, rest, stdout, stderr, *asJSON)
	case "program":
		return cmdProgram(st, rest, stdout, stderr, *asJSON)
	case "workflow":
		return cmdWorkflow(st, rest, stdout, stderr, *asJSON)
	case "status":
		return cmdStatus(st, rest, stdout, stderr, *asJSON)
	case "result":
		return cmdResult(st, rest, stdout, stderr, *asJSON)
	case "cancel":
		return cmdCancel(st, rest, stdout, stderr, *asJSON)
	case "checkpoint":
		return cmdCheckpoint(st, rest, stdout, stderr, *asJSON)
	case "evidence":
		return cmdEvidence(st, rest, stdout, stderr, *asJSON)
	case "attempt":
		return cmdAttempt(st, rest, stdout, stderr, *asJSON)
	case "review-result":
		return cmdReviewResult(st, rest, stdout, stderr, *asJSON)
	case "question":
		return cmdQuestion(st, rest, stdout, stderr, *asJSON)
	case "decision":
		return cmdDecision(st, rest, stdout, stderr, *asJSON)
	case "bind":
		return cmdBind(st, rest, stdout, stderr, *asJSON)
	case "run":
		return cmdRun(st, rest, stdout, stderr, *asJSON)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		return fail(stdout, stderr, *asJSON, fmt.Errorf("unknown command %q", command))
	}
}

// extractGlobals pulls --store and --json out of an argument list wherever they
// appear, returning the globals and everything else.
//
// This is a narrow, explicit transformation rather than a second flag parser:
// only these two flags are global, and a subcommand's own flags must reach that
// subcommand untouched.
func extractGlobals(args []string) (globals, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			globals = append(globals, "--json")
		case a == "--store":
			if i+1 < len(args) {
				globals = append(globals, "--store", args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--store="):
			globals = append(globals, "--store="+strings.TrimPrefix(a, "--store="))
		default:
			rest = append(rest, a)
		}
	}
	return globals, rest
}

func cmdSubmit(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("request-file", "", "path to a request JSON file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-file is required (use - for stdin)"))
	}

	raw, err := readInput(*file)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	req, err := contract.DecodeRequest(raw)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	adm, err := st.Admit(req)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, adm)
	}
	if adm.Duplicate {
		// A duplicate is a successful outcome, not an error: the caller learned
		// that their request is already admitted and nothing new was started.
		// Exiting non-zero here would make a safe retry look like a failure and
		// invite the caller to keep trying.
		fmt.Fprintf(stdout, "request %s was already admitted (digest %s); nothing new was started\n",
			adm.RequestID, adm.RequestDigest)
		return 0
	}
	fmt.Fprintf(stdout, "admitted %s\n  digest:     %s\n  task:       %s\n  stage:      %s\n  store:      %s\n  path:       %s\n",
		adm.RequestID, adm.RequestDigest, adm.TaskID, adm.Stage, adm.Store, adm.Path)
	return 0
}

func cmdStatus(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("request-id", "", "admitted request identifier")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	state, err := st.State(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	req, err := st.Request(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	summary := map[string]any{
		"request_id":          state.RequestID,
		"request_sha256":      state.RequestDigest,
		"policy_digest":       state.PolicyDigest,
		"task_id":             state.TaskID,
		"operation":           req.Operation,
		"stage":               state.Stage,
		"stage_reason":        state.StageReason,
		"workspace":           state.Workspace,
		"generation":          state.Generation,
		"execution_profiles":  state.ExecutionProfiles,
		"execution_assurance": state.ExecutionAssurance,
		"repair_branch":       state.RepairBranch,
		"repair_base_head":    state.RepairBaseHead,
		"checkpoint_session":  state.CheckpointSession,
		"diagnostic_session":  state.DiagnosticSession,
		"revision":            state.Revision,
		"obligations":         state.Obligations,
		"checks":              state.Checks,
		"repair_cycles":       state.RepairCycles,
		"audit_attempts":      state.AuditAttempts,
		"diagnostic_rounds":   state.DiagnosticRounds,
		"terminal":            state.Stage.Terminal(),
		// The bound sessions and the open review are part of the operational
		// picture, not internal trivia. A reviewer has to be told which session it
		// is and which nonce to echo, and an operator has to be able to see that
		// the two roles are held by different sessions.
		"worker_session":  state.WorkerSession,
		"auditor_session": state.AuditorSession,
		"active_review":   state.ActiveReview,
	}
	if asJSON {
		return emit(stdout, stderr, summary)
	}
	fmt.Fprintf(stdout, "%s  stage %s (generation %d, revision %d)\n", state.RequestID, state.Stage, state.Generation, state.Revision)
	if state.StageReason != "" {
		fmt.Fprintf(stdout, "  reason:      %s\n", state.StageReason)
	}
	fmt.Fprintf(stdout, "  operation:   %s\n", req.Operation)
	fmt.Fprintf(stdout, "  workspace:   %s\n", state.Workspace)
	if state.ExecutionAssurance != "" {
		fmt.Fprintf(stdout, "  execution:   %s\n", state.ExecutionAssurance)
	}
	fmt.Fprintf(stdout, "  cycles:      %d/%d   audits: %d/%d   rounds: %d/%d\n",
		state.RepairCycles, state.Limits.RepairCycles,
		state.AuditAttempts, state.Limits.AuditAttempts,
		state.DiagnosticRounds, state.Limits.GuidanceRounds)
	for _, o := range state.Obligations {
		fmt.Fprintf(stdout, "  [%s] %s — %s\n", o.Disposition, o.ID, o.Criterion)
	}
	for _, role := range []control.ProfileRole{control.RoleWorker, control.RoleCheckpoint, control.RoleDiagnostic, control.RoleReviewer} {
		if p, ok := state.ExecutionProfiles[role]; ok {
			model, effort, provider := p.Model, p.Effort, p.Provider
			if model == "" {
				model = "runtime default (not explicitly selected)"
			}
			if effort == "" {
				effort = "runtime default"
			}
			if provider == "" {
				provider = "native CLI configuration"
			}
			fmt.Fprintf(stdout, "  %s: CLI=%s provider=%s model=%s reasoning=%s\n", role, p.Tool, provider, model, effort)
		}
	}
	return 0
}

func cmdProfiles(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("profiles", stderr)
	selected := fs.String("role", "", "filter worker, checkpoint, diagnostic, reviewer or investigation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	roles := []control.ProfileRole{control.RoleWorker, control.RoleCheckpoint, control.RoleDiagnostic, control.RoleReviewer, control.RoleInvestigat}
	if *selected != "" {
		role := control.ProfileRole(*selected)
		valid := false
		for _, r := range roles {
			if role == r {
				valid = true
			}
		}
		if !valid {
			return fail(stdout, stderr, asJSON, fmt.Errorf("unknown profile role %q", *selected))
		}
		roles = []control.ProfileRole{role}
	}
	out := []map[string]any{}
	for _, role := range roles {
		for _, name := range st.Config().ProfilesFor(role) {
			p := st.Config().Profiles[name]
			_, err := st.Config().ResolveProfile(name, role)
			row := map[string]any{"selection": p.Ref(), "launchable": err == nil}
			if err != nil {
				row["reason"] = err.Error()
			}
			out = append(out, row)
			if !asJSON {
				fmt.Fprintf(stdout, "%s role=%s CLI=%s provider=%s model=%s reasoning=%s launchable=%t\n", name, role, p.Tool, p.Provider, p.Model, p.Effort, err == nil)
			}
		}
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{"profiles": out})
	}
	return 0
}

func cmdResult(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flag.NewFlagSet("result", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("request-id", "", "admitted request identifier")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	res, err := st.Result(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, res)
	}
	if !res.Ready {
		fmt.Fprintf(stdout, "%s is not finished: stage %s\n", res.RequestID, res.Stage)
		if res.Reason != "" {
			fmt.Fprintf(stdout, "  reason: %s\n", res.Reason)
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s  %s\n", res.RequestID, res.Status)
	if res.Summary != "" {
		fmt.Fprintf(stdout, "  summary: %s\n", res.Summary)
	}
	for _, f := range res.Findings {
		fmt.Fprintf(stdout, "  finding [%s]: %s\n", f.Criterion, f.Summary)
	}
	return 0
}

func cmdCancel(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("request-id", "", "admitted request identifier")
	why := fs.String("reason", "", "why cancellation was requested")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-id is required"))
	}
	// Cancellation records intent. It does not declare a terminal outcome,
	// because the effects of cancellation still have to be reconciled — a
	// request is CANCELLED only once its owned work is accounted for.
	state, err := st.UpdateCommitted(*id, func(s *control.State) error {
		if s.Stage.Terminal() {
			return fmt.Errorf("agent-engine: request %s is already terminal as %s", s.RequestID, s.ResultStatus)
		}
		s.CancellationRequested = true
		s.CancellationReason = *why
		s.StageReason = "cancellation requested; effects pending reconciliation"
		return nil
	})
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id": state.RequestID,
			"stage":      state.Stage,
			"reason":     state.StageReason,
		})
	}
	fmt.Fprintf(stdout, "cancellation recorded for %s; effects pending reconciliation\n", state.RequestID)
	return 0
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("agent-engine: reading request from stdin: %w", err)
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, errors.New("agent-engine: no request on stdin")
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent-engine: reading %s: %w", path, err)
	}
	return data, nil
}

func emit(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "agent-engine: encoding output: %v\n", err)
		return 1
	}
	return 0
}

// digestOf is the content digest of a report body, used as the caller's claim
// that a committed report backs an approving answer. The store verifies the
// committed bytes against state independently.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fail reports a refusal.
//
// With --json the structured refusal goes to stdout, because a caller parsing
// machine output must be able to see the failure without parsing prose, and
// stderr is reserved for diagnostics. The human-readable form always goes to
// stderr.
func fail(stdout, stderr io.Writer, asJSON bool, err error) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		fmt.Fprintf(stderr, "agent-engine: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "agent-engine: %v\n", err)
	return 1
}
