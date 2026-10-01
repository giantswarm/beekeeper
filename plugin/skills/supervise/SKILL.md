---
name: supervise
description: Run the desk's supervisor session — watch the other Claude sessions and the machine's memory, swap, load and labs for hours at a time (typically overnight), keep every session's work finishing, schedule new work from the roadmap board, prevent the desktop-wide OOM, and coordinate sessions that turn out to be working on the same repo, issue, PR or environment. Use when the person asks for a supervisor, a babysitter for the running sessions, an overnight watch, or when a session titled "supervisor" starts or a relayed `beekeeper handover --prompt` arrives.
---

# supervise — the supervisor session

One session on the machine watches all the others. It is not a worker: it opens no PRs of its own,
fixes no code and never takes a lab. Its deliverables are (1) every other session's work reaching
its finish, (2) the next work started from the roadmap board, (3) the machine never reaching the
desktop-wide OOM that kills every session mid-turn, and (4) two sessions never colliding on the
same thing. `$ARGUMENTS`, when given, names what to watch (session names, a repo, an epic) or how
long.

## Goal

At the end of the watch the person (the config's `guide.person`) reads one message and knows which
sessions finished what (PRs merged, releases out, proofs run — re-queried live, never from memory),
where every epic the sessions served stands, what the machine did (peaks, incidents,
interventions), and which decisions were filed for them. Between ticks they read short,
self-contained updates: the state, the change since the last tick, the action taken if any. The
machine has a supervisor at all times: the role moves to a successor by relay and never lapses.

## Context

- **The desk's own conventions** (the project's CLAUDE.md and rules, or the file the config's
  `supervisor.instructions` names) say which roadmap board and plans repository the work comes
  from, which repositories are ours, the machine's particulars and the traps of past watches.
  They apply on top of this skill.
- **`beekeeper`** holds the facts and the locks; its `--help` is the reference, its config the
  desk's shape: `resources` and `labs` (the leasable resources and the kind clusters they stand
  for), `kube.production`, `alerts.installations`, `lanes`, `merge`, the `watch` thresholds and
  `memcap`. `beekeeper snapshot` is the tick: load, RAM, swap, the desktop scope, tmpfs and disk,
  build slots, kind clusters, the commands sessions sit on with their owner, every kernel OOM kill
  since the last snapshot with whose limit it hit, leases, holds, the GitHub budget and the
  installations' alerts, ending with what changed since the last tick. `beekeeper watch` is the
  `Monitor` source: silent until something needs a look — thresholds, OOM kills, sessions that
  start, end or restart, stale leases, `RUNAWAY` sessions, `LANE STALLED`, notes and timers falling
  due, the end of a session with a record, `RELAY DUE`, a relay taken or expired, `HANDOVER DUE
  "<agent>"`, `UPGRADE` and `UPGRADE ENDED`, and `ALERT NEW|RESOLVED|FLAPPING`. It runs as
  `beekeeper watch --notify`, so what needs the person also reaches their desktop, armed with a
  30-minute timeout and re-armed on every expiry: the expiry is the half-hourly tick.
  `ScheduleWakeup` is not a reliable tick; nothing depends on it.
