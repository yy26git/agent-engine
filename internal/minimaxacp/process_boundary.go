package minimaxacp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	stdRuntime "runtime"
	"syscall"
)

// ProcessBoundary freezes an OS policy around the entire stock process and its
// descendants. It is not itself a passing confinement receipt.
type ProcessBoundary struct {
	Policy       string
	PolicySHA256 string
	TempDir      string
	// WritableRoots are private, exclusively owned launch inputs. A path policy
	// cannot distinguish a pre-existing hard-link alias from an allowed file.
	WritableRoots []string
}

func boundaryCommand(opts Options) (*exec.Cmd, error) {
	if opts.Boundary == nil {
		return exec.Command(opts.Launcher, "acp"), nil
	}
	b := opts.Boundary
	if stdRuntime.GOOS != "darwin" {
		return nil, fmt.Errorf("minimax-acp: candidate process boundary requires macOS")
	}
	if b.Policy == "" || !filepath.IsAbs(b.TempDir) {
		return nil, fmt.Errorf("minimax-acp: candidate boundary requires a policy and absolute temporary directory")
	}
	sum := sha256.Sum256([]byte(b.Policy))
	if b.PolicySHA256 == "" || b.PolicySHA256 != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("minimax-acp: candidate process policy digest mismatch")
	}
	if len(b.WritableRoots) == 0 {
		return nil, fmt.Errorf("minimax-acp: candidate boundary requires explicit writable roots")
	}
	for _, root := range b.WritableRoots {
		if err := verifyUnlinkedTree(root); err != nil {
			return nil, err
		}
	}
	// Pass the frozen bytes, not a file that can change after verification.
	return exec.Command("/usr/bin/sandbox-exec", "-p", b.Policy, opts.Launcher, "acp"), nil
}

// Reject aliases before process creation; the OS policy must also deny creation
// of new hard links. This assumes exclusive ownership of writable roots during
// launch, and does not protect against a separate hostile host process racing it.
func verifyUnlinkedTree(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("minimax-acp: writable root must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("minimax-acp: cannot verify writable root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("minimax-acp: writable root is not a directory: %s", root)
	}
	count := 0
	return filepath.WalkDir(canonical, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 100000 {
			return fmt.Errorf("minimax-acp: writable tree exceeds verification bound")
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil // symlink targets remain subject to the OS path boundary
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("minimax-acp: unqualified writable special file: %s", path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 {
			return fmt.Errorf("minimax-acp: writable hard-link alias refused before spawn: %s", path)
		}
		return nil
	})
}
