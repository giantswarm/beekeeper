# Notifications and reports

Desktop notifications, how deliveries reach a session, and the scheduled status reporter.

## Desktop notifications

`beekeeper watch --notify` sends the events that need a person to the desktop's notification
service (`org.freedesktop.Notifications` on the session bus: dunst, mako, GNOME, KDE) and still
prints every line. Each notification carries a summary, the session or resource involved and the
command that shows more.

- `due`
  - Event: a note or a timer falls due
  - Urgency: normal
- `budget`
  - Event: the GitHub budget under the floor
  - Urgency: normal
- `stale-lease`
  - Event: a lease whose holder's session is gone
  - Urgency: normal
- `no-supervisor`
  - Event: the supervisor loop down for two minutes (on Linux the standby watch restarts its service
    meanwhile); again after `notify.repeat` while it lasts
  - Urgency: critical
- `page-unowned`
  - Event: a firing alert at `alerts.pageSeverity`, of `alerts.team` when set, that no session has
    owned (`beekeeper alerts own`) for `alerts.ownerGrace`; again every `alerts.ownerGrace` while it
    stays unowned
  - Urgency: critical

Nothing routine notifies: sessions starting or ending, alerts other than an unowned page, relays.
The machine's lines never notify: memory, swap, `OOMD IMMINENT`, OOM kills, load, CPU, processes,
tmpfs and disk are the supervisor's to act on, and its watch says them; a person gets no pop-up they
cannot act on. Each event is one notification however many watches run `--notify` on the same state:
the first to claim it in `notify.json` (under `notify.lock`) sends it.

A lasting condition (`budget`) and the supervisor loop down (`no-supervisor`, per gap) notify again
after `notify.repeat`. Quiet hours hold every notification that is not critical and send what they
held as one notification when they end. With no notification service on the bus the watch runs on,
prints its lines and says so once. `notify.json` also keeps the last 20 deliveries with the id the
service returned.

When the supervisor loop does not run, the same watch runs as a systemd user unit,
[`contrib/systemd/beekeeper-notify.service`](../contrib/systemd/beekeeper-notify.service), with
`--standby`; `beekeeper install` adds `--notify` to it only with `watch.notify: true`, so a desk
without desktop notifications leaves the key unset: while the supervisor loop runs it leaves the
notes, timers, session records and relays to the loop's watch and never reads the alerts, so it
takes nothing from the loop's view; what both see (the budget, stale leases) is sent once.

While the loop is down it restarts the loop's systemd service (Linux), and it never starts a session
for the supervisor. The upgrade holds never wait on a supervisor: while no supervisor's watch has
begun an upgrade cycle within five `watch.interval` (it says so in `upgrades-watch.json`), the
standby watch reads the upgrades on the same shared schedule, sets and lifts their holds and says
`UPGRADES read by the standby watch` once; beside a running supervisor's watch it reads none.

