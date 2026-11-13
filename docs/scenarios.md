# Behavioral scenarios

A scenario states one behavior of beekeeper as given, when and then in YAML,
and runs it against the real `beekeeper` binary on a desk of its own. A bug
found live becomes a scenario that fails before its fix. The plan is
[docs/plans/3788-behavioral-scenarios.md](plans/3788-behavioral-scenarios.md).

## Running them

```sh
make test-scenarios                              # every scenario
BEEKEEPER_SCENARIO_FLOW=smoke make test-scenarios  # the flows named (comma-separated)
BEEKEEPER_SCENARIO_TAG=board make test-scenarios   # the scenarios with any of these tags
BEEKEEPER_SCENARIO_REPORT=/tmp/scenarios make test-scenarios  # write the report and artifacts
```

The suite needs Linux with unprivileged user, mount, PID and network
namespaces (`unshare -Urmpnf`), and refuses to run without them: it never
runs against the host. Ubuntu restricts them through AppArmor
(`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`).

CI's `test-scenarios` job runs the suite on every pull request on a machine
executor (Docker's seccomp profile refuses the namespaces). The suite fails
when its wall time exceeds 10 minutes and warns from 8. The job's artifacts
hold `report.txt`, `report.json` (every scenario, step and duration, and the
suite's wall time), `junit/scenarios.xml` (one case per step) and each
scenario's own artifacts under `<flow>/<name>/`: the world's call log
(`calls.jsonl`), every step's output, background logs and the driver's log.

## The control: fails without the change

A pull request that fixes or changes behavior of beekeeper's core adds or extends a scenario that
fails without the change (the rule in [AGENTS.md](../AGENTS.md)). The `scenario-control` step of
the `test-scenarios` job (`.circleci/scenario-control.sh`) checks the "fails without": it builds
the candidate from the pull request's merge base with `main` and runs against it, with the pull
request's harness, only the scenarios the pull request adds or changes. Each must fail there on
an assertion: an expectation, an unanswered call or its timeout. The step fails for a scenario
that passes on the base, since it shows nothing the change does, for one whose run on the base
fails in the harness (the sandbox, the world, the desk's setup), which is no evidence, and when
the merge base does not build.

- **Added** is a scenario whose name and file the base does not have. **Changed** is one whose
  `given`, `when`, `then` or `timeout` differ from the base's, found by name or else by file.
- A change of only `name`, `description`, `issue`, `flow`, `tags`, `pending`, `control`, comments
  or layout is **metadata only** and does not run. Removing `pending` with a fix is one: the
  base's own suite showed the scenario failing. A new `pending` scenario fails on the base as on
  the branch, so it passes.
- A scenario that cannot fail on the base, such as one that states behavior main already has,
  opts out with the reason, which the review checks:

  ```yaml
  control:
    none: states the refill behavior main already has; this change only adds the scenario
  ```

  The opt-out holds for the change that writes it. When a later pull request changes the
  scenario's `given`, `when`, `then` or `timeout` and leaves `control` as it is, the scenario runs
  against that pull request's base again; a reason written for one change does not cover the
  next.

### Go tests

A fix whose regression test is a Go test, not a scenario, needs the same proof. The same step
(`internal/testcontrol`) takes the `Test…` functions the pull request adds or changes in any
`_test.go`, applies the pull request's test files (its `_test.go` files and the files under
`testdata/`, as it adds, changes or deletes them) to a checkout of the merge base, and runs those
tests there with `go test`. The ones that fail on the base run again on the pull request's code,
where they must pass: a test that fails on both shows nothing. At least one test must fail on the
base and pass on the change; the step fails when none does. A pull request with both scenarios
and Go tests passes both parts.

- **Added** is a test whose package on the base has no test of its name. **Changed** is one whose
  code differs from the base's; its doc comment, comments and blank lines do not count. A test the
  pull request moves unchanged from another package is **moved** and does not run.
- A test runs with the fewest of the build tags CI tests with (`scenario`, `integration`, `perf`)
  that build its file. One that builds only with another tag, such as a live test, is listed and
  does not run.
- A package that does not build on the base is reported with its compiler errors. Its tests show
  nothing, as a test the base skips, or whose package's `TestMain` fails, shows nothing.
- The tests of a new package, whose Go code the base does not have, are listed and do not run; a
  package of tests only (such as one that tests a script or a Makefile) runs, new or not. Nor does
  any test of a pull request that changes only its test files and test data: any other changed file
  counts as a change, because a test may read it at runtime.
- A test that cannot fail on the base, such as one of behavior main already has or one of code the
  base lacks, opts out with a paragraph of its doc comment that gives the reason, which the review
  checks:

  ```go
  // TestDoubleFlag checks --double.
  //
  // control: none: --double is new, so the base cannot parse it
  func TestDoubleFlag(t *testing.T) {
  ```

  The opt-out holds for the change that writes it, as a scenario's does. The step passes when each
  new and changed test opts out or is not run, and fails on a control paragraph it cannot read.

The step passes at once, without a build, on a branch that adds or changes neither a scenario nor
a `_test.go` file, on `main` and on tags. The job's artifacts hold its log (`control.log`), the
scenarios' report (`control/report.txt` and `control/report.json`: every scenario with its change,
verdict, first failures on the base and opt-out reason) with each run's artifacts under
`control/<flow>/<name>/`, and the Go tests' report (`go-control/report.txt` and
`go-control/report.json`: every test with its change, tags, run on the base and on the change,
first failures or build errors and opt-out reason) with each `go test` run's command, events and
stderr under `go-control/runs/`. A pull request that changes the Go-test control first runs it on
the clock fix of https://github.com/giantswarm/beekeeper/pull/3832, whose regression test must
fail on the fix's parent (`go-control-history/`). To run the step on a branch:
`bash .circleci/scenario-control.sh` (Linux with namespaces, as for the suite).

