package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The flags of a question's status quo and why.
const (
	flagStatusQuo = "--status-quo"
	flagWhy       = "--why"
)

// decisionArgs are the parts a complete decision for the person carries.
var decisionArgs = []string{"--for", notePerson, flagStatusQuo, sqNow, flagWhy, whyNow, "--default", dfltOK, "--due", "3h"}

func TestNoteKindDefaultsByWhomItIsFor(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, append(slices.Clone(decisionArgs), "approve "+prURL)...); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "--for", supervisorRole.title, "State of the watch"); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "a memo"); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "--for", notePerson, "--kind", noteMemo, "State of the watch, for Pat to read"); err != nil {
		t.Fatalf("a memo for the person is not a question and is not checked: %v", err)
	}
	st, _ := a.store.Read()
	var kinds []string
	for _, n := range st.Notes {
		kinds = append(kinds, n.Kind)
	}
	if want := []string{noteDecision, noteMemo, noteMemo, noteMemo}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds %q, want %q", kinds, want)
	}
}

func TestNoteKindOfAnOlderNote(t *testing.T) {
	for _, c := range []struct {
		n    state.Note
		want string
	}{
		{state.Note{For: "pat"}, noteDecision},
		{state.Note{Text: "[for Pat] an older note"}, noteDecision},
		{state.Note{For: notePerson, Pinned: true}, noteMemo},
		{state.Note{For: supervisorRole.title}, noteMemo},
		{state.Note{For: guideRole.title}, noteMemo},
		{state.Note{}, noteMemo},
		{state.Note{For: notePerson, Kind: noteLogin}, noteLogin},
		{state.Note{For: notePerson, Kind: noteMemo}, noteMemo},
		{state.Note{For: supervisorRole.title, Kind: noteDecision}, noteDecision},
	} {
		if got := noteKind(notePerson, &c.n); got != c.want {
			t.Errorf("%+v: kind %q, want %q", c.n, got, c.want)
		}
	}
}

func TestDecisionNeedsItsDueAndDefault(t *testing.T) {
	a, _ := noteApp(t)
	err := addNote(a, "--kind", noteDecision, "--for", "Sam", "--due", "3h", "Pick the lab?")
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--default") {
		t.Fatalf("a decision without --default: %v", err)
	}
	err = addNote(a, "--kind", noteDecision, "--for", "Sam", "--default", dfltOK, "Pick the lab?")
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--due") {
		t.Fatalf("a decision without --due: %v", err)
	}
	err = addNote(a, append(slices.Clone(decisionArgs[:len(decisionArgs)-2]), "approve "+prURL)...)
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--due") {
		t.Fatalf("a decision for the person without --due: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--default", dfltOK, "--due", "3h", "Pick the lab?"); Code(err) != ExitUsage || !strings.Contains(err.Error(), "--for") {
		t.Fatalf("a decision for nobody: %v", err)
	}
	if err := addNote(a, "--kind", "status", "a line"); Code(err) != ExitUsage || !strings.Contains(err.Error(), noteMemo) {
		t.Fatalf("an unknown kind: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--for", "Sam", "--default", dfltOK, "--due", "3h", "Pick the lab?"); err != nil {
		t.Fatalf("a complete decision for another person: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 1 {
		t.Fatalf("only the complete decision is filed: %+v", st.Notes)
	}
}

func TestMemoNeverReachesTheGuide(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, append(slices.Clone(decisionArgs), "approve "+prURL)...); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "--for", notePerson, "--kind", noteMemo, "State of the watch"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	q, _ := guideQueue(st, nil, notePerson, time.Time{})
	if len(q) != 1 || q[0].Note.ID != 1 {
		t.Fatalf("the queue holds the decision only: %+v", q)
	}
	lines, _ := a.feedLines(st, nil, nil, []overtake{{1, "its filer is archived"}, {2, "its filer is archived"}})
	for _, l := range lines {
		if strings.Contains(l, "#2") {
			t.Fatalf("the feed says the memo: %q", lines)
		}
	}
	s := a.splitNotes(st.Notes)
	if s.guided() != 1 || len(s.Own) != 1 || s.Own[0].ID != 2 {
		t.Fatalf("the hand-over leaves the memo with the role's own notes: %+v", s)
	}
}

func TestNoteReplacesClosesTheNamedNote(t *testing.T) {
	a, out := noteApp(t)
	if err := addNote(a, "--for", supervisorRole.title, "State of the watch at 10:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := noteCommand(a, "pin", "1"); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, "--replaces", "#1", "--for", supervisorRole.title, "State of the watch at 11:00"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || st.Notes[0].ID != 2 || !st.Notes[0].Pinned {
		t.Fatalf("one state memo open, the successor pinned like its predecessor: %+v", st.Notes)
	}
	if !strings.HasSuffix(out.String(), "note #2 (replaces #1)\n") {
		t.Fatalf("printed %q", out.String())
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteReplaced })
	if err != nil || len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, "#1 replaced by #2: State of the watch at 10:00") {
		t.Fatalf("events %+v, %v", evs, err)
	}
	err = addNote(a, "--replaces", "1", "State again")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "#1 is not open") {
		t.Fatalf("replacing a closed note: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 1 {
		t.Fatalf("a refused replacement files nothing: %+v", st.Notes)
	}
}

func TestGuideFeedSaysAReplacedDecision(t *testing.T) {
	a := &app{now: relayNow, cfg: &config.Config{Guide: config.Guide{Person: notePerson}}}
	st := &state.State{Notes: []state.Note{{ID: 2, For: notePerson, Kind: noteDecision, Text: "approve it now?", By: agentC}}}
	guideRole.update(st, func(r *state.Role) { r.Fed = []string{"note#1"} })
	closed := map[int]state.Event{1: {Verb: noteReplaced, Detail: "#1 replaced by #2: approve it?"}}
	lines, _ := a.feedLines(st, nil, closed, nil)
	want := []string{`GUIDE DECISION: #2 for Pat from "Agent three" (ended): approve it now?`, "GUIDE REPLACED: #1 replaced by #2: approve it?"}
	if !slices.Equal(lines, want) {
		t.Fatalf("feed %q, want %q", lines, want)
	}
}

func TestNoteListGroupsDecisionsAndMemos(t *testing.T) {
	a, out := noteApp(t)
	a.printNotes([]state.Note{
		{ID: 1, For: supervisorRole.title, Text: "Board skip: x"},
		{ID: 2, For: notePerson, Text: "approve?", Default: "it waits"},
		{ID: 3, Text: "deferred: y", Pinned: true},
	})
	want := "Decisions (1):\n#2 [for Pat] approve? (default: it waits)\nMemos (2):\n#1 [for Supervisor] Board skip: x\n#3 [pinned] deferred: y\n"
	if out.String() != want {
		t.Fatalf("list:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	a.printNotes([]state.Note{{ID: 3, Text: "deferred: y", Pinned: true}})
	if out.String() != "#3 [pinned] deferred: y\n" {
		t.Fatalf("one kind needs no heading: %q", out.String())
	}
}
