package contract

import (
	"strings"
	"testing"
)

func TestStructuredTaskSchema(t *testing.T) {
	task := &StructuredTask{Function: "report-analysis", SchemaName: "report", SchemaJSON: `{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}},"additionalProperties":false}`, User: "evidence"}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"ok":true}`, `{"ok":false}`} {
		if err := task.ValidateOutput(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{}`, `{"ok":"true"}`, `{"ok":true,"extra":1}`, `[]`, `{"ok":true} {}`} {
		if err := task.ValidateOutput(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	task.SchemaJSON = `{"$ref":"file:///etc/passwd"}`
	if err := task.Validate(); err == nil {
		t.Fatal("external schema resource accepted")
	}
	task.SchemaJSON = `{"type":"object"}`
	task.User = strings.Repeat("x", 512*1024)
	if err := task.Validate(); err == nil {
		t.Fatal("oversized input accepted")
	}
	task.User = "evidence"
	task.Function = "code-repair"
	if err := task.Validate(); err == nil {
		t.Fatal("repair accepted")
	}
}
func TestStructuredTaskUTF8Digest(t *testing.T) {
	task := &StructuredTask{Function: "semantic-authoring", SchemaName: "example", SchemaJSON: `{"type":"object"}`, System: "Evidence only", User: "测试"}
	if task.Digest() != "572c411dc48334ccd68a330e96b8a907ff48a5eb71f0a678091cd81a77637a57" {
		t.Fatal(task.Digest())
	}
}
