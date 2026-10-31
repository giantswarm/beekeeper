// Package superloop is the supervisor's fixed procedure as a pure function:
// beekeeper's events, the roster and the tracked pull requests in, the
// transitions to take out. `beekeeper supervisor loop` gathers the input,
// runs each action through beekeeper's own commands and logs it under its
// key, which is what keeps a restarted loop from taking it twice.
package superloop

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// The kinds of action, each logged as "supervisor.<kind>".
const (
	KindReview   = "review"
	KindGo       = "go"
	KindForward  = "forward"
	KindEscalate = "escalate"
	// KindDone takes an agent off the roster: one that reported done, or
	// one the loop retires (a reviewer whose verdict it acted on, the
	// worker of a pull request it gave the go for that merged).
	KindDone     = "done"
	KindNext     = "next"
	KindRelease  = "release"
	KindClassify = "classify"
	KindQuestion = "question"
	// KindEnd ends the tracking of a pull request that merged or closed.
	KindEnd = "end"
)

// Verbs is every log verb the loop writes.
func Verbs() []string {
	return []string{KindReview, KindGo, KindForward, KindEscalate, KindDone, KindNext, KindRelease, KindClassify, KindQuestion, KindEnd}
}

// The verbs of the events the rules act on.
const (
	VerbDone    = "agents.done"
	VerbIdle    = "agents.idle"
	VerbReport  = "agents.report"
	VerbProblem = "agents.problem"
	VerbPark    = "agents.park"
)

// The verdicts of a review.
const (
	Approved         = "approved"
	ChangesRequested = "changes requested"
)

// The states of a pull request, as gh reports them.
const (
	Open   = "OPEN"
	Merged = "MERGED"
	Closed = "CLOSED"
)

// Pull is a tracked pull request as GitHub reports it.
type Pull struct {
	Ref   string // owner/repo#n
	URL   string
	State string
	Head  string
	// Green is every check on Head concluded successfully (none is not
	// green: they have not reported yet).
	Green   bool
	Reviews []Review // oldest first
}

// Review is one review of a pull request with a verdict.
type Review struct {
	Author  string
	URL     string
	Commit  string
	ID      string
	Head    string
	Verdict string
	Body    string
	At      time.Time
}

// verdictSlack is how much earlier than its round's start a verdict may
// be stamped: GitHub's clock and the machine's differ a little.
const verdictSlack = 30 * time.Second

// verdict is round rd's verdict: the latest review with one on the round's
// head submitted after the round started; nil while the round runs. A
// verdict of an earlier round, or of nobody's round, never counts.
func (p Pull) verdict(rd round) *Review {
	for i := len(p.Reviews) - 1; i >= 0; i-- {
		if r := p.Reviews[i]; r.Verdict != "" && sameHead(r.Head, rd.head) && !r.At.Before(rd.at.Add(-verdictSlack)) {
			return &p.Reviews[i]
		}
	}
	return nil
}

// Worker is an agent on the roster.
type Worker struct {
	Name string
	// SelfHosted is a worker whose model a self-hosted provider serves:
	// its reviewer runs on none of them.
	SelfHosted bool
	// ParkedOn is what the agent is parked on (owner/repo#n, #12, …), ""
	// when it is not parked; ParkedAt since when.
	ParkedOn string
	ParkedAt time.Time
	// Started is a session beekeeper started: only such a one the loop
	// retires; a session its person started stays.
	Started bool
}

// Event is one line of the event log the loop acts on.
type Event struct {
	At     time.Time
	Verb   string
	Agent  string // the party that logged it
	Detail string
}

// Note is an open note.
type Note struct {
	ID   int
	For  string
	By   string
	Text string
}

// Taken is an action the loop took, read back from its log line.
type Taken struct {
	Key   string
	Kind  string
	Ref   string
	Agent string
	At    time.Time
}

// Input is everything a pass plans from.
type Input struct {
	Workers []Worker
	// Pulls are the tracked pull requests (Tracked) by ref.
	Pulls map[string]Pull
	// Events are the log's events since the loop's first start.
	Events []Event
	Notes  []Note
	// Release is a newer release's version, "" when there is none.
	Release string
	// Taken are the actions already taken, oldest first.
	Taken []Taken
	// Rounds is supervisor.reviewRounds.
	Rounds int
	// Person is the guide's person.
	Person string
}

