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

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopPullShort     = 30 * time.Second
	loopPullLong      = 2 * time.Minute
	loopPullBackoff   = 10 * time.Minute
	loopPullMaxAge    = 5 * time.Minute
	loopPullBatchSize = 60
	loopPullWatchFile = "supervisor-pull-observations.json"
)

const (
	pullWatchKey     = "pull watcher"
	pullContexts     = "contexts"
	pullNodes        = "nodes"
	pullPageInfo     = "pageInfo"
	pullHasNext      = "hasNextPage"
	pullEndCursor    = "endCursor"
	pullHeadField    = "headRefOid"
	pullUpdatedField = "updatedAt"
	jsonNullLiteral  = "null"
)

// Only the watcher writes these observations. Consumers never renew a read's
// timestamp by reusing it, and never fall back to a synchronous GitHub read.
type watchedLoopPull struct {
	Pull    superloop.Pull `json:"pull"`
	At      time.Time      `json:"at"`
	Pending bool           `json:"pending,omitempty"`
	Error   string         `json:"error,omitempty"`
}

type loopPullSnapshot struct {
	Pulls      map[string]watchedLoopPull `json:"pulls"`
	Open       []string                   `json:"open,omitempty"`
	ListedAt   time.Time                  `json:"listedAt"`
	Incomplete bool                       `json:"incomplete,omitempty"`
}

type loopPullWatcher struct {
	mu       sync.Mutex
	snapshot loopPullSnapshot
	tracked  map[string]bool
	extra    map[string]bool
	changed  chan struct{}
	ready    chan struct{}
	// Configuration is copied by the pass, never read concurrently from app.cfg.
	authors, orgs, never []string
	floor                int
}

func newPullWatcher() *loopPullWatcher {
	return &loopPullWatcher{snapshot: loopPullSnapshot{Pulls: map[string]watchedLoopPull{}}, tracked: map[string]bool{}, extra: map[string]bool{}, changed: make(chan struct{}, 1), ready: make(chan struct{}, 1)}
}

func (l *supervisorLoop) pullWatcher() *loopPullWatcher {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pullWatch == nil {
		l.pullWatch = newPullWatcher()
		var saved loopPullSnapshot
		if ok, err := l.a.store.ReadFile(loopPullWatchFile, &saved); ok && err == nil && saved.Pulls != nil {
			l.pullWatch.snapshot = saved
		}
	}
	return l.pullWatch
}

func (w *loopPullWatcher) view() loopPullSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.snapshot
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

func (w *loopPullWatcher) set(refs, authors, orgs []string, floor int, never []string) {
	w.mu.Lock()
	next := make(map[string]bool, len(refs))
	added := false
	for _, ref := range refs {
		next[ref] = true
		if !w.tracked[ref] {
			added = true
		}
	}
	configChanged := !slices.Equal(w.authors, authors) || !slices.Equal(w.orgs, orgs) || w.floor != floor || !slices.Equal(w.never, never)
	w.tracked, w.authors, w.orgs, w.floor, w.never = next, slices.Clone(authors), slices.Clone(orgs), floor, slices.Clone(never)
	if configChanged {
		w.snapshot.ListedAt = time.Time{}
	}
	w.mu.Unlock()
	if added || configChanged {
		signalPullWatch(w.changed)
	}
}

func (w *loopPullWatcher) read(ref string, now time.Time) (watchedLoopPull, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.snapshot.Pulls[ref]
	if !ok {
		if !w.extra[ref] {
			w.extra[ref] = true
			signalPullWatch(w.changed)
		}
		return p, fmt.Errorf("%s has no watcher observation: %w", ref, errLoopLater)
	}
	if p.Error != "" {
		return p, fmt.Errorf("%s: %s: %w", ref, p.Error, errLoopLater)
	}
	if p.Pull.State == "" {
		return p, fmt.Errorf("%s unavailable at %s: %w", ref, p.At.Format(time.RFC3339), errLoopLater)
	}
	// A terminal observation is final; it no longer consumes a poll slot.
	if p.Pull.State != superloop.Merged && p.Pull.State != superloop.Closed && (p.At.IsZero() || now.Sub(p.At) > loopPullMaxAge) {
		return p, fmt.Errorf("%s observation is %s old (limit %s): %w", ref, now.Sub(p.At).Round(time.Second), loopPullMaxAge, errLoopLater)
	}
	return p, nil
}

