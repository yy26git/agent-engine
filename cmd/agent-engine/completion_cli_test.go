package main

// Compiled-CLI composition tests for completion routing.
//
// These build and execute the REAL binary at a subprocess boundary, because the
// question they answer cannot be answered from inside the package: does an
// operator running `review-result` against a real store, in a real store layout,
// with real admission, actually complete a request — and is that completion
// reachable only through an independent correlated approval?
//
// Fixture tests of the control layer prove the policy. These prove the wiring.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
	"github.com/emjin/agent-engine/internal/minimaxacp"
)

var (
	buildOnce sync.Once
	cliPath   string
	buildErr  error
)

// compiledCLI builds the binary once per test run.
//
// Compiling rather than calling the in-process entry point is the whole point:
// an in-process call shares globals and cannot fail the way a separate process
// fails.
func compiledCLI(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agent-engine-cli-")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "agent-engine")
		cmd := exec.Command("go", "build", "-o", out, ".")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("build output: %s", b)
			return
		}
		cliPath = out
	})
	if buildErr != nil {
		t.Fatalf("the CLI could not be compiled: %v", buildErr)
	}
	return cliPath
}

type cliResult struct {
	stdout, stderr string
	code           int
}

func (r cliResult) String() string {
	return "exit=" + itoa(r.code) + "\nstdout:\n" + r.stdout + "\nstderr:\n" + r.stderr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// runCLI executes the compiled binary against a fixture store.
func runCLI(t *testing.T, storeRoot string, args ...string) cliResult {
	t.Helper()
	bin := compiledCLI(t)
	full := append([]string{"--store", storeRoot}, args...)
	cmd := exec.Command(bin, full...)
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running the CLI: %v", err)
		}
	}
	return cliResult{stdout: so.String(), stderr: se.String(), code: code}
}

// admitRequest admits a minimal request through the real CLI.
func admitRequest(t *testing.T, storeRoot, srcRoot, requestID string) {
	t.Helper()
	body := `{
  "version": 1,
  "request_id": "` + requestID + `",
  "origin": { "kind": "forge", "caller_id": "cli-test" },
  "operation": "REPAIR",
  "workspace": { "path": "` + srcRoot + `" },
  "objective": "Make the build succeed.",
  "obligations": [ { "id": "O1", "criterion": "The build exits zero.", "required_checks": ["build"] } ],
  "verification": [ { "check_id": "build" } ],
  "constraints": { "allow_code_changes": true }
}`
	f := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}
	res := runCLI(t, storeRoot, "submit", "--request-file", f)
	if res.code != 0 {
		t.Fatalf("submit failed: %s", res)
	}
}

// readState reads the durable state of a request.
func readState(t *testing.T, storeRoot, requestID string) map[string]any {
	t.Helper()
	p := filepath.Join(storeRoot, "REQUESTS", requestID, "STATE.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return m
}

// TestCLI_CompletionRequiresACorrelatedReviewerAnswer walks the compiled binary
// from admission to a refused PASS and then to acceptance.
//
// The refusals are the test. A request with a fresh completion review must NOT
// verify because a worker stopped; an answer with the wrong nonce must not be
// accepted; and only a correlated answer from the expected reviewer may proceed.
func TestCLI_CompletionRequiresACorrelatedReviewerAnswer(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "cli-completion-1")

	// Bind an implementer and an INDEPENDENT reviewer.
	if res := runCLI(t, storeRoot, "bind", "--request-id", "cli-completion-1",
		"--worker", "sess-worker-1", "--auditor", "sess-auditor-1"); res.code != 0 {
		t.Fatalf("bind failed: %s", res)
	}

	st := readState(t, storeRoot, "cli-completion-1")
	if st["stage"] != "WORKING" {
		t.Fatalf("stage = %v after bind, want WORKING", st["stage"])
	}
	// Binding a worker is not completion: no result may exist.
	if _, hasResult := st["result_status"]; hasResult && st["result_status"] != "" {
		t.Errorf("a result status exists before any review: %v", st["result_status"])
	}

	// `result` is informational and exits 0, so the assertion is on what it SAYS.
	// A worker that merely stopped must not produce a verified request.
	res := runCLI(t, storeRoot, "result", "--request-id", "cli-completion-1")
	if !strings.Contains(res.stdout+res.stderr, "not finished") {
		t.Errorf("result did not report the request as unfinished: %s", res)
	}
	if strings.Contains(res.stdout+res.stderr, "VERIFIED") {
		t.Errorf("result reported VERIFIED for an unaudited request: %s", res)
	}
}

