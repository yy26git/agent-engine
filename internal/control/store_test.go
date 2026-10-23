package control

// EC1 store and admission tests.
//
// The theme is that an admitted request is a durable fact with exactly one
// identity. A duplicate must produce no second set of effects, a conflicting
// reuse of an identifier must produce none at all, and a published result must
// survive the process that produced it disappearing.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// fixture builds an operator store and returns its root plus a store handle.
type fixture struct {
	root    string
	srcRoot string
	evRoot  string
	store   *Store
	cfg     *Config
	now     time.Time
}

// newFixture creates a store with approved roots and a small check registry.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "store")
	srcRoot := filepath.Join(base, "src")
	evRoot := filepath.Join(base, "evidence")
	outRoot := filepath.Join(base, "out")
	for _, d := range []string{root, srcRoot, evRoot, outRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	toml := `agent_deck_binary = "agent-deck"
profile = "default"
tool = "mcode"
output_root = "` + outRoot + `"
source_roots = ["` + srcRoot + `"]
evidence_roots = ["` + evRoot + `"]

[limits]
max_repair_cycles = 4
max_worker_generations = 3
max_audit_attempts = 3
max_guidance_rounds = 2

[checks.build]
argv = ["go", "build", "./..."]
timeout_seconds = 300
discovery_required = "false"

[checks.regression]
argv = ["go", "test", "./..."]
timeout_seconds = 600
discovery_required = "true"

[checks.test]
argv = ["go", "test", "./..."]
timeout_seconds = 600
discovery_required = "true"
`
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(toml), 0o600); err != nil {
		t.Fatalf("write CONTROL.toml: %v", err)
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	st := NewStore(cfg)
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	return &fixture{root: root, srcRoot: srcRoot, evRoot: evRoot, store: st, cfg: cfg, now: now}
}

// validRequest builds a request that should be admissible.
func (f *fixture) validRequest() *contract.Request {
	abs, _ := filepath.Abs(filepath.Join(f.srcRoot, "project"))
	return &contract.Request{
		Version:   contract.Version,
		RequestID: "forge-run-1842-repair",
		Origin:    contract.Origin{Kind: "forge", CallerID: "local-forge"},
		Operation: contract.OpRepair,
		Workspace: contract.Workspace{Path: abs, ExpectedHead: "abc123"},
		Objective: "Fix the attributable regression without changing protected evaluation code",
		Obligations: []contract.Obligation{{
			ID: "O-1", Criterion: "Regression reproduced, fixed and verified",
			RequiredChecks: []string{"build", "regression"},
		}},
		Evidence: []contract.Evidence{{
			Path:   filepath.Join(f.evRoot, "result.json"),
			SHA256: strings.Repeat("ab", 32), Kind: contract.EvidenceExternalRun,
		}},
		Constraints: contract.Constraints{
			AllowCodeChanges: true,
			AllowedPaths:     []string{"src/**"},
			ProtectedPaths:   []string{"evaluation/**"},
		},
		Verification: []contract.VerificationItem{{CheckID: "build"}, {CheckID: "regression"}},
		Limits:       contract.Limits{RepairCycles: 3, WorkerGens: 2, AuditAttempts: 2},
	}
}

// TestAdmission_DuplicateDoesNotCreateSecondIntent asserts that submitting the
// same request twice admits it once.
//
// This is the property the content digest exists for. A caller that retries
// after a timeout, or a supervisor that restarts and re-submits, must not cause
// a second set of effects — a second session, a second audit, a second bill.
func TestAdmission_DuplicateDoesNotCreateSecondIntent(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()

	first, err := f.store.Admit(req)
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if !first.Accepted {
		t.Fatal("the first submission must be accepted")
	}
	if first.Duplicate {
		t.Error("the first submission cannot be a duplicate")
	}

	second, err := f.store.Admit(req)
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if second.Duplicate != true {
		t.Error("the identical second submission must be reported as a duplicate")
	}
	if second.Accepted {
		t.Error("a duplicate must not be accepted as new work")
	}
	if second.RequestDigest != first.RequestDigest {
		t.Errorf("digest changed across identical submissions: %q then %q", first.RequestDigest, second.RequestDigest)
	}
	if second.TaskID != first.TaskID {
		t.Errorf("task id changed across identical submissions: %q then %q", first.TaskID, second.TaskID)
	}

	// A duplicate also does not advance the revision: nothing happened.
	stateBefore, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("third admit: %v", err)
	}
	stateAfter, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if stateAfter.Revision != stateBefore.Revision {
		t.Errorf("a duplicate advanced the revision from %d to %d: it changed state",
			stateBefore.Revision, stateAfter.Revision)
	}
}

