package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
)

const overtakenRepo = "o/r"

const (
	refOne  = "o/r#1"
	refTwo  = "o/r#2"
	askedQ  = "merge it?"
	hostArc = "local_archived"
	// flagFor names a note's person, on note add and note list.
	flagFor = "--for"
)

// overtakingWatch is a running watch (it writes) on the state in dir, with
// the desktop's records in dir/desktop and GitHub answering refs: a state,
// or "CLOSED by <ref>" for an issue a closing keyword closed.
func overtakingWatch(t *testing.T, refs map[string]string) (*watch.Watcher, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	w, _, out := notifyingWatch(t, dir, false)
	w.Chores = true
	watchApp(w.Host).as = agentOne // the note commands run outside a Claude session in CI
	w.Doctoring.Store(true)        // the doctor is not under test
	watchApp(w.Host).cfg.Claude.DesktopDir = filepath.Join(dir, "desktop")
	prev := refStates
	t.Cleanup(func() { refStates = prev })
	refStates = func(_ context.Context, rs []github.PR) (map[github.PR]github.RefState, error) {
		m := map[github.PR]github.RefState{}
		for _, r := range rs {
			if s, ok := refs[refName(r)]; ok {
				at, closer, _ := strings.Cut(s, " by ")
				m[r] = github.RefState{Kind: github.RefPullRequest, State: at, Closer: closer}
			}
		}
		return m, nil
	}
	return w, out
}

// keywordClosed is refOne closed by refTwo's closing keyword.
const keywordClosed = github.Closed + " by " + refTwo

