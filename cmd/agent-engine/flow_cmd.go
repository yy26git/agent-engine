package main

// Agent conveniences translate data into the existing admission and execution
// owners. Discovery, preparation and watching never start model processes.
import (
	"context"
	"crypto/sha256"
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

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

type stringsFlag []string

func (v *stringsFlag) String() string     { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(s string) error { *v = append(*v, s); return nil }

func cmdFlows(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("flows", stderr)
	name := fs.String("template", "", "inspect one registered template")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	names := st.Config().EpisodeTemplates.Names()
	if *name != "" {
		names = []string{*name}
	}
	rows, err := flowRows(st, names)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{"flows": rows, "policy_digest": st.Config().Digest, "structured_output_version": 1})
	}
	for _, row := range rows {
		b, _ := json.MarshalIndent(row, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
	return 0
}

func flowRows(st *control.Store, names []string) ([]map[string]any, error) {
	rows := []map[string]any{}
	for _, name := range names {
		tpl, ok := st.Config().EpisodeTemplates.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown approved template %q", name)
		}
		roles := map[string]any{}
		for role, profileName := range tpl.Roles {
			p, ok := st.Config().Profiles[profileName]
			if !ok {
				roles[string(role)] = map[string]any{"profile": profileName, "configured": false}
				continue
			}
			roles[string(role)] = map[string]any{"selection": projectProfile(p.Ref()), "configured": true}
		}
		acceptance := "local operator; never self-approved by a worker"
		if tpl.ReportOnly {
			acceptance = "not applicable; report-only data completion"
		}
		rows = append(rows, map[string]any{"name": name, "phases": tpl.Phases, "roles": roles,
			"required_checks": tpl.RequiredChecks, "return_edges": tpl.ReturnEdges,
			"repair_enabled": tpl.RejectionDisposition() == "bounded_return_edges", "rejection_disposition": tpl.RejectionDisposition(),
			"report_only": tpl.ReportOnly, "structured_tasks": tpl.ReportOnly, "read_only": tpl.ReadOnly, "max_repair_iterations": tpl.MaxRepairIterations,
			"repair_branch_prefix": tpl.RepairBranchPrefix, "plan_acceptance": acceptance,
			"readiness": "configuration only; use episode validate for a concrete request"})
	}
	return rows, nil
}

func cmdEpisodePrepare(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode prepare", stderr)
	id := fs.String("episode-id", "", "stable caller-chosen episode ID")
	template := fs.String("template", "", "registered flow name; see flows")
	workspace := fs.String("workspace", "", "absolute prepared workspace path")
	objective := fs.String("objective", "", "intended behavior")
	caller := fs.String("caller-id", "", "caller identity")
	origin := fs.String("origin", "agent", "caller kind")
	bundle := fs.String("repair-bundle-sha256", "", "prepared bundle receipt digest")
	output := fs.String("output", "", "new JSON request file; omission prints request")
	writable := fs.Bool("allow-code-changes", false, "explicit request for code changes")
	planFile := fs.String("plan-file", "", "existing UTF-8 implementation plan to preserve and review instead of drafting a replacement")
	var criteria, allowed, protected stringsFlag
	fs.Var(&criteria, "criterion", "falsifiable obligation text (repeatable)")
	fs.Var(&allowed, "allowed-path", "allowed path pattern (repeatable)")
	fs.Var(&protected, "protected-path", "protected path pattern (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tpl, ok := st.Config().EpisodeTemplates.Lookup(*template)
	if !ok {
		return fail(stdout, stderr, asJSON, fmt.Errorf("unknown approved template %q; inspect flows first", *template))
	}
	for _, pattern := range append(append([]string{}, allowed...), protected...) {
		if strings.TrimSpace(pattern) == "" {
			return fail(stdout, stderr, asJSON, fmt.Errorf("path patterns must not be blank"))
		}
	}
	if *writable && len(allowed) == 0 {
		return fail(stdout, stderr, asJSON, fmt.Errorf("code changes require at least one explicit --allowed-path"))
	}
	if tpl.ReadOnly && *writable {
		return fail(stdout, stderr, asJSON, fmt.Errorf("read-only template cannot authorize code changes"))
	}
	checks := make([]contract.VerificationItem, 0, len(tpl.RequiredChecks))
	for _, id := range tpl.RequiredChecks {
		if _, err := st.Config().Check(id); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		checks = append(checks, contract.VerificationItem{CheckID: id})
	}
	obligations := make([]contract.WorkflowObligation, 0, len(criteria))
	for i, criterion := range criteria {
		if strings.TrimSpace(criterion) == "" {
			return fail(stdout, stderr, asJSON, fmt.Errorf("criterion must not be blank"))
		}
		obligations = append(obligations, contract.WorkflowObligation{ID: fmt.Sprintf("O-%d", i+1), Criterion: criterion})
	}
	ep := contract.EpisodeRequest{Version: contract.EpisodeVersion, EpisodeID: *id, Template: *template,
		Origin: contract.Origin{Kind: *origin, CallerID: *caller}, Workspace: contract.Workspace{Path: *workspace},
		Objective: *objective, RepairBundleSHA256: *bundle, Obligations: obligations, Verification: checks,
		Constraints: contract.Constraints{AllowCodeChanges: *writable, AllowedPaths: allowed, ProtectedPaths: protected}}
	if *planFile != "" {
		f, err := os.Open(*planFile)
		if err != nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("open imported plan: %w", err))
		}
		body, err := io.ReadAll(io.LimitReader(f, contract.MaxImportedPlanBytes+1))
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("read imported plan: %w", err))
		}
		ep.ImportedPlan = &contract.ImportedPlanDocument{Name: filepath.Base(*planFile), Content: string(body), SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
		if err := contract.ValidateImportedPlanDocument(ep.ImportedPlan); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
	}
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	decoded, err := contract.DecodeEpisodeRequest(b)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	op := contract.OpInvestigate
	if !tpl.ReadOnly {
		op = contract.OpRepair
	}
	req := contract.Request{Version: contract.Version, RequestID: decoded.EpisodeID, Origin: ep.Origin, Operation: op,
		Workspace: ep.Workspace, Objective: ep.Objective, Constraints: ep.Constraints, Verification: ep.Verification}
	for _, ob := range ep.Obligations {
		req.Obligations = append(req.Obligations, contract.Obligation{ID: ob.ID, Criterion: ob.Criterion})
	}
	if err := req.Validate(); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	b = append(b, '\n')
	if *output == "" {
		_, err = stdout.Write(b)
	} else {
		var f *os.File
		f, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, err = f.Write(b)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err == nil {
			return emit(stdout, stderr, map[string]any{"request_file": *output, "episode_id": *id, "admitted": false, "next_action": "episode validate --request-file"})
		}
	}
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return 0
}

