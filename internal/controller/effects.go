package controller

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/decision"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/jev"
	"github.com/giantswarm/beekeeper/internal/observe"
)

// append writes history rows and counts what the metrics count from them: observed changes, missed
// webhooks, wait wakes and refusals.
func (rt *Runtime) append(events ...history.Event) {
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		switch e.Type {
		case observe.EventObserved:
			kind, _ := e.Data["kind"].(string)
			rt.m.Observations.WithLabelValues(e.Source, kind).Inc()
		case observe.EventMissed:
			ev, _ := e.Data["event"].(string)
			rt.m.WebhookMissed.WithLabelValues(ev).Inc()
		case observe.EventRefusal:
			op, _ := e.Data["operation"].(string)
			rt.m.Refusals.WithLabelValues(op).Inc()
		}
	}
	if rt.hist != nil {
		rt.hist.Append(events...)
	}
}

// countWakes counts the wait.woken rows of a reconcile by wait kind and trigger.
func (rt *Runtime) countWakes(events []history.Event, trigger decide.Trigger) {
	t := string(trigger)
	switch trigger {
	case decide.TriggerRetry:
		t = string(decide.TriggerPoll)
	case decide.TriggerStartup, decide.TriggerEngine:
		return
	}
	for _, e := range events {
		if e.Type == decide.EventWaitWoken {
			on, _ := e.Data["on"].(string)
			rt.m.WaitWakes.WithLabelValues(on, t).Inc()
		}
	}
}

// ask records each decision once per key (the history writer knows the keys recorded) and, in mode
// shadow, asks JEV once for the key, asynchronously; JEV's answer is recorded next to the rule's and
// never changes the outcome.
func (rt *Runtime) ask(spec *v1alpha1.InstallationSpec, qs []decision.Question) {
	if len(qs) == 0 || rt.hist == nil {
		return
	}
	now := rt.clock.Now()
	floor := 0.5
	if spec != nil && spec.JEV.ConfidenceFloor > 0 {
		floor = spec.JEV.ConfidenceFloor
	}
	for _, q := range qs {
		hash := q.InputsHash
		if hash == "" {
			hash = decision.Hash(q.Inputs)
		}
		key := history.Key{Point: q.Point, Subject: q.Subject, InputsHash: hash}
		d := history.Decision{At: now, Point: q.Point, Subject: q.Subject, Other: q.Other, Inputs: q.Inputs,
			InputsHash: hash, Rule: q.Rule, Chosen: q.Rule, Mode: q.Mode, Team: q.Team}
		if !rt.hist.Decide(d) {
			continue
		}
		ev := history.Event{At: now, Team: q.Team, Type: "decision", Source: v1alpha1.SourceEngine,
			Data: map[string]any{"point": q.Point, "chosen": q.Rule}}
		if _, err := v1alpha1.ParseRef(q.Subject); err == nil {
			ev.Item = q.Subject
		}
		rt.append(ev)
		if q.Mode != v1alpha1.DecisionModeShadow || rt.cfg.JEV == nil {
			continue
		}
		rt.askJEV(q, key, floor)
	}
}

func (rt *Runtime) askJEV(q decision.Question, key history.Key, floor float64) {
	done := rt.tracker.Begin("jev " + q.Point + " " + q.Subject)
	ctx := rt.context()
	go func() {
		defer done()
		ans, err := rt.cfg.JEV.Ask(ctx, jev.Request{Point: q.Point, Subject: q.Subject, Answers: q.Answers, Fields: q.Fields, Rule: q.Rule})
		if ctx.Err() != nil {
			return
		}
		outcome := decision.Outcome(q.Rule, ans.Answer, ans.Confidence, err, floor)
		answer, conf := ans.Answer, ans.Confidence
		if err != nil {
			answer, conf = "", 0
		}
		rt.hist.Answer(key, answer, conf, jev.ErrorKind(err), outcome)
		rt.m.Decisions.WithLabelValues(q.Point, q.Mode, outcome).Inc()
	}()
}

// scheduleObject wakes an object at at (its wait's next check or a transient retry); one timer per
// object, so a deadline and a retry due together reconcile it once.
func (rt *Runtime) scheduleObject(kind, name string, at time.Time) {
	rt.timers.Schedule("wait/"+kind+"/"+name, at, func() { rt.enqueue(kind, name) })
}

