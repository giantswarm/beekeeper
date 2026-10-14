package cmd

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// fullMemo is a multi-line memo that names an off-board issue on a line of
// its own, past the first.
const fullMemo = "Board skip: deferred work\n- giantswarm/other#12 waits on a release\n- see https://github.com/q/s/issues/4"

// memoDefault is the listed decision's default.
const memoDefault = "the bump waits"

// listNotes is what note list prints with args, on notes.
func listNotes(t *testing.T, notes []state.Note, args ...string) string {
	t.Helper()
	a, out := noteApp(t)
	a.cfg.Board.Owner = defaultOrg
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Notes = notes; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "--json") {
		a.json = true
		args = slices.DeleteFunc(args, func(s string) bool { return s == "--json" })
	}
	c := a.noteCmd()
	c.SetArgs(append([]string{"list"}, args...))
	c.SilenceUsage = true
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

var listedNotes = []state.Note{
	{ID: 1, For: supervisorRole.title, Text: fullMemo, Refs: []string{refTwo}},
	{ID: 2, For: notePerson, Text: "approve beekeeper#7?", Default: memoDefault},
}

func TestNoteListFullPrintsTheWholeTextAndWhatItNames(t *testing.T) {
	want := "Decisions (1):\n" +
		"#2 [for Pat]\n  approve beekeeper#7?\n  default: " + memoDefault + "\n  names: giantswarm/beekeeper#7\n" +
		"Memos (1):\n" +
		"#1 [for Supervisor]\n  Board skip: deferred work\n  - giantswarm/other#12 waits on a release\n  - see https://github.com/q/s/issues/4\n" +
		"  names: giantswarm/other#12, q/s#4, o/r#2\n"
	if got := listNotes(t, listedNotes, "--full"); got != want {
		t.Fatalf("list --full:\n%s\nwant:\n%s", got, want)
	}
}

func TestNoteListDefaultStaysOneEntryPerNote(t *testing.T) {
	want := "Decisions (1):\n#2 [for Pat] approve beekeeper#7? (default: " + memoDefault + ")\nMemos (1):\n#1 [for Supervisor] " + fullMemo + "\n"
	if got := listNotes(t, listedNotes); got != want {
		t.Fatalf("list:\n%s\nwant:\n%s", got, want)
	}
}

func TestNoteListFullJSONCarriesTheNames(t *testing.T) {
	var got []struct {
		Text  string   `json:"text"`
		Names []string `json:"names"`
	}
	if err := json.Unmarshal([]byte(listNotes(t, listedNotes, "--json", "--full")), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != fullMemo || !slices.Equal(got[0].Names, []string{"giantswarm/other#12", "q/s#4", refTwo}) ||
		!slices.Equal(got[1].Names, []string{"giantswarm/beekeeper#7"}) {
		t.Fatalf("list --json --full: %+v", got)
	}
	var plain []map[string]any
	if err := json.Unmarshal([]byte(listNotes(t, listedNotes, "--json")), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain[0]["names"]; ok {
		t.Fatalf("list --json without --full carries names: %v", plain[0])
	}
}