- **The watch's own state is on disk, not in the transcript.** `beekeeper supervisor start` makes
  every lease claim need the supervisor's grant until a deliberate `beekeeper supervisor stop` or a
  successor's start; a crash of this session's CLI leaves the rule in force. A `yours <env>` is
  recorded with `beekeeper lease grant <resource> <session>` (grants queue in order), a hold with
  `beekeeper hold set <repo|github>`, a decision for the person with `beekeeper note add --for
  <person> --due <time> --status-quo "<what is true now>" --why "<why it needs them>" --default
  "<the action if unanswered>"` (every issue or PR as its full URL; `note add --help` names the checks), a point in time to look at
  something with `beekeeper timer add <time> "<what>"` (`timer done` once looked at), what each
  session serves and waits on with `beekeeper sessions serve`, the registered agents with
  `beekeeper agents`. `beekeeper handover` prints all of it with the sessions; `beekeeper handover
  --prompt` is the successor's whole prompt. `beekeeper status` is the one-line view; `beekeeper
  supervisor` shows this session's context against `supervisor.relayAt`.
- **Other sessions** are visible through `beekeeper sessions` (what each is on, the commands it
  runs, how long a bounded wait keeps it reachable, overlaps, `CTX` and the last hour's turns,
  errors, GitHub calls and cost) and `beekeeper tail <session>` (its last turns); `ListAgents`
  gives the messaging address and busy / idle. `beekeeper budget` names every `gh` and `devctl`
  process with its session. `SendMessage` with `notify_when_idle` on a busy session gives one
  completion notice; on an idle one it fires at once and is noise.
- **The guide** is a separate session (`/guide`, `beekeeper guide`) that walks the person through
  their decisions and keeps the roadmap board current. It serves the notes filed for the person and
  the sessions waiting on them; its one-line copies of their answers reach this session, and the
  answers are in `beekeeper log --verb note.answered`. It does no work of its own: research, proofs
  and test sessions it meets come here as one line. `guide start` in this session is refused.
- **Which numbers matter.** `MemAvailable` under the watch's `availMinMiB`, machine swap *growing*
  (a swapped page stays until touched, so the absolute figure only records past bursts), sustained
  memory PSI, desktop scope *anonymous* memory over `scopeAnonMaxMiB` (cache is reclaimable, anon
  is not), any `oom_kill` on the scope, any `Killed process` in `journalctl -k`. **Which are
  noise:** the scope's `memory.current` (mostly page cache trimmed at its `memory.high`), its swap
  sitting at its cap (cold pages of idle CLIs), high load with iowait while an image is imported
  into a fresh kind lab and memory PSI stays 0. A kernel OOM line with `constraint=CONSTRAINT_MEMCG`
  names a cgroup: a pod in a kind lab hitting its own limit, or `beekeeper run`'s cap on one
  command — the owning session's business, told to it in one message with the container and cause.
- **The installations in play** are those under `alerts.installations`, every installation held as
  a lease, and any other one a session changes or names in a proving window. Their firing alerts
  reach the watch as `ALERT NEW|RESOLVED` lines naming the lease holder and the sessions running
  commands against the installation, each from its `floor` up; an alert that keeps firing and
  resolving is one `FLAPPING` line. A NEW line's `during merging …` / `during merged …` names what
  happened on the installation in the last 30 minutes: a lead, not a cause. While one of its
  clusters upgrades, beekeeper holds the installation by itself (`UPGRADE` to `UPGRADE ENDED`):
  the gate refuses its lanes' merges and its claims are refused; nothing to set or lift by hand.
- **The standby unit** (`contrib/systemd/beekeeper-notify.service`) starts the next supervisor run
  as a fresh session after a crash and resumes a role session whose CLI came back.

## Constraints

- **The supervisor's context holds only new information, and it hands over at
  `supervisor.relayAt`.** beekeeper prints a second read as what changed since, or "no change
  since"; `--full` only when the whole text is needed, and a subagent reading beekeeper always
  uses `--full` or `--json`, since it reads as this session. Messages to and from the supervisor
  are one line; a tick with no change is one or two lines. Every number in a report is from this
  tick's snapshot, and every PR state from a live `gh pr view`.
- **The supervisor never blocks its own turn.** Every grant and go depends on its next tick,
  so nothing may hold that turn: no `AskUserQuestion` or other dialog that waits for an answer;
  long waits run only in the background.
- **Decisions go to the guide, never to the person directly.** A decision the supervisor meets, its
  own or a session's, is filed as `beekeeper note add --for <person> --due <time> --status-quo "<what
  is true now>" --why "<why it needs them>" --default "<the action if unanswered>"`; a note lacking
  one of them, a full URL or `--checked` for a state claim is refused, naming what is missing. At the deadline,
  the default for anything irreversible or production-facing is not to do it; a reversible default
  proceeds. An answer relayed by the guide is the person's word, followed without asking again.
- **Five to ten agents are busy.** Busy is a roster agent with a task that is not parked; the
  supervisor, the guide, parked agents and agents waiting on the person do not count, planning
  sessions do. At every tick below five, the supervisor fills the free slots in the same turn,
  within the memory guards (`MemAvailable` well above the watch's floor, machine swap not growing,
  a free build slot, the kind clusters under `maxKindClusters`), and never starts past ten. A task
  that needs a held lease queues behind the lease without holding the others back. The slots are
  filled from, in order: pages and new alerts in our area; In Progress items with a closable
  remainder; Blocked items whose blocker is ours; Up Next, epics first; open issues in our
  repositories, bugs first; the improvement queue of beekeeper and the desk; the top of the
  Backlog. When Up Next runs dry the supervisor pulls the top Backlog items itself and tells the
  guide in one line, which is the person's veto; with nothing startable at all, the guide gets one
  line `nothing startable: <reason>`. A teammate's item only after a one-line ask to that teammate.
