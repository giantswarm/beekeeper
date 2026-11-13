package loop

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

// UnmergedPull is an open pull request in a repository, read to check
// whether recent unmerged work exceeds supervisor.unmergedLimit or whether an
// open pull request already closes an issue.
type UnmergedPull struct {
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

const UnmergedCacheTTL = 5 * time.Minute

type LoopUnmergedObservation struct {
	Pulls  []UnmergedPull
	Etag   string
	At     time.Time
	FullAt time.Time
	err    error // A failed observation is not permission to use the old pulls.
}

func (l *Loop) RepoRecentUnmerged(ctx context.Context, repo string) ([]UnmergedPull, string, error) {
	limit, window := l.Host.UnmergedPolicy()
	if limit <= 0 || window <= 0 {
		return nil, "", nil
	}
	now := l.Host.TimeNow()
	key := strings.ToLower(repo)
	l.Mu.Lock()
	obs, ok := l.Unmerged[key]
	l.Mu.Unlock()
	if !ok || now.Sub(obs.At) >= UnmergedCacheTTL {
		next := obs
		// A conditional read of the most recently updated pull (all states)
		// also notices closures and edits to old pulls. A periodic full refresh
		// bounds reuse even if GitHub omits a validator or a change races us.
		// Only the full read is a GraphQL query and counted as one: the
		// revision spends REST, nothing when unchanged (#3769).
		var etag string
		var unchanged bool
		err := l.Host.LoopGitHub(ctx, LoopRevisionStep, func() error {
			var err error
			etag, unchanged, err = ReadRepoPullRevision(ctx, repo, obs.Etag)
			return err
		})
		if err == nil && (!ok || !unchanged || now.Sub(obs.FullAt) >= time.Hour) {
			err = l.Host.LoopGitHub(ctx, "inventory", func() error {
				pulls, err := Cmd.ReadRepoOpenPulls(ctx, repo)
				if err != nil {
					return err
				}
				next.Pulls, next.FullAt = pulls, now
				return nil
			})
		}
		if err == nil {
			next.Etag, next.At = etag, now
		} else {
			// Admission and cancellation are not repository observations. Retry
			// them when their gates clear, without retaining a transient hold.
			if errors.Is(err, ErrLoopLater) || ctx.Err() != nil {
				return nil, "", err
			}
			// Keep the last successful payload and validator for recovery, but
			// cache the failure too: otherwise every idle pass repeats the read,
			// and a failed full read its GraphQL budget readings, indefinitely.
			next = obs
			next.At = now
		}
		next.err = err
		obs = next
		l.Mu.Lock()
		if l.Unmerged == nil {
			l.Unmerged = map[string]LoopUnmergedObservation{}
		}
		l.Unmerged[key] = obs
		l.Mu.Unlock()
	}
	if obs.err != nil {
		return nil, "", obs.err
	}
	// Re-filter against today's window even when the inventory is unchanged.
	recent := Cmd.RecentUnmergedPulls(repo, obs.Pulls, now, window)
	return recent, Cmd.UnmergedHoldReason(repo, recent, limit, window), nil
}

// escalateUnmergedBacklog files a guide problem once per backlog episode when
// repo has at least supervisor.unmergedLimit open pull requests in
// supervisor.unmergedWindow, and clears the episode once the count drops
// below the limit.
func (l *Loop) escalateUnmergedBacklog(repo string, recent []UnmergedPull, reason string) error {
	stage := "unmerged " + strings.ToLower(repo)
	l.Mu.Lock()
	staged := l.Stages[stage]
	l.Mu.Unlock()
	if reason == "" {
		if staged && !l.DryRun {
			l.Mu.Lock()
			delete(l.Stages, stage)
			delete(l.StageDetail, stage)
			l.Mu.Unlock()
			return l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopUnstageVerb, "[%s] below limit", stage), repo))
		}
		return nil
	}
	if staged || l.DryRun {
		return nil
	}
	limit, window := l.Host.UnmergedPolicy()
	now := l.Host.TimeNow()
	urls := make([]string, 0, len(recent))
	for _, p := range recent {
		if p.URL != "" {
			urls = append(urls, p.URL)
		}
	}
	text := fmt.Sprintf("Repository %s has %d open pull requests created in the last %s (limit %d): %s. "+
		"New board work in %s is held back until reviews and merges drain the backlog below %d.",
		repo, len(recent), Cmd.UnmergedWindowLabel(window), limit, strings.Join(urls, ", "), repo, limit)
	line := fmt.Sprintf("%s reported at %s%s%s", LoopParty.Name, now.UTC().Format(time.RFC3339), FileReportSeparator, text)
	fileKey := fmt.Sprintf("%s %s", stage, now.UTC().Format("20060102T150405Z"))
	l.Mu.Lock()
	if l.Stages == nil {
		l.Stages = map[string]bool{}
	}
	if l.StageDetail == nil {
		l.StageDetail = map[string]string{}
	}
	l.Stages[stage] = true
	l.StageDetail[stage] = fileKey
	l.Mu.Unlock()
	if err := l.Host.Store().Record(Cmd.About(Cmd.Event(LoopParty, LoopStageVerb, "[%s] %s", stage, fileKey), repo)); err != nil {
		return err
	}
	return l.Record(superloop.Action{Kind: superloop.KindFile, Key: fileKey, Ref: repo}, line)
}

