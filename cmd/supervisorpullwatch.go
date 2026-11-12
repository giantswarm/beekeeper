package cmd

import (
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
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	LoopPullShort     = 30 * time.Second
	LoopPullLong      = 2 * time.Minute
	loopPullBackoff   = 10 * time.Minute
	LoopPullMaxAge    = 5 * time.Minute
	loopPullBatchSize = 60
	LoopPullWatchFile = "supervisor-pull-observations.json"
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
}

type loopPullWatcher struct {
	Mu       sync.Mutex
	Snapshot LoopPullSnapshot
	tracked  map[string]bool
	extra    map[string]bool
	changed  chan struct{}
	ready    chan struct{}
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
	return &loopPullWatcher{Snapshot: LoopPullSnapshot{Pulls: map[string]WatchedLoopPull{}}, tracked: map[string]bool{}, extra: map[string]bool{}, changed: make(chan struct{}, 1), ready: make(chan struct{}, 1)}
}

func (l *Loop) PullWatcher() *loopPullWatcher {
	l.Mu.Lock()
	defer l.Mu.Unlock()
	if l.pullWatch == nil {
		l.pullWatch = newPullWatcher()
		var saved LoopPullSnapshot
		if ok, err := l.Host.store.ReadFile(LoopPullWatchFile, &saved); ok && err == nil && saved.Pulls != nil {
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
	return s
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
			if r, ok := parseRef(ref); ok && desk.BotRepo(r.Repo, teams[strings.ToLower(r.Repo)], known) {
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
	return slices.ContainsFunc(tracked, check) || slices.ContainsFunc(w.Snapshot.Open, check)
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
	now := l.Host.timeNow()
	if l.Host.loopNow != nil {
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
	w.Set(nil, l.Host.cfg.Supervisor.Ownership.Authors, l.Host.cfg.GitHub.AllowedOrgs, l.Host.cfg.GitHub.Floor, l.Host.cfg.Supervisor.Ownership.Never)
	own := l.Host.cfg.Supervisor.Ownership
	w.SetBots(own.Bots, superloop.Desk{Orgs: l.Host.cfg.GitHub.AllowedOrgs, Team: own.Team, Review: own.Review, Never: own.Never}, nil, false)
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

type LoopPullSchedule struct{ Failures int }

func (s *LoopPullSchedule) After(l *Loop, next time.Duration, err error) time.Duration {
	if err == nil {
		s.Failures = 0
		l.ClearSaid(pullWatchKey)
		return next
	}
	s.Failures++
	l.SayOnce(pullWatchKey, "SUPERVISOR PULL WATCH held: %v; retrying with backoff up to %s", err, loopPullBackoff)
	return PullWatchRetry(s.Failures)
}

func PullWatchRetry(failures int) time.Duration {
	return min(loopPullBackoff, LoopPullShort*time.Duration(1<<min(failures, 5)))
}

func (w *loopPullWatcher) Poll(ctx context.Context, l *Loop) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	w.Mu.Lock()
	authors, orgs, floor := slices.Clone(w.authors), slices.Clone(w.orgs), w.floor
	desk := superloop.Desk{Orgs: orgs, Never: slices.Clone(w.never)}
	refs := maps.Clone(w.tracked)
	for ref := range w.extra {
		refs[ref] = true
	}
	before := w.Snapshot
	before.Pulls, before.Open = maps.Clone(before.Pulls), slices.Clone(before.Open)
	w.Mu.Unlock()
	now := clk.Now()
	// The beside-running budget watch owns probes. Polling PRs adds no budget
	// probes and never holds the pass's GraphQL admission lock during network IO.
	if floor > 0 {
		st, err := l.Host.store.Peek()
		if err != nil {
			return 0, err
		}
		if BoardBudgetLow(st.Budget, floor, now) || LoopBudgetUnknownOrLow(st.Budget, floor, now) || st.Budget.GraphQL.Remaining < floor+LoopGraphQLReserve {
			return 0, fmt.Errorf("GitHub budget unknown or below github.floor: %w", ErrLoopLater)
		}
	}
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
		if p, ok := before.Pulls[ref]; ok && (p.Pull.State == superloop.Merged || p.Pull.State == superloop.Closed) {
			continue
		}
		if _, ok := parseRef(ref); ok {
			query = append(query, ref)
		}
	}
	slices.Sort(query)
	found, err := ReadLoopPullBatch(ctx, query, before.Pulls, floor)
	if err != nil {
		return 0, err
	}
	for ref, p := range found {
		before.Pulls[ref] = p
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
	w.Mu.Lock()
	w.Snapshot = before
	for ref := range found {
		delete(w.extra, ref)
	}
	w.Mu.Unlock()
	if !l.DryRun {
		_ = l.Host.store.WriteFile(LoopPullWatchFile, before)
	}
	signalPullWatch(w.ready)
	return next, discoveryErr
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
			_, exists := previous[ref]
			details[ref] = !exists || previous[ref].Pull.State == "" || previous[ref].Error != "" || previous[ref].Pull.Updated.IsZero()
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
	return detail
}

func queryLoopPullBatch(ctx context.Context, refs []string, details map[string]bool, floor int) (map[string]WatchedLoopPull, error) {
	var q strings.Builder
	q.WriteString("query { rateLimit { remaining resetAt cost }")
	for i, ref := range refs {
		r, ok := parseRef(ref)
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

func runLoopPullGraphQL(ctx context.Context, query string, floor int) (map[string]json.RawMessage, error) {
	raw, err := github.RunGH(ctx, workerGitHubAPIGroup, inventoryGraphQLCommand, "-f", "query="+query)
	if err != nil && len(raw) == 0 {
		return nil, err
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
			return nil, fmt.Errorf("pull batch: %s", e.Message)
		}
	}
	if err != nil && len(response.Errors) == 0 {
		return nil, err
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
		r, _ := parseRef(ref)
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
