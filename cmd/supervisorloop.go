package cmd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
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
// effect; loopStageVerb a finished stage of a release; loopUnstageVerb a
// stage found obsolete, which no later pass resumes.
// loopGoVerb logs a merge go given to a worker, loopMergeVerb a merge the
// loop ran itself.
const (
	loopClaimVerb    = "supervisor.claim"
	loopGoVerb       = "supervisor." + superloop.KindGo
	loopMergeVerb    = "supervisor." + superloop.KindMerge
	loopEndVerb      = "supervisor." + superloop.KindEnd
	loopFileVerb     = "supervisor." + superloop.KindFile
	loopClaimEndVerb = "supervisor.unclaim"
	loopStageVerb    = "supervisor.stage"
	loopUnstageVerb  = "supervisor.unstage"
	loopFailureVerb  = "supervisor.failure"
	// loopOwnerlessVerb logs since when an open pull request has had no
	// live owner ("[<ref>] since <time>"), and its end ("[<ref>] owned"),
	// so a restarted loop's sweep keeps the stretch.
	loopOwnerlessVerb = "supervisor.ownerless"
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
	out, err := github.RunGH(ctx, "pr", "view", strconv.Itoa(r.N), "--repo", r.Repo, "--json", "state,url,headRefOid,baseRefName,statusCheckRollup,reviews,author,title,headRefName,files,changedFiles,mergeCommit,isDraft,mergeable,mergeStateStatus,closingIssuesReferences,updatedAt")
	if err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view %d --repo %s: %w", r.N, r.Repo, err)
	}
	return parseLoopPull(ref, out)
}

