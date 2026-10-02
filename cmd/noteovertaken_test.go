package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	refOne  = "o/r#1"
	refTwo  = "o/r#2"
	askedQ  = "merge it?"
	hostArc = "local_archived"
)

// overtakingWatch is a running watch (it writes) on the state in dir, with
// the desktop's records in dir/desktop and GitHub answering refs.
func overtakingWatch(t *testing.T, refs map[string]string) (*watcher, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	w, _, out := notifyingWatch(t, dir, false)
	w.chores = true
	w.as = agentOne         // the note commands run outside a Claude session in CI
	w.doctoring.Store(true) // the doctor is not under test
	w.cfg.Claude.DesktopDir = filepath.Join(dir, "desktop")
	prev := refStates
	t.Cleanup(func() { refStates = prev })
	refStates = func(_ context.Context, rs []github.PR) (map[github.PR]string, error) {
		m := map[github.PR]string{}
		for _, r := range rs {
			if s, ok := refs[refName(r)]; ok {
				m[r] = s
			}
		}
		return m, nil
	}
	return w, out
}

func setNotes(t *testing.T, w *watcher, notes ...state.Note) {
	t.Helper()
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) { st.Notes = notes; return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

func openIDs(t *testing.T, w *watcher) []int {
	t.Helper()
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, n := range st.Notes {
		ids = append(ids, n.ID)
	}
	return ids
}

func overtakenEvents(t *testing.T, w *watcher) []state.Event {
	t.Helper()
	evs, err := w.store.Events(0, func(e state.Event) bool { return e.Verb == noteOvertaken })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestNoteClosesOnePollAfterItsIssueCloses(t *testing.T) {
	refs := map[string]string{refOne: github.Open, refTwo: github.Open}
	w, out := overtakingWatch(t, refs)
	setNotes(t, w,
		state.Note{ID: 1, For: personTimo, Text: askedQ, Refs: []string{refOne}},
		state.Note{ID: 2, For: personTimo, Text: "release both?", Refs: []string{refOne, refTwo}},
	)
	w.pending(context.Background(), nil)
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2}) {
		t.Fatalf("closed with every ref open: open %v", ids)
	}
	refs[refOne] = github.Closed
	w.pending(context.Background(), nil)
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
	w.pending(context.Background(), nil)
	if evs := overtakenEvents(t, w); len(evs) != 2 || overtakenReason(evs[1]) != "o/r#1 closed, o/r#2 merged" {
		t.Fatalf("once both settled: %+v", evs)
	}
}

func TestNoteWithOneOpenAndOneClosedRefStaysOpen(t *testing.T) {
	w, _ := overtakingWatch(t, map[string]string{refOne: github.Closed, refTwo: github.Open})
	setNotes(t, w, state.Note{ID: 1, For: personTimo, Text: askedQ, Refs: []string{refOne, refTwo}},
		// A ref GitHub does not answer is not settled.
		state.Note{ID: 2, For: personTimo, Text: askedQ, Refs: []string{refOne, "o/r#404"}})
	for range 2 {
		w.pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2}) || len(overtakenEvents(t, w)) != 0 {
		t.Fatalf("open %v", ids)
	}
}

