# Disk

Disk reclamation is automatic. This page says what is pruned, when, and what is kept.

## Automatic reclamation

Disk reclamation is automatic by default (`disk.prune: auto`): every `disk.every` (1h) the watch
trims Go and sccache caches only when they exceed their caps, and applies the `disk.caches` policies
(application and package-manager caches by path or glob, each with an age, a size budget and a
mode). On `LOW DISK` or `DISK FILLING`, unused caches and unreferenced trash are reclaimed
immediately. Both scheduled and pressure runs reclaim finished worktrees and move disposable build
output into trash on the same filesystem.

Scheduled runs keep trash for an hour; run tmp entries need an hour of inactivity in either mode. It
also discovers harness worktrees under known checkouts. Live processes, running sessions,
uncommitted changes and unpublished commits protect worktrees; a failed process or remote read keeps
data. One `DISK PRUNE` line says what it freed and kept and why. Under disk pressure the same step
also prunes unused Docker images and reclaimable Docker build cache below their normal size caps,
preserving running builds, containers, lab leases and recently tagged images.

A cleanup scan lists remote branches once per shared Git repository and checks a worktree's
publication with one graph query across those tips. Successful query results (including unpublished
heads) are cached by HEAD, remote tips, shallow boundary and object-store metadata; a fetch or moved
tip invalidates them. Trees without an ended task need no publication query. Status, locks and live
references are still read again before removal. The cache is bounded and shared by cleanup runs.

Negative Docker caps still disable that cleanup. If space remains below the start floor after
pruning, the supervisor sends a `GUIDE PROBLEM` with free space, the floor and what remains; starts
recover when disk recovers. `beekeeper disk prune` previews this step; `--apply` runs it. `beekeeper
doctor` uses the same automatic policy. Tmpfs and Docker reclamation remain independent in either
mode. `disk.prune: ask` retains the existing commands and doctor trims described below.

See [automatic disk reclamation](development.md#automatic-disk-reclamation) for discovery, external
output ownership and retention settings. `beekeeper disk holders` lists what the step does not own:
the holders under `~/.local/state` by size, age and owner, and which are eligible for removal,
removing nothing ([state holders](development.md#state-holders)).

The following worktree doctor policy remains available on demand and, with `disk.prune: ask`, on its
original schedule. A finished worker's git worktree keeps its build output (`node_modules` alone is
gigabytes) until something removes it: every `doctor.worktreesEvery` (6h) one watch looks at the
linked worktrees under `doctor.worktreeRoots` (`~/.local/state/beekeeper/worktrees`), under a lock,
the watches sharing when it ran last. A worktree whose branch's newest pull request merged or closed
at least six hours ago, and whose HEAD and index are as old, is finished; the branch is the one on
origin it tracks unless that is `main` or `master`, else the one checked out.

A worktree on a detached HEAD (a release's or a pull request's proof checkout) is finished once its
HEAD and index are three days old. A finished one that holds nothing but build output (ignored
files, and untracked `node_modules`, `dist`, `dist-types`, `bin`, `build`, `out`, `target`,
`.turbo`, `.next`, `.cache`, `.yarn/cache`, `coverage` and Python's caches) is removed whole (`git
worktree remove --force`); one that holds uncommitted changes, untracked files that are no build
output, or commits neither a remote branch nor its pull request has, keeps them and loses only those
build directories.

Any other worktree (an open or no pull request, closed within the six hours) whose HEAD and index
are six hours old loses only those build directories too: the next build writes them again, and git
state, branch and commits stay. A worktree a running process, a running session, an unfinished
roster agent or a desktop session active in the last seven days works in, and a locked one, stay
untouched; an unarchived desktop session alone keeps it no longer.

The hook's refusal to `mv` or `rm` a live worktree follows the same bound, and past it still refuses
a worktree holding uncommitted or unpushed work. A run that removed something is one `WORKTREES`
line with the bytes of each tier and one `worktrees.trim` event naming each path, its size and why;
a failed one is `WORKTREES FAILED`. `beekeeper worktrees` says the same per path (`--apply` does it,
`--root` looks under another folder), and `beekeeper doctor` trims the same (`--dry-run` says what
it would); a negative `doctor.worktreesEvery` turns the watch's and the doctor's runs off.

A `devctl pr merge` or one-repository `release promote` running with no beekeeper gate among its
ancestors is one `ungated merge` line ([The merge gate](merge-gate.md#the-merge-gate)). `--dry-run`
says what it would do. A note for the person that asks nothing (no question mark, no `--option`, no
request verb opening it) is refused: a status line goes to `beekeeper log add "<text>"`.

## Under pressure

Automatic disk pruning releases its reclamation lock during discovery and remote
worktree checks, so an idle supervisor does not lock out a manual prune. Under
pressure, confirmed ended worktrees lose their retention grace; live sessions,
locks, open pull requests and unpublished work remain protected. Finished task
proofs and aside output, and fully retired execution source bundles, can be
reclaimed without waiting for age grace. Unknown ownership, checkouts and pending
execution recovery are retained. The built-in `agy-conversations` and `agy-brain`
cache policies each cap their store at 2 GiB, trimming older entries first while
protecting active conversations and the most recent hour. Each policy can be
configured or disabled through `disk.caches`.

Positive `watch.diskMinMiB` values retain a minimum 2 GiB reserve (5% on smaller
disks). This floor applies to start admission and the watch's LOW DISK warning,
so a tiny configured value cannot silently allow starts until the disk is full.

The disk start floor uses `watch.diskMinMiB`: set it to `-1` to disable LOW DISK
and its start refusal; zero retains the default 5% floor, and a positive value
sets the floor in MiB. Other capacity thresholds keep their existing keys.