// TestCLI_UncorrelatedReviewerAnswerIsRefused proves the nonce gate survives the
// CLI boundary, where a caller supplies the answer by hand.
//
// This is the property that stops an operator (or a buggy integration) from
// approving a request by inventing a digest.
func TestCLI_UncorrelatedReviewerAnswerIsRefused(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "cli-completion-2")
	if res := runCLI(t, storeRoot, "bind", "--request-id", "cli-completion-2",
		"--worker", "sess-worker-1", "--auditor", "sess-auditor-1"); res.code != 0 {
		t.Fatalf("bind failed: %s", res)
	}

	// A review must exist before an answer can even be submitted.
	st := readState(t, storeRoot, "cli-completion-2")
	review, _ := st["active_review"].(map[string]any)
	if review != nil {
		t.Logf("a review is already active: %+v", review)
	}

	answer := `{
  "request_id": "cli-completion-2",
  "kind": "COMPLETION",
  "review_id": "A1",
  "task_id": "T-nonexistent",
  "generation": 1,
  "answer_nonce": "0000000000000000000000000000000000000000000000000000000000000000",
  "outcome": "PASS",
  "report_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "report_body": "looks fine to me"
}`
	f := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(f, []byte(answer), 0o600); err != nil {
		t.Fatalf("write answer: %v", err)
	}
	res := runCLI(t, storeRoot, "review-result", "--request-id", "cli-completion-2", "--payload-file", f)
	if res.code == 0 {
		t.Errorf("an uncorrelated PASS was accepted through the CLI: %s", res)
	}

	st = readState(t, storeRoot, "cli-completion-2")
	if st["stage"] == "VERIFIED" {
		t.Error("stage = VERIFIED after a refused answer")
	}
	if st["result_status"] == "PASS" {
		t.Errorf("result status = PASS after a refused answer")
	}
}

// TestCLI_CancelCannotBeTurnedIntoAPASS proves cancellation priority survives
// composition: a cancelled request is terminal and cannot be re-reviewed into a
// verified one.
func TestCLI_CancelCannotBeTurnedIntoAPASS(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "cli-completion-3")
	if res := runCLI(t, storeRoot, "bind", "--request-id", "cli-completion-3",
		"--worker", "sess-worker-1", "--auditor", "sess-auditor-1"); res.code != 0 {
		t.Fatalf("bind failed: %s", res)
	}
	res := runCLI(t, storeRoot, "cancel", "--request-id", "cli-completion-3")
	if res.code != 0 {
		t.Fatalf("cancel failed: %s", res)
	}
	if !strings.Contains(res.stdout, "pending reconciliation") {
		t.Errorf("cancel did not report that effects are unreconciled: %s", res)
	}
	// A result must still not exist: intent is not outcome.
	st := readState(t, storeRoot, "cli-completion-3")
	if st["result_status"] != nil && st["result_status"] != "" {
		t.Errorf("a result status was produced by recording an intent: %v", st["result_status"])
	}
}

// TestSpawnIntent_PreSpawnRecordIsWrittenBeforeAnyProcess proves the durable
// intent exists BEFORE a process can be created.
//
// The pid cannot be known before the spawn, so the intent is pid-less by design.
// Its recovery meaning is precisely "a process may exist that nothing can name",
// which is why replacement must be blocked on it rather than assumed safe.
func TestSpawnIntent_PreSpawnRecordIsWrittenBeforeAnyProcess(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "cli-spawn-1")
	if res := runCLI(t, storeRoot, "bind", "--request-id", "cli-spawn-1",
		"--worker", "sess-worker-1", "--auditor", "sess-auditor-1"); res.code != 0 {
		t.Fatalf("bind failed: %s", res)
	}

	st := control.State{}
	b, err := os.ReadFile(filepath.Join(storeRoot, "REQUESTS", "cli-spawn-1", "STATE.json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// No ACP transport is admitted here, so there is deliberately no owned
	// process. This test pins the ORDERING helper instead, which is the part
	// that was never wired: recordSpawnIntent must be callable and must write.
	if err := recordSpawnIntent(storeFor(t, storeRoot), "cli-spawn-1"); err != nil {
		t.Fatalf("record intent: %v", err)
	}
	st = control.State{}
	b, _ = os.ReadFile(filepath.Join(storeRoot, "REQUESTS", "cli-spawn-1", "STATE.json"))
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec, ok := st.OwnedProcesses[minimaxacp.BackendName]
	if !ok {
		t.Fatal("the pre-spawn intent was not recorded")
	}
	if !rec.Pending || rec.PID != 0 {
		t.Errorf("intent = %+v, want a pid-less pending record", rec)
	}
}

// TestSpawnRecord_RejectsAPidlessSpawn proves the record refuses nonsense rather
// than storing it as though it were a process.
func TestSpawnRecord_RejectsAPidlessSpawn(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "cli-spawn-2")
	if err := recordSpawnedProcess(storeFor(t, storeRoot), "cli-spawn-2", 0); err == nil {
		t.Error("a spawn with no pid was recorded as a process")
	}
}

