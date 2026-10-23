package agentdeck

// The runtime pin.
//
// Agent Engine reaches Agent Deck only through its CLI, which is the right boundary —
// but a boundary is only as trustworthy as the thing on the other side of it. A
// configured path is not an identity: it can be repointed, the file at that path
// can be replaced, and a name on PATH can resolve to something else entirely.
//
// So the runtime is pinned on two independent axes:
//
//   - the SOURCE commit the executable was built from, and
//   - the measured SHA-256 of the executable itself.
//
// The commit alone is not enough, because one commit builds differently on
// different platforms and toolchains. The hash alone is not enough either,
// because a hash says nothing about provenance: it cannot distinguish the
// reviewed artifact from a rebuilt copy of modified source. Requiring both means
// neither a substituted binary nor an unreviewed build can pass.
//
// Verification happens before any runtime effect, and a refusal is explicit. A
// deployment that has not measured its artifact is BLOCKED, never allowed to
// "probably be fine" — an unpinned runtime is precisely the condition under which
// a false PASS becomes possible, because the reviewing session and the reviewed
// work could be running different code.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrUnpinnedRuntime is returned when a deployment has no approved artifact
// identity, or the configured artifact does not match it.
var ErrUnpinnedRuntime = errors.New("agentdeck: the runtime is not pinned to an approved artifact")

// RuntimeLock is the reviewed, non-secret dependency manifest.
//
// It records what was reviewed and where. It deliberately holds no credentials
// and no build secrets: it is a statement about identity, and it is expected to
// be read by anyone operating the deployment.
type RuntimeLock struct {
	// Schema guards the on-disk format.
	Schema int `json:"schema"`
	// Repository is the upstream the source was taken from.
	Repository string `json:"repository"`
	// SourceCommit is the exact reviewed upstream commit.
	SourceCommit string `json:"source_commit"`
	// Artifacts maps "<goos>/<goarch>" to that platform's measured executable.
	Artifacts map[string]LockedArtifact `json:"artifacts"`
}

// LockedArtifact is one platform's reviewed executable.
type LockedArtifact struct {
	// Path is the absolute operator-owned install location.
	Path string `json:"path"`
	// SHA256 is the measured digest of the executable at that path.
	SHA256 string `json:"sha256"`
	// BuildReceipt is the provenance record for this build.
	BuildReceipt string `json:"build_receipt,omitempty"`
}

// platformKey is the lock key for the running platform.
func platformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

// LoadRuntimeLock reads a lock manifest.
func LoadRuntimeLock(path string) (*RuntimeLock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the runtime lock %s: %v", ErrUnpinnedRuntime, path, err)
	}
	var lock RuntimeLock
	// Strict decoding: an unknown field in a trust manifest is either a typo that
	// silently disables a pin, or a field from a newer build whose meaning this
	// one does not implement. Both must fail loudly.
	if err := decodeStrict(data, &lock); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnpinnedRuntime, path, err)
	}
	if lock.Schema == 0 {
		return nil, fmt.Errorf("%w: %s declares no schema version", ErrUnpinnedRuntime, path)
	}
	if strings.TrimSpace(lock.SourceCommit) == "" {
		return nil, fmt.Errorf("%w: %s pins no source commit", ErrUnpinnedRuntime, path)
	}
	if len(lock.Artifacts) == 0 {
		return nil, fmt.Errorf("%w: %s records no platform artifacts", ErrUnpinnedRuntime, path)
	}
	return &lock, nil
}

// ArtifactFor returns the approved artifact for a platform.
func (l *RuntimeLock) ArtifactFor(goos, goarch string) (LockedArtifact, error) {
	art, ok := l.Artifacts[goos+"/"+goarch]
	if !ok {
		return LockedArtifact{}, fmt.Errorf("%w: no approved artifact is recorded for %s/%s; "+
			"an unbuilt platform stays refused rather than falling back to another platform's binary",
			ErrUnpinnedRuntime, goos, goarch)
	}
	if strings.TrimSpace(art.SHA256) == "" {
		return LockedArtifact{}, fmt.Errorf("%w: the recorded artifact for %s/%s has no measured hash",
			ErrUnpinnedRuntime, goos, goarch)
	}
	return art, nil
}

