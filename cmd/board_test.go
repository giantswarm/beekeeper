package cmd

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

func boardCandidates(n int) []board.Candidate {
	out := make([]board.Candidate, n)
	for i := range out {
		out[i] = board.Candidate{Item: board.Item{Ref: fmt.Sprintf("o/r#%d", i+1)}, Step: "Up Next"}
	}
	return out
}

// idleRef is the item an agent reporting idle still has a record of.
const idleRef = "o/r#2"

func TestNextFreeSkipsOwnedItems(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: "Worker one"}
	idleAgent := state.Party{Session: "s2", Name: "Idle agent"}
	gone := state.Party{Session: "s3", Name: "Gone"}
	late := state.Party{Session: "s4", Name: "Started late"}
	alive := func(p state.Party) bool { return p.Is(running) || p.Is(idleAgent) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: running, Issue: "O/R#1", At: listed.Add(-time.Hour)},
			{Session: idleAgent, Issue: idleRef, At: listed.Add(-time.Hour)},
			{Session: gone, Issue: "o/r#3", At: listed.Add(-time.Hour)},
			{Session: late, Issue: "o/r#4", At: listed.Add(time.Second)},
			{Session: running, Issue: "o/r#5", At: listed.Add(-time.Hour), Ended: listed},
			{Session: me, Issue: "o/r#6", At: listed.Add(-time.Hour)},
		},
		Agents: []state.Agent{
			{Party: idleAgent, LastTask: idleRef},
			{Party: state.Party{Session: "s5", Name: "Busy"}, Task: "https://github.com/o/r/issues/7 and o/r#8"},
			{Party: state.Party{Session: "s6", Name: "Finished"}, Task: "o/r#9", Done: true},
		},
		Notes: []state.Note{{ID: 12, For: "Pat", Text: "decide https://github.com/o/r/issues/10"}},
	}
	cands := boardCandidates(11)
	cands[10].Skip = "assigned to pat"
	res := nextFree(st, cands, pickScope{me: me, alive: alive, listed: listed})
	if res.Pick == nil || res.Pick.Ref != idleRef || len(res.Skipped) != 1 || res.Skipped[0].Skip != `served by "Worker one"` {
		t.Errorf("an idle agent's record is no claim: pick %v, skipped %+v", res.Pick, res.Skipped)
	}
	// The ended record's, the gone session's and the finished agent's items
	// are free; once a running session serves them nothing is.
	st.Records = append(st.Records, state.Record{Session: running, Issue: idleRef}, state.Record{Session: running, Issue: "o/r#3"},
		state.Record{Session: running, Issue: "o/r#5"}, state.Record{Session: running, Issue: "o/r#9"})
	if res = nextFree(st, cands, pickScope{me: me, alive: alive, listed: listed}); res.Pick != nil {
		t.Errorf("all owned, picked %v", res.Pick)
	}
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#1: served by "Worker one"`,
		idleRef + `: served by "Worker one"`,
		`o/r#3: served by "Worker one"`,
		`o/r#4: served by "Started late"`,
		`o/r#5: served by "Worker one"`,
		`o/r#6: served by you`,
		`o/r#7: served by "Busy" (task)`,
		`o/r#8: served by "Busy" (task)`,
		`o/r#9: served by "Worker one"`,
		`o/r#10: note #12 (waits on Pat)`,
		`o/r#11: assigned to pat`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestNextFreeHoldsAnEpicsSubIssuesToItsOwners(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: "Worker one"}
	alive := func(p state.Party) bool { return p.Is(running) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: running, Issue: "o/r#20", At: listed.Add(-time.Hour)},
			{Session: running, Issue: "o/r#21", At: listed.Add(-time.Hour)},
		},
		Notes: []state.Note{{ID: 704, For: "Pat", Pinned: true, Text: "skip the slices of https://github.com/o/r/issues/10"}},
	}
	sub := func(n int, epic string) board.Candidate {
		return board.Candidate{Item: board.Item{Ref: fmt.Sprintf("o/r#%d", n)}, Step: "In Progress", Epic: epic}
	}
	// A sub-issue's own record names it ahead of its epic's.
	cands := []board.Candidate{sub(11, "o/r#10"), sub(12, "O/R#10"), sub(21, "o/r#20"), sub(22, "o/r#20"), sub(31, "o/r#30")}
	res := nextFree(st, cands, pickScope{me: me, alive: alive, listed: listed})
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#11: note #704 (waits on Pat), on epic o/r#10`,
		`o/r#12: note #704 (waits on Pat), on epic O/R#10`,
		`o/r#21: served by "Worker one"`,
		`o/r#22: served by "Worker one", on epic o/r#20`,
	}
	if !slices.Equal(got, want) || res.Pick == nil || res.Pick.Ref != "o/r#31" {
		t.Errorf("pick %v, skipped:\n%s\nwant o/r#31 after:\n%s", res.Pick, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestConcurrentClaimsNeverGetTheSameItem(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	cands := boardCandidates(workers - 2)
	listed := time.Now()
	alive := func(state.Party) bool { return true }
	var wg sync.WaitGroup
	picks := make([]string, workers)
	for i := range workers {
		wg.Go(func() {
			me := state.Party{Session: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("Board pull %d", i)}
			res, err := claimNext(store, cands, pickScope{me: me, alive: alive, listed: listed}, "", func(string) bool { return false })
			if err != nil {
				t.Error(err)
			}
			if res.Pick != nil && res.Claimed {
				picks[i] = res.Pick.Ref
			}
		})
	}
	wg.Wait()
	got := slices.DeleteFunc(slices.Clone(picks), func(s string) bool { return s == "" })
	slices.Sort(got)
	if len(got) != len(cands) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("claims %v: want each of the %d items once", picks, len(cands))
	}
	st, _ := store.Read()
	if len(st.Records) != len(cands) {
		t.Errorf("records %+v", st.Records)
	}
}

// boardPull is a claiming worker's name.
const boardPull = "Board pull"

func TestSecondClaimKeepsAnOpenServe(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cands := boardCandidates(2)
	listed := time.Now()
	alive := func(state.Party) bool { return true }
	me := state.Party{Session: "s1", Name: boardPull}
	open := func(string) bool { return true }
	if res, err := claimNext(store, cands, pickScope{me: me, alive: alive, listed: listed}, "picking up", open); err != nil || !res.Claimed || res.Pick.Ref != refOne {
		t.Fatalf("first claim: %+v, %v", res, err)
	}
	before, _ := store.Read()

	// While o/r#1 is open, a second claim changes nothing and names it.
	res, err := claimNext(store, cands, pickScope{me: me, alive: alive, listed: listed}, "the review", open)
	st, _ := store.Read()
	if err != nil || res.Claimed || res.Pick != nil || res.Held == nil || res.Held.Issue != refOne || !slices.EqualFunc(st.Records, before.Records, recordsEqual) {
		t.Fatalf("second claim over an open serve: %+v, %v; records %+v", res, err, st.Records)
	}
	if err := (&app{out: io.Discard}).printNext(res, len(cands)); Code(err) != ExitRefused {
		t.Errorf("refused claim exits %v, want %d", err, ExitRefused)
	}

	// --replace (or a served item since closed) takes the next item and
	// replaces the record: the session serves one item.
	res, err = claimNext(store, cands, pickScope{me: me, alive: alive, listed: listed}, "the review", func(string) bool { return false })
	st, _ = store.Read()
	if err != nil || !res.Claimed || res.Held != nil || res.Pick.Ref != cands[1].Ref || len(st.Records) != 1 || st.Records[0].Issue != cands[1].Ref || st.Records[0].Waits != "the review" {
		t.Errorf("replacing claim: %+v, %v; records %+v", res, err, st.Records)
	}

	// An agent reporting done holds nothing: its next claim replaces.
	_ = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: me, Task: "board pull", Done: true})
		return nil, nil
	})
	if res, err := claimNext(store, cands, pickScope{me: me, alive: alive, listed: listed}, "", open); err != nil || !res.Claimed || res.Held != nil {
		t.Errorf("claim of a done agent: %+v, %v", res, err)
	}
}

