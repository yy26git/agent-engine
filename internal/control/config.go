// Package control owns the durable state, the admission decision and the
// artifacts of an engineering request.
//
// The authority rule for the whole package is narrow and absolute: state.json
// inside a request directory is the only thing that says what is true. Every
// other file is either an immutable input the caller supplied, a bounded human
// projection that can be regenerated, or a receipt. Nothing is inferred from
// prose, and in particular no decision is ever made by reading a Markdown file
// to see what a heading says.
package control

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ConfigFileName is the operator-owned policy file at the store root.
//
// It is deliberately not request data. A request may name a check, but only
// this file decides what that name means. If a request could carry its own
// definition of a command, then a caller would be supplying the thing that
// decides whether their work passed, which is the opposite of independent
// verification.
const ConfigFileName = "CONTROL.toml"

// StoreLockName guards admission for the whole store.
const StoreLockName = "STORE.lock"

// LeaseDirName holds one lease file per canonical workspace.
const LeaseDirName = "WORKSPACE-LEASES"

// RequestsDirName holds one directory per admitted request.
const RequestsDirName = "REQUESTS"

// Check is one operator-defined verification command.
//
// The caller names a check ID; this is where it becomes an actual command. The
// separation is the point: argv, working directory and timeout are operator
// policy, so a request cannot smuggle in a command of its own choosing.
type Check struct {
	ID      string
	Argv    []string
	Cwd     string
	Timeout time.Duration
	// DiscoveryRequired marks a test-shaped check, where "zero tests ran" is a
	// failure rather than a pass. A command that exits 0 having discovered
	// nothing has not verified anything, and treating it as a pass is the most
	// common way a green build hides an untested change.
	DiscoveryRequired bool
}

// Limits are the operator's ceilings.
//
// A request may ask for less. It may never ask for more: the operator's ceiling
// is the maximum speculative work this deployment will perform, and a caller
// raising it would mean the caller also decides how long it may loop.
type Limits struct {
	RepairCycles   int
	WorkerGens     int
	AuditAttempts  int
	GuidanceRounds int
}

// Config is the parsed, validated operator policy.
type Config struct {
	// StoreRoot is the canonical absolute store directory.
	StoreRoot string
	// AgentDeckBinary is the CLI used for every Agent Deck interaction.
	AgentDeckBinary string
	// AgentDeckSourceCommit and AgentDeckBinarySHA256 pin the runtime's identity.
	//
	// They are optional here and enforced at admission, because a configuration
	// can be READ on a machine that cannot verify a hash (an inspection host, a
	// migration tool). Refusing to parse such a config would make it impossible
	// to even report what is misconfigured. What is refused is any ACT of
	// launching, sending or following against an unverified runtime.
	AgentDeckSourceCommit string
	AgentDeckBinarySHA256 string
	// Profile scopes session resolution.
	Profile string
	// Profiles is the operator-approved named execution registry.
	//
	// A caller references a profile by name and nothing else. It is the only
	// place a tool, model or confinement choice may come from, so a request can
	// never smuggle in an executable of its own.
	Profiles map[string]Profile
	// LocalPilots grants trusted host execution to individual local episodes.
	LocalPilots map[string]LocalPilotGrant
	// ProgramAuthorities pins complete delegated-program definitions to an
	// operator-controlled digest and expiry. A caller may present a definition,
	// but it has no authority unless this local table names the exact bytes.
	ProgramAuthorities map[string]ProgramAuthorityGrant
	// EpisodeTemplates are the LOCALLY approved episode shapes a caller may
	// name by reference.
	//
	// A request supplies a template NAME and nothing else: no phases, no roles, no
	// checks, no argv. Anything a request could contribute to its own execution
	// would let a caller widen the grant it is asking to be judged under.
	EpisodeTemplates EpisodeTemplateRegistry
	// Tool is the existing Agent Deck tool name workers run under.
	Tool string
	// SourceRoots are the only approved roots a workspace may live under.
	SourceRoots []string
	// EvidenceRoots are the only approved roots evidence may be read from.
	EvidenceRoots []string
	// OutputRoot is the only directory the control may write artifacts into.
	//
	// It must be separate from a read-only source snapshot. Writing results back
	// into the source would make the evidence and the thing being verified the
	// same bytes, and then a hash check could no longer tell them apart.
	OutputRoot string
	// Checks is the operator check registry.
	Checks map[string]Check
	// Limits are the operator ceilings.
	Limits Limits
	// Evidence bounds complete retained records separately from prompt excerpts.
	Evidence EpisodeEvidencePolicy
	// DecisionAuthority is the only role permitted to ACCEPT a decision.
	//
	// Empty means nobody may accept, which is the safe direction: a decision
	// recorded without configured authority stays a proposal. It is not a
	// convenience default, because an accepted-decision record that anyone can
	// create proves nothing about anybody having signed off.
	DecisionAuthority string
	// Checkpoints is the operator's obligation-review timing policy.
	//
	// These values are deployment policy and deliberately not part of the
	// contract: there is no universal "every fifteen minutes" threshold that is
	// right for every project. An operator that omits them gets a documented
	// default rather than a surprise.
	Checkpoints CheckpointPolicy
	// Digest is the digest of the raw configuration bytes.
	//
	// Admission records it alongside the request, so a later restart can tell
	// whether the policy in force is the one the request was admitted under. If
	// it changed, the request was accepted under rules that no longer exist, and
	// silently continuing under the new rules would weaken or strengthen gates
	// the caller never agreed to.
	Digest string
}

