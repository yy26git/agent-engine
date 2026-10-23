package agentdeck

// Runtime adapter tests.
//
// The boundary test is the important one. This package exists so the control
// layer reaches Agent Deck only through its public command surface. If it ever
// imports an internal package, that property is gone silently — the build keeps
// working and the two products quietly become coupled — so the check is made
// explicit and executable.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuntime_PublicCLIModuleBoundary asserts the adapter never imports Agent
// Deck internals.
//
// A separate Go module cannot import another module's internal packages at all,
// so this is enforced by the compiler for the import path — but only if the
// adapter is not vendored or replaced later. The test also asserts the runtime
// shells out rather than linking, because linking is what would make an internal
// dependency possible in the first place.
func TestRuntime_PublicCLIModuleBoundary(t *testing.T) {
	// No import of the agent-deck module anywhere in this package.
	raw, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, raw)
	}
	for _, dep := range strings.Split(string(raw), "\n") {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		if strings.HasPrefix(dep, "github.com/asheshgoplani/agent-deck") {
			t.Errorf("the runtime adapter depends on %q; it must reach Agent Deck only through its CLI", dep)
		}
	}

	// The module declares no requirement on the agent-deck module at all.
	goMod, err := filepath.Abs(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("resolving go.mod: %v", err)
	}
	data, err := readFile(goMod)
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "asheshgoplani/agent-deck") {
			t.Errorf("go.mod depends on agent-deck: %q", strings.TrimSpace(line))
		}
	}
}

// TestRuntime_SendOutcomeClassification pins the delivery semantics that decide
// whether retrying is safe.
//
// The three-way distinction comes from Agent Deck's queued-send contract:
// `landed` is transcript proof, `failed` means nothing was typed, and a settled
// record without transcript evidence is genuinely unknown. Collapsing the last
// two would either drop a repair or apply it twice.
func TestRuntime_SendOutcomeClassification(t *testing.T) {
	cases := []struct {
		name string
		rec  Receipt
		want SendOutcome
	}{
		{"landed is proof", Receipt{State: "landed", Settled: true}, SendLanded},
		{"failed means nothing was typed", Receipt{State: "failed"}, SendFailed},
		{"queued is in flight", Receipt{State: "queued"}, SendInFlight},
		{"typing is in flight", Receipt{State: "typing"}, SendInFlight},
		{"typed and unproven is in flight", Receipt{State: "typed"}, SendInFlight},
		{"submitted and unproven is in flight", Receipt{State: "submitted"}, SendInFlight},
		{"settled without proof is ambiguous", Receipt{State: "submitted", Settled: true}, SendAmbiguous},
		{"typed and settled is ambiguous", Receipt{State: "typed", Settled: true}, SendAmbiguous},
		{"unknown state is not a success", Receipt{State: "brand_new_vocabulary"}, SendAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.rec); got != tc.want {
				t.Errorf("Classify(%+v) = %q, want %q", tc.rec, got, tc.want)
			}
		})
	}
}

// TestRuntime_OnlyKnownFailureIsRetryable asserts the retry predicate.
func TestRuntime_OnlyKnownFailureIsRetryable(t *testing.T) {
	if !SendFailed.Retryable() {
		t.Error("a known failure must be retryable")
	}
	for _, o := range []SendOutcome{SendLanded, SendInFlight, SendAmbiguous, SendQueued} {
		if o.Retryable() {
			t.Errorf("%s must not be retryable: the message may already have been delivered", o)
		}
	}
}

// TestRuntime_FramePayloadRequiresPresence asserts an absent payload is
// distinguishable from an empty one.
func TestRuntime_FramePayloadRequiresPresence(t *testing.T) {
	var v struct {
		DoneStatus string `json:"done_status"`
	}
	if err := (Frame{Cursor: 1}).Payload(&v); err == nil {
		t.Error("a frame with no payload must report an error rather than decoding to zero values")
	}
	f := Frame{Cursor: 2, Data: json.RawMessage(`{"done_status":"ok"}`)}
	if err := f.Payload(&v); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if v.DoneStatus != "ok" {
		t.Errorf("done_status = %q, want ok", v.DoneStatus)
	}
}