// readObservedLoopPull is a seam for planner/action fixtures. Production always
// reads the watcher, including --once and --dry-run, which use its saved snapshot.
var readObservedLoopPull = func(l *supervisorLoop, ctx context.Context, ref string, _ time.Time, _ string) (loopPullObservation, error) {
	if err := ctx.Err(); err != nil {
		return loopPullObservation{}, err
	}
	now := l.a.timeNow()
	if l.a.loopNow != nil {
		now = time.Now()
	}
	p, err := l.pullWatcher().read(ref, now)
	return loopPullObservation{pull: p.Pull, at: p.At}, err
}

func (l *supervisorLoop) readPull(ctx context.Context, ref string) (superloop.Pull, error) {
	p, err := readObservedLoopPull(l, ctx, ref, time.Time{}, "")
	return p.pull, err
}

// startLoopPullWatcher owns one cancellable timer and one fetch at a time. Its
// independent clock does not stop at the beginning of a supervisor pass.
var startLoopPullWatcher = func(ctx context.Context, l *supervisorLoop) func() {
	ctx, cancel := context.WithCancel(ctx)
	w := l.pullWatcher()
	w.set(nil, l.a.cfg.Supervisor.Ownership.Authors, l.a.cfg.GitHub.AllowedOrgs, l.a.cfg.GitHub.Floor, l.a.cfg.Supervisor.Ownership.Never)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		schedule := loopPullSchedule{}
		var lastPoll time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.changed:
				// New work must not defeat rate-limit backoff.
				if schedule.failures > 0 {
					continue
				}
				// Coalesce arrivals from any number of sessions into the same
				// short interval rather than spending a request per new park.
				if delay := time.Until(lastPoll.Add(loopPullShort)); delay > 0 {
					timer.Reset(delay)
					continue
				}
			case <-timer.C:
			}
			lastPoll = time.Now()
			next, err := w.poll(ctx, l)
			if ctx.Err() != nil {
				return
			}
			next = schedule.after(l, next, err)
			timer.Reset(next)
		}
	}()
	return func() { cancel(); <-done }
}

var pollOncePullWatcher = func(ctx context.Context, l *supervisorLoop) {
	var schedule loopPullSchedule
	next, err := l.pullWatcher().poll(ctx, l)
	schedule.after(l, next, err)
}

type loopPullSchedule struct{ failures int }

func (s *loopPullSchedule) after(l *supervisorLoop, next time.Duration, err error) time.Duration {
	if err == nil {
		s.failures = 0
		l.clearSaid(pullWatchKey)
		return next
	}
	s.failures++
	l.sayOnce(pullWatchKey, "SUPERVISOR PULL WATCH held: %v; retrying with backoff up to %s", err, loopPullBackoff)
	return pullWatchRetry(s.failures)
}

func pullWatchRetry(failures int) time.Duration {
	return min(loopPullBackoff, loopPullShort*time.Duration(1<<min(failures, 5)))
}

