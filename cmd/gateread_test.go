//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A GitHub budget read that fails once in transit and then answers does not
// refuse the merge; one that is refused (no login) is not sent again.
func TestTheBudgetReadRetriesATransientFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		fail  error
		reads int
		ok    bool
	}{
		{"a DNS failure, then the budget", errors.New("Get \"https://api.github.com/rate_limit\": dial tcp: lookup api.github.com: no such host"), 2, true},
		{"gh not connecting, then the budget", errors.New("error connecting to api.github.com"), 2, true},
		{"no login", errors.New("gh auth token: no oauth token found"), 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reads := 0
			was := readBudget
			t.Cleanup(func() { readBudget = was })
			readBudget = func(*app, context.Context) (github.Budget, error) {
				if reads++; reads == 1 {
					return github.Budget{}, c.fail
				}
				return github.Budget{Remaining: 5000, Limit: 5000}, nil
			}
			g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo})
			b, err := g.gateBudget()
			if (err == nil) != c.ok || reads != c.reads || c.ok && b.Remaining != 5000 {
				t.Fatalf("budget %+v, %v after %d reads, want ok %v after %d", b, err, reads, c.ok, c.reads)
			}
		})
	}
}

// A GitHub budget read that fails in transit on every try is no refusal: the
// merge exits 74 (unread), its line saying the read failed and the merge was
// not judged. A budget GitHub refuses to tell (no login) still refuses (77).
func TestABudgetUnreadInTransitExitsRetryableNotRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		fail  error
		code  int
		reads int
		says  []string
	}{
		{"every try times out in transit", errors.New(`Get "https://api.github.com/repos/giantswarm/devctl": dial tcp: lookup api.github.com: i/o timeout`),
			ExitGateUnread, 1 + len(readBackoff), []string{"reading the GitHub budget failed in transit", "i/o timeout", "4 tries failed in transit", "was not judged"}},
		{"gh without a token", errors.New("gh auth token: no oauth token found"),
			ExitGateRefused, 1, []string{"the GitHub budget is unknown", "gh auth status"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := heldApp(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(),
				Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Budget = nil
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			hostDevctls(t, 0)
			reads := 0
			was := readBudget
			t.Cleanup(func() { readBudget = was })
			readBudget = func(*app, context.Context) (github.Budget, error) {
				reads++
				return github.Budget{}, c.fail
			}
			g := placedGate(a, 7, os.Getpid())
			why, err := g.step()
			if Code(err) != c.code || reads != c.reads {
				t.Fatalf("%q, exit %d (%v) after %d reads, want %d after %d", why, Code(err), err, reads, c.code, c.reads)
			}
			d := lastEvent(t, g, "merge.refused")
			for _, want := range c.says {
				if !strings.Contains(d, want) {
					t.Errorf("line %q does not say %q", d, want)
				}
			}
		})
	}
}

// An installation's HelmReleases that stay unreadable in transit refuse the
// merge (77) after the last try, the refusal naming the read's last error
// and the tries; one try that answers lets the lane go on.
func TestTheHelmReleaseReadRefusesOnlyAfterItsRetries(t *testing.T) {
	lane := config.Lane{Name: serving, Installation: gazelle, Repositories: []string{scratchRepo}}
	for _, answerAt := range []int{0, 3} {
		reads := 0
		was := readHelmReleases
		t.Cleanup(func() { readHelmReleases = was })
		readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) {
			if reads++; reads == answerAt {
				return nil, nil
			}
			return nil, errors.New("context teleport.giantswarm.io-gazelle: signal: killed: ")
		}
		g := runningMerge(t, scratchRepo, lane)
		_, why, err := g.laneReady(merge.Lane{})
		if answerAt != 0 {
			if err != nil || why != "" || reads != answerAt {
				t.Fatalf("answered at try %d: %q, %v after %d reads", answerAt, why, err, reads)
			}
			continue
		}
		if Code(err) != ExitGateRefused || reads != 1+len(readBackoff) {
			t.Fatalf("exit %d (%v) after %d reads, want %d after %d", Code(err), err, reads, ExitGateRefused, 1+len(readBackoff))
		}
		d := lastEvent(t, g, "merge.refused")
		for _, want := range []string{"cannot read the HelmReleases of " + gazelle, "signal: killed", "4 tries failed in transit", "once it answers"} {
			if !strings.Contains(d, want) {
				t.Errorf("refusal %q does not say %q", d, want)
			}
		}
	}
}
