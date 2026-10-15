package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

// The flags of a question's status quo, why and due time.
const (
	flagStatusQuo = "--status-quo"
	flagWhy       = "--why"
	flagDue       = "--due"
	flagDefault   = "--default"
	flagFoldInto  = "--fold-into"
)

// samName is a person other than the guide's, signIn a login's question.
const (
	samName = "Sam"
	signIn  = "sign in"
)

// decisionArgs are the parts a complete decision for the person carries.
var decisionArgs = []string{flagRef, decisionRef, flagFor, notePerson, flagStatusQuo, sqNow, flagWhy, whyNow, flagDefault, dfltOK, flagDue, "3h"}

// decisionRef is the thread of decisionArgs, the pull request it asks about.
const decisionRef = "o/r#7"

// samArgs are the parts a complete decision for Sam, not the guide's
// person, carries: it is not checked for its status quo and why.
var samArgs = []string{flagFor, samName, flagStatusQuo, sqNow, flagDefault, dfltOK, flagDue, "3h", flagRef, decisionRef}

func TestNoteAddFilesOnlyDecisionsAndLogins(t *testing.T) {
	a, _ := noteApp(t)
	if err := addNote(a, append(slices.Clone(decisionArgs), "approve "+prURL)...); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"a memo":            {"--for", notePerson, "--kind", "memo", "State of the watch"},
		"a pinned note":     append(slices.Clone(decisionArgs), "--pin", "approve "+prURL),
		"a replacement":     append(slices.Clone(decisionArgs), "--replaces", "1", "approve "+prURL),
		"a note for nobody": {"--kind", noteLogin, "--until", probeHolds, signIn},
	} {
		if err := addNote(a, args...); err == nil {
			t.Errorf("%s is filed", name)
		}
	}
	for _, sub := range []string{"pin", "unpin"} {
		if _, err := noteCommand(a, sub, "1"); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("note %s: %v", sub, err)
		}
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || st.Notes[0].Kind != noteDecision {
		t.Fatalf("only the decision is filed, as a decision: %+v", st.Notes)
	}
}

func TestDecisionNeedsItsDueAndDefault(t *testing.T) {
	a, _ := noteApp(t)
	err := addNote(a, "--kind", noteDecision, "--for", samName, "--due", "3h", "Pick the lab?")
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--default") {
		t.Fatalf("a decision without --default: %v", err)
	}
	err = addNote(a, "--kind", noteDecision, "--for", samName, "--default", dfltOK, "Pick the lab?")
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
	if err := addNote(a, "--kind", "status", "--for", samName, "a line"); Code(err) != ExitUsage || !strings.Contains(err.Error(), "decision, login") {
		t.Fatalf("an unknown kind: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--for", samName, "--default", dfltOK, "--due", "3h", "Pick the lab?"); err == nil || !strings.Contains(err.Error(), "--status-quo") {
		t.Fatalf("a decision for another person without its status quo: %v", err)
	}
	if err := addNote(a, "--kind", noteDecision, "--for", samName, "--status-quo", sqNow, "--default", dfltOK, "--due", "3h", "--ref", decisionRef, "Pick the lab?"); err != nil {
		t.Fatalf("a complete decision for another person: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 1 {
		t.Fatalf("only the complete decision is filed: %+v", st.Notes)
	}
}

func TestNoteListPrintsOneLinePerNote(t *testing.T) {
	a, out := noteApp(t)
	a.printNotes([]state.Note{
		{ID: 2, For: notePerson, Kind: noteDecision, Text: "approve?", Default: "it waits"},
		{ID: 3, For: notePerson, Kind: noteLogin, Text: signIn},
	})
	if want := "#2 [for Pat] approve? (default: it waits)\n#3 [for Pat] " + signIn + "\n"; out.String() != want {
		t.Fatalf("list:\n%s\nwant:\n%s", out.String(), want)
	}
}
