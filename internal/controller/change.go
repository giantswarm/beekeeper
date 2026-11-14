package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/observe"
	"github.com/giantswarm/beekeeper/internal/wake"
)

// reconcileChange observes a Change with conditional REST reads (its pending reads and a due wait's
// re-observe), decides it, writes its status once when anything changed, and runs the effects: history
// rows, durations, its next check and the Items whose phase depends on it. A Change no Item lists goes.
func (rt *Runtime) reconcileChange(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	key := KindChange + "/" + req.Name
	rt.changeBusy(key, true)
	defer rt.changeBusy(key, false)
	defer rt.startupProgress(key)
	ch := &v1alpha1.Change{}
	if err := rt.store.get(ctx, KindChange, req.Name, ch); err != nil {
		if apierrors.IsNotFound(err) {
			rt.cancelObject(KindChange, req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	inst := rt.installation(ctx)
	if inst == nil {
		return reconcile.Result{}, nil
	}
	ref := changeRef(ch)
	items, err := rt.store.items(ctx, func(it *v1alpha1.Item) bool { return containsStr(it.Status.Changes, ref) },
		client.MatchingFields{wake.IndexChanges: ref})
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(items) == 0 {
		// No tracked Item lists the PR any more: the Change goes, and its wait with it.
		if err := rt.store.delete(ctx, KindChange, ch); err != nil {
			return reconcile.Result{}, err
		}
		rt.cancelObject(KindChange, req.Name)
		return reconcile.Result{}, nil
	}
	var itemRefs []string
	team := ch.Labels[v1alpha1.LabelTeam]
	for _, it := range items {
		itemRefs = append(itemRefs, itemRef(it))
		if t := it.Labels[v1alpha1.LabelTeam]; t != "" {
			team = t
		}
	}

	now := rt.clock.Now()
	p := rt.pending.Take(key)
	defer rt.tookPending(key, p)
	if p != nil && !p.Empty() && rt.retrying(key, now) {
		rt.pending.Add(key, *p)
		p = nil
	}
	trigger, source := decide.TriggerEngine, v1alpha1.SourceEngine
	var reads observe.ReadSet
	var events []string
	if p != nil {
		trigger, source, events = p.Trigger, p.Source, p.Events
		reads = p.Reads
	}
	if rt.retryDue(key, now) {
		trigger = decide.TriggerRetry
	}
	if dueT, due := rt.dueTrigger(ch.Status.Wait, now); due {
		trigger = dueT
		reads.Add(observe.WaitReads(ch.Status.Wait))
	}
	rowSource := observationSource(trigger, source)

	view := ch.DeepCopy()
	if view.Status.PR == nil {
		if r, err := v1alpha1.ParseRef(ref); err == nil {
			view.Status.PR = &v1alpha1.ChangePR{Repo: r.FullRepo(), Number: r.Number}
		}
	}
	view.Status.Items = itemRefs
	old := view.Status.DeepCopy()
	subj := observe.Subject{Team: team, Change: ref}
	var rows []history.Event
	var re *decide.Reobserve
	firstRead := view.Status.PR == nil || view.Status.PR.State == ""
	if firstRead && view.Status.Refusal == nil {
		reads = observe.FullReads()
	}

	if !reads.Empty() && !rt.retrying(key, now) {
		repo := refRepo(ref)
		inputs := rt.inputs.Key(repo, ref)
		if r := view.Status.Refusal; r != nil && r.Inputs == inputs && !rt.cfg.Faults.ForgetRefusals.Load() {
			// Refused on these inputs: not read again until they change.
		} else {
			res := observe.ReadChange(readCtx(ctx), rt.forge, rt.rules, ref, &view.Status, reads)
			switch {
			case res.Err == nil:
				rt.succeeded(key)
				if r := view.Status.Refusal; r != nil && containsStr(res.Done, r.Operation) {
					view.Status.Refusal = nil
				}
			case res.Err.Kind == forge.Refused:
				rows = append(rows, rt.refusal(&view.Status.Refusal, res.Err, inputs, now, subj, ref)...)
			default:
				keep := p
				if keep == nil {
					keep = &observe.Pending{Trigger: trigger, Source: rowSource}
				}
				keep.Reads.Add(reads)
				rt.retry(KindChange, req.Name, keep, res.Err, &inst.Spec)
			}
			obs := observe.DiffChange(old, &view.Status)
			if len(obs) > 0 {
				view.Status.Observed = &v1alpha1.Observed{At: metav1.NewTime(now), Source: rowSource}
			}
			rows = append(rows, observe.Record(startupObservation(obs, observe.KindPR, rowSource, res.Err == nil),
				subj, rowSource, events, now, rt.inputs.Installed).Events...)
			if res.Made || res.Err != nil {
				re = &decide.Reobserve{Made: res.Made && res.Err == nil, Changed: len(obs) > 0}
				if res.Err != nil && res.Err.Kind != forge.Refused {
					re.Err = res.Err
				}
			}
		}
	}

	if rt.cfg.Faults.StopTimers.Load() {
		// stop_timers: only a wait's timer would handle its deadline, and its timers don't fire, so the
		// reads of other triggers don't count as its re-observe.
		re = nil
	}
	dec := rt.cfg.DecideChange(decide.ChangeView{
		Now: now, Spec: &inst.Spec, Change: view, Items: items, CIDurations: rt.ciDurationsOf(ctx, refRepo(ref)),
		Trigger: trigger, Source: source, Reobserve: re, DropWaits: rt.cfg.Faults.DropWaits.Load(),
	})
	rows = append(rows, dec.Events...)
	st := dec.Status
	if !equality.Semantic.DeepEqual(ch.Status, st) {
		mod := ch.DeepCopy()
		mod.Status = st
		if err := rt.store.patchStatus(ctx, KindChange, mod, ch); err != nil {
			if p != nil {
				rt.pending.Add(key, *p)
			}
			return reconcile.Result{}, err
		}
	}
	rt.append(rows...)
	rt.countWakes(dec.Events, trigger)
	for _, d := range dec.Durations {
		rt.addDuration(d.Kind, d.Key, d.D, now)
	}
	rt.nextCheck(KindChange, req.Name, dec.NextCheck)
	for _, ir := range dec.WakeItems {
		rt.addPending(KindItem, v1alpha1.ItemName(ir), observe.Pending{Trigger: decide.TriggerEngine, Source: rowSource})
	}
	rt.budgetChanged()
	return reconcile.Result{}, nil
}

// changeRef is a Change's PR ref: observed, else its annotation.
func changeRef(c *v1alpha1.Change) string {
	if r := wake.ChangeRef(c); r != "" {
		return r
	}
	return c.Annotations[AnnotationRef]
}

// ciDurationsOf is a repo's recorded pipeline durations, read from history once and then kept.
func (rt *Runtime) ciDurationsOf(ctx context.Context, repo string) []time.Duration {
	rt.mu.Lock()
	d, ok := rt.ciDurations[repo]
	rt.mu.Unlock()
	if ok || rt.cfg.HistoryReader == nil {
		return d
	}
	got, err := rt.cfg.HistoryReader.Durations(ctx, history.DurationCI, repo)
	if err != nil {
		return nil
	}
	rt.mu.Lock()
	rt.ciDurations[repo] = got
	rt.mu.Unlock()
	return got
}

func (rt *Runtime) addDuration(kind, key string, d time.Duration, at time.Time) {
	if rt.hist != nil {
		rt.hist.AddDuration(kind, key, d, at)
	}
	if kind != history.DurationCI {
		return
	}
	rt.mu.Lock()
	if cur, ok := rt.ciDurations[key]; ok {
		rt.ciDurations[key] = append(cur, d)
	}
	rt.mu.Unlock()
}
