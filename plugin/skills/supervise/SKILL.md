---
name: supervise
description: Run the desk's supervisor session — watch the other Claude sessions and the machine's memory, swap, load and labs for hours at a time (typically overnight), keep every session's work finishing, schedule new work from the roadmap board, prevent the desktop-wide OOM, and coordinate sessions that turn out to be working on the same repo, issue, PR or environment. Use when the person asks for a supervisor, a babysitter for the running sessions, an overnight watch, or when a session titled "supervisor" starts or a relayed `beekeeper handover --prompt` arrives.
disable-model-invocation: true
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
  "<agent>"`, `UPGRADE` and `UPGRADE ENDED`, `WATCH STALE` (re-arm that watch), and
  `ALERT NEW|RESOLVED|FLAPPING`. It runs as `beekeeper watch --notify`, so what needs the person
  also reaches their desktop, armed with a
  30-minute timeout and re-armed on every expiry: the expiry is the half-hourly tick.
  `ScheduleWakeup` is not a reliable tick; nothing depends on it.
- **The watch's own state is on disk, not in the transcript.** `beekeeper supervisor start` makes
  every lease claim need the supervisor's grant until a deliberate `beekeeper supervisor stop` or a
  successor's start; a crash of this session's CLI leaves the rule in force. A `yours <env>` said to
  the worker (SendMessage, `agents wake`) records its grant, `beekeeper lease grant <resource>
  <session>` records one without the word (grants queue in order), a hold with
  `beekeeper hold set <repo|github>`, a decision for the person with `beekeeper note add --for
  <person> --ref <owner/repo#n> --due <time> --status-quo "<what is true now>" --why "<why it needs them>" --default
  "<the action if unanswered>"` (every issue or PR as its full URL; `note add --help` names the checks; the question names its
  thread with `--ref`), the order of epics with `beekeeper priority` (the person's list, read
  never written here) and `beekeeper board rank`, a hold on an item with `beekeeper board block <item> --by
  <issue>` (the person's stop too: the issue that carries it), which keeps the item's Team, rank
  and Status, holds every issue under it and is released by the closing blocker without a session
  acting, a hold on a person (an answer, a review) with `beekeeper note add --for <person> --ref
  <item> --holds …`, which sets the item Blocked / Waiting and is released by the decision's answer
  or default without a session acting, a point in time to look at
  something with `beekeeper timer add <time> "<what>"` (`timer done` once looked at), what each
  session serves and waits on with `beekeeper sessions serve`, the registered agents with
  `beekeeper agents`. `beekeeper handover` prints all of it with the sessions; `beekeeper handover
  --prompt` is the successor's whole prompt. `beekeeper status` is the one-line view; `beekeeper
  supervisor` shows this session's context against `supervisor.relayAt`. Epics, plans, priorities and
  the person's answers live on the board, in the issue and in `beekeeper log --verb answer.recorded`; the supervisor keeps
  no note of them, and a state summary is the hand-over.
- **Other sessions** are visible through `beekeeper sessions` (what each is on, the commands it
  runs, how long a bounded wait keeps it reachable, overlaps, `CTX` and the last hour's turns,
  errors, GitHub calls and cost) and `beekeeper tail <session>` (its last turns); `ListAgents`
  gives the messaging address and busy / idle. `beekeeper budget` names every `gh` and `devctl`
  process with its session. `SendMessage` with `notify_when_idle` on a busy session gives one
  completion notice; on an idle one it fires at once and is noise.
- **The guide** is a separate session (`/guide`, `beekeeper guide`) that walks the person through
  their decisions and keeps the roadmap board current. It serves the notes filed for the person and
  the sessions waiting on them; its one-line copies of their answers reach this session, and the
  answers are in `beekeeper log --verb answer.recorded`. It does no work of its own: research, proofs
  and test sessions it meets come here as one line. `guide start` in this session is refused.

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
  own or a session's, is filed as `beekeeper note add --for <person> --ref <owner/repo#n> --due <time> --status-quo "<what
  is true now>" --why "<why it needs them>" --default "<the action if unanswered>"`; a note lacking
  one of them, a full URL or `--checked` for a state claim is refused, naming what is missing. At the deadline,
  the default for anything irreversible or production-facing is not to do it; a reversible default
  proceeds. An answer relayed by the guide is the person's word, followed without asking again.
- **Reading is a worker's task.** Board sweeps, epic re-queries and research run as workers; the
  supervisor reads their one-line results, never the boards, epics or transcripts in full itself.
- **Interventions are on the machine, not in the work.** Freeing memory, telling a session its pod
  OOMed, asking two sessions to coordinate, re-tuning the watch thresholds: yes. Fixing a session's
  failing test, merging its PR, re-running its CI, editing its branch: no, that is the session's.
- **Leave nothing running.** A relieved supervisor stops its Monitor and any scheduled loop,
  verifies with `ps` that no `beekeeper watch` of its own and no worker it started still runs, and
  leaves the role as it is. `beekeeper supervisor stop` ends supervision for everyone and runs only
  on the person's word. The end state is the person's summary.

## References

The rest of the role is in this skill's `references/` folder, each file read when its moment comes
and all of them at the first tick:

- `references/scheduling.md`: the five to ten busy agents (waiting ones hold no slot), the board,
  a worker for every ranked epic with open work, a fresh worker per task, the epics reported at a
  session's end. Read before filling free slots.
- `references/merges-and-collisions.md`: merges, holds, lanes and hand-overs by name, sessions that
  overlap. Read when sessions overlap, a lane stalls or a merge needs sequencing.
- `references/machine-and-alerts.md`: the numbers that matter, the installations in play and their
  alerts, the standby unit, consent boundaries. Read when the watch reports memory, an OOM kill,
  an alert or a page.
- `references/relay.md`: handing the watch on, after a crash or a reboot, a first supervisor. Read
  at `RELAY DUE`, in a headless turn and when starting the watch.
