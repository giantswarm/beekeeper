# Remote workers with coordinator parity

Epic: https://github.com/giantswarm/beekeeper/issues/2078

Status: implementation plan for review. Existing delivery is reused; this plan
creates no new worker runtime and makes no claim that the fleet is proven.
The person's current priority is Codex on execution hosts first.

## Outcome and boundaries

A worker on host-a, host-c or host-b can take the same board work as one on
host-main: use GitHub, follow the current rules, access approved MCP tools, test in
an isolated repository worktree, publish, wait for CI, receive independent review
and merge through the coordinator. Terminal executions leave only explicitly
retained, accountable state. Adding a host is one coordinator command, apart
from a provider login if its operator has not supplied one.

Keep the coordinator authoritative for subscription usage, placement, review,
exact-head merge authorization and the board. A host adds machine room, never
another subscription budget. Reuse the execution identity/generation, receipts,
leases and source/publication contracts. Failed host admission does not fall back
to local execution. Do not copy credentials, weaken host trust pins or change an
account as part of this implementation plan. MCP parity means the coordinator's
explicitly exposed tools and capabilities, not unrestricted connector access.

## Current implementation

Baseline inspected: `d5b2236c` on `origin/main`.

- GitHub execution support: https://github.com/giantswarm/beekeeper/issues/2109
  is closed. `cmd/executiongithub*.go` and `internal/execution/github.go` broker
  short-lived authorization and constrain publication. Open user-token work in
  https://github.com/giantswarm/beekeeper/pull/2196 is a separate owner; reuse its
  supported route rather than blocking subscription Codex on a new auth design.
- Repository/toolchain provisioning and isolated worktrees:
  https://github.com/giantswarm/beekeeper/issues/2111 and
  https://github.com/giantswarm/beekeeper/issues/2112 are closed.
  `internal/execution/source.go`, `workspace.go` and `toolinstall.go` supply the
  existing contracts. A closed implementation issue still needs live proof.
- MCP relay: https://github.com/giantswarm/beekeeper/issues/2114 is closed;
  extend `internal/execution/mcp.go` only for an evidenced missing capability.
- `internal/execution/runner_codex.go` already starts and resumes Codex threads.
  `policy.go` binds installed rules/settings/hooks; `cmd/executionhost.go`
  currently assembles embedded skills plus operator `RulesFile`. That does not
  establish that the coordinator's current custom rules/plugins arrive on each
  start. https://github.com/giantswarm/beekeeper/issues/2110 remains open.
- Codex trust and usable host tools have dedicated owners:
  https://github.com/giantswarm/beekeeper/issues/2265 /
  https://github.com/giantswarm/beekeeper/pull/2269 and
  https://github.com/giantswarm/beekeeper/issues/2266 /
  https://github.com/giantswarm/beekeeper/pull/2268. Remote reviewer placement:
  https://github.com/giantswarm/beekeeper/issues/2267 /
  https://github.com/giantswarm/beekeeper/pull/2270. These PRs were open when
  inspected; a plan link does not establish release or host rollout.
- CI waiting is tracked by https://github.com/giantswarm/beekeeper/issues/2242.
  Parked-worker retention is separately owned by
  https://github.com/giantswarm/beekeeper/pull/2244, and failed-check reruns by
  https://github.com/giantswarm/beekeeper/issues/2229 /
  https://github.com/giantswarm/beekeeper/pull/2278. Reuse these routes.
- `cmd/executionrelease.go`, `executionops.go` and execution journals handle
  reconciliation and stopping. Safe retirement must preserve unpublished work
  owned by https://github.com/giantswarm/beekeeper/pull/2251.
  https://github.com/giantswarm/beekeeper/issues/2113 remains the cleanup umbrella.
- https://github.com/giantswarm/beekeeper/issues/2115 remains the one-command
  installer owner. https://github.com/giantswarm/beekeeper/issues/1943 owns
  harness coverage; the fleet proof issue owns fleet proof.

## Delivery order

1. **Codex first:** land and roll the existing trust/tool fixes, CI-wait/park
   route and reviewer placement, then complete the bounded Codex lifecycle proof
   below on one leased host. Do not wait for omp/agy or installer completion.
2. **Current policy on every start:** package the portable bundle, then implement
   atomic synchronization/admission. Bundle packaging can proceed while Codex
   blockers finish. Installer orchestration consumes this route rather than
   developing its own sync implementation.
3. **Safe retirement:** implement the owned-resource inventory, then audit and
   bounded retention. This can progress alongside policy work, with the
   unpublished-work preservation owner consulted before lifecycle changes.
4. **Repeatable fleet setup and proof:** finish the existing installer issue,
   then prove Codex across host-a, host-c and host-b with current policy and
   clean retirement. Only then finish the remaining supported harness proofs
   under the existing adapter umbrella. The original Claude acceptance remains.

Dependencies refer to behavior merged, released and available on the proof host,
not merely an open PR. Independent implementation slices can run in parallel;
review and rollout still follow desk gates. Live proofs require supervisor-granted
host leases. Main/integration repairs remain with their current owners.

## Worker-sized issues

### A. First remote Codex lifecycle

https://github.com/giantswarm/beekeeper/issues/2281

Owner scope: one proof/regression PR extending the existing adapter. Dependencies:
https://github.com/giantswarm/beekeeper/issues/2265,
https://github.com/giantswarm/beekeeper/issues/2266,
https://github.com/giantswarm/beekeeper/issues/2267,
https://github.com/giantswarm/beekeeper/issues/2242 and parked-review retention
from https://github.com/giantswarm/beekeeper/pull/2244.

