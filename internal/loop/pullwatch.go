package loop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	LoopPullShort     = 30 * time.Second
	LoopPullLong      = 2 * time.Minute
	loopPullBackoff   = 10 * time.Minute
	LoopPullMaxAge    = 5 * time.Minute
	loopPullBatchSize = 60
	// loopPullDetailBatchSize bounds a batch of pull requests read on first
	// sight, with their files, reviews and closing issues: the heaviest
	// query the watch asks. GitHub answered a restarted loop's first batch,
	// sixty of them, with 504 (#3809).
	loopPullDetailBatchSize = 20
	// LoopPullMinBatch is the smallest batch a fault of GitHub's halves the
	// watch's batches to: below it, the failure is GitHub's, not the
	// batch's size.
	LoopPullMinBatch = 5
	// loopPullBatchRegrow is how long a halved batch size holds without a
	// fault before it doubles again.
	loopPullBatchRegrow = 30 * time.Minute
	LoopPullWatchFile   = "supervisor-pull-observations.json"
)

const (
	pullWatchKey     = "pull watcher"
	PullContexts     = "contexts"
	PullSuites       = "checkSuites"
	PullNodes        = "nodes"
	PullPageInfo     = "pageInfo"
	PullHasNext      = "hasNextPage"
	PullEndCursor    = "endCursor"
	PullHeadField    = "headRefOid"
	pullUpdatedField = "updatedAt"
	JsonNullLiteral  = "null"
)

// Only the watcher writes these observations. Consumers never renew a read's
// timestamp by reusing it, and never fall back to a synchronous GitHub read.
type WatchedLoopPull struct {
	Pull    superloop.Pull `json:"pull"`
	At      time.Time      `json:"at"`
	Pending bool           `json:"pending,omitempty"`
	Error   string         `json:"error,omitempty"`
}

type LoopPullSnapshot struct {
	Pulls      map[string]WatchedLoopPull `json:"pulls"`
	Open       []string                   `json:"open,omitempty"`
	ListedAt   time.Time                  `json:"listedAt"`
	Incomplete bool                       `json:"incomplete,omitempty"`
	// PolledAt is when a poll last read every pull request it asked for.
	PolledAt time.Time `json:"polledAt,omitzero"`
	// Failing is the episode of failed polls the watch is in, nil once a
	// poll succeeds.
	Failing *LoopPullFailing `json:"failing,omitempty"`
}

// LoopPullFailing is an episode of failed polls: since when, how many
// tries, and what the last one failed on.
type LoopPullFailing struct {
	Since time.Time `json:"since"`
	Tries int       `json:"tries"`
	Error string    `json:"error"`
}

type loopPullWatcher struct {
	Mu       sync.Mutex
	Snapshot LoopPullSnapshot
	tracked  map[string]bool
	problem  map[string]bool
	extra    map[string]bool
	changed  chan struct{}
	ready    chan struct{}
	// Started is when the continuous watch started, zero for a pass's
	// single poll (--once, --dry-run): the stall alarm measures from it.
	Started time.Time
	// batch is how many pull requests a query asks for, 0 for
	// loopPullBatchSize; halved is when a fault of GitHub's last halved it.
	batch  int
	halved time.Time
	// Configuration is copied by the pass, never read concurrently from app.cfg.
	configured           bool
	authors, orgs, never []string
	floor                int
	// bots, listed beside authors, keep only the pull requests in
	// repositories botDesk.BotRepo admits by teams, the team files' owner
	// of each lowercased owner/name (teamsKnown once they were read).
	bots       []string
	botDesk    superloop.Desk
	teams      map[string]string
	teamsKnown bool
}

func newPullWatcher() *loopPullWatcher {
	return &loopPullWatcher{Snapshot: LoopPullSnapshot{Pulls: map[string]WatchedLoopPull{}}, tracked: map[string]bool{}, problem: map[string]bool{}, extra: map[string]bool{}, changed: make(chan struct{}, 1), ready: make(chan struct{}, 1)}
}

func (l *Loop) PullWatcher() *loopPullWatcher {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.pullWatch == nil {
		l.pullWatch = newPullWatcher()
		var saved LoopPullSnapshot
		if ok, err := l.Host.Store().ReadFile(LoopPullWatchFile, &saved); ok && err == nil && saved.Pulls != nil {
			l.pullWatch.Snapshot = saved
		}
	}
	return l.pullWatch
}

func (w *loopPullWatcher) View() LoopPullSnapshot {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	s := w.Snapshot
	s.Pulls = maps.Clone(s.Pulls)
	s.Open = slices.Clone(s.Open)
	if s.Failing != nil {
		f := *s.Failing
		s.Failing = &f
	}
	return s
}

// batchSize is how many pull requests one query asks for: halved by a
// fault of GitHub's, doubled again after each loopPullBatchRegrow without
// one.
func (w *loopPullWatcher) batchSize(now time.Time) int {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if w.batch > 0 && now.Sub(w.halved) >= loopPullBatchRegrow {
		w.batch, w.halved = min(loopPullBatchSize, 2*w.batch), now
		if w.batch == loopPullBatchSize {
			w.batch = 0
		}
	}
	return cmp.Or(w.batch, loopPullBatchSize)
}

// halve takes a batch of n that GitHub failed: the watch's batches hold
// half of it from now on. It is false at LoopPullMinBatch, where a smaller
// batch would not help.
func (w *loopPullWatcher) halve(n int, now time.Time) (int, bool) {
	if n <= LoopPullMinBatch {
		return n, false
	}
	size := max(LoopPullMinBatch, (n+1)/2)
	w.Mu.Lock()
	w.batch, w.halved = size, now
	w.Mu.Unlock()
	return size, true
}

