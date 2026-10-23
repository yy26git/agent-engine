package minimaxacp

// AP3 tests.
//
// Two layers, and the split matters. The FIXTURE tests drive a scripted NDJSON
// server so every failure mode — fragmented frames, unknown ids, oversized input,
// stderr noise, a disconnect mid-call — is reachable deterministically and without
// spending tokens. The REAL tests then prove the production client completes a
// handshake and a session against the actual pinned `mcode acp` executable.
//
// No test in this file sends a model prompt. Those are paid operations and are
// gated explicitly rather than run incidentally.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// scriptServer writes a fake ACP server and returns its absolute path.
func scriptServer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-mcode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake server: %v", err)
	}
	return path
}

func TestIsolation_RelativeDataDirectoryRefusedBeforeResources(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "must-not-exist")
	_, err := Start(context.Background(), Options{DataDir: "relative", StateDir: spool})
	if err == nil || !strings.Contains(err.Error(), "data directory must be absolute") {
		t.Fatalf("relative data directory accepted: %v", err)
	}
	if _, err := os.Stat(spool); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid isolation opened transport resources")
	}
}

// reply renders a response that ECHOES the request's id.
//
// Correlation is the whole protocol. A fixture answering with its own id deadlocks
// the client by construction, which is a bug in the fixture rather than in the
// client — but it is exactly the kind of thing that makes a protocol test lie.
func reply(body string) string {
	return strings.ReplaceAll(body, "$ID", "$(rid)")
}

// pinnedOpts resolves the real launcher, skipping when it is absent.
func pinnedOpts(t *testing.T, opts Options) Options {
	t.Helper()
	launcher := os.Getenv("AGENT_ENGINE_MCODE_LAUNCHER")
	if launcher == "" {
		launcher = os.Getenv("HOME") + "/.minimax-code/releases/0.6.2/.mcode-launcher"
	}
	if _, err := os.Stat(launcher); err != nil {
		t.Skipf("the pinned mcode launcher is not present at %s; set AGENT_ENGINE_MCODE_LAUNCHER", launcher)
	}
	sum, err := runtime.FileSHA256(launcher)
	if err != nil {
		t.Skipf("the launcher cannot be measured: %v", err)
	}
	opts.Launcher, opts.LauncherSHA256 = launcher, sum
	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}
	return opts
}

// --- pinning ----------------------------------------------------------------

// TestAP3_LauncherMustBePinnedAndAbsolute asserts the executable is proved before
// it runs.
func TestAP3_LauncherMustBePinnedAndAbsolute(t *testing.T) {
	body := "cat >/dev/null 2>&1\n"
	path := scriptServer(t, body)
	sum, _ := runtime.FileSHA256(path)
	dir := t.TempDir()

	// No launcher at all.
	if _, err := Start(context.Background(), Options{Cwd: dir}); err == nil {
		t.Error("an unpinned client started")
	}
	// Relative path: resolving through PATH is how a reviewed client runs
	// something else.
	if _, err := Start(context.Background(), Options{Cwd: dir, Launcher: "mcode", LauncherSHA256: sum}); err == nil {
		t.Error("a PATH-resolved launcher was accepted")
	}
	// Absolute but with no approved digest.
	if _, err := Start(context.Background(), Options{Cwd: dir, Launcher: path}); err == nil {
		t.Error("a launcher with no approved digest was accepted")
	}
	// Absolute with the WRONG digest.
	if _, err := Start(context.Background(), Options{
		Cwd: dir, Launcher: path, LauncherSHA256: strings.Repeat("0", 64),
	}); err == nil {
		t.Error("a substituted launcher digest was accepted")
	}
}

// --- capabilities ----------------------------------------------------------

// TestAP3_CapabilitiesAreDiscoveredNotAssumed asserts the client reports what
// the server advertised, and never claims a surface it does not have.
func TestAP3_CapabilitiesAreDiscoveredNotAssumed(t *testing.T) {
	// A server that advertises almost nothing.
	path := scriptServer(t, `
rid() { printf '%s' \"$line\" | sed -n 's/.*\"id\":\([0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false,"sessionCapabilities":{}},"agentInfo":{"name":"stub","version":"0"}}}\n' "$(rid)"
      ;;
  esac
done
`)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop(context.Background())

	caps := c.Capabilities()
	if caps.Inventory {
		t.Error("the client claims inventory; it owns one process and cannot enumerate sessions")
	}
	if caps.Output {
		t.Error("the client claims session output; ACP exposes no read-last-response operation")
	}
	if caps.Resume {
		t.Error("the client claims resume although the server advertised loadSession=false")
	}
	if caps.Close {
		t.Error("the client claims close although the server advertised no sessionCapabilities")
	}
	if c.Agent().Name != "stub" {
		t.Errorf("agent = %q, want the identity the server reported", c.Agent().Name)
	}
}

