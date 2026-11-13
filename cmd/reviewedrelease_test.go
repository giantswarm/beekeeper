//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestReviewFixReviewedGateKeepsAutoReleaseWait(t *testing.T) {
	g := runningMerge(t, scratchRepo, config.Lane{Name: scratchRepo})
	g.ReviewedHead = loopHead
	g.Release = &github.BaseRelease{Base: mainBranch, Auto: true}
	argv, err := g.MergeArgv()
	if err != nil || !slices.Contains(argv, "--release-wait") {
		t.Fatalf("reviewed child lost release tracking: %v %v", argv, err)
	}
}

const reviewFixCandidate = "v1.2.4-rc.1"

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestReviewFixReleaseMetadataSettlesLanesAndToolHolds(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		noRelease bool
		candidate bool
	}{
		{"no release", `{"verdict":"no_release","reason":"no version bump"}`, true, false},
		{"candidate", `{"verdict":"available","tag":"` + reviewFixCandidate + `"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := runningMerge(t, merge.ToolRepo, config.Lane{Name: merge.ToolRepo})
			was := waitReviewedRelease
			t.Cleanup(func() { waitReviewedRelease = was })
			waitReviewedRelease = func(_ context.Context, repository string, number int) ([]byte, error) {
				if repository != merge.ToolRepo || number != 7 {
					t.Fatalf("release wait lost merge identity: %s %d", repository, number)
				}
				return []byte(tc.raw), nil
			}
			result := completeReviewedRelease(t.Context(), github.SquashDoc{Repo: g.Repo, Number: g.Pr, MergeCommitSha: loopFixed}, true)
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			out, ok := merge.ParseDocument(raw)
			if !ok || !out.Merged || out.NoRelease != tc.noRelease || tc.candidate && out.Release != reviewFixCandidate {
				t.Fatalf("lost durable release metadata: %s %+v", raw, out)
			}
			if err := gateApp(g.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				return recordRun(st, 0, g.Lane, g.Me, gate.RunOutcome{Out: out, Rc: result.ExitCode}, time.Now(), ""), nil
			}); err != nil {
				t.Fatal(err)
			}
			st := gateState(t, g)
			if len(st.Merges) != 0 || len(st.Holds) != 0 {
				t.Fatalf("settled merge/tool hold wedged: %+v %+v", st.Merges, st.Holds)
			}
		})
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestReviewFixReleaseMetadataRequiresTheInstallationRoll(t *testing.T) {
	lane := config.Lane{Name: scratchRepo, Installation: gazelle, Repositories: []string{scratchRepo}}
	g := runningMerge(t, scratchRepo, lane)
	was := waitReviewedRelease
	t.Cleanup(func() { waitReviewedRelease = was })
	waitReviewedRelease = func(context.Context, string, int) ([]byte, error) {
		return []byte(`{"verdict":"available","tag":"v1.2.4"}`), nil
	}
	result := completeReviewedRelease(t.Context(), github.SquashDoc{Repo: g.Repo, Number: g.Pr, MergeCommitSha: loopFixed}, true)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := merge.ParseDocument(raw)
	if !ok {
		t.Fatal("unreadable result")
	}
	const rollKey = "flux-system/r"
	now := time.Now()
	if err := gateApp(g.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Roll = []string{rollKey}
		return recordRun(st, 0, lane, g.Me, gate.RunOutcome{Out: out}, now, ""), nil
	}); err != nil {
		t.Fatal(err)
	}
	st := gateState(t, g)
	if len(st.Merges) != 1 || st.Merges[0].Release != "v1.2.4" {
		t.Fatalf("release unknown: %+v", st.Merges)
	}
	hrs := []merge.HelmRelease{{Key: rollKey, Chart: "r", Version: "1.2.3", Ready: true, Range: ">=1.0.0 <2.0.0"}}
	if ready, why := merge.Ready(lane, hrs, &st.Merges[0], now.Add(time.Hour), time.Minute); ready || !strings.Contains(why, "rolling to") {
		t.Fatalf("lane freed before release rolled: %v %s", ready, why)
	}
	hrs[0].Version = "1.2.4"
	if ready, why := merge.Ready(lane, hrs, &st.Merges[0], now.Add(time.Hour), time.Minute); !ready {
		t.Fatalf("lane held after roll: %s", why)
	}
}

func TestReviewFixUnconfirmedReleaseRemainsExplicit(t *testing.T) {
	was := waitReviewedRelease
	t.Cleanup(func() { waitReviewedRelease = was })
	waitReviewedRelease = func(context.Context, string, int) ([]byte, error) { return nil, errors.New("release read unavailable") }
	result := completeReviewedRelease(t.Context(), github.SquashDoc{Repo: scratchRepo, Number: 7, MergeCommitSha: loopFixed}, true)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := merge.ParseDocument(raw)
	if !ok || !out.Merged || !out.Unconfirmed || result.ExitCode != 9 {
		t.Fatalf("lost uncertain release result: %s %+v", raw, out)
	}
}
