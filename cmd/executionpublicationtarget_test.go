package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
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
	rig.created = true
	originalGH := executionPublishGH
	executionPublishGH = func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == workerGitHubAPIGroup {
			return []byte(fmt.Sprintf(`{"state":"open","html_url":%q,"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`, publicationFixturePRURL, branch, priorHead, rig.request.Repository)), nil
		}
		if args[0] == "pr" && args[1] == transcriptListCommand && !strings.Contains(strings.Join(args, " "), "--head "+branch+" ") {
			return []byte(`[]`), nil
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
	if err != nil || target == nil {
		t.Fatalf("resolve continuation: %v, %v", target, err)
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
	for _, content := range []string{"fix round one\n", "fix round two\n"} {
		head := rig.commit(t, "change.txt", content)
		p := rig.publication(t, head)
		p.Title, p.Body = "", ""
		var result executionPublicationResult
		if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
			t.Fatal(err)
		}
		actual, err := publishGit(ctx, rig.remote, "rev-parse", "refs/heads/"+branch)
		if err != nil || actual != head || result.URL != publicationFixturePRURL || rig.creates != 0 {
			t.Fatalf("continuation duplicated or missed PR: %+v actual=%s creates=%d err=%v", result, actual, rig.creates, err)
		}
	}
	// If the assigned PR disappears, even title/body flags cannot create a replacement.
	rig.created = false
	head := rig.commit(t, "change.txt", "another fix\n")
	p := rig.publication(t, head)
	var result executionPublicationResult
	err = coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
	if err == nil || rig.creates != 0 {
		t.Fatalf("missing assignment created a replacement PR: creates=%d err=%v", rig.creates, err)
	}
}

func TestContinuationPublicationRejectsInvalidTargets(t *testing.T) {
	old := executionPublishGH
	t.Cleanup(func() { executionPublishGH = old })
	for _, mode := range []string{noteStatusClosed, "fork", "base", "multiple repairs", "read failure"} {
		t.Run(mode, func(t *testing.T) {
			pull := map[string]any{"state": noteStatusOpen, continuationFixtureURLKey: publicationFixturePRURL,
				"head": map[string]any{fieldRef: "fix/example", continuationFixtureSHAKey: strings.Repeat("a", 40), "repo": map[string]string{continuationFixtureRepoKey: sshTestRepository}},
				"base": map[string]string{fieldRef: mainBranch}}
			head := pull["head"].(map[string]any)
			switch mode {
			case noteStatusClosed:
				pull["state"] = noteStatusClosed
			case "fork":
				head["repo"] = map[string]string{continuationFixtureRepoKey: "other/beekeeper"}
			case "base":
				head[fieldRef] = mainBranch
			}
			executionPublishGH = func(context.Context, ...string) ([]byte, error) {
				if mode == "read failure" {
					return nil, fmt.Errorf("unavailable")
				}
				return json.Marshal(pull)
			}
			repairs := []string{sshTestRepository + "#42"}
			if mode == "multiple repairs" {
				repairs = append(repairs, "giantswarm/beekeeper#43")
			}
			if _, err := continuationPublication(t.Context(), sshTestRepository, repairs); err == nil {
				t.Fatal("invalid continuation accepted")
			}
		})
	}
}

func TestMergedContinuationClosesOnlyRecordedTaskDuplicates(t *testing.T) {
	old := executionPublishGH
	t.Cleanup(func() { executionPublishGH = old })
	const merged = "https://github.com/giantswarm/beekeeper/pull/43"
	const repo = sshTestRepository
	const task = "same task"
	makeStart := func(task, url, branch string) state.Start {
		return state.Start{Execution: &state.ExecutionRecord{
			Request:   execution.StartRequest{Repository: repo, Task: task},
			Published: &state.ExecutionPublication{URL: url, Branch: branch},
		}}
	}
	st := &state.State{Starts: []state.Start{
		makeStart(task, merged, "fix/new"),
		makeStart(task, publicationFixturePRURL, "fix/old"),
		makeStart("different task", "https://github.com/giantswarm/beekeeper/pull/44", "fix/other"),
	}}
	closed := 0
	executionPublishGH = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "pr " + transcriptListCommand:
			if !strings.Contains(strings.Join(args, " "), "--head fix/old ") {
				t.Fatalf("unrelated branch read: %v", args)
			}
			return json.Marshal([]publicationPull{{URL: publicationFixturePRURL, State: emergencyOpenState}})
		case "pr close":
			if args[2] != publicationFixturePRURL || !strings.Contains(args[len(args)-1], merged) {
				t.Fatalf("wrong supersession: %v", args)
			}
			closed++
			return nil, nil
		default:
			t.Fatalf("unexpected command: %v", args)
			return nil, nil
		}
	}
	if err := closeDuplicateExecutionPulls(t.Context(), st, merged); err != nil || closed != 1 {
		t.Fatalf("close duplicate: count=%d err=%v", closed, err)
	}
}
