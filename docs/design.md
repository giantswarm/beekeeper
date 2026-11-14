# Design: a deterministic engine for the software development lifecycle

Status: **approved** · 2026-11-14, revised the same day after a design review
Inputs: the lessons from the previous system, a design review against Giant Swarm's conventions and decisions, and research into the roadmap board, the team's
planning workflow, the agent platform (agent-manager, kagent, klaus-gateway, muster), JEV, and the build and delivery process.

## 0. Summary

- **What it is.** One service per organisation ("installation") runs as a set of Kubernetes controllers on the agent platform. It turns opted-in board items into merged, released and proven changes.
  - The lifecycle is a state machine in plain code. Its decisions are pure functions of what it observes, so they can be replayed.
  - Agents are called only for work that needs judgement or language: implement, review and fix, fix CI, investigate, prove, and write to people.
- **Where things run.**
  - The engine always runs in the cluster: always on, woken by events, cheap when idle.
  - Laptops and build machines run a thin **runner** only for the agent sessions a subscription ties to them.
  - Heavy builds and tests never run on laptops by default. A hook and command shims send them to **executors**: substrate sandboxes with the same toolchains
    and workspaces as the platform's agents, caching by what actually changed. That includes commands that write the working tree, whose changes come back as a
    patch. The spike measured about 0.5 s of overhead per remote run.
- **Fewer CI runs.**
  - Work in progress lives in the engine's own git store, which CircleCI never sees.
  - The single review, including the reviewer's fixes, happens *before* the PR is pushed, and so do the proofs that can run before a merge (the agentlab). In
    the normal case a change costs one PR pipeline, plus the tag pipeline that is the release itself.
  - Later commits (CI fixes, requested changes, updates from main) get no second review. CI covers them.
  - Branches are not updated before merge unless what landed on main overlaps the PR. Main is watched instead, and a red main is reverted automatically.
- **Delivery.** Release, rollout and proof are observed through generic signals: tags, pullable OCI artifacts, Flux status, alerts. devctl is the Giant Swarm
  adapter. Promotion is per repo, a train of proven release candidates, and only for the release lines the installation's teams own.
- **Waiting.** Every wait names what it waits for and what happens at its deadline. Waits wake on webhooks and watches. Polling is a slow, cheap safety net and is visible whenever it's in use.
- **People.** People get decisions, questions and todos with a default and a deadline, through klaus-gateway in Slack. Reversible things never wait on a person.
  On a PR, a person's "changes requested" review blocks the merge and starts a fix.
- **State.** Live state is Kubernetes objects, metrics go to Prometheus, and the per-item history is an event table in Postgres.
- **Decisions.** Every judgement call in the flow is a named **decision point**. A plain rule always has an answer, and JEV can be added. Both answers are recorded, so metrics show which one to trust.
- **Limits.** WIP is limited from implementation to proof, epics especially. It starts low and is raised as the metrics show each stage is healthy.
- **Self-checks.** The engine checks its own invariants continuously. A violation raises an alert, which a responder that doesn't depend on the engine
  investigates and fixes at once. Agents report problems they find through a single tool that deduplicates them, and they never switch tasks to chase those
  problems.
- **Guardrails.** Process guardrails guide first and enforce only once they've proven themselves. Safety guardrails are hard from the start.

## 1. Goals and what "working" means

These are directions, not numbers. Numbers given to agents become targets to game, and the design never gives them any. Limits are configuration values for the
engine, tuned from metrics. In order of priority:

1. Shorter lead time from "Up Next" to proven.
2. More merged PRs.
3. Higher quality: fewer reverts, red mains, failed proofs and incidents caused by changes.
4. Fewer resources and tokens: CPU on laptops, CI runs, test executions, GitHub calls, model tokens.
5. Human interaction that is worth the person's time.

"Working" means each of these is visible as a metric (§15) and moves in the right direction as the WIP limit and decision points are tuned.

## 2. Principles

Each principle answers a failure of the previous system; §19 maps them one by one.

1. **Level-triggered.** On every change, the engine compares what it wants with what it observes. It never depends on having seen a particular event.
2. **One writer per fact.** Only the engine assigns work, and a claim is a field it sets. Runners never claim issues; they accept runs.
3. **Decisions are pure functions.** `decide(observed state, policy, recorded answers) → actions`. Answers from people and JEV are recorded as inputs, so a
   replay of the event log reproduces every decision.
4. **Actions are idempotent and keyed.** Running a reconcile again never duplicates a PR, comment, review request or merge.
5. **A refusal is not retried until its inputs change.**
   - Errors are *transient* (back off and retry), a *refusal* (stop, show the reason, wait for a change), or a *bug* (alert the maintainers).
6. **"Not progressing" and "not observed" are different things.**
   - Every observation records its age.
   - Every actor (run, executor, lease holder) renews a heartbeat.
   - Stall alarms come only from missing heartbeats or missed deadlines.
7. **Gates only gate starting new work.**
   - Work already in WIP always continues.
   - Repairs to main, reverts and incidents always pass.
   - Any work that reduces the gated quantity passes.
8. **Every wait is explicit:** what it waits for, since when, until when, and what happens at the deadline.
9. **Events, not polling.** Webhooks and Kubernetes watches wake work. Polling is the fallback: cheap (conditional requests), slow, and reported as such.
10. **The engine never invents work for itself, and never lets its own failures go unnoticed.**
    - No backlog issues are filed about itself.
    - Its malfunctions are invariant violations. They raise alerts, and those alerts get an immediate investigation and fix from a responder that doesn't depend on the engine (§12.2).
11. **Use the platform instead of rebuilding it:**
    - agent-manager and kagent for agent sessions in the cluster
    - klaus-gateway for Slack
    - muster for tools
    - Flux for deployment
    - Prometheus for metrics
    - CloudNativePG for the per-item history
12. **Interfaces at the edges, one implementation first.** Each edge gets a narrow interface: forge (GitHub, later GitLab), board, notifier (klaus-gateway,
    which talks Slack at Giant Swarm and Matrix elsewhere), CI observer, delivery steps, agent providers.
13. **Small and boring.** One Go module and one binary with several roles, controller-runtime, and few dependencies. Any feature the platform already offers is used, not rebuilt.
14. **Guide first, enforce later.** A bug in a strict guardrail can stop everything, as happened last time. So guardrails come in two classes:
    - **Process guardrails** (the report tool, command shims, no direct issue creation, run limits):
      - They start in *guide* or *warn* mode, fail open, and every bypass is logged.
      - Each is promoted to *enforce* only once its metrics show it never blocks wrongly.
    - **Safety guardrails** are hard from day one: no secret access, no bypassing security scans, merge only a green SHA that descends from the reviewed one, no admin merges.

## 3. Shape: what runs where

```
                      +---------------- agent platform cluster (one namespace per installation) ----------------+
 GitHub / CircleCI -->| webhook receiver -+                                                                      |
 Alertmanager      -->|                   v                                                                      |
 Flux / installations>|            ENGINE (controllers, leader-elected, 2 replicas)                              |
                      |   board sync · scheduler · change/merge · delivery · incidents · asks · budgets · leases |
                      |      |            |              |                 |                                     |
                      |  CRDs (live)   Postgres       git store        executors: substrate sandboxes            |
                      |  state+config  (history)      (WIP branches,   (toolchains + workspaces, like agents)    |
                      |                                checkpoints)    agent-manager sessions (API-key agents)   |
                      |      |                                                                                   |
                      |  runner API (gRPC, Dex OIDC) <-------------+        klaus-gateway <-> Slack             |
                      +--------------------------------------------+---------------------------------------------+
                                                                   | outbound only
                     +-----------------------------+   +-----------+--------------+
                     | build machine               |   | laptop                   |
                     | runner (agents) + executor  |   | runner (agents only)     |
                     | (docker, kind, e2e)         |   | shims+hooks → executors  |
                     +-----------------------------+   +--------------------------+
```

- **Installation.** One per organisation: Giant Swarm, a personal setup (on its own cluster), and each agent-platform customer (in their
  `AgentPlatformOrganization` namespace). Installations share code, never data or credentials, and they never coordinate with each other.
- **Engine.** Kubernetes controllers in Go, with two replicas and leader election. Restarting or upgrading it is a leader handover of a few seconds, with no "wait to restart" state.
- **State.**
  - *Live state and configuration* are custom resources: small, watchable, and visible with `kubectl`. Configuration lives in a git repo applied by Flux, so changes are reviewed and drift is visible.
    - The live objects are Items, Changes, Runs, Asks, Runners and Budgets. Everything operation needs is on them, including a person's answers and each wait's deadline.
    - There are few of them and they change rarely: about a hundred for one team and one or two thousand for ten teams, with well under one write per second.
    - Heartbeats and leases are Kubernetes `Lease` objects. A Wait is a field on its Item or Change, not an object of its own.
    - Executions are never custom resources. An agent runs too many commands for that; they are history only.
  - *Metrics* go to Prometheus (§15).
  - *History* is an append-only event table in Postgres (CloudNativePG): every transition, run, execution, test case, ask and decision with its inputs. It
    answers questions about one item or one test, which need a record of events and not a number. Losing it loses that history, not operation and not the
    metrics.
  - *Work-in-progress branches* live in the engine's own **git store**: bare repos in the cluster, or the workspace-manager mirror if it accepts pushes.
- **Runner.** One binary on laptops and build machines.
  - It logs in with the person's Dex identity, connects *out* to the engine, and accepts runs.
  - It starts the agent CLI with the subscription logged in on that machine, and streams heartbeats, progress and results.
  - When idle it is a single process blocked on a stream. It never polls and never spawns.