// UnmergedStart checks whether nextRef's repository is held back by an
// unmerged pull request backlog, escalating to the guide once per episode.
func (l *Loop) UnmergedStart(ctx context.Context, nextRef string) error {
	r, ok := Cmd.ParseRef(nextRef)
	if !ok {
		return nil
	}
	var st *state.State
	if l.Host != nil && l.Host.Store() != nil {
		st, _ = l.Host.Store().Read()
	}
	exempt := Cmd.IncidentExempt(st, l.Host.TimeNow(), nextRef)
	recent, why, err := l.RepoRecentUnmerged(ctx, r.Repo)
	if err != nil {
		l.SayOnce("unmerged read "+strings.ToLower(r.Repo), "SUPERVISOR LOOP cannot read open pull requests for %s: %v", r.Repo, err)
		if exempt {
			return nil
		}
		wait := fmt.Errorf("open pull request observation failed: %v: %w", err, ErrLoopLater)
		if errors.Is(err, ErrLoopLater) || ctx.Err() != nil {
			return wait // Global budget and cancellation still stop the pass.
		}
		// An unread repository must not prevent other repositories starting.
		return &RefillStartError{cause: wait, output: "repository observation unavailable"}
	}
	l.ClearSaid("unmerged read " + strings.ToLower(r.Repo))
	if err := l.escalateUnmergedBacklog(r.Repo, recent, why); err != nil {
		return err
	}
	if why != "" && !exempt {
		l.SayOnce("unmerged "+strings.ToLower(r.Repo), "SUPERVISOR START WAITS %s: %s", nextRef, why)
		return fmt.Errorf("%s: %w", why, ErrLoopLater)
	}
	l.ClearSaid("unmerged " + strings.ToLower(r.Repo))
	return nil
}

// CheckUnmergedBacklogs inspects active repositories in the pass so a
// repository that accumulates supervisor.unmergedLimit open pull requests in
// supervisor.unmergedWindow escalates to the guide even when board next skips
// it before startItem, and clears its stage once the backlog drains.
func (l *Loop) CheckUnmergedBacklogs(ctx context.Context, in superloop.Input) error {
	limit, window := l.Host.UnmergedPolicy()
	if limit <= 0 || window <= 0 {
		return nil
	}
	repos := map[string]string{}
	addRef := func(ref string) {
		if r, ok := Cmd.ParseRef(ref); ok && l.unreadRepo(ref) == "" {
			repos[strings.ToLower(r.Repo)] = r.Repo
		}
	}
	for ref, p := range in.Pulls {
		if p.State == superloop.Open {
			addRef(ref)
		}
	}
	l.Mu.Lock()
	for _, ref := range l.OpenPulls {
		addRef(ref)
	}
	for stage := range l.Stages {
		if repo, ok := strings.CutPrefix(stage, "unmerged "); ok && repo != "" {
			repos[strings.ToLower(repo)] = repo
		}
	}
	l.Mu.Unlock()
	// Bound the whole optional scan, including admission to the shared GitHub
	// lock. Per-request timeouts alone multiply by the number of repositories.
	ctx, cancel := context.WithTimeout(ctx, LoopPlanningObservationTimeout)
	defer cancel()
	keys := slices.Sorted(maps.Keys(repos))
	l.Mu.Lock()
	cursor := l.UnmergedCursor
	l.Mu.Unlock()
	start := sort.Search(len(keys), func(i int) bool { return keys[i] > cursor })
	keys = append(slices.Clone(keys[start:]), keys[:start]...)
	const unmergedChecksPerPass = 8
	checked := 0
	now := l.Host.TimeNow()
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		l.Mu.Lock()
		obs, cached := l.Unmerged[key]
		l.Mu.Unlock()
		networkCheck := !cached || now.Sub(obs.At) >= UnmergedCacheTTL
		if networkCheck {
			if checked >= unmergedChecksPerPass {
				continue
			}
			checked++
			// Resume after the attempted repository next pass, even if admission
			// used the entire budget, so one unread repository cannot starve others.
			l.Mu.Lock()
			l.UnmergedCursor = key
			l.Mu.Unlock()
		}
		repo := repos[key]
		recent, why, err := l.RepoRecentUnmerged(ctx, repo)
		if err != nil {
			if errors.Is(err, ErrLoopLater) {
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
