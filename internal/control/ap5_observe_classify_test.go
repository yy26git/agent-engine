package control

// AP5.2 ACP observation-classification tests.
//
// These live apart from the multi-source supervision tests so that rewriting one
// cannot silently destroy the other: the classification rules and the rules for
// feeding them into the engine fail in opposite directions, and both must hold.

import (
	"encoding/json"
	"testing"
)

func update(sessionUpdate string) []byte {
	b, _ := json.Marshal(map[string]any{
		"sessionId": "sess-1",
		"update":    map[string]any{"sessionUpdate": sessionUpdate},
	})
	return b
}

func updateWithStop(sessionUpdate, stop string) []byte {
	b, _ := json.Marshal(map[string]any{
		"sessionId":  "sess-1",
		"update":     map[string]any{"sessionUpdate": sessionUpdate},
		"stopReason": stop,
	})
	return b
}

// TestClassifyACP_ToolActivityIsNotCompletion is the single most important
// assertion in this file.
//
// A tool call proves the agent is doing something. Treating it as "the work
// finished" is how a supervisor declares victory over an agent that has merely
// started.
func TestClassifyACP_ToolActivityIsNotCompletion(t *testing.T) {
	for _, v := range []string{"tool_call", "tool_call_update"} {
		obs, ok := ClassifyACPNotification("session/update", update(v))
		if !ok {
			t.Fatalf("%s: payload was not readable", v)
		}
		if obs != ACPObsActivity {
			t.Errorf("%s -> %q, want %q", v, obs, ACPObsActivity)
		}
		if obs.Terminal() {
			t.Errorf("%s was treated as ending the turn", v)
		}
	}
}

// TestClassifyACP_OutputChunksAreActivityNotResult covers the other easy
// conflation: the agent produced text, so something must be true.
func TestClassifyACP_OutputChunksAreActivityNotResult(t *testing.T) {
	for _, v := range []string{"agent_message_chunk", "agent_thought_chunk", "plan"} {
		obs, ok := ClassifyACPNotification("session/update", update(v))
		if !ok {
			t.Fatalf("%s: payload was not readable", v)
		}
		if obs != ACPObsActivity || obs.Terminal() {
			t.Errorf("%s -> %q (terminal=%v), want non-terminal activity", v, obs, obs.Terminal())
		}
	}
}

// TestClassifyACP_CompletedTurnIsNotAnEngineeringPass is the core refusal.
//
// The transport can prove a prompt returned. It cannot prove the work passed,
// and it must never be able to say so.
func TestClassifyACP_CompletedTurnIsNotAnEngineeringPass(t *testing.T) {
	obs, ok := ClassifyACPNotification("session/update", updateWithStop("agent_message_chunk", "end_turn"))
	if !ok {
		t.Fatal("payload was not readable")
	}
	if obs != ACPObsTurnCompleted {
		t.Errorf("observation = %q, want %q", obs, ACPObsTurnCompleted)
	}
	if !obs.Terminal() {
		t.Error("a completed turn must be terminal")
	}
	if obs.EngineeringPass() {
		t.Fatal("a completed prompt reported an engineering PASS; only an independent reviewer may")
	}
}

// TestACPObservation_EngineeringPassIsNeverTrue sweeps the whole vocabulary.
// A single future edit that flips one constant must not silently start
// manufacturing passes.
func TestACPObservation_EngineeringPassIsNeverTrue(t *testing.T) {
	for _, obs := range []ACPObservation{
		ACPObsUnknown, ACPObsTurnStarted, ACPObsActivity,
		ACPObsTurnCompleted, ACPObsTurnFailed, ACPObsCancelled,
	} {
		if obs.EngineeringPass() {
			t.Errorf("%q reported an engineering PASS", obs)
		}
	}
}