// readChangedPast reads the files p's head changes past the change of
// reviewed (github.ChangedPast).
var readChangedPast = func(ctx context.Context, p superloop.Pull, reviewed string) ([]string, error) {
	r, ok := parseRef(p.Ref)
	if !ok || p.Base == "" {
		return nil, fmt.Errorf("%s names no pull request with a base", p.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return github.ChangedPast(ctx, github.RunGH, r.Repo, p.Base, reviewed, p.Head)
}

// readBaseMerge reads whether the approval of approved covers p's head: it
// only merges its base into approved, resolving the merge's conflicts or
// not, or rebases approved onto it (github.ApprovalCovers).
var readBaseMerge = func(ctx context.Context, p superloop.Pull, approved string) (bool, error) {
	r, ok := parseRef(p.Ref)
	if !ok || p.Base == "" {
		return false, fmt.Errorf("%s names no pull request with a base", p.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	only, _, err := github.ApprovalCovers(ctx, github.RunGH, r.Repo, p.Base, approved, p.Head)
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
	Closes []struct {
		URL string `json:"url"`
	} `json:"closingIssuesReferences"`
	Updated     time.Time `json:"updatedAt"`
	Draft       bool      `json:"isDraft"`
	Mergeable   string    `json:"mergeable"`
	MergeState  string    `json:"mergeStateStatus"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	State  string      `json:"state"`
	URL    string      `json:"url"`
	Head   string      `json:"headRefOid"`
	Base   string      `json:"baseRefName"`
	Rollup []loopCheck `json:"statusCheckRollup"`
	Author loopLogin   `json:"author"`
	Title  string      `json:"title"`
	Branch string      `json:"headRefName"`
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

// loopCheck is one entry of a pull request's check rollup: a check run
// reports a status and a conclusion, a commit status a state.
type loopCheck struct {
	StartedAt    time.Time `json:"startedAt"`
	WorkflowName string    `json:"workflowName"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	State        string    `json:"state"`
	// A check run's name and details URL, a commit status's context and
	// target URL.
	Name       string `json:"name"`
	DetailsURL string `json:"detailsUrl"`
	Context    string `json:"context"`
	TargetURL  string `json:"targetUrl"`
}

// The states of a check run and a commit status the loop reads.
const (
	checkCompleted = "COMPLETED"
	checkSuccess   = "SUCCESS"
	checkFailure   = "FAILURE"
	checkError     = "ERROR"
)

// loopRedConclusions are the conclusions of a check run that are red: a
// terminal state that never turns green by itself.
var loopRedConclusions = []string{checkFailure, "CANCELLED", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED"}

// green reports whether the check concluded successfully.
func (c loopCheck) green() bool {
	return c.State == checkSuccess || c.Status == checkCompleted && slices.Contains([]string{checkSuccess, "NEUTRAL", "SKIPPED"}, c.Conclusion)
}

// red reports whether the check concluded red: a run that failed, or a
// status that failed or errored. One queued, running or pending is not.
func (c loopCheck) red() bool {
	return c.Status == checkCompleted && slices.Contains(loopRedConclusions, c.Conclusion) || slices.Contains([]string{checkFailure, checkError}, c.State)
}

// concluded reports whether the check reached a terminal state.
func (c loopCheck) concluded() bool {
	if c.Status != "" {
		return c.Status == checkCompleted
	}
	return slices.Contains([]string{checkSuccess, checkFailure, checkError}, c.State)
}

// superseded reports only cancellations replaced on this head's rollup.
// Failures and timeouts remain red even when a later run exists. Actions run
// IDs also order queued replacements whose startedAt has not been set yet.
func (c loopCheck) superseded(checks []loopCheck) bool {
	if c.Conclusion != "CANCELLED" || c.Status != checkCompleted {
		return false
	}
	oldRun := loopActionsRun(c.DetailsURL)
	oldRepo, _, _ := strings.Cut(c.DetailsURL, "/actions/runs/")
	return slices.ContainsFunc(checks, func(next loopCheck) bool {
		if next.Status == "" {
			return false // A commit status is not a replacement check run.
		}
		newRun := loopActionsRun(next.DetailsURL)
		newRepo, _, _ := strings.Cut(next.DetailsURL, "/actions/runs/")
		newerActionsRun := oldRun != 0 && newRun > oldRun && oldRepo == newRepo
		sameCheck := c.Name != "" && c.Name == next.Name
		sameWorkflow := c.WorkflowName != "" && c.WorkflowName == next.WorkflowName && newerActionsRun
		return (sameCheck || sameWorkflow) && (newerActionsRun || !c.StartedAt.IsZero() && next.StartedAt.After(c.StartedAt))
	})
}

func loopActionsRun(detailsURL string) uint64 {
	// The rollup exposes the Actions run ID through its details URL.
	_, tail, ok := strings.Cut(detailsURL, "/actions/runs/")
	if !ok {
		return 0
	}
	id, _, _ := strings.Cut(tail, "/")
	n, _ := strconv.ParseUint(id, 10, 64)
	return n
}

// parseLoopPull reads gh pr view's JSON of ref.
func parseLoopPull(ref string, out []byte) (superloop.Pull, error) {
	var j loopPullJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return superloop.Pull{}, fmt.Errorf("gh pr view of %s: %w", ref, err)
	}
	p := superloop.Pull{Draft: j.Draft, Mergeable: j.Mergeable, MergeState: j.MergeState, MergeCommit: j.MergeCommit.OID, Ref: ref, URL: j.URL, State: j.State, Head: j.Head, Base: j.Base, Green: len(j.Rollup) > 0, Author: j.Author.Login, Title: j.Title, Branch: j.Branch, ChangedFiles: j.ChangedFiles}
	p.Updated = j.Updated
	for _, issue := range j.Closes {
		if r, ok := parseRef(issue.URL); ok {
			p.Closes = append(p.Closes, refName(r))
		}
	}
	for _, f := range j.Files {
		p.Files = append(p.Files, f.Path)
	}
	for _, c := range j.Rollup {
		if c.superseded(j.Rollup) {
			continue
		}
		p.Checks = append(p.Checks, cmp.Or(c.Name, c.Context))
		if !c.green() {
			p.Green = false
		}
		if c.red() {
			p.Red = append(p.Red, superloop.Check{Name: cmp.Or(c.Name, c.Context), URL: cmp.Or(c.DetailsURL, c.TargetURL)})
		}
	}
	slices.SortStableFunc(p.Red, func(x, y superloop.Check) int { return strings.Compare(x.Name, y.Name) })
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
		Short: "Run the supervisor: a deterministic loop over beekeeper's events",
		Long: `loop is the supervisor: it takes the supervisor's routine transitions,
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
  review    an agent parked from its pull request's worktree at its
            published HEAD (agents park --on owner/repo#n), with every
            check on the head green, for the
            first round and for each new head the last approval does not
            cover, including fixes after changes requested. There is one
            review per head; supervisor.reviewRounds never releases an
            unapproved head. A reviewer starts
            (agents start --review) with supervisor.reviewBrief or the
            built-in brief, on capacity's pick (--harness auto with
            capacity.choices), using reserved review headroom; a worker on a
            self-hosted provider's model gets one on no self-hosted
            provider (--review-of-self-hosted). With supervisor.hosts
            (execution hosts, agents.hosts) the start goes to the first
            host whose own check (capacity --host) has room for its kind
            and that maps the repository, pinned to its default branch;
            the local machine keeps starts while it has room, unless
            supervisor.hostsFirst, which prefers hosts for what a host
            can run, as for a worker. No host with room and no room
            locally: the start waits. A desk with
            no capacity.choices never places remotely
  red       an agent parked from its pull request's worktree at its
            published HEAD (a review request, local or on an execution
            host) whose head has a check that concluded red (failed,
            cancelled, timed out, startup failure, action required; a
            commit status's failure or error): the worker is told each
            such check's name and URL (agents wake), one wake for the
            checks red in a pass, each check once for each head and park;
            the same failure seen again wakes nobody, a new head or a new
            park on the same head is told again. None after the head's
            merge go; a red head gets no review
  merge     every check on the head green and the review policy releases it:
            the loop merges it through the gate with its exact reviewed head,
            configured merge.methods (default squash) and merge.admin flags.
            No worker wakes for the mechanical merge. Behind or conflicting
            branches go back for repair; other gate refusals stay barriers.
            The gate never updates a pinned branch. An approval covers a head
            that only brings the base into the approved one: the approved
            head is its ancestor, every commit it adds that the base lacks
            is a merge, and its
            change against its merge base with the base branch is the
            approved head's, file by file, hunk by hunk, line by line
            (compare API), or differs only where the merges resolve a
            conflict: in a file the base changed since, every line it adds
            or removes beyond the approved change is one the approved or
            the base's change adds or removes (each merge then merges a
            commit of the base branch); or it rebases the approved head
            onto the base under the same rule. A file without a patch counts
            only with the approved blob and no change of the base's to it
            since. Anything else, or a comparison GitHub cuts short, is a
            new head with a new review. Only the verdict of the loop's own
            last round counts, on the head it reviewed and posted after it
            started: an older verdict or a round still running authorizes
            nothing
  forward   the last round's verdict asks for changes: the review, word
            for word, to the worker, whatever its head is now
  rework    the worker parked again after the forward on the head the
            findings were about, or a head that only brings the base into
            it: tell the worker to fix them and push, once per park. Its
            new green head gets its own review. Exhausted rounds never
            authorize a merge or send a merge question to guide.person
  answer    the person answered an escalation (beekeeper answer --note):
            an answer that releases it (the
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
  sweep     an open, non-draft pull request of the desk (gh search prs by
            the desk's logins in github.allowedOrgs, listed every 5 minutes,
            tracked or not) with no live owner for superloop.OrphanWait
            (15m) since it was first seen so (logged as
            supervisor.ownerless, kept across restarts) and since the
            loop's last act on it. A live owner is an agent on the roster
            parked on it for its own sake, serving or parked on it or an
            issue it closes, named for it ("<repo> <n>"), the one the loop
            acts for, or its reviewer; a saved review park and a worker
            waiting on it for another pull request are none. It gets a fixer
            (agents start under its worker's name) for a head that
            conflicts, is red, carries an unfixed changes request, or is
            behind a base its merge does not update (no devctl
            --update-branch, no merge.admin); the merge go again for a green
            head whose go was given and never carried out; a reviewer for
            a green head awaiting its review. Running checks wait; an open
            escalation or ownership question is the person's. One action per stretch,
            at most superloop.MaxSweeps (2) per head, then a leave for the
            guide
  override  the person's answer Override (the option, or a bare override):
            that one pull request is the desk's from then on, reviewed and
            given the go as its own team's; any other answer goes to the
            worker word for word
  end       a reviewed pull request merged or closed: its tracking ends.
            New review parks declare remaining work with --post-merge: the
            loop wakes that worker for it; with no remaining work it closes
            the task and records its report without a wake. Older parks
            retain their release-containment behavior.
  done      agents.done: once its turn ends, the agent leaves the roster
            (agents remove); the loop retires a session beekeeper started
            the same way, once its turn ends: a reviewer once the loop
            acted on its round's verdict (a go, forward, escalation, next
            round or end), and a worker given the merge go after its post-merge
            report or supervisor.postMergeWindow without activity
            (default 2h). A worker that still serves other open work, the
            open issue its sessions serve record names or another open pull
            request it is parked on, stays: a leave line says so once, and it
            goes at its own done, once that work closed, or after
            agents.staleAfter (24h) quiet with no park or wake timer. A managed SSH
            execution stops (agents stop) and leaves the roster only once
            the host acknowledged its termination. Each pass also runs the
            doctor's roster and archive step (removing finished, relieved
            and stale entries with no CLI, archiving owed desktop sessions
            under agents.archiveAgreement and stopping finished or relieved
            background sessions). A session its person started is never
            retired
  next      after a done, and every pass below capacity.ceiling while capacity
            has room: the next item of board
            next starts (agents start, --harness auto with
            capacity.choices) with supervisor.workerBrief; none
            configured, none starts. With supervisor.hosts (execution
            hosts, agents.hosts) the start goes to the first host whose
            check (agents hosts check, capacity --host) has room and that
            maps the item's repository, pinned to its default branch; the
            local machine keeps the start while it has room, unless
            supervisor.hostsFirst, which prefers hosts for what a host
            can run: with no host room a Claude start waits, while a
            repository no host maps, or a choice no host has an adapter
            for (every harness but Claude), starts locally within the
            local room (capacity.local.ceiling among its guards). No
            host with room and no room locally: the start waits. Releases
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
            re-executes the installed one at its next pass. A --check
            that fails says SUPERVISOR RELEASE LOOKUP FAILED, once until
            a look succeeds again
  hosts     every pass, each execution host (agents.hosts) whose pin is
            not this coordinator's release: agents hosts update installs
            the release there, verified, and pins the policy digest it
            prepared in beekeeper's state. A host with executions drains
            first (no new start); a host whose update fails is held (no
            start there), handed on once as a finding like file's, and
            tried again hourly; the other hosts keep running. A
            development build updates none, and nothing while beekeeper
            stop stands
  classify  a report or park the rules cannot place: agents.report without
            agents.done; a park on nothing of an agent still parked
            on nothing, with no timer that wakes it (timer add --wake), no
            note its wait names (#12) and no open note of its own. The
            supervisor.classifier command, given the event as JSON on
            stdin, answers {"class":"needs-person|blocked|failed|question",
            "confidence":0.9}; the event goes to the guide (note add for
            guide.person) with its class. A park the classifier cannot
            place (below supervisor.classifierConfidence, 0.8, or without
            a classifier) files no note: the worker is woken (agents wake)
            to park again on a wait the loop watches (a pull request, a
            timer that wakes it, a note for a person) or to report done.
            An unclassified report goes to the guide unclassified
  file      a problem found (agents.problem) that cites no issue URL:
            the full report is logged in supervisor.file and delivered once
            by guide watch as GUIDE PROBLEM, to file in its owning repository.
            A step failing the same way for 30 minutes or on more than one
            item is filed the same way, once per episode, naming the step,
            the items it blocks and the error.
            A done report's problems arrive together before the done settles.
            No decision note or thread reference is needed. A problem naming
            its issue is already filed; the agent keeps its task
  intake    an issue the desk filed (a problem report's issue URL, a
            follow-up named in a report, an issue a session created with gh
            issue create): put on the configured board in Backlog (board
            move --intake), unless it already sits on another board where it
            is being worked, and ranked on the board (board rank --after)
            right after the epic or item it blocks or follows from
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
parked on a verified review request or the loop reviews it. A plain wait
starts no review and receives no go or forwarded findings; the watch
resumes it when the pull request merges or closes. Every transition happens once: a
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
	mu    sync.Mutex
	a     *app
	since time.Time
	once  bool
	// said are the failures already printed, by key.
	said map[string]bool
	// releaseAt is when the loop last looked for a release.
	releaseAt  time.Time
	startsHeld bool
	stopWatch  func()
	// tidyAt is when the loop last ran board tidy.
	tidyAt time.Time
	// stages are the release or intake stages finished, by key; stageDetail
	// is the recorded detail of each stage.
	stages      map[string]bool
	stageDetail map[string]string
	postMerge   []superloop.Action
	// fillPause defers an unproductive board read until capacity changes,
	// an agent is done or removed, or the configured retry interval passes.
	fillPause fillPause
	// fillEmpty counts the refill's unproductive board reads since its last
	// start or a change that ended its pause; each one doubles the next
	// pause, up to fillBackoff times.
	fillEmpty int
	// boardNone is this pass's empty or failed board next answer: every
	// later start of the pass takes it instead of walking the board again.
	boardNone *loopBoardNone
	// timerActs are timer side effects; shutdown and re-exec await them.
	timerActs sync.WaitGroup
	// inFlight tracks actions and ordered streams across passes.
	// Both wait groups are drained on exit and before re-exec.
	inFlight     map[string]bool
	mergeActions sync.WaitGroup
	passActions  sync.WaitGroup
	actionErr    error
	completed    map[string]bool
	// login is the login gh acts as, once read; teams the team files'
	// owners, read at teamsAt.
	login   string
	teams   map[string]string
	teamsAt time.Time
	// dryRun prints a pass's transitions and takes none.
	dryRun bool
	// bin is the loop's running executable, checked at quiet points so an
	// install replaces this process as soon as no claim is in flight.
	bin          replaceable
	reexecuted   bool
	reexecFailed bool
	// baseMerges are the answers of readBaseMerge, by pull request,
	// approved head and head: two commits' relation does not change.
	baseMerges map[string]bool
	// changedPast are the answers of readChangedPast, by pull request,
	// reviewed head and head.
	changedPast map[string][]string
	// opens are the answers of refStates, by issue or pull request,
	// each kept for loopOpenEvery.
	opens map[string]loopOpen
	// merged are the merged pull requests read, by reference.
	merged map[string]superloop.Pull
	// pulls reuses unchanged PR observations between passes. A new park,
	// answer or reviewer report invalidates them before planning.
	pulls map[string]loopPullObservation
	// openPulls are the desk's open pull requests the sweep last listed,
	// at openPullsAt.
	openPulls   []string
	openPullsAt time.Time
}

// loopOpen is whether an issue or pull request was open when read at at.
type loopOpen struct {
	open bool
	at   time.Time
}

// loopOpenEvery is how long the loop keeps a read of whether an issue or
// pull request is open: a kept worker's served issue is not read every pass.
const loopOpenEvery = 10 * time.Minute

// open reports whether the issue or pull request ref is open, read at most
// every loopOpenEvery; one it cannot read counts as open, said once, so no
// worker is retired on a failed read.
func (l *supervisorLoop) open(ctx context.Context, ref string) bool {
	l.mu.Lock()
	if o, ok := l.opens[ref]; ok && l.a.timeNow().Sub(o.at) < loopOpenEvery {
		l.mu.Unlock()
		return o.open
	}
	l.mu.Unlock()
	r, ok := parseRef(ref)
	if !ok {
		return true
	}
	states, err := refStates(ctx, []github.PR{r})
	st, read := states[r]
	if err != nil || !read {
		l.sayOnce("open "+ref, "SUPERVISOR LOOP cannot tell whether %s is open (%v): its worker stays", ref, err)
		return true
	}
	open := st.State == github.Open
	l.mu.Lock()
	if l.opens == nil {
		l.opens = map[string]loopOpen{}
	}
	l.opens[ref] = loopOpen{open: open, at: l.a.timeNow()}
	l.mu.Unlock()
	return open
}

// loopServes is the issue ag's active sessions serve record names, as
// owner/repo#n: "" without one, once its session ended or once it waits
// for "done".
func loopServes(st *state.State, ag state.Agent) string {
	i := slices.IndexFunc(st.Records, func(r state.Record) bool { return r.Session.Is(ag.Party) })
	if i < 0 {
		return ""
	}
	r := st.Records[i]
	if !r.Ended.IsZero() || strings.HasPrefix(strings.ToLower(r.Waits), "done") {
		return ""
	}
	if ref, ok := parseRef(r.Issue); ok {
		return refName(ref)
	}
	return ""
}

func loopTaskServes(ag state.Agent, parkedOn string) string {
	for _, task := range []string{ag.Task, ag.LastTask} {
		if m := refURL.FindString(task); m != "" {
			if r, ok := parseRef(m); ok && !strings.EqualFold(refName(r), parkedOn) {
				return refName(r)
			}
		}
	}
	return ""
}

// loopOutput serializes parallel loop actions and asynchronous timer output.
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
	if err := a.store.WriteFile(loopPIDFile, os.Getpid()); err != nil {
		return err
	}
	defer func() { _ = a.store.WriteFile(loopPIDFile, 0) }()
	if err := a.writeLoopVersion(); err != nil {
		return err
	}
	defer a.clearLoopVersion()
	out := a.out
	defer func() { a.out = out }()
	if a.loopNow == nil {
		a.loopNow = &atomic.Pointer[time.Time]{}
	}
	defer a.loopNow.Store(nil)
	l, err := a.newSupervisorLoop(false)
	if err != nil {
		return err
	}
	l.once = once
	defer func() {
		cancel()
		l.passActions.Wait()
		l.mergeActions.Wait()
		l.timerActs.Wait()
	}()
	if !once {
		l.stopWatch = a.startLoopWatch(ctx)
		defer func() {
			if l.stopWatch != nil {
				l.stopWatch()
			}
		}()
	}
	for {
		now := time.Now()
		if once {
			a.now = now
		}
		a.loopNow.Store(&now)
		l.reexecuted = false
		l.mu.Lock()
		l.reexecFailed = false
		l.mu.Unlock()
		if err := l.pass(ctx); err != nil {
			if reexec, rerr := l.reexec(ctx); rerr != nil {
				return rerr
			} else if reexec {
				if once {
					return nil
				}
				continue
			}
			return err
		}
		if _, rerr := l.reexec(ctx); rerr != nil {
			return rerr
		}
		if once {
			return nil
		}
		if l.reexecuted {
			continue
		}
		if !l.waitNextPass(ctx) {
			return nil
		}
	}
}

// waitNextPass waits for LoopInterval, ctx.Done(), or a replaced binary so
// an external self-update during the idle sleep triggers re-execution
// without waiting the full LoopInterval on the old binary.
func (l *supervisorLoop) waitNextPass(ctx context.Context) bool {
	d := l.a.cfg.Supervisor.LoopInterval.Duration
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	const checkEvery = time.Second
	if d <= checkEvery {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-ticker.C:
			b := l.bin
			if b == nil {
				b = loopBinary()
				l.mu.Lock()
				l.bin = b
				l.mu.Unlock()
			}
			if b != nil && b.Replaced() && !l.reexecFailed {
				return true
			}
		}
	}
}

// loopLockFile is the lock one running loop holds, beside the state.
const loopLockFile = "supervisor-loop.lock"

// loopPIDFile is the running loop's PID marker, beside the state: 0 once
// it stopped.
const loopPIDFile = "supervisor-loop-pid.json"

// loopUnit is the user service that runs the loop, which beekeeper install
// always installs.
const loopUnit = "beekeeper-supervisor-loop.service"

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
	// Every loop can run actions concurrently, including a directly constructed
	// loop. Give all its output paths the same synchronized writer.
	if _, ok := a.out.(*loopOutput); !ok {
		a.out = &loopOutput{out: a.out}
	}
	starts, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == loopStartVerb })
	if err != nil {
		return nil, err
	}
	l := &supervisorLoop{a: a, since: a.timeNow().UTC(), said: map[string]bool{}, baseMerges: map[string]bool{}, changedPast: map[string][]string{}, dryRun: dryRun, bin: loopBinary()}
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

// reexec re-executes the loop on the binary installed over its path at a
// quiet point (no claim in flight). It returns true when the replacement
// binary was executed, so the caller ends the pass on the old binary.
func (l *supervisorLoop) reexec(ctx context.Context) (bool, error) {
	if l == nil || l.dryRun || l.reexecuted || l.reexecFailed {
		return false, nil
	}
	b := l.bin
	if b == nil {
		b = loopBinary()
		l.mu.Lock()
		l.bin = b
		l.mu.Unlock()
	}
	if b == nil || !b.Replaced() {
		return false, nil
	}
	l.mu.Lock()
	busy := len(l.inFlight) > 0
	l.mu.Unlock()
	if busy {
		return false, nil
	}
	l.mergeActions.Wait()
	l.passActions.Wait()
	if _, err := fmt.Fprintln(l.a.out, "SUPERVISOR LOOP its binary was replaced: re-executing it"); err != nil {
		return false, err
	}
	if out, code, err := beekeeperRun(ctx, l.as(budgetGateArgs...)...); err != nil || code != 0 {
		l.sayOnce("installed-gate", "SUPERVISOR RELEASE FAILED: installed binary failed budget --gate (exit %d): %s", code, oneLine(out))
	}
	l.timerActs.Wait()
	wasWatching := l.stopWatch != nil
	if wasWatching {
		l.stopWatch()
		l.stopWatch = nil
	}
	if err := b.Exec(); err != nil {
		l.mu.Lock()
		l.reexecFailed = true
		l.mu.Unlock()
		l.sayOnce("reexec", "SUPERVISOR LOOP the new binary does not start (%v): carrying on", err)
		if wasWatching {
			l.stopWatch = l.a.startLoopWatch(ctx)
		}
		return false, nil
	}
	l.reexecuted = true
	_ = l.a.writeLoopVersion()
	return true, nil
}

// pass is one look: gather, plan, act.
func (l *supervisorLoop) pass(ctx context.Context) error {
	a := l.a.snapshotAtNow()
	l.mu.Lock()
	l.boardNone = nil
	l.mu.Unlock()
	// A release installed over the loop's path runs from here on: an old
	// binary's saves are refused once the new one wrote the state.
	if reexec, err := l.reexec(ctx); err != nil || reexec {
		return err
	}
	if !l.dryRun {
		if err := l.watchConditions(ctx); err != nil {
			l.holdStarts(true)
			l.sayOnce("watch-conditions", "SUPERVISOR WATCH conditions failed; starts remain held: %v", err)
		} else {
			l.clearSaid("watch-conditions")
		}
	}
	if !l.dryRun {
		if err := l.watchAlerts(ctx); err != nil {
			l.sayOnce("alerts", "SUPERVISOR ALERTS FAILED: %v", err)
		} else {
			l.clearSaid("alerts")
		}
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	// Wall-clock timer jobs keep running while GitHub observations are
	// held. Board commands they invoke still share the query admission gate.
	if !l.dryRun {
		if err := l.fireTimers(ctx, st); err != nil {
			return err
		}
		l.settleOverrides(ctx, st)
	}
	// A budget hold applies to observations too: reading every parked PR
	// while starts are held still drains the shared GraphQL window.
	if a.cfg.GitHub.Floor > 0 && loopBudgetUnknownOrLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) {
		l.sayOnce("graphql budget", "SUPERVISOR GITHUB HELD: GraphQL budget unknown or under github.floor; no board, ownership or PR reads until a fresh reading recovers")
		return nil
	}
	l.clearSaid("graphql budget")
	if !l.dryRun {
		if err := a.loopGitHub(ctx, "pass", func() error { return nil }); err != nil {
			l.sayOnce("graphql allowance", "SUPERVISOR GITHUB HELD: %v", err)
			return nil
		}
		l.clearSaid("graphql allowance")
	}
	if !l.dryRun {
		l.tidy(ctx, st)
		l.updateHosts(ctx, st)
		if err := a.syncClaims(ctx); err != nil {
			l.sayOnce("claims", "SUPERVISOR LOOP claims on GitHub: %v", err)
		}
		if reexec, err := l.reexec(ctx); err != nil || reexec {
			return err
		}
	}
	if err := l.recoverQuota(ctx, st); err != nil {
		return err
	}
	st, err = a.store.Read()
	if err != nil {
		return err
	}
	if err := l.recoverParks(ctx, st); err != nil {
		return err
	}
	st, err = a.store.Read()
	if err != nil {
		return err
	}
	if err := l.endReviewWorkers(ctx, st); err != nil {
		return err
	}
	st, err = a.store.Read()
	if err != nil {
		return err
	}
	own := a.cfg.Supervisor.Ownership
	in := superloop.Input{Rounds: a.cfg.Supervisor.ReviewRounds, Person: a.cfg.Guide.Person, Now: a.timeNow(), PostMergeWindow: a.cfg.Supervisor.PostMergeWindow.Duration,
		Desk: superloop.Desk{Since: l.since, Orgs: a.cfg.GitHub.AllowedOrgs, Authors: own.Authors, Team: own.Team,
			Review: own.Review, Never: own.Never, OwnerWait: own.OwnerWait.Duration},
		DevctlOwners: a.cfg.Merge.DevctlOwners, Admin: a.cfg.Merge.Admin,
		BaseMerged:  func(p superloop.Pull, approved string) bool { return l.baseMerged(ctx, p, approved) },
		ChangedPast: func(p superloop.Pull, reviewed string) ([]string, error) { return l.changed(ctx, p, reviewed) },
		Open:        func(ref string) bool { return l.open(ctx, ref) }, StaleAfter: a.cfg.Agents.StaleAfter.Duration,
		Board: a.cfg.Board.Owner != "" && a.cfg.Board.Project != 0}
	if in.Board {
		in.Filed = l.filedIssues(st)
	}
	for _, ag := range st.Agents {
		w := superloop.Worker{Name: ag.Name, SelfHosted: selfHostedModel(a.cfg.Capacity, agentRecordedModel(st, ag)), Started: st.StartIndex(ag.Party) >= 0}
		if ag.Park != nil {
			w.ParkedOn, w.ParkedAt, w.Waits = loopParkOn(ag.Park), ag.Park.At, ag.Park.Waits
			// A verified park requests review even while retirement waits for
			// the worker's current turn to end.
			w.ReviewRequested = ag.Park.ReviewHead != ""
			w.ParkedHead = ag.Park.ReviewHead
		}
		w.Waking = slices.ContainsFunc(st.Timers, func(t state.Timer) bool { return t.Wake != "" && strings.EqualFold(t.Wake, ag.Name) })
		w.Serves = loopServes(st, ag)
		if !ag.Done {
			w.Mentions = namedRefs(ag.Task, a.cfg.Board.Owner)
		}
		if w.Serves == "" && ag.Park != nil && ag.Park.ReviewHead == "" {
			w.Serves = loopTaskServes(ag, w.ParkedOn)
		}
		in.Workers = append(in.Workers, w)
	}
	for _, rp := range st.ReviewParks {
		if rp.On == "" || rp.ReviewHead == "" {
			continue
		}
		if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool {
			return ag.Park != nil && ag.Park.ReviewHead != "" && strings.EqualFold(loopParkOn(ag.Park), rp.On)
		}) {
			continue
		}
		in.Workers = append(in.Workers, superloop.Worker{
			Name:            rp.Agent,
			ParkedOn:        rp.On,
			ParkedAt:        rp.At,
			ReviewRequested: true,
			ParkedHead:      rp.ReviewHead,
			SelfHosted:      selfHostedModel(a.cfg.Capacity, rp.Model),
			Started:         false,
			Serves:          rp.Serves,
			Saved:           true,
		})
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
	claims, stages, stageDetail := map[string]loopClaim{}, map[string]bool{}, map[string]string{}
	ownerless := map[string]time.Time{}
	failures := map[string]loopFailure{}
	stuckFiled := map[string]time.Time{}
	for _, e := range taken {
		if t, ok := superloop.ParseTaken(e.At, e.Verb, e.Subject, e.Detail); ok {
			in.Taken = append(in.Taken, t)
			// A step taken is no longer failing.
			delete(failures, t.Key)
			if t.Kind == superloop.KindFile && strings.HasPrefix(t.Key, loopStuckKey) {
				// The episode's start ends the key; filed is by step and error.
				i := strings.LastIndexByte(t.Key, ' ')
				stuckFiled[t.Key[:max(i, 0)]] = e.At
			}
			continue
		}
		key, rest, ok := keyed(e.Detail)
		switch {
		case !ok:
		case e.Verb == loopOwnerlessVerb:
			if since, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(rest, ownerlessSince)); err == nil {
				ownerless[key] = since
			} else {
				delete(ownerless, key)
			}
		case e.Verb == loopClaimVerb:
			claims[key] = loopClaim{at: e.At, target: rest}
		case e.Verb == loopClaimEndVerb:
			delete(claims, key)
			in.Taken = slices.DeleteFunc(in.Taken, func(t superloop.Taken) bool { return t.Key == key })
		case e.Verb == loopStageVerb:
			stages[key] = true
			stageDetail[key] = rest
		case e.Verb == loopUnstageVerb:
			delete(stages, key)
			delete(stageDetail, key)
		case e.Verb == loopFailureVerb:
			var f loopFailure
			if json.Unmarshal([]byte(rest), &f) == nil {
				// Failures an older loop logged carry neither time.
				f.At = cmp.Or(f.At, e.At)
				f.Since = cmp.Or(f.Since, e.At)
				kind, _, _ := strings.Cut(key, " ")
				f.Kind = cmp.Or(f.Kind, kind)
				f.Ref = cmp.Or(f.Ref, e.Subject)
				failures[key] = f
			}
		}
	}
	for _, t := range in.Taken {
		delete(claims, t.Key)
	}
	if in.Taken, err = l.dropAdminRepairs(in.Taken, stages, stageDetail); err != nil {
		return err
	}
	l.mu.Lock()
	l.stages, l.stageDetail = stages, stageDetail
	l.mu.Unlock()
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
		if an, ok := parseAnswered(e); ok && an.Asker == loopParty.Name && strings.EqualFold(an.For, a.cfg.Guide.Person) {
			in.Answers = append(in.Answers, superloop.Answer{Ref: an.About, Note: an.ID, Question: an.Question, Text: an.Answer, By: an.For, At: an.At})
		}
	}
	in.Pulls = map[string]superloop.Pull{}
	tracked := superloop.Tracked(in.Workers, in.Taken, in.Answers, in.Desk)
	// The sweep reads the desk's open pull requests that nothing tracks:
	// one whose worker ended without a park, or never parked, has no other
	// way into a pass.
	swept := l.sweepPulls(ctx, in, tracked)
	if len(tracked)+len(swept) > 0 && !l.ownership(ctx, &in.Desk) {
		tracked = nil
	} else {
		tracked = append(tracked, swept...)
	}
	for _, ref := range tracked {
		if !a.parkIsPull(ctx, st, ref) {
			continue
		}
		// A merged pull request stays merged: a worker kept for the open
		// work it serves does not have it read again every pass.
		l.mu.Lock()
		mergedPull, wasMerged := l.merged[ref]
		l.mu.Unlock()
		if wasMerged {
			in.Pulls[ref] = mergedPull
			continue
		}
		p, err := l.observePull(ctx, ref, loopPullChanged(in, ref))
		if err != nil {
			if errors.Is(err, errLoopLater) {
				l.sayOnce("graphql allowance", "SUPERVISOR GITHUB HELD: %v", err)
				return nil
			}
			l.sayOnce("read "+ref, "SUPERVISOR LOOP cannot read %s: %v", ref, err)
			continue
		}
		l.clearSaid("graphql allowance")
		l.clearSaid("read " + ref)
		if r, ok := parseRef(ref); ok {
			p.Team = l.teams[strings.ToLower(r.Repo)]
		}
		p.Reviews = deskVerdicts(p.Reviews, in.Desk.Authors)
		in.Pulls[ref] = p
		if p.State == superloop.Merged {
			l.mu.Lock()
			if l.merged == nil {
				l.merged = map[string]superloop.Pull{}
			}
			l.merged[ref] = p
			l.mu.Unlock()
		}
	}
	// A merge may have landed before its completion record was saved. The
	// planner sees a merged PR then, so reconcile its pending claim before
	// planning the end and retaining its worker for any post-merge work.
	if !l.dryRun {
		for key, c := range claims {
			tuple, ok := strings.CutPrefix(key, "go ")
			if !ok {
				continue
			}
			ref, _, ok := strings.Cut(tuple, "@")
			head := superloop.KeyHead(key)
			p, present := in.Pulls[ref]
			if !ok || !present || p.State != superloop.Merged || p.Head != head {
				continue
			}
			act := superloop.Action{Key: key, Kind: superloop.KindGo, Ref: ref, URL: p.URL, Head: head, Agent: c.target}
			if err := l.record(act, "merged before an interruption; confirmed at "+head); err != nil {
				return err
			}
			in.Taken = append(in.Taken, superloop.Taken{Key: key, Kind: superloop.KindGo, Ref: ref, Agent: c.target, At: a.timeNow()})
			delete(claims, key)
		}
	}
	postMerge := superloop.ReleaseWorkers(in)
	l.mu.Lock()
	l.postMerge = postMerge
	l.mu.Unlock()
	if len(postMerge) > 0 || superloop.HasOverlapWait(in.Workers, in.Taken, in.Answers) {
		sessions, _, sessionErr := loopSessions(a)
		for i, ag := range st.Agents {
			if ag.Execution != nil {
				in.Workers[i].LastActive = executionLastActive(st, ag, a.timeNow())
			} else if sessionErr != nil && !platform.Missing(sessionErr) {
				in.Workers[i].LastActive = a.timeNow() // never retire on a failed observation
			} else if s, ok := claude.Live(sessions, ag.Party); ok {
				in.Workers[i].LastActive = s.LastActive
			}
		}
	}
	if !l.dryRun && len(postMerge) > 0 {
		for i := len(in.Taken) - 1; i >= 0; i-- {
			if t := in.Taken[i]; t.Kind == superloop.KindRelease {
				version := strings.TrimPrefix(t.Key, "release ")
				if err := l.wakeReleased(ctx, superloop.Action{Key: t.Key, Version: version}); err != nil {
					l.sayOnce("release wake "+version, "SUPERVISOR RELEASE WAKE FAILED %s: %v", version, err)
				}
				break
			}
		}
	}
	if !st.AutoInstallPaused {
		// A release claimed and not finished goes on, whatever --check says
		// now that it is installed.
		for key := range claims {
			if v, ok := strings.CutPrefix(key, superloop.KindRelease+" "); ok {
				in.Release = v
			}
		}
		if in.Release == "" && !l.dryRun && a.timeNow().Sub(l.releaseAt) >= a.cfg.Supervisor.ReleaseEvery.Duration {
			l.releaseAt = a.timeNow()
			in.Release = l.newerRelease(ctx)
		}
	}
	if reexec, err := l.reexec(ctx); err != nil || reexec {
		return err
	}
	if l.pauseForReexec() {
		return nil
	}
	if in.Ownerless, err = l.markOwnerless(in, claims, ownerless); err != nil {
		return err
	}
	actions := append(superloop.Plan(in), orphanedReviewClaims(claims, in)...)
	if !l.dryRun {
		if err := l.stuck(failures, stuckFiled); err != nil {
			return err
		}
	}
	if l.dryRun {
		for _, act := range actions {
			if _, err := fmt.Fprintf(a.out, "SUPERVISOR WOULD %s %s\n", strings.ToUpper(act.Kind), superloop.Detail(act, would(act))); err != nil {
				return err
			}
		}
		return l.fill(ctx, st, claims, in.Taken)
	}
	return l.runPassActions(ctx, st, claims, failures, in.Taken, actions)
}

func (l *supervisorLoop) beginInFlight(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[key] {
		return false
	}
	if l.inFlight == nil {
		l.inFlight = map[string]bool{}
	}
	l.inFlight[key] = true
	return true
}

func (l *supervisorLoop) endInFlight(key string) {
	l.mu.Lock()
	delete(l.inFlight, key)
	l.mu.Unlock()
}

func (l *supervisorLoop) execAction(ctx context.Context, st *state.State, claims map[string]loopClaim, failures map[string]loopFailure, act superloop.Action) error {
	if l.pauseForReexec() {
		return nil
	}
	// An earlier pass can finish after this pass took its snapshot.
	l.mu.Lock()
	done := l.completed[act.Key]
	if done {
		l.mu.Unlock()
		return nil
	}

	if l.a.timeNow().Before(failures[act.Key].RetryAt) {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()
	if c, ok := claims[act.Key]; ok && act.Kind != superloop.KindRelease {
		// Claimed before an interruption: what the log shows of its
		// side effect decides, never a second one.
		what, done, err := l.reconcile(ctx, st, act, c)
		if err != nil {
			return err
		}
		if done {
			return l.completeAction(act, what)
		}
	}
	if act.Kind == superloop.KindQuestion {
		if err := l.readdress(act); err != nil {
			return l.failed(act, failures, err)
		}
		l.mu.Lock()
		delete(failures, act.Key)
		l.mu.Unlock()
		return nil
	}
	what, err := l.act(ctx, st, act)
	switch {
	case errors.Is(err, errLoopLater):
		return nil
	case err != nil:
		return l.failed(act, failures, err)
	}
	if act.Kind == superloop.KindGo && what == loopWorkerMergeGo {
		l.rememberAction(act)
		return nil
	}
	return l.completeAction(act, what)
}

// runPassActions runs merges, review starts, intake, worker starts and PR/worker
// transitions independently so a merge waiting on CI or in a lane queue never
// delays a review or worker start, and the reverse.
func (l *supervisorLoop) runPassActions(ctx context.Context, st *state.State, claims map[string]loopClaim, failures map[string]loopFailure, taken []superloop.Taken, actions []superloop.Action) error {
	planned := map[string]bool{}
	var (
		laneOrder  []string
		laneMerges = map[string][]superloop.Action{}
		reviewActs []superloop.Action
		intakeActs []superloop.Action
		doneActs   []superloop.Action
		nextActs   []superloop.Action
		otherActs  []superloop.Action
	)
	for _, act := range actions {
		if act.Kind == superloop.KindFile || act.Kind == superloop.KindDone {
			planned[act.Agent] = true
		}
		switch act.Kind {
		case superloop.KindGo:
			if !l.beginInFlight(act.Key) {
				continue
			}
			lane := act.Ref
			if r, ok := parseRef(act.Ref); ok {
				lane = strings.ToLower(r.Repo)
			}
			if _, ok := laneMerges[lane]; !ok {
				laneOrder = append(laneOrder, lane)
			}
			laneMerges[lane] = append(laneMerges[lane], act)
		case superloop.KindReview:
			reviewActs = append(reviewActs, act)
		case superloop.KindIntake:
			intakeActs = append(intakeActs, act)
		case superloop.KindDone:
			doneActs = append(doneActs, act)
		case superloop.KindNext:
			nextActs = append(nextActs, act)
		default:
			otherActs = append(otherActs, act)
		}
	}

	setErr := func(err error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if err != nil && l.actionErr == nil {
			l.actionErr = err
		}
	}
	// Reserve an ordered stream before launching it. Later passes skip only
	// that stream, while still observing and scheduling unrelated work.
	launch := func(key string, run func() error) {
		if !l.beginInFlight(key) {
			return
		}
		l.passActions.Go(func() {
			defer l.endInFlight(key)
			setErr(run())
		})
	}

	var passMerges sync.WaitGroup
	for _, lane := range laneOrder {
		acts := laneMerges[lane]
		passMerges.Add(1)
		l.mergeActions.Go(func() {
			defer passMerges.Done()
			defer func() {
				for _, act := range acts {
					l.endInFlight(act.Key)
				}
			}()
			for _, act := range acts {
				err := l.execAction(ctx, st, claims, failures, act)
				if err != nil {
					setErr(err)
					return
				}
			}
		})
	}

	for _, act := range reviewActs {
		launch("review:"+act.Ref, func() error {
			return l.execAction(ctx, st, claims, failures, act)
		})
	}

	// When a worker finishes (KindDone) and files a follow-up issue in the
	// same pass, intake and rank that issue before refilling the freed slot;
	// otherwise run intake concurrently in its own stream.
	intakeBeforeFill := len(doneActs) > 0 && len(intakeActs) > 0
	if len(intakeActs) > 0 && !intakeBeforeFill {
		launch("stream:intake", func() error {
			for _, act := range intakeActs {
				if err := l.execAction(ctx, st, claims, failures, act); err != nil {
					return err
				}
			}
			return nil
		})
	}

	launch("stream:workers", func() error {
		if intakeBeforeFill {
			if !l.beginInFlight("stream:intake") {
				return nil // Retry from a fresh snapshot after outstanding intake.
			}
			err := func() error {
				defer l.endInFlight("stream:intake")
				for _, act := range intakeActs {
					if err := l.execAction(ctx, st, claims, failures, act); err != nil {
						return err
					}
				}
				return nil
			}()
			if err != nil {
				return err
			}
		}
		for _, act := range doneActs {
			if err := l.execAction(ctx, st, claims, failures, act); err != nil {
				return err
			}
		}
		for _, act := range nextActs {
			if err := l.execAction(ctx, st, claims, failures, act); err != nil {
				return err
			}
		}
		return l.fill(ctx, st, claims, taken)
	})

	// Keep transitions for one worker ordered, without making a stalled
	// transition hold housekeeping or transitions for another worker.
	transitionOrder := []string{}
	transitions := map[string][]superloop.Action{}
	for _, act := range otherActs {
		key := "transition:" + cmp.Or(act.Agent, act.Kind)
		if _, ok := transitions[key]; !ok {
			transitionOrder = append(transitionOrder, key)
		}
		transitions[key] = append(transitions[key], act)
	}
	for _, key := range transitionOrder {
		acts := transitions[key]
		launch(key, func() error {
			for _, act := range acts {
				if err := l.execAction(ctx, st, claims, failures, act); err != nil {
					return err
				}
			}
			return nil
		})
	}
	launch("stream:roster", func() error {
		return l.roster(ctx, planned)
	})

	if l.once {
		l.passActions.Wait()
		passMerges.Wait()
	} else if len(laneOrder) > 0 {
		done := make(chan struct{})
		go func() {
			passMerges.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Millisecond):
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.actionErr
	l.actionErr = nil
	return err
}

// baseMerged reports whether p's head only merges its base into approved,
// read once per pair of heads; a read that fails is said once and counts as
// no: the new head gets its review.
func (l *supervisorLoop) baseMerged(ctx context.Context, p superloop.Pull, approved string) bool {
	key := p.Ref + " " + approved + " " + p.Head
	l.mu.Lock()
	if ok, read := l.baseMerges[key]; read {
		l.mu.Unlock()
		return ok
	}
	l.mu.Unlock()
	ok, err := readBaseMerge(ctx, p, approved)
	if err != nil {
		l.sayOnce("base merge "+key, "SUPERVISOR LOOP cannot tell whether %s's head only merges its base: %v", p.Ref, err)
		return false
	}
	l.mu.Lock()
	if l.baseMerges == nil {
		l.baseMerges = map[string]bool{}
	}
	l.baseMerges[key] = ok
	l.mu.Unlock()
	return ok
}

// changed lists the files p's head changes past the change of reviewed,
// read once per pair of heads; a read that fails is read again next pass.
func (l *supervisorLoop) changed(ctx context.Context, p superloop.Pull, reviewed string) ([]string, error) {
	key := p.Ref + " " + reviewed + " " + p.Head
	l.mu.Lock()
	if files, read := l.changedPast[key]; read {
		l.mu.Unlock()
		return files, nil
	}
	l.mu.Unlock()
	files, err := readChangedPast(ctx, p, reviewed)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.changedPast == nil {
		l.changedPast = map[string][]string{}
	}
	l.changedPast[key] = files
	l.mu.Unlock()
	return files, nil
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
	if own.Team == "" || l.teams != nil && l.a.timeNow().Sub(l.teamsAt) < teamsEvery {
		return true
	}
	teams, err := readTeams(ctx, own)
	switch {
	case err == nil:
		l.teams, l.teamsAt = teams, l.a.timeNow()
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
	case superloop.KindEscalate, superloop.KindOwnership:
		return "ask the guide's person: " + act.Question
	case superloop.KindDone:
		return "take off the roster " + act.Message
	case superloop.KindNext:
		return "start the next board item"
	case superloop.KindRelease:
		return "install " + act.Version
	case superloop.KindClassify:
		return fmt.Sprintf("send its %s to the guide", act.Event.Verb)
	case superloop.KindIntake:
		return fmt.Sprintf("put %s on the board in Backlog and rank it", act.URL)
	case superloop.KindGo:
		return "merge " + act.URL + " at " + act.Head
	case superloop.KindOwner:
		return fmt.Sprintf("give %s an owner (%s)", act.Ref, act.Agent)
	case superloop.KindForward, superloop.KindRework, superloop.KindAnswer, superloop.KindRelay, superloop.KindRed:
		return "tell: " + truncate(oneLine(act.Message), 300)
	}
	return truncate(oneLine(act.Message), 300)
}

// record logs act taken, which no pass takes again, and says it.
func (l *supervisorLoop) record(act superloop.Action, what string) error {
	detail := superloop.Detail(act, what)
	kind := act.Kind
	if kind == superloop.KindGo && !strings.HasPrefix(what, loopWorkerMergeGo) {
		kind = superloop.KindMerge
	}
	if err := l.a.store.Record(about(event(loopParty, "supervisor."+kind, "%s", detail), act.Ref)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(l.a.out, "SUPERVISOR %s %s\n", strings.ToUpper(kind), escapedLine(detail))
	return err
}

// completeAction remembers only the final successful outcome, after any
// intermediate records and follow-up side effects have finished.
func (l *supervisorLoop) completeAction(act superloop.Action, what string) error {
	if err := l.record(act, what); err != nil {
		return err
	}
	l.rememberAction(act)
	return nil
}

func (l *supervisorLoop) rememberAction(act superloop.Action) {
	l.mu.Lock()
	if l.completed == nil {
		l.completed = map[string]bool{}
	}
	l.completed[act.Key] = true
	l.mu.Unlock()
}

// loopClaim is a claim read back: when, and what its side effect names
// (the session started, the agent woken or removed, the note's question).
type loopClaim struct {
	at     time.Time
	target string
}

// orphanedReviewClaims reports a pending review whose verified park is gone.
// Claims remain in the event log, so the warning must be durable across loop
// restarts too. It does not complete the claim: a later park can still use it.
func orphanedReviewClaims(claims map[string]loopClaim, in superloop.Input) []superloop.Action {
	keys := make([]string, 0, len(claims))
	for key := range claims {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var out []superloop.Action
	for _, key := range keys {
		pending, ok := strings.CutPrefix(key, "review ")
		if !ok {
			continue
		}
		ref, head, ok := strings.Cut(pending, "@")
		pr, valid := parseRef(ref)
		if !ok || !valid || head == "" {
			continue
		}
		if slices.ContainsFunc(in.Workers, func(w superloop.Worker) bool {
			return w.ReviewRequested && strings.EqualFold(w.ParkedOn, ref)
		}) {
			continue
		}
		warning := "leave " + key + " orphaned"
		if slices.ContainsFunc(in.Taken, func(t superloop.Taken) bool { return t.Key == warning }) {
			continue
		}
		url := fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.N)
		out = append(out, superloop.Action{Key: warning, Kind: superloop.KindLeave, Ref: ref, URL: url,
			Message: fmt.Sprintf("%s head %s: orphaned pending review claim; no worker holds a verified review park; claim retained for a new park", url, head)})
	}
	return out
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
	case superloop.KindGo:
		l.mu.Lock()
		repairing := l.stages[act.Key+" repair"]
		l.mu.Unlock()
		var p superloop.Pull
		if !repairing {
			var err error
			p, err = readLoopPull(ctx, act.Ref)
			if err != nil {
				return "", false, err
			}
			if p.State == superloop.Merged && p.Head == act.Head {
				done = true
				break
			}
		}
		if logged("agents.wake", opens) || logged(verbStart, opens) {
			if !repairing && p.State == superloop.Open && p.Head == act.Head && p.Green && !pullNeedsBranchRepair(p) {
				return fmt.Sprintf("%s before an interruption: %s, as the log shows", loopWorkerMergeGo, c.target), true, nil
			}
			done = true
		}
	case superloop.KindForward, superloop.KindRework, superloop.KindAnswer, superloop.KindRelay, superloop.KindRed, superloop.KindOwner:
		done = logged("agents.wake", opens) || logged(verbStart, opens)
	case superloop.KindEnd:
		done = logged("agents.wake", opens) || logged(verbStart, opens) || slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.target && ag.Done }) ||
			!slices.ContainsFunc(st.ReviewParks, func(rp state.ReviewPark) bool { return strings.EqualFold(rp.On, act.Ref) }) && !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.target })
	case superloop.KindDone:
		done = !slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == c.target }) ||
			logged("agents.remove", func(d string) bool { return d == c.target })
	case superloop.KindClassify:
		// An unclassified park's wake claims the agent's name; a note for
		// the guide claims its question.
		if c.target == act.Agent {
			done = logged("agents.wake", opens)
		} else {
			done = logged("note.add", func(d string) bool { return strings.Contains(d, c.target) })
		}
	case superloop.KindEscalate, superloop.KindOwnership:
		done = logged("note.add", func(d string) bool { return strings.Contains(d, c.target) })
	case superloop.KindIntake:
		for _, e := range evs {
			if e.Verb == "board.move" && opens(e.Detail) {
				moveStage := act.Key + " move"
				l.mu.Lock()
				l.stages[moveStage] = true
				if l.stageDetail != nil && l.stageDetail[moveStage] == "" {
					l.stageDetail[moveStage] = e.Detail
				}
				l.mu.Unlock()
			}
		}
		done = logged("board.rank", opens)
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
// consecutive failures defer the action for five minutes, keeping it pending;
// each further identical failure doubles the deferral up to an hour, so a
// refusal that does not change is not retried every five minutes all day.
// Since is when the step began failing this way, on this item or on another
// item still failing the same way; At is the latest attempt.
type loopFailure struct {
	Message string    `json:"message"`
	Count   int       `json:"count"`
	RetryAt time.Time `json:"retryAt"`
	Kind    string    `json:"kind,omitempty"`
	Ref     string    `json:"ref,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

const (
	loopFailureBackoff    = 5 * time.Minute
	loopFailureBackoffCap = time.Hour
)

// loopFailureDelay is the deferral after count identical failures.
func loopFailureDelay(count int) time.Duration {
	d := loopFailureBackoff
	for i := 2; i < count && d < loopFailureBackoffCap; i++ {
		d *= 2
	}
	return min(d, loopFailureBackoffCap)
}

// A step failing the same way for loopStuckAfter, or on more than one item,
// is stuck: the guide hears of it once (stuck). Failures missing their
// scheduled retry by loopStuckRecent drop out.
const (
	loopStuckAfter  = 30 * time.Minute
	loopStuckRecent = 3 * loopFailureBackoff
	loopStuckKey    = "stuck "
)

// recent keeps an episode alive through its scheduled backoff. A step
// missing its retry for loopStuckRecent starts a new episode.
func (f loopFailure) recent(now time.Time) bool {
	at := f.At
	if f.RetryAt.After(at) {
		at = f.RetryAt
	}
	return now.Sub(at) < loopStuckRecent
}

// failed logs act's failure and keeps it in failures, by key.
func (l *supervisorLoop) failed(act superloop.Action, failures map[string]loopFailure, cause error) error {
	l.mu.Lock()
	previous := failures[act.Key]
	f := loopFailure{Message: cause.Error(), Count: 1, Kind: act.Kind, Ref: act.Ref, Since: l.a.now, At: l.a.now}
	// Compare exact messages: a changing timestamp or PID is a different
	// failure and starts a new count, rather than hiding a new cause.
	// An episode expires after its retry has been missed for loopStuckRecent.
	if f.Message == previous.Message && previous.recent(l.a.now) {
		f.Count = previous.Count + 1
		f.Since = cmp.Or(previous.Since, f.Since)
	} else {
		// The same failure of the same step on another item continues
		// that episode rather than starting one.
		for _, o := range l.stuckGroup(failures, f.Kind, f.Message) {
			f.Since = minTime(f.Since, o.Since)
		}
	}
	if f.Count >= 2 {
		f.RetryAt = l.a.timeNow().Add(loopFailureDelay(f.Count))
	}
	failures[act.Key] = f
	l.mu.Unlock()
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

// stuckGroup are the failures of kind with message attempted recently, by key.
func (l *supervisorLoop) stuckGroup(failures map[string]loopFailure, kind, message string) map[string]loopFailure {
	group := map[string]loopFailure{}
	for key, f := range failures {
		if f.Kind == kind && f.Message == message && f.recent(l.a.now) {
			group[key] = f
		}
	}
	return group
}

// stuck files each step failing the same way for loopStuckAfter or on more
// than one item, once per episode: the guide's feed says it as GUIDE
// PROBLEM. An episode filed before it began (filed, by step and error) is a
// new one.
func (l *supervisorLoop) stuck(failures map[string]loopFailure, filed map[string]time.Time) error {
	keys := slices.Sorted(maps.Keys(failures))
	done := map[string]bool{}
	for _, key := range keys {
		f := failures[key]
		sum := sha256.Sum256([]byte(f.Kind + "\x00" + f.Message))
		step := loopStuckKey + f.Kind + " " + hex.EncodeToString(sum[:6])
		if done[step] {
			continue
		}
		done[step] = true
		group := l.stuckGroup(failures, f.Kind, f.Message)
		if len(group) == 0 {
			continue
		}
		since, attempts := l.a.now, 0
		var items []string
		for _, k := range slices.Sorted(maps.Keys(group)) {
			g := group[k]
			since = minTime(since, g.Since)
			attempts += g.Count
			items = append(items, cmp.Or(g.Ref, k))
		}
		if len(group) < 2 && l.a.now.Sub(since) < loopStuckAfter {
			continue
		}
		if at, ok := filed[step]; ok && !at.Before(since) {
			continue
		}
		slices.Sort(items)
		items = slices.Compact(items)
		text := fmt.Sprintf("The supervisor loop's %s step has failed the same way since %s, %d attempt(s), blocking %s: %s. "+
			"It retries with backoff from %s up to %s; own the failure to the component its error names.",
			f.Kind, since.UTC().Format(time.RFC3339), attempts, strings.Join(items, ", "), f.Message, loopFailureBackoff, loopFailureBackoffCap)
		line := fmt.Sprintf("%s reported at %s%s%s", loopParty.Name, l.a.now.UTC().Format(time.RFC3339), fileReportSeparator, text)
		// Each episode its own key: guide watch says each one once.
		fileKey := step + " " + since.UTC().Format("20060102T150405Z")
		if err := l.record(superloop.Action{Kind: superloop.KindFile, Key: fileKey}, line); err != nil {
			return err
		}
		filed[step] = l.a.now
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// errLoopLater is an action that waits for a later pass, quietly: no room
// for a start yet.
var errLoopLater = errors.New("later")

// sayOnce prints a failure once per key and run.
func (l *supervisorLoop) sayOnce(key, format string, args ...any) {
	l.mu.Lock()
	if l.said[key] {
		l.mu.Unlock()
		return
	}
	l.said[key] = true
	l.mu.Unlock()
	_, _ = fmt.Fprintf(l.a.out, format+"\n", args...)
}

func (l *supervisorLoop) clearSaid(key string) {
	l.mu.Lock()
	delete(l.said, key)
	l.mu.Unlock()
}

// newerRelease is a newer release's version, "" when this is the latest or
// the look failed; a failed look is said, once until it succeeds again, so
// the desk does not stay on an old release unnoticed.
func (l *supervisorLoop) newerRelease(ctx context.Context) string {
	out, code, err := beekeeperRun(ctx, selfUpdateCheck...)
	switch {
	case err != nil:
		l.sayOnce(releaseLookupKey, "SUPERVISOR RELEASE LOOKUP FAILED: %v", err)
		return ""
	case code != 0 && code != ExitOutdated:
		l.sayOnce(releaseLookupKey, "SUPERVISOR RELEASE LOOKUP FAILED: self-update --check exits %d: %s", code, oneLine(out))
		return ""
	}
	delete(l.said, releaseLookupKey)
	if code != ExitOutdated {
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
	if b.Owner == "" || b.Project == 0 || b.TidyEvery.Duration <= 0 || a.timeNow().Sub(l.tidyAt) < b.TidyEvery.Duration ||
		boardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) {
		return
	}
	l.tidyAt = a.timeNow()
	out, err := l.run(ctx, loopBoard, boardTidyName)
	if err != nil {
		if !errors.Is(err, errLoopLater) {
			l.sayOnce("board tidy", "SUPERVISOR BOARD TIDY FAILED: %v", err)
		}
		return
	}
	l.clearSaid("board tidy")
	sayBoard(a, out)
	// The board's blocked items reach the person, once each, and again
	// on their re-check timers while they stay blocked.
	if b.EscalateEvery.Duration < 0 {
		return
	}
	if out, err = l.run(ctx, loopBoard, boardEscalateName); err != nil {
		if !errors.Is(err, errLoopLater) {
			l.sayOnce("board escalate", "SUPERVISOR BOARD ESCALATE FAILED: %v", err)
		}
		return
	}
	l.clearSaid("board escalate")
	sayBoard(a, out)
}

// sayBoard prints each line of a board command's output as the loop's.
func sayBoard(a *app, out string) {
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			_, _ = fmt.Fprintln(a.out, "SUPERVISOR BOARD "+line)
		}
	}
}

// boardTidyName is the board tidy subcommand.
const boardTidyName = "tidy"

// releaseLookupKey is the said key of a failed look for a newer release.
const releaseLookupKey = "release lookup"

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
	loopTimer    = "timer"
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

func isLoopGitHubHeldOutput(line string) bool {
	return strings.Contains(line, "loop GitHub ") && strings.HasSuffix(line, ": "+errLoopLater.Error())
}

// run runs a beekeeper command as the loop; a non-zero exit is an error
// with its output.
func (l *supervisorLoop) run(ctx context.Context, args ...string) (string, error) {
	if len(args) >= 4 && args[0] == agentsName && args[1] == agentStartName && !slices.Contains(args, effortFlag) {
		args = append(args, l.startEffort(args)...)
	}
	out, code, err := beekeeperRun(ctx, l.as(args...)...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		line := oneLine(out)
		if isLoopGitHubHeldOutput(line) {
			return out, fmt.Errorf("beekeeper %s: %s: %w", strings.Join(args[:min(2, len(args))], " "), line, errLoopLater)
		}
		return out, &exitError{code: code, msg: fmt.Sprintf("beekeeper %s: exit %d: %s", strings.Join(args[:min(2, len(args))], " "), code, line)}
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
		return l.mergePull(ctx, st, act)
	case superloop.KindForward:
		if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the review of %s asking for changes on %s forwarded", act.URL, act.Head), nil
	case superloop.KindRed:
		if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s at %s: red checks told: %s", act.URL, act.Head, redChecks(act.Key)), nil
	case superloop.KindRework:
		if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s at %s: the review's findings stand with no round left, rework told", act.URL, act.Head), nil
	case superloop.KindAnswer:
		if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the person's answer about %s told", act.URL), nil
	case superloop.KindRelay:
		if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("the owning team's review of %s told", act.URL), nil
	case superloop.KindOwner:
		return l.ownOverlap(ctx, st, act)
	case superloop.KindEscalate:
		if err := l.toGuide(ctx, act, act.URL, act.Question, act.Message,
			fmt.Sprintf("legacy review escalation for head %s; new heads now get their own independent review", shortSHA(act.Head)),
			fmt.Sprintf("leave %s unmerged and its worker parked", act.URL), superloop.EscalationOptions(act.Head)...); err != nil {
			return "", err
		}
		return "to the guide: " + act.Message, nil
	case superloop.KindOwnership:
		if err := l.toGuide(ctx, act, act.URL, act.Question, act.Message,
			"its owning team's review blocks the desk's work, and only a person overrides the ownership of a pull request; the loop reviews it and gives the merge go as for the desk's own on an answer that overrides it",
			fmt.Sprintf("leave %s to its owning team's review; its worker stays parked", act.URL), act.Options...); err != nil {
			return "", err
		}
		return "to the guide: " + act.Message, nil
	case superloop.KindLeave, superloop.KindOverride, superloop.KindReopen:
		return act.Message, nil
	case superloop.KindEnd:
		return l.endMerged(ctx, st, act)
	case superloop.KindDone:
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent })
		if i < 0 {
			return "already off the roster", nil
		}
		// A removal or retirement waits for the session's turn to end:
		// its last report is not cut off, and its session can be archived
		// or stopped.
		if loopInTurn(a, st.Agents[i].Party) {
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
	case superloop.KindIntake:
		return l.intakeIssue(ctx, st, act)
	}
	return "", fmt.Errorf("no transition for %s", act.Kind)
}

// redChecks are the checks a red wake's key names.
func redChecks(key string) string {
	key, _, _ = strings.Cut(key, " episode ")
	_, checks, _ := strings.Cut(key, ": ")
	return checks
}

// loopInTurn reports whether p's session runs a turn.
var loopInTurn = func(a *app, p state.Party) bool {
	a = a.snapshotAtNow()
	if a.executionInTurn(p) {
		return true
	}
	_, ok := a.runningTurn(p)
	return ok
}

// executionLastActive is the latest activity recorded for ag's managed SSH
// execution, or now when its host is unreachable so a failed observation
// never retires a post-merge worker.
func executionLastActive(st *state.State, ag state.Agent, now time.Time) time.Time {
	if ag.Execution == nil {
		return time.Time{}
	}
	r, _ := executionRecord(st, *ag.Execution)
	if r == nil {
		return time.Time{}
	}
	if r.Observation.Reachability == execution.Unreachable {
		return now
	}
	last := r.LastActive
	if r.Observation.LastActive.After(last) {
		last = r.Observation.LastActive
	}
	if r.Published != nil && r.Published.At.After(last) {
		last = r.Published.At
	}
	if !r.Terminal() && r.StopRequest == nil && r.Observation.InTurn && r.Observation.ObservedAt.After(last) {
		last = r.Observation.ObservedAt
	}
	return last
}

func (a *app) executionInTurn(p state.Party) bool {
	st, err := a.store.Read()
	if err != nil {
		return false
	}
	return executionInTurn(st, p, a.timeNow())
}

// executionInTurn reports whether p's managed SSH execution is in a turn:
// the host observation reports an active turn, a wake message is in flight,
// or the coordinator recorded turn activity within stewardQuiet while the
// worker is neither parked nor finished.
func executionInTurn(st *state.State, p state.Party, now time.Time) bool {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Execution != nil && ag.Is(p) })
	if i < 0 {
		return false
	}
	ag := st.Agents[i]
	r, _ := executionRecord(st, *ag.Execution)
	if r == nil || r.Terminal() || r.StopRequest != nil {
		return false
	}
	if r.Observation.InTurn {
		return true
	}
	for _, m := range r.Messages {
		if m.Stage == execution.Uncertain || (!r.Completed && !ag.Done && (m.Stage == executionPending || m.Stage == execution.Stored)) {
			return true
		}
	}
	if ag.Park != nil || r.Completed || ag.Done || now.IsZero() {
		return false
	}
	last := r.LastActive
	if r.Observation.LastActive.After(last) {
		last = r.Observation.LastActive
	}
	if r.Published != nil && r.Published.At.After(last) {
		last = r.Published.At
	}
	return !last.IsZero() && now.Sub(last) < stewardQuiet
}

// loopSessions reads the running Claude sessions and process table for a pass.
var loopSessions = func(a *app) ([]*claude.Session, *proc.Table, error) {
	return a.sessions()
}

// roster runs the doctor's roster and archive step for a pass: it stops left
// sleeping waits, takes off the roster any finished agent (done, relieved or
// stale with no CLI) that superloop.Plan is not still handling in planned, and
// archives or stops owed desktop and background sessions once their turns end.
func (l *supervisorLoop) roster(ctx context.Context, planned map[string]bool) error {
	a := l.a.snapshotAtNow()
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	sessions, t, err := loopSessions(a)
	if err != nil {
		return nil //nolint:nilerr // process or session read failure does not fail the loop pass
	}
	record := func(host string) (*claude.Record, bool) { return claude.ReadRecord(a.cfg, host) }
	inFlight := func(p state.Party) bool { return mergeInFlight(st, p, proc.Alive) }
	waits := sleepingWaits(st, sessions, t, inFlight, a.timeNow())
	for _, w := range waits {
		stopProcesses(w.pids())
		line := "stopped " + w.String()
		_ = a.store.Update(func(*state.State) ([]state.Event, error) {
			return []state.Event{event(loopParty, "agents.stop", "%s", line)}, nil
		})
	}
	busy := func(p state.Party) bool {
		if slices.ContainsFunc(waits, func(w leftWait) bool { return w.agent.Is(p) }) {
			return false
		}
		_, turn := turnRunning(sessions, t, p, a.timeNow())
		return turn || loopInTurn(a, p) || inFlight(p)
	}
	chores := slices.DeleteFunc(planChores(st, sessions, record, busy, a.cfg.Agents.StaleAfter.Duration, a.timeNow(), a.cfg), func(c chore) bool {
		return c.kind != choreRemove || planned[c.agent.Name]
	})
	_, _, err = a.finishRoster(ctx, chores, sessions, record, busy, loopParty, "beekeeper supervisor loop")
	return err
}

// wake tells act's worker its message (agents wake), claimed first.
func (l *supervisorLoop) wake(ctx context.Context, act superloop.Action) error {
	if err := l.claim(act, act.Agent); err != nil {
		return err
	}
	_, err := l.run(ctx, agentsName, agentWakeName, act.Agent, act.Message)
	return err
}

func shouldEndReviewWorker(st *state.State, ag state.Agent) bool {
	if ag.Park == nil || ag.Park.ReviewHead == "" || loopParkOn(ag.Park) == "" {
		return false
	}
	k := st.StartIndex(ag.Party)
	if k < 0 {
		return false
	}
	if ag.Execution != nil {
		return st.Starts[k].Execution != nil
	}
	// The watch stops parked background sessions to free memory. Their
	// roster entry and park remain the durable owner of the review and wake.
	if st.Starts[k].Background {
		return false
	}
	return ag.Park.PostMerge != nil || st.Starts[k].Dir != ""
}

func reviewParkServes(st *state.State, ag state.Agent, on string) string {
	if s := loopServes(st, ag); s != "" && !strings.EqualFold(s, on) {
		return s
	}
	if ref := agentRef(st, ag.Name); ref != "" && !superloop.IsPull(ref) {
		if r, ok := parseRef(ref); ok {
			return refName(r)
		}
	}
	if rp, ok := st.ReviewParkFor(on); ok && rp.Serves != "" {
		return rp.Serves
	}
	return ""
}

func (l *supervisorLoop) endReviewWorkers(ctx context.Context, st *state.State) error {
	for _, ag := range st.Agents {
		if ag.Park == nil || ag.Park.ReviewHead == "" {
			continue
		}
		on := loopParkOn(ag.Park)
		if on == "" {
			continue
		}
		if l.dryRun || !shouldEndReviewWorker(st, ag) {
			continue
		}
		if loopInTurn(l.a, ag.Party) {
			l.sayOnce("turn "+ag.Name, "SUPERVISOR DONE WAITS %s: its session runs a turn", ag.Name)
			continue
		}
		var selector, model string
		worktreeDir := ag.Park.Worktree
		if k := st.StartIndex(ag.Party); k >= 0 {
			model = st.Starts[k].Model
			if st.Starts[k].Execution != nil {
				selector = st.Starts[k].Execution.Selector
			}
			if worktreeDir == "" {
				worktreeDir = st.Starts[k].Dir
			}
		}
		rp := state.ReviewPark{
			On:         on,
			At:         ag.Park.At,
			ReviewHead: ag.Park.ReviewHead,
			Agent:      ag.Name,
			Party:      ag.Party,
			Execution:  ag.Execution,
			Selector:   selector,
			Model:      cmp.Or(ag.Model, model),
			Serves:     reviewParkServes(st, ag, on),
			PostMerge:  ag.Park.PostMerge,
			Worktree:   worktreeDir,
		}
		if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			current.UpsertReviewPark(rp)
			return nil, nil
		}); err != nil {
			return err
		}
		if ref := ag.Execution; ref != nil {
			if r, _ := executionRecord(st, *ref); r == nil || !r.Terminal() {
				if _, err := l.run(ctx, agentsName, loopStop, ag.Name, "--reason", "supervisor loop: desk review requested"); err != nil {
					l.sayOnce("stop "+ag.Name, "SUPERVISOR STOP FAILED %s: %v", ag.Name, err)
					continue
				}
				if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
					if r, _ := executionRecord(current, *ref); r != nil {
						r.Observation.StopAcknowledged = true
						r.Observation.ProcessState = execution.Exited
					}
					return nil, nil
				}); err != nil {
					return err
				}
			}
		}
		if _, err := l.run(ctx, agentsName, loopRemove, ag.Name); err != nil {
			l.sayOnce("remove "+ag.Name, "SUPERVISOR REMOVE FAILED %s: %v", ag.Name, err)
			continue
		}
		if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			current.UpsertReviewPark(rp)
			if i := slices.IndexFunc(current.Agents, func(other state.Agent) bool { return other.Is(ag.Party) }); i >= 0 {
				removeAgent(current, i)
			}
			return nil, nil
		}); err != nil {
			return err
		}
		l.a.cleanRemovedWorktree(ctx, worktreeDir, ag.Park.ReviewHead)
	}
	return nil
}