// Action is one transition to take.
type Action struct {
	Key   string
	Kind  string
	Ref   string // the pull request, when there is one
	URL   string
	Agent string // the worker it concerns
	Head  string
	Round int
	// SelfHosted asks for a reviewer on no self-hosted provider.
	SelfHosted bool
	// Message is what the worker or the guide is told.
	Message string
	// Event is the event a done, next or classify action acts on.
	Event *Event
	// Note is the note a question re-addresses.
	Note int
	// Version is the release to install.
	Version string
}

// Line is the action's log detail: its key first, then what it did.
func (a Action) Line(what string) string { return "[" + a.Key + "] " + what }

// ParseTaken reads a loop log line (verb "supervisor.<kind>", detail
// "[<key>] <agent>: …") back; false for another line.
func ParseTaken(at time.Time, verb, subject, detail string) (Taken, bool) {
	kind, ok := strings.CutPrefix(verb, "supervisor.")
	if !ok || !slices.Contains(Verbs(), kind) {
		return Taken{}, false
	}
	m := takenLine.FindStringSubmatch(detail)
	if m == nil {
		return Taken{}, false
	}
	return Taken{Key: m[1], Kind: kind, Ref: subject, Agent: m[2], At: at}, true
}

// takenLine is a loop log line's detail.
var takenLine = regexp.MustCompile(`^\[([^\]]+)\] (?:([^:]*): )?`)

// Detail is the log detail of action a taken for its agent.
func Detail(a Action, what string) string {
	if a.Agent == "" {
		return a.Line(what)
	}
	return a.Line(a.Agent + ": " + what)
}

// round is a review round the loop started: the head it reviews and when.
type round struct {
	head string
	at   time.Time
}

// ledger is what the taken actions say.
type ledger struct {
	keys map[string]Taken
	// rounds are the review rounds each pull request had, oldest first,
	// escalated the pull requests handed to the guide, ended the ones
	// merged or closed.
	rounds    map[string][]round
	escalated map[string]bool
	ended     map[string]bool
	reviewer  map[string]string // a pull request's worker, from its last review
	// goAgent is the worker the go for a pull request was given to; acts
	// are when the loop acted on a pull request (a review, go, forward,
	// escalation or end), oldest first.
	goAgent map[string]string
	acts    map[string][]time.Time
}

func newLedger(taken []Taken) ledger {
	l := ledger{keys: map[string]Taken{}, rounds: map[string][]round{}, escalated: map[string]bool{}, ended: map[string]bool{}, reviewer: map[string]string{},
		goAgent: map[string]string{}, acts: map[string][]time.Time{}}
	for _, t := range taken {
		l.keys[t.Key] = t
		if slices.Contains([]string{KindReview, KindGo, KindForward, KindEscalate, KindEnd}, t.Kind) {
			l.acts[t.Ref] = append(l.acts[t.Ref], t.At)
		}
		switch t.Kind {
		case KindReview:
			_, head, _ := strings.Cut(t.Key, "@")
			l.rounds[t.Ref] = append(l.rounds[t.Ref], round{head: head, at: t.At})
			l.reviewer[t.Ref] = t.Agent
		case KindEscalate:
			l.escalated[t.Ref] = true
		case KindEnd:
			l.ended[t.Ref] = true
		case KindGo, KindForward:
			if t.Agent != "" {
				l.reviewer[t.Ref] = t.Agent
				if t.Kind == KindGo {
					l.goAgent[t.Ref] = t.Agent
				}
			}
		}
	}
	return l
}

func (l ledger) has(key string) bool { _, ok := l.keys[key]; return ok }

// actedAfter reports whether the loop acted on ref after at: a round's
// verdict is acted on once anything follows the round's start.
func (l ledger) actedAfter(ref string, at time.Time) bool {
	return slices.ContainsFunc(l.acts[ref], func(t time.Time) bool { return t.After(at) })
}

// workerDue reports whether the worker the go for ref was given to waits
// for its retirement: a beekeeper-started session on the roster, not
// retired yet, and done with its task: not parked, or parked on ref
// itself. One parked on anything else (a release, a timer, a note) still
// has work to do.
func (l ledger) workerDue(ref string, workers []Worker) bool {
	agent := l.goAgent[ref]
	return agent != "" && !l.has(retireKey(ref, retiredWorker)) &&
		slices.ContainsFunc(workers, func(w Worker) bool {
			return w.Name == agent && w.Started && (w.ParkedOn == "" || w.ParkedOn == ref)
		})
}

