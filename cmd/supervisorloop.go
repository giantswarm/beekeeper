package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// loopParty is the party the supervisor loop acts and logs as.
var loopParty = state.Party{Name: "supervisor loop"}

// loopStartVerb logs each start of the loop; the first one bounds the events
// it acts on.
const loopStartVerb = "supervisor.loop"

// loopClaimVerb logs an action the loop is about to take, before its side
// effect; loopStageVerb a finished stage of a release.
const (
	loopClaimVerb    = "supervisor.claim"
	loopClaimEndVerb = "supervisor.unclaim"
	loopStageVerb    = "supervisor.stage"
	loopFailureVerb  = "supervisor.failure"
)

// loopEventVerbs are the events the loop's rules act on.
var loopEventVerbs = []string{superloop.VerbDone, superloop.VerbReport, superloop.VerbProblem, superloop.VerbPark}

// beekeeperRun runs this beekeeper binary with args for the loop: its
// output (stdout, then stderr) and exit code. An error is a run that did
// not exit.
var beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", 0, err
	}
	out, errOut, code, err := runArgv(ctx, "", append([]string{bin}, args...))
	return out + errOut, code, err
}

// loopCommand runs a configured command (the classifier, the post-install)
// with stdin: its stdout, its stderr and its exit code.
var loopCommand = runArgv

// runArgv runs argv with stdin: its stdout, its stderr and its exit code.
func runArgv(ctx context.Context, stdin string, argv []string) (string, string, int, error) {
	if len(argv) == 0 {
		return "", "", 0, errors.New("no command")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the loop's own binary or a configured command
	c.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errOut.String(), ee.ExitCode(), nil
	}
	return out.String(), errOut.String(), 0, err
}

// replaceable is the running binary as the loop sees it: whether an
// install renamed another file over its path, and its re-execution.
type replaceable interface {
	Replaced() bool
	Exec(env ...string) error
}

// loopBinary is the binary the loop runs; nil where it cannot tell.
var loopBinary = func() replaceable {
	if b := platform.RunningBinary(); b != nil {
		return b
	}
	return nil
}

// loopBroker puts the sandbox's broker onto the binary installed at the
// loop's path and says so: errLoopLater while it serves calls a restart
// would end (as self-update leaves it, its restart due).
var loopBroker = func(ctx context.Context) (string, error) {
	pid, bin, err := brokerProcess()
	if err != nil {
		return "no broker runs", nil //nolint:nilerr // no broker unit: nothing to put on the new binary
	}
	exe, err := os.Executable()
	if b := platform.RunningBinary(); b != nil {
		exe, err = b.Path, nil
	}
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	t, err := proc.Read()
	if err != nil {
		return "", err
	}
	switch step, calls := brokerDue(pid, bin, exe, fi.ModTime(), t); step {
	case brokerBusy:
		return fmt.Sprintf("the broker serves %d call(s)", calls), errLoopLater
	case brokerRestart:
		if err := restartBroker(ctx); err != nil {
			return "", fmt.Errorf("restarting the broker: %w", err)
		}
		return "the broker restarted on " + exe, nil
	}
	return "the broker runs the installed binary", nil
}

// readLoopPull reads a tracked pull request (owner/repo#n): its state, head,
// the checks on the head and its reviews.
var readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
	r, ok := parseRef(ref)
	if !ok {
		return superloop.Pull{}, fmt.Errorf("%s names no pull request", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "pr", "view", strconv.Itoa(r.N), "--repo", r.Repo, "--json", "state,url,headRefOid,baseRefName,statusCheckRollup,reviews,author,title,headRefName,files,changedFiles")
	if err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", r.N, r.Repo, err)
	}
	return parseLoopPull(ref, out)
}

// readBaseMerge reads whether p's head only merges its base into approved
// (github.BaseMergeOnly).
var readBaseMerge = func(ctx context.Context, p superloop.Pull, approved string) (bool, error) {
	r, ok := parseRef(p.Ref)
	if !ok || p.Base == "" {
		return false, fmt.Errorf("%s names no pull request with a base", p.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	only, _, err := github.BaseMergeOnly(ctx, github.RunGH, r.Repo, p.Base, approved, p.Head)
	return only, err
}

// readDeskLogin is the login gh acts as: the one the desk's agents open
// their pull requests as, unless supervisor.ownership.authors names them.
var readDeskLogin = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", errors.New("gh api user answers no login")
	}
	return login, nil
}

// readTeams reads supervisor.ownership.teamFiles: the team that owns each
// repository (owner/name, lower-cased).
var readTeams = func(ctx context.Context, own config.Ownership) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	repo, dir := own.TeamFilesRepo()
	return github.Teams(ctx, github.RunGH, repo, dir)
}

// teamsEvery is how often the loop reads the team files again.
const teamsEvery = time.Hour

