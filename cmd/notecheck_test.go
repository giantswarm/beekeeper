package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	prURL  = "https://github.com/o/r/pull/7"
	sqNow  = "the PR waits for review"
	whyNow = "only Pat approves releases"
	dfltOK = "the release stays unpublished"
	// notePerson is the guide's person of noteApp.
	notePerson = "Pat"
)

// noteApp is an app with guide.person Pat and a scratch store.
func noteApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &app{store: store, out: &out, as: agentOne, now: relayNow, cfg: &config.Config{Guide: config.Guide{Person: notePerson}}}, &out
}

func addNote(a *app, args ...string) error {
	c := a.noteCmd()
	c.SetArgs(append([]string{"add"}, args...))
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

func TestNoteForPersonRefusesWhatItLacks(t *testing.T) {
	full := noteDraft{Question: "approve o/r#7 (" + prURL + ")", StatusQuo: sqNow, Why: whyNow, Default: dfltOK}
	if m := full.missing(); len(m) != 0 {
		t.Fatalf("a complete note lacks %q", m)
	}
	for name, c := range map[string]struct {
		edit func(*noteDraft)
		want string
	}{
		"bare #N":            {func(d *noteDraft) { d.Question = "approve #7" }, "#7 without its full URL"},
		"owner/repo#N":       {func(d *noteDraft) { d.StatusQuo = "x/y#7 is open" }, "x/y#7 without its full URL"},
		"no status quo":      {func(d *noteDraft) { d.StatusQuo = "" }, "--status-quo"},
		"no why":             {func(d *noteDraft) { d.Why = " " }, "--why"},
		"option, no effect":  {func(d *noteDraft) { d.Options = []string{"publish"} }, `--option "publish" has no`},
		"default wait":       {func(d *noteDraft) { d.Default = "Wait." }, `--default "Wait." is no action`},
		"default nothing":    {func(d *noteDraft) { d.Default = "nothing" }, "is no action"},
		"no default":         {func(d *noteDraft) { d.Default = "" }, "is no action"},
		"claim unchecked":    {func(d *noteDraft) { d.StatusQuo = "CI is green" }, `"green" without --checked`},
		"login, no probe":    {func(d *noteDraft) { d.Kind = noteLogin }, "--until"},
		"probe, no login":    {func(d *noteDraft) { d.Until = "true" }, "--until without --kind login"},
		"status only":        {func(d *noteDraft) { d.Question = "Board pull 83 finished its epic." }, "asks nothing"},
		"status, to the log": {func(d *noteDraft) { d.Question = "worker done" }, "beekeeper log add"},
	} {
		d := full
		c.edit(&d)
		if m := strings.Join(d.missing(), "; "); !strings.Contains(m, c.want) {
			t.Errorf("%s: missing %q, want %q", name, m, c.want)
		}
	}
	ok := full
	ok.StatusQuo, ok.Checked = "CI is green", "gh pr checks"
	ok.Options = []string{"publish: the users get the fix today"}
	ok.Question += " like note #3, timer #4 and " + prURL + "#issuecomment-1"
	if m := ok.missing(); len(m) != 0 {
		t.Fatalf("checked claims, note and timer ids and URL fragments are fine, lacks %q", m)
	}
	for _, q := range []string{"Which lab gets the run?", "Please merge " + prURL} {
		d := full
		d.Question = q
		if m := d.missing(); len(m) != 0 {
			t.Errorf("%q asks, lacks %q", q, m)
		}
	}
}

func TestNoteAddRefusesAndAcceptsForThePerson(t *testing.T) {
	a, out := noteApp(t)
	err := addNote(a, "--for", "pat", "--default", "wait", "approve #7")
	if Code(err) != ExitUsage || !strings.Contains(err.Error(), "--status-quo") || !strings.Contains(err.Error(), "#7 without its full URL") {
		t.Fatalf("an incomplete note for the person: %v", err)
	}
	if err := addNote(a, "--for", "Supervisor", "approve #7"); err != nil {
		t.Fatalf("a note for another name is not checked: %v", err)
	}
	if err := addNote(a, "a memo on #7"); err != nil {
		t.Fatalf("a memo is not checked: %v", err)
	}
	if err := addNote(a, "--for", notePerson, "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "approve "+prURL); err != nil {
		t.Fatalf("a complete note: %v", err)
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 3 || st.Notes[2].Text != "approve "+prURL+" Status quo: "+sqNow+". Why: "+whyNow+"." {
		t.Fatalf("notes %+v", st.Notes)
	}
	if !strings.HasSuffix(out.String(), "note #3\n") {
		t.Fatalf("printed %q", out.String())
	}
}

func TestNoteOnTheSameRefAndVerbFolds(t *testing.T) {
	a, out := noteApp(t)
	args := []string{"--for", notePerson, "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK}
	if err := addNote(a, append(args, "approve "+prURL)...); err != nil {
		t.Fatal(err)
	}
	a.as = "Agent two"
	if err := addNote(a, append(args, "Approve the release, "+strings.ToUpper(prURL[:8])+prURL[8:])...); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, append(args, "review "+prURL)...); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 2 || !strings.Contains(st.Notes[0].Text, "| Also from Agent two: Approve the release") {
		t.Fatalf("the second approve folds, the review does not: %+v", st.Notes)
	}
	if !strings.Contains(out.String(), "note #1 (folded") {
		t.Fatalf("printed %q", out.String())
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "note.folded" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "into #1 (o/r#7, approve)") {
		t.Fatalf("events %+v, %v", evs, err)
	}
}

func TestLoginNoteClosesOnceItsProbePasses(t *testing.T) {
	st := &state.State{Notes: []state.Note{
		{ID: 1, Kind: noteLogin, Until: "true", Text: "sign in"},
		{ID: 2, Kind: noteLogin, Until: "false", Text: "sign in elsewhere"},
		{ID: 3, Text: "no probe"},
	}}
	passed := probeLogins(context.Background(), st.Notes)
	if !slices.Equal(passed, []int{1}) {
		t.Fatalf("passed %v", passed)
	}
	lines, evs := closeProbed(st, append(passed, 3))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "NOTE CLOSED: #1") || len(evs) != 1 || evs[0].Verb != "note.done" {
		t.Fatalf("lines %q, events %+v", lines, evs)
	}
	if len(st.Notes) != 2 || st.Notes[0].ID != 2 {
		t.Fatalf("open %+v", st.Notes)
	}
}
