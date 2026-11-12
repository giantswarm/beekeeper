# The guide and the desk

The guide is the session that walks the person through the decisions waiting on them.
It asks and relays; it never works itself.

## Questions go to the guide

A question put to the person with `AskUserQuestion` blocks the session until the person answers.
The PreToolUse hook therefore refuses it in every session, including the guide's. The guide asks
in conversation and keeps serving messages and its watch; other sessions file their questions
for the guide and carry on:

```
beekeeper note add --for <guide.person> --ref <owner/repo#n> "<the question, every issue or PR as its full URL>" --status-quo "<what is true now>" --why "<why it needs them>" --option "<choice>: <consequence>" --due "<when the default runs>" --default "<the action if nobody answers>"
```

The hook reads the state only for this tool and for
the calls below that do work, and a state it cannot read counts as not the guide.

## The guide asks and relays, it never works itself

The guide's job is to put decisions to its person one at a time and relay the answers word for word.
In the guide's session the hook refuses the calls that do work: `Edit`, `Write`, `NotebookEdit`,
`git commit` and `git push`, `devctl pr merge` and `release promote`, `gh pr merge` and `review` (at
a command position or inside a `-c` string), a GitHub connector tool that merges, reviews or pushes,
and a browser action (`claude-in-chrome`) other than opening, reading or looking at a page
(navigate, read, find, a screenshot or scroll).

The refusal says to file the work in that turn as an issue in its owning repository, put on the
board (`beekeeper board add <issue> --kind <kind>`; inspect with `beekeeper board show <issue>`),
since the supervisor loop is no session a `SendMessage` reaches, and to tell the person in one line,
never to hand the work back to them. What the person asks that the guide's own tools allow, it
carries out at once and reports. It is the escalation point for operational state that is stuck (a
hold, a pinned release, a parked session): it sets, narrows or lifts that state itself to carry out
what the person decided, then reports, and the refusal says so too. Questions, notes, `SendMessage`,
reads and `beekeeper` pass; other sessions are not affected.

The guide's questions carry the same context as `note add --for <person>`: the status quo,
why the decision needs the person, each option's consequences, the recommendation, deadline,
default and full issue or PR URLs. A claim of merged, green or released work includes its check.
Blocking dialogs are refused even when that context is complete.

`beekeeper guide next` serves one decision at a time: the open note for the person due first (by due
time, then the undated in filing order), marked as being asked. It serves the same note until it is
answered (`beekeeper answer --note`, `note answer`) or closed (`note done`); only then comes the
next. Only the guide's session serves decisions.

The guide keeps serving while a decision waits: it asks in conversation rather
than opening a blocking question dialog. The desk watch independently reports
`GUIDE WATCH LAPSED` when the guide feed has no recent heartbeat, and delivers
new decisions and findings without waiting for that feed to re-arm. New findings
become owned work before delivery; a failed delivery is retried.

`beekeeper guide work` lists the guide role's tasks, standing goals and temporary changes to
restore. Record them with `guide work add "<text>" --kind <task|goal|restore> --due <time>`. Close
completed or filed work with `guide work done <id> --reason "<result>"`; schedule an external wait
with `guide work defer <id> --due <time> --reason "<dependency>"`. Both forms of guide hand-over
include this work and configuration override restoration deadlines. The guide's Stop hook requires
`guide work check` in each turn and flags due tasks and restorations, so they cannot silently become
a list in an ended turn. Standing goals remain until explicitly closed.

## Claims across desks

Several desks (other machines, other configurations and boards, one GitHub user, no shared central
instance) can work issues of the same repository. A session record (`board next --claim`, the issue
an `agents start` brief names, `sessions serve`) is the desk's claim of its issue on GitHub as well:
one marker comment per issue and desk, which every desk reads.

```
<!-- beekeeper:claim desk=platform-desk session=0b5c1d2e-… at=2026-11-01T00:12:00Z seen=2026-11-01T06:12:00Z -->
Claimed by beekeeper desk platform-desk for session "0b5c1d2e-…" since 2026-11-01 00:12 UTC.
```