func (w *loopPullWatcher) poll(ctx context.Context, l *supervisorLoop) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	w.mu.Lock()
	authors, orgs, floor := slices.Clone(w.authors), slices.Clone(w.orgs), w.floor
	desk := superloop.Desk{Orgs: orgs, Never: slices.Clone(w.never)}
	refs := maps.Clone(w.tracked)
	for ref := range w.extra {
		refs[ref] = true
	}
	before := w.snapshot
	before.Pulls, before.Open = maps.Clone(before.Pulls), slices.Clone(before.Open)
	w.mu.Unlock()
	now := time.Now()
	// The beside-running budget watch owns probes. Polling PRs adds no budget
	// probes and never holds the pass's GraphQL admission lock during network IO.
	if floor > 0 {
		st, err := l.a.store.Peek()
		if err != nil {
			return 0, err
		}
		if boardBudgetLow(st.Budget, floor, now) || loopBudgetUnknownOrLow(st.Budget, floor, now) || st.Budget.GraphQL.Remaining < floor+loopGraphQLReserve {
			return 0, fmt.Errorf("GitHub budget unknown or below github.floor: %w", errLoopLater)
		}
	}
	var discoveryErr error
	if before.ListedAt.IsZero() || now.Sub(before.ListedAt) >= sweepListEvery {
		open, err := readOpenPulls(ctx, func() ([]string, error) {
			if len(authors) > 0 {
				return authors, nil
			}
			login, err := readDeskLogin(ctx)
			return []string{login}, err
		}, orgs)
		discoveryErr = err
		if err == nil {
			before.Open, before.ListedAt = open, time.Now()
			before.Incomplete = len(open) >= sweepListLimit
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
	found, err := readLoopPullBatch(ctx, query, before.Pulls, floor)
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
	next := loopPullLong
	for _, p := range found {
		if p.Pending || p.Pull.Mergeable == "UNKNOWN" {
			next = loopPullShort
		}
	}
	w.mu.Lock()
	w.snapshot = before
	for ref := range found {
		delete(w.extra, ref)
	}
	w.mu.Unlock()
	if !l.dryRun {
		_ = l.a.store.WriteFile(loopPullWatchFile, before)
	}
	signalPullWatch(w.ready)
	return next, discoveryErr
}

// Light fields change independently of updatedAt (notably CI and mergeability).
const loopPullLightFields = `state url headRefOid baseRefName headRefName updatedAt isDraft mergeable mergeStateStatus reviewDecision mergeCommit { oid }
 commits(last:1) { nodes { commit { statusCheckRollup { state contexts(first:100) { pageInfo { hasNextPage endCursor } nodes {
 __typename ... on CheckRun { name status conclusion detailsUrl startedAt checkSuite { workflowRun { workflow { name } } } }
 ... on StatusContext { context state targetUrl }
 } } } } } }`
const loopPullDetailFields = `author { login } title body additions deletions changedFiles
 files(first:100) { pageInfo { hasNextPage endCursor } nodes { path } }
 closingIssuesReferences(first:100) { pageInfo { hasNextPage endCursor } nodes { url } }
 reviews(first:100) { pageInfo { hasNextPage endCursor } nodes { id author { login } authorAssociation body state submittedAt commit { oid } } }`

// readLoopPullBatch uses one light query (including details on first sight),
// followed only when needed by one query for heads/updatedAt that moved. Sixty
// ordinary PRs therefore cost at most two requests, including a cold start.
var readLoopPullBatch = fetchLoopPullBatch

func fetchLoopPullBatch(ctx context.Context, refs []string, previous map[string]watchedLoopPull, floor int) (map[string]watchedLoopPull, error) {
	result := map[string]watchedLoopPull{}
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

func queryLoopPullBatch(ctx context.Context, refs []string, details map[string]bool, floor int) (map[string]watchedLoopPull, error) {
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
	out := map[string]watchedLoopPull{}
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
			out[ref] = watchedLoopPull{Pull: superloop.Pull{Ref: ref}, At: time.Now(), Error: err.Error()}
			continue
		}
		out[ref] = watchedLoopPull{Pull: p, Pending: pending, At: time.Now()}
	}
	return out, nil
}

