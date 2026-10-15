package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// writeState writes doc as the state document of a's store.
func writeState(t *testing.T, a *app, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.store.(*state.FileStore).Dir(), "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// olderNotes is a state document an older beekeeper wrote: a memo, a pinned
// memo, a note filed before kinds, a pinned note without a kind and a
// decision.
const olderNotes = `{"nextNote": 5, "notes": [
	{"id": 1, "for": "Supervisor", "kind": "memo", "text": "Board skip: o/r#10 until its plan merges", "by": {"name": "Supervisor run 3"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 2, "kind": "memo", "pinned": true, "text": "priorities: beekeeper#943, then https://github.com/o/r/issues/12", "by": {"name": "Guide"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 3, "for": "Guide", "text": "deferred: o/r#13", "by": {"name": "Agent one"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 4, "for": "Supervisor", "pinned": true, "text": "report every merge", "by": {"name": "Guide"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 5, "for": "Pat", "kind": "decision", "text": "merge o/r#7?", "refs": ["o/r#7"], "default": "it waits", "by": {"name": "Agent one"}, "at": "2026-10-01T00:00:00Z"}]}`

func TestNoteMigrateListsAndClosesTheMemosAndPins(t *testing.T) {
	a, out := noteApp(t)
	a.cfg.Board.Owner, a.cfg.StateDir = defaultOrg, t.TempDir()
	writeState(t, a, olderNotes)
	if _, err := noteCommand(a, "migrate"); Code(err) != ExitUsage {
		t.Fatalf("migrate without --dry-run or --apply: %v", err)
	}
	if _, err := noteCommand(a, "migrate", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"#1 [memo, for Supervisor, by \"Supervisor run 3\"]\n  Board skip: o/r#10 until its plan merges\n  names: o/r#10\n",
		"#2 [pinned, by \"Guide\"]\n",
		"  names: giantswarm/beekeeper#943, o/r#12\n",
		"#3 [memo, for Guide, by \"Agent one\"]\n",
		"#4 [pinned, for Supervisor, by \"Guide\"]\n  report every merge\n",
		"4 memos (2 pinned) naming 4 issues and pull requests",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "#5") {
		t.Errorf("the dry run lists the decision:\n%s", got)
	}
	if st, _ := a.store.Read(); len(st.Memos) != 4 || len(st.Notes) != 1 {
		t.Fatalf("a dry run closes nothing: notes %+v, memos %+v", st.Notes, st.Memos)
	}

	if _, err := noteCommand(a, "migrate", "--apply", "2", "5"); Code(err) != ExitRefused || !strings.Contains(err.Error(), "#5") {
		t.Fatalf("closing a decision as a memo: %v", err)
	}
	if _, err := noteCommand(a, "migrate", "--apply", "#2"); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.store.Read(); len(st.Memos) != 3 {
		t.Fatalf("one id closes one memo: %+v", st.Memos)
	}
	if _, err := noteCommand(a, "migrate", "--apply"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.Memos) != 0 || len(st.Notes) != 1 || st.Notes[0].ID != 5 {
		t.Fatalf("every memo closed, the decision open: notes %+v, memos %+v", st.Notes, st.Memos)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteMigrated })
	if err != nil || len(evs) != 4 || evs[0].Detail != "#2 priorities: beekeeper#943, then https://github.com/o/r/issues/12" {
		t.Fatalf("events %+v, %v", evs, err)
	}
	out.Reset()
	if _, err := noteCommand(a, "migrate", "--dry-run"); err != nil || out.String() != "no memos or pinned notes\n" {
		t.Fatalf("after the migration: %q, %v", out.String(), err)
	}
}
