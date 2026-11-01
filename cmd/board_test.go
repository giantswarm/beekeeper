package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

func boardCandidates(n int) []board.Candidate {
	out := make([]board.Candidate, n)
	for i := range out {
		out[i] = board.Candidate{Item: board.Item{Ref: fmt.Sprintf("o/r#%d", i+1)}, Step: stepUpNext}
	}
	return out
}

// idleRef is the item an agent reporting idle still has a record of.
const idleRef = "o/r#2"

// The names, steps and reasons the tests share.
const (
	stepUpNext     = "Up Next"
	stepInProgress = "In Progress"
	workerOne      = "Worker one"
	busyAgent      = "Busy"
	oldBacklog     = "created 2026-06-24: Backlog takes items created within 90 days"
)

func TestNextFreeSkipsOwnedItems(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: workerOne}
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
			{Party: state.Party{Session: "s5", Name: busyAgent}, Task: "https://github.com/o/r/issues/7 and o/r#8"},
			{Party: state.Party{Session: "s6", Name: "Finished"}, Task: "o/r#9", Done: true},
		},
		Notes: []state.Note{{ID: 12, Kind: noteDecision, For: pat, Text: "decide https://github.com/o/r/issues/10"}},
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
		state.Record{Session: running, Issue: "o/r#5"}, state.Record{Session: running, Issue: "o/r#9"}, state.Record{Session: running, Issue: epicTen})
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
		epicTen + `: served by "Worker one"`,
		`o/r#11: assigned to pat`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A decision that holds several items (note add --holds with more than one
// --ref) has board next skip each of them, naming the decision, while the
// Status its hold set stands; one whose Status was set since is offered.
func TestNextFreeSkipsEveryItemADecisionHolds(t *testing.T) {
	const waiting = "Blocked / Waiting"
	cands := boardCandidates(4)
	hold := func(i int) state.ItemHold { return state.ItemHold{Ref: cands[i].Ref, Status: waiting, Was: stepUpNext} }
	// The second named in another case, the third set Up Next since.
	also := []state.ItemHold{hold(1), hold(2)}
	also[0].Ref = strings.ToUpper(also[0].Ref)
	st := &state.State{Notes: []state.Note{{ID: 1917, For: claimSupervisor, Kind: noteDecision, Refs: []string{cands[0].Ref, cands[1].Ref, cands[2].Ref},
		Held: new(hold(0)), AlsoHeld: also}}}
	cands[0].Status, cands[1].Status, cands[2].Status = waiting, waiting, stepUpNext
	res := nextFree(st, cands, pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return false }})
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	held := ": held: decision #1917 for " + claimSupervisor + " until it closes"
	want := []string{cands[0].Ref + held, cands[1].Ref + held}
	if !slices.Equal(got, want) || res.Pick == nil || res.Pick.Ref != cands[2].Ref || res.SkippedBy[kindBlocked] != 2 {
		t.Errorf("pick %v, skipped by %v:\n%s\nwant %s after:\n%s", res.Pick, res.SkippedBy, strings.Join(got, "\n"), cands[2].Ref, strings.Join(want, "\n"))
	}
}

// epicTen is the item the board tests hold by a record.
const epicTen = "o/r#10"

func TestNextFreeHoldsAnEpicsSubIssuesToItsOwners(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: workerOne}
	alive := func(p state.Party) bool { return p.Is(running) || p.Is(me) }
	st := &state.State{
		Records: []state.Record{
			{Session: running, Issue: "o/r#20", At: listed.Add(-time.Hour)},
			{Session: running, Issue: "o/r#21", At: listed.Add(-time.Hour)},
		},
		Agents: []state.Agent{{Party: state.Party{Session: "s5", Name: busyAgent}, Task: "the slices of https://github.com/o/r/issues/10"}},
	}
	sub := func(n int, epic string) board.Candidate {
		return board.Candidate{Item: board.Item{Ref: fmt.Sprintf("o/r#%d", n)}, Step: stepInProgress, Epic: epic}
	}
	// A sub-issue's own record names it ahead of its epic's.
	cands := []board.Candidate{sub(11, "o/r#10"), sub(12, "O/R#10"), sub(21, "o/r#20"), sub(22, "o/r#20"), sub(31, "o/r#30")}
	res := nextFree(st, cands, pickScope{me: me, alive: alive, listed: listed})
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`o/r#11: served by "Busy" (task), on epic o/r#10`,
		`o/r#12: served by "Busy" (task), on epic O/R#10`,
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
	if err := (&app{out: io.Discard}).printNext(res, len(cands), nextView{}); Code(err) != ExitRefused {
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
		st.Records = []state.Record{{Session: me, Issue: refOne}, {Session: other, Issue: idleRef}}
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
		t.Errorf("open o/r#1 %v, o/r#9 %v; asked %q", open(refOne), open("o/r#9"), asked)
	}
}

