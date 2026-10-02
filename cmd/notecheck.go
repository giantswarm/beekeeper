package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The kinds of a note. A decision waits on a person, who answers it by its
// due time or lets its default run; a memo is a session's own record (a
// state summary, a board skip, a list of deferred work) that asks nobody;
// a login asks its person to sign in, and the watch closes it once its
// --until probe passes.
const (
	noteDecision = "decision"
	noteMemo     = "memo"
	noteLogin    = "login"
)

// noteKinds are the kinds `note add --kind` takes.
var noteKinds = []string{noteDecision, noteMemo, noteLogin}

// noteKind is n's kind: the one it was filed with, else, for a note filed
// without one, a decision when it is for person (guides) and not pinned (a
// standing instruction), a memo otherwise.
func noteKind(person string, n *state.Note) string {
	if slices.Contains(noteKinds, n.Kind) {
		return n.Kind
	}
	if !n.Pinned && guides(person, n) {
		return noteDecision
	}
	return noteMemo
}

// decides says whether n waits on its person: a decision or a sign-in.
func decides(person string, n *state.Note) bool { return noteKind(person, n) != noteMemo }

// probeTimeout bounds one run of a probe: a login note's, a timer's.
const probeTimeout = 10 * time.Second

// noteDraft is what `note add` was given for a person: the question and the
// parts the person needs to answer it without asking back.
type noteDraft struct {
	Question, StatusQuo, Why, Default, Due, Checked, Kind, Until string
	Options                                                      []string
}

var (
	// shortRef is an issue or PR named as #N or owner/repo#N; "note #N"
	// and "timer #N" name beekeeper's own items.
	shortRef = regexp.MustCompile(`(?i)(?:^|[^\w/#])((?:note|timer)\s+)?((?:([\w.-]+/[\w.-]+))?#(\d+))\b`)
	// refURL is the full URL of an issue or PR.
	refURL = regexp.MustCompile(`(?i)https?://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)`)
	// pullURL is the full URL of a pull request.
	pullURL = regexp.MustCompile(`(?i)https?://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)
	// stateClaim is a claim about a state the person cannot see from the
	// note: it needs where it was checked.
	stateClaim = regexp.MustCompile(`(?i)\b(merged|green|released|rolled|closed)\b`)
	// noAction is a default that does nothing.
	noAction = regexp.MustCompile(`(?i)^(|-|wait\b.*|waiting\b.*|keep waiting\b.*|none|nothing( happens)?|no default|n/?a|tbd|unknown|ask( again)?|it waits)$`)
)

// text is the note's one-line text: the question, then its status quo,
// why, options and where its claims were checked.
func (d noteDraft) text() string {
	parts := []string{d.Question}
	add := func(label, v string) {
		if v != "" {
			parts = append(parts, label+": "+strings.TrimRight(v, ". ")+".")
		}
	}
	add("Status quo", d.StatusQuo)
	add("Why", d.Why)
	for _, o := range d.Options {
		add("Option", o)
	}
	add("Checked", d.Checked)
	return strings.Join(parts, " ")
}

// asking is a question that asks the person for something: a request
// opened by one of these verbs.
var asking = regexp.MustCompile(`(?i)^(please\s+)?(approve|decide|choose|pick|confirm|answer|review|merge|close|allow|grant|enable|disable|switch|sign|run|restart|delete|remove|rotate|reply|say|tell|give|set|add|accept|reject|unblock|check)\b`)

// asks reports whether d asks its person something: a question mark, an
// --option to choose, or a request verb opening it. Status text asks
// nothing: it goes to `beekeeper log add`.
func (d noteDraft) asks() bool {
	q := strings.TrimSpace(d.Question)
	return strings.Contains(q, "?") || len(d.Options) > 0 || asking.MatchString(q)
}

// missing names what d lacks for its person to answer it, one part each:
// what any question lacks, what happens unanswered, and a login's probe.
func (d noteDraft) missing() []string {
	out := append(d.lacks(), d.unanswered()...)
	switch {
	case d.Kind == noteLogin && strings.TrimSpace(d.Until) == "":
		out = append(out, `--until "<probe command that exits 0 once signed in>"`)
	case d.Kind != noteLogin && d.Until != "":
		out = append(out, "--until without --kind login")
	}
	return out
}

// unanswered names what d lacks for when nobody answers: a default that
// is an action and, for a decision, its due time.
func (d noteDraft) unanswered() []string {
	var out []string
	if dflt := strings.Trim(strings.TrimSpace(d.Default), ".!"); noAction.MatchString(dflt) {
		out = append(out, fmt.Sprintf("--default %q is no action: name what happens unanswered", d.Default))
	}
	if d.Kind == noteDecision && strings.TrimSpace(d.Due) == "" {
		out = append(out, `--due "<when the default runs: 22:55 or 3h>"`)
	}
	return out
}

// lacks names what the question d puts to its person lacks, a note's or
// the guide's own (AskUserQuestion): that it asks, the status quo, the why,
// every option's consequence, the full URL of every issue and PR, and where
// a state claim was checked.
func (d noteDraft) lacks() []string {
	var out []string
	if !d.asks() {
		out = append(out, "a question: it asks nothing (no ?, no --option, no request verb); a status line goes to `beekeeper log add \"<text>\"`")
	}
	if strings.TrimSpace(d.StatusQuo) == "" {
		out = append(out, `--status-quo "<what is true now>"`)
	}
	if strings.TrimSpace(d.Why) == "" {
		out = append(out, `--why "<why it needs the person>"`)
	}
	for _, o := range d.Options {
		if label, cons, ok := strings.Cut(o, ":"); !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(cons) == "" {
			out = append(out, fmt.Sprintf("--option %q has no \": <consequence>\"", o))
		}
	}
	all := d.text() + " " + d.Default
	for _, r := range unlinked(all) {
		out = append(out, r+" without its full URL")
	}
	if d.Checked == "" {
		claims := stateClaim.FindAllString(d.Question+" "+d.StatusQuo+" "+d.Why, -1)
		if len(claims) > 0 {
			out = append(out, fmt.Sprintf("%q without --checked \"<where it was checked>\"", strings.ToLower(claims[0])))
		}
	}
	return out
}

