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
	watchStatusField  = "status"
	watchNameField    = "name"
	watchSuiteCursor  = "more"
	watchMovedHead    = "moved head"
)

func watchSuite(run, workflow int, status, conclusion string) map[string]any {
	return map[string]any{
		watchStatusField: status, "conclusion": conclusion,
		watchWorkflowRun: map[string]any{
			"databaseId": run, watchRunAttempt: 1,
			"url":      fmt.Sprintf("https://github.com/o/r/actions/runs/%d", run),
			"workflow": map[string]any{"databaseId": workflow, watchNameField: watchWorkflowName},
		},
	}
}

func watchJob(run int, conclusion string) map[string]any {
	return map[string]any{
		"__typename": "CheckRun", watchNameField: "ci-ok", watchStatusField: CheckCompleted,
		"conclusion": conclusion,
		"detailsUrl": fmt.Sprintf("https://github.com/o/r/actions/runs/%d/job/%d", run, run),
		"checkSuite": map[string]any{watchWorkflowRun: map[string]any{"workflow": map[string]any{watchNameField: watchWorkflowName}}},
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
			suites: []any{watchSuite(10, 1, CheckCompleted, CheckFailure), watchSuite(20, 1, CheckCompleted, CheckSuccess)},
			jobs:   []any{watchJob(10, CheckFailure), watchJob(20, CheckSuccess)}, green: true,
		},
		{
			name:   "newest failed workflow created no job",
			suites: []any{watchSuite(20, 1, CheckCompleted, CheckFailure), watchSuite(10, 1, CheckCompleted, CheckSuccess)},
			jobs:   []any{watchJob(10, CheckSuccess)}, red: 1,
		},
		{
			name:   "queued replacement created no job",
			suites: []any{watchSuite(20, 1, watchQueued, ""), watchSuite(10, 1, CheckCompleted, CheckSuccess)},
			jobs:   []any{watchJob(10, CheckSuccess)}, pending: true,
		},
		{
			name:   "successful replacement missing old job",
			suites: []any{watchSuite(20, 1, CheckCompleted, CheckSuccess)},
			jobs:   []any{watchJob(10, CheckSuccess)}, pending: true,
		},
		{
			name:   "same workflow name does not hide independent failure",
			suites: []any{watchSuite(10, 1, CheckCompleted, CheckFailure), watchSuite(20, 2, CheckCompleted, CheckSuccess)},
			jobs:   []any{watchJob(20, CheckSuccess)}, red: 1,
		},
		{
			name:   "external suite has no workflow",
			suites: []any{map[string]any{watchStatusField: watchQueued, watchWorkflowRun: nil}},
			jobs:   []any{watchJob(20, CheckSuccess)}, green: true,
		},
		{
			name:   "missing workflow identity stays unread",
			suites: []any{watchSuite(20, 0, CheckCompleted, CheckSuccess)},
			jobs:   []any{watchJob(20, CheckSuccess)}, unread: true,
		},
		{
			name: "missing suites stay unread",
			jobs: []any{watchJob(20, CheckSuccess)}, unread: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakePullGraphQL(t, func(query string, _ int) map[string]any {
				if !strings.Contains(query, "checkSuites(first:100)") || !strings.Contains(query, watchRunAttempt) {
					t.Fatal("batch omitted workflow suites")
				}
				p := pullGraphQLFixture(t, query, CheckSuccess, loopHead, watchFixtureUpdated)
				PullConnection(p, PullSuites)[PullNodes] = tc.suites
				PullConnection(p, PullContexts)[PullNodes] = tc.jobs
				return p
			})
			seen, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
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
	status, conclusion := CheckCompleted, CheckSuccess
	calls := fakePullGraphQL(t, func(query string, _ int) map[string]any {
		p := pullGraphQLFixture(t, query, CheckSuccess, loopHead, watchFixtureUpdated)
		PullConnection(p, PullSuites)[PullNodes] = []any{watchSuite(20, 1, status, conclusion)}
		PullConnection(p, PullContexts)[PullNodes] = []any{watchJob(20, CheckSuccess)}
		return p
	})
	first, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil || !first[loopPR].Pull.Green {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	status, conclusion = watchQueued, ""
	queued, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, first, 0)
	if err != nil || queued[loopPR].Pull.Green || !queued[loopPR].Pending {
		t.Fatalf("queued=%+v err=%v", queued, err)
	}
	status, conclusion = CheckCompleted, CheckFailure
	failed, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, queued, 0)
	if err != nil || failed[loopPR].Pull.Green || len(failed[loopPR].Pull.Red) != 1 || *calls != 3 {
		t.Fatalf("failed=%+v calls=%d err=%v", failed, *calls, err)
	}
}

