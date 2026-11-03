package cmd

import (
	"cmp"
	"fmt"
	"strings"

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
	return a.cfg.Agents.EffortFor(sp.name, cmp.Or(sp.kind, sp.role))
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
