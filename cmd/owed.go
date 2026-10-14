package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// owedRetry is how long a claimed hand-over of an owed outcome waits before
// the next try; owedAsk how long a stopped merge's outcome waits for
// GitHub's answer before its owner hears it without one; owedFor how long
// an outcome stays owed at all; owedBusy how long the gate and merge-child
// that owe it may still be handing it over themselves.
const (
	owedRetry = time.Minute
	owedAsk   = 30 * time.Minute
	owedFor   = 24 * time.Hour
	owedBusy  = 2 * ownerWait
)

// owed is r's outcome as this merge-child owes it to its owner.
func (r childResult) owed(now time.Time) state.Owed {
	o := state.Owed{Owner: r.spec.Owner, Line: r.outcome(), Gate: r.spec.Gate, Child: os.Getpid(),
		Heard: heardFile(r.base, r.spec.Gate), Since: now.UTC(), Tried: now.UTC()}
	if repo, pr, ok := r.stoppedMerge(); ok {
		o.Ask, o.Repo, o.PR = true, repo, pr
		o.Command, o.Stop = r.command(), fmt.Sprintf("%s (exit %d)", signalName(r.stop), r.rc)
	}
	return o
}

// stoppedMerge is the pull request of a merge whose merge-child was stopped
// before devctl reported its end: whether it merged is GitHub's to say.
func (r childResult) stoppedMerge() (string, int, bool) {
	if o, _ := merge.ParseDocument(r.doc); r.stop == 0 || o.Merged {
		return "", 0, false
	}
	argv := r.spec.Argv
	if len(r.spec.Command) > 0 {
		argv = r.spec.Command
	}
	return merge.ParseArgs(argv)
}

// heard says whether the gate's caller heard the outcome o, the marker
// removed.
func heard(o state.Owed) bool {
	_, err := os.Stat(o.Heard)
	_ = os.Remove(o.Heard)
	return err == nil
}

// owe records o, claimed by its merge-child, which hands it over itself.
func (a *app) owe(o state.Owed) error {
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Owed = append(st.Owed, o)
		return nil, nil
	})
}

// paid drops o; once given up on, why goes to the log.
func (a *app) paid(o state.Owed, why ...string) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Owed = slices.DeleteFunc(st.Owed, func(x state.Owed) bool { return x.Key() == o.Key() })
		if len(why) > 0 {
			return []state.Event{event(gateParty, verbUnheard, "%s; %s", o.Line, why[0])}, nil
		}
		return nil, nil
	})
}

// payOwed hands o to its owner: a stopped merge's outcome first from its
// pull request's state on GitHub. A hand-over that fails keeps o owed, its
// failure logged once, for the watch's next try.
func (a *app) payOwed(ctx context.Context, o state.Owed) {
	st, err := a.store.Read()
	if err != nil {
		return
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(o.Owner) })
	if i < 0 {
		a.paid(o, fmt.Sprintf("%q is no registered agent, nobody is woken", o.Owner.Name))
		return
	}
	ag := st.Agents[i]
	line, ok := a.owedLine(ctx, o)
	if !ok {
		return
	}
	q := ag.Session
	if q == "" {
		q = ag.HostSession
	}
	_ = a.store.Log(event(gateParty, verbUnheard, "%s; waking %q", line, ag.Name))
	if err := wakeOwner(a, ctx, gateParty, q, line, ""); err != nil {
		a.failedPay(o, fmt.Sprintf("waking %q failed (%v)", ag.Name, err))
		return
	}
	a.paid(o)
}

// owedLine is the line o hands over, false while a stopped merge waits for
// GitHub's answer (owedAsk at most); a merge GitHub reports merged is logged
// as one.
func (a *app) owedLine(ctx context.Context, o state.Owed) (string, bool) {
	if !o.Ask {
		return o.Line, true
	}
	p, err := pullState(ctx, o.Repo, o.PR)
	if err != nil {
		if time.Since(o.Since) < owedAsk {
			a.failedPay(o, fmt.Sprintf("GitHub does not answer for %s#%d (%v)", o.Repo, o.PR, err))
			return "", false
		}
		return fmt.Sprintf("%s was stopped by %s before devctl reported its end, and GitHub has not answered for %s#%d since: check the pull request, and run it again only if it is still open",
			o.Command, o.Stop, o.Repo, o.PR), true
	}
	stopped := fmt.Sprintf("%s was stopped by %s before devctl reported its end", o.Command, o.Stop)
	switch p.State {
	case github.Merged:
		at := p.MergedAt.UTC().Format(time.RFC3339)
		_ = a.store.Log(event(gateParty, verbMerged, "%s#%d at %s per GitHub, its waiter stopped by %s before devctl reported it: release unknown", o.Repo, o.PR, at, o.Stop))
		return fmt.Sprintf("%s; GitHub says %s#%d merged at %s: do not merge it again, `devctl release wait %s --pr %d` waits for its release",
			stopped, o.Repo, o.PR, at, o.Repo, o.PR), true
	case github.Closed:
		return fmt.Sprintf("%s; GitHub says %s#%d is closed without a merge", stopped, o.Repo, o.PR), true
	}
	return fmt.Sprintf("%s; GitHub says %s#%d is open, not merged: run the same command again", stopped, o.Repo, o.PR), true
}

// failedPay keeps o owed with why it was not handed over, logged the first
// time.
func (a *app) failedPay(o state.Owed, why string) {
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Owed, func(x state.Owed) bool { return x.Key() == o.Key() })
		if i < 0 {
			return nil, nil
		}
		first := st.Owed[i].Why == ""
		st.Owed[i].Why = why
		if first {
			return []state.Event{event(gateParty, verbUnheard, "%s: %s; owed to %q, the watch tries again", why, o.Line, o.Owner.Name)}, nil
		}
		return nil, nil
	})
}

// owedOutcomes hands over each owed outcome whose gate and merge-child no longer
// hand it over themselves (both gone, or owedBusy passed), one claim per
// owedRetry under the state's lock, so one watch tries it: a caller that
// heard it drops it, and one owed past owedFor is given up.
func (w *watcher) owedOutcomes(ctx context.Context) {
	st, err := w.store.Read()
	if err != nil || len(st.Owed) == 0 {
		return
	}
	now := w.now.UTC()
	due := func(o state.Owed) bool {
		busy := (proc.Alive(o.Gate) || proc.Alive(o.Child)) && now.Sub(o.Since) < owedBusy
		return !busy && now.Sub(o.Tried) >= owedRetry
	}
	var claimed []state.Owed
	_ = w.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, o := range st.Owed {
			if due(o) {
				st.Owed[i].Tried = now
				claimed = append(claimed, st.Owed[i])
			}
		}
		return nil, nil
	})
	for _, o := range claimed {
		switch {
		case heard(o):
			w.paid(o)
		case now.Sub(o.Since) > owedFor:
			w.paid(o, fmt.Sprintf("owed to %q since %s and never handed over (%s): given up", o.Owner.Name, clock(now, o.Since), o.Why))
		default:
			w.payOwed(ctx, o)
		}
	}
}