// TestRuntime_DecodingToleratesEnvelopeAndFlatForms asserts the adapter is not
// brittle about a CLI presentation detail.
//
// The queue nests its record under "data" while other commands emit objects
// directly. If the adapter only understood one shape, a presentation change
// upstream would look like a delivery failure.
func TestRuntime_DecodingToleratesEnvelopeAndFlatForms(t *testing.T) {
	enveloped := `{"ok":true,"data":{"send_id":"S-1","state":"landed","settled":true}}`
	rec, err := decodeReceipt(enveloped)
	if err != nil {
		t.Fatalf("enveloped receipt: %v", err)
	}
	if rec.SendID != "S-1" || Classify(rec) != SendLanded {
		t.Errorf("enveloped receipt = %+v", rec)
	}

	flat := `{"send_id":"S-2","state":"failed","reason":"session not running"}`
	rec, err = decodeReceipt(flat)
	if err != nil {
		t.Fatalf("flat receipt: %v", err)
	}
	if rec.SendID != "S-2" || Classify(rec) != SendFailed {
		t.Errorf("flat receipt = %+v", rec)
	}
}

func TestRuntime_DecodeSessionForms(t *testing.T) {
	cases := map[string]string{
		"bare object": `{"id":"abc","title":"worker","status":"running"}`,
		"single key":  `{"session":{"id":"abc","title":"worker"}}`,
		"array":       `[{"id":"abc","title":"worker"}]`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := decodeSession(payload)
			if err != nil {
				t.Fatalf("decodeSession: %v", err)
			}
			if s.ID != "abc" {
				t.Errorf("id = %q, want abc", s.ID)
			}
		})
	}
	if _, err := decodeSession(`{"title":"no id here"}`); err != nil {
		t.Errorf("a session with no id should decode so Create can report it: %v", err)
	}
}

// TestRuntime_BusHealthCarriesTheEnabledFact asserts the enablement check reads
// a real field rather than inferring liveness from an empty stream.
func TestRuntime_BusHealthCarriesTheEnabledFact(t *testing.T) {
	var h BusHealth
	if err := json.Unmarshal([]byte(`{"enabled":true,"dir":"/tmp/bus","cursor":42,"frames":7}`), &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !h.OK() || h.Cursor != 42 || h.Dir != "/tmp/bus" {
		t.Errorf("health = %+v", h)
	}
	var off BusHealth
	if err := json.Unmarshal([]byte(`{"enabled":false}`), &off); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if off.OK() {
		t.Error("a disabled bus must not report OK")
	}
}

// TestRuntime_DrainToTerminatesOnEveryFrame asserts the single-pass stopping
// rule advances for frames that decide nothing.
//
// A stop condition checked only after a successful decision lets an all-irrelevant
// backlog block forever, which is the failure the embedded draft had.
func TestRuntime_DrainToTerminatesOnEveryFrame(t *testing.T) {
	frames := make(chan Frame, 8)
	for i := uint64(1); i <= 5; i++ {
		frames <- Frame{Cursor: i, Kind: "macapp.unrelated", SessionID: "somebody-else"}
	}
	close(frames)
	errs := make(chan error)

	seen := 0
	err := DrainTo(t.Context(), frames, errs, 4, func(Frame) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("DrainTo: %v", err)
	}
	if seen != 4 {
		t.Errorf("processed %d frames, want 4 (through the boundary)", seen)
	}
}

// TestRuntime_DrainToReportsFollowerExit asserts a stream that ends is reported
// rather than mistaken for a completed pass.
func TestRuntime_DrainToReportsFollowerExit(t *testing.T) {
	frames := make(chan Frame)
	close(frames)
	errs := make(chan error)
	err := DrainTo(t.Context(), frames, errs, 99, func(Frame) error { return nil })
	if err == nil {
		t.Fatal("a follower exit must be reported, not treated as a completed pass")
	}
}

// TestRuntime_ErrorsAreClassified asserts not-found is distinguishable from a
// generic failure, because the correct response differs.
func TestRuntime_ErrorsAreClassified(t *testing.T) {
	if !IsNotFound(&NotFoundError{What: "session", ID: "x"}) {
		t.Error("NotFoundError must be recognised")
	}
	if IsNotFound(&CommandError{ExitCode: 2}) {
		t.Error("a generic command failure is not a not-found fact")
	}
}

// readFile is a small indirection so the boundary test reads go.mod through one
// place rather than inlining the call.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