func TestClaimTakesNoSkippedItem(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cands := boardCandidates(1)
	cands[0].Skip = oldBacklog
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
		Agents: []state.Agent{{Party: state.Party{Session: "s2", Name: busyAgent}, Task: "review " + taskPR}},
	}
	// o/r#100 closes o/r#1 and an issue in another repository, the busy
	// agent's o/r#102 closes o/r#3; o/r#101 closes nothing.
	closes := map[string][]string{servedPR: {refOne, "other/x#2"}, taskPR: {taskIssue}}
	cands := append(boardCandidates(4), board.Candidate{Item: board.Item{Ref: "Other/X#2"}, Step: stepUpNext})
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
		{"beekeeper: #534 (a), #535 (b), #565 (c)", owner, []string{"gs/beekeeper#534", "gs/beekeeper#535", "gs/beekeeper#565"}},
		{"o/r: #1, #2; then x/y#3, #4", owner, []string{refOne, idleRef, "x/y#3", "x/y#4"}},
		{"https://github.com/o/r/issues/10, #11 too", owner, []string{"o/r#10", "o/r#11"}},
		// A bare #n before any repository names nothing.
		{"#5 then o/r#16 and #17", owner, []string{"o/r#16", "o/r#17"}},
		{"#5 alone", owner, nil},
		// beekeeper's own items are no issues, and set no context.
		{"o/r#1 waits on note #708, timer #707, memo #1047, decision #894; notes #3 and #4 too", owner, []string{refOne}},
		{"note #708 then #2", owner, nil},
		// A bare #n after prose is no item of the list before it: a memo
		// across repositories names each by its own repository.
		{"Morning, #37850 chip: (1) glean portal steps (gpm#282 encoding fix live as 0.44.1); then Timo ticks #37850 criterion 3. gpm#283 proven; backstage#2597 filed", owner, []string{"gs/gpm#282", "gs/gpm#283", "gs/backstage#2597"}},
		{"o/r#1 is merged, now tick #2", owner, []string{refOne}},
		{"o/r#1 / #2 or #6; #62 & #63", owner, []string{refOne, idleRef, "o/r#6", "o/r#62", "o/r#63"}},
		{"o/r#1 and O/R#1 once", owner, []string{refOne}},
		// Without a board owner a repo#n names nothing.
		{"beekeeper#5 and a/b#6", "", []string{"a/b#6"}},
		{"nothing here, 13:38 either", owner, nil},
	} {
		if got := namedRefs(tc.text, tc.owner); !slices.Equal(got, tc.want) {
			t.Errorf("namedRefs(%q, %q) = %q, want %q", tc.text, tc.owner, got, tc.want)
		}
	}
}