func TestPullWatcherPaginatesWorkflowSuites(t *testing.T) {
	calls := fakePullGraphQL(t, func(query string, call int) map[string]any {
		p := pullGraphQLFixture(t, query, CheckSuccess, loopHead, watchFixtureUpdated)
		PullConnection(p, PullContexts)[PullNodes] = []any{watchJob(10, CheckSuccess)}
		suites := PullConnection(p, PullSuites)
		if call == 1 {
			suites[PullNodes] = []any{watchSuite(10, 1, CheckCompleted, CheckSuccess)}
			suites[PullPageInfo] = map[string]any{PullHasNext: true, PullEndCursor: "more-suites"}
		} else {
			if !strings.Contains(query, `checkSuites(first:100,after:"more-suites")`) {
				t.Fatal("missing suite cursor")
			}
			suites[PullNodes] = []any{watchSuite(20, 1, CheckCompleted, CheckFailure)}
		}
		return p
	})
	seen, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil || *calls != 2 || seen[loopPR].Error != "" || seen[loopPR].Pull.Green || len(seen[loopPR].Pull.Red) != 1 {
		t.Fatalf("truncated suites: calls=%d seen=%+v err=%v", *calls, seen, err)
	}
}

func TestPullWatcherWorkflowRerunAttempt(t *testing.T) {
	fakePullGraphQL(t, func(query string, _ int) map[string]any {
		p := pullGraphQLFixture(t, query, CheckSuccess, loopHead, watchFixtureUpdated)
		latest := watchSuite(20, 1, CheckCompleted, CheckSuccess)
		latest[watchWorkflowRun].(map[string]any)[watchRunAttempt] = 2
		PullConnection(p, PullSuites)[PullNodes] = []any{latest, watchSuite(20, 1, CheckCompleted, CheckFailure)}
		PullConnection(p, PullContexts)[PullNodes] = []any{watchJob(20, CheckSuccess)}
		return p
	})
	seen, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
	if err != nil || seen[loopPR].Error != "" || !seen[loopPR].Pull.Green || len(seen[loopPR].Pull.Red) != 0 {
		t.Fatalf("rerun=%+v err=%v", seen, err)
	}
}

func TestPullWatcherIncompleteWorkflowSuitesStayUnread(t *testing.T) {
	for _, mode := range []string{"missing cursor", "repeated cursor", watchMovedHead} {
		t.Run(mode, func(t *testing.T) {
			fakePullGraphQL(t, func(query string, call int) map[string]any {
				p := pullGraphQLFixture(t, query, CheckSuccess, loopHead, watchFixtureUpdated)
				page := map[string]any{PullHasNext: true, PullEndCursor: watchSuiteCursor}
				if mode == "missing cursor" {
					delete(page, PullEndCursor)
				}
				if mode == watchMovedHead && call == 2 {
					p[PullHeadField] = loopFixed
				}
				PullConnection(p, PullSuites)[PullPageInfo] = page
				return p
			})
			seen, err := FetchLoopPullBatch(t.Context(), []string{loopPR}, nil, 0)
			if err != nil || seen[loopPR].Error == "" || seen[loopPR].Pull.Green {
				t.Fatalf("incomplete suites=%+v err=%v", seen, err)
			}
		})
	}
}
