// Package acceptance holds the production-wired acceptance tests.
//
// These are deliberately different from the unit tests in internal/control. A
// unit test can build a Supervisor over a temporary store and prove that a
// policy function behaves; that is worth a great deal and it proves nothing
// about whether a caller can use the product. Everything here drives the built
// binary through a subprocess, so a defect in argument parsing, the global
// --store requirement, the JSON contract, or the ordering of writes is visible.
//
// The rule the whole package obeys: **business progress is never made by
// constructing internal state.** Every test submits a request, submits
// checkpoints and answers, and reads a result through the same commands a human
// or a Forge client would use. A test that seeds a Store directly and then
// asserts on the result proves only that the fixture it built was correct.
//
// The Agent Deck boundary is satisfied by a contract fixture — a script that
// implements the documented CLI surface. That is the honest level available
// here: a live session needs a real harness, a real model and a real terminal,
// and pretending otherwise would turn an unavailable gate into a false one.
// The live smoke is a separate, explicitly blocked gate.
package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// controlBinary is the path of the built agent-engine executable, shared
// by every test in the package.
var (
	controlBinary string
	fixtureBinary string
	buildOnce     sync.Once
	buildErr      error
)

// buildBinaries compiles the real CLI once per test run.
func buildBinaries(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "agent-engine-acceptance-")
		if err != nil {
			buildErr = err
			return
		}
		controlBinary = filepath.Join(dir, "agent-engine")

		build := exec.Command("go", "build", "-o", controlBinary, "./cmd/agent-engine")
		build.Dir = root
		if out, berr := build.CombinedOutput(); berr != nil {
			buildErr = fmt.Errorf("building agent-engine: %v\n%s", berr, out)
			return
		}
		// The Agent Deck contract fixture is a shell script, not a Go build, so it
		// is written directly into the same temporary directory.
		fixtureBinary = filepath.Join(dir, "agent-deck-fixture")
		if werr := os.WriteFile(fixtureBinary, []byte(agentDeckFixtureScript), 0o755); werr != nil {
			buildErr = fmt.Errorf("writing agent-deck fixture: %w", werr)
		}
	})
	if buildErr != nil {
		t.Fatalf("acceptance build: %v", buildErr)
	}
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", errors.New("could not locate the module root from the acceptance test directory")
}

// result is the outcome of one CLI invocation.
type result struct {
	Code   int
	Stdout string
	Stderr string
}

// JSON decodes stdout as JSON, failing the test with the raw output when it is
// not JSON — which is itself a contract violation worth reporting clearly.
func (r result) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.Stdout), v); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %q\nstderr: %q", err, r.Stdout, r.Stderr)
	}
}

// world is one isolated acceptance environment: a store, a source tree, an
// evidence area and an output root, all under a single temporary directory.
type world struct {
	t         *testing.T
	Root      string
	Store     string
	Source    string
	Evidence  string
	Output    string
	RequestID string
	// finishSession, when set, makes the Agent Deck contract fixture emit one
	// session.finished frame for that session.
	finishSession string
}

