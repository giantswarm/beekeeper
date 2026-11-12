package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/loop"
)

func (a *app) supervisorLoopCmd() *cobra.Command {
	var once, dryRun bool
	var recordDir string
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
            its reason: no reviewer, no go, no forward, and
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
            check on the head green: a pull request gets one desk
            review. Once any round gave a verdict no review starts again,
            whatever head comes next (supervisor.reviewRounds adds none); a
            second round starts only when the first ended without a verdict.
            The reviewer fixes its findings on the branch, pushes one commit
            and approves the head it pushed; findings it cannot fix go to
            the worker. A reviewer starts
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
            no capacity.choices never places remotely. Execution reviewers use
            agents review to read their assigned PR and linked issues and post
            their exact-head review through the coordinator, without a host
            GitHub credential
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
            since. Such a head needs no new park; any other head after the
            review gets the go once its worker parked on it, never a second
            review. Nobody updates an approved branch with the base: the
            gate merges it as it is, its merge-result check the gate; only
            a real conflict or a red merge result goes back for repair. Only
            the verdict of the loop's own rounds counts, posted after the
            round started on its head or on one pushed during it (the
            reviewer's fixes): an older verdict or a round still running
            authorizes nothing
  forward   the last round's verdict asks for changes: the review, word
            for word, to the worker, whatever its head is now
  rework    the worker parked again after the forward on the head the
            findings were about, or on a head that only brings the base
            into it, which carries no fixes: the worker is told to fix them
            and push (agents wake), once for each such park; its new green
            head gets the merge go, no second review. Review and rework
            stay in the loop: no merge question goes to guide.person
  go        (after the review) a green head the worker parked on after the
            one review's verdict, its fixes or any later change, or, when a
            later reviewer finished without a verdict, an earlier round's
            verdict standing: the desk's rule is one review, then the fixes
            and the merge, so the merge go. No second reviewer starts, and
            no note asks guide.person whether to merge; rounds that gave
            no verdict at all get no go, said once as a leave line
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
            conflicts, is red, or carries an unfixed changes request; the merge go again for a green
            head whose go was given and never carried out; a reviewer for
            a green head awaiting its one review; a worker to take over and
            park on a head with commits past that review that nobody parked
            on, for the go without a second review. Running checks wait; an open
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
            acted on its round's verdict (a go, forward, rework
            or end), and a worker given the merge go after its post-merge
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
            and stopping finished or relieved
            background sessions). A session its person started is never
            retired
  delivery  track each issue from implementing through CI, review, fixing,
            merging and releasing to done (available tag, including an RC,
            or confirmed no_release). A merge alone does not free its place.
            delivery.wipLimit (0: off) bounds issues in flight independently
            of capacity; stale observations hold ordinary new starts.
            Pending reviews and fixes reserve room before new issues, and
            proceed even at the limit. Incident repairs, board.order steps
            with wipExempt, and manual starts remain exempt but counted.
            delivery --stats reports throughput and mean cycle time; choose
            the first limit as floor(released/day * mean cycle time in days).
            delivery drop --reason ends tracking only, without closing PRs
            or stopping workers. Stalled work keeps its place and is raised.
  next      after a done, and every pass below capacity.ceiling while capacity
            has room and delivery.wipLimit permits a new issue: the next
            item of board next starts (agents start, --harness auto with
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
review, go or forward:

  - outside github.allowedOrgs: nothing at all, not even a read
  - its repository matches supervisor.ownership.never: nothing, whatever
    else says; references in titles, descriptions, branches, changed paths
    or comments do not affect ownership
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
.Worker. One watcher reads tracked deliveries, parks and sweep pull requests
outside the pass, in aliased GraphQL batches of up to 60. It polls every 30s
while checks or mergeability are pending and every 2m when settled, with
exponential failure backoff capped at 10m. Details are fetched only when the
head or updatedAt changes. Observations older than 5m are unknown; status
shows their age. --once and --dry-run consume the saved observations. A plain wait
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
			if recordDir != "" && !dryRun {
				return fmt.Errorf("--record requires --dry-run")
			}
			if dryRun {
				return a.dryRunSupervisorLoop(cmd.Context(), recordDir)
			}
			return a.runSupervisorLoop(cmd.Context(), once)
		},
	}
	c.Flags().StringVar(&recordDir, "record", "", "write an anonymized planner input.json (requires --dry-run)")
	c.Flags().BoolVar(&once, "once", false, "one pass, then exit")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "one pass that prints the transitions it would take and takes none: read-only capacity and board queries, no log line or timer")
	return c
}

// runSupervisorLoop runs the loop on this command's state.
func (a *app) runSupervisorLoop(ctx context.Context, once bool) error {
	return loop.RunLoop(ctx, a, once)
}

// loopUnit is the user service that runs the loop, which beekeeper install
// always installs.
const loopUnit = "beekeeper-supervisor-loop.service"

// dryRunSupervisorLoop is the loop's dry run on this command's state.
func (a *app) dryRunSupervisorLoop(ctx context.Context, recordDirs ...string) error {
	return loop.DryRunLoop(ctx, a, recordDirs...)
}

// newSupervisorLoop is a loop on this command's state.
func (a *app) newSupervisorLoop(dryRun bool, progress ...func()) (*loop.Loop, error) {
	return loop.NewLoop(a, dryRun, progress...)
}
