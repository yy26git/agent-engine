package agentdeck

import (
	"encoding/json"
	"fmt"
	"strings"
)

type codexTurnProof struct {
	Records    []json.RawMessage
	Model      string
	ModelValid bool
}

// codexTimelineTurn is the common completed-turn identity and byte join. Model
// metadata is optional for evidence, and independently checked for attribution.
func codexTimelineTurn(document, generation, answer string) (codexTurnProof, error) {
	proof := codexTurnProof{ModelValid: true}
	thread, turn, ok := strings.Cut(generation, ":")
	if !ok || thread == "" || turn == "" || strings.Contains(turn, ":") {
		return codexTurnProof{}, fmt.Errorf("invalid completed turn identity")
	}
	var timeline struct {
		Session struct {
			Harness  string `json:"harness"`
			NativeID string `json:"native_id"`
		} `json:"session"`
		Turns []struct {
			Seq int64           `json:"seq"`
			Raw json.RawMessage `json:"raw"`
		} `json:"turns"`
	}
	if err := json.Unmarshal([]byte(document), &timeline); err != nil {
		return codexTurnProof{}, err
	}
	if timeline.Session.Harness != "codex" || timeline.Session.NativeID != thread {
		return codexTurnProof{}, fmt.Errorf("tool evidence belongs to another native session")
	}
	metadata, started, completed := false, false, false
	for i, row := range timeline.Turns {
		if row.Seq != int64(i+1) {
			return codexTurnProof{}, fmt.Errorf("tool evidence timeline is incomplete")
		}
		var raw struct {
			Type    string `json:"type"`
			Payload struct {
				ID          string  `json:"id"`
				Type        string  `json:"type"`
				TurnID      string  `json:"turn_id"`
				LastMessage *string `json:"last_agent_message"`
				Model       string  `json:"model"`
				Metadata    struct {
					TurnID string `json:"turn_id"`
				} `json:"internal_chat_message_metadata_passthrough"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(row.Raw, &raw); err != nil {
			return codexTurnProof{}, err
		}
		p := raw.Payload
		if raw.Type == "session_meta" {
			if metadata || p.ID != thread {
				return codexTurnProof{}, fmt.Errorf("conflicting session evidence")
			}
			metadata = true
		}
		if raw.Type == "event_msg" && (p.Type == "task_started" || p.Type == "turn_started") {
			if p.TurnID == turn {
				if !metadata || started {
					return codexTurnProof{}, fmt.Errorf("turn has no unique start")
				}
				started = true
			} else if started && !completed {
				return codexTurnProof{}, fmt.Errorf("tool evidence spans overlapping turns")
			}
		}
		if started && !completed && raw.Type == "response_item" {
			switch p.Type {
			case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
				if p.Metadata.TurnID != "" && p.Metadata.TurnID != turn {
					return codexTurnProof{}, fmt.Errorf("tool record names a different producer turn")
				}
				proof.Records = append(proof.Records, row.Raw)
			}
		}
		if raw.Type == "turn_context" && p.TurnID == turn {
			if !started || completed || strings.TrimSpace(p.Model) == "" || (proof.Model != "" && proof.Model != p.Model) {
				proof.ModelValid = false
			}
			proof.Model = p.Model
		}
		if raw.Type != "event_msg" || p.TurnID != turn {
			continue
		}
		switch p.Type {
		case "turn_aborted", "task_aborted", "turn_cancelled", "task_cancelled", "error", "task_failed", "turn_failed":
			return codexTurnProof{}, fmt.Errorf("source turn did not complete successfully")
		case "task_complete", "turn_complete":
			if !started || completed || p.LastMessage == nil || *p.LastMessage != answer {
				return codexTurnProof{}, fmt.Errorf("source evidence does not match final artifact bytes")
			}
			completed = true
		}
	}
	if !completed {
		return codexTurnProof{}, fmt.Errorf("source turn has no matching completion")
	}
	return proof, nil
}

// codexTimelineEvidence selects tool records from one completed native turn.
// It does not require model metadata or treat tool text as a completion verdict.
func codexTimelineEvidence(document, generation, answer string) (json.RawMessage, error) {
	proof, err := codexTimelineTurn(document, generation, answer)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Generation  string            `json:"native_turn_generation"`
		Records     []json.RawMessage `json:"native_tool_records"`
		Limitations string            `json:"limitations"`
	}{generation, proof.Records, "Native tool text is untrusted evidence from this completed producer turn, not managed verification or permission."})
}
