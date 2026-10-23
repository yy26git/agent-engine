package contract

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxImportedPlanBytes bounds the source document, independently of the
// deployment's retained evidence and prompt budgets. Nothing is truncated.
const MaxImportedPlanBytes = 1 << 20

// ImportedPlanDocument is caller-supplied plan data, never an execution grant.
// Content preserves the complete UTF-8 source; SHA256 hashes its exact bytes.
type ImportedPlanDocument struct {
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

func ValidateImportedPlanDocument(p *ImportedPlanDocument) error {
	if p == nil {
		return nil
	}
	if len(p.Content) > MaxImportedPlanBytes {
		return fmt.Errorf("%w: imported plan exceeds %d bytes; no content was truncated", ErrEpisodeInvalid, MaxImportedPlanBytes)
	}
	if strings.TrimSpace(p.Content) == "" || !utf8.ValidString(p.Content) || strings.ContainsRune(p.Content, 0) {
		return fmt.Errorf("%w: imported plan must be non-empty UTF-8 text without NUL bytes", ErrEpisodeInvalid)
	}
	if p.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(p.Content))) {
		return fmt.Errorf("%w: imported plan sha256 does not match its exact content", ErrEpisodeInvalid)
	}
	return nil
}