- **Agent sessions in the cluster** go through agent-manager, using API keys or platform models. To the scheduler they look like one more runner. Their liveness
  comes from the platform, not from a runner (§5.4).
- **Executor.** Executors use the same technology as the platform's coding agents: **substrate sandboxes** with the same toolchains and workspaces. That way an
  executor and an agent see the same tools and the same repo.
  - Build, test, vet and lint run there.
  - Docker, kind and the agentlab need privileges that a gVisor sandbox may not grant, so they run on build machines until substrate can host them.
  - Tool caches persist per repo, in the workspace or a cache volume the sandbox mounts.
  - This needs platform work first (§21): sandbox templates with toolchains for the engine's identity, egress to the git store and module proxies, and caches
    that outlive a sandbox. Until it lands, plain pods with a cache volume (as in the spike) are the stand-in behind the same executor interface.
- **Network.** Clusters may deny all traffic by default. Every component ships its own network policies, including DNS.

## 4. Core concepts

| Concept | What it is | Source of truth |
|---|---|---|
| Team | Scope and opt-in unit (see below). A board `Team` option maps to the GitHub team `team-<slug>`. Policies: WIP limits, channel, owned release lines (§8.2). | config repo |
| Person | GitHub login, Dex identity, Slack identity (klaus-gateway linking), teams, runners, providers, lending opt-in, quiet hours | config + GitHub team membership |
| Item | One board issue the engine is responsible for: kind, owner (assignee, set when the engine picks an unassigned item), team, priority, parent, blockers, target repos | board + engine status |
| Epic | An Item with sub-items, and the unit of "topic WIP" | board (Kind `Epic 🎯`, sub-issues) |
| Change | One PR in one repo for one Item. Usually 1:1; a multi-repo item has several Changes, ordered by dependencies. | engine + forge |
| Run | One agent session with one purpose (§9). Holds a heartbeat lease. | engine |
| Execution | One build or test command run by an executor for a Run, cached by its inputs | engine + Postgres |
| Wait | What an Item or Change is waiting for, since when, until when, and what happens at the deadline | engine |
| Ask | A decision, question or todo for a person or team, with a default (only if reversible) and a deadline | engine + notifier |
| Lease | Exclusive, queued use of a scarce resource: lab cluster, test installation, CI slot | engine |
| Budget | Pace and remaining capacity for each provider subscription, API key, GitHub App and CI | engine |
| Decision point | A named, typed judgement taken by a rule and/or JEV, recorded with its inputs | engine + Postgres |
| Pipeline | The delivery steps for a repo: release, then rollout(s), then proof(s) | config, defaults by repo type |

**Teams.** A team's opt-ins are *work* (which kinds) and *alerts*. Its owned repos come from repository metadata (§8.5).

**Kinds.** The engine reads the board's `Kind` field, because almost nothing uses the official issue types (only `Task`, `Bug` and `Enhancement` exist). A small
mapping table lets it switch to issue types later.

| Kind | Treated as |
|---|---|
| `Epic 🎯` | Epic |
| `Story 📑`, `Task 🗒️`, `Feature 💫`, `Bug 🐞` | Implementable |
| `Alert 🚨`, plus incoming alerts | Incident |
| Everything else | Ignored for now |

## 5. Lifecycle

### 5.1 Item (implementable)

```
 Inbox/Backlog ──person: spec done, Status "Up Next"──► READY
 READY ──scheduler: eligible + gates pass + placed──► IMPLEMENTING        ┐
 IMPLEMENTING ──run done, executor checks pass──► REVIEWING               │
 REVIEWING ──review run: fixed, checks + proofs pass──► PUBLISHED         │ WIP
 PUBLISHED (PR open, CI #1) ──red──► FIXING ──► PUBLISHED                 │ (counts
 PUBLISHED ──green (+ other team's approval if their repo)──► MERGING     │  from here
 MERGING ──decision: merge as is / update first──► MERGED                 │  to DONE)
 MERGED ──main CI green──► RELEASING ──► ROLLING_OUT ──► PROVING ──► DONE ┘
 PROVING ──proof fails──► REOPENED (fix-forward Change or revert) ──► IMPLEMENTING
 any ──► WAITING(reason, deadline) ──► back to where it was
 any ──attempts exhausted / refusal──► ESCALATED (an Ask to the owner)
```

| State | Who acts | Board `Status` |
|---|---|---|
| READY | person sets "Up Next"; code checks eligibility and assigns the person it picks the item for | `Up Next ➡️` |
| IMPLEMENTING, REVIEWING, FIXING | agent (Run) and code (executor) | `In Progress ⛏️` |
| PUBLISHED, MERGING, MERGED | code | `In Progress ⛏️` |
| WAITING on a person or another team | person | `Blocked / Waiting ⛔️` |
| RELEASING, ROLLING_OUT, PROVING | code, plus an agent for proofs | `Validation ☑️` |
| DONE | code closes the issue | `Done ✅` |

- **Status ownership.** Once the engine claims an item, it is the only writer of its `Status`.
- **Human edits are inputs.** A person moving a claimed item back to Backlog cancels it: runs stop, the PR is closed and the branch is kept. Changing the priority reorders the queue.

### 5.2 Epic

| Phase | Meaning |
|---|---|
| PLANNED | Has sub-items; none started |
| ACTIVE | At least one sub-item is past READY. Takes an epic WIP slot. |
| DONE | All sub-items are DONE or cancelled |

- An epic has one owner, and sub-items inherit it.
- Sub-items of an ACTIVE epic don't need a new epic slot. This is how finishing comes before starting.

### 5.3 Change (PR)

Conditions in order:

1. `Drafted`: the branch exists in the git store.
2. `Reviewed(r)`: the one review ran at SHA *r*, its findings are fixed, and the pre-publish proofs passed (§8.3).
3. `Published`: the PR is open at SHA *s*, which is *r* or descends from it.
4. `CIGreen(s)`
5. `Approved(s)`: only for another team's repo.
6. `NoChangesRequested`: no person's "changes requested" review is open (§7.2).
7. `Merged`
8. `MainGreen`: the post-merge CI on main passed.

There is exactly one review. Commits after it come from the reviewer's own fixes, from CI fixes, from requested changes or from an update from main, and they
get no second review: CI is enough. The engine merges through the API with `sha=s`, so only a SHA that went green and descends from the reviewed one can ever
merge.

### 5.4 Run

1. `Pending`
2. `Placed`: a runner accepted it and holds a lease.
3. `Running`: heartbeats arrive, along with progress such as tool calls, checkpoints and tokens.
4. One outcome:
   - `Succeeded`
   - `Failed(reason)`
   - `Lost`: the lease expired.

A lost or failed run is retried according to policy, on another runner and possibly another provider, from the last checkpoint in the git store with a handoff
summary. When attempts run out, the item is ESCALATED.

**Liveness depends on where the run is.**
- **Runner on a laptop or build machine:** the runner renews the run's `Lease` and streams progress. A missing renewal means `Lost`.
- **Session in the cluster** (agent-manager or kagent): the platform knows the session's state.
  - A session can stay `READY` while its turn has `FAILED`, so the engine judges a run by its **last turn's state and timestamp**. `FAILED`, `REJECTED` and
    `CANCELED` mean a failed run, and `INPUT_REQUIRED` means the agent needs an answer.
  - The platform is to emit session events and offer a cancel API (§21). Until then, the engine polls `get_session` with `lastTurns: 1` for *active* cluster
    sessions only, at an interval taken from the purpose's expected turn time. It can't stop a session, only stop waiting for it.
  - Before placing work, it checks the agent's readiness with `get_agent_status`. That check doesn't prove the model backend is reachable, so a turn that fails
    within seconds counts as a placement failure, not as the item's failure.
- **Handoff is never a fork.** The platform refuses to fork a checkpoint that includes process state. A run continues in a fresh session, started from a summary
  of the previous one plus its branch in the git store.

## 6. Scheduling

### 6.1 Eligibility

All of these are checked by code:

- The team has opted in, and the Kind is implementable.
- Status is `Up Next ➡️`, the human "may start" signal. Prioritisation stays with people.
- The item is unassigned, or assigned to a participating person (a Person of an opted-in team). An item assigned to anyone else is never touched.
- The **definition of ready** holds: acceptance criteria are present and the target repo is known. This is decision point `ready?`. If the answer is "not
  ready", the owner gets a todo, "refine with `/gs-plans:gs-plan`", and nothing starts.
- No native "blocked by" dependency is still open.

**Picking and assigning.** The engine picks work per person: for a person with free capacity it takes the next eligible item, in the order of §6.3, that is
assigned to that person or to nobody. Once it has picked an item, it assigns the person, who is the item's owner from then on. Unassigned sub-items of an epic
go to the epic's owner first.

**Bugs** skip refinement but still need "Up Next" from a person, with one exception:
- Every new bug is classified by decision point `bug-severity?` (JEV choice: `critical`, `high`, `medium`, `low`). The criteria are configuration, reviewed like code.
- The severity is written to a board field `Severity`, or a label if adding a field isn't wanted.
- Only `critical` bugs go to "Up Next" automatically. Their owner is the team's configured triage owner.
- All other levels are a strong hint, but people prioritise them.

### 6.2 Gates

Gates apply only to starting work:

- **WIP per person:** each participant has a limit for items and one for epics, counting only the work the engine has started for them, never items people track by hand. The limits start low.
- **Team cap (optional):** a limit across all of a team's participants, unset by default.
- **Repo concurrency:** another Change may run in the same repo only if their predicted footprints are disjoint.
  - This is decision point `parallel-safe?`.
  - The rule: sequential per repo unless the specs name disjoint paths or packages.
  - Native dependencies inside an epic always win.