// TestClassifyACP_UnknownVocabularyIsUnknown is the fail-closed property.
//
// An unrecognised value must cost VISIBILITY, never correctness. Guessing here
// would mean inventing a protocol vocabulary this layer has never observed.
func TestClassifyACP_UnknownVocabularyIsUnknown(t *testing.T) {
	for _, v := range []string{"brand_new_vocabulary", "", "TURN_ENDED_MAYBE"} {
		obs, ok := ClassifyACPNotification("session/update", update(v))
		if !ok {
			t.Fatalf("%q: payload was not readable", v)
		}
		if obs != ACPObsUnknown {
			t.Errorf("%q -> %q, want %q", v, obs, ACPObsUnknown)
		}
		if obs.Terminal() {
			t.Errorf("%q was treated as terminal", v)
		}
	}
}

// TestClassifyACP_UnknownMethodDrivesNothing keeps an unfamiliar notification
// from acquiring meaning just by arriving.
func TestClassifyACP_UnknownMethodDrivesNothing(t *testing.T) {
	for _, m := range []string{"session/something_new", "mcode/session/goal_update"} {
		obs, ok := ClassifyACPNotification(m, update("tool_call"))
		if !ok {
			t.Fatalf("%s: payload was not readable", m)
		}
		if obs != ACPObsUnknown {
			t.Errorf("%s -> %q, want %q", m, obs, ACPObsUnknown)
		}
	}
}

// TestClassifyACP_UnparseablePayloadIsDistinguished separates "I could not read
// this" from "I read it and it means nothing", because those need different
// operator responses.
func TestClassifyACP_UnparseablePayloadIsDistinguished(t *testing.T) {
	if _, ok := ClassifyACPNotification("session/update", []byte("not json")); ok {
		t.Error("an unreadable payload reported as classified")
	}
	if _, ok := ClassifyACPNotification("session/update", nil); ok {
		t.Error("an absent payload reported as classified")
	}
	obs, ok := ClassifyACPNotification("session/update", update("mystery"))
	if !ok || obs != ACPObsUnknown {
		t.Errorf("readable-but-meaningless = %q, %v; want unknown, true", obs, ok)
	}
}

// TestClassifyACP_CancelAndFailureAreDistinguishedFromSuccess.
func TestClassifyACP_CancelAndFailureAreDistinguishedFromSuccess(t *testing.T) {
	cases := map[string]ACPObservation{
		"cancelled": ACPObsCancelled,
		"canceled":  ACPObsCancelled,
		"refusal":   ACPObsTurnFailed,
		"error":     ACPObsTurnFailed,
	}
	for stop, want := range cases {
		obs, ok := ClassifyACPNotification("session/update", updateWithStop("agent_message_chunk", stop))
		if !ok {
			t.Fatalf("%s: payload was not readable", stop)
		}
		if obs != want {
			t.Errorf("stop %q -> %q, want %q", stop, obs, want)
		}
		if !obs.Terminal() {
			t.Errorf("stop %q should be terminal", stop)
		}
	}
}

// TestObservationFromReceipt pins the one place a caller could otherwise invent
// its own reading of a prompt answer.
func TestObservationFromReceipt(t *testing.T) {
	cases := map[string]ACPObservation{
		"end_turn":  ACPObsTurnCompleted,
		"endTurn":   ACPObsTurnCompleted,
		"END_TURN":  ACPObsTurnCompleted,
		"cancelled": ACPObsCancelled,
		"refusal":   ACPObsTurnFailed,
		"":          ACPObsUnknown,
		"who_knows": ACPObsUnknown,
	}
	for reason, want := range cases {
		if got := ObservationFromReceipt(reason); got != want {
			t.Errorf("ObservationFromReceipt(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestACPObservation_VendorUpdateIsActivityNotCompletion covers the vendor
// liveness signal, which is the easiest one to over-read.
func TestACPObservation_VendorUpdateIsActivityNotCompletion(t *testing.T) {
	obs, ok := ClassifyACPNotification("mcode/session/current_session_update", update("tool_call"))
	if !ok {
		t.Fatal("payload was not readable")
	}
	if obs != ACPObsActivity || obs.Terminal() || obs.EngineeringPass() {
		t.Errorf("vendor update -> %q (terminal=%v pass=%v), want plain activity",
			obs, obs.Terminal(), obs.EngineeringPass())
	}
}
