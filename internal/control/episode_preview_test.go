package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEpisodePreview_UsesAdmissionPolicyWithoutWriting(t *testing.T) {
	f := episodeBoundFixture(t, allEpisodeRoles())
	ep := episodeRequest(f, "preview-001")
	before, err := os.ReadDir(f.cfg.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ValidateEpisode(ep); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(f.cfg.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("preview changed store entries: %v -> %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StoreRoot, RequestsDirName, ep.EpisodeID)); !os.IsNotExist(err) {
		t.Fatalf("preview admitted request: %v", err)
	}
	ep.Verification[0].CheckID = "unregistered"
	previewErr := f.store.ValidateEpisode(ep)
	_, admissionErr := f.store.AdmitEpisode(ep)
	if previewErr == nil || admissionErr == nil || previewErr.Error() != admissionErr.Error() {
		t.Fatalf("preview/admission diverged: %v / %v", previewErr, admissionErr)
	}
}

func TestEpisodePreview_ConflictingRetryDoesNotRewriteState(t *testing.T) {
	f := episodeBoundFixture(t, allEpisodeRoles())
	ep := episodeRequest(f, "preview-retry")
	if _, err := f.store.AdmitEpisode(ep); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	ep.Objective = "different contents"
	if err := f.store.ValidateEpisode(ep); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("conflicting preview accepted: %v", err)
	}
	after, err := f.store.State(ep.EpisodeID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision != after.Revision {
		t.Fatal("preview rewrote state")
	}
}