func recordsEqual(a, b state.Record) bool {
	return a.Session.Is(b.Session) && a.Issue == b.Issue && a.Waits == b.Waits && a.At.Equal(b.At)
}

func TestServedOpenAsksGitHubPerRecord(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	me := state.Party{Session: "s1", Name: boardPull}
	other := state.Party{Session: "s2", Name: "Other"}
	_ = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = []state.Record{{Session: me, Issue: refOne}, {Session: other, Issue: "o/r#2"}}
		return nil, nil
	})
	var asked []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		// The variables come in map order: sort them.
		var vs []string
		for i := 5; i < len(args); i += 2 {
			vs = append(vs, args[i])
		}
		slices.Sort(vs)
		asked = append(asked, strings.Join(vs, " "))
		return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"CLOSED"}}}}`), nil
	}
	open, err := servedOpen(t.Context(), &board.Client{GH: gh}, store, me)
	if err != nil {
		t.Fatal(err)
	}
	if open("O/R#1") || !open("o/r#9") || len(asked) != 1 || asked[0] != "n=1 o=o r=r" {
		t.Errorf("open o/r#1 %v, o/r#9 %v; asked %q", open("o/r#1"), open("o/r#9"), asked)
	}
}

func TestClaimTakesNoSkippedItem(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cands := boardCandidates(1)
	cands[0].Skip = "created 2026-06-24: Backlog takes items created within 90 days"
	me := state.Party{Session: "s1", Name: boardPull}
	res, err := claimNext(store, cands, pickScope{me: me, alive: func(state.Party) bool { return true }, listed: time.Now()}, "", func(string) bool { return false })
	st, _ := store.Read()
	if err != nil || res.Pick != nil || res.Claimed || len(st.Records) != 0 || len(res.Skipped) != 1 {
		t.Errorf("claim over a skipped item: %+v, %v; records %+v", res, err, st.Records)
	}
}

func TestFindRecordTakesASessionOrTheIssueItServes(t *testing.T) {
	const own, shared, pull = "o/s#1", "o/s#2", "Pull two"
	records := []state.Record{
		{Session: state.Party{Session: "s1", Name: "Pull one"}, Issue: own},
		{Session: state.Party{Session: "s2", Name: pull}, Issue: shared},
		{Session: state.Party{Session: "s3", Name: "Review"}, Issue: shared},
	}
	for q, want := range map[string]int{"O/S#1": 0, pull: 1, "review": 2} {
		if i, err := findRecord(records, q); err != nil || i != want {
			t.Errorf("findRecord(%q) = %d, %v; want %d", q, i, err, want)
		}
	}
	for q, want := range map[string]string{"o/s#3": "no session record serves o/s#3", shared: shared + ` is served by "` + pull + `", "Review": name the session`} {
		if _, err := findRecord(records, q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("findRecord(%q) = %v, want %q", q, err, want)
		}
	}
}

func TestSkipHeldLeases(t *testing.T) {
	me := state.Party{Session: "me", Name: "Me"}
	cands := boardCandidates(4)
	cands[0].Labels = []string{"team/bumblebee", board.LeaseLabel + labOne}
	cands[1].Labels = []string{"Lease/Graveler"}
	cands[2].Labels = []string{"lease/agentlab-2"}
	cands[3].Labels = []string{"lease/glean"}
	cands[3].Skip = "assigned to pat"
	holders := []lease.Holder{
		{Env: labOne, Session: "s1", Name: "Lab holder"},
		{Env: graveler, Session: "me", Name: "Me"},
		{Env: "glean", Session: "s1", Name: "Lab holder"},
	}
	skipHeldLeases(cands, holders, me)
	var got []string
	for _, c := range cands {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#1: needs lease ` + labOne + `, held by "Lab holder"`,
		"o/r#2: ",
		"o/r#3: ",
		"o/r#4: assigned to pat",
	}
	if !slices.Equal(got, want) {
		t.Errorf("lease skips:\n got %q\nwant %q", got, want)
	}
	// The held item is passed over; the one behind it is picked.
	res := nextFree(&state.State{}, cands, pickScope{me: me, alive: func(state.Party) bool { return false }, listed: time.Now()})
	if res.Pick == nil || res.Pick.Ref != "o/r#2" || len(res.Skipped) != 1 {
		t.Errorf("pick %v, skipped %+v", res.Pick, res.Skipped)
	}
	// Every item behind the pick is listed: the free one next in line and
	// the skipped one with its reason.
	got = nil
	for _, c := range res.AfterPick {
		got = append(got, c.Ref+": "+c.Skip)
	}
	if want := []string{"o/r#3: ", "o/r#4: assigned to pat"}; !slices.Equal(got, want) {
		t.Errorf("after pick:\n got %q\nwant %q", got, want)
	}
}

