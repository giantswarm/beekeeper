package merge

import (
	"testing"
	"time"
)

func TestBusyBase(t *testing.T) {
	for doc, want := range map[string]bool{
		`{"exitCode":3,"reason":"the pull request is behind main, which requires branches to be up to date (main moved during the wait) after 3 waits; run the merge again once main is quiet"}`:                                            true,
		`{"exitCode":3,"reason":"the pull request is 2 commit(s) behind main: its checks ran against an older main than the one the merge lands it on (main moved during the wait) after 3 waits; run the merge again once main is quiet"}`: true,
		`{"exitCode":3,"reason":"the pull request is behind main, which requires branches to be up to date; --update-branch updates it from main and waits for its checks again (or update the branch and run the merge again)"}`:           false,
		`{"exitCode":1,"reason":"(main moved during the wait) after 3 waits; run the merge again once main is quiet"}`:                                                                                                                      false,
		`{`: false,
	} {
		if got := BusyBase([]byte(doc)); got != want {
			t.Errorf("BusyBase(%s) = %v, want %v", doc, got, want)
		}
	}
}

func TestBusyNext(t *testing.T) {
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	pauses := []time.Duration{time.Minute, 5 * time.Minute}
	for _, c := range []struct {
		run    int
		after  time.Duration
		budget time.Duration
		pause  time.Duration
		ok     bool
	}{
		{run: 1, after: 50 * time.Minute, budget: time.Hour, pause: time.Minute, ok: true},
		{run: 2, after: 50 * time.Minute, budget: time.Hour, pause: 5 * time.Minute, ok: true},
		{run: 7, after: 50 * time.Minute, budget: time.Hour, pause: 5 * time.Minute, ok: true},
		{run: 7, after: 56 * time.Minute, budget: time.Hour, pause: 5 * time.Minute, ok: false},
		{run: 1, after: time.Minute, budget: 0, pause: time.Minute, ok: false},
	} {
		pause, ok := BusyNext(pauses, c.run, start, start.Add(c.after), c.budget)
		if pause != c.pause || ok != c.ok {
			t.Errorf("BusyNext(run %d, %s in, budget %s) = %s, %v; want %s, %v", c.run, c.after, c.budget, pause, ok, c.pause, c.ok)
		}
	}
}

func TestUpdatesBranch(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{Tool, "pr", mergeVerb, failedRepo, "7", updateBranchFlag}, true},
		{[]string{Tool, "pr", mergeVerb, failedRepo, "7", rebaseFlag, updateBranchFlag + "=true"}, true},
		{[]string{Tool, "pr", mergeVerb, failedRepo, "7"}, false},
		{[]string{Tool, "pr", mergeVerb, failedRepo, "7", "--", updateBranchFlag}, false},
		{[]string{Tool, "release", "promote", failedRepo, updateBranchFlag}, false},
	} {
		if got := UpdatesBranch(c.argv); got != c.want {
			t.Errorf("UpdatesBranch(%v) = %v, want %v", c.argv, got, c.want)
		}
	}
}
