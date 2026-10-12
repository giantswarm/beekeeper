---
name: lease
description: Shared resources on a beekeeper desk (kind lab, installation, browser): need, yours, free, the lease claim's outcomes. Load before asking for one.
---

# lease — a shared resource, one session at a time

The resources are the desk configuration's `resources` (a kind lab, an installation) and the
browser. A lease is the supervisor's grant first, then the worker's claim.

- **The words.** The worker asks the supervisor `need <env>` with its purpose and duration. The
  supervisor answers `yours <env>` (`browser yours` for the browser), which records its grant, and
  `go` for a step the worker announced. Done, the worker runs `beekeeper lease release <env>` and
  says `<env> free` (`browser free`), before its final report. A different rule the supervisor
  states for its watch wins.
- **The claim** follows `yours <env>` at once, never comes before it: `beekeeper lease claim <env> -p
  "<purpose>"`. Its first line is its outcome: `held by you since <time>` (exit 0) is the lease;
  `queued: number <n> behind …` and `refused: <reason>` (exit 3) are none, whatever follows.
- **A claim never waits by itself.** Queued after `yours <env>`, the holder is finishing: the same
  claim with `--wait <duration>` waits minutes in the foreground. A wait of hours is a probe timer
  on `beekeeper lease status <env>` and a park (`beekeeper:park`), never a background wait.
- **A command that needs the lease runs only after the claim exited 0**: `beekeeper lease claim … &&
  <command>`, never `;` and never after a `queued` or `refused` outcome.
- **A held lease is another session's.** Nothing of the worker runs against it, and the worker
  never takes over a held resource.
- **An unanswered `need`** is no reason to sit still: the worker carries on with what needs no
  resource, and still claims nothing without `yours <env>`.