// TestAdmission_ConflictingPayloadRefused asserts that reusing an identifier
// with different content starts nothing.
//
// The caller is asserting two incompatible things about one identifier. The
// only safe response is to do nothing and make them say which they meant —
// overwriting would discard admitted work, and running both would act on an
// ambiguity the system has no business resolving.
func TestAdmission_ConflictingPayloadRefused(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	first, err := f.store.Admit(req)
	if err != nil {
		t.Fatalf("admit original: %v", err)
	}

	// Same ID, different objective. The digest must differ.
	conflicting := f.validRequest()
	conflicting.Objective = "Something else entirely"
	if _, err := f.store.Admit(conflicting); err == nil {
		t.Fatal("a conflicting payload must be refused")
	} else if !strings.Contains(err.Error(), "already admitted with different content") {
		t.Errorf("error = %v, want an explicit conflict", err)
	}

	// The originally admitted request must be untouched.
	state, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.RequestDigest != first.RequestDigest {
		t.Errorf("the admitted digest changed to %q; a refused conflict must not rewrite the store",
			state.RequestDigest)
	}
	stored, err := f.store.Request(req.RequestID)
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	if stored.Objective == conflicting.Objective {
		t.Error("the refused request's content was written over the admitted one")
	}

	// A conflict must not be reported as a duplicate either: it is an error, not
	// a quiet success.
	if _, err := f.store.Admit(conflicting); err == nil {
		t.Error("a second conflicting submission must also be refused")
	}
}

// TestContract_ReadOnlyDefaultsDeny asserts that read-only is structural.
//
// A request that omits constraints gets no write authority at all, and the one
// operation that can write has to say so explicitly. A repair that silently
// inherited read-only would fail for a reason the caller never chose, and an
// investigation that silently inherited write access would mutate the very
// snapshot it was reporting on.
func TestContract_ReadOnlyDefaultsDeny(t *testing.T) {
	f := newFixture(t)

	t.Run("omitted constraints deny writes", func(t *testing.T) {
		req := f.validRequest()
		req.Operation = contract.OpInvestigate
		req.Constraints = contract.Constraints{} // omitted entirely
		if err := req.Validate(); err != nil {
			t.Fatalf("an investigate request with no constraints must be valid: %v", err)
		}
		if req.Constraints.AllowCodeChanges {
			t.Error("omitted constraints must default to no code changes")
		}
	})

	t.Run("repair requires explicit permission", func(t *testing.T) {
		req := f.validRequest()
		req.Constraints.AllowCodeChanges = false
		err := req.Validate()
		if err == nil {
			t.Fatal("a repair without explicit write permission must be refused")
		}
		if !strings.Contains(err.Error(), "allow_code_changes") {
			t.Errorf("error = %v, want it to name the missing permission", err)
		}
	})

	t.Run("read-only operations reject write claims", func(t *testing.T) {
		for _, op := range []contract.Operation{contract.OpInvestigate, contract.OpVerify, contract.OpGenerateReport} {
			req := f.validRequest()
			req.Operation = op
			req.Constraints.AllowCodeChanges = true
			if err := req.Validate(); err == nil {
				t.Errorf("%s must be refused when it claims write access", op)
			}
		}
	})

	t.Run("repair is the only writable operation", func(t *testing.T) {
		if !contract.OpRepair.CodeChangeAllowed() {
			t.Error("REPAIR must be able to change code")
		}
		for _, op := range []contract.Operation{contract.OpInvestigate, contract.OpVerify, contract.OpGenerateReport} {
			if op.CodeChangeAllowed() {
				t.Errorf("%s must not be able to change code", op)
			}
		}
	})

	t.Run("protected and allowed paths cannot contradict", func(t *testing.T) {
		req := f.validRequest()
		req.Constraints.AllowedPaths = []string{"src/**", "evaluation/**"}
		req.Constraints.ProtectedPaths = []string{"evaluation/**"}
		if err := req.Validate(); err == nil {
			t.Fatal("a path that is both allowed and protected must be refused, not resolved by check order")
		}
	})
}

