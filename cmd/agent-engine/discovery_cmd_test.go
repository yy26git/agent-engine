package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

func TestVersion_WithoutStore(t *testing.T) {
	for _, command := range []string{"version", "capabilities"} {
		var out, diag bytes.Buffer
		if run([]string{command, "--json"}, &out, &diag) != 0 {
			t.Fatal(diag.String())
		}
		var row map[string]any
		if err := json.Unmarshal(out.Bytes(), &row); err != nil || row["product"] != "agent-engine" {
			t.Fatal(out.String(), err)
		}
	}
}

func TestDiscovery_NoProcessOrModelCallsAndCallerScope(t *testing.T) {
	root, _ := flowCLIFixture(t)
	cfg, err := control.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	store := control.NewStore(cfg)
	c, err := store.Discover(nil)
	if err != nil || !c.SetupRequired {
		t.Fatal(c, err)
	}
	if _, err := os.Stat(filepath.Join(root, "DEPLOYMENT.json")); !os.IsNotExist(err) {
		t.Fatal("read created deployment")
	}
	d, err := store.InitializeDeployment()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := store.InitializeDeployment()
	if err != nil || d.ID != d2.ID {
		t.Fatal("identity changed", err)
	}
	api := &episodeAPI{store: store, token: strings.Repeat("x", 32), callerPrefix: "codex-", templates: map[string]bool{"other": true}, start: func(string) error { t.Fatal("discovery started supervisor"); return nil }}
	req := httptest.NewRequest(http.MethodGet, "/catalog", nil)
	req.Header.Set("Authorization", "Bearer "+api.token)
	out := httptest.NewRecorder()
	api.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	if err := json.Unmarshal(out.Body.Bytes(), &c); err != nil || len(c.Flows) != 0 || c.DeploymentID != d.ID {
		t.Fatal(c, err)
	}
	api.templates = nil
	out = httptest.NewRecorder()
	api.ServeHTTP(out, req)
	if strings.Contains(out.Body.String(), "launch_env") || strings.Contains(out.Body.String(), "launch_command") || !strings.Contains(out.Body.String(), `"provider_availability":"unknown"`) {
		t.Fatal(out.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "REQUESTS")); !os.IsNotExist(err) {
		t.Fatal("discovery created requests", err)
	}
}