// notesGH is the gh CLI the plan stage check reads through; a seam for
// tests.
var notesGH github.GH = github.RunGH

// planStages names, for every open pull request of a plans repository that
// s links, its stage check that is not green: a plan revision goes to the
// person once its stage outputs are in. A pull request GitHub does not
// answer for is an error, never a pass.
func planStages(ctx context.Context, plans config.Plans, s string) ([]string, error) {
	var out []string
	var seen []string
	for _, m := range pullURL.FindAllStringSubmatch(s, -1) {
		ref := strings.ToLower(m[1]) + "#" + m[2]
		if !plans.Covers(m[1]) || slices.Contains(seen, ref) {
			continue
		}
		seen = append(seen, ref)
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return nil, err
		}
		c, err := github.PullCheck(ctx, notesGH, m[1], n, plans.Check)
		if err != nil {
			return nil, fmt.Errorf("%s: reading its %s check: %w", m[0], plans.Check, err)
		}
		if c.State != github.Open || c.Outcome == github.CheckPass {
			continue
		}
		why := c.Outcome
		switch c.Outcome {
		case github.CheckMissing:
			why = "none on its head"
		case github.CheckFail:
			why = strings.Join(append([]string{"red"}, c.Annotations...), ": ")
		}
		if c.URL != "" {
			why += ", " + c.URL
		}
		out = append(out, fmt.Sprintf("a green %s check on %s (%s): the revision's stage outputs first", plans.Check, m[0], why))
	}
	return out, nil
}

// unlinked lists the issue and PR references in s that have no full URL
// in s: owner/repo#N needs that repository's URL, #N any URL ending in N.
func unlinked(s string) []string {
	urls := refURL.FindAllStringSubmatch(s, -1)
	var out []string
	for _, m := range shortRef.FindAllStringSubmatch(s, -1) {
		if m[1] != "" {
			continue // note #N, timer #N
		}
		linked := slices.ContainsFunc(urls, func(u []string) bool {
			return u[2] == m[4] && (m[3] == "" || strings.EqualFold(u[1], m[3]))
		})
		if !linked && !slices.Contains(out, m[2]) {
			out = append(out, m[2])
		}
	}
	return out
}

// refs are the issues and PRs s links, as owner/repo#N in lower case.
func refs(s string) []string {
	var out []string
	for _, m := range refURL.FindAllStringSubmatch(s, -1) {
		r := strings.ToLower(m[1]) + "#" + m[2]
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// verb is what a note asks for: the first word of its question.
func verb(question string) string {
	f := strings.Fields(question)
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(strings.Trim(f[0], ".,:;!?\"'`"))
}

// foldTarget is the open note for the same person that asks the same verb
// on one of the same issues or PRs as n, or nil.
func foldTarget(notes []state.Note, n state.Note, question string) (*state.Note, string) {
	v, rs := verb(question), refs(n.Text)
	if v == "" || len(rs) == 0 {
		return nil, ""
	}
	for i := range notes {
		o := &notes[i]
		if !strings.EqualFold(o.For, n.For) || verb(o.Text) != v {
			continue
		}
		for _, r := range rs {
			if slices.Contains(refs(o.Text), r) {
				return o, r
			}
		}
	}
	return nil, ""
}

// probeLogins runs the probe of every open login note and returns the
// notes whose probe exits 0.
func probeLogins(ctx context.Context, notes []state.Note) []int {
	var passed []int
	for _, n := range notes {
		if n.Kind != noteLogin || n.Until == "" {
			continue
		}
		if probePasses(ctx, n.Until) {
			passed = append(passed, n.ID)
		}
	}
	return passed
}

// probePasses reports whether a probe, a shell command given on this
// machine, exits 0 within probeTimeout.
func probePasses(ctx context.Context, probe string) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return exec.CommandContext(pctx, "sh", "-c", probe).Run() == nil //nolint:gosec // the probe its filer gave on this machine
}

// closeProbed closes the open login notes among passed and returns their
// watch lines and events.
func closeProbed(st *state.State, passed []int) ([]string, []state.Event) {
	var lines []string
	var evs []state.Event
	st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
		if n.Kind != noteLogin || !slices.Contains(passed, n.ID) {
			return false
		}
		lines = append(lines, fmt.Sprintf("NOTE CLOSED: #%d, its probe passed: %s", n.ID, truncate(n.Text, 200)))
		evs = append(evs, event(watchParty, noteDone, "#%d closed, its probe passed (%s): %s", n.ID, n.Until, n.Text))
		return true
	})
	return lines, evs
}
