//go:build unix

package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Keep the production command tree and execution admission, replacing only
// configuration loading and the app backing the agents subtree with fixtures.
func reviewCommandTree(t *testing.T, a *app) *cobra.Command {
	t.Helper()
	root := New()
	agents, _, err := root.Find([]string{agentsName})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, child := range agents.Commands() {
		if child.Name() == taskKindReview {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("agents registers %d review commands, want one", count)
	}
	root.RemoveCommand(agents)
	root.AddCommand(a.agentsCmd())
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return a.checkExecutionCommand(cmd)
	}
	return root
}

func TestAgentsReviewCommandTreeRelaysAndCompletes(t *testing.T) {
	a, r, pull := reviewRelayRig(t)
	rig := &publicationRig{a: a, request: r, workspace: t.TempDir(), base: pull.Head}
	_, _, _, bodyPath := ownedPublicationHost(t, rig)
	body := "Review: approved\nHead reviewed: `" + pull.Head + "`\nChecked the fix."
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/" + r.Repository + "/pull/7#pullrequestreview-100"
	oldGH, oldReview := github.RunGH, readDeskReview
	t.Cleanup(func() { github.RunGH, readDeskReview = oldGH, oldReview })
	posts := 0
	github.RunGH = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "pr view":
			return []byte(`{"title":"assigned review","closingIssuesReferences":[]}`), nil
		case "pr diff":
			return []byte("assigned diff"), nil
		default:
			if len(args) == 4 && args[2] == "--paginate" {
				return []byte(`[[]]`), nil
			}
			want := []string{workerGitHubAPIGroup, "repos/" + r.Repository + "/pulls/7/reviews", "--method", "POST", reviewRelayRawField, "event=COMMENT", reviewRelayRawField, "commit_id=" + pull.Head, reviewRelayRawField, "body=" + body}
			if strings.Join(args, "\n") != strings.Join(want, "\n") {
				t.Fatalf("unexpected review write: %v", args)
			}
			posts++
			return []byte(`{"html_url":"` + url + `"}`), nil
		}
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{nil, "assigned review"},
		{[]string{"--diff"}, "assigned diff"},
		{[]string{"--body-file", bodyPath}, url},
	} {
		root := reviewCommandTree(t, a)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(append([]string{agentsName, taskKindReview}, tt.args...))
		if err := root.ExecuteContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tt.want) {
			t.Fatalf("review output %q lacks %q", out.String(), tt.want)
		}
	}
	if posts != 1 {
		t.Fatalf("posted %d reviews, want one", posts)
	}
	readDeskReview = func(_ context.Context, repo string, pr int, id string) (superloop.Review, error) {
		if repo != r.Repository || pr != 7 || id != "100" {
			t.Fatal("completion read a different review")
		}
		return superloop.Review{ID: id, URL: url, Author: "fixture-reviewer", Head: pull.Head, Commit: pull.Head, Body: body, Verdict: superloop.Approved, At: time.Now()}, nil
	}
	root := reviewCommandTree(t, a)
	root.SetArgs([]string{agentsName, verbIdle, "--done", "--report", "Review: approved\n" + url, "--problem", ghNone})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.DeskReviews[0].Result == nil || st.DeskReviews[0].Result.URL != url || !st.Agents[0].Done {
		t.Fatal("posted review did not complete the assigned reviewer")
	}
}

func TestAgentsReviewCommandTreePreservesCoordinatorAndRejectsMixedForms(t *testing.T) {
	r := newLoopRig(t)
	r.pull.Green = false
	root := reviewCommandTree(t, r.a)
	root.SetArgs([]string{agentsName, taskKindReview, loopPR})
	if err := root.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "CI must be green") {
		t.Fatalf("coordinator review did not enforce green CI: %v", err)
	}
	for _, args := range [][]string{
		{loopPR, "--diff"}, {loopPR, "--body-file", "unused.md"},
		{"--model", defaultReviewModel}, {"--verdict"}, {"--nits-of", loopHead}, {"--light-lines", "10"},
		{loopPR, loopPR},
	} {
		root = reviewCommandTree(t, r.a)
		root.SetArgs(append([]string{agentsName, taskKindReview}, args...))
		if err := root.ExecuteContext(t.Context()); err == nil {
			t.Fatalf("mixed or invalid review form accepted: %v", args)
		}
	}
	// Calling the handler directly must not let a managed worker select a PR.
	t.Setenv(execution.EnvID, "managed-fixture")
	cmd := r.a.agentReviewCmd()
	if err := cmd.RunE(cmd, []string{loopPR}); err == nil || !strings.Contains(err.Error(), "without a pull request reference") {
		t.Fatalf("managed worker reached coordinator review: %v", err)
	}
}