func TestNextFreeSkipsTheIssuesAServedPRCloses(t *testing.T) {
	const servedPR, taskPR, taskIssue = "o/r#100", "o/r#102", "o/r#3"
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	worker := state.Party{Session: "s1", Name: "PR author"}
	alive := func(p state.Party) bool { return p.Is(worker) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: worker, Issue: servedPR, At: listed.Add(-time.Hour)},
			{Session: worker, Issue: "o/r#101", At: listed.Add(-time.Hour)},
		},
		Agents: []state.Agent{{Party: state.Party{Session: "s2", Name: "Busy"}, Task: "review " + taskPR}},
	}
	// o/r#100 closes o/r#1 and an issue in another repository, the busy
	// agent's o/r#102 closes o/r#3; o/r#101 closes nothing.
	closes := map[string][]string{servedPR: {refOne, "other/x#2"}, taskPR: {taskIssue}}
	cands := append(boardCandidates(4), board.Candidate{Item: board.Item{Ref: "Other/X#2"}, Step: "Up Next"})
	res := nextFree(st, cands, pickScope{me: me, alive: alive, listed: listed, closes: closes})
	var got []string
	for _, c := range append(res.Skipped, res.AfterPick...) {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		refOne + `: served by "PR author" through ` + servedPR,
		idleRef + ": ",
		taskIssue + `: served by "Busy" (task) through ` + taskPR,
		`o/r#4: `,
		`Other/X#2: served by "PR author" through ` + servedPR,
	}
	if res.Pick == nil || res.Pick.Ref != idleRef {
		t.Fatalf("pick %v, want %s", res.Pick, idleRef)
	}
	got = slices.Insert(got, 1, idleRef+": ")
	if !slices.Equal(got, want) {
		t.Errorf("closing-reference skips:\n got %q\nwant %q", got, want)
	}
	// Without the closing references nothing beyond the records is covered.
	if res = nextFree(st, cands, pickScope{me: me, alive: alive, listed: listed}); res.Pick == nil || res.Pick.Ref != refOne {
		t.Errorf("no closes: pick %v, want %s", res.Pick, refOne)
	}
}

