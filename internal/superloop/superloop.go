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
	KindReview  = "review"
	KindGo      = "go"
	KindForward = "forward"
	// KindRework tells the worker that parked again on the head whose
	// findings stand, with no change-request round left, to fix them: the
	// review rounds stay the loop's, and no merge question reaches the
	// person.
	KindRework = "rework"
	// KindEscalate hands the guide's person a pull request that needs a
	// review past MaxReviews: merge its head without one, or have it
	// reworked.
	KindEscalate = "escalate"
	// KindAnswer tells the worker the person's answer to an escalation
	// that did not release the pull request.
	KindAnswer = "answer"
	// KindDone takes an agent off the roster: one that reported done, or
	// one the loop retires (a reviewer whose verdict it acted on, the
	// worker of a pull request it gave the go for that merged).
	KindDone     = "done"
	KindNext     = "next"
	KindRelease  = "release"
	KindClassify = "classify"
	KindQuestion = "question"
	// KindFile hands the problems a done report found and filed nowhere
	// to the guide's feed, to file in their owning repository.
	KindFile = "file"
	// KindEnd ends the tracking of a pull request that merged or closed.
	KindEnd = "end"
	// KindLeave says once why the loop leaves a parked pull request alone:
	// outside the desk's organisations, in supervisor.ownership.never,
	// parked before the loop's first start, opened by somebody else, or
	// another team's; and why a merged pull request's worker stays for the
	// open work it still serves.
	KindLeave = "leave"
	// KindRelay tells the worker parked on another team's pull request
	// that team's review, word for word.
	KindRelay = "relay"
	// KindOwnership asks the guide's person whether the desk takes over a
	// pull request whose owning team's review blocks its worker;
	// KindOverride records the person's answer that it does.
	KindOwnership = "ownership"
	KindOverride  = "override"
	// KindRed wakes the worker parked on a pull request whose parked head
	// has a check that concluded red (failed, cancelled, timed out), with
	// each such check's name and URL: once for each head, check and park.
	KindRed = "red"
	// KindIntake puts an issue the desk filed (a problem report's issue URL,
	// a follow-up named in a report, an issue a session created with gh
	// issue create) onto the configured board in Backlog and ranks it from
	// the priority list, unless it already sits on another board where it
	// is being worked.
	KindIntake = "intake"
)

// Verbs is every log verb the loop writes.
func Verbs() []string {
	return []string{KindReview, KindGo, KindForward, KindRework, KindEscalate, KindAnswer, KindDone, KindNext, KindRelease, KindClassify, KindQuestion, KindFile, KindEnd,
		KindLeave, KindRelay, KindOwnership, KindOverride, KindRed, KindIntake}
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
	// Base is the branch it merges into.
	Base        string
	MergeCommit string
	// Green is every check on Head concluded successfully (none is not
	// green: they have not reported yet).
	Green bool
	// Red are the checks on Head that concluded red (failure, cancelled,
	// timed out, startup failure, action required; a commit status's
	// failure or error), by name.
	Red     []Check
	Reviews []Review // oldest first
	// Author is the login that opened it; Title, Branch (its head branch)
	// and Files (the paths it changes, ChangedFiles of them in all, which
	// gh may list only in part) are what may name a repository the desk
	// does no work on.
	Author, Title, Branch string
	Files                 []string
	ChangedFiles          int
	// Team is the team the team files say owns its repository, "" when
	// none does.
	Team string
}

// Check is one check on a pull request's head: its name and the URL of its
// run.
type Check struct {
	Name, URL string
}

// Review is one review of a pull request with a verdict.
type Review struct {
	ID     string
	Author string
	URL    string
	Commit string
	// Member is a review by a member or collaborator of the repository's
	// owner: only such a review is the owning team's.
	Member  bool
	Head    string
	Verdict string
	Body    string
	At      time.Time
}

// MaxReviews is how many reviews the loop starts for one pull request,
// whatever happens: one, and a second only for a change an approval does
// not cover. A third need is the guide's person's decision.
const MaxReviews = 2

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
	// ReviewRequested distinguishes a proven published branch from a plain wait.
	ReviewRequested bool
	Name            string
	// SelfHosted is a worker whose model a self-hosted provider serves:
	// its reviewer runs on none of them.
	SelfHosted bool
	// ParkedOn is what the agent is parked on (owner/repo#n, #12, …), ""
	// when it is not parked; ParkedAt since when.
	ParkedOn string
	ParkedAt time.Time
	// ParkedHead is the published head a review request proved (the
	// park's review head), "" for a plain wait.
	ParkedHead string
	// Started is a session beekeeper started: only such a one the loop
	// retires; a session its person started stays.
	Started bool
	// Waking is an open timer that wakes the agent by name (timer add
	// --wake): a park on nothing waits for it.
	Waking bool
	// LastActive is the latest observed session activity.
	LastActive time.Time
	// Serves is the issue the agent's active sessions serve record names
	// (owner/repo#n), "" without one or once it waits for "done".
	Serves string
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