// loopPullJSON is gh pr view's answer.
type loopPullJSON struct {
	State  string `json:"state"`
	URL    string `json:"url"`
	Head   string `json:"headRefOid"`
	Base   string `json:"baseRefName"`
	Rollup []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
	Author loopLogin `json:"author"`
	Title  string    `json:"title"`
	Branch string    `json:"headRefName"`
	Files  []struct {
		Path string `json:"path"`
	} `json:"files"`
	ChangedFiles int `json:"changedFiles"`
	Reviews      []struct {
		ID          string    `json:"id"`
		Author      loopLogin `json:"author"`
		Association string    `json:"authorAssociation"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		At          time.Time `json:"submittedAt"`
		Commit      struct {
			OID string `json:"oid"`
		} `json:"commit"`
	} `json:"reviews"`
}

// loopLogin is a GitHub account as gh pr view names it.
type loopLogin struct {
	Login string `json:"login"`
}

// parseLoopPull reads gh pr view's JSON of ref.
func parseLoopPull(ref string, out []byte) (superloop.Pull, error) {
	var j loopPullJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view of %s: %w", ref, err)
	}
	p := superloop.Pull{Ref: ref, URL: j.URL, State: j.State, Head: j.Head, Base: j.Base, Green: len(j.Rollup) > 0, Author: j.Author.Login, Title: j.Title, Branch: j.Branch, ChangedFiles: j.ChangedFiles}
	for _, f := range j.Files {
		p.Files = append(p.Files, f.Path)
	}
	for _, c := range j.Rollup {
		// A check run reports a status and a conclusion, a commit status
		// a state.
		ok := c.State == "SUCCESS" || c.Status == "COMPLETED" && slices.Contains([]string{"SUCCESS", "NEUTRAL", "SKIPPED"}, c.Conclusion)
		if !ok {
			p.Green = false
		}
	}
	for _, r := range j.Reviews {
		member := slices.Contains([]string{"MEMBER", "OWNER", "COLLABORATOR"}, r.Association)
		p.Reviews = append(p.Reviews, superloop.Review{ID: r.ID, Author: r.Author.Login, Member: member, Commit: r.Commit.OID, Head: superloop.ReviewedHead(r.Body, r.Commit.OID), Verdict: superloop.Verdict(r.State, r.Body), Body: r.Body, At: r.At})
	}
	slices.SortStableFunc(p.Reviews, func(x, y superloop.Review) int { return x.At.Compare(y.At) })
	return p, nil
}

func (a *app) supervisorLoopCmd() *cobra.Command {
	var once, dryRun bool
	c := &cobra.Command{
		Use:   "loop [--once] [--dry-run]",
		Short: "Run the supervisor's fixed procedure as a deterministic loop over beekeeper's events: no supervisor session needed",
		Long: `loop takes the supervisor's routine transitions without a supervisor session,
every supervisor.loopInterval (30s; --once: one pass), as the party
"supervisor loop". It is silent while nothing happens and prints and logs
one line per transition (supervisor.<kind>, the pull request its subject,
its key first in the detail):

  alerts    consume the beside-running watch events; the watch reads
            installations every alerts.every; unowned pages are one summary
            (installations, counts, oldest firing). A guide.person note requires
            blocked owned work: an active sessions serve record naming an issue
            and --waits alert:<name>, with no routable parked worker. Notes are
            deduplicated per installation and firing. A worker parked
            on alert:<name> (alertname or installation/alertname) is woken;
            an unresumable session is retired and its original task staffed
            again, with a durable replacement intent across restarts.
            Each worker is woken once per firing, including an already-firing
            alert. Event wakes are held with starts: an alert that resolves
            before the hold lifts does not wake a parked worker.
  budget    every poll consumes watch budget readings; a low or unknown budget
            holds new worker/reviewer starts and event wakes and says so
  timers    due wall-clock and conditional timers send their wakes or run
            their commands, as in watch; line-only reminders stay with watch
  tidy      every board.tidyEvery (1h) while a board is configured and the
            GitHub budget is above its floor: board tidy, its lines printed
            as SUPERVISOR BOARD <line>; a dry run tidies nothing
  leave     a park the desk's ownership leaves alone (below), said once with
            its reason: no reviewer, no go, no forward, no escalation, and
            a reviewer the loop started on it earlier retires
  resume    a settled park whose non-Claude harness reports its session gone:
            restart under
            the same name with supervisor.workerBrief and a capacity pick,
            continuing its open task and the wait's answer; a failed start
            releases the ended task holder so board next can offer it again.
            Claude transcript resumes, live sessions, managed SSH executions and a
            stopped desk are left alone
  review    an agent parked on its pull request (agents park --on
            owner/repo#n) with every check on the head green, for the
            first round, for every new head the worker parked on after an
            approval (one review per head) but one that only merges the
            base into the approved head (below), or after the last round's
            findings were forwarded and the worker parked again on a new
            head while a change-request round is left
            (supervisor.reviewRounds, default 1): a reviewer starts
            (agents start --review) with supervisor.reviewBrief or the
            built-in brief, on capacity's pick (--harness auto with
            capacity.choices), decided like any start; a worker on a
            self-hosted provider's model gets one on no self-hosted
            provider (--review-of-self-hosted). With supervisor.hosts
            (execution hosts, agents.hosts) the start goes to the first
            host whose own check (capacity --host) has room for its kind
            and that maps the repository, pinned to its default branch;
            the local machine keeps starts while it has room, unless
            supervisor.hostsFirst, which prefers hosts always. No host
            with room and no room locally: the start waits. A desk with
            no capacity.choices never places remotely
  go        every check on the head green, and either the last round's
            verdict approves that head, or the worker parked after the
            approval on a head that only merges the base into the approved
            one, or no round is left, its verdict asked for changes, they
            were forwarded and the worker parked again on a new head (its
            fixes) that is more than a merge of the base: the merge go for
            that head, told to the worker (agents wake) with the merge
            command of the repository's path: the gate's devctl pr merge
            <repo> <n> --update-branch on a repository of
            merge.devctlOwners, whose base update the go covers, else gh
            pr merge --match-head-commit <head>. A head only merges the
            base when the approved head is its ancestor, every commit it
            adds is a merge, and its change against its merge base with
            the base branch is the approved head's, file by file, hunk by
            hunk, line by line (compare API); a file without a patch counts
            only with the approved blob and no change of the base's to it
            since. Anything else, or a comparison GitHub cuts short, is a
            new head with a new review. Only the verdict of the loop's own
            last round counts, on the head it reviewed and posted after it
            started: an older verdict or a round still running authorizes
            nothing
  forward   the last round's verdict asks for changes: the review, word
            for word, to the worker, whatever its head is now
  rework    the worker parked again after the forward on the head the
            findings were about with no change-request round left
            (supervisor.reviewRounds counts the rounds that asked for
            changes; an approved head is none), or, rounds left or not, on
            a head that only merges the base into it, which carries no
            fixes: the worker is told to fix them and push (agents wake),
            once for each such park; its new green head gets the next
            round, or the merge go once none is left. Review and rework
            stay in the loop: no merge question goes to guide.person
  answer    the person answered an escalation an earlier loop filed
            (beekeeper answer --note): an answer that releases it (the
            Merge option, or words to merge that refuse nothing) is the
            merge go for the head the escalation named, once its checks
            are green; any other answer goes to the worker word for word,
            and the rounds go on. One nobody answered holds nothing
  relay     another team's pull request: each review its owning team
            posted after the worker parked, told to it word for word
  ownership another team's pull request whose worker waited
            supervisor.ownership.ownerWait (24h) since its park with no
            review of the owning team and no approval of theirs: a decision
            for guide.person (note add, options Override and Wait); the loop
            leaves it alone until the person answers
  override  the person's answer Override (the option, or a bare override):
            that one pull request is the desk's from then on, reviewed and
            given the go as its own team's; any other answer goes to the
            worker word for word
  end       a reviewed pull request merged or closed: its tracking ends
  done      agents.done: the agent leaves the roster (agents remove);
            the loop retires a session beekeeper started the same way,
            once its turn ends: a reviewer once the loop acted on its
            round's verdict (a go, forward, escalation, next round or
            end), and the worker of a pull request the loop gave the go
            for once it merged, reported or not. A managed SSH execution
            stops (agents stop) and leaves the roster only once the host
            acknowledged its termination. A session its person
            started is never retired
  next      after a done, and every pass below capacity.floor while capacity
            has room: the next item of board
            next starts (agents start, --harness auto with
            capacity.choices) with supervisor.workerBrief; none
            configured, none starts. With supervisor.hosts (execution
            hosts, agents.hosts) the start goes to the first host whose
            check (agents hosts check, capacity --host) has room and that
            maps the item's repository, pinned to its default branch; the
            local machine keeps the start while it has room, unless
            supervisor.hostsFirst, which prefers hosts always. No host
            with room and no room locally: the start waits. Releases
            and the loop itself stay on the coordinator. One board read
            answers a pass; nothing free (board next's exit 3 with no
            pick) settles a done's start, never a failure, and no read
            runs while the GitHub budget is under github.floor
  release   a newer release, in stages a later pass resumes until the
            last holds: self-update (verified, installed) while --check
            finds it newer, the sandbox's broker on the installed binary
            (restarted, or waited for while it serves calls),
            supervisor.postInstall once, then self-update --check must
            find it the latest. A loop whose binary was replaced
            re-executes the installed one at its next pass
  classify  a report or park the rules cannot place: agents.report without
            agents.done; a park on nothing of an agent still parked
            on nothing, with no timer that wakes it (timer add --wake), no
            note its wait names (#12) and no open note of its own. The
            supervisor.classifier command, given the event as JSON on
            stdin, answers {"class":"needs-person|blocked|failed|question",
            "confidence":0.9}; the event goes to the guide (note add for
            guide.person) with its class, or unclassified below
            supervisor.classifierConfidence (0.8) or without a classifier
  file      a problem found (agents.problem) that cites no issue URL:
            the full report is logged in supervisor.file and delivered once
            by guide watch as GUIDE PROBLEM, to file in its owning repository.
            A done report's problems arrive together before the done settles.
            No decision note or thread reference is needed. A problem naming
            its issue is already filed; the agent keeps its task
  question  a worker's open note for someone other than guide.person,
            asked before the loop started or after: the note is
            re-addressed to guide.person, into the guide's queue

Ownership decides what the loop does with a parked pull request, before any
review, go, forward or escalation:

  - outside github.allowedOrgs: nothing at all, not even a read
  - in supervisor.ownership.never, or its title, head branch or a changed
    path names a repository there: nothing, whatever else says
  - parked before the loop's first start: nothing; a park from before is
    no request for its review
  - opened by a login outside supervisor.ownership.authors (default: the
    login gh acts as): nothing
  - the desk's team's repository (supervisor.ownership.team, as the team
    files of supervisor.ownership.teamFiles name it, read every hour), one
    supervisor.ownership.review names, or one the person overrode: review
    and go as above
  - another team's repository, or one no team file declares: that team
    reviews; relay and ownership above
  - no supervisor.ownership.team: every repository of the allowed orgs is
    the desk's
A pass reads no pull request while it cannot tell the desk's login; while
it has never read the team files it acts only on review-list and
overridden pull requests.

The briefs are Go templates over .URL, .Repo, .Number, .Head, .Round and
.Worker. A pull request is read (one gh pr view) only while an agent is
parked on it or the loop reviews it. Every transition happens once: a
restarted loop reads its own log lines back and takes no key twice; a
failed one is tried again next pass, its failure printed once. An action
with a side effect is claimed in the log first (supervisor.claim): one
claimed and not logged taken, after an interruption, is taken only when
the log shows no line of its command (agents start, agents wake, agents
remove, note add) since the claim. One loop runs per state: a second is
refused. The loop acts on the events logged since its first start.
--dry-run is one pass that prints each transition it would take as
SUPERVISOR WOULD <KIND> and takes none: it runs read-only queries, logs nothing,
fires no timer and takes no lock, so it can run beside the loop.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun {
				return a.dryRunSupervisorLoop(cmd.Context())
			}
			return a.runSupervisorLoop(cmd.Context(), once)
		},
	}
	c.Flags().BoolVar(&once, "once", false, "one pass, then exit")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "one pass that prints the transitions it would take and takes none: read-only capacity and board queries, no log line or timer")
	return c
}