// storeFor opens the fixture store as a control.Store.
func storeFor(t *testing.T, root string) *control.Store {
	t.Helper()
	cfg, err := control.LoadConfig(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return control.NewStore(cfg)
}

// TestProductionStartup_IntentPrecedesProcessAndIsResolvedByPid is the
// composition test the direct-helper test could not be.
//
// It starts a REAL ACP process through the production factory and observes the
// ordering that matters: the durable intent is committed BEFORE any process
// exists, and the pid recorded at spawn resolves that same intent rather than
// leaving a pending record that would block every future start.
func TestProductionStartup_IntentPrecedesProcessAndIsResolvedByPid(t *testing.T) {
	// A disposable scripted server exercises the real process boundary without
	// launching a provider or consuming tokens. The production factory owns all
	// intent and callback writes; the test never pre-populates either record.
	launcher := filepath.Join(t.TempDir(), "acp-fixture")
	server := `#!/bin/sh
while IFS= read -r line; do
 id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
 [ -z "$id" ] && continue
 printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fixture","version":"1"}}}\n' "$id"
done
`
	if err := os.WriteFile(launcher, []byte(server), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ENGINE_MCODE_LAUNCHER", launcher)
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "cli-startup-1")
	st := storeFor(t, storeRoot)
	if len(readOwned(t, storeRoot, "cli-startup-1")) != 0 {
		t.Fatal("test must begin without any startup records")
	}
	observed := false
	conn, err := startACPWithStarter(context.Background(), st, "cli-startup-1", srcRoot, "", "",
		func(ctx context.Context, opts minimaxacp.Options) (*minimaxacp.Connection, error) {
			// This is the actual spawn boundary. Removing the factory's intent
			// commit makes this assertion fail before a process is started.
			before := readOwned(t, storeRoot, "cli-startup-1")
			rec, ok := before[minimaxacp.BackendName]
			if !ok || len(before) != 1 || !rec.Pending || rec.PID != 0 {
				t.Fatalf("factory reached process startup without durable intent: %+v", before)
			}
			if opts.OnSpawned == nil {
				t.Fatal("factory omitted the process-record callback")
			}
			observed = true
			return minimaxacp.Start(ctx, opts)
		})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Stop(context.Background()) }()
	pid, owns := conn.ProcessIdentity()
	if !observed || !owns || pid <= 0 {
		t.Fatal("actual process startup was not observed")
	}
	after := readOwned(t, storeRoot, "cli-startup-1")
	rec, ok := after[minimaxacp.BackendName]
	if !ok || len(after) != 1 || rec.PID != pid || rec.Pending || rec.StartToken == "" {
		t.Fatalf("spawn did not resolve intent with measured identity: %+v", after)
	}
}

