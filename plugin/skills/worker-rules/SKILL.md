---
name: worker-rules
description: The rules a worker on a beekeeper desk follows from its first tool call to its report — registration, leases, merges, the GitHub budget, the machine, secrets and questions for the person. Load it at the start of every task a supervisor hands out, whether the session was started with `beekeeper agents start` or registered with /register-agent.
---

# worker-rules — how a worker on a beekeeper desk works its task

A worker takes one task from the supervisor its brief names and works it to the finish. These
rules hold for every worker on the desk. The desk's own conventions (the project's CLAUDE.md and
rules: which repositories, boards, installations and release flows are ours) apply alongside them
and win where they are more specific; nothing here relaxes them.

## Goal

The task ends merged, released, rolled and proven live, and the supervisor learns it from one
message: PR links, release versions, the live proof, what is still open, every fact re-queried
live at the time of the report. Then `beekeeper agents idle --done` and the turn ends; beekeeper
takes the session off the roster and archives it. Nothing the next
task needs lives only in this session: the next task goes to a fresh one.

## Context

- **The supervisor** is the session `beekeeper supervisor status` names, or its successor; messages
  go to it by name with `SendMessage`. It reaches a worker whose turn has ended by `beekeeper agents
  wake`. `beekeeper agents register` puts the session on the roster under its name at the start;
  `beekeeper sessions serve "<name>" <owner/repo#n> --waits "<what>"` records the item the session
  serves and what it waits on, kept current as the wait changes.
- **The supervisor's words.** `yours <env>` / `<env> free` for a resource of the config's
  `resources` (a lab, an installation) and `browser yours` / `browser free` for the browser;
  `hold <repo>` / `release <repo>` for merges into a repository; `go` for an announced step. The worker's words: `need <env>` with
  purpose and duration, `merging <n>` before and `merged <n> <version>` after every own merge,
  `tagging <v>` before a tag that rolls installations. Where the supervisor states a different rule
  for its watch, the supervisor's rule wins.
- **Questions for the person** go to the supervisor in one line; it files them as `beekeeper note
  add --for <person>` and the guide asks. An answer the guide relays is the person's word.

## Constraints

- **Leases.** A resource is claimed with `beekeeper lease claim <env> -p "<purpose>"` only after the
  supervisor's `yours <env>`, released with `beekeeper lease release <env>` and returned with
  `<env> free` when done. A held lease is another session's.
- **Merges.** One merge command per PR, through the gate (`devctl pr merge <owner/repo> <n>` for the
  owners under `merge.devctlOwners`, `devctl release promote <owner/repo>` likewise), acting on its
  exit code and never around it: 76 queued, the merge waits on in its lane by itself and its
  outcome wakes the worker, never run it again; 77 refused, its `beekeeper gate:` line says why. A
  `hold <repo>` stands until `release <repo>`; nothing rolls onto an installation while one of its
  clusters upgrades. A teammate's open PR that overlaps the task is
  a hard stop: tell the supervisor. Before a non-mechanical change, read the repository's open PRs
  and recent commits.
- **Git.** Conventional commit subjects of at most 72 characters; no major version bumps; never a
  force push, not even `--force-with-lease` on the worker's own branch: a branch behind its base
  merges the base and pushes normally. Work happens in a worktree of its own, never in a checkout
  another session uses.
- **The machine.** Build, test and lint commands run through `beekeeper run` (the PreToolUse hook
  does it): exit 75 means no slot, rerun in the background, never in a loop; exit 137 means the cap
  fired, bound the parallelism, never raise the cap. No heavy work while `beekeeper watch` reports
  the machine under load or memory pressure. Nothing is left running at the end: no port-forward,
  monitor or background wait; a process is killed by the PID captured at its start, never by a
  pattern. Agents never hard-delete: what goes away is moved aside.
- **GitHub.** GitHub work stops while `beekeeper budget --gate` refuses and resumes after the
  reset. No `--watch` and no sleep loop over `gh` or `devctl`: `devctl pr wait` and `devctl pr
  merge` block by themselves, and in a headless turn (a first turn, a `wake` turn) they run in the
  foreground, since the CLI ends with the turn. A rollout is checked once, when a `beekeeper timer
  add` set for it falls due, never polled.
- **Public repositories carry nothing internal.** Visibility is checked before writing; no
  installation, cluster, customer, employee, chat or secret-store details reach a public issue,
  PR, commit or comment, and a private repository is never cited in a public one: the reasoning is
  written out instead. An issue or PR description is its current state (Problem / Proposed
  solution / Acceptance criteria); the comment thread is the log. A comment written on the person's
  behalf says an agent wrote it.
- **Secrets.** Secret values are never read, printed or compared, not even hashed: keys and
  metadata only. Every call against a cluster names its context or kubeconfig explicitly; the
  production installation (`kube.production`) is never written to.
- **No waiting on the person.** No `AskUserQuestion`, no wait on a permission prompt, a browser
  approval or a settings change: each becomes a one-line question to the supervisor, and the worker
  carries on with what needs no answer or reports and ends its turn with the task open.
- **Subagents** only when the brief allows them. Each one gets these rules in its prompt, at least
  the force-push and secret rules, since a subagent reads none of this.
- **Context stays lean.** Near the supervisor's `agents.relayAt`, the worker writes a handover file,
  sends its path to the supervisor and stops.
