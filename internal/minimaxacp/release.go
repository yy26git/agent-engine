package minimaxacp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MeasureRelease covers the release payload and bundled Node, not just its shim.
// It executes nothing and never reads provider credentials or runtime state.
func MeasureRelease(launcher string) (map[string]string, error) {
	artifacts := map[string]string{}
	measure := func(path string) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		h := sha256.New()
		if _, err := io.Copy(h, file); err != nil {
			return err
		}
		artifacts[path] = hex.EncodeToString(h.Sum(nil))
		return nil
	}
	if err := measure(launcher); err != nil {
		return nil, err
	}
	root := filepath.Dir(launcher)
	entry := filepath.Join(root, "lib", "node_modules", "@minimax-ai", "code", "cli.js")
	if _, err := os.Stat(entry); err != nil {
		return artifacts, fmt.Errorf("release entry is unavailable at %s: %w", entry, err)
	}
	payload := filepath.Dir(entry)
	if err := filepath.WalkDir(payload, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				return fmt.Errorf("release symlink is absolute: %s", path)
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			canonicalPayload, err := filepath.EvalSymlinks(payload)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(canonicalPayload, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("release symlink escapes payload: %s", path)
			}
			targetInfo, err := os.Stat(target)
			if err != nil {
				return err
			}
			if !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("release symlink target is not a regular file: %s", path)
			}
			if len(artifacts) >= 8192 {
				return fmt.Errorf("release manifest exceeds its artifact bound")
			}
			sum := sha256.Sum256([]byte("symlink\x00" + link))
			artifacts[path] = hex.EncodeToString(sum[:])
			// WalkDir also measures the regular target. Pin the link text as
			// well, so repointing it to an identical file still changes identity.
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release contains an unmeasured non-regular artifact: %s", path)
		}
		if len(artifacts) >= 8192 {
			return fmt.Errorf("release manifest exceeds its artifact bound")
		}
		return measure(path)
	}); err != nil {
		return artifacts, err
	}
	body, err := os.ReadFile(launcher)
	if err != nil {
		return artifacts, err
	}
	match := regexp.MustCompile(`['"](/[^'"\n]+/bin/node)['"]`).FindSubmatch(body)
	if len(match) != 2 {
		return artifacts, fmt.Errorf("launcher does not identify its bundled Node")
	}
	if err := measure(string(match[1])); err != nil {
		return artifacts, err
	}
	return artifacts, nil
}

// VerifyReleaseReceipt compares the full payload to an operator-retained receipt.
// Measuring today's files alone is observation, not approval of those files.
func VerifyReleaseReceipt(launcher, receiptPath string) error {
	body, err := os.ReadFile(receiptPath)
	if err != nil {
		return fmt.Errorf("operator release receipt is required: %w", err)
	}
	var receipt struct {
		Artifacts map[string]string `json:"release_artifacts"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		return err
	}
	actual, err := MeasureRelease(launcher)
	if err != nil {
		return err
	}
	if len(actual) != len(receipt.Artifacts) {
		return fmt.Errorf("release artifact set differs from the operator receipt")
	}
	for path, digest := range actual {
		if receipt.Artifacts[path] != digest {
			return fmt.Errorf("release artifact differs from the operator receipt: %s", path)
		}
	}
	return nil
}