// Answer is a person's answer (answer.recorded) to a note the loop filed.
type Answer struct {
	Ref      string // owner/repo#n, the answer's about
	Note     int
	Question string // the note's question
	Text     string // the person's words, an option's label first
	By       string
	At       time.Time
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
	// Answers are the answers to the notes the loop filed, oldest first.
	Answers []Answer
	// Desk is whose pull requests the loop acts on; Now is the pass's time.
	Desk            Desk
	Now             time.Time
	PostMergeWindow time.Duration
	// DevctlOwners is merge.devctlOwners: a go on their repositories names
	// the gate's devctl pr merge --update-branch, on any other gh pr merge
	// --match-head-commit.
	DevctlOwners []string
	// BaseMerged reports whether p's head only brings its base into
	// approved, an earlier head the loop's go covers: it merges the base,
	// resolving the merge's conflicts or not, or rebases onto it
	// (github.ApprovalCovers); nil: never, every new head is a new review.
	BaseMerged func(p Pull, approved string) bool
	// Open reports whether an issue or pull request (owner/repo#n) is
	// open; one it cannot read counts as open. nil: none is, so a merged
	// pull request's worker is retired as before.
	Open func(ref string) bool
	// StaleAfter is agents.staleAfter: a worker kept for the open work it
	// serves goes once it stayed quiet this long with no park or wake
	// timer (0: a day).
	StaleAfter time.Duration
	// Board reports whether a project board is configured: when true,
	// issues the desk files land on it in Backlog and get ranked from the
	// priority list (KindIntake).
	Board bool
	// Filed are the issues sessions on the desk created (gh issue create,
	// create_issue, issue_write), read from their transcripts.
	Filed []FiledIssue
}

// FiledIssue is an issue a session on the desk created, with the issue or
// pull request the session was working on (From) when it filed it.
type FiledIssue struct {
	Ref   string
	URL   string
	Agent string
	From  string
}

// serving says what open work keeps the worker w of the merged ref on the
// roster, quiet since last: the open issue its serve record names, or
// another open pull request it is parked on. "" when nothing does: it
// serves nothing else open, or it stayed quiet StaleAfter with no park or
// wake timer, so it does not pile up.
func (in Input) serving(w Worker, ref string, last time.Time) string {
	if in.Open == nil {
		return ""
	}
	stale := in.StaleAfter
	if stale <= 0 {
		stale = 24 * time.Hour
	}
	if w.ParkedOn == "" && !w.Waking && !in.Now.Before(last.Add(stale)) {
		return ""
	}
	if on := w.ParkedOn; IsPull(on) && !strings.EqualFold(on, ref) && in.Open(on) {
		return "it is parked on " + on + ", still open"
	}
	if is := w.Serves; is != "" && !strings.EqualFold(is, ref) && in.Open(is) {
		return "it serves " + is + ", still open"
	}
	return ""
}

// baseMerged reports whether p's head moved past approved by bringing in
// the base alone.
func (in Input) baseMerged(p Pull, approved string) bool {
	return in.BaseMerged != nil && approved != "" && !sameHead(approved, p.Head) && in.BaseMerged(p, approved)
}

// gated reports whether the gate's devctl pr merge serves repo.
func (in Input) gated(repo string) bool {
	return slices.ContainsFunc(in.DevctlOwners, func(o string) bool { return strings.EqualFold(o, ownerOf(repo)) })
}