// ReviewerName is the roster name of the reviewer of round rd of ref
// (owner/repo#n): "Review <repo> <n> r<rd>".
func ReviewerName(ref string, rd int) string {
	repo, n, _ := strings.Cut(ref, "#")
	_, name, _ := strings.Cut(repo, "/")
	return fmt.Sprintf("Review %s %s r%d", name, n, rd)
}

// Key renderings.
func reviewKey(ref, head string) string    { return fmt.Sprintf("review %s@%s", ref, head) }
func goKey(ref, head string) string        { return fmt.Sprintf("go %s@%s", ref, head) }
func forwardKey(ref, review string) string { return fmt.Sprintf("forward %s %s", ref, review) }
func escalateKey(ref string) string        { return "escalate " + ref }
func endKey(ref string) string             { return "end " + ref }
func eventKey(kind string, e Event) string {
	return fmt.Sprintf("%s %s@%s %s", kind, e.Verb, e.At.UTC().Format(time.RFC3339Nano), e.Agent)
}

// retireKey is the done of the reviewer of a round or the worker of ref.
func retireKey(ref, who string) string { return fmt.Sprintf("done %s %s", ref, who) }

// retiredWorker names the worker in its retirement's key.
const retiredWorker = "worker"

// prRef matches owner/repo#n.
var prRef = regexp.MustCompile(`^[\w.-]+/[\w.-]+#\d+$`)

// IsPull reports whether a park's On names an issue or pull request.
func IsPull(on string) bool { return prRef.MatchString(on) }

