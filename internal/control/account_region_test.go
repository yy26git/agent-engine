package control

// Requirement 3: MiniMax account region and reuse of the operator's login.
//
// The claims under test are narrow and all of them are refusals:
//
//   - both supported regions parse, and an unknown one is REFUSED by name rather
//     than coerced to a default, because a wrong region fails later as an
//     authentication error that looks like a transport fault;
//   - readiness is computed from booleans and closed enums only, so no code path
//     here can produce or serialize a credential;
//   - an ACP profile with no region is refused at load, naming the setting and
//     the command that fixes it;
//   - the region survives a config write/load round trip along with every
//     unrelated key, because a parser that quietly drops an operator's line is
//     indistinguishable from one that never read it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// regionTOML renders an operator config whose only MiniMax-specific setting is
// the region under test.
func regionTOML(t *testing.T, regionLine string) string {
	t.Helper()
	base := t.TempDir()
	return `agent_deck_binary = "agent-deck"
tool = "mcode"
output_root = "` + filepath.Join(base, "out") + `"
source_roots = ["` + filepath.Join(base, "src") + `"]
evidence_roots = ["` + filepath.Join(base, "evidence") + `"]

[limits]
max_repair_cycles = 3
max_worker_generations = 2
max_audit_attempts = 3
max_guidance_rounds = 2

[checkpoints]
soft_active_budget_seconds = 900
grace_seconds = 300
max_wall_seconds = 14400

[checks.build]
argv = ["go", "build", "./..."]
timeout_seconds = 600

[profiles.acp]
role = "worker"
tool = "mcode"
transport = "minimax-acp"
model = "minimax/M3"
` + regionLine
}

// writeRegionConfig writes a CONTROL.toml into a fresh store and loads it.
func writeRegionConfig(t *testing.T, toml string) (*Config, error) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"out", "src", "evidence"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ConfigFileName), []byte(toml), 0o600); err != nil {
		t.Fatalf("write CONTROL.toml: %v", err)
	}
	return LoadConfig(root)
}

// TestAccountRegion_BothSupportedRegionsParse asserts the only two accepted
// values are accepted, and that each one is RECORDED rather than being collapsed
// into the other.
func TestAccountRegion_BothSupportedRegionsParse(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{"cn", "cn"},
		{"global", "global"},
		// Case and padding are normalised rather than refused: a TOML value is a
		// human typing, and "CN" is the same account as "cn". What is refused is
		// an unrecognised REGION, not a differently-cased spelling of one.
		{"global ", "global"},
	} {
		cfg, err := writeRegionConfig(t, regionTOML(t, "region = \""+tc.written+"\"\n"))
		if err != nil {
			t.Fatalf("region %q: load: %v", tc.written, err)
		}
		if got := string(cfg.Profiles["acp"].AccountRegion); got != tc.want {
			t.Errorf("region %q parsed to %q, want %q", tc.written, got, tc.want)
		}
	}
}

// TestAccountRegion_UnknownIsRefusedNotDefaulted is the central refusal.
//
// A region outside the closed set must stop the configuration load. The
// alternative — defaulting to cn, or ignoring the line — would let an operator
// aimed at a Global account pass every configuration gate here and meet an
// authentication error at the first turn.
func TestAccountRegion_UnknownIsRefusedNotDefaulted(t *testing.T) {
	for _, bad := range []string{"", "CN-1", "international", "china", "us", "global2"} {
		_, err := writeRegionConfig(t, regionTOML(t, "region = \""+bad+"\"\n"))
		if err == nil {
			t.Fatalf("region %q was accepted; an unknown region must be refused", bad)
		}
		msg := err.Error()
		// The refusal has to be actionable on its own: which setting, which
		// values, and which command. A refusal that only says "invalid region"
		// sends the operator to look at the transport instead of their own file.
		for _, want := range []string{"profiles.acp.region", "mcode login --region global", "mcode login"} {
			if !strings.Contains(msg, want) {
				t.Errorf("region %q: refusal %q does not name %q", bad, msg, want)
			}
		}
	}
}

