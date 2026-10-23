package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
)

func TestImportedPlanCLI_PrepareEmbedsExactDocument(t *testing.T) {
	store, source := flowCLIFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "implementation.md")
	content := "# Existing plan\r\nFix addition; keep café text and tests unchanged.\r\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	requestFile := filepath.Join(dir, "episode.json")
	args := []string{"--store", store, "--json", "episode", "prepare", "--episode-id", "imported-001", "--template", "feature", "--workspace", source, "--objective", "Follow the existing plan", "--caller-id", "codex-local", "--criterion", "Addition works", "--allow-code-changes", "--allowed-path", "src/**", "--plan-file", path, "--output", requestFile}
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("prepare imported plan: %s %s", out.String(), errOut.String())
	}
	body, err := os.ReadFile(requestFile)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := contract.DecodeEpisodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	if ep.ImportedPlan == nil || ep.ImportedPlan.Content != content || ep.ImportedPlan.SHA256 != want || ep.ImportedPlan.Name != "implementation.md" {
		t.Fatalf("source plan was not preserved: %+v", ep.ImportedPlan)
	}
	// The request is self-contained. Later file edits do not change what was
	// supplied, and preparation never starts providers or creates an admission.
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if ep.ImportedPlan.Content != content {
		t.Fatal("source changed with external file")
	}
	if _, err := os.Stat(filepath.Join(store, "REQUESTS", ep.EpisodeID)); !os.IsNotExist(err) {
		t.Fatalf("prepare admitted work: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "episode", "admit", "--request-file", requestFile}, &out, &errOut) != 0 {
		t.Fatalf("imported plan admission failed: %s %s", out.String(), errOut.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "episode", "artifact", "--episode-id", ep.EpisodeID, "--imported-plan"}, &out, &errOut) != 0 || out.String() != content {
		t.Fatalf("original plan could not be inspected byte-for-byte after deleting source: %s %s", out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "episode", "status", "--episode-id", ep.EpisodeID, "--json"}, &out, &errOut) != 0 || !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "review_imported_plan") {
		t.Fatalf("status lost imported-plan identity: %s %s", out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if run([]string{"--json", "capabilities"}, &out, &errOut) != 0 || !strings.Contains(out.String(), "imported-implementation-plan") {
		t.Fatalf("import support not discoverable: %s %s", out.String(), errOut.String())
	}
}

func TestImportedPlanCLI_RefusesInvalidSourceWithoutWritingRequest(t *testing.T) {
	store, source := flowCLIFixture(t)
	for _, content := range []string{" ", "plan\x00", string([]byte{0xff}), strings.Repeat("x", contract.MaxImportedPlanBytes+1)} {
		dir := t.TempDir()
		path, output := filepath.Join(dir, "plan.md"), filepath.Join(dir, "episode.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		args := []string{"--store", store, "--json", "episode", "prepare", "--episode-id", "invalid-import", "--template", "feature", "--workspace", source, "--objective", "Fix", "--caller-id", "codex-local", "--criterion", "Regression passes", "--plan-file", path, "--output", output}
		if run(args, &out, &errOut) == 0 {
			t.Fatal("invalid plan accepted")
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("invalid import wrote a request: %v", err)
		}
	}
}