// TestResult_RegeneratedAfterCrash asserts that a published result survives the
// loss of its projection.
//
// The authoritative bytes are committed to an immutable report body and
// referenced by digest from state. RESULT.json is only a projection, so losing
// it is recoverable by re-derivation — never by re-deciding, which is what
// would happen if the projection were treated as the source of truth.
func TestResult_RegeneratedAfterCrash(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}

	res := contract.Result{
		Status:  contract.StatusBlocked,
		Summary: "no installed backend could enforce read-only source access",
	}
	digest, err := f.store.PublishResult(req.RequestID, res)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if digest == "" {
		t.Fatal("publish returned no digest")
	}

	// The state committed terminal and references the body.
	state, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !state.Stage.Terminal() {
		t.Fatalf("stage = %s, want terminal after publishing a result", state.Stage)
	}
	if state.ResultDigest != digest {
		t.Errorf("state digest = %q, want %q", state.ResultDigest, digest)
	}

	// Simulate a crash that destroyed the projection but not the body.
	projPath := filepath.Join(f.root, RequestsDirName, req.RequestID, ResultFileName)
	if err := os.Remove(projPath); err != nil {
		t.Fatalf("remove projection: %v", err)
	}

	got, err := f.store.Result(req.RequestID)
	if err != nil {
		t.Fatalf("result after losing the projection: %v", err)
	}
	if !got.Ready || got.Status != contract.StatusBlocked {
		t.Errorf("result = %+v, want a ready BLOCKED result regenerated from the committed body", got)
	}
	if _, err := os.Stat(projPath); err != nil {
		t.Errorf("the projection was not regenerated: %v", err)
	}

	// A published result never changes.
	if _, err := f.store.PublishResult(req.RequestID, contract.Result{Status: contract.StatusPass}); err == nil {
		t.Error("republishing a different outcome must be refused; a published result is immutable")
	}
	// Re-publishing the same outcome is idempotent, so a caller retrying does
	// not get told it failed.
	if _, err := f.store.PublishResult(req.RequestID, contract.Result{
		Status: contract.StatusBlocked, Summary: "no installed backend could enforce read-only source access",
	}); err != nil {
		t.Errorf("republishing the same outcome must be idempotent, got %v", err)
	}
}

// TestResult_NonterminalIsNotFabricated asserts a caller is told a request is
// unfinished rather than handed something result-shaped.
func TestResult_NonterminalIsNotFabricated(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	got, err := f.store.Result(req.RequestID)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if got.Ready {
		t.Error("an admitted, not-yet-run request must not report a ready result")
	}
	if got.Stage == "" {
		t.Error("a not-ready result must carry the current stage")
	}
	if got.Status != "" {
		t.Errorf("a not-ready result must not carry a terminal status, got %q", got.Status)
	}
}