// Pin is a verified runtime identity.
type Pin struct {
	SourceCommit string
	SHA256       string
	Path         string
	Platform     string
}

// String renders the pin for provenance records.
func (p Pin) String() string {
	return fmt.Sprintf("%s@%s (%s)", p.SourceCommit, p.SHA256[:12], p.Platform)
}

// VerifyPin resolves the configured binary against the lock and measures it.
//
// The order is deliberate and is the whole safety property:
//
//  1. The configured path must be absolute, so PATH resolution can never pick
//     the executable.
//  2. Symlinks are resolved, so a link cannot redirect the approved path to an
//     unreviewed file.
//  3. The resolved path must equal the approved path in the lock, so a
//     substituted location is refused even if it holds a matching file.
//  4. The measured digest must equal the approved digest.
//
// A refusal at any step returns ErrUnpinnedRuntime and the caller must not
// proceed to any runtime effect.
func VerifyPin(lock *RuntimeLock, configured string) (Pin, error) {
	if lock == nil {
		return Pin{}, fmt.Errorf("%w: no runtime lock was supplied", ErrUnpinnedRuntime)
	}
	trimmed := strings.TrimSpace(configured)
	if trimmed == "" {
		return Pin{}, fmt.Errorf("%w: no agent_deck_binary is configured", ErrUnpinnedRuntime)
	}
	if !filepath.IsAbs(trimmed) {
		// A bare name would resolve through PATH, which is exactly the substitution
		// the pin exists to prevent.
		return Pin{}, fmt.Errorf("%w: agent_deck_binary must be an absolute path, not %q; "+
			"a name could resolve to a different executable at launch", ErrUnpinnedRuntime, trimmed)
	}
	art, err := lock.ArtifactFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Pin{}, err
	}
	resolved, err := filepath.EvalSymlinks(trimmed)
	if err != nil {
		return Pin{}, fmt.Errorf("%w: the configured runtime %s cannot be resolved: %v",
			ErrUnpinnedRuntime, trimmed, err)
	}
	approved, err := filepath.EvalSymlinks(art.Path)
	if err != nil {
		return Pin{}, fmt.Errorf("%w: the approved runtime %s cannot be resolved: %v",
			ErrUnpinnedRuntime, art.Path, err)
	}
	if resolved != approved {
		return Pin{}, fmt.Errorf("%w: agent_deck_binary resolves to %s but the approved artifact is %s; "+
			"refusing to run an unreviewed executable", ErrUnpinnedRuntime, resolved, approved)
	}
	measured, err := FileSHA256(resolved)
	if err != nil {
		return Pin{}, fmt.Errorf("%w: the approved runtime cannot be read: %v", ErrUnpinnedRuntime, err)
	}
	if !strings.EqualFold(measured, art.SHA256) {
		return Pin{}, fmt.Errorf("%w: the executable at %s hashes to %s but the lock approved %s; "+
			"it has been replaced since it was reviewed",
			ErrUnpinnedRuntime, resolved, measured, art.SHA256)
	}
	return Pin{
		SourceCommit: lock.SourceCommit,
		SHA256:       strings.ToLower(measured),
		Path:         resolved,
		Platform:     platformKey(),
	}, nil
}

// FileSHA256 measures a file's digest.
//
// The size is capped so a misconfigured path pointing at a very large file cannot
// turn a verification step into an unbounded read.
func FileSHA256(path string) (string, error) {
	const maxArtifactBytes = 1 << 30 // 1 GiB is far above any plausible runtime binary
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxArtifactBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxArtifactBytes {
		return "", fmt.Errorf("the file is larger than %d bytes and cannot be a runtime executable", maxArtifactBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decodeStrict decodes JSON refusing unknown fields, so a lock written by a
// newer build cannot be silently under-enforced by an older one.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Trailing content means the manifest is not what it claims to be.
	if dec.More() {
		return errors.New("the manifest has trailing content after the top-level object")
	}
	return nil
}
