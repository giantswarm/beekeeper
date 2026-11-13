package cmd

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	effortFlag = "--effort"
	// defaultEffortFlag carries the loop's supervisor.effort to the starts
	// it runs: the last step of selectedEffort's order.
	defaultEffortFlag = "--default-effort"
)

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

// continuedEffort is the effort the session a start named name continues
// was started with: the parked worker whose fresh continuation the start is
// (continuationName), or the roster's agent of that name it resumes or
// replaces. Kept only on that session's harness, whose levels it was given
// for; "" for any other start.
func continuedEffort(st *state.State, name, harness string) string {
	h := cmp.Or(strings.TrimSpace(harness), claudeComm)
	for _, ag := range st.Agents {
		if !strings.EqualFold(ag.Name, name) && (ag.Park == nil || !ag.Park.Continuing || continuationName(ag) != name) {
			continue
		}
		for _, s := range slices.Backward(slices.Concat(st.EndedWorkers, st.Starts)) {
			if s.Is(ag.Party) && strings.EqualFold(cmp.Or(s.Harness, claudeComm), h) {
				return strings.TrimSpace(s.Effort)
			}
		}
	}
	return ""
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
// The effort is the first of: --effort; the first matching agents.models
// rule; the effort of the choice that picked the harness and model, or of
// the choice of the harness and model the start names; the effort of the
// session the start continues (continuedEffort); --default-effort, the
// loop's supervisor.effort for the sessions it starts. None: the harness
// default. agy takes no effort, so the last two never reach it.
func (a *app) selectedEffort(name, kind, harness, model, explicit, choice string) (string, error) {
	level := strings.TrimSpace(explicit)
	if level == "" {
		level = a.cfg.Agents.EffortFor(name, kind, harness)
	}
	if level == "" {
		level = cmp.Or(strings.TrimSpace(choice), capacity.ChoiceEffort(a.liveCapacity().Choices, harness, a.effectiveStartModel(harness, model)))
	}
	if level == "" && harness != capacity.HarnessAgy {
		st, err := a.store.Read()
		if err != nil {
			return "", err
		}
		level = continuedEffort(st, name, harness)
	}
	if level == "" && harness != capacity.HarnessAgy {
		level = strings.TrimSpace(a.startDefaultEffort)
	}
	if !config.ValidEffort(level) {
		return "", usageErr("invalid effort %q", level)
	}
	if harness == capacity.HarnessAgy {
		if err := refuseAgyEffort(level); err != nil {
			return "", err
		}
	}
	if harness == codex.Harness && level != "" {
		if err := codex.ValidateEffort(filepath.Join(filepath.Dir(a.cfg.Codex.ConfigFile), "models_cache.json"), model, level); err != nil {
			return "", usageErr("%v", err)
		}
	}
	return level, nil
}
