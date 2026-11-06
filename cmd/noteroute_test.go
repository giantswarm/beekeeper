package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// workerNoteApp is noteApp whose caller is a worker on the roster, its
// note threads answered from threads (owner/repo#n: thread); a thread
// missing from it is not answered.
func workerNoteApp(t *testing.T, threads map[string]github.Thread, readErr error) *app {
	t.Helper()
	a, _ := noteApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: agentOne}, Task: "work https://github.com/o/r/issues/7"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	prev := noteThreads
	t.Cleanup(func() { noteThreads = prev })
	noteThreads = func(_ context.Context, rs []github.PR) (map[github.PR]github.Thread, error) {
		m := map[github.PR]github.Thread{}
		for _, r := range rs {
			if th, ok := threads[refName(r)]; ok {
				m[r] = th
			}
		}
		return m, readErr
	}
	return a
}

// redirected are the note.redirected events of a's log.
func redirected(t *testing.T, a *app) []state.Event {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == noteRedirected })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// opReview is the class of a review ask.
const (
	opReview = "review"
	opDefect = "defect"
)

// openThread is decisionRef open, its epic open.
var openThread = map[string]github.Thread{decisionRef: {State: github.Open, Parent: refOne, ParentState: github.Open}}

// The asks of https://github.com/giantswarm/beekeeper/issues/1900, and
// one more of each class: a worker's operational ask never becomes a note
// for the person; the refusal names where it went and is logged.
func TestWorkerOperationalAskIsRefusedNamingWhereItWent(t *testing.T) {
	for _, c := range []struct{ class, question string }{
		{opReview, "Please arrange desk review when the PR is parked?"},
		{opReview, "Can someone start an independent review of " + prURL + "?"},
		{"ci", "Repository CI jobs remain queued without steps; please restore runner availability?"},
		{"ci", "Re-run the failed checks on " + prURL + "?"},
		{"disk", "The disk is almost full: may I prune the build caches?"},
		{"capacity", "GitHub rate limit exceeded: shall I wait for the reset?"},
		{"rollout", "Shall the guide fast-forward the main checkout so the merged change goes live?"},
		{"rollout", "Install the new binary of the release on this machine?"},
		{"sessions", "Restart the worker session that stopped mid-task?"},
		{opDefect, "Please track the guard panic as a defect?"},
		{opDefect, "Who should fix the red main of " + prURL + "?"},
		{opDefect, "File an issue for the crash in the wake hook?"},
		{"publication", "Can the coordinator fix SSH publication for " + prURL + " so the lint fix can be pushed?"},
		{"publication", "How should commit 937e7b9 reach " + prURL + " while the coordinator refuses its publication?"},
	} {
		t.Run(c.question, func(t *testing.T) {
			a := workerNoteApp(t, openThread, nil)
			err := addNote(a, append(slices.Clone(decisionArgs), c.question)...)
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), "a "+c.class+" ask is operational") || !strings.Contains(err.Error(), "Do not file it again") {
				t.Fatalf("refusal: %v", err)
			}
			want, _ := operational(noteDraft{Question: c.question}, config.Plans{})
			if !strings.Contains(err.Error(), want.where) {
				t.Errorf("the refusal names no destination: %v", err)
			}
			st, _ := a.store.Read()
			if len(st.Notes) != 0 {
				t.Errorf("filed: %+v", st.Notes)
			}
			evs := redirected(t, a)
			if len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, agentOne+": "+c.class+": ") {
				t.Errorf("logged %+v", evs)
			}
		})
	}
}

// A worker's ask whose thread is closed or merged, or whose epic is
// closed, is refused as superseded; a thread GitHub does not answer is
// filed with a warning.
func TestWorkerSupersededAskIsRefused(t *testing.T) {
	const ask = "Approve the deployment of " + prURL + "?"
	for name, c := range map[string]struct {
		threads map[string]github.Thread
		readErr error
		want    string
	}{
		"closed thread": {threads: map[string]github.Thread{decisionRef: {State: github.Closed}}, want: decisionRef + " is closed"},
		"merged thread": {threads: map[string]github.Thread{decisionRef: {State: github.Merged}}, want: decisionRef + " is merged"},
		"epic closed":   {threads: map[string]github.Thread{decisionRef: {State: github.Open, Parent: refOne, ParentState: github.Closed}}, want: "the epic of " + decisionRef + ", " + refOne + ", is closed"},
	} {
		t.Run(name, func(t *testing.T) {
			a := workerNoteApp(t, c.threads, c.readErr)
			err := addNote(a, append(slices.Clone(decisionArgs), ask)...)
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), "superseded ("+c.want+")") || !strings.Contains(err.Error(), "board next") {
				t.Fatalf("refusal: %v", err)
			}
			if evs := redirected(t, a); len(evs) != 1 || !strings.Contains(evs[0].Detail, ": superseded: ") {
				t.Errorf("logged %+v", evs)
			}
		})
	}
	t.Run("not read", func(t *testing.T) {
		a := workerNoteApp(t, nil, errors.New("rate limited"))
		if err := addNote(a, append(slices.Clone(decisionArgs), ask)...); err != nil {
			t.Fatalf("a failed read refuses: %v", err)
		}
		if out := a.out.(interface{ String() string }).String(); !strings.Contains(out, "warning: reading the note's threads: rate limited") {
			t.Errorf("no warning: %q", out)
		}
	})
}

