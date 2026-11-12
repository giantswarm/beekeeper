# Behavioral scenarios for beekeeper's core

Epic: https://github.com/giantswarm/beekeeper/issues/3788

Status: plan for review. It names the harness, the scenario format, the first
flows, the regression scenarios, the rule for fix PRs and the order of the
refactor, and it splits the epic into ordered sub-issues (harness first).

## Outcome

Every pull request runs a suite of behavioral scenarios against the real
`beekeeper` binary in under 10 minutes. A scenario states a behavior as
given/when/then in YAML. A bug found live becomes a scenario that fails before
its fix. With that net in place, the largest core files are split into packages
without a change in behavior.

## The model: giantswarm/muster

`internal/testing` in giantswarm/muster is the template. What it does, and what
this plan takes from it:

- **Scenarios as data.** `scenarios/*.yaml` (about 300) are loaded by
  `scenario_loader.go`, validated against a schema and filtered by category,
  concept and tag. A new behavior or bug is a new file, not new Go.
- **A real process.** `muster_manager*.go` builds and starts the real binary per
  scenario on its own port and temporary configuration, waits for readiness and
  cleans up the process tree. Nothing is linked in-process.
- **Fakes at the process boundary.** `mock/` serves mock MCP and OAuth servers;
  `test_tools_faults.go` arms outages (a status for N requests, Retry-After),
  redeploys and restarts as scenario steps.
- **A controllable clock.** `muster_manager_clock.go` gives each instance a
  clock control socket; timers run on short intervals by default, or on
  production intervals moved by `test_advance_clock`.
- **One expectation checker.** `expectations.go` adapts every step's response
  into one view so that no expectation kind is honoured for one step kind and
  silently ignored for another; muster had scenarios passing vacuously before
  that (its lesson, and ours: unknown keys and assertion-free scenarios are
  errors).
- **A structured reporter** with per-step timing, JSON for CI and text for
  people; parallel workers with a bound on concurrent startups.

## What beekeeper has today

Beekeeper already has four partial harnesses. The plan unifies them instead of
adding a fifth:

| Harness | What it does | Fate |
| --- | --- | --- |
| `cmd` `loopRig` (`cmd/supervisorloop_test.go`) | the loop's `app` in-process with faked commands and pull reads | stays: unit tests of the planner's small cases |
| `internal/superloop/testdata/standstills` | recorded planner inputs replayed through the pure planner, with negative controls in CI | stays: planner-level replays |
| `internal/loopworkflow` (5,200 lines, `make test-loop-workflow`) | real binary, scratch Git origin, fake `gh` and `claude` behind one fixture directory | its flows move into scenarios, then it is retired |
| `internal/testenv` (`TestSupervisorWorkflow`) | real binary in user/mount/PID/network namespaces, fake `gh`, `claude`, Git remote helper | its sandbox is reused as the scenario sandbox; its workflow moves into a scenario |
| `internal/perfsuite` (`-tags perf`) | real binary on a generated desk of realistic size; `gh`, `ssh`, harness fakes by argv[0] | stays for budgets; its desk generator is shared with scenarios |

Gaps none of them close: fakes are hand-matched on whole argument strings
(`joined == "api repos/o/r/pulls/7"`), so every new call needs a new case; no
rate-limit, GraphQL cost, ETag/304 or outage model; no controllable clock (the
core calls `time.Now` directly in many places); no SSH execution host; no
self-update, canary or probation; no shared expectation vocabulary; and the
flows are Go, so a bug report does not become a scenario without new Go.

## Harness design

Package `internal/scenario` with build tag `scenario`, run by
`make test-scenarios` (`go test -tags scenario ./internal/scenario`) and a
CircleCI job `test-scenarios` on every pull request.

### Real binary, temporary desk

- The suite builds the candidate once with `-tags scenario` (the only
  difference from a release build: the clock hook below) and copies it into
  each scenario's root.
