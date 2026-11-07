package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
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

const (
	unmergedPullPageLimit = 100
	unmergedPullFields    = "number,url,state,createdAt,isDraft,headRefName,title,body,closingIssuesReferences"
)

// readRepoOpenPullsWithGH lists open non-draft pull requests in repo
// (owner/name) through gh, paginating by createdAt when a page hits the
// 100-item limit so repositories with more than 100 open pull requests are
// inspected in full.
func readRepoOpenPullsWithGH(ctx context.Context, gh github.GH, repo string) ([]unmergedPull, error) {
	search := "draft:false"
	seen := map[string]bool{}
	var all []unmergedPull
	limitArg := strconv.Itoa(unmergedPullPageLimit)
	for range 20 {
		out, err := gh(ctx, "pr", "list", "--repo", repo, "--state", "open", "--search", search, "--limit", limitArg, "--json", unmergedPullFields)
		if err != nil {
			return nil, err
		}
		var page []unmergedPull
		if err := json.Unmarshal(out, &page); err != nil {
			return nil, fmt.Errorf("gh pr list %s: %w", repo, err)
		}
		added := 0
		var oldest time.Time
		for _, p := range page {
			if !p.CreatedAt.IsZero() && (oldest.IsZero() || p.CreatedAt.Before(oldest)) {
				oldest = p.CreatedAt
			}
			key := p.URL
			if p.Number > 0 {
				key = fmt.Sprintf("%s#%d", strings.ToLower(repo), p.Number)
			}
			if key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			all = append(all, p)
			added++
		}
		if len(page) < unmergedPullPageLimit || oldest.IsZero() {
			break
		}
		if added == 0 {
			strictSearch := "draft:false created:<" + oldest.UTC().Format(time.RFC3339)
			if search == strictSearch {
				break
			}
			search = strictSearch
			continue
		}
		search = "draft:false created:<=" + oldest.UTC().Format(time.RFC3339)
	}
	return all, nil
}

// readRepoOpenPulls lists open pull requests in repo (owner/name). Tests
// replace it to avoid calling gh.
var readRepoOpenPulls = func(ctx context.Context, repo string) ([]unmergedPull, error) {
	return readRepoOpenPullsWithGH(ctx, unmergedGH, repo)
}

// findOpenPullClosingIssues checks pulls for an open non-draft pull request
// closing any of closingIssues, and when pulls hit the single-page limit,
// queries gh per issue number so timestamp ties cannot hide an older open PR.
func findOpenPullClosingIssues(ctx context.Context, gh github.GH, repo string, pulls []unmergedPull, ownBranch string, closingIssues, absorbedPulls []string) (issue, prURL string, err error) {
	if matched, url := openPullClosingIssue(repo, pulls, ownBranch, closingIssues, absorbedPulls); matched != "" {
		return matched, url, nil
	}
	if len(pulls) < unmergedPullPageLimit || gh == nil {
		return "", "", nil
	}
	for _, want := range closingIssues {
		r, ok := parseRef(want)
		if !ok || !strings.EqualFold(r.Repo, repo) {
			continue
		}
		out, ghErr := gh(ctx, "pr", "list", "--repo", repo, "--state", "open", "--search", fmt.Sprintf("draft:false %d", r.N), "--limit", strconv.Itoa(unmergedPullPageLimit), "--json", unmergedPullFields)
		if ghErr != nil {
			return "", "", ghErr
		}
		var searched []unmergedPull
		if err := json.Unmarshal(out, &searched); err != nil {
			return "", "", fmt.Errorf("gh pr list %s: %w", repo, err)
		}
		if matched, url := openPullClosingIssue(repo, searched, ownBranch, []string{want}, absorbedPulls); matched != "" {
			return matched, url, nil
		}
	}
	return "", "", nil
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

// incidentExempt reports whether st has an active incident hold on the
// repository of one of refs that explicitly exempts that ref (--except).
func incidentExempt(st *state.State, now time.Time, refs ...string) bool {
	if st == nil || len(refs) == 0 {
		return false
	}
	for _, raw := range refs {
		r, ok := parseRef(raw)
		if !ok {
			continue
		}
		target := refName(r)
		for _, h := range st.Holds {
			if !h.Incident || !h.Active(now) || !strings.EqualFold(h.Target, r.Repo) || h.Except == "" {
				continue
			}
			if er, ok := parseRef(h.Except); ok && strings.EqualFold(refName(er), target) {
				return true
			}
		}
	}
	return false
}

// isTrackedRepairPull reports whether agent name is an authenticated parked
// owner of prRef on the desk.
func isTrackedRepairPull(st *state.State, name, prRef string) bool {
	if st == nil || name == "" || prRef == "" {
		return false
	}
	if rp, ok := st.ReviewParkFor(prRef); ok && strings.EqualFold(rp.Agent, name) {
		return true
	}
	if slices.ContainsFunc(st.Parks, func(p state.Park) bool {
		return strings.EqualFold(p.On, prRef) && strings.EqualFold(p.By.Name, name)
	}) {
		return true
	}
	return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
		return strings.EqualFold(ag.Name, name) && ag.Park != nil && strings.EqualFold(ag.Park.On, prRef)
	})
}

