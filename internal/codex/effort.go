package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ValidateEffort checks the installed model catalog rather than assuming all
// Codex models accept the same reasoning levels. It reads metadata only.
func ValidateEffort(cache, model, level string) error {
	if level == "" {
		return nil
	}
	raw, err := os.ReadFile(cache) //nolint:gosec // configured, metadata-only model catalog
	if err != nil {
		return fmt.Errorf("cannot validate Codex effort: refresh the Codex model catalog: %w", err)
	}
	var catalog struct {
		Models []struct {
			Slug   string `json:"slug"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return fmt.Errorf("decode Codex model catalog: %w", err)
	}
	for _, m := range catalog.Models {
		if m.Slug != model {
			continue
		}
		var levels []string
		for _, l := range m.Levels {
			if l.Effort == level {
				return nil
			}
			levels = append(levels, l.Effort)
		}
		return fmt.Errorf("codex model %s does not support effort %q (supported: %s)", model, level, strings.Join(levels, ", "))
	}
	return fmt.Errorf("codex model %s is absent from the model catalog; refresh it before setting effort", model)
}
