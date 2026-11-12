package loop

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

// SweepListEvery is how often the loop lists the desk's open pull requests
// for the orphan sweep: a third of superloop.OrphanWait, so one without an
// owner is seen well within it.
const SweepListEvery = superloop.OrphanWait / 3

const (
	// SweepListLimit is the most results GitHub's search returns: a list
	// that long may stop short of the desk's open pull requests.
	SweepListLimit = 1000
	// SweepReadsPerPass bounds issue detail reads in delivery discovery.
	// PR observations are batched by the independent watcher.
	SweepReadsPerPass = 50
	// sweepPileStep is how much the pile grows between the lines the loop
	// says about it.
	sweepPileStep = 25
	// LoopSweepFile is the side file supervisor status reads the count from.
	LoopSweepFile = "loop-sweep.json"
)

// LoopSweepCount is what the last pass knew of the desk's open pull
// requests: a growing pile nobody owns is visible, not silent.
type LoopSweepCount struct {
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

// Line says the count in one line, "" when no pull request lacks an owner.
func (c LoopSweepCount) Line(a Host) string {
	if c.Ownerless == 0 && c.Unread == 0 && !c.AtLimit {
		return ""
	}
	line := fmt.Sprintf("open pull requests: %d listed", c.Listed)
	if c.AtLimit {
		line += " (GitHub's search limit: there may be more)"
	}
	line += fmt.Sprintf(", %d without a live owner", c.Ownerless)
	if !c.Oldest.IsZero() {
		line += ", the oldest since " + Cmd.Clock(a.Now(), c.Oldest)
	}
	if c.Unread > 0 {
		line += fmt.Sprintf(", %d not read yet", c.Unread)
	}
	return line
}

// reportSweep publishes the pass's count for supervisor status, and says
// it in the loop's log each time the pile grows by sweepPileStep.
func (l *Loop) reportSweep(ownerless map[string]time.Time, unread map[string]bool) error {
	c := LoopSweepCount{At: l.Host.TimeNow(), Listed: len(l.OpenPulls), AtLimit: len(l.OpenPulls) >= SweepListLimit, Ownerless: len(ownerless), Unread: len(unread)}
	for _, since := range ownerless {
		if c.Oldest.IsZero() || since.Before(c.Oldest) {
			c.Oldest = since
		}
	}
	if pile := c.Ownerless / sweepPileStep; pile > 0 {
		l.SayOnce(fmt.Sprintf("sweep pile %d", pile), "SUPERVISOR SWEEP %s", c.Line(l.Host))
	}
	return l.Host.Store().WriteFile(LoopSweepFile, c)
}

const (
	// SweepRepoBatch bounds the --repo flags in a single gh search prs call so a
	// desk with many team repositories stays within GitHub's search query limits.
	SweepRepoBatch = 25
	SweepOwnerFlag = "--owner"
)

// SweepFlagBatches splits scopes (owners without a slash, owner/repo with
// one) into gh search prs flag slices. Owners and repositories are searched in
// separate calls because gh ANDs --owner with --repo when both are given.
func SweepFlagBatches(scopes []string) [][]string {
	var owners, repos []string
	for _, s := range scopes {
		if strings.Contains(s, "/") {
			repos = append(repos, s)
		} else if s != "" {
			owners = append(owners, s)
		}
	}
	var batches [][]string
	if len(owners) > 0 {
		var flags []string
		for _, o := range owners {
			flags = append(flags, SweepOwnerFlag, o)
		}
		batches = append(batches, flags)
	}
	for start := 0; start < len(repos); start += SweepRepoBatch {
		var flags []string
		for _, r := range repos[start:min(start+SweepRepoBatch, len(repos))] {
			flags = append(flags, LoopRepoFlag, r)
		}
		batches = append(batches, flags)
	}
	if len(batches) == 0 {
		return [][]string{nil}
	}
	return batches
}

// ReadOpenPulls lists the open pull requests (owner/repo#n) the logins
// authors opened, in scopes (owners without a slash, repositories with one)
// when any are named: REST searches per login that spend no GraphQL budget.
var ReadOpenPulls = func(ctx context.Context, authors func() ([]string, error), scopes []string) ([]string, error) {
	logins, err := authors()
	if err != nil {
		return nil, err
	}
	batches := SweepFlagBatches(scopes)
	var out []string
	for _, login := range logins {
		for _, flags := range batches {
			args := append(strings.Fields("search prs --state open --json repository,number"), "--limit", strconv.Itoa(SweepListLimit), "--author", login)
			args = append(args, flags...)
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
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// SweepPulls are the desk's open pull requests the pass reads beside
// tracked, for the orphan sweep: listed every SweepListEvery, less those
// tracked already, outside the desk, or owned by an agent the roster names
// for them (parked on one, or its worker by name), which need no read.
func (l *Loop) SweepPulls(ctx context.Context, in superloop.Input, tracked []string) []string {
	ReadLoopSweep(l, ctx, in)
	var out []string
	for _, ref := range l.OpenPulls {
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
	slices.SortStableFunc(out, func(a, b string) int { return l.Pulls[a].At.Compare(l.Pulls[b].At) })
	return out
}

// OwnerlessSince opens the detail of a LoopOwnerlessVerb line that starts
// a stretch without an owner; OwnerlessEnd is the one that ends it.
const (
	OwnerlessSince = "since "
	OwnerlessEnd   = "owned"
)

// markOwnerless records since when each open pull request of the pass has
// had no live owner (superloop.Ownerless): the first pass that sees it so
// logs it (LoopOwnerlessVerb), and it stays so until a pass sees an owner,
// across restarts. An action of the loop's claimed or in flight on it owns
// it meanwhile. logged is what the log says so far.
func (l *Loop) markOwnerless(in superloop.Input, claims map[string]LoopClaim, logged map[string]time.Time, unread map[string]bool) (map[string]time.Time, error) {
	now := l.Host.TimeNow()
	acting := func(ref string) bool {
		on := func(key string) bool { return strings.Contains(key, " "+ref+"@") || strings.HasSuffix(key, " "+ref) }
		if slices.ContainsFunc(slices.Collect(maps.Keys(claims)), on) {
			return true
		}
		l.Mu.Lock()
		defer l.Mu.Unlock()
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
		if !l.DryRun {
			if err := l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopOwnerlessVerb, "[%s] %s%s", ref, OwnerlessSince, now.UTC().Format(time.RFC3339Nano)), ref)); err != nil {
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
		if _, still := next[ref]; still || l.DryRun {
			continue
		}
		if err := l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopOwnerlessVerb, "[%s] %s", ref, OwnerlessEnd), ref)); err != nil {
			return nil, err
		}
	}
	return next, nil
}
