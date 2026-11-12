package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

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

const unmergedCacheTTL = 5 * time.Minute

type loopUnmergedObservation struct {
	pulls  []unmergedPull
	etag   string
	at     time.Time
	fullAt time.Time
	err    error // A failed observation is not permission to use the old pulls.
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
		next := obs
		err := l.a.loopGitHub(ctx, "inventory", func() error {
			// A conditional read of the most recently updated pull (all states)
			// also notices closures and edits to old pulls. A periodic full refresh
			// bounds reuse even if GitHub omits a validator or a change races us.
			etag, unchanged, err := readRepoPullRevision(ctx, repo, obs.etag)
			if err != nil {
				return err
			}
			if !ok || !unchanged || now.Sub(obs.fullAt) >= time.Hour {
				pulls, err := readRepoOpenPulls(ctx, repo)
				if err != nil {
					return err
				}
				next.pulls, next.fullAt = pulls, now
			}
			next.etag, next.at = etag, now
			return nil
		})
		if err != nil {
			// Admission and cancellation are not repository observations. Retry
			// them when their gates clear, without retaining a transient hold.
			if errors.Is(err, errLoopLater) || ctx.Err() != nil {
				return nil, "", err
			}
			// Keep the last successful payload and validator for recovery, but
			// cache the failure too: otherwise every idle pass repeats the read
			// and its before/after GraphQL budget probes indefinitely.
			next = obs
			next.at = now
		}
		next.err = err
		obs = next
		l.mu.Lock()
		if l.unmerged == nil {
			l.unmerged = map[string]loopUnmergedObservation{}
		}
		l.unmerged[key] = obs
		l.mu.Unlock()
	}
	if obs.err != nil {
		return nil, "", obs.err
	}
	// Re-filter against today's window even when the inventory is unchanged.
	recent := recentUnmergedPulls(repo, obs.pulls, now, window)
	return recent, unmergedHoldReason(repo, recent, limit, window), nil
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
		wait := fmt.Errorf("open pull request observation failed: %v: %w", err, errLoopLater)
		if errors.Is(err, errLoopLater) || ctx.Err() != nil {
			return wait // Global budget and cancellation still stop the pass.
		}
		// An unread repository must not prevent other repositories starting.
		return &refillStartError{cause: wait, output: "repository observation unavailable"}
	}
	l.clearSaid("unmerged read " + strings.ToLower(r.Repo))
	if err := l.escalateUnmergedBacklog(r.Repo, recent, why); err != nil {
		return err
	}
	if why != "" && !exempt {
		l.sayOnce("unmerged "+strings.ToLower(r.Repo), "SUPERVISOR START WAITS %s: %s", nextRef, why)
		return fmt.Errorf("%s: %w", why, errLoopLater)
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
	// Bound the whole optional scan, including admission to the shared GitHub
	// lock. Per-request timeouts alone multiply by the number of repositories.
	ctx, cancel := context.WithTimeout(ctx, loopPlanningObservationTimeout)
	defer cancel()
	keys := slices.Sorted(maps.Keys(repos))
	l.mu.Lock()
	cursor := l.unmergedCursor
	l.mu.Unlock()
	start := sort.Search(len(keys), func(i int) bool { return keys[i] > cursor })
	keys = append(slices.Clone(keys[start:]), keys[:start]...)
	const unmergedChecksPerPass = 8
	checked := 0
	now := l.a.timeNow()
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		l.mu.Lock()
		obs, cached := l.unmerged[key]
		l.mu.Unlock()
		networkCheck := !cached || now.Sub(obs.at) >= unmergedCacheTTL
		if networkCheck {
			if checked >= unmergedChecksPerPass {
				continue
			}
			checked++
			// Resume after the attempted repository next pass, even if admission
			// used the entire budget, so one unread repository cannot starve others.
			l.mu.Lock()
			l.unmergedCursor = key
			l.mu.Unlock()
		}
		repo := repos[key]
		recent, why, err := l.repoRecentUnmerged(ctx, repo)
		if err != nil {
			if errors.Is(err, errLoopLater) {
				break
			}
			continue
		}
		if err := l.escalateUnmergedBacklog(repo, recent, why); err != nil {
			return err
		}
	}
	return nil
}
