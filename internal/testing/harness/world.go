package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/engine"
	"github.com/giantswarm/beekeeper/internal/faults"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/settle"
)

// SettleBound is how long the engine may take to become idle after a step or at a clock instant
// (contract §3.3).
const SettleBound = 5 * time.Second

// Fake is a fake edge the step tools drive (the fake GitHub, the fake JEV).
type Fake interface {
	// Do runs a step tool (github_*, jev_*) and returns its JSON-able result.
	Do(ctx context.Context, tool string, args map[string]any) (any, error)
	// MarkStep records the fake's call index at a step's start, for since_step.
	MarkStep(id string)
}

// Database is the scenario's Postgres database, which postgres_fault cuts and restores.
type Database interface {
	Down() error
	Up() error
}

// Engine is one running engine process.
type Engine interface {
	engine.Stats
	// Start runs the engine until ctx ends.
	Start(ctx context.Context) error
}

// Replayer re-runs every recorded decision (the replay tool).
type Replayer interface {
	Replay(ctx context.Context) (decisions, mismatches int, err error)
}

// Receiver is the webhook receiver on its loopback listener.
type Receiver struct {
	// URL is the listener's base URL (http://127.0.0.1:port); the receiver's path is appended.
	URL string
	// Path is the receiver's path (/github/webhook).
	Path string
	// Secret signs deliveries.
	Secret []byte
	// Registry holds the receiver's metrics.
	Registry *prometheus.Registry
	Client   *http.Client
}

// World is one scenario's running system: its engines, fakes, clock, database and step marks. Every
// component is the scenario's own except the API server and the Postgres server, which only the
// namespace and the database name separate.
type World struct {
	Scenario  *Scenario
	Namespace string
	// Clock is the scenario's fake clock, shared by both replicas and the fakes.
	Clock *clock.Fake
	// Tracker sees every asynchronous unit of the engines, the receiver and the fakes.
	Tracker *settle.Tracker
	// Kube is the harness's own uncached client in Namespace.
	Kube client.Client
	// History reads the scenario's events and decisions.
	History  history.Reader
	DB       Database
	GitHub   Fake
	JEV      Fake
	Receiver Receiver
	// Replicas are engine-0 and, in leader scenarios, engine-1.
	Replicas []*Replica
	// Config is the configuration as last applied (fixture, scenario, config_apply).
	Config Config
	// Logs holds the scenario's engine logs, printed when it fails.
	Logs *LogBuffer
	// Phases are the set-up's phases and when each ended (since the set-up began).
	Phases []Phase

	mu         sync.Mutex
	marks      map[string]*mark
	lastMark   *mark
	dbDown     bool
	deliveries int
	tracked    []statKey
	// life is the context the replicas run under; it ends when the world is torn down.
	life context.Context
	// startReplicas starts every replica in order (engine-0 first, leading); restart reuses it.
	startReplicas func(ctx context.Context) error
	// settleBound overrides SettleBound (the harness's own tests).
	settleBound time.Duration
	// stub is set by the harness's own tests, whose worlds have no engine or fakes.
	stub bool
}

// statKey is a (kind, ref) the scenario counts reconciles of; ("", "") is every reconcile.
type statKey struct{ kind, ref string }

func (k statKey) String() string { return k.kind + "/" + k.ref }

// mark is the system's counters at a step's start.
type mark struct {
	events, decisions int64
	writes            map[string]int
	reconciles        map[string]map[statKey]int
}

// Replica is one engine replica: its process, which a restart replaces, and its counters across
// processes.
type Replica struct {
	Name   string
	Faults *faults.Faults
	// Registry is the running process's metrics registry.
	Registry *prometheus.Registry
	// build makes a new process: its engine, its metrics registry and what to close once it stopped.
	build func() (Engine, *prometheus.Registry, func(), error)

	mu      sync.Mutex
	engine  Engine
	closeFn func()
	cancel  context.CancelFunc
	done    chan error
	running bool
	// baseWrites and baseReconciles are the counts of stopped processes.
	baseWrites     int
	baseReconciles map[statKey]int
}

// NewReplica returns a stopped replica that builds its processes with build.
func NewReplica(name string, f *faults.Faults, build func() (Engine, *prometheus.Registry, func(), error)) *Replica {
	return &Replica{Name: name, Faults: f, build: build, baseReconciles: map[statKey]int{}}
}

// Start builds a new process and runs it in the background.
func (r *Replica) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return fmt.Errorf("%s is running", r.Name)
	}
	eng, reg, closeFn, err := r.build()
	if err != nil {
		return fmt.Errorf("%s: %w", r.Name, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- eng.Start(ctx) }()
	r.engine, r.Registry, r.closeFn, r.cancel, r.done, r.running = eng, reg, closeFn, cancel, done, true
	return nil
}

