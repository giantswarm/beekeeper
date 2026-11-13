# The supervisor loop

The supervisor loop is the desk's supervisor. It is a user service, not a session: it fills
the desk from the board, watches pull requests and outages, and routes alerts.

## Filling the desk

With `supervisor.workerBrief` configured, refill runs on its own `supervisor.loopInterval` cadence,
independently of slow observations, retirement, intake and next bookkeeping. The first refill runs
immediately on startup. Each tick refreshes watch budget, machine and disk holds before filling
the desk to `capacity.ceiling` from the
next free board items, including after a restart or roster loss without a done report. Each start
rechecks capacity and stops when its gates refuse, the ceiling is reached, or the board offers
nothing. Refills never overlap. Shutdown and binary replacement cancel and drain the active refill;
a failed binary replacement restarts its cadence. Configuration reload waits for the active refill.
Completed start claims are not replayed on later ticks. Background refills never refresh the
observation pass's health heartbeat. Start holds and capacity, budget and delivery gates still apply. The stall alarm names the active
refill action (for example, capacity, board next or a worker start). `--once` refills within its pass.
Legacy done-triggered next actions settle as bookkeeping. An empty or failed board
read waits `supervisor.fillRetryEvery` (5m by default) before retrying, doubled for each further
empty or failed read in a row up to four times the interval (5m, 10m, 20m, 20m, …) until a start or
one of the changes below, unless `capacity.ceiling` changes or an agent is done or leaves the roster
first; a park, a resume, a wait or a new task, and the busy count and room they move, do not end it.

An empty or failed read answers the rest of that refill. `board next` exiting 3 with no pick
(nothing free) is an empty read and is never retried as a failed start. No board read runs while
the GitHub budget is under `github.floor` (REST until its reset, or the last GraphQL reading
until its reset, an hour after the reading when GitHub named none, or a GraphQL refusal):
the refill skips its read, without a failure or a pause.

The loop also holds ownership and PR observations under the GraphQL floor or while the reading is
unknown or stale. Board queries and tracked PR reads check a fresh budget against a durable
allowance of 1,500 points per hour, keeping 300 points for two identities' budget probes and
reserving 500 points before each work request. Budget probes run at most every 30 seconds.
Concurrent timer commands share its lock. An interrupted request keeps its reservation. A step's
cost is counted between two fresh readings, each a GraphQL query per identity, so only steps that
query GraphQL take them: the pass admits itself on the saved reading, which it refreshes once it is
a minute old, and the conditional read of a repository's newest pull request takes none.

Traffic between observations counts conservatively toward the allowance as `shared`; a busy desk can
therefore pause the loop early. `budget --json` exposes the window, spent points and `board`,
`pull`, `watch`, `timers`, `inventory` and `shared` totals under `loop`. The embedded watch's note
and parked-PR reads and GitHub timer conditions share the allowance; local timer jobs remain
independent. `budget` names the stored `gh` login and the configured devctl `app` login, with each
account's login, counters and reset under JSON `identities`.

Both routes are explicit, independent of the checkout, and cached separately. Each identity retains
all unexpired observed GraphQL `windows`: a newer high window cannot erase an older drained one, and
counters cannot increase before their window resets. An older unlabelled low reading is retained as
`unlabelled` until its reset rather than silently reassigned to a credential. The top-level REST and
GraphQL readings each select their lowest balance; `budget --gate`, merge admission and the loop use
those minima.

An unknown or missing previously configured identity holds the gate. Refusals on a higher-balance
identity also hold it without hiding the lowest counter. Loop spend tracks each identity's counter
separately; selecting another minimum or a GitHub window resetting never replenishes its own hourly
allowance. One independent watcher reads the pull requests of active deliveries, parks and sweep
discoveries in GraphQL batches of up to 60. It polls every 30 seconds while checks or mergeability
are pending and every two minutes when settled. A pull request no check has reported on ten minutes
after its last update counts as settled: no CI is coming until a push updates it. So does one whose
mergeability GitHub still reports unknown then: the two-minute polls read it until GitHub has it.

