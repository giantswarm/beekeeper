package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// unmergedGH is the gh CLI readRepoOpenPulls uses; tests replace
// readRepoOpenPulls or unmergedGH.
var unmergedGH github.GH = github.RunGH

// unmergedPull is an open pull request in a repository, read to check
// whether recent unmerged work exceeds supervisor.unmergedLimit or whether an
// open pull request already closes an issue.
type unmergedPull struct {
	Number      int       `json:"number"`
	URL         string    `json:"url"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	IsDraft     bool      `json:"isDraft"`
	HeadRefName string    `json:"headRefName"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Closing     []struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"closingIssuesReferences"`
}

// readRepoOpenPulls lists open pull requests in repo (owner/name). Tests
// replace it to avoid calling gh.
var readRepoOpenPulls = func(ctx context.Context, repo string) ([]unmergedPull, error) {
	out, err := unmergedGH(ctx, "pr", "list", "--repo", repo, "--state", "open", "--limit", "100", "--json", "number,url,state,createdAt,isDraft,headRefName,title,body,closingIssuesReferences")
	if err != nil {
		return nil, err
	}
	var pulls []unmergedPull
	if err := json.Unmarshal(out, &pulls); err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w", repo, err)
	}
	return pulls, nil
}

// openPullClosingIssue checks whether any open non-draft pull request in
// pulls (other than ownBranch or one listed in absorbedPulls) already closes
// any of closingIssues. It returns the matched issue and the existing PR URL.
func openPullClosingIssue(repo string, pulls []unmergedPull, ownBranch string, closingIssues, absorbedPulls []string) (issue, prURL string) {
	if len(closingIssues) == 0 {
		return "", ""
	}
	for _, p := range pulls {
		if p.IsDraft || (p.State != "" && !strings.EqualFold(p.State, github.Open)) {
			continue
		}
		if ownBranch != "" && strings.EqualFold(p.HeadRefName, ownBranch) {
			continue
		}
		prRef := ""
		if p.Number > 0 {
			prRef = fmt.Sprintf("%s#%d", repo, p.Number)
		} else if r, ok := parseRef(p.URL); ok {
			prRef = refName(r)
		}
		if prRef != "" && slices.ContainsFunc(absorbedPulls, func(a string) bool { return strings.EqualFold(a, prRef) }) {
			continue
		}
		closed := superloop.ParseClosingIssues(cmp.Or(prRef, repo+"#0"), p.Title+"\n"+p.Body)
		for _, c := range p.Closing {
			cRepo := cmp.Or(c.Repository.NameWithOwner, repo)
			if cRepo != "" && c.Number > 0 {
				ref := strings.ToLower(fmt.Sprintf("%s#%d", cRepo, c.Number))
				if !slices.ContainsFunc(closed, func(x string) bool { return strings.EqualFold(x, ref) }) {
					closed = append(closed, ref)
				}
			}
		}
		for _, want := range closingIssues {
			if slices.ContainsFunc(closed, func(c string) bool { return strings.EqualFold(c, want) }) {
				url := p.URL
				if url == "" && p.Number > 0 {
					url = fmt.Sprintf("https://github.com/%s/pull/%d", repo, p.Number)
				}
				return want, cmp.Or(url, prRef)
			}
		}
	}
	return "", ""
}

// openPullStartGate refuses an implementation worker start when the target
// repository is at supervisor.unmergedLimit or when one of issues already has
// an open pull request (--force aside).
func (a *app) openPullStartGate(ctx context.Context, name string, issues []string, isReview, force, dryRun bool) error {
	if isReview || len(issues) == 0 {
		return nil
	}
	limit, window := a.unmergedPolicy()
	now := a.timeNow()
	byRepo := map[string][]unmergedPull{}
	for _, issue := range issues {
		r, ok := parseRef(issue)
		if !ok {
			continue
		}
		key := strings.ToLower(r.Repo)
		pulls, seen := byRepo[key]
		if !seen {
			var err error
			pulls, err = readRepoOpenPulls(ctx, r.Repo)
			if err != nil {
				continue
			}
			byRepo[key] = pulls
			if why := unmergedHoldReason(r.Repo, recentUnmergedPulls(r.Repo, pulls, now, window), limit, window); why != "" {
				if !force {
					return refused("%s; --force starts anyway", why)
				}
				if dryRun {
					_, _ = fmt.Fprintf(a.out, "dry run over %s (--force)\n", why)
				}
			}
		}
		if matched, prURL := openPullClosingIssue(r.Repo, pulls, "", []string{refName(r)}, nil); matched != "" {
			if !force {
				return refused("%s already has an open pull request %s; update %s or combine with it instead of starting a second worker (--force starts anyway)", issue, prURL, prURL)
			}
			if dryRun {
				_, _ = fmt.Fprintf(a.out, "dry run over open pull request %s on %s (--force)\n", prURL, issue)
			} else if by, err := a.caller(); err == nil && a.store != nil {
				_ = a.store.Record(about(event(by, verbStart, "%s: started over open pull request %s on %s (--force)", name, prURL, issue), issue))
			}
		}
	}
	return nil
}

// recordSessionIssues records session as serving each of issues in st.Records
// without dropping existing active issue records of the same session, so
// multi-issue and bulk runs claim every issue they file or open a PR for.
func recordSessionIssues(st *state.State, session, by state.Party, at time.Time, issues []string) []state.Event {
	if session.Session == "" && session.HostSession == "" && session.Name == "" {
		return nil
	}
	who := servedBy(st, session)
	var evs []state.Event
	for _, raw := range issues {
		r, ok := parseRef(raw)
		if !ok {
			continue
		}
		issue := refName(r)
		if slices.ContainsFunc(st.Records, func(rec state.Record) bool {
			return rec.Session.Is(who) && rec.Ended.IsZero() && strings.EqualFold(rec.Issue, issue)
		}) {
			continue
		}
		rec := state.Record{Session: who, Issue: issue, By: by, At: at.UTC()}
		st.Records = append(st.Records, rec)
		evs = append(evs, about(event(by, verbServe, "%s: %s", rec.Session.Name, recordText(rec)), rec.Issue))
	}
	return evs
}

// recentUnmergedPulls filters pulls to non-draft open pull requests created
// within window before now, sorted oldest first.
func recentUnmergedPulls(repo string, pulls []unmergedPull, now time.Time, window time.Duration) []unmergedPull {
	if window <= 0 {
		return nil
	}
	cutoff := now.Add(-window)
	var out []unmergedPull
	for _, p := range pulls {
		if p.IsDraft || p.CreatedAt.IsZero() || p.CreatedAt.Before(cutoff) {
			continue
		}
		if p.URL == "" && p.Number > 0 {
			p.URL = fmt.Sprintf("https://github.com/%s/pull/%d", repo, p.Number)
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b unmergedPull) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.Number, b.Number))
	})
	return out
}

// unmergedHoldReason formats the hold-back reason when recent meets or
// exceeds limit, or "" otherwise.
func unmergedHoldReason(repo string, recent []unmergedPull, limit int, window time.Duration) string {
	if limit <= 0 || window <= 0 || len(recent) < limit {
		return ""
	}
	oldest := recent[0].URL
	newest := recent[len(recent)-1].URL
	span := oldest
	if newest != "" && newest != oldest {
		span = oldest + " .. " + newest
	}
	return fmt.Sprintf("held: %s has %d unmerged pull requests opened in the last %s (limit %d: %s)", repo, len(recent), unmergedWindowLabel(window), limit, span)
}

func unmergedWindowLabel(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "1d"
		}
		return fmt.Sprintf("%dd", days)
	}
	return d.String()
}

func (a *app) unmergedPolicy() (limit int, window time.Duration) {
	limit = a.cfg.Supervisor.UnmergedLimit
	if limit == 0 {
		limit = config.DefaultUnmergedLimit
	}
	window = a.cfg.Supervisor.UnmergedWindow.Duration
	if window == 0 {
		window = config.DefaultUnmergedWindow
	}
	return limit, window
}

// skipUnmergedBacklog walks the picks of pick in order and marks candidates
// in a repository whose recent open pull requests meet or exceed
// supervisor.unmergedLimit skipped, advancing board next to other
// repositories. A failed read warns once and leaves the pick standing.
func (a *app) skipUnmergedBacklog(ctx context.Context, warn io.Writer, cands []board.Candidate, pick func() *board.Candidate) {
	limit, window := a.unmergedPolicy()
	if limit <= 0 || window <= 0 {
		return
	}
	now := a.timeNow()
	checked := map[string]string{}
	failed := map[string]bool{}
	for {
		p := pick()
		if p == nil {
			return
		}
		r, ok := parseRef(p.Ref)
		if !ok {
			return
		}
		repoKey := strings.ToLower(r.Repo)
		if failed[repoKey] {
			return
		}
		reason, seen := checked[repoKey]
		if !seen {
			pulls, err := readRepoOpenPulls(ctx, r.Repo)
			if err != nil {
				failed[repoKey] = true
				_, _ = fmt.Fprintf(warn, "beekeeper: unmerged pull requests unread for %s: %v\n", r.Repo, err)
				return
			}
			recent := recentUnmergedPulls(r.Repo, pulls, now, window)
			reason = unmergedHoldReason(r.Repo, recent, limit, window)
			checked[repoKey] = reason
		}
		if reason == "" {
			return
		}
		marked := false
		for i := range cands {
			if cands[i].Skip != "" {
				continue
			}
			if cr, ok := parseRef(cands[i].Ref); ok && strings.EqualFold(cr.Repo, r.Repo) {
				cands[i].Skip = reason
				marked = true
			}
		}
		if !marked {
			return
		}
	}
}

const unmergedCacheTTL = 5 * time.Minute

type loopUnmergedObservation struct {
	recent []unmergedPull
	at     time.Time
}

func (l *supervisorLoop) repoRecentUnmerged(ctx context.Context, repo string) ([]unmergedPull, string, error) {
	limit, window := l.a.unmergedPolicy()
	if limit <= 0 || window <= 0 {
		return nil, "", nil
	}
	now := l.a.timeNow()
	key := strings.ToLower(repo)
	l.mu.Lock()
	obs, ok := l.unmerged[key]
	l.mu.Unlock()
	if !ok || now.Sub(obs.at) >= unmergedCacheTTL {
		pulls, err := readRepoOpenPulls(ctx, repo)
		if err != nil {
			return nil, "", err
		}
		recent := recentUnmergedPulls(repo, pulls, now, window)
		obs = loopUnmergedObservation{recent: recent, at: now}
		l.mu.Lock()
		if l.unmerged == nil {
			l.unmerged = map[string]loopUnmergedObservation{}
		}
		l.unmerged[key] = obs
		l.mu.Unlock()
	}
	return obs.recent, unmergedHoldReason(repo, obs.recent, limit, window), nil
}

// escalateUnmergedBacklog files a guide problem once per backlog episode when
// repo has at least supervisor.unmergedLimit open pull requests in
// supervisor.unmergedWindow, and clears the episode once the count drops
// below the limit.
func (l *supervisorLoop) escalateUnmergedBacklog(repo string, recent []unmergedPull, reason string) error {
	stage := "unmerged " + strings.ToLower(repo)
	l.mu.Lock()
	staged := l.stages[stage]
	l.mu.Unlock()
	if reason == "" {
		if staged && !l.dryRun {
			l.mu.Lock()
			delete(l.stages, stage)
			delete(l.stageDetail, stage)
			l.mu.Unlock()
			return l.a.store.Record(about(event(loopParty, loopUnstageVerb, "[%s] below limit", stage), repo))
		}
		return nil
	}
	if staged || l.dryRun {
		return nil
	}
	limit, window := l.a.unmergedPolicy()
	now := l.a.timeNow()
	urls := make([]string, 0, len(recent))
	for _, p := range recent {
		if p.URL != "" {
			urls = append(urls, p.URL)
		}
	}
	text := fmt.Sprintf("Repository %s has %d open pull requests created in the last %s (limit %d): %s. "+
		"New board work in %s is held back until reviews and merges drain the backlog below %d.",
		repo, len(recent), unmergedWindowLabel(window), limit, strings.Join(urls, ", "), repo, limit)
	line := fmt.Sprintf("%s reported at %s%s%s", loopParty.Name, now.UTC().Format(time.RFC3339), fileReportSeparator, text)
	fileKey := fmt.Sprintf("%s %s", stage, now.UTC().Format("20060102T150405Z"))
	l.mu.Lock()
	if l.stages == nil {
		l.stages = map[string]bool{}
	}
	if l.stageDetail == nil {
		l.stageDetail = map[string]string{}
	}
	l.stages[stage] = true
	l.stageDetail[stage] = fileKey
	l.mu.Unlock()
	if err := l.a.store.Record(about(event(loopParty, loopStageVerb, "[%s] %s", stage, fileKey), repo)); err != nil {
		return err
	}
	return l.record(superloop.Action{Kind: superloop.KindFile, Key: fileKey, Ref: repo}, line)
}

// unmergedStart checks whether nextRef's repository is held back by an
// unmerged pull request backlog, escalating to the guide once per episode.
func (l *supervisorLoop) unmergedStart(ctx context.Context, nextRef string) error {
	r, ok := parseRef(nextRef)
	if !ok {
		return nil
	}
	recent, why, err := l.repoRecentUnmerged(ctx, r.Repo)
	if err != nil {
		l.sayOnce("unmerged read "+strings.ToLower(r.Repo), "SUPERVISOR LOOP cannot read open pull requests for %s: %v", r.Repo, err)
		return nil
	}
	l.clearSaid("unmerged read " + strings.ToLower(r.Repo))
	if err := l.escalateUnmergedBacklog(r.Repo, recent, why); err != nil {
		return err
	}
	if why != "" {
		l.sayOnce("unmerged "+strings.ToLower(r.Repo), "SUPERVISOR START WAITS %s: %s", nextRef, why)
		return errLoopLater
	}
	l.clearSaid("unmerged " + strings.ToLower(r.Repo))
	return nil
}

// checkUnmergedBacklogs inspects active repositories in the pass so a
// repository that accumulates supervisor.unmergedLimit open pull requests in
// supervisor.unmergedWindow escalates to the guide even when board next skips
// it before startItem, and clears its stage once the backlog drains.
func (l *supervisorLoop) checkUnmergedBacklogs(ctx context.Context, in superloop.Input) error {
	limit, window := l.a.unmergedPolicy()
	if limit <= 0 || window <= 0 {
		return nil
	}
	repos := map[string]string{}
	addRef := func(ref string) {
		if r, ok := parseRef(ref); ok && l.unreadRepo(ref) == "" {
			repos[strings.ToLower(r.Repo)] = r.Repo
		}
	}
	for ref, p := range in.Pulls {
		if p.State == superloop.Open {
			addRef(ref)
		}
	}
	l.mu.Lock()
	for _, ref := range l.openPulls {
		addRef(ref)
	}
	for stage := range l.stages {
		if repo, ok := strings.CutPrefix(stage, "unmerged "); ok && repo != "" {
			repos[strings.ToLower(repo)] = repo
		}
	}
	l.mu.Unlock()
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for k := range repos {
			if !yield(k) {
				return
			}
		}
	}) {
		repo := repos[key]
		recent, why, err := l.repoRecentUnmerged(ctx, repo)
		if err != nil {
			continue
		}
		if err := l.escalateUnmergedBacklog(repo, recent, why); err != nil {
			return err
		}
	}
	return nil
}
