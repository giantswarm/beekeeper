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
// due time or lets its default run; a login asks its person to sign in, and
// the watch closes it once its --until probe passes.
const (
	noteDecision = state.NoteDecision
	noteLogin    = state.NoteLogin
)

// noteKinds are the kinds `note add --kind` takes.
var noteKinds = []string{noteDecision, noteLogin}

// probeTimeout bounds one run of a probe: a login note's, a timer's.
const probeTimeout = 10 * time.Second

// noteDraft is what `note add` was given for a person: the question and the
// parts the person needs to answer it without asking back.
type noteDraft struct {
	Question, StatusQuo, Why, Default, Due, Checked, Kind, Until string
	Options                                                      []string
	// Recommend is the recommended option, 1-based; 0 recommends none.
	Recommend int
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
	noAction = regexp.MustCompile(`(?i)^(|-|waits?\b.*|waiting\b.*|keep waiting\b.*|none|nothing( happens)?|no default|n/?a|tbd|unknown|ask( again)?|it waits)$`)
)

// isAction reports whether a default does something when it applies.
func isAction(dflt string) bool {
	return !noAction.MatchString(strings.Trim(strings.TrimSpace(dflt), ".!"))
}

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
	if !isAction(d.Default) {
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

// missingStage is a stage check's finding that a plan revision's stage
// output is not in yet ("<plan>: <stage> missing: ..."): the plan is at
// its grill stage, the revision waiting for its passes or sent back by a
// SIMPLIFY or KILL verdict.
var missingStage = regexp.MustCompile(`^[^\s:]+: [^\s:]+ missing: `)

// subject is the part of d's text its stage check reads: the pull requests
// the question asks about. A question that links none leaves the first
// pull request the rest links; the others the note only cites as context.
func (d noteDraft) subject() string {
	if pullURL.MatchString(d.Question) {
		return d.Question
	}
	return pullURL.FindString(d.text())
}

// planStages checks every pull request of a plans repository that s links
// by its stage check. A merged one, a green check, or a red one whose every
// finding is a missing stage output passes: the last is the grill stage,
// whose output is questions for the person, and is named in grill. refused
// names the rest: a closed pull request, a check pending or missing on the
// head, a red one with another finding (the PRD fails its own check). A
// pull request GitHub does not answer for is an error, never a pass.
func planStages(ctx context.Context, plans config.Plans, s string) (refused, grill []string, err error) {
	var seen []string
	for _, m := range pullURL.FindAllStringSubmatch(s, -1) {
		ref := strings.ToLower(m[1]) + "#" + m[2]
		if !plans.Covers(m[1]) || slices.Contains(seen, ref) {
			continue
		}
		seen = append(seen, ref)
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return nil, nil, err
		}
		c, err := github.PullCheck(ctx, notesGH, m[1], n, plans.Check)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: reading its %s check: %w", m[0], plans.Check, err)
		}
		at := ""
		if c.URL != "" {
			at = ", " + c.URL
		}
		const admits = "green, or red only for missing stage outputs (the grill stage), admits the note"
		switch {
		case c.State == github.Merged || (c.State == github.Open && c.Outcome == github.CheckPass):
		case c.State != github.Open:
			refused = append(refused, fmt.Sprintf("an open plan pull request: %s is closed; reopened, its %s check is read again", m[0], plans.Check))
		case c.Outcome == github.CheckMissing:
			refused = append(refused, fmt.Sprintf("a %s check on %s (none on its head): a run on the head, %s", plans.Check, m[0], admits))
		case c.Outcome == github.CheckPending:
			refused = append(refused, fmt.Sprintf("a finished %s check on %s (pending%s): its outcome, %s", plans.Check, m[0], at, admits))
		case len(c.Annotations) > 0 && !slices.ContainsFunc(c.Annotations, func(a string) bool { return !missingStage.MatchString(a) }):
			grill = append(grill, fmt.Sprintf("%s is at its grill stage (%s red only for missing stage outputs: %s%s)", m[0], plans.Check, strings.Join(c.Annotations, "; "), at))
		default:
			why := "no finding"
			if len(c.Annotations) > 0 {
				why = strings.Join(c.Annotations, "; ")
			}
			refused = append(refused, fmt.Sprintf("a PRD that passes its %s check on %s (red at the PRD itself, not a missing stage output: %s%s): a fixed PRD, %s", plans.Check, m[0], why, at, admits))
		}
	}
	return refused, grill, nil
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

// sameQuestion normalises a question for the fold: case, spacing and the
// closing punctuation aside.
func sameQuestion(q string) string {
	return strings.TrimRight(strings.ToLower(strings.Join(strings.Fields(q), " ")), ".?!:; ")
}

// foldKeys are the issues and PRs a note is filed on, as owner/repo#N in
// lower case: its --ref, else the first URL of its status quo. URLs its
// question or status quo cite as context are no key.
func foldKeys(n *state.Note) []string {
	if len(n.Refs) > 0 {
		out := make([]string, len(n.Refs))
		for i, r := range n.Refs {
			out[i] = strings.ToLower(r)
		}
		return out
	}
	rs := refs(n.StatusQuo)
	return rs[:min(1, len(rs))]
}

// foldTarget is the open note for the same person that asks the same
// question (sameQuestion) on one of the same issues or PRs (foldKeys) as
// n, or nil.
func foldTarget(notes []state.Note, n *state.Note) (*state.Note, string) {
	q, keys := sameQuestion(n.Question), foldKeys(n)
	if q == "" || len(keys) == 0 {
		return nil, ""
	}
	for i := range notes {
		o := &notes[i]
		if !strings.EqualFold(o.For, n.For) || sameQuestion(o.Question) != q {
			continue
		}
		for _, k := range keys {
			if slices.Contains(foldKeys(o), k) {
				return o, k
			}
		}
	}
	return nil, ""
}

// foldConflict names what n would lose folded into o: a different
// --default or recommended option. Empty when the fold loses nothing.
func foldConflict(o, n *state.Note) []string {
	var out []string
	if sameQuestion(o.Default) != sameQuestion(n.Default) {
		out = append(out, fmt.Sprintf("--default %q, not %q", o.Default, n.Default))
	}
	if ro, rn := recommended(o), recommended(n); ro != rn {
		out = append(out, fmt.Sprintf("recommends %q, not %q", ro, rn))
	}
	return out
}

// recommended is the label of the option n recommends, or "".
func recommended(n *state.Note) string {
	if n.Recommend < 1 || n.Recommend > len(n.Options) {
		return ""
	}
	label, _, _ := strings.Cut(n.Options[n.Recommend-1], ":")
	return strings.ToLower(strings.TrimSpace(label))
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