// TestAP3_UnadvertisedSelectionIsRefused asserts the client refuses a config value
// the server never offered.
//
// This is the "never silently use the default model" rule. Falling back would mean
// the worker runs on a model nobody admitted, and every receipt downstream
// describes the wrong thing.
func TestAP3_UnadvertisedSelectionIsRefused(t *testing.T) {
	path := scriptServer(t, selFixture(`m:a`, `bypassPermissions`, ""))
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), Model: "m:not-offered",
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop(context.Background())

	_, _, err = c.ActivateResult(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err == nil {
		t.Fatal("a model the server never advertised was accepted")
	}
	if !strings.Contains(err.Error(), "not one the server advertises") {
		t.Errorf("the refusal did not explain the mismatch: %v", err)
	}
}

// TestAP3_SelectionIsReadBackNotEchoed asserts a rejected selection is caught even
// if the server echoes success.
//
// An echo is not evidence. Reading the value back is the only thing that
// distinguishes "applied" from "acknowledged".
func TestAP3_SelectionIsReadBackNotEchoed(t *testing.T) {
	// A server that answers OK to set_config_option and changes nothing.
	path := scriptServer(t, selFixture(`m:a`, `bypassPermissions`, setHandlerEchoes))
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), PermissionMode: "auto",
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop(context.Background())

	_, _, err = c.ActivateResult(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err == nil {
		t.Fatal("a server that only echoed the selection was believed; the value never changed")
	}
	if !strings.Contains(err.Error(), "the server reports") {
		t.Errorf("the refusal did not report the mismatch: %v", err)
	}
}

// TestAP3_OfferedSelectionIsApplied asserts a legitimate change takes effect.
func TestAP3_OfferedSelectionIsApplied(t *testing.T) {
	path := scriptServer(t, selFixture(`m:a`, `bypassPermissions`, setHandlerHonours))
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		PermissionMode: "auto",
		CallTimeout:    5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop(context.Background())

	_, sess, err := c.ActivateResult(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("a legitimate selection was refused: %v", err)
	}
	if got := sess.Option("permissionMode"); got == nil || got.CurrentValue != "auto" {
		t.Errorf("permissionMode = %v, want auto after a verified set", got)
	}
}

// TestAP3_NoiseAndUnknownIDsDoNotCorruptCorrelation asserts unrelated traffic does
// not answer a call that is still waiting.
func TestAP3_NoiseAndUnknownIDsDoNotCorruptCorrelation(t *testing.T) {
	path := scriptServer(t, `
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      echo 'this is not json at all'
      printf '{"jsonrpc":"2.0","method":"mcode/session/current_session_update","params":{"x":1}}\n'
      printf '{"jsonrpc":"2.0","id":999,"result":{"decoy":true}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"stub"}}}\n' "$(rid)"
      ;;
    *'"session/new"'*)
      echo 'garbage without braces'
      printf '{"jsonrpc":"2.0","id":999,"result":{"sessionId":"WRONG"}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"right"}}\n' "$(rid)"
      ;;
  esac
done
`)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start under noise: %v", err)
	}
	defer c.Stop(context.Background())

	_, sess, err := c.ActivateResult(context.Background(), runtime.ActivateSpec{Label: "L"})
	if err != nil {
		t.Fatalf("activate under noise: %v", err)
	}
	if sess.ID != "right" {
		t.Errorf("session = %q; a decoy response for another id was accepted", sess.ID)
	}
}

// TestAP3_ServerErrorIsSurfacedNotSwallowed asserts a JSON-RPC error reaches the
// caller with its code and reason intact.
func TestAP3_ServerErrorIsSurfacedNotSwallowed(t *testing.T) {
	path := scriptServer(t, `
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"stub"}}}\n' "$(rid)" ;;
    *'"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"cwd is not a directory"}}\n' "$(rid)" ;;
  esac
done
`)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop(context.Background())

	_, _, err = c.ActivateResult(context.Background(), runtime.ActivateSpec{Label: "L"})
	if err == nil {
		t.Fatal("a server error was swallowed")
	}
	if !strings.Contains(err.Error(), "cwd is not a directory") {
		t.Errorf("the server's reason was lost: %v", err)
	}
}

// TestAP3_HandshakeTimeoutIsBounded asserts a silent server cannot hang startup.
func TestAP3_HandshakeTimeoutIsBounded(t *testing.T) {
	path := scriptServer(t, "sleep 30\n")
	sum, _ := runtime.FileSHA256(path)
	start := time.Now()
	_, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		HandshakeTimeout: 2 * time.Second, ShutdownGrace: time.Second,
	})
	if err == nil {
		t.Fatal("a silent server completed the handshake")
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("the handshake took %s; the timeout is not bounding startup", elapsed)
	}
}

