package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/emjin/agent-engine/internal/runtime"
)

const CodexReadOnly Confinement = "codex-read-only"

// ConfinementReceipt is qualification evidence, not permission supplied by a
// caller. Only the operator store may contain it. Admission remeasures identity.
type ConfinementReceipt struct {
	Schema        int                `json:"schema"`
	Status        string             `json:"status"`
	Profile       string             `json:"profile"`
	Workspace     string             `json:"workspace"`
	Identity      string             `json:"identity_sha256"`
	Mechanism     string             `json:"mechanism"`
	StartedAt     string             `json:"started_at"`
	EndedAt       string             `json:"ended_at,omitempty"`
	Failure       string             `json:"failure,omitempty"`
	CleanupProven bool               `json:"cleanup_proven"`
	Probes        []ConfinementProbe `json:"probes"`
}

type ConfinementProbe struct {
	Kind         string `json:"kind"`
	SessionID    string `json:"session_id,omitempty"`
	Label        string `json:"label"`
	Marker       string `json:"marker"`
	Command      string `json:"command"`
	Output       string `json:"output,omitempty"`
	OutputPath   string `json:"output_path,omitempty"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
	Before       string `json:"before_sha256"`
	After        string `json:"after_sha256,omitempty"`
	ExitCode     int    `json:"exit_code"`
	Complete     bool   `json:"complete"`
	Clean        bool   `json:"clean"`
}

// CodexQualificationCommand permits the narrow, non-escalating native launch
// used by qualification and production. Unknown overrides are refused.
func CodexQualificationCommand(p Profile) ([]string, error) {
	if p.Tool != "codex" || p.TransportFor() != TransportAgentDeck || !p.ReadOnly || p.Sandbox || p.Worktree || len(p.LaunchCommand) == 0 || !filepath.IsAbs(p.LaunchCommand[0]) {
		return nil, fmt.Errorf("qualification requires a native Codex read-only Agent Deck profile with an absolute launch_command")
	}
	if _, err := p.CreateSpec("/", "qualification"); err != nil {
		return nil, err
	}
	args := append([]string(nil), p.LaunchCommand...)
	for key := range p.LaunchEnv {
		if key != "CODEX_HOME" && key != "HOME" && key != "TMPDIR" {
			if (key == "HTTP_PROXY" || key == "HTTPS_PROXY") && qualifiedLoopbackProxy(p.LaunchEnv[key]) {
				continue
			}
			return nil, fmt.Errorf("qualification refuses unverified launch environment %q", key)
		}
	}
	readOnly, never := false, false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--model", "-m":
			i++
			if i >= len(args) || args[i] != "{model}" {
				return nil, fmt.Errorf("qualification requires the pinned model placeholder")
			}
		case "--sandbox", "-s":
			i++
			if i >= len(args) || args[i] != "read-only" {
				return nil, fmt.Errorf("qualification requires --sandbox read-only")
			}
			readOnly = true
		case "--ask-for-approval", "-a":
			i++
			if i >= len(args) || args[i] != "never" {
				return nil, fmt.Errorf("qualification requires approvals never")
			}
			never = true
		case "-c", "--config":
			i++
			if i >= len(args) || args[i] != "model_reasoning_effort={effort}" {
				return nil, fmt.Errorf("qualification refuses unverified configuration overrides")
			}
		case "--no-daemon":
		case "--disable":
			i++
			if i >= len(args) || !codexExternalFeature(args[i]) {
				return nil, fmt.Errorf("qualification only permits disabling plugins, remote_plugin, apps or hooks")
			}
		default:
			return nil, fmt.Errorf("qualification refuses unsupported launch argument %q", args[i])
		}
	}
	if !readOnly || !never {
		return nil, fmt.Errorf("qualification requires --sandbox read-only and --ask-for-approval never")
	}
	for i := range args {
		args[i] = strings.NewReplacer("{model}", p.Model, "{effort}", p.Effort).Replace(args[i])
	}
	return args, nil
}

// Proxy routing is part of the pinned launch environment and therefore changes
// qualification identity. Only an explicit local HTTP listener is supported;
// arbitrary remote proxies, credentials and URL routing components are refused.
func qualifiedLoopbackProxy(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return false
	}
	port, err := net.LookupPort("tcp", u.Port())
	return err == nil && port > 0 && port <= 65535
}

func codexExternalFeature(name string) bool {
	return name == "plugins" || name == "remote_plugin" || name == "apps" || name == "hooks"
}

func codexDisabledFeatures(args []string) map[string]bool {
	disabled := map[string]bool{}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--disable" {
			disabled[args[i+1]] = true
			i++
		}
	}
	return disabled
}

// ConfinementIdentity includes local configuration and the Agent Deck boundary.
// It excludes authentication files. Unknown external tools cannot be qualified
// by shell-denial probes, so this version refuses their configured presence.
func (c *Config) ConfinementIdentity(p Profile, workspace string) (string, error) {
	args, err := CodexQualificationCommand(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("qualification workspace must be absolute")
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	workspace = canonicalWorkspace
	if _, ok := c.ApprovedSourceRoot(workspace); !ok {
		return "", fmt.Errorf("qualification workspace is outside approved source roots")
	}
	cliHash, err := runtime.FileSHA256(args[0])
	if err != nil {
		return "", err
	}
	if goruntime.GOOS == "darwin" {
		file, err := os.Open(args[0])
		if err != nil {
			return "", err
		}
		magic := make([]byte, 4)
		_, err = file.Read(magic)
		file.Close()
		if err != nil || !(string(magic) == "\xcf\xfa\xed\xfe" || string(magic) == "\xfe\xed\xfa\xcf" || string(magic) == "\xca\xfe\xba\xbe" || string(magic) == "\xbe\xba\xfe\xca") {
			return "", fmt.Errorf("qualification requires the native Codex executable; launcher scripts do not pin their payload")
		}
	}
	deckHash, err := runtime.FileSHA256(c.AgentDeckBinary)
	if err != nil {
		return "", err
	}
	if deckHash != c.AgentDeckBinarySHA256 {
		return "", fmt.Errorf("Agent Deck binary differs from its operator pin")
	}
	home := p.LaunchEnv["CODEX_HOME"]
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		h := p.LaunchEnv["HOME"]
		if h == "" {
			h, err = os.UserHomeDir()
		}
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".codex")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("CODEX_HOME must be absolute")
	}
	disabled := codexDisabledFeatures(args)
	for _, dir := range []string{filepath.Join(home, "plugins"), filepath.Join(home, "hooks"), filepath.Join(workspace, ".codex", "plugins"), filepath.Join(workspace, ".agents", "plugins")} {
		if _, err := os.Stat(dir); err == nil {
			// Cached plugin files do not carry a shell confinement claim. They
			// can remain only when the pinned invocation disables plugin loading,
			// remote acquisition, apps and hooks. Hook files likewise require an
			// explicit disable; configuration for active external tools is still
			// refused below. These flags are part of the receipt identity.
			if filepath.Base(dir) == "plugins" && disabled["plugins"] && disabled["remote_plugin"] && disabled["apps"] && disabled["hooks"] {
				continue
			}
			if filepath.Base(dir) == "hooks" && disabled["hooks"] {
				continue
			}
			return "", fmt.Errorf("qualification requires an isolated Codex home/project without unqualified plugins or hooks: %s", dir)
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	configs, err := codexRuntimeArtifacts(args[0], cliHash)
	if err != nil {
		return "", err
	}

	if goruntime.GOOS == "darwin" {
		hash, err := runtime.FileSHA256("/System/Library/CoreServices/SystemVersion.plist")
		if err != nil {
			return "", err
		}
		configs["macOS-version"] = hash
	}
	for _, path := range []string{filepath.Join(home, "config.toml"), filepath.Join(workspace, ".codex", "config.toml")} {
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			configs[path] = "absent"
			continue
		}
		if err != nil {
			return "", err
		}
		fingerprint, err := codexConfigFingerprint(b)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		configs[path] = fingerprint
	}
	// The normalized declaration omits proof itself to avoid circular identity.
	p.Proven = false
	p.Transport = p.TransportFor()
	p.Confinement = string(CodexReadOnly)
	p.ConfinementReceipt = ""
	p.ConfinementReceiptSHA256 = ""
	b, err := json.Marshal(struct {
		ConfigIdentityVersion string
		Profile               Profile
		Workspace             string
		CLI                   string
		Deck                  string
		Configs               map[string]string
		Home                  string
		DeckProfile           string
		Platform              string
	}{"codex-toml-v1", p, workspace, cliHash, deckHash, configs, home, c.Profile, goruntime.GOOS + "/" + goruntime.GOARCH})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Config) VerifyConfinementReceipt(p Profile, workspace string) (string, error) {
	path := p.ConfinementReceipt
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: profile %q requires an absolute confinement_receipt, not a boolean assertion", ErrConfinementUnproven, p.Name)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfinementUnproven, err)
	}
	if !underRoot(canonical, c.StoreRoot) {
		return "", fmt.Errorf("%w: confinement receipt must be in the protected operator store", ErrConfinementUnproven)
	}
	if _, inside := c.ApprovedSourceRoot(canonical); inside {
		return "", fmt.Errorf("%w: confinement receipt is worker-writable", ErrConfinementUnproven)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", fmt.Errorf("%w: invalid receipt file", ErrConfinementUnproven)
	}
	b, err := os.ReadFile(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if p.ConfinementReceiptSHA256 != "" && p.ConfinementReceiptSHA256 != digest {
		return "", fmt.Errorf("%w: pinned qualification receipt changed", ErrConfinementUnproven)
	}
	var r ConfinementReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfinementUnproven, err)
	}
	identity, err := c.ConfinementIdentity(p, r.Workspace)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfinementUnproven, err)
	}
	canonicalWorkspace, canonicalErr := filepath.EvalSymlinks(workspace)
	canonicalReceiptWorkspace, receiptErr := filepath.EvalSymlinks(r.Workspace)
	if workspace != "" && (canonicalErr != nil || receiptErr != nil || canonicalWorkspace != canonicalReceiptWorkspace) {
		return "", fmt.Errorf("%w: qualification covers a different workspace", ErrConfinementUnproven)
	}
	if r.Schema != 1 || r.Status != "PASSED" || r.Profile != p.Name || r.Mechanism != string(CodexReadOnly) || r.Identity != identity || !r.CleanupProven {
		return "", fmt.Errorf("%w: qualification is failed, incomplete, stale or mismatched: %s", ErrConfinementUnproven, r.Failure)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.EndedAt); err != nil {
		return "", fmt.Errorf("%w: qualification has no completion timestamp", ErrConfinementUnproven)
	}
	if len(r.Probes) != 5 {
		return "", fmt.Errorf("%w: expected five probes", ErrConfinementUnproven)
	}
	seen := map[string]bool{}
	for _, probe := range r.Probes {
		if seen[probe.Kind] || !probe.Complete || !probe.Clean || probe.SessionID == "" || len(probe.Before) != 64 || probe.Command == "" || probe.Label == "" || probe.After != probe.Before || probe.Marker == "" || !strings.Contains(probe.Output, probe.Marker+fmt.Sprintf(":%d", probe.ExitCode)) {
			return "", fmt.Errorf("%w: incomplete or changed probe evidence", ErrConfinementUnproven)
		}
		outputPath, err := filepath.EvalSymlinks(probe.OutputPath)
		if err != nil || !underRoot(outputPath, c.StoreRoot) {
			return "", fmt.Errorf("%w: probe output is not retained in operator store", ErrConfinementUnproven)
		}
		if _, ok := c.ApprovedSourceRoot(outputPath); ok {
			return "", ErrConfinementUnproven
		}
		raw, err := os.ReadFile(outputPath)
		if err != nil || len(raw) > 65536 || string(raw) != probe.Output {
			return "", fmt.Errorf("%w: retained probe output changed", ErrConfinementUnproven)
		}
		hash, err := runtime.FileSHA256(outputPath)
		if err != nil || hash != probe.OutputSHA256 {
			return "", ErrConfinementUnproven
		}
		if probe.Kind == "read-control" {
			if probe.ExitCode != 0 || !strings.Contains(probe.Output, "qualification sentinel") {
				return "", ErrConfinementUnproven
			}
		} else if probe.ExitCode == 0 || (!strings.Contains(probe.Output, "Operation not permitted") && !strings.Contains(probe.Output, "Permission denied") && !strings.Contains(probe.Output, "Read-only file system")) {
			return "", fmt.Errorf("%w: write failure is not a demonstrated denial", ErrConfinementUnproven)
		}
		seen[probe.Kind] = true
	}
	for _, kind := range []string{"read-control", "source-write", "protected-write", "outside-write", "symlink-write"} {
		if !seen[kind] {
			return "", fmt.Errorf("%w: missing %s probe", ErrConfinementUnproven, kind)
		}
	}
	return digest, nil
}

// New native distributions contain executable helpers beside the CLI. A pin on
// the main executable alone cannot freeze those tools; measure the shipped tree.
func codexRuntimeArtifacts(cli, cliHash string) (map[string]string, error) {
	canonical, err := filepath.EvalSymlinks(cli)
	if err != nil {
		return nil, err
	}
	artifacts := map[string]string{}
	bin := filepath.Dir(canonical)
	bundle := filepath.Dir(bin)
	metadata := filepath.Join(bundle, "codex-package.json")
	if _, err := os.Stat(metadata); err == nil && filepath.Base(bin) == "bin" {
		count := 0
		err = filepath.WalkDir(bundle, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			count++
			if count > 8192 {
				return fmt.Errorf("Codex runtime exceeds artifact bound")
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("Codex runtime contains an unmeasured non-regular artifact: %s", path)
			}
			hash := cliHash
			if path != canonical {
				hash, err = runtime.FileSHA256(path)
				if err != nil {
					return err
				}
			}
			artifacts["runtime:"+path] = hash
			return nil
		})
		return artifacts, err
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// Standalone installs may acquire a sibling helper later. Absence is part of
	// identity too, so adding one invalidates the old receipt.
	for _, name := range []string{"codex-code-mode-host", "codex-exec-server"} {
		path := filepath.Join(bin, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			artifacts["runtime:"+path] = "absent"
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Codex helper is not a regular pinned file: %s", path)
		}
		hash, err := runtime.FileSHA256(path)
		if err != nil {
			return nil, err
		}
		artifacts["runtime:"+path] = hash
	}
	return artifacts, nil
}
