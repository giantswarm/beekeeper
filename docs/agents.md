# Agents and their turns

How beekeeper starts, wakes, stops and hands over the agents of a desk.

## Turns while parked

A park on a person or note stays asleep until the answer or note closure,
including when old timers or commands finish in the meantime. Explicit
messages and `agents resume` remain available to the person.

- Park settlement (`watch`, `agents.autoResume`): Only after the named note, issue, PR, hold or
  lease settles.
- Timers and probes: A failed probe stays in the watch; no model checks it. At delivery, the current
  park is read again. A person/note park gets `TIMER WAKE SKIPPED`; another named park accepts only
  a typed GitHub condition on its own reference. A plain external wait accepts its timer, including
  its explicit timeout. An explicit `Timer <id>: …` wait accepts that timer, including probe
  completion and timeout. Skipped or failed wakes keep their timer open for retry until delivery
  succeeds or a durable delivery is queued.
- Command outcomes (`devctl.unheard`): A person park keeps the outcome owed until the park ends,
  within the existing 24-hour outcome retention window. A plain PR wait still receives its own
  command result, including red CI.
- Automatic note/default and hold-release notifications: A parked session gets its own park
  settlement instead, for local and central defaults. Other note outcomes stay in the log. A
  person's explicit answer/message still reaches it.
- Supervisor loop: Verified review findings, red CI, merge go, completed release proof and matching
  alerts settle their corresponding waits; a note park gets no idle wake.
- Idle, stopped-agent and high-usage notices: Parked workers are skipped; notices do not start a
  model turn.
- Headless turn recovery (`resumeOnWait`): Parked/kept workers and serve records waiting on a person
  are skipped.
- Person's messages (UI, Slack feedback, central mailbox, explicit wake/resume/broadcast): An
  explicit message may interrupt the park; there is no periodic model turn to check for it.
- Hooks and harness inboxes: Hooks only run during an existing tool call; inboxes deliver explicit
  queued messages and do not poll a model while idle.

`TestPersonParkEightHoursWithoutTurns` replays eight hours of watch ticks,
probes, reminders, command outcomes and idle/usage notices using a delivery
counter: zero turns until the answer, then exactly one answer turn. It uses
an advanced clock and no paid model calls.

## Agents started without a click

A parked worker's continuation uses a fresh session chosen by current capacity and
provider pace. Incoming events are retained with the parked task until the new
worker accepts its brief; no previous transcript is loaded. The brief contains
the task reference, PR/head when known, the previous wait, latest handover note
and event. Stops, holds and capacity still apply. Role holders use their relay
lifecycle. A park without a recorded task reference stays parked rather than
resuming a transcript to reconstruct the task.

The fresh worker takes its predecessor's place: the continuation of a parked worker, and the
worker the loop starts for a pull request whose worker left the roster for the desk review (red
CI, a review's findings, a rework). It takes over the serve record, the desk-review park, the
notes the old session filed, the timers it set or that wake it, what is owed to it, the alerts
and merges it owns, its leases and grants, and the old session's start names it as successor. An
answer to the old session's note, a timer or a message for it reaches the fresh worker (a parked
one gets its own continuation); the old transcript is never resumed beside it.

The coordinator handles successful owned CI waits directly as desk-review
requests, and already owns approved merges and its GitHub budget retries.
Use a timer's `--run` for a known authorized mechanical next command; release
and rollout probes need no model turn just to check whether they settled.
Events that need reasoning go to a fresh worker. `beekeeper capacity` prints
24-hour wakes, transcript resumes and fresh starts by provider (also
`activity24h` in JSON). Starts count session identities once, including retired
workers; events without a known provider are reported under `unknown`.

