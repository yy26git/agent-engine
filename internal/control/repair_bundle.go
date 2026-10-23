package control

// A repair bundle is a set of clean, committed Git worktrees prepared together
// for one episode. The original checkouts are observations only: this code never
// copies, stages, resets, or writes their files.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const repairBundleSchema = 1

var repairTargetID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// RepairBundleRequest is operator-side preparation input. It is not accepted as
// authority by episode admission; admission must resolve the locally retained
// receipt and compare every field against it.
type RepairBundleRequest struct {
	EpisodeID   string               `json:"episode_id"`
	Template    string               `json:"template"`
	Destination string               `json:"destination"`
	Targets     []RepairBundleTarget `json:"targets"`
}

// RepairBundleTarget identifies one independently based repository checkout.
type RepairBundleTarget struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Base       string `json:"base"`
}

// RepairBundleTargetReceipt records the measured Git identity for one target.
type RepairBundleTargetReceipt struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Workspace  string `json:"workspace"`
	Branch     string `json:"branch"`
	BaseHead   string `json:"base_head"`
	CommonDir  string `json:"git_common_dir"`
}

// RepairBundleReceipt is durable, locally retained preparation evidence. A
// PREPARING receipt is intentionally useful: retries may resume only the same
// exact request and verify already-created worktrees before continuing.
type RepairBundleReceipt struct {
	Schema      int                         `json:"schema"`
	EpisodeID   string                      `json:"episode_id"`
	Template    string                      `json:"template"`
	Destination string                      `json:"destination"`
	RequestSHA  string                      `json:"request_sha256"`
	Status      string                      `json:"status"`
	Targets     []RepairBundleTargetReceipt `json:"targets"`
	ReceiptSHA  string                      `json:"receipt_sha256,omitempty"`
}