// supervisorLoop is one run of the loop: what it said once.
type supervisorLoop struct {
	a     *app
	since time.Time
	// said are the failures already printed, by key.
	said map[string]bool
	// releaseAt is when the loop last looked for a release.
	releaseAt  time.Time
	startsHeld bool
	stopWatch  func()
	// tidyAt is when the loop last ran board tidy.
	tidyAt time.Time
	// stages are the release stages finished, by key.
	stages map[string]bool
	// fillPause defers an unproductive board read until capacity changes,
	// an agent is done or removed, or the configured retry interval passes.
	fillPause fillPause
	// boardNone is this pass's empty or failed board next answer: every
	// later start of the pass takes it instead of walking the board again.
	boardNone *loopBoardNone
	// timerActs are timer side effects; shutdown and re-exec await them.
	timerActs sync.WaitGroup
	// login is the login gh acts as, once read; teams the team files'
	// owners, read at teamsAt.
	login   string
	teams   map[string]string
	teamsAt time.Time
	// dryRun prints a pass's transitions and takes none.
	dryRun bool
	// baseMerges are the answers of readBaseMerge, by pull request,
	// approved head and head: two commits' relation does not change.
	baseMerges map[string]bool
}

// loopOutput serializes loop transitions with asynchronous timer output.
type loopOutput struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *loopOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

func (a *app) runSupervisorLoop(ctx context.Context, once bool) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// One loop per state: a second would take each action again.
	lock := flock.New(filepath.Join(a.store.Dir(), loopLockFile))
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return refused("another supervisor loop runs on this state (%s is locked)", lock.Path())
	}
	defer func() { _ = lock.Unlock() }()
	// Observers read a PID marker instead of briefly contending for this lock.
	if err := a.store.WriteFile("supervisor-loop-pid.json", os.Getpid()); err != nil {
		return err
	}
	defer func() { _ = a.store.WriteFile("supervisor-loop-pid.json", 0) }()
	out := a.out
	a.out = &loopOutput{out: out}
	defer func() { a.out = out }()
	l, err := a.newSupervisorLoop(false)
	if err != nil {
		return err
	}
	defer l.timerActs.Wait()
	if !once {
		l.stopWatch = a.startLoopWatch(ctx)
		defer func() {
			if l.stopWatch != nil {
				l.stopWatch()
			}
		}()
	}
	for {
		a.now = time.Now()
		if err := l.pass(ctx); err != nil {
			return err
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.cfg.Supervisor.LoopInterval.Duration):
		}
	}
}

// loopLockFile is the lock one running loop holds, beside the state.
const loopLockFile = "supervisor-loop.lock"

// dryRunSupervisorLoop is one pass that says what it would take: it runs
// only read-only queries, logs nothing, fires no timer and takes no lock,
// so it can run beside the loop.
func (a *app) dryRunSupervisorLoop(ctx context.Context) error {
	l, err := a.newSupervisorLoop(true)
	if err != nil {
		return err
	}
	a.now = time.Now()
	return l.pass(ctx)
}

// newSupervisorLoop logs the loop's start, unless it is a dry run, and
// bounds its events by its first.
func (a *app) newSupervisorLoop(dryRun bool) (*supervisorLoop, error) {
	starts, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == loopStartVerb })
	if err != nil {
		return nil, err
	}
	l := &supervisorLoop{a: a, since: a.now.UTC(), said: map[string]bool{}, baseMerges: map[string]bool{}, dryRun: dryRun}
	if len(starts) > 0 {
		l.since = starts[0].At
	}
	if dryRun {
		return l, nil
	}
	if err := a.store.Record(event(loopParty, loopStartVerb, "started; acting on the events since %s", l.since.Format(time.RFC3339))); err != nil {
		return nil, err
	}
	return l, nil
}

// pass is one look: gather, plan, act.
func (l *supervisorLoop) pass(ctx context.Context) error {
	a := l.a
	l.boardNone = nil
	// A release installed over the loop's path runs from here on: an old
	// binary's saves are refused once the new one wrote the state.
	if b := loopBinary(); b != nil && !l.dryRun && b.Replaced() {
		if _, err := fmt.Fprintln(a.out, "SUPERVISOR LOOP its binary was replaced: re-executing it"); err != nil {
			return err
		}
		l.timerActs.Wait()
		if l.stopWatch != nil {
			l.stopWatch()
			l.stopWatch = nil
		}
		if err := b.Exec(); err != nil {
			l.sayOnce("reexec", "SUPERVISOR LOOP the new binary does not start (%v): carrying on", err)
			l.stopWatch = a.startLoopWatch(ctx)
		}
	}
	if !l.dryRun {
		if err := l.watchConditions(ctx); err != nil {
			l.startsHeld = true
			l.sayOnce("watch-conditions", "SUPERVISOR WATCH conditions failed; starts remain held: %v", err)
		} else {
			delete(l.said, "watch-conditions")
		}
	}
	if !l.dryRun {
		if err := l.watchAlerts(ctx); err != nil {
			l.sayOnce("alerts", "SUPERVISOR ALERTS FAILED: %v", err)
		} else {
			delete(l.said, "alerts")
		}
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if !l.dryRun {
		if err := l.fireTimers(ctx, st); err != nil {
			return err
		}
		l.tidy(ctx, st)
		if err := a.syncClaims(ctx); err != nil {
			l.sayOnce("claims", "SUPERVISOR LOOP claims on GitHub: %v", err)
		}
	}
	if err := l.recoverParks(ctx, st); err != nil {
		return err
	}
	st, err = a.store.Read()
	if err != nil {
		return err
	}
	own := a.cfg.Supervisor.Ownership
	in := superloop.Input{Rounds: a.cfg.Supervisor.ReviewRounds, Person: a.cfg.Guide.Person, Now: a.now,
		Desk: superloop.Desk{Since: l.since, Orgs: a.cfg.GitHub.AllowedOrgs, Authors: own.Authors, Team: own.Team,
			Review: own.Review, Never: own.Never, OwnerWait: own.OwnerWait.Duration},
		DevctlOwners: a.cfg.Merge.DevctlOwners, BaseMerged: func(p superloop.Pull, approved string) bool { return l.baseMerged(ctx, p, approved) }}
	for _, ag := range st.Agents {
		w := superloop.Worker{Name: ag.Name, SelfHosted: selfHostedModel(a.cfg.Capacity, ag.Model), Started: st.StartIndex(ag.Party) >= 0}
		if ag.Park != nil {
			w.ParkedOn, w.ParkedAt = loopParkOn(ag.Park), ag.Park.At
		}
		w.Waking = slices.ContainsFunc(st.Timers, func(t state.Timer) bool { return t.Wake != "" && strings.EqualFold(t.Wake, ag.Name) })
		in.Workers = append(in.Workers, w)
	}
	for _, n := range st.Notes {
		if n.Kind != noteLogin {
			by := n.By.Name
			if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(n.By) }); i >= 0 {
				by = st.Agents[i].Name
			}
			in.Notes = append(in.Notes, superloop.Note{ID: n.ID, For: n.For, By: by, Text: n.Text})
		}
	}
	taken, err := a.store.Follow("supervisor-loop-taken", func(e state.Event) bool { return strings.HasPrefix(e.Verb, "supervisor.") })
	if err != nil {
		return err
	}
	claims, stages := map[string]loopClaim{}, map[string]bool{}
	failures := map[string]loopFailure{}
	for _, e := range taken {
		if t, ok := superloop.ParseTaken(e.At, e.Verb, e.Subject, e.Detail); ok {
			in.Taken = append(in.Taken, t)
			continue
		}
		key, rest, ok := keyed(e.Detail)
		switch {
		case !ok:
		case e.Verb == loopClaimVerb:
			claims[key] = loopClaim{at: e.At, target: rest}
		case e.Verb == loopClaimEndVerb:
			delete(claims, key)
		case e.Verb == loopStageVerb:
			stages[key] = true
		case e.Verb == loopFailureVerb:
			var f loopFailure
			if json.Unmarshal([]byte(rest), &f) == nil {
				failures[key] = f
			}
		}
	}
	for _, t := range in.Taken {
		delete(claims, t.Key)
	}
	l.stages = stages
	since := l.since
	evs, err := a.store.Follow("supervisor-loop", func(e state.Event) bool {
		return slices.Contains(loopEventVerbs, e.Verb) && !e.At.Before(since)
	})
	if err != nil {
		return err
	}
	for _, e := range evs {
		in.Events = append(in.Events, superloop.Event{At: e.At, Verb: e.Verb, Agent: loopEventAgent(st, e), Detail: e.Detail})
	}
	// The person's answers to the loop's own notes: an escalation's answer
	// is the loop's to act on, no session's.
	answers, err := a.store.Follow("supervisor-loop-answers", func(e state.Event) bool { return e.Verb == answerRecorded })
	if err != nil {
		return err
	}
	for _, e := range answers {
		if an, ok := parseAnswered(e); ok && an.Asker == loopParty.Name {
			in.Answers = append(in.Answers, superloop.Answer{Ref: an.About, Note: an.ID, Question: an.Question, Text: an.Answer, By: an.For, At: an.At})
		}
	}
	in.Pulls = map[string]superloop.Pull{}
	tracked := superloop.Tracked(in.Workers, in.Taken, in.Answers, in.Desk)
	if len(tracked) > 0 && !l.ownership(ctx, &in.Desk) {
		tracked = nil
	}
	for _, ref := range tracked {
		p, err := readLoopPull(ctx, ref)
		if err != nil {
			l.sayOnce("read "+ref, "SUPERVISOR LOOP cannot read %s: %v", ref, err)
			continue
		}
		if r, ok := parseRef(ref); ok {
			p.Team = l.teams[strings.ToLower(r.Repo)]
		}
		in.Pulls[ref] = p
	}
	// A release claimed and not finished goes on, whatever --check says
	// now that it is installed.
	for key := range claims {
		if v, ok := strings.CutPrefix(key, superloop.KindRelease+" "); ok {
			in.Release = v
		}
	}
	if in.Release == "" && !l.dryRun && a.now.Sub(l.releaseAt) >= a.cfg.Supervisor.ReleaseEvery.Duration {
		l.releaseAt = a.now
		in.Release = l.newerRelease(ctx)
	}
	for _, act := range superloop.Plan(in) {
		if l.dryRun {
			if _, err := fmt.Fprintf(a.out, "SUPERVISOR WOULD %s %s\n", strings.ToUpper(act.Kind), superloop.Detail(act, would(act))); err != nil {
				return err
			}
			continue
		}
		if a.now.Before(failures[act.Key].RetryAt) {
			continue
		}
		if c, ok := claims[act.Key]; ok && act.Kind != superloop.KindRelease {
			// Claimed before an interruption: what the log shows of its
			// side effect decides, never a second one.
			what, done, err := l.reconcile(ctx, st, act, c)
			if err != nil {
				return err
			}
			if done {
				if err := l.record(act, what); err != nil {
					return err
				}
				continue
			}
		}
		if act.Kind == superloop.KindQuestion {
			if err := l.readdress(act); err != nil {
				if err := l.failed(act, failures[act.Key], err); err != nil {
					return err
				}
			}
			continue
		}
		what, err := l.act(ctx, st, act)
		switch {
		case errors.Is(err, errLoopLater):
			continue
		case err != nil:
			if err := l.failed(act, failures[act.Key], err); err != nil {
				return err
			}
			continue
		}
		if err := l.record(act, what); err != nil {
			return err
		}
	}
	return l.fill(ctx, st, claims, in.Taken)
}