Files, body and reviews refresh only when the head or `updatedAt` changes. Errors and rate limits
are logged once per episode and back off exponentially to ten minutes, respecting `github.floor`.
The pass never waits on these reads: it consumes dated observations, treating nonterminal ones older
than five minutes as unknown. `beekeeper supervisor status` shows observation age; `--once` and
`--dry-run` consume the watcher's saved observations. Terminal pull requests leave the poll set
after their final read.

Repository backlog inventories use paged GraphQL connections instead of REST search. The loop reuses
each inventory for five minutes, then checks the latest updated pull in all states with an ETag: an
unchanged response costs no REST core points and skips the GraphQL inventory. A full refresh runs at
least hourly; missing validators and failed or incomplete reads never certify an empty backlog.
Background inventory and orphan scans always share a separate durable allowance of 1,000 REST points
per window, reserving 50 points before a batch and preserving an enabled `github.floor` for other
work. The conditional check is admitted while the allowance and the saved reading have room, and
takes no budget reading: it spends no GraphQL, and unchanged no REST either.

Disabling the shared floor for emergency access does not disable this scan allowance or make scans
depend on GraphQL admission. Fresh conditional budget samples settle the reservation; an interrupted
or unreadable sample keeps it. Concurrent callers' spending during a batch counts conservatively.
Exhausting this allowance holds background scans until reset; tracked PR reads, reviews and merges
retain their own admission. Supervisor status names the scan hold.

`supervisor loop --dry-run` previews refill intent with read-only capacity
and board queries and starts nothing.

## Action briefs

The loop stores briefs under `<state dir>/supervisor-loop/`. New filenames keep up to 160 bytes
before `.md`; longer action keys use a readable prefix and 16 hex digits of the key's SHA-256.
A red-check brief still lists every failed check and its available URL, regardless of the filename
length. Quota recovery reopens the review round's brief, including a longer filename written before
an upgrade when the new path does not exist.

## One desk review per pull request

