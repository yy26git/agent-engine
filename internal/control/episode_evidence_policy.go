package control

import "fmt"

const (
	maxEpisodeProducerEvidenceBytes   = 4 << 20
	defaultEpisodeEvidenceBytes       = 64 << 20
	defaultEpisodeEvidencePromptBytes = 128 << 10
)

// EpisodeEvidencePolicy is local storage/presentation policy, never caller input.
// The episode budget includes all retained episode-artifacts, including files
// left by interrupted intake, so retries cannot evade it with uncommitted bytes.
type EpisodeEvidencePolicy struct {
	MaxProducerBytes int
	MaxEpisodeBytes  int
	MaxPromptBytes   int
}

func defaultEpisodeEvidencePolicy() EpisodeEvidencePolicy {
	return EpisodeEvidencePolicy{maxEpisodeProducerEvidenceBytes, defaultEpisodeEvidenceBytes, defaultEpisodeEvidencePromptBytes}
}

func (p EpisodeEvidencePolicy) validate() error {
	if p.MaxProducerBytes <= 0 || p.MaxEpisodeBytes < p.MaxProducerBytes || p.MaxPromptBytes <= 0 || p.MaxPromptBytes > p.MaxProducerBytes {
		return cfgErr("evidence", "require positive max_prompt_bytes <= max_producer_bytes <= max_episode_bytes")
	}
	return nil
}

func (c *Config) episodeEvidencePolicy() EpisodeEvidencePolicy {
	if c.Evidence == (EpisodeEvidencePolicy{}) {
		return defaultEpisodeEvidencePolicy()
	}
	return c.Evidence
}

// Only policy refusals are permanent. Filesystem/transport errors retain their
// original type and are not turned into irreversible budget refusals.
type episodeEvidenceRefusal struct {
	Scope  string
	Actual int64
	Limit  int64
	Digest string
}

// A deterministic refusal that could not be saved must stop the controller.
// Treating it as waiting would repeat the same rejected turn forever.
type episodeEvidenceCommitError struct {
	Refusal error
	Err     error
}

func (e *episodeEvidenceCommitError) Error() string {
	return fmt.Sprintf("%v; durable refusal could not be committed: %v", e.Refusal, e.Err)
}

func (e *episodeEvidenceCommitError) Unwrap() error { return e.Err }

// EpisodeEvidenceRejection survives supervisor restart. It names refused
// bytes, not retained proof, and never permits the phase to advance.
type EpisodeEvidenceRejection struct {
	Phase      string `json:"phase,omitempty"`
	StepKey    string `json:"step_key,omitempty"`
	Scope      string `json:"scope"`
	Actual     int64  `json:"actual_bytes"`
	Limit      int64  `json:"limit_bytes"`
	Digest     string `json:"sha256"`
	NativeTurn string `json:"native_turn,omitempty"`
}

func (e *episodeEvidenceRefusal) Error() string {
	return fmt.Sprintf("agent-engine: %s evidence retention refused: %d bytes exceeds %d-byte limit (sha256=%s); complete evidence required, truncation refused; resolve the local evidence policy and submit a new episode", e.Scope, e.Actual, e.Limit, e.Digest)
}
