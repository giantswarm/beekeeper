---
name: register-agent
description: Register a freshly started, empty Claude session with the running supervisor, then take the one task it hands over and work it to the finish. Use when the person starts an empty session ("Agent one", "Agent four") and types /register-agent, optionally with the session's name.
---

# register-agent — a session the person started, on the supervisor's roster

Every task normally gets a fresh worker the supervisor (the supervisor loop, `beekeeper supervisor
loop`) starts with `beekeeper agents start`. When the person starts an empty session themselves, this skill puts it on the supervisor's
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

- **The supervisor** is the supervisor loop, a service no session holds (`beekeeper supervisor
  status`, exit 3 when it does not run): a worker reports through `agents idle --done --report`,
  parks through `agents park`, asks questions through `note add`, and receives wakes through its
  inbox (`agents wake`). No message reaches the supervisor: `SendMessage` to `the supervisor` is
  refused.
- **The registration** is `beekeeper agents register [--name <name>]`, which puts the session on the
  roster under its title and prints the message's first line, which stands alone: `register:
  <session name> idle, ready for a task`. `beekeeper agents idle` marks a finished task and prints
  the same line; `--done` marks the work finished, so beekeeper retires the session. The body says what the supervisor routes work by: the worktree and its branch, the
  permission mode, and which connectors this session has (the browser extension, GitHub, chat, the
  board tools). A connector can attach only at the next turn boundary; use the connector handshake
  below when a connector tool is missing.
- **The supervisor's words** and the worker's are in the `lease` and `merge` skills. Where the
  supervisor states a different rule for its watch, the supervisor's rule wins.

## Connector handshake

- When a connector tool is missing, use `ListAgents` once to choose an idle peer and send it
  exactly `SYN <own session name>: reply "ACK" to me with SendMessage and do nothing else.` with
  `SendMessage`, substituting this session's name. Finish the turn's other work and end the turn;
  do not keep it open with a background wait. The peer's ACK opens the next turn so the connector
  can attach.
- On receiving a message that starts with `SYN <name>:`, reply exactly `ACK` to that name with
  `SendMessage` and do nothing else. End the turn.
- In the turn the ACK opened, look for the connector tool again. Only if it is still missing in
  that turn, ask the person to switch it on through the normal `note add` route. Never hand the
  work back to the person because a connector tool is missing.

## Constraints

- **The task is worked under the `worker-rules` skill**, the topic skills it names and the desk's
  own conventions (the
  project's CLAUDE.md and rules), with the supervisor as the one it reports to; nothing here
  restates or relaxes them. The session moves its own board item at the start and the close.
- **An unanswered grant is no reason to sit still.** When a grant or go stays unanswered
  for 20 minutes, the session carries on with the work that needs none. A merge needs no word: the
  gate queues it in its lane and wakes the session with the outcome. An
  explicit `hold <repo>` still stands until `release <repo>`, and nothing that needs a lab or the
  browser runs before its claim exited 0 (`beekeeper:lease`).
- **Subagents** only when the supervisor's brief allows them.
- **Waiting is quiet.** While a desktop agent waits for a task or a grant, one bounded background
  wait keeps it reachable (a `sleep` of at most 25 minutes, `run_in_background`, re-armed when it
  expires). The connector handshake above ends the turn instead. It never polls `ListAgents`
  (the handshake's one lookup is not polling) and never asks "any task?"; a task, grant or hold arrives as
  a message. When the person ends the session's shift, the wait is stopped, so nothing is left
  running.
- **No supervisor running:** the session says so to the person in its own transcript and waits for
  their task. Their input reaches a stopped session.
- **Nothing survives the session.** Everything the supervisor or a follow-up task needs is in the
  report, since the next task goes to a fresh session with a standalone brief.
- **The supervisor speaks for the person on the work, the guide carries their decisions.** The
  supervisor's tasks, grants and holds are followed without asking again, and so are the answers
  the guide relays word for word (a message from the guide, the answer in `beekeeper log --verb
  answer.recorded`). What the work shows on GitHub is its own result: the merge, the reply to a reviewer,
  the issue update, never the answer itself. A question for the person goes to the supervisor via `note add`; it files it as a
  note and the guide asks. No question tool (`AskUserQuestion`, `ask_question`) and no waiting on a permission prompt, a browser
  approval or a change to settings or configuration: each is a note for the person with its
  default, the session carries on, and a change the person approves is the guide's to make.

## References

`references/roster.md` in this skill's folder: the title and a name's predecessor, the two kinds of
agent, why no role is handed to an agent, how a message reaches the session. Read it when the
registration is refused or names a predecessor, or a message does not arrive.
