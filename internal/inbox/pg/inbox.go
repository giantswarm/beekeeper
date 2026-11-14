// Package inboxpg is the Postgres inbox (plan §4, §5): the receiver stores each verified delivery once
// (Store), and the leader drains them in arrival order, woken by LISTEN/NOTIFY (Drainer).
package inboxpg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/inbox"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Channel is the NOTIFY channel the inbox trigger signals on.
const Channel = "inbox"

// Store stores deliveries; the receiver's role needs only INSERT on inbox.
type Store struct{ pool *pgxpool.Pool }

var _ inbox.Store = (*Store)(nil)

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Put stores d once: a delivery whose ID was stored before is a duplicate and isn't stored again.
func (s *Store) Put(ctx context.Context, d inbox.Delivery) (duplicate bool, err error) {
	payload := d.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO inbox (delivery_id, event, action, received_at, payload)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, d.ID, d.Event, d.Action, d.ReceivedAt.UTC(), payload)
	if err != nil {
		return false, fmt.Errorf("inbox: store delivery %s: %w", d.ID, err)
	}
	return tag.RowsAffected() == 0, nil
}

// Drainer defaults.
const (
	// DefaultRetention is how long processed deliveries are kept, on the engine clock (GitHub keeps its
	// delivery log as long).
	DefaultRetention = 72 * time.Hour
	// DefaultRetryInterval is how often (real time) the drainer reconnects while Postgres is unreachable.
	DefaultRetryInterval = time.Second
	// DefaultBackoff is the first wait (engine clock) before a delivery whose handler failed is retried;
	// it doubles up to DefaultBackoffCap.
	DefaultBackoff    = 10 * time.Second
	DefaultBackoffCap = 5 * time.Minute
)

const (
	batchSize    = 100
	cleanupEvery = time.Hour
	queryTimeout = 10 * time.Second
)

// Option tunes a Drainer.
type Option func(*Drainer)

// WithRetryInterval sets the real-time reconnect interval while Postgres is unreachable.
func WithRetryInterval(d time.Duration) Option { return func(dr *Drainer) { dr.retry = d } }

// WithRetention sets how long processed deliveries are kept.
func WithRetention(d time.Duration) Option { return func(dr *Drainer) { dr.retention = d } }

// WithBackoff sets the engine-clock backoff for deliveries whose handler failed.
func WithBackoff(first, max time.Duration) Option {
	return func(dr *Drainer) { dr.backoff, dr.backoffCap = first, max }
}

// WithProbeName names the settle probe (default "inbox-<n>", unique per drainer).
func WithProbeName(name string) Option { return func(dr *Drainer) { dr.probeName = name } }

// Drainer hands stored deliveries to a handler in arrival order. Only the leader runs it; deliveries
// stored while no leader runs are drained by the next one.
type Drainer struct {
	pool       *pgxpool.Pool
	clock      clock.Clock
	tracker    *settle.Tracker
	retry      time.Duration
	retention  time.Duration
	backoff    time.Duration
	backoffCap time.Duration
	probeName  string

	wake chan struct{}

	mu          sync.Mutex
	running     bool
	parked      map[string]*parked // deliveries whose handler failed, waiting for their retry
	held        map[string]bool    // deliveries the handler held (inbox.ErrHeld), until their Ack
	early       map[string]bool    // deliveries acknowledged before their handler returned
	acked       []string           // acknowledged deliveries, to be marked processed
	lastCleanup time.Time
}

type parked struct {
	failures int
	due      bool
	timer    clock.Timer
}

var (
	_ inbox.Drainer = (*Drainer)(nil)
	_ inbox.Acker   = (*Drainer)(nil)
)

var probes atomic.Int64

// NewDrainer returns a Drainer on pool. clk stamps processed_at, times retention and handler retries.
// With a tracker it registers a probe that is busy while Run is draining and a stored delivery is
// unprocessed (a handler in flight is tracked too).
func NewDrainer(pool *pgxpool.Pool, clk clock.Clock, tracker *settle.Tracker, opts ...Option) *Drainer {
	d := &Drainer{
		pool:       pool,
		clock:      clk,
		tracker:    tracker,
		retry:      DefaultRetryInterval,
		retention:  DefaultRetention,
		backoff:    DefaultBackoff,
		backoffCap: DefaultBackoffCap,
		probeName:  fmt.Sprintf("inbox-%d", probes.Add(1)),
		wake:       make(chan struct{}, 1),
		parked:     map[string]*parked{},
		held:       map[string]bool{},
		early:      map[string]bool{},
	}
	for _, o := range opts {
		o(d)
	}
	tracker.AddProbe(d.probeName, d.probe)
	return d
}

