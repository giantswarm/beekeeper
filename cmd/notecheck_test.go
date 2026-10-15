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
		"no status quo":      {func(d *noteDraft) { d.StatusQuo = "" }, flagStatusQuo},
		"no why":             {func(d *noteDraft) { d.Why = " " }, flagWhy},
		"option, no effect":  {func(d *noteDraft) { d.Options = []string{"publish"} }, `--option "publish" has no`},
		"default wait":       {func(d *noteDraft) { d.Default = "Wait." }, `--default "Wait." is no action`},
		"default nothing":    {func(d *noteDraft) { d.Default = "nothing" }, "is no action"},
		"no default":         {func(d *noteDraft) { d.Default = "" }, "is no action"},
		"claim unchecked":    {func(d *noteDraft) { d.StatusQuo = "CI is green" }, `"green" without --checked`},
		"login, no probe":    {func(d *noteDraft) { d.Kind = noteLogin }, untilFlag},
		"probe, no login":    {func(d *noteDraft) { d.Until = probeHolds }, "--until without --kind login"},
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
	if err := addNote(a, "--for", notePerson, "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "--due", "3h", "--ref", prURL, "approve "+prURL); err != nil {
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

func TestNoteFoldsOnlyTheSameQuestion(t *testing.T) {
	a, out := noteApp(t)
	args := append(slices.Clone(decisionArgs), "--ref", prURL)
	if err := addNote(a, append(args, "Approve the release "+prURL+"?")...); err != nil {
		t.Fatal(err)
	}
	a.as = "Agent two"
	// The same question, case, spacing and punctuation aside: it folds.
	if err := addNote(a, append(args, "approve  the release "+strings.ToUpper(prURL[:8])+prURL[8:]+"?!")...); err != nil {
		t.Fatal(err)
	}
	// The same first word, the same PR, another question: its own note.
	if err := addNote(a, append(args, "Approve the changelog of "+prURL+"?")...); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 2 || !strings.Contains(st.Notes[0].Text, "| Also from Agent two: approve  the release") {
		t.Fatalf("the restated question folds, the other one does not: %+v", st.Notes)
	}
	if !strings.Contains(out.String(), "note #1 (folded: it asks the same question") || !strings.HasSuffix(out.String(), "note #2\n") {
		t.Fatalf("printed %q", out.String())
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "note.folded" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "into #1 (o/r#7, the same question)") {
		t.Fatalf("events %+v, %v", evs, err)
	}
}

func TestNoteWithAnotherDefaultIsRefusedOrFoldedExplicitly(t *testing.T) {
	a, out := noteApp(t)
	base := []string{flagFor, notePerson, flagStatusQuo, sqNow, flagWhy, whyNow, flagDue, "3h", "--ref", prURL}
	if err := addNote(a, append(base, "--default", dfltOK, "Approve the release?")...); err != nil {
		t.Fatal(err)
	}
	other := append(slices.Clone(base), "--default", "the release ships tomorrow", "approve the release")
	err := addNote(a, other...)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "note #1 for Pat asks the same question on o/r#7") ||
		!strings.Contains(err.Error(), "--fold-into 1") || !strings.Contains(err.Error(), `--default "`+dfltOK+`"`) {
		t.Fatalf("the same question with another default: %v", err)
	}
	if err := addNote(a, append([]string{"--fold-into", "1"}, other...)...); err != nil {
		t.Fatalf("an explicit fold: %v", err)
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || st.Notes[0].Default != dfltOK || !strings.Contains(out.String(), "note #1 (folded: --fold-into") {
		t.Fatalf("folded explicitly, the open note's default stands: %+v, %q", st.Notes, out.String())
	}
	if err := addNote(a, "--fold-into", "1", flagFor, "Supervisor", "a memo"); Code(err) != ExitRefused {
		t.Fatalf("a fold into another person's note: %v", err)
	}
	if err := addNote(a, "--fold-into", "9", "a memo"); Code(err) != ExitRefused {
		t.Fatalf("a fold into no open note: %v", err)
	}
	if err := addNote(a, "--fold-into", "1", "--replaces", "1", "a memo"); Code(err) != ExitUsage {
		t.Fatalf("--fold-into with --replaces: %v", err)
	}
}

func TestNoteFoldKeysOnItsRefNotOnCitedURLs(t *testing.T) {
	a, _ := noteApp(t)
	plan, tools := "https://github.com/o/plans/pull/136", "https://github.com/o/tools/pull/9"
	args := []string{flagFor, notePerson, flagWhy, whyNow, flagDue, "3h"}
	// Two questions opening with "Does", one citing the other's PR as
	// context: their refs differ, so two notes.
	if err := addNote(a, append(args, flagStatusQuo, "the toolchain "+tools+" waits", "--default", dfltOK, "--ref", tools, "Does the toolchain ship Go 1.26?")...); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, append(args, flagStatusQuo, "the plan "+plan+" cites "+tools, "--default", "the plan keeps its testing section", "--ref", plan, "Does the testing plan of "+plan+" cover "+tools+"?")...); err != nil {
		t.Fatal(err)
	}
	// Two different questions on one plan PR, each with its own default:
	// two notes.
	for i, q := range []string{"Does the plan keep its stages?", "Does the plan name its owner?"} {
		if err := addNote(a, append(args, flagStatusQuo, "the plan "+plan+" is open", "--default", "default "+string(rune('A'+i)), "--ref", plan, q)...); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 4 {
		t.Fatalf("four distinct questions stay four notes: %+v", st.Notes)
	}
	for i, d := range []string{dfltOK, "the plan keeps its testing section", "default A", "default B"} {
		if st.Notes[i].Default != d {
			t.Errorf("note #%d default %q, want %q", st.Notes[i].ID, st.Notes[i].Default, d)
		}
	}
}

func TestLoginNoteClosesOnceItsProbePasses(t *testing.T) {
	st := &state.State{Notes: []state.Note{
		{ID: 1, Kind: noteLogin, Until: probeHolds, Text: "sign in"},
		{ID: 2, Kind: noteLogin, Until: probeFails, Text: "sign in elsewhere"},
		{ID: 3, Text: "no probe"},
	}}
	passed := probeLogins(context.Background(), st.Notes)
	if !slices.Equal(passed, []int{1}) {
		t.Fatalf("passed %v", passed)
	}
	lines, evs := closeProbed(st, append(passed, 3))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "NOTE CLOSED: #1") || len(evs) != 1 || evs[0].Verb != noteDone {
		t.Fatalf("lines %q, events %+v", lines, evs)
	}
	if len(st.Notes) != 2 || st.Notes[0].ID != 2 {
		t.Fatalf("open %+v", st.Notes)
	}
}

