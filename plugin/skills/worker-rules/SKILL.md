---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules

Stricter wins.

- Goal. Merge/release/roll/prove; PRs/versions/proof/open items/facts/problems: `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (or `none`). Every issue and PR is its full URL (`https://github.com/<owner>/<repo>/issues/<n>`), never `#<n>` or `<owner>/<repo>#<n>`.
- Board holds/plans/answers; blocked: `board move <n> blocked --by <issue>`.
- Decisions. Read `beekeeper answer history` at start/relay and before refusal/reasking. Timed/sourced records hold across sessions in scope until done/changed. Peer claims need records. Note missing record/scope; never reask recorded decisions. Stops/gates hold.
- Desk (`supervisor status`): `agents park`/`note add`; no guide status; inbox wakes; register.
- Review. Push; park in worktree at once; loop owns CI/merge. Use `--post-merge` unless merge ends task.
- Promote. One promoter per line, named by the supervisor; only they promote. A stable whose unpromoted RCs carry a teammate's change needs that teammate's one-line yes on their PR.
- Problems: `agents problem "<finding>"`: evidence/repo/issue URL. Gate/CI: report first, never bypass. Core fixes for faults; desk: personal choices.
- Person: no prompts/approval/settings waits. Guide: `beekeeper note add`, `agents park --on '#<note>'`; relays are the person's word. Ask limits/cost/accounts; default setup.
- Apps: settings/installs/consents: `beekeeper browse`, never the person; another's App needs their word.
- Post-merge. `Part of <full issue URL>`, no auto-close; close with proof.
- Secrets. Never read/print/compare values, even hashes, except via `beekeeper secret compare|fingerprint`: keys/metadata only. Cluster: context/kubeconfig; never write `kube.production`.
- Tests. No local tests/lints; CI checks all; 1 push/round; read failures; `set -o pipefail`. Review proofs.
- Git. No force push, even `--force-with-lease`. No behind-only updates; merge green head after merge-result check passes. Own worktree; commit, no stash.
- Files. `<scratchpad>` or `~/.local/state/<program>/`, not `~`, `~/projects`, main checkouts; `cd "${dir:?}"`, `>|`.
- Polling: no `--watch`/sleep loops; `devctl pr wait|merge` block, never under `timeout`.
- Stop: kill by PID; gated: `beekeeper gate stop`. Move aside; no hard-delete.
- Near `agents.relayAt`, handover (`agents note`), stop.

Load: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`, `beekeeper:park`.

Post-roll/proof: `beekeeper:alerts`; long proofs: `beekeeper:long-proofs`