// baseMerged reports whether p's head only merges its base into approved,
// read once per pair of heads; a read that fails is said once and counts as
// no: the new head gets its review.
func (l *supervisorLoop) baseMerged(ctx context.Context, p superloop.Pull, approved string) bool {
	key := p.Ref + " " + approved + " " + p.Head
	if ok, read := l.baseMerges[key]; read {
		return ok
	}
	ok, err := readBaseMerge(ctx, p, approved)
	if err != nil {
		l.sayOnce("base merge "+key, "SUPERVISOR LOOP cannot tell whether %s's head only merges its base: %v", p.Ref, err)
		return false
	}
	l.baseMerges[key] = ok
	return ok
}

// ownership readies the desk's policy for a pass's reads: the logins the
// desk opens its pull requests as and, once supervisor.ownership.team is
// set, the team files, read again every teamsEvery. False, said once, while
// the login is unknown: the pass reads no pull request, so nothing is
// reviewed on a guess. Team files never read leave the desk's own team's
// pull requests waiting (Desk.TeamsUnknown); ones that cannot be read
// again keep the last ones read.
func (l *supervisorLoop) ownership(ctx context.Context, d *superloop.Desk) bool {
	if len(d.Authors) == 0 {
		if l.login == "" {
			login, err := readDeskLogin(ctx)
			if err != nil {
				l.sayOnce("login", "SUPERVISOR LOOP cannot tell the login the desk opens pull requests as (%v): it reads none until it can", err)
				return false
			}
			l.login = login
		}
		d.Authors = []string{l.login}
	}
	own := l.a.cfg.Supervisor.Ownership
	if own.Team == "" || l.teams != nil && l.a.now.Sub(l.teamsAt) < teamsEvery {
		return true
	}
	teams, err := readTeams(ctx, own)
	switch {
	case err == nil:
		l.teams, l.teamsAt = teams, l.a.now
	case l.teams == nil:
		l.sayOnce("teams", "SUPERVISOR LOOP cannot read the team files %s (%v): until it can, it acts only on supervisor.ownership.review and overridden pull requests", own.TeamFiles, err)
		d.TeamsUnknown = true
	default:
		l.sayOnce("teams "+l.teamsAt.Format(time.RFC3339), "SUPERVISOR LOOP cannot read the team files %s again (%v): the ones read at %s stand",
			own.TeamFiles, err, l.teamsAt.Format(time.RFC3339))
	}
	return true
}

// would says what a dry run's act would do.
func would(act superloop.Action) string {
	switch act.Kind {
	case superloop.KindReview:
		return fmt.Sprintf("start reviewer %q for %s at %s, round %d", superloop.ReviewerName(act.Ref, act.Round), act.URL, act.Head, act.Round)
	case superloop.KindOwnership:
		return "ask the guide's person: " + act.Question
	case superloop.KindDone:
		return "take off the roster " + act.Message
	case superloop.KindNext:
		return "start the next board item"
	case superloop.KindRelease:
		return "install " + act.Version
	case superloop.KindClassify:
		return fmt.Sprintf("send its %s to the guide", act.Event.Verb)
	case superloop.KindGo, superloop.KindForward, superloop.KindRework, superloop.KindAnswer, superloop.KindRelay:
		return "tell: " + truncate(oneLine(act.Message), 300)
	}
	return truncate(oneLine(act.Message), 300)
}

// record logs act taken, which no pass takes again, and says it.
func (l *supervisorLoop) record(act superloop.Action, what string) error {
	detail := superloop.Detail(act, what)
	if err := l.a.store.Record(about(event(loopParty, "supervisor."+act.Kind, "%s", detail), act.Ref)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(l.a.out, "SUPERVISOR %s %s\n", strings.ToUpper(act.Kind), escapedLine(detail))
	return err
}

// loopClaim is a claim read back: when, and what its side effect names
// (the session started, the agent woken or removed, the note's question).
type loopClaim struct {
	at     time.Time
	target string
}

// keyed reads a loop line's detail: "[<key>] <rest>".
func keyed(detail string) (key, rest string, ok bool) {
	m := keyedLine.FindStringSubmatch(detail)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// keyedLine is a loop line's detail.
var keyedLine = regexp.MustCompile(`(?s)^\[([^\]]+)\] (.*)$`)

// claim logs act's side effect about to happen on target, before it does:
// a restart finds it and reconciles instead of acting twice.
func (l *supervisorLoop) claim(act superloop.Action, target string) error {
	return l.a.store.Record(about(event(loopParty, loopClaimVerb, "[%s] %s", act.Key, target), act.Ref))
}

// reconcile decides a claimed action from the log since its claim: done
// when the command it ran left its line (agents start, agents wake, agents
// remove, note add, all logged as the loop), else not, and it runs.
func (l *supervisorLoop) reconcile(ctx context.Context, st *state.State, act superloop.Action, c loopClaim) (string, bool, error) {
	evs, err := l.a.store.Events(0, func(e state.Event) bool { return e.By.Name == loopParty.Name && !e.At.Before(c.at) })
	if err != nil {
		return "", false, err
	}
	logged := func(verb string, match func(string) bool) bool {
		return slices.ContainsFunc(evs, func(e state.Event) bool { return e.Verb == verb && match(e.Detail) })
	}
	opens := func(d string) bool { return strings.HasPrefix(d, c.target+":") }
	var done bool
	switch act.Kind {
	case superloop.KindReview, superloop.KindNext:
		done = logged(verbStart, opens)
	case superloop.KindGo, superloop.KindForward, superloop.KindRework, superloop.KindAnswer, superloop.KindRelay:
		done = logged("agents.wake", opens)
	case superloop.KindDone:
		done = !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.target }) ||
			logged("agents.remove", func(d string) bool { return d == c.target })
	case superloop.KindEscalate, superloop.KindClassify, superloop.KindOwnership:
		done = logged("note.add", func(d string) bool { return strings.Contains(d, c.target) })
	}
	if !done {
		return "", false, nil
	}
	if act.Kind == superloop.KindReview {
		if err := l.a.assignExecutionReview(ctx, act); err != nil {
			return "", false, err
		}
	}
	return fmt.Sprintf("taken before an interruption: %s, as the log shows", c.target), true, nil
}

// readdress puts a worker's question into the guide's queue and logs it in
// one write.
func (l *supervisorLoop) readdress(act superloop.Action) error {
	person := l.a.cfg.Guide.Person
	detail := superloop.Detail(act, "re-addressed to the guide's person: "+act.Message)
	err := l.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Notes, func(n state.Note) bool { return n.ID == act.Note })
		if i < 0 {
			return nil, fmt.Errorf("note #%d is closed", act.Note)
		}
		st.Notes[i].For = person
		return []state.Event{event(loopParty, "supervisor."+act.Kind, "%s", detail)}, nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(l.a.out, "SUPERVISOR %s %s\n", strings.ToUpper(act.Kind), escapedLine(detail))
	return err
}

