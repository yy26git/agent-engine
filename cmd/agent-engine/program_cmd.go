package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

// cmdProgram exposes only locally pinned single-card admission. Multi-card
// scheduling stays with the future durable coordinator, which can enforce
// dependency joins, shared-resource claims and program-wide budgets.
func cmdProgram(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	if len(args) == 0 {
		return fail(stdout, stderr, asJSON, errors.New("program requires a subcommand: digest, admit-card"))
	}
	switch args[0] {
	case "digest":
		return cmdProgramDigest(args[1:], stdout, stderr, asJSON)
	case "admit-card":
		return cmdProgramAdmitCard(st, args[1:], stdout, stderr, asJSON)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "Usage: agent-engine --store /absolute/store program <digest|admit-card> [flags]")
		return 0
	default:
		return fail(stdout, stderr, asJSON, fmt.Errorf("unknown program subcommand %q", args[0]))
	}
}

func cmdProgramDigest(args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("program digest", stderr)
	file := fs.String("definition-file", "", "path to a v1 program definition JSON file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--definition-file is required"))
	}
	data, err := readInput(*file)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	program, err := contract.DecodeProgramDefinition(data)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	digest, err := program.ProgramDigest()
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return emit(stdout, stderr, map[string]any{"program_id": program.ProgramID, "revision": program.Revision, "definition_sha256": digest})
}

func cmdProgramAdmitCard(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("program admit-card", stderr)
	definitionFile := fs.String("definition-file", "", "locally authorized v1 program definition JSON file")
	cardID := fs.String("card-id", "", "single authorized root card to admit")
	requestFile := fs.String("request-file", "", "path to a v1 episode request JSON file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*definitionFile) == "" || strings.TrimSpace(*cardID) == "" || strings.TrimSpace(*requestFile) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--definition-file, --card-id and --request-file are required"))
	}
	if *definitionFile == "-" && *requestFile == "-" {
		return fail(stdout, stderr, asJSON, errors.New("program and episode cannot both be read from stdin"))
	}
	programBytes, err := readInput(*definitionFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	program, err := contract.DecodeProgramDefinition(programBytes)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if len(program.Cards) != 1 {
		return fail(stdout, stderr, asJSON, errors.New("single-card admission refuses multi-card programs; use the durable program coordinator"))
	}
	card := program.Cards[0]
	if card.CardID != *cardID || len(card.DependsOn) != 0 || len(card.Resources) != 0 {
		return fail(stdout, stderr, asJSON, errors.New("single-card admission requires the sole card to be a dependency-free root with no coordinator-owned resource claims"))
	}
	episodeBytes, err := readInput(*requestFile)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	episode, err := contract.DecodeEpisodeRequest(episodeBytes)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	admission, err := st.AdmitProgramEpisode(program, *cardID, episode)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	programDigest, err := program.ProgramDigest()
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	cardDigest, err := card.CardDigest()
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return emit(stdout, stderr, map[string]any{
		"program_id": program.ProgramID, "program_sha256": programDigest,
		"card_id": card.CardID, "card_sha256": cardDigest,
		"episode_id": episode.EpisodeID, "task_id": admission.TaskID,
		"request_sha256": admission.RequestDigest, "phase": control.PhaseReport,
		"store": st.Config().StoreRoot,
	})
}
