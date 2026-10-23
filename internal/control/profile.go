package control

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/emjin/agent-engine/internal/agentdeck"
)

// Named execution profiles.
//
// A profile is an operator-approved NAME that resolves to a runtime tool, a role
// and a set of capabilities. Callers choose names; they never choose commands.
// That distinction is the whole point: a caller-supplied argv is a caller-chosen
// executable, which is how a request ends up running whatever it felt like.
//
// A profile label is also not a capability. Declaring a profile READ_ONLY asserts
// an intention; enforcing it requires a confinement mechanism at the runtime
// launch seam, and this package refuses to treat the first as the second. That
// refusal is the most important function here — see ResolveProfile.

// ProfileRole is what a session is for.
//
// The role is not cosmetic. A reviewer is judged by whether it is independent of
// the implementer, and independence is only meaningful if the two roles cannot be
// filled by the same session.
type ProfileRole string

const (
	RoleWorker     ProfileRole = "worker"
	RoleReviewer   ProfileRole = "reviewer"
	RoleDiagnostic ProfileRole = "diagnostic"
	RoleCheckpoint ProfileRole = "checkpoint"
	RoleInvestigat ProfileRole = "investigation"
)

// Confinement is the mechanism that would enforce a profile's write scope.
type Confinement string

const (
	// ConfinementNone means nothing restricts what the session can write. It is
	// honest, and it is the only value that needs no evidence.
	ConfinementNone Confinement = "none"
	// ConfinementSandbox means the runtime launches the session inside a
	// container whose mounts decide the write scope.
	ConfinementSandbox Confinement = "sandbox"
	// ConfinementWorktree means the session gets its own git worktree, which
	// isolates concurrent WRITERS but does not restrict what that session may
	// write inside its own tree.
	ConfinementWorktree Confinement = "worktree"
)

// Transport selects HOW a session is created and observed.
//
// It is deliberately separate from the tool. The tool names a program; the
// transport names the mechanism that starts and talks to it. Conflating them is
// what makes "which harness" and "which model" the same question, and they are
// not.
type Transport string

const (
	// TransportAgentDeck is the pinned Agent Deck CLI. It is the DEFAULT, so a
	// configuration written before transports existed keeps its meaning exactly.
	TransportAgentDeck Transport = "agent-deck"
	TransportCursorCLI Transport = "cursor-cli"
	// TransportMiniMaxACP is a native ACP process owned directly by this layer.
	TransportMiniMaxACP Transport = "minimax-acp"
)

// ErrTransportUnsupported is returned when a transport cannot serve a role.
var ErrTransportUnsupported = errors.New("agent-engine: transport cannot serve this role")

// transportRoleOK records which roles each transport may serve.
//
// ACP is worker-only. A reviewer's whole value is that it is an independent
// session whose output this layer can attribute; an ACP reviewer would be a
// process whose lifecycle and receipts are handled differently from the Agent
// Deck reviewers it is supposed to be independent of. Admitting one now would
// mean shipping an independence claim that has not been demonstrated.
var transportRoleOK = map[Transport]map[ProfileRole]bool{
	TransportAgentDeck: {
		RoleWorker: true, RoleReviewer: true, RoleDiagnostic: true,
		RoleInvestigat: true, RoleCheckpoint: true,
	},
	TransportMiniMaxACP: {
		RoleWorker: true,
		// Investigation is allowed, and it is NOT a hole in the separation of
		// duties. An investigator produces evidence; only a COMPLETION review
		// can approve, and that authority is unreachable from this role. The
		// restriction that matters — a reviewer must stay independent — holds,
		// because the reviewer role is still worker-exclusive here.
		RoleInvestigat: true,
	},
}

