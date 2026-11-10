# Beekeeper and the desk

Beekeeper owns the workflow. A desk owns the context in which that workflow
runs. Fix shared behavior in beekeeper so every desk receives the same fix.

## What belongs where

| Beekeeper | The desk's own files and configuration |
| --- | --- |
| Shared skills and hooks, including their enforcement | The person's dated standing orders, their source and scope, priorities and decisions |
| The supervisor loop, scheduling and session lifecycle | The repositories and hosts the desk covers, ownership and access boundaries |
| The merge gate, review and CI coordination | Deployments, environments, dependencies and repository-specific build, deployment and proof procedures |
| The software development lifecycle (SDLC): assign, implement, review, merge, release, roll out and prove | People, responsibilities and contact context |
| Worker and reviewer briefs and rules for reporting, parking and handover | Where secrets are kept and how access is authorized; never secret values |

For example, how a worker submits a change for review belongs in beekeeper.
Which installation consumes that repository's release belongs in the desk.
The mechanism that records and relays a decision belongs in beekeeper; the
person's words, date and scope belong in the desk's decision record. A personal
standing order remains authoritative within its scope until completed or
changed; moving workflow into beekeeper must not discard it.

Desk configuration selects supported behavior, such as repositories, hosts,
capacity and merge policy. It must not implement a second supervisor in prose.
Custom worker or reviewer templates may supply task and repository context or
refer to the installed beekeeper skills. They must not copy their instructions.
Repository-specific procedures explain how that product is built or deployed;
they do not redefine beekeeper's review, merge or worker lifecycle.

## Remove duplicated workflow

A desk brief or skill that restates beekeeper workflow is a defect, even when
its copy currently agrees. Remove the duplicated instructions and reference
the beekeeper-owned skill or documentation. Preserve desk facts and the
person's dated orders; delete a whole file only when nothing else remains.
If the copy supplies behavior beekeeper lacks, file a beekeeper issue with the
missing behavior and acceptance criteria, then fix it in beekeeper. A desk
workaround is not the completed fix. Keep public reports generic and leave
private desk content in the desk.

## Doctor check

`beekeeper doctor` (also with `--dry-run`) reports `desk workflow` warnings
with a file and line number for recognizable merge or worker instructions.
It checks the configured `supervisor.workerBrief` and `supervisor.reviewBrief`
files and Markdown files beneath directories named `briefs` or `skills` in
`guide.desk`, including harness directories such as `.claude/skills`.
An unset desk or brief is skipped. Unreadable configured files and directories
are reported, not treated as clean. Git metadata and symbolic links are not
followed; a reference to an installed skill is not a copied skill.

The check recognizes commands such as `devctl pr merge`, `gh pr merge` and
worker registration, parking and reporting, plus common workflow rules in
prose. It is a heuristic: a quoted command can need no change, and an arbitrary
paraphrase may escape detection. Review each warning against this boundary.
Quoted records in `beekeeper-data` blocks are excluded, and decision and memory
files outside the scanned directories are not scanned. The check prints no
source text, changes no desk files and does not delete personal orders.
