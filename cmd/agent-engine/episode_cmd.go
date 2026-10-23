package main

// Episode admission and progression.
//
// Episodes are a SEPARATELY VERSIONED admission shape from ordinary task and
// workflow requests, so they get their own verb rather than another flag on an
// existing one. A caller that must know which shape it is sending is a caller
// that cannot accidentally send the wrong one.
//
// The verb admits against LOCALLY approved policy and nothing else. A caller
// supplies an episode id, a workspace, an objective and a TEMPLATE NAME. It
// cannot supply phases, roles, checks, argv or capability: those come from the
// operator's CONTROL.toml, because a request that described its own execution
// would be asking to be judged under a grant it wrote itself.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func cmdEpisode(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	if len(args) == 0 {
		return fail(stdout, stderr, asJSON, errors.New("episode requires a subcommand: prepare, validate, launch, watch, admit, status, artifact, handover, accept-plan, drive, request-worker-release"))
	}
	switch args[0] {
	case "prepare":
		return cmdEpisodePrepare(st, args[1:], stdout, stderr, asJSON)
	case "validate":
		return cmdEpisodeValidate(st, args[1:], stdout, stderr, asJSON)
	case "launch":
		return cmdEpisodeLaunch(st, args[1:], stdout, stderr, asJSON)
	case "watch":
		return cmdEpisodeWatch(st, args[1:], stdout, stderr, asJSON)
	case "artifact":
		return cmdEpisodeArtifact(st, args[1:], stdout, stderr, asJSON, false)
	case "handover":
		return cmdEpisodeHandover(st, args[1:], stdout, stderr, asJSON)
	case "accept-plan":
		return cmdEpisodeArtifact(st, args[1:], stdout, stderr, asJSON, true)
	case "admit":
		return cmdEpisodeAdmit(st, args[1:], stdout, stderr, asJSON)
	case "status":
		return cmdEpisodeStatus(st, args[1:], stdout, stderr, asJSON)
	case "drive":
		return cmdEpisodeDrive(st, args[1:], stdout, stderr, asJSON)
	case "request-worker-release":
		return cmdEpisodeWorkerRelease(st, args[1:], stdout, stderr, asJSON)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "Usage: agent-engine --store /absolute/store episode <prepare|validate|launch|watch|admit|status|artifact|handover|accept-plan|drive|request-worker-release> [flags]")
		return 0
	default:
		return fail(stdout, stderr, asJSON, fmt.Errorf("unknown episode subcommand %q", args[0]))
	}
}

func cmdEpisodeHandover(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode handover", stderr)
	id := fs.String("episode-id", "", "admitted episode identifier")
	digest := fs.String("digest", "", "read one retained handover digest instead of creating the latest snapshot")
	output := fs.String("output", "", "private absolute export file; omit or use - for stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--episode-id is required"))
	}
	var h control.EpisodeHandover
	var body []byte
	var artifactDigest string
	var err error
	if strings.TrimSpace(*digest) != "" {
		h, body, err = st.ReadEpisodeHandoverDigest(*id, strings.TrimSpace(*digest))
		artifactDigest = strings.TrimSpace(*digest)
	} else {
		h, artifactDigest, err = st.CreateEpisodeHandover(*id)
		if err == nil {
			body, err = st.ReadEpisodeArtifact(*id, artifactDigest)
		}
	}
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if *output == "" || *output == "-" {
		if asJSON {
			return emit(stdout, stderr, map[string]any{"sha256": artifactDigest, "handover": h})
		}
		if _, err := stdout.Write(append(body, '\n')); err != nil {
			return 1
		}
		fmt.Fprintf(stderr, "handover sha256: %s\n", artifactDigest)
		return 0
	}
	path, err := filepath.Abs(*output)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(stdout, stderr, asJSON, fmt.Errorf("create private handover export: %w", err))
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return fail(stdout, stderr, asJSON, fmt.Errorf("write private handover export: %w", err))
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{"sha256": artifactDigest, "output": path, "episode_id": h.EpisodeID, "source_state_revision": h.SourceStateRevision})
	}
	fmt.Fprintf(stdout, "saved handover %s\n  sha256: %s\n", path, artifactDigest)
	return 0
}

