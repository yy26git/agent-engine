package control

// Repair workspaces are prepared before admission. Moving a workspace after
// admission would invalidate its source baseline, grants and read-only receipts.
// Git worktrees isolate ordinary cooperative workers; they are not a sandbox.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type RepairWorkspace struct {
	Workspace  string `json:"workspace"`
	Branch     string `json:"branch"`
	BaseHead   string `json:"base_head"`
	Repository string `json:"repository"`
}

func RepairBranchName(prefix, episodeID string) string {
	var label strings.Builder
	for _, c := range strings.ToLower(episodeID) {
		if label.Len() >= 48 {
			break
		}
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			label.WriteRune(c)
		} else {
			label.WriteByte('-')
		}
	}
	return prefix + strings.Trim(label.String(), "-") + "-" + sha256Hex([]byte(episodeID))[:16]
}

func repairGit(root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	// A caller's inherited Git overrides must not redirect the repository/index.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("agent-engine: git %s: %w: %.1000s", args[0], err, b)
	}
	return strings.TrimSpace(string(b)), nil
}

func repairGitIdentity(workspace string) (branch, head, common string, err error) {
	top, err := repairGit(workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", err
	}
	canonical, err := canonicalPath(workspace)
	if err != nil {
		return "", "", "", err
	}
	top, err = canonicalPath(top)
	if err != nil || top != canonical {
		return "", "", "", fmt.Errorf("agent-engine: repair workspace must be a Git worktree root")
	}
	branch, err = repairGit(workspace, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", "", "", err
	}
	head, err = repairGit(workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", "", fmt.Errorf("agent-engine: repair worktree needs a committed baseline: %w", err)
	}
	common, err = repairGit(workspace, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err == nil {
		common, err = canonicalPath(common)
	}
	return branch, head, common, err
}

// PrepareRepairWorkspace creates a new branch/worktree from an explicitly chosen
// commit. Existing dirty files are never copied, staged, committed or reset.
// It refuses existing branch/path collisions; failed creation remains visible
// for operator recovery rather than deleting possibly valuable work.
func (s *Store) PrepareRepairWorkspace(repository, destination, episodeID, template, base string) (RepairWorkspace, error) {
	var out RepairWorkspace
	if !filepath.IsAbs(repository) || !filepath.IsAbs(destination) || episodeID == "" || base == "" {
		return out, fmt.Errorf("agent-engine: absolute repository/destination, episode ID and explicit base are required")
	}
	repository, err := canonicalPath(repository)
	if err != nil {
		return out, err
	}
	// Resolve the existing parent too, so a symlink cannot redirect creation out
	// of the operator's approved source roots.
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return out, fmt.Errorf("agent-engine: repair worktree parent must already exist: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	sourceOK, destinationOK := false, false
	for _, root := range s.cfg.SourceRoots {
		canonical, err := canonicalPath(root)
		if err != nil {
			return out, err
		}
		sourceOK = sourceOK || underRoot(repository, canonical)
		destinationOK = destinationOK || underRoot(destination, canonical)
	}
	if !sourceOK || !destinationOK {
		return out, fmt.Errorf("agent-engine: repository and repair worktree must be inside locally approved source roots")
	}
	if underRoot(repository, destination) || underRoot(destination, repository) {
		return out, fmt.Errorf("agent-engine: repair worktree must be separate from the original checkout")
	}
	tpl, ok := s.cfg.EpisodeTemplates.Lookup(template)
	if !ok || tpl.ReadOnly || tpl.RepairBranchPrefix == "" {
		return out, fmt.Errorf("agent-engine: template must approve dedicated repair branches")
	}
	branch := RepairBranchName(tpl.RepairBranchPrefix, episodeID)
	if _, err := repairGit(repository, "check-ref-format", "--branch", branch); err != nil {
		return out, err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return out, fmt.Errorf("agent-engine: repair destination already exists or cannot be inspected; nothing was replaced")
	}
	head, err := repairGit(repository, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return out, err
	}
	if _, err := repairGit(repository, "worktree", "add", "--no-track", "-b", branch, destination, head); err != nil {
		return out, err
	}
	got, actualHead, _, err := repairGitIdentity(destination)
	if err != nil || got != branch || actualHead != head {
		return out, fmt.Errorf("agent-engine: created worktree identity could not be established: %v", err)
	}
	return RepairWorkspace{Workspace: destination, Branch: branch, BaseHead: head, Repository: repository}, nil
}

// VerifyRepairBranch checks the admission-time binding. Legacy/read-only states
// without a branch binding keep their existing behavior.
func (s *State) VerifyRepairBranch() error {
	if s.RepairBundleSHA256 != "" && len(s.RepairBundleTargets) < 2 {
		return fmt.Errorf("agent-engine: incomplete admitted repair bundle identity")
	}
	if len(s.RepairBundleTargets) > 0 {
		if s.RepairBundleSHA256 == "" || len(s.RepairBundleTargets) < 2 {
			return fmt.Errorf("agent-engine: incomplete admitted repair bundle identity")
		}
		for _, target := range s.RepairBundleTargets {
			if !repairTargetID.MatchString(target.ID) || filepath.Clean(target.Workspace) != filepath.Join(filepath.Clean(s.Workspace), target.ID) {
				return fmt.Errorf("agent-engine: repair bundle target %q escaped its admitted namespace", target.ID)
			}
			info, err := os.Lstat(target.Workspace)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(target.Workspace)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || resolved != target.Workspace {
				return fmt.Errorf("agent-engine: repair bundle target %q was replaced or redirected", target.ID)
			}
			branch, _, common, err := repairGitIdentity(target.Workspace)
			if err != nil {
				return err
			}
			if branch != target.Branch || common != target.CommonDir {
				return fmt.Errorf("agent-engine: repair bundle target %q changed branch or Git common directory", target.ID)
			}
			if _, err := repairGit(target.Workspace, "merge-base", "--is-ancestor", target.BaseHead, "HEAD"); err != nil {
				return fmt.Errorf("agent-engine: repair bundle target %q no longer descends from its admitted baseline: %w", target.ID, err)
			}
		}
		return nil
	}
	if s.RepairBranch == "" {
		return nil
	}
	branch, _, common, err := repairGitIdentity(s.Workspace)
	if err != nil {
		return err
	}
	if branch != s.RepairBranch || common != s.RepairGitCommonDir {
		return fmt.Errorf("agent-engine: repair workspace changed branch/repository; expected %q", s.RepairBranch)
	}
	if _, err := repairGit(s.Workspace, "merge-base", "--is-ancestor", s.RepairBaseHead, "HEAD"); err != nil {
		return fmt.Errorf("agent-engine: repair branch no longer descends from the admitted baseline: %w", err)
	}
	return nil
}