// Profile is one operator-approved execution preset.
type Profile struct {
	LauncherSHA256 string
	// Name is the only thing a caller may reference.
	Name string
	// Transport selects how the session is created and observed. Empty means
	// TransportAgentDeck, which is the legacy default.
	Transport Transport
	// Role is what this profile is permitted to do.
	Role ProfileRole
	// Tool is the runtime tool identifier, resolved to a CreateSpec.
	Tool string
	// AccountRegion records WHICH MiniMax deployment the operator's own login
	// authenticates, for a minimax-acp profile.
	//
	// It is not a routing choice this layer makes — it launches `mcode acp`
	// against whatever login the operator already has. It is recorded so that a
	// profile which does not correspond to the operator's account is refused
	// here rather than producing an authentication failure at the first turn,
	// which reads like a transport fault and is expensive to diagnose from the
	// other end. See account_region.go.
	AccountRegion AccountRegion
	// Provider identifies the approved backend. A non-native provider requires
	// an explicit launch command and isolated environment, rather than changing
	// a CLI's global provider configuration.
	Provider      string
	LaunchCommand []string
	LaunchEnv     map[string]string
	// Model and Effort are optional passthroughs. They are emitted only when the
	// operator configures them, because an unsupported flag makes the runtime
	// reject the whole launch.
	Model  string
	Effort string
	// Permission is the frozen permission mode for a transport that has one.
	//
	// It is empty for Agent Deck, which has no equivalent, and recorded rather
	// than defaulted: "whatever the server chose" and "default" are different
	// admissions and only the operator may make that choice.
	Permission string
	// Sandbox requests a container launch.
	Sandbox bool
	// Worktree requests an isolated git worktree.
	Worktree bool
	// ReadOnly declares that this profile must not modify the workspace.
	ReadOnly bool
	// Confinement is the mechanism the operator expects to bound writes. It is a
	// DECLARATION of intent, distinct from Proven, which is evidence.
	Confinement              string
	ConfinementReceipt       string
	ConfinementReceiptSHA256 string
	// Proven records that the operator has DEMONSTRATED this profile's write
	// scope through the real selected tool.
	//
	// It is set by an operator, never inferred, and never defaulted to true. A
	// profile that declares ReadOnly without this is refused rather than
	// launched, because a prompt saying "do not write" is not confinement.
	Proven bool
}

// TransportFor resolves the transport, defaulting an unset value to Agent Deck.
//
// An empty value is the LEGACY default rather than an error, so every profile
// written before transports existed keeps its meaning with no configuration
// change.
func (p Profile) TransportFor() Transport {
	if p.Transport == "" {
		return TransportAgentDeck
	}
	return p.Transport
}

// ConfinementFor reports the mechanism this profile would use.
func (p Profile) ConfinementFor() Confinement {
	if p.Tool == "codex" && p.ReadOnly && len(p.LaunchCommand) > 0 {
		return CodexReadOnly
	}
	switch {
	case p.Sandbox:
		return ConfinementSandbox
	case p.Worktree:
		return ConfinementWorktree
	default:
		return ConfinementNone
	}
}

// ErrUnknownProfile is returned for a name the operator has not approved.
var ErrUnknownProfile = errors.New("agent-engine: no such operator-approved profile")

// ErrRoleMismatch is returned when a profile exists but is not permitted for the
// role it was requested for.
var ErrRoleMismatch = errors.New("agent-engine: the profile is not approved for this role")

// ErrConfinementUnproven is returned when a profile needs a write restriction
// that has not been demonstrated through the real selected tool.
var ErrConfinementUnproven = errors.New("agent-engine: the profile's write confinement has not been demonstrated")

// ErrProfileNotIsolated is returned when the requested role would be filled by a
// session that is not independent of the implementer.
var ErrProfileNotIsolated = errors.New("agent-engine: the profile cannot be used for an independent review role")

// ResolveProfile looks up an operator-approved profile for a role.
//
// Three refusals, each for a different reason, because collapsing them into one
// message is what produces a deployment that "probably configured it right":
//
//   - the name is not approved at all;
//   - the name exists but is approved for a different role, which is how a
//     reviewer profile ends up being used as the implementer;
//   - the profile requires a write restriction that no one has demonstrated.
func (c *Config) ResolveProfile(name string, role ProfileRole) (Profile, error) {
	return c.resolveProfile(name, role, false)
}

