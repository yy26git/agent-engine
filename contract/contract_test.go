package contract

// Contract tests.
//
// The digest is the mechanism admission depends on to tell a duplicate from a
// conflict, so these tests are about one property: two requests hash the same if
// and only if they would produce the same effects. Anything that breaks that —
// key order, a reordered array, an omitted field — turns a safe retry into a
// false conflict or, worse, lets a changed request pass as the accepted one.

import (
	"encoding/json"
	"strings"
	"testing"
)

func baseRequest() *Request {
	return &Request{
		Version:   1,
		RequestID: "forge-run-1842-repair",
		Origin:    Origin{Kind: "forge", CallerID: "local-forge"},
		Operation: OpRepair,
		Workspace: Workspace{Path: "/src/project", ExpectedHead: "abc123"},
		Objective: "Fix the regression",
		Obligations: []Obligation{
			{ID: "O-1", Criterion: "reproduced, fixed and verified", RequiredChecks: []string{"build"}},
			{ID: "O-2", Criterion: "no protected path changed"},
		},
		Constraints: Constraints{
			AllowCodeChanges: true,
			AllowedPaths:     []string{"src/**"},
			ProtectedPaths:   []string{"evaluation/**"},
		},
		Verification: []VerificationItem{{CheckID: "build"}},
		Evidence: []Evidence{
			{Path: "/ev/result.json", SHA256: strings.Repeat("ab", 32), Kind: EvidenceExternalRun},
		},
		Limits: Limits{RepairCycles: 3},
	}
}

func TestDigest_IsStableAcrossSerialisation(t *testing.T) {
	req := baseRequest()
	first, err := CanonicalSHA256(req)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := CanonicalSHA256(req)
		if err != nil {
			t.Fatalf("hash repeat %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("hash is not stable: %q then %q", first, again)
		}
	}

	// A round trip through generic JSON — a different serialiser, or the same
	// one re-emitting the file — must not change the identity.
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reData, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	var rehydrated Request
	if err := json.Unmarshal(reData, &rehydrated); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	third, err := CanonicalSHA256(&rehydrated)
	if err != nil {
		t.Fatalf("hash rehydrated: %v", err)
	}
	if third != first {
		t.Errorf("digest changed after a JSON round trip: %q then %q", first, third)
	}
}

func TestDigest_ArrayOrderIsSignificant(t *testing.T) {
	// Obligations, allowed paths and protected paths are semantically ordered.
	// Sorting them in the digest would make a request permitting src before
	// evaluation identical to one permitting the reverse.
	a := baseRequest()
	b := baseRequest()
	b.Obligations[0], b.Obligations[1] = b.Obligations[1], b.Obligations[0]

	da, err := CanonicalSHA256(a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	db, err := CanonicalSHA256(b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if da == db {
		t.Error("reordering obligations must change the digest: array order is semantically meaningful")
	}

	c := baseRequest()
	d := baseRequest()
	d.Constraints.AllowedPaths = []string{"src/other/**", "src/**"}
	dc, _ := CanonicalSHA256(c)
	dd, _ := CanonicalSHA256(d)
	if dc == dd {
		t.Error("reordering allowed_paths must change the digest: path precedence is semantically meaningful")
	}
}

func TestDigest_EverySafetyFieldContributes(t *testing.T) {
	// Each of these changes what the request is permitted to do. If any were
	// excluded from the digest, a weaker request could be submitted under an
	// already-admitted identity and pass as the accepted one.
	mutations := map[string]func(*Request){
		"allow_code_changes": func(r *Request) { r.Constraints.AllowCodeChanges = false },
		"operation":          func(r *Request) { r.Operation = OpInvestigate },
		"workspace":          func(r *Request) { r.Workspace.Path = "/src/other" },
		"expected_head":      func(r *Request) { r.Workspace.ExpectedHead = "def456" },
		"objective":          func(r *Request) { r.Objective = "Something else" },
		"protected_paths":    func(r *Request) { r.Constraints.ProtectedPaths = nil },
		"allowed_paths":      func(r *Request) { r.Constraints.AllowedPaths = []string{"**"} },
		"repair_cycle_limit": func(r *Request) { r.Limits.RepairCycles = 99 },
		"origin":             func(r *Request) { r.Origin.CallerID = "someone-else" },
		"evidence_digest":    func(r *Request) { r.Evidence = nil },
		"required_checks":    func(r *Request) { r.Obligations[0].RequiredChecks = nil },
		"obligation_added":   func(r *Request) { r.Obligations = append(r.Obligations, Obligation{ID: "O-9", Criterion: "extra"}) },
	}

	base, err := CanonicalSHA256(baseRequest())
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			req := baseRequest()
			mutate(req)
			got, err := CanonicalSHA256(req)
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			if got == base {
				t.Errorf("changing %s did not change the digest: that field is not covered by request identity", name)
			}
		})
	}
}

