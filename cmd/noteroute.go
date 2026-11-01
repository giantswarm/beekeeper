package cmd

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// noteRedirected is the event of a worker's decision note refused before
// it reached its person: an operational ask the supervisor loop owns, or an
// ask its thread or the thread's epic already settled. Its detail is
// "<worker>: <class>: <question>; <where it went>".
const noteRedirected = "note.redirected"

// opClass is a class of a worker's operational ask: what it is about, what
// in its question marks it, and where the ask goes instead of the person.
type opClass struct {
	name  string
	match *regexp.Regexp
	where string
}

// opClasses are the operational asks a worker's note may not put to a
// person, in the order they are tried: each is the supervisor loop's or
// the desk's own machinery, never a person's decision.
var opClasses = []opClass{
	{
		name:  "review",
		match: regexp.MustCompile(`(?i)^(please\s+)?review\b|\b(desk|independent) reviews?\b|\breviewers?\b|\b(arrange|start|request|trigger|staff|schedule|assign|kick off|get|need)\b[^.?!]{0,40}\breview`),
		where: `the supervisor loop starts the desk review when the worker parks on its pull request (beekeeper agents park --on <owner/repo#n> "desk review") and sends its findings or its go through the inbox`,
	},
	{
		name:  "ci",
		match: regexp.MustCompile(`(?i)\b(ci|runners?|github actions|workflow runs?|pipelines?)\b|\b(checks?|jobs?|builds?)\b[^.?!]{0,30}\b(queued|pending|stuck|hang(s|ing)?|red|fail(s|ed|ing)?)\b|\bre-?run\b[^.?!]{0,20}\b(checks?|jobs?|workflows?|builds?)\b`),
		where: "the supervisor loop reads the parked pull request's checks itself; a CI, runner or build outage is a barrier for the worker's report (--problem, naming the owning repository) while it carries on with independent work",
	},
	{
		name:  "disk",
		match: regexp.MustCompile(`(?i)\b(disk( space)?|free space|out of space|no space left|prune)\b`),
		where: "the supervisor prunes disk on its own; nothing waits on a person",
	},
	{
		name:  "capacity",
		match: regexp.MustCompile(`(?i)\b(capacity|rate.?limit(ed)?|worker slots?|(more|fewer|another) (workers?|sessions?)|weekly pace)\b`),
		where: "beekeeper capacity pick and the supervisor's pace decide every start; a rate limit waits for its reset",
	},
	{
		name:  "rollout",
		match: regexp.MustCompile(`(?i)\bfast-?forward\b|\b(main|master) checkout\b|\b(pull|update|sync|reset|roll)\b[^.?!]{0,60}\b(checkout|clone|working copy)\b|\binstall\b[^.?!]{0,30}\b(binary|release|build)\b`),
		where: "rolling a merged change out to a checkout or a machine is the supervisor's install after its release; the worker names it in its report",
	},
	{
		name:  "sessions",
		match: regexp.MustCompile(`(?i)\b(start|stop|restart|wake|kill|archive|spawn|end|resume|relay)\b[^.?!]{0,40}\b(sessions?|workers?|agents?|supervisor|guide)\b`),
		where: "the supervisor loop starts, wakes, relays and stops sessions; the worker parks or reports through beekeeper agents park and agents idle",
	},
}

// reserved marks a decision that needs the person's own authority, whatever
// operation it concerns: spending or cost, a purchase, contract or licence,
// consent, an account or credential, or something made public or touching
// production. Such a question is never an operational ask.
var reserved = regexp.MustCompile(`(?i)\b(spend(s|ing)?|spent|costs?|costly|pay(s|ing)?|paid|prices?|pricing|budgets?|purchase[sd]?|buy(ing)?|invoices?|billing|eur|usd|euros?|dollars?|per (month|year)|monthly|yearly|annual|licen[cs]es?|contracts?|consent(s|ing)?|legal|accounts?|credentials?|secrets?|publish(es|ing)?|public(ly)?|production|customers?)\b|[$€£]\s?\d`)

