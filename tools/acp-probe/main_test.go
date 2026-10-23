package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNotificationEvidenceRetainsLateModelMetadata(t *testing.T) {
	var wire bytes.Buffer
	c := &client{w: bufio.NewWriter(&wire), msgs: make(chan response, 62), trace: &evidence{}}
	for i := 0; i < 60; i++ {
		c.msgs <- response{Method: "session/update", Params: json.RawMessage(`{"text":"busy"}`)}
	}
	body, err := json.Marshal(struct {
		Text  string `json:"text"`
		Model string `json:"model"`
	}{Text: strings.Repeat("x", 500), Model: "measured-model"})
	if err != nil {
		t.Fatal(err)
	}
	c.msgs <- response{Method: "session/update", Params: body}
	id := 1
	c.msgs <- response{ID: &id, Result: json.RawMessage(`{"stopReason":"end_turn"}`)}
	if err := c.call("session/prompt", nil, time.Second, nil); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := c.trace.writeTrace(out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "acp-capability-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved evidence
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.NotificationCount != 61 || saved.NotificationDropped != 0 || len(saved.Notifications) != 61 {
		t.Fatalf("incomplete trace: count=%d dropped=%d retained=%d", saved.NotificationCount, saved.NotificationDropped, len(saved.Notifications))
	}
	if got := saved.Notifications[60]["body"]; got != string(body) {
		t.Fatalf("late model evidence changed: %v", got)
	}
}

func TestNotificationEvidenceReportsBothLimitsWithoutPartialBodies(t *testing.T) {
	for _, limit := range []string{"count", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			c := &client{trace: &evidence{}}
			if limit == "count" {
				for i := 0; i < maxTraceNotifications; i++ {
					c.record(response{Method: "session/update", Params: json.RawMessage(`{}`)})
				}
			} else {
				c.record(response{Method: "session/update", Params: json.RawMessage(`"` + strings.Repeat("x", maxTraceNotificationBytes) + `"`)})
			}
			before := len(c.trace.Notifications)
			if limit == "count" {
				c.record(response{Method: "session/update", Params: json.RawMessage(`{"model":"late"}`)})
			}
			if c.trace.NotificationDropped != 1 || c.trace.NotificationBytes > maxTraceNotificationBytes {
				t.Fatalf("unreported overflow: %+v", c.trace)
			}
			if len(c.trace.Notifications) != before {
				t.Fatal("overflow retained a partial body")
			}
		})
	}
}

func TestCallDeadlineDoesNotResetOnNotifications(t *testing.T) {
	c := &client{w: bufio.NewWriter(&bytes.Buffer{}), msgs: make(chan response, 256), trace: &evidence{}}
	go func() {
		defer close(c.msgs)
		until := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(until) {
			c.msgs <- response{Method: "session/update"}
			time.Sleep(time.Millisecond)
		}
	}()
	start := time.Now()
	if err := c.call("initialize", nil, 20*time.Millisecond, nil); err == nil {
		t.Fatal("unanswered call succeeded")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("notification traffic extended deadline: %s", elapsed)
	}
}

func TestProbeEarlyExitRetainsEvidenceAndCleansUp(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "server")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "evidence")
	if code := runProbe([]string{"--launcher", launcher, "--cwd", root, "--out", out, "--timeout", "2s"}); code == 0 {
		t.Fatal("early exit succeeded")
	}
	data, err := os.ReadFile(filepath.Join(out, "acp-capability-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ev evidence
	if err = json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Status != "FAILED" || ev.Failure == "" || ev.PID <= 0 || ev.EndedAt == "" {
		t.Fatalf("missing failure evidence: %+v", ev)
	}
	if ev.CleanupError != "" {
		t.Fatalf("cleanup failed: %s", ev.CleanupError)
	}
	if err = syscall.Kill(ev.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned process remains: %v", err)
	}
}

func TestProbeOverallDeadlineRetainsEvidence(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "server")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "evidence")
	start := time.Now()
	if code := runProbe([]string{"--launcher", launcher, "--cwd", root, "--out", out, "--timeout", "50ms"}); code == 0 {
		t.Fatal("silent server succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("overall deadline was not bounded")
	}
	data, err := os.ReadFile(filepath.Join(out, "acp-capability-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("context deadline exceeded")) {
		t.Fatalf("deadline not recorded: %s", data)
	}
}

func TestBoundedStderrCapture(t *testing.T) {
	var buf bytes.Buffer
	w := boundedWriter{w: &buf, remaining: 4}
	if n, err := w.Write([]byte("abcdefgh")); err != nil || n != 8 {
		t.Fatalf("write: %d %v", n, err)
	}
	_, _ = w.Write([]byte("more"))
	if buf.String() != "abcd" {
		t.Fatal(buf.String())
	}
}

func TestPromptSelectionRejectsUnknownModelWithoutRPC(t *testing.T) {
	var wire bytes.Buffer
	c := &client{w: bufio.NewWriter(&wire), msgs: make(chan response, 1), trace: &evidence{}}
	if err := c.selectPromptConfiguration("owned", "unknown", nil); err == nil {
		t.Fatal("unknown model accepted")
	}
	if wire.Len() != 0 {
		t.Fatal("unsupported model reached transport")
	}
}

func TestPromptSelectionDoesNotInventReadbackOrAnswerModel(t *testing.T) {
	for _, raw := range []string{`{"configOptions":[{"id":"model","currentValue":"other"}]}`, `{"configOptions":[]}`} {
		var wire bytes.Buffer
		ch := make(chan response, 1)
		id := 1
		ch <- response{ID: &id, Result: json.RawMessage(raw)}
		c := &client{w: bufio.NewWriter(&wire), msgs: ch, trace: &evidence{}}
		options := []map[string]any{{"id": "model", "options": []any{map[string]any{"value": "selected"}}}}
		err := c.selectPromptConfiguration("owned", "selected", options)
		if err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatal(err)
		}
		if c.trace.AnswerModel != "" || c.trace.ConfiguredModel == "selected" {
			t.Fatal("invented model attribution", c.trace)
		}
	}
}

