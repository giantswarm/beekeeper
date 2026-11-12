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

- `TestCLIBudgets`: call of `agents`, `agents start --dry-run`, `agents wake`, `capacity`,
  `supervisor status`, `guide work`, `board show`, `note list`, `budget`: wall and CPU time (median
  of three calls), bytes read, peak memory and processes started (the most of three)
- `TestLoopBudgets`: pass of a running `supervisor loop`: CPU, bytes read and written and processes
  started, idle and with one new event, and idle again after the event log grew to four times its
  size: the reads must not grow with it
- `TestConcurrentCalls`: 20 calls at once (readers and event writers): time until the last ends, and the longest exclusive hold of the state lock
- `TestSessionFootprint`: session: processes and memory of its runner unit, working, idle, parked and off the roster (the last two: none)

How it measures: a call runs under the test binary as its measuring
parent, which reaps it and reads its own `/proc/self/io` (`rchar`, `wchar`,
cached reads included), where the kernel adds a reaped child's and its
descendants' reads and writes; CPU and peak memory come from the call's
rusage, which includes its waited-for descendants. A running loop is read
from its `/proc/<pid>/stat` and `/proc/<pid>/io` before and after a window.
Processes are counted from the fakes' trace and a 20ms sample of `/proc`
for beekeeper re-executing itself. The lock hold is measured by taking a
shared lock every 2ms, which only an exclusive holder refuses.

A budget that an open issue already exceeds today carries that issue
(`limit.Known`). The report shows it without failing. The test fails once
the value is within budget, so the marker is removed and the budget holds
from then on. Today's known one: an idle loop pass on an event log four
times the size and its reads:
[the loop's re-reads and re-polls](https://github.com/giantswarm/beekeeper/issues/3657).
Its CPU budget holds over both window lengths since the loop folds only the
history appended after its last pass.
The idle pass's CPU and processes were known under the same issue until the
loop stopped re-verifying unverified review requests every pass; their
budgets now hold.
The runner units of parked and off-roster sessions were known until
[runner units outliving their sessions](https://github.com/giantswarm/beekeeper/issues/3675)
was fixed; their budget of zero now holds.

`BEEKEEPER_PERF_STRICT=1` holds every budget, known or not; the control
step uses it.

## Where it runs

Never on a desk's coordinator.

- **CircleCI, every pull request and release tag** (`test-performance`):
  `TestCLIBudgets`, `TestLoopBudgets` (30-second windows) and
  `TestConcurrentCalls`. Its control step builds main before the fix of
  [the loop's re-reads](https://github.com/giantswarm/beekeeper/issues/3657)
  and requires the strict loop budgets to reject its idle reads
  (`.circleci/performance-control.sh`).
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

Checkpoint publication encodes the retained projection directly under its reader
lock as compact JSON, avoiding a full slice copy and indentation overhead.

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