func TestNextFreeIsHeldByNoNote(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	st := &state.State{
		Agents: []state.Agent{{Party: state.Party{Session: "s5", Name: busyAgent}, Task: "llm-d#29 to merged"}},
		Notes: []state.Note{
			{ID: 1, For: pat, Kind: noteDecision, Text: "merge model-manager#258 once the GPU is free?", Refs: []string{"x/y#9"}},
			{ID: 2, For: pat, Kind: noteLogin, Text: "sign in for beekeeper: #525"},
		},
	}
	item := func(ref string) board.Candidate {
		return board.Candidate{Item: board.Item{Ref: ref}, Step: stepUpNext}
	}
	cands := []board.Candidate{item("gs/llm-d#29"), item("gs/model-manager#258"), item("x/y#9"), item("gs/beekeeper#525")}
	res := nextFree(st, cands, pickScope{me: me, alive: func(state.Party) bool { return true }, listed: listed, owner: "gs"})
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{`gs/llm-d#29: served by "Busy" (task)`}
	if !slices.Equal(got, want) || res.Pick == nil || res.Pick.Ref != "gs/model-manager#258" {
		t.Errorf("pick %v, skipped:\n%s\nwant gs/model-manager#258 after:\n%s", res.Pick, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range res.AfterPick {
		if c.Skip != "" {
			t.Errorf("%s skipped behind the pick: %s; a note holds nothing", c.Ref, c.Skip)
		}
	}
}

func TestBlockerClearedOffersNoItemItsStatusHolds(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const blocked = "Blocked / Waiting ⛔️"
	closed := listed.Add(-48 * time.Hour)
	item := func(ref string, statusSet time.Time) board.Item {
		return board.Item{Ref: ref, OnBoard: true, Status: blocked, Created: listed.Add(-240 * time.Hour), Updated: listed.Add(-time.Hour),
			Blockers: 1, StatusSet: statusSet, BlockersClosed: closed}
	}
	// #1 was set Blocked for a person after its blocker closed; #2 was
	// Blocked on its dependency alone, which closed since.
	snap := &board.Snapshot{
		Order: []config.BoardStep{{Name: "blocker cleared", Status: []string{blocked}, Unblocked: true}},
		Items: []board.Item{item("o/r#1", closed.Add(time.Hour)), item("o/r#2", closed.Add(-time.Hour))},
	}
	res := nextFree(&state.State{}, board.Rank(snap, config.Board{}, listed), pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: listed})
	if res.Pick == nil || res.Pick.Ref != "o/r#2" || res.Pick.Step != "blocker cleared" {
		t.Fatalf("pick %+v, want o/r#2 offered as blocker cleared", res.Pick)
	}
	// No step offers #1 on its own: it is no candidate, as a Blocked item
	// without recorded blockers is none; the order says why.
	if len(res.Skipped) != 0 || len(res.AfterPick) != 0 {
		t.Errorf("skipped %+v, after the pick %+v, want o/r#1 offered by no step", res.Skipped, res.AfterPick)
	}
	want := "held by its Status Blocked / Waiting ⛔️, set 2026-09-29 after its last blocker closed 2026-09-29: it holds until the Status changes"
	if why := board.Unoffered(snap.Order, snap.Items[0], listed); why != want || skipKind(why) != kindBlocked {
		t.Errorf("o/r#1 unoffered: %q (%s), want %q (%s)", why, skipKind(why), want, kindBlocked)
	}
	// An open blocker skips both, whenever the Status was set.
	for i := range snap.Items {
		snap.Items[i].OpenBlockers, snap.Items[i].BlockersClosed = 1, time.Time{}
	}
	if res = nextFree(&state.State{}, board.Rank(snap, config.Board{}, listed), pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: listed}); res.Pick != nil {
		t.Errorf("blockers open, picked %+v", res.Pick)
	}
}