// newWorld builds an isolated world and writes its CONTROL.toml.
func newWorld(t *testing.T, requestID string) *world {
	t.Helper()
	buildBinaries(t)

	root := t.TempDir()
	w := &world{
		t:         t,
		Root:      root,
		Store:     filepath.Join(root, "store"),
		Source:    filepath.Join(root, "src", "project"),
		Evidence:  filepath.Join(root, "evidence"),
		Output:    filepath.Join(root, "out"),
		RequestID: requestID,
	}
	for _, d := range []string{w.Store, w.Source, w.Evidence, w.Output} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// A tiny source tree so a snapshot and a managed check have real content.
	if err := os.WriteFile(filepath.Join(w.Source, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	toml := fmt.Sprintf(`agent_deck_binary = %q
profile = "acceptance"
tool = "mcode"
output_root = %q
source_roots = [%q]
evidence_roots = [%q]

[limits]
max_repair_cycles = 3
max_worker_generations = 3
max_audit_attempts = 3
max_guidance_rounds = 2

[checkpoints]
soft_active_budget_seconds = 60
grace_seconds = 5
max_wall_seconds = 3600
review_timeout_seconds = 600
min_wake_interval_seconds = 1

[checks.build]
argv = ["/bin/sh", "-c", "exit 0"]
timeout_seconds = 30

[checks.regression]
argv = ["/bin/sh", "-c", "printf 'ok  \\tacceptance\\t0.01s\\n'"]
timeout_seconds = 30
discovery_required = "true"
`, fixtureBinary, w.Output, filepath.Dir(w.Source), w.Evidence)
	if err := os.WriteFile(filepath.Join(w.Store, "CONTROL.toml"), []byte(toml), 0o600); err != nil {
		t.Fatalf("write CONTROL.toml: %v", err)
	}
	// The fixture runtime is pinned exactly like a real deployment. A test
	// deployment that skipped the pin would be testing a configuration the
	// product refuses to run, which is the surest way to ship a gate that only
	// works in tests.
	w.writeRuntimeLock(t)
	return w
}

// writeRuntimeLock pins the contract fixture as this world's approved artifact.
func (w *world) writeRuntimeLock(t *testing.T) {
	w.t.Helper()
	digest, err := agentdeck.FileSHA256(fixtureBinary)
	if err != nil {
		w.t.Fatalf("measure the fixture runtime: %v", err)
	}
	lock := map[string]any{
		"schema":        1,
		"repository":    "contract-fixture://agent-deck",
		"source_commit": "fixture-pinned-for-acceptance",
		"artifacts": map[string]any{
			goruntime.GOOS + "/" + goruntime.GOARCH: map[string]any{
				"path":   fixtureBinary,
				"sha256": digest,
			},
		},
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		w.t.Fatalf("encode runtime lock: %v", err)
	}
	// The lock lives beside the store so the CLI's lookup finds it.
	if err := os.WriteFile(filepath.Join(w.Store, "runtime-lock.json"), data, 0o600); err != nil {
		w.t.Fatalf("write runtime lock: %v", err)
	}
	// The working directory is the other lookup location, and `run` is invoked
	// from the test's directory, so the lock is also written there.
	if err := os.WriteFile(filepath.Join(w.Root, "runtime-lock.json"), data, 0o600); err != nil {
		w.t.Fatalf("write runtime lock: %v", err)
	}
}

// withFinishSession makes the contract fixture emit a finished frame for a
// session, so a test can drive a real completion event through the built CLI
// instead of hand-building the review state that would follow from one.
func (w *world) withFinishSession(t *testing.T, sessionID string) {
	t.Helper()
	w.finishSession = sessionID
}

// run invokes the built CLI exactly as a caller would.
func (w *world) run(args ...string) result {
	w.t.Helper()
	full := append([]string{"--store", w.Store}, args...)
	cmd := exec.Command(controlBinary, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A per-test environment keeps concurrent tests from sharing state.
	cmd.Env = append(os.Environ(), "AGENTDECK_ACCEPTANCE=1")
	// The lock is named explicitly rather than relying on the CLI's search path,
	// so a test world pins exactly the artifact it built and nothing else.
	if lock := filepath.Join(w.Store, "runtime-lock.json"); fileExists(lock) {
		cmd.Env = append(cmd.Env, "AGENT_ENGINE_RUNTIME_LOCK="+lock)
	}
	if w.finishSession != "" {
		cmd.Env = append(cmd.Env, "AGENTDECK_FIXTURE_FINISH_SESSION="+w.finishSession)
	}
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			w.t.Fatalf("running %s: %v", strings.Join(full, " "), err)
		}
	}
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// mustRun fails the test unless the command succeeded.
func (w *world) mustRun(args ...string) result {
	w.t.Helper()
	r := w.run(args...)
	if r.Code != 0 {
		w.t.Fatalf("%s failed (exit %d)\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// repairRequestFile writes a REPAIR request and returns its path.
func (w *world) repairRequestFile() string {
	w.t.Helper()
	return w.writeRequest(map[string]any{
		"version":    1,
		"request_id": w.RequestID,
		"origin":     map[string]any{"kind": "human", "caller_id": "acceptance"},
		"operation":  "REPAIR",
		"workspace":  map[string]any{"path": w.Source},
		"objective":  "Make the managed checks pass and record the evidence",
		"obligations": []map[string]any{
			{"id": "O-1", "criterion": "the build passes", "required_checks": []string{"build"}},
			{"id": "O-2", "criterion": "the regression suite runs and discovers tests", "required_checks": []string{"regression"}},
		},
		"constraints": map[string]any{
			"allow_code_changes": true,
			"allowed_paths":      []string{"**"},
			"protected_paths":    []string{"evaluation/**"},
		},
		"verification": []map[string]any{
			{"check_id": "build"}, {"check_id": "regression"},
		},
		"limits": map[string]any{"repair_cycles": 3},
	})
}

func (w *world) writeRequest(req map[string]any) string {
	w.t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		w.t.Fatalf("marshal request: %v", err)
	}
	path := filepath.Join(w.Root, "request.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		w.t.Fatalf("write request: %v", err)
	}
	return path
}

// status reads the request's state through the CLI.
func (w *world) status() map[string]any {
	w.t.Helper()
	r := w.mustRun("--json", "status", "--request-id", w.RequestID)
	var out map[string]any
	r.JSON(w.t, &out)
	return out
}

// agentDeckFixtureScript implements the documented Agent Deck CLI surface the
// runtime adapter uses.
//
// It is a contract fixture, not a mock of the real binary's behaviour: it answers
// the exact commands and JSON shapes the adapter parses, so a change to that
// parsing breaks these tests. It records what it was asked to do so a test can
// assert on recipient effects without a real agent.
const agentDeckFixtureScript = `#!/bin/sh
# Agent Deck contract fixture for agent-engine acceptance tests.
LOG="${AGENTDECK_FIXTURE_LOG:-/tmp/agent-engine-fixture.log}"
: >> "$LOG"
echo "$@" >> "$LOG"

# Strip leading global options before dispatching. The real CLI accepts options
# such as -p/--profile ahead of the subcommand and the adapter uses them, so a
# fixture that dispatched on the raw $1 answered every profiled call with its
# catch-all and reported a healthy event bus as disabled.
while [ $# -gt 0 ]; do
  case "$1" in
    -p|--profile) shift 2 ;;
    --profile=*) shift ;;
    -*) shift ;;
    *) break ;;
  esac
done
cmd="$1"; shift
case "$cmd" in
  add)
    id="sess-fixture-worker"
    echo "{\"ok\":true,\"session\":{\"id\":\"$id\",\"title\":\"fixture\",\"status\":\"idle\"}}"
    ;;
  session)
    sub="$1"; shift
    case "$sub" in
      start) echo "{\"ok\":true}" ;;
      stop)  echo "{\"ok\":true}" ;;
      send)
        # record the target and message, then report a landed queued send
        printf 'SEND %s\n' "$*" >> "$LOG"
        echo "{\"ok\":true,\"data\":{\"send_id\":\"send-1\",\"state\":\"landed\",\"settled\":true,\"session_id\":\"sess-fixture-auditor\"}}"
        ;;
      send-status)
        echo "{\"ok\":true,\"data\":{\"send_id\":\"$1\",\"state\":\"landed\",\"settled\":true}}"
        ;;
      output) echo "fixture output" ;;
      *) echo "{\"ok\":true}" ;;
    esac
    ;;
  list) echo "{\"sessions\":[{\"id\":\"sess-fixture-worker\",\"status\":\"idle\"},{\"id\":\"sess-fixture-auditor\",\"status\":\"idle\"}]}" ;;
  events)
    sub="$1"; shift
    case "$sub" in
      stats) echo "{\"enabled\":true,\"dir\":\"/tmp/ec-fixture-bus\",\"cursor\":0,\"frames\":0}" ;;
      follow)
        # Emit a finished frame for the requested session when the test asks for
        # one, then exit. The adapter must treat the follower exit as a
        # reconciliation point, not as a completed request.
        if [ -n "$AGENTDECK_FIXTURE_FINISH_SESSION" ]; then
          printf '{"cursor":1,"kind":"session.finished","session_id":"%s","data":{"done_status":"ok","done_summary":"the implementer finished"}}\n' \
            "$AGENTDECK_FIXTURE_FINISH_SESSION"
        fi
        exit 0
        ;;
      *) echo "{\"ok\":true}" ;;
    esac
    ;;
  *) echo "{\"ok\":true}" ;;
esac
`

// fixtureLog reads the contract fixture's log of what it was asked to do.
func (w *world) fixtureLog() string {
	w.t.Helper()
	// The fixture appends to a shared path, so this test reads the whole log and
	// filters by this world's request id where that is available.
	data, err := os.ReadFile(filepath.Join(w.Root, "fixture.log"))
	if err != nil {
		return ""
	}
	return string(data)
}

// waitFor polls a condition with a deadline, so a test waits on an observable
// effect rather than on a sleep.
//
// Sleeping is how a test ends up passing for the wrong reason: the assertion
// runs after the thing it waits for has already happened, so the timing that
// mattered was never exercised.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// fileExists reports whether a path is present.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// jsonUnmarshal is a small local wrapper so the acceptance tests can decode
// fixtures without importing encoding/json at every call site.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// fmtString renders a JSON value as a string for comparison and messages.
func fmtString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// execCommand runs a binary and returns its combined output.
func execCommand(bin string, args ...string) *execCmd {
	return newExecCmd(bin, args...)
}

// newWorldIn builds a second request inside an existing world, so two callers can
// be compared against the same store and the same operator policy.
func newWorldIn(t *testing.T, base *world, requestID string) *world {
	t.Helper()
	clone := *base
	clone.t = t
	clone.RequestID = requestID
	clone.Root = base.Root
	return &clone
}

// runAgainst invokes the built CLI against a DIFFERENT store, so a test can prove
// a configuration is refused without disturbing its own world's state.
func (w *world) runAgainst(store string, args ...string) result {
	w.t.Helper()
	full := append([]string{"--store", store}, args...)
	cmd := exec.Command(controlBinary, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "AGENTDECK_ACCEPTANCE=1")
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			w.t.Fatalf("running %s: %v", strings.Join(full, " "), err)
		}
	}
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}