// Tracked are the pull requests a pass reads: every one an agent is parked
// on, every one the loop started a review of that it has not seen merged
// or closed, nor escalated, and every ended one whose worker waits for its
// retirement.
func Tracked(workers []Worker, taken []Taken) []string {
	l := newLedger(taken)
	set := map[string]bool{}
	for _, w := range workers {
		if IsPull(w.ParkedOn) {
			set[w.ParkedOn] = true
		}
	}
	for ref := range l.rounds {
		set[ref] = true
	}
	var out []string
	for ref := range set {
		if !l.ended[ref] && !l.escalated[ref] || l.ended[ref] && l.workerDue(ref, workers) {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// Plan is the pass: the actions in, in a stable order, none whose key was
// taken.
func Plan(in Input) []Action {
	l := newLedger(in.Taken)
	rounds := max(in.Rounds, 1)
	var out []Action
	add := func(a Action) {
		if !l.has(a.Key) && !slices.ContainsFunc(out, func(b Action) bool { return b.Key == a.Key }) {
			out = append(out, a)
		}
	}
	parked := map[string]Worker{}
	started := map[string]bool{}
	for _, w := range in.Workers {
		if IsPull(w.ParkedOn) {
			parked[w.ParkedOn] = w
		}
		started[w.Name] = w.Started
	}
	// retire takes a session the loop is done with off the roster; one
	// its person started, one gone already, or one whose own done the
	// loop takes, stays as it is.
	retire := func(ref, who, agent, why string) {
		reported := slices.ContainsFunc(in.Events, func(e Event) bool { return e.Verb == VerbDone && e.Agent == agent })
		if started[agent] && !reported {
			add(Action{Key: retireKey(ref, who), Kind: KindDone, Ref: ref, Agent: agent, Message: why})
		}
	}
	refs := make([]string, 0, len(in.Pulls))
	for ref := range in.Pulls {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		p := in.Pulls[ref]
		if p.State == Merged && l.workerDue(ref, in.Workers) {
			retire(ref, retiredWorker, l.goAgent[ref], fmt.Sprintf("%s merged after the merge go", p.URL))
		}
		if l.escalated[ref] || l.ended[ref] {
			continue
		}
		if p.State == Merged || p.State == Closed {
			if len(l.rounds[ref]) > 0 {
				add(Action{Key: endKey(ref), Kind: KindEnd, Ref: ref, URL: p.URL, Message: strings.ToLower(p.State)})
			}
			continue
		}
		w, isParked := parked[ref]
		agent := w.Name
		if agent == "" {
			agent = l.reviewer[ref]
		}
		if agent == "" || p.Head == "" {
			continue
		}
		base := Action{Ref: ref, URL: p.URL, Agent: agent, Head: p.Head}
		rs := l.rounds[ref]
		review := func() {
			a := base
			a.Key, a.Kind, a.Round, a.SelfHosted = reviewKey(ref, p.Head), KindReview, len(rs)+1, w.SelfHosted
			add(a)
		}
		escalate := func(why string) {
			a := base
			a.Key, a.Kind = escalateKey(ref), KindEscalate
			a.Message = fmt.Sprintf("%s: %d review round(s) used, %s", p.URL, len(rs), why)
			add(a)
		}
		if len(rs) == 0 {
			if p.Green && isParked {
				review()
			}
			continue
		}
		last := rs[len(rs)-1]
		v := p.verdict(last)
		if v == nil {
			continue // the round runs
		}
		left := len(rs) < rounds
		switch v.Verdict {
		case Approved:
			if sameHead(last.head, p.Head) {
				if p.Green {
					add(goAction(base, fmt.Sprintf("review round %d approved head %s", len(rs), short(p.Head))))
				}
				continue
			}
			// The head moved past the approval: a new head is a new
			// review, or the guide's when no round is left.
			if !p.Green || !isParked || !w.ParkedAt.After(v.At) {
				continue
			}
			if left {
				review()
				continue
			}
			escalate(fmt.Sprintf("the last approved head %s and the worker parked on head %s", short(last.head), short(p.Head)))
		case ChangesRequested:
			fk := forwardKey(ref, v.ID)
			if !l.has(fk) {
				a := base
				a.Key, a.Kind = fk, KindForward
				a.Message = forwardMessage(p, *v, left)
				add(a)
				continue
			}
			// The fixes count once the worker parked after the forward.
			if !isParked || !w.ParkedAt.After(l.keys[fk].At) {
				continue
			}
			if sameHead(p.Head, last.head) {
				if !left {
					escalate(fmt.Sprintf("the last asked for changes on head %s and the worker parked again without a new head", short(p.Head)))
				}
				continue
			}
			if !p.Green {
				continue
			}
			if left {
				review()
				continue
			}
			add(goAction(base, fmt.Sprintf("%d review round(s) used; head %s carries the fixes of the last round's findings on %s", len(rs), short(p.Head), short(last.head))))
		}
	}
	// A reviewer leaves once the loop acted on its round's verdict.
	reviewed := make([]string, 0, len(l.rounds))
	for ref := range l.rounds {
		reviewed = append(reviewed, ref)
	}
	sort.Strings(reviewed)
	for _, ref := range reviewed {
		for i, rd := range l.rounds[ref] {
			if l.actedAfter(ref, rd.at) {
				retire(ref, fmt.Sprintf("reviewer r%d", i+1), ReviewerName(ref, i+1), fmt.Sprintf("its verdict of review round %d of %s was acted on", i+1, ref))
			}
		}
	}
	// paired is a report agents idle --done logged with its done: the
	// two lines carry their own clocks, a moment apart.
	paired := func(r Event) bool {
		return slices.ContainsFunc(in.Events, func(e Event) bool {
			d := e.At.Sub(r.At)
			return e.Verb == VerbDone && e.Agent == r.Agent && d > -pairWithin && d < pairWithin
		})
	}
	roster := map[string]bool{}
	for _, w := range in.Workers {
		roster[w.Name] = true
	}
	for _, e := range in.Events {
		switch e.Verb {
		case VerbDone:
			dk := eventKey(KindDone, e)
			ev := e
			if !l.has(dk) {
				add(Action{Key: dk, Kind: KindDone, Agent: e.Agent, Event: &ev})
				continue
			}
			add(Action{Key: eventKey(KindNext, e), Kind: KindNext, Agent: e.Agent, Event: &ev})
		case VerbReport:
			// The final report rides with agents.done, which the rules
			// classify.
			if paired(e) {
				continue
			}
			ev := e
			add(Action{Key: eventKey(KindClassify, e), Kind: KindClassify, Agent: e.Agent, Event: &ev})
		case VerbProblem:
			ev := e
			add(Action{Key: eventKey(KindClassify, e), Kind: KindClassify, Agent: e.Agent, Event: &ev})
		case VerbPark:
			if on, _ := ParkedOn(e.Detail); on == "" {
				ev := e
				add(Action{Key: eventKey(KindClassify, e), Kind: KindClassify, Agent: e.Agent, Event: &ev})
			}
		}
	}
	// A worker's open question for someone else than the guide's person,
	// whenever it was asked.
	for _, n := range in.Notes {
		if !roster[n.By] || in.Person == "" || n.For == "" || strings.EqualFold(n.For, in.Person) {
			continue
		}
		add(Action{Key: fmt.Sprintf("question #%d", n.ID), Kind: KindQuestion, Agent: n.By, Note: n.ID,
			Message: fmt.Sprintf("note #%d of %s for %q: %s", n.ID, n.By, n.For, n.Text)})
	}
	if in.Release != "" {
		add(Action{Key: "release " + in.Release, Kind: KindRelease, Version: in.Release})
	}
	return out
}

// pairWithin is how far apart a report and its done may be logged.
const pairWithin = 5 * time.Second

// EventAgent is the roster name of the agent an event of verb concerns:
// the name its detail opens with ("<name> done: …", "<name>: …"), which
// agents idle and agents park write from the roster, else by, the name
// of the party that logged it (its session's title, which may differ).
func EventAgent(verb, detail, by string) string {
	sep := ": "
	if verb == VerbDone || verb == VerbIdle {
		sep = " done: "
	}
	if strings.HasPrefix(verb, "agents.") {
		if name, _, ok := strings.Cut(detail, sep); ok && name != "" {
			return name
		}
	}
	return by
}

// goAction is the merge go for base's head.
func goAction(base Action, why string) Action {
	a := base
	a.Key, a.Kind = goKey(base.Ref, base.Head), KindGo
	a.Message = fmt.Sprintf("Merge go for %s at head %s (%s): merge it now with --match-head-commit %s, then report.", base.URL, base.Head, why, base.Head)
	return a
}

// forwardMessage forwards review r of p word for word.
func forwardMessage(p Pull, r Review, again bool) string {
	next := "Fix every finding, push, and park on the pull request again once CI is green on the new head: the next review round starts then."
	if !again {
		next = "Fix every finding, push, and park on the pull request again once CI is green on the new head: no further review runs, the merge go for the new head follows."
	}
	return fmt.Sprintf("The review of %s asks for changes on head %s. %s The review, word for word:\n\n%s", p.URL, r.Head, next, r.Body)
}

// ParkedOn reads an agents.park detail ("<name>: parked on <on>: <waits>"
// or "<name>: parked: <waits>"): what it is parked on and what it waits
// for.
func ParkedOn(detail string) (on, waits string) {
	_, rest, ok := strings.Cut(detail, ": parked")
	if !ok {
		return "", ""
	}
	if r, isOn := strings.CutPrefix(rest, " on "); isOn {
		on, waits, _ = strings.Cut(r, ": ")
		return on, waits
	}
	return "", strings.TrimPrefix(rest, ": ")
}

// short is a commit's short form.
func short(sha string) string { return sha[:min(len(sha), 9)] }

// Verdict is a review's verdict: GitHub's own (APPROVED, CHANGES_REQUESTED),
// else that of a commented review whose first line ends in ": approved" or
// ": changes requested" (a review posted from the author's own account
// cannot approve on GitHub). "" for a review without one.
func Verdict(state, body string) string {
	switch state {
	case "APPROVED":
		return Approved
	case "CHANGES_REQUESTED":
		return ChangesRequested
	case stateCommented:
		first, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
		first = strings.ToLower(strings.TrimRight(strings.TrimSpace(first), ".!"))
		switch {
		case strings.HasSuffix(first, ": "+Approved):
			return Approved
		case strings.HasSuffix(first, ": "+ChangesRequested):
			return ChangesRequested
		}
	}
	return ""
}

// stateCommented is a review GitHub records without a verdict.
const stateCommented = "COMMENTED"

// headLine is a review body's "Head reviewed: `<sha>`" line.
var headLine = regexp.MustCompile("(?mi)^head reviewed:\\s*`?([0-9a-f]{7,40})`?")

// ReviewedHead is the head a review judged: the commit its body names on a
// "Head reviewed:" line, else the commit GitHub recorded it on.
func ReviewedHead(body, commit string) string {
	if m := headLine.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return commit
}

// sameHead reports whether two commits, either of them possibly short, are
// one.
func sameHead(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return a == b && a != ""
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
