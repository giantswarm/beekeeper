# beekeeper

beekeeper keeps the Claude Code sessions sharing one machine working together. When a dozen
sessions run at once on one workstation, they compete for the same things: RAM and swap (one
OOM kill of the desktop scope ends every session), a handful of kind labs and shared
installations, the one browser the Claude in Chrome extension drives, the merges into a
repository that rolls an installation, and the one GitHub REST budget every `gh` and `devctl`
call draws from.

beekeeper reads what is on disk: the process table, the desktop app's session records, the
transcripts and the git checkouts. It needs no MCP call and no GitHub request to show what
every session does. What must be shared lives in a small state directory that survives
restarts: the supervisor, grants, holds, registered agents and notes. Every session and the
supervisor use the same binary.

## Install

Download the binary for your OS and architecture from the latest release into any directory on
your `PATH`:

```bash
dir=~/.local/bin   # any directory on PATH
curl -fsSL -o "$dir/beekeeper" https://github.com/giantswarm/beekeeper/releases/latest/download/beekeeper-linux-amd64
chmod +x "$dir/beekeeper"
```

From then on `beekeeper self-update` keeps it current: it verifies the release binary's Sigstore
signature and renames it over the old one in one step, so a running `beekeeper watch` keeps
running. `beekeeper self-update --check` exits 125 while a newer release is out.

The session and machine views need Linux (`/proc`, cgroup v2, the journal). Leases, holds and
the budget work on any system.

## What it does

| Command | For |
|---|---|
| `beekeeper sessions` | Every running session: the issues and pull requests its latest turns are about, when it was last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left), its memory, role and leases. Overlaps name what more than one session is on. `--all` adds the paused ones: a message to them does not arrive. |
| `beekeeper tail <session>` | A session's last turns without tool calls: what it said and what it was told. |
| `beekeeper snapshot` | One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots, kind clusters, the commands sessions sit on, every kernel OOM kill since your last snapshot with whose limit it hit, leases, holds, the GitHub budget. It ends with what changed since your last snapshot; `--changes` prints only that. |
| `beekeeper watch` | Silent until something needs a look, then one line: the source of a `Monitor`. Thresholds repeat at most every 10 minutes; OOM kills, sessions that start, end or restart, and stale leases are always reported. |
| `beekeeper budget` | The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), and every `gh` and `devctl` process with its session. `--gate` exits 3 under the floor. |
| `beekeeper lease claim\|release\|status\|grant\|revoke` | One holder per resource: the environments in the configuration and the browser. While a supervisor runs, a session claims only what the supervisor granted it, in grant order. |
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as a model load stops the lane whose components it exercises, not the others), every merge (`merges`, `--except owner/repo`) or every GitHub call (`github`) until lifted or a time passes. The merge gate enforces them. |
| `beekeeper lanes [queue\|drop\|clear]` | Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones in turn order, so who is next is never prose. `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h); `drop` takes a waiting merge out; `clear` frees a lane whose settling release will not roll, after a look at the installation. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies while its session runs and lifts by itself when it is gone. |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. |
| `beekeeper note add\|done` | Open items that outlive a session: a question waiting on a person, a deadline. |
| `beekeeper handover` | Everything the next supervisor needs, as Markdown, from the live state. |
| `beekeeper log` | Every claim, grant, hold, registration and note, as they happened. |
| `beekeeper run [--max SIZE] [--wait DURATION] -- <command>` | Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the `memcap` wrapper) inside a memory-capped systemd scope. When every slot is held it waits once, then exits 75 with the holders; when the cap fires the kernel kills the biggest process in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap killed:`. |
| `beekeeper hook pretooluse` | The Bash tool's PreToolUse hook: rewrites build, test, lint and lab commands to `<this binary> run -- zsh -c '<command>'` with the command verbatim and the tool timeout at 10 minutes (a background run waits 60 minutes), and refuses a third kind cluster, listing the held leases. It puts the merge gate in front of every `devctl pr merge` (below) and passes every other devctl command untouched. |
| `beekeeper free [--apply] [--only SECTIONS] [--summary]` | Show where the memory is and, with `--apply`, free what no running work needs: dead sessions' dirs in the tmpfs `/tmp` (the CLI is gone; an idle session's stay), throwaway temp dirs and orphaned jest or Claude workers. Kind clusters, idle CLIs, heavy or runaway processes and Chrome renderers are only reported. Never runs as root: the swap reset and root-owned leftovers are printed as the commands to run. `--summary` prints the TSV rows a desktop front end parses. |
| `beekeeper self-update` | Install the latest signed release over this binary; `--check` only asks. |

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor), 125 a
newer release is out (`self-update --check`). A claim gates the action it guards:
`beekeeper lease claim staging -p "database migration" && kubectl …`, never a `;` between them.

