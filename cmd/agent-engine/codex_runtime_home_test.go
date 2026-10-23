package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

func TestCodexRuntimeHome_PinnedHomeMustMatchTranscriptReader(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	ref := control.Profile{Name: "pinned-reviewer", Tool: "codex", LaunchEnv: map[string]string{"CODEX_HOME": home}}.Ref()
	t.Setenv("CODEX_HOME", other)
	if err := requireCodexRuntimeHome([]control.ProfileRef{ref}); err == nil || !strings.Contains(err.Error(), "set CODEX_HOME=") {
		t.Fatalf("mismatched home was not actionable: %v", err)
	}
	t.Setenv("CODEX_HOME", home)
	if err := requireCodexRuntimeHome([]control.ProfileRef{ref}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", alias)
	if err := requireCodexRuntimeHome([]control.ProfileRef{ref}); err != nil {
		t.Fatalf("same canonical home refused: %v", err)
	}
	t.Setenv("CODEX_HOME", other)
	if err := requireCodexRuntimeHome([]control.ProfileRef{{Name: "inherited", Tool: "codex"}}); err != nil {
		t.Fatalf("inherited home incorrectly treated as an override: %v", err)
	}
	if err := requireCodexRuntimeHome([]control.ProfileRef{{Tool: "mcode", Transport: control.TransportMiniMaxACP}}); err != nil {
		t.Fatalf("non-Codex transport acquired a Codex requirement: %v", err)
	}
}

func TestRun_CodexHomeMismatchRefusesBeforeAnySpawn(t *testing.T) {
	root, source := newCLIFixture(t)
	project := filepath.Join(source, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	file := writeRequest(t, t.TempDir(), repairRequest(source))
	var out, diagnostic bytes.Buffer
	if code := run([]string{"--store", root, "submit", "--request-file", file}, &out, &diagnostic); code != 0 {
		t.Fatal(diagnostic.String())
	}
	st := storeFor(t, root)
	ref := control.Profile{Name: "frozen-reviewer", Role: control.RoleReviewer, Tool: "codex", LaunchEnv: map[string]string{"CODEX_HOME": t.TempDir()}}.Ref()
	before, err := st.UpdateCommitted("forge-run-1842-repair", func(s *control.State) error {
		s.ExecutionProfiles = map[control.ProfileRole]control.ProfileRef{control.RoleReviewer: ref}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	out.Reset()
	diagnostic.Reset()
	if code := cmdRun(st, []string{"--request-id", before.RequestID}, &out, &diagnostic, true); code == 0 || !strings.Contains(out.String()+diagnostic.String(), "frozen-reviewer") {
		t.Fatalf("run did not reject its frozen home before runtime startup: %s %s", out.String(), diagnostic.String())
	}
	after, err := st.State(before.RequestID)
	if err != nil || after.Revision != before.Revision || len(after.OwnedProcesses) != 0 {
		t.Fatalf("refusal mutated admission or spawned: state=%+v err=%v", after, err)
	}
}