func servedIssuesForPull(st *state.State, prRef string) []string {
	if st == nil || prRef == "" {
		return nil
	}
	var out []string
	add := func(raw string) {
		r, ok := parseRef(raw)
		if !ok || strings.EqualFold(refName(r), prRef) {
			return
		}
		ref := refName(r)
		if !slices.ContainsFunc(out, func(e string) bool { return strings.EqualFold(e, ref) }) {
			out = append(out, ref)
		}
	}
	if rp, ok := st.ReviewParkFor(prRef); ok && rp.Serves != "" {
		add(rp.Serves)
	}
	for _, ag := range st.Agents {
		if ag.Park != nil && strings.EqualFold(ag.Park.On, prRef) {
			if s := cmp.Or(loopServes(st, ag), loopTaskServes(ag, prRef)); s != "" {
				add(s)
			}
		}
	}
	return out
}

// startGateExemptions determines which specific targets and pull requests are
// exempt from the unmerged-backlog hold and self-duplicate PR refusal because
// the start is the named incident repair or an authenticated repair/fixer
// continuation of an existing open pull request.
func (a *app) startGateExemptions(name, brief string, issues []string, now time.Time) (exemptTargets map[string]bool, repairPulls, exemptPulls []string) {
	exemptTargets = map[string]bool{}
	addExemptTarget := func(raw string) {
		r, ok := parseRef(raw)
		if !ok {
			return
		}
		exemptTargets[strings.ToLower(refName(r))] = true
	}
	addPull := func(raw string) {
		r, ok := parseRef(raw)
		if !ok {
			return
		}
		ref := refName(r)
		if !slices.ContainsFunc(exemptPulls, func(e string) bool { return strings.EqualFold(e, ref) }) {
			exemptPulls = append(exemptPulls, ref)
		}
	}
	var st *state.State
	if a.store != nil {
		st, _ = a.store.Read()
	}
	addRepairPull := func(raw string) {
		r, ok := parseRef(raw)
		if !ok {
			return
		}
		ref := refName(r)
		addExemptTarget(ref)
		addPull(ref)
		if !slices.ContainsFunc(repairPulls, func(e string) bool { return strings.EqualFold(e, ref) }) {
			repairPulls = append(repairPulls, ref)
		}
		for _, served := range servedIssuesForPull(st, ref) {
			addExemptTarget(served)
		}
	}

	if a.store != nil {
		if evs, err := a.store.Events(0, func(e state.Event) bool {
			return e.By.Name == loopParty.Name && (e.Verb == loopClaimVerb || e.Verb == loopClaimEndVerb)
		}); err == nil {
			settled := map[string]bool{}
			for i := len(evs) - 1; i >= 0; i-- {
				e := evs[i]
				key, agent, ok := keyed(e.Detail)
				if !ok {
					continue
				}
				if e.Verb == loopClaimEndVerb {
					settled[key] = true
					continue
				}
				if settled[key] || !strings.EqualFold(agent, name) {
					continue
				}
				if !strings.HasPrefix(key, "next") && !strings.HasPrefix(key, "fill ") && !strings.HasPrefix(key, "review ") {
					addRepairPull(e.Subject)
				}
			}
		}
	}

	pullsInBrief := briefPulls(brief)
	allRefs := append(slices.Clone(issues), pullsInBrief...)
	for _, ref := range allRefs {
		if incidentExempt(st, now, ref) {
			addRepairPull(ref)
		}
	}
	for _, pr := range pullsInBrief {
		if isTrackedRepairPull(st, name, pr) {
			addRepairPull(pr)
		}
	}
	for _, iss := range issues {
		if ir, ok := parseRef(iss); ok {
			for _, ab := range superloop.ParseAbsorbedPulls(ir.Repo+"#0", "", brief) {
				addPull(ab)
			}
		}
	}
	return exemptTargets, repairPulls, exemptPulls
}

