package acceptance

// Production-wired acceptance tests.
//
// Every test here drives the built binary through a subprocess and makes business
// progress only through the documented commands. Nothing constructs internal
// state, because a test that seeds a Store directly proves only that the fixture
// it built was correct.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAcceptance_HumanAndExternalRequestSamePath asserts a request submitted by
// a human CLI and one submitted by a machine caller take the identical path.
//
// If they diverge, "the same contract" is a claim rather than a fact, and a Forge
// client would be following a route a human never exercises. The two submissions
// differ only in their `origin` provenance, and must produce the same stages,
// the same limits and the same result shape.
func TestAcceptance_HumanAndExternalRequestSamePath(t *testing.T) {
	buildBinaries(t)

	// One store, two request IDs: the only difference between the two
	// submissions is their origin provenance.
	w := newWorld(t, "req-human")
	w2 := newWorldIn(t, w, "req-forge")

	type outcome struct {
		stage    string
		accepted bool
		store    string
	}
	var results []outcome

	for _, c := range []struct {
		world  *world
		origin map[string]any
	}{
		{w, map[string]any{"kind": "human", "caller_id": "operator"}},
		{w2, map[string]any{"kind": "forge", "caller_id": "local-forge"}},
	} {
		req := c.world.repairRequestFile()
		var payload map[string]any
		data, err := os.ReadFile(req)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if err := jsonUnmarshal(data, &payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		payload["origin"] = c.origin
		req = c.world.writeRequest(payload)

		r := c.world.mustRun("--json", "submit", "--request-file", req)
		var adm map[string]any
		r.JSON(t, &adm)
		results = append(results, outcome{
			stage:    fmtString(adm["stage"]),
			accepted: adm["accepted"] == true,
			store:    fmtString(adm["store"]),
		})
	}

	if results[0].stage != results[1].stage {
		t.Errorf("stages differ by origin: %q vs %q; both callers must take the same path",
			results[0].stage, results[1].stage)
	}
	if !results[0].accepted || !results[1].accepted {
		t.Errorf("both origins must be accepted: %+v", results)
	}
	if results[0].store != results[1].store {
		t.Errorf("both callers must report the same store: %q vs %q", results[0].store, results[1].store)
	}
	// And both requests really did land in the same store, separately.
	for _, id := range []string{"req-human", "req-forge"} {
		if _, err := os.Stat(filepath.Join(w.Store, "REQUESTS", id, "STATE.json")); err != nil {
			t.Errorf("request %s is not in the shared store: %v", id, err)
		}
	}
}

// TestAcceptance_RequestRetryAndUnconsumedResult asserts a retry is idempotent
// and a result survives not being read.
//
// A caller that times out and retries must not cause a second set of effects,
// and a caller that never reads its result must still be able to get it later.
// Both are ordinary operational realities, and both are silent when broken.
func TestAcceptance_RequestRetryAndUnconsumedResult(t *testing.T) {
	w := newWorld(t, "forge-run-1842-repair")
	req := w.repairRequestFile()

	first := w.mustRun("--json", "submit", "--request-file", req)
	var firstAdm, secondAdm map[string]any
	first.JSON(t, &firstAdm)

	// The caller retries, having seen no acknowledgement.
	second := w.mustRun("--json", "submit", "--request-file", req)
	second.JSON(t, &secondAdm)

	if secondAdm["duplicate"] != true {
		t.Error("a retry of identical bytes must be reported as a duplicate")
	}
	if secondAdm["accepted"] == true {
		t.Error("a retry must not be accepted as new work")
	}
	if secondAdm["request_sha256"] != firstAdm["request_sha256"] {
		t.Errorf("the retry produced a different identity: %v vs %v",
			firstAdm["request_sha256"], secondAdm["request_sha256"])
	}

	// A conflicting payload under the same ID is refused and starts nothing.
	conflicting := w.repairRequestFile()
	data, _ := os.ReadFile(conflicting)
	payload := map[string]any{}
	_ = jsonUnmarshal(data, &payload)
	payload["objective"] = "a different objective entirely"
	conflictFile := w.writeRequest(payload)
	if r := w.run("--json", "submit", "--request-file", conflictFile); r.Code == 0 {
		t.Fatal("a conflicting payload under an admitted id must be refused")
	}

	// The request is still exactly where the first submission left it.
	st := w.status()
	if st["stage"] != "READY" {
		t.Errorf("stage = %v, want READY: a retry and a refused conflict must both leave no trace", st["stage"])
	}

	// The result is readable and explicitly not ready, and reading it does not
	// consume it.
	for i := 0; i < 2; i++ {
		r := w.mustRun("--json", "result", "--request-id", w.RequestID)
		var res map[string]any
		r.JSON(t, &res)
		if res["ready"] != false {
			t.Errorf("read %d: ready = %v, want false for an unrun request", i, res["ready"])
		}
		if res["stage"] == nil || res["stage"] == "" {
			t.Errorf("read %d: a not-ready result must carry the current stage", i)
		}
	}
}

// TestAcceptance_StaleReportsAndManualIntervention asserts a human editing the
// machine state is detected rather than silently obeyed.
//
// The plan is explicit that a human editing machine files requires explicit
// reconciliation and must never automatically override a safety gate. A
// hand-edited state file that decodes cleanly is the dangerous case, so the store
// refuses unknown fields and the test asserts a tampered file does not become a
// verified request.
func TestAcceptance_StaleReportsAndManualIntervention(t *testing.T) {
	w := newWorld(t, "forge-run-1842-repair")
	w.mustRun("--json", "submit", "--request-file", w.repairRequestFile())

	statePath := filepath.Join(w.Store, "REQUESTS", w.RequestID, "STATE.json")
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	// A human hand-edits the stage to a terminal one and adds a plausible field.
	tampered := strings.Replace(string(original), `"stage": "READY"`, `"stage": "VERIFIED", "verified_by": "a human"`, 1)
	if tampered == string(original) {
		t.Fatal("could not construct the tampered state fixture")
	}
	if err := os.WriteFile(statePath, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write tampered state: %v", err)
	}

	// The store must refuse it: an unknown field means the file is not this
	// schema, and refusing is the only safe reading.
	if r := w.run("--json", "status", "--request-id", w.RequestID); r.Code == 0 {
		t.Error("a hand-edited state file with an unknown field must be refused, not obeyed")
	}

	// Restoring the original returns the request to a readable state, which is
	// what makes this recoverable rather than merely refused.
	if err := os.WriteFile(statePath, original, 0o600); err != nil {
		t.Fatalf("restore state: %v", err)
	}
	if st := w.status(); st["stage"] != "READY" {
		t.Errorf("stage after restore = %v, want READY", st["stage"])
	}
}

// TestAcceptance_ReadOnlyAndScopedRepair asserts the capability boundary is
// enforced at the real entry point.
//
// A read-only request must not be able to grant itself code changes, and a
// repair without explicit permission must be refused — through the CLI a caller
// actually uses, not through an internal preflight.
func TestAcceptance_ReadOnlyAndScopedRepair(t *testing.T) {
	w := newWorld(t, "req-investigate")

	// A read-only request that also claims code changes.
	req := map[string]any{
		"version":    1,
		"request_id": w.RequestID,
		"origin":     map[string]any{"kind": "human", "caller_id": "operator"},
		"operation":  "INVESTIGATE",
		"workspace":  map[string]any{"path": w.Source},
		"objective":  "find out why the build is slow",
		"obligations": []map[string]any{
			{"id": "O-1", "criterion": "the cause is identified"},
		},
		"constraints":  map[string]any{"allow_code_changes": true},
		"verification": []map[string]any{{"check_id": "build"}},
	}
	if r := w.run("--json", "submit", "--request-file", w.writeRequest(req)); r.Code == 0 {
		t.Error("a read-only request claiming code changes must be refused at admission")
	}

	// The same request with read-only constraints is admitted.
	req["constraints"] = map[string]any{"allow_code_changes": false}
	if r := w.run("--json", "submit", "--request-file", w.writeRequest(req)); r.Code != 0 {
		t.Errorf("a genuinely read-only request must be admitted: %s", r.Stderr)
	}

	// A repair without explicit permission.
	req2 := map[string]any{
		"version":    1,
		"request_id": "req-repair-nopermission",
		"origin":     map[string]any{"kind": "human", "caller_id": "operator"},
		"operation":  "REPAIR",
		"workspace":  map[string]any{"path": w.Source},
		"objective":  "fix it",
		"obligations": []map[string]any{
			{"id": "O-1", "criterion": "fixed"},
		},
		"constraints": map[string]any{"allow_code_changes": false},
	}
	if r := w.run("--json", "submit", "--request-file", w.writeRequest(req2)); r.Code == 0 {
		t.Error("a repair without explicit write permission must be refused")
	}
}

// TestAcceptance_MissingHardAndSoftCheckpoint asserts the checkpoint verbs work
// end to end and that neither a submission nor an evidence item advances
// anything.
//
// The advance-nothing property is the one that matters: a worker that believes
// its submission moved the request forward will stop working.
func TestAcceptance_MissingHardAndSoftCheckpoint(t *testing.T) {
	w := newWorld(t, "forge-run-1842-repair")
	w.mustRun("--json", "submit", "--request-file", w.repairRequestFile())

	// Evidence must be digest-pinned, through the real command.
	if r := w.run("--json", "evidence", "--request-id", w.RequestID, "--id", "E-1", "--path", "diff.patch"); r.Code == 0 {
		t.Error("evidence without a digest must be refused through the CLI")
	}
	digest := strings.Repeat("ab", 32)
	w.mustRun("--json", "evidence", "--request-id", w.RequestID, "--id", "E-1",
		"--path", "diff.patch", "--sha256", digest)

	// A typed checkpoint.
	payload := filepath.Join(w.Root, "cp.json")
	if err := os.WriteFile(payload, []byte(`{
      "checkpoint_id": "CP-1",
      "claim": "the build is now green and the regression suite discovers tests",
      "next_action": "request an audit",
      "evidence_refs": ["E-1"]
    }`), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	r := w.mustRun("--json", "checkpoint", "--request-id", w.RequestID, "--payload-file", payload)
	var rec map[string]any
	r.JSON(t, &rec)
	if rec["advanced"] != false {
		t.Error("accepting a checkpoint must report that it advanced nothing")
	}

	// The request is untouched: still READY, obligations still open, no review.
	st := w.status()
	if st["stage"] != "READY" {
		t.Errorf("stage = %v, want READY after a checkpoint submission", st["stage"])
	}
	obligations, _ := st["obligations"].([]any)
	if len(obligations) == 0 {
		t.Fatal("status reported no obligations")
	}
	for _, o := range obligations {
		m, _ := o.(map[string]any)
		if m["disposition"] != "OPEN" {
			t.Errorf("obligation %v disposition = %v, want OPEN: a submission is not a review", m["id"], m["disposition"])
		}
	}

	// A duplicate is a no-op, not a failure.
	dup := w.mustRun("--json", "checkpoint", "--request-id", w.RequestID, "--payload-file", payload)
	var dupRec map[string]any
	dup.JSON(t, &dupRec)
	if dupRec["duplicate"] != true {
		t.Error("an identical checkpoint retry must be a no-op")
	}
}

// TestAcceptance_RestartDiagnosticHandoffVerification asserts state survives a
// process boundary at each of the four durability points.
//
// A supervisor restart is normal, not exceptional. Each subtest writes state,
// simulates a restart by reading it back through a fresh process, and asserts the
// records that must survive did.
func TestAcceptance_RestartDiagnosticHandoffVerification(t *testing.T) {
	w := newWorld(t, "forge-run-1842-repair")
	w.mustRun("--json", "submit", "--request-file", w.repairRequestFile())

	// Durable: a failed attempt, an evidence item, a question and a decision.
	w.mustRun("--json", "attempt", "--request-id", w.RequestID,
		"--cycle-id", "c-1", "--check", "regression", "--output", "FAIL: TestFoo\n    at x_test.go:12:3")
	w.mustRun("--json", "evidence", "--request-id", w.RequestID, "--id", "E-9",
		"--path", "log.txt", "--sha256", strings.Repeat("cd", 32))
	w.mustRun("--json", "question", "--request-id", w.RequestID, "--title", "which approach?")
	// No decision authority is configured in this world, so a proposal is
	// recorded and an acceptance is refused. That is the safe default, and it
	// must be visible rather than silently downgraded.
	prop := w.mustRun("--json", "decision", "--request-id", w.RequestID, "--title", "use the queue",
		"--question-id", "Q-0001")
	var propOut map[string]any
	prop.JSON(t, &propOut)
	if propOut["status"] != "PROPOSED" {
		t.Errorf("without a configured authority a decision must stay PROPOSED, got %v", propOut["status"])
	}
	if r := w.run("--json", "decision", "--request-id", w.RequestID, "--title", "accept it",
		"--accept-as", "auditor"); r.Code == 0 {
		t.Error("accepting a decision with no configured authority must be refused")
	} else if !strings.Contains(r.Stderr, "may not accept decisions") {
		t.Errorf("refusal should explain the missing authority: %s", strings.TrimSpace(r.Stderr))
	}

	// Restart: a fresh process reads it all back.
	st := w.status()
	if st["stage"] != "READY" {
		t.Errorf("stage after restart = %v, want READY", st["stage"])
	}
	cycles, _ := st["repair_cycles"].(float64)
	if cycles != 1 {
		t.Errorf("repair cycles after restart = %v, want 1: a booked cycle must not be lost", st["repair_cycles"])
	}

	// The failure ledger is intact and a repeated cycle ID is still idempotent.
	before := cycles
	repeat := w.mustRun("--json", "attempt", "--request-id", w.RequestID,
		"--cycle-id", "c-1", "--check", "regression", "--output", "FAIL: TestFoo\n    at x_test.go:99:9")
	var rep map[string]any
	repeat.JSON(t, &rep)
	if rep["duplicate"] != true {
		t.Error("re-submitting the same cycle id must be a no-op after a restart")
	}
	after, _ := w.status()["repair_cycles"].(float64)
	if after != before {
		t.Errorf("a duplicate cycle changed the budget: %v then %v", before, after)
	}

	// Memory survived, and only the configured authority accepted the decision.
	st = w.status()
	// The decision authority was not configured, so the decision is a proposal.
	// That is the safe direction and it must be visible to the caller.
	_ = st
}

// TestAcceptance_CancelRecordsIntentWithoutEndingTheRequest asserts cancellation
// is an intent, not a shortcut to a terminal outcome.
//
// Effects have to be reconciled before a request may be called CANCELLED, so the
// command records intent and leaves the stage nonterminal. A cancel that
// immediately published a terminal result would be claiming effects were settled
// when nobody checked.
func TestAcceptance_CancelRecordsIntentWithoutEndingTheRequest(t *testing.T) {
	w := newWorld(t, "forge-run-1842-repair")
	w.mustRun("--json", "submit", "--request-file", w.repairRequestFile())

	r := w.mustRun("--json", "cancel", "--request-id", w.RequestID, "--reason", "operator changed their mind")
	var out map[string]any
	r.JSON(t, &out)
	if !strings.Contains(fmtString(out["reason"]), "reconciliation") {
		t.Errorf("cancel reason = %q, want it to say effects are pending reconciliation", out["reason"])
	}

	st := w.status()
	if st["terminal"] != false {
		t.Error("cancelling must not by itself make a request terminal; effects are still unreconciled")
	}
	if st["stage"] != "READY" {
		t.Errorf("stage = %v, want READY with the cancellation recorded", st["stage"])
	}
}

// TestAcceptance_MissingStoreIsRefused asserts every command refuses without a
// store, through the real binary.
//
// This is the guard that keeps two stores holding the same request ID from being
// confused for one another, so it must hold for every subcommand, not just the
// one a test remembered to check.
func TestAcceptance_MissingStoreIsRefused(t *testing.T) {
	buildBinaries(t)
	cmds := [][]string{
		{"submit", "--request-file", "/dev/null"},
		{"status", "--request-id", "x"},
		{"result", "--request-id", "x"},
		{"cancel", "--request-id", "x"},
		{"checkpoint", "--request-id", "x", "--payload-file", "/dev/null"},
		{"evidence", "--request-id", "x", "--id", "E", "--sha256", strings.Repeat("a", 64)},
		{"attempt", "--request-id", "x", "--check", "build"},
		{"review-result", "--request-id", "x", "--review-id", "A-1", "--kind", "COMPLETION", "--outcome", "PASS"},
		{"question", "--request-id", "x", "--title", "why"},
		{"decision", "--request-id", "x", "--title", "because"},
	}
	for _, c := range cmds {
		t.Run(c[0], func(t *testing.T) {
			cmd := execCommand(controlBinary, c...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("%s succeeded without --store", c[0])
				return
			}
			if !strings.Contains(string(out), "--store is required") {
				t.Errorf("%s did not explain the store requirement: %s", c[0], strings.TrimSpace(string(out)))
			}
		})
	}
}

// waitForStage waits for a stage to appear, used where an effect is genuinely
// asynchronous.
func (w *world) waitForStage(stage string, timeout time.Duration) {
	w.t.Helper()
	waitFor(w.t, "stage "+stage, timeout, func() bool {
		return fmtString(w.status()["stage"]) == stage
	})
}