The supervisor loop's own watch sends no desktop notifications: the standby's `--notify` does.
`beekeeper install` writes and starts the unit with the binary's path; `journalctl --user -u
beekeeper-notify -f` shows its lines. The unit caps the watch at half a core (`CPUQuota=50%`): at
rest a tick costs a stat per file it read before, since the watches keep the parsed desktop records,
the transcripts' places and context sizes, and their place in the event log across ticks, and read
again only what changed.

### Deliveries

`beekeeper delivery` reads the desk's saved issue deliveries without contacting GitHub.
Each row shows the issue, its pull requests, the step, time in that step, total age,
worker and wait. A desk pull request with no linked issue counts once; a pull request
closing several issues counts once per issue. An unread pull stays `unknown` and counts.
`--json` returns the same reading with ages as durations in nanoseconds and the last
complete observation time. The supervisor status and the report's Queue relay its count.

`beekeeper delivery --stats --since 168h` reads `delivery.step` events for median and
worst residence time per step, including ongoing waits clipped to the window, and
released deliveries per day. Release candidates count as released once a containing tag
is confirmed. A failed or unconfirmed release stays `releasing`; research completed
without a pull request, or work closed unmerged without a continuing worker, is `dropped`.
The release verdict and tag survive removal of the merge from its lane.

An epic in the go status (`board.epics.go`, normally In Progress) gets an
owner session before its sub-issue workers. The owner maintains the ordered
plan and GitHub blocker dependencies, follows reports and reviews, and proves
acceptance after the children merge. It does not implement, review or merge.
It parks on the epic between events; changes to child delivery observations
wake it. Only proven acceptance closes the epic and moves it to Done.

`delivery.epicLimit` caps admitted epic owners (default `0`, off). Existing
owners keep their places; further epics wait in board order. An owner retains
its place while parked or waiting on a decision. `delivery.epicParallel`
defaults to `3`: the loop admits up to three flowing children per epic and
refills when one finishes. Waiting-on-person and blocked children are visible
but do not consume a flowing place. Each child still passes the normal board,
blocker, WIP, capacity, CI and independent review gates. Record dependencies
with GitHub blockers; sub-issue order supplies priority among independent
children. Configure an In Progress board step with `subIssues: true` to offer
the children. Owner sessions do not count against the ordinary delivery WIP.
`supervisor status` includes the last observed epic list, its observation time,
and each open child's queued, flowing, waiting-on-person, blocked or merged
state. An unreadable child remains unknown rather than proving completion.

`delivery.wipLimit` defaults to `0` (off), accepts nonnegative values through `beekeeper config set
delivery.wipLimit <n>`. When set, it holds new issues until a place is free. A delivery observation
older than the greater of ten minutes and three loop intervals also holds new starts. `board next --json`
includes `wip` (limit, count, items, freshness and reason) and `would_pick`, the first otherwise
free item. Its `pick` is null and exit status is 3 when no item is admitted; `skipped` and
`after_pick` still preview the ordinary walk. Claims count and record under one lock.

The loop checks before ordinary board or monitored queue reads. With no known exemption route,
full/stale intake makes no read. Configured exempt steps and active named incident repairs instead
allow exemption-only board discovery (`board next --wip-exempt-only`), retaining the existing
ranking and admission checks, skipping the monitored queue, and never falling back to ordinary
picks. The held limit sets no refill pause, so a completed release frees the next pass.

It checks again immediately before dispatch. With a worker brief configured, every pass that starts
no board worker logs `SUPERVISOR REFILL started 0:` with its reason, including full or stale
delivery admission (count, limit and last complete observation), capacity, GitHub budget, repository
holds, board backoff or an earlier refill still running. A repository hold skips its board items and
blocks new supervisor workers, reviewers and fixers until it expires or is lifted.

The loop logs each skipped item with its hold reason once; an explicit repository or issue exception
may still start. Lane and global merge holds continue to govern merges only. Passes that fail before
dispatch report that failure too. Release observations probe the oldest attempted releases first,
with never-probed releases ahead of retries, so a slow pass or a few failing releases cannot starve
the rest of the backlog and keep published deliveries occupying WIP.

Reviews, forwards, fixes, wakes, rework, merges and repairs keep their existing
admission rules. An active incident hold's named repair and a `board.order` step
with `wipExempt: true` pass the WIP gate and still count. A manual `agents start`
is counted and warns above the limit. Slots needed by `review-queued` deliveries
and fixes without a live worker are reserved before every new issue start,
including done-event `KindNext` starts and work waiting from earlier passes. Only a
start the loop will make reserves one: a delivery in a repository the desk works
on (inside `github.allowedOrgs`, not in `supervisor.ownership.never`) that no hold
of its repository stops, whose review is not already requested or assigned at its
current head, and whose step began within `supervisor.reserveFor` (default `1h`);
an older one is a stalled delivery, not a slot held from new work. The reserved
slots are at most half the room, rounded down, so a pile of deliveries never stops
every new start. Each worker stream carries its frozen
reservations through discovery and the fresh dispatch check. The log names the
reserving deliveries, and how many were not reserved and why, once per episode;
dry-run applies the same reservation. Capacity ceilings, review headroom,
verdict and JSON stay unchanged.

`delivery.stallAfter` defaults to `24h` (a positive duration). A stalled delivery keeps its record
but leaves the count when no live agent remains and its step has exceeded the bound. Review and fix
steps use `supervisor.reserveFor`; other steps use `delivery.stallAfter`. Deliveries confirmed
outside desk ownership do not count. Partial observations retain previous delivery records and reuse
them within the freshness window, including concurrent local starts.

Both watches share durable episode state: a step lasting that long, or a failed/unconfirmed release,
files once to the guide with its reference and wait; recovery is filed once. `WIP FULL` names the
oldest deliveries when the limit has stayed full without a step change for that duration. Restarts
and concurrent watches do not duplicate the reports.

`beekeeper delivery drop <ref> --reason "<why>"` ends only a delivery. It closes no
pull request and stops no worker. The caller and reason are logged. The drop is
bound to the actual observed pull-request heads (an unknown head is refused), so
observing the same head after a restart does not bring it back. A new head opens a
new delivery episode. No delivery is automatically dropped for being stalled.

The separate `supervisor.unmergedLimit` / `supervisor.unmergedWindow` policy still
holds new work per repository (defaults: 10 non-draft open pulls created in seven
days), escalating its own backlog episode. Desk delivery WIP does not replace those
per-repository semantics. Selecting a managed WIP value and an installed end-to-end
proof remain separate work.

### The flow simulated and measured

`beekeeper sim` holds the model of how an issue is meant to flow and compares it with the
event log. The model: a worker starts when a busy slot is free (`capacity.ceiling`, reviewers
up to `capacity.reviewHeadroom` more) and pushes a pull request; CI runs; once it is green the
loop starts a reviewer, which fixes its own findings on the branch or asks for changes; the
repository's lane merges one pull request at a time (`devctl pr merge` with the merge-result
check); the release rolls out. The loop drives every step but the worker's and the reviewer's
turns, so while it is down nothing starts, is reviewed or merges. Each stage has a median
service time with a lognormal spread, and the model has chances for red CI, a change request
and a started issue dropped without a pull request.

`beekeeper sim model` prints the model with this desk's slots. `beekeeper sim run` simulates it
over `--days` with one parameter changed at a time (`--workers`, `--lanes`, `--uptime`,
`--stage merging=40m`, `--update-branch`, `--backlog`, `--arrivals`, `--dropped`, …) and prints
the released issues a day, the cycle time, each stage's wait and service and how busy the slots
and lanes were; the same `--seed` gives the same run. `beekeeper sim observe --since 7d`
measures the real flow from the log: each delivery step's residence (`delivery.step`), the
loop's silences (no `supervisor.*` event for 15 minutes counts as down), the busy agents
against the ceiling (the `USAGE` lines, time-weighted), the worker and reviewer starts, the
loop's failures by step and reason, the merges' exit codes, what the workers parked on and the
releases rolled back. `beekeeper sim compare` (the default) puts both side by side: per stage
the model's median against the log's with the causes the log names, the loop's uptime, the
busy agents, the released issues a day; then the model fitted to the log (the log's stage
times, uptime, drop share, lanes and start rate) is run with one change at a time back towards
the model, and the changes are ranked by the released issues a day each would gain. That
ranking is the order to fix things in, and the fitted run beside the observed rate says
whether the model still holds. `--json` returns every figure.

`beekeeper sim forecast --horizon 24h` runs the desk as it is forward through the model. Its
snapshot is the live state: the active deliveries with the state of each one's agent on the
roster (busy, parked, idle), the parked agents and what they wait on, the timers, holds,
lanes, failed merges and review requests, and two verdicts that are the implementation's own
rather than the model's, run in process: `capacity` (the room for a start, what blocks one, a
standing stop, each provider's ceiling) and `board next` (the free items in the order the loop
would take them, and the held ones with their reasons; `--no-board` skips that GitHub read).
Before the run the snapshot is read for `PROBLEMS`: a `stop` line is what will not move at
all (the loop down, a stop, no room while free items wait, a review recorded as requested
while no reviewer runs, a parked worker on a timer that is gone), a `slow` line what moves
late (a failed merge in its backoff, items long in a step with a parked worker or none, idle
agents beside free items). Then every item in flight gets
the steps it enters and when it is released, or why it is stuck; the free board items the time
a slot takes them; and the totals by 6, 12 and 24 hours. `--from-log 7d` fits the stage times
to that window's log medians first; the `--stage`, `--workers` and other `run` flags apply. The
forecast is saved (`--save=false` only prints) and `beekeeper sim check` compares it with the
desk later: for each item the step the forecast had it in now against the step it is in (on
track, ahead, behind by how long with its agent and what it waits on, stuck as forecast,
gone), the starts and releases expected by now against the real ones, and which problems
still stand or resolved themselves. The intended use is the guide's: `sim forecast` at a
check, `sim check` at the next one, acting on the problems and on the items behind forecast,
and the gap between the two says how far the model is from the desk.

#### Checking the live projection

Capture `beekeeper delivery --json` and `gh pr list --repo <owner/repo> --state open
--limit 100 --json number,url,author` close together. Compare the distinct pull
references in active deliveries with the repository's open pulls after applying
the supervisor's existing ownership policy. Bot pulls need an explicit review
request; author, team and excluded-path policies still apply. A delivery can
contain several pulls, and several issues can share a pull, so compare references
rather than row counts. Check `completeAt` before relying on a snapshot; an
incomplete observation retains the previous timestamp and unread deliveries.

The full active list also includes work without a pull request and merged pulls
waiting for release. A closed pull can remain linked while its worker continues
implementation. These rows are not additional open pull requests. Do not delete
state or change the configured limit to make the two counts equal. Keep raw
snapshots local; public proof should contain only repository-scoped pull links,
version, observation time, comparison totals and unresolved differences. Take
`beekeeper alerts snapshot` after the proof and report its findings separately.

## The scheduled status reporter

With `reporter.every` and `reporter.brief` set, the standby watch (`watch --standby`,
`beekeeper-notify.service`) starts a one-off reporter session once per interval, on the interval's
multiples (on the hour for `1h`), whether or not the supervisor loop runs:

1. **The start.** One headless `claude -p` turn in `bypassPermissions` in a transient user unit
   (`beekeeper-report-<id>`, its output in `journalctl --user -u <unit>`), in `reporter.dir` with
   `reporter.model`, registered on the roster as `Status report HH:MM`, busy with the brief's first
   line. Its prompt names `reporter.person` and the interval it covers, then the brief. It is not
   imported into the desktop: the command-line turn has the claude.ai connectors, and the person's
   window is not switched every hour. `reporter.start` in the log, one watch line.
2. **The post.** The session runs `beekeeper report --since <start> --until <end>` for its interval,
   which renders the whole report from beekeeper's state in seconds, and posts its output with the
   Slack connector, adding at most three sentences of its own judgement: two tool calls. The time
   range is in `reporter.tz`, else the machine's zone, read for each run as `timedatectl` sets it (a
   running watch keeps no stale zone). The session starts with `--settings` adding a PreToolUse hook
   on `slack_send_message` (`beekeeper hook reportcheck`) that refuses a post failing `beekeeper
   reporter check`, with what to fix: the connector takes standard Markdown, so every pull request
   or issue is a link `[<repo>#<n>](https://github.com/<owner>/<repo>/pull/<n>)` whose label names
   the repository and number it links; no bare `#<n>` or `repo#<n>`, session names included (a
   beekeeper note is `note <n>`), no Slack `<url|label>` syntax, a first line naming the zone
   (`EEST`), no time in UTC. beekeeper sees the post in the session's transcript: a
   `slack_send_message` call that returned without an error.
3. **The end.** Once it posted, beekeeper takes the reporter off the roster and stops what still
   runs of its turn by PID (SIGTERM, then SIGKILL after 10 s): `reporter.posted`. A turn that
   ended without a post is ended the same way (`reporter.unposted`), and one that has not posted
   within `reporter.timeout` (20m) is stopped (`reporter.timeout`); each is one watch line.
4. **A pause.** `beekeeper reporter final <time>` starts one last report at that time, outside the
   slots, covering the time since the last report started and saying that the reports pause; with
   its start no scheduled report starts until `beekeeper reporter resume` (`reporter pause` pauses
   at once). A running reporter still posts and is ended. `reporter.final`, `reporter.pause` and
   `reporter.resume` in the log.
5. **No overlap.** While a reporter runs, the next slot starts none: `reporter.skip` once. The slot
   after its end starts the next one; the state's `report` keeps the current or last run, so two
   standby watches start one reporter per slot.

## Feedback on the reports from Slack

With `feedback.context` set, the standby watch reads the person's replies to the reports through
the Slack MCP server behind muster (`feedback.server`, default `slack`), as the person, with
central's muster binary and timeout:

1. **The thread.** A report's post (its `slack_send_message` result) names its channel and ts; the
   watch keeps it in the state's `reportThreads` for `feedback.window` (24h).
2. **The replies.** Every `feedback.every` (5m) it reads each thread (`x_<server>_read_thread`)
   beside the poll. A reply by the report's author that came after the last one delivered goes, as
   a message (`agents wake`), to the agent it names first, `<agent name>: <message>`, or else to the
   supervisor. A delivered reply gets an :eyes: reaction, `feedback.delivered` in the log and a
   `FEEDBACK to <agent>` line; one not delivered is said and read again the next time.
3. **Unreadable.** A thread that cannot be read is said once, `FEEDBACK UNREADABLE <context>:
   <reason>`, and `ENDED` once it reads again. The Slack connector reads what its scopes allow:
   a report posted as a direct message needs `im:history`, one in a public channel
   `channels:history`.
