package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
)

const TaskKindReview = "review"

const ModelPolicyPrefix = "model policy:"

// SelfHostedWorker is the reason a self-hosted choice without kinds
// takes no worker start.
const SelfHostedWorker = ModelPolicyPrefix + " a self-hosted choice takes task kind " + workerRole + " only when its kinds list it"

// ChoiceRule is how a start's choice is made, for every start alike.
const ChoiceRule = "eligible subscription below half its 7-day pace first, then furthest behind pace, then most 7-day budget left per hour until its reset, lower configured cost and configured order breaking ties; a self-hosted choice only when no subscription is eligible"

// ModelPolicyReason is shared by capacity, its pick, and a start's gate.
// Window stops and pace remain the capacity evaluator's responsibility.
func ModelPolicyReason(k config.Capacity, ch config.Choice, pv *ProviderView, bucket string, kind StartFor) string {
	task := strings.TrimSpace(k.TaskKind)
	if task == "" {
		task = workerRole
		if kind != ForWorker {
			task = TaskKindReview
		}
	}
	if len(ch.Kinds) > 0 && !slices.Contains(ch.Kinds, task) {
		return "model policy: task kind " + task + " is not permitted"
	}
	// A self-hosted model writes code only where the desk says so: without
	// kinds, it takes every kind but worker.
	if len(ch.Kinds) == 0 && task == workerRole && k.Providers[ch.EffectiveProvider()].SelfHosted() {
		return SelfHostedWorker
	}
	p := k.TaskPolicies[task]
	review := kind != ForWorker || task == TaskKindReview
	if ch.Class < p.MinClass {
		return fmt.Sprintf("model policy: class %d below %s minimum %d", ch.Class, task, p.MinClass)
	}
	if p.MaxCost > 0 && (ch.Cost == 0 || ch.Cost > p.MaxCost) {
		return "model policy: cost unknown or over the task's maximum"
	}
	if review && (k.AuthorModel != "" || p.DifferentFamily || k.TaskPolicies[TaskKindReview].DifferentFamily) {
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
			if b := pv.FindBucket(bucket); b != nil {
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
