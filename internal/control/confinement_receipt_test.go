package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

func qualifiedFixture(t *testing.T) (*Config, Profile, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	workspace := filepath.Join(root, "source")
	home := filepath.Join(root, "codex")
	for _, path := range []string{store, workspace, home} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cli, deck := filepath.Join(root, "codex-bin"), filepath.Join(root, "deck-bin")
	for _, path := range []string{cli, deck} {
		if err := os.WriteFile(path, append([]byte{0xcf, 0xfa, 0xed, 0xfe}, []byte("fixture executable")...), 0700); err != nil {
			t.Fatal(err)
		}
	}
	deckSHA, _ := runtime.FileSHA256(deck)
	c := &Config{StoreRoot: store, SourceRoots: []string{workspace}, AgentDeckBinary: deck, AgentDeckBinarySHA256: deckSHA}
	p := Profile{Name: "review", Role: RoleReviewer, Tool: "codex", ReadOnly: true, Model: "selected", LaunchCommand: []string{cli, "--model", "{model}", "--sandbox", "read-only", "--ask-for-approval", "never"}, LaunchEnv: map[string]string{"CODEX_HOME": home}, ConfinementReceipt: filepath.Join(store, "proof.json")}
	identity, err := c.ConfinementIdentity(p, workspace)
	if err != nil {
		t.Fatal(err)
	}
	r := ConfinementReceipt{Schema: 1, Status: "PASSED", Profile: p.Name, Workspace: workspace, Identity: identity, Mechanism: string(CodexReadOnly), EndedAt: time.Now().UTC().Format(time.RFC3339Nano), CleanupProven: true}
	for _, kind := range []string{"read-control", "source-write", "protected-write", "outside-write", "symlink-write"} {
		code := 1
		output := "Permission denied\n"
		if kind == "read-control" {
			code = 0
			output = "qualification sentinel\n"
		}
		marker := "M_" + kind
		output += marker + ":" + itoa(code)
		logPath := filepath.Join(store, kind+".log")
		if err := os.WriteFile(logPath, []byte(output), 0600); err != nil {
			t.Fatal(err)
		}
		outputHash, _ := runtime.FileSHA256(logPath)
		r.Probes = append(r.Probes, ConfinementProbe{Kind: kind, SessionID: "session-" + kind, Label: kind, Marker: marker, Command: "fixture qualification", Output: output, OutputPath: logPath, OutputSHA256: outputHash, Before: strings.Repeat("a", 64), After: strings.Repeat("a", 64), ExitCode: code, Complete: true, Clean: true})
	}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(p.ConfinementReceipt, b, 0600); err != nil {
		t.Fatal(err)
	}
	c.Profiles = map[string]Profile{p.Name: p}
	return c, p, workspace
}

func TestConfinementReceipt_BooleanCannotApproveCodex(t *testing.T) {
	c, p, _ := qualifiedFixture(t)
	p.Proven = true
	p.ConfinementReceipt = ""
	c.Profiles[p.Name] = p
	if _, err := c.ResolveProfile(p.Name, p.Role); err == nil {
		t.Fatal("boolean bypassed receipt qualification")
	}
}

func TestConfinementReceipt_PinnedRoundTripAndWorkspace(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	resolved, err := c.ResolveProfile(p.Name, p.Role)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Proven || resolved.ConfinementFor() != CodexReadOnly || resolved.ConfinementReceiptSHA256 == "" {
		t.Fatal(resolved)
	}
	if _, err := c.VerifyConfinementReceipt(resolved.Ref().Profile(), workspace); err != nil {
		t.Fatal("pin did not survive round trip", err)
	}
	if _, err := c.VerifyConfinementReceipt(resolved, filepath.Join(workspace, "other")); err == nil {
		t.Fatal("different workspace reused proof")
	}
}

func TestConfinementReceipt_ChangedDependenciesInvalidate(t *testing.T) {
	for _, kind := range []string{"binary", "config", "arguments", "receipt", "cleanup", "runtime-error", "output"} {
		t.Run(kind, func(t *testing.T) {
			c, p, workspace := qualifiedFixture(t)
			resolved, err := c.ResolveProfile(p.Name, p.Role)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "binary":
				_ = os.WriteFile(p.LaunchCommand[0], []byte("changed executable"), 0700)
			case "config":
				_ = os.WriteFile(filepath.Join(p.LaunchEnv["CODEX_HOME"], "config.toml"), []byte("model = 'changed'\n"), 0600)
			case "output":
				b, _ := os.ReadFile(p.ConfinementReceipt)
				var r ConfinementReceipt
				_ = json.Unmarshal(b, &r)
				_ = os.WriteFile(r.Probes[0].OutputPath, []byte("replaced"), 0600)
			case "arguments":
				resolved.LaunchCommand = append(resolved.LaunchCommand, "--dangerously-bypass-approvals-and-sandbox")
			default:
				b, _ := os.ReadFile(p.ConfinementReceipt)
				var r ConfinementReceipt
				_ = json.Unmarshal(b, &r)
				if kind == "cleanup" {
					r.CleanupProven = false
				} else if kind == "runtime-error" {
					r.Probes[1].Output = "sandbox-exec: unbound variable: TIOCSTI\n" + r.Probes[1].Marker + ":65"
					r.Probes[1].ExitCode = 65
					_ = os.WriteFile(r.Probes[1].OutputPath, []byte(r.Probes[1].Output), 0600)
					r.Probes[1].OutputSHA256, _ = runtime.FileSHA256(r.Probes[1].OutputPath)
				} else {
					r.Status = "RUNNING"
				}
				b, _ = json.Marshal(r)
				_ = os.WriteFile(p.ConfinementReceipt, b, 0600)
				// Even a newly selected receipt must refuse failed evidence, not
				// just the previously pinned digest.
				resolved.ConfinementReceiptSHA256 = ""
			}
			if _, err := c.VerifyConfinementReceipt(resolved, workspace); err == nil {
				t.Fatal("changed dependency remained qualified")
			}
		})
	}
}