func signalPullWatch(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (w *loopPullWatcher) Set(refs, authors, orgs []string, floor int, never []string) {
	w.Mu.Lock()
	next := make(map[string]bool, len(refs))
	added := false
	for _, ref := range refs {
		next[ref] = true
		if !w.tracked[ref] {
			added = true
		}
	}
	configChanged := w.configured && (!slices.Equal(w.authors, authors) || !slices.Equal(w.orgs, orgs) || w.floor != floor || !slices.Equal(w.never, never))
	w.configured, w.tracked, w.authors, w.orgs, w.floor, w.never = true, next, slices.Clone(authors), slices.Clone(orgs), floor, slices.Clone(never)
	if configChanged {
		w.Snapshot.ListedAt = time.Time{}
	}
	w.Mu.Unlock()
	if added || configChanged {
		signalPullWatch(w.changed)
	}
}

func (w *loopPullWatcher) SetProblemPulls(refs []string) {
	w.Mu.Lock()
	next := make(map[string]bool, len(refs))
	added := false
	for _, ref := range refs {
		next[ref] = true
		if !w.problem[ref] && !w.tracked[ref] {
			if _, seen := w.Snapshot.Pulls[ref]; !seen {
				added = true
			}
		}
	}
	w.problem = next
	w.Mu.Unlock()
	if added {
		signalPullWatch(w.changed)
	}
}

// SetBots has the listing find the pull requests bots opened in the
// repositories desk owns, teams being the team files' owners as last read
// (known false until they are): without it the sweep never sees one, and
// a green bot pull request without a worker waits for nobody.
func (w *loopPullWatcher) SetBots(bots []string, desk superloop.Desk, teams map[string]string, known bool) {
	w.Mu.Lock()
	oldScopes, oldOK := w.botDesk.BotScopes(w.teams, w.teamsKnown)
	newScopes, newOK := desk.BotScopes(teams, known)
	changed := !slices.Equal(w.bots, bots) || len(bots) > 0 && (known != w.teamsKnown || oldOK != newOK || !slices.Equal(oldScopes, newScopes))
	w.bots, w.botDesk, w.teams, w.teamsKnown = slices.Clone(bots), desk, maps.Clone(teams), known
	if changed {
		w.Snapshot.ListedAt = time.Time{}
	}
	w.Mu.Unlock()
	if changed {
		signalPullWatch(w.changed)
	}
}

// readBotPulls lists the open pull requests of the watcher's bots in orgs
// that are in a repository of the desk's, the bots whose search stopped
// short of all of theirs, and the bots whose search failed with its error.
// Each bot is searched on its own: one bot's failure (a login GitHub's
// search refuses, a transient error) leaves the others' listed.
func (w *loopPullWatcher) readBotPulls(ctx context.Context, orgs []string) (out, short, listedBots []string, failed map[string]error) {
	w.Mu.Lock()
	bots, desk, teams, known := slices.Clone(w.bots), w.botDesk, maps.Clone(w.teams), w.teamsKnown
	w.Mu.Unlock()
	if len(desk.Orgs) == 0 {
		desk.Orgs = orgs
	}
	scopes, ok := desk.BotScopes(teams, known)
	if !ok {
		return nil, nil, nil, nil
	}
	for _, b := range bots {
		login := superloop.BotLogin(b)
		listed, err := ReadOpenPulls(ctx, func() ([]string, error) { return []string{login}, nil }, scopes)
		if err != nil {
			if failed == nil {
				failed = map[string]error{}
			}
			failed[login] = err
			continue
		}
		listedBots = append(listedBots, login)
		if len(listed) >= SweepListLimit {
			short = append(short, login)
		}
		for _, ref := range listed {
			if r, ok := Cmd.ParseRef(ref); ok && desk.BotRepo(r.Repo, teams[strings.ToLower(r.Repo)], known) {
				out = append(out, ref)
			}
		}
	}
	return out, short, listedBots, failed
}

func (w *loopPullWatcher) needsPoll(tracked []string, now time.Time) bool {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	if w.Snapshot.ListedAt.IsZero() || now.Sub(w.Snapshot.ListedAt) >= SweepListEvery || w.Snapshot.Incomplete {
		return true
	}
	desk := superloop.Desk{Orgs: w.orgs, Never: w.never}
	check := func(ref string) bool {
		if why, _ := desk.Unread(ref); why != "" {
			return false
		}
		p, ok := w.Snapshot.Pulls[ref]
		if !ok || p.Error != "" || p.Pull.State == "" {
			return true
		}
		return p.Pull.State != superloop.Merged && p.Pull.State != superloop.Closed && (p.At.IsZero() || now.Sub(p.At) > LoopPullMaxAge)
	}
	if slices.ContainsFunc(tracked, check) || slices.ContainsFunc(w.Snapshot.Open, check) {
		return true
	}
	for ref := range w.problem {
		if why, _ := desk.Unread(ref); why != "" {
			continue
		}
		p, ok := w.Snapshot.Pulls[ref]
		if !ok || p.Error != "" || p.At.IsZero() {
			return true
		}
		if p.Pull.State != "" && p.Pull.State != superloop.Merged && p.Pull.State != superloop.Closed && now.Sub(p.At) > LoopPullMaxAge {
			return true
		}
	}
	return false
}

func (w *loopPullWatcher) Read(ref string, now time.Time) (WatchedLoopPull, error) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	p, ok := w.Snapshot.Pulls[ref]
	if !ok {
		if !w.extra[ref] {
			w.extra[ref] = true
			signalPullWatch(w.changed)
		}
		return p, fmt.Errorf("%s has no watcher observation: %w", ref, ErrLoopLater)
	}
	if p.Error != "" {
		return p, fmt.Errorf("%s: %s: %w", ref, p.Error, ErrLoopLater)
	}
	if p.Pull.State == "" {
		return p, fmt.Errorf("%s unavailable at %s: %w", ref, p.At.Format(time.RFC3339), ErrLoopLater)
	}
	// A terminal observation is final; it no longer consumes a poll slot.
	if p.Pull.State != superloop.Merged && p.Pull.State != superloop.Closed && (p.At.IsZero() || now.Sub(p.At) > LoopPullMaxAge) {
		return p, fmt.Errorf("%s observation is %s old (limit %s): %w", ref, now.Sub(p.At).Round(time.Second), LoopPullMaxAge, ErrLoopLater)
	}
	return p, nil
}

