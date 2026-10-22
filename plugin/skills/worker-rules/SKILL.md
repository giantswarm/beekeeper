---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules — how a worker on a beekeeper desk works its task

A worker works one task to the finish. The desk's CLAUDE.md and rules win where stricter.

- **Goal.** Merged, released, rolled and proven live, in one report (PR links, versions, the proof, what is open, live facts, "Problems found") by `beekeeper agents idle --done --report "<report>" --problem "<finding>"` (each finding, or `none`).
- **The board holds the order**: priority, holds, plans, answers; next task: `board next`.
- **The supervisor** (`beekeeper supervisor status` names it) is `the supervisor` to `SendMessage` (`the guide` likewise), in one line. `beekeeper agents register` joins the roster.
- **Problems found reach the supervisor at once** in one line: what broke, evidence, owning repository. The supervisor files them; a worker if its task says. A broken platform function (a gate, CI) is never bypassed silently: report first, naming a detour.
- **The person is never asked directly.** No `AskUserQuestion`, no wait on a permission prompt, browser approval or setting: a one-line question to the supervisor for the guide; its relayed answer is the person's word. A barrier is reported, the rest goes on. Ask about limits, cost and accounts, never continuing or local setup: pick the sensible default, say so.
- **Our own Apps** (`beekeeper app`): settings, installs and consents are the worker's `beekeeper browse` job, never the person's; a consent as the person to another's App needs their word.
- **Secrets.** Never read, print or compare a value, even hashed, but by `beekeeper secret compare|fingerprint`: keys and metadata only. Every cluster call names its context or kubeconfig; never a write to `kube.production`.
- **Alerts.** A post-roll or proof check reads `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot`, never Alertmanager or Mimir.
- **Git.** Never a force push, not `--force-with-lease`: a branch behind its base merges it and pushes. Work in your own worktree; no `git stash` (shared): commit waiting work. Files take `>|`: a rerun's bare `>` is noclobber's error, not the gate's.
- **No polling**: no `--watch` or sleep loop; `devctl pr wait|merge` block, never under `timeout`, which cuts them; an agentlab proof only waits: never under `beekeeper run`.
- **Nothing left running**: no port-forward, monitor or background wait; kill by captured PID, a gated run by `beekeeper gate stop`. Nothing is hard-deleted: move it aside.
- **Near `agents.relayAt`**, write a handover file, send its path to the supervisor, stop.

Load the step's skill first: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`,
`beekeeper:park`.
