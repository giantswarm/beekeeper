# Release containment and loop recovery

Epic: https://github.com/giantswarm/beekeeper/issues/2231

Status: implementation plan; the linked sub-issues deliver the behavior. This PR
does not implement a canary, enforce an update gate, or claim rollback is live.

## Outcome and design

Every candidate must prove the core worker → independent review → exact-head
merge workflow before ordinary desks install it. A regression that escapes that
gate must recover automatically within 30 minutes of installation. A running
loop with no useful throughput must be observable without a person noticing it.

Use two complementary environments: a deterministic, credential-free real-binary
integration test on every PR, and a signed-artifact staging run against explicitly
allowlisted GitHub sandbox resources. The fake models external protocols and Git
mutations, not Beekeeper's own parser, planner, validation or merge authorization.
Staging then checks assumptions the recorded adapter cannot prove, including GitHub
permissions, branch protection, required checks and merge queue semantics.

Successful staging creates authenticated evidence bound to the candidate binary;
release discovery alone grants no installation permission. Preserve signature
verification and the existing local compatibility checks. Keep a verified previous
binary and a crash-safe recovery journal. A recovery helper independent of the
candidate loop supervises a 30-minute probation and can restore the prior release.
An idle desk runs a bounded isolated probe rather than waiting for production work.

## Current code and gaps

- `cmd/supervisorloop.go`: `pass`, `parseLoopPull`, `startNext`/`startItem`,
  `startReviewer`, `install` and `failed` connect board reads, capacity, starts,
  review, update and retries. `failed` backs off identical textual errors per
  action; that is not a cross-PR health monitor.
- `internal/superloop/superloop.go` plans actions; `cmd/supervisorloop_test.go`'s
  `loopRig` replaces commands and PR reads, so its synthetic `superloop.Pull`
  cannot prove the external response-to-merge boundary.
- `cmd/supervisorloopmerge.go`: `mergePull` rereads the head and green checks,
  then invokes the gate; `reviewedMerge` enforces the reviewed merge route.
  `cmd/reviewedmergecheck.go` checks base/head, discovers configured checkouts,
  fetches refs and validates a merge worktree. The current no-checkout case
  explicitly falls back to the PR checks; tests must preserve that policy.
- `cmd/selfupdate.go`: `canaryChecks` executes `version`, `budget --gate`,
  `supervisor status`, and `capacity`. `skipRelease` persists failed candidates,
  but these commands prove no start or merge. `internal/update/update.go` verifies
  signatures and exposes `Options.Canary`; pinned `InstallVersion` is a separate
  replacement route used by `cmd/executionhostupdate.go`, so protecting only
  latest-release discovery would leave a bypass.
- `cmd/watch.go`: `checkLoop` detects/restarts a missing loop service.
  `cmd/supervisorwatch.go` publishes shared observations. Neither service liveness
  nor the existing retry backoff establishes successful throughput.
- `internal/update/update.go` describes pinned replacement as atomic with no
  backup. `internal/state` version fencing and `cmd/selfupdate.go` process handover
  make state compatibility and independent recovery explicit prerequisites.
- `.circleci/custom.yml` owns repository-specific jobs; `.circleci/workflows.yml`
  is generated. Existing envtest and stable-version jobs complement unit tests
  but do not drive this real-binary desk workflow.

Incident references: https://github.com/giantswarm/beekeeper/pull/2163 and
https://github.com/giantswarm/beekeeper/issues/2230. The new integration regression
must reproduce the boundary those unit tests missed; it must not merely assert
that a mocked merge command was called.

## Delivery slices

Each numbered slice below is one implementation PR with its own acceptance
criteria. All issues are linked as GitHub sub-issues of the epic. Dependencies
are implementation prerequisites, not permission to combine several slices.

### 1. Build an isolated real-binary supervisor workflow harness

https://github.com/giantswarm/beekeeper/issues/2254

Add a subprocess integration harness with an ephemeral configuration, state store, board, local bare
Git origin and stateful GitHub API/CLI adapter. Run the compiled candidate through supervisor loop
passes; replace only external GitHub and worker/reviewer executables, not the planner, pull parser,
merge validation or gate. Extend internal/testenv as appropriate; keep existing loopRig unit tests.

Dependencies: none.

- [ ] A board task starts a deterministic worker executable through the production start path and records a durable registration.
- [ ] The worker publishes a real local Git branch and parks; a deterministic independent reviewer publishes an exact-head verdict through the same external interface as production.
- [ ] The real parser, planner and merge/gate commands observe the review and merge the Git branch; assert the base commit and durable completion, not merely an invoked command.
- [ ] Unknown external calls fail; outbound network and production credentials are unavailable; concurrent runs and interrupted cleanup cannot touch another run or real desk state.

### 2. Require the loop workflow regression in CircleCI

https://github.com/giantswarm/beekeeper/issues/2255

Use the harness to reproduce the boundary missed by
https://github.com/giantswarm/beekeeper/pull/2163 and
https://github.com/giantswarm/beekeeper/issues/2230. Add the integration job to .circleci/custom.yml
and its build workflow; do not edit generated workflows.yml. Exercise raw GitHub pull/board
responses through readLoopPull/parseLoopPull into checkReviewedMerge and mergePull.