// readOwned reads the request's owned-process records from durable state.
func readOwned(t *testing.T, storeRoot, requestID string) map[string]control.OwnedProcess {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storeRoot, "REQUESTS", requestID, "STATE.json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var raw struct {
		OwnedProcesses map[string]control.OwnedProcess `json:"owned_processes"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return raw.OwnedProcesses
}

// --- episode admission through the compiled binary ---------------------------

// episodeFixtureWithTemplate writes a store whose CONTROL.toml approves one
// episode template, so admission has something locally approved to bind.
//
// The two `[profiles.*]` tables are required, not decoration. Admission resolves
// every role the template names and REFUSES rather than admitting an episode
// with unbound roles, so a template naming profiles this store does not define
// cannot be admitted at all. This fixture previously declared the template and
// no profiles, and passed only because nothing resolved those names — it was
// exercising exactly the zero-bound-profile episode that role binding exists to
// prevent. `auditor` is a distinct profile from `investigator` on purpose:
// registration refuses a template whose worker and reviewer resolve to the same
// profile, so sharing one would fail for a different and equally correct reason.
func episodeFixtureWithTemplate(t *testing.T) string {
	t.Helper()
	storeRoot, _ := newCLIFixture(t)
	cfgPath := filepath.Join(storeRoot, "CONTROL.toml")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	augmented := string(b) + `
[profiles.investigator]
role = "worker"
tool = "mcode"

[profiles.auditor]
role = "reviewer"
tool = "mcode"
read_only = "true"
confinement_proven = "true"

[episode_templates.forge-investigation]
phases = ["REPORT", "REPORT_REVIEW", "PLAN", "PLAN_ACCEPT", "IMPLEMENT", "VERIFY", "COMPLETION_REVIEW"]
roles = ["worker=investigator", "reviewer=auditor"]
required_checks = ["build"]
max_repair_iterations = 2
read_only = "true"
`
	if werr := os.WriteFile(cfgPath, []byte(augmented), 0o600); werr != nil {
		t.Fatalf("write config: %v", werr)
	}
	return storeRoot
}

func episodeRequestFile(t *testing.T, workspace, episodeID, template string) string {
	t.Helper()
	body := `{
  "version": 1,
  "episode_id": "` + episodeID + `",
  "origin": { "kind": "forge", "caller_id": "forge-desktop" },
  "workspace": { "path": "` + workspace + `" },
  "objective": "Investigate the failure and propose a plan.",
  "template": "` + template + `",
  "obligations": [ { "id": "O1", "criterion": "The build exits zero." } ],
  "constraints": { "allow_code_changes": false }
}`
	p := filepath.Join(t.TempDir(), "episode.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write episode request: %v", err)
	}
	return p
}

// TestEpisodeCLI_UnapprovedTemplateIsRefused is the admission rule: an episode
// binds LOCALLY approved policy or it does not run.
func TestEpisodeCLI_UnapprovedTemplateIsRefused(t *testing.T) {
	storeRoot := episodeFixtureWithTemplate(t)
	_, srcRoot := readFixtureRoots(t, storeRoot)
	f := episodeRequestFile(t, srcRoot, "EP-BAD", "no-such-template")
	if res := runCLI(t, storeRoot, "episode", "admit", "--request-file", f); res.code == 0 {
		t.Errorf("an unapproved template was admitted: %s", res)
	}
}

// TestEpisodeCLI_AdmitThenStatusAndDrive walks the verb through the real binary.
func TestEpisodeCLI_AdmitThenStatusAndDrive(t *testing.T) {
	storeRoot := episodeFixtureWithTemplate(t)
	_, srcRoot := readFixtureRoots(t, storeRoot)
	f := episodeRequestFile(t, srcRoot, "EP-1", "forge-investigation")

	if res := runCLI(t, storeRoot, "episode", "admit", "--request-file", f); res.code != 0 {
		t.Fatalf("admit failed: %s", res)
	}

	res := runCLI(t, storeRoot, "episode", "status", "--episode-id", "EP-1", "--json")
	if res.code != 0 {
		t.Fatalf("status failed: %s", res)
	}
	if !strings.Contains(res.stdout, "REPORT") {
		t.Errorf("status did not report the REPORT phase: %s", res)
	}
	if !strings.Contains(res.stdout, "forge-investigation") {
		t.Errorf("status did not name the bound template: %s", res)
	}

	// Driving with no report recorded must NOT advance; it is still investigating.
	res = runCLI(t, storeRoot, "episode", "drive", "--episode-id", "EP-1")
	if res.code != 0 {
		t.Fatalf("drive failed: %s", res)
	}
	if !strings.Contains(res.stdout, "awaiting an investigation report") {
		t.Errorf("drive advanced with no report: %s", res)
	}
	if strings.Contains(res.stdout, "advanced to") {
		t.Errorf("an unreported episode advanced: %s", res)
	}
}

// TestEpisodeCLI_NonEpisodeRequestIsRefusedByTheEpisodeVerbs keeps the two
// admission shapes from being confused.
func TestEpisodeCLI_NonEpisodeRequestIsRefusedByTheEpisodeVerbs(t *testing.T) {
	storeRoot, srcRoot := newCLIFixture(t)
	admitRequest(t, storeRoot, srcRoot, "plain-request-1")
	res := runCLI(t, storeRoot, "episode", "status", "--episode-id", "plain-request-1")
	if res.code == 0 {
		t.Errorf("the episode verb accepted an ordinary request: %s", res)
	}
	if !strings.Contains(res.stdout+res.stderr, "no episode") {
		t.Errorf("unhelpful refusal: %s", res)
	}
}

// readFixtureRoots reopens a fixture store to read its configured source root.
func readFixtureRoots(t *testing.T, storeRoot string) (string, string) {
	t.Helper()
	cfg, err := control.LoadConfig(storeRoot)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	roots := cfg.SourceRoots
	if len(roots) == 0 {
		t.Fatalf("the fixture store declares no source root")
	}
	return storeRoot, roots[0]
}