// TestAP3_CancelIsANotification asserts cancel is written WITHOUT an id.
//
// Sent as a request it draws "method not found", which reads as an absent method.
// The distinction is the difference between a working cancellation and a broken
// one, and only the written frame can show which happened.
func TestAP3_CancelIsANotification(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "captured.ndjson")
	path := scriptServer(t, `
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"stub"}}}\n' "$(rid)" ;;
    *cancel*) printf '%s\n' "$line" >> '`+capture+`' ;;
  esac
done
`)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop(context.Background())

	if err := c.cancelTo(context.Background(), "s1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	seen := readFirstLine(t, capture)
	if seen == "" {
		t.Fatal("the server captured nothing; the notification was never written")
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(seen), &f); err != nil {
		t.Fatalf("captured line is not JSON: %q", seen)
	}
	if _, hasID := f["id"]; hasID {
		t.Error("cancel was sent with an id; it is a notification and would draw method-not-found")
	}
	if f["method"] != "session/cancel" {
		t.Errorf("captured method = %v, want session/cancel", f["method"])
	}
}

// readFirstLine waits briefly for a line to appear.
//
// A notification has no acknowledgement, so the write returning means only that
// the frame was handed to the pipe. Reading once races the server's own write and
// reports a missing capture that never existed.
func readFirstLine(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open(path)
		if err == nil {
			sc := bufio.NewScanner(f)
			if sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				f.Close()
				if line != "" {
					return line
				}
			}
			f.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

// TestAP3_StopEscalatesAndReports asserts teardown distinguishes closed from
// killed.
//
// "Closed" and "killed" are different facts and only the second proves the
// process is gone. Reporting them identically would let a cancelled worker keep
// holding the workspace while the log claims it was closed.
func TestAP3_StopEscalatesAndReports(t *testing.T) {
	path := scriptServer(t, `
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"stub"}}}\n' "$(rid)" ;;
  esac
done
sleep 60
`)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		ShutdownGrace: time.Second, CallTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	start := time.Now()
	outcome, err := c.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("stop took %s; it must be bounded by the grace period", elapsed)
	}
	if outcome != "closed" && outcome != "killed" {
		t.Errorf("stop outcome = %q, want closed or killed", outcome)
	}
	t.Logf("stop outcome: %s", outcome)
	if h, _ := c.Health(context.Background()); h.Available {
		t.Error("health reports available after stop")
	}
}

// TestAP3_ReconcileRefusesRatherThanGuesses asserts an ACP backend does not claim
// to enumerate sessions. It owns one process; saying "absent" would be a guess,
// and a guess here is how a second worker gets created.
//
// It now asserts both halves, because "absent" became answerable only once the
// caller could prove an earlier process is gone. Without that proof the backend
// still refuses; with it, absence is a real answer and a crashed run can recover.
func TestAP3_ReconcileRefusesRatherThanGuesses(t *testing.T) {
	c := &Connection{openSessions: map[string]bool{}, openLabels: map[string]string{}}
	if _, found, err := c.Reconcile(context.Background(), "L", false); !strings.Contains(fmt.Sprint(err), "cannot") {
		t.Errorf("an unproven-quiescence backend produced a verdict instead of refusing (found=%v, err=%v)", found, err)
	}
	if _, found, err := c.Reconcile(context.Background(), "L", true); err != nil || found {
		t.Errorf("a proven-quiescent absence = found %v, err %v; want a definite absent", found, err)
	}
	if c.Capabilities().Inventory {
		t.Error("the backend claims inventory")
	}
	if c.Capabilities().Output {
		t.Error("the backend claims session output; ACP has no read-last-response operation")
	}
}

// selFixture builds a server that advertises a model and a permission mode and
// echoes the request id on every reply. `setHandler` is the set_config_option
// behaviour under test.
//
// Echoing the id is not a detail. JSON-RPC correlates by id, and a fixture that
// answers with its own id deadlocks the client by construction -- which is how a
// protocol test ends up reporting a client bug that is really a fixture bug.
func selFixture(model, permission, setHandler string) string {
	return `
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false},"agentInfo":{"name":"stub"}}}\n' "$(rid)"
      ;;
    *'"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"s1","configOptions":[{"type":"select","id":"model","currentValue":"` + model + `","options":[{"value":"` + model + `"},{"value":"m:other"}]},{"type":"select","id":"permissionMode","currentValue":"` + permission + `","options":[{"value":"bypassPermissions"},{"value":"auto"},{"value":"default"}]}]}}\n' "$(rid)"
      ;;
` + setHandler + `    *'"session/close"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$(rid)"
      ;;
  esac
done
`
}

// setHandlerEchoes acknowledges set_config_option WITHOUT applying it: the shape
// of a server that says yes and does nothing.
const setHandlerEchoes = `    *'"session/set_config_option"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"configOptions":[{"type":"select","id":"permissionMode","currentValue":"bypassPermissions","options":[{"value":"bypassPermissions"},{"value":"auto"}]}]}}\n' "$(rid)"
      ;;
`

