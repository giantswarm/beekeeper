package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// sweepListEvery is how often the loop lists the desk's open pull requests
// for the orphan sweep: a third of superloop.OrphanWait, so one without an
// owner is seen well within it.
const sweepListEvery = superloop.OrphanWait / 3

// readOpenPulls lists the open pull requests (owner/repo#n) the logins
// authors opened, in orgs when any are named: one REST search per login,
// which spends no GraphQL budget.
var readOpenPulls = func(ctx context.Context, authors func() ([]string, error), orgs []string) ([]string, error) {
	logins, err := authors()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, login := range logins {
		args := append(strings.Fields("search prs --state open --limit 200 --json repository,number"), "--author", login)
		for _, o := range orgs {
			args = append(args, "--owner", o)
		}
		c, cancel := context.WithTimeout(ctx, time.Minute)
		b, err := github.RunGH(c, args...)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("gh search prs --author %s: %w", login, err)
		}
		var found []struct {
			Number     int `json:"number"`
			Repository struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(b, &found); err != nil {
			return nil, fmt.Errorf("gh search prs --author %s: %w", login, err)
		}
		for _, f := range found {
			if f.Repository.NameWithOwner != "" && f.Number > 0 {
				out = append(out, fmt.Sprintf("%s#%d", f.Repository.NameWithOwner, f.Number))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// sweepPulls are the desk's open pull requests the pass reads beside
// tracked, for the orphan sweep: listed every sweepListEvery, less those
// tracked already, outside the desk, or owned by an agent the roster names
// for them (parked on one, or its worker by name), which need no read.
func (l *supervisorLoop) sweepPulls(ctx context.Context, in superloop.Input, tracked []string) []string {
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
		var open []string
		err := l.a.loopGitHub(ctx, "sweep", func() (err error) {
			open, err = readOpenPulls(ctx, authors, l.a.cfg.GitHub.AllowedOrgs)
			return err
		})
		if err != nil {
			// The last list stands; the sweep tries again next interval.
			l.sayOnce("sweep list", "SUPERVISOR SWEEP cannot list the desk's open pull requests (%v): the last list stands", err)
		} else {
			l.clearSaid("sweep list")
			l.openPulls = open
		}
		l.openPullsAt = now
	}
	var out []string
	for _, ref := range l.openPulls {
		if slices.ContainsFunc(tracked, func(t string) bool { return strings.EqualFold(t, ref) }) {
			continue
		}
		if reason, _ := in.Desk.Unread(ref); reason != "" {
			continue
		}
		if slices.ContainsFunc(in.Workers, func(w superloop.Worker) bool {
			return !w.Saved && (strings.EqualFold(w.ParkedOn, ref) || strings.EqualFold(w.Name, superloop.PullWorkerName(ref)))
		}) {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// ownerlessSince opens the detail of a loopOwnerlessVerb line that starts
// a stretch without an owner; ownerlessEnd is the one that ends it.
const (
	ownerlessSince = "since "
	ownerlessEnd   = "owned"
)

// markOwnerless records since when each open pull request of the pass has
// had no live owner (superloop.Ownerless): the first pass that sees it so
// logs it (loopOwnerlessVerb), and it stays so until a pass sees an owner,
// across restarts. An action of the loop's claimed or in flight on it owns
// it meanwhile. logged is what the log says so far.
func (l *supervisorLoop) markOwnerless(in superloop.Input, claims map[string]loopClaim, logged map[string]time.Time, unread ...map[string]bool) (map[string]time.Time, error) {
	now := l.a.timeNow()
	var skipped map[string]bool
	if len(unread) > 0 {
		skipped = unread[0]
	}
	acting := func(ref string) bool {
		on := func(key string) bool { return strings.Contains(key, " "+ref+"@") || strings.HasSuffix(key, " "+ref) }
		if slices.ContainsFunc(slices.Collect(maps.Keys(claims)), on) {
			return true
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		for key, flying := range l.inFlight {
			if flying && on(key) {
				return true
			}
		}
		return false
	}
	next := map[string]time.Time{}
	for _, ref := range superloop.Ownerless(in) {
		if acting(ref) {
			continue
		}
		if since, ok := logged[ref]; ok {
			next[ref] = since
			continue
		}
		next[ref] = now
		if !l.dryRun {
			if err := l.a.store.Record(about(event(loopParty, loopOwnerlessVerb, "[%s] %s%s", ref, ownerlessSince, now.UTC().Format(time.RFC3339Nano)), ref)); err != nil {
				return nil, err
			}
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(logged)) {
		if skipped[ref] {
			next[ref] = logged[ref]
			continue
		}
		// A pull request the pass did not read (merged, closed, gone from
		// the list) ends its stretch as one with an owner does.
		if _, still := next[ref]; still || l.dryRun {
			continue
		}
		if err := l.a.store.Record(about(event(loopParty, loopOwnerlessVerb, "[%s] %s", ref, ownerlessEnd), ref)); err != nil {
			return nil, err
		}
	}
	return next, nil
}