func TestDecodeRequest_RefusesUnknownFieldsAndVersions(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		data, err := json.Marshal(baseRequest())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// Simulate a caller misspelling a safety field.
		injected := strings.Replace(string(data), `"allowed_paths"`, `"allowd_paths"`, 1)
		_, err = DecodeRequest([]byte(injected))
		if err == nil {
			t.Fatal("an unknown field must be refused; a misspelled safety field would otherwise be silently ignored")
		}
	})

	t.Run("future version", func(t *testing.T) {
		req := baseRequest()
		req.Version = 99
		data, _ := json.Marshal(req)
		decoded, err := DecodeRequest(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := decoded.Validate(); err == nil {
			t.Fatal("a future schema version must be refused at validation")
		} else if !strings.Contains(err.Error(), "unsupported schema version") {
			t.Errorf("error = %v, want an explicit version refusal", err)
		}
	})

	t.Run("trailing content", func(t *testing.T) {
		data, _ := json.Marshal(baseRequest())
		if _, err := DecodeRequest(append(data, []byte(`{"extra":1}`)...)); err == nil {
			t.Error("trailing content after the request object must be refused")
		}
	})
}

func TestValidate_RefusesUnusableRequests(t *testing.T) {
	cases := map[string]func(*Request){
		"empty objective":      func(r *Request) { r.Objective = "  " },
		"no obligations":       func(r *Request) { r.Obligations = nil },
		"duplicate obligation": func(r *Request) { r.Obligations[1].ID = "O-1" },
		"blank criterion":      func(r *Request) { r.Obligations[0].Criterion = "" },
		"relative workspace":   func(r *Request) { r.Workspace.Path = "relative/path" },
		"traversing workspace": func(r *Request) { r.Workspace.Path = "/src/../../etc" },
		"unknown operation":    func(r *Request) { r.Operation = "RESET_EVERYTHING" },
		"bad evidence digest": func(r *Request) {
			r.Evidence = []Evidence{{Path: "/ev/a.json", SHA256: "not-a-digest", Kind: EvidenceExternalRun}}
		},
		"relative evidence": func(r *Request) {
			r.Evidence = []Evidence{{Path: "ev/a.json", SHA256: strings.Repeat("a", 64), Kind: EvidenceExternalRun}}
		},
		"unknown evidence kind": func(r *Request) {
			r.Evidence = []Evidence{{Path: "/ev/a.json", SHA256: strings.Repeat("a", 64), Kind: "guess"}}
		},
		"negative limit":  func(r *Request) { r.Limits.RepairCycles = -1 },
		"duplicate check": func(r *Request) { r.Verification = []VerificationItem{{CheckID: "build"}, {CheckID: "build"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := baseRequest()
			mutate(req)
			if err := req.Validate(); err == nil {
				t.Errorf("%s must be refused", name)
			}
		})
	}
}

func TestValidateRequestID_RefusesPathTricks(t *testing.T) {
	bad := []string{
		"", ".", "..", "../escape", "a/b", `a\b`, ".hidden", "with space",
		"emoji-\U0001F600", strings.Repeat("x", 129),
	}
	for _, id := range bad {
		if err := ValidateRequestID(id); err == nil {
			t.Errorf("request id %q must be refused", id)
		}
	}
	for _, id := range []string{"forge-run-1842-repair", "a", "A1", "x.y_z-1"} {
		if err := ValidateRequestID(id); err != nil {
			t.Errorf("request id %q must be accepted: %v", id, err)
		}
	}
}

func TestLimits_EffectiveFillsDefaults(t *testing.T) {
	got := Limits{}.Effective()
	if got.RepairCycles != DefaultRepairCycles || got.WorkerGens != DefaultWorkerGens || got.AuditAttempts != DefaultAuditAttempts {
		t.Errorf("defaults not applied: %+v", got)
	}
	// A caller may ask for less, and that must be preserved rather than raised
	// to the default.
	less := Limits{RepairCycles: 1, WorkerGens: 1, AuditAttempts: 1}.Effective()
	if less.RepairCycles != 1 || less.WorkerGens != 1 || less.AuditAttempts != 1 {
		t.Errorf("a lower requested limit was raised to the default: %+v", less)
	}
}

func TestStatus_TerminalOnly(t *testing.T) {
	for _, s := range []Status{StatusPass, StatusFail, StatusBlocked, StatusCancelled} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	if Status("WORKING").Terminal() {
		t.Error("WORKING must not be a result status")
	}
}
