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
restarts: the supervisor, grants, holds, registered agents, notes, timers and session records. Every session and the
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
| `beekeeper status [--bar]` | One line: who supervises, the leases held, the holds in force and the notes and timers that are due. `--bar` prints it as the fixed tab-separated row a desktop bar reads (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper sessions` | Every running session: the issues and pull requests its latest turns are about, when it was last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left), its memory, how full its context is, its last hour (turns, tool calls and their errors, GitHub calls, cost), role and leases. Overlaps name what more than one session is on. `--all` adds the paused ones: a message to them does not arrive. `--json` has every figure per session and their totals (see [Session metrics](#session-metrics)). |
| `beekeeper tail <session>` | A session's last turns without tool calls: what it said and what it was told. |
| `beekeeper snapshot` | One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots, kind clusters, the sessions' last hour and the three that spent the most in it, the commands sessions sit on, every kernel OOM kill since your last snapshot with whose limit it hit, leases, holds, the GitHub budget, the installations' alerts. It ends with what changed since your last snapshot; `--changes` prints only that. |
| `beekeeper watch` | Silent until something needs a look, then one line: the source of a `Monitor`. Thresholds repeat at most every 10 minutes; OOM kills, sessions that start, end or restart, stale leases and every NEW or RESOLVED alert of the installations are always reported. A note or timer that falls due, the end of a session with a record and a relay taken or expired are one line each, once: the state keeps that they were reported, so a second or restarted watch stays silent about them. After `supervisor.shift`, `RELAY DUE` is said at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued), and again only when a quiet moment follows a busy one. A recorded supervisor whose session has ended with no relay open and no restart within `supervisor.restartGrace` is `SUPERVISOR GONE`, once; its CLI back under the same session with a new PID is `SUPERVISOR RESTARTED`. A session over a threshold in `metrics.runaway` is one `RUNAWAY` line per figure, once per watch. `--notify` also sends the events that need a person to the desktop, `--standby` leaves a running supervisor's events to its watch (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper alerts watch\|snapshot\|import\|capture\|replay` | The installations' alerts, read from each Alertmanager through a bounded `kubectl port-forward` (Mimir's with the `giantswarm` tenant, else the plain one), in parallel: `watch` prints one line per NEW or RESOLVED alert since the baseline (pages and your team in capitals, a burst of one alertname as one line, one line when an installation stops or starts answering, the lease holder and the sessions working against it in brackets, and on a NEW line the merges into the installation's lanes and the lease claims on it of the last 30 minutes with their sessions, worded as timing (`during merging …`, `during merged … at …`), not as cause); `snapshot` the current set, grouped; `import` takes over another watcher's per-installation baseline. An alert below its installation's severity floor never appears in either, and an alert that keeps firing and resolving is one FLAPPING line, then quiet until it has been stable for the damper's window; neither a floor nor a damper change prints a burst of lines. `capture <dir>` records each installation's answer, `replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it: a floor or damper setting tried before the watch gets it. One process owns the baseline at a time, so two watches never split the lines. Every port-forward ends with the reading, on SIGINT or SIGTERM, and when beekeeper is killed. |
| `beekeeper budget` | The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), and every `gh` and `devctl` process with its session. `--gate` exits 3 under the floor. |
| `beekeeper lease claim\|release\|status\|grant\|revoke` | One holder per resource: the environments in the configuration and the browser. While a supervisor runs, a session claims only what the supervisor granted it, in grant order. |
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as a model load stops the lane whose components it exercises, not the others), every merge (`merges`) or every GitHub call (`github`) until lifted or a time passes. A merge hold lets one repository or pull request through with `--except owner/repo[#n]`: `hold set --lane serving --except giantswarm/model-manager#172` stops the lane but for the merge it waits for. The merge gate enforces them. |
| `beekeeper lanes [queue\|settle\|drop\|clear]` | Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones in turn order, so who is next is never prose. `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h; seeds keep their order, an arrived unseeded merge passes one whose merge has not arrived); a run with nothing merged stays in its place as `retrying` for its session's retry; `settle <owner/repo> <n> [--for <session>]` registers a merge run outside the gate (in flight when the gate went live, run without the hook): it heads its lane until it merges, then settles the lane like a gated merge; `drop` takes a waiting merge out; `clear` frees a lane whose settling release will not roll, after a look at the installation. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies while its session runs and lifts by itself when it is gone. A CLI restart is not a crash: the rule holds for `supervisor.restartGrace` (1m) after beekeeper first saw the CLI gone (a claim or a watch's poll), and a CLI back under the same session id or desktop record within it keeps the role and an open relay without a new start; `supervisor status` and `status` show it restarting, a refused claim says until when. Past the grace the rule lifts. Another session's start is refused while it runs or restarts, unless the supervisor relayed the role to it. |
| `beekeeper supervisor relay <successor>\|--cancel` | Hand the role over without a gap: the supervisor names its successor, the successor's `supervisor start` takes the role, the grant queue and the pending grants in one step, and the event log shows both. Until then the outgoing supervisor keeps the role and the grant rule; a relay not taken expires after `supervisor.relayTTL` (15m) or is withdrawn with `--cancel`. `supervisor status` in the relieved session exits 4, also after the successor relays onward, cancels a relay or is relieved in turn, until that session supervises again (or for 7 days). |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. |
| `beekeeper note add\|done` | Open items that outlive a session: a decision waiting on a person with its deadline and what happens if nobody answers (`note add --for Timo --due 22:55 --default "the alert stays as is" <text>`), a deadline. `watch` reports a note once when it is due. |
| `beekeeper timer add\|done\|list` | Times to look at something: `timer add 22:55 "check the rollout"` (or a duration, `45m`). `watch` prints one line when a timer is due; it stays open until `timer done`. |
| `beekeeper sessions serve\|unserve` | A record for any session, registered agent or not: `sessions serve <session> <owner/repo#n> [--waits "<what>"]`, the issue or epic it serves and what it waits on. `sessions` and `handover` show it; `watch` prints one line when the session ends, naming the issue to re-query. |
| `beekeeper handover [--prompt]` | Everything the next supervisor needs, as Markdown, from the live state: leases and grants, holds and their exceptions, agents, session records, notes with their defaults, timers, the merge lanes with their queues and settling merges, and what the alert watch reads (the installations and why, the ignored alert names, the baseline). `--prompt` prints the successor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope, the pending state in full and the commands that read the live values; no standing rule and no live value (version, memory figure, pull request state). |
| `beekeeper log [--verb PREFIX]` | Every claim, grant, hold, registration, note, timer, session record and build run, as they happened; `--verb run.` shows only the runs. |
| `beekeeper run [--max SIZE] [--wait DURATION] -- <command>` | Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the `memcap` wrapper) inside a memory-capped systemd scope. When every slot is held it waits once, then exits 75 with the holders; when the cap fires the kernel kills the biggest process in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap killed:`. Each run leaves `run.start` and `run.end` in `beekeeper log` with its scope, session and command, so `snapshot` and `watch` name a cap kill's session and command after the run has ended. |
| `beekeeper hook pretooluse` | The Bash tool's PreToolUse hook: rewrites build, test, lint and lab commands to `<this binary> run -- zsh -c '<command>'` with the command verbatim and the tool timeout at 10 minutes (a background run waits 60 minutes), and refuses a third kind cluster, listing the held leases. It puts the merge gate in front of every `devctl pr merge` (below) and passes every other devctl command untouched. |
| `beekeeper free [--apply] [--only SECTIONS] [--summary]` | Show where the memory is and, with `--apply`, free what no running work needs: dead sessions' dirs in the tmpfs `/tmp` (the CLI is gone; an idle session's stay), throwaway temp dirs and orphaned jest or Claude workers. Kind clusters, idle CLIs, heavy or runaway processes and Chrome renderers are only reported. Never runs as root: the swap reset and root-owned leftovers are printed as the commands to run. `--summary` prints the TSV rows a desktop front end parses. |
| `beekeeper self-update` | Install the latest signed release over this binary; `--check` only asks. |

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor), 4
relieved (`supervisor status` in the session a relay relieved), 125 a newer release is out
(`self-update --check`). A claim gates the action it guards:
`beekeeper lease claim staging -p "database migration" && kubectl …`, never a `;` between them.

`--json` prints any command's result as JSON. A session is identified by the environment
Claude Code gives its tool commands; a person or a script passes `--as <name>`.

Every session is a Claude Code CLI of its own: a desktop session, a `claude --bg` worker (its
daemon and terminal hosts are no session) or a headless `claude -p`. A CLI that a session
started, from its tool shell or through `systemd-run`, is listed under its own `--session-id`
(or `--resume`) and `-n` name, `started by` that session, never as that session restarting; a
`claude -p` its tool shell runs without an id of its own is one of that session's commands.

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
  and the event log records `merging` and `merged` with the release. A run that ends with nothing
  merged (exit 1–4, 7, 8) is `merge.failed` and keeps its place: `lanes` shows it `retrying`, and
  the session's retry of the same pull request runs before every merge that was behind it. It
  holds the lane for `merge.queueTTL` from the failure, then keeps its place for `merge.seedTTL`
  without holding up a free lane; devctl's refusal (exit 5) leaves the lane.

A merge runs when no merge before it in its lane's queue holds its place, nothing else of the lane runs, fewer than
`merge.cap` devctl processes run on the machine, and the lane's installation is ready: every
HelmRelease of the lane's charts Ready, and the previous merge rolled. Rolled means each
HelmRelease of the merged repository's chart that ran the newest version when the merge started
now reports the released version; a release devctl could not confirm (exit 9, a run killed by the
tool timeout) settles for `merge.settle` instead. The installation is read with `kubectl
--context <lanes[].context>` (default: the kubeconfig context named after the installation or
ending in `-<installation>`).

A free lane never idles for a merge that is not there. A place holds against the merges behind
it while its merge is in the gate or was within `merge.queueTTL` (a rerun after exit 76, the retry
of a failed run), and a merge registered with `lanes settle` always does. A place seeded with
`lanes queue --for` whose merge has not arrived holds up only the seeds behind it, so seeds keep
their order among themselves, and never an earlier pull request of its own session and
repository, which cannot arrive first; an arrived merge that was not seeded runs ahead of it, and
`merging` names the places it passed.

A merge the gate did not wrap leaves its lane looking free while its release rolls. `beekeeper
lanes settle <owner/repo> <n>` registers it: until the pull request is merged it heads the lane, so
its own `devctl pr merge` (a retry of a failed run) passes the gate as the lane's next merge and
the others wait behind it, through a lane hold's refusal too. Once merged through the gate, it
settles like any gated merge; merged outside it, GitHub reports no release, so the lane settles for
`merge.settle` from the merge and then frees once its HelmReleases are Ready. A merge waiting
behind the entry asks GitHub (`gh pr view`) at most once a minute; a pull request closed without a
merge leaves the lane.

A merge of giantswarm/devctl opens a tool-release window by itself: a `merges` hold with
giantswarm/devctl excepted, since the release makes every in-flight devctl run refuse until
updated. It lifts once no devctl merge runs and the local `devctl version` reports another
version than when the window opened.

The budget floor uses the last reading in the state when it is younger than `merge.budgetFresh`
(1m, less than one merge's draw at the floor's margin), else a fresh conditional request, which
costs nothing when GitHub answers 304; a failed read refuses.

## Desktop notifications

`beekeeper watch --notify` sends the events that need a person to the desktop's notification
service (`org.freedesktop.Notifications` on the session bus: dunst, mako, GNOME, KDE) and still
prints every line. Each notification carries a summary, the session or resource involved and the
command that shows more.

| Kind | Event | Urgency |
|---|---|---|
| `due` | a note or a timer falls due | normal |
| `oom-line` | the machine near its OOM line: low RAM, swap near the systemd-oomd trigger, memory pressure, the desktop scope near its cap | critical |
| `oom-kill` | a kernel OOM kill outside a build slot (a slot's cap killing its own command is its session's exit code), a systemd-oomd kill | critical |
| `budget` | the GitHub budget under the floor | normal |
| `stale-lease` | a lease whose holder's session is gone | normal |
| `no-supervisor` | a supervisor whose session ended with no successor | normal |

Nothing routine notifies: sessions starting or ending, thresholds of load, tmpfs and disk, alerts,
relays. Each event is one notification however many watches run `--notify` on the same state: the
first to claim it in `notify.json` (under `notify.lock`) sends it. A lasting condition (`oom-line`,
`budget`) notifies again after `notify.repeat`. Quiet hours hold every notification that is not
critical and send what they held as one notification when they end. With no notification service
on the bus the watch runs on, prints its lines and says so once. `notify.json` also keeps the
last 20 deliveries with the id the service returned.

When no supervisor runs, the same watch runs as a systemd user unit,
[`contrib/systemd/beekeeper-notify.service`](contrib/systemd/beekeeper-notify.service), with
`--standby`: while a supervisor's session runs it leaves the notes, timers, session records and
relays to the supervisor's watch and never reads the alerts, so it takes nothing from the
supervisor's view; what both see (the machine, OOM kills, the budget, stale leases) is sent once.
A supervisor runs its own watch with `--notify` too. To install the unit:

```bash
mkdir -p ~/.config/systemd/user
curl -fsSL https://raw.githubusercontent.com/giantswarm/beekeeper/main/contrib/systemd/beekeeper-notify.service |
  sed "s|%h/.local/bin/beekeeper|$(command -v beekeeper)|" > ~/.config/systemd/user/beekeeper-notify.service
