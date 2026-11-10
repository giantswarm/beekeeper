//go:build unix

package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const reviewTestPath = "main.go"

// reviewCaseRedCI is the review start case whose head has failing checks.
const reviewCaseRedCI = "red CI"

func TestLightReview(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		lines, threshold, count int
		files                   []string
		want                    bool
	}{
		{"below boundary", 149, 150, 1, []string{reviewTestPath}, true},
		{"boundary", 150, 150, 1, []string{reviewTestPath}, false},
		{"configured", 180, 200, 1, []string{reviewTestPath}, true},
		{"disabled", 1, -1, 1, []string{reviewTestPath}, false},
		{"docs and tests", 900, -1, 2, []string{"docs/guide.md", "cmd/main_test.go"}, true},
		{"partial paths", 900, 150, 2, []string{"README.md"}, false},
		{"mixed", 900, 150, 2, []string{"README.md", "cmd/main.go"}, false},
		{"no paths", 900, 150, 0, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := lightReview(superloop.Pull{Additions: tt.lines, Files: tt.files, ChangedFiles: tt.count}, tt.threshold)
			if got != tt.want {
				t.Fatalf("light = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReviewRoundPinsHeadAcrossRestarts(t *testing.T) {
	p := superloop.Pull{Ref: loopPR, Head: loopHead}
	for _, st := range []*state.State{
		{ReviewRequests: []state.ReviewRequest{{Ref: p.Ref, Head: p.Head, Round: 1, PID: os.Getpid()}}},
		{DeskReviews: []state.DeskReview{{Ref: p.Ref, Head: p.Head, Round: 1}}},
	} {
		if _, _, err := reviewRound(st, p); Code(err) != ExitRefused {
			t.Fatalf("duplicate not refused: %v", err)
		}
	}
	p.Reviews = []superloop.Review{{Head: loopFixed, Verdict: superloop.ChangesRequested, Body: "main.go:7: broken → fix"}}
	round, previous, err := reviewRound(&state.State{}, p)
	if err != nil || round != 2 || previous != p.Reviews[0].Body {
		t.Fatalf("round=%d previous=%q err=%v", round, previous, err)
	}
	p.Head = loopFixed
	if _, _, err := reviewRound(&state.State{}, p); Code(err) != ExitRefused {
		t.Fatalf("reviewed head not refused: %v", err)
	}
}

func TestAgentsReviewRefusesBeforeStarting(t *testing.T) {
	for _, name := range []string{reviewCaseRedCI, "draft PR", "closed", "reviewed", "requested"} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			switch name {
			case reviewCaseRedCI:
				r.pull.Green = false
			case "draft PR":
				r.pull.Draft = true
			case "closed":
				r.pull.State = superloop.Merged
			case "reviewed":
				r.pull.Reviews = []superloop.Review{{Head: r.pull.Head, Verdict: superloop.Approved, Member: true}}
			case "requested":
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.ReviewRequests = []state.ReviewRequest{{Ref: r.pull.Ref, Head: r.pull.Head, Round: 1, PID: os.Getpid()}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			c := r.a.agentReviewCmd()
			c.SetArgs([]string{loopPR})
			if err := c.ExecuteContext(t.Context()); Code(err) != ExitRefused {
				t.Fatalf("not refused: %v", err)
			}
			if r.called(agentsName, agentStartName) != nil {
				t.Fatal("started a reviewer")
			}
		})
	}
}

func TestAgentsReviewStartsInCheckoutWithFilledBrief(t *testing.T) {
	r := newLoopRig(t)
	dir := t.TempDir()
	gitT(t, dir, "init")
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	r.a.cfg.Agents.Dir = dir
	r.pull.Closes = []string{epicTen}
	r.pull.Additions = 20
	oldParents := reviewParents
	t.Cleanup(func() { reviewParents = oldParents })
	reviewParents = func(_ context.Context, issues []string) ([]string, error) {
		if !slices.Equal(issues, r.pull.Closes) {
			t.Fatalf("issues: %v", issues)
		}
		return []string{issueOneURL}, nil
	}
	c := r.a.agentReviewCmd()
	c.SetArgs([]string{loopPR, modelFlag, defaultReviewModel})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	args := r.called(agentsName, agentStartName)
	if len(args) < 4 {
		t.Fatalf("no start: %v", args)
	}
	for flag, want := range map[string]string{"--dir": dir, modelFlag: defaultReviewModel} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Fatalf("start lacks %s %s: %v", flag, want, args)
		}
	}
	body, err := os.ReadFile(filepath.Clean(args[3]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{loopURL, loopHead, "CI green at " + loopHead, "Light review:", epicTen, issueOneURL, "review round 1", "none (first round)", deskReviewApproved} {
		if !strings.Contains(string(body), want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	c = r.a.agentReviewCmd()
	c.SetArgs([]string{loopPR})
	if err := c.ExecuteContext(t.Context()); Code(err) != ExitRefused {
		t.Fatalf("second review: %v", err)
	}
}

func TestRequireReviewRejectsGoWithoutFixedCommentAndAcceptsPinnedApproval(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.RequireReview = true
	if err := r.a.store.Record(about(event(loopParty, "supervisor.go", "[go %s@%s] %s: merge go", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		body, head string
		want       bool
	}{
		{"", loopHead, false},
		{loopApproved, loopHead, true},
		{loopApproved, loopFixed, false},
		{deskReviewApproved, loopFixed, false},
		{deskReviewApproved, loopHead, true},
		{"Desk review (opus): changes requested", loopHead, false},
	} {
		r.pull.Reviews = []superloop.Review{{Head: tt.head, Verdict: superloop.Verdict("COMMENTED", tt.body), Body: tt.body, Member: true}}
		why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, r.pull, r.pull.Head)
		if err != nil || (why == "") != tt.want {
			t.Errorf("body=%q head=%s: why=%q err=%v", tt.body, tt.head, why, err)
		}
	}
}

func TestSupervisorNitAttestationKeepsApprovalWithoutSecondRound(t *testing.T) {
	r := newLoopRig(t)
	r.a.as = loopParty.Name
	r.a.cfg.Merge.RequireReview = true
	r.park(t, loopPR, r.a.timeNow())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "Desk review (opus): approved\ncmd/x.go:3: spelling → correct spelling")
	r.pull.Head = loopFixed
	old := readChangedPast
	t.Cleanup(func() { readChangedPast = old })
	readChangedPast = func(context.Context, superloop.Pull, string) ([]string, error) { return []string{"cmd/x.go"}, nil }
	if err := r.a.checkReviewNits(t.Context(), r.pull, loopHead); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	why, _, err := r.a.checkMergeHeadAuthority(t.Context(), st, r.pull, r.pull.Head)
	if err != nil || why != "" {
		t.Fatalf("attested nits refused: %q %v", why, err)
	}
	r.pass(t)
	if start := r.called(agentsName, agentStartName); start != nil {
		t.Fatalf("second review started: %v", start)
	}
	if merge := r.called(gateCmdName); merge == nil {
		t.Fatalf("no merge go after nits: %s", r.out)
	}
	r.pull.Head = "3333333333333333333333333333333333333333"
	why, _, err = r.a.checkMergeHeadAuthority(t.Context(), st, r.pull, r.pull.Head)
	if err != nil || why == "" {
		t.Fatalf("attestation covered a later push: %q %v", why, err)
	}
}

func TestAgentsReviewFailureReleasesUnstartedRequest(t *testing.T) {
	r := newLoopRig(t)
	dir := t.TempDir()
	gitT(t, dir, "init")
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	r.a.cfg.Agents.Dir = dir
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			return "review start failed", 1
		}
		return "", 0
	}
	c := r.a.agentReviewCmd()
	c.SetArgs([]string{loopPR})
	if err := c.ExecuteContext(t.Context()); err == nil {
		t.Fatal("start failure ignored")
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReviewRequests) != 0 {
		t.Fatalf("unstarted request retained: %+v", st.ReviewRequests)
	}
}

func TestReviewParentReadFailsClosed(t *testing.T) {
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	for _, raw := range []string{`{"errors":[{"message":"review start failed"}]}`, `{"data":{"repository":{"issue":null}}}`, `not-json-review-parent`} {
		github.RunGH = func(context.Context, ...string) ([]byte, error) { return []byte(raw), nil }
		if _, err := reviewParents(t.Context(), []string{refOne}); err == nil {
			t.Fatalf("unreadable parent accepted: %s", raw)
		}
	}
	github.RunGH = func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":{"issue":{"parent":null}}}}`), nil
	}
	if parents, err := reviewParents(t.Context(), []string{refOne}); err != nil || len(parents) != 0 {
		t.Fatalf("absent parent: %v %v", parents, err)
	}
}

func TestReviewReservationIsSharedWithAutomaticLoop(t *testing.T) {
	r := newLoopRig(t)
	act := superloop.Action{Ref: loopPR, Head: loopHead, Round: 1}
	if err := r.a.reserveReview(act); err != nil {
		t.Fatal(err)
	}
	if err := r.a.reserveReview(act); Code(err) != ExitRefused {
		t.Fatalf("duplicate start not refused: %v", err)
	}
	act.Head = loopFixed
	if err := r.a.reserveReview(act); Code(err) != ExitRefused {
		t.Fatalf("round collision not refused: %v", err)
	}
	act.Round = 2
	if err := r.a.reserveReview(act); err != nil {
		t.Fatalf("new head and round refused: %v", err)
	}
}

// A request alone is intent, not evidence that a reviewer ever launched.
func TestReviewReservationRecoversOrphan(t *testing.T) {
	for _, round := range []int{1, 2} {
		t.Run(superloop.ReviewerName(loopPR, round), func(t *testing.T) {
			r := newLoopRig(t)
			act := superloop.Action{Ref: loopPR, Head: loopHead, Round: 1}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.ReviewRequests = []state.ReviewRequest{{Ref: loopPR, Head: loopHead, Round: round}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if got, _, err := reviewRound(st, r.pull); err != nil || got != 1 {
				t.Fatalf("orphan influenced round: %d %v", got, err)
			}
			if err := r.a.reserveReview(act); err != nil {
				t.Fatal(err)
			}
			if err := r.a.reserveReview(act); Code(err) != ExitRefused {
				t.Fatalf("concurrent start allowed: %v", err)
			}
			// A historical start event must not keep this failed attempt reserved.
			if err := r.a.store.Record(event(loopParty, verbStart, "%s: old failed start", superloop.ReviewerName(loopPR, 1))); err != nil {
				t.Fatal(err)
			}
			r.a.finishReviewRequest(act, false)
			st, err = r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.ReviewRequests) != 0 {
				t.Fatalf("orphan retained: %+v", st.ReviewRequests)
			}
		})
	}
}

func TestReviewReservationKeepsRegisteredReviewer(t *testing.T) {
	r := newLoopRig(t)
	act := superloop.Action{Ref: loopPR, Head: loopHead, Round: 1}
	if err := r.a.reserveReview(act); err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: superloop.ReviewerName(loopPR, 1)}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Delivery can fail after launch: retain the registered reviewer's request.
	r.a.finishReviewRequest(act, false)
	if err := r.a.reserveReview(act); Code(err) != ExitRefused {
		t.Fatalf("registered reviewer duplicated: %v", err)
	}
}