// Whether to merge is never a person's decision: a note that asks it is
// refused from anyone, a worker or not, naming the loop that merges, and
// logged (https://github.com/giantswarm/beekeeper/issues/2207). A question
// that only mentions a merge reaches the person.
func TestMergeAskIsRefusedFromAnyone(t *testing.T) {
	for _, q := range []string{
		"Merge " + prURL + " at head 3333333 without a third review, or have it reworked?",
		"May I merge " + prURL + " now that the review approved it?",
		mergeIt,
		"Is " + prURL + " ok to merge?",
		"Approve the merge of " + decisionRef + "?",
		"Confirm merging " + prURL + " at head abc1234?",
		"Should the loop merge the pull request once CI is green?",
	} {
		t.Run(q, func(t *testing.T) {
			worker := workerNoteApp(t, openThread, nil)
			session, _ := noteApp(t)
			for name, a := range map[string]*app{workerRole: worker, "session": session} {
				err := addNote(a, append(slices.Clone(decisionArgs), q)...)
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), "whether to merge is never a person's decision") || !strings.Contains(err.Error(), "the loop merges it, asking nobody") {
					t.Fatalf("%s: refusal: %v", name, err)
				}
				st, _ := a.store.Read()
				if len(st.Notes) != 0 {
					t.Errorf("%s: filed: %+v", name, st.Notes)
				}
				if evs := redirected(t, a); len(evs) != 1 || !strings.Contains(evs[0].Detail, ": merge: ") {
					t.Errorf("%s: logged %+v", name, evs)
				}
			}
		})
	}
	a, _ := noteApp(t)
	for _, q := range []string{
		"Should merge lanes of o/r settle before a release rolls?",
		"Rebase onto main or merge main into the branch?",
		superloop.OwnershipQuestion(prURL, "team-x"),
		"Approve publishing the release " + prURL + " to the public registry?",
		"Merge the two Helm charts into one, or keep them separate?",
		"Merge main into the release branch or cut a new one?",
		"Do we merge the staging and production clusters?",
		"Allow squash merge only on o/r? It changes the repository settings.",
		"Confirm the merge of the two Slack channels?",
		"Should we merge https://github.com/o/r/issues/12 into https://github.com/o/r/issues/13 as duplicates?",
	} {
		if err := addNote(a, append(slices.Clone(decisionArgs), q)...); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// mergeIt asks to merge the pull request of the note's ref.
const mergeIt = "Can we merge it?"

// A merge of "it" is a pull request's merge only when the note's ref is
// a pull request: owner/repo#n or a pull request's URL, never an issue's.
func TestMergeOfItNeedsAPullRequestRef(t *testing.T) {
	for _, c := range []struct {
		q    string
		refs []string
		want bool
	}{
		{mergeIt, []string{decisionRef}, true},
		{"Is this ready to merge?", []string{prURL}, true},
		{mergeIt, []string{"https://github.com/o/r/issues/12"}, false},
		{mergeIt, nil, false},
		{"Merge the two Helm charts into one, or keep them separate?", []string{decisionRef}, false},
		{"Merge " + decisionRef + " now?", nil, true},
	} {
		if got := isMergeAsk(c.q, c.refs); got != c.want {
			t.Errorf("%q with %v: %v, want %v", c.q, c.refs, got, c.want)
		}
	}
}

// A review's words in a request for log output ask for no review: the note
// passes, while a real ask for a reviewer stays refused.
func TestLogRequestNamingAReviewerIsNoReviewAsk(t *testing.T) {
	for q, want := range map[string]bool{
		"Please send the coordinator log output of the reviewer start":   false,
		"What does the journal say about the reviewer's failed publish?": false,
		"Who is the reviewer of this change?":                            true,
		"Can you get a desk review for it?":                              true,
	} {
		c, ok := operational(noteDraft{Question: q}, config.Plans{})
		if got := ok && c.name == taskKindReview; got != want {
			t.Errorf("%q: review ask = %v (class %q), want %v", q, got, c.name, want)
		}
	}
}

// What is live, public or costly stays the person's: a worker's decision
// on an open thread of an open epic is filed, and so is an operational
// question from a session that is no worker.
func TestPersonsDecisionStillReachesThePerson(t *testing.T) {
	a := workerNoteApp(t, openThread, nil)
	for _, q := range []string{
		"Approve publishing the release " + prURL + " to the public registry?",
		"Rotate the production credential named in " + prURL + "?",
	} {
		if err := addNote(a, append(slices.Clone(decisionArgs), q)...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if len(redirected(t, a)) != 0 {
		t.Error("a person's decision is redirected")
	}
	b, _ := noteApp(t) // no worker: the guide's or a person's own session
	if err := addNote(b, append(slices.Clone(decisionArgs), "Please arrange desk review of "+prURL+"?")...); err != nil {
		t.Fatalf("a non-worker's note: %v", err)
	}
}

// A note on a plans repository's pull request asks for the person's plan
// review: never an operational review ask.
func TestPlanReviewIsNoOperationalAsk(t *testing.T) {
	d := noteDraft{Question: "Review the plan " + prURL + "?"}
	if _, ok := operational(d, config.Plans{Repositories: []string{scratchRepo}}); ok {
		t.Error("a plan review is operational")
	}
	if c, ok := operational(d, config.Plans{}); !ok || c.name != opReview {
		t.Errorf("a code review ask: %+v %v", c, ok)
	}
}

// A decision that needs the person's own authority stays theirs even when
// it concerns CI, disk, capacity or a review: spending, consent, accounts,
// anything public.
func TestReservedDecisionOnAnOperationalSubjectReachesThePerson(t *testing.T) {
	a := workerNoteApp(t, openThread, nil)
	for _, q := range []string{
		"Approve spending EUR 500 per month on additional CI runners?",
		"Buy a larger disk for the build machine at $120?",
		"Raise the capacity by adding another paid subscription?",
		"Consent to a third-party reviewer reading the private code?",
		"Make the CI logs public on " + prURL + "?",
	} {
		if err := addNote(a, append(slices.Clone(decisionArgs), q)...); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	if evs := redirected(t, a); len(evs) != 0 {
		t.Errorf("redirected: %+v", evs)
	}
}

// A fold puts the ask before the open note's person as surely as a note
// of its own: an operational or superseded ask is refused before it folds,
// and the open note is left as it was.
func TestWorkerAskIsClassifiedBeforeItFolds(t *testing.T) {
	for name, c := range map[string]struct {
		threads  map[string]github.Thread
		question string
		want     string
	}{
		"operational": {openThread, "Please arrange desk review when the PR is parked?", "a review ask is operational"},
		"superseded":  {map[string]github.Thread{decisionRef: {State: github.Closed}}, "Approve the deployment of " + prURL + "?", "superseded (" + decisionRef + " is closed)"},
	} {
		t.Run(name, func(t *testing.T) {
			a := workerNoteApp(t, c.threads, nil)
			const open = "Pick the release day?"
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.NextNote = 1
				st.Notes = append(st.Notes, state.Note{ID: 1, Kind: noteDecision, For: notePerson, Text: open, Question: open, Refs: []string{decisionRef}})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, fold := range [][]string{{flagFoldInto, "1"}, nil} {
				err := addNote(a, append(append(slices.Clone(decisionArgs), fold...), c.question)...)
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("fold %v: %v", fold, err)
				}
			}
			st, _ := a.store.Read()
			if len(st.Notes) != 1 || st.Notes[0].Text != open {
				t.Errorf("folded: %+v", st.Notes)
			}
		})
	}
}

// A worker's note whose options are "file this defect" or "who fixes it"
// is a problem report, whatever its question asks: it is refused naming
// agents problem, which the guide files, and never reaches the person.
func TestWorkerDefectOptionsAreAProblemReport(t *testing.T) {
	a := workerNoteApp(t, openThread, nil)
	args := append(slices.Clone(decisionArgs), "--option", "File it as a defect: the guide tracks the guard's panic",
		"--option", "Leave it: the guard keeps panicking", "How does the guard's panic on "+prURL+" go on?")
	err := addNote(a, args...)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "a defect ask is operational") || !strings.Contains(err.Error(), "beekeeper agents problem") {
		t.Fatalf("refusal: %v", err)
	}
	if st, _ := a.store.Read(); len(st.Notes) != 0 {
		t.Errorf("filed: %+v", st.Notes)
	}
	// Scope and product decisions are no defect asks: "issue" and
	// "failures" as bare nouns, or "open" and "report" as bare verbs, are
	// ordinary words that pair with them; only a defect noun makes the ask.
	for _, d := range []noteDraft{
		// The commonest worker decision: do it here, or in a follow-up issue.
		{Question: "Does the cleanup belong in this PR?", Options: []string{"Here: the PR grows by 200 lines", "Open a follow-up issue: the PR stays small"}},
		{Question: "Should the weekly report list failures by host?"},
		{Question: "Which of the open issues in the epic ships first?"},
		// A consequence that names a defect is no defect ask: only the choice is.
		{Question: "Which API shape ships?", Options: []string{"Keep v1: the old bug stays for one release", "Take v2: clients migrate"}},
	} {
		if c, ok := operational(d, config.Plans{}); ok {
			t.Errorf("a decision that names no defect ask is operational (%s): %+v", d.Question, c)
		}
	}
}
