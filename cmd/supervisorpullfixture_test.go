package cmd

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// The workflow rig supplies deterministic observations while exercising planner
// and action semantics. Watcher integration tests restore the production reader.
func fixtureLoopObservation(l *loop.Loop, ctx context.Context, ref string, changed time.Time, step string) (loop.LoopPullObservation, error) {
	if step == "" {
		p, err := loop.ReadLoopPull(ctx, ref)
		return loop.LoopPullObservation{Pull: p, At: asApp(l.Host).timeNow()}, err
	}
	p, err := fixtureObservePull(l, ctx, ref, changed, step)
	return loop.LoopPullObservation{Pull: p, At: l.Pulls[ref].At, Changed: changed}, err
}

func fixtureObservePull(l *loop.Loop, ctx context.Context, ref string, changed time.Time, step string) (superloop.Pull, error) {
	// Bound the entire observation, including shared admission-lock waits
	// and budget probes, rather than only the eventual gh pr view.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return superloop.Pull{}, err
	}
	if seen, ok := l.Pulls[ref]; ok && seen.Changed.Equal(changed) && asApp(l.Host).timeNow().Sub(seen.At) < loopPullEvery {
		return seen.Pull, nil
	}
	var p superloop.Pull
	err := asApp(l.Host).loopGitHub(ctx, step, func() (err error) {
		p, err = loop.ReadLoopPull(ctx, ref)
		return err
	})
	if err != nil {
		return p, err
	}
	if l.Pulls == nil {
		l.Pulls = map[string]loop.LoopPullObservation{}
	}
	at := asApp(l.Host).timeNow()
	// A running loop's clock stays at the start of its pass: this read may be
	// minutes later, and the next pass must not take it for an old one.
	if asApp(l.Host).loopNow != nil {
		at = time.Now()
	}
	l.Pulls[ref] = loop.LoopPullObservation{Pull: p, At: at, Changed: changed}
	return p, nil
}

func fixtureLoopSweep(l *loop.Loop, ctx context.Context, in superloop.Input) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	now := asApp(l.Host).timeNow()
	if l.OpenPullsAt.IsZero() || now.Sub(l.OpenPullsAt) >= loop.SweepListEvery {
		authors := func() ([]string, error) {
			if len(in.Desk.Authors) > 0 {
				return in.Desk.Authors, nil
			}
			if l.Login == "" {
				login, err := loop.ReadDeskLogin(ctx)
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
		err := asApp(l.Host).loopGitHub(ctx, "pull discovery", func() (err error) {
			open, err = loop.ReadOpenPulls(ctx, authors, asApp(l.Host).cfg.GitHub.AllowedOrgs)
			return err
		})
		l.SweepIncomplete = err != nil || len(open) >= loop.SweepListLimit
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
