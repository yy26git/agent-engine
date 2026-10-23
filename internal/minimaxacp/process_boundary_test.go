package minimaxacp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	stdRuntime "runtime"
	"strings"
	"testing"
	"time"
)

func TestProcessBoundaryRefusesUnverifiedPolicyBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "launcher")
	body := []byte("#!/bin/sh\nexit 99\n")
	if err := os.WriteFile(launcher, body, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	spawned := false
	c, err := Start(context.Background(), Options{Launcher: launcher, LauncherSHA256: hex.EncodeToString(sum[:]), Cwd: root,
		Boundary:  &ProcessBoundary{Policy: "(version 1)(allow default)", PolicySHA256: "incorrect", TempDir: root},
		OnSpawned: func(int) error { spawned = true; return nil }})
	if err == nil || c != nil || spawned {
		t.Fatal("unverified boundary reached spawn", c, err, spawned)
	}
}

func TestProcessBoundaryAllowsWorkspaceAndDeniesDescendantWrites(t *testing.T) {
	if stdRuntime.GOOS != "darwin" {
		t.Skip("macOS candidate")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	allowed, protected, outside := filepath.Join(workspace, "allowed"), filepath.Join(workspace, "protected"), filepath.Join(root, "outside")
	for _, p := range []string{protected, outside} {
		if err := os.WriteFile(p, []byte("SENTINEL"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	word := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	policy := "(version 1)(allow default)(deny file-write* (require-not (subpath " + word(workspace) + ")))(deny file-write* (literal " + word(protected) + "))"
	sum := sha256.Sum256([]byte(policy))
	launcher := filepath.Join(root, "launcher")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nprintf ALLOWED > " + quote(allowed) + " || exit 12\n/bin/sh -c " + quote("printf BAD > "+quote(protected)) + "\nprintf BAD > " + quote(outside) + "\nexit 0\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd, err := boundaryCommand(Options{Launcher: launcher, Boundary: &ProcessBoundary{Policy: policy, PolicySHA256: hex.EncodeToString(sum[:]), TempDir: workspace, WritableRoots: []string{workspace}}})
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("boundary fixture timed out")
	}
	for path, want := range map[string]string{allowed: "ALLOWED", protected: "SENTINEL", outside: "SENTINEL"} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", path, b, err)
		}
	}
	denial := strings.ToLower(diagnostics.String())
	if !strings.Contains(denial, "operation not permitted") && !strings.Contains(denial, "permission denied") {
		t.Fatal("no explicit OS write denial", diagnostics.String())
	}
}

func TestProcessBoundary_PreexistingHardLinkRefusesBeforeSpawn(t *testing.T) {
	if stdRuntime.GOOS != "darwin" {
		t.Skip("macOS candidate")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "launcher")
	body := []byte("#!/bin/sh\nexit 99\n")
	if err := os.WriteFile(launcher, body, 0700); err != nil {
		t.Fatal(err)
	}
	launcherHash := sha256.Sum256(body)
	policy := "(version 1)(allow default)(deny file-link)"
	policyHash := sha256.Sum256([]byte(policy))
	spawned := false
	c, err := Start(context.Background(), Options{Launcher: launcher, LauncherSHA256: hex.EncodeToString(launcherHash[:]), Cwd: workspace,
		Boundary:  &ProcessBoundary{Policy: policy, PolicySHA256: hex.EncodeToString(policyHash[:]), TempDir: workspace, WritableRoots: []string{workspace}},
		OnSpawned: func(int) error { spawned = true; return nil }})
	if err == nil || c != nil || spawned || !strings.Contains(err.Error(), "hard-link alias") {
		t.Fatalf("alias did not refuse before spawn: conn=%v err=%v spawned=%v", c, err, spawned)
	}
	content, err := os.ReadFile(secret)
	if err != nil || string(content) != "SECRET" {
		t.Fatal("protected content changed", err)
	}
}