- Each scenario gets its own root: `HOME`, `XDG_CONFIG_HOME`,
  `XDG_STATE_HOME`, a generated `config.yaml` from the scenario's `desk:`
  block, a state store and event log (empty, or seeded from a fixture or from
  perfsuite's generator), and a bare Git origin per repository.
- Where Linux user/mount/PID/network namespaces are available (CI), the
  scenario runs inside `internal/testenv`'s sandbox: no network, no host home,
  no inherited credentials, and every descendant dies with the scenario. Where
  they are not, the runner refuses rather than run against the host.
- `PATH` holds only the fakes and the real `git`. Long-running processes
  (`supervisor loop`, `watch`, `guide watch`, a worker's harness) are started
  as background steps and stopped by PID at the end of the scenario; their
  output is kept in the scenario's artifacts.

### The world server

One in-process **world** per scenario holds every fake's state: GitHub, the
harness sessions, the execution hosts and the clock. Every fake executable
(`gh`, `claude`, `codex`, `agy`, `omp`, `ssh`, `devctl`, `systemctl`,
`journalctl`) is the same small binary, dispatching on `argv[0]`, that sends its
call to the world over a Unix socket named in the environment. So state is
consistent across all subprocesses, every call is logged in order with its
caller, and a call nothing answers fails the scenario (it is not silently
`exit 1`).

### GitHub REST and GraphQL

- **A model, not recorded strings:** repositories, branches and heads (backed by
  the bare origin), issues, pull requests, reviews, comments, check runs and
  suites, workflow runs, releases and assets, labels, sub-issues, and a
  Projects v2 board with its fields.
- **Served over HTTP:** the REST and GraphQL handlers listen on a local port;
  `BEEKEEPER_GITHUB_API` and the other API base variables point there for the
  code that calls HTTP directly.
- **`gh` is a thin translator:** the fake `gh` turns `gh api`, `gh pr`,
  `gh issue`, `gh run` and `gh release` into requests to the same handlers and
  renders gh's output and exit codes, including `gh: HTTP 304` and exit 1 on
  any non-2xx. A contract test, run on demand, compares the fake's rendering
  with the real `gh` against the same server.
- **Budgets:** separate core, search and GraphQL buckets with `X-RateLimit-*`
  headers, a GraphQL cost per query (by operation, or declared by the
  scenario), hourly resets on the scenario clock, and secondary rate limits.
  The world reports spending per caller (loop, worker, guide), so a scenario
  can assert who spent what.
- **Conditional reads:** ETags on reads, `304 Not Modified` without spending
  the budget, exactly as GitHub does.
- **Faults** as scenario steps or `given`: a status for N requests or for a
  clock window (5xx, 403 secondary rate limit, 404), `Retry-After`, a delay
  per response (slow network), an outage, and a matcher on method, path,
  operation name and caller.
- CI is part of the model: a pull request's checks go pending, then green or
  red when a scenario says so, on a named head.

### Harnesses

Fake `claude`, `codex`, `agy` and `omp` speak each CLI's start, resume and
stream protocol as far as beekeeper reads it, and run a **worker script** from
the scenario: steps a worker takes by calling the real `beekeeper` binary and
the real `git` (`agents park`, `agents idle --done`, push a branch to the fake
origin, `gh pr create`), plus turn outcomes: finish, hang, crash, exceed a
budget, report usage. A worker without a script ends its turn idle.

### SSH execution hosts

The fake `ssh` maps a host alias to a host root inside the scenario: its own
home, tool directory and configuration (`worker-distributed.json`), and runs
the remote command there. Hosts can be unreachable (exit 255), slow, have a
changed host key, lack a tool (exit 127) or pin an older Go. Execution
branches published from a host land in the same fake origin.

### Clock

Beekeeper has no clock seam today. A new `internal/clock` package provides
`Now`, `Since`, `After`, `NewTicker` and `Sleep`. A release build uses the
system clock and nothing else. In a `-tags scenario` build, a clock control
socket named in `BEEKEEPER_SCENARIO_CLOCK` gives every process the world's time,
and `advance` wakes the timers and tickers due, so an hour of loop passes takes
seconds. Core packages move to `internal/clock` flow by flow (loop, capacity,
GitHub budget, gate, update); a test lists the migrated packages and fails if
one calls `time.Now` directly again.

### Speed

- Target: the whole suite under 10 minutes on a CircleCI `medium` executor;
  each scenario's default `timeout` is 60 s of real time.
- One build; scenarios run in parallel (bounded by CPU, with a bound on
  concurrent loop startups); no network and no real waits, since the clock is
  advanced.
- The reporter writes per-scenario and per-step durations (JSON and JUnit) to
  the job's artifacts; the job fails when the suite exceeds 10 minutes and
  warns from 8, so growth is seen before it breaks the budget.

## Scenario format

Files under `internal/scenario/scenarios/<flow>/<name>.yaml`. The loader is
strict: unknown keys, an unknown step or expectation, a scenario without a
`then`, or a `wait_for` without a bound are errors, checked by a unit test that
loads every file.

```yaml
name: refill-304-uses-cached-pulls
description: >
  A 304 on the conditional open-PR read is not a failure, and one repository's
  failed observation does not hold starts in another.
issue: https://github.com/giantswarm/beekeeper/issues/3772
flow: refill
tags: [regression, github]
timeout: 60s

given:
  desk:
    ceiling: 4
    providers: [claude]
  github:
    repositories:
      - name: example/a
        issues: [{number: 1, board: Todo}]
      - name: example/b
        issues: [{number: 2, board: Todo}]
    faults:
      - match: {path: "repos/example/a/pulls", conditional: true}
        respond: {status: 304}
      - match: {path: "repos/example/b/pulls"}
        respond: {status: 502}
        times: 3

when:
  - loop: {passes: 1}            # supervisor loop --once
  - advance: 2m
  - loop: {passes: 1}

then:
  - started: {issue: example/a#1}
  - github: {calls: {path: "repos/example/a/pulls", status: 304, min: 1}}
  - no_alarm: "Worker starts are stalled"
```

- **`given`:** `desk` (configuration keys, roster, providers and capacity),
  `state` (a fixture or generator size), `github` (repositories, issues, board,
  PRs, checks, budgets, faults), `harness` (worker scripts), `hosts` (execution
  hosts and their faults), `clock` (start time).
- **`when` steps:** `run` (a CLI call, optionally `as:` a session or the
  person), `loop` (passes, or `background: true`), `worker` (drive a scripted
  worker), `github` (push, check result, review, merge, close), `fault`
  (arm or clear), `host` (up, down, change), `advance` (the clock),
  `wait_for` (poll an expectation until it holds, bounded by clock and real
  time).
- **`then` and inline `expect`:** exit code, output (`contains`,
  `not_contains`, `json_path`), `started` / `not_started`, PR facts (count
  per issue, state, head, merged commit), `github.calls` and `github.budget`
  (spend per caller and bucket), `events` (kinds in the event log),
  `alarm` / `no_alarm`, `notes`, `board` (an item's status), `duration` (a
  call's real-time ceiling) and `process` (running, exited, held the state
  lock for at most a duration). Every step kind is checked by one function on
  one response view, as in muster.
- **`pending: <issue URL>`** marks a scenario of an open bug: the runner
  expects it to fail and reports it as pending; once it passes, the run fails
  and asks for the marker's removal. That keeps every bug's scenario on main
  without a red suite, and makes "fails before the fix" visible.

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

| Bug | Scenario |
| --- | --- |
| https://github.com/giantswarm/beekeeper/issues/3738 duplicate PRs per continuation | an open PR from one execution; a continuation on an execution host publishes; then one open PR for the task, and after its merge none of the task's other PRs stays open |
| https://github.com/giantswarm/beekeeper/issues/3772 HTTP 304 held every start | the scenario in the format section: a 304 uses the cached read; another repository's 502 holds only that repository |
| https://github.com/giantswarm/beekeeper/issues/3769 loop drains GraphQL | one simulated hour of loop passes on a desk of realistic size: the loop's own GraphQL spend leaves the budget above `github.floor`, and starts are never held by the loop's own reads |
| https://github.com/giantswarm/beekeeper/issues/3636 refill starved by a busy stream | a worker stream held by a slow GitHub response (8 s): the refill still starts a worker within one pass interval, and no process holds the state lock across the slow call |
| https://github.com/giantswarm/beekeeper/issues/3742 merge-result checker without the linter | the check's host lacks `golangci-lint` (exit 127): the finding is an environment finding, the check is not dispatched there again, and the loop still reaches a go on green CI for the exact head |
| https://github.com/giantswarm/beekeeper/issues/3345 and https://github.com/giantswarm/beekeeper/issues/3657 canary killed by a slow `supervisor status` | self-update on a large generated desk where `supervisor status` takes longer than the probe: probation is not failed by a slow but healthy loop, a real failure is rolled back with its reason, and `supervisor status` and `capacity --json` stay under their budgets |

Open bugs land their scenario with `pending:`; the fix removes it.

## The rule for fix PRs

Written into `AGENTS.md` ("This repository") and the review skill
(`plugin/skills/review/SKILL.md`):

> A pull request that fixes or changes behavior of beekeeper's core adds or
> extends a scenario under `internal/scenario/scenarios/` that fails without
> the change. A bug's scenario names the issue. Reviews reject a core fix
> without one; a change that cannot be expressed as a scenario says why, and
> the harness gains the missing step in a follow-up.

CI enforces the "fails without the change": a `scenario-control` step builds
the pull request's merge base and runs only the scenarios the pull request adds
or changes against it; each must fail on an assertion (not on loading or a
build error), as the standstill controls already do for the planner. A
scenario that cannot fail on the base (a new command) says so with
`control: none` and a reason, which the review checks.

## Order of the refactor

Only after the flows and regressions above are green on main, and one seam per
pull request. A split moves code and changes no behavior: no scenario, golden
file or fixture changes in the same pull request, and the suite green before
and after.

1. `internal/clock` and the GitHub client seams (done by the harness work).
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

## Open points

- The fake `gh` versus the real `gh` against a fake host: the plan picks the
  translator (no TLS or host-name rewriting in the core's URL checks) and
  guards it with the contract test; if that test shows drift often, revisit.
- Scenario runs need Linux namespaces. CI has them; a developer machine
  without them gets a refusal, not a host run.

## Refill regression before the scenario harness

The regression for https://github.com/giantswarm/beekeeper/issues/3636 is covered in
`cmd/supervisorfillcadence_test.go`: given a blocked pull observation and free capacity,
a refill tick starts board workers before the observation is released. Companion cases
check hold recovery and the action named by the stall alarm. These run in the existing
in-process loop rig; port this behavior to the real-process scenarios when the harness lands.