// ReadObservedLoopPull is a seam for planner/action fixtures. Production always
// reads the watcher, including --once and --dry-run, which use its saved snapshot.
var ReadObservedLoopPull = func(l *Loop, ctx context.Context, ref string, _ time.Time, _ string) (LoopPullObservation, error) {
	if err := ctx.Err(); err != nil {
		return LoopPullObservation{}, err
	}
	now := l.Host.TimeNow()
	if l.Host.LoopNow() != nil {
		now = clk.Now()
	}
	p, err := l.PullWatcher().Read(ref, now)
	return LoopPullObservation{Pull: p.Pull, At: p.At}, err
}

func (l *Loop) readPull(ctx context.Context, ref string) (superloop.Pull, error) {
	p, err := ReadObservedLoopPull(l, ctx, ref, time.Time{}, "")
	return p.Pull, err
}

// StartLoopPullWatcher owns one cancellable timer and one fetch at a time. Its
// independent clock does not stop at the beginning of a supervisor pass.
var StartLoopPullWatcher = func(ctx context.Context, l *Loop) func() {
	ctx, cancel := context.WithCancel(ctx)
	w := l.PullWatcher()
	w.Set(nil, l.Host.Cfg().Supervisor.Ownership.Authors, l.Host.Cfg().GitHub.AllowedOrgs, l.Host.Cfg().GitHub.Floor, l.Host.Cfg().Supervisor.Ownership.Never)
	own := l.Host.Cfg().Supervisor.Ownership
	w.SetBots(own.Bots, superloop.Desk{Orgs: l.Host.Cfg().GitHub.AllowedOrgs, Team: own.Team, Review: own.Review, Never: own.Never}, nil, false)
	w.Mu.Lock()
	w.Started = clk.Now()
	w.Mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := clk.NewTimer(0)
		defer timer.Stop()
		schedule := LoopPullSchedule{}
		var lastPoll time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.changed:
				// New work must not defeat rate-limit backoff.
				if schedule.Failures > 0 {
					continue
				}
				// Coalesce arrivals from any number of sessions into the same
				// short interval rather than spending a request per new park.
				if delay := clk.Until(lastPoll.Add(LoopPullShort)); delay > 0 {
					timer.Reset(delay)
					continue
				}
			case <-timer.C:
			}
			lastPoll = clk.Now()
			next, err := w.Poll(ctx, l)
			if ctx.Err() != nil {
				return
			}
			next = schedule.After(l, next, err)
			timer.Reset(next)
		}
	}()
	return func() { cancel(); <-done }
}

var PollOncePullWatcher = func(ctx context.Context, l *Loop) {
	var schedule LoopPullSchedule
	next, err := l.PullWatcher().Poll(ctx, l)
	schedule.After(l, next, err)
}

// LoopPullSchedule is the watch's episode of failed polls: how many, since
// when.
type LoopPullSchedule struct {
	Failures int
	since    time.Time
}

// After schedules the next poll. A failed poll is said at every try, with
// its count and the next try, and the poll that ends the episode says so: a
// watch whose later failures went unsaid looked dead for a day while it
// retried (#3809). A budget hold, which ends on its own, is said once.
func (s *LoopPullSchedule) After(l *Loop, next time.Duration, err error) time.Duration {
	w, now := l.PullWatcher(), clk.Now()
	if err == nil {
		if s.Failures > 0 {
			_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR PULL WATCH recovered: the poll reads the desk's pull requests again after %d failed tries since %s\n", s.Failures, s.since.UTC().Format(time.RFC3339))
		}
		s.Failures, s.since = 0, time.Time{}
		w.failing(l, nil)
		l.ClearSaid(pullWatchKey)
		return next
	}
	s.Failures++
	if s.since.IsZero() {
		s.since = now
	}
	w.failing(l, &LoopPullFailing{Since: s.since, Tries: s.Failures, Error: Cmd.OneLine(err.Error())})
	var hold *PullBudgetHold
	if errors.As(err, &hold) {
		retry := pullBudgetRetry(hold, s.Failures)
		l.SayOnce(pullWatchKey, "SUPERVISOR PULL WATCH held: %v; next try in %s", err, retry)
		return retry
	}
	retry := PullWatchRetry(s.Failures)
	_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR PULL WATCH held: try %d failed (failing since %s): %v; next try in %s\n", s.Failures, s.since.UTC().Format(time.RFC3339), Cmd.OneLine(err.Error()), retry)
	return retry
}

// failing keeps the watch's episode of failed polls, nil when a poll ends
// it, in its saved snapshot for supervisor status.
func (w *loopPullWatcher) failing(l *Loop, f *LoopPullFailing) {
	w.Mu.Lock()
	changed := f != nil || w.Snapshot.Failing != nil
	w.Snapshot.Failing = f
	w.Mu.Unlock()
	if changed && !l.DryRun {
		_ = l.Host.Store().WriteFile(LoopPullWatchFile, w.View())
	}
}

func PullWatchRetry(failures int) time.Duration {
	return min(loopPullBackoff, LoopPullShort*time.Duration(1<<min(failures, 5)))
}

// PullBudgetHold is the pull watch held by the GitHub budget: its cause,
// and when the hold ends on its own (the reset of a low or refused window).
// An unknown or stale reading has no end of its own: the next poll takes a
// fresh reading.
type PullBudgetHold struct {
	Cause string
	Until time.Time
}

func (h *PullBudgetHold) Error() string { return h.Cause + ": " + ErrLoopLater.Error() }