func (l *supervisorLoop) wakeOrStartFixer(ctx context.Context, st *state.State, act superloop.Action) error {
	if slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == act.Agent }) {
		return l.wake(ctx, act)
	}
	return l.startFixer(ctx, st, act)
}

const defaultFixerBrief = `{{if .ServesURL}}Continue {{.ServesURL}} and fix{{else}}Fix{{end}} the findings on {{.URL}} (branch {{.Branch}} at head {{.Head}} in {{.Repo}}).

{{.Task}}

Fetch origin and work on branch {{.Branch}} in a worktree under ~/.local/state/beekeeper/worktrees/{{.Repo}}/{{.Branch}} (or in the workspace beekeeper cloned for you when BEEKEEPER_EXECUTION_ID is set). Collect all fixes for the round, run the focused checks, then push once to {{.Branch}} (or run beekeeper agents publish when BEEKEEPER_EXECUTION_ID is set), wait for green CI on {{.URL}} and request the next review round with:
cd "<worktree>" && beekeeper agents park --on {{.Repo}}#{{.Number}}{{if .PostMerge}} --post-merge {{printf "%q" .PostMerge}}{{end}} "desk review"
`

const defaultRepairBrief = `{{if .ServesURL}}Continue {{.ServesURL}} and repair{{else}}Repair{{end}} the branch for {{.URL}} (branch {{.Branch}} at head {{.Head}} in {{.Repo}}).

{{.Task}}

Fetch origin and work on branch {{.Branch}} in a worktree under ~/.local/state/beekeeper/worktrees/{{.Repo}}/{{.Branch}} (or in the workspace beekeeper cloned for you when BEEKEEPER_EXECUTION_ID is set). Resolve the conflict or repair the branch, run the focused checks, then push once to {{.Branch}} (or run beekeeper agents publish when BEEKEEPER_EXECUTION_ID is set), wait for green CI on {{.URL}} and park from the worktree with:
cd "<worktree>" && beekeeper agents park --on {{.Repo}}#{{.Number}}{{if .PostMerge}} --post-merge {{printf "%q" .PostMerge}}{{end}} "desk review"
If the new head only rebases onto or merges the base branch to resolve conflicts, its earlier approval still covers it and the supervisor merges once CI is green without starting a new review.
`

