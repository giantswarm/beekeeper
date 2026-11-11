package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// sweepListEvery is how often the loop lists the desk's open pull requests
// for the orphan sweep: a third of superloop.OrphanWait, so one without an
// owner is seen well within it.
const sweepListEvery = superloop.OrphanWait / 3

const (
	// sweepListLimit is the most results GitHub's search returns: a list
	// that long may stop short of the desk's open pull requests.
	sweepListLimit = 1000
	// sweepReadsPerPass bounds issue detail reads in delivery discovery.
	// PR observations are batched by the independent watcher.
	sweepReadsPerPass = 50
	// sweepPileStep is how much the pile grows between the lines the loop
	// says about it.
	sweepPileStep = 25
	// loopSweepFile is the side file supervisor status reads the count from.
	loopSweepFile = "loop-sweep.json"
)

// loopSweepCount is what the last pass knew of the desk's open pull
// requests: a growing pile nobody owns is visible, not silent.
type loopSweepCount struct {
	At time.Time `json:"at"`
	// Listed is the open pull requests the last list held; AtLimit says the
	// list may have stopped short of them.
	Listed  int  `json:"listed"`
	AtLimit bool `json:"atLimit,omitempty"`
	// Ownerless is how many have no live owner, the oldest since Oldest.
	Ownerless int       `json:"ownerless"`
	Oldest    time.Time `json:"oldest,omitzero"`
	// Unread is how many the pass left for a later one.
	Unread int `json:"unread,omitempty"`
}

// line says the count in one line, "" when no pull request lacks an owner.
func (c loopSweepCount) line(a *app) string {
	if c.Ownerless == 0 && c.Unread == 0 && !c.AtLimit {
		return ""
	}
	line := fmt.Sprintf("open pull requests: %d listed", c.Listed)
	if c.AtLimit {
		line += " (GitHub's search limit: there may be more)"
	}
	line += fmt.Sprintf(", %d without a live owner", c.Ownerless)
	if !c.Oldest.IsZero() {
		line += ", the oldest since " + clock(a.now, c.Oldest)
	}
	if c.Unread > 0 {
		line += fmt.Sprintf(", %d not read yet", c.Unread)
	}
	return line
}

// reportSweep publishes the pass's count for supervisor status, and says
// it in the loop's log each time the pile grows by sweepPileStep.
func (l *supervisorLoop) reportSweep(ownerless map[string]time.Time, unread map[string]bool) error {
	c := loopSweepCount{At: l.a.timeNow(), Listed: len(l.openPulls), AtLimit: len(l.openPulls) >= sweepListLimit, Ownerless: len(ownerless), Unread: len(unread)}
	for _, since := range ownerless {
		if c.Oldest.IsZero() || since.Before(c.Oldest) {
			c.Oldest = since
		}
	}
	if pile := c.Ownerless / sweepPileStep; pile > 0 {
		l.sayOnce(fmt.Sprintf("sweep pile %d", pile), "SUPERVISOR SWEEP %s", c.line(l.a))
	}
	return l.a.store.WriteFile(loopSweepFile, c)
}

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
		args := append(strings.Fields("search prs --state open --json repository,number"), "--limit", strconv.Itoa(sweepListLimit), "--author", login)
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
	readLoopSweep(l, ctx, in)
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
	// Least recently read first: with a read bound per pass, every pull
	// request gets its turn.
	slices.SortStableFunc(out, func(a, b string) int { return l.pulls[a].at.Compare(l.pulls[b].at) })
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
func (l *supervisorLoop) markOwnerless(in superloop.Input, claims map[string]loopClaim, logged map[string]time.Time, unread map[string]bool) (map[string]time.Time, error) {
	now := l.a.timeNow()
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
		// A pass cut short by the allowance hold says nothing of a pull
		// request it left unread: its stretch goes on.
		if unread[ref] {
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