// Running reports whether Run is draining (only then is the probe ever busy).
func (d *Drainer) Running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// Close removes the drainer's settle probe.
func (d *Drainer) Close() { d.tracker.RemoveProbe(d.probeName) }

// Unprocessed is the number of stored deliveries not yet handled.
func (d *Drainer) Unprocessed(ctx context.Context) (int, error) {
	var n int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM inbox WHERE processed_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("inbox: unprocessed: %w", err)
	}
	return n, nil
}

// Ack marks a delivery the handler held (inbox.ErrHeld) processed, at the next drain.
func (d *Drainer) Ack(id string) {
	d.mu.Lock()
	if d.held[id] {
		delete(d.held, id)
		d.acked = append(d.acked, id)
	} else if d.running {
		d.early[id] = true
	}
	d.mu.Unlock()
	d.signal()
	d.tracker.Poke()
}

// Run drains unprocessed deliveries in arrival order until ctx ends, woken by NOTIFY. A delivery whose
// handler fails stays unprocessed and is retried after a backoff on the engine clock (later deliveries go
// on meanwhile). One the handler holds (inbox.ErrHeld) stays unprocessed until Ack, out of the drain;
// the next Run hands it out again. While Postgres is unreachable Run retries on a short real-time
// interval. It returns nil when ctx ends.
func (d *Drainer) Run(ctx context.Context, h inbox.Handler) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return errors.New("inbox: drainer already running")
	}
	d.running = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running = false
		for id, p := range d.parked {
			if p.timer != nil {
				p.timer.Stop()
			}
			delete(d.parked, id)
		}
		// Held and acknowledged deliveries not yet marked are handed out again by the next Run.
		d.held, d.early, d.acked = map[string]bool{}, map[string]bool{}, nil
		d.mu.Unlock()
		d.tracker.Poke()
	}()
	d.tracker.Poke()

	retry := time.NewTimer(d.retry)
	retry.Stop()
	defer retry.Stop()
	for {
		_ = d.session(ctx, h) // returns when Postgres is unreachable (or ctx ended)
		if ctx.Err() != nil {
			return nil
		}
		retry.Reset(d.retry)
		select {
		case <-ctx.Done():
			return nil
		case <-retry.C:
		case <-d.wake:
			// The probe saw deliveries waiting, so Postgres is back: reconnect at once.
			retry.Stop()
		}
	}
}

// session listens and drains on one connection until it fails or ctx ends.
func (d *Drainer) session(ctx context.Context, h inbox.Handler) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The listening connection leaves the pool: notifications must not reach other users of it.
	lc := conn.Hijack()
	lctx, cancel := context.WithCancel(ctx)
	listenErr := make(chan error, 1)
	stopped := make(chan struct{})
	defer func() {
		cancel()
		<-stopped
		cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
		_ = lc.Close(cctx)
		ccancel()
	}()
	if _, err := lc.Exec(ctx, "LISTEN "+Channel); err != nil {
		close(stopped)
		return err
	}
	go func() {
		defer close(stopped)
		for {
			if _, err := lc.WaitForNotification(lctx); err != nil {
				listenErr <- err
				return
			}
			d.signal()
		}
	}()
	for {
		if err := d.drain(ctx, h); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-listenErr:
			return err
		case <-d.wake:
		}
	}
}

func (d *Drainer) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// drain handles every unprocessed, unparked delivery in arrival order; it returns an error when Postgres
// is unreachable.
func (d *Drainer) drain(ctx context.Context, h inbox.Handler) error {
	done := d.tracker.Begin("inbox drain")
	defer done()
	if err := d.markAcked(ctx); err != nil {
		return err
	}
	for {
		batch, err := d.fetch(ctx)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, dl := range batch {
			if err := d.handle(ctx, h, dl); err != nil {
				return err
			}
		}
	}
	return d.cleanup(ctx)
}

// markAcked marks the acknowledged deliveries processed; they stay acknowledged when it fails.
func (d *Drainer) markAcked(ctx context.Context) error {
	d.mu.Lock()
	ids := d.acked
	d.acked = nil
	d.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if _, err := d.pool.Exec(qctx, `UPDATE inbox SET processed_at = $2 WHERE delivery_id = ANY($1) AND processed_at IS NULL`,
		ids, d.clock.Now().UTC()); err != nil {
		d.mu.Lock()
		d.acked = append(ids, d.acked...)
		d.mu.Unlock()
		return err
	}
	return nil
}

