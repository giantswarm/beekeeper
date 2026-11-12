package cmd

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (l *Loop) otherReviewParks(party state.Party, ref string) bool {
	st, err := l.Host.store.Read()
	if err != nil {
		// Preserve the task if pending requests cannot be read.
		return true
	}
	for _, rp := range st.ReviewParks {
		if rp.Party.Is(party) && !strings.EqualFold(rp.On, ref) {
			return true
		}
	}
	return false
}

// RecoverReviewRequests upgrades explicit review waits only after re-proving
// their published worktree. Older parks omitted Worktree; their recorded start
// directory supplies it. An API failure leaves the request pending for a later
// pass after budget recovery, never granting authority from the wait text alone.
func (l *Loop) RecoverReviewRequests(ctx context.Context, st *state.State) error {
	if l.DryRun || st.Stop != nil {
		return nil
	}
	now := l.Host.timeNow()
	if l.reviewRecoveries == nil {
		l.reviewRecoveries = map[string]reviewRecovery{}
	}
	seen := map[string]bool{}
	for _, ag := range st.Agents {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := ag.Park
		if ag.Done || ag.Execution != nil || p == nil || p.External || p.Checks || p.ReviewHead != "" || !isDeskReviewWait(p.Waits) {
			continue
		}
		if _, ok := parseRef(p.On); !ok || l.agentUnreadRepo(st, ag, p.On) != "" {
			continue
		}
		dir := p.Worktree
		if dir == "" {
			if k := st.StartIndex(ag.Party); k >= 0 {
				dir = st.Starts[k].Dir
			}
		}
		if dir == "" {
			continue
		}
		// A request that did not verify is looked at again after a backoff,
		// not every pass: each look starts git in its worktree and reads the
		// pull request from GitHub.
		key := reviewRecoveryKey(ag, dir)
		seen[key] = true
		if r, ok := l.reviewRecoveries[key]; ok && now.Before(r.next) {
			continue
		}
		if stop, err := l.stopPassForReexec(ctx); stop || err != nil {
			return err
		}
		l.LifecyclePhase(loopPhaseRecoverReviews)
		verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		head := RecoverParkReviewHead(verifyCtx, p.On, dir)
		cancel()
		if head == "" {
			wait := ReviewRecoveryFirst
			if r, ok := l.reviewRecoveries[key]; ok {
				wait = min(2*r.wait, reviewRecoveryMax)
			}
			l.reviewRecoveries[key] = reviewRecovery{next: now.Add(wait), wait: wait}
			continue
		}
		delete(l.reviewRecoveries, key)
		verified := false
		if err := l.Host.store.Update(func(current *state.State) ([]state.Event, error) {
			for i := range current.Agents {
				a := &current.Agents[i]
				if !a.Is(ag.Party) || a.Done || a.Park == nil || a.Park.On != p.On || !a.Park.At.Equal(p.At) || a.Park.Waits != p.Waits || a.Park.ReviewHead != "" {
					continue
				}
				a.Park.ReviewHead, a.Park.Worktree, a.Park.RefKind = head, dir, github.RefPullRequest
				recordAgentReviewPark(current, *a)
				verified = true
				return []state.Event{event(LoopParty, "agents.review-verified", "%s: review request verified for %s at pushed HEAD %s", a.Name, p.On, head)}, nil
			}
			return nil, nil
		}); err != nil {
			return err
		}
		if verified {
			if err := l.Host.recordFatalRepair(ag.Name, p.On); err != nil {
				return err
			}
		}
	}
	for key := range l.reviewRecoveries {
		if !seen[key] {
			delete(l.reviewRecoveries, key)
		}
	}
	return nil
}

// reviewRecovery is when a review request that did not verify is looked at
// again, and the wait that set it.
type reviewRecovery struct {
	next time.Time
	wait time.Duration
}

// The first retry of a review request that did not verify comes after
// reviewRecoveryFirst; each further one waits twice as long, up to
// reviewRecoveryMax.
const (
	ReviewRecoveryFirst = time.Minute
	reviewRecoveryMax   = 10 * time.Minute
)

// reviewRecoveryKey names one park of one session in one worktree: a new
// park, or the same one in another worktree, is looked at at once.
func reviewRecoveryKey(ag state.Agent, dir string) string {
	p := ag.Park
	return strings.Join([]string{ag.Name, ag.Session, p.On, p.At.UTC().Format(time.RFC3339Nano), p.Waits, dir}, "\x00")
}

// RecoverParkReviewHead starts no git in a worktree that is gone: it cannot
// verify there.
var RecoverParkReviewHead = func(ctx context.Context, on, dir string) string {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return publishedParkHead(ctx, on, dir, runGit, github.RunGH)
}