func TestPromptSelectionUsesEffortAdvertisedAfterModelChange(t *testing.T) {
	for _, tc := range []struct {
		name, actual string
		advertised   bool
		wantError    bool
	}{
		{"max", "max", true, false},
		{"mismatched readback", "high", true, true},
		{"unadvertised", "max", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			ch := make(chan response, 3)
			options := []map[string]any{
				{"id": "model", "currentValue": "preview", "options": []any{map[string]any{"value": "preview"}}},
				{"id": "permissionMode", "currentValue": "default", "options": []any{map[string]any{"value": "default"}}},
			}
			if tc.advertised {
				options = append(options, map[string]any{"id": "thinkingEffort", "currentValue": "default", "options": []any{map[string]any{"value": "max"}}})
			}
			for i := 1; i <= 3; i++ {
				if i == 3 && tc.advertised {
					options[2]["currentValue"] = tc.actual
				}
				raw, err := json.Marshal(map[string]any{"configOptions": options})
				if err != nil {
					t.Fatal(err)
				}
				id := i
				ch <- response{ID: &id, Result: raw}
			}
			c := &client{w: bufio.NewWriter(&wire), msgs: ch, outDir: t.TempDir(), trace: &evidence{}}
			initial := []map[string]any{{"id": "model", "options": []any{map[string]any{"value": "preview"}}}}
			err := c.selectPromptConfiguration("owned", "preview", initial, "max")
			if (err != nil) != tc.wantError {
				t.Fatalf("selection: %v", err)
			}
			if !tc.advertised && strings.Contains(wire.String(), `"configId":"thinkingEffort"`) {
				t.Fatal("unadvertised effort reached transport")
			}
			if !tc.wantError && (c.trace.ConfiguredEffort != "max" || c.trace.RequestedEffort != "max") {
				t.Fatal("max effort not read back", c.trace)
			}
		})
	}
}

func TestWorkspaceHistoryListingDoesNotActivateOrPrompt(t *testing.T) {
	var wire bytes.Buffer
	ch := make(chan response, 2)
	for i, raw := range []string{
		`{"sessions":[{"sessionId":"m27","cwd":"/inspection","title":"M2.7 inspection"}],"nextCursor":"next"}`,
		`{"sessions":[{"sessionId":"m31","cwd":"/inspection"}]}`,
	} {
		id := i + 1
		ch <- response{ID: &id, Result: json.RawMessage(raw)}
	}
	c := &client{w: bufio.NewWriter(&wire), msgs: ch, outDir: t.TempDir(), trace: &evidence{}}
	if err := c.listWorkspaceSessions("/inspection"); err != nil {
		t.Fatal(err)
	}
	if len(c.trace.Sessions) != 2 || c.trace.Sessions[1].Title != "" || c.trace.SessionID != "" {
		t.Fatal("history identity or unnamed title was fabricated", c.trace)
	}
	scanner := bufio.NewScanner(&wire)
	for scanner.Scan() {
		var request struct {
			Method string `json:"method"`
			Params struct {
				Cwd string `json:"cwd"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		if request.Method != "session/list" || request.Params.Cwd != "/inspection" {
			t.Fatal("history check performed another action", request)
		}
	}
}

func TestHistoryListingRejectsOtherWorkspaceAndRepeatedCursor(t *testing.T) {
	for _, raw := range []string{
		`{"sessions":[{"sessionId":"unrelated","cwd":"/other"}]}`,
		`{"sessions":[],"nextCursor":"repeated"}`,
	} {
		var wire bytes.Buffer
		ch := make(chan response, 2)
		for i := 1; i <= 2; i++ {
			id := i
			ch <- response{ID: &id, Result: json.RawMessage(raw)}
		}
		c := &client{w: bufio.NewWriter(&wire), msgs: ch, trace: &evidence{}}
		if err := c.listWorkspaceSessions("/inspection"); err == nil {
			t.Fatal("invalid listing accepted", raw)
		}
		if len(c.trace.Sessions) != 0 {
			t.Fatal("unrelated history retained")
		}
	}
}

func TestHistoryOnlyRefusesPromptCombinationBeforeSpawn(t *testing.T) {
	if code := runProbe([]string{"--list-sessions", "--probe-prompt"}); code != 2 {
		t.Fatal("conflicting read-only and prompt modes accepted", code)
	}
}
