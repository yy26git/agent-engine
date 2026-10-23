package main

import "github.com/emjin/agent-engine/internal/control"

// profileProjection is the public, display-only subset of an admitted profile.
// In particular, it intentionally excludes launch commands, launch environment
// values and receipt paths from ProfileRef.
type profileProjection struct {
	Name          string `json:"name"`
	Profile       string `json:"profile"`
	Role          string `json:"role"`
	Transport     string `json:"transport"`
	Permission    string `json:"permission,omitempty"`
	Confinement   string `json:"confinement,omitempty"`
	Proven        bool   `json:"confinement_proven"`
	AccountRegion string `json:"account_region,omitempty"`
	Tool          string `json:"tool,omitempty"`
	CLI           string `json:"cli,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	Sandbox       bool   `json:"sandbox,omitempty"`
	Worktree      bool   `json:"worktree,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"`
}

func projectProfile(profile control.ProfileRef) profileProjection {
	return profileProjection{
		Name: profile.Name, Profile: profile.Name, Role: string(profile.Role),
		Transport: string(profile.Transport), Permission: profile.Permission,
		Confinement: profile.Confinement, Proven: profile.Proven,
		AccountRegion: string(profile.AccountRegion), Tool: profile.Tool, CLI: profile.Tool,
		Provider: profile.Provider, Model: profile.Model, Effort: profile.Effort,
		Sandbox: profile.Sandbox, Worktree: profile.Worktree, ReadOnly: profile.ReadOnly,
	}
}

func publicExecutionProfiles(profiles map[control.ProfileRole]control.ProfileRef) map[string]profileProjection {
	public := make(map[string]profileProjection, len(profiles))
	for role, profile := range profiles {
		public[string(role)] = projectProfile(profile)
	}
	return public
}