func (c *Config) resolveProfile(name string, role ProfileRole, localPilot bool) (Profile, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return Profile{}, fmt.Errorf("%w: no profile was named", ErrUnknownProfile)
	}
	p, ok := c.Profiles[trimmed]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %q; a caller may reference only names the operator has approved", ErrUnknownProfile, trimmed)
	}
	if p.Role != role {
		return Profile{}, fmt.Errorf("%w: %q is approved for %s, not %s", ErrRoleMismatch, trimmed, p.Role, role)
	}
	// Transport/role compatibility is refused HERE, at resolution, so an
	// unsupported combination never becomes an admitted request that fails later
	// inside a supervision path.
	if allowed, known := transportRoleOK[p.TransportFor()]; !known || !allowed[role] {
		return Profile{}, fmt.Errorf("%w: transport %q cannot serve the %s role for profile %q",
			ErrTransportUnsupported, p.TransportFor(), role, trimmed)
	}
	if !localPilot || p.Role != RoleWorker || p.ReadOnly {
		if err := RequireQualifiedMiniMaxBoundary(p); err != nil {
			return Profile{}, err
		}
	}
	// A profile that needs a write restriction must have that restriction
	// demonstrated. This is the refusal that keeps a read-only pilot honest.
	if p.ReadOnly && p.Tool == "codex" {
		digest, err := c.VerifyConfinementReceipt(p, "")
		if err != nil {
			return Profile{}, err
		}
		p.Proven = true
		p.Confinement = string(CodexReadOnly)
		p.ConfinementReceiptSHA256 = digest
	}
	if p.ReadOnly && !p.Proven {
		return Profile{}, fmt.Errorf("%w: %q declares read-only but its confinement (%s) has not been "+
			"demonstrated through the real selected tool; launching it would be a prompt, not a boundary",
			ErrConfinementUnproven, trimmed, p.ConfinementFor())
	}
	if _, err := p.CreateSpec("/", ""); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// ProfilesFor returns the approved profile names for a role, for operator output.