func (d *Drainer) fetch(ctx context.Context) ([]inbox.Delivery, error) {
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rows, err := d.pool.Query(qctx, `SELECT delivery_id, event, action, received_at, payload FROM inbox
		WHERE processed_at IS NULL AND NOT (delivery_id = ANY($1)) ORDER BY seq LIMIT $2`, d.excluded(true), batchSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (inbox.Delivery, error) {
		var dl inbox.Delivery
		err := r.Scan(&dl.ID, &dl.Event, &dl.Action, &dl.ReceivedAt, &dl.Payload)
		dl.ReceivedAt = dl.ReceivedAt.UTC()
		return dl, err
	})
}

// excluded lists the unprocessed deliveries the drain leaves alone: the parked ones whose retry isn't
// due yet and the held ones; for fetching (not for the probe, which waits for them to be marked) also
// the acknowledged ones.
func (d *Drainer) excluded(fetching bool) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.parked)+len(d.held))
	for id, p := range d.parked {
		if !p.due {
			ids = append(ids, id)
		}
	}
	for id := range d.held {
		ids = append(ids, id)
	}
	if fetching {
		ids = append(ids, d.acked...)
	}
	return ids
}

func (d *Drainer) handle(ctx context.Context, h inbox.Handler, dl inbox.Delivery) error {
	done := d.tracker.Begin("inbox delivery " + dl.ID)
	defer done()
	herr := h(ctx, dl)
	if errors.Is(herr, inbox.ErrHeld) {
		d.mu.Lock()
		early := d.early[dl.ID]
		delete(d.early, dl.ID)
		if !early {
			d.held[dl.ID] = true
		}
		d.mu.Unlock()
		if !early {
			d.unpark(dl.ID)
			return nil
		}
		herr = nil // acknowledged already: processed now
	}
	if herr != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		d.park(dl.ID)
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if _, err := d.pool.Exec(qctx, `UPDATE inbox SET processed_at = $2 WHERE delivery_id = $1 AND processed_at IS NULL`,
		dl.ID, d.clock.Now().UTC()); err != nil {
		return err // handled but not marked: handled again later, which a wake tolerates
	}
	d.unpark(dl.ID)
	return nil
}

// unpark forgets a delivery's retry once its handler succeeded.
func (d *Drainer) unpark(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.parked[id]; ok {
		if p.timer != nil {
			p.timer.Stop()
		}
		delete(d.parked, id)
	}
}

// park keeps a failed delivery out of the drain until its backoff on the engine clock has passed.
func (d *Drainer) park(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.parked[id]
	if p == nil {
		p = &parked{}
		d.parked[id] = p
	}
	wait := d.backoff
	for i := 0; i < p.failures && wait < d.backoffCap; i++ {
		wait *= 2
	}
	wait = min(wait, d.backoffCap)
	p.failures++
	p.due = false
	p.timer = d.clock.AfterFunc(wait, func() {
		d.mu.Lock()
		if cur, ok := d.parked[id]; ok && cur == p {
			p.due = true
		}
		d.mu.Unlock()
		d.signal()
	})
}

// cleanup deletes deliveries processed more than the retention ago, at most once an hour (engine clock).
func (d *Drainer) cleanup(ctx context.Context) error {
	now := d.clock.Now()
	d.mu.Lock()
	due := d.lastCleanup.IsZero() || now.Sub(d.lastCleanup) >= cleanupEvery
	d.mu.Unlock()
	if !due {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if _, err := d.pool.Exec(qctx, `DELETE FROM inbox WHERE processed_at IS NOT NULL AND processed_at < $1`,
		now.Add(-d.retention).UTC()); err != nil {
		if unavailable(err) {
			return err
		}
		return nil // retried at the next drain
	}
	d.mu.Lock()
	d.lastCleanup = now
	d.mu.Unlock()
	return nil
}

// probe is busy while Run is draining and a delivery is unprocessed (deliveries waiting for a handler
// retry on the engine clock aside). A standby, or Postgres being unreachable, is idle: nothing can be
// drained until a leader runs or Postgres is back.
func (d *Drainer) probe() (bool, string) {
	d.mu.Lock()
	running := d.running
	d.mu.Unlock()
	if !running {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var n int
	err := d.pool.QueryRow(ctx, `SELECT count(*) FROM inbox WHERE processed_at IS NULL AND NOT (delivery_id = ANY($1))`,
		d.excluded(false)).Scan(&n)
	if errors.Is(err, context.DeadlineExceeded) {
		return false, "checking for unprocessed deliveries timed out"
	}
	if err != nil || n == 0 {
		return true, ""
	}
	d.signal()
	return false, fmt.Sprintf("%d unprocessed deliveries", n)
}

func unavailable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true
	}
	return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57") || strings.HasPrefix(pgErr.Code, "53")
}
