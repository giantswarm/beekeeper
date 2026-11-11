package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// The workflow rig supplies deterministic observations while exercising planner
// and action semantics. Watcher integration tests restore the production reader.
func fixtureLoopObservation(l *supervisorLoop, ctx context.Context, ref string, changed time.Time, step string) (loopPullObservation, error) {
	if step == "" {
		p, err := readLoopPull(ctx, ref)
		return loopPullObservation{pull: p, at: l.a.timeNow()}, err
	}
	p, err := l.fixtureObservePull(ctx, ref, changed, step)
	return loopPullObservation{pull: p, at: l.pulls[ref].at, changed: changed}, err
}

func (l *supervisorLoop) fixtureObservePull(ctx context.Context, ref string, changed time.Time, step string) (superloop.Pull, error) {
	// Bound the entire observation, including shared admission-lock waits
	// and budget probes, rather than only the eventual gh pr view.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return superloop.Pull{}, err
	}
	if seen, ok := l.pulls[ref]; ok && seen.changed.Equal(changed) && l.a.timeNow().Sub(seen.at) < loopPullEvery {
		return seen.pull, nil
	}
	var p superloop.Pull
	err := l.a.loopGitHub(ctx, step, func() (err error) {
		p, err = readLoopPull(ctx, ref)
		return err
	})
	if err != nil {
		return p, err
	}
	if l.pulls == nil {
		l.pulls = map[string]loopPullObservation{}
	}
	at := l.a.timeNow()
	// A running loop's clock stays at the start of its pass: this read may be
	// minutes later, and the next pass must not take it for an old one.
	if l.a.loopNow != nil {
		at = time.Now()
	}
	l.pulls[ref] = loopPullObservation{pull: p, at: at, changed: changed}
	return p, nil
}

func fixtureLoopSweep(l *supervisorLoop, ctx context.Context, in superloop.Input) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	now := l.a.timeNow()
	if l.openPullsAt.IsZero() || now.Sub(l.openPullsAt) >= sweepListEvery {
		authors := func() ([]string, error) {
			if len(in.Desk.Authors) > 0 {
				return in.Desk.Authors, nil
			}
			if l.login == "" {
				login, err := readDeskLogin(ctx)
				if err != nil {
					return nil, err
				}
				l.login = login
			}
			return []string{l.login}, nil
		}
		// Discovery is delivery work: inventory and orphan detail scans
		// must not spend its allowance and hide approved heads. The
		// listing interval still bounds searches; shared floors still apply.
		var open []string
		err := l.a.loopGitHub(ctx, "pull discovery", func() (err error) {
			open, err = readOpenPulls(ctx, authors, l.a.cfg.GitHub.AllowedOrgs)
			return err
		})
		l.sweepIncomplete = err != nil || len(open) >= sweepListLimit
		if err != nil {
			// The last list stands. Retry next pass so a reset does not
			// leave discovery waiting another full listing interval.
			l.sayOnce("sweep list", "SUPERVISOR SWEEP cannot list the desk's open pull requests (%v): the last list stands", err)
		} else {
			l.clearSaid("sweep list")
			l.openPulls = open
		}
		if err == nil {
			l.openPullsAt = now
		}
	}
}