func (rt *Runtime) cancelObject(kind, name string) {
	rt.timers.Cancel("wait/" + kind + "/" + name)
	rt.mu.Lock()
	delete(rt.retries, kind+"/"+name)
	rt.mu.Unlock()
	rt.dropPending(kind + "/" + name)
}

// dropPending forgets an object's pending observation: there is nothing left to read for the
// deliveries that asked for it.
func (rt *Runtime) dropPending(key string) {
	if p := rt.pending.Drop(key); p != nil {
		for _, id := range p.Deliveries {
			rt.unitDone(id, key)
		}
	}
}

// nextCheck schedules an object's timer at the earlier of its decision's next check and its retry.
func (rt *Runtime) nextCheck(kind, name string, next *time.Time) {
	rt.mu.Lock()
	r, retrying := rt.retries[kind+"/"+name]
	rt.mu.Unlock()
	var at time.Time
	switch {
	case next != nil && retrying:
		at = *next
		if r.at.Before(at) {
			at = r.at
		}
	case next != nil:
		at = *next
	case retrying:
		at = r.at
	default:
		rt.timers.Cancel("wait/" + kind + "/" + name)
		return
	}
	rt.scheduleObject(kind, name, at)
}

// retry keeps an object's failed reads pending and schedules them again on the engine's clock.
func (rt *Runtime) retry(kind, name string, p *observe.Pending, err *forge.Error, spec *v1alpha1.InstallationSpec) {
	key := kind + "/" + name
	now := rt.clock.Now()
	rt.mu.Lock()
	r := rt.retries[key]
	var inst *v1alpha1.Installation
	if spec != nil {
		inst = &v1alpha1.Installation{Spec: *spec}
	}
	r.at = retryAt(err, now, r.attempt, inst)
	r.attempt++
	rt.retries[key] = r
	rt.mu.Unlock()
	if p != nil {
		rt.pending.Add(key, *p)
	}
}

// retrying reports whether an object's reads wait for a retry that isn't due yet.
func (rt *Runtime) retrying(key string, now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	r, ok := rt.retries[key]
	return ok && r.at.After(now)
}

func (rt *Runtime) retryDue(key string, now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	r, ok := rt.retries[key]
	return ok && !r.at.After(now)
}

func (rt *Runtime) succeeded(key string) {
	rt.mu.Lock()
	delete(rt.retries, key)
	rt.mu.Unlock()
}

// waitNext is when a wait must be checked: its next check, else its deadline.
func waitNext(w *v1alpha1.Wait) (time.Time, bool) {
	if w == nil {
		return time.Time{}, false
	}
	if w.NextCheck != nil {
		return w.NextCheck.Time, true
	}
	if !w.Deadline.IsZero() {
		return w.Deadline.Time, true
	}
	return time.Time{}, false
}

// dueTrigger is the trigger of a wait that is due: its deadline's re-observe, a retry while it is
// unobserved, or an overdue re-observe. While the stop_timers fault holds, only a wait's timer would
// handle it, so nothing is due.
func (rt *Runtime) dueTrigger(w *v1alpha1.Wait, now time.Time) (decide.Trigger, bool) {
	if rt.cfg.Faults.StopTimers.Load() {
		return "", false
	}
	return dueTrigger(w, now)
}

func dueTrigger(w *v1alpha1.Wait, now time.Time) (decide.Trigger, bool) {
	at, ok := waitNext(w)
	if !ok || now.Before(at) {
		return "", false
	}
	if w.State == v1alpha1.WaitUnobserved && (w.DeadlineHandled || now.Before(w.Deadline.Time)) {
		return decide.TriggerRetry, true
	}
	return decide.TriggerDeadline, true
}

