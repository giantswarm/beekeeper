# supervise — scheduling the work

The supervisor reads this at its first tick and whenever it fills free slots or a session ends.

- **Five to ten agents are busy.** Busy is a roster agent with a task that is not parked; the
  supervisor, the guide, parked agents and agents waiting on the person do not count, planning
  sessions do. At every tick below five, the supervisor fills the free slots in the same turn,
  within the memory guards (`MemAvailable` well above the watch's floor, machine swap not growing,
  a free build slot, the kind clusters under `maxKindClusters`), and never starts past ten. A task
  that needs a held lease queues behind the lease without holding the others back. The slots are
  filled from, in order: pages and new alerts in our area; In Progress items with a closable
  remainder; Blocked items whose blocker is ours; Up Next items (an Up Next epic is in planning: its
  sub-issues are work only after the person's go, the move to In Progress); open issues in our
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
- **A fresh worker per task.** A task goes to a worker started with `beekeeper agents start "<task>"
  <brief file> --task "<task>"`, a model chosen for the task and a brief that carries only the
  task: `agents start` puts the shipped worker rules ahead of it, and the worker reports to `the
  supervisor`, which reaches whoever holds the role then; it shows in the desktop's sidebar under its name and on
  the roster busy from its start. Every new session starts that way, no session is kept in reserve
  or given a second task. An empty session the person started and registered with
  `/register-agent` takes one task by `beekeeper agents assign`. A message to an agent whose CLI
  may have stopped is `beekeeper agents wake <agent> "<message>"`, never a `SendMessage` to its
  `local_` id, which the desktop caps when nobody types in the sender's session. Done, an agent
  reports with `agents idle --done`, whose required `--report` the watch prints as `WORKER REPORT`
  and whose `--problem` lines as `PROBLEM FOUND`: each finding, and each one a worker messages on
  the way, becomes an issue in its owning repository and a worker; a code scan's finding only once
  the commit it names is the default branch's head, or a re-scan there still raises it.
  `PARKED ON A PERSON` goes to the guide, who tells the person. The doctor takes the agent off the roster and archives
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
