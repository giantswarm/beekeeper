package cmd

import (
	"fmt"
	"strings"
	"testing"
)

const (
	watchWorkflowName = "CI Build"
	watchWorkflowRun  = "workflowRun"
	watchRunAttempt   = "runAttempt"
	watchQueued       = "QUEUED"
)

func watchSuite(run, workflow int, status, conclusion string) map[string]any {
	return map[string]any{
		"status": status, "conclusion": conclusion,
		watchWorkflowRun: map[string]any{
			"databaseId": run, watchRunAttempt: 1,
			"url":      fmt.Sprintf("https://github.com/o/r/actions/runs/%d", run),
			"workflow": map[string]any{"databaseId": workflow, "name": watchWorkflowName},
		},
	}
}

func watchJob(run int, conclusion string) map[string]any {
	return map[string]any{
		"__typename": "CheckRun", "name": "ci-ok", "status": checkCompleted,
		"conclusion": conclusion,
		"detailsUrl": fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/%d", run, run),
		"checkSuite": map[string]any{watchWorkflowRun: map[string]any{"workflow": map[string]any{"name": watchWorkflowName}}},
	}
}

func TestPullWatcherNewestWorkflowGatesGreen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		suites  []any
		jobs    []any
		green   bool
		red     int
		pending bool
		unread  bool
	}{
		{
			name:   "failed job and suite superseded by green on same head",
			suites: []any{watchSuite(10, 1, checkCompleted, checkFailure), watchSuite(20, 1, checkCompleted, checkSuccess)},
			jobs:   []any{watchJob(10, checkFailure), watchJob(20, checkSuccess)}, green: true,
		},
		{
			name:   "newest failed workflow created no job",
			suites: []any{watchSuite(20, 1, checkCompleted, checkFailure), watchSuite(10, 1, checkCompleted, checkSuccess)},
			jobs:   []any{watchJob(10, checkSuccess)}, red: 1,
		},
		{
			name:   "queued replacement created no job",
			suites: []any{watchSuite(20, 1, watchQueued, ""), watchSuite(10, 1, checkCompleted, checkSuccess)},
			jobs:   []any{watchJob(10, checkSuccess)}, pending: true,
		},
		{
			name:   "successful replacement missing old job",
			suites: []any{watchSuite(20, 1, checkCompleted, checkSuccess)},
			jobs:   []any{watchJob(10, checkSuccess)}, pending: true,
		},
		{
			name:   "same workflow name does not hide independent failure",
			suites: []any{watchSuite(10, 1, checkCompleted, checkFailure), watchSuite(20, 2, checkCompleted, checkSuccess)},
			jobs:   []any{watchJob(20, checkSuccess)}, red: 1,
		},
		{
			name:   "external suite has no workflow",
			suites: []any{map[string]any{"status": watchQueued, watchWorkflowRun: nil}},
			jobs:   []any{watchJob(20, checkSuccess)}, green: true,
		},
		{
			name:   "missing workflow identity stays unread",
			suites: []any{watchSuite(20, 0, checkCompleted, checkSuccess)},
			jobs:   []any{watchJob(20, checkSuccess)}, unread: true,
		},
		{
			name: "missing suites stay unread",
			jobs: []any{watchJob(20, checkSuccess)}, unread: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakePullGraphQL(t, func(query string, _ int) map[string]any {
				if !strings.Contains(query, "checkSuites(first:100)") || !strings.Contains(query, watchRunAttempt) {
					t.Fatal("batch omitted workflow suites")
				}
				p := pullGraphQLFixture(t, query, checkSuccess, loopHead, watchFixtureUpdated)
				pullConnection(p, pullSuites)[pullNodes] = tc.suites
				pullConnection(p, pullContexts)[pullNodes] = tc.jobs
				return p
			})
			seen, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := seen[loopPR]
			if *calls != 1 || got.Pull.Green != tc.green || len(got.Pull.Red) != tc.red || got.Pending != tc.pending || (got.Error != "") != tc.unread {
				t.Fatalf("calls=%d observation=%+v", *calls, got)
			}
		})
	}
}