// loopFailure persists identical failures across passes and restarts. Two
// consecutive failures defer the action for five minutes, keeping it pending.
type loopFailure struct {
	Message string    `json:"message"`
	Count   int       `json:"count"`
	RetryAt time.Time `json:"retryAt"`
}

const loopFailureBackoff = 5 * time.Minute

func (l *supervisorLoop) failed(act superloop.Action, previous loopFailure, cause error) error {
	f := loopFailure{Message: cause.Error(), Count: 1}
	// Compare exact messages: a changing timestamp or PID is a different
	// failure and starts a new count, rather than hiding a new cause.
	if f.Message == previous.Message {
		f.Count = previous.Count + 1
	}
	if f.Count >= 2 {
		f.RetryAt = l.a.now.Add(loopFailureBackoff)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := l.a.store.Record(about(event(loopParty, loopFailureVerb, "[%s] %s", act.Key, b), act.Ref)); err != nil {
		return err
	}
	switch f.Count {
	case 1:
		_, err = fmt.Fprintf(l.a.out, "SUPERVISOR %s FAILED %s: %v\n", strings.ToUpper(act.Kind), act.Key, cause)
	case 2:
		_, err = fmt.Fprintf(l.a.out, "SUPERVISOR BACKOFF %s: identical failures; retry after %s\n", act.Key, f.RetryAt.UTC().Format(time.RFC3339))
	}
	return err
}

// errLoopLater is an action that waits for a later pass, quietly: no room
// for a start yet.
var errLoopLater = errors.New("later")

// sayOnce prints a failure once per key and run.
func (l *supervisorLoop) sayOnce(key, format string, args ...any) {
	if l.said[key] {
		return
	}
	l.said[key] = true
	_, _ = fmt.Fprintf(l.a.out, format+"\n", args...)
}

// newerRelease is a newer release's version, "" when this is the latest or
// the look failed.
func (l *supervisorLoop) newerRelease(ctx context.Context) string {
	out, code, err := beekeeperRun(ctx, selfUpdateCheck...)
	if err != nil || code != ExitOutdated {
		return ""
	}
	if m := newerLine.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// tidy runs board tidy every board.tidyEvery while a board is configured
// and the GitHub budget is above its floor, and says what it did.
func (l *supervisorLoop) tidy(ctx context.Context, st *state.State) {
	a := l.a
	b := a.cfg.Board
	if b.Owner == "" || b.Project == 0 || b.TidyEvery.Duration <= 0 || a.now.Sub(l.tidyAt) < b.TidyEvery.Duration ||
		lowBudget(st.Budget, a.cfg.GitHub.Floor, a.now) {
		return
	}
	l.tidyAt = a.now
	out, err := l.run(ctx, loopBoard, boardTidyName)
	if err != nil {
		l.sayOnce("board tidy", "SUPERVISOR BOARD TIDY FAILED: %v", err)
		return
	}
	delete(l.said, "board tidy")
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			_, _ = fmt.Fprintln(a.out, "SUPERVISOR BOARD "+line)
		}
	}
}

// boardTidyName is the board tidy subcommand.
const boardTidyName = "tidy"

// selfUpdateCheck is the look for a newer release.
var selfUpdateCheck = []string{selfUpdateCmdName, "--check"}

// newerLine is self-update --check's line for a newer release.
var newerLine = regexp.MustCompile(`(\S+) is newer than`)

// The subcommands the loop runs that no other constant names.
const (
	loopCapacity = "capacity"
	loopRemove   = "remove"
	loopStop     = bgStopCmd
	loopRecover  = "recover"
	loopBoard    = "board"
	loopAdd      = "add"
	loopNote     = "note"
	// The placement flags a remote start names on its agents start line.
	loopHostFlag = "--host"
	loopRepoFlag = "--repo"
	loopRefFlag  = "--ref"
)

// asFlag is the global flag that names the party a command acts as.
const asFlag = "as"

// as is a beekeeper command line the loop runs as itself.
func (l *supervisorLoop) as(args ...string) []string {
	out := []string{"--" + asFlag, loopParty.Name}
	if l.a.cfgPath != "" {
		out = append([]string{"--" + configFlag, l.a.cfgPath}, out...)
	}
	return append(out, args...)
}

// run runs a beekeeper command as the loop; a non-zero exit is an error
// with its output.
func (l *supervisorLoop) run(ctx context.Context, args ...string) (string, error) {
	out, code, err := beekeeperRun(ctx, l.as(args...)...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, &exitError{code: code, msg: fmt.Sprintf("beekeeper %s: exit %d: %s", strings.Join(args[:min(2, len(args))], " "), code, oneLine(out))}
	}
	return out, nil
}

// act takes one transition and says what it did.
func (l *supervisorLoop) act(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	a := l.a
	switch act.Kind {
	case superloop.KindReview:
		return l.startReviewer(ctx, act)
	case superloop.KindGo:
		for _, worker := range st.Agents {
			if worker.Name != act.Agent || worker.Execution == nil {
				continue
			}
			ref, ok := parseRef(act.Ref)
			if !ok {
				return "", errors.New("invalid remote merge pull request")
			}
			grant, err := a.issueExecutionMerge(ctx, *worker.Execution, ref.Repo, ref.N, act.Head, loopParty)
			if err != nil {
				return "", err
			}
			act.Message = fmt.Sprintf("Merge %s at reviewed head %s using: beekeeper agents merge %s %d --head %s --authorization %s. The grant expires at %s; if it expires, park for a new supervisor go.", act.URL, act.Head, ref.Repo, ref.N, act.Head, grant.ID, grant.ExpiresAt.Format(time.RFC3339))
		}
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("merge go for %s at %s, told", act.URL, act.Head), nil
	case superloop.KindForward:
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the review of %s asking for changes on %s forwarded", act.URL, act.Head), nil
	case superloop.KindRework:
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s at %s: the review's findings stand with no round left, rework told", act.URL, act.Head), nil
	case superloop.KindAnswer:
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the person's answer about %s told", act.URL), nil
	case superloop.KindRelay:
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the owning team's review of %s told", act.URL), nil
	case superloop.KindOwnership:
		if err := l.toGuide(ctx, act, act.URL, act.Question, act.Message,
			"its owning team's review blocks the desk's work, and only a person overrides the ownership of a pull request; the loop reviews it and gives the merge go as for the desk's own on an answer that overrides it",
			fmt.Sprintf("leave %s to its owning team's review; its worker stays parked", act.URL), act.Options...); err != nil {
			return "", err
		}
		return "to the guide: " + act.Message, nil
	case superloop.KindLeave, superloop.KindOverride:
		return act.Message, nil
	case superloop.KindEnd:
		return fmt.Sprintf("%s %s: tracking ends", act.URL, act.Message), nil
	case superloop.KindDone:
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent })
		if i < 0 {
			return "already off the roster", nil
		}
		// A retirement waits for the session's turn to end: its last
		// report is not cut off.
		if act.Event == nil && loopInTurn(a, st.Agents[i].Party) {
			l.sayOnce("turn "+act.Key, "SUPERVISOR DONE WAITS %s: its session runs a turn", act.Agent)
			return "", errLoopLater
		}
		// A managed SSH execution ends with a stop the host acknowledges:
		// removal is refused while its reservation stands. An unreachable
		// host fails the stop; the loop says so once and tries again.
		if ref := st.Agents[i].Execution; ref != nil {
			if r, _ := executionRecord(st, *ref); r == nil || !r.Terminal() {
				if _, err := l.run(ctx, agentsName, loopStop, act.Agent, "--reason", "supervisor loop: task done"); err != nil {
					return "", err
				}
			}
		}
		if err := l.claim(act, act.Agent); err != nil {
			return "", err
		}
		if _, err := l.run(ctx, agentsName, loopRemove, act.Agent); err != nil {
			return "", err
		}
		if act.Message != "" {
			return "off the roster: " + act.Message, nil
		}
		return "off the roster", nil
	case superloop.KindNext:
		return l.startNext(ctx, act)
	case superloop.KindRelease:
		return l.install(ctx, act)
	case superloop.KindClassify:
		return l.classify(ctx, st, act)
	case superloop.KindFile:
		return l.toFile(ctx, st, act)
	}
	return "", fmt.Errorf("no transition for %s", act.Kind)
}

// loopInTurn reports whether p's session runs a turn.
var loopInTurn = func(a *app, p state.Party) bool {
	_, ok := a.runningTurn(p)
	return ok
}

