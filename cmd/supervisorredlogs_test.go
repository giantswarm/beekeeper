package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	redLogRepo = "red/logs"
	redLogRef  = "red/logs#7"
	redLogURL  = "https://github.com/red/logs/actions/runs/1/job/11"
)

func TestRedHandoffIncludesBoundedFailureOutput(t *testing.T) {
	old := readRedJobLog
	t.Cleanup(func() { readRedJobLog = old })
	calls := 0
	readRedJobLog = func(_ context.Context, repo string, job int64) (string, error) {
		calls++
		if repo != redLogRepo || job != 11 {
			t.Fatalf("unexpected log request %s %d", repo, job)
		}
		return strings.Repeat("x", 13000) + "FAIL: useful tail", nil
	}
	url := redLogURL
	act := attachRedLogs(context.Background(), superloop.Action{Ref: redLogRef, Message: url + " " + url})
	if calls != 1 || !strings.Contains(act.Message, "earlier output truncated") || !strings.HasSuffix(act.Message, "FAIL: useful tail") || len(act.Message) > 12500 {
		t.Fatalf("calls=%d, output length=%d", calls, len(act.Message))
	}
}

func TestRedHandoffRetainsLinkWhenLogsUnavailable(t *testing.T) {
	old := readRedJobLog
	t.Cleanup(func() { readRedJobLog = old })
	readRedJobLog = func(context.Context, string, int64) (string, error) { return "", errors.New("unavailable") }
	url := redLogURL
	act := attachRedLogs(context.Background(), superloop.Action{Ref: redLogRef, Message: url})
	if !strings.Contains(act.Message, "CI failure output unavailable; read "+url) {
		t.Fatal(act.Message)
	}
}
