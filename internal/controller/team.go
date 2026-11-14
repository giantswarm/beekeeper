package controller

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/history"
)

// reconcileTeam is a team's decision pass: decide.Team over every Item, then the Team's and its
// People's status and each Item's status.shadow (the only writer of shadows), the shadow rows (each
// would-action once per key) and the decision questions.
func (rt *Runtime) reconcileTeam(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	rt.mu.Lock()
	source := rt.teamSource[req.Name]
	delete(rt.teamSource, req.Name)
	starting := rt.obs.starting || len(rt.obs.startupQueue) > 0
	rt.mu.Unlock()
	if starting {
		// Start-up isn't done deciding Items: the pass runs once it is.
		rt.teamPass(req.Name, orDefault(source, v1alpha1.SourceEngine))
		return reconcile.Result{}, nil
	}
	if rt.cfg.Faults.StopDecisions.Load() {
		return reconcile.Result{}, nil
	}
	team := &v1alpha1.Team{}
	if err := rt.store.get(ctx, KindTeam, req.Name, team); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	inst := rt.installation(ctx)
	if inst == nil {
		return reconcile.Result{}, nil
	}
	allPeople, err := rt.store.people(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	var people []*v1alpha1.Person
	for _, p := range allPeople {
		if containsStr(p.Spec.Teams, team.Name) {
			people = append(people, p)
		}
	}
	sort.Slice(people, func(i, j int) bool { return login(people[i]) < login(people[j]) })
	items, err := rt.store.items(ctx, nil)
	if err != nil {
		return reconcile.Result{}, err
	}
	now := rt.clock.Now()
	dec := rt.cfg.DecideTeam(decide.TeamView{Now: now, Spec: &inst.Spec, Team: team, People: people, Items: items,
		GateEverything: rt.cfg.Faults.GateEverything.Load(), Source: orDefault(source, v1alpha1.SourceEngine)})

	// Shadows first, so the Team's and People's picks never point at an Item that doesn't show its pick.
	byRef := map[string]*v1alpha1.Item{}
	for _, it := range items {
		byRef[itemRef(it)] = it
	}
	for _, ref := range sortedKeys(dec.Shadows) {
		it, ok := byRef[ref]
		if !ok {
			continue
		}
		sh := dec.Shadows[ref]
		if equality.Semantic.DeepEqual(it.Status.Shadow, sh) {
			continue
		}
		mod := it.DeepCopy()
		mod.Status.Shadow = sh
		if err := rt.store.patchStatus(ctx, KindItem, mod, it); err != nil {
			return reconcile.Result{}, err
		}
	}
	for _, p := range people {
		ps, ok := dec.People[login(p)]
		if !ok || equality.Semantic.DeepEqual(p.Status, ps) {
			continue
		}
		mod := p.DeepCopy()
		mod.Status = ps
		if err := rt.store.patchStatus(ctx, KindPerson, mod, p); err != nil {
			return reconcile.Result{}, err
		}
	}
	// The Team shows when it was last decided (leader-decides is read from it and the Installation).
	ts := dec.Team
	if !equality.Semantic.DeepEqual(team.Status, ts) {
		mod := team.DeepCopy()
		mod.Status = ts
		if err := rt.store.patchStatus(ctx, KindTeam, mod, team); err != nil {
			return reconcile.Result{}, err
		}
	}
	rt.mu.Lock()
	rt.lastDecide = now
	rt.mu.Unlock()
	rt.m.LastDecisionTimestamp.Set(float64(now.UnixNano()) / 1e9)
	rt.instQ.Add(instKey)
	rt.append(rt.onceActions(dec.Events)...)
	rt.ask(&inst.Spec, dec.Questions)
	return reconcile.Result{}, nil
}

func login(p *v1alpha1.Person) string {
	if p.Spec.Login != "" {
		return p.Spec.Login
	}
	return p.Name
}

// onceActions drops shadow.action rows whose key was recorded before (an action that disappears and
// reappears is the same action), keeping the set in memory; it is rebuilt from history at leadership.
func (rt *Runtime) onceActions(evs []history.Event) []history.Event {
	var out []history.Event
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, e := range evs {
		if e.Type == decide.EventShadowAction {
			k, _ := e.Data["key"].(string)
			if k != "" {
				if rt.actions[k] {
					continue
				}
				rt.actions[k] = true
			}
		}
		out = append(out, e)
	}
	return out
}