- **Placement:** a runner with a suitable provider must be under its pace.

Exempt from all gates: incidents, repairs to main, reverts, and work that unblocks in-flight work.

### 6.3 Order

Deterministic:

1. incidents
2. main repairs
3. work that unblocks WIP
4. items in ACTIVE epics
5. the board's `Priority` number
6. age

Decision point `next?` runs JEV in shadow mode first (§14).

### 6.4 Placement and budgets

- **Assignment and placement are separate.** The assignee says whose item it is; placement says on which machine it runs. An item can run on a colleague's
  lending runner while its owner stays the same. This is how a team's issues are spread over its members' machines.
- **Candidates, in order:**
  1. the owner's runners
  2. runners in the same team whose person has opted in to lending
  3. other lending runners
  4. cluster sessions (API key), if team policy allows. Incidents always may.
- **Provider choice** by budget pace: the subscription furthest under its weekly pace wins, and others take over when one's short window is spent. This worked last time.
- **Reviewer diversity:** the reviewer uses a different provider from the implementer when budget allows.
- **Machine capacity:**
  - Each runner's own configuration sets how many **slots** (concurrent agent sessions) the machine offers. The person sets it on the machine.
  - The runner also watches **pressure**: CPU load, free memory, free disk, the GitHub API budget of the identity used on that machine, battery, and a "busy" flag the person can set.
  - It reports free slots and pressure on its stream whenever they change.
  - The engine keeps each machine's remaining capacity. A machine takes work only when a slot is free *and* no pressure signal is over its threshold, so a
    machine can be full before all its slots are used.
  - The thresholds are machine configuration, tuned from metrics.
- **Capacity per person** is the remaining capacity of that person's machines. That's what "a person with free capacity" means in §6.1. Lending adds
  colleagues' opted-in machines for placement, but never changes whose item it is. The team's WIP limit still applies on top.
- **Session affinity:** CI fixes and follow-ups prefer the runner that holds the session, so it can be resumed and its context reused. If that runner doesn't
  take the work quickly, any runner continues from the git store.

## 7. Builds, tests and CI

### 7.1 Executors instead of laptop builds

- **Enforced by mechanism, not by prompt.**
  - The runner puts **command shims** on the agent's `PATH` for build and test tools (`make`, `go`, `docker`, `helm`, `kind`, `npm`, …). Shims work with every CLI: Claude, Codex, Gemini.
  - Where the CLI supports hooks, such as Claude Code's `PreToolUse`, a hook backs this up.
  - A per-repo command manifest, generated from the Makefile and CI targets, says which commands are heavy.
  - Every heavy command goes to an executor, whether it reads or writes: test, build, vet, lint, e2e, image builds, and also `go mod tidy`, generators and
    formatters. The executor runs in a substrate sandbox with the same toolchains and workspace as an agent, so a writing command works there exactly as it
    would locally.
- **How it works.**
  1. The runner snapshots the agent's working tree into a hidden checkpoint commit without touching the agent's branch.
  2. It pushes the checkpoint to the git store.
  3. An executor that already holds the repo and its caches fetches it into a work tree of its own for this run, runs the command, and streams the output back.
  4. If the command changed the work tree, the executor returns the change as a binary patch against the checkpoint. The runner applies it to the agent's working tree before the command "returns".
     - The agent is waiting for its command, so nothing else edits the tree meanwhile.
     - Files that git ignores (build outputs) stay on the executor, because later commands run there too.
     - An agent session that runs in the cluster shares the workspace with its executor, and needs no patch at all. The runner keeps a persistent stream to the
       executor, so no connection is set up per command, and paths in the output are mapped back to the agent's.

  To the agent it looks like a local command, and the checkpoint doubles as crash recovery.
- **Measured in the spike** (on a test cluster, Go repo `giantswarm/klaus`):
  - A remote `go test` costs about 0.5 s more than a local one, mostly `kubectl exec`, which the persistent stream removes.
  - A cold run took 22 s remote against 46 s on the laptop. A real edit re-ran only its package, in 1.75 s.
  - Laptop CPU per run fell from up to 300 s to about 0.5 s.
- **No needless runs.**
  - **Change classifier.** Each Execution is keyed by (repo, command, hash of the inputs that command depends on).
    - For Go, the inputs come from the package graph.
    - Otherwise, from path rules in the manifest.
    - A docs-only change runs no tests at all.
  - **Tool caches.** Native caches (`GOCACHE`, buildkit, npm) persist on executors, which gives package-level test caching for free.
- **No "fast stays local" rule.** The round trip costs about half a second, which doesn't justify a decision point. Every heavy command goes remote, and only the docs-only skip remains.
- **Every Execution and every test case is recorded** (from `go test -json` and JUnit output) against its run, change and item. That shows which tests are slow and how often they ran for one issue.

### 7.2 Publish points: the PR exists to merge

- **CI never sees work in progress.** It lives in the git store, so CircleCI doesn't run on it, whatever the repo's branch-build setting. At Giant Swarm this
  matters doubly: CircleCI builds *every* branch push, and `branchPublish` pushes dev images.
- **The review happens before the PR exists.**
  1. The implementer finishes, and executor checks pass.
  2. The review run reviews the branch against the issue and spec, fixes its own findings, and re-checks on executors.
  3. The pre-publish proofs run (§8.3). For an agent platform component that is the agentlab on a build machine, built from the checkpoint.
  4. Only then does the engine push and open the PR, which triggers CI run 1.
  5. The review record (findings, fixes and proof results) is posted on the PR as the App's review.

  This still meets "exactly one review, by the agent that then fixes its findings". The review policy says so in these words, and the record on the PR makes
  each run verifiable, which is what ISO 27001 asks of an automated review.
- **CI only runs again when something forces it:**
  - CI is red: a fix run, then a push.
  - A person requests changes: a fix run, then a push.
  - Docs-only follow-ups are held back and go with the next push.
- **The PR itself:**
  - It carries the issue link and summary, the executor results, and the owner as assignee.
  - For another team's repo it carries exactly one review request. Reminders go to the owner, never to the colleague.
- **People on the PR:**
  - A "changes requested" review from anyone with write access blocks the merge and starts a fix run. The review bypass (§7.3) would let the engine merge past
    it, so the engine holds this rule itself.
  - A plain comment doesn't block. It goes to the owner in their digest, so nothing waits on an unanswered remark.

### 7.3 Merge

- **CI observer.** How the engine learns that CI is green is a provider, set per installation.
  - *Generic (personal, other organisations):* GitHub check runs and commit statuses on SHA *s*.
  - *Giant Swarm:* **devctl's verdict** (`pkg/prwait`, the logic behind `devctl pr wait` and `pr merge`), in its CircleCI-token-free mode
    (`--circleci-from-statuses`, which `devctl serve` already uses).
    - **Why GitHub alone isn't enough.** A CircleCI job behind `requires:` posts nothing to GitHub until it starts, so a multi-stage pipeline looks green
      between its stages. A required check that hasn't reported is simply absent from the API, not "expected". Some required checks only exist on some commits.
      Rulesets can't tell "complete" from "between stages".
    - **What devctl does.** It reads the repo's `.circleci/*.yml` from GitHub, computes the jobs this branch will run, and waits until each has posted a passing
      status. It also combines check runs, statuses, Actions runs and the rulesets' required contexts as a backstop.
    - **How the engine uses it.** The engine imports that verdict as a library and feeds it the signals it already observes through webhooks. There's no extra
      polling and no blocking call, and the merge gate follows the same rules people's `devctl pr merge` follows.
    - **When devctl can't predict the jobs** (matrix jobs, conditions, a runtime continuation, release-assets repos), it says so in a warning. The engine then
      treats the verdict as incomplete. Where the installation has a read-only CircleCI credential, it asks CircleCI. Otherwise it sends the owning team one Ask
      to make the repo's job set predictable, and the repo isn't agent-merged until then.
    - **CircleCI read access is optional:** for those repos, and for job and test timings in the metrics. Re-triggering pipelines would need write access and is left out.
- **Gate:** required checks green on SHA *s*, including the security scans (never bypassed and never admin-merged), the other team's approval where needed, and no open "changes requested" review.
- **Approval identity.**
  - Giant Swarm rulesets require one approving review (`required_approving_review_count: 1`).
  - They don't require up-to-date branches (`strict_required_status_checks_policy: false`).
  - Merges are squash only, with the squash title taken from the PR title.
  - A PR's author can't approve it, so the engine doesn't approve at all. It posts its review as a `COMMENT` review, and the merge goes through a **ruleset bypass**.
- **At Giant Swarm** (checked on four repos, among them muster, klaus and devctl):
  - `devctl repo reconcile` writes two rulesets per repo: `devctl: default branch` holds the required checks, and `devctl: review` requires one approval.
  - Bypass actors in `pull_request` mode are Admin, the owning team, Renovate, and the **`giantswarm-devctl` App**.
  - Today devctl never uses that App bypass. It acts with a person's user-to-server token, so GitHub evaluates the merge as the person and applies *their* team bypass.
  - `agentMerge: false` in Giant Swarm's repository configuration removes all bypass actors. That is set explicitly almost nowhere, so agent merges are the default.
