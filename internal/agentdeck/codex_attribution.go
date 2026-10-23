package agentdeck

import (
	"fmt"
)

// codexTimelineModel joins two public CLI documents. Session-level model
// summaries and model-authored text cannot supply attribution. The native turn
// context must belong to the output's completed turn and its final bytes must
// match. Recall v2 omits the initial model context; the documented v1 format
// preserves the native records needed for this join.
func codexTimelineModel(document, generation, answer string) (string, error) {
	proof, err := codexTimelineTurn(document, generation, answer)
	if err != nil {
		return "", err
	}
	if !proof.ModelValid || proof.Model == "" {
		return "", fmt.Errorf("agentdeck: exact Codex turn attribution requires consistent model context within the completed turn")
	}
	return proof.Model, nil
}