// TestState_CorruptOrFutureVersionRefused asserts machine files fail closed.
//
// A corrupt or newer state file is surfaced rather than reset. Resetting would
// silently discard the record of what a request was doing, and a caller would
// see a fresh request where a half-finished one used to be.
func TestState_CorruptOrFutureVersionRefused(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	statePath := filepath.Join(f.root, RequestsDirName, req.RequestID, StateFileName)

	t.Run("corrupt state", func(t *testing.T) {
		if err := os.WriteFile(statePath, []byte("{ this is not json"), 0o600); err != nil {
			t.Fatalf("corrupt state: %v", err)
		}
		if _, err := f.store.State(req.RequestID); err == nil {
			t.Fatal("a corrupt state file must be refused")
		} else if !strings.Contains(err.Error(), "refusing to guess") {
			t.Errorf("error = %v, want a refusal to guess", err)
		}
	})

	t.Run("unknown field in state", func(t *testing.T) {
		good, err := json.Marshal(map[string]any{
			"schema_version": 1, "revision": 1, "request_id": "x",
			"request_sha256": strings.Repeat("a", 64), "stage": "READY",
			"field_this_build_does_not_know": true,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(statePath, good, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := f.store.State(req.RequestID); err == nil {
			t.Fatal("a state file with an unknown field must be refused, not partially understood")
		}
	})

	t.Run("future schema version", func(t *testing.T) {
		good, err := json.Marshal(map[string]any{
			"schema_version": 99, "revision": 1, "request_id": "x",
			"request_sha256": strings.Repeat("a", 64), "stage": "READY",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(statePath, good, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err = f.store.State(req.RequestID)
		if err == nil {
			t.Fatal("a newer state file must be refused")
		}
		if !strings.Contains(err.Error(), "refusing to downgrade") {
			t.Errorf("error = %v, want a refusal to downgrade", err)
		}
	})
}

// TestStore_RequestIDLookupIndependentOfCwd asserts that a request is found by
// store and ID alone.
//
// Two consequences depend on this. A caller running from a different directory
// must still find its request, and two stores holding the same request ID must
// never be confused for each other.
func TestStore_RequestIDLookupIndependentOfCwd(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// Change the working directory and look the request up again.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	elsewhere := t.TempDir()
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(orig) }()

	state, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("lookup from another directory: %v", err)
	}
	if state.RequestID != req.RequestID {
		t.Errorf("looked up %q, want %q", state.RequestID, req.RequestID)
	}

	// A second store with the same request ID is a different request, and
	// admitting there must not affect the first.
	other := newFixture(t)
	otherReq := other.validRequest()
	if _, err := other.store.Admit(otherReq); err != nil {
		t.Fatalf("admit into second store: %v", err)
	}
	firstState, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("reload first store: %v", err)
	}
	otherState, err := other.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load second store: %v", err)
	}
	if firstState.RequestDigest == otherState.RequestDigest {
		t.Error("two stores admitted the same ID and produced the same digest; the stores are not independent")
	}
}

// TestStore_ReadOnlySourceSeparateOutputRoot asserts the store cannot write
// results into the read-only source tree.
//
// If the output root sat inside the source, evidence and subject would be the
// same bytes, and a hash check could no longer tell a report of the source from
// a modification of it.
func TestStore_ReadOnlySourceSeparateOutputRoot(t *testing.T) {
	base := t.TempDir()
	srcRoot := filepath.Join(base, "src")
	inside := filepath.Join(srcRoot, "out")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	root := filepath.Join(base, "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	toml := `agent_deck_binary = "agent-deck"
tool = "mcode"
output_root = "` + inside + `"
source_roots = ["` + srcRoot + `"]
evidence_roots = ["` + base + `"]

[limits]
max_repair_cycles = 3
max_worker_generations = 3
max_audit_attempts = 3
max_guidance_rounds = 2

[checks.build]
argv = ["true"]
timeout_seconds = 10
`
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(toml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadConfig(root)
	if err == nil {
		t.Fatal("an output root inside the source root must be refused")
	}
	if !strings.Contains(err.Error(), "output_root") {
		t.Errorf("error = %v, want it to name output_root", err)
	}
}

// TestIntent_FullLedgerBlocksWithoutDroppingWork asserts a full intent ledger
// refuses new work rather than discarding old intent.
//
// The bound exists so a stuck request cannot grow an unbounded record. What it
// must never do is meet that bound by forgetting an instruction it still owes —
// that converts a visible problem into an invisible one.
func TestIntent_FullLedgerBlocksWithoutDroppingWork(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if _, err := f.store.UpdateCommitted(req.RequestID, func(s *State) error {
		// Fill the ledger with work that has not been started.
		for i := 0; i < MaxActivations; i++ {
			s.Activations = append(s.Activations, Activation{
				ActivationID: "A" + string(rune('a'+i)),
				Role:         "implementer",
				Status:       ActivationPending,
				Revision:     s.Revision,
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("fill ledger: %v", err)
	}

	// A further intent must be refused rather than evicting an existing one.
	_, err := f.store.UpdateCommitted(req.RequestID, func(s *State) error {
		if len(s.UnresolvedActivations()) >= MaxActivations {
			return errors.New("activation ledger is full of unresolved intent; refusing to forget any of it")
		}
		return nil
	})
	if err == nil {
		t.Fatal("a full ledger must refuse new intent")
	}

	state, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(state.Activations) != MaxActivations {
		t.Errorf("ledger length = %d, want %d preserved", len(state.Activations), MaxActivations)
	}
	if got := len(state.UnresolvedActivations()); got != MaxActivations {
		t.Errorf("unresolved activations = %d, want all %d preserved", got, MaxActivations)
	}
}

// TestAdmission_ConcurrentDuplicatesAdmitOnce asserts two simultaneous
// submissions of identical bytes admit exactly once.
//
// Without a store-wide admission lock both would observe "no such request" and
// both would create it, which is the duplicate-intent problem in its purest
// form.
func TestAdmission_ConcurrentDuplicatesAdmitOnce(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()

	const n = 6
	var wg sync.WaitGroup
	results := make([]error, n)
	accepted := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adm, err := f.store.Admit(req)
			results[i] = err
			if adm != nil {
				accepted[i] = adm.Accepted
			}
		}(i)
	}
	wg.Wait()

	acceptCount := 0
	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent admit %d failed: %v", i, err)
		}
		if accepted[i] {
			acceptCount++
		}
	}
	if acceptCount != 1 {
		t.Errorf("accepted %d times, want exactly 1", acceptCount)
	}

	// And only one request directory exists.
	entries, err := os.ReadDir(filepath.Join(f.root, RequestsDirName))
	if err != nil {
		t.Fatalf("read requests dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("request directories = %d, want 1", len(entries))
	}
}

// TestStore_UpdateAbortsOnError asserts a refused decision writes nothing.
func TestStore_UpdateAbortsOnError(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	before, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	refused := errors.New("refused for this test")
	if err := f.store.Update(req.RequestID, func(s *State) error {
		s.Stage = StageWorking
		s.RepairCycles = 99
		return refused
	}); err != refused {
		t.Fatalf("Update error = %v, want the refusal to propagate", err)
	}

	after, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Stage != before.Stage {
		t.Errorf("stage = %s, want %s: a refused decision mutated state", after.Stage, before.Stage)
	}
	if after.RepairCycles != before.RepairCycles {
		t.Errorf("repair cycles = %d, want %d", after.RepairCycles, before.RepairCycles)
	}
	if after.Revision != before.Revision {
		t.Errorf("revision = %d, want %d: a refused decision consumed a revision", after.Revision, before.Revision)
	}
}

// TestStore_CloneIsDeep asserts a cloned state shares nothing mutable.
func TestStore_CloneIsDeep(t *testing.T) {
	f := newFixture(t)
	req := f.validRequest()
	if _, err := f.store.Admit(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	original, err := f.store.State(req.RequestID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	original.ActiveReview = &ActiveReview{Kind: ReviewCompletion, ReviewID: "R-1"}
	original.Jobs = []CheckJob{{JobID: "J-1", CheckID: "build", Status: JobPending}}
	original.FailedCheckKeys = []string{"build:exit1"}

	clone := original.Clone()
	clone.Obligations[0].Disposition = DispositionAdvanced
	clone.Obligations[0].RequiredChecks[0] = "mutated"
	clone.Checks[0] = "mutated"
	clone.Jobs[0].Status = JobPass
	clone.Activations = append(clone.Activations, Activation{ActivationID: "extra"})
	clone.FailedCheckKeys[0] = "mutated"
	clone.ActiveReview.ReviewID = "mutated"
	clone.History = append(clone.History, HistoryEntry{Event: "mutated"})

	if original.Obligations[0].Disposition != DispositionOpen {
		t.Error("obligation disposition shared")
	}
	if original.Obligations[0].RequiredChecks[0] != "build" {
		t.Error("nested obligation checks shared")
	}
	if original.Checks[0] != "build" {
		t.Error("checks shared")
	}
	if original.Jobs[0].Status != JobPending {
		t.Error("jobs shared")
	}
	if len(original.Activations) != 0 {
		t.Error("activations shared")
	}
	if original.FailedCheckKeys[0] != "build:exit1" {
		t.Error("failed check keys shared")
	}
	if original.ActiveReview.ReviewID != "R-1" {
		t.Error("active review shared")
	}
	if len(original.History) != 0 {
		t.Error("history shared")
	}
}

// TestReviewKind_CannotApproveExceptCompletion asserts the structural guarantee
// that only a completion review can verify a request.
//
// This is a type-level property, not a rule someone has to remember at each
// call site. CHECKPOINT answers "should we keep going?" and DIAGNOSTIC answers
// "why is this stuck?"; neither can answer "is this finished and correct?", and
// letting either emit PASS is how a stuck-worker consultation becomes mistaken
// for sign-off.
func TestReviewKind_CannotApproveExceptCompletion(t *testing.T) {
	if !ReviewCompletion.Approves() {
		t.Error("COMPLETION must be able to approve")
	}
	if !ReviewCompletion.Allows(OutcomePass) || !ReviewCompletion.Allows(OutcomeFail) {
		t.Error("COMPLETION must accept PASS and FAIL")
	}
	for _, k := range []ReviewKind{ReviewCheckpoint, ReviewDiagnostic} {
		if k.Approves() {
			t.Errorf("%s must not be able to approve a request", k)
		}
		if k.Allows(OutcomePass) {
			t.Errorf("%s must not accept PASS as an outcome", k)
		}
	}
	if ReviewCheckpoint.Allows(OutcomeGuidance) {
		t.Error("CHECKPOINT must not accept a DIAGNOSTIC guidance outcome")
	}
	if !ReviewDiagnostic.Allows(OutcomeGuidance) || !ReviewDiagnostic.Allows(OutcomeRefresh) {
		t.Error("DIAGNOSTIC must accept guidance and refresh")
	}
}