- **Decision:** at Giant Swarm the engine acts as the `giantswarm-devctl` App installation itself, with a server-to-server token. The App's existing bypass then
  applies, which fits devctl becoming part of the platform (§8.4).
  - For the engine this replaces "devctl acts on GitHub as the person", the rule of the devctl MCP server decision. That rule still holds for people and for agents calling devctl themselves.
  - As a person, GitHub declines a merge outside the person's own team. As the App it doesn't, so "another team's repo needs that team's approval" is the
    engine's rule to enforce (a safety guardrail, principle 14).
  - The App's private key lives in the platform's secret store, and no agent ever sees it.
  - Narrow the App's bypass to the `review` ruleset only. Then GitHub itself enforces required checks and security scans for the engine, instead of trusting the
    engine's own check (a safety guardrail, principle 14). The App's bypass entry is unused today, so this changes nothing for people using devctl.
  - Repos with `agentMerge: false` have no bypass. There the engine sends one review request to the owning team and waits visibly.
  - Phase 1 verifies the App-as-itself bypass on one repo before anything relies on it. It's documented but nobody has used it yet.
- **Generic (personal, other organisations):** the installation's own App is a bypass actor on the review rule only, or a second App approves.
- **Decision point `update-before-merge?`:**
  - The rule updates only if what landed on main since the PR's base overlaps the PR's footprint (files or packages), or if branch protection requires it.
  - Otherwise the PR merges as it is, so parallel PRs no longer re-run each other's CI.
  - JEV is an advisor here; its use is approved. The metric to watch is red mains caused by merges that skipped the update.
- **Squash commit:**
  - The subject follows the release convention, which drives the release tag.
  - The body carries `Co-authored-by: <owner> <id+login@users.noreply.github.com>`, so the owner is in the git history.

### 7.4 Main is watched

Whatever CI first runs on the merge commit is the merge-result check. At Giant Swarm that's the release candidate's tag pipeline, because generated branch jobs ignore `main`. When it goes red:

1. Determine the culprit. Every merge gets its own tag pipeline, so normally the culprit is the merge whose own pipeline failed first. The engine bisects on
   executors only when that is ambiguous, for example when two merges landed before the first pipeline finished or the failure looks flaky.
2. A **revert Change** (plain `git revert`) is published at top priority and goes through the same publish, review and merge flow. The review run here is short.
3. If the revert doesn't apply cleanly, a fix-forward run starts instead.
4. Merges pause **for that repo only**, except repairs.

**Flaky tests:** decision point `flaky-or-broken?` (rule: the same SHA passes on a single rerun). A test classed as flaky becomes a metric and an item in the
owning team's digest. It is never a global stop.

## 8. Delivery: release, rollout, proof

### 8.1 Generic model

A repo's **pipeline** is an ordered list of steps. Each step has:
- an *observer*: the signal that means the step is done
- optionally an *actuator*: what the engine may do to make it happen
- optionally *gates*: a lease, a time window, a decision

Steps are providers behind interfaces. The defaults use generic cloud-native signals, and anything organisation-specific lives in an adapter.

| Step | Generic default | Giant Swarm adapter |
|---|---|---|
| merge result | check runs on the merge commit or its first tag | the RC tag pipeline: generated branch jobs `ignore: main`, so it's the first CI to see the merge |
| release | semver tag plus GitHub release *and the artifacts can be pulled* (the OCI image and chart resolve) | auto-release (git-cliff) tags `vX.Y.Z-rc.N`; "released" as below |
| rollout (per target) | Flux on the target cluster, with the checks below | testing-stage MCs take RCs automatically; checks as below |
| proof (per target) | a scripted suite on an executor under a lease; an agent proof against the acceptance criteria; an alert window | see below |
| promote | create or move the stable tag or channel | `devctl release promote`, which puts the stable tag on the latest candidate |
| rollout (stable) | the same Flux checks on the stable targets | stable-stage MCs pick it up through semver ranges within one poll (about 10 min) |

- **Released at Giant Swarm:** "released" follows `devctl release wait`, and `devctl release failed` reveals releases that are "Not published".
- **Flux checks for a rollout:** `HelmRelease` is Ready at its current generation with the target version; `Kustomization`/source `lastAppliedRevision` contains
  the commit; workloads are rolled out. At Giant Swarm they are the checks of `devctl rollout wait`, run over the platform's connection to installations.
- **The alert window:** no new alerts for the team or app on the target since rollout.
- **Proofs at Giant Swarm:** before publish, the agentlab (`platform-test`, `backstage-test`, Playwright) on build machines (§8.3). After release, the testing
  installations under an engine lease, and ATS chart tests.

### 8.2 The Giant Swarm flow as the engine sees it

```
MERGED ──auto-release──► RC_TAGGED (vX.Y.Z-rc.N)
RC_TAGGED ──tag pipeline green + artifacts pullable──► RC_RELEASED
RC_RELEASED ──testing MCs: rollout checks pass──► RC_ROLLED_OUT
RC_ROLLED_OUT ──proofs pass (lab / test installation / alert window)──► RC_PROVEN
RC_PROVEN ──promote (per repo, see below)──► STABLE_RELEASED ──stable MCs rolled out──► DONE
```

- **Release tags come from squash subjects.**
  - The squash title is the PR title, which `semantic-pull-request` validates.
  - The engine writes PR titles as conventional commits. The implementer proposes the bump type (`fix`, `feat`, `!`), and the reviewer checks it.
  - git-cliff silently drops a subject it can't parse, so the engine parses the subject itself before merging.
- **A tag is not a release.**
  - A tag can exist while its pipeline failed. "Released" always means the artifacts can be pulled.
  - Auto-release already re-triggers lost webhooks. The engine has read access only and re-triggers nothing: a tag pipeline that stays failed becomes an Ask to the owner, and the item waits visibly.
- **Promotion is per repo, not per item.** It promotes every candidate since the last stable release, which can include several items.
  - So promotion is a **repo-level train**: it waits until every item in it is `RC_PROVEN`, reverted, or explicitly excluded, and those items reach DONE together.
- **Stages.** Giant Swarm's installations form a ladder: *testing* (two testing installations), *stable-testing* (Giant Swarm's test and operations MCs), one
  more installation, and *stable* (every customer MC). Candidates reach testing on their own. Today a stable release reaches every other stage within one poll.
- **Owned lines only.** The configuration names the release lines each team owns, and the engine promotes only those.
  - A colleague's hand-merged commit in an owned line rides along in the train without their yes and without a ping. The candidate's proofs and alert window cover it.
  - A line the installation's teams don't own is never promoted, bumped or rolled back by the engine.
- **Promotion is automatic** (decided), with no human promotion step for any owned line, the engine's own repo and the platform it runs on included. The engine promotes a train when all of these hold:
  - every item in the train is `RC_PROVEN`
  - the alert window on the testing-stage MCs is clean
  - no incident is open for the repo
  - no person has set a hold on the repo or team. The hold is a one-click switch in Slack or a config flag, and the only manual brake.
- **Why the safeguards matter.** Today a promotion reaches every stable MC, and the customers on them, within about 10 minutes, with no soak and no ceiling per customer.
- **The engine promotes now, the ladder later.** Promotion sits behind a promoter interface. A separate proposal at Giant Swarm suggests promotion on evidence,
  a soak on an intermediate installation and a ceiling per customer, owned by the team that owns the stages. When that lands, the engine stops promoting. It
  then supplies the evidence (proofs, alert window) and the holds, and observes the result.
- **After promotion:**
  - Alerts on stable installations that start within the rollout window are linked to the train.
  - They open an incident at top priority, which leads to a fix or revert.
  - The repo's automatic promotion pauses until that incident is closed.
- **Labs.**
  - No reservations in the first version. `devctl reservation` puts a PR's branch builds on an installation before the merge, and it doesn't cover the extras
    collection, where the agent platform's components live. The engine keeps work in progress away from CircleCI, so it has no branch builds to deploy.
  - Candidates reach the testing installations on their own. The engine observes the rollout, proves there, and serialises its own proofs with a lease.
  - People still reserve apps with `devctl reservation` (per app). The engine reads those reservations and doesn't prove an app while a person has it reserved;
    the wait shows who holds it and until when.
  - agentlab runs before publish on the executor's own build machine and needs no shared lease.

### 8.3 Proof plan

- **Fixed at READY.** Each Item's proof plan combines the repo's default steps with the proofs named in the spec's acceptance criteria. Specs say things like
  "proven in the agentlab, then on test installations".
- **Two places for a proof.**
  - *Before publish:* whatever can be proven from the branch, during REVIEWING. An agent platform component is proven in the agentlab before its PR exists: a
    build machine builds the dev image from the checkpoint, swaps it into the lab and runs the lab's suites. A failed proof goes back to the review run, not to
    main.
  - *After release:* what only a real installation shows: Flux delivery, the candidate on the testing installations, the alert window.
- **Each criterion becomes something concrete:** a scripted check, an agent proof (a Run with the purpose *prove*), or an observation such as an alert window.
  - Decision point `proof-plan?` proposes the mapping, and the review run confirms it.
- **When a proof after release fails, the Item reopens.**
  - The default is a fix-forward Change.
  - It's a revert when the candidate has already reached a stage people depend on.

### 8.4 What the engine needs from devctl

devctl becomes the Giant Swarm adapter for promote, release state and rollout checks. For that it needs:

- **The verdict as a library.** `pkg/prwait`'s verdict must be callable on signals the engine supplies, for a given SHA, without blocking.
  - For people and other services: a one-shot `pr wait --once --sha <s>` with the existing envelope and exit codes. Today only `devctl serve` comes close, with a 20 s timeout and no SHA argument.
- **CircleCI-token-free mode as the default** (`--circleci-from-statuses`), with a CircleCI token optional for a failed job's log tail, reruns, and repos whose
  jobs can't be predicted. Then everybody can use devctl without a CircleCI token, and the engine's merge gate is the same as people's.
- **A service identity** instead of tokens in a person's keychain: the `giantswarm-devctl` App installation itself (§7.3).
- **Promote** callable by the engine's identity.
- **Importable Go packages**, ideally, so the adapter doesn't shell out.

