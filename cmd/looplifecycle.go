package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const loopLifecycleFile = "supervisor-loop-lifecycle.json"

// Lifecycle is diagnostic evidence, never a heartbeat. Only the loop holding
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

func (l *supervisorLoop) lifecyclePhase(phase string) {
	if l.lifecycle == nil {
		return
	}
	l.lifecycle.Phase, l.lifecycle.At = phase, time.Now().UTC()
	// Diagnostics must not change loop admission or turn a successful pass
	// into a failure. Recovery reports missing evidence when this write fails.
	_ = l.a.store.WriteFile(loopLifecycleFile, l.lifecycle)
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
