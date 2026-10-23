package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/agentdeck"
	"github.com/emjin/agent-engine/internal/control"
)

func flowCLIFixture(t *testing.T) (string, string) {
	t.Helper()
	store, source := newCLIFixture(t)
	f, err := os.OpenFile(filepath.Join(store, "CONTROL.toml"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`
[profiles.worker]
role = "worker"
tool = "mcode"
model = "fixture-worker"
[profiles.audit]
role = "reviewer"
tool = "mcode"
read_only = "true"
confinement_proven = "true"
[episode_templates.feature]
phases = ["REPORT", "REPORT_REVIEW", "PLAN", "PLAN_ACCEPT", "IMPLEMENT", "VERIFY", "COMPLETION_REVIEW"]
roles = ["worker=worker", "reviewer=audit"]
required_checks = ["build"]
max_repair_iterations = 1
read_only = "false"
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return store, source
}

func TestFlowCLI_DiscoverPrepareValidateWithoutAdmission(t *testing.T) {
	store, source := flowCLIFixture(t)
	var out, diagnostics bytes.Buffer
	call := func(args ...string) int {
		out.Reset()
		diagnostics.Reset()
		return run(append([]string{"--store", store, "--json"}, args...), &out, &diagnostics)
	}
	if code := call("flows", "--template", "feature"); code != 0 {
		t.Fatalf("flows: %s", diagnostics.String())
	}
	if !strings.Contains(out.String(), "fixture-worker") || !strings.Contains(out.String(), "PLAN_ACCEPT") {
		t.Fatal(out.String())
	}
	requestFile := filepath.Join(t.TempDir(), "episode.json")
	args := []string{"episode", "prepare", "--episode-id", "feature-001", "--template", "feature", "--workspace", source, "--objective", "Fix addition", "--caller-id", "codex-local", "--criterion", "Addition regression passes", "--allow-code-changes", "--allowed-path", "src/**", "--protected-path", "tests/**", "--output", requestFile}
	if code := call(args...); code != 0 {
		t.Fatalf("prepare: %s", diagnostics.String())
	}
	b, err := os.ReadFile(requestFile)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := contract.DecodeEpisodeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Verification[0].CheckID != "build" || ep.Constraints.ProtectedPaths[0] != "tests/**" {
		t.Fatalf("lost request requirements: %+v", ep)
	}
	if code := call(args...); code == 0 {
		t.Fatal("prepare overwrote an existing request")
	}
	if code := call("episode", "validate", "--request-file", requestFile); code == 0 {
		t.Fatal("unqualified fixture runtime reported ready")
	}
	var refusal map[string]any
	if err := json.Unmarshal(out.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal["ready"] != false || refusal["agents_started"] != false || refusal["code"] == nil {
		t.Fatal(refusal)
	}
	if _, err := os.Stat(filepath.Join(store, "REQUESTS", "feature-001")); !os.IsNotExist(err) {
		t.Fatalf("validation created request: %v", err)
	}
}

func TestFlowCLI_PrepareRequiresCriteriaAndExplicitWriteScope(t *testing.T) {
	store, source := flowCLIFixture(t)
	for _, extra := range [][]string{nil, {"--criterion", "Regression passes"}, {"--allow-code-changes"}, {"--allow-code-changes", "--criterion", "Regression passes"}} {
		var out, errOut bytes.Buffer
		args := []string{"--store", store, "--json", "episode", "prepare", "--episode-id", "feature-001", "--template", "feature", "--workspace", source, "--objective", "Fix addition", "--caller-id", "codex-local"}
		if run(append(args, extra...), &out, &errOut) == 0 {
			t.Fatalf("incomplete requirements accepted: %v", extra)
		}
	}
}

func TestFlowCLI_WatchDoesNotCreateUnknownEpisode(t *testing.T) {
	store, _ := flowCLIFixture(t)
	var out, errOut bytes.Buffer
	if run([]string{"--store", store, "episode", "watch", "--episode-id", "absent", "--timeout", "1ms"}, &out, &errOut) == 0 {
		t.Fatal("absent episode accepted")
	}
	if _, err := os.Stat(filepath.Join(store, "REQUESTS", "absent")); !os.IsNotExist(err) {
		t.Fatal("watch created request")
	}
}

func TestFlowCLI_WatchAcceptanceTerminalAndLaunchRetry(t *testing.T) {
	store, source := flowCLIFixture(t)
	req := map[string]any{"version": 1, "episode_id": "watch-001", "template": "feature",
		"origin":    map[string]string{"kind": "agent", "caller_id": "codex-local"},
		"workspace": map[string]string{"path": source}, "objective": "Fix addition",
		"obligations":  []map[string]string{{"id": "O-1", "criterion": "Regression passes"}},
		"constraints":  map[string]any{"allow_code_changes": true, "allowed_paths": []string{"src/**"}},
		"verification": []map[string]string{{"check_id": "build"}}}
	file := writeRequest(t, t.TempDir(), req)
	var out, errOut bytes.Buffer
	if run([]string{"--store", store, "episode", "admit", "--request-file", file}, &out, &errOut) != 0 {
		t.Fatal(errOut.String())
	}
	cfg, err := control.LoadConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	st := control.NewStore(cfg)
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "--json", "episode", "watch", "--episode-id", "watch-001", "--timeout", "1ms"}, &out, &errOut) != 3 {
		t.Fatal("active observation did not time out", errOut.String())
	}
	if !strings.Contains(errOut.String(), "not cancelled") {
		t.Fatal(errOut.String())
	}
	_, err = st.UpdateCommitted("watch-001", func(s *control.State) error { s.Episode.Phase = string(contract.PhasePlanAccept); return nil })
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.State("watch-001")
	if err != nil {
		t.Fatal(err)
	}
	revision := state.Revision
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "--json", "episode", "watch", "--episode-id", "watch-001"}, &out, &errOut) != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), `"waiting_for_operator":true`) {
		t.Fatal(out.String())
	}
	state, err = st.State("watch-001")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != revision || len(state.OwnedProcesses) > 0 {
		t.Fatal("watch changed execution state")
	}
	_, err = st.PublishResult("watch-001", contract.Result{Status: contract.StatusFail, Summary: "fixture failed check"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "--json", "episode", "watch", "--episode-id", "watch-001"}, &out, &errOut) != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), `"terminal":true`) || !strings.Contains(out.String(), `"result_digest"`) {
		t.Fatal(out.String())
	}
	state, err = st.State("watch-001")
	if err != nil {
		t.Fatal(err)
	}
	revision = state.Revision
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "--json", "episode", "launch", "--request-file", file}, &out, &errOut) != 0 {
		t.Fatal(errOut.String())
	}
	// A terminal retry observes frozen admission without requiring runtime launch.
	if !strings.Contains(out.String(), `"terminal": true`) {
		t.Fatal(out.String())
	}
	state, err = st.State("watch-001")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != revision || len(state.OwnedProcesses) > 0 {
		t.Fatal("terminal launch retry started or rewrote work")
	}
	req["objective"] = "changed task"
	changed := writeRequest(t, t.TempDir(), req)
	out.Reset()
	errOut.Reset()
	if run([]string{"--store", store, "episode", "launch", "--request-file", changed}, &out, &errOut) == 0 {
		t.Fatal("conflicting retry accepted")
	}
}

func TestFlowAPI_DiscoveryAndPreflightRespectScopeWithoutStarting(t *testing.T) {
	root, source := flowCLIFixture(t)
	cfg, err := control.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	api := &episodeAPI{store: control.NewStore(cfg), token: "fixture-token", callerPrefix: "codex-",
		templates: map[string]bool{"feature": true}, start: func(string) error { starts++; return nil }}
	request := func(method, path, token string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}
	if r := request("GET", "/flows", "bad", nil); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if r := request("GET", "/flows", api.token, nil); r.Code != 200 || !strings.Contains(r.Body.String(), "fixture-worker") {
		t.Fatal(r.Code, r.Body.String())
	}
	api.templates = map[string]bool{"not-feature": true}
	if r := request("GET", "/flows", api.token, nil); r.Code != 200 || strings.Contains(r.Body.String(), "fixture-worker") {
		t.Fatal("out-of-scope discovery", r.Body.String())
	}
	api.templates = map[string]bool{"feature": true}
	ep := contract.EpisodeRequest{Version: 1, EpisodeID: "codex-001", Template: "feature", Origin: contract.Origin{Kind: "agent", CallerID: "codex-local"}, Workspace: contract.Workspace{Path: source}, Objective: "Fix addition", Constraints: contract.Constraints{AllowCodeChanges: true, AllowedPaths: []string{"src/**"}}, Obligations: []contract.WorkflowObligation{{ID: "O1", Criterion: "Regression passes"}}}
	body, _ := json.Marshal(ep)
	r := request("POST", "/episodes/validate", api.token, body)
	if r.Code != 409 || !strings.Contains(r.Body.String(), "prerequisite_upstream_pin") {
		t.Fatal(r.Code, r.Body.String())
	}
	ep.EpisodeID = "forge-001"
	body, _ = json.Marshal(ep)
	if r := request("POST", "/episodes/validate", api.token, body); r.Code != 403 {
		t.Fatal(r.Code, r.Body.String())
	}
	if starts != 0 {
		t.Fatal("preflight launched supervisor")
	}
	if _, err := os.Stat(filepath.Join(root, "REQUESTS")); !os.IsNotExist(err) {
		t.Fatalf("API preflight wrote store: %v", err)
	}
}

func TestFlowAdapter_UsesExplicitStoreAndCompiledCLI(t *testing.T) {
	store, _ := flowCLIFixture(t)
	adapter, err := filepath.Abs("../../examples/local-caller/agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", adapter, "flows", "--json")
	cmd.Env = append(os.Environ(), "AGENT_ENGINE_BINARY="+compiledCLI(t), "AGENT_ENGINE_STORE="+store)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("adapter: %v %s", err, b)
	}
	if !strings.Contains(string(b), "fixture-worker") {
		t.Fatal(string(b))
	}
	cmd = exec.Command("sh", adapter, "flows", "--json")
	cmd.Env = append(os.Environ(), "AGENT_ENGINE_BINARY="+compiledCLI(t), "AGENT_ENGINE_STORE=relative")
	if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "must be absolute") {
		t.Fatalf("adapter guessed store: %v %s", err, b)
	}
}

func TestFlowAPI_ThinClientPreviewsPinnedFixtureWithoutExecuting(t *testing.T) {
	store, source := flowCLIFixture(t)
	runtimePath := filepath.Join(t.TempDir(), "agent-deck-fixture")
	marker := runtimePath + ".executed"
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	digest, err := agentdeck.FileSHA256(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	lock := agentdeck.RuntimeLock{Schema: 1, SourceCommit: strings.Repeat("a", 40), Artifacts: map[string]agentdeck.LockedArtifact{runtime.GOOS + "/" + runtime.GOARCH: {Path: runtimePath, SHA256: digest}}}
	lockBytes, _ := json.Marshal(lock)
	lockPath := filepath.Join(t.TempDir(), "runtime-lock.json")
	if err := os.WriteFile(lockPath, lockBytes, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ENGINE_RUNTIME_LOCK", lockPath)
	configPath := filepath.Join(store, "CONTROL.toml")
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`agent_deck_binary = "agent-deck"`), []byte(`agent_deck_binary = "`+runtimePath+`"`), 1)
	if err := os.WriteFile(configPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := control.LoadConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("x", 32)
	tokenFile := filepath.Join(t.TempDir(), "fixture.token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	starts := 0
	api := &episodeAPI{store: control.NewStore(cfg), token: token, callerPrefix: "codex-", templates: map[string]bool{"feature": true}, start: func(string) error { starts++; return nil }}
	server := httptest.NewServer(api)
	defer server.Close()
	ep := contract.EpisodeRequest{Version: 1, EpisodeID: "codex-client-001", Template: "feature", Origin: contract.Origin{Kind: "agent", CallerID: "codex-local"}, Workspace: contract.Workspace{Path: source}, Objective: "Fix addition", Constraints: contract.Constraints{AllowCodeChanges: true, AllowedPaths: []string{"src/**"}}, Obligations: []contract.WorkflowObligation{{ID: "O1", Criterion: "Regression passes"}}}
	requestBytes, _ := json.Marshal(ep)
	requestFile := filepath.Join(t.TempDir(), "episode.json")
	if err := os.WriteFile(requestFile, requestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	client, err := filepath.Abs("../../examples/local-caller/client.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"flows"}, {"validate", requestFile}} {
		cmd := exec.Command("python3", append([]string{client, "--url", server.URL, "--token-file", tokenFile}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("client %v: %v %s", args, err, b)
		}
		if args[0] == "validate" {
			var result map[string]any
			if err := json.Unmarshal(b, &result); err != nil {
				t.Fatal(err)
			}
			if result["ready"] != true || result["agents_started"] != false {
				t.Fatal(string(b))
			}
		}
	}
	if starts != 0 {
		t.Fatal("client preview invoked supervisor")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("preflight executed pinned binary")
	}
	if _, err := os.Stat(filepath.Join(store, "REQUESTS")); !os.IsNotExist(err) {
		t.Fatal("client preview admitted work")
	}
}