func readEpisodeFile(file string) (*contract.EpisodeRequest, error) {
	if strings.TrimSpace(file) == "" {
		return nil, fmt.Errorf("--request-file is required")
	}
	b, err := readInput(file)
	if err != nil {
		return nil, err
	}
	return contract.DecodeEpisodeRequest(b)
}

type prerequisiteError struct {
	step string
	err  error
}

func (e *prerequisiteError) Error() string { return e.err.Error() }
func (e *prerequisiteError) Unwrap() error { return e.err }

func episodePreflight(st *control.Store, ep *contract.EpisodeRequest) error {
	info, err := os.Stat(ep.Workspace.Path)
	if err != nil {
		return &prerequisiteError{"workspace", err}
	}
	if !info.IsDir() {
		return &prerequisiteError{"workspace", fmt.Errorf("workspace must be an existing directory")}
	}
	if err := st.ValidateEpisode(ep); err != nil {
		return &prerequisiteError{"admission_policy", err}
	}
	tpl, _ := st.Config().EpisodeTemplates.Lookup(ep.Template)
	refs := []control.ProfileRef{}
	for _, name := range tpl.Roles {
		refs = append(refs, st.Config().Profiles[name].Ref())
	}
	if err := requireCodexRuntimeHome(refs); err != nil {
		return &prerequisiteError{"codex_home", err}
	}
	for _, id := range tpl.RequiredChecks {
		if _, err := st.Config().Check(id); err != nil {
			return err
		}
	}
	for _, name := range tpl.Roles {
		if st.Config().Profiles[name].TransportFor() == control.TransportMiniMaxACP {
			if err := verifyACPRelease(st); err != nil {
				return &prerequisiteError{"minimax_release", err}
			}
			break
		}
	}
	_, err = pinnedRuntime(st.Config(), io.Discard, false)
	if err != nil {
		return &prerequisiteError{"upstream_pin", err}
	}
	return nil
}

func prerequisiteRefusal(err error) map[string]any {
	code, actor, action := "invalid_request", "caller", "correct the request fields and validate again"
	var prerequisite *prerequisiteError
	if errors.As(err, &prerequisite) {
		code, actor = "prerequisite_"+prerequisite.step, "local operator"
		action = "resolve " + prerequisite.step + " using the reported error and docs/agent-quickstart.md, then validate again"
	}
	return map[string]any{"ok": false, "ready": false, "error": err.Error(), "code": code,
		"responsible_actor": actor, "next_action": action, "agents_started": false}
}