func TestSkipKind(t *testing.T) {
	for reason, want := range map[string]string{
		`served by "Worker one" through o/r#100`: kindServed,
		"assigned to sam":                        kindAssigned,
		"3 of 4 blockers open":                   kindBlocked,
		"held by its Status Blocked, set 2026-10-02 after its last blocker closed 2026-10-01: it holds until the Status changes": kindBlocked,
		"no activity since 2026-01-01":                                                      kindStale,
		`needs lease agentlab-1, held by "Lab holder"`:                                      kindLease,
		"sub-issue of epic o/r#8, in planning (Up Next)":                                    kindPlanning,
		"epic in planning (Up Next), no plan pull request names it: its planning session's": kindPlanning,
		"board fields unknown: projectItems: Resource limits for this query exceeded.":      kindUnknown,
		"sub-issues unknown: issue.subIssues: Resource limits for this query exceeded.":     kindUnknown,
		oldBacklog:                            kindOrder,
		"no step of board.order offers Inbox": kindOrder,
		"blocker cleared takes items with recorded blockers, it has none": kindOrder,
	} {
		if got := skipKind(reason); got != want {
			t.Errorf("skipKind(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestPrintNextPreviewListsTheFreeItemsAndCountsTheSkips(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	running := state.Party{Session: "s1", Name: workerOne}
	cands := boardCandidates(7)
	st := &state.State{
		Records: []state.Record{{Session: running, Issue: cands[2].Ref, At: listed.Add(-time.Hour)}},
		Agents:  []state.Agent{{Party: state.Party{Session: "s5", Name: busyAgent}, Task: cands[3].Ref + " and " + cands[6].Ref}},
		Notes:   []state.Note{{ID: 12, For: pat, Kind: noteDecision, Text: "merge " + cands[1].Ref + "?", Refs: []string{cands[1].Ref}}},
	}
	cands[0].Skip = oldBacklog
	for i := range cands {
		cands[i].Title = fmt.Sprintf("Item %d", i+1)
	}
	res := nextFree(st, cands, pickScope{me: me, alive: func(state.Party) bool { return true }, listed: listed})
	if want := map[string]int{kindOrder: 1, kindServed: 3}; !maps.Equal(res.SkippedBy, want) {
		t.Errorf("skipped by %v, want %v", res.SkippedBy, want)
	}
	var out strings.Builder
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"o/r#2 Item 2\n",
		"free behind it, next in line: 2 of 5\n",
		"  o/r#5  Item 5  Up Next\n",
		"  o/r#6  Item 6  Up Next\n",
		"skipped: 4 in all: served 3, order 1 (--json lists each with its reason)\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("preview lacks %q:\n%s", line, out.String())
		}
	}
	// A claim prints its pick and how many items are behind it, not the list.
	res.Claimed = true
	out.Reset()
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "behind it: 5 more, 2 of them free") || strings.Contains(s, "Item 5") {
		t.Errorf("claim output:\n%s", s)
	}
	// What GitHub answered with an error is named, in the text and --json.
	res.Claimed = false
	res.Missing = []string{`board.order "queue": Resource limits for this query exceeded.`}
	res.AfterPick[0].Missing = []string{"projectItems: Resource limits for this query exceeded."}
	out.Reset()
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "unknown, GitHub answered with an error: board.order \"queue\": Resource limits for this query exceeded.\n") {
		t.Errorf("preview lacks the missing search:\n%s", s)
	}
	out.Reset()
	if err := (&app{out: &out, json: true}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, `"missing": [`) || !strings.Contains(s, `"projectItems: Resource limits for this query exceeded."`) {
		t.Errorf("--json lacks missing:\n%s", s)
	}
	res.Missing, res.AfterPick[0].Missing = nil, nil
	// The preview names each epic in planning or past its go with its plan.
	res.Epics = []board.Epic{
		{Ref: "o/r#8", Title: "Epic", Status: "Up Next", Plan: &board.Plan{URL: "https://github.com/o/plans/pull/1", State: "draft", Head: "6a672cdede08"}},
		{Ref: "o/r#12", Title: "Other", Status: "In Progress"},
	}
	out.Reset()
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"epics in planning and past their go: 2\n",
		"  o/r#8 Epic (Up Next): plan https://github.com/o/plans/pull/1 (draft, head 6a672cd)\n",
		"  o/r#12 Other (In Progress): no plan pull request names it\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("preview lacks %q:\n%s", line, out.String())
		}
	}
}

func TestPrintNextListsEachSkipBeforeThePick(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	running := state.Party{Session: "s1", Name: workerOne}
	cands := boardCandidates(4)
	st := &state.State{
		Records: []state.Record{{Session: running, Issue: cands[1].Ref, At: listed.Add(-time.Hour)}},
		Agents:  []state.Agent{{Party: state.Party{Session: "s5", Name: busyAgent}, Task: "Which model for " + cands[2].Ref + "?\nThe GPU is busy."}},
	}
	cands[0].Skip = oldBacklog
	res := nextFree(st, cands, pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: listed})
	var out strings.Builder
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	skips := "skipped " + cands[0].Ref + ": " + oldBacklog + "\n" +
		"skipped " + cands[1].Ref + ": served by \"Worker one\"\n" +
		"skipped " + cands[2].Ref + ": served by \"Busy\" (task)\n"
	want := skips + cands[3].Ref + " "
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("output:\n%s\nwant it to start with:\n%s", out.String(), want)
	}
	// The text names the skipped items --json does, in its order.
	var refs []string
	for _, c := range res.Skipped {
		refs = append(refs, c.Ref)
	}
	if !slices.Equal(refs, []string{cands[0].Ref, cands[1].Ref, cands[2].Ref}) {
		t.Errorf("--json's skipped %v", refs)
	}
	// With nothing free, the skips are all it prints before the refusal.
	cands = cands[:3]
	res = nextFree(st, cands, pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: listed})
	out.Reset()
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{}); Code(err) != ExitRefused {
		t.Fatalf("no free item: err %v, want exit refused", err)
	}
	if !strings.HasPrefix(out.String(), skips) {
		t.Errorf("output without a pick:\n%s", out.String())
	}
}