- The desk signs with `board.claims.desk`, a neutral name, never the machine's: the issue may be
  public. Unset, the desk publishes no claim and still honours the other desks'. The session is
  named by its id, which the claiming desk resolves with `beekeeper sessions`.
- The supervisor loop's pass and the watch keep the comments in line with the records, as do the
  commands that change a record: a new claim is a new comment (the desk's released one on that issue
  deleted), a claim that holds is edited in place, refreshed every `board.claims.refresh` (6h), and
  a record that ends (`agents idle --done`, `sessions unserve`, the session's end) releases it in
  place (`released=<time>`). One comment per issue and desk, never one per refresh. A pull request
  carries no claim.
- `board next` reads the claims of each pick in turn, up to the first item no other desk claims, and
  skips a claimed one (`claimed by desk <name> (session <id>) since <time>`). `--claim` publishes
  the desk's claim at once, waiting for a sync another process runs; should another desk's be older
  (the lower comment id, since every claim opens with a new comment), it gives its own back and
  exits 3 naming the winner. `sessions serve` over another desk's claim warns that two desks work
  the issue.
- `agents start` refuses a brief whose issue another desk claims, with that claim's desk, session
  and time (exit 3); `--force` starts it anyway and the event log records the claim overridden
  (`agents.start`).
- A claim unrefreshed for `board.claims.staleAfter` (24h) is stale: shown with the pick or the
  start, never honoured. A read GitHub fails is a warning (`claims unread`), the pick or start going
  on as if unclaimed. `board.claims.off` publishes and reads none.
- Reads go through REST with each thread's ETag cached in the state folder: an unchanged thread
  costs no rate limit.

## Work from repositories of different owners

One configured project can hold native issue items from accessible repositories of several owners.
Add an issue with `beekeeper board add <issue-url> --status "Up Next" --kind <kind>`, then place it
with `beekeeper board rank <issue-url> --top` (or `--before` / `--after`). GitHub's project
permissions decide whether it can be added; a refused add returns the permission error without
setting fields or ranking it. No second project or draft tracking item is needed.

`board next` reads those native items together. Priority is determined by issue
Status and order on the board only: Status order comes first, then position
within each status, regardless of repository owner. An unstaffed foreign epic
in In Progress offers its open sub-issues through the same epic gate as any
other epic. The supervisor loop starts the selected issue using the existing
capacity verdict and harness choice.

## Fresh successors (the guide)

Every run of the guide is numbered: "Guide run N", N one above the highest run recorded (the
state's run, the names of the holder, its relay and the holders it relieved, and the holder's
desktop title). `guide status` names the run. A start in a session not named as that run is the
next run: the session keeps the name its CLI takes messages under (its `-n` name), and a steward
sets its desktop title to the run's name. No session is kept in reserve or repurposed: a relay
and a crash both start a fresh session.

- **Relay:** `guide relay` starts "Guide run N+1" and opens the relay to it. Its brief has its
  first turn, which runs headless, take the role with `guide start` and end at once: a headless
  turn that arms a watch never ends, so the desktop never gets the session's CLI, and opening its
  row would start a second CLI on the same session. The reopen after that turn warms its desktop
  CLI, and the standby watch sees the holder's CLI back under a new PID and sends it `beekeeper:
  your CLI restarted (…) and its watch is gone. Run beekeeper guide handover --prompt and follow
  it.` (`GUIDE RESUME`).
- **Crash or CLI exit:** once the holder's CLI has been gone past its `restartGrace` with no
  relay open, the standby watch starts the next run the same way, with the relay from the gone
  holder, once: the open relay keeps later polls and a restarted watch from starting another.
  `GUIDE GONE` says so, and `GUIDE SUCCESSOR` (`GUIDE SUCCESSOR FAILED`) says how the start went.
  A session whose headless turn of beekeeper's start or wake runs is not gone; the reopen after
  that turn, which waits while the desktop's window has the focus (up to 25 minutes), is no turn:
  the `restartGrace` covers it.
- **A successor that does not come up:** a successor starts in `guide.dir`, else in the folder
  its predecessor's desktop session started from: never in the worktree the desktop made for
  the predecessor. A successor whose first turn ended with no desktop CLI past the
  `restartGrace` is not replaced: the standby watch resumes it headless with the same message,
  and that turn keeps the role's watch and its CLI. One whose resume never ran did not come up
  (`GUIDE SUCCESSOR DOWN`): the first is one note for `guide.person`, the next successor starts 5
  minutes later, the one after 10 minutes, and after three none starts until a holder runs again.
- **Stopped workers:** a registered agent with a task whose CLI does not run (a `claude --bg` worker
  a reboot stopped, a desktop session closed) does not come back by itself. `watch` says `AGENTS
  STOPPED` once per agent, and the `handover` Agents section marks it, each with how to resume it:
  `claude --bg --resume <session> "…"` for a background worker, its `claude://code/continue` link
  for a desktop session, or `beekeeper agents wake <name> <message>` for codex, omp and agy workers.

  Those workers count as running while their harness runner lives, including between turns and
  before their first transcript. Nothing is resumed automatically: a burst of resumed workers after
  a login is the supervisor's call against the machine's memory. The line tells a worker parked on a
  person (its serve record's `--waits` names the person, a note, the supervisor or the guide:
  `parked on a person: …`) apart from one whose headless turn ended on a background wait again after
  the reopen resumed it once (`ended on a wait again after its resume at …`).
- **Review headroom:** `capacity.reviewHeadroom` (default 2, zero disables it) reserves
  extra subscription sessions above a positive worker ceiling for `--review` starts, and as
  many above a positive `capacity.local.ceiling`.
  Reviews run before new workers within a supervisor pass. Memory, swap, load, lab
  and build guards, usage window stops and weekly pace still apply; disk holds a
  review only under half of the worker floor, since a review builds little and the
  watch prunes from the full floor on. `capacity` and
  its JSON show the reserve, occupancy above the worker ceiling and busy sessions.
  Filing findings starts no filer session.
- **Capacity:** the supervisor keeps up to `capacity.ceiling` agents busy (10 when unset; a ceiling
  of 0 refuses every unforced start with `no start: ceiling 0`, and does so for the subscription
  providers alone: a `capacity.providers` entry of `kind: self-hosted`, a model served on the desk's
  own machines, keeps starting on its own ceiling of sessions at once). Busy is a roster agent with
  a task that is neither parked, kept nor waiting: an agent kept on purpose (`agents keep`), one
  parked on a decision (`agents park`), one a timer wakes and one whose session waits on its person
  are parked; one whose session runs only waits (a `sleep` or a polling loop's, `gh run watch` or
  `--watch`, `devctl pr wait`, `pr merge` or `release wait`, `beekeeper gate` or `lease … --wait`)
  and whose transcript and its subagents' have been quiet for 5 minutes is waiting, listed apart
  (`waiting: <agent> (waits: <command>)`, `--json` `waiting`).

  Parked and waiting agents do no work and never hold a slot: only the busy count against the
  ceiling, in `capacity`, the start gate and the watch's CAPACITY lines. The supervisor, the guide
  and a role's successor are not counted. `capacity.local.ceiling`, when set, caps the busy agents
  the coordinator's own machine runs apart from `capacity.ceiling`, which bounds the subscriptions
  across the desk and its execution hosts: every busy agent whose session runs locally counts, a
  self-hosted provider's included; an agent on an execution host counts against that host's
  `maxBusy` and never against it.

  Every local start, the loop's and one by hand, worker and resume alike, is refused at it (`no
  start: N busy locally at capacity.local.ceiling N`); a review, which runs only on the coordinator,
  may use `capacity.reviewHeadroom` above it as above the worker ceiling (`… plus review headroom
  N`), the machine's guards still applying. `capacity` prints it as its `local:` line (`--json`
  `local`). Unset, there is no local cap; 0 refuses every unforced local start.

  `beekeeper capacity` (and `--json`) prints the count, the headroom that bounds a new start
  (MemAvailable against `capacity.availMinMiB`, disk swap's growth over the watch's readings of
  `watch.swapWindow` against `capacity.swapGrowthMaxMiB`, never the swap in use, the 1-minute load
  against the watch's HIGH LOAD limit (`watch.loadMax`, or `watch.loadPerCoreMax` × cores), the kind
  labs against their cap, and for a start that builds the free build slots) and one verdict, `room
  for N starts` or what blocks one, each guard that refuses named; it reads, changes nothing and
  makes no GitHub call.

  Disk swap's growth is weighed with memory pressure and MemAvailable, the three figures in its
  line: over the max it blocks a start under pressure (memory PSI, full avg60, over
  `capacity.swapPSIMax`, or MemAvailable under the floor; either unknown counts as pressure) and is
  a warning in parentheses after the verdict without (`room for 7 starts (disk swap growing +391
  MiB/h over 256, without pressure: memory PSI 0% (max 0%), MemAvailable 43.0 GiB (floor 20 GiB))`):
  swap may sit full for days without a byte moving, and the window lets a single burst leave the
  rate.

  Disk swap's growth counts new swap-outs only: the swap in use less zswap's share (`SwapTotal` −
  `SwapFree` − `Zswapped` of `/proc/meminfo`) less zswap's writeback (`zswpwb` of `/proc/vmstat`,
  the pages zswap moves from its pool to the swap device, which grow disk swap with swap in use
  flat). The writeback is shown apart, never weighed: the headroom line says `disk swap N MiB +G
  MiB/h (max M), zswap Z MiB writing back W MiB/h`, and the watch's SWAP line `disk +G MiB/h, zswap
  writeback W MiB/h over the last 10m`.

  The build slot guards only a start that builds: `capacity --builds` and `agents start --builds`
  mark a task that builds, tests or lints (its commands take the slot under `beekeeper run`), and
  such a start, refused by a busy slot alone, waits its turn in the build slot queue with the runs,
  first in first out, rather than being refused while run after run holds the slot. A start without
  the mark does not build (a filer, a dispatcher, a reviewer, a promoter proving a release
  read-only) and has no use for the slot: it is judged on memory, swap, load and the labs alone, and
  a busy slot is a note in parentheses after its verdict, never a `no start`, so the machine's one
  slot held by a worker's compile refuses no start that only reads.

  `watch` weighs a start that does not build. `agents start` refuses a start whose verdict has no
  room (exit 3, the guards named, no session created) unless `--force`, which starts it and records
  the verdict overridden (`agents.start` in the log). `watch` says `CAPACITY FULL <busy> of <ceiling>`
  at a positive ceiling, with the headroom line and one `ENDED` line when the count recovers; a
  ceiling of 0 suppresses `CAPACITY FULL`. The `handover` carries the count, `status` shows `busy <n>/<ceiling>`.

  When `capacity.usage.command` is configured, beekeeper runs that command (bounded by
  `capacity.usage.timeout`, default `15s`) to read the subscription's 5-hour and 7-day usage windows
  as JSON on stdout (`{"fiveHour": {"usedPercent": 42.5, "resetsAt": "2026-10-27T23:00:00Z"},
  "sevenDay": {"usedPercent": 61.0, "resetsAt": "2026-11-01T12:00:00Z"}}`, either window optional
  unless its bands or `highPercent` are configured), caches the reading in `usage.json` for
  `capacity.usage.every` (default `5m`, re-reading at once once a window's `resetsAt` passes), and
  derives `ceiling` from the ascending `{upTo, ceiling}` bands in `capacity.usage.fiveHour` and
  `capacity.usage.sevenDay` (taking the lower `ceiling` when both windows have bands, and `0` at or
  past a window's last `upTo`).

  `beekeeper capacity` appends `; usage 5h …, 7d …` to its `capacity:` line and includes `usage` in
  `--json`; `beekeeper snapshot` prints `usage: …` (`--json` `usage`); `beekeeper watch` prints
  `USAGE: …` every `capacity.usage.reportEvery` (default `30m`, negative turns periodic lines off,
  `watch --once` prints once). Once `fiveHour.usedPercent` reaches `capacity.usage.highPercent`
  (default `95`), `watch` emits `USAGE HIGH` once (`ENDED USAGE HIGH` once reset), tells each
  running busy worker once per window episode to finish its current step and park, and gates
  `capacity`, unforced `agents start`, unforced `agents resume` (`--force` overrides) and `watch`'s
  `autoResume` until `fiveHour.resetsAt`.

Capacity choices may set a session `effort`, independently for each harness:

```yaml
capacity:
  choices:
    - {harness: codex, model: gpt-6-astra, effort: high}
    - {harness: agy, model: example-model}
agents:
  models:
    - {match: '.*', kind: review, harness: codex, effort: high}
```

An explicit `agents start --effort` wins, followed by the first matching effort
rule, then the selected capacity choice's effort. A rule's optional `harness`
limits its effort; its model selector remains independent of harness selection.
Unscoped rules retain their existing behavior. Local and SSH starts resolve the
effort after choosing the harness, including dry runs and quota retries. Codex
checks the selected model's `supported_reasoning_levels` in `models_cache.json`
beside `codex.configFile`; missing, unreadable or unknown model metadata refuses
the start with a catalog diagnostic. Refresh the catalog with Codex before
starting with an effort setting. agy choices must omit effort. Starts record the
resolved effort, and resumed sessions retain it.

  `capacity.providers.claude: {}` defaults to `[claude, -p, /usage, --output-format, json]`. This
  local command uses no model tokens and needs no credential reader or status line. It runs under
  beekeeper's state directory at the provider's `every` (default `5m`), with its `timeout`. `Current
  session` supplies the 5-hour window and `Current week (all models)` the 7-day window; without a
  `Current session` line (an idle 5-hour window) the 5-hour window reads 0% used with no reset, and
  only a missing `Current week (all models)` fails the reading; per-model weeks are retained as
  supplemental buckets.

  Reset times use the timezone printed by Claude and the next occurrence of the date or time. An
  API-key login reports `no plan windows` with no subscription room, cached at the same interval; an
  unparseable window fails with its raw line. Recently elapsed resets retain their printed time
  rather than extending the old window. An explicit `command` continues to override this default.
  Only the bare default command is parsed as Claude's local `/usage` output; custom readers
  (including absolute executable paths) must print beekeeper's usage JSON.

  `capacity.providers.agy` without a `command` (even `agy: {}`, or one with only a `ceiling`)
  defaults to `[agy, -p, /usage]`, agy's local usage report, which takes no model turn. Like
  Claude's, it runs under beekeeper's state directory (`usage/agy`) at the provider's `every`
  (default `5m`), with its `timeout`, without the requesting agy worker's `BEEKEEPER_AGY_AGENT` and
  `BEEKEEPER_AGENT_NAME`, and fails closed once older than `staleAfter`. Each model bucket's `Five
  Hour Limit Remaining` and `Weekly Limit Remaining` lines become that bucket's 5-hour and 7-day
  windows (used = 100 − remaining) in `buckets`; a bucket printing only one of them is left out, so
  a choice on it gets no start while the complete buckets still count, and a report with no complete
  bucket is a failed read.

  A desk needs no timer of its own to feed `beekeeper capacity`. An explicit `"fiveHour":
  {"usedPercent": null, "resetsAt": null}` means no short window is open yet and counts as 0% used;
  weekly usage and pace still apply. Missing fields and unknown weekly usage remain errors. A
  `capacity.providers` entry of `kind: self-hosted` is a model served on the desk's own machines
  rather than a subscription: it sets no usage command and has no pace, only a `ceiling` of busy
  sessions at once (unset, 10), which `capacity.ceiling` never bounds — a desk that lowers
  `capacity.ceiling` to 0 to hold a subscription on its weekly pace still starts on its own model,
  with the machine guards as the only further bound.

  Its ceiling may follow the serving mode of the model it serves: `serveModeCommand` prints the mode
  on stdout (cached for `serveModeStaleAfter`, default `1m`) and `serveModeCeilings` names the
  ceiling per mode, falling back to `ceiling` while the mode names none or the read failed, which
  the line says as `serving mode unknown (…)`. The provider's line reads `provider <name>: <n> busy
  of ceiling <c>; serving mode <mode>; <verdict>` and `--json` carries `kind`, `servingMode` and
  `servingModeError`.

  Busy agents on a model of that provider count against its ceiling, `beekeeper capacity` and
  `--json` show both figures, and `agents start --harness omp` or `--harness auto` judging a start
  on it is judged against that ceiling. Unlike a subscription provider, it never earns a USAGE HIGH:
  only its ceiling and the machine guards stop a start. With `serveModeModes` each mode names the
  machine's nodes it occupies and the sessions it sustains (the ceiling of the mode per `sessions`),
  and `serveModeSwitchCommand` is the platform's own tool that switches into the mode named as its
  last argument (bounded by `serveModeSwitchTimeout`, default `1m`): beekeeper relays to it and
  holds no control of the model itself.

  A node a mode occupies is then a leasable resource, and `beekeeper lease claim <node>` the generic
  constraint "needs node N exclusively": while a constraint holds a node, the model never runs in a
  mode that occupies it — the claim lowers the provider's ceiling to the widest mode free of the
  node at once, drains it (no new session starts on the model; running sessions finish or park),
  runs the switch, and only then grants the lease; `lease release` switches back and raises the
  ceiling again once no constraint needs the node.

  One switch runs at a time (a machine-wide lock), never from a session that runs on the model
  itself, and the watch retries a switch a drain or a failure left pending. `beekeeper status`
  carries `serving mode <mode> (<n> sessions)` and each held node with the mode that stands while it
  holds; the event log carries `serve.drained`, `serve.switch` and `serve.refused`. Every failure
  leaves the mode as the reader reports it: a refused or failed switch keeps the mode, says so in
  the line and the log, and stays pending for the watch.

The command-line send is one headless `claude -p` turn whose only tool is SendMessage, addressed
by the name ListAgents shows (the session's title): Claude Code has no send command, and its
peer messaging is not a documented interface, so it is isolated in `internal/peer`.
Its turn's peer socket (`uds:…`) ends with the turn, so `agents wake` heads a message from a
roster agent or a role's holder with the name it is answered by (`[from "<name>": reply by
SendMessage to "<name>", …]`), and the PreToolUse hook refuses a `SendMessage` to a peer socket
that no longer exists with that hint. A `SendMessage` by the roster name of an omp, agy or codex
agent, which no SendMessage reaches, is written to that agent's inbox by the hook, which then
refuses the call itself with where the message went.

`beekeeper status --bar` prints one row for a desktop bar (waybar, polybar, i3blocks), the same
shape as the rows of `free --summary`, so one bar module reads both. The contract is fixed: six
tab-separated fields, always present, in this order.

| Field | Value |
|---|---|
| 1 | `beekeeper`, the row's key |
| 2 | `loop` while the supervisor loop runs; `!loop` when it does not |
| 3 | the number of leases held |
| 4 | the number of holds in force |
| 5 | the number of notes and timers whose time has come |
| 6 | the busy agents against `capacity.ceiling`, `<n>/<ceiling>` |

## Guide report ownership

Guide reports use a shared durable delivery record. The first consumer to retain a report owns its
delivery; the other consumer respects the same record. Re-arming the foreground watch does not
replay reports already retained by either path. Recovery reports close their matching delivery work
without creating a new task, including episodes raised and recovered between polls. Refill, review
and merge-decision stalls raise one alarm per episode.

When filing or recognizing an existing issue for a report, record its mapping:

```sh
beekeeper guide work done <id> --reason "Filed the defect" --issue https://github.com/owner/repo/issues/123
```

An issue URL in the completion reason also records the mapping, preserving the existing guide filing
workflow. Matching report bodies then increment that issue's local observation tally, visible in
`beekeeper guide work`, without creating more due work. Reporter and observation-time headers do not
affect the fingerprint. Different report bodies remain new findings. Once the issue is resolved, use
`beekeeper guide work unmap https://github.com/owner/repo/issues/123` to allow subsequent
observations to raise new work. Mapping lifecycle is explicit; the watch does not poll GitHub for
every repeated report.

A guide watch re-executes the installed executable after replacement by an update
or rollback, preserving its PID, arguments and durable delivery records. It checks
for replacement before consuming the next feed. A failed re-execution is reported
once with manual recovery instructions.
