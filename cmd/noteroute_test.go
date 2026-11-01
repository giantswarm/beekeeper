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
const opReview = "review"

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
