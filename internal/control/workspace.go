package control

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Workspace capability, leases and immutable snapshots.
//
// The uncomfortable finding this stage has to encode is that most of what a
// supervisor would like to claim about confinement it cannot actually claim.
//
// A source hash detects that something was written to a read-only tree. It does
// not prevent the write. A same-user `chmod` is not a security boundary from the
// same user. A cooperative lease serialises processes that agree to honour it
// and does nothing about a process that does not. So:
//
//   - The snapshot is real, reproducible and load-bearing: checks and audits read
//     pinned content, and a receipt carries the digest of exactly what was read.
//   - The lease is real and is honestly labelled as a serialisation mechanism for
//     registered operations, not as containment.
//   - Read-only execution is BLOCKED unless confinement is *demonstrated*. It is
//     never inferred from a promise in configuration, because a promise is
//     precisely what a prompt-shaped "sandbox" is.

// Capability is what a request is permitted to do to the workspace.
type Capability string

const (
	// CapabilityReadOnly permits reading the source and writing only to a
	// bounded output area.
	CapabilityReadOnly Capability = "READ_ONLY"
	// CapabilityScopedWrite permits writing inside declared allowed paths and
	// never inside declared protected paths.
	CapabilityScopedWrite Capability = "SCOPED_WRITE"
)

// CapabilityDecision is the outcome of a capability preflight.
type CapabilityDecision struct {
	// Allowed is false when the request must be BLOCKED at admission.
	Allowed bool
	// Capability is what would be granted.
	Capability Capability
	// Reason explains a refusal, for the operator.
	Reason string
	// BoundedOutput is the only directory writable under a read-only grant.
	BoundedOutput string
}

// ErrNoConfinement is returned when an operation requires confinement that cannot
// be demonstrated.
//
// It is a refusal, not a warning. Returning "probably fine" here would let a
// read-only investigation modify the snapshot it is reporting on, and every
// digest recorded afterwards would then describe content nobody intended to
// attest to.
var ErrNoConfinement = errors.New("agent-engine: the environment cannot demonstrate the confinement this operation requires")

// PreflightCapability decides what a request may do before anything runs.
//
// For a write operation it checks that permission was explicitly granted and
// that allowed and protected paths do not overlap. For a read-only operation it
// requires a bounded output area, because a read-only grant with nowhere to
// write its findings is not a usable operation.
func (s *Store) PreflightCapability(requestID string) (CapabilityDecision, error) {
	adm, err := s.loadRequestDir(requestID)
	if err != nil {
		return CapabilityDecision{}, err
	}
	conf := adm.Request.Constraints

	// A protected path that is also allowed is a contradiction that would be
	// resolved by whichever check ran last. Refuse it here rather than depend on
	// evaluation order later.
	allowed := map[string]bool{}
	for _, p := range conf.AllowedPaths {
		allowed[normalizeScope(p)] = true
	}
	for _, p := range conf.ProtectedPaths {
		if allowed[normalizeScope(p)] {
			return CapabilityDecision{
				Reason: fmt.Sprintf("%q is both allowed and protected; the grant is contradictory", p),
			}, nil
		}
	}

	if adm.Request.Operation.CodeChangeAllowed() {
		if !conf.AllowCodeChanges {
			return CapabilityDecision{Reason: "a repair must grant code changes explicitly"}, nil
		}
		return CapabilityDecision{
			Allowed:    true,
			Capability: CapabilityScopedWrite,
			Reason:     "code changes explicitly permitted inside the declared scope",
		}, nil
	}

	// Read-only needs somewhere to put its output.
	if s.cfg.OutputRoot == "" {
		return CapabilityDecision{
			Reason: "no output root is configured, so a read-only operation has nowhere to record findings",
		}, nil
	}
	return CapabilityDecision{
		Allowed:       true,
		Capability:    CapabilityReadOnly,
		BoundedOutput: s.cfg.OutputRoot,
		Reason:        "read-only with a bounded output area",
	}, nil
}

func normalizeScope(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(p, "/**")
	return strings.TrimSuffix(p, "/")
}

// --- leases ----------------------------------------------------------------

// ErrWorkspaceBusy is returned when another registered request already holds the
// workspace lease.
var ErrWorkspaceBusy = errors.New("agent-engine: another request holds this workspace lease")

