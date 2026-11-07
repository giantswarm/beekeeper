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

// Missing-heartbeat detail is attached only to matching, post-install evidence.
// A stalled pass still fails at the original deadline; no startup marker can
// qualify a candidate for promotion.
func (c *recoveryController) loopSilence(jVersion string, installed time.Time) string {
	var life loopLifecycle
	found, err := c.store.ReadFile(loopLifecycleFile, &life)
	if err != nil {
		return "candidate lifecycle unreadable"
	}
	if !found || life.Version != normalizeLoopVersion(jVersion) || life.Started.Before(installed) || life.At.Before(life.Started) || life.At.After(c.now()) {
		var incumbent loopVersionRecord
		if ok, readErr := c.store.ReadFile(loopVersionFile, &incumbent); readErr == nil && ok && incumbent.Version != "" && normalizeLoopVersion(incumbent.Version) != normalizeLoopVersion(jVersion) && proc.Alive(incumbent.PID) {
			return "no matching candidate loop startup recorded after install; another-version loop is still alive"
		}
		return "no matching candidate loop startup recorded after install"
	}
	if life.Exited {
		return fmt.Sprintf("candidate loop exited during %s (exit %d; %s)", life.Phase, life.Code, life.Failure)
	}
	return fmt.Sprintf("candidate loop last reached %s; no completed-pass heartbeat for %s", life.Phase, c.now().Sub(life.At).Round(time.Second))
}