func TestReviewRequestFromExitedStarterIsInactive(t *testing.T) {
	request := state.ReviewRequest{Ref: loopPR, Head: loopHead, Round: 1, PID: 1 << 30}
	if reviewRequestActive(&state.State{}, request) {
		t.Fatal("exited starter still owns request")
	}
	st := &state.State{DeskReviews: []state.DeskReview{{Ref: loopPR, Head: loopHead, Round: 1}}}
	if !reviewRequestActive(st, request) {
		t.Fatal("assigned review lost its reservation")
	}
}

func TestRequireReviewCarriesApprovalOverBaseOnlyMerge(t *testing.T) {
	for _, covered := range []bool{true, false} {
		r := newLoopRig(t)
		r.a.cfg.Merge.RequireReview = true
		r.pull.Head = loopFixed
		r.pull.Reviews = []superloop.Review{{Head: loopHead, Verdict: superloop.Approved, Body: deskReviewApproved, Member: true}}
		r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
			if p.Head != loopFixed || approved != loopHead {
				t.Fatalf("wrong coverage comparison: %s..%s", approved, p.Head)
			}
			return covered, nil
		}
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		why, _, err := r.a.checkMergeHeadAuthority(t.Context(), st, r.pull, r.pull.Head)
		if err != nil || (why == "") != covered {
			t.Fatalf("covered %v: why=%q err=%v", covered, why, err)
		}
	}
}