`--json` prints any command's result as JSON. A session is identified by the environment
Claude Code gives its tool commands; a person or a script passes `--as <name>`.

## The merge gate

Sessions keep typing `devctl pr merge <owner/repo> <n> [flags]`; beekeeper never replaces devctl.
The PreToolUse hook rewrites the call to `<this binary> gate -- devctl pr merge …` (a background
call gets `--wait 30m`), and the gate decides:

- **Refused, exit 77**, one line starting `beekeeper gate: refused,` that says why and what to do:
  the repository, its lane, `merges` or `github` is held (the hold's reason); the GitHub budget is
  under `github.floor`, or unknown, checked before devctl makes a single request; the lane's
  installation cannot be read (a lapsed `tsh` login); the lane waited `merge.settleTimeout` for a
  release that did not roll.
- **Queued, exit 76**, one line starting `beekeeper gate: queued,` with the merge's position and
  whom it waits behind; when the machine-wide devctl cap is the reason, the line says
  `<n> devctl processes run machine-wide (cap <n>), not a lane problem`. The merge keeps its place for `merge.queueTTL` (15m): run the same
  command again, best with `run_in_background`, where the wait is 30 minutes instead of 2.
- **Otherwise devctl runs once**, its JSON document and exit code (devctl's own 0–9) unchanged,
  and the event log records `merging` and `merged` with the release.

A merge runs when it is first in its lane's queue, nothing else of the lane runs, fewer than
`merge.cap` devctl processes run on the machine, and the lane's installation is ready: every
HelmRelease of the lane's charts Ready, and the previous merge rolled. Rolled means each
HelmRelease of the merged repository's chart that ran the newest version when the merge started
now reports the released version; a release devctl could not confirm (exit 9, a run killed by the
tool timeout) settles for `merge.settle` instead. The installation is read with `kubectl
--context <lanes[].context>` (default: the kubeconfig context named after the installation or
ending in `-<installation>`).

A merge of giantswarm/devctl opens a tool-release window by itself: a `merges` hold with
giantswarm/devctl excepted, since the release makes every in-flight devctl run refuse until
updated. It lifts once no devctl merge runs and the local `devctl version` reports another
version than when the window opened.

The budget floor uses the last reading in the state when it is younger than `merge.budgetFresh`
(1m, less than one merge's draw at the floor's margin), else a fresh conditional request, which
costs nothing when GitHub answers 304; a failed read refuses.

## Configuration

`$XDG_CONFIG_HOME/beekeeper/config.yaml` (or `--config`, or `$BEEKEEPER_CONFIG`). Every field is
optional. The defaults are the numbers proven on an 86 GiB workstation whose Claude Desktop
scope is capped at 48 GiB, so tune `watch` to your machine.

```yaml
resources: [kind-1, kind-2, staging, production]   # leasable besides the browser
grantTTL: 30m               # a grant expires this long after its resource is free
github:
  floor: 2500               # budget under which GitHub work stops
  probeRepo: giantswarm/devctl
overlaps:
  ignore: [giantswarm/giantswarm, giantswarm/roadmap]   # trackers every session mentions
  activeWithin: 1h
watch:
  interval: 30s
  repeat: 10m
  budgetEvery: 5m
  availMinMiB: 10240        # machine MemAvailable
  swapMaxMiB: 10000         # systemd-oomd kills the largest swap user at 90 %
  scopeAnonMaxMiB: 28000    # desktop scope anonymous memory (cache is reclaimable, anon is not)
  scopeMaxMiB: 45000
  loadMax: 45
  psiMax: 10
  tmpMaxMiB: 20000
  diskMinMiB: 102400
lanes:                      # merges that roll the same components of an installation
  - name: serving
    installation: gazelle
    context: teleport.giantswarm.io-gazelle   # default: the context named after the installation
    repositories: [giantswarm/model-manager, giantswarm/cluster-manager]
merge:
  cap: 5                    # devctl processes on the machine when a merge starts
  queueTTL: 15m             # a queued merge keeps its place this long after its run ended
  seedTTL: 12h              # a place queued with lanes queue --for, from its seeding or last arrival
  settle: 5m                # a lane waits this long after a merge whose release is unknown
  settleTimeout: 30m        # then refuses its next merge while the release has not rolled
  budgetFresh: 1m           # the last budget reading is used this long, then read afresh
checks:                     # what beekeeper does not read itself
  - name: alerts
    watch: [python3, /path/to/installation-alerts.py, watch]       # each line is relayed as an event
    snapshot: [python3, /path/to/installation-alerts.py, snapshot] # a snapshot section
    every: 5m
    timeout: 4m
```

State lives in `$XDG_STATE_HOME/beekeeper/` (`state.json`, `events.jsonl`, each caller's last
snapshot) and leases in `leases/`, one directory per held resource.

## Development

See [docs/development.md](docs/development.md).