// Stop stops the running process and waits for it, keeping its counters for keys.
func (r *Replica) Stop(keys []statKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return nil
	}
	r.cancel()
	var err error
	select {
	case err = <-r.done:
	case <-time.After(15 * time.Second):
		err = fmt.Errorf("%s did not stop within 15s", r.Name)
	}
	r.baseWrites += r.engine.Writes()
	for _, k := range keys {
		r.baseReconciles[k] += r.engine.Reconciles(k.kind, k.ref)
	}
	if r.closeFn != nil {
		r.closeFn()
	}
	r.running = false
	r.engine, r.closeFn = nil, nil
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

// Running reports whether the replica's process runs.
func (r *Replica) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Exited returns the process's error when it stopped on its own.
func (r *Replica) Exited() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false, nil
	}
	select {
	case err := <-r.done:
		r.done <- err
		return true, err
	default:
		return false, nil
	}
}

// IsLeader reports whether the running process leads.
func (r *Replica) IsLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running && r.engine.IsLeader()
}

// Engine returns the running process (nil when stopped).
func (r *Replica) Engine() Engine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.engine
}

// Writes counts object writes across the replica's processes.
func (r *Replica) Writes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.baseWrites
	if r.engine != nil {
		n += r.engine.Writes()
	}
	return n
}

// Reconciles counts reconciles of k across the replica's processes.
func (r *Replica) Reconciles(k statKey) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.baseReconciles[k]
	if r.engine != nil {
		n += r.engine.Reconciles(k.kind, k.ref)
	}
	return n
}

// Leader returns the leading replica, or nil.
func (w *World) Leader() *Replica {
	for _, r := range w.Replicas {
		if r.IsLeader() {
			return r
		}
	}
	return nil
}

