package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	LoopLifecycleFile       = "supervisor-loop-lifecycle.json"
	LoopPhaseStartup        = "startup"
	LoopPhaseBetween        = "between passes"
	LoopPhaseObserve        = "observe workers and pull requests"
	LoopPhaseRecover        = "recover quota and parks"
	loopPhaseContinueParks  = "continue settled parks"
	loopPhaseRecoverParks   = "recover ended parks"
	loopPhaseRecoverReviews = "recover review requests"
	loopPhaseEndReviews     = "end review workers"
	loopPhaseTidy           = "tidy and sync claims"
)

const LoopPhaseStartupHistory = "read loop startup history"

// LoopFailureAbandoned is the lifecycle failure of a loop that exited at its
// shutdown deadline with a pass still running.
const LoopFailureAbandoned = "pass abandoned at shutdown deadline"

// A SIGTERM cancels the pass, but work that does not watch its context (a
// state lock wait of minutes, a slow read) would hold the exit past the
// service manager's stop timeout and end in SIGKILL. After LoopShutdownGrace
// the loop abandons that work: it quiesces its state writes, waiting at most
// LoopShutdownLockWait for the state lock and a few seconds for a commit, and
// exits. Grace plus waits stay well under systemd's default 90 s.
var (
	LoopShutdownGrace    = 10 * time.Second
	LoopShutdownLockWait = 2 * time.Second
	LoopShutdownExit     = os.Exit
)

// BoundLoopShutdown exits the process LoopShutdownGrace after ctx ends unless
// done closes first, the loop having returned. It writes the lifecycle,
// PID and version records the loop's own deferred cleanup would have written,
// reading the lifecycle from its file, never from the running loop's memory.
// A store that cannot quiesce its writes is exited without: an unfinished
// write is recovered by the next reader, as after a SIGKILL, only sooner.
func BoundLoopShutdown(ctx context.Context, done <-chan struct{}, store state.Store, out io.Writer) {
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	t := clk.NewTimer(LoopShutdownGrace)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	if q, ok := store.(interface {
		Quiesce(time.Duration) (func(), error)
	}); ok {
		// Not released before the exit: no write of the abandoned pass may
		// start in between. A replaced exit (tests) releases it on return.
		if release, err := q.Quiesce(LoopShutdownLockWait); err == nil {
			defer release()
		}
	}
	var life LoopLifecycle
	if found, err := store.ReadFile(LoopLifecycleFile, &life); err == nil && found {
		life.At, life.Exited, life.Code, life.Failure = clk.Now().UTC(), true, 0, LoopFailureAbandoned
		_ = store.WriteFile(LoopLifecycleFile, life)
	}
	_ = store.WriteFile(LoopPIDFile, 0)
	_ = store.WriteFile(LoopVersionFile, LoopVersionRecord{})
	_, _ = fmt.Fprintf(out, "SUPERVISOR LOOP stopped: its pass still ran %s after the stop signal; abandoned it\n", LoopShutdownGrace)
	LoopShutdownExit(0)
}

// Phase transitions are bounded progress evidence. Only the loop holding
// the exclusive loop lock writes it; failed contenders cannot overwrite it.
// No command output, paths, configuration or lock-holder identities are saved.
type LoopLifecycle struct {
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	At      time.Time `json:"at"`
	Phase   string    `json:"phase"`
	Exited  bool      `json:"exited"`
	Code    int       `json:"code"`
	Failure string    `json:"failure,omitempty"`
}

var LoopLifecycleNow = clk.Now

func (l *Loop) LifecyclePhase(phase string) {
	if l.Lifecycle == nil {
		return
	}
	l.Lifecycle.Phase, l.Lifecycle.At = phase, LoopLifecycleNow().UTC()
	// Diagnostics must not change loop admission or turn a successful pass
	// into a failure. Recovery reports missing evidence when this write fails.
	_ = l.Host.Store().WriteFile(LoopLifecycleFile, l.Lifecycle)
	if !l.DryRun && phase != LoopPhaseBetween {
		// Retained controllers understand this observation, even when they do
		// not understand lifecycle records. No goroutine refreshes a stuck phase.
		_ = l.refreshThroughputHeartbeat(l.Lifecycle.At)
	}
}

func LoopFailureKind(err error) string {
	var busy *state.LockBusyError
	var path *os.PathError
	var stale *state.StaleWriterError
	var syntax *json.SyntaxError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, state.ErrRecoveryWriterFenced):
		return "state writes fenced after binary recovery"
	case errors.As(err, &stale):
		return "state writer incompatible"
	case errors.As(err, &syntax):
		return "invalid JSON state"
	case errors.As(err, &busy):
		return "state lock busy"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.As(err, &path):
		return "filesystem failure"
	default:
		return "loop error (see candidate service journal)"
	}
}

// eventHistoryFilter checkpoints only when the reader actually decodes another
// event, including events the caller discards. A blocked read or parser cannot
// keep itself alive. Side-file writes do not acquire the event reader's mutex.
func (l *Loop) eventHistoryFilter(keep func(state.Event) bool) func(state.Event) bool {
	return EventProgressFilter(keep, func() { l.LifecyclePhase("read event history") }, clk.Now)
}

func EventProgressFilter(keep func(state.Event) bool, checkpoint func(), now func() time.Time) func(state.Event) bool {
	last := now()
	return func(e state.Event) bool {
		if at := now(); at.Sub(last) >= throughputCadence {
			checkpoint()
			last = at
		}
		return keep(e)
	}
}