// leasePath returns the lease file for a workspace within this store.
//
// The lease is keyed by the digest of the canonical workspace path, so two
// spellings of the same directory cannot take two different leases. That
// canonicalisation is the whole point: two leases over one directory would
// serialise nothing.
func (s *Store) leasePath(workspace string) (string, error) {
	root, err := canonicalPath(workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.cfg.StoreRoot, LeaseDirName, sha256Hex([]byte(root))+".lock"), nil
}

// AcquireWorkspaceLease takes the registered-operation lease for a workspace.
//
// This serialises cooperating callers and nothing more. It is not a security
// boundary, it does not confine writes, and a process that does not know about
// it is entirely unaffected. Callers must not treat holding it as meaning the
// workspace cannot change.
func (s *Store) AcquireWorkspaceLease(workspace, requestID string) (release func(), err error) {
	path, err := s.leasePath(workspace)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("agent-engine: creating lease dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("agent-engine: opening lease: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrWorkspaceBusy, path)
		}
		return nil, fmt.Errorf("agent-engine: taking lease: %w", err)
	}
	_ = os.WriteFile(path, []byte(requestID+"\n"), 0o600)
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// acquireEpisodeWorkspaces covers the primary workspace and every registered
// bundle target, so a single-target caller cannot overlap an active bundle.
func (s *Store) acquireEpisodeWorkspaces(state *State) (func(), error) {
	var roots []string
	if state.Workspace != "" {
		roots = append(roots, state.Workspace)
	}
	for _, target := range state.RepairBundleTargets {
		roots = append(roots, target.Workspace)
	}
	canonical := map[string]bool{}
	for _, root := range roots {
		path, err := canonicalPath(root)
		if err != nil {
			return nil, err
		}
		canonical[path] = true
	}
	roots = roots[:0]
	for path := range canonical {
		roots = append(roots, path)
	}
	sort.Strings(roots)
	var releases []func()
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, root := range roots {
		release, err := s.AcquireWorkspaceLease(root, state.RequestID)
		if err != nil {
			releaseAll()
			return nil, err
		}
		releases = append(releases, release)
	}
	return releaseAll, nil
}

// canonicalPath resolves a path to its canonical form, following symlinks.
//
// A lease keyed on a symlink's own name rather than its target would let the
// same directory be leased twice through two paths, which is exactly the race
// the lease exists to prevent.
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("agent-engine: resolving %s: %w", p, err)
	}
	// EvalSymlinks fails on a path that does not exist yet, which is a legitimate
	// state for a worktree about to be created. Falling back to the cleaned
	// absolute path is correct there.
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved, nil
	}
	return filepath.Clean(abs), nil
}

// --- immutable snapshots ---------------------------------------------------

// Snapshot is a reproducible digest of approved source content.
//
// It is what makes a receipt mean something. A check that ran against a moving
// worktree produced a result about bytes that no longer exist, so every
// verification receipt carries the snapshot it read and is invalidated when the
// content changes.
type Snapshot struct {
	Files map[string]string `json:"files,omitempty"`
	// SnapshotID identifies this snapshot within the request.
	SnapshotID string `json:"snapshot_id"`
	// Root is the canonical directory the snapshot covers.
	Root string `json:"root"`
	// ContentDigest is the digest over the sorted per-file digests.
	ContentDigest string `json:"content_digest"`
	// FileCount is how many files were included.
	FileCount int `json:"file_count"`
	// Excluded names the paths deliberately left out, so an exclusion is a
	// recorded decision rather than an invisible gap.
	Excluded []string `json:"excluded,omitempty"`
	// TakenAt is when the snapshot was taken.
	TakenAt string `json:"taken_at"`
}

// excludedNames are never part of a snapshot.
//
// These are generated output and tool caches. They are excluded by NAME because
// the name is the property that identifies them, and none of them is source a
// check would ever need to read.
//
// A control directory is deliberately NOT in this list. Excluding a module by
// name meant that verifying Agent Engine itself silently snapshotted an empty tree,
// which is the worst possible failure for a trust boundary: the check would pass
// against content that was never examined. The control store is excluded by
// PATH instead, because what must be avoided is a specific self-referential
// directory, not any directory that happens to be called "agent-engine".
var excludedNames = map[string]bool{
	".agent-workflow": true,
	".git":            true,
	"node_modules":    true,
	".agent-deck":     true,
	".DS_Store":       true,
	".venv":           true,
	"__pycache__":     true,
	".pytest_cache":   true,
	".mypy_cache":     true,
	"dist":            true,
	".next":           true,
	".turbo":          true,
}

