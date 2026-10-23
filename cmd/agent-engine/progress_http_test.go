package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/control"
)

func progressAPIFixture(t *testing.T) (*episodeAPI, string) {
	t.Helper()
	root, source := flowCLIFixture(t)
	cfg, err := control.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	store := control.NewStore(cfg)
	if _, err = store.InitializeDeployment(); err != nil {
		t.Fatal(err)
	}
	ep := contract.EpisodeRequest{Version: 1, EpisodeID: "codex-notification-001", Template: "feature", Origin: contract.Origin{Kind: "agent", CallerID: "codex-local"}, Workspace: contract.Workspace{Path: source}, Objective: "Fixture notification", Constraints: contract.Constraints{AllowCodeChanges: true, AllowedPaths: []string{"src/**"}}, Obligations: []contract.WorkflowObligation{{ID: "O1", Criterion: "Preserve fixture behavior"}}}
	if _, err = store.AdmitEpisode(&ep); err != nil {
		t.Fatal(err)
	}
	return &episodeAPI{store: store, token: strings.Repeat("x", 32), callerPrefix: "codex-", templates: map[string]bool{"feature": true}, start: func(string) error { t.Fatal("observer started supervisor"); return nil }}, root
}

func TestProgressWriterHelper(t *testing.T) {
	root := os.Getenv("EMJIN_PROGRESS_WRITER_STORE")
	if root == "" {
		return
	}
	cfg, err := control.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.NewStore(cfg).UpdateCommitted("codex-notification-001", func(s *control.State) error {
		s.Episode.Phase = string(contract.PhasePlanAccept)
		s.Episode.PlanDigest = strings.Repeat("b", 64)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvents_SeparateWriterDecisionReplayAndDisconnect(t *testing.T) {
	api, root := progressAPIFixture(t)
	server := httptest.NewServer(api)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/episodes/codex-notification-001/events", nil)
	req.Header.Set("Authorization", "Bearer "+api.token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	next := func() control.ProgressFrame {
		t.Helper()
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var frame control.ProgressFrame
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
					t.Fatal(err)
				}
				return frame
			}
		}
		t.Fatal("stream ended", scanner.Err())
		return control.ProgressFrame{}
	}
	initial := next()
	if initial.Type != "snapshot" || initial.WaitingForOperator {
		t.Fatal(initial)
	}
	writer := exec.Command(os.Args[0], "-test.run=^TestProgressWriterHelper$")
	writer.Env = append(os.Environ(), "EMJIN_PROGRESS_WRITER_STORE="+root)
	if output, err := writer.CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	decision := next()
	if decision.Kind != "decision-required" || decision.PlanDigest != strings.Repeat("b", 64) || !decision.RequiresAction {
		t.Fatal(decision)
	}
	response.Body.Close()
	state, err := api.store.State("codex-notification-001")
	if err != nil || state.CancellationRequested || state.Episode.PlanAuthorized {
		t.Fatal("observer changed authority", err)
	}
	frames, _, err := api.store.ProgressFrames(state.RequestID, initial.EventID)
	if err != nil || len(frames) != 1 || frames[0].EventID != decision.EventID {
		t.Fatal("restart lost update", frames, err)
	}
	frames, _, err = api.store.ProgressFrames(state.RequestID, decision.EventID)
	if err != nil || len(frames) != 0 {
		t.Fatal("quiet waiting produced duplicate", frames, err)
	}
	if _, _, err = api.store.ProgressFrames(state.RequestID, decision.EventID+"bad"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestEvents_AuthenticationScopeAndCursor(t *testing.T) {
	api, _ := progressAPIFixture(t)
	for _, test := range []struct {
		token, prefix, cursor string
		want                  int
	}{{"wrong", "codex-", "", 401}, {api.token, "other-", "", 404}, {api.token, "codex-", "bad", 409}} {
		api.callerPrefix = test.prefix
		r := httptest.NewRequest("GET", "/episodes/codex-notification-001/events", nil)
		r.Header.Set("Authorization", "Bearer "+test.token)
		r.Header.Set("Last-Event-ID", test.cursor)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestEvents_ProductionPythonWatchReportsDecision(t *testing.T) {
	api, _ := progressAPIFixture(t)
	_, err := api.store.UpdateCommitted("codex-notification-001", func(s *control.State) error {
		s.Episode.Phase = string(contract.PhasePlanAccept)
		s.Episode.PlanDigest = strings.Repeat("b", 64)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	defer server.Close()
	directory := t.TempDir()
	token := filepath.Join(directory, "token")
	if err := os.WriteFile(token, []byte(api.token), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := filepath.Abs("../../examples/local-caller/client.py")
	if err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(directory, "cursor.json")
	cmd := exec.Command("python3", client, "--url", server.URL, "--token-file", token, "watch", "codex-notification-001", "--duration", "10", "--cursor-file", cursor)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	var frame control.ProgressFrame
	if err := json.Unmarshal(output, &frame); err != nil {
		t.Fatal(err, string(output))
	}
	if !frame.WaitingForOperator || frame.PlanDigest != strings.Repeat("b", 64) {
		t.Fatal(frame)
	}
	receipt, err := os.ReadFile(cursor)
	if err != nil || !strings.Contains(string(receipt), frame.EventID) {
		t.Fatal("cursor not retained", err)
	}
	state, err := api.store.State(frame.EpisodeID)
	if err != nil || state.Episode.PlanAuthorized || state.CancellationRequested {
		t.Fatal("observer granted authority", err)
	}
}

func TestEvents_ProductionPythonWatchReportsRuntimeFailure(t *testing.T) {
	api, _ := progressAPIFixture(t)
	_, err := api.store.UpdateCommitted("codex-notification-001", func(s *control.State) error {
		s.RuntimeBlock = &control.RuntimeBlock{Kind: "cleanup-unresolved", Reason: "owned reviewer process survived shutdown", IntendedOutcome: "BLOCKED", Generation: s.Generation, FirstObservedAt: "2026-10-13T12:00:00Z"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	defer server.Close()
	directory := t.TempDir()
	token := filepath.Join(directory, "token")
	if err := os.WriteFile(token, []byte(api.token), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := filepath.Abs("../../examples/local-caller/client.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", client, "--url", server.URL, "--token-file", token, "watch", "codex-notification-001", "--duration", "10")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	var frame control.ProgressFrame
	if err := json.Unmarshal(output, &frame); err != nil {
		t.Fatal(err, string(output))
	}
	if !frame.RequiresAction || frame.WaitingForOperator || frame.ActionKind != "reconcile-cleanup" || frame.Reason != "owned reviewer process survived shutdown" {
		t.Fatal("runtime failure did not yield an actionable observation", frame)
	}
	state, err := api.store.State(frame.EpisodeID)
	if err != nil || state.Episode.PlanAuthorized || state.CancellationRequested {
		t.Fatal("observer changed authority", err)
	}
}

// Cross-repository qualification is explicit; normal Go tests need no Node or
// Forge checkout. This drives the actual compiled Forge adapter against our API.
func TestEvents_ProductionForgeAdapter(t *testing.T) {
	module := os.Getenv("AGENT_ENGINE_FORGE_OBSERVER_MODULE")
	node := os.Getenv("AGENT_ENGINE_FIXTURE_NODE")
	if module == "" || node == "" {
		t.Skip("cross-repository fixture inputs not configured")
	}
	api, _ := progressAPIFixture(t)
	server := httptest.NewServer(api)
	defer server.Close()
	script := filepath.Join(t.TempDir(), "observer.mjs")
	code := `import * as adapter from ` + strconv.Quote("file://"+module) + `;
const constructors = Object.values(adapter).filter(v => typeof v === "function" && typeof v.prototype?.resumeObservation === "function" && typeof v.prototype?.awaitCompletion === "function");
if (constructors.length !== 1) throw new Error("expected exactly one compatible repair adapter");
const a = new constructors[0]({endpoint:process.argv[2],bearerToken:process.env.AGENT_ENGINE_FIXTURE_TOKEN,completionTimeoutMs:15000,onProgress:f=>console.log(JSON.stringify(f))});
await a.resumeObservation('codex-notification-001');
const result=await a.awaitCompletion();
if(result.exitCode!==1) throw new Error('fixture FAILED result became success');
`
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script, server.URL)
	cmd.Env = append(os.Environ(), "AGENT_ENGINE_FIXTURE_TOKEN="+api.token)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	// A separate committed writer changes the state while Forge is subscribed.
	time.Sleep(2 * time.Second)
	_, err := api.store.UpdateCommitted("codex-notification-001", func(s *control.State) error {
		s.Episode.Phase = string(contract.PhasePlanAccept)
		s.Episode.PlanDigest = strings.Repeat("b", 64)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	_, err = api.store.UpdateCommitted("codex-notification-001", func(s *control.State) error {
		s.Episode.Phase = string(contract.PhaseReportReview)
		s.RuntimeBlock = &control.RuntimeBlock{Kind: "reviewer-unavailable", Reason: "fixture reviewer exited without an answer", IntendedOutcome: "BLOCKED", Generation: s.Generation, FirstObservedAt: "2026-10-13T12:00:00Z"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	_, err = api.store.UpdateCommitted("codex-notification-001", func(s *control.State) error {
		s.Stage = control.StageFailed
		s.StageReason = "fixture terminal failure; no model invoked"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err, output.String())
	}
	if !strings.Contains(output.String(), `"waiting_for_operator":true`) || !strings.Contains(output.String(), `"terminal":true`) {
		t.Fatal("Forge missed decision or terminal", output.String())
	}
	if !strings.Contains(output.String(), `"action_kind":"inspect-reviewer"`) || !strings.Contains(output.String(), `"reason":"fixture reviewer exited without an answer"`) {
		t.Fatal("Forge missed the actionable runtime failure", output.String())
	}
	state, err := api.store.State("codex-notification-001")
	if err != nil || state.Episode.PlanAuthorized || state.CancellationRequested {
		t.Fatal("observer changed authority", err)
	}
}
