package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func mergeReviewRig(t *testing.T) (*app, execution.MergeUse, *superloop.Pull) {
	t.Helper()
	a, r := executionTestApp(t)
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	r.LeaseRequired = true
	head := strings.Repeat("a", 40)
	now := time.Now()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request = r
		st.Starts[0].Execution.Lease = &execution.LeaseGrant{ExpiresAt: now.Add(time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	e := about(event(loopParty, "supervisor.review", "[review %s#7@%s] worker: independent round", r.Repository, head), fmt.Sprintf("%s#7", r.Repository))
	if err := a.store.Record(e); err != nil {
		t.Fatal(err)
	}
	pull := &superloop.Pull{State: superloop.Open, Head: head, Green: true, Reviews: []superloop.Review{{ID: "worker-review", Head: head, Verdict: superloop.Approved, At: now.Add(time.Second)}}}
	old := readLoopPull
	t.Cleanup(func() { readLoopPull = old })
	readLoopPull = func(context.Context, string) (superloop.Pull, error) { return *pull, nil }
	use := execution.MergeUse{ID: "review-grant", Ref: r.Ref, Repository: r.Repository, PR: 7, Head: head}
	assignMergeReview(t, a, use, 1)
	return a, use, pull
}

func TestExecutionMergeWorkerApprovalCannotAuthorize(t *testing.T) {
	for _, afterRejection := range []bool{false, true} {
		t.Run(fmt.Sprint(afterRejection), func(t *testing.T) {
			a, use, pull := mergeReviewRig(t)
			if afterRejection {
				rejected := superloop.Review{ID: "independent-review", Verdict: superloop.ChangesRequested}
				completeMergeReview(t, a, use, &rejected)
				pull.Reviews = append([]superloop.Review{rejected}, pull.Reviews...)
			}
			if _, err := a.issueExecutionMerge(context.Background(), use.Ref, use.Repository, use.PR, use.Head, loopParty); err == nil {
				t.Error("worker-authored approval authorized issuance")
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if err := a.executionMergeLive(context.Background(), st, use); err == nil {
				t.Error("worker-authored approval authorized use")
			}
		})
	}
}

func TestExecutionMergeUnrelatedAndExceptedHolds(t *testing.T) {
	cases := []struct {
		name string
		hold state.Hold
		deny bool
	}{
		{name: "unrelated repository", hold: state.Hold{Target: "other/repository"}},
		{name: "unrelated lane", hold: state.Hold{Target: merge.LanePrefix + "other-lane"}},
		{name: "unrelated upgrade", hold: state.Hold{Target: "upgrade:installation-b/cluster"}},
		{name: "excepted PR", hold: state.Hold{Target: holdMerges, Except: "giantswarm/beekeeper#7"}},
		{name: "excepted session", hold: state.Hold{Target: holdMerges, ExceptSession: "assigned-worker-session"}},
		{name: "global", hold: state.Hold{Target: holdMerges}, deny: true},
		{name: "repository", hold: state.Hold{Target: sshTestRepository}, deny: true},
		{name: "selected lane", hold: state.Hold{Target: merge.LanePrefix + "selected"}, deny: true},
		{name: "selected upgrade", hold: state.Hold{Target: "upgrade:installation-a/cluster"}, deny: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			a, use, pull := mergeReviewRig(t)
			completeMergeReview(t, a, use, &pull.Reviews[0])
			a.cfg.Lanes = []config.Lane{{Name: "selected", Repositories: []string{use.Repository}, Installation: "installation-a"}}
			old := readHelmReleases
			t.Cleanup(func() { readHelmReleases = old })
			readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) { return nil, nil }
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				if test.hold.ExceptSession != "" {
					test.hold.ExceptSession = st.Starts[0].Name
				}
				st.Holds = []state.Hold{test.hold}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if err := a.executionMergeLive(context.Background(), st, use); (err != nil) != test.deny {
				t.Fatalf("live policy deny=%v: %v", test.deny, err)
			}
			if _, err := a.issueExecutionMerge(context.Background(), use.Ref, use.Repository, use.PR, use.Head, loopParty); (err != nil) != test.deny {
				t.Fatalf("issuance policy deny=%v: %v", test.deny, err)
			}
		})
	}
}

func assignMergeReview(t *testing.T, a *app, use execution.MergeUse, number int) state.DeskReview {
	t.Helper()
	ref := fmt.Sprintf("%s#%d", use.Repository, use.PR)
	name := superloop.ReviewerName(ref, number)
	reviewer := state.Party{Session: fmt.Sprintf("independent-session-%d", number), Name: name}
	var worker string
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		worker = st.Starts[0].Name
		st.Starts = append(st.Starts, state.Start{Party: reviewer, By: state.Party{Name: loopParty.Name, Host: "coordinator"}, At: time.Now().Add(-time.Second)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.assignExecutionReview(context.Background(), superloop.Action{Ref: ref, URL: fmt.Sprintf("https://github.com/%s/pull/%d", use.Repository, use.PR), Head: use.Head, Round: number, Agent: worker}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return *executionReviewRound(st, use)
}

func completeMergeReview(t *testing.T, a *app, use execution.MergeUse, review *superloop.Review) state.DeskReview {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if executionReviewRound(st, use) == nil {
		assignMergeReview(t, a, use, 1)
		st, err = a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
	}
	round := executionReviewRound(st, use)
	if review.ID == "" {
		review.ID = "independent-result"
	}
	review.Author = "shared-github-account"
	review.Head, review.Commit = use.Head, use.Head
	review.URL = fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-100", use.Repository, use.PR)
	review.Body = fmt.Sprintf("Review: %s\nHead reviewed: `%s`", review.Verdict, use.Head)
	review.At = time.Now()
	old := readDeskReview
	t.Cleanup(func() { readDeskReview = old })
	readDeskReview = func(_ context.Context, repository, id string) (superloop.Review, error) {
		if repository != use.Repository || id != "100" {
			t.Fatal("review fetched from a different repository or URL")
		}
		return *review, nil
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		return nil, a.captureExecutionReviewReport(context.Background(), st, round.Reviewer, "Review: "+review.Verdict+"\n"+review.URL, time.Now(), true)
	}); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return *executionReviewRound(st, use)
}

func TestExecutionMergeFixWindowPreservesRepairReadinessWaiver(t *testing.T) {
	a, use, pull := mergeReviewRig(t)
	completeMergeReview(t, a, use, &pull.Reviews[0])
	lane := config.Lane{Name: "repair-lane", Repositories: []string{use.Repository}, Installation: "installation-a"}
	a.cfg.Lanes = []config.Lane{lane}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	st.Holds = []state.Hold{{Target: merge.LanePrefix + lane.Name, Except: fmt.Sprintf("%s#%d", use.Repository, use.PR)}}
	st.Merges = []state.Merge{{Repo: "other/repository", PR: 8, Lane: lane.Name, Phase: state.Settling, Finished: time.Now()}}
	old := readHelmReleases
	t.Cleanup(func() { readHelmReleases = old })
	readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) {
		t.Fatal("fix window read blocked installation readiness")
		return nil, nil
	}
	if err := a.executionMergeLive(context.Background(), st, use); err != nil {
		t.Fatal("explicit repair window denied", err)
	}
	st.Holds = nil
	readHelmReleases = func(context.Context, config.Lane) ([]merge.HelmRelease, error) {
		return nil, fmt.Errorf("installation unavailable")
	}
	if err := a.executionMergeLive(context.Background(), st, use); err == nil {
		t.Fatal("ordinary merge ignored unavailable readiness")
	}
}

func TestExecutionMergeAuthenticatesSessionAndPinsResultAcrossRestart(t *testing.T) {
	a, use, pull := mergeReviewRig(t)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	round := executionReviewRound(st, use)
	forged := st.Starts[0].Party
	forged.Name = round.Reviewer.Name
	// A worker can copy the review URL and display name and share the login.
	// Its authenticated process/session is still the worker's.
	if err := a.captureExecutionReviewReport(context.Background(), st, forged, "Review result: https://github.com/giantswarm/beekeeper/pull/7#pullrequestreview-100", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if round.Result != nil {
		t.Fatal("worker's forged reviewer name captured a result")
	}
	if err := a.executionMergeLive(context.Background(), st, use); err == nil {
		t.Fatal("unreported worker approval admitted")
	}
	approved := superloop.Review{ID: "authentic-review", Verdict: superloop.Approved}
	completed := completeMergeReview(t, a, use, &approved)
	pull.Reviews = append([]superloop.Review{approved}, pull.Reviews...)
	grant, err := a.issueExecutionMerge(context.Background(), use.Ref, use.Repository, use.PR, use.Head, loopParty)
	if err != nil {
		t.Fatal(err)
	}
	use.ID = grant.ID
	if grant.DeskReviewID != completed.ID || grant.ReviewID != approved.ID {
		t.Fatal("grant omitted independent result identity")
	}
	reopened, err := state.Open(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	st.WorkerReports = nil
	if err := a.executionMergeLive(context.Background(), st, use); err != nil {
		t.Fatal("durable result lost after report pruning/restart", err)
	}
	// A new independent round invalidates even an otherwise live old grant.
	assignMergeReview(t, a, use, 2)
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.executionMergeLive(context.Background(), st, use); err == nil {
		t.Fatal("new round reused an old approval")
	}
	next := superloop.Review{ID: "next-result", Verdict: superloop.Approved}
	completeMergeReview(t, a, use, &next)
	pull.Reviews = append(pull.Reviews, next)
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.executionMergeLive(context.Background(), st, use); err == nil {
		t.Fatal("old grant accepted another round's result")
	}
}

func TestExecutionMergeReviewerCannotBeImpersonatedWithAs(t *testing.T) {
	a, use, _ := mergeReviewRig(t)
	var name string
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		round := executionReviewRound(st, use)
		round.Reviewer.Session = ""
		round.Reviewer.HostSession = "codex-independent-session"
		name = round.Reviewer.Name
		st.Agents = append(st.Agents, state.Agent{Party: round.Reviewer, Task: "independent review"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	a.as = name
	c := a.agentsCmd()
	c.SetArgs([]string{verbIdle, "--done", "--report", "Review: approved https://github.com/giantswarm/beekeeper/pull/7#pullrequestreview-100", "--problem", ghNone})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "assigned session") {
		t.Fatalf("--as was not refused by reviewer session authentication: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if executionReviewRound(st, use).Result != nil || len(st.WorkerReports) != 0 {
		t.Fatal("forged reviewer report persisted")
	}
}

func TestExecutionMergeRejectsEditedAuthenticatedReview(t *testing.T) {
	a, use, pull := mergeReviewRig(t)
	completeMergeReview(t, a, use, &pull.Reviews[0])
	pull.Reviews[0].Body += "\nedited verdict text"
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.executionMergeLive(context.Background(), st, use); err == nil {
		t.Fatal("edited independent result admitted")
	}
	if _, err := a.issueExecutionMerge(context.Background(), use.Ref, use.Repository, use.PR, use.Head, loopParty); err == nil {
		t.Fatal("edited independent result granted authority")
	}
}

func TestExecutionReviewAssignmentRequiresActualSupervisorStart(t *testing.T) {
	a, use, _ := mergeReviewRig(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.DeskReviews = nil
		st.Starts[1].By = st.Starts[0].Party
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	act := superloop.Action{Ref: fmt.Sprintf("%s#7", use.Repository), Head: use.Head, Round: 1, Agent: stWorkerName(t, a)}
	if err := a.assignExecutionReview(context.Background(), act); err == nil {
		t.Fatal("worker-created session adopted as independent reviewer")
	}
}

func stWorkerName(t *testing.T, a *app) string {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st.Starts[0].Name
}

func TestExecutionReviewCompletionVerdictAndRetryAreAuthenticated(t *testing.T) {
	a, use, pull := mergeReviewRig(t)
	rejected := superloop.Review{ID: "reviewer-rejection", Verdict: superloop.ChangesRequested}
	round := completeMergeReview(t, a, use, &rejected)
	text := "Review: " + superloop.ChangesRequested + "\n" + rejected.URL
	// Simulate a shared-account edit after the independent rejection was stored.
	rejected.Verdict = superloop.Approved
	rejected.Body = "Review: approved\nHead reviewed: `" + use.Head + "`"
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		return nil, a.captureExecutionReviewReport(context.Background(), st, round.Reviewer, text, time.Now(), true)
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if executionReviewRound(st, use).Result.Verdict != superloop.ChangesRequested {
		t.Fatal("identical retry recaptured a shared-account edit")
	}
	pull.Reviews = append(pull.Reviews, rejected)
	if err := a.executionMergeLive(context.Background(), st, use); err == nil {
		t.Fatal("edited rejection authorized use")
	}
	// The first completion must also agree with the authenticated decision text.
	assignMergeReview(t, a, use, 2)
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	next := executionReviewRound(st, use)
	if err := a.captureExecutionReviewReport(context.Background(), st, next.Reviewer, text, time.Now(), true); err == nil {
		t.Fatal("public approval replaced an authenticated rejection")
	}
	if next.Result != nil {
		t.Fatal("conflicting verdict persisted")
	}
}