Everything else (architect-orb, generated CI, the shared GitHub workflows) the engine only *observes* through generic signals: check runs, tags, OCI, Flux. It never depends on how they're built.

### 8.5 Repository metadata

For each repo the engine needs:
- the owning team
- the checks that are relevant to merging
- whether agents may merge
- the pipeline type

Sources:
- **Generic provider:** GitHub team permissions, CODEOWNERS, and branch rulesets (required checks).
- **Giant Swarm adapter:** Giant Swarm's repository configuration (`repositories/team-*.yaml`), which has `requiredChecks`, `agentMerge`, flavours and `lifecycle`.

"Another team's repo" means the owning team differs from the item's team. Only the checks marked as merge-relevant can block a merge. That prevents a failing
board automation from looking like a broken main.

### 8.6 Personal setup

Same model, different providers:
- **Release:** tags on the person's own repos.
- **Rollout:** Flux on their own cluster.
- **Proof:** an agent proof using the tools of the systems a change affects, plus an alert window from that cluster's Alertmanager.

## 9. Code vs agents

| Deterministic code | Agents (judgement or language) |
|---|---|
| board sync, eligibility, ordering, WIP, gates | **implement:** issue to commits, using executors |
| placement, budgets and pacing, provider choice | **review and fix:** one per Change, fixes its own findings |
| git store, checkpoints, publishing, PR text assembly | **fix CI:** diagnose a red PR or main and fix it |
| CI observation, merge decision (rule or JEV), merging, squash message | **resolve conflicts:** when an update is forced |
| culprit finding, revert creation, merge pause per repo | **investigate:** alert to diagnosis to a fix Item |
| release, rollout observation, rollout PRs (version bumps) | **prove:** run the acceptance proof in a leased lab where a script isn't enough |
| leases, deadlines, defaults, escalation | **compose:** turn a structured Ask into a clear message; turn a free-text answer into a typed one (JEV first) |
| problem reports: fingerprinting, deduplication, description updates, recurrence counts | **classify:** bug severity, duplicates, new information (JEV) |
| metrics, the event log, invariants for the engine's own health | **respond:** investigate engine alerts and fix them, independently of the engine |

**Agents get context, not credentials.**
- A run starts with a bundle: issue, spec, epic context, related PRs, review threads, the repo's AGENTS.md. Agents don't spend tokens or GitHub calls on discovery.
- Agents have **no GitHub write access**: they push only to the git store. Extra reads go through an engine MCP tool (via muster) that is cached and counts against the shared budget.
- On laptops, the runner starts the CLI with its own user directory and environment, so the person's `gh` login and dotfiles aren't visible to it.

## 10. Coordination

- **Inside an installation, one authority.** The engine is the only scheduler. One claim per issue is a consequence of having a single writer, not a protocol between peers.
- **Shared limits are owned in one place:**
  - **GitHub API:** a token bucket per App, with priority classes: merges and repairs first, then publishing, then observation, then metrics. It also tracks
    what other users of the same App consume, from the rate-limit headers.
  - **CI capacity:** in-flight pipelines are capped per repo and organisation.
  - **Labs and test installations:** leases with a queue, holder heartbeats and a maximum hold time. This worked last time.
- **Runners are dumb on purpose.** They don't share state with each other. A runner that disappears loses nothing: its lease expires and the work continues
  elsewhere from the git store. Because the engine publishes, a remote machine can always update an existing PR.
- **Across installations: no coordination, by design.** That is what keeps personal and company data apart.

## 11. Waiting and waking

Every wait is a structured record on the Item or Change:

```
wait: { on: ci | review | person | team | release | rollout | window | lease | budget | dependency,
        ref: <the thing observed>, since, deadline, at_deadline: re-observe | default | escalate }
```

- **Wake sources:**
  - GitHub App webhooks, including `projects_v2_item` for board changes, check runs and statuses for CI, and pull request reviews
  - CircleCI's outbound webhooks (workflow and job completed), where the installation has a CircleCI credential (optional, §7.3). Otherwise GitHub's status and check webhooks wake the devctl verdict.
  - Alertmanager webhooks
  - Flux and installation watches
  - runner streams (runs on laptops and build machines)
  - platform session events (runs in the cluster), polled per active session until the platform emits them
  - timers (requeue at the deadline)

  A **wake index** (`ref` → waiters) means one event reconciles exactly the items waiting on it.
- **Deadlines come from metrics.** A CI wait's deadline is a multiple of that pipeline's observed duration. At the deadline the engine re-observes once. If the
  situation is unchanged, it acts as configured: for example, a stuck pipeline gets escalated, never re-triggered blindly.
- **Safety net:** a slow, periodic resync per source.
  - On GitHub it uses conditional requests (ETag). An unchanged answer (304) doesn't count against the rate limit.
  - Board resyncs read only opted-in teams and the active statuses, never the 5,600-item board.
- **Blindness is detected and reported.**
  - Each source has a freshness check: GitHub's webhook delivery log, and a cheap conditional probe.
  - A source that falls behind is marked *degraded*. Its waiters switch to polling and show "unobserved", not "stalled". No work is declared stuck because nothing was observed.

## 12. Failures and incidents

| Failure | Detected by | Handling |
|---|---|---|
| Runner or agent process dies | the run's lease expires | the run becomes `Lost`, then is retried elsewhere from the last checkpoint |
| Cluster session's turn fails, session stays `READY` | last turn's state (an event, or a poll until events exist) | a failed run; failing within seconds of placement is a placement failure (§5.4) |
| Agent is alive but stuck | no progress (tool calls, checkpoints, tokens) within the expected time for that purpose | stop it, retry with a different provider, ESCALATE when attempts run out |
| Wait never wakes | deadline timer | re-observe, then act as configured |
| Same refusal again | error class `refusal` | no retry until the inputs change; the item shows the reason |
| Red PR CI | check runs | fix run. Review isn't blocked, because it already happened before publishing. |
| Red main | main check runs | culprit, revert, merge pause for that repo only (§7.4) |
| Tag exists but the release isn't published | artifacts can't be pulled; `devctl release failed` at Giant Swarm | an Ask to the owner; the item waits visibly (§8.2) |
| Rollout stuck on a target | Flux status not Ready at the target version by the deadline | re-observe once, then an Ask to the owner, or an incident if alerts fire |
| Unrelated job fails (e.g. a board automation) | the job isn't in the repo's merge-relevant check set | ignored for merge purposes and shown as FYI |
| Observer blind | freshness check | source marked degraded, polling fallback, nothing declared stalled |
| Disk or resource burst | executor pod limits; runner checks free disk before accepting | the executor pod is evicted and its Execution retried; the runner pauses intake |
| Merge process dies holding a lock | there is no merge lock; merges are idempotent API calls on a SHA | nothing to clean up |
| Engine upgrade | leader election | handover in seconds; state lives in CRDs, not in memory |
| Human merges or edits by hand | observed like any other change | reconciled; main is still watched, so a bad hand merge gets reverted |
| Engine bug | a violated invariant, reconcile errors, decision latency, queue age (§12.2) | an alert; the independent responder investigates and fixes at once; a page if unfixed by the deadline |

### 12.1 Alerts and incidents

- **Intake.**
  - Every installation connected to the platform sends alerts to the engine's Alertmanager receiver.
  - Alerts are grouped by fingerprint into one **Incident** item per problem.
  - An incident is acted on only if the alert's team label belongs to a team that has **opted in to alerts**. Others are counted, not handled.
- **Steps 1 and 2 now:**
  1. An investigation run starts immediately. It is exempt from WIP and gets budget priority, including cluster sessions on API keys.
  2. Its report goes to the team's channel and the on-call person, with a severity and a recommended action. Triage and severity is a decision point.
  3. If a code fix is identified, it becomes an Item at top priority and goes through the normal flow.
  4. Escalation is an Ask to on-call with a deadline. If the incident isn't resolved by then, or the severity crosses a threshold, it pages the human.
- **Step 3 later: live actions.**
  - Teams define an allowlist per alert type, such as restart, roll back a release, or scale.
  - Each action starts as an **approval card**. klaus-gateway's existing Approve and Deny buttons calling a muster tool fit exactly.
  - An action becomes automatic only after its approval record shows it is safe.

### 12.2 The engine's own health

The failure to prevent: someone starts an epic in the evening, and by morning nothing has moved because the engine tripped over itself. Unnoticed malfunctions
and unprioritised bug issues are not acceptable.

- **Invariants, not guesses.** The engine continuously evaluates what must always be true and exports each invariant as a metric:
  - Every WIP item has either a run with a fresh heartbeat or a wait with a future deadline.
  - No wait is past its deadline plus a grace period.
  - No item has stayed in one state much longer than that stage's observed p90.
  - No refusal is repeated on unchanged inputs.
  - Every source is either fresh or marked degraded.
  - No gate is holding a repair.
  - A pending run gets placed within its expected time.
  - Every merge reaches its merge-result check.
  - The leader decides at least once in each expected interval.
- **Alerts:**
  - Prometheus rules on these invariants, plus the usual ones: reconcile errors, no leader, missing metrics.
  - A watchdog alert goes to a dead-man's switch outside the cluster, so "the engine and its monitoring are both down" is noticed too.
- **An independent responder.** A broken engine can't be trusted to schedule its own repair. Engine alerts therefore go from Alertmanager straight to an
  investigation agent started through agent-manager, not through the engine's scheduler.
  - **Access:** it can read the engine's custom resources, logs, metrics, event log and repo.
  - **Reporting:** it reports to the engine's channel at once.
  - **Safe actions:** it may take allowlisted safe actions, such as restarting engine pods, redelivering a webhook, or releasing a lease whose holder is gone.
    These follow the same approval-card path as on-call step 3.
  - **Fixes:** it opens a fix PR on the engine repo.
