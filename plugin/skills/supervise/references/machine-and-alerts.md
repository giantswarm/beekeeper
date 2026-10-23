# supervise — the machine, alerts and consent

The supervisor reads this at its first tick and when the watch reports memory, an OOM kill, an alert
or a page.

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
  resolving is one `FLAPPING` line; a reading that misses alerts (`… missing from this reading`)
  keeps them until the next reading confirms the resolve. A NEW line's `during merging …` / `during merged …` names what
  happened on the installation in the last 30 minutes: a lead, not a cause. While one of its
  clusters upgrades, beekeeper holds the installation by itself (`UPGRADE` to `UPGRADE ENDED`):
  the gate refuses its lanes' merges and its claims are refused; nothing to set or lift by hand.
- **The standby unit** (`contrib/systemd/beekeeper-notify.service`) starts the next supervisor run
  as a fresh session after a crash and resumes a role session whose CLI came back.
- **Alerts go to whoever changed the area.** A new alert on an installation in play, in an area a
  session handed out or changed, goes at once to that session by name, with the alert and where it
  fires. A page has an owning session within one tick: the session that changed the area,
  otherwise a worker started for it, which records it with `beekeeper alerts own`; `PAGE UNOWNED`
  is a page nobody took. A page caused by a teammate's resource on our installation is
  contained at once by a worker, reversibly (suspend, scale to 0), which tells the owner
  and notifies the guide. Another team's alert in its own area is noted in the tick and left alone.
  A `RUNAWAY` session is told its figure in one message; a `LANE STALLED` lane gets its absent
  place checked with that place's session and dropped (`lanes drop`) only when that merge will not
  come (a place whose pull request merged or closed leaves by itself). At `HANDOVER DUE "<agent>"`, `beekeeper agents handover "<agent>"` hands that agent to a
  fresh session in its place.
- **Consent boundaries.** Bounding or waiting beats killing; never kill a busy session's work; never
  archive or stop a session or take over a held lab without the person's explicit ask; the doctor's
  archive request under the person's standing archive rule is that ask, a relieved run's own session
  included. RAM-backed scratch is deleted only for *dead* sessions (CLI gone, PR merged, no worktree) and only after
  looking at what is there. A kill of a runaway process names an explicit PID from the live process
  list, never a pattern.