// refusal records a refused read on an object's status: the first time (or with new inputs) with a
// refusal row; again on unchanged inputs (only when refusals are forgotten) as a retry.
func (rt *Runtime) refusal(st **v1alpha1.Refusal, fe *forge.Error, inputs string, now time.Time, subj observe.Subject, ref string) []history.Event {
	old := *st
	if old != nil && old.Operation == fe.Operation && old.Code == fe.Code && old.Inputs == inputs {
		r := old.DeepCopy()
		r.Retries++
		*st = r
		rt.mu.Lock()
		rt.refusalRe[ref] = r.Retries
		rt.mu.Unlock()
		return nil
	}
	r := &v1alpha1.Refusal{Operation: fe.Operation, Code: fe.Code, Inputs: inputs}
	r.Since.Time = now
	if old != nil && old.Operation == fe.Operation && old.Code == fe.Code {
		r.Since = old.Since
	}
	*st = r
	// The refusal stands until a webhook for its repo or ref (or a configuration or installation
	// change): count those webhooks from now on.
	rt.inputs.Watch(refRepo(ref), ref)
	return []history.Event{{At: now, Team: subj.Team, Item: subj.Item, Change: subj.Change, Type: observe.EventRefusal,
		Source: v1alpha1.SourceEngine, Data: map[string]any{"operation": fe.Operation, "code": fe.Code}}}
}

// budgetChanged asks the Budget reconciler to write the budget when what it shows changed.
func (rt *Runtime) budgetChanged() {
	if rt.forge == nil {
		return
	}
	b := rt.forge.Budget()
	now := rt.clock.Now()
	rt.mu.Lock()
	changed := !rt.budgetWritten || budgetKey(b, now) != rt.lastBudget
	rt.mu.Unlock()
	if changed {
		rt.budgetQ.Add(v1alpha1.BudgetName)
	}
}

// budgetSig is what of the budget makes a write: the REST window, the limits and reset times, and
// what is deferred. The GraphQL points and the App's calls an idle tick spends don't.
type budgetSig struct {
	restLimit, restRemaining, gqlLimit, appLimit int
	restReset, gqlReset, appReset                time.Time
	restPaused, gqlPaused, appPaused             bool
}

func budgetKey(b forge.Budget, now time.Time) budgetSig {
	return budgetSig{restLimit: b.REST.Limit, restRemaining: b.REST.Remaining, gqlLimit: b.GraphQL.Limit,
		appLimit: b.App.Limit, restReset: b.REST.ResetAt, gqlReset: b.GraphQL.ResetAt, appReset: b.App.ResetAt,
		restPaused: paused(b.REST, now), gqlPaused: paused(b.GraphQL, now), appPaused: paused(b.App, now)}
}

// readCtx is the context of observation reads.
func readCtx(ctx context.Context) context.Context {
	return forge.WithClass(ctx, forge.ClassObservation)
}

// changeBusy marks a Change's reconcile as running; when it ends, the Items that deferred to it are
// reconciled.
func (rt *Runtime) changeBusy(key string, busy bool) {
	rt.mu.Lock()
	if busy {
		rt.busyChanges[key] = true
		rt.mu.Unlock()
		return
	}
	delete(rt.busyChanges, key)
	waiting := rt.waiters[key]
	delete(rt.waiters, key)
	rt.mu.Unlock()
	for _, name := range sortedKeys(waiting) {
		rt.itemQ.Add(name)
	}
}

// deferToChanges puts an Item's reconcile off while one of its Changes is observed (has reads pending
// that aren't waiting for a retry, or is being reconciled): the Change wakes it when it is done. The
// Item's pending observation is kept.
func (rt *Runtime) deferToChanges(item string, changes []string, p *observe.Pending) bool {
	now := rt.clock.Now()
	var on []string
	for _, cr := range changes {
		key := KindChange + "/" + v1alpha1.ChangeName(cr)
		rt.mu.Lock()
		busy := rt.busyChanges[key]
		rt.mu.Unlock()
		if busy || (rt.pending.Has(key) && !rt.retrying(key, now)) {
			on = append(on, key)
		}
	}
	if len(on) == 0 {
		return false
	}
	if p != nil {
		rt.pending.Add(KindItem+"/"+item, *p)
	}
	rt.mu.Lock()
	for _, key := range on {
		if rt.waiters[key] == nil {
			rt.waiters[key] = map[string]bool{}
		}
		rt.waiters[key][item] = true
	}
	stillBusy := false
	for _, key := range on {
		stillBusy = stillBusy || rt.busyChanges[key] || rt.pending.Has(key)
	}
	rt.mu.Unlock()
	if !stillBusy {
		// The Change finished meanwhile: don't wait for a wake that already happened.
		rt.itemQ.Add(item)
	}
	return true
}