// wake tells act's worker its message (agents wake), claimed first.
func (l *supervisorLoop) wake(ctx context.Context, act superloop.Action) error {
	if err := l.claim(act, act.Agent); err != nil {
		return err
	}
	_, err := l.run(ctx, agentsName, agentWakeName, act.Agent, act.Message)
	return err
}

// briefData is what a brief template renders.
type briefData struct {
	URL, Repo, Head, Worker, Task string
	Number, Round                 int
}

// defaultReviewBrief is the review brief without supervisor.reviewBrief.
const defaultReviewBrief = `Review {{.URL}} at head {{.Head}}, review round {{.Round}}, the work of {{.Worker}}.

Read the pull request, the issue it closes and the diff. Judge whether it does what the issue asks, and find its bugs. Post one review on the pull request (gh pr review {{.Number}} --repo {{.Repo}} --comment --body-file <file>): its first line "Review: approved" or "Review: changes requested", its second "Head reviewed: ` + "`{{.Head}}`" + `", then every blocking finding with its file and line. Then report: beekeeper agents idle --done --report "<the same Review: verdict line, a newline, and the exact review URL>" --problem none (or --problem "<finding, evidence, owning repo>" for each problem the review met).
`

// brief renders the template in file (def when file is empty) with d into
// a file under the state's directory, named after key.
func (l *supervisorLoop) brief(file, def, key string, d briefData) (string, error) {
	text := def
	if file != "" {
		b, err := os.ReadFile(file) //nolint:gosec // the configured brief template
		if err != nil {
			return "", err
		}
		text = string(b)
	}
	t, err := template.New(filepath.Base(file)).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	dir := filepath.Join(l.a.store.Dir(), "supervisor-loop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, briefName.ReplaceAllString(key, "-")+".md")
	return path, os.WriteFile(path, b.Bytes(), 0o600)
}

// briefName is what a key may not keep in a file name.
var briefName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// startReviewer starts the reviewer of act's head.
func (l *supervisorLoop) startReviewer(ctx context.Context, act superloop.Action) (string, error) {
	if l.startsHeld {
		return "", errLoopLater
	}
	r, _ := parseRef(act.Ref)
	brief, err := l.brief(l.a.cfg.Supervisor.ReviewBrief, defaultReviewBrief, act.Key, briefData{URL: act.URL, Repo: r.Repo, Number: r.N, Head: act.Head, Round: act.Round, Worker: act.Agent})
	if err != nil {
		return "", err
	}
	place, err := l.startPlacement(ctx, r.Repo, true, act.SelfHosted)
	if err != nil {
		if errors.Is(err, errLoopLater) {
			l.sayOnce("room "+act.Key, "SUPERVISOR REVIEW WAITS %s: no room for a start yet on any host", act.URL)
			return "", errLoopLater
		}
		return "", err
	}
	name := superloop.ReviewerName(act.Ref, act.Round)
	args := []string{agentsName, agentStartName, name, brief, "--review"}
	if len(l.a.cfg.Capacity.Choices) > 0 {
		// The reviewer takes capacity's pick, decided as a worker's start
		// is; a self-hosted model's work gets one on no self-hosted provider.
		args = append(args, "--harness", harnessAuto)
		if act.SelfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	args = append(args, place.remoteStartFlags()...)
	if err := l.claim(act, name); err != nil {
		return "", err
	}
	if _, err := l.run(ctx, args...); err != nil {
		if Code(err) == ExitRefused {
			if place.remote() {
				// A refusal of a placed start on a host is either room or
				// an execution its interrupted start left reserved.
				if what, rerr := l.remoteStartRefused(ctx, name); rerr == nil {
					return what, nil
				} else if !errors.Is(rerr, errLoopLater) {
					return "", rerr
				}
			}
			l.sayOnce("room "+act.Key, "SUPERVISOR REVIEW WAITS %s: no room for a start yet (%v)", act.URL, err)
			return "", errLoopLater
		}
		return "", err
	}
	if err := l.a.assignExecutionReview(ctx, act); err != nil {
		return "", err
	}
	if place.remote() {
		return fmt.Sprintf("reviewer %q started for %s at %s, round %d, on %s", name, act.URL, act.Head, act.Round, place.Selector), nil
	}
	return fmt.Sprintf("reviewer %q started for %s at %s, round %d", name, act.URL, act.Head, act.Round), nil
}

// repoName is owner/repo's repo.
func repoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return name
}

// selfHostedModel reports whether model is a choice of a self-hosted
// provider; an omp agent's recorded provider/id is the choice that names
// its bare id.
func selfHostedModel(k config.Capacity, model string) bool {
	if model == "" {
		return false
	}
	for _, c := range k.Choices {
		if c.Model == model || c.Harness+"/"+c.Model == model || ompChoiceModel(c, model) {
			if p, ok := k.Providers[c.EffectiveProvider()]; ok && p.SelfHosted() {
				return true
			}
		}
	}
	return false
}

// startHost is one placement of a loop start: local with Selector "", else
// an execution host (agents.hosts) whose repository is mapped and whose
// ref to pin is Ref.
type startHost struct {
	Selector string
	Repo     string
	Ref      string
}

// remote reports whether the start runs on an execution host.
func (h startHost) remote() bool { return h.Selector != "" }

// roomOpen says whether a `capacity --json` answer leaves room for a
// start. An unreadable answer leaves the room open: the start judges it
// again itself, as before.
func roomOpen(out string) bool {
	var v struct {
		Room int `json:"room"`
	}
	if json.Unmarshal([]byte(out), &v) != nil {
		return true
	}
	return v.Room > 0
}

// capacityRoom reads one `capacity [--host <selector>] … --json` verdict:
// whether a start fits there. A run that failed or refused (a host that
// fails its check) has no room.
func (l *supervisorLoop) capacityRoom(ctx context.Context, args ...string) bool {
	out, code, err := beekeeperRun(ctx, l.as(append([]string{loopCapacity}, args...)...)...)
	return err == nil && code == 0 && roomOpen(out)
}

// startFlags are the capacity flags of a start's kind: review marks a
// review, selfHosted the review of a self-hosted model's work.
func startFlags(review, selfHosted bool) []string {
	args := []string{"--json"}
	if review {
		args = append(args, "--"+reviewFlag)
		if selfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	return args
}

// placesRemotely says whether the loop's starts may be placed on an
// execution host: supervisor.hosts names one and capacity.choices gives a
// remote start its model name. Then the local room alone never stops a
// start; startPlacement judges each host's too.
func (l *supervisorLoop) placesRemotely() bool {
	return len(l.a.cfg.Supervisor.Hosts) > 0 && len(l.a.cfg.Capacity.Choices) > 0
}

// startPlacement picks where one of the loop's starts runs, judged like
// the start itself would be (review and selfHosted mark the reviewer
// kinds). Without supervisor.hosts, or for a desk with no capacity.choices
// (a remote start needs a model name from the pick), it stays local (the
// zero value). Otherwise the local verdict (capacity --json, with the
// start kind's flags) decides first: room keeps the start local unless
// supervisor.hostsFirst; a capacity read that did not run waits, as it
// did before hosts existed. Then the first host whose own check (capacity
// --host, which probes the host first) has room and that maps the
// repository takes it, pinned to the repository's default branch; a host
// that cannot be read, maps no repository or has no room is said once (or
// passed quietly) and skipped. With supervisor.hostsFirst and no host with
// room the start waits, as it does on a full machine; without the
// preference the local machine attempts it, whose own start gate makes it
// wait when it has none either.
func (l *supervisorLoop) startPlacement(ctx context.Context, repo string, review, selfHosted bool) (startHost, error) {
	k := l.a.cfg.Supervisor
	if !l.placesRemotely() {
		// Nothing to place remotely: the local machine does what it did
		// before, and the start's own gate makes it wait when it has no
		// room.
		return startHost{}, nil
	}
	flags := startFlags(review, selfHosted)
	out, code, err := beekeeperRun(ctx, l.as(append([]string{loopCapacity}, flags...)...)...)
	if err != nil || code != 0 {
		return startHost{}, errLoopLater
	}
	if roomOpen(out) && !k.HostsFirst {
		return startHost{}, nil
	}
	for _, selector := range k.Hosts {
		h, err := l.a.executionHost(selector)
		if err != nil {
			l.sayOnce("host "+selector, "SUPERVISOR HOST FAILED %s: %v", selector, err)
			continue
		}
		if _, mapped := h.Repositories[repo]; !mapped {
			l.sayOnce("host "+selector+" "+repo, "SUPERVISOR HOST WAITS: %s maps no %s", selector, repo)
			continue
		}
		if !l.capacityRoom(ctx, append([]string{"--host", selector}, flags...)...) {
			continue
		}
		ref, err := ghDefaultBranch(ctx, repo)
		if err != nil || ref == "" {
			l.sayOnce("branch "+repo, "SUPERVISOR HOST WAITS: %s: no default branch of %s to pin a remote start (%v)", selector, repo, err)
			continue
		}
		return startHost{Selector: selector, Repo: repo, Ref: ref}, nil
	}
	if k.HostsFirst {
		return startHost{}, errLoopLater
	}
	return startHost{}, nil
}

// remoteStartFlags are what a placed start names on its agents start line:
// the host, the mapped repository and the ref to pin.
func (h startHost) remoteStartFlags() []string {
	if !h.remote() {
		return nil
	}
	return []string{loopHostFlag, h.Selector, loopRepoFlag, h.Repo, loopRefFlag, h.Ref}
}

// ghDefaultBranch reads a repository's default branch.
var ghDefaultBranch = func(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "api", "repos/"+repo, "--jq", ".default_branch")
	return strings.TrimSpace(string(out)), err
}

// remoteStartRefused decides a refused placed start on a host: an
// interruption between the reservation and first-task acceptance leaves
// the managed execution reserved, and every later agents start then refuses
// the name; the loop recovers the recorded execution (agents recover), the
// way a hand start's operator would, and reports its result as the start's.
// A start a host refused for room, with nothing reserved, waits for a
// later pass as before.
func (l *supervisorLoop) remoteStartRefused(ctx context.Context, name string) (string, error) {
	st, err := l.a.store.Read()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) && ag.Execution != nil })
	if i < 0 {
		return "", errLoopLater
	}
	if rec, _ := executionRecord(st, *st.Agents[i].Execution); rec != nil && rec.Terminal() {
		return "", errLoopLater
	}
	out, err := l.run(ctx, agentsName, loopRecover, name)
	if err != nil {
		l.sayOnce("recover "+name, "SUPERVISOR RECOVER FAILED %s: %v", name, err)
		return "", errLoopLater
	}
	return fmt.Sprintf("%q recovered the execution its interrupted start reserved: %s", name, oneLine(out)), nil
}

