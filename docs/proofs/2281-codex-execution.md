# Remote Codex lifecycle evidence

Part of https://github.com/giantswarm/beekeeper/issues/2281.

## Environment and admission

The 2026-11-07 run used Beekeeper `v0.175.0-rc.234` (`f578add`) on
the coordinator and execution host A, pinned Codex `0.165.0`, and the
existing Codex subscription. The supervisor allocated host A through the
desk's configured `supervisor.hosts` / `hostsFirst` procedure. No provider
login, account, spending setting, or guard was changed.

The assigned board item was in a repository outside Beekeeper. An explicit
repository mapping and source checkout were provisioned, followed by the
supported host update to prepare and pin the policy. A preparation-only attempt with
an absent source checkout failed before any model turn; supported recovery
confirmed its terminal state rather than replacing it silently.

Host checks before and after the successful start reported Codex version,
login, and guard checks passed. The guard check exercises denied raw push,
merge, and forbidden-repository creation without executing those operations.
The prepared policy digest remained unchanged.

Public evidence uses stable aliases for executions, conversations, threads and
source commits. Their exact identifiers and repository mapping remain in the
coordinator's private evidence; aliases preserve correlation without publishing
personal setup or session identifiers.

## First real task execution

| Observation | Evidence |
| --- | --- |
| Execution | `execution A`, generation 1 |
| Coordinator conversation | `conversation A` |
| Actual Codex thread | `thread A` |
| Pinned source commit | `source commit A` |
| Isolation | Worker `pwd` matched its execution-owned workspace |
| Supported shell route | `pwd`, `git rev-parse HEAD`, status, and version command exited 0 |
| GitHub read | Worker read the assigned issue and overlapping held PR through `gh` |
| Subscription accounting | Exactly one worker entry in the coordinator's Codex busy list; within the configured shared ceiling |
| Preserved work | Commit `work commit A` retires the obsolete deployment script; no device or cluster deployment |
| Park callback | Coordinator acknowledged the worker's park at 13:00 UTC |

This attempt did **not** publish a PR. The worker's focused checks failed
before running because of the existing envtest provisioning defect,
https://github.com/giantswarm/beekeeper/issues/3341. Remote shell-input
validation and worker-to-worker lifecycle commands were also refused by
the guard. The worker reported these failures and preserved its commit.

The parked runner subsequently stopped on coordinator lease expiry. The
retained lease was issued at 13:20:00 UTC with expiry 13:21:30 UTC;
inspection confirmed stop intent and acknowledgement. Concurrent desk
operations reported two-minute state-lock waits. Contention is a suspected
cause, rather than a confirmed renewal error, because the expiry record
does not retain that error. The core follow-up is
https://github.com/giantswarm/beekeeper/issues/3384. Expiry and revocation
were not relaxed to continue the proof.

## Recovery and remaining acceptance

Supported recovery confirmed that the stopped execution is terminal. Its
verified recovery bundle retains the original commit under synthetic
recovery merge `recovery merge A`. Real envtest
assets were then provisioned using setup-envtest's absolute binary-directory
argument and cached under the standard assets marker.

The new execution is `execution B`, generation 1,
with coordinator conversation `conversation B` and
actual Codex thread `thread B`. It read the
issue and reported that its recovery fixture had not arrived yet, then
completed its first turn. The coordinator delivered the bundle into the
new workspace and requested continuation on that recorded thread.
The post-start host check again passed version, login, and guard checks
with the same prepared policy digest.

Publication, CI parking, same-thread wake with retained publication,
independent exact-head review, coordinator merge, and the final report must
all be observed before the proof issue closes. A new execution recovering
the commit is not a same-thread wake of the stopped execution.

Residual execution artifacts and recovery work belong to
https://github.com/giantswarm/beekeeper/issues/2113. The assigned task's
remaining implementation and deployment request remain separate follow-up;
this proof authorizes no hardware deployment.

## Alert snapshot

The supported `beekeeper alerts snapshot` route at 13:31 UTC reported 34
active alerts, including one critical alert and warnings. Operational details
were reported privately to the supervisor. This task performed no hardware or
cluster rollout, and the snapshot does not establish causality.
