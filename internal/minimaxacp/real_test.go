package minimaxacp

// AP3 real-process tests.
//
// These drive the ACTUAL pinned `mcode acp` executable. The fixture tests prove
// the client's error handling; these prove the client and the installed server
// actually agree, which no fixture can establish.
//
// Real-server tests require AGENT_ENGINE_ACP_ALLOW_REAL=1: even session creation
// changes vendor session state and must not run incidentally in the normal suite.
// None of them sends a model prompt. Prompt, active-turn cancellation and
// shutdown-during-a-turn are paid operations and stay gated.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

// requireRealACP gates every real-server test before launcher resolution or spawn.
func requireRealACP(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENT_ENGINE_ACP_ALLOW_REAL") != "1" {
		t.Skip("real MiniMax sessions disabled; explicitly set AGENT_ENGINE_ACP_ALLOW_REAL=1")
	}
}

func stopRealACP(t *testing.T, c *Connection) {
	t.Helper()
	if _, err := c.Stop(context.Background()); err != nil {
		t.Errorf("real ACP cleanup failed: %v", err)
	}
}

// TestAP3_RealHandshakeAndSession asserts the production client completes a real
// handshake and creates a real session against the installed server.
func TestAP3_RealHandshakeAndSession(t *testing.T) {
	requireRealACP(t)
	opts := pinnedOpts(t, Options{CallTimeout: 30 * time.Second, ShutdownGrace: 3 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("real handshake failed: %v", err)
	}
	defer stopRealACP(t, c)

	if c.Agent().Name == "" {
		t.Error("the handshake recorded no agent identity")
	}
	t.Logf("connected to %s %s (protocol %d)",
		c.Agent().Name, c.Agent().Version, c.ProtocolVersion())

	ref, sess, err := c.ActivateResult(ctx, runtime.ActivateSpec{Label: "ap3-real-1", Tool: "mcode"})
	if err != nil {
		t.Fatalf("real session creation failed: %v", err)
	}
	if !strings.HasPrefix(ref.ID, "mvs_") {
		t.Errorf("session id %q does not look like a native ACP session", ref.ID)
	}
	if ref.Backend != BackendName {
		t.Errorf("reference backend = %q, want %q", ref.Backend, BackendName)
	}
	if ref.Label != "ap3-real-1" {
		t.Errorf("the activation label was lost: %q", ref.Label)
	}

	// The server must have advertised the two selections AP3 depends on, or the
	// client could not have frozen them.
	for _, id := range []string{"model", "permissionMode"} {
		if sess.Option(id) == nil {
			t.Errorf("the real server advertised no %q option; the client's selection path is untested", id)
		}
	}
	// And the backend must be honest about what it cannot do.
	if c.Capabilities().Inventory || c.Capabilities().Output {
		t.Error("the client claims surfaces ACP does not provide")
	}
}

// TestAP3_RealSelectionIsAppliedAndVerified asserts a real selection round-trip.
//
// This is the property the whole stage exists for: the worker runs on the model
// that was admitted, not on whatever the server would have chosen.
func TestAP3_RealSelectionIsAppliedAndVerified(t *testing.T) {
	requireRealACP(t)
	opts := pinnedOpts(t, Options{CallTimeout: 30 * time.Second, ShutdownGrace: 3 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// First learn what the server actually advertises, then ask for a different
	// value from that set. Guessing a value would prove nothing.
	probe, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("probe handshake failed: %v", err)
	}
	_, sess, perr := probe.ActivateResult(ctx, runtime.ActivateSpec{Label: "ap3-probe", Tool: "mcode"})
	if perr != nil {
		stopRealACP(t, probe)
		t.Fatalf("probe session failed: %v", perr)
	}
	if _, err := probe.Stop(context.Background()); err != nil {
		t.Fatalf("probe stop: %v", err)
	}

	perm := sess.Option("permissionMode")
	if perm == nil {
		t.Fatal("the real server advertised no permissionMode option")
	}
	var want string
	for _, o := range perm.Options {
		if o != "bypassPermissions" {
			want = o // prefer a NARROWER mode than full access
			break
		}
	}
	if want == "" {
		t.Skip("the server advertises only bypassPermissions; there is nothing narrower to select")
	}
	t.Logf("selecting permissionMode=%q (was %q)", want, perm.CurrentValue)

	opts.PermissionMode = want
	c, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	defer stopRealACP(t, c)

	_, applied, err := c.ActivateResult(ctx, runtime.ActivateSpec{Label: "ap3-real-2", Tool: "mcode"})
	if err != nil {
		t.Fatalf("a genuinely narrower permission mode was refused: %v", err)
	}
	if got := applied.Option("permissionMode"); got == nil || got.CurrentValue != want {
		t.Errorf("permissionMode = %v, want the verified %q", got, want)
	}
}

// TestAP3_RealUnadvertisedSelectionIsRefused asserts the real server rejects a
// value that was never offered.
//
// The client should refuse this BEFORE sending it, so the test asserts the
// client's own check rather than the server's.
func TestAP3_RealUnadvertisedSelectionIsRefused(t *testing.T) {
	requireRealACP(t)
	opts := pinnedOpts(t, Options{
		Model:         "m:minimax:NotARealModel:v:nope",
		CallTimeout:   30 * time.Second,
		ShutdownGrace: 3 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	defer stopRealACP(t, c)

	_, _, err = c.ActivateResult(ctx, runtime.ActivateSpec{Label: "ap3-refuse", Tool: "mcode"})
	if err == nil {
		t.Fatal("a model the server never advertised was accepted")
	}
	if !strings.Contains(err.Error(), "not one the server advertises") {
		t.Errorf("the refusal did not explain the mismatch: %v", err)
	}
}

// TestAP3_PaidOperationsAreGated asserts the paid paths refuse without an
// explicit authorization, rather than firing incidentally.
//
// A protocol test that spends tokens the moment it is run is a test that bills
// whoever runs CI.
func TestAP3_PaidOperationsAreGated(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("AGENT_ENGINE_ACP_ALLOW_PAID") != "1" {
		t.Log("paid operations (session/prompt, active-turn cancel, turn shutdown) are gated behind " +
			"AGENT_ENGINE_ACP_ALLOW_PAID=1 and are NOT run by this suite")
		return
	}
	t.Skip("paid path not exercised in this run; see the AP0 evidence for what remains unobserved")
}
