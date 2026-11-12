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

The step passes at once, without a build, on a branch that adds or changes no scenario, on `main`
and on tags. The job's artifacts hold its log (`control.log`), its report (`control/report.txt`
and `control/report.json`: every scenario with its change, verdict, first failures on the base
and opt-out reason) and each run's artifacts under `control/<flow>/<name>/`. To run it on a
branch: `bash .circleci/scenario-control.sh` (Linux with namespaces, as for the suite).

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
| `run` | runs the candidate with `args` (and `env`, `stdin`) to its exit |
| `background` | starts the candidate and leaves it running; needs a `name` |
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
| `worker` | what the sessions named `name` did: the `turns` they began, the last `outcome`, whether a CLI is `running` | every step |
| `origin` | a `branch` of `repository`'s origin: whether it `exists`, how many commits it is `ahead` of `main` | every step |

In `then`, an expectation reads the step named by `step`, or the last step.
A `worker` expectation may also name the `harness` and the `host` of the
sessions; `name` and an `origin`'s `branch` are regular expressions that
match the whole name.

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
armed a moment after a step would wait for the next one. Each step goes to
the scenario's `clock.jsonl` artifact: the world's time, the processes that
acknowledged it, how many timers fired, how long it took and how long it
waited for quiet.

`internal/clock`'s `TestMigratedPackagesUseTheClock` lists the migrated
flows (the supervisor loop and its timers, capacity, the GitHub budget, the
gate, self-update and probation, and the state) and fails when one of their
files calls the `time` package's clock again. To migrate a flow, replace
those calls with `internal/clock`'s (imported as `clk` in `cmd`, whose
`clock` function formats times) and add its files to that list.

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
  and `turn.completed` with its tokens. `app-server` answers the usage probe;
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
      codex: {session: 10, week: 5}
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
