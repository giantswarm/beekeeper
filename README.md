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

```bash
bin=$(go env GOBIN)   # any directory on PATH
gh release download --repo giantswarm/beekeeper --pattern beekeeper-linux-amd64 --output "$bin/.beekeeper.new" --clobber
chmod +x "$bin/.beekeeper.new" && mv "$bin/.beekeeper.new" "$bin/beekeeper"
```

The rename replaces the binary without disturbing a running `beekeeper watch`; writing over it in
place fails while one runs.

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
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a proving window, a broken main) or every GitHub call (`github`) until lifted or a time passes. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies while its session runs and lifts by itself when it is gone. |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. |
| `beekeeper note add\|done` | Open items that outlive a session: a question waiting on a person, a deadline. |
| `beekeeper handover` | Everything the next supervisor needs, as Markdown, from the live state. |
| `beekeeper log` | Every claim, grant, hold, registration and note, as they happened. |

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor). A claim
gates the action it guards: `beekeeper lease claim graveler -p "Dex restart" && kubectl …`,
never a `;` between them.

`--json` prints any command's result as JSON. A session is identified by the environment
Claude Code gives its tool commands; a person or a script passes `--as <name>`.

## Configuration

`$XDG_CONFIG_HOME/beekeeper/config.yaml` (or `--config`, or `$BEEKEEPER_CONFIG`). Every field is
optional. The defaults are the numbers proven on an 86 GiB workstation whose Claude Desktop
scope is capped at 48 GiB, so tune `watch` to your machine.

```yaml
resources: [kind-1, kind-2, graveler, gazelle]   # leasable besides the browser
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