Dependencies: 1.

- [ ] Fixture responses include base/head/branch, review, checks and board data; no preconstructed superloop.Pull bypasses parsing.
- [ ] An injected historical missing-base or unavailable-validation regression fails the workflow before merge; the current supported no-checkout policy and valid checkout workflow pass.
- [ ] Cover a non-Go repository, a diverged Go merge result, moved reviewed head, red checks, absent
  queue where required, budget hold and failed worker start; refusals never produce false merge
  proof.
- [ ] CircleCI runs the real-binary job on every PR initially, including changes to loop, merge,
  capacity, watch, update and CI configuration; failure blocks release publication and merge via a
  required check.
- [ ] Failures retain sanitized command/event trace and Git refs; the job has a finite deadline,
  LANG=C.UTF-8, no live account dependency and cannot silently skip. Record the required-check
  configuration prerequisite.

### 3. Run signed release candidates through a sandbox canary

https://github.com/giantswarm/beekeeper/issues/2256

Add an explicit isolated staging mode and runner for a verified release artifact, reusing the
workflow scenario. A configurable allowlisted sandbox repository and board exercise real GitHub
review/check/merge operations; local recorded mode remains the credential-free PR check. The staged
candidate must never replace the coordinator executable or load a live desk configuration.

Dependencies: 1, 2.

- [ ] The actual signed candidate starts a deterministic worker, obtains independent exact-head review, merges a fresh sandbox PR and observes its merged state from GitHub.
- [ ] Emit a machine-readable result bound to version, commit, artifact digest, OS/architecture, scenario schema, sandbox run and observed merge commit; a command exit alone is insufficient.
- [ ] A failure, timeout, missing permission, unavailable sandbox, or missing merge observation emits no success; interrupted runs clean up only owned resources and are resumable/idempotent.
- [ ] Document least-privilege sandbox App permissions, required checks/queue, setup and teardown; fork PRs receive no credentials. Configure staging resources explicitly without personal defaults.

### 4. Gate coordinator and execution-host updates on canary evidence

https://github.com/giantswarm/beekeeper/issues/2257

Extend internal/update and cmd/selfupdate.go to require trusted release evidence before replacement,
in addition to existing signature verification and canaryChecks. Cover supervisorLoop.install and
the exact-version InstallVersion route used by cmd/executionhostupdate.go. Publish authenticated
canary evidence as a release artifact or trusted status bound to the verified binary digest.

Dependencies: 3.

- [ ] Absent, pending, failed, tampered, stale-schema or mismatched-version/digest/platform evidence leaves the installed executable unchanged; success for another candidate cannot be reused.
- [ ] Both latest-release and pinned execution-host installs enforce eligibility; local canaryChecks continue as an additional desk compatibility check.
- [ ] Release publishing waits for the CI regression check, then runs staging canary; desks can discover a candidate but cannot install it before authenticated success.
- [ ] Transient service/budget failures remain retryable; a failed candidate is quarantined with a durable reason. Define trust verification and evidence validity, and test replay/mismatch cases.
- [ ] Document bootstrap: deploy the gate to existing desks before claiming protected rollout; pre-gate clients are an explicit rollout limitation.

### 5. Detect stalled merge and start throughput from durable events

https://github.com/giantswarm/beekeeper/issues/2258

Add a shared health evaluator used by cmd/supervisorwatch.go and cmd/watch.go, consuming durable
loop/merge/start outcomes and eligible PR observations. cmd/supervisorloop.go failed currently backs
off per exact message; keep retry policy separate from health. cmd/watch.go checkLoop currently
checks service liveness, not useful progress.

Dependencies: 2.

- [ ] Persist normalized failure reason codes, operation, candidate version and time; three
  consecutive same-reason actual merge/start failures trip health even across PRs and process
  restarts. Successful corresponding operations reset their streak.
- [ ] An approved, exact-head, green, otherwise eligible PR waiting without a merge for a configurable duration alerts the guide; propose a two-hour default with a bounded evaluation cadence.
- [ ] Empty backlog, intentional stop/hold, budget exhaustion, red CI and pending review do not count as release regressions. Unreadable observations report unknown/degraded, never healthy.
- [ ] Deduplicate one alert per incident and emit recovery; include candidate, oldest eligible wait and affected public PR URLs without host-specific data.
- [ ] Fake-clock tests cover restarts, interleaved successes, changing incidental text, stale observations and escalation; health works even when the loop is alive but cannot merge.

### 6. Retain a verified last-known-good update and recovery journal

https://github.com/giantswarm/beekeeper/issues/2259

Extend internal/update installation to retain the previous verified binary and a durable install
journal before atomic replacement. InstallVersion currently stages and replaces without backup.
Define rollback-compatible state handling with internal/state version fencing and the process
restart rules in cmd/selfupdate.go; do not assume an older binary can read state written by the
candidate.

Dependencies: 4.