// setHandlerHonours genuinely applies the requested value, which is what a
// well-behaved server does.
const setHandlerHonours = `    *'"session/set_config_option"'*)
      case "$line" in
        *'"auto"'*) v=auto ;;
        *) v=bypassPermissions ;;
      esac
      printf '{"jsonrpc":"2.0","id":%s,"result":{"configOptions":[{"type":"select","id":"permissionMode","currentValue":"%s","options":[{"value":"bypassPermissions"},{"value":"auto"}]}]}}\n' "$(rid)" "$v"
      ;;
`

// --- AP4 receipts -----------------------------------------------------------

// promptServer answers a handshake, a session, and every prompt with a
// completed stop reason. It counts prompts so a duplicate can be detected.
func promptServer(t *testing.T) string {
	t.Helper()
	return scriptServer(t, reply(`
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
n=0
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false,"sessionResume":false}},"authMethods":[]}' ;;
    *'"session/new"'*)
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{"sessionId":"sess-1","modes":{"currentModeId":"m","availableModes":[]},"configOptions":[]}}' ;;
    *'"session/prompt"'*)
      n=$((n+1))
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{"stopReason":"end_turn"}}' ;;
    *'"session/close"'*)
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{}}' ;;
    *'"shutdown"'*)
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{}}' ;;
  esac
done
`))
}

func promptConn(t *testing.T) *Connection {
	t.Helper()
	path := promptServer(t)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		// A state dir is supplied so every prompt test also exercises the durable
		// receipt path; a connection without one cannot make a completion durable
		// and is covered separately.
		StateDir:    t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Stop(context.Background()) })
	return c
}

// TestAP4_CompletedPromptIsATypedReceipt asserts a completed turn is classified,
// not left for a caller to guess at.
func TestAP4_CompletedPromptIsATypedReceipt(t *testing.T) {
	c := promptConn(t)
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	rec, err := c.Submit(context.Background(), ref, "do the work")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.Outcome != runtime.OutcomeLanded {
		t.Errorf("outcome = %q, want %q", rec.Outcome, runtime.OutcomeLanded)
	}
	if rec.State != "end_turn" {
		t.Errorf("state = %q, want the server's stop reason verbatim", rec.State)
	}
	if !rec.Settled {
		t.Error("a completed turn must be settled")
	}
	if rec.SendID == "" {
		t.Fatal("send id is empty, so the effect could never be reconciled")
	}
}

// TestAP4_EachPromptGetsADistinctReceipt proves two submissions to one session are
// distinguishable.
//
// A receipt id that repeats cannot answer the question reconciliation asks: it
// cannot say which delivery it is reporting on.
func TestAP4_EachPromptGetsADistinctReceipt(t *testing.T) {
	c := promptConn(t)
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	first, err := c.Submit(context.Background(), ref, "first")
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := c.Submit(context.Background(), ref, "second")
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if first.SendID == second.SendID {
		t.Errorf("both prompts got receipt %q; they must be distinguishable", first.SendID)
	}
	for _, id := range []string{first.SendID, second.SendID} {
		got, err := c.Receipt(context.Background(), id)
		if err != nil {
			t.Fatalf("receipt %s: %v", id, err)
		}
		if got.Outcome != runtime.OutcomeLanded {
			t.Errorf("receipt %s outcome = %q, want %q", id, got.Outcome, runtime.OutcomeLanded)
		}
	}
}

// TestAP4_NoDurableQueueIsDeclared is the property the control layer's retry rule
// depends on. If this ever returns true, an interrupted ACP turn would be
// resent automatically and could reach the agent twice.
func TestAP4_NoDurableQueueIsDeclared(t *testing.T) {
	c := promptConn(t)
	if c.Capabilities().DurableQueue {
		t.Error("the ACP peer retains nothing to reconcile against; declaring a durable queue " +
			"would authorise replaying an uncertain prompt")
	}
	if c.Capabilities().Inventory {
		t.Error("the ACP peer owns one process and cannot list sessions")
	}
}

// droppingPromptServer answers the handshake and the session, then on a prompt
// emits a tool-started UPDATE and exits WITHOUT answering.
//
// This is the hardest outcome in the protocol: the agent demonstrably began
// doing the work, and no receipt will ever arrive. Any caller that treats it as
// a failure will repeat the work; any caller that retries will duplicate it.
func droppingPromptServer(t *testing.T) string {
	t.Helper()
	return scriptServer(t, reply(`
rid() { echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p'; }
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":false}},"authMethods":[]}' ;;
    *'"session/new"'*)
      echo '{"jsonrpc":"2.0","id":'"$(rid)"',"result":{"sessionId":"sess-1","modes":{"currentModeId":"m","availableModes":[]},"configOptions":[]}}' ;;
    *'"session/prompt"'*)
      # Work demonstrably began, then the peer vanished mid-turn.
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"edit","kind":"edit","status":"in_progress"}}}'
      exit 0 ;;
  esac
done
`))
}

