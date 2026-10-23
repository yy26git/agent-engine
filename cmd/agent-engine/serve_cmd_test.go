package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

func TestLocalEpisodeAPI_PreparesAuthorizedRepairBundle(t *testing.T) {
	root, source := newCLIFixture(t)
	configPath := filepath.Join(root, "CONTROL.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	repairPolicy := `
[episode_templates.forge-repair]
phases = ["REPORT", "REPORT_REVIEW", "PLAN", "PLAN_ACCEPT", "IMPLEMENT", "VERIFY", "COMPLETION_REVIEW"]
roles = ["worker=investigator", "reviewer=auditor"]
required_checks = ["build"]
max_repair_iterations = 2
repair_branch_prefix = "agent-engine/repair/"
`
	if err := os.WriteFile(configPath, append(config, []byte(repairPolicy)...), 0600); err != nil {
		t.Fatal(err)
	}
	forgeRoot, androidRoot := filepath.Join(source, "forge"), filepath.Join(source, "android")
	for _, repo := range []string{forgeRoot, androidRoot} {
		if err := os.MkdirAll(repo, 0700); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-C", repo, "init", "-b", "main"}, {"-C", repo, "config", "user.name", "Fixture"}, {"-C", repo, "config", "user.email", "fixture@example.invalid"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("baseline\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-C", repo, "add", "seed.txt"}, {"-C", repo, "commit", "-m", "baseline"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	api := &episodeAPI{store: storeFor(t, root), token: "secret", callerPrefix: "forge-", templates: map[string]bool{"forge-repair": true}}
	request := control.RepairBundleRequest{EpisodeID: "forge-case-bundle", Template: "forge-repair", Destination: filepath.Join(source, "bundles", "forge-case-bundle"), Targets: []control.RepairBundleTarget{{ID: "forge", Repository: forgeRoot, Base: "HEAD"}, {ID: "android", Repository: androidRoot, Base: "HEAD"}}}
	if err := os.MkdirAll(filepath.Dir(request.Destination), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	run := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/repair-bundles", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		api.ServeHTTP(response, req)
		return response
	}
	response := run(body)
	if response.Code != 201 {
		t.Fatalf("preparation status=%d body=%s", response.Code, response.Body.String())
	}
	var receipt control.RepairBundleReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "READY" || len(receipt.Targets) != 2 || len(receipt.ReceiptSHA) != 64 {
		t.Fatalf("bad API receipt: %#v", receipt)
	}
	request.EpisodeID = "other-caller-episode"
	body, _ = json.Marshal(request)
	if response := run(body); response.Code != 403 {
		t.Fatalf("cross-caller preparation status=%d", response.Code)
	}
}

func TestLocalEpisodeAPI_AuthenticationAdmissionStatusAndCancellation(t *testing.T) {
	root := episodeFixtureWithTemplate(t)
	_, source := readFixtureRoots(t, root)
	path := episodeRequestFile(t, source, "EP-HTTP", "forge-investigation")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	api := &episodeAPI{store: storeFor(t, root), token: "01234567890123456789012345678901", callerPrefix: "forge-", start: func(id string) error {
		if id != "EP-HTTP" {
			t.Fatal(id)
		}
		calls++
		return nil
	}}
	request := func(method, path, token, origin string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", origin)
		res := httptest.NewRecorder()
		api.ServeHTTP(res, req)
		return res
	}
	if r := request("POST", "/episodes", "bad", "", body); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if r := request("POST", "/episodes", api.token, "https://example.com", body); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if calls != 0 {
		t.Fatal("unauthorized request launched supervision")
	}
	if r := request("POST", "/episodes", api.token, "", body); r.Code != 202 {
		t.Fatal(r.Code, r.Body.String())
	}
	if calls != 1 {
		t.Fatal("admission did not request supervision")
	}
	r := request("GET", "/episodes/EP-HTTP", api.token, "", nil)
	var status map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["stage"] == nil || status["phase"] != "REPORT" {
		t.Fatal(status)
	}
	if r := request("GET", "/episodes/EP-HTTP/result", api.token, "", nil); r.Code != 200 || r.Body.String() != "{\"ready\":false}\n" {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := request("POST", "/episodes/EP-HTTP/cancel", api.token, "", nil); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	state, err := api.store.State("EP-HTTP")
	if err != nil {
		t.Fatal(err)
	}
	if !state.CancellationRequested || state.Stage.Terminal() {
		t.Fatal("cancel became a terminal claim before cleanup")
	}
	// A terminal stage alone is not a durable result receipt.
	if _, err := api.store.UpdateCommitted("EP-HTTP", func(s *control.State) error {
		s.Stage = control.StageBlocked
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := request("GET", "/episodes/EP-HTTP/result", api.token, "", nil); r.Code != 200 || r.Body.String() != "{\"ready\":false}\n" {
		t.Fatal("terminal stage fabricated result readiness", r.Code, r.Body.String())
	}
}

func TestLocalEpisodeAPI_RefusedAdmissionDoesNotStartSupervisor(t *testing.T) {
	root := episodeFixtureWithTemplate(t)
	_, source := readFixtureRoots(t, root)
	path := episodeRequestFile(t, source, "EP-HTTP-REFUSED", "unapproved")
	body, _ := os.ReadFile(path)
	api := &episodeAPI{store: storeFor(t, root), token: "secret", callerPrefix: "forge-", start: func(string) error { t.Fatal("refused episode launched"); return nil }}
	req := httptest.NewRequest("POST", "/episodes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	res := httptest.NewRecorder()
	api.ServeHTTP(res, req)
	if res.Code != 409 {
		t.Fatal(res.Code, res.Body.String())
	}
	if _, err := api.store.State("EP-HTTP-REFUSED"); err == nil {
		t.Fatal("refused request persisted")
	}
}

func TestLocalEpisodeAPI_CallerWorkflowAndEpisodeIsolation(t *testing.T) {
	root := episodeFixtureWithTemplate(t)
	_, source := readFixtureRoots(t, root)
	file := episodeRequestFile(t, source, "EP-FORGE-OWNED", "forge-investigation")
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	st := storeFor(t, root)
	forge := &episodeAPI{store: st, token: "forge-token", callerPrefix: "forge-", templates: map[string]bool{"forge-investigation": true}, start: func(string) error { return nil }}
	request := func(api *episodeAPI, method, path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+api.token)
		out := httptest.NewRecorder()
		api.ServeHTTP(out, req)
		return out
	}
	if r := request(forge, "POST", "/episodes", body); r.Code != 202 {
		t.Fatal(r.Body.String())
	}
	starts := 0
	codex := &episodeAPI{store: st, token: "codex-token", callerPrefix: "codex-", templates: map[string]bool{"codex-feature": true}, start: func(string) error { starts++; return nil }}
	for _, route := range []struct{ method, path string }{{"GET", "/episodes/EP-FORGE-OWNED"}, {"GET", "/episodes/EP-FORGE-OWNED/result"}, {"POST", "/episodes/EP-FORGE-OWNED/cancel"}} {
		if r := request(codex, route.method, route.path, nil); r.Code != 404 {
			t.Fatalf("cross-caller %s exposed episode: %d", route.path, r.Code)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	payload["episode_id"] = "EP-CODEX-WRONG-FLOW"
	payload["origin"] = map[string]any{"kind": "agent", "caller_id": "codex-local"}
	body, _ = json.Marshal(payload)
	if r := request(codex, "POST", "/episodes", body); r.Code != 403 {
		t.Fatalf("caller selected another workflow: %d %s", r.Code, r.Body.String())
	}
	state, err := st.State("EP-FORGE-OWNED")
	if err != nil {
		t.Fatal(err)
	}
	if starts != 0 || state.CancellationRequested {
		t.Fatal("cross-caller request caused effects")
	}
	// A different locally registered workflow uses the same admission engine.
	template, _ := st.Config().EpisodeTemplates.Lookup("forge-investigation")
	template.Name = "codex-feature"
	if err := st.Config().EpisodeTemplates.Register(template); err != nil {
		t.Fatal(err)
	}
	payload["episode_id"] = "EP-CODEX-OWNED"
	payload["template"] = template.Name
	payload["objective"] = "Investigate a feature requirement supplied by Codex."
	payload["obligations"] = []map[string]string{{"id": "feature-evidence", "criterion": "The report names a concrete implementation and verification strategy."}}
	body, _ = json.Marshal(payload)
	if r := request(codex, "POST", "/episodes", body); r.Code != 202 {
		t.Fatalf("distinct caller workflow refused: %d %s", r.Code, r.Body.String())
	}
	if starts != 1 {
		t.Fatal("Codex workflow did not launch through the common engine")
	}
	if r := request(forge, "GET", "/episodes/EP-CODEX-OWNED", nil); r.Code != 404 {
		t.Fatal("Forge saw Codex episode")
	}
}