## How a scenario runs

1. The suite builds the candidate once with `-tags scenario`.
2. Each scenario gets a fresh root and runs in parallel with the others. Its
   driver (the test binary) enters `internal/testenv`'s sandbox: new user,
   mount, PID and network namespaces, a filesystem with read-only system
   directories and the scenario's `/work`, no host home, no network and no
   inherited credentials. When the scenario ends, the kernel kills every
   process it started.
3. The driver writes the desk: `/work/config.yaml` (a base configuration
   overlaid by `given.desk.config`), a state store, `HOME` and the XDG
   directories, and a bare Git origin with one commit on `main` for each of
   `given.repositories` and of `given.github`'s repositories under
   `/work/origins/<owner>/<name>.git`, and a root for each of
   `given.hosts` (see [Execution hosts](#execution-hosts)).
4. It starts the scenario's **world** on a Unix socket. `PATH` holds only
   the candidate, `git`, `sh` and the fakes: `gh`, `claude`, `codex`,
   `agy`, `omp`, `ssh`, `devctl`, `systemctl`, `systemd-run` and
   `journalctl`. They are one executable that dispatches on its name and
   hands each call to the world. The world logs every call in order. A
   call that no `given.calls` entry answers and no fake models (see
   [the fake machines](#the-fake-machines)) gets exit 1 and **fails the
   scenario**. With `given.github`, `gh` and the GitHub API are answered
   by a model instead: see [The scenario's GitHub](scenarios-github.md).
   Beside it the world serves its clock (see [The clock](#the-clock)),
   which every process reads.
5. It runs the `when` steps, checks each step's inline `expect`, then the
   `then` expectations, and stops every background process by PID.

## Writing one

Put a file at `internal/scenario/scenarios/<flow>/<name>.yaml`. The loader
is strict. A scenario fails to load when it has an unknown key, step or
expectation, has no `when` or no `then`, has a step or expectation with more
or fewer than one kind, has a `wait_for` or a `settle` without a timeout, or
has a worker script that names an unknown harness, outcome or saved name. A
unit test loads every file.

```yaml
name: board-next-fake-board          # unique, lower-case and dashes
description: board next picks the one Ready issue of a fake board.
flow: smoke                          # the directory it lives in
tags: [smoke, board]
issue: https://github.com/giantswarm/beekeeper/issues/3789   # optional
timeout: 30s                         # real time; default 60s
# pending: https://github.com/giantswarm/beekeeper/issues/<n>  # an open bug

given:
  desk:
    config:                          # laid over the base config, map by map
      capacity: {ceiling: 4}
  repositories:
    - name: example/app
  calls:
    - program: gh
      # Every pattern must match one argument whole (a regular expression;
      # . matches newlines). Arguments may come in any order.
      args: [api, graphql, 'query=.*items\(first:80.*']
      respond: {stdout: '{"data": …}', stderr: '', exit: 0}
      times: 1                       # answer at most once; default always
  clock:
    start: "2026-11-12T09:00:00Z"   # the world's time at the start; default now
  tools: [go, golangci-lint]         # further programs on PATH, answered by given.calls only

when:
  - name: next
    run: {args: [board, next, --json], env: {}, stdin: ''}
    expect:
      - exit: 0
  - name: loop
    background: {args: [supervisor, loop]}
  - wait_for:
      timeout: 10s                   # required
      interval: 200ms                # default 100ms
      expect:
        - events: {contains: [supervisor.end]}
  - advance: 30m                     # the world's clock, timers due fire
  - stop: loop

then:
  - {step: next, json_path: {path: pick.ref, equals: example/app#1}}
  - calls: {program: gh, args: [graphql], min: 1}
  - process: {name: loop, running: false}
```

### Steps

| Step | Does |
| --- | --- |
| `run` | runs the candidate with `args` (and `env`, `stdin`) to its exit; `release` runs that release's build |
| `background` | starts the candidate and leaves it running; needs a `name`; `release` as for `run` |
| `stop` | stops the named background step by PID (SIGTERM, then SIGKILL) |
| `wait_for` | polls its expectations until all hold, bounded by `timeout` |
| `github` | something that happens on [the scenario's GitHub](scenarios-github.md#steps) |
| `fault` | arms or clears a [fault](scenarios-github.md#faults) of the scenario's GitHub |
| `advance` | moves the world's clock forward and wakes what is due ([The clock](#the-clock)) |
| `host` | changes an execution host's faults: `fault`, `slow` or `clear`, see [Execution hosts](#execution-hosts) |

### Expectations

Each expectation has exactly one kind. One checker reads one view of a step
for every step kind. An expectation of something the step does not observe,
such as the `exit` of a `wait_for`, fails instead of passing.

| Kind | Checks | Observed for |
| --- | --- | --- |
| `exit` | the exit code | `run` |
| `output`, `stderr` | `contains`, `not_contains`, `matches` (a regular expression) | `run` |
| `json_path` | `path` (`a.b[0].c`, `.` for all) of the JSON output: `equals`, `exists` or `len` | `run` |
| `duration` | the step's real time is at most `max` | every step |
| `events` | the desk's event log `contains` / `not_contains` verbs | every step |
| `calls` | the world's calls of `program` matching `args` number at least `min`, at most `max` | every step |
| `process` | a background step is `running`, or exited with `exit` | every step |
| `github` | [the scenario's GitHub](scenarios-github.md#expectations): calls, budgets, a pull request, an issue | every step |
| `worker` | what the sessions named `name` did: the `turns` they began, the last `outcome` (none while a begun turn has not reported), whether a CLI is `running` | every step |
| `origin` | a `branch` of `repository`'s origin: whether it `exists`, how many commits it is `ahead` of `main` | every step |
| `log` | what the background step `name` wrote so far: `contains`, `not_contains`, `matches` | every step |

In `then`, an expectation reads the step named by `step`, or the last step.
A `worker` expectation may also name the `harness` and the `host` of the
sessions; `name` and an `origin`'s `branch` are regular expressions that
match the whole name. A `log` reads a background step's standard output and
error as one text: the lines a running loop and its watch say
(`SUPERVISOR STARTS HELD`, `HIGH LOAD`), which no event records.

### The clock

Every process of a scenario reads the world's time: the candidate is built
with `-tags scenario`, and `BEEKEEPER_SCENARIO_CLOCK` names the world's clock
socket. The core flows read the time through `internal/clock`, never through
the `time` package's `Now`, `Since`, `Until`, `After`, `Sleep`, `NewTimer`,
`NewTicker` or `AfterFunc`. A release build uses the system clock and never
reads the variable; a test checks that.

The world's time starts at `given.clock.start`, or at the system's time. It
runs as the system's does, so a pass that waits a second waits a second.
`advance` moves it forward. The step returns once every process has fired
the timers, tickers and sleeps that are then due and acknowledged the new
time. A process that ends before it acknowledges is not waited for; one
that does not acknowledge within 10 seconds fails the step, named by its
PID and command line. A process started later reads the advanced time. A
ticker that missed several periods ticks once, as Go's does.

```yaml
- advance: 30m                       # at once
- advance:
    by: 1h
    step: 5m                         # in 12 steps
    quiet: 100ms                     # before each step and after the last
    settle:                          # after each step, as a wait_for
      timeout: 5s
      interval: 20ms
      expect:                        # calls counted from that step on
        - calls: {program: gh, min: 1}
```

With `step`, the clock moves in steps. After each step, `settle` waits until
the work the step woke has happened, before the next step. Its `calls`
count only the calls since that step. With `quiet`, each step first waits
until the desk has been quiet that long: no process armed a timer on the
clock and no fake was called. The work the last step woke is then done and
waits on the clock again, so the step reaches it; without `quiet`, a timer
armed a moment after a step would wait for the next one. `quiet` is at most
5 s; a desk that does not rest that long within 30 s fails the step, which
names how long ago it was last busy. Each step goes to
the scenario's `clock.jsonl` artifact: the world's time, the processes that
acknowledged it, how many timers fired, how long it took and how long it
waited for quiet.

`internal/clock`'s `TestMigratedPackagesUseTheClock` lists the migrated
flows (the supervisor loop and its timers, capacity, the GitHub budget, the
gate, self-update and probation, and the state) and fails when one of their
files calls the `time` package's clock again. To migrate a flow, replace
those calls with `internal/clock`'s (imported as `clk` in `cmd`, whose
`clock` function formats times) and add its files to that list.

### Driving the supervisor loop

A scenario drives the loop pass by pass: `run: {args: [supervisor, loop,
--once]}` is one pass, a step with its own exit code and output (the
`SUPERVISOR …` lines). Three things hold for such a scenario:

- A pass starts no worker and no reviewer before the watch has read the
  GitHub budget and the machine (`SUPERVISOR STARTS HELD`). Poll the watch
  once first, with `run: {args: [watch, --once]}`. Its budget probe reads
  the repository `github.probeRepo` names, so name one of the scenario's;
  its `devctl auth status` and its journal reads (`journalctl -k`,
  `journalctl -u systemd-oomd`) are answered in `given.calls`. The reading is
  good for five minutes of the world's clock, so keep the advances between
  the passes short.
- What a pass started runs after the pass returned. Wait for a worker's turn
  with a `wait_for` on its `worker` expectation before the next pass reads
  what it did.
- The refill names a worker after its item, `<repository> <number>`
  (`app 1`), and the loop names a reviewer `Review <repository> <pull> r<round>`:
  worker scripts match those names. A worker that needs a second turn runs on
  a headless harness (`codex`), where the loop's continuation is a fresh
  session's next turn. The desk's codex has a model catalog
  (`~/.codex/models_cache.json`) with the models of its codex choices, which
  the loop's `supervisor.effort` is validated against.

### Releases and self-update

`given.releases` gives a scenario beekeeper's own releases, so that
`self-update` runs for real: the lookup, the download, the verification,
the canary, the replacement, probation and the recovery controller.

```yaml
given:
  releases:
    installed: v1.0.0-rc.1            # the desk runs this release's build
    published: [v1.0.0-rc.2, v1.0.0-rc.3]   # on the scenario's GitHub
when:
  - run: {args: [self-update, --version, v1.0.0-rc.2]}
  - run: {release: v1.0.0-rc.1, args: [version]}   # a process still on a release
```

- **A release is a build.** The suite builds the candidate once more for
  each version, stamped as the release pipeline stamps it
  (`-ldflags -X …/pkg/project.version=<version>`), once per run. The builds
  are in the sandbox under `/work/releases/<version>/`; `installed` is
  `/work/bin/beekeeper`. Without `installed` the desk runs the candidate,
  whose version is the checkout's.
- **Published releases** are releases of `giantswarm/beekeeper` on the
  scenario's GitHub (`given.github` is created for them when the scenario
  has none): this platform's binary and its bundle as assets, and a
  pre-release where the version has a pre-release part.
- **A scenario build asks the scenario's GitHub** for releases
  (`BEEKEEPER_GITHUB_API`) and verifies a download against a scenario
  bundle, the binary's SHA-256, where a release build verifies the cosign
  bundle of a CircleCI build. A release build has neither seam
  (`internal/update/source_release.go`).
- **The recovery controller** of a watched candidate is a unit the updater
  asks the service manager for (`beekeeper-recovery.service`). It evaluates
  once a minute, and one evaluation with its isolated workflow probe takes
  longer than a step of the world's clock, so evaluations would be dropped.
  A scenario therefore answers that `systemd-run` call in `given.calls` and
  runs the unit's command itself, as a background step of the last known
  good release with the controller's own cadence flags:
  `background: {release: <last known good>, args: [self-update, --recovery-controller,
  --recovery-target, /work/bin/beekeeper, --recovery-every, 500ms, --recovery-step, 1m]}`.
  Each evaluation then counts for a minute of probation and comes every
  500 ms; the controller exits once it has promoted or rolled back, which
  `process: {name: …, exit: 0}` waits for. A loop that owes the heartbeat
  runs with `supervisor.loopInterval: 1s` and writes it with every pass:
  it is there once the loop's log has its first `SUPERVISOR REFILL` line.
- **Each published release is a commit** of its own on the repository's
  `main`, in the order of `published`: the lookup takes a tag that shares
  its commit with an older release for a stale one.
- **A running loop installs a newer release itself** at its first pass.
  Where a scenario installs by hand, a fault answers that loop's lookup
  (`match: {path: 'repos/giantswarm/beekeeper/git/matching-refs/.*', caller: <loop>}`).
- **Use release candidates** for a rollback. A full release that saved the
  state stamps it as its writer, and a rollback to an older release is then
  refused; a pre-release neither stamps the state nor is judged by the
  stamp.

### Pending scenarios

`pending: <issue URL>` marks the scenario of an open bug. The run expects
the scenario to fail and reports it as pending. Once the scenario passes,
the run fails and asks for the marker's removal, so the fix removes the
marker in the same pull request. A failure of the harness itself, such as
no sandbox, fails the run even for a pending scenario.

## The fake machines

`internal/scenario/harness` models the programs beekeeper runs its workers
with. A fake is a real process named as the real CLI is, because beekeeper
finds its sessions in the process table. A `given.calls` entry still wins
over a model, so a scenario can override any call.

- **`claude`**
  - `--bg … -- <brief>` starts a background session: a `claude daemon` runs
    the session's CLI under a `bg-pty-host`, with the CLI's record and
    transcript under `~/.claude`.
  - `--bg --resume <id> <message>` continues a stopped session,
    `agents --json [--all]` lists them and `stop <id>` ends one.
  - `-p … <prompt>` is a headless turn, and
    `--print --input-format stream-json` an execution host's managed session.
  - `-p /usage`, `plugin list`, `auth status` and `--version` answer the
    probes.
- **`codex`**: `exec --json … -` and `exec resume --json … <thread> -` under
  beekeeper's `codex-run`, with `thread.started`, `turn.started`, the items
  and `turn.completed` with its tokens. `app-server` answers the usage probe,
  or refuses a stretch of its requests (`harness.usage.codex.fail`);
  `--version` and `login status` answer a host's probes.
- **`agy`**: `--input-format stream-json --output-format stream-json` under
  `agy-run`, with `init` naming the conversation, a `user_input` step per
  message and `result`. Also `models` and `--version`.
- **`omp`**: `--mode rpc` with its standard input on its inbox, one turn per
  `steer` command. Also `models --json` and `--version`.
- **`systemd-run`, `systemctl`**: the user's service manager. The world runs
  each transient unit as its own child and answers `show -p ActiveState`,
  `list-units`, `stop`, `freeze`, `thaw`, `reset-failed` and
  `is-system-running`; `--scope` runs the command in place. A unit's
  processes carry its name in `BEEKEEPER_SCENARIO_UNIT`, which a build with
  the `scenario` tag reads where a release build reads the cgroup: a sandbox
  can create none.
- **`ssh`**: maps a destination to its host's root (`given.hosts`) and runs
  the remote command there under the host's environment; the name
  `coordinator` leads back to the desk. A host's fault ends the call as
  OpenSSH ends it.
- **`git`**: the real git with one transport replaced.
  `https://github.com/<owner>/<name>.git` leads to the bare origin
  `/work/origins/<owner>/<name>.git`. Beekeeper names a repository by that
  URL wherever it fetches or pushes itself, so every transport of a scenario
  ends in the same origins.

The fakes pass [the world's clock](#the-clock) on. An execution's runner
starts its harness with a cleaned environment, without
`BEEKEEPER_SCENARIO_CLOCK`; the fake names the clock again, so the
`beekeeper` a worker script calls reads the world's time on every machine.
What the fakes write themselves carries the system's time: a session's
record and transcript, and the reset times a usage probe reports.
`given.clock.start` and `advance` do not move those yet.

Not modeled yet: a message to a running `claude` session (`agents wake` of a
live background session) and the desktop app. `gh` is not a fake of this
package: with `given.github`, [the scenario's GitHub](scenarios-github.md)
answers it; without, a scenario answers the `gh` calls of its flow in
`given.calls`. A host's pinned `gh` answers only the host's probes itself.

### Worker scripts

`given.harness.workers` says what the sessions do. A session runs the first
script whose `name` (a regular expression matching the whole name),
`harness` and `host` match; a session without a script ends its turn idle.

```yaml
given:
  harness:
    usage:                           # what a harness's usage probe reports
      claude: {session: 40, week: 20}  # percent of the 5-hour and 7-day windows
      codex:
        session: 10
        week: 5
        fail:                        # the account refuses a stretch of the requests
          after: 1                   # once this many are answered (0)
          times: 2                   # this many (0: every one after); then it answers
          message: 'failed to fetch codex rate limits: GET https://chatgpt.com/backend-api/wham/usage failed: 401 Unauthorized; content-type=; body='
          code: -32603               # the JSON-RPC error code (-32603)
          # hang: true                 # instead: no answer; a helper keeps the output open
    models:                          # what agy models and omp models list
      agy: [gemini-3-pro]
    workers:
      - name: app-worker             # default: every session
        harness: claude              # default: every harness
        host: builder                # default: every machine; local is the desk
        repeat: false                # repeat the last turn beyond the list
        turns:
          - steps:
              - git: [clone, --quiet, https://github.com/example/app.git, app]
              - write: {path: app/change.txt, content: "a change\n"}
              - git: [commit, --quiet, -am, "feat: a change"]
                dir: app             # relative to the session's directory
              - gh: [pr, create, --repo, example/app, --head, work, --title, t, --body, b]
                save: pull           # the trimmed output, as ${pull} later
              - beekeeper: [agents, park, --on, '${pull}', desk review]
              - run: [sh, -c, 'exit 3']
                exit: 3              # the exit code the step must end with (0)
            outcome: finish          # finish, hang, crash or budget
            tokens: {input: 1200, output: 300}
            usage: {session: 60, week: 25}  # the probe's answer from this turn on
            reply: done
          - outcome: crash
            exit: 3                  # a crash's exit code (1)
```

A step runs as the session's tool command: in its directory, under its
identity (`CLAUDE_CODE_SESSION_ID`, the runner's `BEEKEEPER_*_AGENT`), with
the real `beekeeper` and `git`. `${name}`, `${session}`, `${harness}`,
`${dir}`, `${turn}` and `${host}` are the session's own. A step that ends
with another exit code than its script says stops the script and **fails
the scenario**: a refused park is not a silent no-op.

| Outcome | The turn |
| --- | --- |
| `finish` | ends with its reply and its tokens; the session stays as its harness leaves it between turns |
| `hang` | never ends: the CLI stays alive and silent |
| `crash` | ends the CLI with `exit` (1) and no reply |
| `budget` | ends on the provider's limit as the harness reports it (Claude's limit message, codex's `turn.failed`, agy's `AGY_ERROR` 429); the usage probe then says the session window is full |

Every turn is kept in the scenario's artifacts (`workers.jsonl`), with each
step's command line, exit code and output; the units' output is under
`units/` and the claude daemon's in `claude-daemon-<machine>.log`.

### Execution hosts

`given.hosts` gives the desk SSH execution hosts. Each is a root of its own
under `/work/hosts/<name>` (home, `bin`, the pinned `go` and `gh`, a source
checkout of every repository it serves, its own `config.yaml` with
`agents.execution`), provisioned as an operator would and prepared with the
real `beekeeper agents hosts prepare`; the desk's `agents.hosts.<name>` names
it with the policy digest it prepared.

```yaml
given:
  repositories:
    - name: example/app
  hosts:
    - name: builder                  # ssh alias, selector and identity
      harnesses: [claude]            # default: claude
      repositories: [example/app]    # default: every repository
      maxBusy: 2
      fault: unreachable             # optional, see below
      slow: 300ms                    # delay of every ssh call to it
      config: {}                     # laid over the host's own config.yaml
```

| Fault | What happens |
| --- | --- |
| `unreachable` | `ssh` refuses the connection: exit 255 |
| `host-key-changed` | `ssh` fails the host key check: exit 255 with OpenSSH's warning |
| `missing-tool` | the remote shell does not find beekeeper: exit 127 |
| `older-go` | the host pins Go 1.21.0 while its repository's `go.mod` names 1.26.0 |
| `slow` | every call is delayed; one slower than `ConnectTimeout` times out with 255 |

The `host` step changes a host's faults while the scenario runs:
`host: {name: builder, fault: unreachable}`, `host: {name: builder, slow: 2s}`
or `host: {name: builder, clear: true}`. `older-go` is part of a host's
provisioning and only `given.hosts` names it.

### The builder

`given.builder` gives the desk the Kubernetes builder of its remote builds
([Remote build slots](remote-builds.md)) without a cluster. The driver sets
the desk's `run.remote` and puts a fake `kubectl` on its `PATH`.

```yaml
given:
  builder:
    tools: [go]                      # the Go image's toolchain: go, golangci-lint
```

- **`kubectl`** keeps the Jobs the desk creates and answers `get`, `create`,
  `wait` and `delete` from them. `exec` runs its command in place, under the
  Job's container environment and the image's `PATH`, so the desk's own
  upload, launch and follow scripts run as they are. A pod's `/work` is the
  sandbox's: one builder runs at a time, and deleting its Job empties it.
- **The image** is `/work/builder/bin`: the system's shell and coreutils,
  and a fake for each of `tools`. An image without `golangci-lint` is one
  the desk's merge validation installs the linter into.
- **`go`** in the pod passes every build, vet and test. `go install` puts
  the fake of the tool it names into `GOBIN`, as the real one puts the
  tool there. **`golangci-lint`** reports no findings.

Every call of `kubectl`, `go` and `golangci-lint` is in the world's call
log, so `calls` expectations may name them. The image's tools are the
pod's, apart from `given.tools` on the desk's `PATH`, and no tool there may
be named `kubectl`.

### A generated desk

`given.state` seeds the desk's state before the first step. `generate` fills
it like a coordinator's that has worked for weeks, at rest: idle workers on
the roster, serve records of sessions that ended, deliveries in flight (each
with a pull request under review), finished workers' reports and an event
log. Every command reads all of it, which is what a large desk costs.

```yaml
given:
  state:
    generate:
      repository: example/app        # default: the scenario's first
      agents: 120                    # idle workers, each with its start
      records: 1500                  # serve records of ended sessions
      deliveries: 1200               # deliveries in flight
      reports: 3000                  # reports, about a kibibyte each
      events: 20000                  # events, about 400 bytes each
```

The issues and pull requests it names (numbers from 10000 on) are not on the
scenario's GitHub. A scenario that runs a loop on a generated desk sees that
loop read them and find nothing.
