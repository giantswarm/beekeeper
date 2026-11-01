package cmd

import (
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/guard"
)

// projectPrompt is the portable fallback for non-Claude harnesses. Keep the
// composed rules in the first message, never in the shared worker home where
// concurrent starts in different projects could replace each other's rules.
// Real starts and dry runs use the same composition and byte cap.
func projectPrompt(dir, prompt string) string {
	if instructions := guard.ProjectInstructions(dir, codex.DefaultProjectDocMaxBytes); instructions != "" {
		return instructions + "\n\n" + prompt
	}
	return prompt
}