Acceptance: two Codex starts leave policy valid; usable guarded shell tools;
GitHub research, focused tests, owned-branch publication and PR creation; CI
park/wake on the same thread; independent review and exact-head coordinator
merge. Record version, host, execution identity, commit and PR/merge URLs. A
new gap gets its owning issue; this proof must not duplicate in-flight fixes.

### B. Portable policy bundle

https://github.com/giantswarm/beekeeper/issues/2282

Child of https://github.com/giantswarm/beekeeper/issues/2110. No implementation
prerequisite. One PR adds a reproducible versioned manifest for current applicable
rules, skills, plugin/tool declarations and portable harness settings, Codex first.
Credential stores and operator-local settings are excluded explicitly. Validate
schema, sizes, paths and content; test rule changes, missing/tampered artifacts,
unsupported required capabilities and preservation of host configuration.

### C. Atomic synchronization and start admission

https://github.com/giantswarm/beekeeper/issues/2283

Child of https://github.com/giantswarm/beekeeper/issues/2110; depends on B. One PR
resolves the approved bundle for every start, atomically prepares it, and verifies
its digest immediately before launch. A rules change reaches the next worker;
failed synchronization starts none. Existing active/parked workers retain their
admitted policy. Test interrupted/concurrent preparation and changed coordinator
content; hosts check names expected/installed manifest metadata and capability
failures. Prove two starts across a harmless rule change.

### D. Owned-resource inventory and safe retirement

https://github.com/giantswarm/beekeeper/issues/2284

Child of https://github.com/giantswarm/beekeeper/issues/2113. No policy dependency;
coordinate with https://github.com/giantswarm/beekeeper/pull/2251. One PR records
owned worktrees/refs, scratch, source bundles, worker-only caches/downloads and
credential handles against execution/generation. Terminal report, stop, failed
start and crash converge on idempotent retirement. Parks preserve resumable work.
Preserve dirty/unpublished work durably before moving owned resources aside;
revoke authorization with retry state. Test containment, symlinks, concurrent
workers, interruption and unavailable hosts. Never sweep global home or /tmp.

### E. Leftover audit and bounded retained state

https://github.com/giantswarm/beekeeper/issues/2285

Child of https://github.com/giantswarm/beekeeper/issues/2113; depends on D. One PR
makes hosts check report orphan resources, overdue retirement/revocation and logs
past retention without mutating the host. Explicit bounded reconciliation uses
the same lifecycle, and accounts for shared sources/toolchains, active/parked
workers, recoverable work and retained evidence. Unknown ownership prevents
cleanup. Seeded audit tests and a leased-host before/after inventory are required.
Document retained classes/durations; honor the desk's move-aside rules.

### F. Finish the existing CI-wait slice

https://github.com/giantswarm/beekeeper/issues/2242

Reuse its owner. A remote worker can request a coordinator-owned CI condition
wait, end its turn, and receive one durable wake on success/failure/deadline.
No worker polling loop, remote timer authority or direct merge bypass is needed.
Test restart/replay, red checks, timeout and parked publication/review retention.
A wait cannot silently succeed without observed checks. Work with the retention
and CI-rerun PRs rather than taking over their files. Required for A.

### G. Finish the existing one-command installer

https://github.com/giantswarm/beekeeper/issues/2115

Reuse its owner. Depends on C and E for the final readiness/cleanliness claim.
One idempotent coordinator command provisions verified beekeeper/harness/toolchain
versions, approved repositories, the policy and MCP route, and runs hosts check.
Keep provider login as the explicit remaining operator prerequisite; never copy
its credentials. Refuse unknown host identity/keys, preserve unrelated files,
resume interrupted setup, and do not advertise an unready harness. Prove fresh
setup and a rerun on a leased host. Reuse existing repository/worktree/MCP code.

### H. Fleet acceptance, then remaining harnesses

the fleet proof issue and
https://github.com/giantswarm/beekeeper/issues/1943

Reuse these owners after A, C, E and G. Run the acceptance matrix below; link
proofs back to the epic. The remaining omp/agy work, including
https://github.com/giantswarm/beekeeper/pull/2252, follows Codex and does not delay
its first usable host. Do not reopen closed provisioning or MCP slices without
an observed regression and a narrowly scoped issue.

## Proof and completion

Unit/protocol regressions use temporary repositories and fake transports; live
proof is additional evidence, not a substitute. CI is the full-check owner.
Each implementation worker records its published head, focused tests and residual
risk before desk review; each live proof names the installed release and host.

- First usable Codex
  - Hosts: One leased host
  - Required evidence: Guarded tools, valid policy after repeated starts, GitHub read/push/PR, same-thread CI park/wake, independent review, coordinator merge
- Current configuration
  - Hosts: Each configured host
  - Required evidence: Changed coordinator rule/skill reaches next start; expected manifest matches; approved MCP tool works; unsupported required capability refuses admission
- Lifecycle and cleanup
  - Hosts: Each configured host
  - Required evidence: Terminal success/stop/crash, dirty-work recovery, parked-worker retention, bounded revocation retry, before/after resource inventory and clean hosts check
- Fleet parity
  - Hosts: host-a, host-c, host-b
  - Required evidence: Loop places real Codex board work on each host in a repository beyond desk/beekeeper, repository tests pass, PR reviewed/merged, resources retired
- Epic completion
  - Hosts: All configured supported harnesses
  - Required evidence: Claude acceptance retained, remaining harness proofs linked, installer fresh/rerun proof, workarounds removed only after replacement behavior is proven

An unreachable host is unknown, never a clean audit. A closed issue, successful
start or passing version probe alone is insufficient. Retained recovery artifacts
are explicitly inventoried with an owner and retention policy; losing unpublished
work cannot be used to obtain a clean host. Post-roll proofs also take the desk's
standard alerts snapshot. This plan PR completes planning only; implementation,
release and fleet acceptance stay open under their linked owners.
