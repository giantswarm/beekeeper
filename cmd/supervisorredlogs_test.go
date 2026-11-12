package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	redLogRepo = "red/logs"
	redLogRef  = "red/logs#7"
	redLogURL  = "https://github.com/red/logs/actions/runs/1/job/11"
)

func TestRedHandoffIncludesBoundedFailureOutput(t *testing.T) {
	oldGH := github.RunGH
	t.Cleanup(func() { github.RunGH = oldGH })
	calls := 0
	github.RunGH = github.Retrying(func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		want := []string{workerGitHubAPIGroup, "--allow-escape-sequences", "repos/red/logs/actions/jobs/11/logs"}
		if github.Repo(ctx) != redLogRepo || !slices.Equal(args, want) {
			t.Fatalf("unexpected gh call repo=%q args=%v", github.Repo(ctx), args)
		}
		return []byte(strings.Repeat("x", 13000) + "FAIL: useful tail"), nil
	}, time.Millisecond)
	url := redLogURL
	act := loop.AttachRedLogs(context.Background(), superloop.Action{Ref: redLogRef, Message: url + " " + url})
	if calls != 1 || !strings.Contains(act.Message, "CI failure output from "+url+":") || !strings.Contains(act.Message, "earlier output truncated") || !strings.HasSuffix(act.Message, "FAIL: useful tail") || len(act.Message) > 12500 {
		t.Fatalf("calls=%d, output length=%d, msg=%q", calls, len(act.Message), act.Message)
	}
}

func TestRedHandoffRetainsLinkWhenLogsUnavailable(t *testing.T) {
	old := loop.ReadRedJobLog
	t.Cleanup(func() { loop.ReadRedJobLog = old })
	loop.ReadRedJobLog = func(context.Context, string, int64) (string, error) { return "", errors.New("unavailable") }
	url := redLogURL
	act := loop.AttachRedLogs(context.Background(), superloop.Action{Ref: redLogRef, Message: url})
	if !strings.Contains(act.Message, "CI failure output unavailable; read "+url) {
		t.Fatal(act.Message)
	}
}
