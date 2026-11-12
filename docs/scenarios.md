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
   `given.repositories` under `/work/origins/<owner>/<name>.git`.
4. It starts the scenario's **world** on a Unix socket. `PATH` holds only
   the candidate, real `git`, `sh` and the fakes: `gh`, `claude`, `codex`,
   `agy`, `omp`, `ssh`, `devctl`, `systemctl` and `journalctl`. They are one
   executable that dispatches on its name and hands each call to the world.
   The world logs every call in order. A call that no `given.calls` entry
   answers gets exit 1 and **fails the scenario**. With `given.github`, `gh`
   and the GitHub API are answered by a model instead: see
   [The scenario's GitHub](scenarios-github.md).
5. It runs the `when` steps, checks each step's inline `expect`, then the
   `then` expectations, and stops every background process by PID.

## Writing one

Put a file at `internal/scenario/scenarios/<flow>/<name>.yaml`. The loader
is strict. A scenario fails to load when it has an unknown key, step or
expectation, has no `when` or no `then`, has a step or expectation with more
or fewer than one kind, or has a `wait_for` without a timeout. A unit test
loads every file.

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

In `then`, an expectation reads the step named by `step`, or the last step.

### Pending scenarios

`pending: <issue URL>` marks the scenario of an open bug. The run expects
the scenario to fail and reports it as pending. Once the scenario passes,
the run fails and asks for the marker's removal, so the fix removes the
marker in the same pull request. A failure of the harness itself, such as
no sandbox, fails the run even for a pending scenario.
