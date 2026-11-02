package cmd

import (
	"cmp"
	"fmt"

	"github.com/giantswarm/beekeeper/internal/config"
)

const effortFlag = "--effort"

func (a *app) startSelection(kind, model, level string) (config.AgentModel, error) {
	entry := a.cfg.Agents.Models[kind]
	entry.Model = cmp.Or(model, entry.Model)
	entry.Effort = cmp.Or(level, entry.Effort)
	if !config.ValidEffort(entry.Effort) {
		return entry, usageErr("invalid effort %q: low, medium, high, xhigh, max", entry.Effort)
	}
	return entry, nil
}

func effortFlags(level string) []string {
	if level == "" {
		return nil
	}
	return []string{effortFlag, level}
}

func effortText(level string) string {
	if level == "" {
		return ""
	}
	return fmt.Sprintf(", effort %s", level)
}

// startTurnLine describes a headless task even when only effort required it.
func startTurnLine(profile, level string) string {
	if profile == "" {
		return "its task runs as a headless turn" + effortText(level)
	}
	return profiledTurn(profile) + effortText(level)
}
