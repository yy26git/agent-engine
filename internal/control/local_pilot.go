package control

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

const AssuranceTrustedLocal = "trusted-local"

// LocalPilotGrant comes only from CONTROL.toml, never a caller request. It
// permits ordinary host execution, not a qualified confinement mechanism.
type LocalPilotGrant struct {
	RequestID           string `json:"request_id"`
	Workspace           string `json:"workspace"`
	WorkerProfile       string `json:"worker_profile"`
	ExpiresAt           string `json:"expires_at"`
	MaxWallSeconds      int    `json:"max_wall_seconds"`
	MaxRepairIterations int    `json:"max_repair_iterations"`
	ProfileSHA256       string `json:"profile_sha256"`
	SHA256              string `json:"sha256"`
}

func profileDigest(p Profile) string {
	b, _ := json.Marshal(p.Ref())
	return sha256Hex(b)
}

func (g LocalPilotGrant) digest() string {
	g.SHA256 = ""
	b, _ := json.Marshal(g)
	return sha256Hex(b)
}

func canonicalPilotWorkspace(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("pilot workspace must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve pilot workspace: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("pilot workspace must be an existing directory")
	}
	return canonical, nil
}

func (g LocalPilotGrant) validate(now time.Time, requestID, workspace string) error {
	if err := contract.ValidateRequestID(g.RequestID); err != nil {
		return err
	}
	if g.RequestID != requestID || g.WorkerProfile == "" {
		return fmt.Errorf("pilot grant does not name this request and worker")
	}
	root, err := canonicalPilotWorkspace(workspace)
	if err != nil {
		return err
	}
	if root != g.Workspace {
		return fmt.Errorf("pilot grant is for workspace %q, not %q", g.Workspace, root)
	}
	expiry, err := time.Parse(time.RFC3339, g.ExpiresAt)
	if err != nil || !now.Before(expiry) {
		return fmt.Errorf("pilot grant is expired or has an invalid expires_at")
	}
	if g.MaxWallSeconds <= 0 || g.MaxRepairIterations <= 0 {
		return fmt.Errorf("pilot requires positive wall-clock and repair ceilings")
	}
	return nil
}

func loadLocalPilots(doc map[string]any, cfg *Config) error {
	raw, present := doc["local_pilots"]
	if !present {
		return nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return cfgErr("local_pilots", "expected request-keyed tables")
	}
	cfg.LocalPilots = make(map[string]LocalPilotGrant, len(table))
	for id, entry := range table {
		field := "local_pilots." + id
		t, ok := entry.(map[string]any)
		if !ok {
			return cfgErr(field, "expected a grant table")
		}
		for key := range t {
			switch key {
			case "workspace", "worker_profile", "expires_at", "max_wall_seconds", "max_repair_iterations":
			default:
				return cfgErr(field+"."+key, "unknown pilot setting")
			}
		}
		g := LocalPilotGrant{RequestID: id, Workspace: stringField(t, "workspace"),
			WorkerProfile: stringField(t, "worker_profile"), ExpiresAt: stringField(t, "expires_at"),
			MaxWallSeconds: intField(t, "max_wall_seconds"), MaxRepairIterations: intField(t, "max_repair_iterations")}
		if err := contract.ValidateRequestID(id); err != nil {
			return cfgErr(field, "%s", err)
		}
		if !filepath.IsAbs(g.Workspace) || strings.TrimSpace(g.WorkerProfile) == "" {
			return cfgErr(field, "absolute workspace and worker_profile required")
		}
		if _, err := time.Parse(time.RFC3339, g.ExpiresAt); err != nil {
			return cfgErr(field+".expires_at", "RFC3339 expiry required")
		}
		if g.MaxWallSeconds <= 0 || g.MaxRepairIterations <= 0 ||
			g.MaxWallSeconds > int(cfg.Checkpoints.MaxWallDuration/time.Second) || g.MaxRepairIterations > cfg.Limits.RepairCycles {
			return cfgErr(field, "positive pilot ceilings must fit local wall-clock and repair limits")
		}
		cfg.LocalPilots[id] = g
	}
	return nil
}

// localPilotFor pins a local grant and the worker it actually authorizes.
func (s *Store) localPilotFor(ep *contract.EpisodeRequest, template EpisodeTemplate) (*LocalPilotGrant, error) {
	g, ok := s.cfg.LocalPilots[ep.EpisodeID]
	if !ok {
		return nil, nil
	}
	if template.ReadOnly || template.Roles[RoleWorker] != g.WorkerProfile {
		return nil, fmt.Errorf("local pilot grant does not match the writable template's worker")
	}
	root, err := canonicalPilotWorkspace(g.Workspace)
	if err != nil {
		return nil, err
	}
	g.Workspace = root
	if err := g.validate(s.now(), ep.EpisodeID, ep.Workspace.Path); err != nil {
		return nil, err
	}
	if g.MaxWallSeconds > int(s.cfg.Checkpoints.MaxWallDuration/time.Second) || g.MaxRepairIterations > s.cfg.Limits.RepairCycles {
		return nil, fmt.Errorf("pilot ceilings exceed local policy")
	}
	p, exists := s.cfg.Profiles[g.WorkerProfile]
	if !exists || p.Role != RoleWorker || p.ReadOnly {
		return nil, fmt.Errorf("pilot must name an approved writable worker")
	}
	if p.TransportFor() == TransportMiniMaxACP && strings.TrimSpace(p.Model) == "" {
		return nil, fmt.Errorf("MiniMax pilot requires an explicit worker model selector")
	}
	g.ProfileSHA256 = profileDigest(p)
	g.SHA256 = g.digest()
	return &g, nil
}

// AuthorizeProfileExecution checks the frozen grant at real start/resume seams.
// It never reads a later local grant into an old admission. Cleanup does not
// call this gate: expiry may stop new execution, but cannot prevent termination.
func (s *State) AuthorizeProfileExecution(p Profile, workspace string, now time.Time) error {
	if err := s.VerifyRepairBranch(); err != nil {
		return err
	}
	if s.LocalPilot == nil {
		return RequireQualifiedMiniMaxBoundary(p)
	}
	g := s.LocalPilot
	if s.ExecutionAssurance != AssuranceTrustedLocal || g.SHA256 == "" || g.SHA256 != g.digest() {
		return fmt.Errorf("invalid frozen trusted-local pilot authorization")
	}
	if err := g.validate(now, s.RequestID, workspace); err != nil {
		return err
	}
	if s.Workspace != g.Workspace || s.Episode == nil ||
		s.Episode.Budget.WallSeconds > g.MaxWallSeconds || s.Episode.Budget.RepairIterations > g.MaxRepairIterations {
		return fmt.Errorf("pilot state scope or budgets exceed its frozen grant")
	}
	pinned, ok := s.ExecutionProfiles[p.Role]
	if !ok || profileDigest(pinned.Profile()) != profileDigest(p) {
		return fmt.Errorf("profile is not pinned to this pilot role")
	}
	worker, ok := s.ExecutionProfiles[RoleWorker]
	if !ok || worker.Name != g.WorkerProfile || profileDigest(worker.Profile()) != g.ProfileSHA256 {
		return fmt.Errorf("pilot worker differs from its frozen authorization")
	}
	if p.TransportFor() == TransportMiniMaxACP && (p.Role != RoleWorker || p.ReadOnly || p.Name != g.WorkerProfile) {
		return ErrConfinementUnproven
	}
	return nil
}