// keptEvents are the note.kept events of w's log.
func keptEvents(t *testing.T, w *watch.Watcher) []state.Event {
	t.Helper()
	evs, err := watchApp(w.Host).store.Events(0, func(e state.Event) bool { return e.Verb == watch.NoteKept })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// listedFor is what note list --for person prints.
func listedFor(t *testing.T, w *watch.Watcher, person string) string {
	t.Helper()
	var out bytes.Buffer
	prev := watchApp(w.Host).out
	watchApp(w.Host).out = &out
	defer func() { watchApp(w.Host).out = prev }()
	c := watchApp(w.Host).noteCmd()
	c.SetArgs([]string{"list", flagFor, person})
	c.SilenceUsage = true
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestKeywordCloseKeepsTheNoteOpenAndSaysSoOnce(t *testing.T) {
	refs := map[string]string{refOne: keywordClosed}
	w, out := overtakingWatch(t, refs)
	// The supervisor filed it: no worker's task end can settle it.
	setNotes(t, w, state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, By: state.Party{Name: supRun3}, Refs: []string{refOne}})
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1}) || len(overtakenEvents(t, w)) != 0 {
		t.Fatalf("a keyword close overtook the note: open %v", ids)
	}
	if listed := listedFor(t, w, personTimo); !strings.Contains(listed, askedQ) {
		t.Fatalf("note list --for %s: %q", personTimo, listed)
	}
	const line = "NOTE KEPT: #1, o/r#1 closed by o/r#2's closing keyword, not by its worker: " + askedQ
	if said := out.String(); strings.Count(said, line) != 1 {
		t.Fatalf("watch said %q", said)
	}
	if evs := keptEvents(t, w); len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "#1 kept: o/r#1 closed by o/r#2's closing keyword, not by its worker"+overtakenSep) {
		t.Fatalf("events %+v", evs)
	}
	// Reopened and closed by hand: a deliberate close overtakes it.
	refs[refOne] = github.Closed
	w.Pending(context.Background(), nil)
	if evs := overtakenEvents(t, w); len(evs) != 1 || overtakenReason(evs[0]) != "o/r#1 closed" || len(openIDs(t, w)) != 0 {
		t.Fatalf("closed by hand: %+v, open %v", evs, openIDs(t, w))
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWorkerNoteIsKeptWhileItsTaskRunsAndOvertakenOnceDone(t *testing.T) {
	refs := map[string]string{refOne: keywordClosed, refTwo: github.Closed}
	w, out := overtakingWatch(t, refs)
	worker := state.Party{Name: agentOne}
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: worker, Task: "the task"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	setNotes(t, w,
		state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, By: worker, Refs: []string{refOne}},
		// Closed by hand while the worker runs: still not its worker's close.
		state.Note{ID: 2, Kind: noteDecision, For: personTimo, Text: "keep it?", By: worker, Refs: []string{refTwo}},
	)
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2}) || len(overtakenEvents(t, w)) != 0 {
		t.Fatalf("overtaken while its worker runs: open %v", ids)
	}
	said := out.String()
	for _, line := range []string{
		`NOTE KEPT: #1, o/r#1 closed by o/r#2's closing keyword, its worker "Agent one" still runs: ` + askedQ,
		`NOTE KEPT: #2, o/r#2 closed, its worker "Agent one" still runs: keep it?`,
	} {
		if strings.Count(said, line) != 1 {
			t.Fatalf("watch said %q, want %q once", said, line)
		}
	}
	// The worker ends its task with the issues closed: they settle its
	// notes, its next task notwithstanding.
	for _, args := range [][]string{
		{verbIdle, flagDone, flagReport, "merged https://github.com/o/r/pull/2", flagProblem, noProblems},
		{"assign", agentOne, "another task"},
	} {
		c := watchApp(w.Host).agentsCmd()
		c.SetArgs(args)
		c.SetOut(watchApp(w.Host).out)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	w.Pending(context.Background(), nil)
	if ids := openIDs(t, w); len(ids) != 0 {
		t.Fatalf("its worker done: open %v", ids)
	}
	evs := overtakenEvents(t, w)
	if len(evs) != 2 || overtakenReason(evs[0]) != `o/r#1 closed by o/r#2's closing keyword, its worker "Agent one" done` || overtakenReason(evs[1]) != `o/r#2 closed, its worker "Agent one" done` {
		t.Fatalf("events %+v", evs)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWorkerNoteClosedAfterItsTaskEndedIsOvertaken(t *testing.T) {
	w, _ := overtakingWatch(t, map[string]string{refOne: keywordClosed})
	// The worker is off the roster already; its note carries the task's end.
	setNotes(t, w, state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, By: state.Party{Name: agentOne}, Refs: []string{refOne}, TaskEnded: relayNow})
	w.Pending(context.Background(), nil)
	if evs := overtakenEvents(t, w); len(evs) != 1 || overtakenReason(evs[0]) != `o/r#1 closed by o/r#2's closing keyword, its worker "Agent one" done` {
		t.Fatalf("events %+v", evs)
	}
}

func setNotes(t *testing.T, w *watch.Watcher, notes ...state.Note) {
	t.Helper()
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) { st.Notes = notes; return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

func openIDs(t *testing.T, w *watch.Watcher) []int {
	t.Helper()
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, n := range st.Notes {
		ids = append(ids, n.ID)
	}
	return ids
}

func overtakenEvents(t *testing.T, w *watch.Watcher) []state.Event {
	t.Helper()
	evs, err := watchApp(w.Host).store.Events(0, func(e state.Event) bool { return e.Verb == noteOvertaken })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestNoteClosesOnePollAfterItsIssueCloses(t *testing.T) {
	refs := map[string]string{refOne: github.Open, refTwo: github.Open}
	w, out := overtakingWatch(t, refs)
	setNotes(t, w,
		state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, Refs: []string{refOne}},
		state.Note{ID: 2, Kind: noteDecision, For: personTimo, Text: "release both?", Refs: []string{refOne, refTwo}},
	)
	w.Pending(context.Background(), nil)
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2}) {
		t.Fatalf("closed with every ref open: open %v", ids)
	}
	refs[refOne] = github.Closed
	w.Pending(context.Background(), nil)
	if ids := openIDs(t, w); !slices.Equal(ids, []int{2}) {
		t.Fatalf("one poll after the issue closed: open %v", ids)
	}
	evs := overtakenEvents(t, w)
	if len(evs) != 1 || overtakenReason(evs[0]) != "o/r#1 closed" || !strings.HasPrefix(evs[0].Detail, "#1 overtaken: ") {
		t.Fatalf("events %+v", evs)
	}
	if said := out.String(); !strings.Contains(said, "NOTE OVERTAKEN: #1, o/r#1 closed: "+askedQ) || strings.Contains(said, "#2,") {
		t.Fatalf("watch said %q", said)
	}
	refs[refTwo] = github.Merged
	w.Pending(context.Background(), nil)
	if evs := overtakenEvents(t, w); len(evs) != 2 || overtakenReason(evs[1]) != "o/r#1 closed, o/r#2 merged" {
		t.Fatalf("once both settled: %+v", evs)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestNoteWithOneOpenAndOneClosedRefStaysOpen(t *testing.T) {
	w, _ := overtakingWatch(t, map[string]string{refOne: github.Closed, refTwo: github.Open})
	setNotes(t, w, state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, Refs: []string{refOne, refTwo}},
		// A ref GitHub does not answer is not settled.
		state.Note{ID: 2, Kind: noteDecision, For: personTimo, Text: askedQ, Refs: []string{refOne, "o/r#404"}})
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2}) || len(overtakenEvents(t, w)) != 0 {
		t.Fatalf("open %v", ids)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestAnsweredNoteIsNeverOvertaken(t *testing.T) {
	refs := map[string]string{refOne: github.Open}
	w, _ := overtakingWatch(t, refs)
	setNotes(t, w,
		state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, Refs: []string{refOne}},
		state.Note{ID: 2, Kind: noteDecision, For: personTimo, Text: "asked now", Refs: []string{refOne}},
	)
	c := watchApp(w.Host).noteCmd()
	c.SetArgs([]string{verbAnswer, "1", "yes"})
	c.SetOut(watchApp(w.Host).out)
	c.SilenceUsage = true
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	// The guide asks #2 now: its answer must find it open.
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		guideRole.Update(st, func(r *state.Role) { r.Asking = 2 })
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	refs[refOne] = github.Closed
	w.Pending(context.Background(), nil)
	if evs := overtakenEvents(t, w); len(evs) != 0 {
		t.Fatalf("an answered or asked note overtaken: %+v", evs)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{2}) {
		t.Fatalf("open %v", ids)
	}
}