// Replica returns the replica named name.
func (w *World) Replica(name string) (*Replica, error) {
	for _, r := range w.Replicas {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no replica %q", name)
}

// trackedKeys are the reconcile counters the scenario reads.
func trackedKeys(s *Scenario) []statKey {
	keys := []statKey{{}}
	seen := map[statKey]bool{{}: true}
	for _, st := range s.Steps {
		if st.Tool != "reconciles" {
			continue
		}
		for arg, kind := range map[string]string{argItem: "Item", argChange: "Change"} {
			if ref, ok := st.Args[arg].(string); ok {
				k := statKey{kind, ref}
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
	}
	return keys
}

// MarkStep records every counter at the start of step id, for since_step.
func (w *World) MarkStep(ctx context.Context, id string) error {
	if w.GitHub != nil {
		w.GitHub.MarkStep(id)
	}
	if w.JEV != nil {
		w.JEV.MarkStep(id)
	}
	m := &mark{writes: map[string]int{}, reconciles: map[string]map[statKey]int{}}
	if w.History != nil {
		ev, dec, err := w.History.MaxIDs(ctx)
		switch {
		case err == nil:
			m.events, m.decisions = ev, dec
		case w.isDBDown() && w.lastMark != nil:
			// Nothing reaches Postgres while it is down, so the last mark still holds.
			m.events, m.decisions = w.lastMark.events, w.lastMark.decisions
		default:
			return fmt.Errorf("history mark: %w", err)
		}
	}
	for _, r := range w.Replicas {
		m.writes[r.Name] = r.Writes()
		rc := map[statKey]int{}
		for _, k := range w.tracked {
			rc[k] = r.Reconciles(k)
		}
		m.reconciles[r.Name] = rc
	}
	w.mu.Lock()
	if w.marks == nil {
		w.marks = map[string]*mark{}
	}
	w.marks[id] = m
	w.lastMark = m
	w.mu.Unlock()
	return nil
}

// markOf returns the mark of since_step (nil for none).
func (w *World) markOf(args map[string]any) (*mark, error) {
	id, ok := args[argSinceStep].(string)
	if !ok {
		return nil, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	m, ok := w.marks[id]
	if !ok {
		return nil, fmt.Errorf("no mark for step %q", id)
	}
	return m, nil
}

func (w *World) isDBDown() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dbDown
}

func (w *World) setDBDown(down bool) {
	w.mu.Lock()
	w.dbDown = down
	w.mu.Unlock()
}

// SettleError is a settle that didn't finish: what was still busy, and where the goroutine dump is.
type SettleError struct {
	Bound time.Duration
	Busy  []string
	Dump  string
	Cause error
}

func (e *SettleError) Error() string {
	return fmt.Sprintf("the engine did not settle within %s; still busy: %s; goroutines: %s (%v)", e.Bound, strings.Join(e.Busy, ", "), e.Dump, e.Cause)
}

// settlePoll is how often Settle looks for timers that came due while the engine is busy.
const settlePoll = 5 * time.Millisecond

// Settle waits until the engine is idle (contract §3.3): nothing tracked is busy and no timer is due.
// Timers due at the current instant are fired as they come due, even while other work is in flight
// (a real clock would fire them at once). It waits at most SettleBound.
func (w *World) Settle(ctx context.Context) error {
	if w.Tracker == nil {
		return nil
	}
	bound := w.bound()
	deadline := time.Now().Add(bound)
	for {
		if err := w.exited(); err != nil {
			return err
		}
		fired := w.fireDue()
		left := time.Until(deadline)
		err := w.Tracker.Wait(ctx, min(max(left, 0), settlePoll))
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil && !fired && !w.due():
			return nil
		case time.Now().After(deadline):
			if err == nil {
				err = fmt.Errorf("timers keep coming due at %s", w.Clock.Now().Format(time.RFC3339))
			}
			return w.settleError(bound, err)
		}
	}
}

// due reports whether a timer is due at the current instant.
func (w *World) due() bool {
	next, ok := w.Clock.NextDeadline()
	return ok && !next.After(w.Clock.Now())
}

// fireDue runs the timers due at the current instant; it reports whether there were any.
func (w *World) fireDue() bool {
	if !w.due() {
		return false
	}
	w.Clock.Set(w.Clock.Now())
	return true
}

// bound is how long Settle waits.
func (w *World) bound() time.Duration {
	if w.settleBound > 0 {
		return w.settleBound
	}
	return SettleBound
}

// exited reports a replica process that stopped on its own.
func (w *World) exited() error {
	for _, r := range w.Replicas {
		if gone, err := r.Exited(); gone {
			return fmt.Errorf("%s exited: %v", r.Name, err)
		}
	}
	return nil
}

func (w *World) settleError(bound time.Duration, cause error) error {
	_, busy := w.Tracker.Idle()
	if next, ok := w.Clock.NextDeadline(); ok && !next.After(w.Clock.Now()) {
		busy = append(busy, "a timer due at "+next.Format(time.RFC3339))
	}
	return &SettleError{Bound: bound, Busy: busy, Dump: dumpGoroutines(w.Scenario.Name), Cause: cause}
}

// maxDumps caps the goroutine dumps one test binary writes.
const maxDumps = 20

var dumps atomic.Int32

// DumpDir is where goroutine dumps go; StartEnv empties it.
func DumpDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "beekeeper", "scenario-dumps")
}

// dumpGoroutines writes every goroutine's stack to a file and returns its path.
func dumpGoroutines(name string) string {
	if dumps.Add(1) > maxDumps {
		return fmt.Sprintf("(not written: more than %d dumps)", maxDumps)
	}
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	dir := DumpDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "(not written: " + err.Error() + ")"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-goroutines.txt", name, time.Now().UnixNano()))
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return "(not written: " + err.Error() + ")"
	}
	return path
}

// AdvanceClock moves the clock by d from due timer to due timer, settling at each instant
// (contract §3.2).
func (w *World) AdvanceClock(ctx context.Context, d time.Duration) error {
	target := w.Clock.Now().Add(d)
	for {
		if err := w.Settle(ctx); err != nil {
			return err
		}
		if !w.Clock.Step(target) {
			break
		}
	}
	w.Clock.Set(target)
	return w.Settle(ctx)
}

// gather collects the metrics of every running replica and the receiver.
func (w *World) gatherers() []prometheus.Gatherer {
	var gs []prometheus.Gatherer
	for _, r := range w.Replicas {
		r.mu.Lock()
		if r.running && r.Registry != nil {
			gs = append(gs, r.Registry)
		}
		r.mu.Unlock()
	}
	if w.Receiver.Registry != nil {
		gs = append(gs, w.Receiver.Registry)
	}
	return gs
}

// Phase is a named point of the set-up.
type Phase struct {
	Name string
	At   time.Duration
}

func (p Phase) String() string { return fmt.Sprintf("%s %s", p.Name, p.At.Round(time.Millisecond)) }

// LogBuffer keeps the last lines of a scenario's logs.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewLogBuffer keeps at most max lines.
func NewLogBuffer(max int) *LogBuffer { return &LogBuffer{max: max} }

// Add appends a line.
func (b *LogBuffer) Add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// String returns the kept lines.
func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}
