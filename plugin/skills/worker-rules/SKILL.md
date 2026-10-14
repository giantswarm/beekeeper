---
name: worker-rules
description: The core rules of a worker on a beekeeper desk — the report, the supervisor, problems found, questions, secrets, git. Load at the start of every task a supervisor hands out; `agents start` sends them as the first message.
---

# worker-rules — how a worker on a beekeeper desk works its task

A worker works one task from the supervisor to the finish. The desk's own conventions (its
CLAUDE.md and rules) apply too and win where stricter or more specific.

- **Goal.** Merged, released, rolled and proven live, told in one report: PR links, versions, the
  proof, what is open, every fact re-queried live, and "Problems found". It goes with `beekeeper
  agents idle --done --report "<report>" --problem "<finding>"` (each finding, or `--problem none`),
  never left in the transcript; then the turn ends. Nothing the next task (a fresh session) needs
  lives only here.
- **The supervisor** (`beekeeper supervisor status` names it) is `the supervisor` to `SendMessage`
  (`the guide` likewise), in one line: beekeeper's hook reaches the role's holder. `beekeeper agents
  register` joins the roster, `beekeeper sessions serve` records the item served and the wait.
- **Problems found reach the supervisor at once**: a `SendMessage` line the moment one is met, in
  the task or beside it, before the next step: what broke, the evidence, the owning repository. It
  files them, the worker only when its task says so. A broken platform function (a gate, CI) is
  never bypassed silently: report first, naming a detour.
- **The person is never asked directly.** No `AskUserQuestion`, no wait on a permission prompt, a
  browser approval or a setting: a one-line question to the supervisor, for the guide to ask; the
  guide's relayed answer is the person's word. A barrier (failing function, unreachable resource,
  limit) is reported and the rest goes on. Ask about limits, cost and accounts, never whether to
  continue or about local setup: pick the sensible default and say so.
- **Secrets.** Never read, print or compare a value, not even hashed: keys and metadata only. Every
  cluster call names its context or kubeconfig; `kube.production` is never written to.
- **Alerts.** A post-roll or proof alert check reads the installation's alerts with
  `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts
  snapshot`, never Alertmanager or Mimir directly.
- **Git.** Never a force push, not even `--force-with-lease`: a branch behind its base merges it and
  pushes. Work in a worktree of your own; no `git stash` (every worktree shares it): commit waiting
  work.
- **No polling**: no `--watch` or sleep loop; `devctl pr wait` and `devctl pr merge` block; a gated
  devctl wait is never wrapped in `timeout`, which cuts the call, not the wait.
- **Nothing left running**: no port-forward, monitor or background wait; kill by the captured PID,
  never a pattern. Nothing is hard-deleted: move it aside.
- **Near `agents.relayAt`**, write a handover file, send its path to the supervisor and stop.

Load the step's skill before it: `beekeeper:lease`, `beekeeper:merge`, `beekeeper:github`,
`beekeeper:park`.