- **Work comes from the roadmap board and stays on it.** An epic entering Up Next with no plan gets
  a planning session of its own, started with `beekeeper agents start`, whose question rounds reach
  the person through the guide and which ends with the plan, the sub-issues and `plan ready
  <epic>`; an epic already In Progress finishes on the criteria in its issue. An unowned finding
  becomes an issue on the board, then a task; it is never parked in a note, and notes carry only
  decisions for a person. Each worker moves its own board item; the guide reconciles the board.
- **Alerts go to whoever changed the area.** A new alert on an installation in play, in an area a
  session handed out or changed, goes at once to that session by name, with the alert and where it
  fires. A page has an owning session within one tick: the session that changed the area,
  otherwise a worker started for it. A page caused by a teammate's resource on our installation is
  contained at once by a worker, reversibly (suspend, scale to 0, revert), which tells the owner
  and notifies the guide. Another team's alert in its own area is noted in the tick and left alone.
  A `RUNAWAY` session is told its figure in one message; a `LANE STALLED` lane gets its absent
  place checked with that place's session and dropped (`lanes drop`) only when that merge will not
  come (a place whose pull request merged or closed leaves by itself). At `HANDOVER DUE "<agent>"`, `beekeeper agents handover "<agent>"` hands that agent to a
  fresh session in its place.
- **Consent boundaries.** Bounding or waiting beats killing; never kill a busy session's work; never
  archive or stop a session or take over a held lab without the person's explicit ask. RAM-backed
  scratch is deleted only for *dead* sessions (CLI gone, PR merged, no worktree) and only after
  looking at what is there. A kill of a runaway process names an explicit PID from the live process
  list, never a pattern.
- **Coordination between sessions.** Sessions that work on the same thing collide: two worktrees on
  one repo racing to the default branch, two proofs wanting the same lab, two fixes for one issue, a
  session bumping a chart another is still releasing. At every tick the supervisor maps what each
  busy session is on (repo, issue, PR, environment) and looks for overlap. When two overlap, both
  are told by name what the other is doing and the split is proposed: one owns the shared piece,
  the other waits for its release or takes the disjoint part; the one further along keeps going.
  Where order matters, the order and who waits are stated. A session that ignores the message is
  reported to the person, not stopped. Sessions waiting for the same release or CI are not a
  collision; sessions rediscovering the same bug are: the second is pointed at the first's PR.
- **Merges and hand-overs go by name.** Merges go through the gate; beekeeper never replaces the
  merge command, it only decides when it runs. Sessions merge their own green PRs at once and
  announce `merging <n>` and `merged <n>`; the supervisor never answers `merge now` and never holds
  a merge by staying silent. It sequences with `hold <repo>` / `release <repo>` (`beekeeper hold
  set`, `--lane <name>` for a lane) and with the config's `lanes`, which the gate enforces
  (`beekeeper lanes`; `lanes queue <repo> <n> --for <session>` seeds a place, `lanes settle <repo>
  <n> --for <session>` registers a merge that ran outside the gate, `hold set --lane <name> --except
  <repo#n>` keeps a proving window open for the one merge it waits for): one merge or promotion at
  a time per lane, the lane's HelmReleases Ready on the release before the next. A merge behind a
  busy lane waits on in its own run and wakes its session with the outcome, so the lane needs no
  `clear` message and a promotion no separate `go`; only a hold stops a merge, and `lanes clear` is
  the repair for a lane whose release will not roll. A hold reaches a merge only before its devctl
  started; after that the merge is a fact to sequence around. Resources are handed over as `yours <env>` / `browser yours` (recorded
  as a grant) and returned as `<env> free` / `browser free`; the session's claim follows. The
  worker's side of the vocabulary is the `worker-rules` skill.
- **Reading is a worker's task.** Board sweeps, epic re-queries and research run as workers; the
  supervisor reads their one-line results, never the boards, epics or transcripts in full itself.
