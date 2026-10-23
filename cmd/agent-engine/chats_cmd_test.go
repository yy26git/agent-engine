package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

func TestChats_RetainedInspectionDoesNotMutateState(t *testing.T) {
	root, source := newCLIFixture(t)
	path := writeRequest(t, t.TempDir(), repairRequest(source))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--store", root, "submit", "--request-file", path}, &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	st := storeFor(t, root)
	const id = "forge-run-1842-repair"
	if _, err := st.UpdateCommitted(id, func(s *control.State) error {
		s.BackendBindings = map[control.ProfileRole]control.SessionBinding{control.RoleWorker: {Backend: "minimax-acp", SessionID: "retained-chat"}}
		s.ExecutionProfiles = map[control.ProfileRole]control.ProfileRef{control.RoleWorker: {Model: "m:minimax:MiniMax-M2.7:v:thinking"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := st.State(id)
	stdout.Reset()
	stderr.Reset()
	if code := cmdChats(st, []string{"--request", id}, &stdout, &stderr, true); code != 0 {
		t.Fatal(stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "retained-chat") || !strings.Contains(stdout.String(), "receipts.ndjson") || !strings.Contains(stdout.String(), "M2.7") {
		t.Fatal(stdout.String())
	}
	after, _ := st.State(id)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatal("chat inspection changed durable state")
	}
}