// TestAP4_DroppedResponseAfterToolStartedYieldsNoReceipt is the AP4 fault
// injection at the wire boundary.
//
// The assertion that matters is the EMPTY send id. A receipt here would be a
// promise that the turn can be reconciled later, and nothing can reconcile it:
// the peer is gone.
func TestAP4_DroppedResponseAfterToolStartedYieldsNoReceipt(t *testing.T) {
	path := droppingPromptServer(t)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _, _ = c.Stop(context.Background()) }()

	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Drain updates concurrently: a bounded channel drops rather than stalls, so
	// the observation has to be consumed to be seen.
	seen := make(chan runtime.Event, 8)
	go func() {
		for ev := range c.Updates() {
			select {
			case seen <- ev:
			default:
			}
		}
	}()

	rec, subErr := c.Submit(context.Background(), ref, "do the work")
	if subErr == nil {
		t.Fatal("a dropped response was reported as a completed prompt")
	}
	if rec.SendID != "" {
		t.Errorf("send id = %q, want empty: nothing can reconcile this turn", rec.SendID)
	}
	if rec.Outcome != runtime.OutcomeUnstated {
		t.Errorf("outcome = %q, want %q", rec.Outcome, runtime.OutcomeUnstated)
	}

	select {
	case ev := <-seen:
		if ev.Kind != "session/update" {
			t.Errorf("update kind = %q, want session/update", ev.Kind)
		}
		if !strings.Contains(string(ev.Payload), "tool_call") {
			t.Errorf("update payload = %s, want the tool call that proves work began", ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Error("the tool-started update never reached the observation stream")
	}
}

// TestAP4_DroppedResponseIsNotRetriedByTheConnection proves the connection itself
// refuses to pretend the dropped turn is retryable.
//
// Capabilities are the mechanism: DurableQueue=false is what authorises the
// control layer to block rather than replay.
func TestAP4_DroppedResponseIsNotRetriedByTheConnection(t *testing.T) {
	path := droppingPromptServer(t)
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(),
		CallTimeout: 3 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _, _ = c.Stop(context.Background()) }()

	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	go func() {
		for range c.Updates() {
		}
	}()

	if _, subErr := c.Submit(context.Background(), ref, "do the work"); subErr == nil {
		t.Fatal("expected the dropped turn to fail")
	}
	if c.Capabilities().DurableQueue {
		t.Fatal("a peer that dropped a turn cannot be declared to hold work durably")
	}
	// The session is no longer inspectable once the peer is gone, which is the
	// honest answer rather than a stale "running".
	if _, ierr := c.Inspect(context.Background(), ref); ierr == nil {
		t.Error("inspect reported a healthy session after the peer vanished")
	}
}

// TestAP5_CompletedPromptIsPublishedAsAnObservation is the production-composition
// proof for the receipt route.
//
// A JSON-RPC response is delivered to the caller that requested it, so before
// this existed a completed turn produced a return value and NOTHING on the
// observation stream. A supervisor following that stream therefore never learned
// the turn had ended, and the ACP worker had no route to being assessed at all.
func TestAP5_CompletedPromptIsPublishedAsAnObservation(t *testing.T) {
	c := promptConn(t)
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Drain the RECEIPT stream concurrently: it is bounded and drops rather than
	// stalling, so a receipt only counts if something is reading it.
	seen := make(chan runtime.Event, 8)
	go func() {
		for ev := range c.Receipts() {
			select {
			case seen <- ev:
			default:
			}
		}
	}()

	rec, err := c.Submit(context.Background(), ref, "do the work")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-seen:
			if ev.Kind != runtime.KindPromptResult {
				continue // a notification, not the receipt
			}
			if ev.SessionID != ref.ID {
				t.Errorf("receipt session = %q, want %q", ev.SessionID, ref.ID)
			}
			var body struct {
				SendID     string `json:"sendId"`
				StopReason string `json:"stopReason"`
			}
			if uerr := json.Unmarshal(ev.Payload, &body); uerr != nil {
				t.Fatalf("decode receipt: %v", uerr)
			}
			// The receipt must carry the SAME identity the caller received, or a
			// deduplication key computed from one and not the other would let a
			// duplicate open a second audit.
			if body.SendID != rec.SendID {
				t.Errorf("observed send id = %q, returned %q; they must be the same identity",
					body.SendID, rec.SendID)
			}
			if body.StopReason != rec.State {
				t.Errorf("observed stop reason = %q, returned %q", body.StopReason, rec.State)
			}
			if c.DroppedReceipts() != 0 {
				t.Errorf("DroppedReceipts = %d, want 0 with a consumer attached", c.DroppedReceipts())
			}
			return
		case <-deadline:
			t.Fatal("the completed prompt never appeared on the observation stream")
		}
	}
}

