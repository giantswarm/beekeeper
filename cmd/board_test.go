package cmd

import (
	"context"
	"fmt"
	"io"
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
	res := nextFree(st, cands, me, alive, listed, nil)
	if res.Pick == nil || res.Pick.Ref != idleRef || len(res.Skipped) != 1 || res.Skipped[0].Skip != `served by "Worker one"` {
		t.Errorf("an idle agent's record is no claim: pick %v, skipped %+v", res.Pick, res.Skipped)
	}
	// The ended record's, the gone session's and the finished agent's items
	// are free; once a running session serves them nothing is.
	st.Records = append(st.Records, state.Record{Session: running, Issue: idleRef}, state.Record{Session: running, Issue: "o/r#3"},
		state.Record{Session: running, Issue: "o/r#5"}, state.Record{Session: running, Issue: "o/r#9"})
	if res = nextFree(st, cands, me, alive, listed, nil); res.Pick != nil {
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
	res := nextFree(st, cands, me, alive, listed, nil)
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
			res, err := claimNext(store, cands, me, alive, listed, "", func(string) bool { return false }, nil)
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
	if res, err := claimNext(store, cands, me, alive, listed, "picking up", open, nil); err != nil || !res.Claimed || res.Pick.Ref != refOne {
		t.Fatalf("first claim: %+v, %v", res, err)
	}
	before, _ := store.Read()

	// While o/r#1 is open, a second claim changes nothing and names it.
	res, err := claimNext(store, cands, me, alive, listed, "the review", open, nil)
	st, _ := store.Read()
	if err != nil || res.Claimed || res.Pick != nil || res.Held == nil || res.Held.Issue != refOne || !slices.EqualFunc(st.Records, before.Records, recordsEqual) {
		t.Fatalf("second claim over an open serve: %+v, %v; records %+v", res, err, st.Records)
	}
	if err := (&app{out: io.Discard}).printNext(res, len(cands)); Code(err) != ExitRefused {
		t.Errorf("refused claim exits %v, want %d", err, ExitRefused)
	}

	// --replace (or a served item since closed) takes the next item and
	// replaces the record: the session serves one item.
	res, err = claimNext(store, cands, me, alive, listed, "the review", func(string) bool { return false }, nil)
	st, _ = store.Read()
	if err != nil || !res.Claimed || res.Held != nil || res.Pick.Ref != cands[1].Ref || len(st.Records) != 1 || st.Records[0].Issue != cands[1].Ref || st.Records[0].Waits != "the review" {
		t.Errorf("replacing claim: %+v, %v; records %+v", res, err, st.Records)
	}

	// An agent reporting done holds nothing: its next claim replaces.
	_ = store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: me, Task: "board pull", Done: true})
		return nil, nil
	})
	if res, err := claimNext(store, cands, me, alive, listed, "", open, nil); err != nil || !res.Claimed || res.Held != nil {
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
	res, err := claimNext(store, cands, me, func(state.Party) bool { return true }, time.Now(), "", func(string) bool { return false }, nil)
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
	res := nextFree(&state.State{}, cands, me, func(state.Party) bool { return false }, time.Now(), nil)
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
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	worker := state.Party{Session: "s1", Name: "Worker one"}
	alive := func(p state.Party) bool { return p.Is(worker) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: worker, Issue: "o/r#100", At: listed.Add(-time.Hour)},
			{Session: worker, Issue: "o/r#101", At: listed.Add(-time.Hour)},
		},
		Agents: []state.Agent{{Party: state.Party{Session: "s2", Name: "Busy"}, Task: "review o/r#102"}},
	}
	// o/r#100 closes o/r#1 and an issue in another repository, the busy
	// agent's o/r#102 closes o/r#3; o/r#101 closes nothing.
	closes := map[string][]string{"o/r#100": {"o/r#1", "other/x#2"}, "o/r#102": {"o/r#3"}}
	cands := append(boardCandidates(4), board.Candidate{Item: board.Item{Ref: "Other/X#2"}, Step: "Up Next"})
	res := nextFree(st, cands, me, alive, listed, closes)
	var got []string
	for _, c := range append(res.Skipped, res.AfterPick...) {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#1: served by "Worker one" through o/r#100`,
		`o/r#2: `,
		`o/r#3: served by "Busy" (task) through o/r#102`,
		`o/r#4: `,
		`Other/X#2: served by "Worker one" through o/r#100`,
	}
	if res.Pick == nil || res.Pick.Ref != "o/r#2" {
		t.Fatalf("pick %v, want o/r#2", res.Pick)
	}
	got = slices.Insert(got, 1, "o/r#2: ")
	if !slices.Equal(got, want) {
		t.Errorf("closing-reference skips:\n got %q\nwant %q", got, want)
	}
	// Without the closing references nothing beyond the records is covered.
	if res = nextFree(st, cands, me, alive, listed, nil); res.Pick == nil || res.Pick.Ref != "o/r#1" {
		t.Errorf("no closes: pick %v, want o/r#1", res.Pick)
	}
}