// startNext starts the next ranked item with the worker brief, when
// capacity has room — on an execution host when supervisor.hosts names one
// with room (and the board item's repository is mapped there), else on the
// coordinator's own machine.
func (l *supervisorLoop) startNext(ctx context.Context, act superloop.Action) (string, error) {
	if l.startsHeld {
		return "", errLoopLater
	}
	k := l.a.cfg.Supervisor
	if k.WorkerBrief == "" {
		return "no supervisor.workerBrief: no worker starts", nil
	}
	if !l.placesRemotely() {
		// The room check as before: a machine with no room reads no board.
		if _, code, err := beekeeperRun(ctx, l.as(loopCapacity)...); err != nil || code != 0 {
			return "", errLoopLater
		}
	}
	next, err := l.nextItem(ctx)
	if err != nil {
		return "", err
	}
	if next == nil {
		return "the board offers nothing: no worker starts", nil
	}
	return l.startItem(ctx, act, *next)
}

// loopBoardNone is a board next answer without a pick: nothing free, or
// the error that kept the board from answering.
type loopBoardNone struct{ err error }

// nextItem is the board's first free ranked item, or nil when none is free.
// A walk costs a large share of the GraphQL budget: an empty or failed one
// answers the rest of the pass.
// No walk runs while the GitHub budget is under github.floor: the pass
// answers errLoopLater, so a done event waits unsettled and costs nothing.
func (l *supervisorLoop) nextItem(ctx context.Context) (*loopBoardItem, error) {
	if l.boardNone != nil {
		return nil, l.boardNone.err
	}
	st, err := l.a.store.Read()
	if err != nil {
		return nil, err
	}
	if boardBudgetLow(st.Budget, l.a.cfg.GitHub.Floor, l.a.now) {
		l.boardNone = &loopBoardNone{err: errLoopLater}
		l.sayOnce("board budget", "SUPERVISOR NEXT HELD: the GitHub budget is under github.floor (%d), no board walk until it recovers", l.a.cfg.GitHub.Floor)
		return nil, errLoopLater
	}
	delete(l.said, "board budget")
	next, err := l.readNextItem(ctx)
	if next == nil {
		l.boardNone = &loopBoardNone{err: err}
	}
	return next, err
}

// boardBudgetLow reports whether a board walk, which costs a large share of
// the GraphQL budget, must wait: the REST budget is under floor until its
// reset, GraphQL refuses, or the last GraphQL reading, taken within its
// hourly window, is under floor.
func boardBudgetLow(b *state.Budget, floor int, now time.Time) bool {
	if lowBudget(b, floor, now) {
		return true
	}
	if b == nil || b.GraphQL == nil {
		return false
	}
	g := b.GraphQL
	return g.Blocks(now) || (g.Remaining < floor && now.Sub(g.At) < time.Hour)
}

// readNextItem reads board next --json. With nothing free board next exits
// 3 after printing its document with no pick: that is the board's answer,
// nothing free, never a failure to retry.
func (l *supervisorLoop) readNextItem(ctx context.Context) (*loopBoardItem, error) {
	out, code, err := beekeeperRun(ctx, l.as(loopBoard, "next", "--json")...)
	if err != nil {
		return nil, err
	}
	failed := func() error {
		return &exitError{code: code, msg: fmt.Sprintf("beekeeper %s next: exit %d: %s", loopBoard, code, oneLine(out))}
	}
	if code != 0 && code != ExitRefused {
		return nil, failed()
	}
	// The output is stdout and then stderr: the document is its first value.
	var next struct {
		Pick json.RawMessage `json:"pick"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&next); err != nil {
		if code != 0 {
			return nil, failed()
		}
		return nil, fmt.Errorf("board next --json: %w", err)
	}
	if code != 0 {
		// Only a document that says no pick settles a refusal.
		if string(next.Pick) != "null" {
			return nil, failed()
		}
		return nil, nil
	}
	if len(next.Pick) == 0 || string(next.Pick) == "null" {
		return nil, nil
	}
	var pick loopBoardItem
	if err := json.Unmarshal(next.Pick, &pick); err != nil {
		return nil, fmt.Errorf("board next --json: %w", err)
	}
	if _, ok := parseRef(pick.Ref); !ok {
		return nil, fmt.Errorf("board next: invalid item ref %q", pick.Ref)
	}
	return &pick, nil
}

type loopBoardItem struct {
	Ref string `json:"ref"`
	URL string `json:"url"`
}

func (l *supervisorLoop) startItem(ctx context.Context, act superloop.Action, next loopBoardItem) (string, error) {
	k := l.a.cfg.Supervisor
	r, _ := parseRef(next.Ref)
	place, err := l.startPlacement(ctx, r.Repo, false, false)
	if err != nil {
		if errors.Is(err, errLoopLater) {
			l.sayOnce("room "+act.Key, "SUPERVISOR NEXT WAITS %s: no room for a start yet on any host", next.URL)
			return "", errLoopLater
		}
		return "", err
	}
	brief, err := l.brief(k.WorkerBrief, "", "next "+next.Ref, briefData{URL: next.URL, Repo: r.Repo, Number: r.N})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s %d", repoName(r.Repo), r.N)
	args := []string{agentsName, agentStartName, name, brief}
	if len(l.a.cfg.Capacity.Choices) > 0 {
		// The start takes capacity's choice of harness and model, as a
		// reviewer's does.
		args = append(args, "--harness", harnessAuto)
	}
	if place.remote() {
		// A remote start needs its mapped repository and a ref to pin; the
		// host's start judges its own capacity, so --harness auto's pick
		// comes from that host's decision.
		args = append(args, "--host", place.Selector, "--repo", place.Repo, "--ref", place.Ref)
	}
	if err := l.claim(act, name); err != nil {
		return "", err
	}
	if _, err := l.run(ctx, args...); err != nil {
		if Code(err) == ExitRefused && place.remote() {
			// A refusal of a placed start on a host is either room or
			// an execution its interrupted start left reserved. The
			// recovery, when it succeeds, is the start taken after all:
			// it is decided before any failure closes the claim, so a
			// recovered execution is reconciled from the claim even
			// when the pass dies before the taken line.
			what, rerr := l.remoteStartRefused(ctx, name)
			if rerr == nil {
				return what, nil
			}
			if !errors.Is(rerr, errLoopLater) {
				err = rerr
			}
		}
		if strings.HasPrefix(act.Key, loopFillKey) {
			// A returned failure is settled, rather than an interruption
			// that a later start under the same name could reconcile.
			closed := about(event(loopParty, loopClaimEndVerb, "[%s] start failed or refused", act.Key), act.Ref)
			if closeErr := l.a.store.Record(closed); closeErr != nil {
				return "", fmt.Errorf("closing failed start claim: %w", closeErr)
			}
		}
		if Code(err) == ExitRefused {
			return "", errLoopLater
		}
		return "", err
	}
	if place.remote() {
		return fmt.Sprintf("%q started on %s on %s at %s", name, next.URL, place.Selector, place.Ref), nil
	}
	return fmt.Sprintf("%q started on %s", name, next.URL), nil
}

// install takes release act.Version through its stages, each resumed on a
// later pass while the release's claim stands: the install (self-update,
// while --check still finds it newer), the broker on the installed binary,
// supervisor.postInstall (once), then --check finding it the latest.
func (l *supervisorLoop) install(ctx context.Context, act superloop.Action) (string, error) {
	version := act.Version
	if err := l.claim(act, version); err != nil {
		return "", err
	}
	_, code, err := beekeeperRun(ctx, l.as(selfUpdateCheck...)...)
	if err != nil {
		return "", err
	}
	if code == ExitOutdated {
		if _, err := l.run(ctx, selfUpdateCmdName); err != nil {
			return "", err
		}
	}
	what := []string{version + " installed"}
	broker, err := loopBroker(ctx)
	if errors.Is(err, errLoopLater) {
		l.sayOnce("broker "+act.Key, "SUPERVISOR RELEASE WAITS %s: %s, its restart waits for them", version, broker)
		return "", errLoopLater
	}
	if err != nil {
		return "", err
	}
	what = append(what, broker)
	if argv := l.a.cfg.Supervisor.PostInstall; len(argv) > 0 {
		stage := act.Key + " post-install"
		if !l.stages[stage] {
			out, errOut, code, err := loopCommand(ctx, "", argv)
			if err != nil || code != 0 {
				return "", fmt.Errorf("%s installed, supervisor.postInstall failed (exit %d, %v): %s", version, code, err, oneLine(out+errOut))
			}
			if err := l.a.store.Record(event(loopParty, loopStageVerb, "[%s] ran", stage)); err != nil {
				return "", err
			}
			l.stages[stage] = true
		}
		what = append(what, "supervisor.postInstall ran")
	}
	out, code, err := beekeeperRun(ctx, l.as(selfUpdateCheck...)...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s installed, but self-update --check exits %d: %s", version, code, oneLine(out))
	}
	return strings.Join(append(what, "the latest"), ", "), nil
}

// loopParkOn preserves explicit wait owners. Older workers sometimes omit
// --on and name the pull request in their wait instead. Only an unambiguous
// pull URL supplies that missing owner; issue URLs and prose do not.
func loopParkOn(p *state.Park) string {
	if p.On != "" {
		return p.On
	}
	matches := loopParkPull.FindAllStringSubmatch(p.Waits, -1)
	on := ""
	for _, m := range matches {
		ref := m[1] + "#" + m[2]
		if on != "" && on != ref {
			return ""
		}
		on = ref
	}
	return on
}

var loopParkPull = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/([0-9]+)\b`)

// classification is the classifier's answer.
type classification struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
}