Local worker starts by hand require `--dir <checkout> --branch <new-branch>`.
The supervisor loop's starts are outside this rule: it knows a repository, not a
checkout, so they run in `agents.dir` and their briefs say where to work.
The directory must be exactly one main repository checkout; parent folders,
organisation folders and linked worktrees are refused with accepted checkout
paths. The session launches in the checkout to load its repository instructions,
then works in `<stateDir>/worktrees/<repo>/<branch>`, created from the freshly
fetched origin default branch (`agents.worktreesDir` overrides the worktree
place). The branch's upstream is `origin/<branch>`, never the default branch, so
`git push` and `git push -u origin <branch>` create the branch on origin whatever
the checkout's `push.default`. Review starts require a PR URL in the brief and
use a detached `<stateDir>/worktrees/<repo>/review-<number>` instead. Existing
worktree destinations are refused. The prompt forbids commits, stashes and
checkouts in the main checkout. Split tasks spanning repositories into one task
per repository. `--dry-run` previews the path and rules without fetching or
creating a worktree.

`beekeeper agents start <name> <brief file>` starts an agent the way a person would start a
session and hand it a brief, without the click. The first prompt is the worker rules beekeeper
ships (the `worker-rules` skill, under the binary's version), then the brief as the task, so a
brief carries only its task and reports to `the supervisor`, whoever holds the role by then; a
hand-over puts the rules ahead of the follow-up's prompt again. The roster shows it busy with its
`--task` from the moment it is started, and the desktop shows it working from its first turn.

**The model.** `--model` is the session's model, which the starter picks per task; without it the
session runs on Claude Code's default, and a desk's workers are never started on its most
expensive model by default. The start records the model (the id its first reply names, `--model`
may be an alias) and its turns keep it. The desktop's import of a start often drops the model, and
the desktop then records its own default once it warms the session's CLI; the desktop refuses a
session's switch of its own model, so a steward, another session beekeeper started, sets it with
`set_session_model` on the session's row. A record holding another model than the start's counts as
lost as much as one holding none: the reopen restores it through a steward too. If the desktop
records another model and no steward can restore it, a seeded desktop start
fails before sending its task or launching a headless task. The seed and roster entry stay
recorded for inspection; restore the desktop model before waking the session. A later wake
still runs headless on the requested model while the desktop records another model.
`agents handover` keeps the start's model unless `--model` overrides it, and `agents wake`
resumes on it.

**The start gate.** Before anything is recorded, the start is judged as `beekeeper capacity` judges
one: the busy agents against `capacity.ceiling`, MemAvailable, disk swap's growth, the 1-minute load
and the kind labs, and for a task that builds, tests or lints, marked `--builds`, the free build
slots. A start refused by the busy slot alone waits its turn in the build slot queue with `beekeeper
run`'s waiters, first in first out, up to `MEMCAP_WAIT`, else 8 minutes, 60 in a background Bash
call, and is judged again once its turn came; a turn that does not come refuses it with the waiters
still ahead.

A start without the mark is one that does not build and is not held by a busy slot. A verdict
without room refuses the start with exit 3 and the guards named, and no session is created;
`--force` starts it anyway, the verdict overridden recorded in the event log.

**The task runs as a desktop turn.** Claude Desktop marks a row working only while its own CLI of
the session runs a turn, so a `claude -p` turn outside the desktop leaves the row idle while it
works. A start therefore runs no task headless: a seed turn (`claude -p` under the id beekeeper
chose, with `--tools ""` and `--strict-mcp-config`, the worker prompt as its first prompt and the
note that it only answers "ready") creates the transcript with its model and title, in a transient
user unit `beekeeper-agent-<id>`.

Once it ended, beekeeper imports the session into the desktop under its name, past the window's
focus (the person's typing holds the link 1 minute at most); a title or model the import dropped is
restored by a steward, the title by the session's own desktop CLI when no other steward runs (a
model never: see the model above). The desktop's CLI of the session, which the import warms, then
takes the message that starts the task as its turn; when the desktop runs none, a steward's
`send_message` through the desktop's session messaging starts one with it, the route a relay revives
a role holder by.

Before either spawn, `makeRoom` keeps the desktop under its cap of CLIs by ending one of beekeeper's
own idle CLIs: a finished worker's or a role run a relay relieved, then a parked worker's, and, when
the CLI is for a role's holder or relay successor, a worker's idle on its task (a message by name
starts it again); never a role holder's or a person's session. A revived role holder with no row in
the desktop yet is imported first, so a relay successor gets its CLI at the cap and while the person
types.

Only where the desktop cannot run the turn (it does not run, it did not import the session, at its
cap with no CLI of beekeeper's to end or the person still typing, so the session has no row, or no
steward took the send) is the session resumed headless as `agents wake` does, and `start` says why;
that turn's reopen imports a session the desktop did not. A start that delivers no turn of the task
fails: it exits non-zero, logs `agents.start` "task not delivered" with the reason, and `agents`
(REACHABLE) and the watch's AGENTS STOPPED line say "task not delivered" until `agents wake`
delivers a turn, so a worker that never got its task is never silent.

A role's relay successor keeps its headless first turn, which only takes the role; its relay hands
it the desktop turn. That first prompt starts with the role skill's bare slash command, which loads
the skill into the session.

## Waking a session

Claude Desktop delivers a `SendMessage` to a desktop id (`local_<id>`) itself: it starts the
session's CLI when none runs, which makes it the only route that wakes a stopped desktop session,
and it caps the route. After ten such sends since its person last typed in the sending session,
the desktop answers "Paused until your user's next message", so a supervisor nobody types in
loses it within hours. A send by name (a UDS peer) is not capped, but it reaches only a running
CLI.

`beekeeper agents wake <agent> "<message>"` covers both. A session whose CLI runs gets the
message by name. For a session with none, beekeeper resumes the session's own CLI from the
command line: one headless turn, `claude -p --resume <id>`, continuing its transcript under the
same id, as `agents start` runs a first turn. Once the turn ends, `agents reopen` shows the
session in the desktop for a moment, which warms its desktop CLI, and later messages by name
reach it. A wake while the unit starts, before its CLI is a peer, is refused (exit 3): send
again in a minute. So is a wake while the session's import is pending (a start's import and first
desktop turn, or a reopen waiting for the person), which would run a headless turn beside the CLI
the import warms: the refusal says what the import waits for and until when, and `agents desktop
<agent>` lets a reopen go ahead.

