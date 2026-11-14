# Phase 1 (foundation and dry run): scenarios and test harness

Status: **approved** · 2026-11-14
Inputs: the [design](../design.md) (§6, §11, §12.2, §14, §17, §20), the lessons from the previous system, the spike, and research into muster's scenario
runner, `giantswarm/k8senv`, the roadmap board (ProjectV2 #273), GitHub's webhooks and rate limits, devctl's `pkg/prwait` and JEV.

This document has three parts, in the order they should be reviewed:
1. **What the dry run means** (§1–§2): how the engine behaves when it observes but doesn't act. The scenarios encode this.
2. **The harness** (§3): how scenarios run: the real core, the fakes, the injected clock.
3. **The format and vocabulary** (§4–§6): the YAML shape (muster's), the names scenarios assert on, and the catalogue.

The scenarios are in [`internal/testing/scenarios/<concept>/*.yaml`](../../internal/testing/scenarios/); [`CATALOGUE.md`](../../internal/testing/scenarios/CATALOGUE.md) next to them lists them all.

## 1. What the dry run means

Phase 1 builds the engine's foundation and runs it as a dry run for one opted-in team on the roadmap board. The engine observes the board and GitHub and records
what it *would* do. It never writes to GitHub, never sends an Ask, and starts no runs.

Nobody implements by hand, so nothing is compared with people's work. The dry run proves the App, the webhook route, webhook coverage, API cost, freshness and
the invariants on the organisation's real traffic (colleagues' PRs, Renovate, CI), and it shows each participant the queue the engine would work through for
them. Phase 2 then switches acting on.

### 1.1 Which board items the engine tracks

- **Items** exist for the team's board items in the active statuses (`Up Next ➡️`, `In Progress ⛏️`, `Blocked / Waiting ⛔️`, `Validation ☑️`)
  whose issue is open, whoever they're assigned to, plus the team's `Bug 🐞` items in `Inbox 📥` (so that `bug-severity?` can classify every new bug). Items
  assigned to people who aren't participants are observed for their links (dependencies, epics, PRs) and never touched.
- An Item whose issue is closed by its merged PR stays tracked (`Merged`, then `Validating`) until the board says Done, although the board query no longer returns it.
- An item that leaves that set by its **status** (moved to Backlog or Done, or its issue closed without a merged PR) keeps its Item, with phase `Backlog` or
  `Done`, with no wait and no pick, for `itemRetention` (7 days), then the Item is deleted. That keeps its history readable.
- An item that leaves by **scope** (its Team changes, it's archived or removed from the board) loses its Item at once, with an `item.removed` event. Its Changes
  go with it, and their waits leave the wake index.
- A board item that was never in the set doesn't become an Item when a webhook mentions it: the engine reads it, sees it's out of scope, and drops it.
- When a resync no longer returns an existing Item, the engine reads that one item by its node id to learn why. Nothing depends on having seen the webhook.
- Epics: the board query includes each epic's sub-issues with their state and status, so an epic's phase is known even when its sub-issues aren't Items themselves.

### 1.2 What the engine observes and what it would do

- **The lifecycle is observed.** Each Item's `phase` is derived from the board `Status`, the issue's state and the PRs that close it, whoever moves them. It uses the states of §5.1:

  | Observed | `phase` |
  |---|---|
  | `Inbox 📥`, `Backlog 📦` | `Backlog` |
  | `Up Next ➡️` | `Ready` |
  | `In Progress ⛏️`, no open or merged PR | `Implementing` |
  | `In Progress ⛏️`, PR open, CI pending or green but not yet approved where needed | `Published` |
  | `In Progress ⛏️`, PR open, CI red on the head SHA, or a "changes requested" review open | `Fixing` |
  | `In Progress ⛏️`, PR open, CI green (or no merge-relevant checks) and approved where needed | `Merging` |
  | PR merged (whether or not that closed the issue), until the board says Validation or Done | `Merged` |
  | `Blocked / Waiting ⛔️` | `Waiting` |
  | `Validation ☑️`, the issue open or closed by the merged PR | `Validating` |
  | `Done ✅`, or the issue closed without a merged PR | `Done` |

  An Epic is `Planned` (no sub-item past `Ready`, or no sub-items), `Active` (at least one sub-item past `Ready`, `Done` included), or `Done` (every sub-item
  `Done` or closed; closed as "not planned" counts as cancelled).
- **Two kinds of work in progress:**
  - *In flight:* any tracked implementable item in `Implementing` through `Validating`, whoever works on it (epics aren't counted). The order (§6.3) puts work
    that unblocks in-flight work first, and such work is exempt from the gates. The engine never touches in-flight work it didn't start.
  - *Engine WIP:* the items the engine has started. WIP limits count only these: each participant (a configured Person) has `wip.items` and `wip.epics` (1 and 1
    by default), and a team may set a cap across its participants (`cap.items`, `cap.epics`, unset by default). An epic counts once the engine has started one
    of its sub-items, until the epic is `Done`. In the dry run the engine starts nothing, so engine WIP is always 0, and the limits bound what a single decision
    pass would start.
- **The engine's decisions are recorded, never executed.** For every Item the engine computes eligibility (§6.1); for every participant the items it would
  start now, the gates that hold others (§6.2), and each item's place in the order (§6.3); and the actions it would take. These go on `status.shadow` and into
  the event table.
- **Picking.** A decision pass for a team runs on every change to its Items, People or configuration, and on every board resync even when nothing changed.
  - Participants take turns in login order, round-robin, each taking the next eligible item in the order of §6.3 that is assigned to them or to nobody.
    Unassigned sub-items of an epic are only for the epic's owner.
  - A participant's **picks** are the items the engine would start for them now: as many as their free WIP allows (`free` is the limit minus engine WIP). Each
    pick is computed as if the earlier picks of the same pass had started: it takes a slot of the person's WIP and of the team cap, an epic slot if its epic
    doesn't hold one yet, and its repo unless `parallel-safe?`.
  - **Epic slots.** An epic holds a slot of its owner's `wip.epics` (and of the team's `cap.epics`) once the engine has started one of its sub-items, until the
    epic is `Done`. Starting a sub-item of an epic that holds no slot yet needs a free one, even if people made the epic `Active` earlier. Its siblings then
    need none. Ranks are computed once per pass from the observed state.
  - **Repos.** A repo is taken by the engine's WIP and by earlier picks of the same pass, never by work in flight that the engine didn't start.
  - An item a gate holds is skipped, and the person's next startable item is picked instead. The items skipped above a person's last pick, or the person's top
    candidate when they get no pick at all, are recorded as **held** for that person, with the gate: `TeamCap` (the team's item or epic cap), `EpicWIP` (the
    person's epic limit) or `RepoConcurrency`. The person's own WIP limit only bounds the number of picks and never shows as a hold.
  - An item is held for at most one person: the first in turn order for whom it was skipped or the top candidate.
  - Gate-exempt work (work that unblocks in-flight work) is picked even past the team cap or into a taken repo. It still counts against the person's own WIP.
  - Team `picks` and `held` are listed in turn order. `next?` is one decision per person and pass, whose answer is the person's first pick.
  - Each pick has the would-actions `assign {login}` (if unassigned), `set-status {status: "In Progress ⛏️"}` and `start-run {purpose: implement}`. A held item has none.
  - `rank` is the item's position in the team's whole order (1-based).
  - Placement on runners and machine capacity come with phase 2.
- **Waits are on what's pending.** The engine holds a wait wherever it expects an observation:

  | Situation | Wait |
  |---|---|
  | `Ready`, blocked by an open issue | `dependency` on that issue (Item) |
  | CI not final on the head SHA | `ci` on the head SHA (Change) |
  | CI green, awaiting a review or another team's approval | `review` on the PR (Change) |
  | Merged, merge-result check not final | `ci` on the merge commit (Change) |

  A **Change** is a PR that closes a tracked item, whoever opened it. In the dry run these are colleagues' PRs, which gives real CI and review traffic. Items in
  flight that the engine didn't start carry no wait of their own: nothing is expected of the engine there. From phase 2 the engine's own items get runs, and
  their waits.
- Each wait has `on`, `ref`, `since`, `deadline`, `atDeadline`, `state` and `reason` (§11). Deadlines come from configuration. For `ci` the deadline is
  `deadlineFactor` × the repo's p90 pipeline duration (nearest rank) once there are `minHistory` durations, otherwise `defaultDeadline`.
- **At a deadline the engine re-observes once** (the conditional reads that wait needs). If the situation is unchanged, the wait becomes `overdue` (one
  `wait.overdue` row) and re-observes after 1 minute, doubling, capped at `overdueBackoffCap` (10 minutes). Phase 1 escalates nothing: the only owners would be
  colleagues, whose work is left alone. A long pipeline or a slow review is a flow metric, not an engine fault.
- **Unobserved is not stalled.** If the wait's source is degraded and can't be polled, or the re-observe fails (5xx, rate limit, budget reserve), the wait shows
  `unobserved` with the reason, and no invariant calls it stalled; the source's own `degraded` state is what alerts. A deadline that passed while unobserved is
  handled once after the wait can be observed again.

### 1.3 Observation

- **Webhooks first.** The engine subscribes to `projects_v2_item`, `issues`, `sub_issues`, `issue_dependencies`, `pull_request`, `pull_request_review`,
  `check_run`, `check_suite`, `status`, `push` and `installation_repositories`. A delivery only wakes the engine. The engine reads the state it needs from
  GitHub itself and trusts nothing in the payload except the ids that tell it what to read (§17).
- **The receiver** answers only `POST /github/webhook`, with a 10 MiB body limit (inclusive). It verifies `X-Hub-Signature-256`, deduplicates on
  `X-GitHub-Delivery`, and stores each verified delivery durably (the Postgres inbox) before it answers 2xx. If it can't store a delivery it answers 503, which
  GitHub logs as a failed delivery. `ping`, unknown events and other projects' items are acknowledged and ignored. Deliveries that arrive while no replica leads
  are processed by the next leader.
- **The wake index.** A delivery maps to refs (`issue:…`, `pr:…`, `ci:<repo>@<sha>`, `board:<node>`). Only the Items and Changes waiting on those refs, and
  the Items and Changes those refs belong to (an issue's Item, a PR's Change), are reconciled. So a push wakes its Change even when the Change has no wait.
- **Resyncs are the safety net**, on the engine's clock:
  - *Board:* the filtered GraphQL query `BoardTeamItems` (`items(query: "team:\"Bumblebee🐝\" status:\"Up Next ➡️\",\"In Progress ⛏️\",\"Blocked / Waiting ⛔️\",\"Validation ☑️\"
    is:open")` plus the team's Inbox bugs), every 10 minutes by default. It never pages the 5,600-item board.
  - The team's Inbox bugs come from a second aliased `items(query:)` in the same request, so a resync is one GraphQL call.
  - The engine checks the filter's option names against the board schema (`BoardSchema`) at startup and on configuration changes, because a misspelt option
    silently returns nothing. An invalid filter sets the condition `BoardFilterValid=False` (reason `UnknownTeamOption`), marks `board` degraded with reason
    `filter-invalid`, and runs no board query. A configuration change that makes it valid runs a board resync at once.
  - *Changes:* conditional REST reads (ETag) of the observed PRs, their check runs and statuses, every 15 minutes by default. An unchanged answer is a free 304.
    ETags are kept on the objects, so a restart doesn't cost a full re-read.
- **Every observation records its source:** `webhook`; `resync`; `reobserve` (at a deadline, while polling, or for a missed delivery found in the log);
  `startup`. A change first seen by any source but a webhook, for an event the subscription covers, counts as a **missed webhook**, including a delivery that
  arrives after a resync has already seen the change ("late is missed"). That's the coverage measurement of phase 1. Changes in repos outside the App's
  installation can't send webhooks to it, so they aren't counted as missed.
- **Freshness per source** (`webhooks`, `board`), first checked at startup:
  - Every 5 minutes the engine reads the App's webhook delivery log (`GET /app/hook/deliveries`, with the App's JWT and its own rate limit). A delivery GitHub
    logged as failed marks `webhooks` degraded (reason `deliveries-failed`). So does a log it can't read (`delivery-log-unreadable`). The engine reads a failed
    delivery's payload from the log (one GET per delivery) to learn which refs to re-observe. It never redelivers, because phase 1 writes nothing to GitHub.
  - Every 10 minutes a 1-point GraphQL probe (`BoardProbe`) asks for the team's most recently updated board items. A board change newer than anything the board
    webhooks reported marks `board` degraded (`probe-gap`). The probe only judges freshness; the resync that runs at the same tick does the observing.
  - A source whose reads are deferred by the budget reserve is degraded with reason `budget`.
  - A source degrades at the first failed or deferred scheduled check (a transient retry of a check isn't a check of its own). From then on `polling` is true:
    its waiters poll every `polling.interval` (5 minutes), conditionally where possible, the first poll one interval later, and board resyncs run at that
    interval too. This shows as `engine_polling_active{source}`.
  - A source is fresh again after a full check interval with no new gap and, for `webhooks`, at least one successful delivery since the gap.
- **The API budget.** One token bucket per App and API (`rest`, `graphql`; the delivery log's `app` limit separately), with the priority classes of §10. Phase
  1 only spends from *observation* and *metrics*. The bucket follows the rate-limit headers, so other users of the same App count too. Observation stops at a
  reserve (20% of the limit), and waits for the reset time on the engine's clock. 304s don't count. A primary rate limit on an API stops every call to that API
  until its reset. Secondary rate limits are honoured with `Retry-After`.
- **Errors.** Transient errors (5xx, timeouts) are retried on the engine's clock after 10 s, doubling, capped at 5 minutes (no jitter in tests). Refusals on
  reads (404, 403 without rate limit, 410, 422) are recorded with their inputs and not retried until those inputs change: a webhook for that repo or issue, an
  `installation_repositories` change, or a configuration change. A private repo outside the installation is a refusal; a public one is read normally.

### 1.4 Decision points in shadow

- Phase 1 exercises the decision points that §6 needs: `ready?`, `next?`, `parallel-safe?` and `bug-severity?`. The others belong to later phases' flows.
- Decision points run only for subjects that pass the code checks first: `ready?` only for Up Next, implementable items of an opted-in team that are unassigned or assigned to a participant.
- **The rule always decides** in mode `shadow`. Each decision is recorded once per (point, subject, inputs hash), and JEV is asked once for the same key. The
  subject is an item's bare ref or a person's login. The inputs are what the question is about, never who it's addressed to: `ready?` hashes title, body and the
  issue's repo, not the assignees (the todo's recipient follows the current assignee without a new decision). JEV's answer is recorded next to the rule's and
  never changes the outcome. JEV is a side effect whose answer is an input, so `decide` stays pure and a replay reproduces every decision.
- JEV gets only what the question needs: title and body for `ready?` and `bug-severity?`, candidate summaries for `next?`, footprints for `parallel-safe?`.
- An answer below the confidence floor (`jev.confidenceFloor`, 0.5) is recorded as `jev_low_confidence`, whatever it says. An error or a timeout is recorded as
  `jev_error`. The rule's answer stands either way. Mode `rule` asks no JEV at all.
- **Rules in phase 1:**

  | Point | Subject | Answers | Rule |
  |---|---|---|---|
  | `ready?` | item | `ready`, `not-ready` | an acceptance-criteria heading with a list item, and a known target repo (details below) |
  | `next?` | person | an item ref | the order of §6.3 (details below) |
  | `parallel-safe?` | item, with `other` | `yes`, `no` | sequential per repo unless both items name footprints (a `Footprint:` line) and they're disjoint |
  | `bug-severity?` | item | `critical`, `high`, `medium`, `low` | `medium` until a person sets it; a would-action records the severity it would write (there's no `Severity` field yet) |

  - `ready?`: the target repo is known when the issue lives in a code repo or the body has a `Repo:` line. Bugs skip refinement: only the target repo is
    checked. "Not ready" adds one would-todo "refine with `/gs-plans:gs-plan`" to the assignee, or to `team:<name>` when unassigned.
  - `next?`: incidents, main repairs, work that unblocks in-flight work, items in `Active` epics, `Priority` (lower first, unset last), age (older first). Phase
    1 sees no incidents or repairs: `Alert 🚨` items are counted, not handled (phase 4).

### 1.5 Engine health

- The invariants of §12.2 are exported as `engine_invariant_violations{invariant}`, whose value is the number of violating objects (a Change missing its wait
  counts once, not again for its Item). They're computed when metrics are collected, from the objects and the clock, never on the timer queue they watch.

  | Invariant | In phase 1 it counts |
  |---|---|
  | `wip-has-run-or-wait` | `Ready` Items with an open blocker but no `dependency` wait, and Changes with CI, review or merge result pending but no wait. From phase 2, also engine WIP without a run |
  | `wait-within-deadline` | waits past deadline plus grace (5 minutes) that weren't handled |
  | `stage-age-under-p90` | always 0 in phase 1: there is no engine WIP to age |
  | `refusal-not-repeated` | refusals retried on unchanged inputs |
  | `source-fresh-or-degraded` | sources whose freshness verdict is older than twice their check interval |
  | `gate-not-holding-repair` | gate-exempt items (work that unblocks in-flight work, in phase 1) held by a gate |
  | `pending-run-placed` | always 0: no runs in phase 1 |
  | `merge-reaches-result-check` | merges whose merge-result check is still missing after its deadline's re-observe |
  | `leader-decides` | 1 when the last decision pass is older than twice the board resync interval |

- Alert rules on these, plus reconcile errors, no leader and missing metrics, are tested with `promtool test rules`, outside the scenarios.
- While Postgres is down, history rows are buffered in memory (up to 10,000, then dropped and counted) and written once it's back. Deciding and updating objects never wait for Postgres.

### 1.6 Never writing to GitHub

- In dry-run mode the engine is built with a read-only forge client: the mutating methods don't exist on the type it gets.
- The only POSTs it sends are GraphQL queries and the minting of installation tokens with the App's JWT (`apps/create-installation-access-token`, api `app`),
  which changes nothing on GitHub. Tokens are cached until shortly before they expire.
- The fake GitHub records every call. Every scenario fails if any other mutating REST call, or any GraphQL mutation, was made (§3.6).

## 2. What the phase 1 scenarios cover

| Concept | Covers | Design |
|---|---|---|
| `board` | observing the board and issues: scope, kinds, fields, links, resyncs, startup | §4, §6.1, §11 |
| `eligibility` | each condition of §6.1, `ready?`, blockers, bugs | §6.1, §14 |
| `epic` | `Planned`/`Active`/`Done`, epic slots | §5.2 |
| `pick` | picking per person, assignment, order, would-actions | §6.1, §6.3 |
| `gate` | per-person WIP, the team cap, epic WIP, repo concurrency, exemptions | §6.2 |
| `decision` | decision points in shadow with JEV, replay | §14 |
| `wait` | wait records, the wake index, deadlines, re-observe, overdue, merge results | §11 |
| `source` | freshness, degraded sources, polling fallback, recovery | §11 |
| `budget` | the token bucket, conditional requests, rate limits, backoff, refusals | §10, §11 |
| `webhook` | the hardened receiver | §17 |
| `invariant` | the invariants of §12.2 | §12.2 |
| `history` | the event table and decisions in Postgres | §3, §15 |
| `leader` | two replicas, handover, restart | §3, §12 |
| `shadow` | never writing, would-actions recorded once | §20.2 |

**Learnings that apply to observation**, each with scenarios tagged `learning-<slug>`:

| Learning | Tag |
|---|---|
| The PR watcher went blind for 30–60 minutes several times a day | `learning-blind-watcher` |
| Waits didn't wake for 8–61 hours | `learning-waits-dont-wake` |
| "No progress" raised because nothing was observed | `learning-unobserved-not-stalled` |
| 200k REST calls a week; GraphQL budget exhausted | `learning-api-budget` |
| 98% of scheduled wake-ups failed, retrying every minute | `learning-failed-wakeups` |
| The same refusal retried about 20 times | `learning-repeated-refusal` |
| A failed board automation read as a broken main | `learning-false-alarm` |
| Gates blocked their own repairs | `learning-gate-blocks-repair` |
| A hand merge bypassed the checks | `learning-hand-merge` |
| Self-update stalls | `learning-self-update-stall` |
| An evening's epic didn't move overnight | `learning-overnight-epic` |

**Out of scope for phase 1** (they get scenarios in their phase): runs, runners and placement (dead workers, hand-offs), the engine's own WIP over time,
escalations, reviews before publishing, merging, delivery, Asks sent through klaus-gateway, incidents, `report_problem`.

## 3. The harness

### 3.1 Black box, real core, fake edges

- **Real:**
  - The engine runs in-process, built from the same wiring as the binary, with the real controllers, the real webhook receiver (an `http.Handler` on a loopback
    listener), the real GitHub client and the real Postgres writer.
  - A real kube-apiserver from **k8senv**. Verified for phase 1: controller-runtime watches, status updates, server-side apply, finalizers, label and field
    selectors, and watch resume after a disconnect all work on its kine/SQLite backend, with 2–15 ms from write to reconcile.
    - A pool of 4–8 instances per test binary, CRDs installed through `WithCRDDir` (cached per CRD hash), one fresh namespace per scenario (never deleted), the manager stopped before `Release()`.
    - k8senv authorizes everything, so the few tests that need permission denials (the engine's RBAC) use envtest.
  - A real Postgres, one per test run: the schema is migrated once into a template database, and each scenario gets `CREATE DATABASE … TEMPLATE`
    (milliseconds). The server comes from `embedded-postgres` with a cached binary, so no Docker is needed on laptops, executors or CI.
- **Fakes**, in-process, behind the interfaces of principle 12:
  - **GitHub:** an `httptest` server that speaks the subset of REST and GraphQL the engine uses, backed by a small state model: repos (visibility, required
    checks, required approvals, installed or not), issues, board items and fields, sub-issues, dependencies, PRs, check runs, statuses, reviews, installation
    repos, rate limits, the webhook delivery log.
    - GraphQL is matched by operation name and answered from the model, with point costs like GitHub's.
    - It honours ETags (`If-None-Match` → 304, free) and returns rate-limit headers.
    - Every state change made by a step produces the webhook GitHub would send, signed with the fixture's secret, and an entry in the delivery log. A step can
      have it lost (logged as failed), never sent (not logged), delayed on the engine's clock, or duplicated.
    - A merged PR closes the issues it names in `closes`, as GitHub does for the default branch.
    - Node ids are deterministic: `PVT_<org>_<project>` for the board, `PVTI_<org>_<project>_<ref>` for a board item, `I_<ref>` for an issue, `PR_<ref>` for a PR.
    - It records every call: replica, API, method, path or operation, REST operation id, conditional, status, points.
    - Faults: 5xx, secondary rate limits with `Retry-After`, an exhausted primary budget, remaining budget lowered by "other users" of the App, and failing deliveries.
  - **JEV:** typed answers per decision point and subject (by default the rule's answer with confidence 0.9), errors, timeouts (failing at once) and low
    confidence. It records each request and the fields it carried.
  - No other fakes in phase 1: there are no runners, sessions, executors or Asks.

### 3.2 Time

- There is one `Clock` interface in the engine (`Now`, `AfterFunc`). Production uses the system clock; scenarios use a fake that only moves when a step says `clock_advance`.
- All engine timing goes through it: wait deadlines, re-observe backoff, resync and freshness intervals, budget resets, `Retry-After`, retry backoff, invariant windows, event timestamps, metric ages.
- Deadlines live on a clock-driven **timer queue** that wakes the controllers through a channel source. The engine never uses controller-runtime's `RequeueAfter` or `time.Sleep`.
- `clock_advance` moves the clock from one due timer to the next in deadline order, settling the engine (§3.3) at each instant, so a timer scheduled within the advanced span fires too.
- Two things stay on real time and are kept short in tests: client-go's leader election (only in `leader` scenarios) and kube API retries.
- Replicas are named `engine-0` and `engine-1`; `engine-0` starts first and leads.

### 3.3 Settling

- After every step, and at every instant inside a `clock_advance`, the harness waits until the engine is **idle**. Idle is tracked exactly, not by a quiet window:
  - no queued or in-flight reconcile in any controller,
  - every write the engine made has come back through its informers (the client records the resourceVersion of each write, the informers mark it seen),
  - no delivery in the inbox unprocessed, no due timer, no outstanding JEV call.
- That makes "nothing happened" assertions (no call made, nothing reconciled) meaningful, and keeps a long `clock_advance` fast: an idle periodic tick costs a few milliseconds.
- Idle is bounded at 5 s. Not settling is a failure that reports what was still busy, with a goroutine dump, like muster's "stalled".
- `wait_for_state` (a retry window, as in muster) is only needed in `leader` scenarios.

### 3.4 Isolation, parallelism, speed

- Every scenario has its own engine, namespace, database, fake GitHub, fake JEV and clock. Nothing is shared but the k8senv pool and the Postgres server.
- Scenarios run with `t.Parallel()` under `go test ./internal/testing/...`, filterable by name (`-run`), concept (`-scenario.concept`) and tag (`-scenario.tag`).
- **Budget:** each scenario under 1 s once the pool is warm (those advancing the clock by days under 3 s), and the whole phase 1 suite under 60 s on 8 cores.
  The suite lists every scenario over its budget.

### 3.5 Unit tests

`decide`, each decision-point rule, the wake-index mapping, the deadline arithmetic, the invariant evaluators and the receiver's checks get table-driven unit
tests. Scenarios cover behaviour across the system.

### 3.6 Checks after every scenario

Unless a scenario says otherwise in `post_checks`:
- `no_github_writes`: no mutating REST call (token minting excepted) and no GraphQL mutation. It can't be switched off.
- `no_reconcile_errors`
- `invariants_hold`: every `engine_invariant_violations` series is 0, except those named in `expect_violations`.
- `settled`: the engine settled after every step.

### 3.7 Lessons taken from muster's runner

- Strict YAML decoding (`KnownFields(true)`). Unknown keys fail the load, and so does a missing `description`.
- One expectation evaluator for every step tool, and meta-tests that every expectation kind and every step field is acted on, so nothing passes vacuously.
- Tags can be filtered on.
- `success` defaults to **true** (muster's default of false made a missing key mean "must fail").

## 4. Scenario format

The shape is muster's: a scenario is a list of steps, each calling a harness tool with arguments and stating what's expected of the result.

```yaml
name: wait-ci-wakes-on-check-run           # kebab-case, prefixed by concept, equals the file name
concept: wait
tags: [phase1, wake-index]
description: |                              # required; Given/When/Then
  Given ... When ... Then ...
pre_configuration:
  fixture: base                            # fixtures/base.yaml, merged first
  github: { issues: [...], pulls: [...] }   # added to (or overriding) the fixture
  engine: { replicas: 1 }                  # 2 only for leader scenarios
expect_violations: []                      # invariants this scenario expects to see violated
post_checks: {}                            # rare overrides of §3.6
steps:
  - id: ci-completes
    description: CI finishes green on the head SHA
    tool: github_check_run
    args: { repo: giantswarm/klaus, sha: a1, name: "ci/circleci: go-test", status: completed, conclusion: success }
  - id: wait-ended
    tool: get_change
    args: { ref: giantswarm/klaus#55 }
    expected:
      json_path: { status.ci.state: green, status.wait.on: review }
```

### 4.1 Pre-configuration

`fixture: base` ([`fixtures/base.yaml`](../../internal/testing/scenarios/fixtures/base.yaml)) defines the installation (org `giantswarm`, project 273, mode
`dry-run`, default intervals), the team **bumblebee** (board option `Bumblebee🐝`, no team cap), the participants `alice` and `bob` (WIP 1 item and 1 epic
each), the repos `giantswarm/roadmap`, `giantswarm/klaus` and `giantswarm/muster` (installed, required checks, one required approval), full rate limits, and JEV
agreeing with the rule. The clock starts at Monday 2026-11-16 09:00 UTC. A scenario adds to it; lists of named things merge by name or ref:

```yaml
pre_configuration:
  fixture: base
  clock: "2026-11-16T20:00:00Z"
  config:                                  # Installation, Team and Person objects
    teams: [{ name: bumblebee, cap: { items: 1 } }]
  github:
    repos: [{ name: alice/klaus-experiments, visibility: public, installed: false }]
    issues:
      - ref: giantswarm/roadmap#101
        title: Retry transient errors in the klaus client
        body: |
          ### Acceptance criteria
          - [ ] retries 5xx up to three times
          Repo: giantswarm/klaus
        assignees: []
        state: open                        # or closed / closed_not_planned
        createdAt: "2026-11-10T10:00:00Z"
        parent: giantswarm/roadmap#90
        blockedBy: [giantswarm/roadmap#100]
        board: { status: "Up Next ➡️", kind: "Story 📑", team: "Bumblebee🐝", priority: 2 }
    pulls:
      - ref: giantswarm/klaus#55
        author: alice
        head: a1                           # head SHA
        state: open                        # or closed / merged (with mergeSha)
        closes: [giantswarm/roadmap#101]
        checks: [{ name: "ci/circleci: go-test", status: in_progress }]
        statuses: [{ context: "ci/circleci: go-build", state: success }]
        reviews: [{ by: bob, state: approved }]
  history:                                 # rows seeded in Postgres
    ci: [{ repo: giantswarm/klaus, durations: [20m, 25m, 30m] }]
    stages: [{ team: bumblebee, phase: Implementing, durations: [2h, 3h, 4h] }]
  jev:
    answers:
      - { point: "ready?", item: giantswarm/roadmap#101, answer: not-ready, confidence: 0.4 }
      - { point: "next?", person: alice, answer: giantswarm/roadmap#102 }
```

Refs are short throughout: `owner/repo#n` for issues and PRs, `owner/repo@sha` for commits. Times are RFC 3339 in UTC.

### 4.2 Step tools

**Injecting.** Every `github_*` tool that changes state sends the webhook GitHub would send. Each takes `deliver`: `delivered` (default), `lost` (GitHub logs a
failed delivery), `never` (nothing sent, nothing logged), `duplicate`, or `{ delay: 10m }`.

| Tool | Args | Webhook |
|---|---|---|
| `github_issue_create` | `ref`, `title`, `body`, `assignees`, `labels`, `createdAt`, `board` | `issues.opened` (+ `projects_v2_item.created` with `board`) |
| `github_issue_edit` | `ref`, any of `title`, `body`, `assignees`, `labels`, `state` | `issues.edited` / `assigned` / `unassigned` / `closed` / `reopened` |
| `github_board_set` | `issue`, any of `status`, `kind`, `team`, `priority` | `projects_v2_item.edited`, one per field |
| `github_board_add` / `github_board_archive` / `github_board_remove` | `issue`, `board` | `projects_v2_item.created` / `archived` / `deleted` |
| `github_sub_issue` | `parent`, `child`, `op: add\|remove` | `sub_issues` |
| `github_dependency` | `issue`, `blocked_by`, `op: add\|remove` | `issue_dependencies` |
| `github_pr_open` | `ref`, `author`, `head`, `closes`, `draft`, `base` | `pull_request.opened` |
| `github_pr_push` | `ref`, `head` | `pull_request.synchronize` |
| `github_pr_review` | `ref`, `by`, `state: approved\|changes_requested\|commented` | `pull_request_review.submitted` |
| `github_pr_merge` | `ref`, `merge_sha`, `by` | `pull_request.closed` (merged) and `push` |
| `github_pr_close` | `ref` | `pull_request.closed` |
| `github_check_run` | `repo`, `sha`, `name`, `status`, `conclusion` | `check_run.created` / `completed` |
| `github_status` | `repo`, `sha`, `context`, `state` | `status` |
| `github_installation_repos` | `add`, `remove` | `installation_repositories` |
| `github_fault` | `kind: http_error\|secondary_rate_limit\|rate_limit\|deliveries\|clear`, with `status`, `times`, `match`, `retry_after`, `api`, `remaining`, `reset_in`, `mode` | — |
| `webhook_post` | `event`, `action`, `body`, `signature: valid\|invalid\|missing`, `delivery_id`, `size_bytes`, `method`, `path` | the raw request; returns `{status_code}` |
| `clock_advance` | `by` | — |
| `jev_set` | `point`, `item` or `person`, `answer`, `confidence`, `error: timeout\|server` | — |
| `config_apply` | `teams`, `people`, `installation` (merged by name) | — (as Flux would apply it) |
| `engine_fault` | `kind: kill_leader\|restart\|stop_timers\|stop_decisions\|stop_freshness\|drop_waits\|forget_refusals\|gate_everything\|resume` | — |
| `postgres_fault` | `kind: down\|up` | — |
| `settle` | — | — (normally implicit) |

- `github_fault`: `match` selects the calls it applies to, by the same rule as `github_calls.path` (below) against the path, or exactly against a GraphQL
  operation name; `times` limits how often (default: until `clear`). `rate_limit` sets an API's `remaining` and `reset_in`, which also models other users of the
  App. `deliveries` with `mode: lost|never` affects every delivery until `clear`.
- `webhook_post` defaults: `POST /github/webhook`, `action` placed in the body, a generated `delivery_id`, and `size_bytes` pads the body inside the signature.
- `engine_fault` simulates engine bugs, so the invariants that catch them can be seen firing: `stop_timers` stops wait-deadline timers only (resyncs and
  freshness checks go on), `stop_decisions` stops decision passes, `stop_freshness` stops freshness checks, `drop_waits` stops creating waits, `forget_refusals`
  forgets recorded refusals, `gate_everything` makes every gate hold, `resume` removes every injected bug (overdue timers then fire at once). `kill_leader` and
  `restart` are faults of the environment, not bugs. These hooks only exist in test builds.

**Reading.** Each returns JSON for the expectations.

| Tool | Args | Returns |
|---|---|---|
| `get_item` / `get_change` | `ref` | the object (§5.1); fails with "not found" if there is none |
| `get_team` / `get_person` | `name` / `login` | the Team / Person object |
| `get_installation` | — | the Installation object |
| `get_budget` | — | the GitHub App Budget object |
| `list_items` | `team`, `phase`, `eligible`, `class` | `{count, items: [refs]}` |
| `events` | `item`, `change`, `type`, `source`, `since_step` | `{count, rows: [{type, item, change, source, at, data}]}`, oldest first |
| `decisions` | `point`, `item`, `since_step` | `{count, rows: [{point, item, subject, other, rule, jev, jevConfidence, jevError, chosen, mode, outcome, inputsHash, at}]}` |
| `metric` | `name`, `labels` | `{value, series}`: the sum over matching series (0 if none) and how many matched |
| `github_calls` | `since_step`, `replica`, `api`, `method`, `path`, `operation`, `code` | `{count, rest, graphql, app, graphqlPoints, mutating, conditional, notModified, modified, rateLimited}` |
| `jev_calls` | `point`, `since_step` | `{count, requests: [{point, item, person, fields}]}` |
| `reconciles` | `item`, `change`, `replica`, `since_step` | `{count}` |
| `object_writes` | `replica`, `since_step` | `{count}` |
| `replay` | — | `{decisions, mismatches}`: every recorded decision re-run through `decide` from its recorded inputs |

- A read tool without filters counts everything.
- `github_calls.api` is `rest`, `graphql` or `app`, and its `mutating` count excludes token minting.
- `github_calls.path` matches a REST path without its query: a prefix match, where `*` stands for one path segment. `operation` is a GraphQL operation name or a
  REST operation id from GitHub's OpenAPI description (`pulls/get`, `checks/list-for-ref`, `repos/list-commit-statuses-for-ref`, `issues/get`,
  `apps/list-webhook-deliveries`, `apps/get-webhook-delivery`). `code` filters by status code.

### 4.3 Expectations

As in muster:
- `success` (default **true**), `error_contains`
- `json_path`: a map of dotted paths to expected values. A number indexes an array, and `[key=value]` selects the array element whose `key` equals `value` (e.g.
  `status.shadow.actions[type=assign].args.login`, `status.conditions[type=BoardFilterValid].status`). Maps match partially, arrays exactly, `null` means absent
  or empty.
- `json_path_max`, `json_path_min`: numeric bounds
- `contains`, `not_contains`: case-insensitive substrings of the JSON result
- `wait_for_state: 10s`: retry the step until the other expectations hold

## 5. Vocabulary scenarios assert on

### 5.1 Objects (`beekeeper.giantswarm.io/v1alpha1`)

Everything the engine decides is in `status`; `spec` holds configuration only.

- **Item** `status`:
  - `issue {repo, number, title, state}`, `board {status, kind, team, priority}`
  - `class`: `Implementable` | `Epic` | `Incident` | `Ignored`
  - `phase` (§1.2), `assignees`, `owner`
  - `parent`, `subItems`, `blockedBy [{ref, state}]`
  - `targetRepos`, `acceptanceCriteria` (bool), `footprint` (paths)
  - `changes` (PR refs)
  - `eligible {value, reasons}`; reasons: `NotUpNext`, `TeamNotOptedIn`, `KindNotImplementable`, `AssignedToNonParticipant`, `NotReady`, `BlockedByOpenIssue`, `Closed`
  - `shadow.pick {person, rank}`; `shadow.heldBy {person, gate, detail}` with gates `TeamCap`, `EpicWIP`, `RepoConcurrency`; `shadow.actions [{key, type, args}]`:

    | `type` | `args` |
    |---|---|
    | `assign` | `{login}` |
    | `set-status` | `{status}` |
    | `start-run` | `{purpose}` (`implement`) |
    | `todo` | `{to, text}` (`to` is a login or `team:<name>`) |
    | `set-severity` | `{severity}` |

  - `wait {on, ref, since, deadline, atDeadline, state, reason}`; `on`: `dependency` | `ci` | `review`; `ref`: `issue:<ref>`, `ci:<repo>@<sha>`, `pr:<ref>`;
    `state`: `watching` | `overdue` | `unobserved`; `reason` (when not watching): `budget`, `source-degraded`, `http-<code>`
  - `refusal {operation, code, since}` while a read for it stands refused
  - `observed {at, source}`
- **Change** `status`: `pr {repo, number, state: open|closed|merged, draft, author, headSha, mergeSha}`, `items`, `ci {sha, state: pending|green|red|none, relevant, ignored}`
  (always the head SHA), `review {state: none|approved|changes-requested, needsOtherTeam}`, `mergeResult {sha, state: pending|green|red, relevant, ignored}`,
  `wait`, `refusal`, `observed`.
- **Team** `status`: `inFlight` (count), `wip {items, epics}` (engine WIP), `picks [{person, item}]`, `held [{person, item, gate}]`, `lastDecisionAt`.
- **Person** `status`: `wip {items, epics}` (engine WIP), `free {items, epics}`, `picks` (item refs, in order), `held [{item, gate}]`.
- **Installation** `status`: `sources` keyed by name (`webhooks`, `board`), each `{state: fresh|degraded, reason, since, polling}`; source reasons
  `deliveries-failed`, `delivery-log-unreadable`, `probe-gap`, `budget`, `filter-invalid`,
  `board-unreadable` (a failed or refused board check); `leader` (the replica's name); `lastDecisionAt`; `conditions`
  (`BoardFilterValid`).
- **Budget** (`github-app`) `status`: `rest {limit, remaining, resetAt}`, `graphql {…}`, `app {…}`, `deferred {rest, graphql}`.

### 5.2 Event types (Postgres `events`)

Every row has `at` (engine clock), `item` and/or `change`, `source` and `data`. `source` is `webhook`, `resync`, `reobserve`, `startup`, `deadline` or `engine`.
An `observed` row is written once per observed change, not once per read or delivery.

| Type | `data` |
|---|---|
| `observed` | `{kind, fields}`: `kind` is `board`, `issue`, `pr`, `ci`, `review`, `merge`, `sub-issues` or `dependencies`; `fields` names what changed (e.g. `pr.state`, `ci.state`, `board.status`) |
| `item.removed` | `{reason: team-changed\|archived\|deleted\|retention}` |
| `phase.changed` | `{from, to}` |
| `eligibility.changed` | `{value, reasons}` |
| `decision` | `{point, chosen}` (details in `decisions`) |
| `shadow.pick` | `{person, rank}` |
| `shadow.held` | `{person, gate}` |
| `shadow.action` | `{key, type, args}` |
| `wait.started`, `wait.woken`, `wait.ended`, `wait.deadline`, `wait.reobserved`, `wait.unobserved`, `wait.overdue` | `{on, ref, deadline, reason}` |
| `source.degraded`, `source.fresh` | `{source, reason}` |
| `webhook.missed` | `{event, ref}` |
| `refusal` | `{operation, code}` |

### 5.3 Metrics

| Metric | Labels |
|---|---|
| `engine_github_requests_total` | `api` (rest/graphql/app), `class`, `operation`, `code`, `conditional` |
| `engine_github_graphql_points_total` | `class` |
| `engine_github_rate_remaining` | `api` |
| `engine_webhook_deliveries_total` | `event` (the subscribed events and `ping`, else `other`), `outcome` (accepted, bad_signature, missing_signature, duplicate, too_large, ignored, unavailable) |
| `engine_observations_total` | `source`, `kind` (counts observed changes, not reads) |
| `engine_webhook_missed_total` | `event` |
| `engine_source_degraded` | `source` |
| `engine_polling_active` | `source` |
| `engine_waits` | `on`, `state` |
| `engine_wait_wakes_total` | `on`, `trigger` (webhook, resync, deadline, poll) |
| `engine_items` | `team`, `class`, `phase` |
| `engine_decisions_total` | `point`, `mode`, `outcome` (agree, disagree, jev_error, jev_low_confidence) |
| `engine_refusals_total` | `operation` |
| `engine_invariant_violations` | `invariant` |
| `engine_last_decision_timestamp_seconds` | — |
| `engine_history_dropped_total` | — |
| `engine_reconcile_errors_total` | `controller` |

## 6. Catalogue

[`internal/testing/scenarios/CATALOGUE.md`](../../internal/testing/scenarios/CATALOGUE.md) lists every scenario by concept, with its tags and its Given/When/Then. It's generated from the files.