- **Merging engine fixes.**
  - When the engine's merge path works, its own fixes merge like any change, at top priority.
  - When it doesn't, the engine repo is the one place where a person merges by hand. The main watch still applies.
  - If the responder can't fix the problem by its deadline, a person is paged.
- **Releases of the engine and its platform.** They promote automatically like every owned line (§8.2). This is a stated choice: a bad release of the engine,
  muster, agent-manager, klaus-gateway or kagent can take down the engine, its Asks and the responder together, and no person stands in front of it. What
  catches it is the dead-man's switch outside the cluster and, while the platform still runs, the independent responder.
  - **One at a time:** the engine promotes at most one platform component (engine, muster, agent-manager, klaus-gateway, kagent, substrate) at a time. The next
    waits until the engine's invariants have stayed green for a window after the previous rollout.
  - **Never while degraded:** it never promotes any of them while an engine invariant is violated.
- **Deduplication.** One incident per alert fingerprint, so a flapping invariant doesn't create a stream of issues.

### 12.3 Problems agents find along the way

Agents working on one task often find unrelated bugs. They should report them once, and then carry on with their task.

- **One way to report.**
  - Agents get an MCP tool, `report_problem`, through muster in the cluster and through the runner's local MCP endpoint on laptops. It takes: title,
    observation, evidence, suspected repo or component, the repo to file it in, and signature hints (error text, test name, top stack frames).
  - The agent decides where the report is filed, as part of the call. The tool description carries the rule: anything with customer or infrastructure
    information goes to a private repo; internal issues go to Giant Swarm's internal issue repo or the private repo concerned; only overarching external issues
    go to `giantswarm/roadmap` or a public repo.
  - **Guard for public repos.** Writing to a public issue publishes it, and that can't be undone. So it passes a hard guard, a safety guardrail that fails
    closed. The guard applies to every write to a public issue, including the description updates of deduplication, not only to new issues.
    1. The engine reads the target repo's visibility. Private and internal repos skip the guard.
    2. **Screen (deterministic).** Everything that would be written (title, description, evidence) is screened for:
       - secrets, using gitleaks' rules, which Giant Swarm already runs
       - IP addresses, internal hostnames and domains
       - cloud account and subscription IDs, and email addresses
       - a configured list of sensitive names: customers, installations and management clusters, generated from the installation inventory the platform is connected to
    3. **Judgement.** Decision point `sensitive?` (JEV, 0–1): does the text identify a customer, describe infrastructure, or contain information that is internal only?
    4. **Outcome.** The issue is filed publicly only if the screen finds nothing *and* JEV says "not sensitive" above its confidence floor. Otherwise it goes to
       the team's private fallback repo, and the agent is told where.
       - Nothing is redacted and posted.
       - A person can still make it public later.
  - Agents have no GitHub write access in the first place (§9).
  - Where a CLI could still reach GitHub, a hook catches `gh issue create`/`comment` and the GitHub MCP write tools and points to `report_problem`. It warns first and blocks later (principle 14).
- **What the engine does with a report:**
  1. **Exact match:** it computes a deterministic fingerprint (normalised error signature, test, package, stack frames) and compares it with the fingerprints stored in open issues.
  2. **Probable duplicate:** if there's no exact match, it narrows candidates by repo or component plus text search. Then decision point `duplicate-of?` (JEV) chooses one of them or "new".
  3. **For a duplicate:** decision point `adds-information?` decides whether the report says something new.
     - If it does, the new facts are merged into the issue *description*, in a maintained "Reports" section. Agents never post comments.
     - If it doesn't, the issue only records the recurrence.
  4. **For a new problem:** an issue is created in the repo the agent named (Kind `Bug 🐞`, team taken from repo ownership, Status `Inbox 📥`) and classified by `bug-severity?` (§6.1).
  5. **Reply to the agent:** "recorded as <link>, continue your task". Reporting never makes an agent switch tasks.
- **Recurrences.** GitHub allows one reaction of each type per account, so the engine's App can add 👍 only once. The engine adds 👍 on the first
  recurrence. The real count, with the last-seen date, is a line in the description that the engine maintains.
- **Soft limits.** After several reports in one run, the tool still records them but tells the agent to focus on its task. The excess is recorded as a metric, never refused.

## 13. People: decisions, questions, todos

- **Kinds of Ask:**
  - **decision:** choose among options; has a default and a deadline
  - **question:** free text; the answer is turned into a typed answer by JEV, with an LLM as fallback
  - **todo:** something only a person can do, such as pressing a passkey for org settings, entering a credential in the vault, or opening an upstream PR from a
    prepared fork branch. Completion is observed where possible, not self-reported.
  - **review request:** exactly one, to another team, on GitHub
- **Rules:**
  - Asked once.
  - A reversible decision has a default and applies it at the deadline.
  - An irreversible one never defaults: it waits, visibly.
  - Non-urgent Asks are batched into a digest per person. Urgent ones are sent immediately.
  - Escalation goes from the owner to the team, and for incidents to on-call.
- **Channel.**
  - **Giant Swarm:** klaus-gateway in Slack. It needs one generalisation: an endpoint like `/reviews` that addresses a *person* or a team, offers *arbitrary
    choices*, and shows the default and deadline. Answer buttons call an engine MCP tool through muster *as the clicking person*, so answers come back
    authenticated without a new callback mechanism.
  - **Personal and other installations:** klaus-gateway as well, with a Matrix adapter. The engine uses klaus-gateway the same way in every setup and has a single notifier implementation.
- **Keeping interaction meaningful is measured:**
  - Asks per person per week, and time to answer
  - Share of defaults applied
  - Share of answers that changed the outcome

  An Ask type whose default is almost always accepted gets flagged to the team as a candidate for automation. The engine never removes it on its own.

## 14. Decision points and JEV

A decision point is a named, typed question asked inside the state machine, and it is the only place judgement enters deterministic code.

| Decision point | Type | Rule (always available) |
|---|---|---|
| `ready?` | choice: ready / not ready (+ reason) | has acceptance criteria and a target repo |
| `next?` | choice among eligible items | the order in §6.3 |
| `parallel-safe?` | yes or no for a pair of Changes | sequential per repo unless the footprints are disjoint |
| `update-before-merge?` | yes or no | overlap of footprints with main since base |
| `flaky-or-broken?` | choice | passes on one rerun of the same SHA |
| `culprit?` | ranking of the merges since green | the merge whose own pipeline failed first; bisect only when ambiguous |
| `incident-severity?` | choice | the alert's severity label |
| `bug-severity?` | choice: critical / high / medium / low | none; `medium` until a person sets it |
| `duplicate-of?` | choice among candidate open issues, or "new" | an exact fingerprint match, otherwise "new" |
| `adds-information?` | yes or no | the report has evidence fields the issue doesn't |
| `proof-plan?` | a mapping from acceptance criteria to proofs | the repo's default proofs |
| `answer-parse` | typed answer from free text | none (LLM fallback) |

- **Modes per decision point:**
  - `rule`
  - `shadow`: JEV is asked and recorded, but the rule decides.
  - `jev`: JEV decides, but only among the options the rule considers *safe*, and above a confidence floor. Otherwise the rule decides.
- **Determinism.** JEV's answers are recorded as inputs, so replay is still exact.
- **Promotion.** Each point starts in `shadow`. The metrics compare outcomes, and promotion is a configuration change reviewed like code.
- **Data.** JEV is approved for code and issues. The engine sends what the question needs and nothing more.

## 15. Metrics

Metrics are Prometheus metrics, labelled by team, repo, stage and provider, with the dashboards in Grafana. Questions about one item or one test (the tokens of
this issue, how often this test ran for it, why it waited) are answered from the event table in Postgres, because a label per issue or per test would not fit
Prometheus.

- **Flow:**
  - lead time per stage (cumulative flow)
  - WIP and its age
  - throughput
  - time spent in each wait reason
  - rework loops: CI reds, review changes requested, proof failures, reverts
- **Cost:**
  - tokens per run, item and provider, against each provider's pace
  - executions per item, cache hit rate, local vs remote
  - CI pipelines per Change, PR vs main
  - GitHub calls per item and per hour
  - laptop CPU used by runners
- **Tests:**
  - per-test duration and executions per item
  - slowest tests and flakiest tests per repo
  - how often executor results and CircleCI results disagree
- **Quality:**
  - red mains (and caused by which decision)
  - reverts, failed proofs
  - incidents linked to changes
- **People:**
  - Asks per person, time to answer
  - defaults applied, answers that changed the outcome
- **Decision points:**
  - rule vs JEV agreement
  - outcome when they disagreed
- **Engine health:**
  - reconcile errors, decision latency
  - source freshness
  - queue ages
  - leader changes

## 16. Cost of running (expectations to check against metrics)

- **Engine:** two small pods. Idle CPU is in the tens of millicores, because controllers do nothing while nothing changes. Memory is in the low hundreds of MB. Postgres is one small instance.
- **Laptop runner:** one process. Near-zero CPU when idle; it starts nothing until a run arrives. During a run the load is the agent CLI itself; builds and tests go elsewhere.
- **Executors:** substrate sandboxes scale with work and can be suspended when idle. Per-repo caches keep reruns cheap: in the spike, warm reruns took about 1
  s. Build machines are used as available for Docker, kind and the agentlab.
- **GitHub:** event-driven. Per Change, roughly tens of REST calls. When idle, close to zero, because resyncs are conditional. Board reads go through the webhook and filtered resyncs.
- **CircleCI:** in the normal case, one PR pipeline per Change, plus the tag pipeline that is the release itself.
- **Tokens:** in the normal case, two sessions per Change (implement; review and fix), started with precomputed context. JEV calls cost fractions of a cent.
- **Code:** expected to be an order of magnitude smaller than last time, because the platform supplies sessions, chat, tools, deployment, storage and identity.
  Treat this as an expectation, not a rule.

