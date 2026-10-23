package agentdeck

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Captured through the pinned upstream's public CLI from a real Codex report
// on 2026-10-11. Irrelevant metadata was omitted; completion bytes are unchanged.
func TestCodexAttribution_RealCompletedReport(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-live-turn-attribution.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Turns []struct {
			Raw struct {
				Payload struct {
					TurnID      string `json:"turn_id"`
					LastMessage string `json:"last_agent_message"`
				} `json:"payload"`
			} `json:"raw"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	last := doc.Turns[len(doc.Turns)-1].Raw.Payload
	generation := "01a12a24-4522-7002-a2f8-98c9e7508c7c:" + last.TurnID
	model, err := codexTimelineModel(string(b), generation, last.LastMessage)
	if err != nil || model != "gpt-6.1-sol" {
		t.Fatalf("model=%q err=%v", model, err)
	}
}

func TestCodexAttribution_PublicCLIJoin(t *testing.T) {
	document, err := os.ReadFile("testdata/codex-live-turn-attribution.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Turns []struct {
			Raw struct {
				Payload struct {
					LastMessage string `json:"last_agent_message"`
				} `json:"payload"`
			} `json:"raw"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	output := map[string]any{"success": true, "session_id": "deck-session", "tool": "codex", "role": "assistant", "conversation_id": "01a12a24-4522-7002-a2f8-98c9e7508c7c", "codex_turn_generation": "01a12a24-4522-7002-a2f8-98c9e7508c7c:01a12a24-5ace-7a21-8b9e-efc15ad6056d", "content": doc.Turns[len(doc.Turns)-1].Raw.Payload.LastMessage}
	writeOutput := func() {
		b, err := json.Marshal(output)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "output.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeOutput()
	if err := os.WriteFile(filepath.Join(dir, "timeline.json"), document, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(filepath.Join(dir, "calls")) + "\ncase \"$3 $4\" in\n'session output') cat " + quote(filepath.Join(dir, "output.json")) + ";;\n'recall timeline') cat " + quote(filepath.Join(dir, "timeline.json")) + ";;\n*) exit 7;;\nesac\n"
	cli := filepath.Join(dir, "agent-deck")
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runtime := NewCLIRuntime(cli, "pilot")
	answer, err := runtime.Output(context.Background(), "deck-session", 16000)
	if err != nil || answer.Model != "gpt-6.1-sol" || answer.SessionID != "deck-session" || answer.NativeTurn != output["codex_turn_generation"] || len(answer.Evidence) == 0 {
		t.Fatalf("%+v %v", answer, err)
	}
	trace, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if string(trace) != "-p pilot session output deck-session --json --max-tokens 16000\n-p pilot recall timeline 01a12a24-4522-7002-a2f8-98c9e7508c7c --json --v1\n" {
		t.Fatalf("unexpected public operations: %s", trace)
	}
	output["conversation_id"] = "wrong-thread"
	writeOutput()
	if _, err := runtime.Output(context.Background(), "deck-session", 16000); err == nil {
		t.Fatal("accepted inconsistent public output identity")
	}
	output["conversation_id"] = "01a12a24-4522-7002-a2f8-98c9e7508c7c"
	output["model"] = "swapped"
	writeOutput()
	if _, err := runtime.Output(context.Background(), "deck-session", 16000); err == nil {
		t.Fatal("accepted conflicting output model")
	}
	delete(output, "model")
	writeOutput()
	if err := os.Remove(filepath.Join(dir, "calls")); err != nil {
		t.Fatal(err)
	}
	// A fresh conversation needs one bounded public index refresh. This is
	// metadata work, not another send, model invocation or configuration write.
	script = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(filepath.Join(dir, "calls")) + "\ncase \"$3 $4\" in\n'session output') cat " + quote(filepath.Join(dir, "output.json")) + ";;\n'recall timeline') if [ ! -f " + quote(filepath.Join(dir, "indexed")) + " ]; then printf '%s\\n' '{\"code\":\"NOT_FOUND\",\"success\":false}'; exit 0; fi; cat " + quote(filepath.Join(dir, "timeline.json")) + ";;\n'recall search') touch " + quote(filepath.Join(dir, "indexed")) + "; printf '%s\\n' '{\"success\":true}';;\n*) exit 7;;\nesac\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	answer, err = runtime.Output(context.Background(), "deck-session", 16000)
	if err != nil || answer.Model != "gpt-6.1-sol" {
		t.Fatalf("fresh conversation: %+v %v", answer, err)
	}
	trace, err = os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(trace), "recall timeline") != 2 || strings.Count(string(trace), "recall search") != 1 || strings.Contains(string(trace), "config") {
		t.Fatalf("unbounded or mutating index recovery: %s", trace)
	}
	// Reader disablement is an operator decision; an output read cannot change it.
	if err := os.Remove(filepath.Join(dir, "calls")); err != nil {
		t.Fatal(err)
	}
	script = strings.Replace(script, `{"code":"NOT_FOUND","success":false}`, `{"code":"INVALID_OPERATION","success":false}`, 1)
	if err := os.Remove(filepath.Join(dir, "indexed")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if answer, err := runtime.Output(context.Background(), "deck-session", 16000); err != nil || answer.Model != "" {
		t.Fatalf("missing optional attribution must remain unknown: %+v %v", answer, err)
	}
	trace, err = os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(trace), "recall search") || strings.Contains(string(trace), "config") {
		t.Fatalf("disabled reader caused a side effect: %s", trace)
	}
}

func TestCodexAttribution_RefusesUncorrelatedEvidence(t *testing.T) {
	base := `{"session":{"harness":"codex","native_id":"thread","model":"invented"},"turns":[
	{"seq":1,"raw":{"type":"session_meta","payload":{"id":"thread"}}},
	{"seq":2,"raw":{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}},
	{"seq":3,"raw":{"type":"turn_context","payload":{"turn_id":"turn","model":"measured"}}},
	{"seq":4,"raw":{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn","last_agent_message":"answer"}}}]}`
	for _, tc := range []struct{ name, document, generation, answer string }{
		{"wrong thread", base, "other:turn", "answer"},
		{"wrong turn", base, "thread:other", "answer"},
		{"different final bytes", base, "thread:turn", "answer "},
		{"session model alone", strings.Replace(base, `"model":"measured"`, `"model":""`, 1), "thread:turn", "answer"},
		{"context for another turn", strings.Replace(base, `"type":"turn_context","payload":{"turn_id":"turn"`, `"type":"turn_context","payload":{"turn_id":"other"`, 1), "thread:turn", "answer"},
		{"interrupted", strings.Replace(base, `"type":"task_complete"`, `"type":"turn_aborted"`, 1), "thread:turn", "answer"},
		{"not completed", strings.Replace(base, `"type":"task_complete"`, `"type":"item_completed"`, 1), "thread:turn", "answer"},
		{"missing start", strings.Replace(base, `"type":"task_started"`, `"type":"item_completed"`, 1), "thread:turn", "answer"},
		{"wrong metadata", strings.Replace(base, `"id":"thread"`, `"id":"other"`, 1), "thread:turn", "answer"},
		{"missing row", strings.Replace(base, `"seq":3`, `"seq":8`, 1), "thread:turn", "answer"},
		{"malformed identity", base, "thread:turn:other", "answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if model, err := codexTimelineModel(tc.document, tc.generation, tc.answer); err == nil || model != "" {
				t.Fatalf("accepted %q: %v", model, err)
			}
		})
	}
	if model, err := codexTimelineModel(base, "thread:turn", "answer"); err != nil || model != "measured" {
		t.Fatalf("valid join: %q %v", model, err)
	}
	// A second context cannot silently change the model underneath one answer.
	conflict := strings.Replace(base, `{"seq":4,`, `{"seq":4,"raw":{"type":"turn_context","payload":{"turn_id":"turn","model":"swapped"}}},{"seq":5,`, 1)
	if _, err := codexTimelineModel(conflict, "thread:turn", "answer"); err == nil {
		t.Fatal("accepted conflicting model")
	}
	duplicate := strings.Replace(base, `{"seq":4,`, `{"seq":4,"raw":{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn","last_agent_message":"answer"}}},{"seq":5,`, 1)
	if _, err := codexTimelineModel(duplicate, "thread:turn", "answer"); err == nil {
		t.Fatal("accepted duplicate completion")
	}
}

func TestCommandBuffer_ConsumesButRefusesOverflow(t *testing.T) {
	b := cappedCommandBuffer{limit: 4}
	if n, err := b.Write([]byte("abcd")); n != 4 || err != nil || b.overflow {
		t.Fatal(n, err, b.overflow)
	}
	if n, err := b.Write([]byte("efgh")); n != 4 || err != nil || !b.overflow || b.String() != "abcd" {
		t.Fatal(n, err, b.String())
	}
}
