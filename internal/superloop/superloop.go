// Package superloop is the supervisor's fixed procedure as a pure function:
// beekeeper's events, the roster and the tracked pull requests in, the
// transitions to take out. `beekeeper supervisor loop` gathers the input,
// runs each action through beekeeper's own commands and logs it under its
// key, which is what keeps a restarted loop from taking it twice.
package superloop

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/findings"
)

// The kinds of action, each logged as "supervisor.<kind>".
const (
	mergeableConflicting = "CONFLICTING"
	mergeStateDirty      = "DIRTY"

	KindReview = "review"
	KindGo     = "go"
	// KindMerge records a coordinator mutation, never a worker grant.
	KindMerge   = "merge"
	KindForward = "forward"
	// KindRework tells the worker that parked again on the head whose
	// findings stand to fix them: the
	// review rounds stay the loop's, and no merge question reaches the
	// person.
	KindRework = "rework"
	// KindAnswer tells the worker the person's answer to an ownership
	// question that did not override the owning team.
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
	// KindProgress retains an owned delivery report without guide work.
	KindProgress = "progress"
	// KindEnd ends the tracking of a pull request that merged or closed.
	KindEnd = "end"
	// KindLeave says once why the loop leaves a parked pull request alone:
	// outside the desk's organisations, in supervisor.ownership.never,
	// parked before the loop's first start, opened by somebody else,
	// another team's, or green on a head past its approval with no park
	// since the round that approved it; and why a merged pull request's
	// worker stays for the open work it still serves.
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
	// issue create) onto the configured board in Backlog and places it on the
	// board, unless it already sits on another board where it is being worked.
	KindIntake = "intake"
	// KindOwner gives an overlapping pull request a live owner when a worker
	// parks on it and its owner is gone or it has made no progress for
	// OverlapWait.
	KindOwner = "owner"
)

// Verbs is every log verb the loop writes.
func Verbs() []string {
	return []string{KindReview, KindGo, KindForward, KindRework, KindAnswer, KindDone, KindNext, KindRelease, KindClassify, KindQuestion, KindFile, KindProgress, KindEnd,
		KindLeave, KindRelay, KindOwnership, KindOverride, KindRed, KindIntake, KindOwner, KindMerge}
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
	// Closes names the issues closed by this pull request.
	Closes []string
	// Absorbs names the original pull requests a combining pull request
	// supersedes or folds in.
	Absorbs               []string
	Draft                 bool
	Mergeable, MergeState string
	ReviewDecision        string
	Ref                   string // owner/repo#n
	URL                   string
	State                 string
	Head                  string
	// Base is the branch it merges into.
	Base        string
	MergeCommit string
	// Green is every non-superseded check on Head concluded successfully (none is not
	// green: they have not reported yet).
	Green bool
	// Red are the checks on Head that concluded red (failure, cancelled,
	// timed out, startup failure, action required; a commit status's
	// failure or error), by name. Superseded cancellations are excluded.
	Red []Check
	// Checks are the names of the non-superseded checks on Head.
	CheckStates []string // per-check and workflow conclusions, for progress observation
	Checks      []string
	Reviews     []Review // oldest first
	// Author is the login that opened it; Title, Body, Branch (its head branch)
	// and Files describe the change. ChangedFiles is the total count; gh
	// may list only some paths. Ownership exclusions use Ref alone.
	Author, Title, Body, Branch string
	Files                       []string
	ChangedFiles                int
	Additions, Deletions        int
	// Team is the team the team files say owns its repository, "" when
	// none does.
	Team string
	// Updated is when GitHub last saw activity on it (a push, a review, a
	// comment): the sweep counts a stretch without an owner from no earlier.
	Updated time.Time
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
	// Commit is the head GitHub attached the review to: the pull request's
	// head when it was posted, unless its poster named another.
	Commit string
	// Member is a review by a member or collaborator of the repository's
	// owner: only such a review is the owning team's.
	Member  bool
	Head    string
	Verdict string
	Body    string
	At      time.Time
	// Replaced is a review of a head its author replaced before the review
	// came (Input.replaced): no round's verdict.
	Replaced bool
}

