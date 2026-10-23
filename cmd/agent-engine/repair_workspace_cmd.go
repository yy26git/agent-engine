package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emjin/agent-engine/internal/control"
	"io"
	"os"
)

func cmdPrepareRepair(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("prepare-repair", stderr)
	repo := fs.String("repository", "", "absolute original checkout (never modified)")
	dest := fs.String("destination", "", "absolute new worktree in approved source roots")
	id := fs.String("episode-id", "", "episode identity for the dedicated branch")
	template := fs.String("template", "", "approved template with repair_branch_prefix")
	base := fs.String("base", "", "explicit commit/ref to repair; dirty source files are not copied")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	out, err := st.PrepareRepairWorkspace(*repo, *dest, *id, *template, *base)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return emit(stdout, stderr, out)
}

func cmdPrepareRepairBundle(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("prepare-repair-bundle", stderr)
	file := fs.String("request-file", "", "operator-authored JSON describing episode ID, template, destination and repository targets")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		return fail(stdout, stderr, asJSON, errors.New("--request-file is required"))
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req control.RepairBundleRequest
	if err := dec.Decode(&req); err != nil {
		return fail(stdout, stderr, asJSON, fmt.Errorf("decode repair bundle request: %w", err))
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fail(stdout, stderr, asJSON, errors.New("repair bundle request has trailing JSON"))
	}
	out, err := st.PrepareRepairBundle(req)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	return emit(stdout, stderr, out)
}
