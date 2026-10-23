package control

// Durable file writing, confinement and digests.
//
// This logic is relocated from the Agent Deck draft's internal/workflow/store.go
// (writeMachineFileUnlocked) and internal/workflow/transaction.go, with
// attribution preserved. It is carried over rather than reimplemented because
// the four properties it provides are the ones that make a decision
// recoverable, and they are easy to get subtly wrong:
//
//   - Confinement: a write must stay inside the store it belongs to.
//   - Symlink refusal: a link at the target must not redirect machine state.
//   - A unique staging name: two writers must never share a temp path.
//   - A directory sync: a rename is atomic, but not durable until the directory
//     entry is flushed.
//
// The one deliberate difference from the draft is that the draft's
// symlink-preserving configuration writer is not reused here. That behaviour is
// right for a user config that may be symlinked into a dotfiles repository, and
// wrong for a request state file: following a link there would let something
// outside the store decide where the authority for a request lives.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrUnsafePath is returned when a write would leave the store or follow a
// symlink for a machine file.
var ErrUnsafePath = errors.New("agent-engine: refusing to write outside the store or through a symlink")

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DigestFile returns the SHA-256 of a file's contents.
func DigestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("agent-engine: reading %s to digest: %w", path, err)
	}
	return sha256Hex(data), nil
}

// writeDurable commits a file inside root atomically and durably.
func writeDurable(root, rel string, data []byte, perm os.FileMode) error {
	full := filepath.Join(root, filepath.Clean("/"+rel))
	if err := checkConfined(root, full); err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("agent-engine: creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(full)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("agent-engine: creating temp for %s: %w", rel, err)
	}
	tmpName := tmp.Name()
	// The rename is the commit point; anything before it is disposable.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent-engine: setting mode on %s: %w", rel, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent-engine: writing %s: %w", rel, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent-engine: syncing %s: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent-engine: closing %s: %w", rel, err)
	}
	// Re-checked immediately before the rename: the gap between the first check
	// and the rename is exactly where a symlink swap would land.
	if err := checkConfined(root, full); err != nil {
		return err
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("agent-engine: committing %s: %w", rel, err)
	}
	return syncDir(dir)
}

// writeJSONDurable encodes v as indented JSON and commits it durably.
func writeJSONDurable(root, rel string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("agent-engine: encoding %s: %w", rel, err)
	}
	return writeDurable(root, rel, append(data, '\n'), perm)
}

// checkConfined refuses a target that is a symlink or resolves outside root.
func checkConfined(root, full string) error {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(cleanRoot, filepath.Clean(full))
	if err != nil || rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("%w: %s is not inside %s", ErrUnsafePath, full, cleanRoot)
	}
	// Lstat, not Stat: Stat would follow the link and report the target, which
	// is exactly the case that has to be refused.
	if fi, lerr := os.Lstat(full); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnsafePath, full)
	}
	return nil
}

// syncDir flushes a directory entry so a completed rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("agent-engine: opening %s to sync: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("agent-engine: syncing %s: %w", dir, err)
	}
	return nil
}

// readJSONStrict decodes a machine file, refusing unknown fields.
//
// Unknown-field refusal is the difference between noticing a corrupted or
// hand-edited state file and silently accepting it with a field missing. A
// missing field is worse than a parse error here, because the file still
// decodes — it just no longer says what it claimed to say.
func readJSONStrict(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("agent-engine: %s is not readable as this schema, refusing to guess: %w", path, err)
	}
	return nil
}
