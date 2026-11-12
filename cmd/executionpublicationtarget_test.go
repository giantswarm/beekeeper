package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	continuationFixtureURLKey  = "html_url"
	continuationFixtureRepoKey = "full_name"
	continuationFixtureSHAKey  = "sha"
)

func TestExecutionContinuationPublishesExistingPull(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	const branch = "beekeeper-execution/prior-execution/1"
	priorHead := rig.commit(t, "change.txt", "original change\n")
	if _, err := publishGit(ctx, rig.workspace, "push", rig.remote, priorHead+":refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	// The assigned pull request as GitHub answers it by its number; the
	// continuation never lists pull requests by branch, nor creates one.
	pullState, pullBranch := noteStatusOpen, branch
	pullPath := fmt.Sprintf("repos/%s/pulls/42", rig.request.Repository)
	originalGH := executionPublishGH
	executionPublishGH = func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == workerGitHubAPIGroup && args[1] == pullPath {
			return []byte(fmt.Sprintf(`{"state":%q,"html_url":%q,"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`, pullState, publicationFixturePRURL, pullBranch, priorHead, rig.request.Repository)), nil
		}
		if joined := strings.Join(args[:2], " "); joined == "pr "+transcriptListCommand || joined == publicationFixtureCreate {
			t.Errorf("continuation read or created pull requests by branch: %v", args)
			return nil, fmt.Errorf("fixture refuses %s", joined)
		}
		return originalGH(ctx, args...)
	}
	// The loop's recorded repair claim, not the new execution's identity,
	// determines the destination. Passing an unrelated source ref grants nothing.
	const name = "continuation fixture"
	on := rig.request.Repository + "#42"
	if err := rig.a.store.Record(about(event(loopParty, loopClaimVerb, "[red %s@%s] %s", on, priorHead, name), on)); err != nil {
		t.Fatal(err)
	}
	_, repairs, _ := rig.a.startGateExemptions(name, "", nil, rig.a.timeNow())
	target, err := continuationPublication(ctx, rig.request.Repository, repairs)
	if err != nil || target == nil || target.Branch != branch || target.Head != priorHead || target.URL != publicationFixturePRURL {
		t.Fatalf("resolve continuation: %+v, %v", target, err)
	}
	rig.request.PublicationBranch, rig.request.PublicationPR = target.Branch, target.URL
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec := st.Starts[0].Execution
		rec.Request = rig.request
		rec.Handle.Commit = priorHead
		rec.Published = target
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := executionGitHubTargetBranch(execution.Journal{Request: rig.request}); got != branch {
		t.Fatalf("GitHub push target = %q", got)
	}
	remoteHead := func() string {
		t.Helper()
		head, err := publishGit(ctx, rig.remote, "rev-parse", "refs/heads/"+branch)
		if err != nil {
			t.Fatal(err)
		}
		return head
	}
	var last string
	for _, content := range []string{"fix round one\n", "fix round two\n"} {
		head := rig.commit(t, "change.txt", content)
		p := rig.publication(t, head)
		p.Title, p.Body = "", ""
		var result executionPublicationResult
		if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
			t.Fatal(err)
		}
		if actual := remoteHead(); actual != head || result.URL != publicationFixturePRURL || result.Branch != branch || rig.creates != 0 {
			t.Fatalf("continuation duplicated or missed PR: %+v actual=%s creates=%d", result, actual, rig.creates)
		}
		last = head
	}
	// The assigned pull request moved to another branch, or was closed: no
	// push, and even title/body flags create no replacement.
	for _, change := range []func(){
		func() { pullBranch = "another-branch" },
		func() { pullBranch, pullState = branch, noteStatusClosed },
	} {
		change()
		p := rig.publication(t, rig.commit(t, "change.txt", "another fix "+pullBranch+pullState+"\n"))
		var result executionPublicationResult
		err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
		if err == nil || rig.creates != 0 || remoteHead() != last {
			t.Fatalf("unavailable assignment (branch %s, %s) was published: creates=%d err=%v", pullBranch, pullState, rig.creates, err)
		}
	}
}

func TestContinuationPublicationRejectsInvalidTargets(t *testing.T) {
	old := executionPublishGH
	t.Cleanup(func() { executionPublishGH = old })
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name    string
		change  func(pull, head map[string]any)
		repairs []string
		readErr bool
	}{
		{name: "closed pull request", change: func(pull, _ map[string]any) { pull["state"] = noteStatusClosed }},
		{name: "head in a fork", change: func(_, head map[string]any) {
			head["repo"] = map[string]string{continuationFixtureRepoKey: "other/beekeeper"}
		}},
		{name: "head on the base branch", change: func(_, head map[string]any) { head[fieldRef] = mainBranch }},
		{name: "two pull requests in the repository", repairs: []string{sshTestRepository + "#42", sshTestRepository + "#43"}},
		{name: "unreadable pull request", readErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pull := map[string]any{"state": noteStatusOpen, continuationFixtureURLKey: publicationFixturePRURL,
				"head": map[string]any{fieldRef: "fix/example", continuationFixtureSHAKey: sha, "repo": map[string]string{continuationFixtureRepoKey: sshTestRepository}},
				"base": map[string]string{fieldRef: mainBranch}}
			if tc.change != nil {
				tc.change(pull, pull["head"].(map[string]any))
			}
			executionPublishGH = func(context.Context, ...string) ([]byte, error) {
				if tc.readErr {
					return nil, fmt.Errorf("unavailable")
				}
				return json.Marshal(pull)
			}
			repairs := tc.repairs
			if len(repairs) == 0 {
				repairs = []string{sshTestRepository + "#42"}
			}
			if _, err := continuationPublication(t.Context(), sshTestRepository, repairs); err == nil {
				t.Fatal("invalid continuation accepted")
			}
		})
	}
	// A repair in another repository is no target here, and is not read.
	executionPublishGH = func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("read a pull request of another repository: %v", args)
		return nil, nil
	}
	if target, err := continuationPublication(t.Context(), sshTestRepository, []string{"giantswarm/devctl#42"}); target != nil || err != nil {
		t.Fatalf("other repository's repair = %+v, %v", target, err)
	}
}