## 17. Separation and security

- **Installations are separate.** Each has its own cluster or namespace, GitHub App, store, notifier, and runner registrations. A laptop that serves both runs
  two runners, each with its own OS user (or rootless container), user directory, workspaces and logins. Neither can read the other's files.
- **Agents never hold secrets.**
  - In the cluster, the egress gateway injects credentials.
  - On laptops, agents get no GitHub token at all.
  - A credential a person must enter becomes a todo.
- **Org-level actions** (repo settings, App permissions) are todos for a passkey holder, completed through the company's own reconciliation. The engine observes the result.
- **Upstream projects:** agents prepare fork branches and drafts. Opening them is a todo for the owner.
- **Ownership:**
  - **Pull requests:** colleagues' PRs are left alone. The engine never reviews, comments on, rebases or merges them.
  - **Releases of an owned line are the team's release.** Anything merged to main of a repo whose line the team owns ships with the next proven train, whoever
    merged it (§8.2). To keep a change back, don't merge it yet, or set the one-click hold on the repo. An incident after a promotion goes to the owning team,
    which finds the cause in the train.
    - This replaces the previous system's rule "colleagues' releases are left alone" for owned lines. Lines the team doesn't own are still never promoted, bumped or rolled back.
    - When a team opts in, its members are told once: "merged to main means shipped with the next proven train".
  - **Another team's repo** gets one review request and needs their approval.