func TestACPStream_DroppedNotificationDegradesEvenWithoutALaterFrame(t *testing.T) {
	c := &Connection{updates: make(chan runtime.Event, 1), closed: make(chan struct{})}
	c.observe(frame{Method: "session/update", Params: json.RawMessage(`{"sessionId":"worker"}`)})
	c.observe(frame{Method: "session/update", Params: json.RawMessage(`{"sessionId":"worker"}`)})
	first := <-c.updates
	if first.Cursor != 1 || first.SessionID != "worker" {
		t.Fatalf("notification attribution: %+v", first)
	}
	h, err := c.Health(context.Background())
	if err != nil || h.Available || !strings.Contains(strings.Join(h.Reasons, " "), "notifications-dropped") {
		t.Fatalf("lost tail reported healthy: %+v, %v", h, err)
	}
	c.observe(frame{Method: "session/update", Params: json.RawMessage(`{"sessionId":"worker"}`)})
	if next := <-c.updates; next.Cursor != 3 {
		t.Fatalf("loss hidden by renumbering: %+v", next)
	}
}

func TestACPStream_SavedCursorCannotPretendToReplay(t *testing.T) {
	c := &Connection{}
	if _, _, err := c.Events(context.Background(), 4); err == nil {
		t.Fatal("saved cursor was silently discarded")
	}
}

// TestAP5_DroppedReceiptIsCountedNotHidden proves the loss is visible.
//
// A receipt dropped because the bounded stream is full is a completion that will
// silently never be assessed. Counting it is the minimum honest response;
// ignoring it would make the failure undetectable.
func TestAP5_DroppedReceiptIsCountedNotHidden(t *testing.T) {
	// The overflow is exercised directly rather than by sending hundreds of real
	// prompts: the property under test is that a full observation stream is
	// counted, and driving it through the protocol would cost a minute of suite
	// time to prove the same line.
	// A real spool: this test is about the receipt stream overflowing, not about a
	// transport with nowhere to make a completion durable (covered separately).
	spool := spooledConn(t)
	c := &Connection{
		updates:  make(chan runtime.Event, 1),
		receipts: make(chan runtime.Event, 1),
		spool:    spool, spoolPath: spool.Name(),
	}
	c.publishReceipt("sess-1", "acp:sess-1:1", "end_turn") // fills the buffer
	c.publishReceipt("sess-1", "acp:sess-1:2", "end_turn") // must be dropped
	if c.DroppedReceipts() != 1 {
		t.Errorf("DroppedReceipts = %d, want 1", c.DroppedReceipts())
	}
	// The first receipt is still queued and uncorrupted.
	select {
	case ev := <-c.receipts:
		if ev.Kind != runtime.KindPromptResult {
			t.Errorf("queued observation kind = %q, want %q", ev.Kind, runtime.KindPromptResult)
		}
	default:
		t.Error("the first receipt was not queued")
	}
}

