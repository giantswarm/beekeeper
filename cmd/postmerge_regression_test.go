package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const postMergeRegressionWork = "verify the rollout"

func TestParkPostMergeCanExplicitlyClearWork(t *testing.T) {
	w := parkingWatch(t, map[string]string{refOne: github.Open})
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	parkReviewHead = func(string) (string, string) { return loopHead, parkVerificationUnavailable }
	for _, step := range []struct {
		name  string
		flags []string
		want  string
	}{
		{name: "declare work", flags: []string{"--post-merge", postMergeRegressionWork}, want: postMergeRegressionWork},
		{name: "omit inherits", want: postMergeRegressionWork},
		{name: "explicit empty clears", flags: []string{"--post-merge", ""}},
		{name: "omit preserves cleared work"},
	} {
		t.Run(step.name, func(t *testing.T) {
			args := append([]string{"--on", refOne}, step.flags...)
			if err := park(t, w, append(args, deskReviewWait)...); err != nil {
				t.Fatal(err)
			}
			st, err := watchApp(w.Host).store.Read()
			if err != nil {
				t.Fatal(err)
			}
			rp, ok := st.ReviewParkFor(refOne)
			current := st.Agents[0].Park
			if !ok || current == nil || current.PostMerge == nil || *current.PostMerge != step.want || rp.PostMerge == nil || *rp.PostMerge != step.want {
				t.Fatalf("current park %+v and saved park %+v, want %q", current, rp, step.want)
			}
		})
	}
}

func TestExecutionParkPostMergeCanExplicitlyClearWork(t *testing.T) {
	a, request := executionTestApp(t)
	on := sshTestRepository + "#7"
	was := executionReviewGH
	t.Cleanup(func() { executionReviewGH = was })
	executionReviewGH = func(context.Context, ...string) ([]byte, error) {
		return fmt.Appendf(nil, `{"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}}}`, executionBranch(request.Ref), loopHead, sshTestRepository), nil
	}
	work, empty := postMergeRegressionWork, ""
	for _, step := range []struct {
		name string
		next *string
		want string
	}{
		{name: "declare work", next: &work, want: work},
		{name: "omit inherits", want: work},
		{name: "explicit empty clears", next: &empty},
		{name: "omit preserves cleared work"},
	} {
		t.Run(step.name, func(t *testing.T) {
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				_, events, err := a.applyExecutionControl(t.Context(), st, st.Agents[0].Party, execution.ControlRequest{
					Operation: execution.ControlPark,
					Data:      mustExecutionJSON(t, executionPark{On: on, Waits: deskReviewWait, Head: loopHead, PostMerge: step.next}),
				})
				return events, err
			}); err != nil {
				t.Fatal(err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			rp, ok := st.ReviewParkFor(on)
			current := st.Agents[0].Park
			if !ok || current == nil || current.ReviewHead != loopHead || current.PostMerge == nil || *current.PostMerge != step.want || rp.PostMerge == nil || *rp.PostMerge != step.want {
				t.Fatalf("remote park %+v and saved park %+v, want %q", current, rp, step.want)
			}
		})
	}
}
