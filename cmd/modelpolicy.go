package cmd

import (
	"strings"
)

const (
	authorModelFlag     = "author-model"
	harnessDefaultModel = "harness default"
	authorNeedsReview   = "--author-model requires a review"
	taskKindArg         = "--kind"
)

func (a *app) ruleModel(name, kind string) string {
	if a.cfg == nil {
		return ""
	}
	rules := a.cfg.Agents
	rules.Model = "" // a harness-specific fallback is resolved after choosing the harness
	return rules.ModelFor(name, kind)
}

func (a *app) requireStartModel(name, model string) error {
	if a.cfg.Agents.RequireModel && strings.TrimSpace(model) == "" {
		return refused("%s: agents.requireModel is true, but no model resolved: set --model, agents.models, or the harness model in the configuration", name)
	}
	return nil
}

func startDecision(v *View) string {
	if v == nil || v.Choice == nil {
		return ""
	}
	if v.Choice.Harness == "" {
		return ""
	}
	return v.Choice.Line() + "; " + v.Choice.Decision
}
