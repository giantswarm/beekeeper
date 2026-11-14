// Package historypg is the Postgres history (plan §5, contract §1.5): a Writer that never blocks the
// engine, buffering rows in memory and writing them in order on its own goroutine, and the Reader the
// harness, replay and the engine's startup use.
package historypg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// Defaults of Options.
const (
	DefaultMaxBuffered   = 10000
	DefaultBatchSize     = 500
	DefaultRetryInterval = time.Second
	DefaultQueryTimeout  = 10 * time.Second
)

// Options tune a Writer. The zero value is production's.
type Options struct {
	// Tracker, when set, gets a probe that is busy while rows are buffered and Postgres is reachable.
	// Rows buffered while Postgres is unreachable count as idle: they wait for it to come back.
	Tracker *settle.Tracker
	// ProbeName names the probe (default "history-<n>", unique per writer).
	ProbeName string
	// MaxBuffered is how many rows are kept while they can't be written; more are dropped and counted
	// (default 10,000).
	MaxBuffered int
	// BatchSize is the most rows written in one transaction (default 500).
	BatchSize int
	// RetryInterval is how often (real time) a write is retried while Postgres is unreachable (default
	// 1s). Like kube API retries this is infrastructure, not engine time.
	RetryInterval time.Duration
	// QueryTimeout bounds each write and the startup read of the decision keys (default 10s).
	QueryTimeout time.Duration
}

// op is one buffered row (or the update of one).
type op struct {
	sql  string
	args []any
	what string
}

// Writer implements history.Writer and history.Reader on Postgres.
type Writer struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	log   logr.Logger
	opts  Options

	mu          sync.Mutex
	queue       []op
	keys        map[history.Key]struct{}
	keysLoaded  bool
	reachable   bool
	loggedDown  bool
	writing     bool
	dropped     int
	lastDropLog int

	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeCtx  context.Context
}

var (
	_ history.Writer = (*Writer)(nil)
	_ history.Reader = (*Writer)(nil)
)

var probes atomic.Int64

// NewWriter returns a Writer on pool and starts its flushing goroutine (stop it with Close). It reads the
// recorded decision keys once, so Decide knows what was recorded before; if Postgres can't be read now,
// the keys are read before the first write instead (a key Decide didn't know is then not inserted twice:
// the decisions table is unique on it). clk stamps rows that come without a time.
func NewWriter(pool *pgxpool.Pool, clk clock.Clock, logger logr.Logger, opts Options) *Writer {
	if opts.MaxBuffered <= 0 {
		opts.MaxBuffered = DefaultMaxBuffered
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = DefaultRetryInterval
	}
	if opts.QueryTimeout <= 0 {
		opts.QueryTimeout = DefaultQueryTimeout
	}
	if opts.ProbeName == "" {
		opts.ProbeName = fmt.Sprintf("history-%d", probes.Add(1))
	}
	w := &Writer{
		pool:      pool,
		clock:     clk,
		log:       logger.WithName("history"),
		opts:      opts,
		keys:      map[history.Key]struct{}{},
		reachable: true,
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.QueryTimeout)
	if err := w.LoadKeys(ctx); err != nil {
		w.log.Info("decision keys not read; reading them before the first write", "error", err.Error())
	}
	cancel()
	opts.Tracker.AddProbe(opts.ProbeName, w.probe)
	go w.loop()
	return w
}

// LoadKeys (re)reads the recorded decision keys into the set Decide consults. The engine calls it when it
// gains leadership, so keys another replica recorded are known.
func (w *Writer) LoadKeys(ctx context.Context) error {
	keys, err := w.DecisionKeys(ctx)
	if err != nil {
		return err
	}
	w.mu.Lock()
	for _, k := range keys {
		w.keys[k] = struct{}{}
	}
	w.keysLoaded = true
	w.mu.Unlock()
	return nil
}

// Close writes what it can within ctx, then stops the writer and removes its probe. Rows it couldn't
// write are lost (and counted as dropped).
func (w *Writer) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.closeCtx = ctx
		close(w.stop)
	})
	<-w.done
	w.opts.Tracker.RemoveProbe(w.opts.ProbeName)
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.queue); n > 0 {
		w.dropped += n
		w.queue = nil
		return fmt.Errorf("history: %d rows not written", n)
	}
	return nil
}

