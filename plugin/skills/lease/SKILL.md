---
name: lease
description: Shared resources on a beekeeper desk (kind lab, installation, browser): claim, queue, park, free, the lease claim's outcomes. Load before using one.
---

# lease — a shared resource, one session at a time

The resources are the desk configuration's `resources` (a kind lab, an installation) and the
browser. The supervisor is the supervisor loop, no session: nobody hands a lease out by message,
and a free lease is the claim's.

- **The claim** comes right before the work that needs it: `beekeeper lease claim <env> -p
  "<purpose>"`. Its first line is its outcome: `held by you since <time>` (exit 0) is the lease;
  `queued: number <n> behind …` and `refused: <reason>` (exit 3) are none, whatever follows.
- **A held resource is another session's.** Nothing of the worker runs against it, and the
  worker never takes over a held resource. To wait for it, queue first: `beekeeper lease grant
  <env> "<your name>"` records your place, then the claim.
- **A claim never waits by itself.** Queued, the holder is finishing: `--wait <duration>` waits
  up to 9m in the foreground, `--park` longer: the worker ends its turn, the watch resumes it once
  the lease is its. No background wait; `lease release <env>` withdraws a queued claim.
- **A command that needs the lease runs only after the claim exited 0**: `beekeeper lease claim … &&
  <command>`, never `;` and never after a `queued` or `refused` outcome.
- **Done**, the worker runs `beekeeper lease release <env>` before its final report.
- **During a cluster upgrade** only the supervisor loop's or a person's `--upgrade-unblock` grant
  admits a claim of that installation; anything else waits for the upgrade to end.
- **A refused claim** is no reason to sit still: the worker carries on with what needs no
  resource.