The desktop also starts a CLI for a `local_` send while a headless turn of that session runs (a
first turn, a wake): two copies of one session then run turns side by side. The PreToolUse hook
therefore sends a `SendMessage` to a `local_` id whose session has a running CLI to that CLI by
its name (`updatedInput`), which queues the message in the running turn. A name that two running
CLIs carry is refused with their PIDs, since a send by it reaches neither for sure. A send to a
session with no running CLI passes to the desktop, which starts it.

## Stopping everything

When the person says stop, every worker has to end its turn at once, and nothing may start,
wake or merge until they lift it. Ending the turns is not enough on its own: a start's or wake's
reopen unit imports the session into the desktop once its turn ends, a killed turn too; the
standby service revives role sessions and starts successors; and the watch's doctor reopens
workers without a desktop row. `beekeeper stop --reason "<the person's words>"` covers all of it:

- it records the stop (who, when, the reason) and holds `merges` and `github`, unless a hold
  that holds them whole stands already; one that lets a merge through (`--except`, a
  tool-release window) or would end by itself (`--until`, `--lift-when`) gives way to the
  stop's, and the resume puts it back as it was;
- it ends the devctl of every running gated merge and promotion before it merges (through its
  recorded merge-child, as a TaskStop of the gate does), and says, as GitHub reports it, whether
  the pull request merged before the stop or nothing merged (run that merge again after the
  resume); a waiting merge keeps its place behind the hold;
