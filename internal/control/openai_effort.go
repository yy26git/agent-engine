package control

import (
	"fmt"
	"strings"
)

// Codex capability data, verified 2026-10-13 against the installed bundled
// catalogue. API effort and Codex Ultra (delegation) are distinct surfaces.
// Unknown model ids remain selectable; their CLI is the final authority.
func validateCodexEffort(p Profile) error {
	if p.Tool != "codex" || p.Effort == "" || (p.Provider != "" && p.Provider != "native" && p.Provider != "openai") {
		return nil
	}
	valid := map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultra": true}
	if !valid[p.Effort] {
		return fmt.Errorf("profile %q: invalid Codex effort %q; use low, medium, high, xhigh (Extra High), max, or a level advertised by the selected CLI model", p.Name, p.Effort)
	}
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.6", "gpt-5.5"} {
		if p.Model != model && !strings.HasPrefix(p.Model, model+"-20") {
			continue
		}
		unsupported := p.Effort == "none" || p.Effort == "minimal" ||
			(model == "gpt-5.5" && (p.Effort == "max" || p.Effort == "ultra")) ||
			((model == "gpt-6-luna" || model == "gpt-5.6-luna") && p.Effort == "ultra")
		if unsupported {
			return fmt.Errorf("profile %q: Codex model %q does not advertise effort %q; no default was substituted", p.Name, p.Model, p.Effort)
		}
		break
	}
	return nil
}