systemctl --user daemon-reload
systemctl --user enable --now beekeeper-notify.service
journalctl --user -u beekeeper-notify -f   # its lines
```

`beekeeper status --bar` prints one row for a desktop bar (waybar, polybar, i3blocks), the same
shape as the rows of `free --summary`, so one bar module reads both. The contract is fixed: five
tab-separated fields, always present, in this order.

| Field | Value |
|---|---|
| 1 | `beekeeper`, the row's key |
| 2 | the supervising session's name; `-` when none is recorded; `!<name>` when the recorded one's session no longer runs |
| 3 | the number of leases held |
| 4 | the number of holds in force |
| 5 | the number of notes and timers whose time has come |

## Session metrics

How each session has been doing, read from what beekeeper already reads, never from GitHub:

| Figure | Source |
|---|---|
| busy time (entries less than 5 minutes apart), turns, tool calls, tool errors and the most repeated failing call, GitHub calls (`gh` and `devctl` commands, tools of a GitHub MCP server) | the transcript |
| tokens (input, cache writes by TTL, cache reads, output) and cost | the transcript's usage fields, once per API response, priced from `metrics.models` |
| context: the last request's tokens and their share of the model's window | the transcript |
| idle time | the transcript's modification time |
| memory: the process tree's anonymous memory, and each capped run's scope (`memory.current`) | `/proc` and the memcap scopes its `run.start` events name |
| `gh` and `devctl` processes | the process table, attributed as `budget` does |
| merges queued, merged, refused and failed | the gate's events in the event log |
| leases and how long each has been held | the lease directories |

The transcript's figures cover its last 512 KiB, the window `sessions` already reads for what a
session is on (`since` in `--json` is its first entry, `whole` says it is the whole transcript),
and the last hour of it. `sessions --json` has them per session, `sessions --json` and `snapshot
--json` the totals; `snapshot` names the three sessions that spent the most in the last hour.

Cost is priced per response from the model's entry in `metrics.models` (US dollars per million
tokens). The defaults are the Claude API list prices; a model without an entry, or a fast-mode
response without a `fast` multiplier, makes the cost `cost unknown`, never a guess. There is no
Prometheus export: nothing on the lab machine scrapes one.

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
  seedTTL: 12h              # a place queued with lanes queue --for, from its seeding or last arrival; a failed run's, from the failure
  settle: 5m                # a lane waits this long after a merge whose release is unknown
  settleTimeout: 30m        # then refuses its next merge while the release has not rolled
  budgetFresh: 1m           # the last budget reading is used this long, then read afresh
alerts:
  installations:            # read always; a held lease whose name has a kube context is read too
    - production            # context teleport.giantswarm.io-<name>, else <name>, else *@<name>
    - {name: lab, context: admin@lab, floor: warning}   # floor: the lowest severity shown (none, info, warning, notify, critical, page)
  ignore: [Heartbeat, InhibitionOutsideWorkingHours, Watchdog]   # the default; setting it replaces it
  team: my-team             # marked in capitals and counted
  collapse: 3               # more changes of one alertname in one reading are one line
  every: 5m
  timeout: 1m               # per installation, port-forwards included
  flap: {changes: 4, window: 1h}   # an alert's 4th change within 1h is one FLAPPING line, then quiet until stable for 1h
supervisor:                 # what handover --prompt tells the successor supervisor
  skill: supervise          # the skill it runs; or instructions: ~/supervisor.md, a file that opens the prompt
  scope: The lab machine's sessions, kind labs and merge lanes.   # default: the resources, lanes and installations configured
  shift: 8h                 # watch says RELAY DUE after this, at a quiet moment; default: never
  relayTTL: 15m             # a relay not taken by the successor's start expires
  restartGrace: 1m          # the grant rule holds this long after the supervisor's CLI was first seen gone
metrics:
  models:                   # USD per million tokens and the context window; an entry replaces the default of its id
    claude-opus-5-5: {input: 4, output: 20, cacheWrite5m: 5, cacheWrite1h: 8, cacheRead: 0.2, contextWindow: 1000000}
    my-model: {input: 1, output: 5, cacheWrite5m: 1.25, cacheWrite1h: 2, cacheRead: 0.1, fast: 2, contextWindow: 200000}   # fast: fast mode's multiplier
  runaway:                  # watch says RUNAWAY once per session and figure over these; negative: off
    githubCallsPerHour: 1000
    sameErrorRepeats: 10    # the same failing tool call in the last hour
    contextFill: 0.9
notify:                     # what watch --notify sends to the desktop
  kinds: [due, oom-line, oom-kill, budget, stale-lease, no-supervisor]   # the default: all
  quietHours: "22:00-07:00" # local time; holds everything but critical; default: none
  urgency: {oom-kill: critical, due: normal}   # low, normal, critical; oom-line and oom-kill default to critical
  repeat: 30m               # a lasting condition (oom-line, budget) notifies again after this
```

State lives in `$XDG_STATE_HOME/beekeeper/` (`state.json`, which an older beekeeper still running
writes back with the fields it does not know, `events.jsonl`, each caller's last
snapshot, the alert baseline `alerts.json` with its owner's `alerts.lock`, the notification ledger
`notify.json` with `notify.lock`) and leases in `leases/`, one directory per held resource.

## Development

See [docs/development.md](docs/development.md).