A pull request gets one desk review
([#3861](https://github.com/giantswarm/beekeeper/issues/3861)). The loop starts it once the
head its worker parked on is green. Once any round gave a verdict, no review starts again
for that pull request, whatever head comes next: not by the loop, not by `agents review` by
hand (it refuses with exit 3), not after a continuation; `supervisor.reviewRounds` adds no
rounds. A running reviewer holds the pull request even if its head moves before the verdict.
A second round starts only when the first ended without any verdict, since the one review
has still to come.

- **The reviewer fixes its findings.** It pushes one commit to the branch and approves the
  head it pushed, without waiting for its CI. A red check on that head returns to the reviewer
  for a repair turn; once it is green the loop gives it the merge go and merges it.
  The approval stays the pull request's review when the head moves on later.
- **Findings the reviewer cannot fix** go to the worker word for word. Its green fix, parked
  on, gets the merge go without a second review.
- **Nobody updates the branch.** No worker, fixer, reviewer or the loop merges the base into
  an approved or green branch or rebases it to make it current: that only runs CI again.
  The loop merges the approved head as it is, and the merge-result check of base plus head
  is the gate. A real conflict returns to the reviewer for one repair, without another
  review verdict. A red merge result goes back to the worker as a finding. The repaired
  head merges on the same approval, and other pull requests keep merging meanwhile.

The behavioral scenarios `delivery/board-item-to-merged-and-done`,
`delivery/moved-head-keeps-its-active-reviewer`,
`delivery/push-after-approval-merges-without-a-second-review`,
`delivery/reviewer-fixes-and-the-loop-merges`, `merge-result/approved-head-merges-as-is-after-main-moved`,
`merge-result/a-real-conflict-is-repaired-once` and
`merge-result/red-result-hands-back-while-others-merge` hold these rules
([#3862](https://github.com/giantswarm/beekeeper/issues/3862)).

## Outages and emergencies

`beekeeper fatal status` reports each outage's scope. A red repository default
branch holds merges into that branch, except the reserved repair PR, and release
promotions from that repository. Workers in that repository and other repositories
keep starting; other repositories' merges and releases continue. The repair PR
keeps its exception through review and worker handoffs, but still requires green
CI and merge-result validation, independent review, and the normal stop and hold
checks. Infrastructure outages retain the global ordinary-start barrier. A
successful probe clears only its own outage; an unreadable probe clears none.

A default branch is red when a completed check run on its head failed, or a status context did in
every run of it on the head. CircleCI reports a job of each pipeline that builds a commit, the
branch's and a tag's, under one context, and GitHub's combined status keeps only the newest report:
a red context whose other run passed is no outage, one whose other run is still pending is unknown
until it ends, and a job that runs once, such as a release's push, is red when it fails.

The fatal sentinel (`beekeeper fatal scan`, once a minute) starts one repair
session per outage at a time, at most three per outage, ten minutes apart. A
repair start that fails runs no repair: the incident keeps its error
(`startError` and the outcome in `fatal status`, the `fatal.repair` event and
the sentinel's journal), and the next pass starts the next attempt without the
ten minutes. When the last start fails, the guide gets a GUIDE PROBLEM with that
error, and the sentinel starts no further repair of the outage.

On the person's explicit emergency authorization, the current guide can run `beekeeper emergency on --reason
"<their verbatim words>" --until 30m`, then `beekeeper emergency merge owner/repo 123`. The merge
requires GitHub's approval or an approved desk review of the observed head and matches that head; it
bypasses the merge lane. GitHub branch protection still applies, except for a desk-approved head
in a repository of `merge.admin`: there it passes `--admin`, as the loop's own merge does, once that
head is green, mergeable and contains its base, unless the base has a merge queue.
`emergency merge owner/repo 123 --admin --head <sha>` is the person's exception for a pull request
GitHub keeps at REVIEW_REQUIRED or with an unset review decision (no GitHub review required): the
current guide interprets the verbatim authorization, including a general merge order, and selects
one PR and head per invocation.

It must be green, mergeable and up to date with its base at that exact head, with an approved desk
review or the loop's go. It reads the base's effective rules and requires every required context on
that head, including any required App identity. It refuses unreadable requirements, classic
protection, and non-review rules it cannot verify. Ruleset `required_reviewers` is a review
requirement covered by this exception. `emergency hold set/lift`, `emergency start` (forced capacity
admission, retaining infrastructure outage barriers), and `emergency capacity capacity.ceiling 4`
use the same expiring grant.

Every action and result records the quoted reason. Under an active grant, admin merge attempts and
results also name the full PR URL and exact head, including refused or failed attempts; a moved head
requires a new invocation and approval or loop go for that head. `beekeeper emergency off` ends it
early; a successor guide needs a new grant. Direct merge commands and other guide work remain
guarded.

## Bot pull requests

A green bot pull request the desk did not open is left alone until its login
is in `supervisor.ownership.bots`. The sweep then lists the bot's open pull
requests in the repositories the desk owns (no `team`, the team's own by its
team files, or `supervisor.ownership.review`; never `never`): one without a
live owner for 15 minutes gets the loop's review, on approval its go, and the
gated merge follows as for the desk's own. A head the bot moves after the
approval keeps it when it only rebases onto the base or merges it; any other
change gets a worker that takes it over and parks for the go, never a second
review ([one desk review per pull request](#one-desk-review-per-pull-request)). A worker's park on one
requests the same review at once.

Independently, `beekeeper emergency auto-install pause --reason "<their words>"`
pauses the loop's release installation without stopping reviews, merges or
workers. Resume it with `beekeeper emergency auto-install resume --reason
"<their words>"`. The pause persists across restarts, until explicitly resumed;
`beekeeper emergency` shows both controls. An installation already running
finishes; the pause prevents the next installation step. Manual self-update
remains available.

## Release proofs after a merge

A worker whose reviewed pull request merges stays reachable for its release proof
and final `agents idle --done` report. The loop retires an unreported worker only
after `supervisor.postMergeWindow` (default `2h`) without activity, once its turn
ends; the log names its merged pull request and missing report. A worker whose
task spans several pull requests stays while it still serves an open issue
(`sessions serve`) or is parked on another open pull request; it is retired at
its own done report, once that work closed, or after `agents.staleAfter`
without activity, park or wake timer. A completed
self-update wakes retained workers whose merge commit the installed release
contains, after the broker and post-install steps finish. Stopping or retiring
a worker immediately reconciles any gate run that ended with it; a runner
still alive outside the worker continues under the gate.

## Alerts

The loop routes only relevant alerts, those the watch publishes at
`alerts.pageSeverity`:

1. The team's own alerts (`alerts.team`) are relevant 24/7: an alert of the
   team that only `InhibitionOutsideWorkingHours` inhibits is read and routed
   like any other. With `alerts.team` unset every alert is the desk's.
2. A silenced alert is ignored, the team's included: beekeeper creates no
   silence, so every silence is another person's or team's. It gets no
   summary line and no note.

Everything else, another team's or a team-less alert, gets no note and no
summary line. A relevant alert nobody owns that a session record explicitly
waits on (`sessions serve … --waits alert:<name>`) is one decision for the
guide, which decides whether a worker starts on it; the rest are one
`SUPERVISOR ALERTS unowned` summary line per pass. A worker parked on an
alert (`agents park --on alert:<name>`) is woken by it whoever's it is.

## The pull watch

One watcher reads the desk's pull requests for every pass: the parked ones, the tracked deliveries
and the open ones its listing finds. It reads them in GraphQL batches of up to 60, and those on
first sight, with their files, reviews and closing issues, in batches of up to 20. What it read is
saved in `supervisor-pull-observations.json`; a pass trusts an observation for 5 minutes.

GitHub can fail a batch it cannot answer in time: a 5xx, no answer, or its HTML error page in
place of JSON. The watcher then asks for the same pull requests again in halves, down to batches
of 5, and the halved size holds for the polls after it. It doubles again after each 30 minutes
without a fault. A batch that still fails at 5 is skipped, and the rest are read. What a failed
poll did read stands.

| Line | Says |
| --- | --- |
| `SUPERVISOR PULL WATCH GitHub failed a batch of …` | GitHub's answer (its HTTP status, the page's start), the halved size |
| `SUPERVISOR PULL WATCH read the … pull requests it asked for; …` | a poll that read everything after a fault, the size from now on |
| `SUPERVISOR PULL WATCH held: try <n> failed (failing since …)` | every failed poll, its error and the next try (1, 2, 4, 8, 10 minutes) |
| `SUPERVISOR PULL WATCH recovered: …` | the poll that ends the episode, with its failed tries |
| `SUPERVISOR PULL WATCH held: <budget cause>` | a budget hold, said once; it ends on its own |

`supervisor status` shows the episode: `pull watch failing since …, <n> tries (last complete poll
…): <error>`. When the watch has not read all its pull requests for 30 minutes, measured from its
start after a restart, the loop files one alarm for the guide. The alarm names the failed tries and
the last error, since no parked pull request gets a desk review, a merge decision or its CI's wake
until the watch reads again. `SUPERVISOR STALL ENDED: the pull watch reads the desk's pull requests
again` says when a poll completes.

## The supervisor loop down

The supervisor is the supervisor loop; no session holds its role. While the loop does not run,
the standby watch on Linux starts its `beekeeper-supervisor-loop` systemd service again (on
macOS launchd's `KeepAlive` restarts a failed loop instead), at most once a minute and
never while `beekeeper stop` stands (`SUPERVISOR LOOP RESTARTED`, or `SUPERVISOR LOOP RESTART
FAILED` with the reason). Once it has been down two minutes every watch says `SUPERVISOR LOOP
DOWN` once and the standby sends a critical `no-supervisor` notification, again after
`notify.repeat` while the gap lasts; the loop back is `SUPERVISOR LOOP BACK`, once. A watch under
a scratch configuration (`--config`, `BEEKEEPER_CONFIG`) restarts no service.

The loop's service can be alive while release work makes no progress. Both watches also
evaluate durable merge and worker-start outcomes, at most once a minute. Three consecutive
failures with the same normalized reason for one operation file one incident for the guide;
a successful corresponding operation resets that streak, including across restarts.
An approved, exact-head, green PR selected by the loop's merge policy also files an incident
once it has waited `watch.throughputAfter` (default `2h`). The report includes the running
candidate version, oldest eligible wait and public PR URLs. Recovery is filed once.
Capacity and budget refusals, stops, red CI and pending review do not add failures.
Missing observations get one minute for initialization; unreadable observations, or a snapshot
older than three minutes, report unknown rather than healthy. Retry backoff is independent.

### Pending merge decisions

The loop itself measures every merge decision the planner requests for an approved head. A
decision that neither merged nor was overtaken (merged, closed or a new head) within 15 minutes
files one alarm for the guide that names what holds it:

| Cause | What the alarm names |
| --- | --- |
| hold | the `hold:<target>` that stops the merge |
| GitHub budget | the budget under `github.floor`, `merge.resetFirst`, or held loop passes |
| outage | the default-branch outage that holds its repository |
| in flight | since when its merge command runs in the merge gate |
| lane | the pull request whose merge command runs ahead of it in the loop's stream for its lane |
| merge gate | the gate's own queued or running run, with its lane, phase and PID |
| merge policy | the repository policy refusal the loop retries |
| merge queue | the server-side merge queue it waits in |
| unread | the pull request the loop cannot read, so no pass plans or runs its decision |
| repair | the failed merge and why the branch repair it requested has no worker yet |
| failed | the last run's error and, under backoff, when the loop retries |
| restart | a fenced loop that runs no action while it restarts into a replaced binary |
| pass | the error that ended the pass before it planned |

The alarm is delivered once per pending head and cause, and no sooner than 15 minutes after
that head's last alarm: a hold that changes is named again, one that alternates is not repeated.
A pass that ends before it plans (held GitHub observations, a restart, an error) still measures
the pending decisions and names what ended it. The waits, the causes already named and the last
cause read survive a restart of the loop, so a new release neither restarts the 15 minutes nor
repeats an alarm. `SUPERVISOR STALL ENDED` says when an alarmed decision is no longer pending.

## A new release

A release installed over the loop's binary is re-executed at the next quiet point: no claim in
flight (a merge, a review or worker start, a refill, intake, retirement). While claims are in
flight the loop says `SUPERVISOR LOOP restart held: claims are in flight (<claims>)` once, with
the time it re-executes at the latest, and its passes go on: they observe, plan and run the merge
decisions, and skip their housekeeping (reports, timers, alerts, wakes, recoveries) and every
other action, which wait for the new binary. A merge's claim ends when GitHub reports the merge:
its gate leaves the release wait to its merge-child (`--until-merged`, [the merge
gate](merge-gate.md)), and the loop tracks the delivery's release on its own.

`supervisor.restartWait` (default `10m`) bounds the wait. Past it, or once the installed release
refuses a save of the old binary, the loop says `SUPERVISOR LOOP restart waited …`, cancels the
actions in flight and re-executes; the new binary reconciles their claims, and a merge's gate and
merge-child run on outside the loop. A loop whose state writes are fenced after a binary recovery
re-executes at once.

## Filing findings

The loop places a finding itself when the report names its owner and supplies a stable title and
complete current description. Use an existing issue's full URL when known:

```text
https://github.com/example/app/issues/12: Queue stalls

## Problem
The queue stalls when a lease expires.

## Proposed solution
Keep queued work when the lease expires.

## Acceptance criteria
The next free worker receives the queued work.
```

For a new finding, the first line is `example/app: Queue stalls`. Send the multiline text as one
argument to `agents problem` or `agents idle --problem`. The loop reads every page of the owner's
open issues and matches the explicit issue number or a unique title (ignoring case and whitespace).
An identical description causes no issue write. New facts replace the description in place; the
report must retain relevant existing facts and acceptance criteria. There are no issue comments.
A title absent from the inventory creates one issue. The ordinary intake step puts it in Backlog
and ranks it by its originating item and the board rules.

Findings use one serialized stream. Durable receipts remember reports already placed across loop
restarts and reporter assignments, so an old repeated report cannot overwrite newer facts. Board
intake is retried after placement without creating another issue. Other failed GitHub reads and writes
remain pending; an unread inventory is never treated as empty.

Free prose, ambiguous titles, a closed explicit target, an owner outside the configured scope, a
repository GitHub does not find or whose issues are disabled, or an unconfigured board leave the
full finding for `GUIDE PROBLEM`. So do the one-line findings of a final report, which the loop
delivers together. The guide follows the same rule: known findings add nothing, new facts rewrite
descriptions, and only new findings create issues.