// ownerOf is owner/repo's owner.
func ownerOf(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
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
	// Question is an escalation's or an ownership decision's question for
	// the guide's person; Options an ownership decision's choices.
	Question string
	Options  []string
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

// escalation is a pull request handed to the guide's person: the head it
// names, when, and the person's answer once there is one.
type escalation struct {
	head   string
	at     time.Time
	answer *Answer
}

// open reports whether the escalation still waits for the person.
func (e escalation) open() bool { return e.answer == nil }

// ledger is what the taken actions say.
type ledger struct {
	keys map[string]Taken
	// rounds are the review rounds each pull request had, oldest first,
	// escalated the pull requests handed to the guide, ended the ones
	// merged or closed.
	rounds    map[string][]round
	escalated map[string]escalation
	ended     map[string]bool
	reviewer  map[string]string // a pull request's worker, from its last review
	// goAgent is the worker the go for a pull request was given to; acts
	// are when the loop acted on a pull request (a review, go, forward,
	// escalation or end), oldest first.
	goAgent map[string]string
	acts    map[string][]time.Time
	// asked are the pull requests whose ownership the person was asked
	// about, overridden the ones they let the desk take over.
	asked      map[string]escalation
	overridden map[string]bool
	// red are the checks the loop woke a worker for, by redKey.
	red map[string]bool
}

func newLedger(taken []Taken, answers []Answer) ledger {
	l := ledger{keys: map[string]Taken{}, rounds: map[string][]round{}, escalated: map[string]escalation{}, ended: map[string]bool{}, reviewer: map[string]string{},
		goAgent: map[string]string{}, acts: map[string][]time.Time{}, asked: map[string]escalation{}, overridden: map[string]bool{}, red: map[string]bool{}}
	for _, t := range taken {
		l.keys[t.Key] = t
		if slices.Contains([]string{KindReview, KindGo, KindForward, KindRework, KindEscalate, KindAnswer, KindEnd, KindLeave}, t.Kind) {
			l.acts[t.Ref] = append(l.acts[t.Ref], t.At)
		}
		switch t.Kind {
		case KindReview:
			_, head, _ := strings.Cut(t.Key, "@")
			l.rounds[t.Ref] = append(l.rounds[t.Ref], round{head: head, at: t.At})
			l.reviewer[t.Ref] = t.Agent
		case KindEscalate:
			_, head, _ := strings.Cut(t.Key, "@")
			l.escalated[t.Ref] = escalation{head: head, at: t.At}
		case KindEnd:
			l.ended[t.Ref] = true
		case KindOwnership:
			l.asked[t.Ref] = escalation{at: t.At}
		case KindOverride:
			l.overridden[t.Ref] = true
		case KindRed:
			// "red <ref>@<head> <parked>: <check>; <check>…"
			if prefix, checks, ok := strings.Cut(t.Key, ": "); ok {
				for name := range strings.SplitSeq(checks, redSep) {
					l.red[prefix+": "+name] = true
				}
			}
		case KindGo, KindForward:
			if t.Agent != "" {
				l.reviewer[t.Ref] = t.Agent
				if t.Kind == KindGo {
					l.goAgent[t.Ref] = t.Agent
				}
			}
		}
	}
	// The person's answer to an escalation is the last one recorded after
	// it to the loop's question about that pull request.
	// So is the answer to a question about its ownership.
	for _, an := range answers {
		for prefix, asked := range map[string]map[string]escalation{escalationPrefix: l.escalated, ownershipPrefix: l.asked} {
			e, ok := asked[an.Ref]
			if ok && an.At.After(e.at) && strings.HasPrefix(an.Question, prefix) {
				e.answer = &an
				asked[an.Ref] = e
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
// for its post-merge report: a beekeeper-started session still on the
// roster and not retired yet. A park on a release, timer or note keeps
// the worker reachable too.
func (l ledger) workerDue(ref string, workers []Worker) bool {
	agent := l.goAgent[ref]
	return agent != "" && !l.has(retireKey(ref, retiredWorker)) &&
		slices.ContainsFunc(workers, func(w Worker) bool {
			return w.Name == agent && w.Started
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
func reworkKey(ref, head string, parked time.Time) string {
	return fmt.Sprintf("rework %s@%s %s", ref, head, parked.UTC().Format(time.RFC3339Nano))
}
func escalateKey(ref, head string) string   { return fmt.Sprintf("escalate %s@%s", ref, head) }
func answerKey(ref string, note int) string { return fmt.Sprintf("answer %s #%d", ref, note) }
func endKey(ref string) string              { return "end " + ref }
func intakeKey(ref string) string           { return "intake " + strings.ToLower(ref) }

// redKey is the key of the wake that tells the red checks names of ref's
// head to the park of parked; a single check's redKey is its own mark.
func redKey(ref, head string, parked time.Time, names ...string) string {
	return fmt.Sprintf("red %s@%s %s: %s", ref, head, parked.UTC().Format(time.RFC3339Nano), strings.Join(names, redSep))
}

// redSep separates the checks of one red wake's key.
const redSep = "; "

// redName is a check's name as a key holds it: on one line, without the
// brackets that end a key or the separator of its checks.
func redName(name string) string {
	name = strings.NewReplacer("[", "(", "]", ")", ";", ",").Replace(name)
	return strings.Join(strings.Fields(name), " ")
}

func eventKey(kind string, e Event) string {
	return fmt.Sprintf("%s %s@%s %s", kind, e.Verb, e.At.UTC().Format(time.RFC3339Nano), e.Agent)
}

// retireKey is the done of the reviewer of a round or the worker of ref.
func retireKey(ref, who string) string { return fmt.Sprintf("done %s %s", ref, who) }

// retiredWorker names the worker in its retirement's key.
const retiredWorker = "worker"

// leftServing is the reason of the leave that keeps a merged pull
// request's worker on the roster for the open work it still serves.
const leftServing = "serving"

// prRef matches owner/repo#n.
var prRef = regexp.MustCompile(`^[\w.-]+/[\w.-]+#\d+$`)

// IsPull reports whether a park's On names an issue or pull request.
func IsPull(on string) bool { return prRef.MatchString(on) }

// Tracked are the pull requests a pass reads: every verified review request
// an agent is parked on, every one the loop started a review of that it
// has not seen merged or closed, and every ended one whose worker waits
// for its retirement. An escalation an earlier loop filed holds none of
// them: their review rounds are the loop's. None outside the
// desk's organisations or in its never list is read, nor one whose park
// is from before the loop's first start.
func Tracked(workers []Worker, taken []Taken, answers []Answer, desk Desk) []string {
	l := newLedger(taken, answers)
	set := map[string]bool{}
	for ref := range l.rounds {
		set[ref] = true
	}
	// A pull request every park on is from before the loop's first start
	// is left alone; one parked on since is read.
	fresh, stale := parks(workers, desk)
	for ref := range fresh {
		set[ref] = true
	}
	for ref := range stale {
		if !fresh[ref] {
			delete(set, ref)
		}
	}
	var out []string
	for ref := range set {
		if reason, _ := desk.unread(ref); reason != "" {
			continue
		}
		// An open question about its ownership keeps it read: the owning
		// team's reviews still reach its worker meanwhile.
		if !l.ended[ref] || l.workerDue(ref, workers) {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// parks are the pull requests workers are parked on: fresh those parked
// on since the desk's loop first started, stale those parked on before.
func parks(workers []Worker, desk Desk) (fresh, stale map[string]bool) {
	fresh, stale = map[string]bool{}, map[string]bool{}
	for _, w := range workers {
		if !IsPull(w.ParkedOn) || !w.ReviewRequested {
			continue
		}
		if early, _ := desk.early(w.ParkedAt); early {
			stale[w.ParkedOn] = true
		} else {
			fresh[w.ParkedOn] = true
		}
	}
	return fresh, stale
}

// Plan is the pass: the actions in, in a stable order, none whose key was
// taken.
func Plan(in Input) []Action {
	l := newLedger(in.Taken, in.Answers)
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
		// Of two verified review requests on one pull request, the later
		// park is its request. A plain waiter cannot take its place.
		if cur, ok := parked[w.ParkedOn]; IsPull(w.ParkedOn) && w.ReviewRequested && (!ok || !w.ParkedAt.Before(cur.ParkedAt)) {
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
	// leave says once why the loop leaves ref alone.
	leave := func(ref, url, agent, reason, why string) {
		add(Action{Key: leaveKey(ref, reason), Kind: KindLeave, Ref: ref, URL: url, Agent: agent, Message: why})
	}
	// A park the loop leaves alone without reading its pull request.
	fresh, _ := parks(in.Workers, in.Desk)
	for _, w := range in.Workers {
		if !IsPull(w.ParkedOn) || !w.ReviewRequested {
			continue
		}
		if reason, why := in.Desk.unread(w.ParkedOn); reason != "" {
			leave(w.ParkedOn, "", w.Name, reason, why)
		} else if early, why := in.Desk.early(w.ParkedAt); early && !fresh[w.ParkedOn] {
			leave(w.ParkedOn, "", w.Name, leftBefore, why)
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
			// The end action durably marks when the merge was observed. A
			// quiet worker retains its inbox while waiting for its release.
			if end, ok := l.keys[endKey(ref)]; ok {
				last := end.At
				for _, w := range in.Workers {
					if w.Name == l.goAgent[ref] {
						if w.LastActive.After(last) {
							last = w.LastActive
						}
						if w.ParkedAt.After(last) {
							last = w.ParkedAt
						}
					}
				}
				window := in.PostMergeWindow
				if window <= 0 {
					window = 2 * time.Hour
				}
				if !in.Now.Before(last.Add(window)) {
					// A worker whose task spans several pull requests
					// stays while it serves other open work; its own
					// done or the close of that work retires it.
					agent := l.goAgent[ref]
					i := slices.IndexFunc(in.Workers, func(w Worker) bool { return w.Name == agent })
					if why := in.serving(in.Workers[i], ref, last); why != "" {
						leave(ref, p.URL, agent, leftServing, fmt.Sprintf("%s merged; %s stays on the roster: %s", p.URL, agent, why))
					} else {
						retire(ref, retiredWorker, agent, fmt.Sprintf("%s: no report after merge; no activity for %s", p.URL, window))
					}
				}
			}
		}
		if l.ended[ref] {
			continue
		}
		// An escalation an earlier loop filed and nobody answered holds
		// nothing: the review rounds go on as for any other.
		esc, escalated := l.escalated[ref]
		escalated = escalated && !esc.open()
		ask, asked := l.asked[ref]
		if p.State == Merged || p.State == Closed {
			if len(l.rounds[ref]) > 0 {
				add(Action{Key: endKey(ref), Kind: KindEnd, Ref: ref, URL: p.URL, Message: strings.ToLower(p.State)})
			}
			continue
		}
		w, isParked := parked[ref]
		if !isParked && slices.ContainsFunc(in.Workers, func(w Worker) bool { return w.Name == l.reviewer[ref] && w.ParkedOn == ref && !w.ReviewRequested }) {
			continue
		}
		if early, _ := in.Desk.early(w.ParkedAt); isParked && early {
			continue
		}
		agent := w.Name
		if agent == "" {
			agent = l.reviewer[ref]
		}
		if agent == "" || p.Head == "" {
			continue
		}
		base := Action{Ref: ref, URL: p.URL, Agent: agent, Head: p.Head}
		switch o, reason, why := in.Desk.owner(p, l.overridden[ref]); o {
		case nobodys:
			leave(ref, p.URL, agent, reason, why)
			continue
		case theirs:
			leave(ref, p.URL, agent, reason, why)
			planRed(l, p, w, isParked, add)
			planTheirs(in, l, p, w, isParked, base, ask, asked, add)
			continue
		case unknown:
			continue
		}
		planRed(l, p, w, isParked, add)
		rs := l.rounds[ref]
		// An escalation logged before its key named a head was on the
		// head of the last round, whose findings stood unfixed.
		if escalated && esc.head == "" && len(rs) > 0 {
			esc.head = rs[len(rs)-1].head
		}
		if escalated {
			// The person answered: a release is the merge go for the head
			// the escalation named; any other answer goes to the worker,
			// and the pull request's review rounds go on from there.
			// A release of a head the worker moved past goes on as the
			// rounds say of the new head, unless the new head only merges
			// the base into the released one.
			an := esc.answer
			released := Releases(an.Text)
			if released && (sameHead(esc.head, p.Head) || p.Green && in.baseMerged(p, esc.head)) {
				if p.Green {
					why := fmt.Sprintf("%s released it, answering note #%d: %s", an.By, an.Note, oneLine(an.Text))
					if !sameHead(esc.head, p.Head) {
						why += fmt.Sprintf("; head %s only merges %s into the released head %s", short(p.Head), baseName(p), short(esc.head))
					}
					add(goAction(in, base, why))
				}
				continue
			}
			if ak := answerKey(ref, an.Note); !released && !l.has(ak) {
				a := base
				a.Key, a.Kind = ak, KindAnswer
				a.Message = answerMessage(p, esc.head, *an)
				add(a)
				continue
			}
		}
		review := func() {
			a := base
			a.Key, a.Kind, a.Round, a.SelfHosted = reviewKey(ref, p.Head), KindReview, len(rs)+1, w.SelfHosted
			add(a)
		}
		// escalate hands a head that needs a review past MaxReviews to the
		// person, once: one answered already goes on as its answer says.
		escalate := func(reviewed string) {
			if escalated && sameHead(esc.head, p.Head) {
				return
			}
			a := base
			a.Key, a.Kind = escalateKey(ref, p.Head), KindEscalate
			a.Question = EscalationQuestion(p.URL, p.Head)
			a.Message = fmt.Sprintf("%s: its %d reviews are used, the last on head %s, and head %s changes more than the base since: it gets no third review", p.URL, len(rs), short(reviewed), short(p.Head))
			add(a)
		}
		// cycles is how many of the rounds asked for changes: the rounds
		// supervisor.reviewRounds caps. An approved head is no cycle; a new
		// head after it gets a review only for a change the approval does
		// not cover, and never past MaxReviews.
		cycles := 0
		for _, rd := range rs {
			if v := p.verdict(rd); v != nil && v.Verdict == ChangesRequested {
				cycles++
			}
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
		left := cycles < rounds && len(rs) < MaxReviews
		switch v.Verdict {
		case Approved:
			if sameHead(last.head, p.Head) {
				if p.Green {
					add(goAction(in, base, fmt.Sprintf("review round %d approved head %s", len(rs), short(p.Head))))
				}
				continue
			}
			// The head moved past the approval. The approval covers a head
			// that only brings the base into the approved one (the gate's
			// --update-branch, the worker's merge of the base and its
			// conflicts' resolution, a rebase onto it); any other change
			// gets a review while one of MaxReviews is left, else goes to
			// the person.
			if !p.Green || !isParked || !w.ParkedAt.After(v.At) {
				continue
			}
			if in.baseMerged(p, last.head) {
				add(goAction(in, base, fmt.Sprintf("review round %d approved head %s; head %s only brings %s into it", len(rs), short(last.head), short(p.Head), baseName(p))))
				continue
			}
			if len(rs) < MaxReviews {
				review()
				continue
			}
			escalate(last.head)
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
			// A head that only merges the base into the head the findings
			// were about carries no fixes: it is that head, never a go.
			merged := p.Green && in.baseMerged(p, last.head)
			if sameHead(p.Head, last.head) || merged {
				// The findings stand unfixed: with no round left, or on a
				// base merge, the worker reworks, once for each such park.
				// Review and rework are the loop's; no merge question goes
				// to the person, and an answered escalation of this head
				// is handled above.
				if (!left || merged) && (!escalated || !sameHead(esc.head, p.Head)) {
					a := base
					a.Key, a.Kind = reworkKey(ref, p.Head, w.ParkedAt), KindRework
					a.Message = reworkMessage(p, last.head, cycles, rounds, left)
					add(a)
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
			add(goAction(in, base, fmt.Sprintf("%d of %d change-request round(s) used, %d of %d review(s); head %s carries the fixes of the last round's findings on %s", cycles, rounds, len(rs), MaxReviews, short(p.Head), short(last.head))))
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
	// near reports whether a and b were logged a moment apart: the lines
	// of one agents idle --done carry their own clocks.
	near := func(a, b Event) bool {
		d := a.At.Sub(b.At)
		return a.Agent == b.Agent && d > -pairWithin && d < pairWithin
	}
	// paired is a report or problem agents idle --done logged with its
	// done.
	paired := func(r Event) bool {
		return slices.ContainsFunc(in.Events, func(e Event) bool { return e.Verb == VerbDone && near(e, r) })
	}
	// forwarded is a problem of the reviewer of one of the loop's rounds
	// that matches the changes request of its round the loop forwarded
	// (or forwards in this pass): the worker has it in the review, word for
	// word.
	forwarded := func(e Event) bool {
		for ref, rs := range l.rounds {
			p, tracked := in.Pulls[ref]
			for i, rd := range rs {
				if ReviewerName(ref, i+1) != e.Agent || !tracked {
					continue
				}
				v := p.verdict(rd)
				if v == nil || v.Verdict != ChangesRequested {
					continue
				}
				fk := forwardKey(ref, v.ID)
				if (l.has(fk) || slices.ContainsFunc(out, func(a Action) bool { return a.Key == fk })) && matches(problemText(e), v.Body) {
					return true
				}
			}
		}
		return false
	}
	roster := map[string]bool{}
	workers := map[string]Worker{}
	for _, w := range in.Workers {
		roster[w.Name] = true
		workers[w.Name] = w
	}
	// waiting is a park on nothing that still waits on something: a timer
	// that wakes the agent, a note its wait names, or a note it asked
	// that is open. One whose agent is no longer parked on nothing has
	// nothing left to decide. An event from a replaced park is settled too.
	waiting := func(e Event) bool {
		w, ok := workers[e.Agent]
		if !ok || w.ParkedAt.IsZero() || w.ParkedOn != "" || e.At.Before(w.ParkedAt) {
			return true
		}
		if _, waits := ParkedOn(e.Detail); w.Waking || noteRef.MatchString(waits) {
			return true
		}
		return slices.ContainsFunc(in.Notes, func(n Note) bool { return n.By == e.Agent })
	}
	classify := func(e Event) {
		ev := e
		add(Action{Key: eventKey(KindClassify, e), Kind: KindClassify, Agent: e.Agent, Event: &ev})
	}
	intake := func(ref, url, agent, from string) {
		if !in.Board || ref == "" {
			return
		}
		if reason, _ := in.Desk.unread(ref); reason != "" {
			return
		}
		if url == "" {
			repo, n, _ := strings.Cut(ref, "#")
			url = fmt.Sprintf("https://github.com/%s/issues/%s", repo, n)
		}
		add(Action{Key: intakeKey(ref), Kind: KindIntake, Ref: ref, URL: url, Agent: agent, Message: from})
	}
	originOf := func(e Event, excludeURL string) string {
		if w, ok := workers[e.Agent]; ok {
			if w.Serves != "" && !strings.EqualFold(w.Serves, issueRefFromURL(excludeURL)) {
				return w.Serves
			}
			if w.ParkedOn != "" && !strings.EqualFold(w.ParkedOn, issueRefFromURL(excludeURL)) {
				return w.ParkedOn
			}
		}
		for _, o := range in.Events {
			if (o.Verb == VerbReport || o.Verb == VerbDone) && near(o, e) {
				if m := pullURL.FindString(o.Detail); m != "" {
					return issueRefFromURL(m)
				}
				for _, u := range issueURL.FindAllString(o.Detail, -1) {
					if !strings.EqualFold(u, excludeURL) {
						return issueRefFromURL(u)
					}
				}
			}
		}
		return ""
	}
	for _, e := range in.Events {
		switch e.Verb {
		case VerbDone:
			dk := eventKey(KindDone, e)
			ev := e
			if !l.has(dk) {
				// The report's problems: one naming its issue is filed;
				// the others go to the guide's feed to file, before the
				// agent leaves the roster.
				var filed, unfiled []string
				for _, p := range in.Events {
					if p.Verb != VerbProblem || !near(p, e) || forwarded(p) {
						continue
					}
					if Filed(p.Detail) {
						filed = append(filed, problemText(p))
						for _, u := range issueURL.FindAllString(p.Detail, -1) {
							intake(issueRefFromURL(u), u, p.Agent, originOf(p, u))
						}
					} else {
						unfiled = append(unfiled, problemText(p))
					}
				}
				fk := eventKey(KindFile, e)
				if len(unfiled) > 0 && !l.has(fk) {
					add(Action{Key: fk, Kind: KindFile, Agent: e.Agent, Event: &ev, Message: strings.Join(unfiled, "\n")})
					continue
				}
				a := Action{Key: dk, Kind: KindDone, Agent: e.Agent, Event: &ev}
				var says []string
				if len(filed) > 0 {
					says = append(says, fmt.Sprintf("its report names %d problem(s) filed: %s", len(filed), strings.Join(filed, "; ")))
				}
				if len(unfiled) > 0 {
					says = append(says, fmt.Sprintf("%d problem(s) filed nowhere are in the guide's feed to file", len(unfiled)))
				}
				a.Message = strings.Join(says, "; ")
				add(a)
				continue
			}
			for _, p := range in.Events {
				if p.Verb == VerbProblem && near(p, e) && !forwarded(p) && Filed(p.Detail) {
					for _, u := range issueURL.FindAllString(p.Detail, -1) {
						intake(issueRefFromURL(u), u, p.Agent, originOf(p, u))
					}
				}
			}
			add(Action{Key: eventKey(KindNext, e), Kind: KindNext, Agent: e.Agent, Event: &ev})
		case VerbReport:
			for _, u := range filedIssueURLs(e.Detail) {
				intake(issueRefFromURL(u), u, e.Agent, originOf(e, u))
			}
			// The final report rides with agents.done, which the rules
			// classify.
			if !paired(e) {
				classify(e)
			}
		case VerbProblem:
			// A problem of a done report rides with its done, which
			// files it; a reviewer's that matches the changes request
			// the loop forwarded is the forward's. Any other is a
			// finding, never a decision: one naming its issue is filed,
			// one naming none goes to the guide's feed to file in its
			// owning repository, and the agent keeps its task.
			if !paired(e) && !forwarded(e) {
				if Filed(e.Detail) {
					for _, u := range issueURL.FindAllString(e.Detail, -1) {
						intake(issueRefFromURL(u), u, e.Agent, originOf(e, u))
					}
				} else {
					ev := e
					add(Action{Key: eventKey(KindFile, e), Kind: KindFile, Agent: e.Agent, Event: &ev, Message: problemText(e)})
				}
			}
		case VerbPark:
			if on, _ := ParkedOn(e.Detail); on == "" && !waiting(e) {
				classify(e)
			}
		}
	}
	for _, f := range in.Filed {
		intake(f.Ref, f.URL, f.Agent, f.From)
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
	// Drain pending reviews before refilling worker capacity.
	slices.SortStableFunc(out, func(a, b Action) int {
		if a.Kind == KindNext && b.Kind != KindNext {
			return 1
		}
		if b.Kind == KindNext && a.Kind != KindNext {
			return -1
		}
		return 0
	})
	return out
}

// planRed wakes worker w, parked on p, when a check on the head it parked
// on concluded red: the loop reviews no head that is not green, so the
// worker would wait for nothing. One wake names every check red now that
// no earlier wake of this park named; a check is told once for each head
// and park (a check run again that fails again on the same head is told
// once the worker parked again), and none after the head's merge go.
func planRed(l ledger, p Pull, w Worker, isParked bool, add func(Action)) {
	if !isParked || p.State != Open || p.Head == "" || !sameHead(w.ParkedHead, p.Head) || l.has(goKey(p.Ref, p.Head)) {
		return
	}
	var names, lines []string
	for _, c := range p.Red {
		name := redName(c.Name)
		if name == "" || slices.Contains(names, name) || l.red[redKey(p.Ref, p.Head, w.ParkedAt, name)] {
			continue
		}
		names = append(names, name)
		line := name
		if c.URL != "" {
			line += " " + c.URL
		}
		lines = append(lines, line)
	}
	if len(names) == 0 {
		return
	}
	add(Action{Key: redKey(p.Ref, p.Head, w.ParkedAt, names...), Kind: KindRed, Ref: p.Ref, URL: p.URL, Agent: w.Name, Head: p.Head,
		Message: redMessage(p, lines)})
}

// redMessage tells the worker parked on p which checks on its head
// concluded red.
func redMessage(p Pull, lines []string) string {
	return fmt.Sprintf("CI is red on %s at head %s, the head you parked on; no review starts until every check is green. Red: %s. "+
		"Read the failure, fix it and push (or rerun a check that failed for no reason of the change), then park again from the worktree: beekeeper agents park --on %s \"desk review\".",
		p.URL, short(p.Head), strings.Join(lines, "; "), p.Ref)
}

// planTheirs plans another team's pull request p: the person's answer about
// its ownership, the owning team's reviews told to the worker parked on it,
// and, once that team let the worker wait OwnerWait without a review, the
// person asked whether the desk takes it over.
func planTheirs(in Input, l ledger, p Pull, w Worker, isParked bool, base Action, ask escalation, asked bool, add func(Action)) {
	if asked && ask.answer != nil {
		an := *ask.answer
		a := base
		if Overrides(an.Text) {
			a.Key, a.Kind = overrideKey(p.Ref), KindOverride
			a.Message = fmt.Sprintf("%s overrode %s's ownership of %s, answering note #%d: %s; the desk reviews it from here",
				an.By, teamName(p.Team), p.URL, an.Note, oneLine(an.Text))
			add(a)
			return
		}
		if ak := answerKey(p.Ref, an.Note); !l.has(ak) {
			a.Key, a.Kind = ak, KindAnswer
			a.Message = ownershipAnswerMessage(p, an)
			add(a)
		}
	}
	if !isParked {
		return
	}
	reviews := ownerReviews(p, w.ParkedAt)
	for _, r := range reviews {
		a := base
		a.Key, a.Kind, a.Agent = relayKey(p.Ref, r.ID), KindRelay, w.Name
		a.Message = relayMessage(p, r, p.Team)
		add(a)
	}
	if asked || len(reviews) > 0 || ownerApproved(p) || in.Now.Sub(w.ParkedAt) < in.Desk.OwnerWait {
		return
	}
	a := base
	a.Key, a.Kind, a.Agent = ownershipKey(p.Ref), KindOwnership, w.Name
	a.Question, a.Options = OwnershipQuestion(p.URL, p.Team), OwnershipOptions(p.Team)
	a.Message = fmt.Sprintf("%s parked on %s at %s for %s's review, and none came in %s",
		w.Name, p.URL, w.ParkedAt.UTC().Format(time.RFC3339), teamName(p.Team), in.Desk.OwnerWait)
	add(a)
}

// issueURL is an issue's URL: a problem naming one is filed.
var issueURL = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/issues/\d+`)

// pullURL is a pull request's URL.
var pullURL = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// filedReportClause matches a clause in a report that names newly filed issues.
var filedReportClause = regexp.MustCompile(`(?i)\b(?:filed|created|opened|follow[- ]?ups?|problems?\s+found)\b[^.;\n]*`)

// filedIssueURLs returns the issue URLs a report names as newly filed or
// found problems, leaving out the closed task issue the report summarizes.
func filedIssueURLs(report string) []string {
	var out []string
	for _, clause := range filedReportClause.FindAllString(report, -1) {
		for _, u := range issueURL.FindAllString(clause, -1) {
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	}
	return out
}

// issueRefFromURL converts https://github.com/owner/repo/issues/n (or /pull/n)
// to owner/repo#n.
func issueRefFromURL(u string) string {
	s := strings.TrimPrefix(u, "https://github.com/")
	for _, sep := range []string{"/issues/", "/pull/"} {
		if repo, n, ok := strings.Cut(s, sep); ok && repo != "" && n != "" {
			return repo + "#" + n
		}
	}
	return ""
}

// Filed reports whether a problem found names its issue's URL: it is
// filed.
func Filed(problem string) bool { return issueURL.MatchString(problem) }

// problemText is an agents.problem's finding, without its agent's name.
func problemText(e Event) string { return strings.TrimPrefix(e.Detail, e.Agent+": ") }

// matches reports whether a problem says what a review's body says: most
// of its words (four letters or more) are the body's.
func matches(problem, body string) bool {
	in := map[string]bool{}
	for _, w := range words(body) {
		in[w] = true
	}
	ws := words(problem)
	if len(ws) == 0 {
		return false
	}
	n := 0
	for _, w := range ws {
		if in[w] {
			n++
		}
	}
	return n*10 >= len(ws)*6
}

// words are s's words of four letters or more, lower case.
func words(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) {
		if len(w) >= 4 {
			out = append(out, w)
		}
	}
	return out
}

// noteRef is a note a park's wait names (#12), not an issue's owner/repo#n.
var noteRef = regexp.MustCompile(`(?:^|[^\w/.-])#\d+\b`)

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

// goAction is the merge go for base's head, naming the merge command of
// its repository's path: the gate's devctl pr merge --update-branch where
// devctl serves it, whose base update the go covers, else gh pr merge
// --match-head-commit.
func goAction(in Input, base Action, why string) Action {
	a := base
	a.Key, a.Kind = goKey(base.Ref, base.Head), KindGo
	repo, n, _ := strings.Cut(base.Ref, "#")
	if in.gated(repo) {
		a.Message = fmt.Sprintf("Merge go for %s at head %s (%s): merge it now through the gate, `devctl pr merge %s %s --update-branch`, once `gh pr view` shows head %s. This go covers a head the gate's --update-branch moves on by merging the base alone: let that merge run to its end, then report.",
			base.URL, base.Head, why, repo, n, short(base.Head))
		return a
	}
	a.Message = fmt.Sprintf("Merge go for %s at head %s (%s): merge it now with `gh pr merge %s --repo %s --squash --match-head-commit %s`, then report.", base.URL, base.Head, why, n, repo, base.Head)
	return a
}

// baseName is p's base branch, "the base" when unknown.
func baseName(p Pull) string {
	if p.Base == "" {
		return "the base"
	}
	return p.Base
}

// escalationPrefix opens every escalation's question: an answer to a note
// with it is the person's answer to an escalation.
const escalationPrefix = "Merge "

// EscalationQuestion is the question an escalation asks the guide's
// person: the pull request and the head it names, which changes more than
// its reviews saw. An earlier loop's escalations asked it of findings left
// unfixed; their answers count alike.
func EscalationQuestion(url, head string) string {
	return fmt.Sprintf("%s%s at head %s without a third review, or have it reworked?", escalationPrefix, url, short(head))
}

// EscalationOptions are an escalation's choices: the first releases it.
func EscalationOptions(head string) []string {
	return []string{
		fmt.Sprintf("Merge: the supervisor loop gives the worker the merge go for head %s", short(head)),
		"Rework: the worker is told the answer, reworks the change and parks again",
	}
}

// releases are the whole answers, in their own words, that release an
// escalation: a bare go to merge and nothing else.
var releases = []string{"merge", "merge it", "you can merge", "you can merge it", "go ahead and merge", "ok merge", "ok merge it",
	"yes merge", "yes merge it", "lgtm", "ship it", "approved"}

// Releases reports whether the person's answer to an escalation releases
// the pull request. It fails closed: only the first option's label
// ("Merge", its words after it or not) or a bare go to merge (releases)
// does; any other words, which may ask for fixes first, do not, and go to
// the worker word for word.
func Releases(answer string) bool {
	label, _, _ := strings.Cut(strings.TrimSpace(answer), " — ")
	if strings.EqualFold(strings.TrimSpace(label), "merge") {
		return true
	}
	words := strings.Fields(strings.ToLower(strings.Map(func(r rune) rune {
		if strings.ContainsRune(".,!:;", r) {
			return ' '
		}
		return r
	}, answer)))
	return slices.Contains(releases, strings.Join(words, " "))
}

// answerMessage tells the worker of p the person's answer an to the
// escalation of head.
func answerMessage(p Pull, head string, an Answer) string {
	return fmt.Sprintf("%s answered the escalation of %s at head %s (note #%d), word for word: %s\n\nThe loop does not read it as releasing the merge: rework as it says, push, and once CI is green on the new head, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree.", an.By, p.URL, short(head), an.Note, an.Text)
}

// reworkMessage tells the worker of p, parked again on reviewed, the head
// its last review asked changes on, or on a head that only merges the base
// into it, to fix them.
func reworkMessage(p Pull, reviewed string, cycles, rounds int, left bool) string {
	at := fmt.Sprintf("at head %s, the head its last review asked for changes on", short(p.Head))
	if !sameHead(p.Head, reviewed) {
		at = fmt.Sprintf("at head %s, which only brings %s into head %s, the head its last review asked for changes on", short(p.Head), baseName(p), short(reviewed))
	}
	next := "the merge go for the new head follows"
	if left {
		next = "the next review round starts then"
	}
	return fmt.Sprintf("You parked on %s again %s: those findings stand unfixed, %d of %d change-request round(s) are used, and no merge go comes for this head. Nobody else decides it: fix every finding of that review (forwarded to you, and on the pull request), push, and once CI is green on the new head, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree: %s.",
		p.URL, at, cycles, rounds, next)
}

// oneLine is s on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// forwardMessage forwards review r of p word for word.
func forwardMessage(p Pull, r Review, again bool) string {
	next := "Fix every finding, push, and once CI is green on the new head, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree: the next review round starts then."
	if !again {
		next = "Fix every finding, push, and once CI is green on the new head, run `cd \"<worktree>\" && beekeeper agents park --on <owner/repo#n> \"desk review\"` from the published worktree: no further review runs, the merge go for the new head follows."
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

// ReleaseWorkers returns the retained workers whose merged pull requests may
// be carried by an installed release. Containment is checked by the caller.
func ReleaseWorkers(in Input) []Action {
	l := newLedger(in.Taken, in.Answers)
	var out []Action
	for ref, p := range in.Pulls {
		if p.State != Merged || !l.workerDue(ref, in.Workers) {
			continue
		}
		agent := l.goAgent[ref]
		if slices.ContainsFunc(in.Events, func(e Event) bool { return e.Agent == agent && e.Verb == VerbDone }) {
			continue
		}
		out = append(out, Action{Ref: ref, Agent: agent, URL: p.URL, Head: p.MergeCommit})
	}
	slices.SortFunc(out, func(a, b Action) int { return strings.Compare(a.Ref, b.Ref) })
	return out
}
