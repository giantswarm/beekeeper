package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/observe"
	"github.com/giantswarm/beekeeper/internal/wake"
)

// HandleDelivery handles one webhook delivery from the inbox: it maps it to wake refs and wakes the
// Items and Changes owning or waiting on them, each with the reads the ref needs. Nothing in the
// payload is trusted beyond the ids. A delivery it can't read names nothing. When the inbox can
// acknowledge deliveries later, a delivery whose reads are queued is held (inbox.ErrHeld) until they
// ran, so a leader that stops first leaves it to the next one.
func (rt *Runtime) HandleDelivery(ctx context.Context, d inbox.Delivery) error {
	defer func() {
		select {
		case rt.drained <- struct{}{}:
		default:
		}
	}()
	if !rt.leading.Load() {
		return nil
	}
	m, err := wake.Map(d.Event, d.Payload)
	if err != nil {
		rt.log.Info("unreadable delivery", "id", d.ID, "event", d.Event, "error", err.Error())
		return nil
	}
	id := ""
	if _, ok := rt.cfg.Inbox.(inbox.Acker); ok && d.ID != "" {
		id = d.ID
		rt.holdUnit(id, unitRoute)
	}
	rt.route(ctx, m, v1alpha1.SourceWebhook, decide.TriggerWebhook, id)
	if id != "" && rt.routed(id) {
		return inbox.ErrHeld
	}
	return nil
}

