package observe

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/observe/observetest"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// Fixtures: repositories, issues and pull requests, the checks on them, and the other team.
const (
	repoKlaus     = "giantswarm/klaus"
	repoRoadmap   = "giantswarm/roadmap"
	refKlaus55    = "giantswarm/klaus#55"
	refRoadmap2   = "giantswarm/roadmap#2"
	refRoadmap90  = "giantswarm/roadmap#90"
	refRoadmap100 = "giantswarm/roadmap#100"
	refRoadmap101 = "giantswarm/roadmap#101"
	refAB1        = "a/b#1"
	refAB2        = "a/b#2"

	checkGoBuild = "go-build"
	checkGoTest  = "go-test"
	success      = "success"

	atlas = "Atlas 🗺️"
)

// GitHub's issue and pull request states: GraphQL spells them in upper case, REST in lower case.
const (
	gqlOpen   = "OPEN"
	gqlClosed = "CLOSED"
	restOpen  = "open"
)

// Fields observations name, and the trigger of a webhook's reads.
const (
	fieldBoardStatus = "board.status"
	fieldCIChecks    = "ci.checks"
	triggerWebhook   = "webhook"
)

func boardItem(status string, mut ...func(*forge.BoardItem)) forge.BoardItem {
	bi := forge.BoardItem{NodeID: "PVTI_101", Status: status, Kind: "Story 📑", Team: "Bumblebee🐝", IsIssue: true,
		UpdatedAt: t0, Issue: forge.Issue{Ref: refRoadmap101, Title: "Retry", State: gqlOpen,
			Assignees: []string{"alice"}}}
	for _, m := range mut {
		m(&bi)
	}
	return bi
}

