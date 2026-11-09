package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
)

const (
	taskKindReview      = "review"
	authorModelFlag     = "author-model"
	modelPolicyPrefix   = "model policy:"
	harnessDefaultModel = "harness default"
	authorNeedsReview   = "--author-model requires a review"
	taskKindArg         = "--kind"
	// selfHostedWorker is the reason a self-hosted choice without kinds
	// takes no worker start.
	selfHostedWorker = modelPolicyPrefix + " a self-hosted choice takes task kind " + workerRole + " only when its kinds list it"
	// choiceRule is how a start's choice is made, for every start alike.
	choiceRule = "eligible subscription furthest behind its 7-day pace, then most 7-day budget left per hour until its reset, lower configured cost and configured order breaking ties; a self-hosted choice only when no subscription is eligible"
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

// modelPolicyReason is shared by capacity, its pick, and a start's gate.
// Window stops and pace remain the capacity evaluator's responsibility.
func modelPolicyReason(k config.Capacity, ch config.Choice, pv *providerView, bucket string, kind startFor) string {
	task := strings.TrimSpace(k.TaskKind)
	if task == "" {
		task = workerRole
		if kind != forWorker {
			task = taskKindReview
		}
	}
	if len(ch.Kinds) > 0 && !slices.Contains(ch.Kinds, task) {
		return "model policy: task kind " + task + " is not permitted"
	}
	// A self-hosted model writes code only where the desk says so: without
	// kinds, it takes every kind but worker.
	if len(ch.Kinds) == 0 && task == workerRole && k.Providers[ch.EffectiveProvider()].SelfHosted() {
		return selfHostedWorker
	}
	p := k.TaskPolicies[task]
	review := kind != forWorker || task == taskKindReview
	if ch.Class < p.MinClass {
		return fmt.Sprintf("model policy: class %d below %s minimum %d", ch.Class, task, p.MinClass)
	}
	if p.MaxCost > 0 && (ch.Cost == 0 || ch.Cost > p.MaxCost) {
		return "model policy: cost unknown or over the task's maximum"
	}
	if review && (k.AuthorModel != "" || p.DifferentFamily || k.TaskPolicies[taskKindReview].DifferentFamily) {
		family := strings.TrimSpace(k.AuthorFamily)
		for _, author := range k.Choices {
			if choiceModelMatches(author, k.AuthorModel) {
				family = strings.TrimSpace(author.Family)
				break
			}
		}
		if family == "" {
			return "model policy: author-model has no configured family"
		}
		candidateFamily := strings.TrimSpace(ch.Family)
		if candidateFamily == "" || strings.EqualFold(candidateFamily, family) {
			return "model policy: review needs a different known model family"
		}
	}
	if !review && ch.ReviewReserve > 0 && pv != nil {
		sd := pv.SevenDay
		if bucket != "" {
			if b := pv.findBucket(bucket); b != nil {
				sd = b.SevenDay
			}
		}
		if sd == nil {
			return "model policy: weekly usage unknown for review reserve"
		}
		if sd.UsedPercent >= 100-ch.ReviewReserve {
			return "model policy: weekly window reserved for reviews"
		}
	}
	return ""
}

func startDecision(v *capacityView) string {
	if v == nil || v.Choice == nil {
		return ""
	}
	if v.Choice.Harness == "" {
		return ""
	}
	return v.Choice.line() + "; " + v.Choice.Decision
}
