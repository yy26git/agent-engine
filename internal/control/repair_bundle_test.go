package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestRepairBundle_PreparesDistinctWorktreesAndIsIdempotent(t *testing.T) {
	f, forge := branchFixture(t)
	android := filepath.Join(f.srcRoot, "android-original")
	gitRepairFixture(t, android)
	forgeBefore, err := repairGit(forge, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	androidBefore, err := repairGit(android, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	request := RepairBundleRequest{
		EpisodeID:   "forge-android-case",
		Template:    "standard_fix",
		Destination: filepath.Join(f.srcRoot, "bundle"),
		Targets: []RepairBundleTarget{
			{ID: "forge", Repository: forge, Base: "HEAD"},
			{ID: "android", Repository: android, Base: "HEAD"},
		},
	}
	first, err := f.store.PrepareRepairBundle(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "READY" || !validRepairBundleReceipt(first) || len(first.Targets) != 2 {
		t.Fatalf("invalid prepared receipt: %#v", first)
	}
	second, err := f.store.PrepareRepairBundle(request)
	if err != nil {
		t.Fatal(err)
	}
	if second.ReceiptSHA != first.ReceiptSHA {
		t.Fatal("idempotent preparation changed its receipt")
	}
	for _, target := range first.Targets {
		branch, head, common, err := repairGitIdentity(target.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		if branch != target.Branch || head != target.BaseHead || common != target.CommonDir {
			t.Fatalf("target %s identity mismatch", target.ID)
		}
		if err := os.WriteFile(filepath.Join(target.Workspace, "worker-output.txt"), []byte(target.ID), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(target.Repository, "worker-output.txt")); !os.IsNotExist(err) {
			t.Fatalf("worker output escaped into original %s checkout", target.ID)
		}
	}
	forgeAfter, _ := repairGit(forge, "status", "--porcelain=v1")
	androidAfter, _ := repairGit(android, "status", "--porcelain=v1")
	if forgeBefore != forgeAfter || androidBefore != androidAfter {
		t.Fatal("preparation changed one of the original checkouts")
	}
}

func TestRepairBundle_RefusesConflictingRequestAndUnreceiptedDestination(t *testing.T) {
	f, forge := branchFixture(t)
	android := filepath.Join(f.srcRoot, "android-original")
	gitRepairFixture(t, android)
	request := RepairBundleRequest{EpisodeID: "bundle-conflict", Template: "standard_fix", Destination: filepath.Join(f.srcRoot, "bundle"), Targets: []RepairBundleTarget{{ID: "forge", Repository: forge, Base: "HEAD"}, {ID: "android", Repository: android, Base: "HEAD"}}}
	prepared, err := f.store.PrepareRepairBundle(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Targets[0].ID = "forge-renamed"
	if _, err := f.store.PrepareRepairBundle(request); err == nil {
		t.Fatal("accepted a different request for an existing episode receipt")
	}
	workspace := prepared.Targets[0].Workspace
	saved := workspace + ".saved"
	if err := os.Rename(workspace, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(forge, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PrepareRepairBundle(RepairBundleRequest{EpisodeID: "bundle-conflict", Template: "standard_fix", Destination: filepath.Join(f.srcRoot, "bundle"), Targets: []RepairBundleTarget{{ID: "forge", Repository: forge, Base: "HEAD"}, {ID: "android", Repository: android, Base: "HEAD"}}}); err == nil {
		t.Fatal("accepted a target replaced with a symlink")
	}
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, workspace); err != nil {
		t.Fatal(err)
	}
	other := request
	other.EpisodeID = "unreceipted"
	other.Destination = filepath.Join(f.srcRoot, "already-exists")
	if err := os.Mkdir(other.Destination, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PrepareRepairBundle(other); err == nil {
		t.Fatal("adopted an unreceipted destination")
	}
}

func TestRepairBundle_RequiresDistinctGitRepositories(t *testing.T) {
	f, forge := branchFixture(t)
	_, err := f.store.PrepareRepairBundle(RepairBundleRequest{
		EpisodeID: "bundle-duplicate-repo", Template: "standard_fix", Destination: filepath.Join(f.srcRoot, "bundle-duplicate-repo"),
		Targets: []RepairBundleTarget{{ID: "forge", Repository: forge, Base: "HEAD"}, {ID: "android", Repository: forge, Base: "HEAD"}},
	})
	if err == nil {
		t.Fatal("accepted two target namespaces backed by the same Git repository")
	}
}

func TestRepairBundle_AdmissionPinsEveryTargetAndRechecksBranches(t *testing.T) {
	f, forge := branchFixture(t)
	android := filepath.Join(f.srcRoot, "android-original")
	gitRepairFixture(t, android)
	episodeID := "bundle-admission"
	receipt, err := f.store.PrepareRepairBundle(RepairBundleRequest{
		EpisodeID: episodeID, Template: "standard_fix", Destination: filepath.Join(f.srcRoot, "bundle-admission"),
		Targets: []RepairBundleTarget{{ID: "forge", Repository: forge, Base: "HEAD"}, {ID: "android", Repository: android, Base: "HEAD"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ep := episodeRequest(f, episodeID)
	ep.Workspace.Path, ep.Workspace.ExpectedHead = receipt.Destination, ""
	ep.RepairBundleSHA256 = receipt.ReceiptSHA
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	state, err := f.store.State(episodeID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Workspace != receipt.Destination || state.RepairBundleSHA256 != receipt.ReceiptSHA || len(state.RepairBundleTargets) != 2 {
		t.Fatalf("admission lost receipt bindings: %#v", state)
	}
	if err := state.VerifyRepairBranch(); err != nil {
		t.Fatal(err)
	}
	if _, err := repairGit(receipt.Targets[0].Workspace, "checkout", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := state.VerifyRepairBranch(); err == nil {
		t.Fatal("accepted a target after its dedicated branch changed")
	}
	if _, err := repairGit(receipt.Targets[0].Workspace, "checkout", receipt.Targets[0].Branch); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(receipt.Targets[1].Workspace); err != nil {
		t.Fatal(err)
	}
	if err := state.VerifyRepairBranch(); err == nil {
		t.Fatal("accepted an admitted bundle after one target worktree was removed")
	}
}

func TestRepairBundle_RejectsDigestWithoutTargetBindings(t *testing.T) {
	state := &State{Workspace: "/tmp/empty-bundle", RepairBundleSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := state.VerifyRepairBranch(); err == nil {
		t.Fatal("accepted a bundle digest with no admitted target identities")
	}
}

func TestRepairBundle_RecoversCrashAfterWorktreeCreationBeforeReceiptUpdate(t *testing.T) {
	f, forge := branchFixture(t)
	android := filepath.Join(f.srcRoot, "android-original")
	gitRepairFixture(t, android)
	request := RepairBundleRequest{EpisodeID: "bundle-partial-recovery", Template: "standard_fix", Destination: filepath.Join(f.srcRoot, "bundle-partial-recovery"), Targets: []RepairBundleTarget{{ID: "forge", Repository: forge, Base: "HEAD"}, {ID: "android", Repository: android, Base: "HEAD"}}}
	request.Destination, _ = canonicalProspectivePath(request.Destination)
	for i := range request.Targets {
		request.Targets[i].Repository, _ = canonicalPath(request.Targets[i].Repository)
		request.Targets[i].Base, _ = repairGit(request.Targets[i].Repository, "rev-parse", "--verify", "HEAD^{commit}")
	}
	sort.Slice(request.Targets, func(i, j int) bool { return request.Targets[i].ID < request.Targets[j].ID })
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256(requestBytes)
	receipt := RepairBundleReceipt{Schema: repairBundleSchema, EpisodeID: request.EpisodeID, Template: request.Template, Destination: request.Destination, RequestSHA: hex.EncodeToString(requestHash[:]), Status: "PREPARING", Targets: make([]RepairBundleTargetReceipt, 0, 2)}
	for _, target := range request.Targets {
		receipt.Targets = append(receipt.Targets, RepairBundleTargetReceipt{ID: target.ID, Repository: target.Repository, Workspace: filepath.Join(request.Destination, target.ID)})
	}
	key := sha256.Sum256([]byte(request.EpisodeID))
	receiptPath := filepath.Join(f.cfg.StoreRoot, "REPAIR_BUNDLES", hex.EncodeToString(key[:])+".json")
	if err := writeRepairBundleReceipt(receiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(request.Destination, 0700); err != nil {
		t.Fatal(err)
	}
	first := request.Targets[0]
	branch := RepairBranchName("agent-engine/repair/", request.EpisodeID+"-"+first.ID)
	if _, err := repairGit(first.Repository, "worktree", "add", "--no-track", "-b", branch, filepath.Join(request.Destination, first.ID), first.Base); err != nil {
		t.Fatal(err)
	}

	prepared, err := f.store.PrepareRepairBundle(RepairBundleRequest{EpisodeID: request.EpisodeID, Template: request.Template, Destination: filepath.Join(f.srcRoot, "bundle-partial-recovery"), Targets: []RepairBundleTarget{{ID: "forge", Repository: forge, Base: "HEAD"}, {ID: "android", Repository: android, Base: "HEAD"}}})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Status != "READY" || len(prepared.Targets) != 2 || prepared.Targets[0].Branch != branch {
		t.Fatalf("partial preparation did not recover the created worktree: %#v", prepared)
	}
}