func TestOffBoardAsksAboutWhatIsNoBoardItem(t *testing.T) {
	owners := map[string]string{refOne: "you", "o/r#100": `"Worker one"`, "x/y#2": "note #1 (waits on Pat)"}
	cands := []board.Candidate{{Item: board.Item{Ref: "O/R#1"}}, {Item: board.Item{Ref: idleRef}}}
	if got := offBoard(owners, cands); !slices.Equal(got, []string{"o/r#100", "x/y#2"}) {
		t.Errorf("off the board %q, want the pull request and the other repository's issue", got)
	}
}

func TestHoldTextSaysWhatBoardNextMakesOfTheItem(t *testing.T) {
	const item, blocker = "o/r#5", "o/r#9"
	for _, tc := range []struct {
		h     board.Held
		block bool
		want  string
	}{
		{board.Held{Ref: item, By: blocker, ByOpen: true, Changed: true, Blockers: 1, OpenBlockers: 1}, true,
			"blocked o/r#5 by o/r#9 (open): 1 of 1 blockers open, board next skips it until they close"},
		{board.Held{Ref: item, By: blocker, Blockers: 1}, true,
			"o/r#5 is blocked by o/r#9 (closed) already: 0 of 1 blockers open, none holds it from board next"},
		{board.Held{Ref: item, By: blocker, ByOpen: true, Changed: true}, false,
			"unblocked o/r#5: o/r#9 (open) no longer blocks it: 0 of 0 blockers open, none holds it from board next"},
		{board.Held{Ref: item, By: blocker, ByOpen: true}, false,
			"o/r#5 is not blocked by o/r#9 (open): 0 of 0 blockers open, none holds it from board next"},
	} {
		if got := holdText(&tc.h, tc.block); got != tc.want {
			t.Errorf("holdText(%+v, %v) = %q, want %q", tc.h, tc.block, got, tc.want)
		}
	}
}

