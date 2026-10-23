package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestIngress_ObjectCompletionMeasuresBodyAndRejectsFalseDigest(t *testing.T) {
	f := completionFixture(t)
	auditedRequest(t, f)
	body := judgedCompletionBody(t, f)
	s := mustState(t, f, "forge-run-1842-repair")
	var wire map[string]any
	if err := json.Unmarshal([]byte(testReviewResponse(s, OutcomePass, body)), &wire); err != nil {
		t.Fatal(err)
	}
	wire["report_body"] = json.RawMessage(body)
	delete(wire, "report_sha256")
	rt := newFakeRuntime()
	e := profileEngine(t, f, rt)
	setOutput := func() {
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		rt.outputs[s.AuditorSession] = string(raw)
	}
	setOutput()
	answer, err := e.CollectReviewerAnswer(context.Background(), s.RequestID, 0)
	if err != nil || answer.ReportDigest != sha256Hex([]byte(answer.Body)) || !answer.Trusted {
		t.Fatalf("object report not measured: %+v %v", answer, err)
	}
	wire["report_sha256"] = strings.Repeat("0", 64)
	setOutput()
	if _, err := e.CollectReviewerAnswer(context.Background(), s.RequestID, 0); err == nil {
		t.Fatal("false supplied digest accepted")
	}
	delete(wire, "report_sha256")
	wire["answer_nonce"] = "stale"
	setOutput()
	if _, err := e.CollectReviewerAnswer(context.Background(), s.RequestID, 0); err == nil {
		t.Fatal("stale object report accepted")
	}
	if _, err := f.store.SubmitAnswer(s.RequestID, answer); err != nil {
		t.Fatal(err)
	}
	after := mustState(t, f, s.RequestID)
	if !after.Stage.Terminal() {
		t.Fatalf("canonical completion did not settle: %s", after.Stage)
	}
}

func TestIngress_ObjectReportRefusesNonApprovingAndUntypedBodies(t *testing.T) {
	for _, raw := range []string{`{"kind":"COMPLETION","outcome":"FAIL","report_body":{}}`, `{"kind":"COMPLETION","outcome":"PASS","report_body":[]}`} {
		if _, err := decodeReviewEnvelope(raw); err == nil {
			t.Fatalf("invalid body accepted: %s", raw)
		}
	}
}

func TestCompletionRecovery_ExistingAnswerSettlesWithoutReplacementAndPendingSpawnRefuses(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiescent", true: "unidentified-spawn"}[pending], func(t *testing.T) {
			f := completionFixture(t)
			auditedRequest(t, f)
			body := judgedCompletionBody(t, f)
			id := "forge-run-1842-repair"
			if _, err := f.store.UpdateCommitted(id, func(s *State) error {
				s.Episode = &EpisodeRoot{EpisodeID: id, Phase: string(PhaseCompletionReview)}
				if pending {
					s.OwnedProcesses = map[string]OwnedProcess{"worker": {Pending: true}}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s := mustState(t, f, id)
			rt := newFakeRuntime()
			rt.outputs[s.AuditorSession] = testReviewResponse(s, OutcomePass, body)
			e := profileEngine(t, f, rt)
			handled, err := e.RecoverCompletedEpisodeReview(context.Background(), id)
			if !handled {
				t.Fatal("pending completion escaped to ordinary startup")
			}
			after := mustState(t, f, id)
			if pending {
				if err == nil || after.Stage.Terminal() || after.ActiveReview == nil {
					t.Fatalf("unknown writer permitted approval: %v %+v", err, after.ActiveReview)
				}
			} else if err != nil || !after.Stage.Terminal() {
				t.Fatalf("existing correlated answer did not settle: %v %s", err, after.Stage)
			}
		})
	}
}