func (h *PullBudgetHold) Unwrap() error { return ErrLoopLater }

// pullBudgetRetry is when a budget hold is tried again: at the reset of a
// low window, else soon, since a reading costs no allowance. Neither waits
// out the exponential backoff GitHub's refusals of a batch get: a watch that
// was held on a stale reading until well after the reading recovered kept
// every observation stale (#3841).
func pullBudgetRetry(h *PullBudgetHold, failures int) time.Duration {
	if !h.Until.IsZero() {
		return min(loopPullBackoff, max(LoopPullShort, clk.Until(h.Until)))
	}
	return min(LoopPullLong, PullWatchRetry(failures))
}

// pullBudgetReadTimeout bounds the pull watch's own budget reading, its
// save included.
const pullBudgetReadTimeout = 30 * time.Second

// pullBudgetHeld is the pull watch's GitHub budget gate, nil when its batch
// may read; the batch keeps LoopGraphQLReserve above github.floor. A saved
// reading that is missing, failed, stale or of a window that has reset is
// resolved by a reading of the watch's own, as the held pass resolves it
// (#3724): the loop's watch and passes sample too rarely, or fail to, on a
// loaded machine, and a pull watch that only waited for their reading left
// every pull observation stale and held starts and merge decisions (#3841).
func (l *Loop) pullBudgetHeld(ctx context.Context, floor int, now time.Time) error {
	st, err := l.Host.Store().Peek()
	if err != nil {
		return err
	}
	cause, until, fresh := LoopBudgetHold(st.Budget, floor, LoopGraphQLReserve, now)
	if cause == "" {
		return nil
	}
	if fresh && !l.DryRun {
		b, err := l.readPullBudget(ctx)
		if err != nil {
			return &PullBudgetHold{Cause: cause + "; the watch's own reading failed: " + Cmd.OneLine(err.Error())}
		}
		if cause, until, _ = LoopBudgetHold(b, floor, LoopGraphQLReserve, clk.Now()); cause == "" {
			return nil
		}
	}
	return &PullBudgetHold{Cause: cause, Until: until}
}

// readPullBudget takes the pull watch's own budget reading, the rate limit
// reads alone, beside a pass: under the loop's configuration lock, as the
// refill reads the configuration.
func (l *Loop) readPullBudget(ctx context.Context) (*state.Budget, error) {
	ctx, cancel := context.WithTimeout(ctx, pullBudgetReadTimeout)
	defer cancel()
	l.RefillConfigMu.RLock()
	defer l.RefillConfigMu.RUnlock()
	return Cmd.RefreshLoopBudget(l.Host, ctx)
}

