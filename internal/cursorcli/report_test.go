package cursorcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func launcher(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestReportEnvelopeAndDurableSpawn(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "session_id": "session-1", "result": "```json\n{\"ok\":true}\n```"})
	p := launcher(t, fmt.Sprintf("printf '%%s' '%s'", raw))
	spawned := false
	turn, err := Run(context.Background(), Options{Executable: p, Model: "composer-2.5", Workspace: t.TempDir(), Prompt: "data", OnSpawned: func(pid int) error { spawned = pid > 0; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !spawned || turn.Text != `{"ok":true}` || turn.SessionID != "session-1" {
		t.Fatalf("%+v", turn)
	}
}
func TestReportFailedSpawnAndDeadline(t *testing.T) {
	p := launcher(t, "sleep 60")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, Options{Executable: p, Model: "test", Workspace: t.TempDir(), OnSpawned: func(int) error { return nil }})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatal("unbounded cancellation", err)
	}
	_, err = Run(context.Background(), Options{Executable: p, Model: "test", Workspace: t.TempDir(), OnSpawned: func(int) error { return fmt.Errorf("record refused") }})
	if err == nil {
		t.Fatal("spawn recording failure ignored")
	}
}
