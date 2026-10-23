package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/emjin/agent-engine/internal/control"
)

// Agent Deck resolves Codex transcripts in its own process environment. The
// wrapper's environment selects the launched Codex home, but cannot select the
// home used by a later queue worker or transcript reader.
func requireCodexRuntimeHome(profiles []control.ProfileRef) error {
	for _, ref := range profiles {
		p := ref.Profile()
		if p.Tool != "codex" || p.TransportFor() != control.TransportAgentDeck {
			continue
		}
		explicit := p.LaunchEnv["CODEX_HOME"]
		if explicit == "" {
			explicit = os.Getenv("CODEX_HOME")
		}
		expected, err := codexHome(explicit, p.LaunchEnv["HOME"])
		if err != nil {
			return fmt.Errorf("Codex profile %q home: %w", p.Name, err)
		}
		actual, err := codexHome(os.Getenv("CODEX_HOME"), os.Getenv("HOME"))
		if err != nil {
			return fmt.Errorf("Agent Deck Codex home: %w", err)
		}
		if actual != expected {
			return fmt.Errorf("Codex profile %q launches in %q, but Agent Deck resolves transcripts in %q; set CODEX_HOME=%s when launching Agent Engine (profile launch_env alone does not scope queued delivery or transcript lookup)", p.Name, expected, actual, expected)
		}
	}
	return nil
}

func codexHome(explicit, userHome string) (string, error) {
	if explicit == "" {
		if userHome == "" {
			var err error
			userHome, err = os.UserHomeDir()
			if err != nil {
				return "", err
			}
		}
		explicit = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(explicit) {
		return "", fmt.Errorf("CODEX_HOME must be absolute")
	}
	return filepath.EvalSymlinks(explicit)
}
