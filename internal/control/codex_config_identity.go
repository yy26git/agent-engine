package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// codexConfigFingerprint pins parsed configuration, including unknown settings.
// Only the documented startup-tooltip counters are omitted. They change on
// every launch and do not select models, permissions or tools. Other TUI fields
// (including notifications and screen-reader configuration) remain pinned.
func codexConfigFingerprint(data []byte) (string, error) {
	var config map[string]any
	if _, err := toml.Decode(string(data), &config); err != nil {
		return "", fmt.Errorf("invalid Codex TOML: %w", err)
	}
	if err := refuseCodexExternalConfig(config); err != nil {
		return "", err
	}
	if value, exists := config["tui"]; exists {
		tui, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("Codex tui configuration must be a table")
		}
		if value, exists := tui["model_availability_nux"]; exists {
			counters, ok := value.(map[string]any)
			if !ok {
				return "", fmt.Errorf("Codex startup-tooltip state must be a table")
			}
			for model, value := range counters {
				count, ok := value.(int64)
				if !ok || count < 0 || model == "" {
					return "", fmt.Errorf("invalid Codex startup-tooltip counter for %q", model)
				}
			}
			delete(tui, "model_availability_nux")
			if len(tui) == 0 {
				delete(config, "tui")
			}
		}
	}
	// The library sorts map keys and preserves TOML scalar types (in particular
	// integer versus float and string versus datetime); JSON alone would not.
	var canonical bytes.Buffer
	if err := toml.NewEncoder(&canonical).Encode(config); err != nil {
		return "", fmt.Errorf("cannot canonicalize Codex configuration: %w", err)
	}
	sum := sha256.Sum256(canonical.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func refuseCodexExternalConfig(config map[string]any) error {
	for key, value := range config {
		// Inspect parsed keys rather than lines: quoted/dotted keys and inline
		// tables cannot hide capabilities behind comments or escape sequences.
		name := strings.ToLower(key)
		if strings.Contains(name, "mcp_servers") || strings.Contains(name, "plugins") || strings.Contains(name, "hooks") || strings.HasPrefix(name, "notify") || strings.Contains(name, "execpolicy") {
			return fmt.Errorf("Codex qualification refuses external tool/hook configuration; those capabilities need separate confinement evidence")
		}
		switch value := value.(type) {
		case map[string]any:
			if err := refuseCodexExternalConfig(value); err != nil {
				return err
			}
		case []map[string]any:
			for _, item := range value {
				if err := refuseCodexExternalConfig(item); err != nil {
					return err
				}
			}
		case []any:
			if err := refuseCodexExternalArray(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func refuseCodexExternalArray(values []any) error {
	for _, value := range values {
		switch value := value.(type) {
		case map[string]any:
			if err := refuseCodexExternalConfig(value); err != nil {
				return err
			}
		case []any:
			if err := refuseCodexExternalArray(value); err != nil {
				return err
			}
		}
	}
	return nil
}