func cmdEpisodeAdmit(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode admit", stderr)
	file := fs.String("request-file", "", "path to a v1 episode request JSON file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-file is required"))
	}
	data, err := readInput(*file)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	ep, err := contract.DecodeEpisodeRequest(data)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	adm, err := st.AdmitEpisode(ep)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"episode_id":  ep.EpisodeID,
			"template":    ep.Template,
			"task_id":     adm.TaskID,
			"store":       st.Config().StoreRoot,
			"phase":       string(control.PhaseReport),
			"request_sha": adm.RequestDigest,
		})
	}
	fmt.Fprintf(stdout, "admitted episode %s\n", ep.EpisodeID)
	fmt.Fprintf(stdout, "  template: %s\n", ep.Template)
	fmt.Fprintf(stdout, "  task:     %s\n", adm.TaskID)
	fmt.Fprintf(stdout, "  phase:    %s\n", control.PhaseReport)
	fmt.Fprintf(stdout, "  store:    %s\n", st.Config().StoreRoot)
	return 0
}

func cmdEpisodeStatus(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode status", stderr)
	id := fs.String("episode-id", "", "admitted episode identifier")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--episode-id is required"))
	}
	state, err := st.State(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	summary := state.EpisodeSummary()
	if summary == "" {
		return fail(stdout, stderr, asJSON,
			fmt.Errorf("%s carries no episode; use `status` for an ordinary request", *id))
	}
	if asJSON {
		status := map[string]any{
			"episode_id":          state.Episode.EpisodeID,
			"execution_assurance": state.ExecutionAssurance,
			"template":            state.EpisodeTemplate,
			"phase":               state.Episode.Phase,
			"attempt":             state.Episode.Attempt,
			"report":              state.Episode.ReportDigest,
			"plan":                state.Episode.PlanDigest,
			"authorized":          state.Episode.PlanAuthorized,
			"handover_sha256":     state.EpisodeHandoverDigest,
			"budget":              state.EpisodeBudgetRemaining(),
			"phase_count":         len(state.Episode.PhaseHistory),
		}
		if state.Episode.PlanAuthorized {
			status["plan_acceptance_source"] = state.Episode.AuthorizedBy
		}
		if document := state.Episode.ImportedPlan; document != nil {
			status["imported_plan_sha256"] = document.SHA256
			status["plan_mode"] = "review_imported_plan"
		}
		if release := state.EpisodeWorkerRelease; release != nil {
			status["worker_release"] = release
		} else if digest, derr := control.WorkerReleaseBindingDigest(state); derr == nil {
			status["worker_release_binding_sha256"] = digest
		}
		if binding := state.ProgramBinding; binding != nil {
			status["program_id"] = binding.Definition.ProgramID
			status["program_sha256"] = binding.ProgramDigest
			status["program_card_id"] = binding.CardID
			status["program_card_sha256"] = binding.CardDigest
			status["program_authority_ref"] = binding.AuthorityRef
			if state.Episode.PlanAuthorized {
				for i := len(state.ProgramAcceptances) - 1; i >= 0; i-- {
					if state.ProgramAcceptances[i].PlanDigest == state.Episode.PlanDigest {
						status["program_acceptance"] = state.ProgramAcceptances[i]
						break
					}
				}
			}
		}
		return emit(stdout, stderr, status)
	}
	fmt.Fprintln(stdout, summary)
	if release := state.EpisodeWorkerRelease; release != nil {
		fmt.Fprintf(stdout, "worker release: %s", release.Status)
		if release.Reason != "" {
			fmt.Fprintf(stdout, " — %s", release.Reason)
		}
		fmt.Fprintln(stdout)
	} else if digest, derr := control.WorkerReleaseBindingDigest(state); derr == nil {
		fmt.Fprintf(stdout, "worker release binding sha256: %s\n", digest)
	}
	return 0
}