// classifierInput is the event the classifier reads on stdin.
type classifierInput struct {
	Verb   string    `json:"verb"`
	Agent  string    `json:"agent"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// loopClasses are the classes a classifier may answer.
var loopClasses = []string{"needs-person", "blocked", "failed", "question"}

// classify routes an event the rules cannot place to the guide, with the
// classifier's class when it is sure enough.
func (l *supervisorLoop) classify(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	k := l.a.cfg.Supervisor
	e := *act.Event
	label := "unclassified"
	why := "beekeeper's supervisor loop has no rule for it and no classifier (supervisor.classifier)"
	if len(k.Classifier) > 0 {
		in, _ := json.Marshal(classifierInput{Verb: e.Verb, Agent: e.Agent, Detail: e.Detail, At: e.At})
		// Its stdout is the answer; its stderr is for the failure.
		out, errOut, code, err := loopCommand(ctx, string(in), k.Classifier)
		var c classification
		switch {
		case err != nil || code != 0:
			why = fmt.Sprintf("its classifier failed (exit %d, %v): %s", code, err, oneLine(errOut))
		case json.Unmarshal([]byte(strings.TrimSpace(out)), &c) != nil || !slices.Contains(loopClasses, c.Class):
			why = "its classifier answered no class: " + oneLine(out)
		case c.Confidence < k.ClassifierConfidence:
			why = fmt.Sprintf("its classifier says %s at confidence %.2f, below %.2f", c.Class, c.Confidence, k.ClassifierConfidence)
		default:
			label = c.Class
			why = fmt.Sprintf("its classifier says %s (confidence %.2f), which only a person or the guide resolves", c.Class, c.Confidence)
		}
	}
	ref := agentRef(st, e.Agent)
	if ref == "" {
		return "", fmt.Errorf("%s's %s names no issue or pull request to file the guide's note on", e.Agent, e.Verb)
	}
	q := fmt.Sprintf("Decide what %s does next about its %s (%s)?", e.Agent, strings.TrimPrefix(e.Verb, "agents."), label)
	if err := l.toGuide(ctx, act, ref, q, fmt.Sprintf("%s logged %s at %s: %s", e.Agent, e.Verb, e.At.UTC().Format(time.RFC3339), e.Detail), why,
		fmt.Sprintf("leave %s as it is: it keeps its task", e.Agent)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s to the guide (%s) on %s", e.Verb, label, ref), nil
}

// fileReportSeparator identifies reports stored in the event itself. Older
// file transitions only summarized delivery to a note and must not be replayed.
const fileReportSeparator = "; file in the owning repositories:\n"

// toFile leaves the full finding in the settled transition's event. The
// guide's feed reads that event once; no decision or thread is needed.
func (l *supervisorLoop) toFile(_ context.Context, _ *state.State, act superloop.Action) (string, error) {
	e := *act.Event
	return fmt.Sprintf("%s reported at %s%s%s", e.Agent, e.At.UTC().Format(time.RFC3339), fileReportSeparator, act.Message), nil
}

// agentRef is the issue or pull request the agent works: what it is
// parked on, else its serve record, else the first one its task names.
func agentRef(st *state.State, name string) string {
	for _, ag := range st.Agents {
		if ag.Name != name {
			continue
		}
		if ag.Park != nil && superloop.IsPull(ag.Park.On) {
			return ag.Park.On
		}
		for _, r := range st.Records {
			if r.Session.Is(ag.Party) && r.Issue != "" {
				return r.Issue
			}
		}
		for _, task := range []string{ag.Task, ag.LastTask} {
			if m := refURL.FindString(task); m != "" {
				return m
			}
		}
	}
	return ""
}

// toGuide files a decision for the guide's person on ref (note add).
func (l *supervisorLoop) toGuide(ctx context.Context, act superloop.Action, ref, question, statusQuo, why, dflt string, options ...string) error {
	person := l.a.cfg.Guide.Person
	if person == "" {
		return errors.New("guide.person is unset: no guide's queue to file it in")
	}
	question = truncate(question, 150)
	if err := l.claim(act, question); err != nil {
		return err
	}
	args := []string{loopNote, loopAdd, "--for", person}
	if ref != "" {
		args = append(args, "--ref", ref)
	}
	args = append(args, "--due", l.a.cfg.Supervisor.NoteDue.String(),
		"--status-quo", truncate(statusQuo, 2900), "--why", why, "--default", dflt,
		"--checked", "beekeeper supervisor loop: gh pr view and the event log")
	for _, o := range options {
		args = append(args, "--option", o)
	}
	_, err := l.run(ctx, append(args, question)...)
	return err
}

// loopEventAgent is the roster name of the agent e concerns: the name its
// detail opens with, else the roster entry of the party that logged it,
// else that party's name.
func loopEventAgent(st *state.State, e state.Event) string {
	by := e.By.Name
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(e.By) }); i >= 0 {
		by = st.Agents[i].Name
	}
	return superloop.EventAgent(e.Verb, e.Detail, by)
}

// fireTimers takes only timers with wakes or commands; watch retains line-only
// reminders and their desktop notifications. Checks run outside the state
// lock; settling inside Update lets a concurrent watch take a timer first
// without either firing it twice. Side effects follow the successful save.
func (l *supervisorLoop) fireTimers(ctx context.Context, st *state.State) error {
	a := l.a
	var acting []state.Timer
	for _, t := range st.Timers {
		if timerActs(t) {
			acting = append(acting, t)
		}
	}
	found := checkTimers(ctx, acting, a.now, lowBudget(st.Budget, a.cfg.GitHub.Floor, a.now) || a.resetFirst(st))
	// Avoid a write on every idle loop pass.
	_, _, _, changed := settleTimersMatching(st, found, a.now, timerActs)
	if !changed {
		return nil
	}
	var lines []string
	var fires []timerFire
	if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, fires, _ = settleTimersMatching(current, found, a.now, timerActs)
		return evs, nil
	}); err != nil {
		return err
	}
	// Each batch keeps its own time: later passes update a.now while these
	// wakes and commands can still be running.
	snapshot := *a
	w := &watcher{app: &snapshot}
	for _, line := range lines {
		w.emitNow("timer", "%s", line)
	}
	l.timerActs.Go(func() {
		w.actTimers(ctx, fires)
		w.timerActs.Wait()
	})
	return nil
}