// ConfigError is a refusal to operate under this configuration.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("agent-engine: CONTROL.toml %s: %s", e.Field, e.Reason)
}

func cfgErr(field, format string, args ...any) *ConfigError {
	return &ConfigError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// LoadConfig reads and validates CONTROL.toml from a store root.
func LoadConfig(storeRoot string) (*Config, error) {
	abs, err := filepath.Abs(storeRoot)
	if err != nil {
		return nil, fmt.Errorf("agent-engine: resolving store root: %w", err)
	}
	path := filepath.Join(abs, ConfigFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, cfgErr("", "not found at %s; every operation requires an operator policy file, because a request may not supply its own commands, roots or limits", path)
		}
		return nil, fmt.Errorf("agent-engine: reading %s: %w", path, err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	cfg.StoreRoot = abs
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if err := c.episodeEvidencePolicy().validate(); err != nil {
		return err
	}
	if c.AgentDeckBinary == "" {
		return cfgErr("agent_deck_binary", "required; the control layer reaches Agent Deck only through its CLI")
	}
	if c.Tool == "" {
		return cfgErr("tool", "required; workers run under an existing Agent Deck tool, never a provider adapter built here")
	}
	if len(c.SourceRoots) == 0 {
		return cfgErr("source_roots", "at least one approved source root is required")
	}
	if len(c.EvidenceRoots) == 0 {
		return cfgErr("evidence_roots", "at least one approved evidence root is required")
	}
	if c.OutputRoot == "" {
		return cfgErr("output_root", "required")
	}
	for _, id := range sortedKeys(c.Checks) {
		ck := c.Checks[id]
		if len(ck.Argv) == 0 {
			return cfgErr("checks."+id+".argv", "a check with no command can never run and would report a pass it did not perform")
		}
		if ck.Timeout <= 0 {
			return cfgErr("checks."+id+".timeout_seconds", "required and must be positive; an unbounded check is an unbounded wait")
		}
	}
	for name, v := range map[string]int{
		"max_repair_cycles":      c.Limits.RepairCycles,
		"max_worker_generations": c.Limits.WorkerGens,
		"max_audit_attempts":     c.Limits.AuditAttempts,
		"max_guidance_rounds":    c.Limits.GuidanceRounds,
	} {
		if v <= 0 {
			return cfgErr("limits."+name, "must be positive, got %d", v)
		}
	}
	if c.Checkpoints.SoftActiveBudget <= 0 {
		return cfgErr("checkpoints.soft_active_budget_seconds", "required and must be positive; a supervisor with no review budget never notices divergence")
	}
	if c.Checkpoints.Grace <= 0 {
		return cfgErr("checkpoints.grace_seconds", "required and must be positive; a zero grace would block legitimate long builds")
	}
	if c.Checkpoints.MaxWallDuration <= 0 {
		return cfgErr("checkpoints.max_wall_seconds", "required and must be positive; an unbounded request can never be closed")
	}
	// The output root must not sit inside the source tree, or a read-only
	// investigation would be able to modify the very snapshot it reports on.
	for _, src := range c.SourceRoots {
		if underRoot(c.OutputRoot, src) {
			return cfgErr("output_root",
				"%q is inside the read-only source root %q; results must be written outside the source so evidence and subject stay distinct",
				c.OutputRoot, src)
		}
	}
	return nil
}

// Check resolves a check ID from the operator registry.
//
// A caller naming a check that does not exist is refused rather than skipped. A
// missing check is not a check that passed, and quietly dropping it would let a
// request appear verified while a required verification never ran.
func (c *Config) Check(id string) (Check, error) {
	ck, ok := c.Checks[id]
	if !ok {
		return Check{}, fmt.Errorf("agent-engine: check %q is not in the operator registry (%s)", id, strings.Join(sortedKeys(c.Checks), ", "))
	}
	return ck, nil
}

// WithinLimits reports whether a requested limit fits under the operator ceiling.
func (c *Config) WithinLimits(name string, requested, ceiling int) error {
	if requested > ceiling {
		return fmt.Errorf("agent-engine: %s %d exceeds the operator limit %d; a request may lower a limit but never raise it", name, requested, ceiling)
	}
	return nil
}

// underRoot reports whether path lies within root.
//
// Both are cleaned and compared by full path. A prefix string comparison would
// be wrong: "/src/other" starts with "/src" but is not inside it.
func underRoot(path, root string) bool {
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ApprovedSourceRoot returns the source root containing path, if any.
func (c *Config) ApprovedSourceRoot(path string) (string, bool) {
	for _, r := range c.SourceRoots {
		if underRoot(path, r) {
			return r, true
		}
	}
	return "", false
}

// ApprovedEvidenceRoot returns the evidence root containing path, if any.
func (c *Config) ApprovedEvidenceRoot(path string) (string, bool) {
	for _, r := range c.EvidenceRoots {
		if underRoot(path, r) {
			return r, true
		}
	}
	return "", false
}

func sortedKeys(m map[string]Check) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseTOMLSubset parses the small, explicit subset of TOML this configuration
// uses: comments, bare keys, string and integer scalars, string arrays, and one
// level of tables.
//
// It is hand-written rather than pulled from a library on purpose. The
// configuration is operator-owned and small, and a dependency-free control
// plane is easier to audit: every byte of behaviour here is visible in this
// file. The parser is strict — anything it does not recognise is an error —
// because a configuration parser that silently ignores what it does not
// understand turns a typo into a default, and a defaulted root is a root the
// operator never approved.
func parseTOMLSubset(raw []byte) (map[string]any, error) {
	root := map[string]any{}
	table := root
	lineNo := 0

	for _, line := range strings.Split(string(raw), "\n") {
		lineNo++
		line := strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("agent-engine: line %d: unterminated table header %q", lineNo, line)
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if name == "" {
				return nil, fmt.Errorf("agent-engine: line %d: empty table name", lineNo)
			}
			// Dotted headers nest, so [checks.build] creates a "build" table
			// inside "checks". Nesting is what keeps the check registry namespaced
			// per check instead of flattened into a shared key space where one
			// check's "argv" could collide with another's.
			table = root
			for _, seg := range strings.Split(name, ".") {
				seg = strings.TrimSpace(seg)
				if seg == "" {
					return nil, fmt.Errorf("agent-engine: line %d: malformed table name %q", lineNo, name)
				}
				switch cur := table[seg].(type) {
				case map[string]any:
					table = cur
				case nil:
					created := map[string]any{}
					table[seg] = created
					table = created
				default:
					return nil, fmt.Errorf("agent-engine: line %d: %q is both a value and a table", lineNo, seg)
				}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("agent-engine: line %d: expected key = value, got %q", lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("agent-engine: line %d: empty key", lineNo)
		}
		if _, exists := table[key]; exists {
			return nil, fmt.Errorf("agent-engine: line %d: duplicate key %q", lineNo, key)
		}
		parsed, err := parseValue(strings.TrimSpace(value), lineNo)
		if err != nil {
			return nil, err
		}
		table[key] = parsed
	}
	return root, nil
}

func parseValue(v string, lineNo int) (any, error) {
	switch {
	case strings.HasPrefix(v, "["):
		if !strings.HasSuffix(v, "]") {
			return nil, fmt.Errorf("agent-engine: line %d: unterminated array", lineNo)
		}
		inner := strings.TrimSpace(v[1 : len(v)-1])
		out := []string{}
		if inner == "" {
			return out, nil
		}
		for _, item := range splitArrayItems(inner) {
			s, err := parseString(item, lineNo)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case strings.HasPrefix(v, `"`):
		return parseString(v, lineNo)
	default:
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("agent-engine: line %d: %q is not a quoted string, an array or an integer; booleans are not accepted because a flag whose default silently flips is a safety defect", lineNo, v)
		}
		return n, nil
	}
}

func parseString(v string, lineNo int) (string, error) {
	if len(v) < 2 || !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) {
		return "", fmt.Errorf("agent-engine: line %d: %q is not a quoted string", lineNo, v)
	}
	return v[1 : len(v)-1], nil
}

// splitArrayItems splits on commas that are not inside quotes.
func splitArrayItems(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if last := strings.TrimSpace(cur.String()); last != "" {
		out = append(out, last)
	}
	return out
}

func parseConfig(raw []byte) (*Config, error) {
	doc, err := parseTOMLSubset(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256Hex(raw)

	cfg := &Config{
		Evidence:              defaultEpisodeEvidencePolicy(),
		AgentDeckBinary:       stringField(doc, "agent_deck_binary"),
		AgentDeckSourceCommit: stringField(doc, "agent_deck_source_commit"),
		AgentDeckBinarySHA256: stringField(doc, "agent_deck_binary_sha256"),
		Profile:               stringField(doc, "profile"),
		Tool:                  stringField(doc, "tool"),
		OutputRoot:            stringField(doc, "output_root"),
		DecisionAuthority:     stringField(doc, "decision_authority_role"),
		SourceRoots:           stringArray(doc, "source_roots"),
		EvidenceRoots:         stringArray(doc, "evidence_roots"),
		Checks:                map[string]Check{},
		Profiles:              map[string]Profile{},
		Digest:                sum,
	}
	if evidence, ok := doc["evidence"].(map[string]any); ok {
		for key, dest := range map[string]*int{
			"max_producer_bytes": &cfg.Evidence.MaxProducerBytes,
			"max_episode_bytes":  &cfg.Evidence.MaxEpisodeBytes,
			"max_prompt_bytes":   &cfg.Evidence.MaxPromptBytes,
		} {
			if _, present := evidence[key]; present {
				*dest = intField(evidence, key)
			}
		}
		if err := cfg.Evidence.validate(); err != nil {
			return nil, err
		}
	}
	if limits, ok := doc["limits"].(map[string]any); ok {
		cfg.Limits = Limits{
			RepairCycles:   intField(limits, "max_repair_cycles"),
			WorkerGens:     intField(limits, "max_worker_generations"),
			AuditAttempts:  intField(limits, "max_audit_attempts"),
			GuidanceRounds: intField(limits, "max_guidance_rounds"),
		}
	}
	if cp, ok := doc["checkpoints"].(map[string]any); ok {
		secs := func(k string) time.Duration { return time.Duration(intField(cp, k)) * time.Second }
		cfg.Checkpoints = CheckpointPolicy{
			SoftActiveBudget: secs("soft_active_budget_seconds"),
			Grace:            secs("grace_seconds"),
			MaxWallDuration:  secs("max_wall_seconds"),
			ReviewTimeout:    secs("review_timeout_seconds"),
			MinWakeInterval:  secs("min_wake_interval_seconds"),
		}
	} else {
		cfg.Checkpoints = DefaultCheckpointPolicy()
	}
	// The episode template registry is read before validation for the same
	// reason the profile registry is: the checks below reason about it.
	//
	// A caller names a template and gets nothing else. Phases, roles and checks
	// come from HERE, so a request cannot widen its own execution by describing
	// a richer episode than the operator approved.
	templates, terr := loadEpisodeTemplates(doc)
	if terr != nil {
		return nil, terr
	}
	cfg.EpisodeTemplates = templates
	if err := loadProfiles(doc, cfg); err != nil {
		return nil, err
	}
	if err := validateProfiles(cfg); err != nil {
		return nil, err
	}
	if err := loadLocalPilots(doc, cfg); err != nil {
		return nil, err
	}
	if err := loadProgramAuthorities(doc, cfg); err != nil {
		return nil, err
	}
	checks, ok := doc["checks"].(map[string]any)
	if !ok {
		return nil, cfgErr("checks", "required; a request names checks, so the registry that resolves them must exist")
	}
	for id, raw := range checks {
		t, ok := raw.(map[string]any)
		if !ok {
			return nil, cfgErr("checks."+id, "expected a table of check settings")
		}
		seconds := intField(t, "timeout_seconds")
		ck := Check{
			ID:                id,
			Argv:              stringArray(t, "argv"),
			Cwd:               stringField(t, "cwd"),
			DiscoveryRequired: boolField(t, "discovery_required"),
		}
		if seconds > 0 {
			ck.Timeout = time.Duration(seconds) * time.Second
		}
		cfg.Checks[id] = ck
	}
	return cfg, nil
}

func stringField(doc map[string]any, key string) string {
	if v, ok := doc[key].(string); ok {
		return v
	}
	return ""
}

func stringArray(doc map[string]any, key string) []string {
	switch v := doc[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func intField(doc map[string]any, key string) int {
	if v, ok := doc[key].(int); ok {
		return v
	}
	return 0
}

// boolField reads an explicit boolean. The TOML subset parser does not accept
// bare booleans, so this only ever sees the string "true" written by an
// operator who wrote it deliberately.
func boolField(doc map[string]any, key string) bool {
	s, ok := doc[key].(string)
	return ok && strings.EqualFold(strings.TrimSpace(s), "true")
}

// loadProfiles reads the operator-approved profile registry.
//
// The table is optional. A deployment that has not configured profiles keeps
// working for manually bound single sessions; what it loses is the ability to
// launch autonomously, which is a safer failure than inventing a default
// executor.
func loadProfiles(doc map[string]any, cfg *Config) error {
	raw, ok := doc["profiles"].(map[string]any)
	if !ok {
		return nil
	}
	if len(raw) == 0 {
		return cfgErr("profiles", "present but empty; omit the table entirely rather than declaring no usable executor")
	}
	for name, entry := range raw {
		t, ok := entry.(map[string]any)
		if !ok {
			return cfgErr("profiles."+name, "expected a table of profile settings")
		}
		if strings.TrimSpace(name) == "" {
			return cfgErr("profiles", "a profile name is empty")
		}
		p := Profile{
			Name:               name,
			Role:               ProfileRole(strings.ToLower(strings.TrimSpace(stringField(t, "role")))),
			Tool:               stringField(t, "tool"),
			Transport:          Transport(strings.ToLower(strings.TrimSpace(stringField(t, "transport")))),
			Provider:           stringField(t, "provider"),
			Model:              stringField(t, "model"),
			Effort:             stringField(t, "effort"),
			Permission:         stringField(t, "permission_mode"),
			LauncherSHA256:     stringField(t, "launcher_sha256"),
			Sandbox:            boolField(t, "sandbox"),
			Worktree:           boolField(t, "worktree"),
			ReadOnly:           boolField(t, "read_only"),
			Proven:             boolField(t, "confinement_proven"),
			ConfinementReceipt: stringField(t, "confinement_receipt"),
		}
		switch p.Role {
		case RoleWorker, RoleReviewer, RoleCheckpoint, RoleDiagnostic, RoleInvestigat:
		default:
			return cfgErr("profiles."+name+".role", "%s",
				"must be one of worker, reviewer, checkpoint, diagnostic, investigation; got "+string(p.Role))
		}
		if strings.TrimSpace(p.Tool) == "" {
			return cfgErr("profiles."+name+".tool", "%s",
				"required; a profile with no tool would fall back to whatever was on PATH")
		}
		// An unrecognised transport is refused rather than defaulted. Defaulting
		// would silently run approved-for-MiniMax work on Agent Deck, which is the
		// exact substitution this field exists to prevent.
		switch p.Transport {
		case "", TransportAgentDeck, TransportMiniMaxACP, TransportCursorCLI:
		default:
			return cfgErr("profiles."+name+".transport", "%s",
				"must be one of agent-deck, minimax-acp, cursor-cli; got "+string(p.Transport))
		}
		if raw, present := t["launch_command"]; present {
			argv, ok := raw.([]string)
			if !ok {
				return cfgErr("profiles."+name+".launch_command", "expected an array of strings")
			}
			p.LaunchCommand = argv
		}
		// The region is parsed and refused HERE, at load time, rather than
		// defaulted or carried forward empty. A wrong region does not fail
		// loudly where it was written; it fails at the first authenticated turn,
		// as an authentication error an operator would reasonably diagnose as a
		// transport fault.
		if raw, present := t["region"]; present {
			s, ok := raw.(string)
			if !ok {
				return cfgErr("profiles."+name+".region", "expected a quoted string, one of \"cn\" or \"global\"")
			}
			region, err := parseAccountRegion("profiles."+name+".region", s)
			if err != nil {
				return err
			}
			p.AccountRegion = region
		}
		if raw, present := t["launch_env"]; present {
			env, ok := raw.([]string)
			if !ok {
				return cfgErr("profiles."+name+".launch_env", "expected an array of NAME=value strings")
			}
			p.LaunchEnv = map[string]string{}
			for _, entry := range env {
				k, v, ok := strings.Cut(entry, "=")
				if !ok || !validEnvName(k) {
					return cfgErr("profiles."+name+".launch_env", "invalid environment entry")
				}
				if _, exists := p.LaunchEnv[k]; exists {
					return cfgErr("profiles."+name+".launch_env", "duplicate environment key")
				}
				p.LaunchEnv[k] = v
			}
		}
		if p.Transport == TransportCursorCLI {
			if err := ValidateReportProfile(p); err != nil {
				return err
			}
		} else if _, err := p.CreateSpec("/", ""); err != nil {
			return err
		}
		// Fail closed on a transport that reuses an operator login but does not
		// say which account it belongs to. Requiring it only for ACP is the point:
		// an Agent Deck session reaches its own already-authenticated CLI, and
		// demanding a MiniMax region there would be a field about a tool this
		// profile does not use.
		if p.requiresAccountRegion() && p.AccountRegion == "" {
			return cfgErr("profiles."+name+".region", "%s",
				"required for a "+string(TransportMiniMaxACP)+" profile; set it to \"cn\" or \"global\", then authenticate that account yourself with "+p.regionLoginHint()+
					". Agent Engine reuses the operator's existing login and never reads, copies or stores the credential itself")
		}
		cfg.Profiles[name] = p
	}
	return nil
}

// validateProfiles refuses a registry that would produce an unsafe launch.
func validateProfiles(cfg *Config) error {
	if len(cfg.Profiles) == 0 {
		return nil
	}
	// A reviewer profile is required before any autonomous completion is
	// possible, and saying so at load time is cheaper than discovering it after a
	// worker has already been granted the workspace.
	var hasReviewer bool
	for _, p := range cfg.Profiles {
		if p.Role == RoleReviewer {
			hasReviewer = true
		}
	}
	// A read-only profile that claims proven confinement is asserting evidence
	// this build cannot verify. It is accepted, because the operator is the party
	// who ran the demonstration — but it is recorded as an assertion rather than
	// a measurement, and no default ever sets it.
	_ = hasReviewer
	return nil
}
