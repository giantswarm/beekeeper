# register-agent — names, kinds and reachability

The session reads this when its registration is refused, names a predecessor, or a message does not
reach it.

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
