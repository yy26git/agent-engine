package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEpisodeExecution_BaselinePrecedesWorkerAndSurvivesRepair(t *testing.T) {
	f := execFixture(t, execTemplate(allEpisodeRoles(), []string{unrunnableCheckID}, nil))
	id := "EP-BASELINE"
	admitEpisode(t, f, id)
	rt := newFakeRuntime()
	e, stop := episodeEngine(t, f, rt, id)
	defer stop()
	workspace := filepath.Join(f.srcRoot, "project")
	before, err := TakeSnapshot(workspace, f.now)
	if err != nil {
		t.Fatal(err)
	}
	driveToImplement(t, e, f, rt, id)
	st := mustState(t, f, id)
	if len(sentTo(rt, st.WorkerSession)) != 1 {
		t.Fatal("production phase did not deliver the worker instruction")
	}
	if st.BaselineDigest != before.ContentDigest {
		t.Fatalf("worker dispatched without its pre-work baseline: %q, want %q", st.BaselineDigest, before.ContentDigest)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n\nfunc repaired() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := TakeSnapshot(workspace, f.now)
	if err != nil || changed.ContentDigest == before.ContentDigest {
		t.Fatalf("edit did not change measured content: %v", err)
	}
	// A new repair round uses the same owner path; it must not redefine the
	// first baseline even after the worker changed content.
	if _, err := e.dispatchEpisodeRole(context.Background(), id, PhaseImplement, 2, RoleWorker, "repair"); err != nil {
		t.Fatal(err)
	}
	if got := mustState(t, f, id).BaselineDigest; got != before.ContentDigest {
		t.Fatalf("repair moved baseline: %q", got)
	}
}

func TestCompletion_FirstAttemptEditReportsMeasuredCodeChange(t *testing.T) {
	f := completionFixture(t)
	id := "forge-run-1842-repair"
	review := auditedRequest(t, f)
	if _, err := f.store.RecordBaseline(id, func() time.Time { return f.now }); err != nil {
		t.Fatal(err)
	}
	workspace := dirOf(t, f)
	files, err := filepath.Glob(filepath.Join(workspace, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("fixture contains no source: %v", err)
	}
	source, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], append(source, []byte("\n// first-attempt edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := submitCompletionPass(t, f, review, judgedCompletionBody(t, f)); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.Result(id)
	if err != nil || !result.CodeChanged || result.Workspace == nil || result.Workspace.BaselineSHA256 == result.Workspace.FinalSHA256 {
		t.Fatalf("measured edit not reported: %+v %v", result, err)
	}
	if st := mustState(t, f, id); st.RepairCycles != 0 || st.Generation != 1 {
		t.Fatal("test passed through repair/generation counters rather than content measurement")
	}
}