- **Subscriptions** stay on their person's machine. Lending is the person's own opt-in, and the engine never moves credentials. Check each provider's terms for automated use before enabling lending.
- **The public webhook endpoint** is the only thing the engine exposes to the internet, and only where an installation needs it (for example a personal cluster
  that has no other way to receive GitHub's webhooks). It is protected in layers:
  - **Its own surface:** a separate receiver deployment with no other API and its own network policy. It can only write events for the engine to read.
  - **Gateway route:** exactly one path, with TLS and request-size and rate limits at the gateway. Source addresses are restricted to GitHub's published hook
    ranges (from `api.github.com/meta`, refreshed automatically).
  - **Every delivery checked:** the `X-Hub-Signature-256` HMAC is verified against the App's webhook secret, which lives in the secret store. Delivery IDs are deduplicated against replays.
  - **Nothing trusted from the payload:** a delivery only wakes the engine, which then reads the state it needs from GitHub itself.

## 18. Where the wishes conflict, and what is proposed

1. **Throughput and fully used budgets vs a low WIP.** A low WIP makes lead times short but leaves budget unused at first. *Proposal:* WIP wins. Unused budget
   is accepted while the stages prove themselves, and the limits go up as the metrics allow. The engine never invents work to fill budget, because that would
   take prioritisation away from people.
2. **"Any pre-merge check must reproduce company CI faithfully" vs fewer CI runs.** *Proposal:* CircleCI stays the only merge gate. Executors give fast feedback
   and are never a gate, so they don't have to be faithful, only good predictors. They run the same targets, and the disagreement rate is a metric.
3. **Not updating branches vs a green main.** *Proposal:* accept the occasional red main, with automatic culprit finding and revert, over re-running every PR
   after every merge. Where branch protection requires up-to-date branches, the engine complies and the metrics show the cost, so the team can decide.
4. **Review before the PR vs work visible on GitHub early.** Colleagues see the PR only once it has been reviewed. *Proposal:* accept this. The board status
   shows progress, and it roughly halves the CI runs.
5. **"Nothing reversible waits on a person" vs another team's approval, customer timelines, shared labs.** These aren't reversible decisions; they're ownership
   and capacity. *Proposal:* they are visible waits with deadlines, and reminders go to the owner, never to the colleague. They occupy WIP, which shows their
   real cost.
6. **Deterministic engine vs JEV.** *Proposal:*
   - JEV's answers are recorded inputs, so replay is still exact.
   - It only chooses among options the rules call safe.
   - Each point starts in shadow mode.
7. **Replacing on-call vs PR-only authority.** A PR-only fix takes a full merge, release and rollout. *Proposal:* step 3 with an allowlist and approval cards,
   promoted to automatic action by its own record.
8. **One shared service vs "the laptop is mine".** *Proposal:*
   - A runner is installed and capped by its person.
   - Lending is opt-in.
   - The workstation guard refuses work while the person needs the machine.
9. **A small codebase vs a wide scope** (GitLab, three providers, three chat systems, personal setups, customers, incidents, delivery). *Proposal:* interfaces
   from day one, implementations only when a phase needs them (§20).
10. **The engine working on itself.** Last time it merged as much as it broke. *Proposal:*
    - The engine's repo is an ordinary repo: it gets work only through "Up Next" from a person, and never files issues about itself.
    - Its malfunctions are invariant alerts, fixed at once by the independent responder (§12.2).
    - Its own releases promote automatically, like every owned line. A bad engine release stops everyone's work; the dead-man's switch and the independent responder are what catch it (§12.2).
11. **WIP until production vs customers' own timelines.** *Decided:*
    - DONE means stable and rolled out on the installations Giant Swarm controls, with proofs passed there.
    - Customer installations are a tail outside WIP. Rollout is still observed there, and any alert after it is linked back to the Change.
12. **Short lead time vs promotion reaching every customer within 10 minutes.** *Decided:* promotion is automatic once the train is proven (§8.2).
    - The risk that remains: a defect that the proofs and the testing-stage alert window miss reaches customers within minutes.
    - What limits it:
      - incidents linked to the train
      - fix or revert at top priority
      - the repo's promotion pausing until the incident is closed
      - a one-click hold for people
      - a customer ceiling (the proposal in §8.2) as a gate, if it lands
13. **Strict process vs a system that can't block itself.** Agents should follow the process (report tool, no task switching, offloaded builds), but strict
    enforcement with bugs in it stopped everything last time. *Proposal:* principle 14. Process guardrails guide and warn first and fail open; enforcement is
    earned per guardrail through its metrics. Only safety guardrails are hard from the start.

## 19. From learnings to design

| Learning | Design answer |
|---|---|
| Central loop at 73–80% of a core, watchers at 30–40% | controllers idle on watches; no loop when nothing changes (§3, §16) |
| 6,500–12,900 processes per minute | the runner and engine spawn nothing while idle; agents' builds go to executors (§7.1) |
| Workstation load of 19 with one agent | builds and tests leave the laptop; workstation guard (§6.4, §7.1) |
| 200k REST calls a week; GraphQL budget exhausted | webhooks, conditional resyncs, filtered board reads, one token bucket (§10, §11) |
| 98% of scheduled wake-ups failed | no time-based retries for waits; a wake index on events; deadlines only re-observe (§11) |
| Dead workers stayed "working" for hours | run leases with heartbeats (§5.4, §12) |
| Waits didn't wake for 8–61 hours | level-triggered reconcile and an explicit wait record (§2, §11) |
| Hand-offs lost; remote machines couldn't publish to an existing PR | the engine publishes; WIP lives in the git store; any runner continues (§7.2, §10) |
| No review on red CI, and the worker waited for it | review happens before publishing, so CI and review never wait on each other (§7.2) |
| A green, approved PR refused because a bookkeeping record was missing | the merge gate reads only observed forge state on the SHA, with no side records (§7.3) |
| The same refusal retried about 20 times per PR | refusals aren't retried until their inputs change (§2.5) |
| A failed board automation read as a broken main | an explicit merge-relevant check set; pauses apply per repo (§7.4, §12) |
| Blind watchers; "no progress" raised when nothing was observed | freshness checks; "unobserved" is not "stalled" (§11) |
| Self-update stalls; versions rolled back by their own health checks | leader handover; health judged by the engine's metrics (§3, §12.2) |
| A 12 GB disk burst killed a merge that kept holding the queue | executor resource limits; no merge locks (§12) |
| Gates blocked their own repairs | gates only gate starting work; repairs are exempt (§2.7, §6.2) |
| 325 PRs merged on itself against 320 issues opened on it | no self-filed issues; malfunctions are deduplicated invariant alerts, handled by a responder outside the engine (§2.10, §12.2) |
| An evening's epic didn't move overnight because the system tripped over itself | invariants, alerts, an independent responder, a page to a person at the deadline (§12.2) |
| Strict enforcement with bugs blocked everything | guide first, enforce later; process guardrails fail open (§2.14) |
| A hand merge bypassed the check and turned main red | main is watched no matter who merged (§7.4) |
| A top-priority fix took 12 hours; 520 of 1,260 starts were by hand | the above, plus the order in §6.3; hand-starts are a metric |
| Worked: one item, one agent, one PR, one review | kept as the core flow |
| Worked: budget pacing, decisions with defaults, leases, scripted build machines, config reviewed as code | all kept (§6.4, §13, §10, §3) |

## 20. Phases

### 20.1 How every phase is built

**Scenarios first.**
- **What a scenario is.** Behaviour is written down as fast end-to-end scenarios *before* it's implemented, as Giant Swarm does in muster
  (`internal/testing/scenarios`, about 300 YAML scenarios) and with `giantswarm/k8senv`.
- **The order inside a phase:**
  1. A phase starts by turning this design into scenarios.
  2. A person reviews them.
  3. Only then do agents implement, until the scenarios pass.

  This keeps agents implementing what was specified, not what they infer. A behaviour without a scenario isn't done.
- **Format.** muster's runner is internal to muster, so we copy its YAML shape rather than import it. Each scenario has:
  - `name`, `description`, `concept`, `tags`
  - `pre_configuration`: the starting state of every fake (board items, repos, rulesets, runners, sessions)
  - `steps`: each step injects something (a webhook, a session's turn state, a person's answer or silence, a clock advance, a fault) and states its `expected`
    outcome. Outcomes are object state with `wait_for_state`, calls recorded by the fakes, metrics, or rows in the event table.
- **Conventions:**
  - The description carries Given/When/Then.
  - One behaviour per scenario.
  - Names are kebab-case and prefixed by concept: `wait-…`, `run-…`, `merge-…`, `invariant-…`, `ask-…`.
  - Regressions are tagged with their issue.
- **Black box, real core, fake edges.**
  - **Real:** the real engine runs against a real kube-apiserver from k8senv (pooled instances, CRDs cached, a namespace per scenario) and real Postgres (one
    per run, a template database per scenario).
  - **Fakes:** every edge behind the interfaces of principle 12 gets a fake that records calls and can be driven into faults:
    - GitHub: REST plus HMAC-signed webhooks, with 5xx, rate limits and lost deliveries
    - the platform's sessions: scripted turns, including "session `READY`, turn `FAILED`"
    - runners: heartbeats, slots, pressure, death
    - executors
    - klaus-gateway: records Asks; the scenario answers or stays silent
    - JEV: typed answers, failures, low confidence
    - Alertmanager and Flux status
  - **devctl's verdict** runs for real, on the fake GitHub signals.
  - **Check first:** in phase 1, verify that controller-runtime's watches work on k8senv's kine-backed API server. If they don't, use envtest, as muster's Kubernetes mode does.
- **Time is injected.** There is one `Clock` everywhere, and scenarios advance it, so deadlines, defaults, escalations and invariant windows fire without sleeping.
  - This shapes the code: deadlines are scheduled on the engine's own clock-driven timer queue, which wakes the controllers. controller-runtime's `RequeueAfter` isn't clock-controlled.
- **Fast.** No network, no sleeps, scenarios run in parallel. The whole suite is quick enough to run on every change, on executors during implementation and in CI.
- **Learnings become scenarios.** Every failure of the previous system and every row of §19 is a scenario that must pass before the engine acts in phase 2. Examples:
  - a dead worker
  - a wait that never wakes
  - the same refusal repeated
  - red CI with no review
  - a false alarm that stops everything
  - a blind watcher
  - a gate blocking its own repair
  - a hand merge past the checks
  - an overnight epic that never moved

  The traceability table becomes executable.
- **Unit tests are for the pure parts.** Decision-point rules and `decide` get table-driven unit tests. Scenarios cover behaviour across the system.

**Sessions use subagents.**
- Each phase runs in its own session, because the epic is too big for one context.
- Sessions use subagents to keep their context managed: for research, for reading large files and logs, for writing batches of scenarios per concept, and for independent implementation pieces.
- The session keeps the plan, the decisions and the integration. Subagents return conclusions, not file dumps.
- Every phase brief says so.

### 20.2 The phases

1. **Foundation and a short dry run.** One opted-in team, board and GitHub only. The engine observes and records what it *would* do, with no actions, for a few days.
   - There is no human work to shadow (the previous system did the work, and nobody implements by hand), so the dry run doesn't compare the engine with people.
     It proves the App, the webhook route, webhook coverage, API cost, freshness and the invariants on the organisation's real traffic, and shows each person
     the queue the engine would work through for them.
   - Engine alerts are live from day one.
   - Phase 2 then switches acting on.
2. **Implement to merge.**
   - **Company:** one team, Claude only, laptop runners, cluster executors, Slack Asks, epic WIP of 1, and the independent responder for engine alerts. Also
     `report_problem` with deduplication, in warn mode.
   - **Personal, in parallel (decided):** a personal installation on its own cluster, first in shadow and then acting. It needs:
     - Matrix, reached through klaus-gateway's Matrix adapter
     - the protected public webhook endpoint (§17)
     - slow, conditional board polling for boards owned by a user account, which get no `projects_v2_item` webhooks
3. **Delivery.** Release, rollout, proof, and leases for labs and test installations.
4. **Incidents.** Steps 1 and 2 for one team that has opted in to alerts.
5. **Breadth.**
   - Codex and Gemini, and lending.
   - JEV points promoted from shadow where the metrics justify it.
   - More teams, and higher WIP limits.
6. **Elsewhere.**
   - Proofs on personal live systems.
   - Platform customers.
   - GitLab.
   - On-call step 3.

## 21. Decisions and open points

Decided on 2026-11-14:

| Point | Decision |
|---|---|
| Promotion | automatic once the train is proven, with the safeguards in §8.2, owned lines only; no human step, the engine's repo included. The engine promotes until the proposed ladder takes over. |
| DONE | stable, rolled out and proven on the installations Giant Swarm controls. Customer installations are a tail outside WIP. |
| Bugs | need "Up Next" from a person. JEV classifies severity, and only `critical` goes to "Up Next" automatically. Agents report through `report_problem` with deduplication (§6.1, §12.3). |
| Engine health | invariants, alerts and an independent responder that investigates and fixes at once (§12.2) |
| Guardrails | guide first, enforce later; process guardrails fail open (§2.14) |
| Other Kinds | ignored for now |
| Personal installation | in parallel, from phase 2 |
| devctl | becomes the Giant Swarm adapter with the four changes in §8.4 |
| klaus-gateway | gets a generalised Ask endpoint; the engine builds no Slack integration of its own |

Decided in the design review on 2026-11-14:

| Point | Decision |
|---|---|
| Approval identity | at Giant Swarm, the `giantswarm-devctl` App's `pull_request`-mode bypass, narrowed to the review ruleset, verified in phase 1. The engine enforces other teams' approval (§7.3) |
| CI observer | per installation: at Giant Swarm devctl's verdict (token-free mode, a library, CircleCI read optional), elsewhere GitHub's signals (§7.3). Supersedes "read access to CircleCI" |
| Agentlab | proven before publish, during REVIEWING (§7.2, §8.3) |
| Reservations | none in the first version (§8.2) |
| Release lines | the engine promotes owned lines only; a colleague's commit in an owned line rides along (§8.2) |
| Picking work | per person: the next "Up Next" item that is theirs or unassigned, then the engine assigns them (§6.1) |
| Lending | stays as designed: team issues are spread over team members' machines (§6.4) |
| Review | exactly one. Later commits need no second review; CI is enough (§5.3) |
| People on the PR | a "changes requested" review blocks and starts a fix run; comments go to the owner's digest (§7.2) |
| Report destination | the agent names the repo in the `report_problem` call (§12.3) |
| State | live state in Kubernetes objects, metrics in Prometheus, per-item history in a Postgres event table (§3, §15) |
| Executors | kept as designed, because a small machine is under stress after one or two Go builds (§7.1) |

Decided after the spike on 2026-11-14:

| Point | Decision |
|---|---|
| Cluster sessions | the platform gets session events and a cancel API; until then the engine polls each active cluster session's last turn. Runners report as before (§5.4). |
| Handoff | a fresh session with a summary plus the git-store branch, never a fork (§5.4) |
| Executors | substrate sandboxes with the same toolchains and workspaces as agents. Build machines handle Docker, kind and the agentlab. Plain pods are only a stand-in (§3, §7.1). |
| "Fast stays local" | dropped; every heavy command goes remote (§7.1) |
| Personal webhooks | a public endpoint, protected in layers (§17) |
| Personal notifier | Matrix, through a Matrix adapter in klaus-gateway (§13) |
| Network | every component ships its network policies (§3) |
| Culprit finding | the merge whose own tag pipeline failed first; bisect only when ambiguous (§7.4) |

Decided on the review notes:

| Point | Decision |
|---|---|
| Commands that write | they run remotely too, in a substrate sandbox; their changes come back as a patch (§7.1) |
| Commits after the review | no second review once CI is green, and no extra check |
| Capacity | slots per machine, in its runner's configuration; pressure (load, memory, disk, GitHub budget, battery, busy) can fill it before its slots do. The engine tracks what remains (§6.4) |
| Reservations | the engine respects people's per-app reservations (§8.2) |
| Platform promotions | one component at a time, and never while an engine invariant is violated (§12.2) |
| Public repos | a hard guard: visibility check, deterministic screen, then JEV `sensitive?`. It fails closed to the team's private repo (§12.3). |
| Colleagues' commits in an owned line | releasing an owned line is the team's release and ships every commit merged to it, whoever merged it; colleagues' PRs stay untouched (§17) |

Decided on 2026-11-14, before phase 1 was built:

| Point | Decision |
|---|---|
| WIP limits | per person (items and epics), counting only the engine's own work; an optional team cap, unset by default (§6.2) |
| Phase 1 | the foundation plus a short dry run, then acting; nothing is compared with human work (§20.2) |
| Repository | the engine replaces the old system in the `beekeeper` repository, as a new major version; the design moves there |

No design points are open.

Needs a person outside the engine:
- an org owner generates a private key for `giantswarm-devctl` for the platform's secret store
- a `devctl repo reconcile` change that narrows the App's bypass to the review ruleset
- the devctl changes in §8.4
- devctl: the verdict as a library, a one-shot `pr wait --once --sha`, and the token-free mode as the default (§8.4)
- optionally, a read-only CircleCI credential, for repos whose jobs devctl can't predict and for CI timings in the metrics
- the klaus-gateway Ask endpoint in §13, and its Matrix adapter
- **kagent and agent-manager:** session events and a cancel API
- **substrate:** sandbox templates with toolchains for the engine's identity, egress to the git store and module proxies, and caches that outlive a sandbox
- **the personal installation's cluster:** a Matrix setup and the public webhook route