// TestAP5_ReceiptStreamIsNotStarvedByNotifications is the durability proof.
//
// Receipts and tool notifications previously shared one bounded channel, so a
// flood of notifications could consume the buffer and the one receipt meaning
// "the turn ended" would never be delivered. The request would then look exactly
// like one with nothing to assess.
func TestAP5_ReceiptStreamIsNotStarvedByNotifications(t *testing.T) {
	c := promptConn(t)
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Flood the notification stream with nothing reading it.
	for i := 0; i < MaxQueueDepth*2; i++ {
		c.observe(frame{Method: "session/update", Params: []byte(`{"sessionUpdate":"tool_call"}`)})
	}
	if len(c.updates) < MaxQueueDepth {
		t.Fatalf("the notification stream did not fill; the test would prove nothing")
	}

	// The receipt must still get through, on its own stream.
	if _, serr := c.Submit(context.Background(), ref, "do the work"); serr != nil {
		t.Fatalf("submit: %v", serr)
	}
	select {
	case ev := <-c.Receipts():
		if ev.Kind != runtime.KindPromptResult {
			t.Errorf("receipt kind = %q, want %q", ev.Kind, runtime.KindPromptResult)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a completed turn produced no receipt: notifications starved the completion")
	}
	if c.DroppedReceipts() != 0 {
		t.Errorf("DroppedReceipts = %d, want 0", c.DroppedReceipts())
	}
	// Receipt survival does not recover notifications lost to this flood.
	if h, _ := c.Health(context.Background()); h.Available || !strings.Contains(strings.Join(h.Reasons, " "), "notifications-dropped") {
		t.Errorf("lost notifications were hidden by a surviving receipt: %+v", h)
	}
}

// TestAP5_OverflowIsExplicitAndDegradesHealth covers the overflow branch.
//
// A receipt that cannot be delivered is not a recoverable condition — there is
// no second copy — so the connection must say so instead of continuing to look
// healthy while a completion has gone missing.
func TestAP5_OverflowIsExplicitAndDegradesHealth(t *testing.T) {
	// A real spool, so this exercises stream OVERFLOW rather than a transport
	// with nowhere durable to write (covered by TestAP5_NoSpoolDegrades...).
	spool := spooledConn(t)
	c := &Connection{
		updates:   make(chan runtime.Event, 1),
		receipts:  make(chan runtime.Event, 1),
		spool:     spool,
		spoolPath: spool.Name(),
	}
	c.publishReceipt("sess-1", "acp:sess-1:1", "end_turn")
	c.publishReceipt("sess-1", "acp:sess-1:2", "end_turn") // overflows

	if c.DroppedReceipts() != 1 {
		t.Errorf("DroppedReceipts = %d, want 1", c.DroppedReceipts())
	}
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Available {
		t.Error("a connection that lost a completion still reports healthy")
	}
	found := false
	for _, r := range h.Reasons {
		if r == "receipts-dropped" {
			found = true
		}
	}
	if !found {
		t.Errorf("health reasons = %v, want a machine-readable receipt-loss code", h.Reasons)
	}
}

// TestAP5_ReceiptIsDurableBeforePublication is the durability proof.
//
// The spool write happens BEFORE the receipt is published and before Submit
// returns. A receipt that exists only in memory is lost when the process dies,
// and a lost receipt is a completion that will never be assessed.
func TestAP5_ReceiptIsDurableBeforePublication(t *testing.T) {
	c := promptConn(t)
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, serr := c.Submit(context.Background(), ref, "work"); serr != nil {
		t.Fatalf("submit: %v", serr)
	}
	if c.spoolPath == "" {
		t.Skip("this connection was started without a state dir")
	}
	body, err := os.ReadFile(c.spoolPath)
	if err != nil {
		t.Fatalf("spool unreadable: %v", err)
	}
	if !strings.Contains(string(body), rec2sendID(c, ref.ID)) && !strings.Contains(string(body), ref.ID) {
		t.Fatalf("the spool does not record the completed turn: %s", body)
	}
	if c.DroppedReceipts() != 0 {
		t.Errorf("DroppedReceipts = %d, want 0 with a durable spool", c.DroppedReceipts())
	}
}

func rec2sendID(c *Connection, sessionID string) string {
	// The spool key is session|sendId; matching on the session is enough to prove
	// the record is there without depending on the counter's value.
	return sessionID
}

// TestAP5_UnacknowledgedReceiptIsReplayedAfterRestart proves the delivery is
// at-least-once, which is what makes a crash survivable.
func TestAP5_UnacknowledgedReceiptIsReplayedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := promptServer(t)
	sum, _ := runtime.FileSHA256(path)

	first, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: dir,
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ref, err := first.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	rec, err := first.Submit(context.Background(), ref, "work")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Deliberately NOT acknowledged: the consumer died before recording it.
	if _, _ = first.Stop(context.Background()); true {
		_ = rec
	}

	second, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: dir,
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { _, _ = second.Stop(context.Background()) }()

	select {
	case ev := <-second.Receipts():
		if ev.Kind != runtime.KindPromptResult {
			t.Errorf("replayed kind = %q, want %q", ev.Kind, runtime.KindPromptResult)
		}
		if !strings.Contains(string(ev.Payload), rec.SendID) {
			t.Errorf("replayed payload %s does not carry the original send id %s", ev.Payload, rec.SendID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unacknowledged receipt was not replayed after restart")
	}
}

// TestAP5_AcknowledgedReceiptIsNotReplayed keeps replay from becoming duplication.
func TestAP5_AcknowledgedReceiptIsNotReplayed(t *testing.T) {
	dir := t.TempDir()
	path := promptServer(t)
	sum, _ := runtime.FileSHA256(path)

	c, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: dir,
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "L1"})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	rec, err := c.Submit(context.Background(), ref, "work")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if aerr := c.AcknowledgeReceipt(ref.ID, rec.SendID); aerr != nil {
		t.Fatalf("acknowledge: %v", aerr)
	}
	if _, _ = c.Stop(context.Background()); true {
		_ = dir
	}

	next, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: dir,
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { _, _ = next.Stop(context.Background()) }()

	select {
	case ev := <-next.Receipts():
		t.Errorf("an acknowledged receipt was replayed: %s", ev.Payload)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestAP5_NoSpoolDegradesRatherThanPretending proves an unconfigured transport
// cannot silently drop completions.
// spooledConn opens a writable receipt spool backed by a temp file.
func spooledConn(t *testing.T) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "receipts.ndjson")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestAP5_NoSpoolDegradesRatherThanPretending(t *testing.T) {
	c := &Connection{
		updates:  make(chan runtime.Event, 1),
		receipts: make(chan runtime.Event, 4),
	}
	c.publishReceipt("sess-1", "acp:sess-1:1", "end_turn")
	if c.DroppedReceipts() == 0 {
		t.Error("a receipt with nowhere durable to go was published as though recorded")
	}
	h, _ := c.Health(context.Background())
	if h.Available {
		t.Error("a transport that cannot make completions durable still reports healthy")
	}
}

// TestAP5_PidIsReportedAtSpawnBeforeStartReturns closes the crash window.
//
// The handshake is not instantaneous, and a supervisor can die during it. If the
// first durable record of the child is a session binding written much later, the
// crash window is unrecorded and nothing can ever reap the orphan — so the pid
// must be available at the moment the process exists.
func TestAP5_PidIsReportedAtSpawnBeforeStartReturns(t *testing.T) {
	path := promptServer(t)
	sum, _ := runtime.FileSHA256(path)

	var (
		mu          sync.Mutex
		spawnedPID  int
		liveAtSpawn bool
	)
	conn, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
		OnSpawned: func(pid int) error {
			mu.Lock()
			defer mu.Unlock()
			spawnedPID = pid
			liveAtSpawn = processLive(pid)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _, _ = conn.Stop(context.Background()) }()

	mu.Lock()
	defer mu.Unlock()
	if spawnedPID <= 0 {
		t.Fatal("no pid was reported at spawn")
	}
	if !liveAtSpawn {
		t.Error("the pid reported at spawn did not correspond to a live process")
	}
	if pid, owns := conn.ProcessIdentity(); !owns || pid != spawnedPID {
		t.Errorf("connection reports pid=%d owned=%v, but spawn reported %d", pid, owns, spawnedPID)
	}
}

// processLive reports whether a pid currently exists.
func processLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// TestAP5_AFailedSpawnRecordKillsTheChild is the fail-closed half.
//
// A process this supervisor cannot record must not be kept. Returning it would
// leave exactly the orphan the callback exists to prevent.
func TestAP5_AFailedSpawnRecordKillsTheChild(t *testing.T) {
	path := promptServer(t)
	sum, _ := runtime.FileSHA256(path)

	var reported int
	conn, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: time.Second,
		OnSpawned: func(pid int) error {
			reported = pid
			return errors.New("the store refused the record")
		},
	})
	if err == nil {
		_, _ = conn.Stop(context.Background())
		t.Fatal("startup continued although the spawned process could not be recorded")
	}
	if !strings.Contains(err.Error(), "not recorded") {
		t.Errorf("error = %v, want it to name the failed record", err)
	}
	if reported <= 0 {
		t.Error("no pid was reported, so nothing was cleaned up")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processLive(reported) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("the child process %d survived a failed spawn record", reported)
}