- it stops every running `beekeeper-agent-*`, `beekeeper-wake-*`, `beekeeper-reopen-*` and
  `beekeeper-report-*` unit, the standby service (`beekeeper-notify.service`) and the units
  `stop.units` names (a timer of the desk's own that would start or change something);
- it ends every session beekeeper started: its headless turn, its CLI and what it left running,
  a background session through `claude stop` so its daemon does not resume it. A session the
  person started, the guide's, the supervisor's and the caller's own are never touched, nor
  are their units.

While the stop stands, `agents start` (`--force` included), `agents handover`, `agents wake`,
`agents desktop`, a timer's wake, a note answer's wake, a parked agent's resume and the gate's
wakes are refused with exit 3, naming the stop's reason; a role's successor, a revive, a reopen,
a desktop import and a scheduled status report do not start; a wake timer that falls due waits
and fires after the resume. `beekeeper status` starts with `STOPPED: …`, `capacity` says `no
start: beekeeper is stopped …`, and the watch says `STOPPED: …` once and `ENDED STOPPED` after
the resume. Running `stop` again stops what came up since and keeps the first reason. A unit
that does not stop, a session that does not end or a devctl that cannot be signalled is named on
a `FAILED` line and makes `stop` exit 1, without the all-clear; the stop stands.

`beekeeper resume` lifts it: the units it stopped started again first, then the holds the stop
set lifted (a hold that stood before stays, one it replaced is put back unless its time passed),
starts and wakes allowed. A unit that does not start again keeps beekeeper stopped, with that
unit still to start, and makes `resume` exit 1. It starts no worker: it names the sessions
the stop ended, which wait for a wake. It starts the guide when the guide's session runs no CLI
(a reboot, a crash), as `beekeeper guide start --new` does. The stop never changes
`capacity.ceiling`, so the capacity after the resume is what it was.

## A repository's instructions on the first write

Claude Code loads the instructions of the project a session started in, not those of another
repository the session goes on to change. The hook closes that gap: a session's first `Edit`,
`Write` or `NotebookEdit`, or first `git commit` (in the call's directory, behind `cd <dir> &&`
or with `git -C <dir>`), in a git repository other than `$CLAUDE_PROJECT_DIR` (its worktrees
included) carries that repository's instructions as `additionalContext`, once per session,
subagent and repository:

- `CLAUDE.md`, `AGENTS.md` and `.claude/CLAUDE.md`, with their `@path` imports three levels deep;
- `.claude/rules/**/*.md`;
- the `hooks` and `permissions` of `.claude/settings.json`, which do not run in the session;
- the files these name, linked or in backticks, on a line that makes them mandatory ("must read",
  "read … first", "mandatory", "always read", "required reading").

The context is at most 10,000 bytes: a file that does not fit is cut with a note naming it, the
files after it are listed for the agent to read. Only regular text files inside the repository
are read: no symlink out of it, no `.git`, and no name that holds or may hold a secret or a
person's own setup (`settings.local.json`, `*.local.md`, `.env*`, keys and certificates, sops
files, anything named secret, credential, password, token or kubeconfig). The call's permission
is untouched: a rewrite such as the build slot or the merge gate carries the context beside it, a
refused call carries none and does not count as the first write. The markers are one file per
session and repository under `<stateDir>/reads/`, created exclusively so two concurrent calls
inject once; a session's markers go 7 days after its last first write.

Register the hook for every tool so queued relays reach the next tool call:

```json
"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command",
  "command": "~/.go/bin/beekeeper hook pretooluse"}]}]
```

## Background sessions

A desk that runs its sessions headless instead of in Claude Desktop sets `agents.mode:
background` (or passes `agents start --mode background` per start). Every session beekeeper
starts then is a Claude Code background session with Remote Control, run by the claude daemon,
which `claude agents`, `claude attach` and Remote Control show:

- **Start.** `agents start`, `agents handover` (whose follow-up is a background session too,
  listed without a desktop id and reported without the desktop's title and model lines) and a
  role's successor run `claude --bg
  --remote-control <name> --permission-mode bypassPermissions -n <name> [--model m]` in the
  agent's directory, under the worker profile's flags, with the worker prompt as the first prompt,
  which the first turn works. The model is `--model`, else `agents.model`, else Claude Code's
  default. No seed turn, no desktop import, no `claude://` link and no reopen unit.
- **A clean identity.** The call runs with none of the caller's `CLAUDE*` or `CCD*` variables, so
  a session started from inside another one never inherits its id or name: its commands, notes
  and holds are booked under its own name.
- **A daemon of its own.** Every `claude --bg` (a start, a resume) runs in a transient scope of
  its own under `app.slice` (`systemd-run --user --scope --collect`, `claude-daemon-<time>.scope`):
  the first one after boot starts the claude daemon as its child, which stays in that scope, never
  in the calling unit (the standby watch's `beekeeper-notify.service` with its `MemoryMax` and
  `CPUQuota`, a wake's unit), whose limits every background session would share and whose stop
  would end them all. `beekeeper doctor`, and so the watch every tick, moves a daemon it finds in a
  service or another app/session scope, with the processes under it there, into a scope of its own
  (`claude-daemon-<time>-<pid>.scope`) and says so in a `DOCTOR` line naming the unit and its
  limits (`daemon.move` in the log); no session stops. Dedicated daemon scopes and explicit
  build caps stay in place. Services are unsafe even with `Restart=on-failure`: an upgrade
  can exit cleanly and leave the replacement daemon subject to the old service's stop.
- **Independent guide recovery.** The standby checks CLI identities at least every 30 seconds,
  outside the main watch's accounting and network work. After `guide.restartGrace` (one minute
  by default), it resumes the guide or starts its successor, respecting stops and open relays.
  The stopped-agent remedy is `beekeeper agents wake`, which cleans the environment and scopes
  background resumes; never start the daemon through a raw resume in another session.
- **On the roster at once.** The daemon chooses the session id (`--bg` ignores `--session-id`);
  `agents start` reads it from `claude agents --json` and records the start (`background: true`)
  and the roster entry, busy with `--task`, before it returns. No `agents assign` follows. The
  session's first turn starts as `claude --bg` returns, a moment before the start is recorded
  (within `claude agents`' listing, a minute at most): a beekeeper command of that first moment
  can find the session off the roster, so a worker's brief does not open with one.
  A role successor's opening `beekeeper <role> start` is the exception: the relay
  matches its run name until the daemon's session id is recorded.
- **Wakes by messaging.** `agents wake`, and every wake beekeeper sends itself (a note's answer, a
  timer, the gate's word, a parked agent's resume, `agents resume`), sends a running background
  session the message by name. A session counts as a background one when beekeeper started it so,
  or, under `agents.mode: background`, when it has no desktop session and `claude agents --json --all`
  lists it as a background session; a session the person runs in a terminal keeps the headless
  route.

  A stopped one is continued by `claude --bg --resume <id> <message>` with no other flag: the
  session keeps the Remote Control, mode, name and model it was started with, where any flag would
  start a copy under a new id (beekeeper fails such a wake, naming the copy). The standby watch
  continues a stopped role holder the same way.
- **A role's successor** is relayed by its run name until the daemon has chosen its id, which the
  relay then records; its one session takes the role and goes on with it, without the desktop's
  second turn.
- **Nothing of the desktop.** The watch's rowless-import and the doctor's reopen skip background
  starts, and `agents desktop` refuses one.

Sessions with a desktop session keep the desktop route in either mode. `agents.mode: desktop`, the
default, keeps everything above as it was.

## Agents handed over near their context limit

Every registered agent's context (the CTX column) is watched, not only the guide's. Once an agent
session's context reaches `agents.relayAt` (default 400k),
`watch` says `HANDOVER DUE "<agent>" at <n>k: beekeeper agents handover "<agent>"` at the
agent's first quiet moment: no tool command of its own running and no gated merge of its own in
flight. An agent with a task whose CLI no longer runs (its headless turn ended while it waits on a
grant or a person) is measured by its transcript and is due the same way, with `(its CLI does not
run: …)` in the line. It says it once per agent session, across the watch's restarts. An agent
holding the supervisor's or the guide's role moves by relay instead.

An agent in its task's last step is held, not handed over: its `sessions serve --waits` names the
report or says the merge landed (`report`, `merged`, `release`, `rolled out`, `proof`), or the gate
saw a merge of its own land (settling, its release and rollout pending). `watch` says `HANDOVER HELD
"<agent>" at <n>k: last step, report expected (<the evidence>); due at <HH:MM> or at <ceiling>k`
once, and holds the hand-over for `agents.lastStepGrace` (30m) from that evidence or until the
context reaches `agents.lastStepCeiling` (default a quarter above `agents.relayAt`), whichever
comes first; past either, `HANDOVER DUE` follows and says how long it was held. An agent that
reported done (`agents idle --done`) is never handed over: the doctor archives it. A hand-over of a
worker at its report would start a fresh session that finds nothing to do while the original's
`WORKER REPORT DONE` arrives seconds later.

The supervisor then runs `beekeeper agents handover "<agent>"`. It refuses (exit 3, before it asks
for anything) unless a PermissionRequest hook for every tool runs `beekeeper hook permissionrequest`
where the follow-up starts: in the user settings or in the `.claude/settings.json` or
`settings.local.json` of its folder or of the checkout that folder is in. Without it the follow-up
would stop at its first card after the old session was stopped. It refuses an agent whose roster row
holds no task (none open, none it reported idle on), whose follow-up would have nothing to carry on:
`--task "<task>"` gives it one, which the follow-up's prompt and row carry, and `watch` says no
`HANDOVER DUE` for such an agent. Otherwise it prints one line per step:

1. **The note.** A peer message asks the agent to record what is in flight and what is next with
   `beekeeper agents note "<...>"` and end its turn; the hand-over waits `agents.noteWait` (3m)
   for it and carries on without it after that. The ask is one headless relay turn; a turn that
   called no SendMessage sent nothing and is run again, three turns at most. An agent whose CLI
   does not run, or does not take the message by name, is asked in one headless turn resumed from
   its transcript instead (as `agents wake` does, without the desktop's reopen), which is stopped
   once the note is written or the wait is over. Without a note the step says what the follow-up's
   prompt rests on: the task, the `sessions serve` record (the issue and what it waited on) and the
   last events. The command exits non-zero whenever no follow-up was started.
2. **The prompt.** `beekeeper agents handover --prompt "<agent>"` prints it: the roster task (of an
   idle agent the last task it reported idle on, which a repeated `agents idle` or a re-register
   keeps), the brief the agent was started with (a follow-up's brief is its predecessor's, not the
   prompt around it), its `sessions serve` record, its gated merges, its last events and its note.
   No standing rules and no live values: the follow-up is told to check the live state first.
3. **The start.** The follow-up is started as `agents start` starts one, in the old session's folder
   and model (the one it was started on, unless `--model` overrides it), under the agent's name: it
   takes over the roster entry, the task and the session record, busy with the task. It shows in the
   sidebar and runs its first turn from the prompt alone. Before the old session ends, the roster
   lists the follow-up under the name, busy with that task: a row or task lost meanwhile is restored
   (`roster:` line), and a follow-up that cannot be listed (another running session holds the name)
   fails the hand-over, leaving the old session running.

   In the same step it takes over the leases the old session holds and the grants queued for it, in
   their places (one `lease.handover` event): it claims or releases them as its own, no `lease
   release --force` needed. Until the old session ends both carry the agent's name: a `lease grant`,
   a `yours` or a SendMessage by that name reaches the follow-up, the roster's session (a message
   through its own CLI's socket), never an ambiguity refusal.
4. **The end.** Once the follow-up's transcript holds its first reply, the old session's CLI and
   every process under it get SIGTERM, then SIGKILL after 10 s, by PID: the desktop shows it
   stopped. A start or wake unit of the old session still running its reopen is stopped too, so the
   desktop does not warm a CLI of it again.
5. **The archive.** Once that CLI has exited, the old session's desktop row is archived through a
   steward, as the doctor archives a finished worker's, so only the follow-up's row carries the
   agent's name, also along a chain of hand-overs. A row it cannot archive now (no steward idle, a
   CLI of the old session running again) the doctor owes and asks for while that CLI runs no turn.
6. **The log.** One `agents.handover` event in `beekeeper log`.

A configuration named with `--config` or `$BEEKEEPER_CONFIG` is passed to the started session
as `$BEEKEEPER_CONFIG` and named in the note's command, so both write to the same state.

## Dependabot alerts

Workers read Dependabot alert states with `beekeeper security alert <owner/repo> <number>`
through their existing GitHub identity. The [security-scans rule and App setup](security-scans.md)
describe the read-only permission and how to handle inaccessible alerts.