func TestNamedRefsReadsTheFormsPeopleWrite(t *testing.T) {
	const owner = "gs"
	for _, tc := range []struct {
		text, owner string
		want        []string
	}{
		{"decide https://github.com/o/r/issues/10 and https://github.com/O/R/pull/11/files", owner, []string{"o/r#10", "o/r#11"}},
		{"https://github.com/o/r/issues/7 and o/r#8", owner, []string{"o/r#7", "o/r#8"}},
		{"model-manager#258 and llm-d#29", owner, []string{"gs/model-manager#258", "gs/llm-d#29"}},
		{"beekeeper#525, #555 and #563 wait", owner, []string{"gs/beekeeper#525", "gs/beekeeper#555", "gs/beekeeper#563"}},
		{"beekeeper: #524 (a), #525 (b), #555 (c)", owner, []string{"gs/beekeeper#524", "gs/beekeeper#525", "gs/beekeeper#555"}},
		{"o/r: #1, #2; then x/y#3, #4", owner, []string{"o/r#1", "o/r#2", "x/y#3", "x/y#4"}},
		{"https://github.com/o/r/issues/10, #11 too", owner, []string{"o/r#10", "o/r#11"}},
		// A bare #n before any repository names nothing.
		{"#5 then o/r#6 and #7", owner, []string{"o/r#6", "o/r#7"}},
		{"#5 alone", owner, nil},
		// beekeeper's own items are no issues, and set no context.
		{"o/r#1 waits on note #708, timer #707, memo #1047, decision #894; notes #3 and #4 too", owner, []string{"o/r#1", "o/r#4"}},
		{"note #708 then #2", owner, nil},
		{"o/r#1 and O/R#1 once", owner, []string{"o/r#1"}},
		// Without a board owner a repo#n names nothing.
		{"beekeeper#5 and o/r#6", "", []string{"o/r#6"}},
		{"nothing here, 13:38 either", owner, nil},
	} {
		if got := namedRefs(tc.text, tc.owner); !slices.Equal(got, tc.want) {
			t.Errorf("namedRefs(%q, %q) = %q, want %q", tc.text, tc.owner, got, tc.want)
		}
	}
}