func TestPullWatcherWorkflowChangesWithoutPRUpdate(t *testing.T) {
	status, conclusion := checkCompleted, checkSuccess
	calls := fakePullGraphQL(t, func(query string, _ int) map[string]any {
		p := pullGraphQLFixture(t, query, checkSuccess, loopHead, watchFixtureUpdated)
		pullConnection(p, pullSuites)[pullNodes] = []any{watchSuite(20, 1, status, conclusion)}
		pullConnection(p, pullContexts)[pullNodes] = []any{watchJob(20, checkSuccess)}
		return p
	})
	first, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil || !first[loopPR].Pull.Green {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	status, conclusion = watchQueued, ""
	queued, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, first, 0)
	if err != nil || queued[loopPR].Pull.Green || !queued[loopPR].Pending {
		t.Fatalf("queued=%+v err=%v", queued, err)
	}
	status, conclusion = checkCompleted, checkFailure
	failed, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, queued, 0)
	if err != nil || failed[loopPR].Pull.Green || len(failed[loopPR].Pull.Red) != 1 || *calls != 3 {
		t.Fatalf("failed=%+v calls=%d err=%v", failed, *calls, err)
	}
}

func TestPullWatcherPaginatesWorkflowSuites(t *testing.T) {
	calls := fakePullGraphQL(t, func(query string, call int) map[string]any {
		p := pullGraphQLFixture(t, query, checkSuccess, loopHead, watchFixtureUpdated)
		pullConnection(p, pullContexts)[pullNodes] = []any{watchJob(10, checkSuccess)}
		suites := pullConnection(p, pullSuites)
		if call == 1 {
			suites[pullNodes] = []any{watchSuite(10, 1, checkCompleted, checkSuccess)}
			suites[pullPageInfo] = map[string]any{pullHasNext: true, pullEndCursor: "more-suites"}
		} else {
			if !strings.Contains(query, `checkSuites(first:100,after:"more-suites")`) {
				t.Fatal("missing suite cursor")
			}
			suites[pullNodes] = []any{watchSuite(20, 1, checkCompleted, checkFailure)}
		}
		return p
	})
	seen, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil || *calls != 2 || seen[loopPR].Error != "" || seen[loopPR].Pull.Green || len(seen[loopPR].Pull.Red) != 1 {
		t.Fatalf("truncated suites: calls=%d seen=%+v err=%v", *calls, seen, err)
	}
}

func TestPullWatcherWorkflowRerunAttempt(t *testing.T) {
	fakePullGraphQL(t, func(query string, _ int) map[string]any {
		p := pullGraphQLFixture(t, query, checkSuccess, loopHead, watchFixtureUpdated)
		latest := watchSuite(20, 1, checkCompleted, checkSuccess)
		latest[watchWorkflowRun].(map[string]any)[watchRunAttempt] = 2
		pullConnection(p, pullSuites)[pullNodes] = []any{latest, watchSuite(20, 1, checkCompleted, checkFailure)}
		pullConnection(p, pullContexts)[pullNodes] = []any{watchJob(20, checkSuccess)}
		return p
	})
	seen, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil || seen[loopPR].Error != "" || !seen[loopPR].Pull.Green || len(seen[loopPR].Pull.Red) != 0 {
		t.Fatalf("rerun=%+v err=%v", seen, err)
	}
}

func TestPullWatcherIncompleteWorkflowSuitesStayUnread(t *testing.T) {
	for _, mode := range []string{"missing cursor", "repeated cursor", "moved head"} {
		t.Run(mode, func(t *testing.T) {
			fakePullGraphQL(t, func(query string, call int) map[string]any {
				p := pullGraphQLFixture(t, query, checkSuccess, loopHead, watchFixtureUpdated)
				page := map[string]any{pullHasNext: true, pullEndCursor: "more"}
				if mode == "missing cursor" {
					delete(page, pullEndCursor)
				}
				if mode == "moved head" && call == 2 {
					p[pullHeadField] = loopFixed
				}
				pullConnection(p, pullSuites)[pullPageInfo] = page
				return p
			})
			seen, err := fetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
			if err != nil || seen[loopPR].Error == "" || seen[loopPR].Pull.Green {
				t.Fatalf("incomplete suites=%+v err=%v", seen, err)
			}
		})
	}
}
