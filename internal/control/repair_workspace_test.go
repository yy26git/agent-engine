package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepairFixture(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := repairGit(root, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("committed baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := repairGit(root, "add", "source.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := repairGit(root, "-c", "user.name=Agent Engine test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture baseline"); err != nil {
		t.Fatal(err)
	}
}

func branchFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	canonical, err := canonicalPath(f.srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.SourceRoots = []string{canonical}
	f.cfg.Profiles = episodeProfiles()
	tpl := episodeTemplateWith(allEpisodeRoles())
	tpl.RepairBranchPrefix = "agent-engine/repair/"
	if err := f.cfg.EpisodeTemplates.Register(tpl); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(f.srcRoot, "original")
	gitRepairFixture(t, repo)
	return f, repo
}

func TestRepairWorkspace_IsolatesRepairAndPreservesDirtyOriginal(t *testing.T) {
	f, repo := branchFixture(t)
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("another writer's work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := repairGit(repo, "status", "--porcelain=v1")
	dest := filepath.Join(f.srcRoot, "repair")
	out, err := f.store.PrepareRepairWorkspace(repo, dest, "episode-1", "standard_fix", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "source.txt")); string(b) != "committed baseline\n" {
		t.Fatal("copied dirty source or chose wrong baseline")
	}
	if err := os.WriteFile(filepath.Join(dest, "source.txt"), []byte("worker repair\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "source.txt")); string(b) != "another writer's work\n" {
		t.Fatal("changed original checkout")
	}
	after, _ := repairGit(repo, "status", "--porcelain=v1")
	branch, _ := repairGit(repo, "symbolic-ref", "--short", "HEAD")
	if before != after || branch != "main" || out.Branch != RepairBranchName("agent-engine/repair/", "episode-1") {
		t.Fatal("changed original branch/index or misidentified repair branch")
	}
	if _, err := f.store.PrepareRepairWorkspace(repo, dest, "episode-1", "standard_fix", "HEAD"); err == nil {
		t.Fatal("overwrote existing repair worktree")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "source.txt")); string(b) != "worker repair\n" {
		t.Fatal("lost repair on collision")
	}
}

func TestRepairWorkspace_AdmissionPinsBranchAndRefusesChangedOwner(t *testing.T) {
	f, repo := branchFixture(t)
	ep := episodeRequest(f, "EP-BRANCH")
	ep.Workspace.Path, ep.Workspace.ExpectedHead = repo, ""
	if _, err := f.store.AdmitEpisode(ep); err == nil || !strings.Contains(err.Error(), "dedicated episode branch") {
		t.Fatalf("admitted original branch: %v", err)
	}
	if _, err := f.store.State(ep.EpisodeID); err == nil {
		t.Fatal("branch refusal left admitted state")
	}
	out, err := f.store.PrepareRepairWorkspace(repo, filepath.Join(f.srcRoot, "repair"), ep.EpisodeID, ep.Template, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ep.Workspace.Path = out.Workspace
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	s, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if s.RepairBranch != out.Branch || s.RepairBaseHead != out.BaseHead || s.RepairGitCommonDir == "" {
		t.Fatal("initial state did not freeze branch identity")
	}
	if err := s.VerifyRepairBranch(); err != nil {
		t.Fatal(err)
	}
	if _, err := repairGit(out.Workspace, "switch", "-c", "wrong-branch"); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyRepairBranch(); err == nil {
		t.Fatal("accepted branch switch")
	}
	e, err := NewEngine(f.store, &fakeRuntime{}, ep.EpisodeID, EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.LaunchProfile(t.Context(), ep.EpisodeID, RoleWorker, "impl", out.Workspace, "branch-switch"); err == nil {
		t.Fatal("launched worker after branch switch")
	}
	if _, err := e.executeEpisodeVerification(ep.EpisodeID, fullTemplate(), true, 1); err != nil {
		t.Fatal(err)
	}
	fresh, _ := f.store.State(ep.EpisodeID)
	if fresh.Episode.Execution.Verification != nil {
		t.Fatal("ran verification on wrong branch")
	}
}

func TestRepairWorkspace_RefusesOutOfScopeAndBranchCollision(t *testing.T) {
	f, repo := branchFixture(t)
	if _, err := f.store.PrepareRepairWorkspace(repo, filepath.Join(t.TempDir(), "outside"), "EP", "standard_fix", "HEAD"); err == nil {
		t.Fatal("created out-of-scope worktree")
	}
	if _, err := repairGit(repo, "branch", RepairBranchName("agent-engine/repair/", "EP")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(f.srcRoot, "repair")
	if _, err := f.store.PrepareRepairWorkspace(repo, dest, "EP", "standard_fix", "HEAD"); err == nil {
		t.Fatal("reused preexisting branch")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("branch collision created worktree")
	}
}