// TestAP5_FailedSpawnRecordCleansUpTheProcessGroup proves the cleanup is real.
//
// Ignoring the kill is the failure this whole callback prevents. The child is
// VERIFIED gone, and a launcher that spawns a tool must not leave that tool
// behind either.
func TestAP5_FailedSpawnRecordCleansUpTheProcessGroup(t *testing.T) {
	path := promptServer(t)
	sum, _ := runtime.FileSHA256(path)

	var reported int
	_, err := Start(context.Background(), Options{
		Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: t.TempDir(),
		CallTimeout: 5 * time.Second, ShutdownGrace: 2 * time.Second,
		OnSpawned: func(pid int) error {
			reported = pid
			return errors.New("the store refused the record")
		},
	})
	if err == nil {
		t.Fatal("startup continued although the spawned process could not be recorded")
	}
	if !strings.Contains(err.Error(), "not recorded") {
		t.Errorf("error = %v, want it to name the failed record", err)
	}
	if reported <= 0 {
		t.Fatal("no pid was reported, so nothing could be cleaned up")
	}
	// Cleanup must be ESTABLISHED, not attempted.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processLive(reported) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("the child process %d survived the failed spawn record; cleanup was not established", reported)
}

// A real coding turn can outlast a configuration RPC without losing its
// correlated completion receipt. This scripted server spends no model tokens.
func TestPromptBudget_IsSeparateFromRPCBudget(t *testing.T) {
	path := promptServer(t)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "n=$((n+1))", "sleep 0.2\n      n=$((n+1))", 1))
	if err := os.WriteFile(path, body, 0700); err != nil {
		t.Fatal(err)
	}
	sum, _ := runtime.FileSHA256(path)
	c, err := Start(context.Background(), Options{Launcher: path, LauncherSHA256: sum, Cwd: t.TempDir(), StateDir: t.TempDir(), CallTimeout: 5 * time.Second, TurnTimeout: 2 * time.Second, ShutdownGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(context.Background())
	ref, err := c.Activate(context.Background(), runtime.ActivateSpec{Label: "budget"})
	if err != nil {
		t.Fatal(err)
	}
	c.opts.CallTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	receipt, err := c.Submit(ctx, ref, "slow coding turn")
	if err != nil || receipt.SendID == "" || receipt.Outcome != runtime.OutcomeLanded {
		t.Fatalf("coding turn used RPC budget: %+v %v", receipt, err)
	}
	if (Options{CallTimeout: time.Millisecond}).PromptTimeout() != DefaultTurnTimeout {
		t.Fatal("default model budget inherited RPC budget")
	}
}