// desktopRecord writes a desktop session record under dir.
func desktopRecord(t *testing.T, dir, host, cli string, archived bool) {
	t.Helper()
	d := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"sessionId":"` + host + `","cliSessionId":"` + cli + `","isArchived":` + map[bool]string{true: probeHolds, false: "false"}[archived] + `}`
	if err := os.WriteFile(filepath.Join(d, host+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// archivedNotes are notes filed by sessions of the desktop records under
// dir: an archived worker, a stopped one and an archived guide run.
func archivedNotes(t *testing.T, dir string) []state.Note {
	t.Helper()
	desktopRecord(t, dir, hostArc, "cli-archived", true)
	desktopRecord(t, dir, "local_stopped", "cli-stopped", false)
	desktopRecord(t, dir, "local_run", "cli-run", true)
	worker := state.Party{Session: "cli-archived", HostSession: hostArc, Name: uiAgentName}
	return []state.Note{
		{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, By: worker},
		{ID: 2, Kind: noteDecision, For: personTimo, Text: askedQ, By: state.Party{Session: "cli-stopped", HostSession: "local_stopped", Name: "Agent ten"}},
		{ID: 3, Kind: noteDecision, For: personTimo, Text: askedQ, By: state.Party{Session: "cli-run", HostSession: "local_run", Name: supRun3}},
		// With a ref the ref decides, not the session.
		{ID: 6, Kind: noteDecision, For: personTimo, Text: askedQ, By: worker, Refs: []string{refOne}},
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestNoteWithoutRefsStaysOpenOnceItsSessionIsArchived(t *testing.T) {
	w, out := overtakingWatch(t, nil)
	setNotes(t, w, archivedNotes(t, watchApp(w.Host).cfg.Claude.DesktopDir)...)
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2, 3, 6}) {
		t.Fatalf("open %v", ids)
	}
	if evs := overtakenEvents(t, w); len(evs) != 0 {
		t.Fatalf("events %+v", evs)
	}
	if said := out.String(); strings.Contains(said, "OVERTAKEN") {
		t.Fatalf("watch said %q", said)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGuideFeedSaysOrphanedNotesOnce(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.Claude.DesktopDir = filepath.Join(t.TempDir(), "desktop")
	a.cfg.Guide.Person = personTimo
	st := &state.State{Notes: archivedNotes(t, a.cfg.Claude.DesktopDir)}
	orphans := findOrphaned(st, claude.Archived(a.cfg))
	if len(orphans) != 1 || orphans[0].Id != 1 {
		t.Fatalf("orphans %+v", orphans)
	}
	want := `GUIDE ORPHANED #1 for ` + personTimo + `, its filing session "` + uiAgentName + `" is archived; ask it, or close it with note done 1 --overtaken: ` + askedQ
	if lines, _ := a.feedLines(st, nil, nil, orphans); !slices.Contains(lines, want) {
		t.Fatalf("first poll: %q", lines)
	}
	if lines, _ := a.feedLines(st, nil, nil, orphans); slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "GUIDE ORPHANED") }) {
		t.Fatalf("said twice: %q", lines)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWatchOnceNamesOvertakenNotesAndWritesNothing(t *testing.T) {
	w, out := overtakingWatch(t, map[string]string{refOne: github.Closed, refTwo: github.Closed + " by o/r#3"})
	w.Chores = false
	setNotes(t, w, state.Note{ID: 7, Kind: noteDecision, For: personTimo, Text: askedQ, Refs: []string{refOne}},
		state.Note{ID: 8, Kind: noteDecision, For: personTimo, Text: askedQ, Refs: []string{refTwo}})
	w.Pending(context.Background(), nil)
	st, _ := watchApp(w.Host).store.Read()
	if ids := openIDs(t, w); !slices.Equal(ids, []int{7, 8}) || len(overtakenEvents(t, w)) != 0 || len(keptEvents(t, w)) != 0 || st.Notes[1].Kept != "" {
		t.Fatalf("--once wrote: open %v, kept %q", ids, st.Notes[1].Kept)
	}
	if out := out.String(); !strings.Contains(out, "NOTE OVERTAKEN (--once writes nothing): #7, o/r#1 closed") ||
		!strings.Contains(out, "NOTE KEPT (--once writes nothing): #8, o/r#2 closed by o/r#3's closing keyword, not by its worker") {
		t.Fatalf("watch said %q", out)
	}
}

func TestNoteAddRefAndDoneOvertaken(t *testing.T) {
	a, _ := noteApp(t)
	sam := []string{flagFor, samName, flagStatusQuo, sqNow, flagDefault, dfltOK, flagDue, "3h"}
	if err := addNote(a, append(slices.Clone(sam), "--ref", refOne, "--ref", "https://github.com/o/r/pull/2", "--ref", refOne, "pick a lab?")...); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, append(slices.Clone(sam), "--ref", "r#1", "pick a lab?")...); err == nil {
		t.Fatal("a ref without its repository was taken")
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || !slices.Equal(st.Notes[0].Refs, []string{refOne, refTwo}) {
		t.Fatalf("notes %+v", st.Notes)
	}
	done := func(args ...string) error {
		c := a.noteCmd()
		c.SetArgs(append([]string{paramDone}, args...))
		c.SetOut(a.out)
		c.SetErr(a.out)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	if err := done("1", "--overtaken", " "); err == nil {
		t.Fatal("--overtaken without a reason was taken")
	}
	if err := done("1", "--overtaken", "the session came back by itself"); err != nil {
		t.Fatal(err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteOvertaken || e.Verb == noteDone })
	if err != nil || len(evs) != 1 || evs[0].Verb != noteOvertaken || overtakenReason(evs[0]) != "the session came back by itself" {
		t.Fatalf("events %+v, %v", evs, err)
	}
}

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestGuideFeedSaysOvertakenOnce(t *testing.T) {
	a, _ := noteApp(t)
	st := &state.State{Notes: []state.Note{{ID: 4, Kind: noteDecision, For: notePerson, Text: askedQ, By: state.Party{Name: agentOne}}}}
	if lines, _ := a.feedLines(st, nil, nil, nil); len(lines) != 1 {
		t.Fatalf("first poll: %q", lines)
	}
	st.Notes = nil
	closed := map[int]state.Event{4: overtakenEvent(watch.WatchParty, state.Note{ID: 4, Kind: noteDecision, Text: askedQ}, "o/r#1 merged")}
	if lines, _ := a.feedLines(st, nil, closed, nil); !slices.Equal(lines, []string{"GUIDE CLOSED #4 overtaken: o/r#1 merged"}) {
		t.Fatalf("closed: %q", lines)
	}
	if lines, _ := a.feedLines(st, nil, closed, nil); lines != nil {
		t.Fatalf("said twice: %q", lines)
	}
}

// The same merge settles an older question but cannot settle one filed
// about the already merged PR, even after its worker reports done.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestNoteOvertakenOnlyByMergeAfterItWasAdded(t *testing.T) {
	merged := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		added    time.Time
		ended    time.Time
		wantOpen bool
	}{
		{"added before merge", merged.Add(-time.Hour), time.Time{}, false},
		{"added after merge", merged.Add(time.Hour), time.Time{}, true},
		{"added at merge", merged, time.Time{}, true},
		{"worker done after existing merge", merged.Add(time.Hour), merged.Add(2 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := overtakingWatch(t, nil)
			current := github.Open
			statusQuo := "The referenced PR is open."
			if tc.wantOpen {
				current = github.Merged
				statusQuo = "The referenced PR is merged."
			}
			refStates = func(_ context.Context, _ []github.PR) (map[github.PR]github.RefState, error) {
				return map[github.PR]github.RefState{{Repo: overtakenRepo, N: 1}: {Kind: github.RefPullRequest, State: current, MergedAt: merged}}, nil
			}
			setNotes(t, w, state.Note{ID: 1, Kind: noteDecision, For: "note reader", Text: "Does this change need a decision?", StatusQuo: statusQuo, At: tc.added, TaskEnded: tc.ended, Refs: []string{refOne}})
			// A note against an open PR stays open until the merge.
			w.Pending(context.Background(), nil)
			if len(openIDs(t, w)) != 1 {
				t.Fatal("note closed before a later merge")
			}
			current = github.Merged
			for range 2 {
				w.Pending(context.Background(), nil)
			}
			if got := len(openIDs(t, w)) == 1; got != tc.wantOpen {
				t.Fatalf("open = %v, want %v", got, tc.wantOpen)
			}
			wantEvents := 1
			if tc.wantOpen {
				wantEvents = 0
			}
			if got := len(overtakenEvents(t, w)); got != wantEvents {
				t.Fatalf("overtaken events = %d, want %d", got, wantEvents)
			}
		})
	}
}

