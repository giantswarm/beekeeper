package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// The workflow rig supplies deterministic observations while exercising planner
// and action semantics. Watcher integration tests restore the production reader.
func fixtureLoopObservation(l *Loop, ctx context.Context, ref string, changed time.Time, step string) (LoopPullObservation, error) {
	if step == "" {
		p, err := ReadLoopPull(ctx, ref)
		return LoopPullObservation{Pull: p, At: l.Host.timeNow()}, err
	}
	p, err := fixtureObservePull(l, ctx, ref, changed, step)
	return LoopPullObservation{Pull: p, At: l.Pulls[ref].At, Changed: changed}, err
}

func fixtureObservePull(l *Loop, ctx context.Context, ref string, changed time.Time, step string) (superloop.Pull, error) {
	// Bound the entire observation, including shared admission-lock waits
	// and budget probes, rather than only the eventual gh pr view.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return superloop.Pull{}, err
	}
	if seen, ok := l.Pulls[ref]; ok && seen.Changed.Equal(changed) && l.Host.timeNow().Sub(seen.At) < loopPullEvery {
		return seen.Pull, nil
	}
	var p superloop.Pull
	err := l.Host.loopGitHub(ctx, step, func() (err error) {
		p, err = ReadLoopPull(ctx, ref)
		return err
	})
	if err != nil {
		return p, err
	}
	if l.Pulls == nil {
		l.Pulls = map[string]LoopPullObservation{}
	}
	at := l.Host.timeNow()
	// A running loop's clock stays at the start of its pass: this read may be
	// minutes later, and the next pass must not take it for an old one.
	if l.Host.loopNow != nil {
		at = time.Now()
	}
	l.Pulls[ref] = LoopPullObservation{Pull: p, At: at, Changed: changed}
	return p, nil
}

func fixtureLoopSweep(l *Loop, ctx context.Context, in superloop.Input) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	now := l.Host.timeNow()
	if l.OpenPullsAt.IsZero() || now.Sub(l.OpenPullsAt) >= SweepListEvery {
		authors := func() ([]string, error) {
			if len(in.Desk.Authors) > 0 {
				return in.Desk.Authors, nil
			}
			if l.Login == "" {
				login, err := ReadDeskLogin(ctx)
				if err != nil {
					return nil, err
				}
				l.Login = login
			}
			return []string{l.Login}, nil
		}
		// Discovery is delivery work: inventory and orphan detail scans
		// must not spend its allowance and hide approved heads. The
		// listing interval still bounds searches; shared floors still apply.
		var open []string
		err := l.Host.loopGitHub(ctx, "pull discovery", func() (err error) {
			open, err = ReadOpenPulls(ctx, authors, l.Host.cfg.GitHub.AllowedOrgs)
			return err
		})
		l.SweepIncomplete = err != nil || len(open) >= SweepListLimit
		if err != nil {
			// The last list stands. Retry next pass so a reset does not
			// leave discovery waiting another full listing interval.
			l.SayOnce("sweep list", "SUPERVISOR SWEEP cannot list the desk's open pull requests (%v): the last list stands", err)
		} else {
			l.ClearSaid("sweep list")
			l.OpenPulls = open
		}
		if err == nil {
			l.OpenPullsAt = now
		}
	}
}
