package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
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
)

// owed is r's outcome as this merge-child owes it to its owner.
func (r childResult) owed(now time.Time) state.Owed {
	o := state.Owed{Owner: r.spec.Owner, Line: r.outcome(), Gate: r.spec.Gate, Child: os.Getpid(),
		Heard: heardFile(r.base, r.spec.Gate), Since: now.UTC(), Tried: now.UTC(), Cancelled: r.stoppedBy != (state.Party{})}
	if repo, pr, ok := r.stoppedMerge(); ok {
		stop := r.stop
		if stop == 0 {
			stop = syscall.Signal(r.rc - 128)
		}
		o.Ask, o.Repo, o.PR = true, repo, pr
		o.Command, o.Stop = r.command(), fmt.Sprintf("%s (exit %d)", signalName(stop), r.rc)
	}
	return o
}

// stoppedMerge is the pull request of a merge whose merge-child was stopped,
// or whose devctl a signal ended, before devctl reported its end: whether it
// merged is GitHub's to say.
func (r childResult) stoppedMerge() (string, int, bool) {
	if o, _ := merge.ParseDocument(r.doc); r.stop == 0 && r.rc <= 128 || o.Merged {
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

// payOwed hands o to its owner, on the roster or off it (agents wake by its
// session id): a stopped merge's outcome first from its pull request's state
// on GitHub. An owner whose session is gone gets no wake and one line, and
// its records go back to the board; a hand-over that fails otherwise keeps o
// owed, its failure logged once, for the watch's next try.
func (a *app) payOwed(ctx context.Context, o state.Owed) {
	st, err := a.store.Read()
	if err != nil {
		return
	}
	owner := o.Owner
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(o.Owner) }); i >= 0 {
		owner = st.Agents[i].Party
	}
	q := wakeID(owner)
	if q == "" {
		a.paid(o, fmt.Sprintf("%q has no session id, nobody is woken", owner.Name))
		return
	}
	if p := wakeTargetPark(st, q); p != nil && ParkOnPerson(p, a.cfg.Guide.Person) {
		// Keep the result owed until the person's wait settles. An old
		// command completing is no answer to a later decision.
		return
	}
	line, ok := a.owedLine(ctx, o)
	if !ok {
		return
	}
	_ = a.store.Log(event(gateParty, verbUnheard, "%s; waking %q", line, owner.Name))
	if err := wakeOwner(a, ctx, gateParty, q, line, ""); err != nil {
		if errors.Is(err, errGone) {
			a.paid(o, a.goneWaiter(owner, err))
			return
		}
		a.failedPay(o, fmt.Sprintf("waking %q failed (%v)", owner.Name, err))
		return
	}
	a.paid(o)
}

// goneWaiter is the one line for a waiter whose session is gone (err says
// why). One off the roster gives its records back to the board, the issues
// it served named; a roster agent's are the supervisor's to staff again.
func (a *app) goneWaiter(p state.Party, err error) string {
	var issues []string
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		if onRoster(st, p) {
			return nil, nil
		}
		for _, r := range clearRecords(st, p) {
			issues = append(issues, r.Issue)
		}
		return nil, nil
	})
	line := fmt.Sprintf("%q is not woken: %v", p.Name, err)
	if len(issues) > 0 {
		line += "; back to the board: " + strings.Join(issues, ", ")
	}
	return line
}

// owedLine is the line o hands over, false while a stopped merge waits for
// GitHub's answer (owedAsk at most); a merge GitHub reports merged is logged
// as one.
func (a *app) owedLine(ctx context.Context, o state.Owed) (string, bool) {
	if !o.Ask {
		return o.Line, true
	}
	p, err := gate.PullState(ctx, o.Repo, o.PR)
	if err != nil {
		if time.Since(o.Since) < owedAsk {
			a.failedPay(o, fmt.Sprintf("GitHub does not answer for %s#%d (%v)", o.Repo, o.PR, err))
			return "", false
		}
		if o.Cancelled {
			return fmt.Sprintf("%s was cancelled on request; GitHub has not answered whether it merged: check the pull request, do not rerun it", o.Command), true
		}
		return fmt.Sprintf("%s was stopped by %s before devctl reported its end, and GitHub has not answered for %s#%d since: check the pull request, and run it again only if it is still open",
			o.Command, o.Stop, o.Repo, o.PR), true
	}
	stopped := fmt.Sprintf("%s was stopped by %s before devctl reported its end", o.Command, o.Stop)
	switch p.State {
	case github.Merged:
		at := p.MergedAt.UTC().Format(time.RFC3339)
		err := a.store.Update(func(*state.State) ([]state.Event, error) {
			e := about(event(gateParty, verbMerged, "%s#%d at %s per GitHub, its waiter stopped by %s before devctl reported it: release unknown", o.Repo, o.PR, at, o.Stop),
				state.Merge{Repo: o.Repo, PR: o.PR}.Key())
			return a.mergeEvents([]state.Event{e})
		})
		if err != nil {
			a.failedPay(o, fmt.Sprintf("recording the confirmed merge of %s#%d failed (%v)", o.Repo, o.PR, err))
			return "", false
		}
		return fmt.Sprintf("%s; GitHub says %s#%d merged at %s: do not merge it again, `devctl release wait %s --pr %d` waits for its release",
			stopped, o.Repo, o.PR, at, o.Repo, o.PR), true
	case github.Closed:
		if o.Cancelled {
			return fmt.Sprintf("%s was cancelled on request; GitHub says %s#%d is closed without a merge: do not rerun it", o.Command, o.Repo, o.PR), true
		}
		return fmt.Sprintf("%s; GitHub says %s#%d is closed without a merge", stopped, o.Repo, o.PR), true
	}
	if o.Cancelled {
		return fmt.Sprintf("%s was cancelled on request; GitHub says %s#%d is open, not merged: do not rerun it", o.Command, o.Repo, o.PR), true
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