// A person's to-do is theirs whatever happens to the issue it names: its
// ref closing never closes it, and the watch reads no GitHub for it
// (https://github.com/giantswarm/beekeeper/issues/3848).
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestTodoIsNeverOvertakenByItsRef(t *testing.T) {
	w, out := overtakingWatch(t, map[string]string{refOne: github.Closed})
	setNotes(t, w, state.Note{ID: 1, Kind: noteTodo, For: personTimo, Text: "rotate the robot password", Refs: []string{refOne}})
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1}) || len(overtakenEvents(t, w)) != 0 {
		t.Fatalf("the to-do was overtaken: open %v", ids)
	}
	if said := out.String(); strings.Contains(said, "NOTE OVERTAKEN") || strings.Contains(said, "NOTE KEPT") {
		t.Fatalf("watch said %q", said)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if refs := watch.OvertakeRefs(st); len(refs) != 0 {
		t.Fatalf("the watch reads a to-do's refs: %v", refs)
	}
}

// reopenNote runs note reopen with args on a.
func reopenNote(a *app, args ...string) error {
	c := a.noteCmd()
	c.SetArgs(append([]string{reopenName}, args...))
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

// A note the overtaken rule closed stays in the file store's history:
// note show reads it back with its reason, and note reopen puts it back
// whole, without the hold its close released, where its settled ref no
// longer overtakes it.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestReopenRestoresAnOvertakenNote(t *testing.T) {
	w, _ := overtakingWatch(t, map[string]string{refOne: github.Closed})
	setNotes(t, w, state.Note{ID: 1, Kind: noteDecision, For: personTimo, Text: askedQ, Default: dfltOK, Refs: []string{refOne}})
	w.Pending(context.Background(), nil)
	if ids := openIDs(t, w); len(ids) != 0 || len(overtakenEvents(t, w)) != 1 {
		t.Fatalf("not overtaken: open %v", ids)
	}
	var out bytes.Buffer
	watchApp(w.Host).out = &out
	c := watchApp(w.Host).noteCmd()
	c.SetArgs([]string{noteShowName, "1"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{askedQ, "[status: " + noteStatusClosed + "]", "reason: o/r#1 closed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("note show lacks %q: %q", want, out.String())
		}
	}
	if err := reopenNote(watchApp(w.Host), "1"); err != nil {
		t.Fatal(err)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].For != personTimo || !slices.Equal(st.Notes[0].Refs, []string{refOne}) || st.Notes[0].Reopened.IsZero() {
		t.Fatalf("reopened %+v", st.Notes)
	}
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1}) || len(overtakenEvents(t, w)) != 1 {
		t.Fatalf("the reopened note was overtaken again: open %v", ids)
	}
}