func TestDiffItem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		before *forge.BoardItem
		after  forge.BoardItem
		want   []Observation
	}{
		{
			name:  "first observation is one board row, never missed",
			after: boardItem(StatusUpNext),
			want: []Observation{{Kind: KindBoard, Fields: []string{fieldBoardStatus, "board.kind", "board.team",
				"issue.title", "issue.state", "assignees"}}},
		},
		{
			name:   "nothing changed",
			before: ptr(boardItem(StatusUpNext)),
			after:  boardItem(StatusUpNext, func(b *forge.BoardItem) { b.UpdatedAt = t0.Add(time.Hour) }),
		},
		{
			name:   "status change is a missed projects_v2_item",
			before: ptr(boardItem(StatusUpNext)),
			after:  boardItem(StatusInProgress),
			want: []Observation{{Kind: KindBoard, Fields: []string{fieldBoardStatus},
				Missed: []Missed{{Event: eventProjectsV2Item, Repo: repoRoadmap}}}},
		},
		{
			name:   "assignee and state are issues",
			before: ptr(boardItem(StatusUpNext)),
			after: boardItem(StatusUpNext, func(b *forge.BoardItem) {
				b.Issue.Assignees = []string{"carol"}
				b.Issue.State = "closed"
			}),
			want: []Observation{{Kind: KindIssue, Fields: []string{"issue.state", "assignees"},
				Missed: []Missed{{eventIssues, repoRoadmap}, {eventIssues, repoRoadmap}}}},
		},
		{
			name:   "blocker added is issue_dependencies; its state isn't missed",
			before: ptr(boardItem(StatusUpNext)),
			after: boardItem(StatusUpNext, func(b *forge.BoardItem) {
				b.Issue.BlockedBy = []forge.LinkedIssue{{Ref: refRoadmap100, State: gqlOpen}}
			}),
			want: []Observation{{Kind: KindDependencies, Fields: []string{"blockedBy"},
				Missed: []Missed{{eventIssueDependencies, repoRoadmap}}}},
		},
		{
			name: "blocker closed is observed, not missed",
			before: ptr(boardItem(StatusUpNext, func(b *forge.BoardItem) {
				b.Issue.BlockedBy = []forge.LinkedIssue{{Ref: refRoadmap100, State: gqlOpen}}
			})),
			after: boardItem(StatusUpNext, func(b *forge.BoardItem) {
				b.Issue.BlockedBy = []forge.LinkedIssue{{Ref: refRoadmap100, State: gqlClosed}}
			}),
			want: []Observation{{Kind: KindDependencies, Fields: []string{"blockedBy.state"}}},
		},
		{
			name:   "linked PR is a pull_request in its repo; closed PRs don't count",
			before: ptr(boardItem(StatusInProgress)),
			after: boardItem(StatusInProgress, func(b *forge.BoardItem) {
				b.Issue.ClosedBy = []forge.LinkedPR{{Ref: "alice/x#3", State: gqlOpen}, {Ref: "giantswarm/klaus#9", State: gqlClosed}}
			}),
			want: []Observation{{Kind: KindPR, Fields: []string{"changes"}, Missed: []Missed{{eventPullRequest, "alice/x"}}}},
		},
		{
			name:   "sub-issue added",
			before: ptr(boardItem(StatusUpNext)),
			after: boardItem(StatusUpNext, func(b *forge.BoardItem) {
				b.Issue.Parent = refRoadmap90
			}),
			want: []Observation{{Kind: KindSubIssues, Fields: []string{"parent"}, Missed: []Missed{{eventSubIssues, repoRoadmap}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var old *v1alpha1.ItemStatus
			if tt.before != nil {
				old = &v1alpha1.ItemStatus{}
				ApplyBoardItem(old, *tt.before)
			}
			cur := &v1alpha1.ItemStatus{}
			if old != nil {
				cur = old.DeepCopy()
			}
			ApplyBoardItem(cur, tt.after)
			got := DiffItem(old, cur)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DiffItem =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestApplyBoardItemKeepsDecidedFields(t *testing.T) {
	t.Parallel()
	st := &v1alpha1.ItemStatus{Phase: v1alpha1.PhaseReady, SubIssues: []v1alpha1.SubIssue{{Ref: refAB1, Phase: "Active"}}}
	ApplyBoardItem(st, boardItem(StatusUpNext, func(b *forge.BoardItem) {
		b.Issue.SubIssues = []forge.LinkedIssue{{Ref: refAB2, State: gqlOpen}, {Ref: refAB1, State: gqlClosed, StateReason: "NOT_PLANNED"}}
		p := 2
		b.Priority = &p
	}))
	if st.Phase != v1alpha1.PhaseReady {
		t.Errorf("phase = %q", st.Phase)
	}
	want := []v1alpha1.SubIssue{{Ref: refAB1, State: v1alpha1.IssueClosedNotPlanned, Phase: "Active"}, {Ref: refAB2, State: v1alpha1.IssueOpen}}
	if !reflect.DeepEqual(st.SubIssues, want) {
		t.Errorf("subIssues = %+v", st.SubIssues)
	}
	if !reflect.DeepEqual(st.SubItems, []string{refAB1, refAB2}) {
		t.Errorf("subItems = %v", st.SubItems)
	}
	if st.Issue.Repo != repoRoadmap || st.Issue.Number != 101 || st.Issue.State != v1alpha1.IssueOpen || *st.Board.Priority != 2 {
		t.Errorf("issue/board = %+v %+v", st.Issue, st.Board)
	}
}

func prStatus(head string, checks ...v1alpha1.CheckFact) *v1alpha1.ChangeStatus {
	return &v1alpha1.ChangeStatus{
		PR:    &v1alpha1.ChangePR{Repo: repoKlaus, Number: 55, State: v1alpha1.PROpen, HeadSHA: head},
		Facts: &v1alpha1.ChangeFacts{HeadSHA: head, Head: checks},
	}
}

func check(name, status, conclusion string) v1alpha1.CheckFact {
	return v1alpha1.CheckFact{Kind: FactCheck, Name: name, Status: status, Conclusion: conclusion}
}

func TestDiffChange(t *testing.T) {
	t.Parallel()
	inProgress := check(checkGoTest, "in_progress", "")
	green := check(checkGoTest, "completed", success)
	tests := []struct {
		name     string
		old, cur *v1alpha1.ChangeStatus
		want     []Observation
	}{
		{
			name: "first observation",
			old:  &v1alpha1.ChangeStatus{PR: &v1alpha1.ChangePR{Repo: repoKlaus, Number: 55}},
			cur:  prStatus("a1", inProgress),
			want: []Observation{{Kind: KindPR, Fields: []string{"pr.state", "pr.headSha", fieldCIChecks}}},
		},
		{
			name: "check completes on the same head",
			old:  prStatus("a1", inProgress),
			cur:  prStatus("a1", green),
			want: []Observation{{Kind: KindCI, Fields: []string{fieldCIChecks}, Missed: []Missed{{eventCheckRun, repoKlaus}}}},
		},
		{
			name: "push: the push is missed, not the new head's checks",
			old:  prStatus("a1", green),
			cur:  prStatus("b2"),
			want: []Observation{
				{Kind: KindPR, Fields: []string{"pr.headSha"}, Missed: []Missed{{eventPullRequest, repoKlaus}}},
				{Kind: KindCI, Fields: []string{fieldCIChecks}},
			},
		},
		{
			name: "unchanged",
			old:  prStatus("a1", green),
			cur:  prStatus("a1", green),
		},
		{
			name: "review submitted",
			old:  prStatus("a1", green),
			cur: func() *v1alpha1.ChangeStatus {
				s := prStatus("a1", green)
				s.Facts.Reviews = []v1alpha1.ReviewFact{{By: "bob", State: "approved"}}
				return s
			}(),
			want: []Observation{{Kind: KindReview, Fields: []string{"review"}, Missed: []Missed{{eventPullRequestReview, repoKlaus}}}},
		},
		{
			name: "merged",
			old:  prStatus("a1", green),
			cur: func() *v1alpha1.ChangeStatus {
				s := prStatus("a1", green)
				s.PR.State, s.PR.MergeSHA, s.Facts.MergeSHA = v1alpha1.PRMerged, "m1", "m1"
				return s
			}(),
			want: []Observation{{Kind: KindPR, Fields: []string{"pr.state", "pr.mergeSha"}, Missed: []Missed{{eventPullRequest, repoKlaus}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DiffChange(tt.old, tt.cur); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DiffChange =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestRecord(t *testing.T) {
	t.Parallel()
	obs := []Observation{{Kind: KindCI, Fields: []string{fieldCIChecks}, Missed: []Missed{{eventCheckRun, repoKlaus}, {eventStatus, "alice/x"}}}}
	installed := func(repo string) bool { return repo == repoKlaus }
	subj := Subject{Team: "bumblebee", Change: refKlaus55}

	rows := Record(obs, subj, v1alpha1.SourceResync, nil, t0, installed)
	if len(rows.Events) != 2 || rows.Events[1].Type != EventMissed || !reflect.DeepEqual(rows.Missed, map[string]int{eventCheckRun: 1}) {
		t.Errorf("resync rows = %+v", rows)
	}
	for _, events := range [][]string{{eventCheckRun}, {eventCheckSuite}} {
		rows = Record(obs, subj, v1alpha1.SourceWebhook, events, t0, installed)
		if len(rows.Events) != 1 || rows.Missed != nil {
			t.Errorf("%v webhook rows = %+v", events, rows)
		}
	}
	// A webhook vouches only for its own event: a status delivery doesn't announce a check run.
	rows = Record(obs, subj, v1alpha1.SourceWebhook, []string{eventStatus}, t0, installed)
	if len(rows.Events) != 2 || !reflect.DeepEqual(rows.Missed, map[string]int{eventCheckRun: 1}) {
		t.Errorf("status webhook rows = %+v", rows)
	}
	board := []Observation{{Kind: KindBoard, Fields: []string{fieldBoardStatus}, Missed: []Missed{{eventProjectsV2Item, repoRoadmap}}}}
	rows = Record(board, Subject{Item: refRoadmap101}, v1alpha1.SourceWebhook, []string{eventPullRequest}, t0, nil)
	if !reflect.DeepEqual(rows.Missed, map[string]int{eventProjectsV2Item: 1}) {
		t.Errorf("pull_request webhook masked a board change: %+v", rows)
	}
	rows = Record(board, Subject{Item: refRoadmap101}, v1alpha1.SourceWebhook, nil, t0, nil)
	if rows.Missed == nil {
		t.Errorf("a webhook source naming no event vouched for a board change: %+v", rows)
	}
}

func TestReadChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := observetest.New()
	f.PRs[refKlaus55] = forge.PullRequest{Ref: refKlaus55, State: restOpen, HeadSHA: "a1", Base: "main", Author: "alice"}
	f.Checks["giantswarm/klaus@a1"] = []forge.CheckRun{{Name: checkGoTest, Status: "in_progress"}}
	f.StatusesBy["giantswarm/klaus@a1"] = []forge.CommitStatus{{Context: checkGoBuild, State: success}}
	f.Rules[repoKlaus] = forge.BranchRules{RequiredChecks: []string{checkGoTest, checkGoBuild}, RequiredApprovals: 1}
	rules := &RulesCache{Reader: f}

	st := &v1alpha1.ChangeStatus{PR: &v1alpha1.ChangePR{Repo: repoKlaus, Number: 55}}
	res := ReadChange(ctx, f, rules, refKlaus55, st, ReadSet{Head: true})
	if res.Err != nil || !res.Made {
		t.Fatalf("first read: %+v", res)
	}
	if st.PR.State != v1alpha1.PROpen || st.PR.HeadSHA != "a1" || len(st.Facts.Head) != 2 || st.Facts.RequiredApprovals != 1 {
		t.Fatalf("first read status: %+v %+v", st.PR, st.Facts)
	}
	if n := len(f.Calls); n != 5 { // PR, reviews, checks, statuses (all on a first read), rules
		t.Fatalf("calls = %d: %+v", n, f.Calls)
	}

	// Unchanged: every read is a 304 and the rules come from memory.
	f.Calls = nil
	ReadChange(ctx, f, rules, refKlaus55, st, FullReads())
	for _, c := range f.Calls {
		if !c.Conditional || !c.NotModified {
			t.Errorf("call not a 304: %+v", c)
		}
	}
	if len(f.Calls) != 3 { // the PR's 304 says its reviews are unchanged too
		t.Errorf("calls = %d", len(f.Calls))
	}

	// The check completes; a ci read of the head sees it, the statuses stay.
	f.Checks["giantswarm/klaus@a1"] = []forge.CheckRun{{Name: checkGoTest, Status: "completed", Conclusion: success}}
	ReadChange(ctx, f, rules, refKlaus55, st, ReadSet{SHAs: []string{"a1"}})
	want := []v1alpha1.CheckFact{check(checkGoTest, "completed", success),
		{Kind: FactStatus, Name: checkGoBuild, Status: "completed", Conclusion: success}}
	if !reflect.DeepEqual(st.Facts.Head, want) {
		t.Errorf("head facts = %+v", st.Facts.Head)
	}

	// A 502 on the check runs stops the reads after it and keeps the PR read before it.
	f.PRs[refKlaus55] = forge.PullRequest{Ref: refKlaus55, State: restOpen, HeadSHA: "b2", Base: "main"}
	f.Fail(forge.OpChecksListForRef, &forge.Error{Kind: forge.Transient, API: forge.APIREST, Code: 502})
	f.Calls = nil
	res = ReadChange(ctx, f, rules, refKlaus55, st, ReadSet{PR: true})
	if res.Err == nil || res.Err.Kind != forge.Transient || res.Err.Operation != forge.OpChecksListForRef || !res.Made {
		t.Errorf("failed read = %+v", res)
	}
	if st.PR.HeadSHA != "b2" || st.Facts.Head != nil {
		t.Errorf("after the push: head %q facts %+v", st.PR.HeadSHA, st.Facts.Head)
	}
	if len(f.CallsOf(forge.OpStatusesListForRef)) != 0 {
		t.Error("statuses read after a failed check-runs read")
	}
	for k := range st.ETags {
		if contains([]string{"/repos/giantswarm/klaus/commits/a1/check-runs"}, k) {
			t.Errorf("stale etag kept: %s", k)
		}
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	var got []time.Duration
	for n := range 8 {
		got = append(got, Backoff(10*time.Second, 5*time.Minute, n))
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Backoff = %v", got)
	}
}

func failed(id string) forge.Delivery    { return forge.Delivery{ID: id, StatusCode: 503} }
func delivered(id string) forge.Delivery { return forge.Delivery{ID: id, StatusCode: 202} }

func TestLogCheck(t *testing.T) {
	t.Parallel()
	// source-blind-webhooks-degrade-and-poll: fails at 09:05, nothing at 09:10, a delivery at 09:10,
	// fresh at 09:15.
	const interval = 5 * time.Minute
	s := NewSource(v1alpha1.SourceWebhooks, nil, t0)
	steps := []struct {
		at         time.Duration
		deliveries []forge.Delivery
		failed     int
		tr         Transition
		state      string
	}{
		{5 * time.Minute, []forge.Delivery{failed("1")}, 1, Degraded, v1alpha1.SourceDegraded},
		{10 * time.Minute, nil, 0, Unchanged, v1alpha1.SourceDegraded},
		{15 * time.Minute, []forge.Delivery{delivered("2")}, 0, Fresh, v1alpha1.SourceFresh},
		{20 * time.Minute, nil, 0, Unchanged, v1alpha1.SourceFresh},
		// A failure followed by a success in one batch: degraded, and the success counts.
		{25 * time.Minute, []forge.Delivery{failed("3"), delivered("4")}, 1, Degraded, v1alpha1.SourceDegraded},
		{30 * time.Minute, nil, 0, Fresh, v1alpha1.SourceFresh},
	}
	for i, st := range steps {
		f, tr := s.LogCheck(t0.Add(st.at), interval, st.deliveries)
		if len(f) != st.failed || tr != st.tr || s.State != st.state {
			t.Fatalf("step %d: failed %d tr %v state %s", i, len(f), tr, s.State)
		}
		if s.Verdict != t0.Add(st.at) {
			t.Fatalf("step %d: verdict not renewed", i)
		}
	}
	if s.Polling {
		t.Error("still polling when fresh")
	}
}

func TestSourceGapKeepsFirstDegradation(t *testing.T) {
	t.Parallel()
	s := NewSource(v1alpha1.SourceWebhooks, nil, t0)
	if tr := s.Gap(t0, v1alpha1.SourceReasonLogUnreadable); tr != Degraded || !s.Polling {
		t.Fatalf("first gap: %v", tr)
	}
	if tr := s.Gap(t0.Add(5*time.Minute), v1alpha1.SourceReasonLogUnreadable); tr != Unchanged {
		t.Fatalf("second gap: %v", tr)
	}
	if !s.Since.Equal(t0) {
		t.Errorf("since moved: %v", s.Since)
	}
	if tr := s.Resolve(t0.Add(6 * time.Minute)); tr != Fresh {
		t.Errorf("resolve: %v", tr)
	}
	restored := NewSource(v1alpha1.SourceWebhooks, &v1alpha1.SourceStatus{State: v1alpha1.SourceDegraded, Reason: "probe-gap", Polling: true}, t0)
	if !restored.Degraded() || !restored.Polling {
		t.Errorf("restored = %+v", restored)
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	// source-degraded-board-polls-then-recovers: silent changes at 09:01 and 09:11.
	var p Probe
	item := func(node string, at time.Duration) forge.ProbeItem {
		return forge.ProbeItem{IsIssue: true, NodeID: node, UpdatedAt: t0.Add(at)}
	}
	if g := p.Judge([]forge.ProbeItem{item("a", -time.Hour), item("b", -2*time.Hour)}); g != nil {
		t.Fatalf("first probe found gaps: %v", g)
	}
	if g := p.Judge([]forge.ProbeItem{item("a", time.Minute), item("b", -2*time.Hour)}); len(g) != 1 || g[0].NodeID != "a" {
		t.Fatalf("09:10 probe: %v", g)
	}
	if g := p.Judge([]forge.ProbeItem{item("b", 11*time.Minute), item("a", time.Minute)}); len(g) != 1 || g[0].NodeID != "b" {
		t.Fatalf("09:20 probe: %v", g)
	}
	if g := p.Judge([]forge.ProbeItem{item("b", 11*time.Minute), item("a", time.Minute)}); g != nil {
		t.Fatalf("09:30 probe: %v", g)
	}
	// A change a webhook reported is no gap.
	p.Reported("a", t0.Add(35*time.Minute))
	if g := p.Judge([]forge.ProbeItem{item("a", 35*time.Minute)}); g != nil {
		t.Fatalf("reported change judged a gap: %v", g)
	}
	if len(p.known) != 0 || !p.Since().Equal(t0.Add(35*time.Minute)) {
		t.Errorf("after the mark passed: known %v, since %s", p.known, p.Since())
	}
	// Draft issues and PRs on the board are never reported, so never a gap.
	draft := forge.ProbeItem{NodeID: "d", UpdatedAt: t0.Add(40 * time.Minute)}
	if g := p.Judge([]forge.ProbeItem{draft}); g != nil {
		t.Fatalf("a draft item judged a gap: %v", g)
	}
}

func TestScope(t *testing.T) {
	t.Parallel()
	const team = "Bumblebee🐝"
	tests := []struct {
		name   string
		bi     forge.BoardItem
		tracks bool
	}{
		{"active", boardItem(StatusUpNext), true},
		{"backlog", boardItem(StatusBacklog), false},
		{"inbox story", boardItem(StatusInbox), false},
		{"inbox bug", boardItem(StatusInbox, func(b *forge.BoardItem) { b.Kind = KindBug }), true},
		{"other team", boardItem(StatusUpNext, func(b *forge.BoardItem) { b.Team = atlas }), false},
		{"closed", boardItem(StatusInProgress, func(b *forge.BoardItem) { b.Issue.State = gqlClosed }), false},
		{"archived", boardItem(StatusUpNext, func(b *forge.BoardItem) { b.Archived = true }), false},
		{"draft", boardItem(StatusUpNext, func(b *forge.BoardItem) { b.IsIssue = false }), false},
	}
	for _, tt := range tests {
		if got := Tracks(tt.bi, team); got != tt.tracks {
			t.Errorf("%s: Tracks = %v", tt.name, got)
		}
	}
	if r := LeftScope(forge.BoardItem{}, false, team); r != RemovedDeleted {
		t.Errorf("deleted: %q", r)
	}
	if r := LeftScope(boardItem(StatusUpNext, func(b *forge.BoardItem) { b.Archived = true }), true, team); r != RemovedArchived {
		t.Errorf("archived: %q", r)
	}
	if r := LeftScope(boardItem(StatusUpNext, func(b *forge.BoardItem) { b.Team = atlas }), true, team); r != RemovedTeamChanged {
		t.Errorf("team: %q", r)
	}
	if r := LeftScope(boardItem(StatusBacklog), true, team); r != "" {
		t.Errorf("status: %q", r)
	}
	schema := forge.BoardSchema{Options: map[string][]string{"Team": {atlas, team}}}
	if u := UnknownOptions(schema, []string{"Bumblebee 🐝", team}); !reflect.DeepEqual(u, []string{"Bumblebee 🐝"}) {
		t.Errorf("unknown = %v", u)
	}
}

func TestInputs(t *testing.T) {
	t.Parallel()
	in := NewInputs("giantswarm")
	if !in.Installed("giantswarm/happa") || in.Installed("alice/x") {
		t.Fatal("org fallback")
	}
	k := in.Key("giantswarm/happa", "giantswarm/happa#7")
	in.SetRepos([]string{repoKlaus})
	if in.Installed("giantswarm/happa") {
		t.Error("happa installed")
	}
	k2 := in.Key("giantswarm/happa", "giantswarm/happa#7")
	if k == k2 {
		t.Error("repos change kept the key")
	}
	in.Webhook("giantswarm/happa#7")
	if in.Key("giantswarm/happa", "giantswarm/happa#7") != k2 {
		t.Error("a webhook no refusal watches changed the key")
	}
	in.Watch("giantswarm/happa", "giantswarm/happa#7")
	in.Webhook(repoKlaus)
	if in.Key("giantswarm/happa", "giantswarm/happa#7") != k2 {
		t.Error("another repo's webhook changed the key")
	}
	in.Webhook("giantswarm/happa#7")
	if in.Key("giantswarm/happa", "giantswarm/happa#7") == k2 {
		t.Error("the PR's webhook kept the key")
	}
	in.Retain(func(k string) bool { return k == "giantswarm/happa" })
	if len(in.webhooks) != 1 {
		t.Errorf("retained %v", in.webhooks)
	}
}

func TestPendingMerge(t *testing.T) {
	t.Parallel()
	var s PendingSet
	s.Add("Change/x", Pending{Trigger: "resync", Source: v1alpha1.SourceResync, Reads: FullReads()})
	s.Add("Change/x", Pending{Trigger: triggerWebhook, Source: v1alpha1.SourceWebhook, Reads: ReadSet{SHAs: []string{"a1"}}})
	s.Add("Change/x", Pending{Trigger: "poll", Source: v1alpha1.SourceReobserve})
	p := s.Take("Change/x")
	if p.Source != v1alpha1.SourceWebhook || p.Trigger != triggerWebhook || !p.Reads.PR || !reflect.DeepEqual(p.Reads.SHAs, []string{"a1"}) {
		t.Errorf("merged = %+v", p)
	}
	if s.Take("Change/x") != nil {
		t.Error("taken twice")
	}

	// A wake that asks for no reads doesn't take the attribution of reads another source asked for, and
	// the events and deliveries of both are kept.
	s.Add("Item/y", Pending{Trigger: "resync", Source: v1alpha1.SourceResync, ReadBoard: true})
	s.Add("Item/y", Pending{Trigger: "engine", Source: v1alpha1.SourceWebhook})
	s.Add("Item/y", Pending{Trigger: triggerWebhook, Source: v1alpha1.SourceReobserve, Events: []string{eventIssues}, Deliveries: []string{"d1"}})
	if !s.HasDelivery("Item/y", "d1") || s.HasDelivery("Item/y", "d2") {
		t.Error("HasDelivery")
	}
	p = s.Drop("Item/y")
	if p.Source != v1alpha1.SourceResync || !reflect.DeepEqual(p.Events, []string{eventIssues}) || !reflect.DeepEqual(p.Deliveries, []string{"d1"}) {
		t.Errorf("merged = %+v", p)
	}
}

func TestApplyIncompleteBoardItemKeepsLinks(t *testing.T) {
	t.Parallel()
	st := &v1alpha1.ItemStatus{Parent: refRoadmap90,
		SubIssues: []v1alpha1.SubIssue{{Ref: refRoadmap2, State: v1alpha1.IssueOpen}}, SubItems: []string{refRoadmap2},
		BlockedBy: []v1alpha1.Blocker{{Ref: refRoadmap100, State: v1alpha1.IssueOpen}},
		Changes:   []string{refKlaus55}}
	bi := forge.BoardItem{NodeID: "PVTI_1", Status: StatusUpNext, IsIssue: true, Incomplete: true,
		Issue: forge.Issue{Ref: refRoadmap101, State: restOpen, Title: "new title",
			SubIssues: []forge.LinkedIssue{{Ref: "giantswarm/roadmap#3", State: restOpen}}}}
	ApplyBoardItem(st, bi)
	if st.Issue.Title != "new title" || st.Parent != refRoadmap90 || len(st.SubIssues) != 2 ||
		!reflect.DeepEqual(st.SubItems, []string{refRoadmap2, "giantswarm/roadmap#3"}) || len(st.BlockedBy) != 1 ||
		!reflect.DeepEqual(st.Changes, []string{refKlaus55}) {
		t.Errorf("incomplete read = %+v", st)
	}
	bi.Incomplete = false
	ApplyBoardItem(st, bi)
	if st.Parent != "" || len(st.SubIssues) != 1 || st.BlockedBy != nil || st.Changes != nil {
		t.Errorf("complete read = %+v", st)
	}
}