// TakeSnapshot computes a reproducible digest of a source tree.
//
// "Reproducible" is the property that matters: the same content must always
// produce the same digest, on any machine, at any time, so a receipt recorded
// yesterday can be checked today. Three things make that true here — files are
// TakeSnapshot computes a reproducible digest of a source tree.
//
// "Reproducible" is the property that matters: the same content must always
// produce the same digest, on any machine, at any time, so a receipt recorded
// yesterday can be checked today. Three things make that true here — files are
// hashed by content only, the per-file digests are sorted before combining, and
// volatile metadata such as modification time is excluded. A digest that moved
// because a file was touched would invalidate every receipt on a `touch`.
//
// excludePaths names absolute directories to leave out regardless of their
// name. It exists for the control store when the store happens to live inside
// the workspace: that directory describes the snapshot, so including it would
// make the digest depend on its own output. It is a path, not a name, so a
// module directory called "agent-engine" is ordinary subject matter.
func TakeSnapshot(root string, now time.Time, excludePaths ...string) (Snapshot, error) {
	canon, err := canonicalPath(root)
	if err != nil {
		return Snapshot{}, err
	}
	info, err := os.Stat(canon)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent-engine: snapshot root: %w", err)
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("agent-engine: snapshot root %s is not a directory", canon)
	}

	excludedRoots := make([]string, 0, len(excludePaths))
	for _, p := range excludePaths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if c, cerr := canonicalPath(p); cerr == nil {
			excludedRoots = append(excludedRoots, c)
		}
	}
	// A path equal to the snapshot root would exclude everything, which is never
	// what the caller meant.
	filtered := excludedRoots[:0]
	for _, p := range excludedRoots {
		if p != canon {
			filtered = append(filtered, p)
		}
	}
	excludedRoots = filtered

	type entry struct {
		rel  string
		hash string
	}
	var entries []entry
	var excluded []string

	walkErr := filepath.Walk(canon, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			// A single unreadable path does not silently become "unchanged". It is
			// skipped and recorded, so a snapshot that could not see everything says
			// so in its own output.
			rel, _ := filepath.Rel(canon, path)
			excluded = append(excluded, rel)
			return nil
		}
		rel, err := filepath.Rel(canon, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		base := filepath.Base(path)
		if fi.IsDir() {
			// An excluded absolute directory is pruned whole, so nothing under it
			// is enumerated and nothing under it can perturb the digest.
			for _, ex := range excludedRoots {
				if path == ex || strings.HasPrefix(path, ex+string(filepath.Separator)) {
					excluded = append(excluded, rel+"/")
					return filepath.SkipDir
				}
			}
			if excludedNames[base] {
				excluded = append(excluded, rel+"/")
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			// Symlinks and devices are not source content; recording them would
			// make the digest depend on what they point at right now.
			return nil
		}
		if excludedNames[base] {
			excluded = append(excluded, rel)
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			excluded = append(excluded, rel)
			return nil
		}
		entries = append(entries, entry{rel: filepath.ToSlash(rel), hash: sha256Hex(data)})
		return nil
	})
	if walkErr != nil {
		return Snapshot{}, walkErr
	}

	// Sorting the per-file digests is what makes the combination order-independent.
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", e.rel, e.hash)
	}
	sort.Strings(excluded)

	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		files[entry.rel] = entry.hash
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))
	return Snapshot{
		Files:         files,
		SnapshotID:    "S-" + sum[:12],
		Root:          canon,
		ContentDigest: sum,
		FileCount:     len(entries),
		Excluded:      excluded,
		TakenAt:       now.UTC().Format(time.RFC3339),
	}, nil
}

// Verify re-reads the tree and reports whether it still matches.
//
// This is how a manual edit outside the control layer is caught. It detects; it
// does not prevent, and the distinction is stated at every call site rather than
// left for a reader to assume.
func (s Snapshot) Verify(now time.Time) (bool, Snapshot, error) {
	fresh, err := TakeSnapshot(s.Root, now)
	if err != nil {
		return false, Snapshot{}, err
	}
	return fresh.ContentDigest == s.ContentDigest, fresh, nil
}

// PathTouched reports whether a changed path falls inside this snapshot's root.
func (s Snapshot) Covers(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(s.Root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
