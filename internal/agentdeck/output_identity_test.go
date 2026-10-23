package agentdeck

import "testing"

func TestOutputIdentity_ReportedSessionMismatchIsRefused(t *testing.T) {
	if _, err := decodeOutput("queried", `{"success":true,"session_id":"different","content":"PASS"}`, 100); err == nil {
		t.Fatal("a different session was silently stamped as the queried session")
	}
	out, err := decodeOutput("queried", `{"success":true,"session_id":"queried","content":"answer"}`, 100)
	if err != nil || out.Model != "" {
		t.Fatalf("missing model must remain unknown: %+v %v", out, err)
	}
}