// route wakes what a delivery names, attributed to source: a webhook, or a re-observe of a delivery
// the log shows as failed. The reads it queues for delivery id (when not "") are its units: the
// delivery is acknowledged once each ran.
func (rt *Runtime) route(ctx context.Context, m wake.Delivery, source string, trigger decide.Trigger, id string) {
	if m.Ignore {
		return
	}
	if m.ProjectNode != "" {
		rt.mu.Lock()
		project := rt.obs.projectID
		rt.mu.Unlock()
		if project != "" && m.ProjectNode != project {
			return // another board's item
		}
		if m.ContentType != "" && m.ContentType != "Issue" {
			return // draft issues and PRs on the board aren't items
		}
	}
	if source == v1alpha1.SourceWebhook {
		keys := []string{m.Repo}
		for _, ref := range m.Refs {
			if _, v, ok := wake.Split(ref); ok {
				keys = append(keys, v)
			}
		}
		rt.inputs.Webhook(keys...)
	}
	req := taskReq{Source: source, Trigger: trigger, Events: []string{m.Event}}
	if id != "" {
		req.Deliveries = []string{id}
	}
	if m.InstallationRepos {
		rt.requestTask(taskRepos, req)
	}
	for _, ref := range m.Refs {
		kind, value, _ := wake.Split(ref)
		items, _ := rt.store.items(ctx, func(it *v1alpha1.Item) bool { return containsStr(wake.ItemRefs(it), ref) },
			client.MatchingFields{wake.IndexRefs: ref})
		for _, it := range items {
			rt.addPending(KindItem, it.Name, req.pending(observe.Pending{ReadBoard: true}))
		}
		changes, _ := rt.store.changes(ctx, func(c *v1alpha1.Change) bool { return containsStr(wake.ChangeRefs(c), ref) },
			client.MatchingFields{wake.IndexRefs: ref})
		for _, c := range changes {
			var reads observe.ReadSet
			switch kind {
			case "pr":
				reads = observe.ReadSet{PR: true, Reviews: true}
			case "ci":
				_, sha, _ := wake.SplitCI(ref)
				reads = observe.ReadSet{SHAs: []string{sha}}
			default:
				reads = observe.FullReads()
			}
			rt.addPending(KindChange, c.Name, req.pending(observe.Pending{Reads: reads}))
		}
		switch {
		case kind == "board" && len(items) == 0:
			// A board item no Item owns: read it to learn whether a team tracks it.
			rt.request(&rt.obs.resolve, unitResolve, value, req)
			rt.taskQ.Add(taskResolve)
		case kind == "pr" && m.LinkPR == value:
			// The PR's closing references may have changed: read the issues it closes now, and re-read
			// those Items and the ones that listed it (never a board resync: a webhook vouches only for
			// what its own event covers).
			rt.request(&rt.obs.closing, unitClosing, value, req)
			rt.taskQ.Add(taskClosing)
		}
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// taskReq is what asked for a read the observer makes: the source and trigger what it finds is
// attributed to, the webhook events that vouch for it, and the deliveries waiting for it.
type taskReq struct {
	Source     string
	Trigger    decide.Trigger
	Events     []string
	Deliveries []string
}

// merge adds q: the better-ranked source wins, the events and deliveries add up.
func (r *taskReq) merge(q taskReq) {
	p := observe.Pending{Source: r.Source, Trigger: r.Trigger, Events: r.Events, Deliveries: r.Deliveries}
	p.Merge(observe.Pending{Source: q.Source, Trigger: q.Trigger, Events: q.Events, Deliveries: q.Deliveries})
	r.Source, r.Trigger, r.Events, r.Deliveries = p.Source, p.Trigger, p.Events, p.Deliveries
}

// pending is the Pending of reads p asked for by r.
func (r taskReq) pending(p observe.Pending) observe.Pending {
	p.Source, p.Trigger = r.Source, r.Trigger
	p.Events = append([]string(nil), r.Events...)
	p.Deliveries = append([]string(nil), r.Deliveries...)
	return p
}

// request queues key's read in one of the observer's request maps, held for r's deliveries.
func (rt *Runtime) request(m *map[string]*taskReq, unit, key string, r taskReq) {
	for _, id := range r.Deliveries {
		rt.holdUnit(id, unit+key)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if *m == nil {
		*m = map[string]*taskReq{}
	}
	if cur, ok := (*m)[key]; ok {
		cur.merge(r)
		return
	}
	cp := r
	(*m)[key] = &cp
}

// requestTask asks for a task on r's behalf (held for r's deliveries until it succeeds).
func (rt *Runtime) requestTask(task string, r taskReq) {
	for _, id := range r.Deliveries {
		rt.holdUnit(id, unitTask+task)
	}
	rt.mu.Lock()
	rt.obs.taskDeliveries[task] = union(rt.obs.taskDeliveries[task], r.Deliveries)
	rt.mu.Unlock()
	rt.runTaskNow(task, r.Source)
}

// Units a held delivery waits for: its routing, an object's pending reads (kind/name), and the
// observer's reads it asked for.
const (
	unitRoute   = "route"
	unitResolve = "resolve/"
	unitClosing = "closing/"
	unitTask    = "task/"
)

// heldDelivery is a delivery whose reads haven't all run.
type heldDelivery struct {
	units map[string]bool
	// held is true once its handler returned inbox.ErrHeld: the last unit done acknowledges it.
	held bool
}

// holdUnit notes that delivery id waits for unit.
func (rt *Runtime) holdUnit(id, unit string) {
	if id == "" {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	d := rt.deliveries[id]
	if d == nil {
		d = &heldDelivery{units: map[string]bool{}}
		rt.deliveries[id] = d
	}
	d.units[unit] = true
}

// unitDone notes that delivery id's unit ran; the last one acknowledges a held delivery.
func (rt *Runtime) unitDone(id, unit string) {
	rt.mu.Lock()
	d := rt.deliveries[id]
	if d == nil {
		rt.mu.Unlock()
		return
	}
	delete(d.units, unit)
	ack := len(d.units) == 0 && d.held
	if len(d.units) == 0 {
		delete(rt.deliveries, id)
	}
	rt.mu.Unlock()
	if a, ok := rt.cfg.Inbox.(inbox.Acker); ok && ack {
		a.Ack(id)
	}
}

// unitsDone notes that unit ran for every delivery in ids.
func (rt *Runtime) unitsDone(ids []string, unit string) {
	for _, id := range ids {
		rt.unitDone(id, unit)
	}
}

// routed ends a delivery's routing: it reports whether reads are left (the delivery is held).
func (rt *Runtime) routed(id string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	d := rt.deliveries[id]
	if d == nil {
		return false
	}
	delete(d.units, unitRoute)
	if len(d.units) == 0 {
		delete(rt.deliveries, id)
		return false
	}
	d.held = true
	return true
}

// tookPending notes that the reads of a taken pending observation ran, for each delivery that asked
// for them and doesn't wait on key again (a retry or a deferral put them back).
func (rt *Runtime) tookPending(key string, p *observe.Pending) {
	if p == nil {
		return
	}
	for _, id := range p.Deliveries {
		if !rt.pending.HasDelivery(key, id) {
			rt.unitDone(id, key)
		}
	}
}

// union adds the entries of b missing from a.
func union(a, b []string) []string {
	for _, v := range b {
		if !containsStr(a, v) {
			a = append(a, v)
		}
	}
	return a
}