func (w *loopPullWatcher) Poll(ctx context.Context, l *Loop) (time.Duration, error) {
	w.Mu.Lock()
	authors, orgs, floor := slices.Clone(w.authors), slices.Clone(w.orgs), w.floor
	desk := superloop.Desk{Orgs: orgs, Never: slices.Clone(w.never)}
	wasTracked := maps.Clone(w.tracked)
	problem := maps.Clone(w.problem)
	refs := maps.Clone(w.tracked)
	for ref := range w.problem {
		refs[ref] = true
	}
	for ref := range w.extra {
		refs[ref] = true
	}
	before := w.Snapshot
	before.Pulls, before.Open = maps.Clone(before.Pulls), slices.Clone(before.Open)
	listed := LoopPullSnapshot{Open: slices.Clone(before.Open), ListedAt: before.ListedAt, Incomplete: before.Incomplete}
	w.Mu.Unlock()
	now := clk.Now()
	// Polling PRs never holds the pass's GraphQL admission lock during
	// network IO. The budget gate's own reading has its own bound: on a
	// busy state commit it waits out most of it, and the listing and the
	// batch still get their whole minute after it (#3841).
	if floor > 0 {
		if err := l.pullBudgetHeld(ctx, floor, now); err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var discoveryErr error
	if before.ListedAt.IsZero() || now.Sub(before.ListedAt) >= SweepListEvery {
		open, err := ReadOpenPulls(ctx, func() ([]string, error) {
			if len(authors) > 0 {
				return authors, nil
			}
			login, err := ReadDeskLogin(ctx)
			return []string{login}, err
		}, orgs)
		incomplete := len(open) >= SweepListLimit
		if err == nil {
			// A bot's pull requests beyond the search's limit, or a bot's
			// failed search, leave the desk's own listing whole: the sweep
			// takes the rest as their pull requests merge, and the next
			// listing searches the failed bot again.
			bots, short, listedBots, failed := w.readBotPulls(ctx, orgs)
			if len(short) > 0 {
				l.SayOnce("sweep bot limit", "SUPERVISOR SWEEP pull requests of %s in %s reach GitHub's search limit of %d: the sweep sees only the most recent", strings.Join(short, ", "), strings.Join(orgs, ", "), SweepListLimit)
			}
			for _, login := range slices.Sorted(maps.Keys(failed)) {
				l.SayOnce("sweep bot search "+login, "SUPERVISOR SWEEP cannot list the pull requests of %s of supervisor.ownership.bots (%v): the sweep lists the desk's own without them", login, failed[login])
			}
			for _, login := range listedBots {
				l.ClearSaid("sweep bot search " + login)
			}
			open = append(open, bots...)
			slices.Sort(open)
			open = slices.Compact(open)
		}
		discoveryErr = err
		if err == nil {
			before.Open, before.ListedAt = open, clk.Now()
			before.Incomplete = incomplete
		} else {
			before.Incomplete = true
		}
	}
	for _, ref := range before.Open {
		refs[ref] = true
	}
	var query []string
	for ref := range refs {
		if why, _ := desk.Unread(ref); why != "" {
			delete(refs, ref)
			continue
		}
		if p, ok := before.Pulls[ref]; ok && (p.Pull.State == superloop.Merged || p.Pull.State == superloop.Closed || problem[ref] && !wasTracked[ref] && p.Error == "" && p.Pull.State == "" && !p.At.IsZero()) {
			continue
		}
		if _, ok := Cmd.ParseRef(ref); ok {
			query = append(query, ref)
		}
	}
	// The oldest observations first, those never read before them: what a
	// poll cut short left unread is read first by the next.
	slices.SortFunc(query, func(a, b string) int {
		return cmp.Or(before.Pulls[a].At.Compare(before.Pulls[b].At), strings.Compare(a, b))
	})
	found, faults, err := w.readBatches(ctx, l, query, before.Pulls, floor)
	for ref, p := range found {
		before.Pulls[ref] = p
	}
	// A new listing stands with a poll that read what it listed: passes
	// would otherwise take each pull request it found as unread.
	if err != nil {
		before.Open, before.ListedAt, before.Incomplete = listed.Open, listed.ListedAt, listed.Incomplete
	}
	// Stop watching removed references; keep final observations for active parks.
	for ref := range before.Pulls {
		if !refs[ref] {
			delete(before.Pulls, ref)
		}
	}
	before.Open = slices.DeleteFunc(before.Open, func(ref string) bool {
		p := before.Pulls[ref].Pull
		return p.State == superloop.Merged || p.State == superloop.Closed
	})
	next := LoopPullLong
	for _, p := range found {
		if p.Pending || p.Pull.Mergeable == "UNKNOWN" {
			next = LoopPullShort
		}
	}
	if err == nil {
		before.PolledAt = clk.Now()
	}
	// What a failed poll read stands: a batch GitHub fails no longer
	// discards the batches it answered.
	w.Mu.Lock()
	w.Snapshot = before
	for ref := range found {
		delete(w.extra, ref)
	}
	w.Mu.Unlock()
	if !l.DryRun {
		_ = l.Host.Store().WriteFile(LoopPullWatchFile, before)
	}
	if faults > 0 && err == nil {
		_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR PULL WATCH read the %d pull requests it asked for; GitHub failed %d of its batches, which hold %d from now on\n", len(query), faults, w.batchSize(clk.Now()))
	}
	if err == nil || len(found) > 0 {
		signalPullWatch(w.ready)
	}
	if err != nil {
		return 0, err
	}
	return next, discoveryErr
}

// readBatches reads refs in batches of the watch's size, those read on
// first sight in batches of at most loopPullDetailBatchSize, the others
// first. A batch GitHub fails (PullBatchFault: a 5xx, no answer in time, a
// body that is not JSON) is asked again in halves, down to
// LoopPullMinBatch; one that still fails is skipped, and the rest are
// read. It returns what it read, how many batches GitHub failed, and its
// first error: a batch GitHub kept failing, or any other failure, which
// ends the poll.
func (w *loopPullWatcher) readBatches(ctx context.Context, l *Loop, refs []string, previous map[string]WatchedLoopPull, floor int) (map[string]WatchedLoopPull, int, error) {
	found := map[string]WatchedLoopPull{}
	var light, detail []string
	for _, ref := range refs {
		if pullNeedsDetails(previous, ref) {
			detail = append(detail, ref)
		} else {
			light = append(light, ref)
		}
	}
	var skipped error
	faults := 0
	for _, group := range []struct {
		refs  []string
		limit int
	}{{light, loopPullBatchSize}, {detail, loopPullDetailBatchSize}} {
		for rest := group.refs; len(rest) > 0; {
			n := min(len(rest), group.limit, w.batchSize(clk.Now()))
			got, err := ReadLoopPullBatch(ctx, rest[:n], previous, floor)
			var fault *PullBatchFault
			switch {
			case err == nil:
				maps.Copy(found, got)
			case errors.As(err, &fault) && ctx.Err() == nil:
				faults++
				if size, ok := w.halve(n, clk.Now()); ok {
					_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR PULL WATCH GitHub failed a batch of %d pull requests (%v); asking again in batches of %d\n", n, Cmd.OneLine(err.Error()), size)
					continue
				}
				_, _ = fmt.Fprintf(l.Host.Out(), "SUPERVISOR PULL WATCH GitHub failed a batch of %d pull requests from %s at the smallest size (%v); reading the rest\n", n, rest[0], Cmd.OneLine(err.Error()))
				skipped = cmp.Or(skipped, err)
			default:
				return found, faults, err
			}
			rest = rest[n:]
		}
	}
	return found, faults, skipped
}

// pullNeedsDetails says whether a batch reads ref's details (author, body,
// files, reviews, closing issues): on first sight, and after an error or an
// unknown state.
func pullNeedsDetails(previous map[string]WatchedLoopPull, ref string) bool {
	p, exists := previous[ref]
	return !exists || p.Pull.State == "" || p.Error != "" || p.Pull.Updated.IsZero()
}

// PullBatchFault is GitHub failing a pull batch rather than refusing it: a
// 5xx, no answer in time, or a body that is not JSON (GitHub's HTML error
// page, an empty or a cut-off body). A smaller batch may pass where this one
// failed.
type PullBatchFault struct{ Err error }

func (f *PullBatchFault) Error() string { return f.Err.Error() }

func (f *PullBatchFault) Unwrap() error { return f.Err }

// errNoJSON stands for gh's error when it exited 0 on a body that is not
// JSON.
var errNoJSON = errors.New("gh exited 0")

// pullBatchErr is gh's error of a pull batch, a PullBatchFault when GitHub
// kept failing it (github.Transient).
func pullBatchErr(err error) error {
	if github.Transient(err) {
		return &PullBatchFault{Err: err}
	}
	return err
}

// bodyHead is the start of a body that is not JSON, on one line.
func bodyHead(body []byte) string {
	const most = 60
	head := []rune(strings.Join(strings.Fields(string(body[:min(len(body), 4*most)])), " "))
	if len(head) > most {
		return string(head[:most]) + "…"
	}
	return string(head)
}

// Light fields change independently of updatedAt (notably CI and mergeability).
const loopPullLightFields = `state url headRefOid baseRefName headRefName updatedAt isDraft mergeable mergeStateStatus reviewDecision mergeCommit { oid }
 commits(last:1) { nodes { commit {
 checkSuites(first:100) { pageInfo { hasNextPage endCursor } nodes { status conclusion workflowRun { databaseId runAttempt url workflow { databaseId name } } } }
 statusCheckRollup { state contexts(first:100) { pageInfo { hasNextPage endCursor } nodes {
 __typename ... on CheckRun { name status conclusion detailsUrl startedAt checkSuite { workflowRun { workflow { name } } } }
 ... on StatusContext { context state targetUrl }
 } } } } } }`
const loopPullDetailFields = `author { login } title body additions deletions changedFiles
 files(first:100) { pageInfo { hasNextPage endCursor } nodes { path } }
 closingIssuesReferences(first:100) { pageInfo { hasNextPage endCursor } nodes { url } }
 reviews(first:100) { pageInfo { hasNextPage endCursor } nodes { id author { login } authorAssociation body state submittedAt commit { oid } } }`

// ReadLoopPullBatch uses one light query (including details on first sight),
// followed only when needed by one query for heads/updatedAt that moved. Sixty
// ordinary PRs therefore cost at most two requests, including a cold start.
var ReadLoopPullBatch = FetchLoopPullBatch

func FetchLoopPullBatch(ctx context.Context, refs []string, previous map[string]WatchedLoopPull, floor int) (map[string]WatchedLoopPull, error) {
	result := map[string]WatchedLoopPull{}
	for start := 0; start < len(refs); start += loopPullBatchSize {
		chunk := refs[start:min(start+loopPullBatchSize, len(refs))]
		details := map[string]bool{}
		for _, ref := range chunk {
			details[ref] = pullNeedsDetails(previous, ref)
		}
		first, err := queryLoopPullBatch(ctx, chunk, details, floor)
		if err != nil {
			return nil, err
		}
		var moved []string
		for _, ref := range chunk {
			p := first[ref]
			old, exists := previous[ref]
			if exists && !details[ref] && p.Pull.State != "" && (p.Pull.Head != old.Pull.Head || !p.Pull.Updated.Equal(old.Pull.Updated)) {
				moved = append(moved, ref)
			}
			if exists && !details[ref] && p.Pull.State != "" && !slices.Contains(moved, ref) {
				p.Pull = combineLoopPull(p.Pull, old.Pull)
			}
			result[ref] = p
		}
		if len(moved) > 0 {
			for _, ref := range moved {
				details[ref] = true
			}
			fresh, err := queryLoopPullBatch(ctx, moved, details, floor)
			if err != nil {
				return nil, err
			}
			for ref, p := range fresh {
				result[ref] = p
			}
		}
	}
	return result, nil
}

func combineLoopPull(light, detail superloop.Pull) superloop.Pull {
	detail.State, detail.URL, detail.Head, detail.Base, detail.Branch = light.State, light.URL, light.Head, light.Base, light.Branch
	detail.Updated, detail.Draft, detail.Mergeable, detail.MergeState = light.Updated, light.Draft, light.Mergeable, light.MergeState
	detail.ReviewDecision, detail.MergeCommit = light.ReviewDecision, light.MergeCommit
	detail.Green, detail.Red, detail.Checks = light.Green, light.Red, light.Checks
	detail.CheckStates = light.CheckStates
	return detail
}

func queryLoopPullBatch(ctx context.Context, refs []string, details map[string]bool, floor int) (map[string]WatchedLoopPull, error) {
	var q strings.Builder
	q.WriteString("query { rateLimit { remaining resetAt cost }")
	for i, ref := range refs {
		r, ok := Cmd.ParseRef(ref)
		if !ok {
			return nil, fmt.Errorf("invalid pull reference %q", ref)
		}
		owner, repo, _ := strings.Cut(r.Repo, "/")
		fmt.Fprintf(&q, " p%d: repository(owner:%q,name:%q) { pullRequest(number:%d) { %s", i, owner, repo, r.N, loopPullLightFields)
		if details[ref] {
			q.WriteString(loopPullDetailFields)
		}
		q.WriteString(" } }")
	}
	q.WriteString(" }")
	data, err := runLoopPullGraphQL(ctx, q.String(), floor)
	if err != nil {
		return nil, err
	}
	out := map[string]WatchedLoopPull{}
	for i, ref := range refs {
		var repo struct {
			Pull json.RawMessage `json:"pullRequest"`
		}
		if err := json.Unmarshal(data[fmt.Sprintf("p%d", i)], &repo); err != nil {
			return nil, err
		}
		expanded, expandErr := expandLoopPullConnections(ctx, ref, repo.Pull, details[ref], floor)
		p, pending, err := parseWatchedLoopPull(ref, expanded, details[ref])
		err = errors.Join(expandErr, err)
		if err != nil {
			out[ref] = WatchedLoopPull{Pull: superloop.Pull{Ref: ref}, At: clk.Now(), Error: err.Error()}
			continue
		}
		out[ref] = WatchedLoopPull{Pull: p, Pending: pending, At: clk.Now()}
	}
	return out, nil
}

func parseWatchedLoopPull(ref string, raw []byte, details bool) (superloop.Pull, bool, error) {
	if len(raw) == 0 || string(raw) == JsonNullLiteral {
		return superloop.Pull{Ref: ref}, false, nil
	}
	// Normalize GraphQL connections to gh pr view's representation so check,
	// review and ownership interpretation have a single implementation.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return superloop.Pull{}, false, err
	}
	for _, name := range []string{"files", "reviews", "closingIssuesReferences"} {
		if !details {
			delete(fields, name)
			continue
		}
		if len(fields[name]) == 0 || fields[name][0] == '[' {
			continue
		}
		var connection struct {
			Nodes    json.RawMessage
			PageInfo struct{ HasNextPage bool }
		}
		if err := json.Unmarshal(fields[name], &connection); err != nil {
			return superloop.Pull{}, false, err
		}
		if name != "files" && connection.PageInfo.HasNextPage {
			return superloop.Pull{}, false, fmt.Errorf("%s %s truncated; observation unknown", ref, name)
		}
		fields[name] = connection.Nodes
	}
	var checks []LoopCheck
	var suites *loopWatchedSuites
	pending := true
	switch {
	case len(fields["commits"]) > 0 && fields["commits"][0] == '{':
		var commits struct {
			Nodes []struct {
				Commit struct {
					Suites *loopWatchedSuites `json:"checkSuites"`
					Rollup *struct {
						State    string
						Contexts struct {
							PageInfo struct{ HasNextPage bool }
							Nodes    []struct {
								LoopCheck
								CheckSuite struct {
									WorkflowRun struct{ Workflow struct{ Name string } }
								}
							}
						}
					} `json:"statusCheckRollup"`
				}
			}
		}
		if err := json.Unmarshal(fields["commits"], &commits); err != nil {
			return superloop.Pull{}, false, err
		}
		if len(commits.Nodes) != 1 || commits.Nodes[0].Commit.Suites == nil {
			return superloop.Pull{}, false, errors.New("workflow suites missing; observation unknown")
		}
		suites = commits.Nodes[0].Commit.Suites
		if len(commits.Nodes) > 0 && commits.Nodes[0].Commit.Rollup != nil {
			rollup := commits.Nodes[0].Commit.Rollup
			if rollup.Contexts.PageInfo.HasNextPage {
				return superloop.Pull{}, false, errors.New("check rollup truncated; observation unknown")
			}
			pending = len(rollup.Contexts.Nodes) == 0
			for _, node := range rollup.Contexts.Nodes {
				c := node.LoopCheck
				c.WorkflowName = node.CheckSuite.WorkflowRun.Workflow.Name
				checks = append(checks, c)
				if !c.Concluded() {
					pending = true
				}
			}
		}
		fields["statusCheckRollup"], _ = json.Marshal(checks)
	case len(fields["statusCheckRollup"]) > 0:
		if err := json.Unmarshal(fields["statusCheckRollup"], &checks); err != nil {
			return superloop.Pull{}, false, err
		}
		pending = len(checks) == 0 || slices.ContainsFunc(checks, func(c LoopCheck) bool { return !c.Concluded() })
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return superloop.Pull{}, false, err
	}
	p, err := ParseLoopPull(ref, normalized)
	if err == nil && p.State == superloop.Open && suites != nil {
		err = suites.apply(&p, checks)
		pending = pending || !p.Green && len(p.Red) == 0
	}
	return p, pending, err
}