// openPullStartGate refuses an implementation worker start when the target
// repository is at supervisor.unmergedLimit or when one of issues already has
// an open pull request (--force aside).
func (a *app) openPullStartGate(ctx context.Context, name, brief string, issues []string, isReview, force, dryRun bool) error {
	if isReview || len(issues) == 0 {
		return nil
	}
	limit, window := a.unmergedPolicy()
	now := a.timeNow()
	exemptTargets, repairPulls, exemptPulls := a.startGateExemptions(name, brief, issues, now)
	byRepo := map[string][]unmergedPull{}
	checkedHold := map[string]bool{}
	for _, issue := range issues {
		r, ok := parseRef(issue)
		if !ok {
			continue
		}
		target := refName(r)
		targetKey := strings.ToLower(target)
		key := strings.ToLower(r.Repo)
		isExemptTarget := exemptTargets[targetKey]
		skipDup := slices.ContainsFunc(exemptPulls, func(p string) bool { return strings.EqualFold(p, target) })
		if isExemptTarget && skipDup {
			continue
		}
		pulls, seen := byRepo[key]
		if !seen {
			var err error
			pulls, err = readRepoOpenPulls(ctx, r.Repo)
			if err != nil {
				if !force {
					return refused("open pull requests for %s cannot be read (%v); retry once GitHub is readable (--force starts anyway)", r.Repo, err)
				}
				if dryRun {
					_, _ = fmt.Fprintf(a.out, "dry run over unreadable open pull requests for %s: %v (--force)\n", r.Repo, err)
				}
				continue
			}
			byRepo[key] = pulls
		}
		if !isExemptTarget && len(repairPulls) > 0 {
			var authorizedPulls []unmergedPull
			for _, p := range pulls {
				prRef := ""
				if p.Number > 0 {
					prRef = fmt.Sprintf("%s#%d", r.Repo, p.Number)
				} else if pr, ok := parseRef(p.URL); ok {
					prRef = refName(pr)
				}
				if prRef != "" && slices.ContainsFunc(repairPulls, func(rp string) bool { return strings.EqualFold(rp, prRef) }) {
					authorizedPulls = append(authorizedPulls, p)
				}
			}
			if matched, _ := openPullClosingIssue(r.Repo, authorizedPulls, "", []string{target}, nil); matched != "" {
				isExemptTarget = true
			}
		}
		if !isExemptTarget && !checkedHold[key] {
			checkedHold[key] = true
			if why := unmergedHoldReason(r.Repo, recentUnmergedPulls(r.Repo, pulls, now, window), limit, window); why != "" {
				if !force {
					return refused("%s; --force starts anyway", why)
				}
				if dryRun {
					_, _ = fmt.Fprintf(a.out, "dry run over %s (--force)\n", why)
				}
			}
		}
		if skipDup {
			continue
		}
		matched, prURL, findErr := findOpenPullClosingIssues(ctx, unmergedGH, r.Repo, pulls, "", []string{target}, exemptPulls)
		if findErr != nil {
			if !force {
				return refused("open pull requests for %s cannot be read (%v); retry once GitHub is readable (--force starts anyway)", r.Repo, findErr)
			}
			if dryRun {
				_, _ = fmt.Fprintf(a.out, "dry run over unreadable open pull requests for %s: %v (--force)\n", r.Repo, findErr)
			}
			continue
		}
		if matched != "" {
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

// hookPRCreate enforces the duplicate open pull request refusal on raw
// non-draft `gh pr create` commands and records the session's issue claims in
// st.Records when admitted.
func (a *app) hookPRCreate(session, repo, head, title, body string) string {
	ownBranch := strings.TrimSpace(head)
	if _, branch, ok := strings.Cut(ownBranch, ":"); ok {
		ownBranch = branch
	}
	selfRef := cmp.Or(repo, "unknown/unknown") + "#0"
	closing := superloop.ParseClosingIssues(selfRef, title+"\n"+body)
	if len(closing) == 0 {
		return ""
	}
	absorbs := superloop.ParseAbsorbedPulls(selfRef, title, body)
	ctx := context.Background()
	byRepo := map[string][]unmergedPull{}
	for _, iss := range closing {
		r, ok := parseRef(iss)
		if !ok {
			continue
		}
		key := strings.ToLower(r.Repo)
		pulls, seen := byRepo[key]
		if !seen {
			var err error
			pulls, err = readRepoOpenPulls(ctx, r.Repo)
			if err != nil {
				return fmt.Sprintf("open pull requests for %s cannot be read (%v); retry once GitHub is readable", r.Repo, err)
			}
			byRepo[key] = pulls
		}
		matched, prURL, err := findOpenPullClosingIssues(ctx, unmergedGH, r.Repo, pulls, ownBranch, []string{refName(r)}, absorbs)
		if err != nil {
			return fmt.Sprintf("open pull requests for %s cannot be read (%v); retry once GitHub is readable", r.Repo, err)
		}
		if matched != "" {
			return fmt.Sprintf("%s already has an open pull request %s; update %s or combine with it instead of opening a duplicate PR", matched, prURL, prURL)
		}
	}
	store := a.store
	if store == nil && a.cfg != nil && a.cfg.StateDir != "" {
		store, _ = state.Open(a.cfg.StateDir)
	}
	if store != nil {
		who := state.Party{Session: session}
		if by, err := a.caller(); err == nil {
			who = by
			if who.Session == "" {
				who.Session = session
			}
		}
		now := a.timeNow()
		_ = store.Update(func(st *state.State) ([]state.Event, error) {
			return recordSessionIssues(st, who, who, now, closing), nil
		})
	}
	return ""
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
// repositories. A failed read warns once and marks the unreadable
// repository's candidates skipped so board next fails closed on that repo.
func (a *app) skipUnmergedBacklog(ctx context.Context, warn io.Writer, cands []board.Candidate, pick func() *board.Candidate) {
	limit, window := a.unmergedPolicy()
	if limit <= 0 || window <= 0 {
		return
	}
	now := a.timeNow()
	var st *state.State
	if a.store != nil {
		st, _ = a.store.Read()
	}
	checked := map[string]string{}
	for {
		p := pick()
		if p == nil {
			return
		}
		if incidentExempt(st, now, p.Ref) {
			return
		}
		r, ok := parseRef(p.Ref)
		if !ok {
			return
		}
		repoKey := strings.ToLower(r.Repo)
		reason, seen := checked[repoKey]
		if !seen {
			pulls, err := readRepoOpenPulls(ctx, r.Repo)
			if err != nil {
				_, _ = fmt.Fprintf(warn, "beekeeper: unmerged pull requests unread for %s: %v\n", r.Repo, err)
				reason = fmt.Sprintf("held: open pull requests for %s cannot be read: %v", r.Repo, err)
			} else {
				recent := recentUnmergedPulls(r.Repo, pulls, now, window)
				reason = unmergedHoldReason(r.Repo, recent, limit, window)
			}
			checked[repoKey] = reason
		}
		if reason == "" {
			return
		}
		marked := false
		for i := range cands {
			if cands[i].Skip != "" || incidentExempt(st, now, cands[i].Ref) {
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
	var st *state.State
	if l.a != nil && l.a.store != nil {
		st, _ = l.a.store.Read()
	}
	exempt := incidentExempt(st, l.a.timeNow(), nextRef)
	recent, why, err := l.repoRecentUnmerged(ctx, r.Repo)
	if err != nil {
		l.sayOnce("unmerged read "+strings.ToLower(r.Repo), "SUPERVISOR LOOP cannot read open pull requests for %s: %v", r.Repo, err)
		if exempt {
			return nil
		}
		return errLoopLater
	}
	l.clearSaid("unmerged read " + strings.ToLower(r.Repo))
	if err := l.escalateUnmergedBacklog(r.Repo, recent, why); err != nil {
		return err
	}
	if why != "" && !exempt {
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