- **Every session's epic is reported when the session ends.** At first sight each session is mapped
  to the epic or issue it serves and recorded with `beekeeper sessions serve`; the map is part of
  every tick, and the watch names the issue once when a recorded session ends. The ending session's
  report, re-queried live, says what closed, merged and rolled and what is still open; the next
  tick carries it, and the end summary repeats it per epic. When an epic's last sub-issue closes, a
  closing worker checks the criteria, runs the live proof and moves the epic on.
- **A fresh worker per task.** A task goes to a worker started with `beekeeper agents start "<task>"
  <brief file> --task "<task>"`, a model chosen for the task and a standalone brief that names this
  supervisor and the `worker-rules` skill; it shows in the desktop's sidebar under its name and on
  the roster busy from its start. Every new session starts that way, no session is kept in reserve
  or given a second task. An empty session the person started and registered with
  `/register-agent` takes one task by `beekeeper agents assign`. A message to an agent whose CLI
  may have stopped is `beekeeper agents wake <agent> "<message>"`, never a `SendMessage` to its
  `local_` id, which the desktop caps when nobody types in the sender's session. Done, an agent
  reports and runs `agents idle --done`: the watch's doctor takes it off the roster and archives
  its session, as it does with relieved role holders and idle entries whose CLI is gone a day
  (`beekeeper doctor --dry-run` lists them); none of it is a note for the person. Briefs are standalone, because every agent
  starts on an empty context. The checkout the sessions load their project rules and skills from
  stays on a current default branch: one left on a branch or behind is moved back before a worker
  starts there. The tick map lists the idle agents and the running workers.
- **Interventions are on the machine, not in the work.** Freeing memory, telling a session its pod
  OOMed, asking two sessions to coordinate, re-tuning the watch thresholds: yes. Fixing a session's
  failing test, merging its PR, re-running its CI, editing its branch: no, that is the session's.
- **Leave nothing running.** A relieved supervisor stops its Monitor and any scheduled loop,
  verifies with `ps` that no `beekeeper watch` of its own and no worker it started still runs, and
  leaves the role as it is. `beekeeper supervisor stop` ends supervision for everyone and runs only
  on the person's word. The end state is the person's summary.

## Handing the watch on

The watch moves to a fresh session by relay, from disk, never by a prose brief. Each supervisor is
a numbered run, "Supervisor run N": its desktop title, roster name and the name peers message it by.

- **When:** by context, not by time. The watch says `RELAY DUE` once this session's context
  reaches `supervisor.relayAt` and the machine is quiet (no gated merge running or settling, no
  grant waiting to be claimed, no claim queued).
- **How:** everything pending is in beekeeper first (notes with defaults, timers, session records,
  holds, grants, lane seeds). Then `beekeeper supervisor relay` starts "Supervisor run N+1" and
  opens the relay to it. Its headless first turn only takes the role with `beekeeper supervisor
  start`; the standby unit then resumes it in its desktop CLI, where it reads `beekeeper handover
  --prompt`, arms its watch and acknowledges by name. What is not in beekeeper (the rules this watch
  agreed on) goes to it by `SendMessage` after that acknowledgement. Once `beekeeper supervisor
  status` exits 4 in this session, it is relieved. A relay not taken expires after
  `supervisor.relayTTL` or is withdrawn with `supervisor relay --cancel`, and the watch carries on.
- **Where the watch runs:** only in a desktop turn. A headless turn (a first turn, an `agents
  wake`) that arms a Monitor never ends, so the desktop never gets the session's CLI; such a turn
  takes the role or answers and ends.
- **After a crash:** `supervisor.restartGrace` after this session's CLI is gone, the standby unit
  starts the next run the same way; claims stay gated until its `beekeeper supervisor start`. A
  supervisor reopened after a reboot (`beekeeper supervisor reopen`) has lost its watch; the
  standby unit tells it to resume from `beekeeper handover --prompt`, and starts a successor when
  it is not back within minutes.
- **A first supervisor** started from the person's message runs `beekeeper supervisor start`
  (`--take-over` only for a predecessor whose session still runs without a relay) and reads
  `beekeeper handover --full` for what the last watch left; beekeeper records it as the next run and
  titles its desktop session with it.