// Check suites expose workflows which never created a job. Read them in the
// same batch as the rollup, on every poll (CI does not change PR updatedAt).
type loopWatchedSuites struct {
	PageInfo struct{ HasNextPage bool }
	Nodes    []struct {
		Status, Conclusion string
		WorkflowRun        *struct {
			DatabaseID uint64
			RunAttempt int
			URL        string
			Workflow   struct {
				DatabaseID uint64
				Name       string
			}
		}
	}
}

func (s *loopWatchedSuites) apply(p *superloop.Pull, checks []LoopCheck) error {
	if s.PageInfo.HasNextPage || s.Nodes == nil {
		return errors.New("workflow suites incomplete; observation unknown")
	}
	runs := make([]LoopWorkflowRun, 0, len(s.Nodes))
	for _, suite := range s.Nodes {
		run := suite.WorkflowRun
		if run == nil {
			continue // External checks are already represented by the rollup.
		}
		runs = append(runs, LoopWorkflowRun{
			ID: run.DatabaseID, WorkflowID: run.Workflow.DatabaseID,
			Attempt: run.RunAttempt, Head: p.Head, Name: run.Workflow.Name,
			URL: run.URL, Status: suite.Status, Conclusion: suite.Conclusion,
		})
	}
	raw, err := json.Marshal(map[string]any{"workflow_runs": runs})
	if err != nil {
		return err
	}
	return ApplyLoopWorkflowRuns(p, raw, checks)
}

