package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/minimaxacp"
)

// Inspection is read-only: it never starts, attaches to or replaces a worker.
func cmdChats(st *control.Store, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet("chats", stderr)
	id := fs.String("request", "", "request whose retained role chats should be shown")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := st.State(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	dir, err := st.RequestDir(*id)
	if err != nil {
		return fail(stdout, stderr, asJSON, err)
	}
	type chat struct {
		Role          control.ProfileRole `json:"role"`
		Backend       string              `json:"backend"`
		Session       string              `json:"session"`
		SelectedModel string              `json:"selected_model,omitempty"`
		Transcript    string              `json:"transcript,omitempty"`
		InspectArgv   []string            `json:"inspect_argv,omitempty"`
	}
	var chats []chat
	for role, b := range s.BackendBindings {
		c := chat{Role: role, Backend: b.Backend, Session: b.SessionID, SelectedModel: s.ExecutionProfiles[role].Model}
		if strings.HasPrefix(b.Backend, minimaxacp.BackendName) {
			c.Transcript = filepath.Join(dir, "minimax-acp")
			if b.Backend != minimaxacp.BackendName {
				c.Transcript = filepath.Join(c.Transcript, runtimeDigest(b.Backend))
			}
			c.Transcript = filepath.Join(c.Transcript, "receipts.ndjson")
			if launcher, err := acpLauncherPath(); err == nil {
				c.InspectArgv = []string{launcher, "--session", b.SessionID}
			}
		}
		chats = append(chats, c)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i].Role < chats[j].Role })
	if asJSON {
		return encode(stdout, map[string]any{"request_id": *id, "chats": chats, "inspection_note": "Selected models are configuration, not answer attribution. Read retained receipts at any time. Open a native chat only after supervision stops; opening it can start an interactive process."})
	}
	for _, c := range chats {
		fmt.Fprintf(stdout, "%s: selected=%s backend=%s session=%s\n", c.Role, c.SelectedModel, c.Backend, c.Session)
		if c.Transcript != "" {
			fmt.Fprintf(stdout, "  Retained transcript: %s\n", c.Transcript)
		}
		if len(c.InspectArgv) > 0 {
			for i, a := range c.InspectArgv {
				c.InspectArgv[i] = qualifyWord(a)
			}
			fmt.Fprintf(stdout, "  After supervision stops: %s\n", strings.Join(c.InspectArgv, " "))
		}
	}
	return 0
}

func encode(w io.Writer, value any) int {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return 1
	}
	return 0
}
