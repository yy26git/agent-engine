package agentdeck

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreate_SelectionReachesPublicCLI(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	cli := filepath.Join(dir, "agent-deck")
	// A non-billable process boundary test, not an in-memory CreateSpec test.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + log + "'\nprintf '%s\\n' '{\"id\":\"selection-session\"}'\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	spec := CreateSpec{Path: dir, Title: "review", Tool: "codex", Model: "selected-model", Effort: "high", Sandbox: true, WorktreeBranch: "codex/test-selection", NewBranch: true, NoParent: true, Wrapper: "env CODEX_HOME='/isolated/codex' {command}"}
	rt := NewCLIRuntime(cli, "test")
	if _, err := rt.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	want := []string{"-p", "test", "add", dir, "--json", "--title", "review", "-c", "codex", "--no-parent", "--model", "selected-model", "--effort", "high", "--sandbox", "--worktree", "codex/test-selection", "--new-branch", "--wrapper", "env CODEX_HOME='/isolated/codex' {command}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CLI selection changed: got %q want %q", got, want)
	}
}