func TestNoteOnAPlanPullNeedsItsStageCheckGreenOrAtTheGrillStage(t *testing.T) {
	red := func(job string) string {
		return `{"state":"OPEN","statusCheckRollup":[{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/plans/actions/runs/1/job/` + job + `"}]}`
	}
	rollups := map[string]string{
		"1": `{"state":"OPEN","statusCheckRollup":[]}`,
		"2": red("2"),
		"3": `{"state":"OPEN","statusCheckRollup":[{"__typename":"CheckRun","name":"plan-stages","status":"COMPLETED","conclusion":"SUCCESS"}]}`,
		"4": `{"state":"MERGED","statusCheckRollup":[]}`,
		"5": red("5"),
		"6": red("6"),
		"7": `{"state":"OPEN","statusCheckRollup":[{"__typename":"CheckRun","name":"plan-stages","status":"IN_PROGRESS","detailsUrl":"https://github.com/o/plans/actions/runs/1/job/7"}]}`,
		"8": `{"state":"CLOSED","statusCheckRollup":[]}`,
	}
	annotations := map[string]string{
		"2": `["p: index.html missing: the last p/index.html (2433d45) is older than the PRD's last change (350fb6c)","p: contrarian missing: this pull request changes p/PRD.md and no p/contrarian"]`,
		"5": `["p: index.html missing: no p/index.html","p: PRD.md has no ### Slices table"]`,
		"6": `[]`,
	}
	var calls []string
	prev := notesGH
	t.Cleanup(func() { notesGH = prev })
	notesGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "api" {
			return []byte(annotations[strings.Split(args[1], "/")[4]]), nil
		}
		return []byte(rollups[args[2]]), nil
	}
	a, out := noteApp(t)
	a.cfg.Plans = config.Plans{Repositories: []string{"O/Plans"}, Check: config.DefaultPlansCheck}
	add := func(url string) error {
		return addNote(a, "--for", notePerson, "--status-quo", sqNow, "--why", whyNow, "--default", dfltOK, "--due", "3h", "--ref", url, "decide the grill question on "+url+"?")
	}
	plan := "https://github.com/o/plans/pull/"
	refusals := map[string]string{
		"1": "a plan-stages check on " + plan + "1 (none on its head): a run on the head, green, or red only for missing stage outputs (the grill stage), admits the note",
		"5": "a PRD that passes its plan-stages check on " + plan + "5 (red at the PRD itself, not a missing stage output: p: index.html missing: no p/index.html; p: PRD.md has no ### Slices table, https://github.com/o/plans/actions/runs/1/job/5): a fixed PRD,",
		"6": "(red at the PRD itself, not a missing stage output: no finding, https://github.com/o/plans/actions/runs/1/job/6)",
		"7": "a finished plan-stages check on " + plan + "7 (pending, https://github.com/o/plans/actions/runs/1/job/7): its outcome,",
		"8": "an open plan pull request: " + plan + "8 is closed; reopened, its plan-stages check is read again",
	}
	for n, want := range refusals {
		if err := add(plan + n); Code(err) != ExitUsage || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: refused naming the stage and what admits it: %v", n, err)
		}
	}
	for _, url := range []string{plan + "2", plan + "3", plan + "4", prURL} {
		if err := add(url); err != nil {
			t.Fatalf("%s: the grill stage, a green check, a merged plan or another repository: %v", url, err)
		}
	}
	if want := "admitted: " + plan + "2 is at its grill stage (plan-stages red only for missing stage outputs: p: index.html missing:"; !strings.Contains(out.String(), want) {
		t.Fatalf("the admitted grill stage is named: %q", out.String())
	}
	about := func(subject, context string) error {
		return addNote(a, "--for", notePerson, "--status-quo", sqNow, "--why", whyNow+" See "+context+".", "--default", dfltOK, "--due", "3h", "--ref", subject, "decide the grill question on "+subject+"?")
	}
	if err := about(plan+"3", plan+"5"); err != nil {
		t.Fatalf("a note about a green plan that only mentions a red one is admitted: %v", err)
	}
	if err := about(plan+"5", plan+"3"); Code(err) != ExitUsage || !strings.Contains(err.Error(), "a PRD that passes its plan-stages check on "+plan+"5") {
		t.Fatalf("a note about a red plan is refused: %v", err)
	}
	if err := addNote(a, "a memo on "+plan+"1"); err != nil {
		t.Fatalf("a memo is not checked: %v", err)
	}
	if len(calls) != 14 {
		t.Fatalf("one read per plans pull request and one per red check's annotations: %q", calls)
	}
}