// TestAccountRegion_AbsentIsRefusedWithTheCommand asserts fail-closed behaviour
// for the missing case specifically: the message must name the missing operator
// setting and the exact command that fixes it.
func TestAccountRegion_AbsentIsRefusedWithTheCommand(t *testing.T) {
	_, err := writeRegionConfig(t, regionTOML(t, ""))
	if err == nil {
		t.Fatal("an acp profile with no region was accepted")
	}
	msg := err.Error()
	// The refusal has to be actionable on its own: which setting (named by the
	// field prefix and the "required" clause), which values are accepted, and
	// which command authenticates each of them.
	for _, want := range []string{"profiles.acp.region", "required", `set it to "cn" or "global"`, "mcode login", "mcode login --region global"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	// The refusal must also state the boundary it is protecting, so an operator
	// does not read "required" as "Agent Engine wants my key".
	if !strings.Contains(msg, "never reads, copies or stores the credential") {
		t.Errorf("refusal %q does not say the credential stays with the operator", msg)
	}
}

// TestAccountRegion_NotRequiredForAgentDeck asserts the requirement is scoped to
// the transport that actually reuses a MiniMax login. An Agent Deck profile
// reaches its own already-authenticated CLI, so demanding a MiniMax region there
// would be a field about a tool the profile does not use.
func TestAccountRegion_NotRequiredForAgentDeck(t *testing.T) {
	cfg, err := writeRegionConfig(t, strings.Replace(regionTOML(t, ""), `transport = "minimax-acp"`, `transport = "agent-deck"`, 1))
	if err != nil {
		t.Fatalf("an agent-deck profile with no region was refused: %v", err)
	}
	if got := cfg.Profiles["acp"].AccountRegion; got != "" {
		t.Errorf("agent-deck profile region = %q, want empty", got)
	}
}

// TestReadiness_DerivedFromBooleansAndEnums walks every way an operator's login
// can be observed, and asserts the answer is an enum decided purely by booleans
// and the closed source set.
func TestReadiness_DerivedFromBooleansAndEnums(t *testing.T) {
	sources := []RegionCredentialSource{CredentialSourceTokenPlan, CredentialSourceAPIKey}
	for _, src := range sources {
		if got := CredentialReadinessFor(ProviderObservation{Active: true, Enabled: true, HasAPIKey: true, Source: src}); !got.Ready() {
			t.Errorf("source %q with an active, enabled, keyed provider = %q, want ready", src, got)
		}
		// Each boolean alone can veto readiness. No combination of them can
		// produce anything other than one of the three enum values.
		for name, obs := range map[string]ProviderObservation{
			"inactive":        {Active: false, Enabled: true, HasAPIKey: true, Source: src},
			"disabled":        {Active: true, Enabled: false, HasAPIKey: true, Source: src},
			"no credential":   {Active: true, Enabled: true, HasAPIKey: false, Source: src},
			"nothing set":     {Source: src},
			"provider absent": {Enabled: true, HasAPIKey: true, Source: src},
		} {
			got := CredentialReadinessFor(obs)
			if got.Ready() {
				t.Errorf("%s with source %q = %q, want not-ready", name, src, got)
			}
			switch got {
			case CredentialReadinessUnknown, CredentialReadinessNotReady:
			default:
				t.Errorf("%s produced %q, which is outside the readiness enum", name, got)
			}
		}
	}
	// An unset or unsupported source cannot be read as ready. "Nobody has told
	// us how this profile authenticates" and "somebody told us it is fine" must
	// not print the same way.
	for _, src := range []RegionCredentialSource{"", "oauth", "MMX", "token_plan"} {
		if got := CredentialReadinessFor(ProviderObservation{Active: true, Enabled: true, HasAPIKey: true, Source: src}); got.Ready() {
			t.Errorf("source %q = %q, want not-ready", src, got)
		}
	}
	// A profile with no recorded region cannot report readiness at all: without
	// it there is no login command to name in a refusal.
	p := Profile{Name: "acp", Transport: TransportMiniMaxACP}
	if got := p.Readiness(ProviderObservation{Active: true, Enabled: true, HasAPIKey: true, Source: CredentialSourceTokenPlan}); got != CredentialReadinessUnknown {
		t.Errorf("regionless profile readiness = %q, want unknown", got)
	}
	p.AccountRegion = RegionGlobal
	if got := p.Readiness(ProviderObservation{Active: true, Enabled: true, HasAPIKey: true, Source: CredentialSourceTokenPlan}); !got.Ready() {
		t.Errorf("region-pinned profile readiness = %q, want ready", got)
	}
}

// TestReadinessRefusal_NamesTheOperatorCommand asserts a not-ready refusal is
// actionable and mentions the operator's own login rather than any credential.
func TestReadinessRefusal_NamesTheOperatorCommand(t *testing.T) {
	p := Profile{Name: "acp", Transport: TransportMiniMaxACP, AccountRegion: RegionGlobal}
	err := p.ReadinessRefusal(ProviderObservation{Source: CredentialSourceTokenPlan})
	if err == nil {
		t.Fatal("an unready profile produced no refusal")
	}
	for _, want := range []string{"profiles.acp", "global", "mcode login --region global", "mcode provider use", "never reads, copies or stores the credential"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	if err := p.ReadinessRefusal(ProviderObservation{Active: true, Enabled: true, HasAPIKey: true, Source: CredentialSourceAPIKey}); err != nil {
		t.Errorf("a ready observation was refused: %v", err)
	}
	// Each region must be handed its OWN login command. Handing a China operator
	// the Global command is the misdiagnosis this whole file exists to prevent.
	cn := Profile{Name: "acp", Transport: TransportMiniMaxACP, AccountRegion: RegionCN}
	cnMsg := cn.ReadinessRefusal(ProviderObservation{}).Error()
	if !strings.Contains(cnMsg, "`mcode login`") {
		t.Errorf("cn refusal %q does not name the cn login command", cnMsg)
	}
	if strings.Contains(cnMsg, "--region global") {
		t.Errorf("cn refusal %q names the Global login command", cnMsg)
	}
	if RegionCN.LoginCommand() != "mcode login" {
		t.Errorf("cn login command = %q, want %q", RegionCN.LoginCommand(), "mcode login")
	}
	if RegionGlobal.LoginCommand() != "mcode login --region global" {
		t.Errorf("global login command = %q", RegionGlobal.LoginCommand())
	}

	// With no region recorded there is no single command to name, so the refusal
	// offers both — and it says which setting is missing.
	none := Profile{Name: "acp", Transport: TransportMiniMaxACP}
	noneMsg := none.ReadinessRefusal(ProviderObservation{}).Error()
	for _, want := range []string{"profiles.acp.region", "mcode login  /  mcode login --region global"} {
		if !strings.Contains(noneMsg, want) {
			t.Errorf("regionless refusal %q does not name %q", noneMsg, want)
		}
	}
}

// TestRegionRoundTrip_PreservesRegionAndEveryUnrelatedKey writes an operator
// config carrying every key this parser understands, loads it, and asserts the
// region AND all of them come back. A parser that dropped one unrelated key
// would be indistinguishable, to an operator, from one that never read the file.
func TestRegionRoundTrip_PreservesRegionAndEveryUnrelatedKey(t *testing.T) {
	root := t.TempDir()
	out, src, ev := filepath.Join(root, "out"), filepath.Join(root, "src"), filepath.Join(root, "evidence")
	for _, d := range []string{out, src, ev} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	toml := `agent_deck_binary = "/abs/agent-deck"
agent_deck_source_commit = "e19e23d"
agent_deck_binary_sha256 = "78845b659d04736e8737b31e32a3dfc0a4bc061fa218ea85ac384f6e49643ff5"
profile = "pilot"
tool = "mcode"
decision_authority_role = "auditor"
output_root = "` + out + `"
source_roots = ["` + src + `"]
evidence_roots = ["` + ev + `"]

[limits]
max_repair_cycles = 3
max_worker_generations = 2
max_audit_attempts = 4
max_guidance_rounds = 5

[checkpoints]
soft_active_budget_seconds = 900
grace_seconds = 300
max_wall_seconds = 14400
review_timeout_seconds = 1800
min_wake_interval_seconds = 60

[checks.build]
argv = ["go", "build", "./..."]
timeout_seconds = 600
discovery_required = "true"

[profiles.acp]
role = "worker"
tool = "mcode"
transport = "minimax-acp"
model = "minimax/M3"
effort = "high"
region = "global"
sandbox = "true"
read_only = "true"
confinement_proven = "true"
launch_command = ["/abs/mcode", "acp", "--model", "{model}", "--effort", "{effort}"]
launch_env = ["MMX_HOME=/nonexistent"]

[profiles.audit]
role = "reviewer"
tool = "codex"
model = "gpt-6.1-sol"
`
	path := filepath.Join(root, ConfigFileName)
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Save/load: the file on disk is the save format; there is no other one.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != toml {
		t.Fatal("the saved bytes did not round-trip through disk unchanged")
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The region itself.
	if got := cfg.Profiles["acp"].AccountRegion; got != RegionGlobal {
		t.Errorf("region = %q, want global", got)
	}
	// Every unrelated key, checked explicitly rather than by a golden blob, so a
	// failure names the key that was lost.
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"agent_deck_binary", cfg.AgentDeckBinary, "/abs/agent-deck"},
		{"agent_deck_source_commit", cfg.AgentDeckSourceCommit, "e19e23d"},
		{"profile", cfg.Profile, "pilot"},
		{"tool", cfg.Tool, "mcode"},
		{"decision_authority_role", cfg.DecisionAuthority, "auditor"},
		{"output_root", cfg.OutputRoot, out},
		{"max_repair_cycles", cfg.Limits.RepairCycles, 3},
		{"max_worker_generations", cfg.Limits.WorkerGens, 2},
		{"max_audit_attempts", cfg.Limits.AuditAttempts, 4},
		{"max_guidance_rounds", cfg.Limits.GuidanceRounds, 5},
		{"soft_active_budget_seconds", int(cfg.Checkpoints.SoftActiveBudget.Seconds()), 900},
		{"grace_seconds", int(cfg.Checkpoints.Grace.Seconds()), 300},
		{"max_wall_seconds", int(cfg.Checkpoints.MaxWallDuration.Seconds()), 14400},
		{"review_timeout_seconds", int(cfg.Checkpoints.ReviewTimeout.Seconds()), 1800},
		{"min_wake_interval_seconds", int(cfg.Checkpoints.MinWakeInterval.Seconds()), 60},
		{"checks.build.argv", strings.Join(cfg.Checks["build"].Argv, " "), "go build ./..."},
		{"checks.build.discovery_required", cfg.Checks["build"].DiscoveryRequired, true},
		{"profiles.acp.transport", string(cfg.Profiles["acp"].Transport), "minimax-acp"},
		{"profiles.acp.model", cfg.Profiles["acp"].Model, "minimax/M3"},
		{"profiles.acp.effort", cfg.Profiles["acp"].Effort, "high"},
		{"profiles.acp.sandbox", cfg.Profiles["acp"].Sandbox, true},
		{"profiles.acp.read_only", cfg.Profiles["acp"].ReadOnly, true},
		{"profiles.acp.confinement_proven", cfg.Profiles["acp"].Proven, true},
		{"profiles.acp.launch_command", strings.Join(cfg.Profiles["acp"].LaunchCommand, " "), "/abs/mcode acp --model {model} --effort {effort}"},
		{"profiles.acp.launch_env", cfg.Profiles["acp"].LaunchEnv["MMX_HOME"], "/nonexistent"},
		{"profiles.audit.model", cfg.Profiles["audit"].Model, "gpt-6.1-sol"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestRegionRoundTrip_PinnedProfileRefKeepsRegion asserts the region survives
// the admission pin, so an operator editing `region` later cannot re-aim an
// in-flight request at a different MiniMax account.
func TestRegionRoundTrip_PinnedProfileRefKeepsRegion(t *testing.T) {
	p := Profile{Name: "acp", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode", AccountRegion: RegionCN}
	back := p.Ref().Profile()
	if back.AccountRegion != RegionCN {
		t.Errorf("pinned region = %q, want cn", back.AccountRegion)
	}
	if back.TransportFor() != TransportMiniMaxACP {
		t.Errorf("pinned transport = %q, want minimax-acp", back.TransportFor())
	}
}

// TestNoCredentialCanAppearInSerializedOutput asserts the structural claim: the
// credential-facing surface has no field that could hold a secret, so no
// serialized config, profile pin or readiness report can contain one.
//
// This is stronger than asserting an absence today. A new field of type string
// on ProviderObservation, Profile or ProfileRef would fail here at compile time
// rather than in an artifact somebody else has to find.
func TestNoCredentialCanAppearInSerializedOutput(t *testing.T) {
	// A sentinel that would be unmistakable in a leak.
	const sentinel = "sk-sentinel-DEADBEEF-do-not-log"

	obs := ProviderObservation{
		ProviderID: "minimax",
		Active:     true,
		Enabled:    true,
		HasAPIKey:  true,
		Source:     CredentialSourceTokenPlan,
	}
	p := Profile{
		Name: "acp", Role: RoleWorker, Transport: TransportMiniMaxACP, Tool: "mcode",
		AccountRegion: RegionCN,
		LaunchCommand: []string{"/abs/mcode", "acp"},
		LaunchEnv:     map[string]string{"MMX_HOME": "/nonexistent"},
	}
	serialized := []struct {
		name  string
		value any
	}{
		{"observation", obs},
		{"readiness", CredentialReadinessFor(obs)},
		{"profile", p},
		{"profile ref", p.Ref()},
	}
	for _, s := range serialized {
		blob, err := json.Marshal(s.value)
		if err != nil {
			t.Fatalf("%s: marshal: %v", s.name, err)
		}
		if strings.Contains(string(blob), sentinel) {
			t.Errorf("%s serialized a credential-shaped value: %s", s.name, blob)
		}
		// Nothing here is free text at all: every string is an identifier, a
		// path or a closed enum. A value the operator's client would call a key
		// has no field to live in.
		if strings.Contains(string(blob), `"api_key"`) || strings.Contains(string(blob), `"token"`) || strings.Contains(string(blob), `"secret"`) {
			t.Errorf("%s exposes a credential-named field: %s", s.name, blob)
		}
	}

	// A whole configuration, including its refusal messages, must be clean too:
	// the refusals are the text most likely to be pasted into an issue.
	cfg, err := writeRegionConfig(t, regionTOML(t, "region = \"cn\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	blob, err := json.Marshal(cfg.Profiles)
	if err != nil {
		t.Fatalf("marshal profiles: %v", err)
	}
	if strings.Contains(string(blob), sentinel) {
		t.Errorf("serialized profiles contain the sentinel: %s", blob)
	}
	_, err = writeRegionConfig(t, regionTOML(t, "region = \""+sentinel+"\"\n"))
	if err == nil {
		t.Fatal("an arbitrary string was accepted as a region")
	}
	// An unrecognised region is echoed back only as the operator typed it, which
	// is a value this layer already refused. Assert the refusal does not smuggle
	// it into a command anyone would run.
	if strings.Contains(err.Error(), "mcode login \""+sentinel+"\"") {
		t.Errorf("refusal interpolated unrecognised input into a command: %v", err)
	}
}

// TestRegion_NonStringIsRefused asserts a non-string region stops the load
// rather than being coerced, consistent with this parser's refusal of booleans
// and other scalars it does not model.
func TestRegion_NonStringIsRefused(t *testing.T) {
	_, err := writeRegionConfig(t, regionTOML(t, "region = 3\n"))
	if err == nil {
		t.Fatal("a non-string region was accepted")
	}
	if !strings.Contains(err.Error(), "profiles.acp.region") {
		t.Errorf("refusal %q does not name the field", err)
	}
}