func TestPrintNextLimit(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cands := boardCandidates(9)
	for i := range 3 {
		cands[i].Skip = oldBacklog
	}
	res := nextFree(&state.State{}, cands, pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: listed})
	var whole strings.Builder
	if err := (&app{out: &whole}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(whole.String(), "--limit") {
		t.Errorf("without --limit the output names it:\n%s", whole.String())
	}
	var out strings.Builder
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{limit: 2}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, line := range []string{
		"skipped " + cands[0].Ref + ": ",
		"skipped " + cands[1].Ref + ": ",
		"… 1 more skipped above the pick (--limit 2)\n",
		cands[3].Ref + " ",
		"free behind it, next in line: 5 of 5\n",
		"  " + cands[4].Ref + "  ",
		"  " + cands[5].Ref + "  ",
		"  … 3 more free (--limit 2)\n",
		"skipped: 3 in all: order 3",
	} {
		if !strings.Contains(s, line) {
			t.Errorf("--limit 2 output lacks %q:\n%s", line, s)
		}
	}
	for _, c := range []board.Candidate{cands[2], cands[6]} {
		if strings.Contains(s, c.Ref+":") || strings.Contains(s, "  "+c.Ref+"  ") {
			t.Errorf("--limit 2 lists %s:\n%s", c.Ref, s)
		}
	}
	// --json caps skipped and after_pick the same and counts what it left out.
	out.Reset()
	if err := (&app{out: &out, json: true}).printNext(res, len(cands), nextView{limit: 2}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Pick          board.Candidate   `json:"pick"`
		Skipped       []board.Candidate `json:"skipped"`
		AfterPick     []board.Candidate `json:"after_pick"`
		SkippedMore   int               `json:"skipped_more"`
		AfterPickMore int               `json:"after_pick_more"`
		SkippedBy     map[string]int    `json:"skipped_by"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Pick.Ref != cands[3].Ref || len(doc.Skipped) != 2 || len(doc.AfterPick) != 2 || doc.SkippedMore != 1 || doc.AfterPickMore != 3 || doc.SkippedBy[kindOrder] != 3 {
		t.Errorf("--json --limit 2: %s", out.String())
	}
	// Without --limit, --json names nothing left out.
	out.Reset()
	if err := (&app{out: &out, json: true}).printNext(res, len(cands), nextView{}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Contains(s, "_more") {
		t.Errorf("--json without --limit:\n%s", s)
	}
}

// fieldRef is a candidate's ref, by its --fields name.
const fieldRef = "ref"

func TestPrintNextFields(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cands := boardCandidates(3)
	cands[0].Skip = oldBacklog
	res := nextFree(&state.State{}, cands, pickScope{me: state.Party{Session: "me"}, alive: func(state.Party) bool { return true }, listed: listed})
	var out strings.Builder
	// --fields implies --json.
	if err := (&app{out: &out}).printNext(res, len(cands), nextView{fields: []string{fieldRef, "skip"}}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("--fields prints no JSON: %v\n%s", err, out.String())
	}
	want := map[string]any{
		"pick":       map[string]any{fieldRef: cands[1].Ref},
		"skipped":    []any{map[string]any{fieldRef: cands[0].Ref, "skip": oldBacklog}},
		"after_pick": []any{map[string]any{fieldRef: cands[2].Ref}},
	}
	for k, v := range want {
		if !reflect.DeepEqual(doc[k], v) {
			t.Errorf("%s = %v, want %v", k, doc[k], v)
		}
	}
	if doc["skipped_by"] == nil {
		t.Errorf("--fields drops skipped_by:\n%s", out.String())
	}
	if err := checkFields([]string{fieldRef, "title", "url", "step", "priorityOf"}); err != nil {
		t.Error(err)
	}
	if err := checkFields([]string{fieldRef, "nope"}); err == nil || !strings.Contains(err.Error(), "--fields nope: no such field") || !strings.Contains(err.Error(), "ref,url,title") {
		t.Errorf("unknown field: %v", err)
	}
}

// unserved is the In Progress item no session serves.
const unserved = "t/w#4"

func TestNextFreeSkipsWhatARunningSessionServesOrWorksOn(t *testing.T) {
	listed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	recorded := state.Party{Session: "s1", Name: workerOne}
	working := state.Party{Session: "s2", Name: "No record"}
	alive := func(p state.Party) bool { return p.Is(recorded) || p.Is(working) || p.Is(me) }
	st := &state.State{Records: []state.Record{{Session: recorded, Issue: "t/w#1", At: listed.Add(-time.Hour)}}}
	inProgress := func(n int) board.Candidate {
		return board.Candidate{Item: board.Item{Ref: fmt.Sprintf("t/w#%d", n)}, Step: stepInProgress}
	}
	// The record's item, the item a session works on with no record, and
	// the issue the pull request it works on closes are served, In Progress
	// or not; the record wins over the work.
	cands := []board.Candidate{inProgress(1), inProgress(2), inProgress(3), inProgress(4)}
	sc := pickScope{me: me, alive: alive, listed: listed, closes: map[string][]string{"t/w#9": {"t/w#3"}},
		works: []sessionWork{{party: working, refs: []string{"T/W#2", "t/w#9", "t/w#1"}}}}
	res := nextFree(st, cands, sc)
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`t/w#1: served by "Worker one"`,
		`t/w#2: served by "No record" (works on it)`,
		`t/w#3: served by "No record" (works on it) through t/w#9`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// An In Progress item no session serves is offered, and the free list
	// holds no served item.
	if res.Pick == nil || res.Pick.Ref != unserved || len(res.AfterPick) != 0 || res.SkippedBy[kindServed] != 3 {
		t.Errorf("pick %v, after %+v, by %v", res.Pick, res.AfterPick, res.SkippedBy)
	}
	cands = append(cands, inProgress(5), inProgress(2))
	res = nextFree(st, append([]board.Candidate{inProgress(6)}, cands...), sc)
	for _, c := range res.AfterPick {
		if c.Skip == "" && c.Ref != unserved && c.Ref != "t/w#5" {
			t.Errorf("free behind the pick: %s, served", c.Ref)
		}
	}
}

func TestNextFreeSkipsWhatATitleNamesByNumber(t *testing.T) {
	listed := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	me := state.Party{Session: "me", Name: "Me"}
	bs := state.Party{Session: "bs", Name: "BS 3057 roster read error"}
	fix := state.Party{Session: "fx", Name: "FIX backstage 3067 avatars-502"}
	ap := state.Party{Session: "ap", Name: "AP 4.141.1 promoter"}
	gh := state.Party{Session: "gh", Name: "devctl pin"}
	st := &state.State{Agents: []state.Agent{{Party: gh, Task: "GH 6757 devctl pin follows release (sonnet)"}}}
	sc := pickScope{me: me, alive: func(state.Party) bool { return true }, listed: listed,
		works: []sessionWork{{party: bs, title: bs.Name}, {party: fix, title: fix.Name}, {party: ap, title: ap.Name}}}
	ref := func(r string) board.Candidate { return board.Candidate{Item: board.Item{Ref: r}} }
	cands := []board.Candidate{
		ref("giantswarm/backstage#3057"), ref("giantswarm/backstage#3067"), ref("giantswarm/github#6757"),
		ref("giantswarm/beekeeper#3057"), ref("giantswarm/agent-platform#4"), ref("giantswarm/backstage#3068"),
	}
	res := nextFree(st, cands, sc)
	var got []string
	for _, c := range res.Skipped {
		got = append(got, c.Ref+": "+c.Skip)
	}
	want := []string{
		`giantswarm/backstage#3057: served by "BS 3057 roster read error" (title)`,
		`giantswarm/backstage#3067: served by "FIX backstage 3067 avatars-502" (title)`,
		`giantswarm/github#6757: served by "devctl pin" (task)`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("skipped:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// BS names no beekeeper item, a version no number, and another number
	// nothing.
	if res.Pick == nil || res.Pick.Ref != "giantswarm/beekeeper#3057" {
		t.Errorf("pick %v", res.Pick)
	}
	for _, c := range res.AfterPick {
		if c.Skip != "" {
			t.Errorf("%s: %s, want free", c.Ref, c.Skip)
		}
	}
}

func TestTagNamesARepositoryByItsLettersInOrder(t *testing.T) {
	const bs = "backstage"
	for _, tc := range []struct {
		tag, name string
		want      bool
	}{
		{"BS", bs, true}, {"GH", "gh-pages", true}, {"ACC", "app-catalog-cleanup-tool", true},
		{"AL", "agentlog", true}, {"BK", pluginName, true}, {bs, bs, true},
		{"BS", pluginName, false}, {"SB", bs, false}, {"B", bs, false}, {"FIX", bs, false},
	} {
		if got := tagNames(tc.tag, tc.name); got != tc.want {
			t.Errorf("tagNames(%q, %q) = %v", tc.tag, tc.name, got)
		}
	}
}

func TestServingWorkLeavesOutRolesIdleAgentsAndQuietSessionsNotBusy(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	session := func(id string, active time.Time) *claude.Session {
		return &claude.Session{ID: id, Name: id, LastActive: active}
	}
	sessions := []*claude.Session{
		session("wk", now), session("pl", now), session("sv", now), session("id", now), session("qt", now.Add(-2*time.Hour)), session("bz", now.Add(-2*time.Hour)),
	}
	work := make([]claude.Work, len(sessions))
	for i, s := range sessions {
		work[i] = claude.Work{Refs: []string{"t/w#" + s.ID, "n/lab#1", "t/w#2", unserved}}
	}
	st := &state.State{
		Supervisor: &state.Supervisor{Party: state.Party{Session: "sv"}},
		Agents: []state.Agent{
			{Party: state.Party{Session: "wk"}, Task: "its task"},
			{Party: state.Party{Session: "id"}, LastTask: "its last task"},
			{Party: state.Party{Session: "bz"}, Task: "its task"},
		},
	}
	var got []string
	for _, w := range servingWork(st, sessions, work, now.Add(-time.Hour)) {
		got = append(got, w.party.Session+": "+strings.Join(w.refs, " "))
	}
	want := []string{"wk: t/w#wk n/lab#1 t/w#2", "pl: t/w#pl n/lab#1 t/w#2", "bz: t/w#bz n/lab#1 t/w#2"}
	if !slices.Equal(got, want) {
		t.Errorf("serving work:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
