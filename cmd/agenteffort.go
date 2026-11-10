package cmd

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
)

const effortFlag = "--effort"

// startEffort resolves a start's effort: the flag, else the first matching
// agents.models rule; empty keeps the harness default.
func (a *app) startEffort(name, kind, level string) (string, error) {
	level = strings.TrimSpace(level)
	if level == "" && a.cfg != nil {
		level = a.cfg.Agents.EffortFor(name, kind)
	}
	if !config.ValidEffort(level) {
		return level, usageErr("invalid effort %q: low, medium, high, xhigh, max", level)
	}
	return level, nil
}

// sessionEffort is a session's effort as startAgent launches it: the start's,
// else its role's, else the first matching agents.models rule.
func (a *app) sessionEffort(sp agentStart) string {
	if sp.effort != "" {
		return sp.effort
	}
	if sp.role == guideName {
		if e := strings.TrimSpace(a.cfg.Guide.Effort); e != "" {
			return e
		}
	}
	return a.cfg.Agents.EffortFor(sp.name, cmp.Or(sp.kind, sp.role), "claude")
}

// refuseAgyEffort refuses an effort agy would silently ignore.
func refuseAgyEffort(level string) error {
	if level == "" {
		return nil
	}
	return usageErr("agy does not expose a session effort option; choose a model with the desired effort")
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

// selectedEffort resolves policy only after the harness is known. A retry on a
// different capacity choice must resolve afresh, never inherit the failed pick.
func (a *app) selectedEffort(name, kind, harness, model, explicit, choice string) (string, error) {
	level := strings.TrimSpace(explicit)
	if level == "" {
		level = a.cfg.Agents.EffortFor(name, kind, harness)
	}
	if level == "" {
		level = strings.TrimSpace(choice)
	}
	if !config.ValidEffort(level) {
		return "", usageErr("invalid effort %q", level)
	}
	if harness == harnessAgy {
		if err := refuseAgyEffort(level); err != nil {
			return "", err
		}
	}
	if harness == "codex" && level != "" {
		if err := codex.ValidateEffort(filepath.Join(filepath.Dir(a.cfg.Codex.ConfigFile), "models_cache.json"), model, level); err != nil {
			return "", usageErr("%v", err)
		}
	}
	return level, nil
}
