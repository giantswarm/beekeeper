# Remote Codex lifecycle evidence

Part of https://github.com/giantswarm/beekeeper/issues/2281.

## Environment and admission

The 2026-11-07 run used Beekeeper `v0.175.0-rc.234` (`f578add`) on
the coordinator and execution host A, pinned Codex `0.165.0`, and the
existing Codex subscription. The supervisor allocated host A through the
desk's configured `supervisor.hosts` / `hostsFirst` procedure. No provider
login, account, spending setting, or guard was changed.

The assigned board item was
https://github.com/teemow/heatpump-firmware/issues/91. An explicit repository
mapping and source checkout were provisioned, followed by the supported
host update to prepare and pin the policy. A preparation-only attempt with
an absent source checkout failed before any model turn; supported recovery
confirmed its terminal state rather than replacing it silently.

Host checks before and after the successful start reported Codex version,
login, and guard checks passed. The guard check exercises denied raw push,
merge, and forbidden-repository creation without executing those operations.
The prepared policy digest remained
`sha256:2f19537e61002614af2c2cc0c8926a28c974b81d037e620ea76e15c0db495b80`.

## First real task execution

| Observation | Evidence |
| --- | --- |
| Execution | `336e3f82-757f-4650-9b11-cf169120db10`, generation 1 |
| Coordinator conversation | `f0601d88-1d62-43d5-9786-64ca3c87d52f` |
| Actual Codex thread | `01a1b613-6bd3-7aa0-afaa-e4cef5961830` |
| Pinned source commit | `fd355bea13bd084a1e2bad0037f95209398561d2` |
| Isolation | Worker `pwd` matched its execution-owned workspace |
| Supported shell route | `pwd`, `git rev-parse HEAD`, status, and version command exited 0 |
| GitHub read | Worker read the assigned issue and overlapping held PR through `gh` |
| Subscription accounting | Exactly one worker entry in the coordinator's Codex busy list; ceiling 21, room 18 at observation |
| Preserved work | Commit `db3cc2b` retires the obsolete deployment script; no device or cluster deployment |
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
recovery merge `38244a2dd01c9f27f0008d677e0f1b3af3cd062d`. Real envtest
assets were then provisioned using setup-envtest's absolute binary-directory
argument and cached under the standard assets marker.

The new execution is `b5d9b076-82e5-4ec3-b296-51799651a69b`, generation 1,
with coordinator conversation `c5e6b8cd-c647-43eb-b794-f47f9fcdbd53` and
actual Codex thread `01a1b635-7cc4-7073-9f83-5bd2ca48bce7`. It read the
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
https://github.com/giantswarm/beekeeper/issues/2113. The firmware issue's Rust
clock implementation and deployment request remains separate follow-up;
this proof authorizes no hardware deployment.

## Alert snapshot

The supported `beekeeper alerts snapshot` route at 13:31 UTC reported 34
active alerts: one critical heat-pump lockout, a stale-volume-attachment
warning, failed jobs, and CVE backlog/workload alerts. These findings were
reported to the supervisor. This task performed no hardware or cluster
rollout, and the snapshot does not establish causality.
