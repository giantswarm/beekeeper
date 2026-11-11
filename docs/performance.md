# Performance budgets

beekeeper runs beside the sessions it starts, on the same machine. Its own
CPU, reads and memory come out of theirs, so it has budgets, and CI fails a
change that breaks one. The suite lives in `internal/perfsuite` behind the
`perf` build tag.

## What it measures

Every test builds the real binary (or takes `BEEKEEPER_PERF_BINARY`) and
generates a desk of the size of a busy coordinator: about 100 sessions (a
third each working, idle and parked on a review), a 6 MiB `state.json`, a
12 MiB event log, 150 open pull requests and three execution hosts. GitHub,
SSH and the harnesses are fakes (the test binary under their names, on a PATH
of nothing else; a local HTTP server for the GitHub API). Nothing reaches the
network, a real desk or an account.

| test | budget per |
|---|---|
| `TestCLIBudgets` | call of `agents`, `agents start --dry-run`, `agents wake`, `capacity`, `supervisor status`, `guide work`, `board show`, `note list`, `budget`: wall and CPU time (median of three calls), bytes read, peak memory and processes started (the most of three) |
| `TestLoopBudgets` | pass of a running `supervisor loop`: CPU, bytes read and written and processes started, idle and with one new event, and idle again after the event log grew to four times its size: the reads must not grow with it |
| `TestConcurrentCalls` | 20 calls at once (readers and event writers): time until the last ends, and the longest exclusive hold of the state lock |
| `TestSessionFootprint` | session: processes and memory of its runner unit, working, idle, parked and off the roster (the last two: none) |

How it measures: CPU and peak memory from the process's rusage, which
includes its waited-for descendants; bytes read and written from
`/proc/<pid>/io` (`rchar`, `wchar`, cached reads included), read while the
exited process is still a zombie; processes from the fakes' trace and a
sample of `/proc` for beekeeper re-executing itself; the lock hold by
taking a shared lock every 2ms, which only an exclusive holder refuses.

A budget an open issue has over today carries that issue (`limit.Known`):
the report shows it, and the test fails once the value is within budget, so
the marker goes and the budget holds from then on.

## Where it runs

Never on a desk's coordinator.

- **CircleCI, every pull request and release tag** (`test-performance`):
  `TestCLIBudgets`, `TestLoopBudgets` (30-second windows) and
  `TestConcurrentCalls`. Its control step builds main before the fix of
  [the loop's re-reads](https://github.com/giantswarm/beekeeper/issues/3657)
  and requires the loop budgets to reject it (`.circleci/performance-control.sh`).
- **GitHub Actions, nightly** (`.github/workflows/performance.yaml`, also by
  hand and on pull requests that change the suite): everything with
  two-minute loop windows (`BEEKEEPER_PERF_LONG=1`), and the footprint test
  under a systemd user manager (`BEEKEEPER_PERF_SYSTEMD=1`).

The tests skip without `BEEKEEPER_PERF=1`, so `make test` and `go test ./...`
never run them. Outside CI they refuse to run unless the process sits in a
cgroup that limits both CPU and memory:

```sh
make test-performance-local   # systemd-run --user --scope -p CPUQuota=100% -p MemoryMax=1G
```

## The report

Each run prints a table of every value against its budget, with the commit,
the ref (a release tag on tag builds) and the binary's version, and keeps it
as `report.md` and `report.json` with the fakes' call traces in the job's
`performance` artifacts. A regression shows as `OVER` on the commit that
caused it.

## Changing a budget

Budgets sit beside their tests (`cliBudgets`, `loopIdle*`, `concurrent*`,
`footprintBudgets`). Raise one only with the reason in the pull request; a
change that makes beekeeper leaner lowers it.
