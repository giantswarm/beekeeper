package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// reconcileBudget shows the GitHub App's budget as the rate-limit headers last reported it, on the
// Budget object github-app, and sets engine_github_rate_remaining. It writes only when the REST
// window, a limit, a reset time or a deferral changed: the points and App calls an idle tick spends
// make no write.
func (rt *Runtime) reconcileBudget(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	b := rt.forge.Budget()
	now := rt.clock.Now()
	for api, l := range map[string]forge.APILimit{forge.APIREST: b.REST, forge.APIGraphQL: b.GraphQL, forge.APIApp: b.App} {
		if l.Known || l.Limit > 0 {
			rt.m.SetRateRemaining(api, l.Remaining)
		}
	}
	obj := &v1alpha1.Budget{}
	err := rt.store.get(ctx, KindBudget, v1alpha1.BudgetName, obj)
	if apierrors.IsNotFound(err) {
		obj = &v1alpha1.Budget{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.BudgetName}}
		if err := rt.store.create(ctx, KindBudget, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return reconcile.Result{}, err
		}
	} else if err != nil {
		return reconcile.Result{}, err
	}
	st := v1alpha1.BudgetStatus{
		REST: apiBudget(b.REST), GraphQL: apiBudget(b.GraphQL), App: apiBudget(b.App),
		Deferred: v1alpha1.Deferred{REST: paused(b.REST, now), GraphQL: paused(b.GraphQL, now), App: paused(b.App, now)},
	}
	rt.mu.Lock()
	rt.lastBudget, rt.budgetWritten = budgetKey(b, now), true
	rt.mu.Unlock()
	if equality.Semantic.DeepEqual(obj.Status, st) {
		return reconcile.Result{}, nil
	}
	mod := obj.DeepCopy()
	mod.Status = st
	return reconcile.Result{}, rt.store.patchStatus(ctx, KindBudget, mod, obj)
}

func apiBudget(l forge.APILimit) v1alpha1.APIBudget {
	out := v1alpha1.APIBudget{Limit: l.Limit, Remaining: l.Remaining, Used: l.Used}
	if !l.ResetAt.IsZero() {
		t := metav1.NewTime(l.ResetAt.UTC().Truncate(time.Second))
		out.ResetAt = &t
	}
	return out
}

// paused reports whether observation reads on an API are deferred now.
func paused(l forge.APILimit, now time.Time) bool {
	return l.Paused && (l.PausedUntil.IsZero() || l.PausedUntil.After(now))
}
