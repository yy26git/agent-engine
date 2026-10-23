package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func TestEpisodeWorkerReleaseCLI_ReportsDigestAndPersistsExactRequest(t *testing.T) {
	storePath, source := flowCLIFixture(t)
	requestBody := map[string]any{
		"version": 1, "episode_id": "release-cli-episode", "template": "feature",
		"origin":    map[string]string{"kind": "agent", "caller_id": "local-test"},
		"workspace": map[string]string{"path": source}, "objective": "Fix the feature",
		"obligations":  []map[string]string{{"id": "O-1", "criterion": "The feature behaves as specified"}},
		"constraints":  map[string]any{"allow_code_changes": true, "allowed_paths": []string{"src/**"}, "protected_paths": []string{"tests/**"}},
		"verification": []map[string]string{{"check_id": "build"}},
	}
	requestPath := writeRequest(t, t.TempDir(), requestBody)
	if code := run([]string{"--store", storePath, "episode", "admit", "--request-file", requestPath}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("fixture episode admission failed")
	}
	cfg, err := control.LoadConfig(storePath)
	if err != nil {
		t.Fatal(err)
	}
	store := control.NewStore(cfg)
	if _, err := store.UpdateCommitted("release-cli-episode", func(st *control.State) error {
		st.Episode.Phase = string(control.PhaseImplement)
		st.Episode.PlanAuthorized = true
		st.Episode.PlanDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		st.Episode.PlanBaseline = &control.Snapshot{ContentDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
		st.BackendBindings = map[control.ProfileRole]control.SessionBinding{
			control.RoleWorker: {Backend: "minimax-acp", SessionID: "mvs_fixture", Generation: st.Generation, Resolved: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var statusOut, diagnostics bytes.Buffer
	if code := run([]string{"--store", storePath, "episode", "status", "--episode-id", "release-cli-episode", "--json"}, &statusOut, &diagnostics); code != 0 {
		t.Fatalf("status failed: %s", diagnostics.String())
	}
	var status map[string]any
	if err := json.Unmarshal(statusOut.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	digest, ok := status["worker_release_binding_sha256"].(string)
	if !ok || len(digest) != 64 {
		t.Fatalf("status did not expose the exact release digest: %s", statusOut.String())
	}
	if code := run([]string{"--store", storePath, "episode", "request-worker-release", "--episode-id", "release-cli-episode", "--expected-binding-sha256", digest}, &bytes.Buffer{}, &diagnostics); code == 0 {
		t.Fatal("release request succeeded without an active supervisor owner")
	}
	ownerRelease, err := store.AcquireOwnership("release-cli-episode")
	if err != nil {
		t.Fatalf("establish fixture supervisor ownership: %v", err)
	}
	defer ownerRelease()
	statusOut.Reset()
	if code := run([]string{"--store", storePath, "episode", "request-worker-release", "--episode-id", "release-cli-episode", "--expected-binding-sha256", digest, "--json"}, &statusOut, &diagnostics); code != 0 {
		t.Fatalf("release request failed: %s", diagnostics.String())
	}
	var response struct {
		WorkerRelease control.EpisodeWorkerRelease `json:"worker_release"`
	}
	if err := json.Unmarshal(statusOut.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.WorkerRelease.Status != control.WorkerReleaseRequested || response.WorkerRelease.BindingSHA256 != digest {
		t.Fatalf("CLI returned a different or non-pending release: %+v", response.WorkerRelease)
	}
	state, err := store.State("release-cli-episode")
	if err != nil {
		t.Fatal(err)
	}
	if state.EpisodeWorkerRelease == nil || state.EpisodeWorkerRelease.SessionID != "mvs_fixture" || state.EpisodeWorkerRelease.Status != control.WorkerReleaseRequested {
		t.Fatalf("CLI did not persist the exact request: %+v", state.EpisodeWorkerRelease)
	}
	if _, err := contract.DecodeEpisodeRequest(mustRead(t, requestPath)); err != nil {
		t.Fatalf("fixture request became invalid: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