// note reopen drops what the close released: the board items the note
// held and the watch's reason for keeping it.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestReopenHoldsNothing(t *testing.T) {
	a, out := noteApp(t)
	n := state.Note{ID: 3, Kind: noteDecision, For: notePerson, Text: askedQ, Default: dfltOK, Refs: []string{refOne},
		Held: &state.ItemHold{Ref: refOne}, Kept: "o/r#1 closed, its worker still runs"}
	if err := a.store.Record(overtakenEvent(watch.WatchParty, n, "o/r#1 closed")); err != nil {
		t.Fatal(err)
	}
	if err := reopenNote(a, "3"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].Held != nil || st.Notes[0].Kept != "" || st.Notes[0].Text != askedQ {
		t.Fatalf("reopened %+v", st.Notes)
	}
}

// An overtaken event written before the note was retained whole names the
// note's text but not its person: note show says it, and note reopen
// refuses, saying to file it again, instead of "not found".
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestReopenOfAnOldOvertakenNoteSaysToFileItAgain(t *testing.T) {
	a, out := noteApp(t)
	old := event(watch.WatchParty, noteOvertaken, "#5 overtaken: o/r#1 closed%srotate the robot password", overtakenSep)
	if err := a.store.Record(old); err != nil {
		t.Fatal(err)
	}
	c := a.noteCmd()
	c.SetArgs([]string{noteShowName, "5"})
	if err := c.Execute(); err != nil || !strings.Contains(out.String(), "rotate the robot password") {
		t.Fatalf("note show: %v: %q", err, out)
	}
	err := reopenNote(a, "5")
	if err == nil || !strings.Contains(err.Error(), "file it again") || !strings.Contains(err.Error(), "rotate the robot password") {
		t.Fatalf("reopen: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 0 {
		t.Fatalf("reopened %+v", st.Notes)
	}
}
