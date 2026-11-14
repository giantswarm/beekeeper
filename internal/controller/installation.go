package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/observe"
)

// reconcileInstallation writes the Installation's status from the leader's observation state (the
// sources, the leader, BoardFilterValid) when it changed, and acts on configuration changes (of the
// Installation, a Team or a Person): the client's configuration is applied, the board filter is checked
// again, the branch rules are read again, refused reads are retried and every Item is decided again.
// The start-up sequence runs once an Installation exists.
func (rt *Runtime) reconcileInstallation(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	inst := rt.installation(ctx)
	if inst == nil {
		return reconcile.Result{}, nil
	}
	rt.applySpec(inst)
	if teams, err := rt.store.teams(ctx); err == nil {
		rt.sawTeams(len(teams) > 0)
	}
	rt.mu.Lock()
	ran := rt.obs.startupRan
	rt.mu.Unlock()
	if !ran {
		rt.taskQ.Add(taskStartup)
	}

	// Configuration changes.
	version := rt.lastConfigVersion(ctx, inst)
	rt.mu.Lock()
	prev := rt.lastConfig
	rt.lastConfig = version
	rt.mu.Unlock()
	if ran && prev != "" && prev != version {
		rt.rules.Forget()
		rt.runTaskNow(taskFilter, v1alpha1.SourceEngine)
		rt.retryRefused(ctx, v1alpha1.SourceEngine)
		if items, err := rt.store.items(ctx, nil); err == nil {
			for _, it := range items {
				rt.addPending(KindItem, it.Name, observe.Pending{Trigger: decide.TriggerEngine, Source: v1alpha1.SourceEngine})
			}
		}
		if teams, err := rt.store.teams(ctx); err == nil {
			for _, t := range teams {
				rt.teamPass(t.Name, v1alpha1.SourceEngine)
			}
		}
	}

	st := rt.installationStatus(inst)
	if equality.Semantic.DeepEqual(inst.Status, st) {
		return reconcile.Result{}, nil
	}
	// Written anyway: the sources carry their latest verdicts (an idle check alone writes nothing).
	rt.stampVerdicts(&st)
	mod := inst.DeepCopy()
	mod.Status = st
	return reconcile.Result{}, rt.store.patchStatus(ctx, KindInstallation, mod, inst)
}

// stampVerdicts sets each source's checkedAt to when its freshness was last judged.
func (rt *Runtime) stampVerdicts(st *v1alpha1.InstallationStatus) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.obs.restored {
		return
	}
	for _, src := range []*observe.Source{rt.obs.webhooks, rt.obs.board} {
		s, ok := st.Sources[src.Name]
		if !ok || src.Verdict.IsZero() {
			continue
		}
		t := metav1.NewTime(src.Verdict.UTC().Truncate(time.Second))
		s.CheckedAt = &t
		st.Sources[src.Name] = s
	}
}

// applySpec hands the Installation's spec to Config.OnInstallation when the leader first sees it and
// when its generation changes.
func (rt *Runtime) applySpec(inst *v1alpha1.Installation) {
	if rt.cfg.OnInstallation == nil {
		return
	}
	key := string(inst.UID) + "/" + strconv.FormatInt(inst.Generation, 10)
	rt.mu.Lock()
	same := rt.appliedSpec == key
	rt.appliedSpec = key
	rt.mu.Unlock()
	if !same {
		rt.cfg.OnInstallation(inst.Spec.DeepCopy())
	}
}

// sawTeams notes, the first time there is an Installation and a Team, that decision passes are due
// from now on (leader-decides).
func (rt *Runtime) sawTeams(any bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !any {
		rt.decidable = time.Time{}
		return
	}
	if rt.decidable.IsZero() {
		rt.decidable = rt.clock.Now()
	}
}

// installationStatus is the Installation's status as the leader sees it now.
func (rt *Runtime) installationStatus(inst *v1alpha1.Installation) v1alpha1.InstallationStatus {
	st := *inst.Status.DeepCopy()
	now := rt.clock.Now()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st.Leader = rt.cfg.Replica
	st.ObservedGeneration = inst.Generation
	if rt.obs.restored {
		if st.Sources == nil {
			st.Sources = map[string]v1alpha1.SourceStatus{}
		}
		for _, src := range []*observe.Source{rt.obs.webhooks, rt.obs.board} {
			s := src.Status()
			if old, ok := inst.Status.Sources[src.Name]; ok {
				s.CheckedAt = old.CheckedAt
			}
			st.Sources[src.Name] = s
		}
	}
	if !rt.lastDecide.IsZero() {
		t := metav1.NewTime(rt.lastDecide)
		st.LastDecisionAt = &t
	}
	if rt.obs.filterChecked {
		cond := metav1.Condition{Type: v1alpha1.ConditionBoardFilterValid, Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonBoardFilterValid, Message: "every team's board option is an option of the board's Team field",
			ObservedGeneration: inst.Generation, LastTransitionTime: metav1.NewTime(now)}
		if len(rt.obs.unknown) > 0 {
			cond.Status, cond.Reason = metav1.ConditionFalse, v1alpha1.ReasonUnknownTeamOption
			cond.Message = "unknown Team option(s) " + quoteAll(rt.obs.unknown) + "; their board queries don't run"
		}
		if old := meta.FindStatusCondition(st.Conditions, cond.Type); old != nil && old.Status == cond.Status {
			cond.LastTransitionTime = old.LastTransitionTime
		}
		meta.SetStatusCondition(&st.Conditions, cond)
	}
	return st
}

func quoteAll(s []string) string {
	q := make([]string, len(s))
	for i, v := range s {
		q[i] = `"` + v + `"`
	}
	return strings.Join(q, ", ")
}
