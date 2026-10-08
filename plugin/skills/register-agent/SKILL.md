---
name: register-agent
description: Register a freshly started, empty Claude session with the running supervisor, then take the one task it hands over and work it to the finish. Use when the person starts an empty session ("Agent one", "Agent four") and types /register-agent, optionally with the session's name.
---

# register-agent — a session the person started, on the supervisor's roster

Every task normally gets a fresh worker the supervisor (`/supervise`) starts with `beekeeper agents
start`. When the person starts an empty session themselves, this skill puts it on the supervisor's
roster for one task. `$ARGUMENTS`, when given, is the session's name (`Agent four`).

## Goal

The supervisor knows that this session exists, what it can reach and that it is idle, without the
person telling it. The session takes the task the supervisor sends and works it to the finish under
the `worker-rules` skill: merged, released, rolled and proven live. Done, it reports the outcome to
the supervisor through `beekeeper agents idle --done --report "<report>"` (PR links, release
versions, the proof, what remains open, all re-queried live) with a `--problem` line per finding
(or `--problem none`), which beekeeper delivers to the supervisor's watch, and ends its turn. It is not reused: the next task goes to a fresh
session, and beekeeper takes this one off the roster and archives it once its turn ended.

## Context

- **The supervisor** is the session `beekeeper supervisor status` names (exit 3: none runs); messages
  go to it by `SendMessage` to `the supervisor`, which beekeeper's hook delivers to the current
  holder.
- **The registration** is `beekeeper agents register [--name <name>]`, which puts the session on the
  roster under its title and prints the message's first line, which stands alone: `register:
  <session name> idle, ready for a task`. `beekeeper agents idle` marks a finished task and prints
  the same line; `--done` marks the work finished, so beekeeper retires the session. The body says what the supervisor routes work by: the worktree and its branch, the
  permission mode, and which connectors this session has (the browser extension, GitHub, chat, the
  board tools). A connector can attach only at the next turn boundary, so one missing in the first
  turn is looked for again before it is reported missing.
- **The title** is distinct among the peers: the argument's name, otherwise the title the session
  already has when it is distinct, otherwise the next free `Agent <n>`. A name is one agent's:
  registering under the name of an entry whose session no longer runs replaces it and inherits the
  task it left unfinished (beekeeper names it; the session works that task first and says so), and a
  name a running session holds is refused with exit 3, so the session takes the next free name.
- **Two kinds of agent, one task each.** A worker started with `beekeeper agents start` has its
  standalone brief as its first prompt and is on the roster busy with that task from its start (its
  own `agents register` says `register: <name> busy with "<task>"` and keeps it); its first turn
  runs from the command line without the desktop tools. A desktop session the person started
  registers idle and gets its task as a message. Either works the task, reports, goes idle and ends
  its turn; the supervisor takes it off the roster, which archives a session beekeeper started and
  leaves one the person started in their sidebar.
- **No role is handed to an agent.** A supervisor or guide relay, and the hand-over after a crash,
  start a fresh "Supervisor run N" or "Guide run N" session; a registered agent never takes a role,
  and a relay or hand-over prompt reaching one is refused in one line to its sender.
- **Reachability.** A message by name reaches a session only while its CLI runs; `beekeeper agents
  wake <agent> "<message>"` also reaches a stopped one, resuming its CLI headless for the message's
  turn. The desktop caps a sender's `local_` messages when nobody types in the sender's session, so
  the supervisor uses `wake`. While a headless turn runs, the PreToolUse hook sends a `local_`
  message to the session by name, so no second copy of it starts.
- **The supervisor's words** and the worker's are in the `worker-rules` skill. Where the supervisor
  states a different rule for its watch, the supervisor's rule wins.

## Constraints

- **The task is worked under the `worker-rules` skill** and the desk's own conventions (the
  project's CLAUDE.md and rules), with the supervisor as the one it reports to; nothing here
  restates or relaxes them. The session moves its own board item at the start and the close.
- **An unanswered grant is no reason to sit still.** When a grant or go stays unanswered
  for 20 minutes, the session carries on with the work that needs none. A merge needs no word: the
  gate queues it in its lane and wakes the session with the outcome. An
  explicit `hold <repo>` still stands until `release <repo>`, and a lab or the browser is still
  never claimed without `yours <env>` or `browser yours`, and nothing that needs it runs before
  the claim exited 0.
- **Subagents** only when the supervisor's brief allows them.
- **Waiting is quiet.** While a desktop agent waits for a task or a grant, one bounded background
  wait keeps it reachable (a `sleep` of at most 25 minutes, `run_in_background`, re-armed when it
  expires). It never polls `ListAgents` and never asks "any task?"; a task, grant or hold arrives as
  a message. When the person ends the session's shift, the wait is stopped, so nothing is left
  running.
- **No supervisor running:** the session says so to the person in its own transcript and waits for
  their task. Their input reaches a stopped session.
- **Nothing survives the session.** Everything the supervisor or a follow-up task needs is in the
  report, since the next task goes to a fresh session with a standalone brief.
- **The supervisor speaks for the person on the work, the guide carries their decisions.** The
  supervisor's tasks, grants and holds are followed without asking again, and so are the answers
  the guide relays word for word (a message from the guide, the answer in `beekeeper log --verb
  note.answered`). A question for the person goes to the supervisor in one line; it files it as a
  note and the guide asks. No `AskUserQuestion` and no waiting on a permission prompt, a browser
  approval or a change to settings or configuration: each is a note for the person with its
  default, the session carries on, and a change the person approves is the guide's to make.