func parseWatchedLoopPull(ref string, raw []byte, details bool) (superloop.Pull, bool, error) {
	if len(raw) == 0 || string(raw) == jsonNullLiteral {
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
	var checks []loopCheck
	pending := true
	switch {
	case len(fields["commits"]) > 0 && fields["commits"][0] == '{':
		var commits struct {
			Nodes []struct {
				Commit struct {
					Rollup *struct {
						State    string
						Contexts struct {
							PageInfo struct{ HasNextPage bool }
							Nodes    []struct {
								loopCheck
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
		if len(commits.Nodes) > 0 && commits.Nodes[0].Commit.Rollup != nil {
			rollup := commits.Nodes[0].Commit.Rollup
			if rollup.Contexts.PageInfo.HasNextPage {
				return superloop.Pull{}, false, errors.New("check rollup truncated; observation unknown")
			}
			pending = len(rollup.Contexts.Nodes) == 0
			for _, node := range rollup.Contexts.Nodes {
				c := node.loopCheck
				c.WorkflowName = node.CheckSuite.WorkflowRun.Workflow.Name
				checks = append(checks, c)
				if !c.concluded() {
					pending = true
				}
			}
		}
		fields["statusCheckRollup"], _ = json.Marshal(checks)
	case len(fields["statusCheckRollup"]) > 0:
		if err := json.Unmarshal(fields["statusCheckRollup"], &checks); err != nil {
			return superloop.Pull{}, false, err
		}
		pending = len(checks) == 0 || slices.ContainsFunc(checks, func(c loopCheck) bool { return !c.concluded() })
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return superloop.Pull{}, false, err
	}
	p, err := parseLoopPull(ref, normalized)
	return p, pending, err
}

// Discovery shares the watcher's timer; a slow listing cannot stall a pass.
var readLoopSweep = func(l *supervisorLoop, _ context.Context, _ superloop.Input) {
	s := l.pullWatcher().view()
	l.openPulls, l.openPullsAt, l.sweepIncomplete = s.Open, s.ListedAt, s.Incomplete
}

type loopPullContextKey struct{}

func readLoopContextPull(ctx context.Context, ref string) (superloop.Pull, error) {
	if l, ok := ctx.Value(loopPullContextKey{}).(*supervisorLoop); ok {
		return l.readPull(ctx, ref)
	}
	return readLoopPull(ctx, ref)
}

func (s loopPullSnapshot) ageLine(now time.Time) string {
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
		if seen.At.IsZero() || now.Sub(seen.At) > loopPullMaxAge {
			stale++
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("pull observations: %d, oldest %s, %d stale (limit %s)", count, max(0, now.Sub(oldest)).Round(time.Second), stale, loopPullMaxAge)
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
		return nil, fmt.Errorf("pull batch reached github.floor: %w", errLoopLater)
	}

	return response.Data, nil
}

// Follow overflow pages only for connections that need them. Most polls use
// none. Never infer green or an approval from a truncated check/review list.
func expandLoopPullConnections(ctx context.Context, ref string, raw []byte, details bool, floor int) ([]byte, error) {
	if len(raw) == 0 || string(raw) == jsonNullLiteral {
		return raw, nil
	}
	var full map[string]any
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, err
	}
	names := []string{pullContexts}
	if details {
		names = append(names, "files", "reviews", "closingIssuesReferences")
	}
	for {
		cursors := map[string]string{}
		for _, name := range names {
			conn := pullConnection(full, name)
			page, _ := conn[pullPageInfo].(map[string]any)
			if next, _ := page[pullHasNext].(bool); next {
				cursor, _ := page[pullEndCursor].(string)
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
		if wrapper.Pull == nil || full[pullHeadField] != wrapper.Pull[pullHeadField] || full[pullUpdatedField] != wrapper.Pull[pullUpdatedField] {
			return nil, fmt.Errorf("%s moved during pagination; retry next poll", ref)
		}
		for name, cursor := range cursors {
			from, to := pullConnection(wrapper.Pull, name), pullConnection(full, name)
			if from == nil {
				return nil, fmt.Errorf("%s missing continuation", name)
			}
			page, _ := from[pullPageInfo].(map[string]any)
			if next, _ := page[pullHasNext].(bool); next && page[pullEndCursor] == cursor {
				return nil, fmt.Errorf("%s repeated continuation cursor", name)
			}
			nodes, _ := to[pullNodes].([]any)
			more, _ := from[pullNodes].([]any)
			to[pullNodes], to[pullPageInfo] = append(nodes, more...), page
		}
	}
}

func pullConnection(p map[string]any, name string) map[string]any {
	if name != pullContexts {
		c, _ := p[name].(map[string]any)
		return c
	}
	commits, _ := p["commits"].(map[string]any)
	nodes, _ := commits[pullNodes].([]any)
	if len(nodes) == 0 {
		return nil
	}
	node, _ := nodes[0].(map[string]any)
	commit, _ := node["commit"].(map[string]any)
	rollup, _ := commit["statusCheckRollup"].(map[string]any)
	contexts, _ := rollup[pullContexts].(map[string]any)
	return contexts
}

func (w *loopPullWatcher) knows(ref string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, observed := w.snapshot.Pulls[ref]
	return observed || w.tracked[ref] || w.extra[ref] || slices.Contains(w.snapshot.Open, ref)
}
