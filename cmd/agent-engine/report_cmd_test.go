package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/control"
)

// Exercises config parsing, the compiled CLI and the native process boundary;
// fixture providers never contact a model service.
func TestReportCLI_AdmitRunRetrieveWithoutAgentDeckOrReviewer(t *testing.T) {
	store, source := newCLIFixture(t)
	deck := filepath.Join(t.TempDir(), "agent-deck")
	marker := deck + ".executed"
	if err := os.WriteFile(deck, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	digest, _ := agentdeck.FileSHA256(deck)
	lock := agentdeck.RuntimeLock{Schema: 1, SourceCommit: strings.Repeat("a", 40), Artifacts: map[string]agentdeck.LockedArtifact{runtime.GOOS + "/" + runtime.GOARCH: {Path: deck, SHA256: digest}}}
	raw, _ := json.Marshal(lock)
	lockFile := filepath.Join(t.TempDir(), "lock.json")
	if err := os.WriteFile(lockFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ENGINE_RUNTIME_LOCK", lockFile)
	cursor := filepath.Join(t.TempDir(), "cursor")
	body := "#!/bin/sh\ncat >/dev/null\nsleep 1\nprintf '%s' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"fixture-chat\",\"result\":\"{\\\"ok\\\":false}\"}'\n"
	if err := os.WriteFile(cursor, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cursorHash, _ := control.DigestFile(cursor)
	cfgPath := filepath.Join(store, "CONTROL.toml")
	config, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config, []byte(`agent_deck_binary = "agent-deck"`), []byte(fmt.Sprintf("agent_deck_binary = %q", deck)), 1)
	config = append(config, []byte(fmt.Sprintf(`
[profiles.reporter]
role = "investigation"
tool = "cursor"
transport = "cursor-cli"
model = "composer-2.5"
read_only = "true"
launch_command = [%q]
launcher_sha256 = %q
[episode_templates.report]
report_only = "true"
read_only = "true"
phases = ["REPORT"]
roles = ["investigation=reporter"]
max_repair_iterations = 1
`, cursor, cursorHash))...)
	if err := os.WriteFile(cfgPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"version": 1, "episode_id": "forge-report-fixture", "template": "report", "origin": map[string]string{"kind": "forge", "caller_id": "forge-fixture"}, "workspace": map[string]string{"path": source}, "objective": "Describe supplied evidence", "obligations": []map[string]string{{"id": "report", "criterion": "Describe the supplied logs"}}, "structured_task": map[string]string{"function": "report-analysis", "schema_name": "test", "schema_json": `{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`, "user": "log:1 mismatch"}}
	file := writeRequest(t, t.TempDir(), request)
	for _, args := range [][]string{{"episode", "validate", "--request-file", file, "--json"}, {"episode", "admit", "--request-file", file, "--json"}, {"run", "--request-id", "forge-report-fixture", "--json"}, {"run", "--request-id", "forge-report-fixture", "--json"}} {
		if r := runCLI(t, store, args...); r.code != 0 {
			t.Fatalf("%v: %s", args, r)
		}
	}
	state := readState(t, store, "forge-report-fixture")
	if state["stage"] != "REPORT_COMPLETE" || state["result_status"] != "COMPLETE" {
		t.Fatal(state)
	}
	st := storeFor(t, store)
	output, err := st.ReportOutput("forge-report-fixture")
	if err != nil || output.OutputJSON != `{"ok":false}` || output.SelectedModel != "composer-2.5" {
		t.Fatal(output, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Agent Deck executed for a native data report")
	}
}