// Worker is an agent on the roster.
type Worker struct {
	// Done affects observation only; Plan continues to use its existing events.
	Done bool
	// Issues are all active serve records.
	Issues []string
	// Pulls are the pull requests and primary task items (owner/repo#n) the
	// worker's active task, serve record, publication or review park owns.
	Pulls []string
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
	// Waits is what the agent's park says it waits for.
	Waits string
	// Saved is a saved review park whose agent left the roster: it keeps
	// the review flow going, and owns nothing (the sweep's live owner).
	Saved bool
	// Mentions are the issues and pull requests (owner/repo#n, lower case)
	// the agent's task names.
	Mentions []string
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
	PullReadAt map[string]time.Time
	OpenPulls  []string
	// ClosedIssues contains only successfully read, closed issues.
	ClosedIssues map[string]bool
	Workers      []Worker
	// Pulls are the tracked pull requests (Tracked) by ref.
	Pulls map[string]Pull
	// ProblemPulls are pull requests observed solely to classify unsettled
	// worker problem reports without entering review, overlap or sweep planning.
	ProblemPulls map[string]Pull
	// Events are the log's events since the loop's first start.
	Events []Event
	Notes  []Note
	// Release is a newer release's version, "" when there is none.
	Release string
	// Taken are the actions already taken, oldest first.
	Taken []Taken
	// Rounds is supervisor.reviewRounds. Plan no longer reads it: a pull
	// request gets one desk review whatever it says (MaxReviews).
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
	// the gate's devctl pr merge, on any other gh pr merge
	// --match-head-commit.
	DevctlOwners []string
	// MergeMethods selects each repository's merge method; unset means squash.
	MergeMethods map[string]string
	// Admin is merge.admin: the repositories (owner/repo) whose go names
	// gh pr merge --admin --match-head-commit to merge past their branch
	// ruleset after the approved review.
	Admin []string
	// BaseMerged reports whether p's head only brings its base into
	// approved, an earlier head the loop's go covers: it merges the base,
	// resolving the merge's conflicts or not, or rebases onto it
	// (github.ApprovalCovers); nil: never, every new head waits for a park
	// before its go.
	BaseMerged func(p Pull, approved string) bool
	// NitsChecked is an explicit supervisor attestation against the approved head.
	NitsChecked func(p Pull, approved string) bool
	// Open reports whether an issue or pull request (owner/repo#n) is
	// open; one it cannot read counts as open. nil: none is, so a merged
	// pull request's worker is retired as before.
	Open func(ref string) bool
	// StaleAfter is agents.staleAfter: a worker kept for the open work it
	// serves goes once it stayed quiet this long with no park or wake
	// timer (0: a day).
	StaleAfter time.Duration
	// Board reports whether a project board is configured: when true,
	// issues the desk files land on it in Backlog (KindIntake).
	Board bool
	// Filed are the issues sessions on the desk created (gh issue create,
	// create_issue, issue_write), read from their transcripts.
	Filed []FiledIssue
	// Ownerless is since when each open pull request of Ownerless has had
	// no live owner, as the loop saw it: the sweep gives one past
	// OrphanWait.
	Ownerless map[string]time.Time
	// Elsewhere are the pull requests of Ownerless that another desk's
	// worker opened, by ref: that desk's live claim on an issue each serves
	// (PullIssues), in its words. Such a worker parks on its own desk, so a
	// green head awaiting its first review is its request for this desk's
	// review, which the sweep starts without OrphanWait.
	Elsewhere map[string]string
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

// Action is one transition to take.
type Action struct {
	Key   string
	Kind  string
	Ref   string // the pull request, when there is one
	Block string // the overlapping pull request or issue waiting for Ref
	// OwnerIssue and BlockIssue are the issue endpoints of an overlap hold.
	// Empty when either PR closes no issue or both resolve to the same issue.
	OwnerIssue string
	BlockIssue string
	URL        string
	Agent      string // the worker it concerns
	Head       string
	Round      int
	// RepairAgent is the reviewer responsible for conflicts and red CI after approval.
	// It does not change the delivery owner or authorize another review.
	RepairAgent string
	// The verdict the merge decision observed. A newer change request must
	// force replanning before the loop records its exact-head authorization.
	ReviewID      string
	ReviewVerdict string
	// SelfHosted asks for a reviewer on no self-hosted provider.
	SelfHosted bool
	// Message is what the worker or the guide is told.
	Message string
	// Question is an ownership decision's question for
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
	// New loops execute a merge; old go records remain readable for recovery.
	if kind == KindMerge {
		kind = KindGo
	}
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

// prRef matches owner/repo#n.
var prRef = regexp.MustCompile(`^[\w.-]+/[\w.-]+#\d+$`)

// IsPull reports whether a park's On names an issue or pull request.
func IsPull(on string) bool { return prRef.MatchString(on) }

// Tracked are the pull requests a pass reads: every verified review request
// an agent is parked on, every overlapping pull request another worker is
// parked on, every one the loop started a review of that it
// has not seen merged or closed, and every ended one whose worker waits
// for its retirement. None outside the
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
	fresh, stale := parks(workers, l, desk)
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
		requestedAgain := slices.ContainsFunc(workers, func(w Worker) bool {
			return w.ParkedOn == ref && w.ReviewRequested && w.ParkedAt.After(l.keys[endKey(ref)].At) && fresh[ref]
		})
		if !l.ended[ref] || requestedAgain || l.workerDue(ref, workers) {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// parks are the pull requests workers are parked on: fresh those parked
// on since the desk's loop first started, stale those parked on before.
func parks(workers []Worker, l ledger, desk Desk) (fresh, stale map[string]bool) {
	fresh, stale = map[string]bool{}, map[string]bool{}
	for _, w := range workers {
		if !IsPull(w.ParkedOn) || (!w.ReviewRequested && !waitsOnOtherPull(w, l)) {
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
	in = in.markReplaced(l)
	// Report pairing is local to an agent. Index once instead of walking
	// the entire retained event history for every completed worker.
	eventsByAgent := make(map[string][]Event)
	for _, e := range in.Events {
		eventsByAgent[e.Agent] = append(eventsByAgent[e.Agent], e)
	}
	var out []Action
	plannedKeys := make(map[string]bool)
	add := func(a Action) {
		if l.has(a.Key) {
			return
		}
		if !plannedKeys[a.Key] {
			plannedKeys[a.Key] = true
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
		if !w.Saved {
			started[w.Name] = w.Started
		}
	}
	// retire takes a session the loop is done with off the roster; one
	// its person started, one gone already, or one whose own done the
	// loop takes, stays as it is.
	retire := func(ref, who, agent, why string) {
		reported := slices.ContainsFunc(eventsByAgent[agent], func(e Event) bool { return e.Verb == VerbDone })
		if started[agent] && !reported {
			add(Action{Key: retireKey(ref, who), Kind: KindDone, Ref: ref, Agent: agent, Message: why})
		}
	}
	// leave says once why the loop leaves ref alone.
	leave := func(ref, url, agent, reason, why string) {
		add(Action{Key: leaveKey(ref, reason), Kind: KindLeave, Ref: ref, URL: url, Agent: agent, Message: why})
	}
	// A park the loop leaves alone without reading its pull request.
	fresh, _ := parks(in.Workers, l, in.Desk)
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
	swept := sweptBy(in.Taken)
	absorbed := absorbedByMerged(in)
	refs := make([]string, 0, len(in.Pulls))
	for ref := range in.Pulls {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		by := absorbed[strings.ToLower(ref)]
		if by == "" {
			continue
		}
		p := in.Pulls[ref]
		p.Ref = cmp.Or(p.Ref, ref)
		why := fmt.Sprintf("%s was absorbed by merged combining pull request %s", cmp.Or(p.URL, ref), by)
		for _, w := range in.Workers {
			if w.Saved || w.Name == "" {
				continue
			}
			if strings.EqualFold(w.Name, PullWorkerName(ref)) ||
				(l.reviewer[ref] != "" && strings.EqualFold(w.Name, l.reviewer[ref])) ||
				(strings.EqualFold(w.ParkedOn, ref) && in.serving(w, ref, in.Now) == "") {
				retire(ref, retiredWorker, w.Name, why)
			}
		}
		for i := range l.rounds[ref] {
			retire(ref, fmt.Sprintf("reviewer r%d", i+1), ReviewerName(ref, i+1), why)
		}
		if !l.ended[ref] {
			if _, isParked := parked[ref]; len(l.rounds[ref]) > 0 || isParked {
				add(Action{Key: endKey(ref), Kind: KindEnd, Ref: ref, URL: p.URL, Message: "absorbed by " + by})
			}
		}
	}
	for _, ref := range refs {
		if absorbed[strings.ToLower(ref)] != "" {
			continue
		}
		p := in.Pulls[ref]
		p.Ref = cmp.Or(p.Ref, ref)
		if p.State == Open && l.ended[ref] {
			l.reopen(ref)
			delete(swept.lastAct, ref)
			for key := range swept.sweeps {
				if strings.HasPrefix(key, ref+"@") {
					delete(swept.sweeps, key)
				}
			}
		}
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
		ask, asked := l.asked[ref]
		if p.State != Open {
			if (p.State == Merged || p.State == Closed) && len(l.rounds[ref]) > 0 {
				add(Action{Key: endKey(ref), Kind: KindEnd, Ref: ref, URL: p.URL, Message: strings.ToLower(p.State)})
			}
			continue
		}
		planPull(in, l, ref, p, parked, ask, asked, leave, add)
		planOverlap(in, l, ref, p, parked, func(r string) bool {
			return slices.ContainsFunc(out, func(a Action) bool { return strings.EqualFold(a.Ref, r) })
		}, add)
		// The sweep acts on what nothing above acted on.
		if !slices.ContainsFunc(out, func(a Action) bool { return strings.EqualFold(a.Ref, ref) && a.Kind != KindLeave }) {
			planSweep(in, l, swept, ref, p, leave, add)
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
		return slices.ContainsFunc(eventsByAgent[r.Agent], func(e Event) bool { return e.Verb == VerbDone && near(e, r) })
	}
	// forwarded is a problem of the reviewer of one of the loop's rounds
	// that matches the changes request of its round the loop forwarded
	// (or forwards in this pass): the worker has it in the review, word for
	// word.
	forwarded := func(e Event) bool {
		// A complete current description explicitly asks for filing, even
		// when some of its words also occur in a forwarded review.
		if _, ok := findings.Parse(problemText(e)); ok {
			return false
		}
		for ref, rs := range l.rounds {
			p, tracked := in.Pulls[ref]
			for i, rd := range rs {
				if ReviewerName(ref, i+1) != e.Agent || !tracked {
					continue
				}
				v := p.verdict(rd, l.pushed(p))
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
		for _, o := range eventsByAgent[e.Agent] {
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
	progress := func(e Event) bool {
		// An explicit current finding is placed even when it names the
		// worker's own issue, and individually before a done report settles.
		if _, ok := findings.Parse(problemText(e)); ok {
			key := findingKey(e)
			if !l.has(key) {
				ev := e
				add(Action{Key: key, Kind: KindFile, Agent: e.Agent, Event: &ev, Message: problemText(e)})
			}
			return true
		}
		key := progressKey(e)
		if l.has(key) || l.has(eventKey(KindProgress, e)) {
			return true
		}
		isPaired := paired(e)
		if problemAlreadyFiled(l, eventsByAgent[e.Agent], e, near, isPaired) {
			return true
		}
		if !ownDeliveryProblem(in, l, e, isPaired) {
			return false
		}
		ev := e
		add(Action{Key: key, Kind: KindProgress, Agent: e.Agent, Event: &ev, Message: problemText(e)})
		return true
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
				awaitingPull := false
				for _, p := range eventsByAgent[e.Agent] {
					if p.Verb != VerbProblem || !near(p, e) || forwarded(p) || progress(p) {
						continue
					}
					if problemAwaitsPull(in, l, p, true) {
						awaitingPull = true
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
				if awaitingPull {
					continue
				}
				fk := eventKey(KindFile, e)
				if len(unfiled) > 0 && !l.has(fk) {
					add(Action{Key: fk, Kind: KindFile, Agent: e.Agent, Event: &ev, Message: strings.Join(unfiled, "\n")})
					continue
				}
				pendingFinding := slices.ContainsFunc(eventsByAgent[e.Agent], func(p Event) bool {
					_, explicit := findings.Parse(problemText(p))
					return p.Verb == VerbProblem && near(p, e) && explicit && !l.has(findingKey(p))
				})
				if pendingFinding {
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
			for _, p := range eventsByAgent[e.Agent] {
				if p.Verb == VerbProblem && near(p, e) && !forwarded(p) && !progress(p) && Filed(p.Detail) {
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
			if !paired(e) && !forwarded(e) && !progress(e) && !problemAwaitsPull(in, l, e, false) {
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
	// Keep canonical keys throughout planning: pending forwards are compared
	// with event findings before actions receive their durable episode suffix.
	for i := range out {
		if episode := l.episode[out[i].Ref]; episode != "" {
			out[i].Key += episodeTag + episode
		}
	}
	return out
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
