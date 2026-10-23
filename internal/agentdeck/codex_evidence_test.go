package agentdeck

import (
	"encoding/json"
	"strings"
	"testing"
)

func evidenceTimeline(events ...string) string {
	rows := make([]map[string]any, 0, len(events))
	for i, event := range events {
		rows = append(rows, map[string]any{"seq": i + 1, "raw": json.RawMessage(event)})
	}
	b, _ := json.Marshal(map[string]any{"session": map[string]string{"harness": "codex", "native_id": "thread"}, "turns": rows})
	return string(b)
}

func TestCodexEvidence_CompletedTurnToolsWithoutModel(t *testing.T) {
	meta := `{"type":"session_meta","payload":{"id":"thread"}}`
	start := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}`
	finish := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn","last_agent_message":"artifact"}}`
	tool := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"call","output":"Chunk ID: abc123\nexit=1"}}`
	prior := strings.ReplaceAll(tool, "abc123", "prior")
	after := strings.ReplaceAll(tool, "abc123", "after")
	document := evidenceTimeline(meta, prior, start, tool, finish, after)
	evidence, err := codexTimelineEvidence(document, "thread:turn", "artifact")
	if err != nil || !strings.Contains(string(evidence), "abc123") || strings.Contains(string(evidence), "prior") || strings.Contains(string(evidence), "after") {
		t.Fatalf("evidence=%s err=%v", evidence, err)
	}
	if model, err := codexTimelineModel(document, "thread:turn", "artifact"); err == nil || model != "" {
		t.Fatal("missing model metadata supplied attribution")
	}
	for name, doc := range map[string]string{
		"wrong session":           strings.Replace(document, `"native_id":"thread"`, `"native_id":"other"`, 1),
		"gap":                     strings.Replace(document, `"seq":2`, `"seq":20`, 1),
		"overlap":                 evidenceTimeline(meta, start, strings.Replace(start, `"turn"`, `"other"`, 1), tool, finish),
		"aborted":                 evidenceTimeline(meta, start, `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn"}}`, finish),
		"wrong producer metadata": evidenceTimeline(meta, start, `{"type":"response_item","payload":{"type":"function_call_output","output":"unrelated","internal_chat_message_metadata_passthrough":{"turn_id":"other"}}}`, finish),
		"unfinished":              evidenceTimeline(meta, start, tool),
		"wrong bytes":             strings.Replace(document, `"last_agent_message":"artifact"`, `"last_agent_message":"another artifact"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codexTimelineEvidence(doc, "thread:turn", "artifact"); err == nil {
				t.Fatal("uncorrelated evidence accepted")
			}
		})
	}
}
