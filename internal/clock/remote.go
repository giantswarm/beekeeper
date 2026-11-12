//go:build scenario

package clock

import (
	"encoding/json"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The protocol between the world's Server and a process's remote, JSON
// lines on one Unix socket connection per process: the process says hello,
// the server answers with its offset and sends every later offset, and the
// process acknowledges each once the timers then due have fired.

// hello is a process's first line: who it is, for the world's report.
type hello struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
}

// offset is the server's line: from Seq on, the world's time is the system
// clock plus Offset nanoseconds.
type offset struct {
	Seq    int64 `json:"seq"`
	Offset int64 `json:"offset"`
}

// ack is the process's answer to an offset: Fired timers, tickers and
// sleeps were due and have fired.
type ack struct {
	Seq   int64 `json:"seq"`
	Fired int   `json:"fired"`
}

// dialTimeout bounds the connection and the server's first answer.
const dialTimeout = 5 * time.Second

// remote is a process's view of the world's clock: the system clock plus
// the world's offset, and the process's timers on it. Each timer also runs
// on the system clock, re-armed whenever the offset moves, so the world's
// time passes as the system's does between advances.
type remote struct {
	offset atomic.Int64

	mu      sync.Mutex
	waiters map[*waiter]struct{}

	conn net.Conn
}

// dial connects to the world's clock at path and reads its offset.
func dial(path string) (*remote, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, err
	}
	r := &remote{waiters: map[*waiter]struct{}{}, conn: conn}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	cmd := strings.Join(os.Args, " ")
	if len(cmd) > 200 {
		cmd = cmd[:200]
	}
	if err := json.NewEncoder(conn).Encode(hello{PID: os.Getpid(), Command: cmd}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	dec := json.NewDecoder(conn)
	var first offset
	if err := dec.Decode(&first); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	r.offset.Store(first.Offset)
	go r.follow(dec)
	return r, nil
}

// follow applies every offset the world sends and acknowledges it. When
// the world goes, the last offset stays.
func (r *remote) follow(dec *json.Decoder) {
	enc := json.NewEncoder(r.conn)
	for {
		var o offset
		if err := dec.Decode(&o); err != nil {
			_ = r.conn.Close()
			return
		}
		fired := r.set(o.Offset)
		if err := enc.Encode(ack{Seq: o.Seq, Fired: fired}); err != nil {
			_ = r.conn.Close()
			return
		}
	}
}

func (r *remote) now() time.Time {
	return time.Now().Add(time.Duration(r.offset.Load()))
}

// set moves the clock to offset and fires what is then due, earliest
// first; it returns how many fired.
func (r *remote) set(off int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offset.Store(off)
	now := r.now()
	var due []*waiter
	for w := range r.waiters {
		if w.when.After(now) {
			w.arm(now)
			continue
		}
		due = append(due, w)
	}
	slices.SortFunc(due, func(a, b *waiter) int { return a.when.Compare(b.when) })
	for _, w := range due {
		r.fire(w, now)
	}
	return len(due)
}

// waiter is a timer, ticker or sleep on the world's clock.
type waiter struct {
	r *remote
	// when is the world's time it is due at; period a ticker's period.
	when   time.Time
	period time.Duration
	// c receives the time it fires at, f is called instead (AfterFunc).
	c chan time.Time
	f func()
	// sys wakes it on the system clock; gen names sys's arming, so that a
	// stale wake does nothing.
	sys    *time.Timer
	gen    uint64
	active bool
}

// arm sets w's system timer to its deadline from now (r.mu held).
func (w *waiter) arm(now time.Time) {
	if w.sys != nil {
		w.sys.Stop()
	}
	w.gen++
	gen := w.gen
	w.sys = time.AfterFunc(w.when.Sub(now), func() { w.r.wake(w, gen) })
}

// wake is w's system timer firing.
func (r *remote) wake(w *waiter, gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !w.active || w.gen != gen {
		return
	}
	now := r.now()
	if now.Before(w.when) {
		w.arm(now)
		return
	}
	r.fire(w, now)
}

// fire delivers w at now and, for a ticker, sets its next tick; a tick a
// slow receiver has not taken yet is dropped, as time.Ticker does (r.mu
// held).
func (r *remote) fire(w *waiter, now time.Time) {
	if w.c != nil {
		select {
		case w.c <- now:
		default:
		}
	}
	if w.f != nil {
		go w.f()
	}
	if w.period > 0 {
		w.when = w.when.Add((now.Sub(w.when)/w.period + 1) * w.period)
		w.arm(now)
		return
	}
	r.stop(w)
}

// start makes w due after d from now and adds it (r.mu held).
func (r *remote) start(w *waiter, d time.Duration) {
	now := r.now()
	w.when = now.Add(d)
	w.active = true
	r.waiters[w] = struct{}{}
	w.arm(now)
}

// stop takes w out (r.mu held): it fires no more.
func (r *remote) stop(w *waiter) {
	w.active = false
	w.gen++
	if w.sys != nil {
		w.sys.Stop()
	}
	delete(r.waiters, w)
}

// drain takes a time already delivered but not received out of w's
// channel, saying whether there was one: after Stop or Reset no stale time
// is received, and, as with time.Timer since Go 1.23, a timer whose time
// was not received yet counts as stopped by them.
func (w *waiter) drain() bool {
	if w.c == nil {
		return false
	}
	select {
	case <-w.c:
		return true
	default:
		return false
	}
}

func (r *remote) newTimer(d time.Duration, f func()) *Timer {
	w := &waiter{r: r, f: f}
	if f == nil {
		w.c = make(chan time.Time, 1)
	}
	r.mu.Lock()
	r.start(w, d)
	r.mu.Unlock()
	return &Timer{C: w.c, t: remoteTimer{w}}
}

func (r *remote) newTicker(d time.Duration) *Ticker {
	w := &waiter{r: r, period: d, c: make(chan time.Time, 1)}
	r.mu.Lock()
	r.start(w, d)
	r.mu.Unlock()
	return &Ticker{C: w.c, t: remoteTicker{w}}
}

func (r *remote) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	<-r.newTimer(d, nil).C
}

type remoteTimer struct{ w *waiter }

func (t remoteTimer) Stop() bool {
	r := t.w.r
	r.mu.Lock()
	defer r.mu.Unlock()
	was := t.w.active
	r.stop(t.w)
	return t.w.drain() || was
}

func (t remoteTimer) Reset(d time.Duration) bool {
	r := t.w.r
	r.mu.Lock()
	defer r.mu.Unlock()
	was := t.w.drain() || t.w.active
	r.start(t.w, d)
	return was
}

type remoteTicker struct{ w *waiter }

func (t remoteTicker) Stop() {
	r := t.w.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stop(t.w)
	t.w.drain()
}

func (t remoteTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	r := t.w.r
	r.mu.Lock()
	defer r.mu.Unlock()
	t.w.drain()
	t.w.period = d
	r.start(t.w, d)
}
