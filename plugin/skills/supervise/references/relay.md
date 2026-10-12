# supervise — handing the watch on

The supervisor reads this before a relay, after a crash or a reboot, and as a first supervisor.

The watch moves to a fresh session by relay, from disk, never by a prose brief. Each supervisor is
a numbered run, "Supervisor run N": its desktop title, roster name and the name peers message it by.

- **When:** by context, not by time. The watch says `RELAY DUE` once this session's context
  reaches `supervisor.relayAt` and the machine is quiet (no gated merge running or settling, no
  grant waiting to be claimed, no claim queued). Still over it `supervisor.relayGrace` (30m) later,
  the standby watch relays this session itself (`RELAYED`).
- **How:** everything pending is in beekeeper first (notes with defaults, timers, session records,
  holds, grants, lane seeds); a standing rule this watch agreed on is a pinned note (`beekeeper note
  add --pin`), which every hand-over carries. Then `beekeeper supervisor relay` starts "Supervisor
  run N+1" and opens the relay to it. Its headless first turn only takes the role with `beekeeper
  supervisor start`, which tells every running agent the new name. Once that turn ended, the
  relay prints one SendMessage to the successor's `local_` id: this session makes exactly that
  call, which has the desktop start the successor's CLI, where it reads `beekeeper handover --prompt` (one part in full: `--section
  <name>`), arms its watch and acknowledges by name. A decision listed there as answered is not
  asked again. Once `beekeeper supervisor
  status` exits 4 in this session, it is relieved. A relay not taken expires after
  `supervisor.relayTTL` or is withdrawn with `supervisor relay --cancel`, and the watch carries on.
- **Where the watch runs:** only in a desktop turn. A headless turn (a first turn, an `agents
  wake`) that arms a Monitor never ends, so the desktop never gets the session's CLI; such a turn
  takes the role or answers and ends. The one exception is the standby
  watch's headless resume, whose message says the desktop runs no CLI of the session: that turn
  keeps the watch.
- **After a crash:** `supervisor.restartGrace` after this session's CLI is gone, the standby unit
  starts the next run the same way; claims stay gated until its `beekeeper supervisor start`. A
  supervisor reopened after a reboot (`beekeeper supervisor reopen`) has lost its watch; the
  standby unit tells it to resume from `beekeeper handover --prompt`, and starts a successor when
  it is not back within minutes.
- **A first supervisor** started from the person's message runs `beekeeper supervisor start`
  (`--take-over` only for a predecessor whose session still runs without a relay) and reads
  `beekeeper handover --full` for what the last watch left; beekeeper records it as the next run and
  titles its desktop session with it.
