package minimaxacp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseReceipt_DetectsPayloadAndBundledNodeChanges(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, ".mcode-launcher")
	node := filepath.Join(root, "runtime", "bin", "node")
	payload := filepath.Join(root, "lib", "node_modules", "@minimax-ai", "code")
	if err := os.MkdirAll(filepath.Dir(node), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(payload, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{launcher: "#!/bin/sh\nexec '" + node + "' cli.js\n", node: "node-payload", filepath.Join(payload, "cli.js"): "entry", filepath.Join(payload, "chunk.js"): "chunk", filepath.Join(payload, "package.json"): "{}"}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := MeasureRelease(launcher)
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(root, "approved.json")
	body, _ := json.Marshal(map[string]any{"release_artifacts": manifest})
	if err := os.WriteFile(receipt, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReleaseReceipt(launcher, receipt); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(payload, "chunk.js"), node} {
		old, _ := os.ReadFile(path)
		if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyReleaseReceipt(launcher, receipt); err == nil {
			t.Fatal("unapproved payload accepted:", path)
		}
		if err := os.WriteFile(path, old, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReleaseReceipt_PinsInternalNpmLinksAndRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "lib", "node_modules", "@minimax-ai", "code")
	bin := filepath.Join(payload, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(root, "runtime", "bin", "node")
	if err := os.MkdirAll(filepath.Dir(node), 0700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, ".mcode-launcher")
	for path, body := range map[string]string{launcher: "#!/bin/sh\nexec '" + node + "' cli.js\n", node: "node", filepath.Join(payload, "cli.js"): "entry", filepath.Join(payload, "tool.js"): "same", filepath.Join(payload, "other.js"): "same"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(bin, "tool")
	if err := os.Symlink("../../tool.js", link); err != nil {
		t.Fatal(err)
	}
	manifest, err := MeasureRelease(launcher)
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(root, "approved.json")
	body, _ := json.Marshal(map[string]any{"release_artifacts": manifest})
	if err := os.WriteFile(receipt, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReleaseReceipt(launcher, receipt); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../other.js", link); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReleaseReceipt(launcher, receipt); err == nil {
		t.Fatal("identical target bytes hid changed link identity")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../../../runtime/bin/node", link); err != nil {
		t.Fatal(err)
	}
	if _, err := MeasureRelease(launcher); err == nil {
		t.Fatal("escaped npm link accepted")
	}
}
