# Commands

Every `beekeeper` command, grouped by what it is for, and the conventions they share.

## Looking at the desk

### `beekeeper capacity`

`beekeeper capacity [--builds] [--review] [--review-of-self-hosted] [--exclude <choice>]`

The busy, parked, waiting and idle agents against `capacity.ceiling`, the headroom that bounds a new
start (MemAvailable, disk swap's growth over the watch's readings weighed with memory pressure and
MemAvailable, the 1-minute load against the watch's limit, the kind labs against their cap, and for
a start that builds the free build slots) and one verdict: `room for N starts` or what blocks one,
each guard that refuses named; a swap growth without pressure is a warning in parentheses after it.

When `capacity.providers` and `capacity.choices` are configured, prints each provider's busy agents,
usage windows (with the 7-day pace share) and verdict, and the eligible subscription choice below
50% of pace first, then furthest behind its 7-day pace (with remaining 7-day budget per hour, lower
configured cost, and `capacity.choices` order breaking ties, from the model's bucket for
multi-bucket providers) and self-hosted choices as fallbacks. Each eligible provider has room, its
5-hour window below threshold and its 7-day use within its worker weekly cap (`sevenDayMax`) and,
for a worker start, not ahead of the even pace of its window (a negative `paceMargin` tightens it; a
positive one, a choice's `skipPace` and `capacity.reviewSkipPace` are read and ignored); a stale or
failed usage read counts as no room (`ceiling 0`, `room 0`) for that provider, except built-in
Codex: an ordinary read or validation failure keeps its last good windows (including active limits),
or its configured ceiling when no valid saved reading exists; the watch retains an unreadable
warning and reports it to the guide, while explicit quota/rate-limit stops stay blocking until a
successful read clears them.

`--exclude` skips matching choices (by model, `harness/model`, provider or harness). `--review`
marks a review start, which may use `capacity.reviewHeadroom` extra subscription sessions above the
worker ceiling and is not refused by the 7-day pace check (machine guards and provider window stops
still apply), and `--review-of-self-hosted` the review of a pull request a self-hosted model's
worker wrote, which takes no choice on a self-hosted provider.

`--builds` judges a start whose task builds, tests or lints, which waits for a free build slot
(`agents start --builds` waits its turn in the slot queue); without it the verdict is for a start
that does not build, and a busy slot is a note after the verdict, never a `no start`. Read-only, no
GitHub call. A provider of `kind: self-hosted` (a model served on the desk's own machines) reads no
subscription usage and has no pace: its own ceiling of busy sessions bounds it, optionally the one
its serving mode names (`serveModeCommand`, `serveModeCeilings`), and `capacity.ceiling` bounds none
of it, so a desk that lowers it to 0 to keep a subscription on its weekly pace still starts on its
own model.

### `beekeeper capacity pick`

`beekeeper capacity pick [--review-of-self-hosted]`

One line: the `<harness> <model>` the next start takes, or `none`. It is the decision `agents start`
without `--harness`, the supervisor loop's reviewers and its next starts take: the eligible
subscription below 50% of pace first, then furthest behind its 7-day pace (then remaining 7-day
budget per hour, lower configured cost, and `capacity.choices` order breaking ties), with
self-hosted choices as fallbacks. Each eligible provider has a free slot under its running-session
ceiling, whose 5-hour and 7-day windows are under their stop percent (`fiveHourMax`, `sevenDayMax`)
and, for a worker start, whose 7-day use is not ahead of the even pace of its window, with the
machine's headroom a hard gate and `capacity.ceiling` bounding the subscriptions together.

A review may use `capacity.reviewHeadroom` extra subscription sessions above the worker ceiling
(default 2) and is not refused by the 7-day pace check; machine guards and provider window stops
still apply; `--review-of-self-hosted` decides the reviewer of a pull request a self-hosted model's
worker wrote, which takes no self-hosted provider. `--json` gives the pick with every choice's
reason, each eligible subscription choice's `rate` (remaining percent per hour) and `behindPace` in
percentage points, each provider's windows and verdict, and the headroom. Read-only.

### `beekeeper status`

`beekeeper status [--bar]`

One line: who supervises, the leases held, the holds in force, the notes and timers that are due and
the busy agents against the ceiling (`busy 4/10`). `--bar` prints it as the fixed tab-separated row
a desktop bar reads (see [Desktop notifications](notifications.md#desktop-notifications)).

### `beekeeper sessions`

Every running session, Claude Code's, omp's, agy's and codex's (marked `omp`, `agy` or `codex`
`busy` or `idle`, see [omp agents](harnesses.md#omp-agents), [agy agents](harnesses.md#agy-agents)
and [codex agents](harnesses.md#codex-agents)): the issues and pull requests its latest turns acted
on (its `sessions serve` record, its `gh` and `devctl` commands and GitHub tool calls, what it
created; a ref it only quoted or read is a mention, kept in `--json` as `mentioned`), when it was
last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left),
its memory, how full its context is, its last hour (turns, tool calls and their errors, GitHub
calls, cost), role and leases, after `archived` or `test` for a session the guide's feed leaves out.

Overlaps name what more than one session acts on. The session records follow: the running sessions',
then the parked ones (a worker that runs no CLI while it stays on the roster parked or kept still
serves its item; its line names the worker and its park reason, `--json` has them in `parked`), then
the ended ones. `--all` adds the paused ones and the omp, agy and codex sessions of the last 24
hours no process runs: a message to them does not arrive. `--json` has every figure per session and
their totals (see [Session metrics](capacity.md#session-metrics)).

### `beekeeper tail`

`beekeeper tail <session>`

A session's last turns without tool calls: what it said and what it was told. The session is named
as `sessions serve` names one: its desktop title, its roster name, a session id or a PID.

### `beekeeper snapshot`

One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots and their
CPU budget and use (`memcap.slice`'s `cpu.max` and `cpu.stat` over a 500 ms sample), kind clusters,
the sessions' last hour and the three that spent the most in it, the commands sessions sit on, every
kernel OOM kill since your last snapshot with whose limit it hit (a memcap scope no `run.start`
names says `cap unknown`, never the default cap; a test run's scope is a test kill), leases, holds,
the GitHub budget, the installations' alerts and their running upgrades (`upgrades: prod/mc 35.0.1 → 35.1.1
for 9m (control plane 3/4, node pools 15/15) · test none`).

A caller inside a Claude session that has taken one before gets only what changed since, or one `no
change` line; `--full` prints the whole screen and then what changed.

### `beekeeper ui`

The person's screen, for a terminal rather than a session's context: six tabs over the live state —
the machine (load, RAM, swap, pressure, the desktop scope, disks, build slots, kind labs, the waits
and the OOM kills), every session, Claude Code, omp, agy and codex (its state — busy, idle, waiting
on its person, omp's, agy's and codex's own —, idle age, memory, context fill, last hour, what it
runs, what it waits on its person for — a waiting session stands out; `enter` opens its pane, which
follows the session live: the history, then the current turn with its tool calls as they land,
re-read every refresh; `k`/`j` scroll back and forward, `G` follows again; `m` writes the session a
message, stamped as the person's through the screen: by name to a Claude Code CLI, into the inbox of
an omp, agy or codex agent beekeeper started; `t` takes a Claude Code session over: its permission
requests come to the pane with its turns beside them, `a` allows and `d` denies, and one left
unanswered for 290 s, or when `t` hands it back or the screen quits, goes to the session's own
window, which the pane says), the shares (held leases and their staleness, free resources, grant
queues, holds, the merge lanes), the supervision (the supervisor and guide roles with their relay
state, the agents, the open notes with their defaults, the timers, the session records), the
installations' alerts by severity with the running upgrades and the GitHub budget and its pollers,
and the event log.

Refreshes every two seconds (the budget and the upgrades every minute, in the background); besides
those messages and take-overs it reads only: it takes no lease and lifts no hold — those stay with
the commands, whose exit codes the sessions gate on.

### `beekeeper watch`

`beekeeper watch [--tick <duration>]`

Routine events are batched every `watch.batchInterval` (5m, negative disables; `--tick` overrides),
as one `BATCH` stdout line containing a JSON array of timestamped items. Empty ticks are silent;
`--once` prints immediately. Each original event is durably recorded before buffering, including
across shutdown/restart. Action events remain immediate. Silent until something needs a look, then
one line: the source of a `Monitor`. A threshold breach, an unreadable source and a stalled lane are
one line when they start and one `ENDED` line when they end, never repeated while they last; OOM
kills, sessions that start, end or restart, stale leases and every NEW or RESOLVED alert of the
installations are always reported.

A note or timer that falls due, the end of a session with a record and a relay taken or expired are
one line each, once: the state keeps that they were reported, so a second or restarted watch stays
silent about them. The supervisor loop down for two minutes is `SUPERVISOR LOOP DOWN`, once, and
back `SUPERVISOR LOOP BACK` (see [The supervisor loop
down](supervisor-loop.md#the-supervisor-loop-down)); no session holds the supervisor's role. A
session over a threshold in `metrics.runaway` is one `RUNAWAY` line per figure, once per watch.

A fork rate `watch.forkRateMax` over the machine's usual one for two samples is one `PROCESS STORM`
line, and more than `watch.stackMax` long-running copies of one command line one `STACKED` line
each, with the commands, the parent and the owning session, and more than `watch.toolProcsMax`
(1000) processes of the CLIs in `watch.tools` (kubectl, helm, tsh, gh, flux, devctl) machine-wide
one `LOAD` line with their commands and the sessions that run them, and an `ENDED` line each.

Free space on `/` is `LOW DISK` under `watch.diskMinMiB`, `DISK NEARLY FULL` under
`watch.diskCriticalMiB`, and `DISK FILLING` while it would run out within `watch.diskFillWithin` at
its current rate, with the commands and sessions that wrote. `TMPFS` (the tmpfs `/tmp` over
`watch.tmpMaxMiB`) says when the automatic free of tmpfs last ran and what it freed; a run that
freed something is one `FREED` line ([`beekeeper
doctor`](agents.md#agents-started-without-a-click)).

A stalled lane (see `lanes`) is one `LANE STALLED` line, repeated at most every 10 minutes while it
lasts. A `devctl pr merge` or one-repository `release promote` running with no beekeeper gate among
its ancestors is one `UNGATED MERGE` line with the command, its session and its parent, and an
`ENDED` line when it ends (see [The merge gate](merge-gate.md#the-merge-gate)). A settling merge
leaves its lane once its release rolled and the lane's HelmReleases are Ready, however late; one not
settled past `merge.settleTimeout` is one `LANE STUCK` line with what the lane waits for (the
HelmRelease off the release, the one not Ready, an installation that cannot be read), repeated at
most every 10 minutes, and one `ENDED` line when it settles or the lane is cleared; `lanes` marks it
`stuck since`.

A cluster upgrade on an installation (see [Cluster upgrades](clusters.md#cluster-upgrades)) is
`UPGRADE <installation>/<cluster> <from> → <to>` when it begins and `UPGRADE ENDED … (since HH:MM)`
when it ends, once each. The quiet rules keep what is noise for the supervisor out of its lines:
other teams' alerts matching `alerts.quiet` (by default their e2e test clusters, `t-*`, and, once
`alerts.team` is set, every other team's and team-less `notify` alert), an alert back after a
reading that missed it with its old start (an Alertmanager restart, a reconnect), and the start, end
and restart of the short-lived sessions in `watch.quietSessions` (by default beekeeper's tests,
`test: *`).

A rule never holds back an alert of `alerts.team`, one on an installation in play (leased, claimed
or merged into within the last 30 minutes, or with a merge settling), or a page unless the rule
names a cluster; everything else is said as before. What they hold back is logged (`beekeeper log --verb
watch.quiet`) and counted in `snapshot`. With `sink.repo` set, a running watch (not `--once`)
dispatches `sink.workflow` (`workflow_dispatch` on `sink.ref`, else the default branch) for each
event of `sink.verbs` within one interval, with the inputs `verb`, `subject` (the issue or pull
request, `owner/repo#n`) and `time` (RFC 3339) and nothing else: `plan.grilling` and `plan.ready` (a
`note add` for `guide.person` admitted on a plan pull request at its grill stage, or open with its
stage check green), `session.serve` and `board.claim` (a session's record of the issue it serves),
`merged` (a pull request the merge gate merged); all of them by default.

Every watch on the machine offers the events, `sink.json` in the state directory claims each one
once; an event is dispatched from the sink's first poll on and within 10 minutes of it. A dispatch
is logged (`sink.dispatched`); a failed one is one `SINK FAILED` line and `sink.failed`, and is
tried again by the next poll. `--notify` also sends the events that need a person to the desktop,
`--standby` leaves a running supervisor's events to its watch (see [Desktop
notifications](notifications.md#desktop-notifications)).

### `beekeeper alerts watch|snapshot|import|capture|replay|discover|own`

The installations' alerts, read from each Alertmanager through a bounded `kubectl port-forward`
(Mimir's with the `giantswarm` tenant, else the plain one), in parallel: `watch` prints one line per
NEW or RESOLVED alert since the baseline (pages and your team in capitals, a burst of one alertname
as one line, one line when an installation stops or starts answering, the lease holder and the
sessions working against it in brackets, and on a NEW line the merges into the installation's lanes
and the lease claims on it of the last 30 minutes with their sessions, worded as timing (`during
merging …`, `during merged … at …`), not as cause); `snapshot` the current set, grouped; `import`
takes over another watcher's per-installation baseline.

An alert below its installation's severity floor never appears in either, and an alert that keeps
firing and resolving, or fires again within the damper's window after it resolved, is one FLAPPING
line with its first start, then quiet until it has been stable for the window; a reading that misses
alerts (stretched, cut short, partial) resolves none: one line says how many it missed, they keep
their first start, and they resolve when the next reading misses them too; neither a floor nor a
damper change prints a burst of lines.

`capture <dir>` records each installation's answer, `replay <dir>...` prints what the watch would
for recorded answers, from the current baseline without writing it: a floor or damper setting tried
before the watch gets it. `own <installation/alertname#fingerprint | fingerprint |
installation/alertname | alertname>` records the caller (`--by` another running session, `--as` a
person) as the owner of the firing alerts it names (`alert.owned`): one instance by its fingerprint,
every instance by a bare name, each named in the output as
`<installation>/<alertname>#<fingerprint>`; `disown` with the same forms releases them
(`alert.disowned`), and the alert counts as unowned from then; ownership ends with the owning
session, a disown or the alert's resolution.

`beekeeper watch` says `PAGE UNOWNED <installation> <alertname> <where> for <duration>` (one line
per alertname, `(+n)` for its other unowned alerts) for a firing alert at `alerts.pageSeverity`
(default `page`), of `alerts.team` when it is set, that has had no owner for `alerts.ownerGrace`
(15m), and again every `alerts.ownerGrace` while it stays unowned; the owner's session ending or a
disown starts the count again, and a non-paging alert never gets the line.

One process owns the baseline at a time, so two watches never split the lines. `beekeeper watch`
keeps one port-forward per installation from tick to tick, a dropped one replaced at the next
attempt, and reads the kubeconfig's contexts again only once it changed, so a tick is HTTP requests,
no kubectl or `tsh` run; a one-shot reading's forwards end with it, and every forward ends on SIGINT
or SIGTERM and when beekeeper is killed. The kept forwards are the watch's own, never a session's
wait.

With `alerts.kubeconfig` set, every kubectl call of the alert and upgrade reads (contexts,
port-forwards) gets those files as `KUBECONFIG`, behind the machine kubeconfig unless the list names
it: an installation whose context lives only in a kubeconfig of the watch's own (`KUBECONFIG=<file>
tsh kube login <cluster>`) reads with no `KUBECONFIG` in the environment, and the machine kubeconfig
is not written. PagerDuty is the second source: with `alerts.pagerduty.context` set, the watch reads
the open (triggered and acknowledged) incidents of `alerts.pagerduty.services` every
`alerts.pagerduty.every` (1m) through muster's read-only PagerDuty tools as the person
(`x_pd_list_incidents`, and `x_pd_list_alerts_from_incident` once per new incident for its alert's
`installation` label), so a page for the team is a watch line within a minute whether or not its
installation's Alertmanager is read: `PAGERDUTY NEW <installation> <TEAM> #<number> <title> since <start>`,
`PAGERDUTY ACKNOWLEDGED …` and `PAGERDUTY RESOLVED …` once it is no longer open; a failed listing is
tried once more over a fresh connection (every muster call connects afresh), a timed-out one names
its stage (connect, auth, tool call) and deadline, and a PagerDuty that read and then does not
answer for 3 minutes (muster answers `auth_required` for a reading or two while it reconnects the
server) is one unreachable line, said again every 15 minutes, with its open incidents kept.

`alerts watch` reads it once too, `snapshot` lists the open incidents, its baseline is
`pagerduty.json` with an owner of its own. beekeeper holds no PagerDuty token: the person's muster
sign-in is the only credential. With `alerts.discover.every` set, the installations read are also
every cluster Teleport lists (`tsh kube ls`), but `alerts.discover.exclude` (workload clusters and
the proxy), that carries a HelmRelease labelled `alerts.discover.label=alerts.team`: the watch
discovers them every `alerts.discover.every`, logging one without a kube context into the watch's
own kubeconfig (the first file of `alerts.kubeconfig` that is not the machine kubeconfig,
`KUBECONFIG=<file> tsh kube login <cluster>`), keeps them in `alerts-discovery.json` and says
`ALERTS DISCOVERED <installation> carries <team>'s <releases>` and `ALERTS UNDISCOVERED …` when one
joins or leaves; one that does not answer keeps what the last discovery found.

`alerts discover` discovers once and lists them, so an installation the team's releases roll onto is
read without a hand-kept list or a context in the person's kubeconfig.

### `beekeeper spend`

What the API requests of a window cost, from every transcript under `claude.projectsDir`, every omp
session file under `omp.sessionsDir`, every agy transcript under `agy.brainDir` and every codex
rollout file under `codex.sessionsDir`, read whole: the sessions' and their subagents', running,
paused or archived. A request counts once by its id, however many transcripts carry it (a fork
copies its history). An omp model is listed as `provider/model` and priced by that name in
`metrics.models`, or else by its model.

`--since` (default `00:00`) and `--until` take a time (`15:04`), RFC 3339 or, for `--since`, a
duration; `--tz` the zone. One line per model with its tokens; a model without a price in
`metrics.models` is named unpriced and left out of the total. `sessions` prices only the last 512
KiB of each live session's transcript: `spend` is the figure for a day.

### `beekeeper claude-login`

Local Claude CLI login state and access-token expiry, without credential values. The watch reports
it and warns one hour before expiry, leaving one login note for the guide's person to complete
`claude auth login` through their browser session under the browser lease. The note's probe closes
it after renewal. Expired tokens without a refresh token refuse Claude launches with a
login-specific explanation. Refreshable tokens remain eligible for the CLI's own refresh; the watch
reports that refresh success is unverified. Alternate authentication and unavailable file metadata
have an explicitly unknown expiry. `CLAUDE_CONFIG_DIR` selects the account for both the reading and
the note's probe. Exit 3 unless a saved login is outside the warning window.

### `beekeeper doctor repository`

`beekeeper doctor repository <owner>/<repo> [...]`

Read-only per-owner GraphQL probe: prints each resolved repository and its owner type (`User` or
`Organization`), using the same authentication routing as note and park checks. Supports `--json`.

### `beekeeper budget`

The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), the
GraphQL limit from a real `rateLimit` query (one point, read again after `merge.budgetFresh`), a row
each for the code-search and search limits from `/rate_limit` (free) with remaining, limit and
reset, or `SPENT` until the reset, and every `gh` and `devctl` process with its session: the
callers. A GraphQL refusal shows even while its counter looks healthy, named as the hourly limit
spent or a secondary limit, with the time it ends when GitHub names one; the watch says it once with
the callers, and its end.

`--gate` exits 3 under the floor, while GraphQL is refused or while the merges queued for the reset
go first (`merge.resetFirst`). `--gate --bucket code_search` (or `search`, repeatable or
comma-separated) also exits 3 while that search bucket is spent, naming its reset: `beekeeper budget --gate --bucket
code_search && gh search code …`. Search limits that cannot be read fail such a gate (exit 1), as an
unread core budget does; a gate naming no bucket ignores the search limits. `/rate_limit` shows no
secondary limit, so a search the gate passes can still be refused by one.

### `beekeeper ps`

`beekeeper ps [name|pid...]`

The process table with every command line masked: PID, parent, age, CPU time, resident memory, and
the command line as the watch's `STACKED` line prints it (the program, its subcommands and flag
names; flag values, `key=value` words, URLs and what follows a short option's letter left out). A
masked line ends in `(masked)`, so a left-out value is never read as an empty one. A number narrows
the list to a PID, a word to the processes whose masked line or name holds it. `--json` has the
rows. The hook's refusal of a whole command-line read names it ([Secret
reads](secrets.md#secret-reads)).

### `beekeeper sessions serve|unserve`

A record for any session, registered agent or not: `sessions serve <session> <item> [--waits
"<what>"]`, the issue or epic (`owner/repo#n`), repository (`owner/repo`), or quoted free text it
serves and what it waits on (the session is an id, a PID, a session's own name or a registered
agent's roster name or task title, the roster name with a suffix such as ` (opus, builds)`; a name
that matches nothing is refused with the closest roster names; the record stores the roster name of
a registered agent, the session's own name otherwise); `sessions unserve <session|owner/repo#n>`
removes it, by the issue for the one record serving it; `sessions serve --clear` removes the calling
session's own record, and `agents idle --done` clears it too.

A worker (a registered agent holding no supervisor or guide role) records only its own session: its
serve naming another session is refused (exit 3), nothing written. `sessions` and `handover` show
it; `watch` prints one line when the session ends, naming the issue to re-query.

### `beekeeper handover`

`beekeeper handover [--section <name>]`

Everything on the desk, as Markdown, from the live state: whether the supervisor loop runs, leases
and grants, holds and their exceptions, agents, session records, notes with their defaults, the
decisions answered within the last day, timers, the merge lanes with their queues and settling
merges, what the alert watch reads (the installations and why, the ignored alert names, the
baseline), and the board holds (`--section board-holds`): read from GitHub with the read `board
next` makes, every held board item with what holds it, its open blockers (`board block`), its Status
set after they closed, or the open decision that holds it (`note add --holds`) with its person; a
failed read is shown as not read, never filled from the state.

It shows what a reader acts on first: the notes the guide serves (for `guide.person` and for the
guide) are one count line, records of sessions ended over an hour ago one line (a parked or kept
agent's record stays listed, with its park), and the answered decisions those of the last day with a
count of the rest of 72 hours. The supervisor has no hand-over prompt: it is the supervisor loop, no
session. `--section <name>` prints one section with everything it holds (`notes`, `records`,
`answers`, …).

### `beekeeper log`

`beekeeper log [--verb PREFIX] [--since DURATION] [-n N]`

Every claim, grant, hold, registration, note, timer, session record and build run, as they happened;
`--verb run.` shows only the runs. The latest 50 by default; `--since 30d` (a duration, a time or
RFC 3339) every event of that window. The log keeps every event since the machine's first, never
trimmed or rotated.

## Shared resources and holds

### `beekeeper lease claim|release|status|grant|revoke`

One holder per resource: the environments in the configuration, the shared installations of the
central instance when one is configured ([The central
instance](remote-hosts.md#the-central-instance)), the host's model server (`model-server`, whose
claim carries `--gib <n>`: the GiB its models may hold, see [The model
server](capacity.md#the-model-server)) and the browser. A free lease with no pending grants is
claimed directly; when grants are queued, only the head grantee claims after the current holder
releases.

A destructive purpose (node restart or down) is recorded on the lease and shown to the next holder
after release. `lease grant <resource> <session>` queues a session for a held resource, behind the
earlier grants or, with `--first` (the supervisor loop's or a person's, never a session's), ahead of
them, so its claim of the held resource queues and `lease claim --park` waits for the release. While
a cluster upgrade runs on the installation of the resource's name, nobody claims it except by the
supervisor loop's or a person's `lease grant <installation> <session> --upgrade-unblock "<why>"`,
for the work that unblocks the upgrade (see [Cluster upgrades](clusters.md#cluster-upgrades)).

A grantee that no longer needs a queued grant withdraws it with `lease release <resource>`: its
grant leaves the queue, its park on the lease ends and the next grant moves up (`lease.withdraw` in
the log). `lease revoke <resource> <session>` withdraws another session's grant; a parked grantee
whose grant it revokes is resumed with the words to ask again, also while the lease is held. A lab's
claim keeps its `agentlab.yaml` (found through `agentlab list`) in the lease, its hash and
`platform.valuesFiles` in the record; the release compares the lab's configuration with it, comments
aside, and a change is refused naming the keys that differ until `--restore` puts the claim-time
file back (the released one kept as `agentlab.yaml.released`) or `--keep-config "<why>"` declares
it.

Restoration preserves a symlinked `agentlab.yaml` and atomically updates its target; a broken
symlink is refused. A declared change, and one a `--force` release finds, is shown to the lab's next
claim; `lease status <lab>` says whether the configuration is at its claim-time state, or, free, the
change waiting for the next claim.

### `beekeeper lease kubeconfig`

`beekeeper lease kubeconfig <lab>`

The kubeconfig of a kind lab whose lease you hold, written into the lease
(`<leaseDir>/<lab>/kubeconfig`, mode 0600) and freed with it; prints the bare path, never the
content. Use `kubectl --kubeconfig "$(beekeeper lease kubeconfig <lab>)" get nodes`, or pass
`--export` to print `export KUBECONFIG=<path>` for shell use. A lab claim writes it already when the
cluster runs. Before each command of the lease's holder the hook checks the copy against the lab's
API server and rewrites it from kind once it no longer reaches it (a lab recreated under the lease
has a new CA, often a new port); it stays a copy in the lease, not kind's own kubeconfig, so a
released lease no longer reaches the cluster.

In the agent sandbox the broker writes it and the session points it at its sandbox proxy (see [The
agent sandbox](sandbox.md#the-agent-sandbox)).

### `beekeeper lease up|down`

`beekeeper lease up|down <lab>`

Create (`agentlab up`, asking nothing) or tear down (`agentlab down`) the kind cluster of a lab
whose lease you hold, the cluster `labs` maps the lease to: in the lab's directory its
`agentlab.yaml` must name that cluster, elsewhere agentlab runs the lab it knows by that name.
Capped as `beekeeper run` caps it; `up` prints the lease's kubeconfig line, `down` drops it. In the
agent sandbox the broker runs it on the host (see [The agent
sandbox](sandbox.md#the-agent-sandbox)).

### `beekeeper hold set|lift|check`

Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as
a model load stops the lane whose components it exercises, not the others), every merge (`merges`)
or every GitHub call (`github`) until lifted, a time passes or its probe passes (`--lift-when
"<shell command>"`: the watch lifts the hold once the command exits 0 and logs it as `hold.lift`; a
repository's hold whose probe names the repository and `release` or `tag`, such as `gh release view
v1.2.3 --repo owner/repo`, lets that repository's own promote through, the step that cuts the
release it waits on, and lists as `owner/repo except its promote`).

A merge hold lets one repository or pull request through with `--except owner/repo[#n]`: `hold set --lane
serving --except giantswarm/model-manager#172` stops the lane but for the merge it waits for. A
merge hold lets one session through with `--except-session "<name>"` (its title): `hold set --lane <line> --except-session
"BK 0.156 promoter"` keeps every other merge off the line while its promoter promotes; every other
session's refusal (exit 77) names the session the hold admits.

A lane hold that excepts one pull request (`owner/repo#n`) is also its fix window: that merge runs
without the lane's HelmReleases Ready or the previous release rolled, for the fix of a rollout only
the fix can repair, logged as `merge.window`. The merge gate enforces them.

### `beekeeper stop`

`beekeeper stop --reason "<words>"`, `beekeeper resume`

The person's stop in one command (see [Stopping everything](agents.md#stopping-everything)): records
the stop, holds `merges` and `github`, stops every unit of beekeeper's starts, wakes, reopens and
status reports, the standby service and the units `stop.units` names, ends every session beekeeper
started (never the person's own or the guide's), and refuses every start, wake, revive, reopen and
import while it stands. `resume` lifts the holds it set, starts the units it stopped again, starts
the guide when its session runs no CLI and names the sessions that wait for a wake.

### `beekeeper lanes`

`beekeeper lanes [queue|settle|urgent|drop|clear]`

Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones
in turn order, so who is next is never prose. `urgent <owner/repo> <n> --reason <why>` lets one
privacy or security fix's merge run under the GitHub budget floor, once per reset window (see the
merge gate). `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an
agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h;
seeds keep their order, an arrived unseeded merge passes one whose merge has not arrived); a run
with nothing merged stays in its place as `retrying` for its session's retry; `settle <owner/repo> <n>
[--for <session>]` registers a merge run outside the gate (in flight when the gate went live, run
without the hook): it heads its lane until it merges, then settles the lane like a gated merge;
`drop` takes a waiting merge out (a place whose pull request merged or closed leaves by itself at
the next `watch` poll), and ends the devctl of a running merge whose pull request merged or closed
(`watch` does so by itself `merge.hungAfter`, 45m, after the merge); `clear` is the repair for a
lane whose settling release will not roll, after a look at the installation.

A lane where no merge runs and whose first arrived merge has waited longer than `merge.stallAfter`
(5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left
it) is `stalled`, with the waiting merge and those places named. A lane with a queue says when it
drains: its places times its pace, the median start-to-merge time (on to the settle where it rolls)
of its last ten merges in the event log.

### `beekeeper run`

`beekeeper run [--local SIZE] [--wait DURATION] [--class build|benchmark|lab] -- <command>`

Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the
`memcap` wrapper) inside a memory-capped systemd scope, at nice 10, under `memcap.slice`'s CPU
budget (`memcap.cpuQuota`, half the cores; `memcap.cpuWeight`, 50 against the desktop's 100), which
every run sets again at the slot it takes and the slots share by equal weight. The command's own
parallelism follows its slot's share: `GOMAXPROCS` and `CARGO_BUILD_JOBS` are the quota's whole
cores over `memcap.slots`, at least one, unless the caller sets them, so a linter or a compile with
a thread per core cannot lift the load that gates the starts.

A run of a session that holds a slot already joins it at once (its scope in the slot's slice,
sharing the slot's cap) rather than taking a second one. When every slot is held it waits once, in
the build slot queue it shares with `agents start --builds` (a ticket per waiter under
`<slotDir>/queue/`, the slots taken first in first out; the `memcap` wrapper takes none), up to
`--wait`, else `MEMCAP_WAIT`, else 8 minutes, 60 in a background Bash call (`MEMCAP_BACKGROUND=1`,
which the PreToolUse hook exports), then exits 75 naming each slot's holder (its session's name, its
process, since when and for how long, its command) and the wait it applied and where that came from
(`gave up after 60m (the background default)`); `--class benchmark` or `--class lab` (a run of
`agentlab` without a build is `lab` by default) takes no slot: a benchmark or a lab proof is long by
design and waits on its model or its lab, so it waits only for the memory its cap needs and runs in
its own scope under `memcap.slice` with that cap, builds beside it, memory the bound (a model's
memory still takes `lease claim model-server --gib`); a run that declares no class and cannot build
(a single `true`, `false`, `echo` or `printf`, or a command that only prints its help or version,
`go version`, `make help`, `kubectl --help`) runs at once, uncapped and outside the slot queue;
`watch` names a run holding a slot past `memcap.holdBound` (30m) in one `SLOT HELD LONG` line with
its holder and since when; for the length of the command it holds the Go build cache's use lock
shared, so no trim of the cache removes an entry a compile reads, and a run that begins during a
trim waits for its end within the same wait; when the cap fires the kernel kills the biggest process
in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap
killed:`.

A command that exits and leaves processes in its scope (a test's envtest kube-apiserver) would keep
the slot's slice, and so the slot, busy: `run` stops the scope before it releases the slot, names
what it stopped in one `beekeeper run:` line and in its `run.end` event; `watch` names a slot scope
whose run is gone (every process in it adopted by the service manager) in one `ORPHANED SLOT SCOPE`
line at the sample it first sees it, and stops it a minute later (`STOPPED orphaned slot scope …
slot 1 is free`).

Each run leaves `run.start` and `run.end` in `beekeeper log` with its scope, session and command
(the slot's holder record names it too: a shell's `-c` script on one line, and one too long for the
line leads with the build that holds the slot, `… go test …`, never with a `cd`, an assignment or a
`git checkout` before it), so `snapshot` and `watch` name a cap kill's session and command after the
run has ended. `MEMCAP_TEST=1` marks a test's run: its scope is `memcap-test-…`, and a kill in it is
reported as a test kill (one quiet `test kill:` line in `watch`), never as a build's.

When the caller sets no `SSH_KNOWN_HOSTS` and the machine's known_hosts (`~/.ssh/known_hosts`,
`/etc/ssh/ssh_known_hosts`) holds a line Go's `knownhosts` cannot parse, the command gets
`SSH_KNOWN_HOSTS` naming `<stateDir>/ssh/known_hosts`, a copy without those lines, so go-git's SSH
transport reads it; the machine's files stay untouched. Opt-in `run.remote` offloads eligible Go and
Cargo commands to reusable Kubernetes builder Jobs with source snapshots, reconnectable output and
local fallback; `--local` keeps writes local.

See [remote builds](remote-builds.md) for configuration and image requirements. A failed run keeps
the tail of its output — the `--- FAIL` lines a truncated screen lost — masked like every output
leaving the run, in `<stateDir>/run-tails/<scope>.log` (a remote run's: `remote-<id>.log`), and
prints it again at the end, so a failing test can be named without rerunning the suite; `run.tail`
sizes it (default `256K`, capped at 1M, `off` keeps nothing) and `beekeeper run tail [name]` lists
the kept tails and prints one.

## The board, GitHub and the browser

### `beekeeper board add`

`beekeeper board add <owner/repo#n> [--status <status>] [--kind <kind>]`

Add an issue idempotently, set the configured Team and Status (default Backlog), and optionally set
Kind. Prints the resulting fields; omitted Kind preserves its value.

### `beekeeper board show`

`beekeeper board show <owner/repo#n>`

Print an issue's board fields through its own projectItems, without reading the whole board.
Supports `--json`. Project and field IDs are cached in local state for one hour.

### `beekeeper board next`

`beekeeper board next [--claim] [--skip <refs>] [--limit <n>] [--fields <names>]|move|rank
(order)|tidy [--dry-run]|escalate [<item>...]`

The project board's next item of work: `next` walks `board.order` (steps by Status, Kind and labels,
a Kind taking an item without one by a label naming it, `bug` or `kind/bug` for Bug; `--json` names
each item's step (`step`) and what of it the step's matchers took (`match`: `status Backlog, kind
Bug by label bug`, `search`, an epic's for its sub-issues); an epic's open sub-issues; blocked items
whose blockers all closed; items created within a window; a GitHub search; a sub-issue that is a
board item held to the order by its own Status, whatever its Team) over one read of the board, under
the epic gate (`board.epics`, whatever the order says: an epic in Up Next is in planning, skipped
with its plan, `epic in planning (Up Next), plan <url> (draft): its sub-issues wait for the go`, or
as its planning session's when no plan names it, never offered through its sub-issues, and an issue
whose nearest epic among its parents on the board is in planning is skipped as `sub-issue of epic <owner/repo#n>,
in planning (Up Next)`; an epic not yet scheduled for planning, in a status ahead of Up Next (Inbox,
Backlog) or without a Status, has no plan and no go: it is skipped as `epic Backlog, not scheduled
for planning: …`, never offered through its sub-issues, and an issue whose nearest epic is one is
skipped as `sub-issue of epic <owner/repo#n>, not scheduled for planning (Backlog)`; moving the epic
to In Progress is the go, from which its open sub-issues are offered in their order; its plan is the
open or merged pull request of `plans.repositories` whose description names the epic, and its state
never gates), and prints each item above the pick that was skipped on a line of its own before it,
`skipped <owner/repo#n>: <reason>` (the same list as `--json`'s `skipped`), then the first free item
with why it is picked, without `--claim` the free items behind it in their order (the preview of the
board's work) and the skipped items' count by kind (`served`, `assigned`, `blocked`, `stale`,
`lease`, `claimed`, `planning`, `order`, `asked`; `--json` lists every item behind the pick as
`after_pick`, with its skip reason or free, and the counts as `skipped_by`) and each epic in
planning or past its go with its plan's URL, state and head (`--json` `epics`, also the hand-over's
Epics section, `handover --prompt` and `guide handover --prompt` included); for an epic past its go,
the go read back from the latest go comment on its private thread (one more request per 20 such
epics of one owner, no board-wide read): the head implemented (`implemented`, with `implementedPlan`
when another plan PR names it now, `goAt`, `goComment`) and how the plan moved on since, `plan moved
since the go: <n> commits` (`moved`), `superseded by <newer plan URL>` or `plan closed` (`since`),
and `no go recorded` (`noGo`) when there is none: served by a running session (a `sessions serve`
record, a busy agent's task; one naming a pull request serves the issues it closes too, `… through
owner/repo#<pr>`) or by an agent on the roster whose CLI is gone while it is busy with its task or
kept (a task names an issue by URL, as `owner/repo#n`, as `repo#n` of `board.owner`, as a repository
before a list, `beekeeper: #524, #525`, or as a bare `#n` after the last repository named,
`beekeeper#173, #176`; a bare `#n` before any repository names nothing, and `note #n`, `timer #n`,
`memo #n` and `decision #n` are beekeeper's own items), assigned outside `board.people`, blocked by
an open recorded blocker, without activity for `board.staleAfter`, or needing a lease another
session holds (a `lease/<resource>` label on the issue, `lease/agentlab-1`: the item is passed over
until the lease is free and the items behind it are offered meanwhile; a picked item's free lease is
printed with it); a sub-issue offered through an epic passes the same checks, and a serve record or
task naming the epic covers it (`…, on epic owner/repo#n`).

No note holds an item: a hold is a blocked-by (`board block`), a priority a board move or reorder
(`board move`, `board rank` / `board order`). `--limit <n>` lists at most n items of each list (the
skipped lines above the pick, the free items behind it, `--json`'s `skipped` and `after_pick`) and
says how many it left out (`--json` `skipped_more`, `after_pick_more`); `--fields ref,title,url`
(implies `--json`) keeps only those fields of each item, an unknown name refused with the names it
knows; without either the output is the whole walk.

`--skip <owner/repo#n|URL>` (repeated or comma-separated) passes over the items it names (`skipped
by --skip`, kind `asked`, unless something else holds one already), so the next free item behind
them is the pick; without `--claim` the pick is followed by the call that offers the candidate after
it, `next candidate: board next --skip <the skips and the pick>` (`--json` `next_skip`), and a scan
walks every free candidate by repeating that call alone until exit 3 says none is left.

`--claim` records the pick as the calling session's serve under the state lock, so two concurrent
claims never get the same item; it changes nothing on the board, the caller moves the item with
`board move` once it judged it, and ends with the session, `agents idle` or `sessions unserve`. A
second `--claim` while the session serves an open issue or PR is refused (exit 3) with its record,
unchanged; `--claim --replace` takes the next item and replaces the record.

Another desk's claim of an item on GitHub skips it too (`claimed by desk <name> (session <id>) since <time>`),
and `--claim` publishes the desk's own at once ([Claims across
desks](guide.md#claims-across-desks)). `move <issue> <status>` sets an item's Status by the board's
canonical name or an unambiguous part ("up next"), and refuses anything else with the board's
values; an issue without an item on the board is added first (`addProjectV2ItemById`, the issue's
own mutation, never a read of the board's items) and given `board.team` as its Team; issues from any
accessible repository are added to the configured board regardless of the repository's owner,
subject to GitHub permissions. An archived item, which GitHub refuses to update, is restored to the
board (`unarchiveProjectV2Item`) before the move when its issue is open, and left as it is when the
issue is closed (`left <issue> archived in <Status>: the issue is closed`, exit 0, nothing written).
`move --intake` (the supervisor loop's intake) puts the issue in the status only when it has none on
the board and is not worked on another board; it puts a closed issue nowhere and leaves an archived
item archived, each a `left …` line that the loop's intake reads as nothing to rank.

A move to the blocked Status says what holds the item: `--by <issue>` records the open issue that
does as its blocker first (as `board block`), and without it the move is refused unless the item has
an open recorded blocker already; a wait on a pull request, a review, a hold or the GitHub gate is a
park, the item left in its Status. Moving an epic to In Progress is the go: when a plan names it,
the go is recorded as an agent-written comment naming the plan and its head commit on the epic's
private thread (the epic in a private repository, else the plan pull request in a private one),
logged as `board.go`; an epic whose repository and plan are both public has no private thread and
its move is refused before anything is written.

Every call of a move acts on the issue's repository (`GH_REPO`), so a `gh` link that picks its login
by the owner (devctl's agent link) reaches a personal account's repository with the person's own
login; only an issue GitHub does not resolve is refused, with GitHub's reason. Priority is
determined by issue Status and order on the board only: the steps' Status orders the work (In
Progress, then Up Next, then Backlog, as `board.order` lists them), and a step's `workScope` is read
and ignored.

Within a step an item's position on the board orders it: `next` walks each step's items in the
board's position order (the default order of a project's items), the order a view without a sort
shows and a drag in it changes; on the roadmap board those are the **Epics 🎯** view (epics and
features, grouped by Status) and the **Backlogs ✨** view (Up Next and Backlog tasks), filtered to
the team (`team:"<board.team>"`), while a sorted view (Search, Inbox) shows another order and
changes none.

`rank <issue> --top|--before <issue>|--after <issue>` (or `order`) sets it: it moves the issue's
item to the top of the board or right before or after another issue's item
(`updateProjectV2ItemPosition`; `--before` is the item after the other one, then the other one after
the item, so the other one's predecessor is never read), adds an issue missing from the board first
as `move` does (with `board.team`, no Status; any accessible repository, subject to GitHub
permissions), reads only the two issues' `projectItems` and never the board's items, acts on each
issue's repository, prints the neighbours the move names (`at the top, above <issue>`, `after <issue>`,
`before <issue>`; `--json` `above`, `below`) and logs `board.rank`; refused (exit 3) for an issue
that does not exist, another issue with no item on the board (an issue of a repository outside the
board's organisation, whose `projectItems` name none, included) and an item placed against itself.
An archived item has no place in board order and GitHub refuses to move it: `rank` leaves it, says
so in one line (`left <issue> archived on the board …`, `--json` `archived`, `closed`) and exits 0,
writing and logging nothing. `--unarchive` restores an open issue's item to the board first and
ranks it (`, restored from the archive`); a closed issue's item stays archived.

`tidy` keeps the board free of finished work without any setup by the person: every closed issue or
pull request among the team's items (`board.team`; every item without one) whose Status is not Done
is moved to Done, so the board never shows a closed issue as In Progress, and one closed for
`board.archiveAfter` (default `336h`, two weeks; negative: never, the moves to Done still happen) is
archived (`archiveProjectV2Item`) after that move; a board without a Done status has its items
archived as they are.

An archived item is not deleted: it stays searchable on the board (`is:archived`) and each line
names its restore, `gh project item-archive <number> --owner <owner> --id <item> --undo`. At most
100 writes per run, the rest counted and left for the next; each item done is logged as
`board.tidy`; `--dry-run` prints what it would do (`would: …`) and writes nothing; silent when
nothing is to be done. The supervisor loop runs it every `board.tidyEvery` (default `1h`; negative:
never) while the GitHub budget is above its floor.

GitHub's own project workflows cannot be switched on through its API, so tidy needs none; a person
may still enable, in the board's Workflows settings, **Item closed** (Status to Done at once) and **Auto-archive
items** (GitHub's own archiving, by its own filter), and tidy then finds less to do. `escalate
[<owner/repo#n>...]` looks at each open item in Blocked (Blocked / Waiting): its Status and when it
was set, its open recorded blockers, its assignees, its newest comments, the workers parked on it
and the person's open notes and recorded answers (`answer.recorded`) on its thread.

It files no note, printing `skipped <url>: <why>`, for an item assigned to anyone outside
`board.people`, one with an open note for `guide.person` on it (`--ref` or `--holds`), one whose
thread the person answered since its Status was set, one whose newest comment by a person comes from
outside the organisation (a customer's), or one whose block is explained: an open recorded blocker,
a worker parked on it or on its task (its serve record or task names the item), or a comment since
its Status was set (or the day before) naming another issue or pull request; any other gets one note
for `guide.person` naming what it waits on, who, since when and the evidence read.

Every item looked at gets a timer (`re-check blocked <owner/repo#n>`) that runs `board escalate` on
it again after `board.escalateEvery` (default `168h`; negative: never escalate); while it is open
the item is not looked at again. A note's timer carries the item's state it was filed on
(`--asked`): when it fires and the item is still blocked and unchanged (Status and when it was set,
open blockers, assignees, newest comment by a person) it is not asked again (`unchanged <url>`),
only re-checked; once it left Blocked nothing happens.

An item whose recorded blockers all closed before its Status was set is left out (the
blocker-cleared step offers it); one whose Status was set after is held by it and looked at. The
read takes the Blocked items whatever `board.order` names. The supervisor loop runs it after each
`board tidy`; a worker that finds its item blocked moves it there (`board move <item> blocked`).

### `beekeeper person`

`beekeeper person <login> [--org <org>] [--refresh]`, `beekeeper person --team <slug> [--org <org>]`

With `--team`, the logins of the team, one per line, read with the person's own `gh` login and not
kept; a team that login may not read answers `permission missing` (exit 3). Otherwise: Whether a
GitHub login is a member of the org (default `giantswarm`), private memberships included: prints
`member` or `not a member` (exit 0), or `permission missing` (exit 3, the reason on stderr) when the
token may not read the whole membership, never `not a member` then.

The roster is read with the person's own `gh` login (the `gh` on `PATH` outside `agents.shell.path`,
`GH_TOKEN` and `GITHUB_TOKEN` ignored), after the token's own membership proved active, and kept in
the state directory for a day (`--refresh` reads it again); the token stays in the process. An agent
asks this instead of `gh api orgs/<org>/members/<login>`, which the App's token answers 404 for a
private member. In the sandbox the broker answers.

### `beekeeper proof upload`

`beekeeper proof upload <image> --repo <owner/repo> [--alt <text>]`

Puts a proof screenshot where a GitHub thread shows it and prints its Markdown image line to paste
into the comment or PR body: the image (PNG, JPEG or GIF, at most 10 MiB) re-encoded as PNG, which
drops its metadata, committed to the images-only branch `beekeeper-proofs` of the repository
`--repo` through the Git data API with the `gh` on PATH (an agent's gh carries the App's token;
beekeeper never reads it), the link pinned to that commit. Private and internal repositories use
authenticated repository image links, visible to repository readers. The App needs contents write
access. See [the browser](agents.md#agents-started-without-a-click).

### `beekeeper browse`

`beekeeper browse "<steps>" [--dir <dir>] [--model <model>] [--timeout <duration>] [--allow-deploy
"<what, where>"] [--person-ok] [--file <path>]…`

Runs browser steps in a headless `claude -p --chrome` turn that has the CLI's own Claude in Chrome
tools and nothing else, so no navigate waits on a site approval, and prints the turn's report, the
deploy the run declared (`allowed deploy:`), the calls its classifier refused (`refused:`, each in
the call's own words with the classifier's reason), the screenshots that timed out on a tab not
drawn (`not rendered:`: behind another tab, or its window occluded or minimised; after the first
such timeout the hook refuses every further screenshot and zoom of the turn, which reads the page's
text instead), the tabs the turn worked in and each window a page opened that it followed in its own
tab (`windows:`), whether it closed every tab it opened, the blank ones included (`tabs closed:`),
or which it left open, in its group or stranded outside it, which fails the command (`tabs left
open:`), the calls that found the Claude in Chrome connection gone and what came of the reconnect
(`connection lost:`), then its transcript and each screenshot as an image file.

The turn checks the connection with `list_connected_browsers` before each step and reconnects once
in the turn when it is gone; a connection the turn did not get back is reconnected once by `browse`
itself, a fresh CLI resuming the turn's session with a fresh Chrome connection, which goes on with
the steps not done; a reconnect that finds no browser ends the command with a non-zero exit,
`browser connection lost at <call>`, naming the steps the report lists as not done.

For the desktop turns of the sessions beekeeper starts; hold the browser lease while it runs.
`--allow-deploy` declares the one deploy or create click the task covers, which the turn's auto mode
then allows for this run only. `--file <path>` (repeatable) shares a file the steps upload through a
page's file input: the turn uploads its copy with `file_upload`, a path outside the worker's
directories is refused before the turn and every other upload by the hook (`shared for upload:`,
`refused: … [Not Shared]`); the hook passes a `file_upload` only to the input beekeeper's
upload-target check marked visible and enabled in the intended form or comment box, never to a
hidden one.

Steps that name an App on record (`beekeeper app`), by name or callback host, run with that App's
auto mode rule and prompt words, the report's `allowed consent:` line naming the record; steps that
name a loopback lab host (`*.127.0.0.<n>.nip.io`, `localhost`) run with the lab's rule (`allowed lab
sign-in:`); the turn's start record has the hook act in it wherever it runs. The hook refuses a
close of the tab the turn's group was made with while another tab of the turn is open, which would
dissolve the group and strand that tab outside it, and the turn's Stop hook (`beekeeper hook
browsestop`, in its `--settings`) has a turn that would end with tabs of its own open go on once to
close them.

A turn never takes the shared Chrome window from a person working at it: with a Chrome window
currently focused and their keyboard or pointer in use, `browse` starts no turn and fails with
`person at Chrome`, naming the probe to park on, `beekeeper browse away` (exit 0 once they leave
Chrome, 3 while they work at it); during a turn a window Chrome opens while they type stays on their
screen, and once they focus a Chrome window the turn yields: it stops, the window goes back at once
(`window yielded:`) and the command fails with `person at Chrome`.

`--person-ok` skips the presence guard for this run only when the person explicitly permits it;
leases and screen locks still apply. `beekeeper browse --help`, and [the
browser](agents.md#agents-started-without-a-click).

### `beekeeper app create <name>`

`beekeeper app create <name> [--timeout <duration>]`

Creates the GitHub App `<name>` the organisation declares (its manifest in `apps.repo` at
`apps.manifest`, read from the default branch) through GitHub's manifest flow and takes every
credential the flow answers into the shared vault item `<name>`, no value reaching a person or a
session; the person's two clicks, Create and Install, are named and waited for, in an agent session
too, which holds the browser lease ([Secret operations](secrets.md#secret-operations)).

### `beekeeper app create <purpose>`

`beekeeper app create <purpose> --installation <name> --base-domain <domain> [--replace] [--timeout <duration>]`

Creates the App of `<purpose>` for one installation from the purpose's manifest template in
`apps.repo` (`apps/<purpose>/manifest.template.json`), `{installation}` and `{baseDomain}` rendered,
as `app create <name>` does for a declared App, its credentials into the vault item of the rendered
name; `--replace` creates the successor (`<name>-2`, `-3`, …) beside the old App, whose deletion it
names as the person's step ([GitHub Apps](github-apps.md#github-apps)).

### `beekeeper app create <installation>`

`beekeeper app create <installation> --manifest <file> [--org <org> | --user <login>] [--no-open]
[--replace] [--timeout <duration>]`

Creates the GitHub App a local manifest declares through GitHub's manifest flow on a loopback
listener beekeeper owns, converts the one-time code in its own process and writes the App's private
key and client secret into the SOPS Secret `apps.secrets` names for `<installation>`, the App owned
by an organisation or, with `--user`, a personal account; stdout is the App's id and client id
alone, and no code, key or secret reaches a session ([GitHub Apps](github-apps.md#github-apps)).

### `beekeeper app installation`

`beekeeper app installation <installation> --app <id|slug> [--org <org>] [--suspend | --unsuspend | --repos <name>,…
(--reinstall | --token <ref>)]`

Reads, as the App itself with the `private-key` of the Secret `apps.secrets` names for
`<installation>` (in beekeeper's process, no value in any output), what GitHub's settings pages keep
behind sudo mode: the App's installation on `--org` (default: `apps.repo`'s owner), its repository
selection and suspension, the repositories its installation token reaches (named for a selected
installation, counted for one on all; the token revoked after), the App's device flow (GitHub's
device code request with its client id: `enabled` or `disabled`, the code discarded) and its user
token expiry, which no API carries (named unread).

A private App is named by its id. `--suspend` and `--unsuspend` set the suspension as the App
(`PUT`/`DELETE /app/installations/<id>/suspended`), no person either way. `--repos` with `--token`
makes a selected installation reach exactly those repositories (additions first, then removals, read
back): GitHub changes an installation's repositories only for a token with
`installation_repositories=write` (a fine-grained token's "GitHub App installation repository
access"), which no App can declare, so `--token` names an organisation owner's token in the shared
vault or a SOPS file.

`--repos` with `--reinstall` deletes the installation as the App, after looking the repositories up,
and answers the installation page with them preselected; its Install click meets GitHub's sudo mode,
an organisation owner's confirmation. Refused in one line naming the setting: `--repos` without a
token, on an installation on all repositories (no API narrows one) or on a suspended one. `beekeeper
log` (`app.installation`) records each read and change ([GitHub Apps](github-apps.md#github-apps)).

### `beekeeper app allow`

`beekeeper app allow <name> [--client-id <id>] [--permission "<scope>: <level>"] [--callback-url <url>]
[--delete [--manifest <file>]] --word "<the person's words>"`, `app list`, `app revoke <name>`, `app
check <url> [--title <title>] [--words <words>] [--navigate]`

The Apps the organisation declares (the App's manifest in `apps.repo` at `apps.manifest`, `{name}`
for the App's name; an App no manifest declares is not recorded) and the person asked a grant for,
on their word (a note's answer), by whom and when (`app.allow`, `app.revoke` in the log).
`--client-id` records a consent: the App's name as GitHub's consent page titles it, the OAuth client
id its consent URL carries and the callback hosts of its declaration (an App without callback URLs,
one used through the device flow, has no consent page to record: refused naming the missing callback
URLs).

`--permission` records one change of the App's permissions (`"Members: Read-only"`), for an App
without callback URLs too. `--callback-url` records one callback URL added to the App's settings, on
a callback host its declaration names (another host is refused: the declaration gains it first),
without user info. `--delete` records the App's deletion on its Advanced page, under the
organisation or the person's own account; with `--manifest <file>` the local manifest of `app create --manifest`
declares it.

`app list` also shows beekeeper's ledger of the Apps `app create` created and where their
credentials went. With one on record, `beekeeper hook pretooluse` answers a Chrome click on GitHub's
consent page, or the navigation to the App's permissions page and the calls of that change, itself
(below); `app check` prints that decision for a page, dry: `allow:`, `refuse:` or `no decision:` and
the reason, `--title` the page's title where the Chrome tools redacted the URL's client id,
`--words` the call's own words (its summary, a typed text, a form value), `--navigate` for a
navigation to the URL instead of a click on it.

## The supervisor and the guide

### `beekeeper supervisor`

`beekeeper supervisor [status]`, `supervisor loop`

The supervisor is the supervisor loop (`supervisor loop`, below), which the
`beekeeper-supervisor-loop` user service runs: `beekeeper install` always installs it. No session
holds the supervisor's role and nothing starts or relays one. `supervisor status` (and the bare
`supervisor`) says whether the loop runs, exit 3 when it does not. While the loop is down the
standby watch restarts its systemd service on Linux (at most once a minute, never during a
`beekeeper stop`; on macOS launchd restarts a failed loop) and, once it stayed down two minutes,
says `SUPERVISOR LOOP DOWN` and notifies (`no-supervisor`) until it runs again (`SUPERVISOR LOOP
BACK`). A message to "the supervisor" is refused, naming `agents idle --done --report`, `agents park --on`
and `note add`.

### `beekeeper guide start`

`beekeeper guide start [--new]|stop|status|relay`

The guide: the session that walks the person through the decisions waiting on them; the supervisor
is the supervisor loop, no session. It has no grant power. Only beekeeper starts a guide run, and
only such a run takes the role: its role moves by relay (a relay to a fresh "Guide run N+1", its
start, relief with exit 4, restart grace counted from when its CLI was first seen gone, a fresh
successor after a crash); `guide start --new` starts the next run for the person from a terminal (a
gone guide, none recorded), and `beekeeper resume` starts one when the guide's session runs no CLI;
`guide handover --prompt` is the successor's prompt (`guide.skill`, default `guide`, or
`guide.instructions`, then the board holds and the epics with their plans as `handover --section
board-holds` and `--section epics` show them, its queue and an open relay).

### `beekeeper guide next`

The one decision the guide asks now: the open note for its person due first, marked as being asked
(`guide.asking`), served again until `answer --note` (or `note answer`) or `note done` closes it;
only then the next. Only the guide's session calls it; `--json` the item. On a machine with a
central instance its notes for the person are served beside the machine's, as `<context>#<n>`.

### `guide.personNames`

`guide.personNames`, `guide.desk`

Other names of the guide's person: `note add --for` one of them files the note for `guide.person`,
and a name nobody on the desk goes by is refused. `guide.desk` is the guide's memory, a desk
checkout: its session may write files there (commits stay the desk's), and a refused write to a
harness's own memory names it. `beekeeper install` turns Claude Code's own memory off
(`autoMemoryEnabled: false`, on execution hosts too) and the hook refuses every session's writes to
Claude Code's, Codex's, agy's and omp's memory folders, naming the desk.

### `beekeeper guide queue`

The guide's queue: every open decision filed for its person, `guide.person` (`note add --for`, or an
older note's `[for <person>]` text prefix; any case), with its owning session (the one that filed
it, and whether it still runs), deadline and default, then every session whose record says it waits
on the person (`sessions serve … --waits "<person>: <ask>"`, the person in any case; with
`guide.person` unset, every `--waits`), while its CLI runs or stopped (`(stopped)`) less than
`guide.waitingTTL` ago (default 2h; 0: never folds), except the guide's own session, an archived
one, a test (titled `test: …`) and an agent that reported its task after the wait (`agents idle`,
`--done`, or taken off the roster).

The sessions stopped longer ago fold into one line, `<n> stopped session(s) waited on <person>`;
`--full` lists them. `guide watch` says nothing for a session that only aged out. With
`guide.person` unset, every note filed `--for` anyone, and a line that says so. On a machine with a
central instance, its open notes for the person follow the machine's, as `<context>#<n>`. Delta
output like `sessions`.

### `beekeeper guide watch`

`beekeeper guide watch [--once]`

The guide's feed, silent otherwise: `GUIDE DECISION` for each new open decision of the queue (only
`guide.person`'s; with it unset, every `--for` note and one `GUIDE:` line that says so), `GUIDE
WAITING` for each session of the queue newly waiting on its person (only an explicit `--waits`: the
desktop's turn summary does not count), `GUIDE ANSWERED`, `GUIDE DEFAULTED` or `GUIDE CLOSED` for a
note of the queue closed (`GUIDE CLOSED #<id> overtaken: <reason>` for an overtaken one, `GUIDE
REPLACED: #<id> replaced by #<new>: <text>` for one a successor replaced), `GUIDE ORPHANED #<id> for <person>,
its filing session "<name>" is archived; ask it, or close it with note done <id> --overtaken: <text>`
for an open decision without `--ref` whose filing worker session is archived (`note add --ref`), and
the guide's relay: `GUIDE RELAY DUE` once its context reaches `guide.relayAt` (200k), or a new feed
line arrives after an idle hour with context over half that threshold, `GUIDE RELAY TAKEN`, `GUIDE
RELAY EXPIRED`, `GUIDE RESTARTED`.

Each is one line, once: what it said is kept in the state (`guide.fed`). About itself: it
re-executes after binary replacement, reporting `GUIDE WATCH STALE` once if re-execution fails, and
`GUIDE SAVE REFUSED` with the lines a save refused by a newer release would have recorded (again
only when they change), each naming the command that re-arms it.

## Agents

### `beekeeper agents register|assign|idle`

The roster of empty sessions registered as spare capacity. `register` names the session by its title
(a `claude --bg` worker by its `-n` name) unless `--name` overrides it. `--harness omp|agy|codex`
records that the session runs on another harness than Claude Code and was not started by `agents
start`: beekeeper has no message route to it, so `agents resume` and the watch never wake it
headless, and it leaves its own park with `agents resume`. A caller whose parent chain passes two
claude session CLIs (a claude started inside another session) is refused, naming that parent
session, whose ids it inherits; the parent's entry stays as it is.

A name is one agent's: registering under the name of a session that no longer runs (what `agents`
shows as `not running`: stopped, closed or asleep) replaces its entry, saying so; a task the
replaced entry left unfinished becomes the new entry's, with its assignment time, named in the
`agents.register` event (`replaces <id>, takes over "<task>"`) and in the output, so the fresh
session works it and a later `assign` to the name is refused as busy until `idle`.

Re-registering keeps the session's own open task, so a session `agents start` registered busy stays
busy when it registers itself (`register: <name> busy with "<task>"`); two dropped entries with open
tasks are refused (exit 3). A name a running session's entry holds is refused (exit 3). `assign` and
`remove` take a session id, a name or a unique part of one, and refuse a name several entries share.
`idle --done` requires the final report, `--report "<text>"` (`-` reads stdin), and its "Problems
found": `--problem "<finding>"` once per broken function, workaround, follow-up or problem the task
met (evidence and owning repository in the line), or `--problem none`; without them, or with `none`
beside a finding, it is refused (exit 3). beekeeper delivers them itself: both are logged
(`agents.report`, `agents.problem`) and the supervisor's watch prints them once, `WORKER REPORT by
"<agent>" (task: …): <report>` and `PROBLEM FOUND by "<agent>": <line>` per finding, for the
supervisor to file and hand to a worker; a finding that names its issue's URL
(`https://github.com/<owner>/<repo>/issues/<n>`) is filed already, and its line says so.

The supervisor loop logs unfiled findings intact in `supervisor.file`; `guide watch` delivers them
once as `GUIDE PROBLEM` to file in their owning repositories, before the agent leaves the roster.
These status reports create no decision notes and need no thread reference. Printed reports escape
newlines, carriage returns and backslashes onto one physical line; the event keeps the original
text. Legacy file events summarizing an already-delivered note are excluded, and deduplication keys
are retained only while the corresponding reports remain in the log.

Two identical transition failures defer another attempt for five minutes, including across loop
restarts; the first failure and backoff are each announced once. A step that keeps failing the same
way, for 30 minutes or on more than one item, is logged once per episode in `supervisor.file` naming
the step, the items it blocks, the attempts and the error, and `guide watch` says it as `GUIDE
PROBLEM`; the episode ends once every item has missed its scheduled retry by 15 minutes.

### `beekeeper agents start`

`beekeeper agents start <name> <brief file> [--task t] [--model m] [--dir d] [--harness
claude|omp|agy|codex|auto] [--exclude c] [--review] [--review-of-self-hosted] [--mode
desktop|background] [--builds] [--force] [--dry-run]`

Starts an agent session without a click that runs in Claude Desktop from its first turn; with
`--mode background` (default `agents.mode`) a Claude Code background session with Remote Control
instead, with none of the caller's `CLAUDE*` variables, on the roster when the start returns, and
nothing of the desktop: see [Background sessions](agents.md#background-sessions). Under a worker
profile (`agents.profile`) its task runs as a headless turn under the profile instead and no seed
turn runs; `--dry-run` prints the profile applied, the first turn's command line and the first
prompt with its size, and starts nothing.

See [Worker profiles](worker-sessions.md#worker-profiles). The start is judged first as `beekeeper
capacity` judges one, `--builds` marking a task that builds, tests or lints and so waits its turn
for a build slot in the queue `beekeeper run`'s waiters share, first in first out; a verdict without
room refuses it with exit 3, the guards named, and no session is created, unless `--force`, which
starts it and records the verdict overridden in the event log.

`--harness auto` picks the first non-excluded `(harness, model)` choice in `capacity.choices` whose
provider has room, the pick `beekeeper capacity pick` prints; without `--harness` a desk with
`capacity.choices` starts on that pick too (unless `--model` names a model none of them has), one
without them on claude (`--exclude` skips matching choices, `--review` marks a review start, using
reserved review headroom, and `--review-of-self-hosted` the review of a self-hosted model's work,
which takes no self-hosted provider).

Before the session exists it records the id beekeeper chose as one of beekeeper's starts (the
state's `starts`, kept 30 days) and registers it on the roster under the name, busy from its start
with `--task` (by default the brief's first line) or with the open task of a stopped entry under
that name. A seed turn without tools (`claude -p` in a transient user unit `beekeeper-agent-<id>`)
creates the transcript with the worker prompt; the session is then imported into the desktop
(`claude://resume?session=<id>`, sidebar row `local_<id>` titled with the name), the desktop
switched back to the session it showed (`claude://code/continue`), and the task runs as a desktop
turn of the session's desktop CLI, or of one a steward's `send_message` starts; headless only where
the desktop cannot run it, said with the reason.

See [Agents started without a click](agents.md#agents-started-without-a-click). `--harness omp`
starts an omp agent instead, see [omp agents](harnesses.md#omp-agents); `--harness agy` starts an
agy (Antigravity CLI) agent in a persistent unit with a FIFO inbox and `PreInvocation` mid-turn
delivery, see [agy agents](harnesses.md#agy-agents); `--harness codex` starts a codex (OpenAI Codex
CLI) agent in a persistent unit with an isolated `CODEX_HOME` and FIFO inbox, see [codex
agents](harnesses.md#codex-agents).

### `beekeeper agents desktop`

`beekeeper agents desktop [agent]`

Asks for an agent's desktop turn (the browser): its import or reopen shows it in the desktop without
waiting for the window's focus, the person's typing holding it 1 minute at most; an agent with
neither a CLI nor a waiting reopen is shown at once. By default the calling agent. See [Agents
started without a click](agents.md#agents-started-without-a-click).

### `beekeeper agents wake`

`beekeeper agents wake <agent> <message> [--permission-mode m]`

Messages a registered agent, or a session off the roster by its id (a CLI session id, a `local_` id,
an `omp_`/`agy_`/`codex_` id) or a role holder by its name, or a worker taken off the roster by its
exact name (it comes back on the roster with its last task, in its own conversation; `agents resume`
does the same, and a harness that cannot continue is refused with the name of its stored transcript;
an SSH worker whose process exited goes on in its own workspace and conversation, a parked one for a
settled wait too, unless it was stopped, retired or runs on omp), without Claude Desktop's `local_`
route and its cap (a session's desktop sends pause after ten since its person last typed in it).

A running CLI gets the message by name; a session with a desktop row and no CLI gets it as a desktop
turn, a steward's `send_message` starting its desktop CLI; only where the desktop cannot run it
(said with the reason) is it resumed headless, `claude -p --resume <id>` with the message as its
turn, in its directory, permission mode (a start's bypass, else the desktop record's) and recorded
model, in a transient unit `beekeeper-wake-<id>-<wake>`, one per wake; its `ExecStopPost` starts
`agents reopen <local_ id>` in a unit of its own (`beekeeper-reopen-<id>-<reopen>`), which warms the
desktop's CLI again.

`agents` shows `live, first turn running` or `live, wake turn running` while a headless turn is the
session's CLI. A background session (see [Background sessions](agents.md#background-sessions)) with
no running CLI is continued by `claude --bg --resume <id> <message>` instead, with no desktop route.
See [Waking a session](agents.md#waking-a-session).

### `beekeeper agents handover`

`beekeeper agents handover <agent> [--prompt] [--model m] [--dir d] [--task t]`

Hands a registered agent over to a fresh session near its context limit, one line per step: asks it
by peer message, or in a headless turn resumed from its transcript when its CLI does not run or take
the message, for `beekeeper agents note "<what is in flight, what is next>"` (waiting
`agents.noteWait` at most), builds the follow-up's prompt, stops the original CLI, turns and child
processes before starting the follow-up as `agents start` does under the agent's name (it takes over
the roster entry, the task, the session record, the leases the old session holds and the grants
queued for it, in their places), makes sure the roster lists the follow-up busy with its task (a
`claude --bg` predecessor is stopped through `claude stop` first, so its daemon does not resume it),
archives the old session's desktop row once its CLI has exited, and logs `agents.handover`.

A CLI of the old session the desktop revives later, or of a run a relay relieved past 2 minutes and
running no tool command, the watch stops with its reopen units (`REVIVED`, logged as
`agents.revived`), so one session answers to the name; a CLI the doctor warmed to archive its own
desktop session runs on for 5 minutes, for its archive call. A live merge gate refuses handover
until it finishes. The doctor never warms a superseded session to archive itself; another steward
archives it.

`--prompt` prints the prompt only. `watch` says `HANDOVER DUE` once per agent session at
`agents.relayAt`, or `HANDOVER HELD` while the agent's task is in its last step
(`agents.lastStepGrace`). See [Agents handed over near their context
limit](agents.md#agents-handed-over-near-their-context-limit).

### `beekeeper agents park`

`beekeeper agents park [--on <#note|<context>#note|owner/repo#n>] "<what>"`, `agents resume
[<agent>]`

`park` parks the calling agent, its task kept, on what settles its wait: counted parked, not busy,
in `agents` and `capacity`, it ends its turn. On a machine with a central instance, `--on <context>#<n>`
parks on its open note `n` (a bare number the machine has no open note of is the central instance's,
one open on both is refused); the watch reads the central instance's open notes (`note_list`) each
poll while such a park stands and, once its note closed, its answer from the central feed
(`read_feed`).

Once the note is closed, the issue or pull request merged or closed, or the pull request given a
review or comment by anyone but its author since the park (one posted under the author's own login,
as a desk whose sessions share one login posts its own review, settles nothing), the watch says
`AGENT RESUMABLE` once; a park on a person (a note, or a wait naming `guide.person`, an answer, a
review or a decision) the supervisor's watch says once as `PARKED ON A PERSON`, for the guide to
tell the person, and a park on a lease (`lease claim --park`, or a wait naming only the supervisor
or guide, including a gate, an install chain, a timer or a probe) never: one on a lease it says once
as `PARKED ON A LEASE` with the agent's place in the lease's queue, for the supervisor's grant; with
`agents.autoResume` it resumes the agent with what settled it, a person's answer word for word.

`resume` does it by hand. Run by the parked agent itself with no agent named (or naming itself),
`resume` ends its own park, wakes nobody and prints what settled the wait. An agent of a harness
beekeeper has no message route to (a session of another harness that registered itself with `agents
register --harness`, not one `agents start` started on that harness) is never woken headless, which
would start a second, unrelated Claude Code session: `resume` ends its park and says so, its park
line and `AGENT RESUMABLE` name its own `resume` instead of the watch, and `agents.autoResume`
leaves its park resumable for its own `resume`.

A managed SSH worker's `resume` ends its own park on the coordinator, and only its own. A session
off the roster (any session that never registered) parks too: its park is kept apart from the roster
(`parks` in the state), settles the same way and resumes it by its session id, as `agents wake <session
id>` does; one whose session is gone by then gets no wake and one line, and the issues it served go
back to the board; joining the roster (`agents register`) ends it.

A roster agent without a task is refused. See [Agents started without a
click](agents.md#agents-started-without-a-click).

### `beekeeper agents archivable`

`beekeeper agents archivable <local_id>...`

Confirms, per desktop session, that it is a finished worker beekeeper started (one of its starts,
off the roster, no role a relay did not relieve, unarchived, its CLI in no turn but the caller's
own) and exits 3 for any other, a session the person started included. A steward asked to archive
runs it first and archives only what it confirms (see [Agents started without a
click](agents.md#agents-started-without-a-click)).

### `beekeeper agents broadcast`

`beekeeper agents broadcast <message>`

Sends one message by name to every registered agent whose CLI runs, one after the other, the
supervisor and the caller left out; a stopped agent is not resumed for it. One `agents.broadcast`
event logs whom it reached.

### `beekeeper agents problem`

`beekeeper agents problem <finding>`

Records a mid-task finding immediately as `agents.problem` and delivers it to the supervisor’s
watch, preserving the worker’s task and park. Include evidence and the owning repository, with full
issue and PR URLs. Repeating a finding, including at `idle --problem`, logs and delivers it once per
worker assignment. Reports explicitly naming only the worker’s claimed issues or owned open PRs stay
in the delivery timeline as `supervisor.progress`, including after the worker leaves; they create no
guide filing task. Unknown ownership and reports naming another repository remain findings. Repeated
fingerprints already mapped to filed issues increment their observation tally without creating new
guide work.

### `beekeeper agents note`

`beekeeper agents note <text>`

The calling agent's hand-over note, logged as an `agents.note` event; the next `agents handover`
puts the latest one into the follow-up's prompt. It waits 3 s at most for the state lock: a lock
held longer fails the note unsaved, naming the lock's holder and, when one is pending, the desktop
import of the caller's session, to be noted again.

## Notes, answers, reports and timers

### `beekeeper note add|answer|done`

Questions for a person, of two kinds (`--kind`): a decision (the default) or a sign-in (`login`);
every note names its person (`--for`). A decision waits on a person with its deadline and what
happens if nobody answers (`note add --for Ada --ref owner/repo#n --due 22:55 --status-quo "<what is
true now>" --why "<why it needs Ada>" --default "the alert stays as is" <text>`); the guide puts
them to its person (`guide queue`, `guide watch`). A session's state is its hand-over, rendered from
live state; deferred work is a board item, a priority a rank (`board rank`) and a hold a blocked-by
(`board block`); a hold on a person is the decision itself, `note add --holds`: every `--ref` is set
`Blocked / Waiting` on the board, which no step of `board next` offers (`board next` skips it as
`held: decision #<n> for <person> until it closes`; one refused leaves none held), and each set back
to the Status it had once the decision closes, answered, by its default, as overtaken or by hand
(`board.released`, `BOARD RELEASED`; its filer told), unless its Status was changed while held
(`HOLD LEFT`, left as it is, the filer told why) or another open decision still holds it (`HOLD
KEPT`); `--holds` without `--ref`, or on an issue not on the board, is refused with exit 3.

`note list --for <person>` lists one person's notes; `note list --full` prints each note's whole
text, every line, with its default and the issues and pull requests it names from its text and
`--ref` links, and `--json --full` adds them as each note's `names`. A decision names the issue or
PR it concerns (`--ref`, the first one the thread its answer is about), and is refused with exit 2
without them, `--due` and a `--default` that is an action. A decision for `guide.person` (any
`--for` with it unset) is refused, naming what it lacks, without `--status-quo`, `--why`, a
`--default` that is an action (not `wait` or `none`), every `--option` as `<choice>: <consequence>`,
the full URL of every `#N` or `owner/repo#N`, and `--checked "<source>"` for a claim that something
is merged, green, released, rolled or closed, and while a pull request it links in a plans
repository (`plans.repositories`) is closed, or open with its stage check (`plans.check`,
`plan-stages`) pending, missing or red for anything but missing stage outputs, naming the stage and
what would admit it (a check red only for `<plan>: <stage> missing` findings is the grill stage,
whose output is questions for the person: admitted, the stage named on stderr); one folds into the
open note for the same person (`note.folded`) only when it asks the same question (case, spacing and
closing punctuation aside) on the same issue or PR, its `--ref` or else the first URL of its
`--status-quo` (URLs cited as context and a shared first word never fold); the same question with a
different `--default` or recommended option is refused, naming the open note, and `--fold-into <n>`
folds into open note `n` for the same person explicitly, whose default and recommendation stand.

`--kind login --until "<probe>"`: the watch runs the probe each tick and closes the note once it
exits 0; a login is refused without its probe. A note for a person on a thread they answered on
within 72 hours (`answer.recorded`) is filed with a warning that quotes the answer. `watch` reports
a note once when it is due; a decision due unanswered closes with its default (`NOTE DEFAULTED #<id>: <default>`,
`note.defaulted`), and the session that filed it is told; an answer within the day after replaces
the default.

A decision renders as one Slack message (`beekeeper serve --help`): it is refused with exit 2
without `--status-quo`, with a question over one line or 150 characters, a status quo over 3000,
more than 10 `--option` or a label over 75 characters, or a `--recommend <n>` (1-based) that names
none of them; `--for team:<name>` puts it to a team. On a machine with a central instance
(`central.context`) a decision is filed there once its checks pass (`note_add`, through muster as
the person): the central instance puts it in Slack, its number is the central note's (`note #<n>
(central <context>: …)`), and its answer or default reaches the filing session through the person's
mailbox (a role's run relayed since: the role's holder now, or its next start); a sign-in, `--holds`
and `--fold-into` a machine's note stay on the machine; `--fold-into <context>#<n>` (or a bare `<n>`
only the central instance has open) folds a decision into the central note `n` (`note_add`'s
`fold_into`), whose filer is told its answer.

With the central instance unreachable before the call went out, the decision is filed on the
machine, as stderr says; a call sent and left unanswered files it nowhere else and exits 69. `--ref
owner/repo#n` (repeatable) links a note to the issues or PRs it asks about: once every one is closed
or merged, `watch` closes it as overtaken (`NOTE OVERTAKEN`, `note.overtaken` with the reason;
`watch --once` names it and writes nothing), when its worker closed them at the end of its task: an
issue a pull request's closing keyword closed at its merge (`<ref> closed by <owner/repo#pr>'s
closing keyword`), or any close while the worker that filed the note still runs its task, keeps the
note open, said once per reason as `NOTE KEPT: #<id>, <reason>: <text>` (`note.kept`); the worker's
`agents idle` ends its task, and from then on its settled issues overtake its notes, a keyword close
included; a note without `--ref` is never closed by the watch: once the session that filed it is
archived (a stopped session is not), `guide watch` names it to the guide as orphaned, to ask or
close by hand (a role's run, current or relieved, never orphans its notes).

Login notes, and the note the guide asks now, are never overtaken. `note show <id>` prints one
note's full text, audience, status and retained answer (`--json` returns one object); answered
file-backed notes require their audit event, and the central store retains closed notes for its
history period. `note done <id> --overtaken "<why>"` closes one so by hand. `note answer <id>
[--choice <n>] [<answer>]` is `beekeeper answer <its first --ref> --note <id>` (below); a note
without a `--ref` is refused, naming that form.

On a machine with a central instance, `note list`, `note answer` and `note done` also act on its
notes, named `<context>#<n>` ([The central instance](remote-hosts.md#the-central-instance)). A
decision that asks whether to merge a pull request (to approve, confirm or authorise its merge) is
refused from anyone and logged as `note.redirected`: the supervisor loop merges a pull request after
its desk review and the fixes of its findings, once its checks are green, and asks nobody.

The full hand-over carries `standing.md` in the state directory verbatim; the guide successor prompt
references the file once, which the person edits (an instruction ends when its block is deleted).

### `beekeeper answer`

`beekeeper answer <owner/repo#n> [--note <id>] [--choice <n>] [--via cli|slack] [--post] [<words>]`

Records the person's words verbatim about the issue or pull request they concern, however they came:
`answer.recorded` in `beekeeper log` with the thread and the words; `--note` closes that note
(`--choice <n>` one of its options, 1-based, the words optional then) and tells the session that
filed it (`NOTE ANSWERED #<id> by <person>: <answer> (on <thread>)`, which `watch` prints too; a
note a role's run filed goes to the role's holder now, or waits for the guide's next `guide start`
while none can be told); without a note the sessions serving the thread (`sessions serve`) are told
(`ANSWER on <thread> by <person>: <words>`).

A decision its default closed within the last day is answered all the same: the answer replaces the
default, its filer told so (`… (replaces the default applied: <default>)`); a day later it is
refused. Nothing is posted to GitHub: what shows there is the work's own result. `--post` also
records the decision as a comment in plain words (who decided, the words, the issue or pull request
by its URL when the comment is elsewhere; no note, session or channel): a thread in a private
repository gets it itself; a public one gets nothing, the comment goes to the first private plan
pull request (`plans.repositories`) whose Epic line names the thread or an epic above it, else to
the nearest epic above it in a private repository; with neither the answer is refused (exit 3),
naming what to add, nothing posted.

The hand-over's answers section (those since the predecessor's start) and `note add`'s re-ask
warning read `answer.recorded`. The event records `--via cli` or `--via slack` as how the words
came.

### `beekeeper reporter final|pause|resume`

Pauses the scheduled reporter: `final 06:45` (or `45m`) starts one last report at that time,
covering the time since the last one, then pauses; `pause` pauses now; `resume` starts the current
slot's report at the standby watch's next poll and the schedule again.

### `beekeeper report`

`beekeeper report [--since 1h|15:04] [--until 15:04] [--tz zone]`

Renders the status report from beekeeper's own state, as Markdown that passes `reporter check`: the
pull requests and promotions the gate merged in the window with title, release and rollout; the
running work (roles, busy agents, sessions serving an issue) with the issue, what it waits on and
its leases; what waits on `reporter.person` (open notes per owning session, sessions waiting on an
answer, the open drafts of `reporter.reviews`); the merge lanes, timers and holds; the machine.
Every time in `reporter.tz` (`--tz`; default the machine's zone); GitHub reads for the titles and
drafts, continuing through draft pages when needed. See [The scheduled status
reporter](notifications.md#the-scheduled-status-reporter).

### `beekeeper reporter check`

Checks a report on stdin as the reporter's post hook does (see [The scheduled status
reporter](notifications.md#the-scheduled-status-reporter)): one line per problem and exit 3, or
`ok`.

### `beekeeper reporter`

The scheduled status reporter (see [The scheduled status
reporter](notifications.md#the-scheduled-status-reporter)): its schedule, when the next one starts,
and the current or last run with its outcome.

### `beekeeper timer add|done|list|check`

Times to look at something: `timer add 22:55 "check the rollout"` (or a duration, `45m`). `watch`
prints one line when a timer is due; it stays open until `timer done`. A timer can wait on a
condition from its time instead: `--when "pr-merged owner/repo#n"` (also `pr-checks owner/repo#n`,
which holds once the head's checks settle, every one green or any red (failed, cancelled, timed
out), or the pull request merged or closed, and tells the woken agent which, with each red check's
name and URL; `issue-closed owner/repo#n`, `helmrelease-ready context/namespace/name [version]`,
`controlplane-ready context/namespace/name`) or `--probe "<command>"` (exit 0: it holds), checked by
the watch every `--every` (5m), each distinct check once per tick however many timers share it and a
GitHub one only above the budget floor; it fires within one tick of the condition holding.

`controlplane-ready` reads a KubeadmControlPlane of Cluster API v1beta1 or v1beta2: every replica
ready and up to date, or the v1beta1 `Ready` condition, once the status has observed the spec.
`helmrelease-ready` with a version holds once the HelmRelease is Ready and runs that chart version:
the newest release of `status.history`, or without a history the attempted revision while Ready
(current Flux leaves the applied revision empty, so a `--probe` of it never holds).

A condition that has not held for 24 h is one `TIMER STUCK` line (event `timer.stuck`) and the timer
keeps waiting; `timer list -v` says since when. A reference the watch cannot read (an unknown
context, no access, no such kind or object) is one `TIMER UNREADABLE` line, again when the reason
changes, and the timer keeps waiting; `timer list -v` shows what each timer's last check found,
`timer check <id>` runs it now. `--until` ends the wait: the timer then fires as timed out, or with
`--expire` closes unfired (`TIMER EXPIRED`).

`--wake <agent>` wakes the agent with the timer's text (as `agents wake`; `TIMER WAKE FAILED` when
it cannot), `--run "<command>"` runs a command, its output in `timers/<id>.log` of the state
directory and its exit code logged (`timer.ran`, `TIMER RAN`); without either the `TIMER` line is
for the supervisor. Such a timer closes when it fires; with `--repeat daily`, `weekdays` or `weekly`
a `--wake` or `--run` timer is re-armed at the same local time instead and stays open until `timer
done` (a morning refresh, a Friday summary).

## Hooks, secrets and the sandbox

### `beekeeper hook pretooluse`

`beekeeper hook pretooluse [--format claude|agy|codex]`

The PreToolUse hook for Claude Code (`--format claude`, matcher `*`), `agy` (`--format agy`, in
`~/.gemini/config/hooks.json` under `"beekeeper"`, covering `run_command`, `manage_task`, file-write
tools
`write_file`/`write_to_file`/`edit_file`/`replace_file_content`/`multi_replace_file_content`/`notebook_edit`,
read and search tools
`read_file`/`view_file`/`grep_search`/`find_by_name`/`list_dir`/`list_directory`, `ask_question`,
`send_message` and `call_mcp_tool`/`mcp_*`) and `codex` (`--format codex`, in `~/.codex/hooks.json`
and `<stateDir>/codex/home/hooks.json` with matcher `.*`, covering
`Bash`/`bash`/`exec_command`/`shell_command`/`shell`/`local_shell`, `apply_patch`, `write_stdin`,
`view_image` and `update_plan`; `PostToolUse`, `PermissionRequest` and `SessionStart` are Claude
Code hooks).

On `--format agy` it answers `{"decision":"deny","reason":"…"}` on refusal,
`{"decision":"allow","overwrite":{"CommandLine":"…"}}` (or `{"Recipient":"…"}` on `send_message`)
when rewriting, and `{"decision":"ask"}` on pass (`agy` 1.4.7 denies empty stdout or `{}`); on
`--format codex` it fails closed on any internal error, answers
`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"…"}}`
on refusal,
`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"…"}}}`
when rewriting a command for `beekeeper run` or `beekeeper gate`, and `{}` on pass.

It rewrites build, test and lint commands to `<this binary> run -- zsh -c '<command>'`, in a flat
list of simple commands the builds alone (a pipeline stays whole, neighbouring builds share a run,
and a line that leans on its own shell, by an assignment, a compound command or a command the line
may have built, stays whole), with the command verbatim and the tool timeout at 10 minutes, and
starts background calls that invoke `beekeeper run` directly, after a `cd` or `export` chain, or
through a readable shell wrapper with `export MEMCAP_BACKGROUND=1;`, so the run waits 60 minutes for
a slot instead of 8.

Other background calls pass unchanged. A wrapper that cannot be detected (dynamic paths, unreadable
files, non-shell scripts or nesting beyond four levels) needs explicit `--wait 60m`: a command that
runs one at a command position, in a `-c` or `eval` string or in a here-document a shell reads,
never one named inside a quoted argument (a `grep` pattern, a `jq` filter); lab commands
(`agentlab`) and `graphify update` take no slot. It refuses a third kind cluster, listing the held
leases.

It puts the gate in front of every `devctl pr merge`, `pr wait`, `release wait` and `rollout wait`,
behind prefix commands, in pipelines and lists and in a here-document a shell reads, and refuses one
it cannot reach: hidden in a `-c` or `eval` string, in a script file the call runs, in an
interpreter's code or in the program a shell or an interpreter reads from its input, naming the
place and the line (below); every other devctl command passes untouched.

It refuses a `git push` to `main` or `master` or a force-push (`--force`, `-f`,
`--force-with-lease`, `--force-if-includes`, `+refspec`) and a `gh pr merge` (or `gh` write) without
an explicit repository after an unfollowed `cd`, judged by the target repository's directory (`git -C <dir>`
or the subshell/chain `cd <dir>` before the command, not a later `cd`). It refuses a delete that
reaches further than it names ([Deletes](safety.md#deletes)), a command that would print secret
values (below, [Secret reads](secrets.md#secret-reads)) and a call that would send one off the
machine ([What leaves the machine](threat-model.md#what-leaves-the-machine)), a kube context switch
and a write to production ([Kube contexts and production
writes](clusters.md#kube-contexts-and-production-writes)), a command that opens a page in the
person's browser (`gh auth login --web`, `xdg-open`) outside the session holding the `browser`
lease, a `muster auth login` without `--no-browser` (its last occurrence, outside a comment, read as
muster reads its flags) or a `MUSTER_NO_BROWSER` assignment true in front of it in every session,
the lease holder's too, since muster opens the person's default browser outside the Claude in Chrome
tab group where no session's browser tools reach the page, naming `muster auth login --no-browser`
(which passes in every session: it prints the sign-in URL and waits for the callback) and the
`beekeeper browse` step under the `browser` lease that opens the printed URL, a Chrome call that
acts on GitHub's consent page (`github.com/login/oauth/authorize`: a click, a key, a typed text, a
form value, a batch with one) of an App no `beekeeper app allow` record covers, naming the page and
the record's form, while one on the consent page of an App on record, its client id (or its name in
GitHub's title, where the tool redacts the id) with one of its declared callback hosts, is allowed
with the record in the reason, as is a fixture user's sign-in or consent on the lab's own identity
provider on a loopback host (`*.127.0.0.<n>.nip.io`, `localhost`), and a consent page on any other
host is refused (a path or title naming a consent or grant, or an OAuth authorization page where the
call's own words name a grant); a change of an App's permissions likewise from the record of the one
change the person asked for (`app allow --permission`): the navigation to that App's permissions
page under the organisation's settings, the calls there in the change's words and Save changes, and
the installation's Review request and Accept new permissions naming the App are allowed, the
permissions page of any other App and any other call on the recorded one refused, naming the
record's form; the page is the tab's last known URL and title in the session's transcript, and a
call whose page it does not show gets no decision ([the
browser](agents.md#agents-started-without-a-click)), a command that loads a model on the host's
model server outside the session holding `model-server` ([The model
server](capacity.md#the-model-server)), a board-wide project read (`gh project item-list` without
`--query`, a `gh api graphql` query over a `projectV2`'s `items` without a narrowing `query:`),
naming its estimated cost, the GraphQL budget last read, and `beekeeper board add`, `beekeeper board
show`, `gh project item-edit --id`, `beekeeper board move` and a filtered read instead, in a session
`agents start` started a `gh api` read of an org's members, a login's membership, an org's teams or
a team's members, or a login's orgs (the REST paths, or the same query in a `gh api graphql` body),
naming `beekeeper person <login>` instead, since the App's token an agent's `gh` carries sees public
memberships only, in such a session a search over issues or pull requests that spans more than one
repository (`gh search issues` or `prs` without exactly one `--repo` or `repo:`, or `gh api
search/issues` likewise), naming the per-repository `gh issue list --repo <owner/repo> --search`
instead, since GitHub drops a repository the token cannot read from such a search without an error,
a blocking `AskUserQuestion` (or `agy` `ask_question`) call in any session ([Questions go to the
guide](guide.md#questions-go-to-the-guide)), and in the guide's session the calls that do work and a
question without its status quo, why and full links ([The guide asks and
relays](guide.md#the-guide-asks-and-relays-it-never-works-itself)).

A `SendMessage` to `the supervisor` or `the guide` goes to the session holding that role now, by the
name its running CLI answers to (else its desktop session), so a brief names the role and a relay
never makes it stale. A relieved run is never its target: right after a relay, while the successor's
CLI runs without a peer socket, the message is refused with when to send again. The holder is
beekeeper's own record, matched to its CLI by session id, never a title: while another live session
carries the holder's title, the message goes to the holder's CLI by its own socket (`uds:…`), and
`beekeeper doctor` and the watch (`SAME TITLE`) name every title two live sessions carry.

A relay or a role's `start` refuses a run name another live session carries, naming that session. A
`SendMessage` to a desktop id (`local_…`) whose session has a running CLI goes to that CLI by name,
so a headless turn gets no second copy beside it (below, [Waking a
session](agents.md#waking-a-session)). A `SendMessage` from the session holding the supervisor role
that says `yours <resource>` records the grant of that resource to its target as `lease grant` does,
and tells the supervisor what it recorded, or why nothing was.

A session's first write or `git commit` in another repository carries that repository's `CLAUDE.md`,
`AGENTS.md`, rules and mandatory reads ([A repository's instructions on the first
write](agents.md#a-repositorys-instructions-on-the-first-write)). Every GitHub write to a repository
outside `github.allowedOrgs` (a pull request, issue, comment, review, close, edit, merge or release
there, a `gh api` or `curl` write, a `git push` to such a remote, a connector post) is refused
before it runs, naming the rule, and logged as `hook.refused` ([Writes outside the allowed
organizations](safety.md#writes-outside-the-allowed-organizations)).

### `beekeeper hook posttooluse`

The PostToolUse hook (matcher `*`): replaces a tool result that carries an indexed secret value or a
token pattern with its redacted copy before the model sees it, logs `scan.redact` and files one
rotation note per indexed reference (see [What reaches the
model](threat-model.md#what-reaches-the-model)).

### `beekeeper secret compare|fingerprint|copy|set|rotate|unset|reveal`

The credential operations no agent runs itself: equality, keyed fingerprints, a SOPS file copied
under a new name and namespace, a value into a SOPS path or a consumer's stdin, a generated value
into the shared vault and a SOPS path, a rotation into every SOPS path that carried the old value,
keys removed from a SOPS file. They answer key names, lengths, equality and fingerprints, never a
value; `reveal` answers the configuration a SOPS file encrypts next to its secrets (ids, URLs, peer
names) once the classifier agrees it is no secret; `store` is the person's own, a once-shown value
from the clipboard or a download into a vault field; `page` is an agent's, the same value created in
the person's browser by a blind turn and taken into the vault without any session seeing it (see
[Secret operations](secrets.md#secret-operations)).

### `beekeeper sandbox render|install`

The agent sandbox: the policy Claude Code enforces on every session's commands, rendered as a
managed-settings drop-in, and the root command that installs it (see [The agent
sandbox](sandbox.md#the-agent-sandbox)).

### `beekeeper scan`

`beekeeper scan [index|add <ref>|sweep]`

The transcript value scanner: without a subcommand what the fingerprint index holds; `index`
rebuilds it from `scan.sops` and `scan.vaults`, `add` indexes one value from stdin, `sweep` counts
each reference and token rule in every transcript and names the files, never a value (see [What
reaches the model](threat-model.md#what-reaches-the-model)).

### `beekeeper hook sessionstart`

The SessionStart hook: writes the agent shell's prelude into the session's environment file
(`$CLAUDE_ENV_FILE`), which Claude Code sources before parsing each Bash command. It drops the vault
credentials (`OP_SESSION_*`, `OP_SERVICE_ACCOUNT_TOKEN`, `OP_CONNECT_TOKEN`) from the environment
and removes the aliases and shell functions of `agents.shell.unalias` (default `grep`, `find`, `ls`,
`cp`, `mv`, `rm`, the harness's own `grep` and `find` shadows among them), so each name runs the
tool on `PATH`, and with `agents.shell.globs: literal` (the default) an unmatched glob stays as
written instead of failing the command with zsh's `no matches found`.

The directories of `agents.shell.path` go first on `PATH`, in their order and once each: the agent's
own programs, such as a `gh` link to devctl that acts with the devctl App's short-lived token in
place of the person's long-lived `gh` login. Outside the agent sandbox it records on the session's
roster entry the `gh` its shell resolves then (so does `agents register`), and the watch says `GH
UNBROKERED` for an agent whose `gh` is not in one of those directories: it acts on the person's own
login, or not at all, instead of the App's token.

Through devctl's link a `gh` call acts with the App's token, which reaches the organisations the App
is installed on, unless it names a repository of another owner (`--repo`, a `repos/<owner>/…` path
of `gh api`, an issue or pull request URL, `GH_REPO`): that call runs on the person's own login. `gh
api graphql` and `gh search` name no repository, so a GraphQL query of a personal account's
repository answers `Could not resolve to a Repository` on the App's token while REST reads it; such
a query runs with `GH_REPO=<owner/repo>` naming it, and the PreToolUse hook writes that into a plain
`gh api graphql` read whose query names repositories of one owner. beekeeper's own GraphQL reads
(the board, `report`, `notes`) do that themselves: each request reads the repositories of one owner
and acts on one of them. The person's interactive setup stays theirs; an agent writes its commands
for the plain tools.

### `beekeeper lint briefs`

`beekeeper lint briefs <file or folder>...`

Refuses dated lines, "until X ships" clauses, notes on the release that fixed something,
workarounds, role run numbers and shell examples' bare `>` into a file in skills and briefs (every
Markdown file below a folder), one `path:line: rule: why` per finding, exit 3 on any.

### `beekeeper hook permissionrequest`

The PermissionRequest hook: answers `allow` only for a session `agents start` started in bypass that
now runs in `acceptEdits`; every other request gets no answer, so the person sees the normal card.
Below.

## Disk, memory and updates

### `beekeeper disk prune`

`beekeeper disk prune [--apply|--dry-run] [--report]`

Preview (the default, also `--dry-run`) or run the automatic disk step: the `disk.caches` policies
(beekeeper's own caches by default), finished worktrees, harness worktrees, external build output
and trash. The preview lists each cache with what it holds and would free; `--report` adds the user
cache directory's largest entries no policy covers.

### `beekeeper disk holders`

`beekeeper disk holders [--min-mib N]`

List the state home's holders (`~/.local/state` and its aside directories) by size and last write
with an owner read from the name, and say which are eligible for removal; removes nothing. The watch
says `STATE HOLDERS` while the eligible ones take `disk.holders.warnGiB`.

### `beekeeper disk usage`

`beekeeper disk usage [--min-mib N] [--budget DURATION]`

List what holds the disk by owner, largest first: each subdirectory of beekeeper's state, each
program's directory in the user cache (with its `disk.caches` policy, or none) and each worktree, a
checkout's harness worktrees included. Reads metadata only and removes nothing.

### `beekeeper worktrees`

`beekeeper worktrees [--apply] [--root DIR]...`

Preview or apply the legacy worktree doctor policy under `doctor.worktreeRoots`: remove finished
clean worktrees and trim regenerable build output from idle ones. A live or locked worktree stays.
The watch uses this policy with `disk.prune: ask`; `beekeeper disk prune` previews the default
automatic policy.

### `beekeeper cache clear go`

`beekeeper cache clear go [--modcache] [--wait 1h]`

Clear the shared Go build cache, and the module cache with `--modcache`, once no `beekeeper run`
holds it: it waits up to `--wait` for the runs, holds back new ones meanwhile, and logs a
`gocache.clear` event. Refused inside a `beekeeper run`. The hook refuses `go clean -cache`, `go
clean -modcache` and a delete of either cache, naming it.

### `beekeeper free`

`beekeeper free [--apply] [--only SECTIONS] [--summary]`

Show where the memory is and, with `--apply`, free what no running work needs: dead sessions' dirs
in the tmpfs `/tmp` (`session-dirs`: the CLI is gone, also a newer one of its desktop session, no
roster agent with its task or role holder is the session, the git worktree it works in is gone and
no process works or holds a file inside; an idle session's stay), orphaned go-build dirs
(`go-build`: unchanged for an hour, no go command running that began before their last change,
nothing held inside), throwaway temp dirs and orphaned jest or Claude workers; each section of dirs
ends with how many it frees and their size.

The watch frees `session-dirs` and `go-build` by itself every `doctor.freeEvery` (1h). Kind
clusters, idle CLIs, heavy or runaway processes and Chrome renderers are only reported. Each Claude
CLI is listed with its session's title and id, its roster state (busy, parked, idle), its role
(supervisor, guide, relay spare) and the hours since its transcript changed; one whose session is
untouched for `--cli-stale-hours` (12) with no role and not busy or parked is marked stale with the
memory its exit returns, and stays running.

Never runs as root: the swap reset and root-owned leftovers are printed as the commands to run.
`--summary` prints the TSV rows a desktop front end parses.

### `beekeeper self-update`

`beekeeper self-update [--version <tag>]`

Install the latest signed release, or an exact tag (including release candidates), over this binary;
`--check` only asks and works with `--version`.

## Conventions

Timer times accept `now`, a clock time, or a duration. The time and `--until`
accept duration units `w`, `d`, `h`, `m`, `s`, `ms`, `us` (or `µs`) and `ns`, including
combinations such as `1w2d3h`. Days are 24 hours and weeks are 168 hours:
`beekeeper timer add now "check readiness" --probe "true" --until 7d` starts
checking immediately with the same deadline as `--until 168h`.

Every reading command is written for an agent whose context is its scarcest resource: it prints only
what the caller has not read. `snapshot`, `sessions`, `lanes`, `agents` and `handover` keep a read
mark per caller and command (`seen.<command>.<caller>.json` in the state directory;
`snapshot.<caller>.json` for `snapshot`): a caller that has read before gets the facts that are new
or changed since, the keys of those gone (`gone: …`), or one `no change since HH:MM` line.

A moving figure (an age, a countdown, a memory size, a context in tokens) is no change. `--full`
prints everything, `--json` is always complete, `guide handover --prompt` is always whole (a
successor has read nothing), and a person at a terminal (no Claude session) always gets the whole
text. `watch` says each event once and each lasting condition when it starts and when it ends,
across restarts: it keeps its open conditions and one-time events (runaways, stale leases) in the
caller's mark (`seen.watch.<caller>.json`), so a watch restarted after an install resumes silently
and says only an `ENDED` or what is new; `watch --once` keeps no mark and says every condition it
finds. Its lines carry no restated thresholds or explanations, those stay in `--help` and here.

Marks are per host session (`CLAUDE_CODE_HOST_SESSION_ID`, else the session): a subagent or `claude -p`
worker inside a session shares its parent's marks, so its reads advance what the parent will see as
read. A subagent reading beekeeper passes `--full` or `--json`.

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor, the supervisor
loop not running), 4 relieved (`guide status` in the session a relay relieved), 69 the central
instance is unreachable (a central verb, see [The central
instance](remote-hosts.md#the-central-instance)), 125 a newer release is out (`self-update --check`).
A claim's first line is its outcome: `held by you since <time>` (exit 0), `queued: number <n> behind <holder>`
or `refused: <reason>` (exit 3); a kind lab's held claim adds its kubeconfig's export line after it.

A claim exits at once; `--wait <duration>` claims again every 10 seconds until it is held or the
duration has passed, in the foreground and at most 9 minutes: a background wait does not outlive a
desktop turn. A longer wait is a park: `--park` parks the calling agent (`agents park`) on a queued
claim, after its line, and the claim waits in its queue without a process; once the resource is free
and the agent's grant is first in its queue, the watch resumes the agent with the words to claim it
(below).

A queued claim no longer needed is withdrawn with `beekeeper lease release <resource>`, which ends
its park too. A claim gates the action it guards: `beekeeper lease claim staging -p "database
migration" && kubectl …`, never a `;` between them.

`--json` prints any command's result as JSON. A session is identified by the environment
Claude Code gives its tool commands, and named by its desktop title, the environment's name or
the name its CLI's record holds (a `claude --bg` worker's `-n` title); a person or a script
passes `--as <name>`.

Every session is a Claude Code CLI of its own: a desktop session, a `claude --bg` worker (its
daemon and terminal hosts are no session) or a headless `claude -p`. A session is listed under
the id and name its CLI's record names, the session the process runs now: a `--bg` worker the
daemon started or woke in its pre-started spare CLI, whose command line names no session, and a
resumed CLI that went on under a new id are listed as what they run; a spare no session has
claimed is not listed. A CLI that a session started, from its tool shell or through
`systemd-run`, is listed under its own id and name, `started by` that session, never as that
session restarting; a `claude -p` its tool shell runs without an id of its own is one of that
session's commands.