- [ ] Journal records previous/candidate version and digest, installation time, probation state and quarantine; crash recovery is idempotent at every replacement boundary.
- [ ] Keep a verified previous executable accessible without a network download; reject a modified backup and preserve the installed binary on validation error.
- [ ] Specify backward-compatible writes during probation or an explicit compatible state
  migration/recovery protocol; preserve work, approvals, notes and post-install events, never
  blindly restore an old whole-state snapshot.
- [ ] Test supported upgrade/downgrade state combinations, concurrent updater exclusion, symlinks, disk-full and crash interruption; garbage collection preserves the active recovery target.
- [ ] Last-known-good is promoted only after successful probation, with documented first-install behavior when no recovery target exists.

### 7. Roll back unhealthy updates automatically during probation

https://github.com/giantswarm/beekeeper/issues/2260

Add a recovery controller outside the candidate supervisor process, using the retained
executable/journal and shared health evaluator. Connect watch/standby launch and restart handover so
a broken loop or failed candidate watch cannot prevent recovery. Quarantine the failed version
across coordinator and execution-host update routes.

Dependencies: 5, 6.

- [ ] Within 30 minutes of installation, a candidate with a three-failure streak, failed start
  probe, or missing health heartbeat is restored to the verified compatible prior release; schedule
  at most one minute between recovery evaluations.
- [ ] Probation requires a bounded isolated workflow/start probe when the desk is idle; no
  production backlog is necessary to establish health. External GitHub outages are classified
  separately and keep the candidate unproven rather than falsely attributing a code defect.
- [ ] The recovery helper survives candidate death, restarts the relevant loop/watch/broker safely, preserves in-flight work and honors active operation locks with a bounded recovery policy.
- [ ] One concise notification identifies failed and restored versions and reason; one rollback per candidate prevents oscillation and ordinary self-update cannot reinstall quarantine.
- [ ] Missing/corrupt recovery target or incompatible state holds new updates/starts, reports
  recovery failure, and never destroys current state; fault-injection tests prove rollback of the
  supported broken-release case without a person.

### 8. Prove release containment and recovery end to end

https://github.com/giantswarm/beekeeper/issues/2261

Extend the workflow harness with good and deliberately broken candidate artifacts, durable health
state, updater and independently launched recovery controller. Add an operations guide and CI
recovery scenario in .circleci/custom.yml; bind evidence to the complete release path.

Dependencies: 2, 3, 4, 5, 6, 7.

- [ ] Inject the unavailable merge validation regression: PR integration fails and staging emits no eligibility evidence, so neither coordinator nor execution-host self-update replaces a good binary.
- [ ] Inject a defect after successful staging: the installed candidate fails the desk workflow, recovery restores the good release within 30 minutes and a subsequent worker/review/merge succeeds.
- [ ] Simulated time checks exact boundaries; one subprocess crash/restart exercise proves recovery independence and journal replay, rather than only evaluating a pure function.
- [ ] Cover idle desk probing, failure of recovery notification, unavailable GitHub, corrupt backup and incompatible state, with explicit expected held/degraded outcomes.
- [ ] Document rollout order, evidence inspection, quarantine recovery and rollback limitations.
  Publish sanitized proof artifacts showing candidate rejection, restored version, preserved work
  and successful subsequent merge.

## Sequencing and release proof

Land 1 → 2 first to stop repeating the parser/merge regression. Deliver staging
and eligibility (3 → 4) next; deploy gate-capable clients before making the
protected-release claim. Health (5) can proceed after 2 while retention (6) follows
4. Enable automatic rollback (7) only after both are proven. Finish with the full
containment and recovery proof (8). No single slice closes the epic.

Proposed operational defaults are three consecutive actual failures with a stable
reason code, a two-hour eligible-backlog alert, a 30-minute post-install probation
and recovery evaluation at most every minute. Intentional holds and external
service failures are distinct from candidate regressions. Configure these bounds;
tests must cover them without real-time waits. The release proof must include the
actual replacement/restart duration within the 30-minute recovery limit.

The independent recovery controller and retained compatible binary must already
exist before a candidate is installed. Do not depend on downloading an old release,
the candidate successfully starting its own watch, or an old state snapshot that
would discard work. If safe recovery is impossible, report a held/degraded state;
that case does not satisfy the epic's automatic-recovery acceptance criterion.

## Validation and scope

For each implementation slice, run only its focused local tests with
`LANG=C.UTF-8`; CircleCI runs the full suite and the new workflow job. This plan PR
changes documentation only: validate links, dependency order, sub-issue membership,
public-content hygiene and whitespace. No local full-suite build is needed.

Epic completion requires both proof chains: the historical merge-validation defect
is rejected before ordinary installation, and a post-canary broken candidate is
restored within 30 minutes without a person, followed by a successful workflow.
Keep sanitized evidence with versions, artifact identities and observed merge
commits. No host names, absolute filesystem locations, credentials, production
repositories or personal configuration belong in fixtures or published evidence.

This does not redesign merge authorization, bypass required CI, replace existing
unit tests, or automatically roll back changes in other repositories. Sandbox
provisioning and trusted evidence publication require explicit service configuration;
record those prerequisites in the implementing PRs rather than assuming a desk's
personal setup.
