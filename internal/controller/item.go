package controller

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/observe"
	"github.com/giantswarm/beekeeper/internal/wake"
)

// reconcileItem observes an Item (its pending reads and a due wait's re-observe), decides it, writes
// its status once when anything changed (never status.shadow, which only the team pass writes), and
// runs the effects: history rows, decision questions, its next check, its Changes and the team pass.
func (rt *Runtime) reconcileItem(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	key := KindItem + "/" + req.Name
	deferred := false
	defer func() {
		if !deferred {
			rt.startupProgress(key)
		}
	}()
	item := &v1alpha1.Item{}
	if err := rt.store.get(ctx, KindItem, req.Name, item); err != nil {
		if apierrors.IsNotFound(err) {
			rt.cancelObject(KindItem, req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	inst := rt.installation(ctx)
	if inst == nil {
		return reconcile.Result{}, nil
	}
	now := rt.clock.Now()
	p := rt.pending.Take(key)
	// The deliveries that asked for these reads are done with this key once it ran, unless a deferral or
	// a retry below puts the reads back.
	defer rt.tookPending(key, p)
	if rt.deferToChanges(req.Name, item.Status.Changes, p) {
		// A Change of the item is being observed: decide once it is (a merge and the issue it closes
		// arrive together), so the phase doesn't pass through what neither observation alone shows.
		deferred = true
		return reconcile.Result{}, nil
	}
	if p != nil && !p.Empty() && rt.retrying(key, now) {
		// The reads wait for their retry.
		rt.pending.Add(key, *p)
		p = nil
	}
	team := rt.teamOf(ctx, item)
	option := ""
	teamName := item.Labels[v1alpha1.LabelTeam]
	if team != nil {
		option, teamName = team.Spec.BoardOption, team.Name
	}
	ref := itemRef(item)
	subj := observe.Subject{Team: teamName, Item: ref}

	trigger, source := decide.TriggerEngine, v1alpha1.SourceEngine
	var events []string
	if p != nil {
		trigger, source, events = p.Trigger, p.Source, p.Events
	}
	if rt.retryDue(key, now) {
		trigger = decide.TriggerRetry
	}
	dueT, due := rt.dueTrigger(item.Status.Wait, now)
	if due {
		trigger = dueT
	}
	rowSource := observationSource(trigger, source)

	view := item.DeepCopy()
	old := item.Status.DeepCopy()
	var rows []history.Event
	var re *decide.Reobserve
	scopeReason := ""

	var bi *forge.BoardItem
	found := true
	read := false
	if p != nil && p.Board != nil {
		bi, found = p.Board, p.Found
	} else if (p != nil && p.ReadBoard) || due {
		node := ""
		if item.Status.Board != nil {
			node = item.Status.Board.NodeID
		}
		inputs := rt.inputs.Key(refRepo(ref), ref)
		switch {
		case node == "":
		case item.Status.Refusal != nil && item.Status.Refusal.Inputs == inputs && !rt.cfg.Faults.ForgetRefusals.Load():
			// Refused before on these inputs: not read again until they change.
		default:
			read = true
			got, ok, err := rt.forge.BoardItem(readCtx(ctx), node)
			switch fe, isForge := forge.AsError(err); {
			case err == nil:
				bi, found = &got, ok
				rt.succeeded(key)
				view.Status.Refusal = nil
				if source == v1alpha1.SourceWebhook && ok {
					rt.reported(got)
				}
			case isForge && fe.Kind == forge.Refused:
				rows = append(rows, rt.refusal(&view.Status.Refusal, fe, inputs, now, subj, ref)...)
			default:
				if !isForge {
					fe = &forge.Error{Kind: forge.Transient, API: forge.APIGraphQL, Operation: forge.OpBoardItemsByNode, Err: err}
				}
				re = &decide.Reobserve{Made: fe.Code != 0, Err: fe}
				rt.graphQLFailed(fe)
				keep := p
				if keep == nil {
					keep = &observe.Pending{Trigger: trigger, Source: rowSource, ReadBoard: true}
				}
				rt.retry(KindItem, req.Name, keep, fe, &inst.Spec)
			}
		}
	}
	if bi != nil || !found {
		var b forge.BoardItem
		if bi != nil {
			b = *bi
		}
		scopeReason = observe.LeftScope(b, found, option)
		if team == nil {
			scopeReason = ""
		}
		if scopeReason == "" && bi != nil {
			observe.ApplyBoardItem(&view.Status, b)
			obs := observe.DiffItem(old, &view.Status)
			if len(obs) > 0 {
				view.Status.Observed = &v1alpha1.Observed{At: metav1.NewTime(now), Source: rowSource}
			}
			rows = append(rows, observe.Record(startupObservation(obs, observe.KindBoard, rowSource, true),
				subj, rowSource, events, now, rt.inputs.Installed).Events...)
			if read {
				re = &decide.Reobserve{Made: true, Changed: len(obs) > 0}
			}
		}
	}

	// An epic's sub-issues that are Items show their own board status and phase (the epic's board read
	// carries only their state).
	for i := range view.Status.SubIssues {
		sub := &view.Status.SubIssues[i]
		si := &v1alpha1.Item{}
		if err := rt.store.get(ctx, KindItem, v1alpha1.ItemName(sub.Ref), si); err == nil && si.Status.Board != nil {
			sub.Status, sub.Phase = si.Status.Board.Status, si.Status.Phase
		}
	}
	var parent *v1alpha1.Item
	if view.Status.Parent != "" {
		pi := &v1alpha1.Item{}
		if err := rt.store.get(ctx, KindItem, v1alpha1.ItemName(view.Status.Parent), pi); err == nil {
			parent = pi
		}
	}
	var changes []*v1alpha1.Change
	for _, cr := range view.Status.Changes {
		c := &v1alpha1.Change{}
		if err := rt.store.get(ctx, KindChange, v1alpha1.ChangeName(cr), c); err == nil {
			changes = append(changes, c)
		}
	}
	participants := rt.participants(ctx, teamName)

	if rt.cfg.Faults.StopTimers.Load() {
		// stop_timers: only a wait's timer would handle its deadline, and its timers don't fire, so the
		// reads of other triggers don't count as its re-observe.
		re = nil
	}
	dec := rt.cfg.DecideItem(decide.ItemView{
		Now: now, Spec: &inst.Spec, Team: team, Participants: participants, Item: view, Parent: parent,
		Changes: changes, Trigger: trigger, Source: source, Reobserve: re,
		LeftScope: scopeReason != "", ScopeReason: scopeReason, DropWaits: rt.cfg.Faults.DropWaits.Load(),
	})
	rows = append(rows, dec.Events...)

	if dec.Delete {
		if scopeReason != "" && !hasEvent(dec.Events, decide.EventItemRemoved) {
			rows = append(rows, history.Event{At: now, Team: teamName, Item: ref, Type: decide.EventItemRemoved,
				Source: rowSource, Data: map[string]any{"reason": scopeReason}})
		}
		rt.append(rows...)
		rt.countWakes(dec.Events, trigger)
		if err := rt.store.delete(ctx, KindItem, item); err != nil {
			return reconcile.Result{}, err
		}
		rt.cancelObject(KindItem, req.Name)
		for _, cr := range item.Status.Changes {
			rt.enqueue(KindChange, v1alpha1.ChangeName(cr))
		}
		rt.teamPass(teamName, rowSource)
		return reconcile.Result{}, nil
	}

	st := dec.Status
	st.Shadow = item.Status.Shadow
	if !equality.Semantic.DeepEqual(item.Status, st) {
		mod := item.DeepCopy()
		mod.Status = st
		if err := rt.store.patchStatus(ctx, KindItem, mod, item); err != nil {
			if p != nil {
				rt.pending.Add(key, *p)
			}
			return reconcile.Result{}, err
		}
	}
	rt.append(rows...)
	rt.countWakes(dec.Events, trigger)
	rt.ask(&inst.Spec, dec.Questions)
	rt.nextCheck(KindItem, req.Name, dec.NextCheck)

	// Changes: create the ones the item gained (read with the item's source), wake the ones it lost.
	if err := rt.linkChanges(ctx, item.Status.Changes, st.Changes, teamName, rowSource); err != nil {
		return reconcile.Result{}, err
	}
	if dec.TeamPass {
		rt.teamPass(teamName, rowSource)
	}
	// An epic's phase follows its sub-items', and a sub-item's owner its epic's.
	if item.Status.Phase != st.Phase || item.Status.Board == nil || boardStatus(item) != boardStatus(&v1alpha1.Item{Status: st}) {
		if st.Parent != "" {
			rt.addPending(KindItem, v1alpha1.ItemName(st.Parent), observe.Pending{Trigger: decide.TriggerEngine, Source: rowSource})
		}
	}
	if item.Status.Owner != st.Owner || item.Status.Class != st.Class {
		for _, sub := range st.SubItems {
			rt.addPending(KindItem, v1alpha1.ItemName(sub), observe.Pending{Trigger: decide.TriggerEngine, Source: rowSource})
		}
	}
	rt.budgetChanged()
	return reconcile.Result{}, nil
}

// startupObservation records a leader's start-up read of an object even when it found nothing new
// (one observed row with no fields), so every start shows what it observed; other reads record only
// changes.
func startupObservation(obs []observe.Observation, kind, source string, ok bool) []observe.Observation {
	if len(obs) > 0 || source != v1alpha1.SourceStartup || !ok {
		return obs
	}
	return []observe.Observation{{Kind: kind, Fields: []string{}}}
}

// observationSource is what an observation is attributed to: a re-observe for deadlines and polls,
// else the source that asked for the reads (a retry keeps the webhook it retries).
func observationSource(t decide.Trigger, source string) string {
	switch t {
	case decide.TriggerDeadline, decide.TriggerPoll:
		return v1alpha1.SourceReobserve
	}
	if source == "" || source == v1alpha1.SourceEngine {
		if t == decide.TriggerRetry {
			return v1alpha1.SourceReobserve
		}
		return v1alpha1.SourceEngine
	}
	return source
}

func hasEvent(evs []history.Event, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func boardStatus(it *v1alpha1.Item) string {
	if it.Status.Board == nil {
		return ""
	}
	return it.Status.Board.Status
}

// refRepo is the owner/repo of a ref.
func refRepo(ref string) string {
	if r, err := v1alpha1.ParseRef(ref); err == nil {
		return r.FullRepo()
	}
	return ""
}

// itemRef is an Item's issue ref: observed, else its annotation.
func itemRef(it *v1alpha1.Item) string {
	if r := wake.ItemRef(it); r != "" {
		return r
	}
	return it.Annotations[AnnotationRef]
}

// teamOf is the Team an Item belongs to (its team label).
func (rt *Runtime) teamOf(ctx context.Context, it *v1alpha1.Item) *v1alpha1.Team {
	name := it.Labels[v1alpha1.LabelTeam]
	if name == "" {
		return nil
	}
	t := &v1alpha1.Team{}
	if err := rt.store.get(ctx, KindTeam, name, t); err != nil {
		return nil
	}
	return t
}

// participants are the logins of a team's People, sorted.
func (rt *Runtime) participants(ctx context.Context, team string) []string {
	people, err := rt.store.people(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range people {
		if containsStr(p.Spec.Teams, team) {
			login := p.Spec.Login
			if login == "" {
				login = p.Name
			}
			out = append(out, login)
		}
	}
	sort.Strings(out)
	return out
}

// linkChanges creates a Change for every PR the item gained and wakes the ones it lost (a Change no
// Item lists any more goes).
func (rt *Runtime) linkChanges(ctx context.Context, before, after []string, team, source string) error {
	for _, cr := range after {
		name := v1alpha1.ChangeName(cr)
		c := &v1alpha1.Change{}
		err := rt.store.get(ctx, KindChange, name, c)
		if err == nil {
			if !containsStr(before, cr) {
				rt.enqueue(KindChange, name)
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if _, perr := v1alpha1.ParseRef(cr); perr != nil {
			continue
		}
		key := KindChange + "/" + name
		rt.pending.Add(key, observe.Pending{Trigger: triggerOf(source), Source: source, Reads: observe.FullReads()})
		rt.touch(key)
		obj := &v1alpha1.Change{ObjectMeta: metav1.ObjectMeta{Name: name,
			Labels:      map[string]string{v1alpha1.LabelTeam: team},
			Annotations: map[string]string{AnnotationRef: cr}}}
		if err := rt.store.create(ctx, KindChange, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			// Skipped, not fatal: the item's other Changes are linked, and the next read of the item
			// tries again.
			rt.log.Error(err, "create a Change", "pr", cr)
			rt.m.ReconcileErrors.WithLabelValues(KindChange).Inc()
			rt.dropPending(key)
			rt.startupProgress(key)
			continue
		}
		rt.changeQ.Add(name)
	}
	for _, cr := range before {
		if !containsStr(after, cr) {
			rt.enqueue(KindChange, v1alpha1.ChangeName(cr))
		}
	}
	return nil
}
