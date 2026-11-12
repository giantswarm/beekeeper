package cmd

import (
	"fmt"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

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
