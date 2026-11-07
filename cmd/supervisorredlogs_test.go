package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestRedHandoffIncludesBoundedFailureOutput(t *testing.T) {
	old := readRedJobLog
	t.Cleanup(func() { readRedJobLog = old })
	calls := 0
	readRedJobLog = func(_ context.Context, repo string, job int64) (string, error) {
		calls++
		if repo != "o/r" || job != 11 {
			t.Fatalf("unexpected log request %s %d", repo, job)
		}
		return strings.Repeat("x", 13000) + "FAIL: useful tail", nil
	}
	url := "https://github.com/o/r/actions/runs/1/job/11"
	act := attachRedLogs(context.Background(), superloop.Action{Ref: "o/r#7", Message: url + " " + url})
	if calls != 1 || !strings.Contains(act.Message, "earlier output truncated") || !strings.HasSuffix(act.Message, "FAIL: useful tail") || len(act.Message) > 12500 {
		t.Fatalf("calls=%d, output length=%d", calls, len(act.Message))
	}
}

func TestRedHandoffRetainsLinkWhenLogsUnavailable(t *testing.T) {
	old := readRedJobLog
	t.Cleanup(func() { readRedJobLog = old })
	readRedJobLog = func(context.Context, string, int64) (string, error) { return "", errors.New("unavailable") }
	url := "https://github.com/o/r/actions/runs/1/job/11"
	act := attachRedLogs(context.Background(), superloop.Action{Ref: "o/r#7", Message: url})
	if !strings.Contains(act.Message, "CI failure output unavailable; read "+url) {
		t.Fatal(act.Message)
	}
}