func TestConfinementReceipt_ExternalToolsNeedSeparateProof(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	_ = os.WriteFile(filepath.Join(p.LaunchEnv["CODEX_HOME"], "config.toml"), []byte("[mcp_servers.writer]\ncommand = 'write-server'\n"), 0600)
	if _, err := c.ConfinementIdentity(p, workspace); err == nil {
		t.Fatal("shell proof covered an external writer")
	}
}

func TestConfinementReceipt_CachedPluginsRequirePinnedDisabling(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	cache := filepath.Join(p.LaunchEnv["CODEX_HOME"], "plugins", "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "plugin.json"), []byte(`{"name":"cached-writer"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{nil, {"--disable", "plugins"}, {"--disable", "remote_plugin"}, {"--disable", "plugins", "--disable", "remote_plugin", "--disable", "hooks"}, {"--disable", "plugins", "--disable", "remote_plugin", "--disable", "apps"}} {
		candidate := p
		candidate.LaunchCommand = append(append([]string(nil), p.LaunchCommand...), flags...)
		if _, err := c.ConfinementIdentity(candidate, workspace); err == nil {
			t.Fatal("incomplete plugin disabling qualified cached capabilities", flags)
		}
	}
	p.LaunchCommand = append(p.LaunchCommand, "--disable", "plugins", "--disable", "remote_plugin", "--disable", "apps", "--disable", "hooks")
	if _, err := c.ConfinementIdentity(p, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VerifyConfinementReceipt(p, workspace); err == nil {
		t.Fatal("new flags reused old proof")
	}
	if err := os.WriteFile(filepath.Join(p.LaunchEnv["CODEX_HOME"], "config.toml"), []byte("[mcp_servers.writer]\ncommand = 'writer'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConfinementIdentity(p, workspace); err == nil {
		t.Fatal("disabled plugins qualified standalone MCP configuration")
	}
}

func TestConfinementReceipt_HooksRequireExplicitDisabling(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	if err := os.Mkdir(filepath.Join(p.LaunchEnv["CODEX_HOME"], "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConfinementIdentity(p, workspace); err == nil {
		t.Fatal("active hooks qualified")
	}
	p.LaunchCommand = append(p.LaunchCommand, "--disable", "hooks")
	if _, err := c.ConfinementIdentity(p, workspace); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--enable", "plugins"}, {"--disable", "unknown"}, {"--disable"}, {"-c", "features.plugins=true"}} {
		candidate := p
		candidate.LaunchCommand = append(append([]string(nil), p.LaunchCommand...), flags...)
		if _, err := CodexQualificationCommand(candidate); err == nil {
			t.Fatal("unsafe override allowed", flags)
		}
	}
}

func TestConfinementReceipt_AddedNativeHelperInvalidates(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	helper := filepath.Join(filepath.Dir(p.LaunchCommand[0]), "codex-code-mode-host")
	if err := os.WriteFile(helper, []byte("new helper"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VerifyConfinementReceipt(p, workspace); err == nil {
		t.Fatal("new executable helper inherited old qualification")
	}
}

func TestCodexRuntimeArtifacts_BundleIncludesHelpersAndRejectsLinks(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{filepath.Join(root, "codex-package.json"): "{}", filepath.Join(bin, "codex"): "cli", filepath.Join(bin, "codex-code-mode-host"): "host"} {
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(bin, "codex")
	hash, _ := runtime.FileSHA256(cli)
	files, err := codexRuntimeArtifacts(cli, hash)
	if err != nil || len(files) != 3 {
		t.Fatal(err, files)
	}
	if err := os.Symlink(cli, filepath.Join(root, "unmeasured-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := codexRuntimeArtifacts(cli, hash); err == nil {
		t.Fatal("symlinked bundle tool was not refused")
	}
}

func TestConfinementReceipt_LoopbackProxyMustBePinnedAndRequalified(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	before, err := c.ConfinementIdentity(p, workspace)
	if err != nil {
		t.Fatal(err)
	}
	p.LaunchEnv["HTTPS_PROXY"] = "http://127.0.0.1:15236"
	after, err := c.ConfinementIdentity(p, workspace)
	if err != nil || before == after {
		t.Fatalf("proxy identity missing: %s %s %v", before, after, err)
	}
	if _, err := c.VerifyConfinementReceipt(p, workspace); err == nil {
		t.Fatal("changed routing reused old qualification")
	}
	for _, proxy := range []string{"http://proxy.example:15236", "http://localhost:15236", "https://127.0.0.1:15236", "http://user:secret@127.0.0.1:15236", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:00", "http://127.0.0.1:70000", "http://127.0.0.1:15236/path", "http://127.0.0.1:15236?route=other", "http://127.0.0.1:15236#fragment"} {
		p.LaunchEnv["HTTPS_PROXY"] = proxy
		if _, err := CodexQualificationCommand(p); err == nil {
			t.Fatal("unqualified proxy accepted:", proxy)
		}
	}
	p.LaunchEnv["HTTPS_PROXY"] = "http://[::1]:15236/"
	if _, err := CodexQualificationCommand(p); err != nil {
		t.Fatal(err)
	}
	p.LaunchEnv["LD_PRELOAD"] = "unverified"
	if _, err := CodexQualificationCommand(p); err == nil {
		t.Fatal("proxy support enabled unrelated launch environment")
	}
}
