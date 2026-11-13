# Behavioral scenarios for beekeeper's core

Epic: https://github.com/giantswarm/beekeeper/issues/3788

Status: plan, redesigned for speed by https://github.com/giantswarm/beekeeper/issues/3954. The
first harness started the real binary for every call of every scenario and took 9 minutes. This
revision runs every scenario inside the test process in a few seconds, and keeps the real binary
for a handful of smoke scenarios. The scenario format, the flows, the regression scenarios, the
rule for fix PRs and the order of the refactor stay. [The move](#the-move) lists the pull requests
that take the existing scenarios over, one step each.

## Outcome

Every pull request runs every behavioral scenario in a few seconds, on one CI executor and on a
laptop, without sharding: the in-process suite in under 5 seconds of wall time, and the full run,
the [smoke scenarios](#real-binary-smoke-scenarios) on the real binary included, in under 10,
excluding the builds. A scenario states a behavior as given/when/then in YAML and runs the
real command code of `beekeeper`: the loop with its streams and refill, and the commands that
workers, the guide and the person call. It runs against fakes of everything outside: GitHub, the
harness CLIs, the service manager, the SSH execution hosts and the clock. A bug found live becomes
a scenario that fails before its fix. With that net in place, the largest core files are split
into packages without a change in behavior.

## Why the first harness took 9 minutes

Timo's verdict on the epic (2026-11-13): "The job was to add fast tests and by that I mean a full
run takes a few seconds. 10 minutes is a complete failure." The first revision of this plan set
"the whole suite under 10 minutes" and took muster's model: the real binary per scenario. The
CircleCI report of `main` at d8280445 (job 66486, two shards since
https://github.com/giantswarm/beekeeper/pull/3911) shows where the time went:

- 71 scenarios, 1,062 s of scenario time, 371 s and 287 s of wall time in the two shards.
- `advance`: 455 s, 43 %. The world's clock waits in real time until every process has
  acknowledged the new time and the desk has been quiet, because no process can see whether the
  others are idle.
- `run`: 321 s, of which 103 `supervisor loop --once` passes took 227 s, 2.2 s each. One pass of
  delivery/board-item-to-merged-and-done starts about 20 fakes (`gh`, `journalctl`, `devctl`,
  `claude`, `systemd-run`), each a process that dials the world, and the loop's own `beekeeper`
  calls, each a process that loads the configuration and the state again.
- `wait_for`: 250 s of polling processes in real time.
- The five slowest scenarios (the loop hour, two self-updates, the large-desk probation and the
  execution continuation) took 554 s, half of it. An hour of loop passes on a world clock is
  still a hundred passes of processes.

No tuning of that design reaches seconds: every call is a process start, and a loop flow is many
passes of many calls. The shards hid the cost and did not change it.

## The design in one paragraph

Each scenario runs in one `testing/synctest` bubble of the test process. Every `beekeeper` call
is a goroutine that runs the real command tree, `cmd.Run(ctx, proc.Invocation)`, with its own
arguments, environment, directory, standard I/O and process id: a `run` step, the loop's own
subcommands, a worker script's `agents park`, a host's command behind the fake `ssh`, a service
unit. Every process beekeeper starts, and every other thing it asks of the operating system about
processes, goes through one seam, `internal/proc`. A release build answers it with the operating
system, as today; a scenario answers it with its world. The bubble's clock is Go's fake clock: it
moves only when every goroutine of the scenario waits on it, so `advance: 1h` takes no real time
and needs neither an acknowledgement nor a quiet window. Only `git` is started as a real process
(and `sh` for the builder's scripts).

## What this plan takes from giantswarm/muster

`internal/testing` in giantswarm/muster was the first template. Taken from it, and kept:

- **Scenarios as data.** A new behavior or bug is a new YAML file, not new Go, loaded by a strict
  loader that filters by flow and tag.
- **Fakes at the process boundary**, with faults as scenario steps: a status for N requests,
  `Retry-After`, outages, delays.
- **One expectation checker.** Every step's response is adapted into one view, so no expectation
  kind is honoured for one step kind and silently ignored for another; unknown keys and
  assertion-free scenarios are errors (muster's lesson, and ours).
- **A structured reporter** with per-step timing: JSON for CI, text for people.

Not taken: the real process per scenario and the clock behind a control socket. They suit muster,
whose scenarios are a few requests to one MCP server. A beekeeper flow is many loop passes over
time, and every pass multiplies the cost of a process.

## What beekeeper has today

The plan unifies the existing harnesses instead of adding another:

- `internal/scenario` (`make test-scenarios`)
  **What it does:** the first scenario harness: the real binary per call in a namespace sandbox,
  fakes as processes on a world socket, a socket clock
  **Fate:** becomes the in-process driver; its process driver keeps only the smoke scenarios
- `cmd` `loopRig` (`cmd/supervisorloop_test.go`)
  **What it does:** the loop's `app` in-process with faked commands and pull reads
  **Fate:** stays: unit tests of the planner's small cases, and the proof that the loop runs
  in-process
- `internal/superloop/testdata/standstills`
  **What it does:** recorded planner inputs replayed through the pure planner, with negative
  controls in CI
  **Fate:** stays: planner-level replays
- `internal/loopworkflow` (5,200 lines, `make test-loop-workflow`)
  **What it does:** real binary, scratch Git origin, fake `gh` and `claude` behind one fixture
  directory
  **Fate:** its flows move into in-process scenarios, then it is retired
- `internal/testenv` (`TestSupervisorWorkflow`)
  **What it does:** real binary in user/mount/PID/network namespaces, fake `gh`, `claude`, Git
  remote helper
  **Fate:** its sandbox runs the smoke scenarios; its workflow moves into a scenario
- `internal/perfsuite` (`-tags perf`)
  **What it does:** real binary on a generated desk of realistic size; `gh`, `ssh`, harness fakes
  by argv[0]
  **Fate:** stays for the real-time budgets, including those the large-desk scenarios had; its
  desk generator is shared with scenarios

## Harness design

Package `internal/scenario` with build tag `scenario`, run by `make test-scenarios`
(`go test -tags scenario ./internal/scenario`) and the CircleCI job `test-scenarios` on every pull
request.

### One bubble per scenario

- The runner loads every scenario, then runs them one after another. Each runs inside
  `synctest.Test` with its own root (`t.TempDir()`): `HOME`, the XDG directories, a `config.yaml`
  from the scenario's `desk:`, a state store and event log (empty, seeded from a fixture or from
  perfsuite's generator), a bare Git origin per repository and a root per execution host.
- The `when` steps run in the bubble. `run` calls `cmd.Run` in a new process of the world and
  waits for its exit; `background` starts one and leaves it running; `stop` sends it SIGTERM
  through the seam and waits for its exit.
- When the scenario ends, the runner stops every process of the world. A goroutine still alive
  after that fails the scenario with its stack, so every process dies with its scenario as it
  did in the sandbox.
- One scenario at a time: the process environment and the seam's world belong to the test
  process, and the bubble keeps one scenario's goroutines and timers apart from the next one's.
  Caches that live for a process's lifetime (a `sync.Once`, a package variable holding a path or
  a reading, such as the state lock's command name `state.SetCommand` keeps) move onto the
  invocation. The processes of one scenario run at once in one address space (a background loop,
  a worker's `agents park`, a `run` step), so such a variable would be one process's for all of
  them, and the next scenario's too.

### The process seam: internal/proc

`internal/proc` already starts some commands (`proc.Command`) and reads the process table. It
becomes beekeeper's one view of its own process and of the processes it starts and finds:

- **Starts.** `proc.Command(ctx, name, args...)` returns a `*proc.Cmd` with `exec.Cmd`'s fields
  and methods (`Env`, `Dir`, the standard streams and pipes, `Start`, `Wait`, `Run`, `Output`,
  `Process`). The 123 `exec.Command` calls in 86 core files move to it.
- **The invocation.** `cmd.Run(ctx, proc.Invocation)` runs the command tree; `cmd.Main` builds the
  invocation from the operating system. The arguments, the environment (`Getenv`, `LookupEnv`,
  `Environ`, and `Setenv` and `Unsetenv`, which change the invocation's environment and what its
  children inherit), the working directory (`Getwd`, and `filepath.Abs`, which reads it for a
  relative path), the temporary directory, standard input, output and error, the process id and
  parent process id, the executable's path, the home directory and the version come from the
  invocation: from the context, or from the object built for the invocation (the app, the state
  store). That is 184 environment reads, 19 environment writes in 9 files (the gate's variables
  for devctl, `HOME` in `guard`, `CODEX_HOME`, `BEEKEEPER_CONFIG`), 57 `os.Environ`, 62
  `os.Getpid`, 83 uses of the standard streams, 44 `os.Executable`, 44 `os.UserHomeDir`, 26
  `os.Getwd`, 35 `filepath.Abs`, 6 `os.TempDir` and 35 reads of the version. In-process, a write
  to the test process's environment would reach every process of the world at once and none of
  the invocations that read their own, and a relative path would resolve against the test
  process's directory instead of the invocation's.
- **Signals and the process table.** `proc.NotifyContext` in place of `signal.Notify` (21 calls);
  sending a signal (`syscall.Kill` and `os.FindProcess` outside the started commands, 13 calls),
  `Alive`, `Started`, `Read` (the table the core finds its sessions, units and loops in), a
  process's environment and directory, and the core's own reads of `/proc` (33 in 17 files: a
  process's executable in `stalewatch`, `supervisor` and `gatetakeover`, its standard output in
  `gatemerge`, a peer's credentials in `sandbox` and `secret`). `internal/machine`'s readings of
  the machine's memory and load stay the machine's, as in the sandbox.
- **Replacing the process.** `proc.Exec` in place of `syscall.Exec` (5 calls: the loop's
  re-execution after an update on Linux and macOS, `sandbox`'s run of the caller's command and
  its gpg signing, and `guard run`). Under a world the process keeps its id and runs the new
  program; a direct call would replace the test process and end the suite.
- **What a bubble cannot wait on.** A goroutine blocked in a system call or in the network poller
  is not waiting on the fake clock, so while one is blocked the clock cannot move: a goroutine
  blocked in `accept` would stop the scenario's time for good. Under a world, Unix sockets
  (`proc.Listen` and `proc.Dial`, 24 calls) are in-memory pipes, a state change notification is
  the store's own instead of fsnotify's, a file lock is taken with a retry
  (`TryLockContext`), never a blocking `Lock`, and HTTP goes through `proc.HTTPClient`, whose
  transport hands the request to the world.
- **The guard.** `TestCoreUsesProc`, like the clock's guard, fails when a core package (`cmd`,
  `internal/...` and `pkg`, apart from `internal/proc` itself and the test harnesses) calls one of
  these operating system functions directly (the environment's reads and writes and
  `filepath.Abs` among them) or reads `/proc` itself. Each step of the move adds what it
  moved to the list.

A release build's implementation of every seam function is the call it replaces, so release
behavior does not change at any step. The seam is a context value and a few constructors, not a
build tag: the test binary installs the world. The `scenario` tag keeps selecting what it selects
today apart from the clock: the update's release source (a SHA-256 bundle instead of cosign's) and
the unit a process runs in. In-process, the unit comes from the world's process table.

### The clock

Go's `testing/synctest` (generally available since Go 1.25; `go.mod` pins 1.26) fakes the `time`
package inside a bubble: `Now`, `Sleep`, timers, tickers and `AfterFunc`. The fake clock moves
only when every goroutine in the bubble is durably blocked (on a channel, a timer, a sleep, a
condition), and then straight to the next time something is due. For the scenarios:

- The runner first moves the clock to `given.clock.start`, or to the system's time when the
  scenario starts, as today: one sleep before anything else runs.
- `advance: 30m` is a sleep of the runner. Every timer, ticker and sleep due in the 30 minutes
  fires at its own time and in order, each after the work the previous one woke is done. There is
  no acknowledgement, no 10-second wait and no `quiet` window, and a ticker misses a tick only
  as it does live: when its receiver waits on something else past the next one.
- `wait_for` and `settle` check their expectations after `synctest.Wait`, when the desk is idle,
  and else let the clock move by their `interval`. Their `timeout` is world time.
- CPU work takes no world time. A slow GitHub response, a slow host or a slow command is a delay
  of world time the world adds to that call (a fault), never real slowness.
- The scenario half of `internal/clock` (the socket, its server and the remote timers, about 700
  lines) and `TestMigratedPackagesUseTheClock` retire with the process driver: inside a bubble
  the `time` package itself is fake, including the about 400 calls of its clock that the core
  still makes. `clk` stays an alias of the `time` package until a cleanup inlines it.
- File modification times are the system's. A flow that compares one with the clock sees the
  world's offset, as it does today (see [Open points](#open-points)).

### The world

The world of the first harness moves into the bubble: one object that holds GitHub, the harness
sessions, the service units, the hosts and the call log. Its handlers stay; its transport changes.
`proc.Command` hands the world the call (program, arguments, environment, directory, standard
input) as a function call, where a fake executable dialed a socket. A `given.calls` entry still
answers first, then the program's model; a call nothing answers still fails the scenario, and the
call log and `calls` expectations do not change.

- **A process table.** Every program the core starts becomes a process of the world: process id,
  parent, command line, environment, directory, start time and exit code. `proc.Read` returns this
  table, so the core finds a session under its harness, a unit or a running loop as it does live,
  and `agents`, `capacity` and the loop's session sweep read the same table. Ending a process
  cancels its goroutine; a signal reaches its `proc.NotifyContext`.
- **`beekeeper` itself.** A start of the executable's path is a process that runs `cmd.Run` with
  the invocation the parent's `proc.Cmd` describes: its environment, directory and streams. A
  release is the same code with the release's version in the invocation. Its binary on disk is a
  stub file that names the version, which the update's download, verification and installation
  handle as bytes, and which a start of that path runs as that version.
- **`git`** runs as a real process with the scenario's `HOME`. The scenario's global Git
  configuration sets `url.<origin>.insteadOf` for `https://github.com/<owner>/`, so every fetch
  and push reaches the bare origins without a remote helper process.
- **Delays.** A `given.calls` entry and a GitHub fault may delay their answer by world time
  (`delay: 40s`). A delay on a `beekeeper` call is how a scenario states a slow command, such as
  the `supervisor status` that outlasts the probation probe.

### GitHub REST and GraphQL

The model of the first harness stays (`internal/scenario/fakegithub`): repositories, branches and
heads backed by the bare origins, issues, pull requests, reviews, comments, checks, workflow runs,
releases, labels, sub-issues and a Projects v2 board; budgets per bucket with GraphQL costs,
conditional reads with ETags and 304, faults and CI as part of the model. Only its transport
changes: a `gh` call goes to the translator as a function call and renders gh's output and exit
code as today, and the code that calls the API over HTTP gets `proc.HTTPClient`, whose transport
hands each request to the same handlers in memory. The contract test against the real `gh` stays.

### Harnesses

The fake `claude`, `codex`, `agy` and `omp` keep their protocols and the scenario's worker
scripts; they become processes of the world. The claude daemon, its pty host and the session's
CLI are processes of the table, as beekeeper finds them live; codex's JSON stream and omp's inbox
run over in-memory pipes. A script's steps call `beekeeper` and `git` through `proc.Command` with
the session's identity, as a tool call does. The outcomes stay: `finish`, `hang` (blocks until
stopped), `crash` and `budget`.

### SSH execution hosts

`ssh <host> <command>` runs the command as a process of that host: the host's home, configuration,
`PATH` and identity in the invocation, its root inside the scenario's. The faults stay
(`unreachable`, `host-key-changed`, `missing-tool`, `older-go`, `drops-source`), and `slow` is a
delay of world time. The builder's `kubectl` stays the world's model; its `exec` runs the desk's
upload, launch and follow scripts with the real `sh`, and the image's tools are shell scripts that
append their call to the call log.

### Isolation

No namespaces are needed, because nothing but `git` (and the builder's `sh`) runs on the host, and
only in the scenario's directories. The world looks both up once and starts them by their absolute
paths. Before the first scenario, the test process points its own `HOME`, XDG directories,
`TMPDIR` and `PATH` at an empty directory, so a call that bypasses the seam fails instead of
reading the person's home or temporary files or starting a program of the machine. The sandbox's
PID namespace kept a scenario's process ids apart from the machine's; in-process, the world's
process ids start above the kernel's highest (`pid_max`, at most 2^22), so a signal or a `/proc`
read that bypasses the seam finds no process instead of a person's real session or loop. The
guard test keeps such calls out of the core. No network is used: the world answers HTTP in
memory.

### Speed

- **Budget.** The in-process suite's wall time, excluding the build, stays under 5 s on CI's
  executor, and the job warns from 5 s. The full run, the in-process suite and the smoke
  scenarios on the real binary together, fails above 10 s, so no second budget lets a run of
  every scenario grow past a few seconds.
- **Per scenario.** A scenario's `timeout` is its real time, 1 s by default. A watchdog outside
  the bubble ends the run when a scenario exceeds it: it prints every goroutine's stack, writes
  the report with that scenario failed and exits, since a bubble whose goroutines do not return
  cannot be abandoned. A scenario that holds the clock still or loops is found in a second, not
  at the job's timeout.
- **Reporting.** The reporter keeps the real time and the world time of every scenario and step
  (JSON, JUnit and text) and the suite's wall time.
- **The known risk is `git`.** It stays a real process, a few milliseconds per call on CI, and a
  delivery flow calls it many times (worktrees, publishing, merge-result checks). The reporter
  counts the `git` calls and their real time per scenario from step 5 on.
- **When the budget fails,** the cost is profiled first: the configuration and state each command
  loads, and `git` calls the core repeats within a pass, which are cut in the core. Only after
  that may the runner split the scenarios across one worker process per CPU in the same job.

### Real-binary smoke scenarios

Some things only a process shows: the binary's build stamp, `main`'s dispatch, an install that
renames a new binary over the running one and re-executes it, the operating system's signals,
file locks between two processes and a harness found in the real process table. A scenario marked
`driver: process` also runs on the process driver of today, in `internal/testenv`'s sandbox, with
the fake executable on the world socket. Smoke scenarios have no `advance` step (the loader
refuses one there), so the socket clock is not needed:

- smoke/version, smoke/capacity-json-empty-desk and smoke/board-next-fake-board: the binary starts,
  reads its desk and the fake GitHub through `gh`.
- harness/claude-worker-parks-on-its-pull: a worker found in the real process table under the fake
  `claude`, parking with the real binary.
- selfupdate/binary-replaced-and-re-executed (new, release-installs-and-serves-probation without
  its probation): the installed file is replaced under a running loop, which re-executes as the
  new version.
- smoke/loop-stops-on-sigterm (new): a background loop stopped by SIGTERM exits 0 and leaves the
  state lock to the next command.

Each also runs in-process, so the smoke set proves the seams against the operating system. Its
run on the real binary, in the same job, counts toward the full run's 10 s (see [Speed](#speed)).

## Scenario format

Files under `internal/scenario/scenarios/<flow>/<name>.yaml`; [docs/scenarios.md](../scenarios.md)
is the reference as built. The loader is strict: unknown keys, an unknown step or expectation, a
scenario without a `then`, or a `wait_for` without a bound are errors, checked by a unit test that
loads every file.

```yaml
name: refill-304-uses-cached-pulls
description: >
  A 304 on the conditional open-PR read is not a failure, and one repository's
  failed observation does not hold starts in another.
issue: https://github.com/giantswarm/beekeeper/issues/3772
flow: refill
tags: [regression, github]

# Abridged: the given.calls of the watch's devctl and journalctl reads are left out.
given:
  desk:
    config:
      capacity: {ceiling: 4}
      github: {probeRepo: example/a}
  github:
    repositories:
      - name: example/a
        issues: [{number: 1, board: Ready}]
      - name: example/b
        issues: [{number: 2, board: Ready}]
    faults:
      - match: {path: "repos/example/a/pulls", conditional: true}
        respond: {status: 304}
      - match: {path: "repos/example/b/pulls"}
        respond: {status: 502}
        times: 3

when:
  - run: {args: [watch, --once]}
  - run: {args: [supervisor, loop, --once]}
  - advance: 2m
  - run: {args: [supervisor, loop, --once]}

then:
  - github: {calls: {path: "repos/example/a/pulls", status: 304, min: 1}}
  - worker: {name: a 1, turns: 1}
```

- **`given`:** `desk` (configuration keys, roster, providers and capacity), `state` (a fixture
  or generator size), `github` (repositories, issues, board, PRs, checks, budgets, faults),
  `calls`, `harness` (worker scripts), `hosts`, `builder`, `releases` and `clock` (start time).
- **`when` steps:** `run`, `background`, `stop`, `wait_for`, `github`, `fault`, `host` and
  `advance`, as [docs/scenarios.md](../scenarios.md#steps) describes them.
- **`then` and inline `expect`:** `exit`, `output`, `stderr`, `json_path`, `duration`, `events`,
  `calls`, `process`, `github`, `worker`, `origin` and `log`. Every step kind is checked by one
  function on one response view.
- **`pending: <issue URL>`** marks a scenario of an open bug: the runner expects it to fail and
  reports it as pending; once it passes, the run fails and asks for the marker's removal.

What the in-process run changes in the format:

- `duration` measures world time. Real-time cost belongs to perfsuite, which already budgets
  `capacity`, `supervisor status` and `budget` on the desk of record; the large-desk scenarios
  keep their behavior and leave their real-time ceilings there.
- `timeout` is the scenario's real time, 1 s by default.
- `advance` loses `quiet`, since the bubble is idle whenever the clock moves; `step` and `settle`
  stay.
- `release:` on `run` and `background` runs that release's version in-process.
- `driver: process` marks a smoke scenario, which runs on both drivers.
- `delay` on a `given.calls` entry, as on a GitHub fault: world time before the answer.

## First core flows

Each is one or more scenarios in its own directory:

1. **Delivery:** board item → `agents start` → branch and PR → CI pending then
   green → park on desk review → reviewer approves the head → merge go for
   that head → merge → issue closed → board Done. Variants: red CI hands the
   PR back; a push after approval; a stale approval does not merge.
2. **Refill:** free slots and ready items start workers up to the ceiling,
   respecting providers' capacity picks, the room cap and per-repository
   holds; a busy worker stream does not stop the refill.
3. **GitHub floor and budget holds:** a bucket under `github.floor` holds
   GitHub work and starts until reset, then resumes; the loop's own reads stay
   inside their share; a secondary rate limit backs off.
4. **Duplicate-PR prevention:** a continuation (local or on an execution host)
   continues the open PR of its task; after a merge, superseded PRs of the
   same task are closed.
5. **Merge-result check:** the merged result is checked on the exact head
   before the go; an environment finding (missing tool) is not a code
   finding; a red result gives no go.
6. **Self-update:** a newer release is staged, its canary passes, it is
   installed, it serves probation, and a candidate whose loop sends no
   heartbeat is rolled back with the reason named; the state fence clears
   after a compatible release passes.
7. **Guide relay, notes and answers:** a note for the person, an answer
   recorded, the waiting worker woken; a relay hands a session over and keeps
   its model.
8. **Disk and load guards:** low disk prunes without asking; load above the
   guard holds starts and releases them.

## Tonight's bugs as regression scenarios

- https://github.com/giantswarm/beekeeper/issues/3738 duplicate PRs per continuation
  **Scenario:** an open PR from one execution; a continuation on an execution host publishes; then one
  open PR for the task, and after its merge none of the task's other PRs stays open
- https://github.com/giantswarm/beekeeper/issues/3772 HTTP 304 held every start
  **Scenario:** the scenario in the format section: a 304 uses the cached read; another repository's 502
  holds only that repository
- https://github.com/giantswarm/beekeeper/issues/3769 loop drains GraphQL
  **Scenario:** one simulated hour of loop passes on a desk of realistic size: the loop's own GraphQL
  spend leaves the budget above `github.floor`, and starts are never held by the loop's own reads
- https://github.com/giantswarm/beekeeper/issues/3636 refill starved by a busy stream
  **Scenario:** a worker stream held by a slow GitHub response (8 s of world time): the refill still
  starts a worker within one pass interval, and no process holds the state lock across the slow call
- https://github.com/giantswarm/beekeeper/issues/3742 merge-result checker without the linter
  **Scenario:** the check's host lacks `golangci-lint` (exit 127): the finding is an environment
  finding, the check is not dispatched there again, and the loop still reaches a go on green CI for
  the exact head
- https://github.com/giantswarm/beekeeper/issues/3345
  and https://github.com/giantswarm/beekeeper/issues/3657 canary killed by a slow `supervisor status`
  **Scenario:** self-update on a large generated desk where `supervisor status` takes longer than the
  probe (a delay of world time): probation is not failed by a slow but healthy loop, and a real
  failure is rolled back with its reason; perfsuite keeps `supervisor status` and `capacity --json`
  under their real-time budgets

Open bugs land their scenario with `pending:`; the fix removes it.

## The rule for fix PRs

Written into `AGENTS.md` ("This repository") and the review skill
(`plugin/skills/review/SKILL.md`):

> A pull request that fixes or changes behavior of beekeeper's core adds or
> extends a scenario under `internal/scenario/scenarios/` that fails without
> the change. A bug's scenario names the issue. Reviews reject a core fix
> without one; a change that cannot be expressed as a scenario says why, and
> the harness gains the missing step in a follow-up.

CI enforces the "fails without the change" with the `scenario-control` step: each scenario the
pull request adds or changes must fail on the pull request's merge base on an assertion (not on
loading or a build error), or say why it cannot with `control: none` and a reason, which the
review checks. In-process, the harness is linked with the core, so the step applies the pull
request's `internal/scenario` tree and scenarios to a checkout of the merge base and runs them
there with `go test`, as the Go-test control applies test files. A merge base the pull request's
harness does not build against is reported as no evidence, as a base that does not build is
today. A run takes milliseconds, so the step costs about one build of the base.

## The move

Each step is one pull request under https://github.com/giantswarm/beekeeper/issues/3954, green on
the full suite and parked for review. While both drivers exist, a scenario runs in-process once
its flow has moved and on the process driver until then, and CI runs both. A moving scenario keeps
its `given`, `when` and `then`, apart from the format changes above; a scenario whose behavior
expectation changes is a finding, filed as its own issue. Steps 2 to 4 change no behavior: the
existing suite stays green on them, and the guard test is their control.

1. **This plan.**
2. **Starts through `internal/proc`.** `proc.Cmd` and `proc.Command`, the 123 `exec.Command`
   calls of the core, and `TestCoreUsesProc` with `os/exec` on its list.
3. **The invocation through `internal/proc`.** `cmd.Run(ctx, proc.Invocation)`; the environment
   (read and written), directory (relative paths included), temporary directory, standard
   streams, process ids, executable, home directory and version taken from it; process-lifetime
   caches onto it; the guard's list grows. Split by package group (`cmd` first, then `internal`)
   where one pull request would be too large to review.
4. **What a bubble cannot wait on, through `internal/proc`.** Signals, the process table and the
   core's own reads of `/proc`, Unix sockets, state change notifications, blocking file locks, the
   HTTP transport and every `syscall.Exec`, the loop's re-execution among them.
5. **The in-process driver.** The bubble, the world's in-process transport and process table,
   GitHub in memory, the watchdog, the reporter's world time and the per-scenario `driver`. The
   28 scenarios without worker sessions, hosts, a builder or releases move: smoke, github, clock
   and usage, and those of board, budget, refill, guards, guide and regression, the loop hour
   among them.
6. **Harnesses and units in-process.** The harness CLIs and the service manager as processes of
   the world. The 32 scenarios with worker sessions on the desk move: delivery, duplicate-pr,
   merge-result, findings, reports and the local harness scenarios, and the rest of board,
   budget, refill, guards and guide.
7. **Hosts, the builder and releases in-process.** `ssh` as a host's process, the builder's
   scripts, releases as versioned invocations. The last 15 move: the execution-host scenarios,
   selfupdate and the remaining regression scenarios; the large-desk real-time ceilings leave
   for perfsuite.
8. **The process driver keeps only the smoke scenarios.** The socket clock goes, and the two new
   smoke scenarios come. The sharding of https://github.com/giantswarm/beekeeper/pull/3911 is
   reverted: one container, no `BEEKEEPER_SCENARIO_SHARD`. The job enforces the budget, and
   [docs/scenarios.md](../scenarios.md) and [docs/scenarios-github.md](../scenarios-github.md)
   describe the in-process run.

## Order of the refactor

Only after the flows and regressions above are green on main, and one seam per
pull request. A split moves code and changes no behavior: no scenario, golden
file or fixture changes in the same pull request, and the suite green before
and after.

1. `internal/clock` and the GitHub client seams (done by the harness work); `internal/proc`
   (done by [the move](#the-move)).
2. **`cmd/supervisorloop.go` (5,266 lines) → `internal/loop`:** pass
   orchestration, worker stream, refill and alarms. The first core split; the
   epic is done when it has merged with the suite green.
3. `cmd/capacity.go` → `internal/capacity`, `cmd/gate.go` → `internal/gate`.
4. `internal/state/state.go` → store, lock, events and snapshot, then
   `internal/config/config.go` by section.
5. `cmd/watch.go` and `cmd/agentstart.go` → `internal/watch` and
   `internal/start`; `internal/superloop/superloop.go` by planner stage.

## Sub-issues

In order, each with its own "Done when":

1. Harness core: runner, strict loader, reporter, sandbox, world server, CI job — https://github.com/giantswarm/beekeeper/issues/3789
2. Fake GitHub: REST and GraphQL model, `gh` translator, budgets, ETags, faults — https://github.com/giantswarm/beekeeper/issues/3790
3. Fake harnesses and SSH execution hosts — https://github.com/giantswarm/beekeeper/issues/3791
4. Controllable clock: `internal/clock` and migration of the core flows — https://github.com/giantswarm/beekeeper/issues/3792
5. Scenarios: delivery, refill, duplicate PRs and merge-result check — https://github.com/giantswarm/beekeeper/issues/3793
6. Scenarios: budget holds, self-update, guide relay and notes, disk and load guards — https://github.com/giantswarm/beekeeper/issues/3794
7. Regression scenarios for the bugs of 2026-11-11/12 — https://github.com/giantswarm/beekeeper/issues/3795
8. The rule: AGENTS.md, review brief and the `scenario-control` CI step — https://github.com/giantswarm/beekeeper/issues/3796
9. Move `internal/loopworkflow` and `testenv`'s workflow into scenarios and retire them — https://github.com/giantswarm/beekeeper/issues/3797
10. First core split: `cmd/supervisorloop.go` → `internal/loop` — https://github.com/giantswarm/beekeeper/issues/3798
11. Further core splits: capacity, gate, state, config, watch, start — https://github.com/giantswarm/beekeeper/issues/3799
12. A full run in a few seconds: the in-process redesign and [the move](#the-move) — https://github.com/giantswarm/beekeeper/issues/3954

Sub-issue 12 replaces the process parts of 1 (the sandbox and the world socket for every
scenario), 3 (fakes as processes) and 4 (the socket clock); what they built otherwise stays.
Sub-issue 9 moves onto the in-process driver.

## Open points

- **The seams' fidelity.** What the in-process run fakes differently from the operating system
  shows only on the smoke scenarios. A live bug that escapes because of a seam gains a smoke
  scenario as well as its in-process one.
- **File modification times** are the system's while the clock is the world's. A flow that
  compares the two (a heartbeat file's age) sees the world's offset, as in the first harness; it
  moves to a time the state records where a scenario needs it.
- **What `synctest` refuses** (a channel created outside the bubble, a goroutine left in a system
  call) is fixed in the core where it is a defect, such as a lock held across a sleep, and in the
  seam otherwise.
- **The fake `gh` versus the real `gh`:** the translator stays, guarded by the contract test; if
  that test shows drift often, revisit.

## Refill regression before the scenario harness

The regression for https://github.com/giantswarm/beekeeper/issues/3636 is covered in
`cmd/supervisorfillcadence_test.go`: given a blocked pull observation and free capacity,
a refill tick starts board workers before the observation is released. Companion cases
check hold recovery and the action named by the stall alarm. These run in the existing
in-process loop rig; port this behavior to the scenarios when the harness lands.
