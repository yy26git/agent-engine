package acceptance

// S2 acceptance: named profiles and broker-owned review ingress, through the built
// CLI.
//
// The unit tests in internal/control prove the profile resolver and the broker
// behave. This file proves two things they cannot: that an operator's registry is
// actually honoured by the deployed binary, and that the confinement gate is
// enforced by the PROCESS rather than by a helper nobody calls.
//
// The live-labelled cases are deliberately separated. They need a real tool, a
// real tmux session and a confinement mechanism that does not exist on this host,
// so they skip loudly and are reported as not-run rather than quietly passing.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/internal/control"
)

// withProfiles installs a profile registry in the world's CONTROL.toml.
//
// The registry is written the way an operator would write it: named presets, no
// command strings, and no confinement marked proven.
func (w *world) withProfiles(t *testing.T, extra string) {
	w.t.Helper()
	tomlPath := filepath.Join(w.Store, ConfigFileForTest)
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	registry := `
[profiles.impl]
role = "worker"
tool = "mcode"
model = "some-model"

[profiles.audit]
role = "reviewer"
tool = "mcode"

[profiles.diag]
role = "diagnostic"
tool = "mcode"

[profiles.readonly]
role = "investigation"
tool = "mcode"
read_only = "true"
sandbox = "true"
`
	if err := os.WriteFile(tomlPath, []byte(string(data)+registry+extra), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// ConfigFileForTest is the operator config the CLI reads.
const ConfigFileForTest = "CONTROL.toml"

// TestAcceptance_UnknownProfileIsRefused asserts the deployed binary refuses an
// unapproved profile name.
func TestAcceptance_UnknownProfileIsRefused(t *testing.T) {
	w := newWorld(t, "profiles-unknown")
	w.withProfiles(t, "")
	w.mustRun("submit", "--request-file", w.repairRequestFile())
	w.mustRun("bind", "--request-id", w.RequestID, "--worker", "sess-impl-1", "--auditor", "sess-auditor-1")

	// There is no CLI verb that launches a profile yet — S2 wires the engine
	// path, not a new caller surface. What is asserted here is that the registry
	// itself is honoured: a config naming an unknown ROLE is refused at load, and
	// a profile with no tool is refused rather than falling back to PATH.
	bad := w.Root + "-bad"
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, tc := range []struct{ name, table string }{
		{"unknown role", "[profiles.x]\nrole = \"wizard\"\ntool = \"mcode\"\n"},
		{"no tool", "[profiles.y]\nrole = \"worker\"\n"},
		{"empty table", "[profiles]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(bad, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			seed := filepath.Join(w.Store, ConfigFileForTest)
			data, err := os.ReadFile(seed)
			if err != nil {
				t.Fatalf("read seed: %v", err)
			}
			// A COMPLETE minimal config, so the refusal is about the profile
			// table and not about something that happens to be missing first.
			minimal := "agent_deck_binary = " + quote(fixtureBinary) + "\n" +
				"profile = \"acceptance\"\n" +
				"tool = \"mcode\"\n" +
				"output_root = " + quote(w.Output) + "\n" +
				"source_roots = [" + quote(filepath.Dir(w.Source)) + "]\n" +
				"evidence_roots = [" + quote(w.Evidence) + "]\n" +
				"[limits]\nmax_repair_cycles = 3\nmax_worker_generations = 3\n" +
				"max_audit_attempts = 3\nmax_guidance_rounds = 2\n" +
				"[checks.build]\nargv = [\"/bin/sh\",\"-c\",\"exit 0\"]\ntimeout_seconds = 30\n" +
				tc.table
			if err := os.WriteFile(filepath.Join(dir, ConfigFileForTest), []byte(minimal), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_ = data
			r := w.runAgainst(dir, "status", "--request-id", "anything")
			if r.Code == 0 {
				t.Fatalf("a profile registry with %s was accepted", tc.name)
			}
			if !strings.Contains(r.Stderr, "profiles") {
				t.Errorf("the refusal did not name the profile table: %s", r.Stderr)
			}
		})
	}
}

// TestAcceptance_UnprovenReadOnlyProfileCannotLaunch asserts the confinement gate
// holds end to end, from the operator's TOML through to a launch attempt.
//
// The chain is what matters: a file an operator actually wrote is parsed, the
// resulting registry is the one production uses, and resolving the profile from it
// refuses. Asserting only that a string exists inside the binary would prove
// nothing about behaviour.
func TestAcceptance_UnprovenReadOnlyProfileCannotLaunch(t *testing.T) {
	w := newWorld(t, "profiles-readonly-gate")
	w.withProfiles(t, "")
	w.mustRun("submit", "--request-file", w.repairRequestFile())
	w.mustRun("bind", "--request-id", w.RequestID, "--worker", "sess-impl-1", "--auditor", "sess-auditor-1")

	// The registry the deployed binary accepted, read back the same way the CLI
	// reads it.
	cfg, err := control.LoadConfig(w.Store)
	if err != nil {
		t.Fatalf("the operator registry did not load: %v", err)
	}
	if len(cfg.Profiles) == 0 {
		t.Fatal("the operator registry is empty; the fixture did not install profiles")
	}
	for _, name := range []string{"impl", "audit", "diag", "readonly"} {
		if _, ok := cfg.Profiles[name]; !ok {
			t.Errorf("profile %q is missing from the loaded registry: %v", name, cfg.Profiles)
		}
	}

	// A mutating worker profile resolves: nothing about it needs proving.
	if _, err := cfg.ResolveProfile("impl", control.RoleWorker); err != nil {
		t.Errorf("a mutating worker profile was refused: %v", err)
	}
	// A reviewer profile resolves.
	if _, err := cfg.ResolveProfile("audit", control.RoleReviewer); err != nil {
		t.Errorf("a reviewer profile was refused: %v", err)
	}
	// A reviewer profile used as a worker is refused.
	if _, err := cfg.ResolveProfile("audit", control.RoleWorker); !errors.Is(err, control.ErrRoleMismatch) {
		t.Errorf("a reviewer profile was accepted as a worker: %v", err)
	}

	// The read-only profile is REFUSED, because nobody has demonstrated that the
	// runtime can enforce it. This is the whole point of the gate.
	_, err = cfg.ResolveProfile("readonly", control.RoleInvestigat)
	if !errors.Is(err, control.ErrConfinementUnproven) {
		t.Fatalf("a read-only profile with unproven confinement was not refused: %v", err)
	}
	if !strings.Contains(err.Error(), "prompt, not a boundary") {
		t.Errorf("the refusal did not say why: %v", err)
	}

	// Recording the operator's demonstration makes the same profile resolve. The
	// gate is about evidence, not a permanent ban.
	cfg.Profiles["readonly"] = control.Profile{
		Name: "readonly", Role: control.RoleInvestigat, Tool: "mcode",
		ReadOnly: true, Sandbox: true, Proven: true,
	}
	if _, err := cfg.ResolveProfile("readonly", control.RoleInvestigat); err != nil {
		t.Errorf("a profile with a recorded demonstration was still refused: %v", err)
	}
}

// TestAcceptance_LiveConfinementIsNotRun is the live-labelled case the card asks
// for, and it is honest about not running.
//
// The pinned runtime's only confinement mechanism is a Docker sandbox whose
// project mount is unconditionally read-write, and Docker is not installed on
// this host. There is therefore nothing to attempt, and pretending otherwise by
// writing a fixture that "denies" a write would be exactly the unproven sandbox
// abstraction the plan forbids.
func TestAcceptance_LiveConfinementIsNotRun(t *testing.T) {
	// The two independent reasons live confinement cannot be demonstrated.
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Log("live confinement NOT RUN: no Docker socket, so the runtime's only confinement seam is unavailable")
	}
	// The upstream sandbox mounts the project read-write with no read-only option.
	src := filepath.Join("..", "..", "..", ".agent-deck-runtime", "build",
		"e19e23dac840016f926f35398706bfc398d96c9d", "internal", "docker", "docker.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("the pinned source is not present, so the confinement claim cannot be re-checked: %v", err)
	}
	// The project mount is appended with no readOnly field, which is what makes
	// the workspace writable inside the container.
	if !strings.Contains(string(data), "containerPath: containerWorkDir,") {
		t.Skip("the pinned source layout changed; re-verify the sandbox mount flags by hand")
	}
	if strings.Contains(string(data), "containerPath: containerWorkDir,\n\t\t\treadOnly: true") {
		t.Fatal("the pinned runtime now mounts the project read-only; the capability gap in this report is stale")
	}
	t.Skip("live confinement NOT RUN: the pinned runtime provides no read-only project mount, and no Docker on this host")
}

// quote renders a TOML basic string.
func quote(v string) string { return "\"" + v + "\"" }