const defaultContinuationBrief = `{{if .ServesURL}}Continue {{.ServesURL}} after {{.URL}}{{if .Head}} ({{.Head}}){{end}} in {{.Repo}}.{{else}}Continue the task for {{.URL}}{{if .Head}} ({{.Head}}){{end}} in {{.Repo}}.{{end}}

{{.Task}}

Fetch origin and work from {{.Branch}} in a worktree under ~/.local/state/beekeeper/worktrees/{{.Repo}} (or in the workspace beekeeper cloned for you when BEEKEEPER_EXECUTION_ID is set). Complete the remaining work and finish with beekeeper agents idle --done --report and --problem.
`

func (l *supervisorLoop) startFixer(ctx context.Context, st *state.State, act superloop.Action) error {
	if err := l.incidentStart(act.Ref); err != nil {
		return err
	}
	if l.holdingStarts() {
		return errLoopLater
	}
	r, ok := parseRef(act.Ref)
	if !ok {
		return refused("invalid pull request ref %q", act.Ref)
	}
	rp, hasRP := st.ReviewParkFor(act.Ref)
	var servesURL, postMerge string
	if hasRP {
		if rp.Serves != "" && !strings.EqualFold(rp.Serves, act.Ref) {
			servesURL = issueURL(rp.Serves)
		}
		if rp.PostMerge != nil {
			postMerge = strings.TrimSpace(*rp.PostMerge)
		}
	}
	name := cmp.Or(act.Agent, fmt.Sprintf("%s %d", repoName(r.Repo), r.N))
	place, err := l.startPlacement(ctx, name, r.Repo, false, false)
	if err != nil {
		if errors.Is(err, errLoopLater) {
			l.sayOnce("room "+act.Key, "SUPERVISOR FIXER WAITS %s: no room for a start yet on any host", act.URL)
			return errLoopLater
		}
		return err
	}
	tmpl := defaultFixerBrief
	if act.Kind == superloop.KindGo {
		tmpl = defaultRepairBrief
	}
	branch := ""
	head := act.Head
	if act.Kind == superloop.KindEnd {
		tmpl = defaultContinuationBrief
		l.mu.Lock()
		merged := l.merged[act.Ref]
		l.mu.Unlock()
		head = cmp.Or(merged.MergeCommit, act.Head, rp.ReviewHead)
		branch = cmp.Or(place.Ref, merged.Base, head)
		if place.remote() && place.Ref == "" {
			place.Ref = cmp.Or(merged.Base, merged.MergeCommit)
		}
	} else {
		if p, err := readLoopPull(ctx, act.Ref); err == nil {
			branch = p.Branch
		}
		if branch == "" {
			branch = fmt.Sprintf("pr-%d", r.N)
		}
		if place.remote() {
			place.Ref = branch
		}
	}
	brief, err := l.brief("", tmpl, act.Key, briefData{
		URL:       act.URL,
		Repo:      r.Repo,
		Number:    r.N,
		Head:      head,
		Branch:    branch,
		Worker:    act.Agent,
		Task:      act.Message,
		ServesURL: servesURL,
		PostMerge: postMerge,
	})
	if err != nil {
		return err
	}
	args := []string{agentsName, agentStartName, name, brief}
	if len(l.a.cfg.Capacity.Choices) > 0 {
		args = append(args, "--harness", harnessAuto)
	}
	if place.remote() {
		args = append(args, "--host", place.Selector, "--repo", place.Repo, "--ref", place.Ref)
	}
	args = append(args, place.excludeFlags()...)
	if err := l.claim(act, name); err != nil {
		return err
	}
	if _, err := l.run(ctx, args...); err != nil {
		if Code(err) == ExitRefused && place.remote() {
			if _, rerr := l.remoteStartRefused(ctx, name); rerr == nil {
				return nil
			} else if !errors.Is(rerr, errLoopLater) {
				return rerr
			}
		}
		if Code(err) == ExitRefused {
			return errLoopLater
		}
		return err
	}
	if hasRP && rp.Serves != "" {
		if err := l.a.store.Update(func(current *state.State) ([]state.Event, error) {
			if i := slices.IndexFunc(current.Agents, func(ag state.Agent) bool { return strings.EqualFold(ag.Name, name) }); i >= 0 {
				inheritReviewParkOnAgent(current, &current.Agents[i], rp, l.a.timeNow().UTC())
			}
			return nil, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// briefData is what a brief template renders.
type briefData struct {
	URL, Repo, Head, Branch, Worker, Task, ServesURL, PostMerge string
	Number, Round                                               int
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
	if err := l.incidentStart(act.Ref); err != nil {
		return "", err
	}
	if l.holdingStarts() {
		return "", errLoopLater
	}
	r, _ := parseRef(act.Ref)
	brief, err := l.brief(l.a.cfg.Supervisor.ReviewBrief, defaultReviewBrief, act.Key, briefData{URL: act.URL, Repo: r.Repo, Number: r.N, Head: act.Head, Round: act.Round, Worker: act.Agent})
	if err != nil {
		return "", err
	}
	var authorFlags []string
	if l.a.cfg.Capacity.TaskPolicies[taskKindReview].DifferentFamily {
		st, err := l.a.store.Read()
		if err != nil {
			return "", err
		}
		if m := reviewAuthorModel(st, act); m != "" {
			authorFlags = []string{"--" + authorModelFlag, m}
		} else {
			return "", refused("model policy: review requires the author's recorded model")
		}
	}
	name := superloop.ReviewerName(act.Ref, act.Round)
	place, err := l.startPlacement(ctx, name, r.Repo, true, act.SelfHosted, authorFlags...)
	if err != nil {
		if errors.Is(err, errLoopLater) {
			l.sayOnce("room "+act.Key, "SUPERVISOR REVIEW WAITS %s: no room for a start yet on any host", act.URL)
			return "", errLoopLater
		}
		return "", err
	}
	args := []string{agentsName, agentStartName, name, brief, "--review"}
	args = append(args, authorFlags...)
	if len(l.a.cfg.Capacity.Choices) > 0 {
		// The reviewer takes capacity's pick, decided as a worker's start
		// is; a self-hosted model's work gets one on no self-hosted provider.
		args = append(args, "--harness", harnessAuto)
		if act.SelfHosted {
			args = append(args, "--"+reviewOfSelfHostedFlag)
		}
	}
	args = append(args, place.remoteStartFlags()...)
	args = append(args, place.excludeFlags()...)
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

func agentRecordedModel(st *state.State, ag state.Agent) string {
	if ag.Model != "" {
		return ag.Model
	}
	if k := st.StartIndex(ag.Party); k >= 0 {
		return st.Starts[k].Model
	}
	return ""
}

func reviewAuthorModel(st *state.State, act superloop.Action) string {
	for _, ag := range st.Agents {
		if ag.Name == act.Agent {
			if m := agentRecordedModel(st, ag); m != "" {
				return m
			}
		}
	}
	if rp, ok := st.ReviewParkFor(act.Ref); ok {
		if rp.Model != "" {
			return rp.Model
		}
		if k := st.StartIndex(rp.Party); k >= 0 && st.Starts[k].Model != "" {
			return st.Starts[k].Model
		}
	}
	for _, rp := range st.ReviewParks {
		if rp.Agent == act.Agent {
			if rp.Model != "" {
				return rp.Model
			}
			if k := st.StartIndex(rp.Party); k >= 0 && st.Starts[k].Model != "" {
				return st.Starts[k].Model
			}
		}
	}
	return ""
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
	// Exclude are the choices a local start leaves to the hosts
	// (supervisor.hostsFirst): those a mapped host can run, which wait for
	// a host with room rather than start here.
	Exclude []string
}

// excludeFlags are what a local start placed past the hosts' choices
// names on its agents start line.
func (h startHost) excludeFlags() []string {
	var args []string
	for _, e := range h.Exclude {
		args = append(args, "--"+excludeFlag, e)
	}
	return args
}

// hostChoices are the choices of k one of hosts can run, each as
// harness/model, the form --exclude matches exactly; mixed says whether
// k also has a choice no host can run, which only the local machine
// starts.
func hostChoices(k config.Capacity, hosts []execution.Host) (choices []string, mixed bool) {
	for _, ch := range k.Choices {
		if !hostRunsHarness(ch.Harness, hosts) {
			mixed = true
			continue
		}
		c := cmp.Or(strings.TrimSpace(ch.Harness), claudeComm) + "/" + strings.TrimSpace(ch.Model)
		if !slices.Contains(choices, c) {
			choices = append(choices, c)
		}
	}
	return choices, mixed
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

// startPlacement applies host preferences to workers and GitHub-enabled
// reviewers, retaining their review capacity flags.
// Without supervisor.hosts, or for a desk with no capacity.choices
// (a remote start needs a model name from the pick), it stays local (the
// zero value). Otherwise the local verdict (capacity --json, with the
// start kind's flags) decides first: room keeps the start local unless
// supervisor.hostsFirst; a capacity read that did not run waits, as it
// did before hosts existed. Then the first host whose own check (capacity
// --host, which probes the host first) has room and that maps the
// repository takes it, pinned to the repository's default branch; a host
// that cannot be read, maps no repository or has no room is said once (or
// passed quietly) and skipped; a host's check weighs only the choices it
// can run (Claude Code). With supervisor.hostsFirst and no host with room
// a start a host could run waits, as it does on a full machine; a start no
// host can run starts locally within the local room (capacity.local.
// ceiling among its guards): one whose repository no host maps, or the
// choices no host has an adapter for, the host-run choices excluded (see
// hostsFirstLocal). Without the preference the local machine attempts it,
// whose own start gate makes it wait when it has none either.
func (l *supervisorLoop) startPlacement(ctx context.Context, name, repo string, review, selfHosted bool, extra ...string) (startHost, error) {
	kind := workerRole
	if review {
		kind = taskKindReview
	}
	model := l.a.ruleModel(name, kind)
	flags := startFlags(review, selfHosted)
	if model != "" {
		flags = append(flags, modelFlag, model)
	}
	flags = append(flags, extra...)
	// A remote reviewer needs the per-worker GitHub adapter to read the PR
	// and post its review. Publication-only hosts cannot perform reviews.
	if review && (l.a.cfg.GitHub.WorkerApp == nil || !l.placesRemotely()) {
		if !l.capacityRoom(ctx, flags...) {
			return startHost{}, errLoopLater
		}
		return startHost{}, nil
	}
	k := l.a.cfg.Supervisor
	if !l.placesRemotely() {
		// Nothing to place remotely: the local machine does what it did
		// before, and the start's own gate makes it wait when it has no
		// room. When agents.models selects a model, check its room before
		// claiming the action.
		if model != "" && !l.capacityRoom(ctx, flags...) {
			return startHost{}, errLoopLater
		}
		return startHost{}, nil
	}
	out, code, err := beekeeperRun(ctx, l.as(append([]string{loopCapacity}, flags...)...)...)
	if err != nil || code != 0 {
		return startHost{}, errLoopLater
	}
	if roomOpen(out) && !k.HostsFirst {
		return startHost{}, nil
	}
	mapped := false
	for _, selector := range k.Hosts {
		h, err := l.a.executionHost(selector)
		if err != nil {
			l.sayOnce("host "+selector, "SUPERVISOR HOST FAILED %s: %v", selector, err)
			continue
		}
		if !h.AdmitsRepository(repo) {
			l.sayOnce("host "+selector+" "+repo, "SUPERVISOR HOST WAITS: %s maps no %s", selector, repo)
			continue
		}
		mapped = true
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
		return l.hostsFirstLocal(ctx, flags, model, out, mapped)
	}
	if model != "" && !roomOpen(out) {
		return startHost{}, errLoopLater
	}
	return startHost{}, nil
}

// hostsFirstLocal is supervisor.hostsFirst's answer when no host took a
// start: hosts are preferred only for what a host can run. A repository
// no host maps, or a desk whose choices no host can run, starts locally
// while the local verdict (localRoom) has room. With a mapped repository
// the choices a host can run wait for one; the others start locally, the
// host-run choices excluded from the start, while the local verdict
// without them has room and picks no self-hosted choice the local verdict
// (local, with every choice) does not pick itself: excluding the
// subscriptions a host runs never makes a self-hosted model the pick while
// a subscription is eligible. Everything else waits.
func (l *supervisorLoop) hostsFirstLocal(ctx context.Context, flags []string, model, local string, mapped bool) (startHost, error) {
	localRoom := roomOpen(local)
	var hosts []execution.Host
	for _, selector := range l.a.cfg.Supervisor.Hosts {
		if h, err := l.a.executionHost(selector); err == nil {
			hosts = append(hosts, h)
		}
	}
	hosted, mixed := hostChoices(filterChoiceModel(l.a.cfg.Capacity, model), hosts)
	if !mapped || len(hosted) == 0 {
		if !localRoom {
			return startHost{}, errLoopLater
		}
		return startHost{}, nil
	}
	if !mixed {
		return startHost{}, errLoopLater
	}
	place := startHost{Exclude: hosted}
	out, code, err := beekeeperRun(ctx, l.as(append(append([]string{loopCapacity}, flags...), place.excludeFlags()...)...)...)
	if err != nil || code != 0 || !roomOpen(out) {
		return startHost{}, errLoopLater
	}
	providers := l.a.cfg.Capacity.Providers
	if pick := verdictProvider(out); providers[pick].SelfHosted() && !providers[verdictProvider(local)].SelfHosted() {
		l.sayOnce("self-hosted "+pick, "SUPERVISOR HOST WAITS: no host has room, and without the choices a host runs the local pick is self-hosted %s while a subscription is eligible", pick)
		return startHost{}, errLoopLater
	}
	return place, nil
}

// verdictProvider is the provider of a `capacity --json` answer's pick:
// empty when it picks none or cannot be read.
func verdictProvider(out string) string {
	var v struct {
		Choice *struct {
			Provider string `json:"provider"`
		} `json:"choice"`
	}
	if json.Unmarshal([]byte(out), &v) != nil || v.Choice == nil {
		return ""
	}
	return v.Choice.Provider
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
	if l.holdingStarts() {
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
	l.mu.Lock()
	if l.boardNone != nil {
		err := l.boardNone.err
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	st, err := l.a.store.Read()
	if err != nil {
		return nil, err
	}
	if boardBudgetLow(st.Budget, l.a.cfg.GitHub.Floor, l.a.timeNow()) {
		l.mu.Lock()
		l.boardNone = &loopBoardNone{err: errLoopLater}
		l.mu.Unlock()
		l.sayOnce("board budget", "SUPERVISOR NEXT HELD: the GitHub budget is under github.floor (%d), no board walk until it recovers", l.a.cfg.GitHub.Floor)
		return nil, errLoopLater
	}
	l.clearSaid("board budget")
	next, err := l.readNextItem(ctx)
	if next == nil {
		l.mu.Lock()
		l.boardNone = &loopBoardNone{err: err}
		l.mu.Unlock()
	}
	return next, err
}

// boardBudgetLow reports whether a board walk, which costs a large share of
// the GraphQL budget, must wait: the REST budget is under floor until its
// reset, GraphQL refuses, or the last GraphQL reading is under floor until
// its window resets (its reset, else an hour after the reading).
func boardBudgetLow(b *state.Budget, floor int, now time.Time) bool {
	if lowBudget(b, floor, now) {
		return true
	}
	if b == nil || b.GraphQL == nil {
		return false
	}
	g := b.GraphQL
	if g.Blocks(now) {
		return true
	}
	if g.Remaining >= floor {
		return false
	}
	if !g.Reset.IsZero() {
		return now.Before(g.Reset)
	}
	return now.Sub(g.At) < time.Hour
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
		if isLoopGitHubHeldOutput(oneLine(out)) {
			return nil, errLoopLater
		}
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
	none := string(next.Pick) == "null"
	if code != 0 {
		// Only a document that says no pick settles a refusal.
		if !none {
			return nil, failed()
		}
		return nil, nil
	}
	if none || len(next.Pick) == 0 {
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
	if err := l.incidentStart(next.Ref); err != nil {
		return "", err
	}
	k := l.a.cfg.Supervisor
	r, _ := parseRef(next.Ref)
	name := fmt.Sprintf("%s %d", repoName(r.Repo), r.N)
	place, err := l.startPlacement(ctx, name, r.Repo, false, false)
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
	args = append(args, place.excludeFlags()...)
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
	st, err := l.a.store.Read()
	if err != nil {
		return "", err
	}
	if st.AutoInstallPaused {
		return "", errLoopLater
	}

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
			_ = l.a.store.Record(event(loopParty, loopClaimEndVerb, "[%s] %v", act.Key, err))
			return "", err
		}
	}
	if out, code, err := beekeeperRun(ctx, l.as(budgetGateArgs...)...); err != nil || code != 0 {
		return "", fmt.Errorf("%s installed, but budget --gate exits %d: %s", version, code, oneLine(out))
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
		l.mu.Lock()
		doneStage := l.stages[stage]
		l.mu.Unlock()
		if !doneStage {
			out, errOut, code, err := loopCommand(ctx, "", argv)
			if err != nil || code != 0 {
				return "", fmt.Errorf("%s installed, supervisor.postInstall failed (exit %d, %v): %s", version, code, err, oneLine(out+errOut))
			}
			if err := l.a.store.Record(event(loopParty, loopStageVerb, "[%s] ran", stage)); err != nil {
				return "", err
			}
			l.mu.Lock()
			l.stages[stage] = true
			l.mu.Unlock()
		}
		what = append(what, "supervisor.postInstall ran")
	}
	out, code, err := beekeeperRun(ctx, l.as(selfUpdateCheck...)...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s installed, but self-update --check exits %d: %s", version, code, oneLine(out))
	}
	if err := l.wakeReleased(ctx, act); err != nil {
		return "", err
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
	// A park the loop cannot place is the worker's to classify: it is told
	// how to park on a wait the loop watches, and nothing goes to the
	// person, who could weigh no option of it.
	if label == "unclassified" && e.Verb == parkEvent {
		act.Message = unclassifiedParkMessage(e)
		if err := l.wake(ctx, act); err != nil {
			return "", err
		}
		return fmt.Sprintf("woke %s to park on a wait the loop watches (%s): no note for the person", e.Agent, why), nil
	}
	ref := agentRef(st, e.Agent)
	if ref == "" {
		return fmt.Sprintf("dropped %s from %s: names no issue or pull request to file the guide's note on; %s", e.Verb, e.Agent, e.Detail), nil
	}
	q := fmt.Sprintf("Decide what %s does next about its %s (%s)?", e.Agent, strings.TrimPrefix(e.Verb, "agents."), label)
	if err := l.toGuide(ctx, act, ref, q, fmt.Sprintf("%s logged %s at %s: %s", e.Agent, e.Verb, e.At.UTC().Format(time.RFC3339), e.Detail), why,
		fmt.Sprintf("leave %s as it is: it keeps its task", e.Agent)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s to the guide (%s) on %s", e.Verb, label, ref), nil
}

// unclassifiedParkMessage tells the worker whose park e the loop cannot
// place how to park so that it can.
func unclassifiedParkMessage(e superloop.Event) string {
	return fmt.Sprintf("Your park at %s (%q) names no wait the supervisor loop watches, so nothing would ever resume you. "+
		"If your task is finished, report it: agents idle --done --report and --problem. Otherwise park again on what you wait for: "+
		"a pull request (agents park --on <owner/repo#n>), a time or an external state (timer add --wake <you>, then agents park), "+
		"or a person's decision (note add --for <person> with its status quo, why, options and default, then agents park --on '#<note>').",
		e.At.UTC().Format(time.RFC3339), truncate(oneLine(e.Detail), 300))
}

// fileReportSeparator identifies reports stored in the event itself. Older
// file transitions only summarized delivery to a note and must not be replayed.
const fileReportSeparator = "; file in the owning repositories:\n"

// toFile leaves the full finding in the settled transition's event. The
// guide's feed reads that event once; no decision or thread is needed.
func (l *supervisorLoop) toFile(_ context.Context, st *state.State, act superloop.Action) (string, error) {
	e := *act.Event
	from := ""
	if ref := agentRef(st, e.Agent); ref != "" && !strings.Contains(act.Message, ref) {
		from = " (from " + ref + ")"
	}
	return fmt.Sprintf("%s reported at %s%s%s%s", e.Agent, e.At.UTC().Format(time.RFC3339), from, fileReportSeparator, act.Message), nil
}

// readIntakeAnchor resolves the board item that ref should be ranked right
// after (Client.RankAnchor).
var readIntakeAnchor = func(ctx context.Context, a *app, ref, from string) (string, error) {
	return a.boardClient().RankAnchor(ctx, ref, from)
}

func (l *supervisorLoop) ownOverlap(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	blockStage := act.Key + " blocked"
	l.mu.Lock()
	blocked := l.stages[blockStage]
	l.mu.Unlock()
	if act.Block != "" && !blocked && l.a.cfg.Board.Owner != "" && l.a.cfg.Board.Project != 0 {
		if _, err := l.run(ctx, loopBoard, "unblock", act.Ref, "--by", act.Block); err != nil {
			return "", fmt.Errorf("remove inverse overlap dependency: %w", err)
		}
		if _, err := l.run(ctx, loopBoard, "block", act.Block, "--by", act.Ref); err != nil {
			return "", fmt.Errorf("record overlap dependency: %w", err)
		}
		if err := l.a.store.Record(about(event(loopParty, loopStageVerb, "[%s] %s blocked by %s", blockStage, act.Block, act.Ref), act.Ref)); err != nil {
			return "", err
		}
		l.mu.Lock()
		l.stages[blockStage] = true
		l.mu.Unlock()
	}
	if err := l.wakeOrStartFixer(ctx, st, act); err != nil {
		return "", err
	}
	if act.Block != "" {
		return fmt.Sprintf("owner %q assigned to %s (%s blocked by %s)", act.Agent, cmp.Or(act.URL, act.Ref), act.Block, act.Ref), nil
	}
	return fmt.Sprintf("owner %q assigned to %s", act.Agent, cmp.Or(act.URL, act.Ref)), nil
}

// filedIssues reads the issues created by sessions active since the loop
// started (Work.Created), with the item or pull request each session was
// working on when it filed the issue.
func (l *supervisorLoop) filedIssues(st *state.State) []superloop.FiledIssue {
	sessions, _, err := loopSessions(l.a)
	if err != nil || len(sessions) == 0 {
		return nil
	}
	var out []superloop.FiledIssue
	for _, s := range sessions {
		if !s.LastActive.IsZero() && s.LastActive.Before(l.since) {
			continue
		}
		w, _ := harnessForSession(s).ReadTranscript(l.a, s, l.a.timeNow())
		if len(w.Created) == 0 {
			continue
		}
		p := s.Party()
		agent := p.Name
		from := ""
		if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) }); i >= 0 {
			agent = st.Agents[i].Name
			from = agentRef(st, agent)
		} else if st.Guide != nil && st.Guide.Holder != nil && st.Guide.Holder.Is(p) {
			agent = guideName
		}
		if from == "" {
			for _, list := range [][]string{w.Refs, w.Mentioned} {
				for _, r := range list {
					if !slices.ContainsFunc(w.Created, func(c string) bool { return strings.EqualFold(c, r) }) {
						from = r
						break
					}
				}
				if from != "" {
					break
				}
			}
		}
		for _, c := range w.Created {
			r, ok := parseRef(c)
			if !ok {
				continue
			}
			ref := refName(r)
			url := fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N)
			out = append(out, superloop.FiledIssue{Ref: ref, URL: url, Agent: agent, From: from})
		}
	}
	return out
}

// intakeIssue puts act.Ref onto the configured board in Backlog, unless it
// already sits on another board where it is being worked, and ranks it on the
// board right after the epic or item it blocks or follows from.
func (l *supervisorLoop) intakeIssue(ctx context.Context, st *state.State, act superloop.Action) (string, error) {
	if boardBudgetLow(st.Budget, l.a.cfg.GitHub.Floor, l.a.timeNow()) {
		return "", errLoopLater
	}
	moveStage := act.Key + " move"
	l.mu.Lock()
	moveLine := l.stageDetail[moveStage]
	moved := l.stages[moveStage]
	l.mu.Unlock()
	if !moved {
		if err := l.claim(act, act.Ref); err != nil {
			return "", err
		}
		out, err := l.run(ctx, loopBoard, "move", act.Ref, "backlog", "--intake")
		if err != nil {
			if Code(err) == ExitRefused && strings.Contains(err.Error(), " is no issue:") {
				return fmt.Sprintf("dropped intake of %s: not an issue (%s)", act.Ref, oneLine(out)), nil
			}
			return "", err
		}
		l.mu.Lock()
		l.boardNone = nil
		l.fillPause = fillPause{}
		l.mu.Unlock()
		moveLine = oneLine(out)
		if strings.HasPrefix(moveLine, "left ") {
			return moveLine, nil
		}
		if err := l.a.store.Record(event(loopParty, loopStageVerb, "[%s] %s", moveStage, moveLine)); err != nil {
			return "", err
		}
		l.mu.Lock()
		l.stages[moveStage] = true
		if l.stageDetail != nil {
			l.stageDetail[moveStage] = moveLine
		}
		l.mu.Unlock()
	}
	from := cmp.Or(act.Message, agentRef(st, act.Agent))
	anchor, err := readIntakeAnchor(ctx, l.a, act.Ref, from)
	if err != nil {
		return "", err
	}
	if anchor == "" && from != "" && !superloop.IsPull(from) {
		if r, ok := parseRef(from); ok && !strings.EqualFold(refName(r), act.Ref) {
			anchor = refName(r)
		}
	}
	if anchor != "" && !strings.EqualFold(anchor, act.Ref) {
		rankOut, err := l.run(ctx, loopBoard, "rank", act.Ref, "--after", anchor)
		if err != nil {
			return "", err
		}
		if moveLine == "" {
			return oneLine(rankOut), nil
		}
		return fmt.Sprintf("%s; %s", moveLine, oneLine(rankOut)), nil
	}
	return moveLine, nil
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
		for i := len(st.WorkerReports) - 1; i >= 0; i-- {
			if r := st.WorkerReports[i]; r.By.Is(ag.Party) || r.By.Name == name {
				if m := refURL.FindString(r.Task + " " + r.Text); m != "" {
					return m
				}
			}
		}
	}
	for i := len(st.WorkerReports) - 1; i >= 0; i-- {
		if r := st.WorkerReports[i]; r.By.Name == name {
			if m := refURL.FindString(r.Task + " " + r.Text); m != "" {
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
	githubHeld := boardBudgetLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow()) ||
		a.cfg.GitHub.Floor > 0 && loopBudgetUnknownOrLow(st.Budget, a.cfg.GitHub.Floor, a.timeNow())
	found := a.loopTimerChecks(ctx, acting, a.timeNow(), githubHeld || a.resetFirst(st))
	// Avoid a write on every idle loop pass.
	_, _, _, changed := settleTimersMatching(st, found, a.timeNow(), a.cfg.Supervisor.LoopInterval.Duration, timerActs)
	if !changed {
		return nil
	}
	var lines []string
	var fires []timerFire
	if err := a.store.Update(func(current *state.State) ([]state.Event, error) {
		var evs []state.Event
		lines, evs, fires, _ = settleTimersMatching(current, found, a.timeNow(), a.cfg.Supervisor.LoopInterval.Duration, timerActs)
		return evs, nil
	}); err != nil {
		return err
	}
	// Each batch keeps its own time while later passes advance the clock.
	w := &watcher{app: a.snapshotAtNow()}
	for _, line := range lines {
		w.emitNow("timer", "%s", line)
	}
	l.timerActs.Go(func() {
		w.actTimers(ctx, fires)
		w.timerActs.Wait()
	})
	return nil
}

// loopReleaseContains fails closed: only this program's repository is installed
// by self-update, and GitHub must prove the merge is an ancestor of its tag.
var loopReleaseContains = func(ctx context.Context, ref, commit, version string) (bool, error) {
	r, ok := parseRef(ref)
	if !ok || r.Repo != "giantswarm/beekeeper" || commit == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := github.RunGH(ctx, "api", "repos/"+r.Repo+"/compare/"+commit+"..."+version, "--jq", ".status")
	if err != nil {
		return false, err
	}
	status := strings.TrimSpace(string(out))
	return status == "ahead" || status == "identical", nil
}

func (l *supervisorLoop) wakeReleased(ctx context.Context, release superloop.Action) error {
	l.mu.Lock()
	workers := slices.Clone(l.postMerge)
	l.mu.Unlock()
	for _, w := range workers {
		stage := "post-merge wake " + w.Ref + "@" + w.Head + " " + w.Agent
		checked := release.Key + " excludes " + w.Ref + "@" + w.Head
		l.mu.Lock()
		skip := w.Head == "" || l.stages[stage] || l.stages[checked]
		l.mu.Unlock()
		if skip {
			continue
		}
		contains, err := loopReleaseContains(ctx, w.Ref, w.Head, release.Version)
		if err != nil {
			return err
		}
		if !contains {
			if err := l.a.store.Record(event(loopParty, loopStageVerb, "[%s] merge absent from installed release", checked)); err != nil {
				return err
			}
			l.mu.Lock()
			l.stages[checked] = true
			l.mu.Unlock()
			continue
		}
		w.Key = stage
		w.Message = fmt.Sprintf("Release %s is installed and contains the merge of %s (%s). Complete the live proof and board update, then report with agents idle --done --report and --problem.", release.Version, w.URL, w.Head)
		if err := l.wake(ctx, w); err != nil {
			return err
		}
		if err := l.a.store.Record(event(loopParty, loopStageVerb, "[%s] woke worker", stage)); err != nil {
			return err
		}
		l.mu.Lock()
		l.stages[stage] = true
		l.mu.Unlock()
	}
	return nil
}

// startEffort keeps a matching agents.models effort ahead of the loop's
// default; the start resolves that rule itself.
func (l *supervisorLoop) startEffort(args []string) []string {
	kind := workerRole
	if slices.Contains(args, "--"+reviewFlag) || slices.Contains(args, "--"+reviewOfSelfHostedFlag) {
		kind = taskKindReview
	}
	if i := slices.Index(args, taskKindArg); i >= 0 && i+1 < len(args) {
		kind = args[i+1]
	}
	if l.a.cfg.Agents.EffortFor(args[2], kind) != "" {
		return nil
	}
	return effortFlags(l.a.cfg.Supervisor.Effort)
}

func (l *supervisorLoop) holdingStarts() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startsHeld
}

func (l *supervisorLoop) holdStarts(held bool) {
	l.mu.Lock()
	l.startsHeld = held
	l.mu.Unlock()
}

// pauseForReexec lets outstanding streams drain without launching more work
// on a replaced binary. Only the main loop calls reexec at the quiet point.
func (l *supervisorLoop) pauseForReexec() bool {
	l.mu.Lock()
	b, failed := l.bin, l.reexecFailed
	l.mu.Unlock()
	return !failed && b != nil && b.Replaced()
}
