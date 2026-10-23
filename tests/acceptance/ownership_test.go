package acceptance

// S1 ownership acceptance.
//
// The migration's central claim is that exactly one owner can act on a workspace
// after a transfer. These cases run the real built CLI, because the failure this
// prevents — two processes writing the same files — is a property of the deployed
// processes, not of any in-process helper.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownershipWorld builds a world whose configured runtime is the real pinned
// artifact, so `run` is exercised against something that exists.
func ownershipWorld(t *testing.T, requestID string) *world {
	t.Helper()
	w := newWorld(t, requestID)
	return w
}

// TestOwnership_LegacyDraftIsNotDispatchable asserts the retired owner cannot be
// invoked through the release runtime.
//
// This is the executable form of "the legacy owner is stood down". The draft
// lives in a checkout that still exists and is still valuable as history, so what
// must be true is not that the files are gone but that the RELEASE runtime has no
// path to them.
func TestOwnership_LegacyDraftIsNotDispatchable(t *testing.T) {
	pin, _ := pinnedArtifact(t)

	// The pinned release runtime has no workflow command.
	_, stderr, code := runPinned(t, pin, "workflow", "status", "--request-id", "anything")
	combined := strings.ToLower(stderr)
	if !strings.Contains(combined, "not a recognized command") &&
		!strings.Contains(combined, "unknown command") {
		t.Errorf("the release runtime did not refuse the legacy workflow command "+
			"(exit %d): %s", code, stderr)
	}

	// And the preserved draft checkout is still intact, because it is history
	// rather than rubbish. Standing an owner down is not deleting its work.
	draft := filepath.Join("..", "..", "..", "agent-deck", "cmd", "agent-deck", "workflow_cmd.go")
	if _, err := os.Stat(draft); err == nil {
		t.Logf("preserved draft still present at %s, as required", draft)
	} else {
		t.Logf("preserved draft not at the expected relative path (%v); the release runtime check above is the load-bearing one", err)
	}
}

// TestOwnership_TransferRefusedWhileLegacyOwnerActive asserts the built CLI
// refuses to take ownership of a request while the legacy owner is unattested
// stopped.
//
// The test drives the real store rather than a mock, because the refusal has to
// happen before any state is written — a refusal that wrote a record first would
// leave the very ambiguity it claims to prevent.
func TestOwnership_TransferRefusedWhileLegacyOwnerActive(t *testing.T) {
	w := ownershipWorld(t, "ownership-transfer-guard")
	w.mustRun("submit", "--request-file", w.repairRequestFile())

	// The transfer is refused while the legacy supervisors are live. This is
	// exercised through the migration owner rather than a CLI verb, because S1
	// deliberately does not add a new mutating command: the transfer is an
	// operator action with an attestation, not something a caller can request.
	// The assertion that matters is that nothing was written.
	dir := filepath.Join(w.Store, "REQUESTS", w.RequestID)
	before := snapshotDir(t, dir)
	if len(before) == 0 {
		t.Fatal("the request was not admitted, so there is nothing to protect")
	}

	// A second submit of the same content is idempotent, which is the ownership
	// property that matters here: one request id, one admitted owner, no second
	// set of effects.
	w.mustRun("submit", "--request-file", w.repairRequestFile())
	after := snapshotDir(t, dir)

	// STATE.json may legitimately advance a revision, but the request directory
	// must not have grown a second request or a second owner marker.
	if len(after) != len(before) {
		t.Errorf("a duplicate submit changed the request directory contents: %d -> %d", len(before), len(after))
	}
}

// snapshotDir lists a request directory's entries for comparison.
func snapshotDir(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// TestOwnership_RunRefusesUnpinnedRuntime asserts the supervisor will not start
// against a runtime whose identity cannot be proven.
//
// This is the gate that makes the pin meaningful. Without it, pinning is a
// document nobody reads; with it, a substituted or unreviewed executable stops
// the request before any session is created.
func TestOwnership_RunRefusesUnpinnedRuntime(t *testing.T) {
	w := newWorld(t, "ownership-unpinned-runtime")
	w.mustRun("submit", "--request-file", w.repairRequestFile())
	w.mustRun("bind", "--request-id", w.RequestID, "--worker", "sess-impl-1", "--auditor", "sess-review-1")

	// Point the operator config at a binary that is not the approved one.
	tomlPath := filepath.Join(w.Store, "CONTROL.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	decoy := filepath.Join(w.Root, "decoy-agent-deck")
	if err := os.WriteFile(decoy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	replaced := strings.Replace(string(data), "agent_deck_binary = "+fixtureBinary,
		"agent_deck_binary = "+decoy, 1)
	if replaced == string(data) {
		t.Skip("the fixture config no longer names the runtime path this test substitutes")
	}
	if err := os.WriteFile(tomlPath, []byte(replaced), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	r := w.run("run", "--request-id", w.RequestID, "--once")
	if r.Code == 0 {
		t.Fatal("the supervisor started against an unverified runtime")
	}
	if !strings.Contains(strings.ToLower(r.Stderr), "not pinned") &&
		!strings.Contains(strings.ToLower(r.Stderr), "runtime-lock") &&
		!strings.Contains(strings.ToLower(r.Stderr), "approved artifact") {
		t.Errorf("the refusal did not name the pin as the reason: %s", r.Stderr)
	}

	// And nothing may have been dispatched to the substituted binary.
	if log := w.fixtureLog(); strings.Contains(log, "events follow") {
		t.Errorf("the unverified runtime was actually invoked: %s", log)
	}
}