// PrepareRepairBundle creates (or idempotently resumes) a bundle. The receipt
// intent is fsynced before any worktree is created, closing the unnameable
// partial-creation window. An unrelated existing destination is never adopted.
func (s *Store) PrepareRepairBundle(req RepairBundleRequest) (RepairBundleReceipt, error) {
	var empty RepairBundleReceipt
	if strings.TrimSpace(req.EpisodeID) == "" || strings.TrimSpace(req.Template) == "" || !filepath.IsAbs(req.Destination) || len(req.Targets) < 2 {
		return empty, fmt.Errorf("agent-engine: bundle requires episode ID, approved template, absolute destination and at least two targets")
	}
	tpl, ok := s.cfg.EpisodeTemplates.Lookup(req.Template)
	if !ok || tpl.ReadOnly || tpl.RepairBranchPrefix == "" {
		return empty, fmt.Errorf("agent-engine: template must approve dedicated repair branches")
	}
	destination, err := canonicalProspectivePath(req.Destination)
	if err != nil {
		return empty, err
	}
	if err := s.requireSourceRoot(destination); err != nil {
		return empty, err
	}
	req.Destination = destination
	req.Targets = append([]RepairBundleTarget(nil), req.Targets...)
	sort.Slice(req.Targets, func(i, j int) bool { return req.Targets[i].ID < req.Targets[j].ID })
	seen := map[string]bool{}
	seenRepositories := map[string]bool{}
	seenCommonDirs := map[string]bool{}
	for i := range req.Targets {
		t := &req.Targets[i]
		if !repairTargetID.MatchString(t.ID) || seen[t.ID] || !filepath.IsAbs(t.Repository) || strings.TrimSpace(t.Base) == "" {
			return empty, fmt.Errorf("agent-engine: each bundle target needs a unique safe ID, absolute repository and explicit base")
		}
		seen[t.ID] = true
		t.Repository, err = canonicalPath(t.Repository)
		if err != nil {
			return empty, err
		}
		if err := s.requireSourceRoot(t.Repository); err != nil {
			return empty, err
		}
		_, _, sourceCommon, err := repairGitIdentity(t.Repository)
		if err != nil {
			return empty, err
		}
		if seenRepositories[t.Repository] || seenCommonDirs[sourceCommon] {
			return empty, fmt.Errorf("agent-engine: each repair bundle target must identify a distinct Git repository")
		}
		seenRepositories[t.Repository] = true
		seenCommonDirs[sourceCommon] = true
		// Freeze symbolic refs before recording intent. A branch moving between a
		// crash and retry must not silently change the selected source revision.
		t.Base, err = repairGit(t.Repository, "rev-parse", "--verify", "--end-of-options", t.Base+"^{commit}")
		if err != nil {
			return empty, err
		}
		if underRoot(t.Repository, destination) || underRoot(destination, t.Repository) {
			return empty, fmt.Errorf("agent-engine: bundle and source repository must be separate")
		}
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return empty, err
	}
	requestSum := sha256.Sum256(encoded)
	requestDigest := hex.EncodeToString(requestSum[:])
	receiptDir := filepath.Join(s.cfg.StoreRoot, "REPAIR_BUNDLES")
	keySum := sha256.Sum256([]byte(req.EpisodeID))
	receiptPath := filepath.Join(receiptDir, hex.EncodeToString(keySum[:])+".json")

	unlock, err := s.storeLock()
	if err != nil {
		return empty, err
	}
	defer unlock()
	if err := os.MkdirAll(receiptDir, 0o700); err != nil {
		return empty, err
	}
	var receipt RepairBundleReceipt
	if raw, readErr := os.ReadFile(receiptPath); readErr == nil {
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return empty, fmt.Errorf("agent-engine: repair bundle receipt is corrupt: %w", err)
		}
		if receipt.Schema != repairBundleSchema || receipt.EpisodeID != req.EpisodeID || receipt.Template != req.Template || receipt.RequestSHA != requestDigest {
			return empty, fmt.Errorf("agent-engine: episode already has a different repair bundle preparation")
		}
		if receipt.Status == "READY" && !validRepairBundleReceipt(receipt) {
			return empty, fmt.Errorf("agent-engine: repair bundle receipt digest is invalid")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return empty, readErr
	} else {
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			return empty, fmt.Errorf("agent-engine: bundle destination exists without a matching local receipt; refusing adoption")
		}
		receipt = RepairBundleReceipt{Schema: repairBundleSchema, EpisodeID: req.EpisodeID, Template: req.Template, Destination: destination, RequestSHA: requestDigest, Status: "PREPARING", Targets: make([]RepairBundleTargetReceipt, 0, len(req.Targets))}
		for _, target := range req.Targets {
			receipt.Targets = append(receipt.Targets, RepairBundleTargetReceipt{ID: target.ID, Repository: target.Repository, Workspace: filepath.Join(destination, target.ID)})
		}
		if err := writeRepairBundleReceipt(receiptPath, receipt); err != nil {
			return empty, err
		}
	}
	if receipt.Destination != destination || len(receipt.Targets) != len(req.Targets) {
		return empty, fmt.Errorf("agent-engine: retained bundle receipt does not match the requested target mapping")
	}
	if info, statErr := os.Lstat(destination); statErr == nil {
		resolved, resolveErr := filepath.EvalSymlinks(destination)
		if resolveErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || resolved != destination {
			return empty, fmt.Errorf("agent-engine: bundle destination was replaced or redirected")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return empty, statErr
	} else if receipt.Status == "READY" {
		return empty, fmt.Errorf("agent-engine: completed bundle destination is missing")
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return empty, err
	}
	for i, target := range req.Targets {
		r := &receipt.Targets[i]
		if r.ID != target.ID || r.Repository != target.Repository || r.Workspace != filepath.Join(destination, target.ID) {
			return empty, fmt.Errorf("agent-engine: retained bundle target mapping conflicts with request")
		}
		head := target.Base
		branch := RepairBranchName(tpl.RepairBranchPrefix, req.EpisodeID+"-"+target.ID)
		if r.Branch == "" {
			if _, err := repairGit(target.Repository, "check-ref-format", "--branch", branch); err != nil {
				return empty, err
			}
			if _, statErr := os.Lstat(r.Workspace); statErr == nil {
				// A crash may have occurred after `git worktree add` succeeded but
				// before its measured identity was journaled. Recover only an exact
				// expected worktree; an unrelated path is never adopted.
				info, infoErr := os.Lstat(r.Workspace)
				resolved, resolveErr := filepath.EvalSymlinks(r.Workspace)
				if infoErr != nil || resolveErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || resolved != r.Workspace {
					return empty, fmt.Errorf("agent-engine: interrupted bundle path was replaced or redirected")
				}
				gotBranch, gotHead, gotCommon, identityErr := repairGitIdentity(r.Workspace)
				_, _, sourceCommon, sourceErr := repairGitIdentity(target.Repository)
				if identityErr != nil || sourceErr != nil || gotBranch != branch || gotHead != head || gotCommon != sourceCommon {
					return empty, fmt.Errorf("agent-engine: interrupted bundle path exists with an unexpected Git identity")
				}
				r.Branch, r.BaseHead, r.CommonDir = gotBranch, gotHead, gotCommon
				if err := writeRepairBundleReceipt(receiptPath, receipt); err != nil {
					return empty, err
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return empty, statErr
			} else {
				if _, err := repairGit(target.Repository, "worktree", "add", "--no-track", "-b", branch, r.Workspace, head); err != nil {
					return empty, err
				}
				var gotCommon string
				r.Branch, r.BaseHead, gotCommon, err = repairGitIdentity(r.Workspace)
				if err != nil {
					return empty, err
				}
				_, _, sourceCommon, sourceErr := repairGitIdentity(target.Repository)
				if sourceErr != nil {
					return empty, sourceErr
				}
				if r.Branch != branch || r.BaseHead != head || gotCommon != sourceCommon {
					return empty, fmt.Errorf("agent-engine: created worktree identity differs from explicit target base")
				}
				r.CommonDir = gotCommon
				if err := writeRepairBundleReceipt(receiptPath, receipt); err != nil {
					return empty, err
				}
			}
		} else if err := verifyBundleTarget(*r, head); err != nil {
			return empty, err
		}
	}
	receipt.Status = "READY"
	receipt.ReceiptSHA = ""
	unsigned, err := json.Marshal(receipt)
	if err != nil {
		return empty, err
	}
	sum := sha256.Sum256(unsigned)
	receipt.ReceiptSHA = hex.EncodeToString(sum[:])
	if err := writeRepairBundleReceipt(receiptPath, receipt); err != nil {
		return empty, err
	}
	return receipt, nil
}

func (s *Store) requireSourceRoot(path string) error {
	for _, root := range s.cfg.SourceRoots {
		canonical, err := canonicalPath(root)
		if err != nil {
			return err
		}
		if underRoot(path, canonical) {
			return nil
		}
	}
	return fmt.Errorf("agent-engine: repository and bundle must be inside locally approved source roots")
}

func canonicalProspectivePath(path string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("agent-engine: destination parent must exist and resolve: %w", err)
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func verifyBundleTarget(r RepairBundleTargetReceipt, expectedHead string) error {
	info, err := os.Lstat(r.Workspace)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(r.Workspace)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || resolved != r.Workspace {
		return fmt.Errorf("agent-engine: repair bundle target %q was replaced or redirected", r.ID)
	}
	branch, head, common, err := repairGitIdentity(r.Workspace)
	if err != nil {
		return err
	}
	if branch != r.Branch || common != r.CommonDir || head != expectedHead || r.BaseHead != expectedHead {
		return fmt.Errorf("agent-engine: repair bundle target %q changed branch, base or Git common directory", r.ID)
	}
	return nil
}

func writeRepairBundleReceipt(path string, r RepairBundleReceipt) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repair-bundle-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validRepairBundleReceipt(r RepairBundleReceipt) bool {
	want := r.ReceiptSHA
	r.ReceiptSHA = ""
	b, err := json.Marshal(r)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(b)
	return want != "" && want == hex.EncodeToString(sum[:])
}

// loadRepairBundleForAdmission resolves an exact local receipt and measures
// every worktree before returning its bindings. The caller rechecks under the
// store lock immediately before it commits admission.
func (s *Store) loadRepairBundleForAdmission(episodeID, template, workspace, digest string) (RepairBundleReceipt, error) {
	var receipt RepairBundleReceipt
	keySum := sha256.Sum256([]byte(episodeID))
	path := filepath.Join(s.cfg.StoreRoot, "REPAIR_BUNDLES", hex.EncodeToString(keySum[:])+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return receipt, fmt.Errorf("agent-engine: no locally retained repair bundle receipt: %w", err)
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return receipt, fmt.Errorf("agent-engine: repair bundle receipt is corrupt: %w", err)
	}
	if receipt.Schema != repairBundleSchema || receipt.Status != "READY" || !validRepairBundleReceipt(receipt) || receipt.ReceiptSHA != digest || receipt.EpisodeID != episodeID || receipt.Template != template || receipt.Destination != workspace || len(receipt.Targets) < 2 {
		return receipt, fmt.Errorf("agent-engine: repair bundle receipt does not match the admitted episode, template, workspace and digest")
	}
	for _, target := range receipt.Targets {
		if err := verifyBundleTarget(target, target.BaseHead); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}