func cmdEpisodeValidate(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode validate", stderr)
	file := fs.String("request-file", "", "episode JSON file or -")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ep, err := readEpisodeFile(*file)
	if err == nil {
		err = episodePreflight(st, ep)
	}
	if err != nil {
		if asJSON {
			_ = emit(stdout, stderr, prerequisiteRefusal(err))
			fmt.Fprintln(stderr, err)
			return 1
		}
		return fail(stdout, stderr, false, err)
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "ready": true, "episode_id": ep.EpisodeID,
		"policy_digest": st.Config().Digest, "agents_started": false, "live_provider_proven": false,
		"next_action": "episode launch --request-file (admission and launch recheck policy)"})
}

func cmdEpisodeLaunch(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode launch", stderr)
	file := fs.String("request-file", "", "episode JSON file (not stdin)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "-" {
		return fail(stdout, stderr, asJSON, fmt.Errorf("launch requires a retained request file for exact retry"))
	}
	ep, err := readEpisodeFile(*file)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	// Existing exact retries reconcile through admission's frozen state; do not
	// require a newly expired live grant to observe a completed admission.
	adm, err := st.AdmitEpisode(ep)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	fmt.Fprintf(stderr, "admitted %s; supervising %s\n", ep.EpisodeID, adm.RequestID)
	return cmdRun(st, []string{"--request-id", adm.RequestID}, stdout, stderr, asJSON)
}

func cmdEpisodeWatch(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode watch", stderr)
	id := fs.String("episode-id", "", "admitted episode ID")
	interval := fs.Duration("interval", 2*time.Second, "read-only polling interval (minimum 100ms)")
	timeout := fs.Duration("timeout", 5*time.Minute, "bounded watch duration; timeout exits 3")
	events := fs.Bool("events", false, "emit durable event frames with resumable cursors")
	follow := fs.Bool("follow", false, "keep observing while local plan acceptance is pending")
	cursor := fs.String("cursor", "", "resume after a delivered event ID")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" || *interval < 100*time.Millisecond || *timeout <= 0 {
		return fail(stdout, stderr, asJSON, fmt.Errorf("episode-id, interval >=100ms and positive timeout required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if *events || *follow || *cursor != "" {
		err := observeCLI(st, ctx, *id, *cursor, *interval, *follow, func(frame control.ProgressFrame) error {
			if asJSON {
				return json.NewEncoder(stdout).Encode(frame)
			}
			_, err := fmt.Fprintf(stdout, "%s phase=%s stage=%s: %s\n", frame.EpisodeID, frame.Phase, frame.Stage, frame.NextAction)
			return err
		})
		if err == context.Canceled || err == context.DeadlineExceeded {
			fmt.Fprintln(stderr, "watch stopped; episode execution was not cancelled")
			return 3
		}
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		return 0
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	previous := ""
	for {
		state, err := st.State(*id)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		if state.Episode == nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("request is not an episode"))
		}
		view := state.ProgressSnapshot()
		action := view.NextAction
		waiting := view.RequiresAction
		row := map[string]any{"episode_id": *id, "phase": state.Episode.Phase, "stage": state.Stage, "stage_reason": view.Reason,
			"terminal": state.Stage.Terminal(), "result_status": state.ResultStatus, "result_digest": state.ResultDigest,
			"plan_digest": state.Episode.PlanDigest, "waiting_for_operator": view.WaitingForOperator, "requires_action": view.RequiresAction, "action_kind": view.ActionKind, "next_action": action,
			"budget": state.EpisodeBudgetRemaining(), "observer_only": true}
		for i := len(state.Episode.Execution.Steps) - 1; i >= 0; i-- {
			step := state.Episode.Execution.Steps[i]
			if step.Phase == state.Episode.Phase {
				row["current_step"] = map[string]any{"status": step.Status, "reason": step.Reason, "role": step.Role, "profile": step.Profile, "session_id": step.Session, "model_echo": step.ModelEcho}
				break
			}
		}
		b, _ := json.Marshal(row)
		if string(b) != previous {
			if asJSON {
				fmt.Fprintln(stdout, string(b))
			} else {
				fmt.Fprintf(stdout, "%s phase=%s stage=%s: %s\n", *id, state.Episode.Phase, state.Stage, action)
			}
			previous = string(b)
		}
		if state.Stage.Terminal() || waiting {
			return 0
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "watch stopped; episode execution was not cancelled")
			return 3
		case <-ticker.C:
		}
	}
}
