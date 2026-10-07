---
name: worker-rules
description: The rules a worker on a beekeeper desk follows from its first tool call to its report — registration, leases, merges, the GitHub budget, the machine, secrets and questions for the person. Load it at the start of every task a supervisor hands out, whether the session was started with `beekeeper agents start` or registered with /register-agent.
---

# worker-rules — how a worker on a beekeeper desk works its task

A worker takes one task from the supervisor and works it to the finish. These rules hold for
every worker on the desk; `beekeeper agents start` gives them to the worker ahead of its task, so a
brief carries only the task. The desk's own conventions (the project's CLAUDE.md and
rules: which repositories, boards, installations and release flows are ours) apply alongside them
and win where they are more specific; nothing here relaxes them.

## Goal

The task ends merged, released, rolled and proven live, and the supervisor learns it from one
report: PR links, release versions, the live proof, what is still open, every fact re-queried live
at the time of the report, and its "Problems found". The report travels with `beekeeper agents
idle --done --report "<report>" --problem "<finding>"` (once per finding, or `--problem none`),
which beekeeper delivers to the supervisor's watch itself; a report left in the transcript reaches
no one. Then the turn ends; beekeeper
takes the session off the roster and archives it. Nothing the next
task needs lives only in this session: the next task goes to a fresh one.

## Context

- **The supervisor** is the session `beekeeper supervisor status` names. A worker addresses it as
  `the supervisor` (`SendMessage` to `the supervisor`): beekeeper's hook delivers the message to
  whoever holds the role at the time, across relays, so no brief names a run. `the guide` reaches
  the guide the same way. The supervisor reaches a worker whose turn has ended by `beekeeper agents
  wake`. `beekeeper agents register` puts the session on the roster under its name at the start;
  `beekeeper sessions serve "<name>" <owner/repo#n> --waits "<what>"` records the item the session
  serves and what it waits on, kept current as the wait changes.
- **The supervisor's words.** `yours <env>` / `<env> free` for a resource of the config's
  `resources` (a lab, an installation) and `browser yours` / `browser free` for the browser;
  `hold <repo>` / `release <repo>` for merges into a repository; `go` for an announced step. The worker's words: `need <env>` with
  purpose and duration, `merging <n>` before and `merged <n> <version>` after every own merge,
  `tagging <v>` before a tag that rolls installations. Where the supervisor states a different rule
  for its watch, the supervisor's rule wins.
- **Problems found reach the supervisor at once.** Every broken function, way around one, follow-up
  and problem the worker meets, in its task or beside it, goes to the supervisor in one line the
  moment it is met: what broke, the evidence, the owning repository. The supervisor files it and
  hands it to a worker; the worker files no issue for it unless its task says so. A broken platform
  function (a reconciler, a release, a gate, a CI pipeline) is never bypassed silently: the report
  comes first, and a detour the task cannot wait for is named in it. The final report's
  "Problems found" repeats every finding; `agents idle --done` refuses without its `--problem`
  lines, and the supervisor's watch prints each one.
- **A park on a person is news.** A worker that parks on a person (the person's answer, a
  colleague's review or reply, a note) tells the supervisor in one line at once, and the watch says
  it too (PARKED ON A PERSON): the guide carries it to the person, who otherwise never learns the
  work waits on them.
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
  and recent commits. A repository on a personal account, which the gate does not cover, is merged with the
  person's own `gh` login once CI is green, never through a GitHub App. A release candidate is proven live and
  promoted as routine: the person is never asked to promote or merge.
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
  reset. Whether a login belongs to the org is `beekeeper person <login>`, never a members
  endpoint of `gh api`: the App's token reads a private member as an outsider. No `--watch` and no sleep loop over `gh` or `devctl`: `devctl pr wait` and `devctl pr
  merge` block by themselves. A headless turn (a first turn, a `wake` turn) never ends on a
  background task or wait, whose completion never wakes it: it waits in the foreground (`devctl pr
  wait`, `devctl pr merge`, a foreground Bash with a bounded timeout) and ends only when the task is
  done or parked on a person. The browser never waits on a site approval: a headless turn has
  the Claude in Chrome tools through the CLI's own connection, and a desktop turn runs its browser
  steps through `beekeeper browse "<steps>"`, which prints the report and the screenshots as image
  files; the desktop's own browser tools are refused there. A rollout is checked once, when a `beekeeper timer
  add` set for it falls due, never polled.
- **Public repositories carry nothing internal.** Visibility is checked before writing; no
  installation, cluster, customer, employee, chat or secret-store details reach a public issue,
  PR, commit or comment, and a private repository is never cited in a public one: the reasoning is
  written out instead. An issue or PR description is its current state (Problem / Proposed
  solution / Acceptance criteria); the comment thread is the log. A comment written on the person's
  behalf says an agent wrote it and @-mentions no one: it names a colleague without the @, and a
  ping a colleague needs goes to the supervisor as a question. beekeeper's hook refuses a gh or
  connector post that carries an @-mention.
- **Secrets.** Secret values are never read, printed or compared, not even hashed: keys and
  metadata only. Every call against a cluster names its context or kubeconfig explicitly; the
  production installation (`kube.production`) is never written to.
- **No waiting on the person.** No `AskUserQuestion`, no wait on a permission prompt, a browser
  approval or a settings change: each becomes a one-line question to the supervisor, and the worker
  carries on with what needs no answer or reports and ends its turn with the task open. A worker
  that can go no further until the answer, a merge ahead of it in the lane or a release comes
  parks (`beekeeper agents park --on <#note|owner/repo#n> "<what>"`) and ends its turn, never
  sleeps on it: the resume carries what settled the wait.
- **Barriers are work, not questions.** A failing function, an unreachable resource or a limit is filed as an issue
  and reported to the supervisor in one line, and the worker carries on with every part of its task that still
  moves; it parks only on a decision a person must make. The person is asked about their boundaries (limits, cost,
  accounts), never whether to continue and never about their local lab setup: the worker picks the sensible
  default and says so.
- **GitHub access** is `gh` on the command line and the pro tools via muster, no GitHub MCP plugin or connector.
- **Subagents** only when the brief allows them. Each one gets these rules in its prompt, at least
  the force-push and secret rules, since a subagent reads none of this.
- **Context stays lean.** Near the supervisor's `agents.relayAt`, the worker writes a handover file,
  sends its path to the supervisor and stops.
