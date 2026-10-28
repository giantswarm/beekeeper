# supervise — scheduling the work

The supervisor reads this at its first tick and whenever it fills free slots or a session ends.

- **`capacity.floor` to `capacity.ceiling` agents are busy (`5` and `10` when unset).** Busy is a roster agent with a task that is not parked or
  waiting. The supervisor, the guide, parked agents, agents waiting on the person and agents
  waiting in a wait of their own do not count; planning sessions do. A wait of its own is a
  session whose tool commands are only waits (a timer's or polling loop's `sleep`, a CI watch, a
  merge's turn at the gate) and whose transcript and its subagents' transcripts have been quiet
  for five minutes. Such an agent does no work and spends next to no budget, so it never holds a
  slot: `beekeeper capacity` lists it under `waiting` and counts only the busy against the
  ceiling. The supervisor reads that verdict and never counts sessions by hand. Waiters that
  wake can push busy over the ceiling. No agent is stopped or parked for that: each one finishes
  its work. `capacity` says `no start: <n> busy at the ceiling of <m>` and, when `capacity.ceiling`
  is positive, the watch says `CAPACITY FULL` until busy drops under `capacity.ceiling`; while
  `capacity.ceiling` is `0`, `capacity` says `no start: ceiling 0`, `agents start` refuses without
  `--force`, and the watch suppresses both `CAPACITY LOW` and `CAPACITY FULL` (`capacity.floor` of
  `0` suppresses `CAPACITY LOW`; unset `capacity.floor` and `capacity.ceiling` default to `5` and
  `10`). When `capacity.usage.command` is configured, beekeeper derives `floor` and `ceiling` from
  the subscription's 5-hour and 7-day usage bands, prints `USAGE` on the watch every
  `capacity.usage.reportEvery` (30m by default), and at `capacity.usage.highPercent` (95% by
  default) on the 5-hour window emits `USAGE HIGH`, tells running busy workers once to finish their
  step and park, and holds unforced `agents start`, `agents resume` and `autoResume` until
  `fiveHour.resetsAt`; on `ENDED USAGE HIGH`, `autoResume` resumes settled `--on` parks and the
  supervisor resumes workers parked on the 5-hour usage window reset within the restored capacity
  verdict. When `capacity.providers` and `capacity.choices` are configured, beekeeper evaluates each
  provider's windows and ceiling (counting busy agents against the provider and bucket they run on,
  with a stale or failed usage read counting as `ceiling 0, room 0` for that provider, and the
  machine guards and `capacity.ceiling` capping the total), prints each provider's windows and
  verdict and the chosen `(harness, model)` pair in `beekeeper capacity` and once per `watch` tick,
  and `agents start --harness auto` takes the first non-excluded choice (`--exclude`) with room
  within its 5-hour threshold, worker weekly cap and 7-day pace margin (`--review` skips the pace
  check and worker weekly cap). Until the verdict allows a start the supervisor starts nothing, an unstaffed epic included
  (below). At every tick below `capacity.floor`, the supervisor fills the free slots in the same
  turn, within the memory guards (`MemAvailable` well above the watch's floor, machine swap not
  growing, a free build slot, the kind clusters under `maxKindClusters`), and never starts past
  `capacity.ceiling`. A task
  that needs a held lease queues behind the lease without holding the others back. The slots are
  filled from, in order: pages and new alerts in our area; In Progress items with a closable
  remainder; Blocked items whose blocker is ours; Up Next items (an Up Next epic is in planning: its
  sub-issues are work only after the person's go, their `beekeeper board move` of the epic to In Progress); open issues in our
  repositories, bugs first; the improvement queue of beekeeper and the desk; the top of the
  Backlog. When Up Next runs dry the supervisor pulls the top Backlog items itself and tells the
  guide in one line, which is the person's veto; with nothing startable at all, the guide gets one
  line `nothing startable: <reason>`. A teammate's item only after a one-line ask to that teammate.
- **Every ranked epic with open work has a worker.** If an epic of `beekeeper priority` is not
  held, has free work under it and has no session serving it, `beekeeper board next` prints it
  first as `EPIC UNSTAFFED priority <n> <epic URL> …` (`--json`: `unstaffed`). At every tick,
  each such epic gets a worker on its next item before any other slot is filled, highest rank
  first, within the capacity verdict. When the verdict leaves no room, waiting and parked agents
  do not hold the slots: only busy agents do. A busy agent on unranked work gives way first,
  finishing its current step and parking. If an unstaffed epic still has no worker after that,
  the guide gets one line `EPIC UNSTAFFED <epic URL>: <why no worker>`, and the person hears it
  through the guide. A ranked epic is never left without a worker because its slots are taken by
  sessions that only wait.
- **Work comes from the roadmap board and stays on it.** `beekeeper board next` picks it, ranked by
  `beekeeper priority`; an epic's hold is `board block`, never a note. An epic entering Up Next with no plan gets
  a planning session of its own, started with `beekeeper agents start`, whose question rounds reach
  the person through the guide and which ends with the plan, the sub-issues and `plan ready
  <epic>`; an epic already In Progress finishes on the criteria in its issue. An unowned finding
  becomes an issue on the board, then a task; it is never parked in a note, and notes carry only
  decisions for a person. Each worker moves its own board item; the guide reconciles the board.
- **A fresh worker per task.** A task goes to a worker started with `beekeeper agents start "<task>"
  <brief file> --task "<task>"`, a model chosen for the task and a brief that carries only the
  task: `agents start` puts the shipped worker rules ahead of it, and the worker reports through
  `agents idle --done --report`, parks through `agents park`, asks questions through `note add` and
  receives wakes through its inbox (`agents wake`; in Claude Code a message to `the supervisor` also
  reaches whoever holds the role then); it shows in the desktop's sidebar under its name and on
  the roster busy from its start. Under `agents.mode: background` it starts as a background session
  with Remote Control instead (`claude agents`, `claude attach <id>`), with none of this session's
  `CLAUDE*` variables, and is on the roster when `agents start` returns: no `agents assign`, no
  desktop import, no `claude://` link, and no launcher of the desk's own. Every new session starts that way, no session is kept in reserve
  or given a second task. An empty session the person started and registered with
  `/register-agent` takes one task by `beekeeper agents assign`. A message to an agent whose CLI
  may have stopped is `beekeeper agents wake <agent> "<message>"`, never a `SendMessage` to its
  `local_` id, which the desktop caps when nobody types in the sender's session; a background
  session gets it by name while it runs and is continued by `claude --bg --resume` once stopped. Done, an agent
  reports with `agents idle --done`, whose required `--report` the watch prints as `WORKER REPORT`
  and whose `--problem` lines as `PROBLEM FOUND`: each finding, and each one a worker messages on
  the way, becomes an issue in its owning repository and a worker; a code scan's finding only once
  the commit it names is the default branch's head, or a re-scan there still raises it.
  `PARKED ON A PERSON` goes to the guide, who tells the person; `PARKED ON A LEASE` is the
  supervisor's own, settled by its grant, never the guide's. The doctor takes the agent off the roster and archives
  its session, as it does with relieved role holders and idle entries whose CLI is gone a day
  (`beekeeper doctor --dry-run` lists them); none of it is a note for the person. Briefs are standalone, because every agent
  starts on an empty context. The checkout the sessions load their project rules and skills from
  stays on a current default branch: one left on a branch or behind is moved back before a worker
  starts there. The tick map lists the idle agents and the running workers.
- **Every session's epic is reported when the session ends.** At first sight each session is mapped
  to the epic or issue it serves and recorded with `beekeeper sessions serve`; the map is part of
  every tick, and the watch names the issue once when a recorded session ends. The ending session's
  report, re-queried live, says what closed, merged and rolled and what is still open; the next
  tick carries it, and the end summary repeats it per epic. When an epic's last sub-issue closes, a
  closing worker checks the criteria, runs the live proof and moves the epic on.