// Discovery shares the watcher's timer; a slow listing cannot stall a pass.
var ReadLoopSweep = func(l *Loop, _ context.Context, _ superloop.Input) {
	s := l.PullWatcher().View()
	l.OpenPulls, l.OpenPullsAt, l.SweepIncomplete = s.Open, s.ListedAt, s.Incomplete
}

type loopPullContextKey struct{}

func readLoopContextPull(ctx context.Context, ref string) (superloop.Pull, error) {
	if l, ok := ctx.Value(loopPullContextKey{}).(*Loop); ok {
		return l.readPull(ctx, ref)
	}
	return ReadLoopPull(ctx, ref)
}

func (s LoopPullSnapshot) AgeLine(now time.Time) string {
	var oldest time.Time
	count, stale := 0, 0
	for _, seen := range s.Pulls {
		if seen.Pull.State == superloop.Merged || seen.Pull.State == superloop.Closed {
			continue
		}
		count++
		if oldest.IsZero() || seen.At.Before(oldest) {
			oldest = seen.At
		}
		if seen.At.IsZero() || now.Sub(seen.At) > LoopPullMaxAge {
			stale++
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("pull observations: %d, oldest %s, %d stale (limit %s)", count, max(0, now.Sub(oldest)).Round(time.Second), stale, LoopPullMaxAge)
}

// FailingLine says the watch's episode of failed polls, "" while it reads.
func (s LoopPullSnapshot) FailingLine(now time.Time) string {
	f := s.Failing
	if f == nil {
		return ""
	}
	read := "never"
	if !s.PolledAt.IsZero() {
		read = max(0, now.Sub(s.PolledAt)).Round(time.Second).String() + " ago"
	}
	return fmt.Sprintf("pull watch failing since %s, %d tries (last complete poll %s): %s", f.Since.UTC().Format(time.RFC3339), f.Tries, read, f.Error)
}

// runLoopPullGraphQL asks GitHub one pull batch. What GitHub fails to
// answer is a PullBatchFault: a 5xx or no answer in time after gh's tries,
// GraphQL's own timeout, and a body that is not JSON, which names gh's
// error (with its HTTP status) and the body's start rather than the JSON
// decoder's "invalid character '<'" (#3809).
func runLoopPullGraphQL(ctx context.Context, query string, floor int) (map[string]json.RawMessage, error) {
	raw, err := github.RunGH(ctx, workerGitHubAPIGroup, inventoryGraphQLCommand, "-f", "query="+query)
	if body := bytes.TrimSpace(raw); len(body) > 0 && !json.Valid(body) {
		return nil, &PullBatchFault{Err: fmt.Errorf("pull batch: GitHub answered no JSON (%q): %w", bodyHead(body), cmp.Or(err, errNoJSON))}
	}
	if err != nil && len(raw) == 0 {
		return nil, pullBatchErr(err)
	}
	var response struct {
		Data   map[string]json.RawMessage
		Errors []struct{ Message, Type string }
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	for _, e := range response.Errors {
		if e.Type != "NOT_FOUND" {
			err := fmt.Errorf("pull batch: %s", e.Message)
			// "Something went wrong while executing your query. This may
			// be the result of a timeout, …"
			if strings.Contains(strings.ToLower(e.Message), "timeout") {
				return nil, &PullBatchFault{Err: err}
			}
			return nil, err
		}
	}
	if err != nil && len(response.Errors) == 0 {
		return nil, pullBatchErr(err)
	}
	var budget struct{ Remaining int }
	if err := json.Unmarshal(response.Data["rateLimit"], &budget); err != nil {
		return nil, err
	}
	if floor > 0 && budget.Remaining <= floor {
		return nil, fmt.Errorf("pull batch reached github.floor: %w", ErrLoopLater)
	}

	return response.Data, nil
}

// Follow overflow pages only for connections that need them. Most polls use
// none. Never infer green or an approval from a truncated check/review list.
func expandLoopPullConnections(ctx context.Context, ref string, raw []byte, details bool, floor int) ([]byte, error) {
	if len(raw) == 0 || string(raw) == JsonNullLiteral {
		return raw, nil
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, err
	}
	names := []string{PullContexts, PullSuites}
	if details {
		names = append(names, "files", "reviews", "closingIssuesReferences")
	}
	for {
		cursors := map[string]string{}
		for _, name := range names {
			conn := PullConnection(full, name)
			page, _ := conn[PullPageInfo].(map[string]any)
			if next, _ := page[PullHasNext].(bool); next {
				cursor, _ := page[PullEndCursor].(string)
				if cursor == "" {
					return nil, fmt.Errorf("%s has no continuation cursor", name)
				}
				cursors[name] = cursor
			}
		}
		if len(cursors) == 0 {
			return json.Marshal(full)
		}
		fields := loopPullLightFields
		if details {
			fields += loopPullDetailFields
		}
		for name, cursor := range cursors {
			fields = strings.Replace(fields, name+"(first:100)", fmt.Sprintf("%s(first:100,after:%q)", name, cursor), 1)
		}
		r, _ := Cmd.ParseRef(ref)
		owner, repo, _ := strings.Cut(r.Repo, "/")
		query := fmt.Sprintf("query { rateLimit { remaining } p0: repository(owner:%q,name:%q) { pullRequest(number:%d) { %s } } }", owner, repo, r.N, fields)
		data, err := runLoopPullGraphQL(ctx, query, floor)
		if err != nil {
			return nil, err
		}
		var wrapper struct {
			Pull map[string]any `json:"pullRequest"`
		}
		if err := json.Unmarshal(data["p0"], &wrapper); err != nil {
			return nil, err
		}
		if wrapper.Pull == nil || full[PullHeadField] != wrapper.Pull[PullHeadField] || full[pullUpdatedField] != wrapper.Pull[pullUpdatedField] {
			return nil, fmt.Errorf("%s moved during pagination; retry next poll", ref)
		}
		for name, cursor := range cursors {
			from, to := PullConnection(wrapper.Pull, name), PullConnection(full, name)
			if from == nil {
				return nil, fmt.Errorf("%s missing continuation", name)
			}
			page, _ := from[PullPageInfo].(map[string]any)
			if next, _ := page[PullHasNext].(bool); next && page[PullEndCursor] == cursor {
				return nil, fmt.Errorf("%s repeated continuation cursor", name)
			}
			nodes, _ := to[PullNodes].([]any)
			more, _ := from[PullNodes].([]any)
			to[PullNodes], to[PullPageInfo] = append(nodes, more...), page
		}
	}
}

func PullConnection(p map[string]any, name string) map[string]any {
	if name != PullContexts && name != PullSuites {
		c, _ := p[name].(map[string]any)
		return c
	}
	commits, _ := p["commits"].(map[string]any)
	nodes, _ := commits[PullNodes].([]any)
	if len(nodes) == 0 {
		return nil
	}
	node, _ := nodes[0].(map[string]any)
	commit, _ := node["commit"].(map[string]any)
	if name == PullSuites {
		suites, _ := commit[PullSuites].(map[string]any)
		return suites
	}
	rollup, _ := commit["statusCheckRollup"].(map[string]any)
	contexts, _ := rollup[PullContexts].(map[string]any)
	return contexts
}

func (w *loopPullWatcher) knows(ref string) bool {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	_, observed := w.Snapshot.Pulls[ref]
	return observed || w.tracked[ref] || w.extra[ref] || slices.Contains(w.Snapshot.Open, ref)
}