func TestAnsweredNoteIsNeverOvertaken(t *testing.T) {
	refs := map[string]string{refOne: github.Open}
	w, _ := overtakingWatch(t, refs)
	setNotes(t, w,
		state.Note{ID: 1, For: personTimo, Text: askedQ, Refs: []string{refOne}},
		state.Note{ID: 2, For: personTimo, Text: "asked now", Refs: []string{refOne}},
	)
	c := w.noteCmd()
	c.SetArgs([]string{"answer", "1", "yes"})
	c.SetOut(w.out)
	c.SilenceUsage = true
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	// The guide asks #2 now: its answer must find it open.
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		guideRole.update(st, func(r *state.Role) { r.Asking = 2 })
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	refs[refOne] = github.Closed
	w.pending(context.Background(), nil)
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
	raw := `{"sessionId":"` + host + `","cliSessionId":"` + cli + `","isArchived":` + map[bool]string{true: "true", false: "false"}[archived] + `}`
	if err := os.WriteFile(filepath.Join(d, host+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// archivedNotes are notes filed by sessions of the desktop records under
// dir: an archived worker, a stopped one and an archived supervisor run.
func archivedNotes(t *testing.T, dir string) []state.Note {
	t.Helper()
	desktopRecord(t, dir, hostArc, "cli-archived", true)
	desktopRecord(t, dir, "local_stopped", "cli-stopped", false)
	desktopRecord(t, dir, "local_run", "cli-run", true)
	worker := state.Party{Session: "cli-archived", HostSession: hostArc, Name: uiAgentName}
	return []state.Note{
		{ID: 1, For: personTimo, Text: askedQ, By: worker},
		{ID: 2, For: personTimo, Text: askedQ, By: state.Party{Session: "cli-stopped", HostSession: "local_stopped", Name: "Agent ten"}},
		{ID: 3, For: personTimo, Text: askedQ, By: state.Party{Session: "cli-run", HostSession: "local_run", Name: supRun3}},
		{ID: 4, For: personTimo, Text: askedQ, By: worker, Pinned: true},
		{ID: 5, Text: "a memo", By: worker},
		// With a ref the ref decides, not the session.
		{ID: 6, For: personTimo, Text: askedQ, By: worker, Refs: []string{refOne}},
	}
}

func TestNoteWithoutRefsStaysOpenOnceItsSessionIsArchived(t *testing.T) {
	w, out := overtakingWatch(t, nil)
	setNotes(t, w, archivedNotes(t, w.cfg.Claude.DesktopDir)...)
	for range 2 {
		w.pending(context.Background(), nil)
	}
	if ids := openIDs(t, w); !slices.Equal(ids, []int{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("open %v", ids)
	}
	if evs := overtakenEvents(t, w); len(evs) != 0 {
		t.Fatalf("events %+v", evs)
	}
	if said := out.String(); strings.Contains(said, "OVERTAKEN") {
		t.Fatalf("watch said %q", said)
	}
}

func TestGuideFeedSaysOrphanedNotesOnce(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.Claude.DesktopDir = filepath.Join(t.TempDir(), "desktop")
	st := &state.State{Notes: archivedNotes(t, a.cfg.Claude.DesktopDir)}
	orphans := findOrphaned(st, claude.Archived(a.cfg))
	if len(orphans) != 1 || orphans[0].id != 1 {
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

func TestWatchOnceNamesOvertakenNotesAndWritesNothing(t *testing.T) {
	w, out := overtakingWatch(t, map[string]string{refOne: github.Closed})
	w.chores = false
	setNotes(t, w, state.Note{ID: 7, For: personTimo, Text: askedQ, Refs: []string{refOne}})
	w.pending(context.Background(), nil)
	if ids := openIDs(t, w); !slices.Equal(ids, []int{7}) || len(overtakenEvents(t, w)) != 0 {
		t.Fatalf("--once wrote: open %v", ids)
	}
	if out := out.String(); !strings.Contains(out, "NOTE OVERTAKEN (--once writes nothing): #7, o/r#1 closed") {
		t.Fatalf("watch said %q", out)
	}
}

func TestNoteAddRefAndDoneOvertaken(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, "--ref", refOne, "--ref", "https://github.com/o/r/pull/2", "--ref", refOne, "a memo"); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "--ref", "r#1", "a memo"); err == nil {
		t.Fatal("a ref without its repository was taken")
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || !slices.Equal(st.Notes[0].Refs, []string{refOne, refTwo}) {
		t.Fatalf("notes %+v", st.Notes)
	}
	done := func(args ...string) error {
		c := a.noteCmd()
		c.SetArgs(append([]string{"done"}, args...))
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
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteOvertaken || e.Verb == "note.done" })
	if err != nil || len(evs) != 1 || evs[0].Verb != noteOvertaken || overtakenReason(evs[0]) != "the session came back by itself" {
		t.Fatalf("events %+v, %v", evs, err)
	}
}

func TestGuideFeedSaysOvertakenOnce(t *testing.T) {
	a, _ := noteApp(t)
	st := &state.State{Notes: []state.Note{{ID: 4, For: notePerson, Text: askedQ, By: state.Party{Name: agentOne}}}}
	if lines, _ := a.feedLines(st, nil, nil, nil); len(lines) != 1 {
		t.Fatalf("first poll: %q", lines)
	}
	st.Notes = nil
	closed := map[int]state.Event{4: overtakenEvent(watchParty, state.Note{ID: 4, Text: askedQ}, "o/r#1 merged")}
	if lines, _ := a.feedLines(st, nil, closed, nil); !slices.Equal(lines, []string{"GUIDE CLOSED #4 overtaken: o/r#1 merged"}) {
		t.Fatalf("closed: %q", lines)
	}
	if lines, _ := a.feedLines(st, nil, closed, nil); lines != nil {
		t.Fatalf("said twice: %q", lines)
	}
}
