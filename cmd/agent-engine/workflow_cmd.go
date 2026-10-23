package main

// The workflow admission verb.
//
// It is the only new caller surface S3 adds, and it is deliberately thin: it
// decodes a v2 request, calls the admission owner, and prints the reference a
// caller needs for the next run. There is no launch, no scheduling and no
// readiness here, because admission is where a graph becomes durable work and
// nothing about it should start a process.
//
// `workflow-run` is NOT implemented at this stage. Launching a graph belongs to
// S5, and a verb that admitted work it could not start would be a caller trap.

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func cmdWorkflow(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	// The subcommand is taken positionally rather than through a flagSet. The
	// top-level parser stops at the first non-flag argument, so "workflow" and its
	// subcommand arrive here verbatim; parsing them into a flagSet before reading
	// the subcommand would leave Arg(0) empty and print usage instead of
	// dispatching.
	if len(args) == 0 {
		flagSet("workflow", stderr).Usage()
		return 2
	}
	switch sub := args[0]; sub {
	case "admit":
		return cmdWorkflowAdmit(st, args[1:], stdout, stderr, asJSON)
	case "ref":
		return cmdWorkflowRef(st, args[1:], stdout, stderr, asJSON)
	default:
		fmt.Fprintf(stderr, "unknown workflow subcommand %q; expected admit or ref\n", sub)
		return 2
	}
}

func cmdWorkflowAdmit(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("workflow admit", stderr)
	file := fs.String("request-file", "", "path to a v2 workflow request JSON file, or - for stdin")
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
	if len(data) > control.MaxWorkflowAdmissionBytes {
		return fail(stdout, stderr, asJSON, fmt.Errorf("agent-engine: the workflow request is %d bytes, over the %d byte limit",
			len(data), control.MaxWorkflowAdmissionBytes))
	}
	req, err := contract.DecodeWorkflowRequest(data)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}

	adm, err := st.AdmitWorkflow(req)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, map[string]any{
			"request_id":        adm.RequestID,
			"workflow_ref":      adm.Ref,
			"definition_sha256": adm.DefinitionSHA256,
			"request_sha256":    adm.RequestSHA256,
			"input_sha256":      adm.InputDigest,
			"children":          adm.Children,
			"child_count":       len(adm.Children),
			"duplicate":         adm.Duplicate,
			"launched":          false,
		})
	}
	if adm.Duplicate {
		fmt.Fprintf(stdout, "%s was already admitted; nothing new was created\n", adm.RequestID)
	} else {
		fmt.Fprintf(stdout, "admitted %s\n", adm.RequestID)
	}
	fmt.Fprintf(stdout, "  definition: %s\n", adm.DefinitionSHA256)
	fmt.Fprintf(stdout, "  request:    %s\n", adm.RequestSHA256)
	fmt.Fprintf(stdout, "  input:      %s\n", adm.InputDigest)
	fmt.Fprintf(stdout, "  children:   %d\n", len(adm.Children))
	for _, id := range sortedKeys(adm.Children) {
		fmt.Fprintf(stdout, "    %-24s %s\n", id, adm.Children[id])
	}
	fmt.Fprintln(stdout, "  keep the definition digest to reuse this graph with a new input")
	return 0
}

func cmdWorkflowRef(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("workflow ref", stderr)
	digest := fs.String("sha256", "", "the definition digest to describe")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*digest) == "" {
		return fail(stdout, stderr, asJSON, errors.New("--sha256 is required"))
	}
	ref := contract.WorkflowRef{Version: contract.WorkflowVersion, SHA256: *digest}
	if err := contract.ValidateWorkflowRef(ref); err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	if asJSON {
		return emit(stdout, stderr, ref)
	}
	fmt.Fprintf(stdout, "workflow_ref = { version = %d, sha256 = %q }\n", ref.Version, ref.SHA256)
	return 0
}

// sortedKeys gives deterministic output for the child map.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