// Append buffers events.
func (w *Writer) Append(events ...history.Event) {
	ops := make([]op, 0, len(events))
	for _, e := range events {
		at := e.At
		if at.IsZero() {
			at = w.clock.Now()
		}
		ops = append(ops, op{
			sql:  `INSERT INTO events (at, team, item, change, type, source, data) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			args: []any{at.UTC(), e.Team, e.Item, e.Change, e.Type, e.Source, jsonObject(e.Data)},
			what: "event " + e.Type,
		})
	}
	w.enqueue(ops...)
}

// Decide buffers a decision unless its key was recorded before (then it reports false).
func (w *Writer) Decide(d history.Decision) bool {
	k := d.Key()
	at := d.At
	if at.IsZero() {
		at = w.clock.Now()
	}
	o := op{
		sql: `INSERT INTO decisions (at, point, subject, other, inputs, inputs_hash, rule, jev, jev_confidence, jev_error, chosen, mode, outcome, team)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT (point, subject, inputs_hash) DO NOTHING`,
		args: []any{at.UTC(), d.Point, d.Subject, d.Other, jsonObject(d.Inputs), d.InputsHash, d.Rule, d.JEV, d.JEVConfidence, d.JEVError, d.Chosen, d.Mode, d.Outcome, d.Team},
		what: "decision " + d.Point,
	}
	w.mu.Lock()
	if _, seen := w.keys[k]; seen {
		w.mu.Unlock()
		return false
	}
	w.keys[k] = struct{}{}
	w.enqueueLocked(o)
	w.mu.Unlock()
	w.signal()
	return true
}

// Answer buffers JEV's answer and the outcome for a recorded decision; it is written after the decision.
func (w *Writer) Answer(k history.Key, jev string, confidence float64, jevErr string, outcome string) {
	w.enqueue(op{
		sql:  `UPDATE decisions SET jev = $4, jev_confidence = $5, jev_error = $6, outcome = $7 WHERE point = $1 AND subject = $2 AND inputs_hash = $3`,
		args: []any{k.Point, k.Subject, k.InputsHash, jev, confidence, jevErr, outcome},
		what: "answer " + k.Point,
	})
}

// AddDuration buffers an observed duration.
func (w *Writer) AddDuration(kind, key string, d time.Duration, at time.Time) {
	if at.IsZero() {
		at = w.clock.Now()
	}
	w.enqueue(op{
		sql:  `INSERT INTO durations (kind, key, value, at) VALUES ($1, $2, $3, $4)`,
		args: []any{kind, key, d, at.UTC()},
		what: "duration " + kind,
	})
}

// Pending is the number of buffered rows not yet written.
func (w *Writer) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}

// Dropped is the number of rows dropped: while the buffer was full, or refused by Postgres.
func (w *Writer) Dropped() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

func (w *Writer) enqueue(ops ...op) {
	if len(ops) == 0 {
		return
	}
	w.mu.Lock()
	w.enqueueLocked(ops...)
	w.mu.Unlock()
	w.signal()
}

func (w *Writer) enqueueLocked(ops ...op) {
	room := w.opts.MaxBuffered - len(w.queue)
	if room < 0 {
		room = 0
	}
	if len(ops) > room {
		w.dropped += len(ops) - room
		if w.dropped-w.lastDropLog >= 1000 || w.lastDropLog == 0 {
			w.log.Info("history buffer full, dropping rows", "dropped", w.dropped, "buffered", len(w.queue))
			w.lastDropLog = w.dropped
		}
		ops = ops[:room]
	}
	w.queue = append(w.queue, ops...)
}

func (w *Writer) signal() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// probe is busy while rows wait and Postgres is reachable (or is being written to). When the last write
// found Postgres unreachable it checks once whether it is back, so the harness waits for the flush right
// after postgres_fault up.
func (w *Writer) probe() (bool, string) {
	w.mu.Lock()
	n, reachable, writing := len(w.queue), w.reachable, w.writing
	w.mu.Unlock()
	if n == 0 {
		return true, ""
	}
	if writing || reachable {
		return false, fmt.Sprintf("%d history rows to write", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.pool.Ping(ctx); err != nil {
		return true, ""
	}
	w.mu.Lock()
	w.reachable = true
	w.mu.Unlock()
	w.signal()
	return false, fmt.Sprintf("%d history rows to write (Postgres is back)", n)
}

func (w *Writer) loop() {
	defer close(w.done)
	var retry *time.Timer
	var retryC <-chan time.Time
	for {
		select {
		case <-w.stop:
			if retry != nil {
				retry.Stop()
			}
			_ = w.flush(w.closeCtx)
			return
		case <-w.kick:
			// While Postgres is unreachable only the retry timer (or the probe finding it back) writes.
			if retryC != nil && !w.isReachable() {
				continue
			}
		case <-retryC:
			retryC = nil
		}
		if err := w.flush(context.Background()); err != nil && retryC == nil {
			if retry == nil {
				retry = time.NewTimer(w.opts.RetryInterval)
			} else {
				retry.Reset(w.opts.RetryInterval)
			}
			retryC = retry.C
		}
	}
}

func (w *Writer) isReachable() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reachable
}

// flush writes buffered rows in order until none are left; it returns an error when Postgres is
// unreachable (the rows stay buffered).
func (w *Writer) flush(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.mu.Lock()
		n := min(len(w.queue), w.opts.BatchSize)
		batch := append([]op(nil), w.queue[:n]...)
		loaded := w.keysLoaded
		w.writing = n > 0
		w.mu.Unlock()
		if n == 0 {
			return nil
		}
		if !loaded {
			lctx, cancel := context.WithTimeout(ctx, w.opts.QueryTimeout)
			err := w.LoadKeys(lctx)
			cancel()
			if err != nil {
				w.unreachable(err)
				return err
			}
		}
		written, refused, err := w.write(ctx, batch)
		w.mu.Lock()
		w.queue = w.queue[written:]
		w.dropped += refused
		w.writing = false
		back := err == nil && w.loggedDown
		if err == nil {
			w.reachable = true
			w.loggedDown = false
		}
		left := len(w.queue)
		w.mu.Unlock()
		if err != nil {
			w.unreachable(err)
			return err
		}
		if back {
			w.log.Info("Postgres reachable again, writing buffered history", "buffered", left)
		}
		w.opts.Tracker.Poke()
	}
}

func (w *Writer) unreachable(err error) {
	w.mu.Lock()
	logIt := !w.loggedDown
	w.loggedDown = true
	w.reachable = false
	w.writing = false
	n := len(w.queue)
	w.mu.Unlock()
	if logIt {
		w.log.Info("Postgres unreachable, buffering history", "buffered", n, "error", err.Error())
	}
	w.opts.Tracker.Poke()
}

// write writes ops in one transaction. If Postgres refuses a row (a data error, not unavailability), the
// rows are written one at a time and the refused ones dropped. It reports how many ops are done (written
// or refused) from the front, how many of those were refused, and an error when Postgres is unreachable.
func (w *Writer) write(parent context.Context, ops []op) (done, refused int, err error) {
	ctx, cancel := context.WithTimeout(parent, w.opts.QueryTimeout)
	defer cancel()
	err = pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, o := range ops {
			b.Queue(o.sql, o.args...)
		}
		return tx.SendBatch(ctx, b).Close()
	})
	if err == nil {
		return len(ops), 0, nil
	}
	if unavailable(err) {
		return 0, 0, err
	}
	for i, o := range ops {
		ctx, cancel := context.WithTimeout(parent, w.opts.QueryTimeout)
		_, err := w.pool.Exec(ctx, o.sql, o.args...)
		cancel()
		if err != nil {
			if unavailable(err) {
				return i, refused, err
			}
			refused++
			w.log.Error(err, "Postgres refused a history row; dropped", "row", o.what)
		}
	}
	return len(ops), refused, nil
}

// unavailable reports whether err means Postgres can't be reached (retry later) rather than that it
// refused the statement (which no retry fixes).
func unavailable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true // network, timeout, closed pool
	}
	switch {
	case strings.HasPrefix(pgErr.Code, "08"), // connection exception
		strings.HasPrefix(pgErr.Code, "53"),          // insufficient resources
		strings.HasPrefix(pgErr.Code, "57"),          // operator intervention (shutdown, cannot connect now)
		pgErr.Code == "40001", pgErr.Code == "40P01", // serialization failure, deadlock
		pgErr.Code == "25006": // read-only transaction (failover in progress)
		return true
	}
	return false
}

// jsonObject marshals m at enqueue time (so the caller may change m afterwards); nil is {}.
func jsonObject(m map[string]any) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"marshalError": err.Error()})
	}
	return b
}