// operational is the class of d's question when it is an operational ask
// (opClasses), and whether it is one. A note that links a plans
// repository's pull request is the person's plan review, and a reserved
// question (spending, consent, accounts, anything public or in production)
// the person's own decision: neither is one.
func operational(d noteDraft, plans config.Plans) (opClass, bool) {
	for _, m := range pullURL.FindAllStringSubmatch(d.text(), -1) {
		if plans.Covers(m[1]) {
			return opClass{}, false
		}
	}
	if reserved.MatchString(d.Question) {
		return opClass{}, false
	}
	for _, c := range opClasses {
		if c.match.MatchString(d.Question) {
			return c, true
		}
	}
	return opClass{}, false
}

// noteThreads reads the threads a note links (github.Threads); a seam for
// the tests.
var noteThreads = func(ctx context.Context, refs []github.PR) (map[github.PR]github.Thread, error) {
	return github.Threads(ctx, notesGH, refs)
}

// settledAsk names why a note on linked asks what is settled: a thread
// closed or merged, or an issue whose parent epic is closed. Empty: none
// is. A thread GitHub does not answer is warned about on stderr, never a
// refusal: a person's decision is not held back by a failed read.
func settledAsk(ctx context.Context, stderr io.Writer, linked []string) []string {
	var refs []github.PR
	for _, s := range linked {
		if r, ok := parseRef(s); ok {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	threads, err := noteThreads(ctx, refs)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: reading the note's threads: %v; filed without the supersede check for the threads not read\n", err)
	}
	var out []string
	for _, r := range refs {
		t, ok := threads[r]
		if !ok {
			continue
		}
		switch {
		case t.State == github.Merged || t.State == github.Closed:
			out = append(out, fmt.Sprintf("%s is %s", refName(r), strings.ToLower(t.State)))
		case t.Parent != "" && t.ParentState == github.Closed:
			out = append(out, fmt.Sprintf("the epic of %s, %s, is closed", refName(r), t.Parent))
		}
	}
	return out
}

// routeWorkerNote refuses a worker's decision before it reaches its
// person when it is an operational ask or a superseded one, logging
// note.redirected and naming where the ask went so the worker does not
// file it again. Nil: the note is the person's to decide.
func (a *app) routeWorkerNote(ctx context.Context, stderr io.Writer, me state.Party, d noteDraft, linked []string) error {
	q := truncate(strings.TrimSpace(d.Question), 200)
	var why string
	if c, ok := operational(d, a.cfg.Plans); ok {
		why = fmt.Sprintf("a %s ask is operational, never a person's decision: %s", c.name, c.where)
		a.logRedirect(stderr, me, c.name, q, c.where)
		return refused("note refused: %s. Do not file it again", why)
	}
	if s := settledAsk(ctx, stderr, linked); len(s) > 0 {
		where := "nobody: its ask is settled; take the next step from the board (beekeeper board next) or report the task over (beekeeper agents idle --done)"
		a.logRedirect(stderr, me, "superseded", q, strings.Join(s, "; "))
		return refused("note refused: it is superseded (%s); it goes to %s. Do not file it again", strings.Join(s, "; "), where)
	}
	return nil
}

// logRedirect logs a worker's refused note; a failed write is named on
// stderr and refuses nothing more.
func (a *app) logRedirect(stderr io.Writer, me state.Party, class, question, where string) {
	if a.managedExecutionID() != "" {
		return // another host's worker: its coordinator keeps the log
	}
	e := event(me, noteRedirected, "%s: %s: %s; %s", me.Name, class, question, where)
	if err := a.record(e); err != nil {
		_, _ = fmt.Fprintln(stderr, notLogged(e, err))
	}
}

// isWorker reports whether me is a worker on the roster, or a worker on
// another host (a managed execution).
func (a *app) isWorker(me state.Party) (bool, error) {
	if a.managedExecutionID() != "" {
		return true, nil
	}
	st, err := a.store.Read()
	if err != nil {
		return false, err
	}
	return rosterRole(st, me) == workerRole, nil
}