func TestNextFreeSkipsWhatANoteNamesAsPeopleWrite(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	st := &state.State{
		Agents: []state.Agent{{Party: state.Party{Session: "s5", Name: "Busy"}, Task: "llm-d#29 to merged"}},
		Notes: []state.Note{
			{ID: 1, For: "Pat", Text: "model-manager#258 waits on the GPU", Refs: []string{"o/r#9"}},
			{ID: 2, Text: "dispatched beekeeper: #525 (a), #555 (b), #563 (c)"},
			{ID: 3, For: "Pat", Text: "#4 is nobody's: no repository before it"},
		},
	}
	item := func(ref string) board.Candidate {
		return board.Candidate{Item: board.Item{Ref: ref}, Step: "Up Next"}
	}
	cands := []board.Candidate{item("gs/model-manager#258"), item("gs/beekeeper#525"), item("gs/beekeeper#555"),
		item("gs/beekeeper#563"), item("gs/llm-d#29"), item("o/r#9"), item("gs/x#4"), item("o/r#4")}
	res := nextFree(st, cands, pickScope{me: me, alive: func(state.Party) bool { return true }, listed: listed, owner: "gs"})
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`gs/model-manager#258: note #1 (waits on Pat)`,
		`gs/beekeeper#525: note #2 (waits on the supervisor)`,
		`gs/beekeeper#555: note #2 (waits on the supervisor)`,
		`gs/beekeeper#563: note #2 (waits on the supervisor)`,
		`gs/llm-d#29: served by "Busy" (task)`,
		`o/r#9: note #1 (waits on Pat)`,
	}
	if !slices.Equal(got, want) || res.Pick == nil || res.Pick.Ref != "gs/x#4" {
		t.Errorf("pick %v, skipped:\n%s\nwant gs/x#4 after:\n%s", res.Pick, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := len(res.AfterPick); n != 1 || res.AfterPick[0].Skip != "" {
		t.Errorf("after the pick %+v, want o/r#4 free", res.AfterPick)
	}
}

func TestSkipKind(t *testing.T) {
	for reason, want := range map[string]string{
		`served by "Worker one" through o/r#100`:              "served",
		`note #704 (waits on the supervisor), on epic o/r#10`: "note",
		"assigned to pat":                                                 "assigned",
		"3 of 4 blockers open":                                            "blocked",
		"no activity since 2026-01-01":                                    "stale",
		`needs lease agentlab-1, held by "Lab holder"`:                    "lease",
		"created 2026-06-24: Backlog takes items created within 90 days":  "order",
		"no step of board.order offers Inbox":                             "order",
		"blocker cleared takes items with recorded blockers, it has none": "order",
	} {
		if got := skipKind(reason); got != want {
			t.Errorf("skipKind(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestPrintNextPreviewListsTheFreeItemsAndCountsTheSkips(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: "Worker one"}
	st := &state.State{
		Records: []state.Record{{Session: running, Issue: "o/r#3", At: listed.Add(-time.Hour)}},
		Notes:   []state.Note{{ID: 12, For: "Pat", Text: "o/r#4 and o/r#7"}},
	}
	cands := boardCandidates(7)
	cands[0].Skip = "created 2026-06-24: Backlog takes items created within 90 days"
	for i := range cands {
		cands[i].Title = fmt.Sprintf("Item %d", i+1)
	}
	res := nextFree(st, cands, pickScope{me: me, alive: func(state.Party) bool { return true }, listed: listed})
	if want := map[string]int{"note": 2, "order": 1, "served": 1}; !maps.Equal(res.SkippedBy, want) {
		t.Errorf("skipped by %v, want %v", res.SkippedBy, want)
	}
	var out strings.Builder
	if err := (&app{out: &out}).printNext(res, len(cands)); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"o/r#2 Item 2\n",
		"skipped above it:\n",
		"free behind it, next in line: 2 of 5\n",
		"  o/r#5  Item 5  Up Next\n",
		"  o/r#6  Item 6  Up Next\n",
		"skipped: 4 in all: note 2, order 1, served 1 (--json lists each with its reason)\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("preview lacks %q:\n%s", line, out.String())
		}
	}
	// A claim prints its pick and how many items are behind it, not the list.
	res.Claimed = true
	out.Reset()
	if err := (&app{out: &out}).printNext(res, len(cands)); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "behind it: 5 more, 2 of them free") || strings.Contains(s, "Item 5") {
		t.Errorf("claim output:\n%s", s)
	}
}