func TestMergedPullClosesOnlyRecordedSupersededExecutionPulls(t *testing.T) {
	oldGH, oldRead := executionPublishGH, readRepoOpenPulls
	t.Cleanup(func() { executionPublishGH, readRepoOpenPulls = oldGH, oldRead })
	const repo = sshTestRepository
	pullURL := func(n int) string { return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n) }
	merged := superloop.Pull{Ref: repo + "#43", URL: pullURL(43), Title: "fix: the task", Body: "Closes #10"}
	// Every execution shares one brief line: the task is the issue a pull
	// request closes, never the line.
	const task = "Work only in the worktree of the shared brief"
	start := func(repository, url, branch string) state.Start {
		return state.Start{Execution: &state.ExecutionRecord{
			Request:   execution.StartRequest{Repository: repository, Task: task},
			Published: &state.ExecutionPublication{URL: url, Branch: branch},
		}}
	}
	st := &state.State{Starts: []state.Start{
		start(repo, pullURL(43), "beekeeper-execution/merged/1"),
		start(repo, pullURL(43), "beekeeper-execution/merged/1"), // its continuation
		start(repo, pullURL(42), "beekeeper-execution/older/1"),
		start(repo, pullURL(44), "beekeeper-execution/other-issue/1"),
		start(repo, pullURL(46), "beekeeper-execution/moved/1"),
		start(repo, pullURL(47), "beekeeper-execution/linked/1"),
		start("giantswarm/devctl", "https://github.com/giantswarm/devctl/pull/42", "beekeeper-execution/older/1"),
	}}
	var open []unmergedPull
	if err := json.Unmarshal([]byte(`[
		{"number":42,"url":"https://github.com/giantswarm/beekeeper/pull/42","state":"OPEN","headRefName":"beekeeper-execution/older/1","title":"fix: the task","body":"Closes #10"},
		{"number":44,"url":"https://github.com/giantswarm/beekeeper/pull/44","state":"OPEN","headRefName":"beekeeper-execution/other-issue/1","title":"fix: the task","body":"Closes #11"},
		{"number":45,"url":"https://github.com/giantswarm/beekeeper/pull/45","state":"OPEN","headRefName":"fix/by-a-person","title":"fix: the task","body":"Fixes #10"},
		{"number":46,"url":"https://github.com/giantswarm/beekeeper/pull/46","state":"OPEN","headRefName":"renamed","title":"fix: the task","body":"Closes #10"},
		{"number":47,"url":"https://github.com/giantswarm/beekeeper/pull/47","state":"OPEN","headRefName":"beekeeper-execution/linked/1","title":"fix: the task","body":"Part of #10","closingIssuesReferences":[{"number":10,"repository":{"nameWithOwner":"giantswarm/beekeeper"}}]}
	]`), &open); err != nil {
		t.Fatal(err)
	}
	readRepoOpenPulls = func(_ context.Context, r string) ([]unmergedPull, error) {
		if r != repo {
			t.Fatalf("read the open pull requests of %s", r)
		}
		return open, nil
	}
	var closedPulls []string
	executionPublishGH = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) < 3 || strings.Join(args[:2], " ") != "pr close" {
			t.Fatalf("unexpected command: %v", args)
		}
		comment := args[len(args)-1]
		if !strings.Contains(comment, merged.URL) || !strings.Contains(comment, "https://github.com/giantswarm/beekeeper/issues/10") {
			t.Fatalf("supersession comment %q names neither the merged pull request nor the issue", comment)
		}
		closedPulls = append(closedPulls, args[2])
		return nil, nil
	}
	got, err := closeDuplicateExecutionPulls(t.Context(), st, merged)
	want := []string{pullURL(42), pullURL(47)}
	if err != nil || !slices.Equal(got, want) || !slices.Equal(closedPulls, want) {
		t.Fatalf("closed %v (reported %v), want %v; err %v", closedPulls, got, want, err)
	}

	// A merged pull request that closes no issue names no task, and one
	// whose repository has no other execution publication has nothing to
	// close: neither reads GitHub.
	readRepoOpenPulls = func(_ context.Context, r string) ([]unmergedPull, error) {
		t.Fatalf("read the open pull requests of %s", r)
		return nil, nil
	}
	for _, tc := range []struct {
		name   string
		merged superloop.Pull
		st     *state.State
	}{
		{"no closing issue", superloop.Pull{Ref: repo + "#43", URL: pullURL(43), Body: "Part of #10"}, st},
		{"no other publication", merged, &state.State{Starts: st.Starts[:2]}},
	} {
		if got, err := closeDuplicateExecutionPulls(t.Context(), tc.st, tc.merged); err != nil || len(got) != 0 {
			t.Fatalf("%s: closed %v, err %v", tc.name, got, err)
		}
	}
}
