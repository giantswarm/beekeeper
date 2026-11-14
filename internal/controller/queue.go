package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/giantswarm/beekeeper/internal/settle"
)

// trackedQueue is a controller's work queue that knows exactly which requests are queued or in
// flight, so the settle tracker sees a reconcile from the moment it is asked for until it is done
// (contract §3.3). A request added while it is processed stays busy until it is processed again.
type trackedQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	name    string
	tracker *settle.Tracker

	mu         sync.Mutex
	dirty      map[reconcile.Request]bool
	processing map[reconcile.Request]bool
	shutdown   bool
}

func newTrackedQueue(name string, rl workqueue.TypedRateLimiter[reconcile.Request], t *settle.Tracker) *trackedQueue {
	q := &trackedQueue{
		TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueueWithConfig(rl,
			workqueue.TypedRateLimitingQueueConfig[reconcile.Request]{Name: name}),
		name: name, tracker: t,
		dirty: map[reconcile.Request]bool{}, processing: map[reconcile.Request]bool{},
	}
	t.AddProbe("reconcile/"+name, q.idle)
	return q
}

func (q *trackedQueue) mark(r reconcile.Request) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		return false
	}
	q.dirty[r] = true
	return true
}

func (q *trackedQueue) Add(r reconcile.Request) {
	if q.mark(r) {
		q.TypedRateLimitingInterface.Add(r)
	}
}

func (q *trackedQueue) AddAfter(r reconcile.Request, d time.Duration) {
	if q.mark(r) {
		q.TypedRateLimitingInterface.AddAfter(r, d)
	}
}

func (q *trackedQueue) AddRateLimited(r reconcile.Request) {
	if q.mark(r) {
		q.TypedRateLimitingInterface.AddRateLimited(r)
	}
}

func (q *trackedQueue) Get() (reconcile.Request, bool) {
	r, shutdown := q.TypedRateLimitingInterface.Get()
	if !shutdown {
		q.mu.Lock()
		delete(q.dirty, r)
		q.processing[r] = true
		q.mu.Unlock()
	}
	return r, shutdown
}

func (q *trackedQueue) Done(r reconcile.Request) {
	q.mu.Lock()
	delete(q.processing, r)
	q.mu.Unlock()
	q.TypedRateLimitingInterface.Done(r)
	q.tracker.Poke()
}

func (q *trackedQueue) ShutDown() {
	q.stop()
	q.TypedRateLimitingInterface.ShutDown()
}

func (q *trackedQueue) ShutDownWithDrain() {
	q.stop()
	q.TypedRateLimitingInterface.ShutDownWithDrain()
}

// stop forgets what is queued: a stopped controller does no more work, so it is not busy.
func (q *trackedQueue) stop() {
	q.mu.Lock()
	q.shutdown = true
	q.dirty = map[reconcile.Request]bool{}
	q.processing = map[reconcile.Request]bool{}
	q.mu.Unlock()
	q.tracker.RemoveProbe("reconcile/" + q.name)
	q.tracker.Poke()
}

func (q *trackedQueue) idle() (bool, string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.dirty) == 0 && len(q.processing) == 0 {
		return true, ""
	}
	var names []string
	for r := range q.dirty {
		names = append(names, "queued "+r.Name)
	}
	for r := range q.processing {
		names = append(names, "reconciling "+r.Name)
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = append(names[:5], fmt.Sprintf("+%d", len(names)-5))
	}
	return false, fmt.Sprint(names)
}

// enqueuer is a controller source the engine adds requests to synchronously (timer callbacks, wakes,
// tasks). Requests added before the controller started are held until it starts.
type enqueuer struct {
	name    string
	ns      string
	tracker *settle.Tracker

	mu      sync.Mutex
	q       workqueue.TypedRateLimitingInterface[reconcile.Request]
	buf     []reconcile.Request
	stopped bool
}

func newEnqueuer(name, ns string, t *settle.Tracker) *enqueuer {
	e := &enqueuer{name: name, ns: ns, tracker: t}
	t.AddProbe("enqueue/"+name, e.idle)
	return e
}

// Start implements source.Source.
func (e *enqueuer) Start(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	e.mu.Lock()
	e.q = q
	buf := e.buf
	e.buf = nil
	e.mu.Unlock()
	for _, r := range buf {
		q.Add(r)
	}
	// The held requests moved to the queue: this source's probe is idle now (settle re-checks).
	e.tracker.Poke()
	go func() {
		<-ctx.Done()
		e.stop()
	}()
	return nil
}

func (e *enqueuer) String() string { return "engine source " + e.name }

// Add asks for a reconcile of name.
func (e *enqueuer) Add(name string) {
	// The namespace makes the request the same key as the object's own events: one object is never
	// reconciled twice at once.
	r := reconcile.Request{}
	r.Namespace, r.Name = e.ns, name
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	q := e.q
	if q == nil {
		e.buf = append(e.buf, r)
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	q.Add(r)
}

func (e *enqueuer) stop() {
	e.mu.Lock()
	e.stopped, e.q, e.buf = true, nil, nil
	e.mu.Unlock()
	e.tracker.RemoveProbe("enqueue/" + e.name)
	e.tracker.Poke()
}

func (e *enqueuer) idle() (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) == 0 {
		return true, ""
	}
	return false, fmt.Sprintf("%d requests before %s started", len(e.buf), e.name)
}