func (c *Config) ProfilesFor(role ProfileRole) []string {
	var out []string
	for name, p := range c.Profiles {
		if p.Role == role {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// CheckProfileIsolation refuses a reviewer profile that would be filled by the
// implementer.
//
// Same CLI in two separate fresh sessions IS supported, because a vendor's tooling
// is not the thing that makes a review independent. What is not supported is the
// same SESSION, or a session that is a continuation of the implementer's own
// conversation.
func (c *Config) CheckProfileIsolation(workerProfile, reviewerProfile string) error {
	w, err := c.ResolveProfile(workerProfile, RoleWorker)
	if err != nil {
		return err
	}
	r, err := c.ResolveProfile(reviewerProfile, RoleReviewer)
	if err != nil {
		return err
	}
	if strings.TrimSpace(w.Name) == strings.TrimSpace(r.Name) {
		// The registry should not hold the same name in two roles, and it cannot,
		// so this is a defensive check on a hand-edited config.
		return fmt.Errorf("%w: %q cannot be both the implementer and the reviewer",
			ErrProfileNotIsolated, w.Name)
	}
	return nil
}

// CreateArgs returns the exact runtime argv for launching this profile.
//
// Model and Effort are included only when configured, because the runtime refuses
// an unknown flag for the selected tool and a launch that fails is worse than a
// launch that is less specific. No caller-supplied text ever reaches this list.
func (p Profile) CreateArgs(path string) []string {
	spec, err := p.CreateSpec(path, "")
	if err != nil {
		return nil
	}
	return spec.Args()
}

// ActivationLabel is the deterministic label a session is created under.
//
// It is derived rather than random so that a Create whose outcome is unknown can
// be reconciled by looking for exactly this label, instead of by creating another
// session and hoping. Root, task, role, generation and a nonce are all in it so
// two activations can never collide.
func ActivationLabel(rootID, taskID, role string, generation int, nonce string) string {
	sum := sha256Hex([]byte(strings.Join([]string{rootID, taskID, role, itoa(generation), nonce}, "\x00")))
	return fmt.Sprintf("agentengine-%s-%s-g%d-%s", sanitizeLabel(taskID), sanitizeLabel(role), generation, sum[:8])
}

// sanitizeLabel reduces a value to characters that are safe in a session title.
//
// A title becomes a tmux name and a filename, so anything outside a conservative
// set is replaced rather than escaped: an escape that behaves differently in two
// places is worse than a lossy name.
func sanitizeLabel(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		out = "x"
	}
	return out
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }

// ProfileRef records the resolved profile identity in task state.
//
// The identity is pinned, not the name alone: an operator may later edit what
// "worker" means, and a request admitted under the old meaning must keep running
// under it rather than silently inheriting the new one.
type ProfileRef struct {
	LauncherSHA256 string      `json:"launcher_sha256,omitempty"`
	Name           string      `json:"name"`
	Role           ProfileRole `json:"role"`
	// Transport is PINNED at admission, so a later configuration edit cannot
	// change how an in-flight request is launched or observed.
	Transport Transport `json:"transport"`
	// Permission is the frozen permission mode this profile was admitted with.
	// It is pinned for the same reason Transport is: an operator editing the
	// configuration later must not change the permission contract under a request
	// that was admitted against the old one.
	Permission string `json:"permission,omitempty"`
	// Confinement is pinned alongside it: the transport says HOW the session is
	// reached, the confinement says WHAT it is permitted to change. Conflating
	// them is what makes "which harness" and "what can it write" one question,
	// and they are not.
	Confinement              string `json:"confinement,omitempty"`
	ConfinementReceipt       string `json:"confinement_receipt,omitempty"`
	ConfinementReceiptSHA256 string `json:"confinement_receipt_sha256,omitempty"`
	// AccountRegion is pinned for the same reason Transport is: the deployment a
	// request was admitted against is the deployment it must keep running
	// against. A later edit to `region` must not silently re-aim an in-flight
	// request at a different MiniMax account.
	AccountRegion AccountRegion     `json:"account_region,omitempty"`
	Tool          string            `json:"tool,omitempty"`
	Provider      string            `json:"provider,omitempty"`
	LaunchCommand []string          `json:"launch_command,omitempty"`
	LaunchEnv     map[string]string `json:"launch_env,omitempty"`
	Model         string            `json:"model,omitempty"`
	Effort        string            `json:"effort,omitempty"`
	Sandbox       bool              `json:"sandbox,omitempty"`
	Worktree      bool              `json:"worktree,omitempty"`
	ReadOnly      bool              `json:"read_only,omitempty"`
	// Proven records the confinement evidence that existed at admission.
	Proven bool `json:"confinement_proven"`
}

// Ref converts a resolved profile into its durable identity.
func (p Profile) Ref() ProfileRef {
	return ProfileRef{
		Name: p.Name, Role: p.Role, LauncherSHA256: p.LauncherSHA256, Transport: p.TransportFor(), Confinement: p.Confinement,
		ConfinementReceipt: p.ConfinementReceipt, ConfinementReceiptSHA256: p.ConfinementReceiptSHA256,
		Permission: p.Permission, AccountRegion: p.AccountRegion,
		Tool: p.Tool, Model: p.Model, Effort: p.Effort,
		Provider: p.Provider, LaunchCommand: append([]string(nil), p.LaunchCommand...), LaunchEnv: cloneEnv(p.LaunchEnv),
		Sandbox: p.Sandbox, Worktree: p.Worktree, ReadOnly: p.ReadOnly, Proven: p.Proven,
	}
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func (r ProfileRef) Profile() Profile {
	// Transport and Confinement are restored here, and their absence from this
	// list was a silent-loss bug: Ref() pins both, so a profile reconstructed
	// from its own pinned identity would come back with an EMPTY transport and
	// silently fall back to Agent Deck. A pinned identity that does not survive
	// the round trip is not pinned.
	return Profile{
		Name: r.Name, Role: r.Role, LauncherSHA256: r.LauncherSHA256, Tool: r.Tool, Provider: r.Provider,
		Model: r.Model, Effort: r.Effort, Sandbox: r.Sandbox, Worktree: r.Worktree,
		ReadOnly: r.ReadOnly, Proven: r.Proven, AccountRegion: r.AccountRegion,
		Transport: r.Transport, Confinement: r.Confinement, Permission: r.Permission,
		ConfinementReceipt: r.ConfinementReceipt, ConfinementReceiptSHA256: r.ConfinementReceiptSHA256,
		LaunchCommand: append([]string(nil), r.LaunchCommand...), LaunchEnv: cloneEnv(r.LaunchEnv),
	}
}

// CreateSpec is the production launch builder. Custom commands replace the
// tool command through --wrapper, so unsupported native model flags are never
// sent for a custom CLI. Values become single shell words, never shell syntax.
func (p Profile) CreateSpec(path, title string) (agentdeck.CreateSpec, error) {
	s := agentdeck.CreateSpec{Path: path, Title: title, Tool: p.Tool, Model: p.Model, Effort: p.Effort, Sandbox: p.Sandbox, NoParent: true}
	if p.Transport == TransportCursorCLI {
		return s, fmt.Errorf("profile %q: cursor-cli reports execute through their native transport, not Agent Deck", p.Name)
	}
	if err := validateCodexEffort(p); err != nil {
		return s, err
	}
	if p.Worktree {
		return s, fmt.Errorf("profile %q: worktree activation requires branch and verification-workspace binding; this control path does not support it", p.Name)
	}
	if len(p.LaunchCommand) == 0 {
		if (p.Provider != "" && p.Provider != "native") || len(p.LaunchEnv) != 0 {
			return s, fmt.Errorf("profile %q: provider/environment override requires launch_command", p.Name)
		}
		return s, nil
	}
	if !strings.HasPrefix(p.LaunchCommand[0], "/") {
		return s, fmt.Errorf("profile %q: launch_command executable must be absolute", p.Name)
	}
	joined := strings.Join(p.LaunchCommand, "\x00")
	if p.Model == "" || !strings.Contains(joined, "{model}") {
		return s, fmt.Errorf("profile %q: custom launch requires model and a {model} argument", p.Name)
	}
	if p.Effort != "" && !strings.Contains(joined, "{effort}") {
		return s, fmt.Errorf("profile %q: selected effort requires an {effort} argument", p.Name)
	}
	var words []string
	if len(p.LaunchEnv) > 0 {
		words = append(words, "env")
		keys := make([]string, 0, len(p.LaunchEnv))
		for k := range p.LaunchEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !validEnvName(k) {
				return s, fmt.Errorf("profile %q: invalid environment name %q", p.Name, k)
			}
			if strings.Contains(p.LaunchEnv[k], "{command}") || strings.ContainsRune(p.LaunchEnv[k], 0) {
				return s, fmt.Errorf("profile %q: invalid environment value", p.Name)
			}
			words = append(words, shellWord(k+"="+p.LaunchEnv[k]))
		}
	}
	for _, arg := range p.LaunchCommand {
		arg = strings.NewReplacer("{model}", p.Model, "{effort}", p.Effort, "{provider}", p.Provider).Replace(arg)
		if strings.Contains(arg, "{command}") || strings.ContainsRune(arg, 0) {
			return s, fmt.Errorf("profile %q: invalid launch argument", p.Name)
		}
		words = append(words, shellWord(arg))
	}
	s.Model = ""
	s.Effort = ""
	s.Wrapper = strings.Join(words, " ")
	return s, nil
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (s *State) ReviewSession(kind ReviewKind) string {
	switch kind {
	case ReviewCheckpoint:
		if s.CheckpointSession != "" {
			return s.CheckpointSession
		}
		if _, ok := s.ExecutionProfiles[RoleCheckpoint]; ok {
			return ""
		}
	case ReviewDiagnostic:
		if s.DiagnosticSession != "" {
			return s.DiagnosticSession
		}
		if _, ok := s.ExecutionProfiles[RoleDiagnostic]; ok {
			return ""
		}
	}
	return s.AuditorSession
}