func cmdEpisodeWorkerRelease(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode request-worker-release", stderr)
	id := fs.String("episode-id", "", "admitted episode identifier")
	digest := fs.String("expected-binding-sha256", "", "exact worker binding digest from episode status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--episode-id is required"))
	}
	held, err := st.OwnershipHeld(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if !held {
		return fail(stdout, stderr, asJSON, errors.New("no active supervisor owns this episode; start or resume `run --request-id` before requesting worker release"))
	}
	release, err := st.RequestEpisodeWorkerRelease(*id, *digest)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{"worker_release": release})
	}
	fmt.Fprintf(stdout, "worker release %s for %s\n", release.Status, release.EpisodeID)
	fmt.Fprintln(stdout, "The active supervisor will consume the request; this does not authorize a replacement.")
	return 0
}

func cmdEpisodeDrive(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("episode drive", stderr)
	id := fs.String("episode-id", "", "admitted episode identifier")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--episode-id is required"))
	}
	state, err := st.State(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if state.Episode == nil {
		return fail(stdout, stderr, asJSON,
			fmt.Errorf("%s carries no episode; use `task-exec` for an ordinary request", *id))
	}
	// Advancing a phase is a state decision, not a transport operation, so it
	// needs no runtime. Requiring one would mean an episode cannot be progressed
	// without standing up a process whose identity nothing in this step uses.
	out, derr := st.DriveEpisode(*id, state.EpisodeTemplate)
	if derr != nil {
		return fail(stdout, stderr, asJSON, derr)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"episode_id": out.EpisodeID, "phase": out.Phase,
			"advanced": out.Advanced, "blocked": out.Blocked,
			"budget": out.BudgetRemaining,
		})
	}
	if out.Advanced != "" {
		fmt.Fprintf(stdout, "advanced to %s\n", out.Advanced)
	}
	if out.Blocked != "" {
		fmt.Fprintf(stdout, "waiting: %s\n", out.Blocked)
	}
	if out.Advanced == "" && out.Blocked == "" {
		fmt.Fprintf(stdout, "no change; still at %s\n", out.Phase)
	}
	return 0
}

func cmdEpisodeArtifact(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool, accept bool) int {
	fs := flagSet("episode artifact", stderr)
	id := fs.String("episode-id", "", "admitted episode identifier")
	digest := fs.String("digest", "", "retained artifact digest")
	imported := fs.Bool("imported-plan", false, "read the exact original imported plan instead of a generated artifact")
	authFile := fs.String("authorization-file", "", "operator authorization JSON file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *imported {
		if accept || *id == "" || *digest != "" || *authFile != "" {
			return fail(stdout, stderr, asJSON, fmt.Errorf("--imported-plan requires --episode-id and cannot accept a plan or be combined with --digest or --authorization-file"))
		}
		state, err := st.State(*id)
		if err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		if state.Episode == nil || state.Episode.ImportedPlan == nil {
			return fail(stdout, stderr, asJSON, fmt.Errorf("episode has no imported plan"))
		}
		if asJSON {
			return emit(stdout, stderr, state.Episode.ImportedPlan)
		}
		if _, err := io.WriteString(stdout, state.Episode.ImportedPlan.Content); err != nil {
			return fail(stdout, stderr, asJSON, err)
		}
		return 0
	}
	if *id == "" || *digest == "" {
		return fail(stdout, stderr, asJSON, fmt.Errorf("--episode-id and --digest are required"))
	}
	body, err := st.ReadEpisodeArtifact(*id, *digest)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if !accept {
		_, err = stdout.Write(append(body, '\n'))
		if err != nil {
			return 1
		}
		return 0
	}
	authBody, err := readInput(*authFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	var plan contract.ImplementationPlan
	var auth contract.PlanAuthorization
	if err = json.Unmarshal(body, &plan); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if err = json.Unmarshal(authBody, &auth); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if !strings.HasPrefix(auth.AcceptedBy, "operator:") {
		return fail(stdout, stderr, asJSON, fmt.Errorf("interactive acceptance requires an operator: identity"))
	}
	accepted, err := st.AcceptEpisodePlan(*id, auth, &plan)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return emit(stdout, stderr, map[string]any{"accepted": true, "plan_digest": accepted})
}
