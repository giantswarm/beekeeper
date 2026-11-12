package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	loopLifecycleFile       = "supervisor-loop-lifecycle.json"
	loopPhaseStartup        = "startup"
	loopPhaseBetween        = "between passes"
	loopPhaseObserve        = "observe workers and pull requests"
	loopPhaseRecover        = "recover quota and parks"
	loopPhaseContinueParks  = "continue settled parks"
	loopPhaseRecoverParks   = "recover ended parks"
	loopPhaseRecoverReviews = "recover review requests"
	loopPhaseEndReviews     = "end review workers"
	loopPhaseTidy           = "tidy and sync claims"
)

const loopPhaseStartupHistory = "read loop startup history"

// loopFailureAbandoned is the lifecycle failure of a loop that exited at its
// shutdown deadline with a pass still running.
const loopFailureAbandoned = "pass abandoned at shutdown deadline"

// A SIGTERM cancels the pass, but work that does not watch its context (a
// state lock wait of minutes, a slow read) would hold the exit past the
// service manager's stop timeout and end in SIGKILL. After loopShutdownGrace
// the loop abandons that work: it quiesces its state writes, waiting at most
// loopShutdownLockWait for the state lock and a few seconds for a commit, and
// exits. Grace plus waits stay well under systemd's default 90 s.
var (
	loopShutdownGrace    = 10 * time.Second
	loopShutdownLockWait = 2 * time.Second
	loopShutdownExit     = os.Exit
)

// boundLoopShutdown exits the process loopShutdownGrace after ctx ends unless
// done closes first, the loop having returned. It writes the lifecycle,
// PID and version records the loop's own deferred cleanup would have written,
// reading the lifecycle from its file, never from the running loop's memory.
// A store that cannot quiesce its writes is exited without: an unfinished
// write is recovered by the next reader, as after a SIGKILL, only sooner.
func boundLoopShutdown(ctx context.Context, done <-chan struct{}, store state.Store, out io.Writer) {
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	t := clk.NewTimer(loopShutdownGrace)
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
		if release, err := q.Quiesce(loopShutdownLockWait); err == nil {
			defer release()
		}
	}
	var life loopLifecycle
	if found, err := store.ReadFile(loopLifecycleFile, &life); err == nil && found {
		life.At, life.Exited, life.Code, life.Failure = clk.Now().UTC(), true, 0, loopFailureAbandoned
		_ = store.WriteFile(loopLifecycleFile, life)
	}
	_ = store.WriteFile(loopPIDFile, 0)
	_ = store.WriteFile(loopVersionFile, loopVersionRecord{})
	_, _ = fmt.Fprintf(out, "SUPERVISOR LOOP stopped: its pass still ran %s after the stop signal; abandoned it\n", loopShutdownGrace)
	loopShutdownExit(0)
}

// Phase transitions are bounded progress evidence. Only the loop holding
// the exclusive loop lock writes it; failed contenders cannot overwrite it.
// No command output, paths, configuration or lock-holder identities are saved.
type loopLifecycle struct {
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	At      time.Time `json:"at"`
	Phase   string    `json:"phase"`
	Exited  bool      `json:"exited"`
	Code    int       `json:"code"`
	Failure string    `json:"failure,omitempty"`
}

var loopLifecycleNow = clk.Now

func (l *supervisorLoop) lifecyclePhase(phase string) {
	if l.lifecycle == nil {
		return
	}
	l.lifecycle.Phase, l.lifecycle.At = phase, loopLifecycleNow().UTC()
	// Diagnostics must not change loop admission or turn a successful pass
	// into a failure. Recovery reports missing evidence when this write fails.
	_ = l.a.store.WriteFile(loopLifecycleFile, l.lifecycle)
	if !l.dryRun && phase != loopPhaseBetween {
		// Retained controllers understand this observation, even when they do
		// not understand lifecycle records. No goroutine refreshes a stuck phase.
		_ = l.refreshThroughputHeartbeat(l.lifecycle.At)
	}
}

func loopFailureKind(err error) string {
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

// candidateLoop reads the lifecycle of the candidate's own loop: life is nil
// unless that loop started after the install, and otherAlive says a loop of
// another version still runs, the one that hands over at its next quiet pass.
func (c *recoveryController) candidateLoop(jVersion string, installed time.Time) (life *loopLifecycle, otherAlive bool, err error) {
	var l loopLifecycle
	found, err := c.store.ReadFile(loopLifecycleFile, &l)
	if err != nil {
		return nil, false, err
	}
	if found && l.Version == normalizeLoopVersion(jVersion) && !l.Started.Before(installed) && !l.At.Before(l.Started) && !l.At.After(c.now()) {
		return &l, false, nil
	}
	var incumbent loopVersionRecord
	if ok, readErr := c.store.ReadFile(loopVersionFile, &incumbent); readErr == nil && ok && incumbent.Version != "" && normalizeLoopVersion(incumbent.Version) != normalizeLoopVersion(jVersion) && proc.Alive(incumbent.PID) {
		otherAlive = true
	}
	return nil, otherAlive, nil
}

// Missing-heartbeat detail is attached only to matching, post-install evidence.
// A stalled pass still fails at its deadline; no startup marker can qualify a
// candidate for promotion.
func (c *recoveryController) loopSilence(jVersion string, installed time.Time) string {
	life, otherAlive, err := c.candidateLoop(jVersion, installed)
	switch {
	case err != nil:
		return "candidate lifecycle unreadable"
	case life == nil && otherAlive:
		return "no matching candidate loop startup recorded after install; another-version loop is still alive"
	case life == nil:
		return "no matching candidate loop startup recorded after install"
	case life.Exited:
		return fmt.Sprintf("candidate loop exited during %s (exit %d; %s)", life.Phase, life.Code, life.Failure)
	}
	return fmt.Sprintf("candidate loop last reached %s; no health checkpoint for %s", life.Phase, c.now().Sub(life.At).Round(time.Second))
}

// eventHistoryFilter checkpoints only when the reader actually decodes another
// event, including events the caller discards. A blocked read or parser cannot
// keep itself alive. Side-file writes do not acquire the event reader's mutex.
func (l *supervisorLoop) eventHistoryFilter(keep func(state.Event) bool) func(state.Event) bool {
	return eventProgressFilter(keep, func() { l.lifecyclePhase("read event history") }, clk.Now)
}

func eventProgressFilter(keep func(state.Event) bool, checkpoint func(), now func() time.Time) func(state.Event) bool {
	last := now()
	return func(e state.Event) bool {
		if at := now(); at.Sub(last) >= throughputCadence {
			checkpoint()
			last = at
		}
		return keep(e)
	}
}
