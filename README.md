# beekeeper

beekeeper keeps the Claude Code sessions sharing one machine working together. When a dozen
sessions run at once on one workstation, they compete for the same things: RAM and swap (one
OOM kill of the desktop scope ends every session), a handful of kind labs and shared
installations, the one browser the Claude in Chrome extension drives, the merges into a
repository that rolls an installation, and the one GitHub REST budget every `gh` and `devctl`
call draws from.

beekeeper reads what is on disk: the process table, the record each Claude Code CLI keeps of
the session it runs (`~/.claude/sessions`), the desktop app's session records, the transcripts
and the git checkouts. It needs no MCP call and no GitHub request to show what
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

Then put it in place for your user, after a look at what it would do:

```bash
beekeeper install --dry-run   # every file, hook and service step; changes nothing
beekeeper install
```

`install` merges the PreToolUse, PermissionRequest, PostToolUse and SessionStart hooks into Claude Code's user settings
(`~/.claude/settings.json`, or `$CLAUDE_CONFIG_DIR`) with the binary's absolute path, writes and
starts the [standby service](#desktop-notifications) (a systemd user unit, a launch agent on
macOS), with `teleport.proxy` set the [Teleport login's keeper](#the-teleport-login), and on systemd the memory guard sized to the machine's RAM: `memcap.slice` for `beekeeper
run`'s capped commands and a drop-in for the Claude Desktop scope that runs (run install again
with the app running when it does not). Without a config it writes a starter one, the [example
configuration](docs/examples/config.yaml) with every key commented out. A file already as install
writes it stays, and one that differs and that install did not write it keeps and names; a second
run changes nothing. A new Claude Code session picks the hooks up. `beekeeper uninstall` (also
with `--dry-run`) stops the units and removes exactly what install wrote, as `install.json` in
the state directory records it, and leaves the config and the state unless `--purge`. On Linux
without systemd install writes the hooks and the config and says the service is not available.

The hooks are user-wide, but they act only in the desk's scope: a session beekeeper started or knows
(an `agents start`, the supervisor's or the guide's holder, an agent on the roster), or one whose
working directory or project lies under one of `hooks.scope.dirs`. A session on the person's own
projects outside these gets nothing from them: no refusal, rewrite, redaction, prelude or permission
answer. Unset, every session is in scope.

```yaml
hooks:
  scope:
    dirs: [~/projects/notebook, ~/projects/work]
```

From then on `beekeeper self-update` keeps it current: it verifies the release binary's Sigstore
signature and renames it over the old one in one step, so a running `beekeeper watch` keeps
running, and a `devctl pr merge` gate call waiting for its turn re-executes the new binary at its
place. A running watch keeps the code it started with: every watch, `--once` included, says
`WATCH STALE` once per watch whose binary was replaced, naming the version it runs and the one
installed, and that a re-arm (a restart) picks the new one up; no watch re-executes itself.
`beekeeper self-update --check` exits 125 while a newer release is out.

An install leaves the older binaries already running on their old code until they end or are
re-armed: the `agents start` of a worker started before it (its desktop reopen saves the state
after the first turn), a watch, the standby unit, a waiting gate call. Their saves keep what the
new release recorded: every object of `state.json` that carries per-entry data (the state itself,
roster entries and their keep markers, notes, timers, grants, holds, lanes, session records,
starts, archives, the roles) writes the members its binary does not know back unchanged, with its
entry. The state carries the version of the newest beekeeper that saved it (`writer`); an older
process's first save after it is logged once as `state.stale-writer`, naming the process, its
command and its version, and `beekeeper doctor` (a `DOCTOR stale writer` line in the watch) lists
each such process while it runs, since it still acts by its older rules. A process older than this
mechanism (v0.71.1 and before) keeps only the state's top-level members and drops the nested ones.

The session and machine views need Linux (`/proc`, cgroup v2, the journal). Leases, holds and
the budget work on any system.

### The role plugin

The commands are half of a desk; the roles that drive them are the other half. The repository is
also a Claude Code plugin marketplace whose plugin [`plugin/`](plugin) carries them as skills:
`/beekeeper:supervise` (the supervisor), `/beekeeper:guide` (the session that walks the person
through their decisions), `/beekeeper:register-agent` (an empty session joining the roster) and
`/beekeeper:worker-rules` (what every worker follows; `agents start` puts it ahead of every brief). Their text is generic:
the desk's installations, resources, lanes and person are the configuration's keys (below), and
the desk's own conventions — which board, which repositories, local traps — stay in the project
the sessions run in, which the skills defer to. `beekeeper lint briefs <file or folder>...`
refuses what makes a skill or a brief go stale: a dated line, an "until X ships" clause, a note on
the release that fixed something, a workaround, a role's run number. The shipped skills pass it in
CI, and a project holding its own skills can run it as a CI step (exit 3 on a finding).

```bash
claude plugin marketplace add giantswarm/beekeeper
claude plugin install beekeeper@beekeeper   # --scope project for one project only
```

or, for a project's shared settings (`.claude/settings.json`):

```json
{
  "extraKnownMarketplaces": {
    "beekeeper": {"source": {"source": "github", "repo": "giantswarm/beekeeper"}}
  },
  "enabledPlugins": {"beekeeper@beekeeper": true}
}
```

The plugin follows the default branch; `"ref": "<release tag>"` in the source pins it to the
binary's release. With `supervisor.skill: beekeeper:supervise` and `guide.skill: beekeeper:guide`
a relayed successor opens with the plugin's role.

## What it does

| Command | For |
|---|---|
| `beekeeper capacity` | The busy, parked and idle agents against `capacity.floor` and `capacity.ceiling`, the headroom that bounds a new start (MemAvailable, the swap's growth over the watch's readings, the free build slots, the kind labs against their cap) and one verdict: `room for N starts` or what blocks one. Read-only, no GitHub call. |
| `beekeeper status [--bar]` | One line: who supervises, the leases held, the holds in force, the notes and timers that are due and the busy agents against the target (`busy 4/5-10`). `--bar` prints it as the fixed tab-separated row a desktop bar reads (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper sessions` | Every running session, Claude Code's and omp's (marked `omp busy` or `omp idle`, see [omp agents](#omp-agents)): the issues and pull requests its latest turns acted on (its `sessions serve` record, its `gh` and `devctl` commands and GitHub tool calls, what it created; a ref it only quoted or read is a mention, kept in `--json` as `mentioned`), when it was last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left), its memory, how full its context is, its last hour (turns, tool calls and their errors, GitHub calls, cost), role and leases, after `archived` or `test` for a session the guide's feed leaves out. Overlaps name what more than one session acts on. `--all` adds the paused ones and the omp sessions of the last 24 hours no process runs: a message to them does not arrive. `--json` has every figure per session and their totals (see [Session metrics](#session-metrics)). |
| `beekeeper tail <session>` | A session's last turns without tool calls: what it said and what it was told. |
| `beekeeper snapshot` | One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots, kind clusters, the sessions' last hour and the three that spent the most in it, the commands sessions sit on, every kernel OOM kill since your last snapshot with whose limit it hit (a memcap scope no `run.start` names says `cap unknown`, never the default cap; a test run's scope is a test kill), leases, holds, the GitHub budget, the installations' alerts and their running upgrades (`upgrades: prod/mc 35.0.1 → 35.1.1 for 9m (control plane 3/4, node pools 15/15) · test none`). A caller inside a Claude session that has taken one before gets only what changed since, or one `no change` line; `--full` prints the whole screen and then what changed. |
| `beekeeper ui` | The person's screen, for a terminal rather than a session's context: six tabs over the live state — the machine (load, RAM, swap, pressure, the desktop scope, disks, build slots, kind labs, the waits and the OOM kills), every session, Claude Code and omp (its state — busy, idle, waiting on its person, omp's own —, idle age, memory, context fill, last hour, what it runs, what it waits on its person for — a waiting session stands out; `enter` opens its pane, which follows the session live: the history, then the current turn with its tool calls as they land, re-read every refresh; `k`/`j` scroll back and forward, `G` follows again; `m` writes the session a message, stamped as the person's through the screen: by name to a Claude Code CLI, into the inbox of an omp agent beekeeper started; `t` takes a Claude Code session over: its permission requests come to the pane with its turns beside them, `a` allows and `d` denies, and one left unanswered for 290 s, or when `t` hands it back or the screen quits, goes to the session's own window, which the pane says), the shares (held leases and their staleness, free resources, grant queues, holds, the merge lanes), the supervision (the supervisor and guide roles with their relay state, the agents, the open notes with their defaults, the timers, the session records), the installations' alerts by severity with the running upgrades and the GitHub budget and its pollers, and the event log. Refreshes every two seconds (the budget and the upgrades every minute, in the background); besides those messages and take-overs it reads only: it takes no lease and lifts no hold — those stay with the commands, whose exit codes the sessions gate on. |
| `beekeeper watch` | Silent until something needs a look, then one line: the source of a `Monitor`. A threshold breach, an unreadable source and a stalled lane are one line when they start and one `ENDED` line when they end, never repeated while they last; OOM kills, sessions that start, end or restart, stale leases and every NEW or RESOLVED alert of the installations are always reported. A note or timer that falls due, the end of a session with a record and a relay taken or expired are one line each, once: the state keeps that they were reported, so a second or restarted watch stays silent about them. Once the supervisor session's context (its transcript's last request, the `CTX` column) reaches `supervisor.relayAt` (400k tokens), `RELAY DUE` is said at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued, no relay open), once per supervisor and again only after a relay is cancelled or expires; `supervisor status` and `handover` show the supervisor's context. A recorded supervisor whose CLI has stayed gone past `supervisor.restartGrace` with no relay open is `SUPERVISOR GONE`, once: claims stay gated until a successor's `supervisor start`; a supervisor back after it is `SUPERVISOR BACK`, once; its CLI back under the same session with a new PID is `SUPERVISOR RESTARTED`. A session over a threshold in `metrics.runaway` is one `RUNAWAY` line per figure, once per watch. A fork rate `watch.forkRateMax` over the machine's usual one for two samples is one `PROCESS STORM` line, and more than `watch.stackMax` long-running copies of one command line one `STACKED` line each, with the commands, the parent and the owning session, and more than `watch.toolProcsMax` (1000) processes of the CLIs in `watch.tools` (kubectl, helm, tsh, gh, flux, devctl) machine-wide one `LOAD` line with their commands and the sessions that run them, and an `ENDED` line each. A stalled lane (see `lanes`) is one `LANE STALLED` line, repeated at most every 10 minutes while it lasts. A settling merge leaves its lane once its release rolled and the lane's HelmReleases are Ready, however late; one not settled past `merge.settleTimeout` is one `LANE STUCK` line with what the lane waits for (the HelmRelease off the release, the one not Ready, an installation that cannot be read), repeated at most every 10 minutes, and one `ENDED` line when it settles or the lane is cleared; `lanes` marks it `stuck since`. A cluster upgrade on an installation (see [Cluster upgrades](#cluster-upgrades)) is `UPGRADE <installation>/<cluster> <from> → <to>` when it begins and `UPGRADE ENDED … (since HH:MM)` when it ends, once each. The quiet rules keep what is noise for the supervisor out of its lines: other teams' alerts matching `alerts.quiet` (by default their e2e test clusters, `t-*`, and, once `alerts.team` is set, every other team's and team-less `notify` alert), an alert back after a reading that missed it with its old start (an Alertmanager restart, a reconnect), and the start, end and restart of the short-lived sessions in `watch.quietSessions` (by default beekeeper's tests, `test: *`). A rule never holds back an alert of `alerts.team`, one on an installation in play (leased, claimed or merged into within the last 30 minutes, or with a merge settling), or a page unless the rule names a cluster; everything else is said as before. What they hold back is logged (`beekeeper log --verb watch.quiet`) and counted in `snapshot`. `--notify` also sends the events that need a person to the desktop, `--standby` leaves a running supervisor's events to its watch (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper alerts watch\|snapshot\|import\|capture\|replay\|own` | The installations' alerts, read from each Alertmanager through a bounded `kubectl port-forward` (Mimir's with the `giantswarm` tenant, else the plain one), in parallel: `watch` prints one line per NEW or RESOLVED alert since the baseline (pages and your team in capitals, a burst of one alertname as one line, one line when an installation stops or starts answering, the lease holder and the sessions working against it in brackets, and on a NEW line the merges into the installation's lanes and the lease claims on it of the last 30 minutes with their sessions, worded as timing (`during merging …`, `during merged … at …`), not as cause); `snapshot` the current set, grouped; `import` takes over another watcher's per-installation baseline. An alert below its installation's severity floor never appears in either, and an alert that keeps firing and resolving is one FLAPPING line, then quiet until it has been stable for the damper's window; neither a floor nor a damper change prints a burst of lines. `capture <dir>` records each installation's answer, `replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it: a floor or damper setting tried before the watch gets it. `own <installation/alertname>` records the caller (`--by` another running session, `--as` a person) as the owner of the firing alerts it names (`alert.owned`); ownership ends with the owning session or the alert's resolution. `beekeeper watch` says `PAGE UNOWNED <installation> <alertname> <where> for <duration>` (one line per alertname, `(+n)` for its other unowned alerts) for a firing alert at `alerts.pageSeverity` (default `page`), of `alerts.team` when it is set, that has had no owner for `alerts.ownerGrace` (15m), and again every `alerts.ownerGrace` while it stays unowned; the owner's session ending starts the count again, and a non-paging alert never gets the line. One process owns the baseline at a time, so two watches never split the lines. Every port-forward ends with the reading, on SIGINT or SIGTERM, and when beekeeper is killed. |
| `beekeeper budget` | The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), and every `gh` and `devctl` process with its session. `--gate` exits 3 under the floor. |
| `beekeeper lease claim\|release\|status\|grant\|revoke` | One holder per resource: the environments in the configuration, the shared installations of the central instance when one is configured ([The central instance](#the-central-instance)), the host's model server (`model-server`, whose claim carries `--gib <n>`: the GiB its models may hold, see [The model server](#the-model-server)) and the browser. While a supervisor runs, a session claims only what the supervisor granted it, in grant order. While a cluster upgrade runs on the installation of the resource's name, nobody claims it except by the supervisor's `lease grant <installation> <session> --upgrade-unblock "<why>"`, for the work that unblocks the upgrade (see [Cluster upgrades](#cluster-upgrades)). |
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as a model load stops the lane whose components it exercises, not the others), every merge (`merges`) or every GitHub call (`github`) until lifted, a time passes or its probe passes (`--lift-when "<shell command>"`: the watch lifts the hold once the command exits 0 and logs it as `hold.lift`). A merge hold lets one repository or pull request through with `--except owner/repo[#n]`: `hold set --lane serving --except giantswarm/model-manager#172` stops the lane but for the merge it waits for. A lane hold that excepts one pull request (`owner/repo#n`) is also its fix window: that merge runs without the lane's HelmReleases Ready or the previous release rolled, for the fix of a rollout only the fix can repair, logged as `merge.window`. The merge gate enforces them. |
| `beekeeper lanes [queue\|settle\|drop\|clear]` | Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones in turn order, so who is next is never prose. `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h; seeds keep their order, an arrived unseeded merge passes one whose merge has not arrived); a run with nothing merged stays in its place as `retrying` for its session's retry; `settle <owner/repo> <n> [--for <session>]` registers a merge run outside the gate (in flight when the gate went live, run without the hook): it heads its lane until it merges, then settles the lane like a gated merge; `drop` takes a waiting merge out (a place whose pull request merged or closed leaves by itself at the next `watch` poll), and ends the devctl of a running merge whose pull request merged or closed (`watch` does so by itself `merge.hungAfter`, 45m, after the merge); `clear` is the repair for a lane whose settling release will not roll, after a look at the installation. A lane where no merge runs and whose first arrived merge has waited longer than `merge.stallAfter` (5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left it) is `stalled`, with the waiting merge and those places named. |
| `beekeeper board next [--claim]\|move` | The project board's next item of work: `next` walks `board.order` (steps by Status, Kind and labels; an epic's open sub-issues; blocked items whose blockers all closed; items created within a window; a GitHub search; a sub-issue that is a board item held to the order by its own Status, whatever its Team) over one read of the board and prints the first free item with why it is picked and why each item above it was skipped: served by a running session (a `sessions serve` record, a busy agent's task) or by an agent on the roster whose CLI is gone while it is busy with its task or kept, waiting on an open note, assigned outside `board.people`, blocked by an open recorded blocker, or without activity for `board.staleAfter`; a sub-issue offered through an epic passes the same checks, and a serve record, task or note naming the epic covers it (`…, on epic owner/repo#n`). `--claim` records the pick as the calling session's serve under the state lock, so two concurrent claims never get the same item; it changes nothing on the board, the caller moves the item with `board move` once it judged it, and ends with the session, `agents idle` or `sessions unserve`. A second `--claim` while the session serves an open issue or PR is refused (exit 3) with its record, unchanged; `--claim --replace` takes the next item and replaces the record. `move <issue> <status>` sets an item's Status by the board's canonical name or an unambiguous part ("up next"), and refuses anything else with the board's values. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies from its start until a deliberate `supervisor stop` (which ends supervision and starts no successor) or a successor's `supervisor start`: a supervisor whose CLI crashed keeps it in force, its grants and queue stay recorded, and every claim waits for the successor. A CLI back under the same session id or desktop record within `supervisor.restartGrace` (30s) of beekeeper first seeing it gone (a claim or a watch's poll) is a restart and keeps the role and an open relay without a new start; `supervisor status` and `status` show it restarting, then gone. Another session's start is refused while it runs or restarts, unless the supervisor relayed the role to it; past the grace a successor's start takes the role. The start prints the holder's first look in a few KB: leases, holds, lanes, what waits on it (pinned and open notes, the guide's as a count, the decisions answered since the last relay, timers) and the agents with their tasks. A start that changes the holder runs `agents broadcast` in a transient unit, so every running agent learns the name that answers now. |
| `beekeeper supervisor reopen` | Open the recorded supervisor's desktop session, starting the app if needed (the login unit, see [Fresh successors](#fresh-successors-and-a-supervisor-started-again-without-a-click)). |
| `beekeeper supervisor relay [--cancel]` | Hand the role over without a gap: beekeeper starts the next run, "Supervisor run N+1", as a fresh session (as `agents start` does, so its desktop title, roster name and messaging name are one) and opens the relay to it; its first turn's `supervisor start` takes the role, the grant queue and the pending grants in one step, and the event log shows both. Until then the outgoing supervisor keeps the role and the grant rule; a relay not taken expires after `supervisor.relayTTL` (15m) or is withdrawn with `--cancel`, and a successor that does not start withdraws it. `supervisor status` in the relieved session exits 4, also after the successor relays onward, cancels a relay or is relieved in turn, until that session supervises again (or for 7 days). Once the successor took the role, the doctor owes the relieved run's desktop row the archive and does it once that run's CLI runs no turn, which frees the desktop CLI slot it held (`guide relay` likewise); the successor's `start` line says so. |
| `beekeeper guide start\|stop\|status\|relay` | The guide: the session that walks the person through the decisions waiting on them, next to the supervisor and never in its session (`guide start` in the supervisor's session and `supervisor start` in the guide's are refused, and neither relays to the other's holder). It has no grant power. Its role moves like the supervisor's (a relay to a fresh "Guide run N+1", its start, relief with exit 4, restart grace, a fresh successor after a crash) without touching the supervisor's record; `guide handover --prompt` is the successor's prompt (`guide.skill`, default `guide`, or `guide.instructions`, then its queue and an open relay). |
| `beekeeper guide next` | The one decision the guide asks now: the open note for its person due first, marked as being asked (`guide.asking`), served again until `note answer` or `note done` closes it; only then the next. Only the guide's session calls it; `--json` the item. |
| `beekeeper guide queue` | The guide's queue: every open decision filed for its person, `guide.person` (`note add --for`, or an older note's `[for <person>]` text prefix; any case; a memo or a pinned note is none), with its owning session (the one that filed it, and whether it still runs), deadline and default, then every session whose record says it waits on the person (`sessions serve … --waits "<person>: <ask>"`, the person in any case; with `guide.person` unset, every `--waits`), while its CLI runs or stopped (`(stopped)`) less than `guide.waitingTTL` ago (default 2h; 0: never folds), except the guide's own session, an archived one, a test (titled `test: …`) and an agent that reported its task after the wait (`agents idle`, `--done`, or taken off the roster). The sessions stopped longer ago fold into one line, `<n> stopped session(s) waited on <person>`; `--full` lists them. `guide watch` says nothing for a session that only aged out. With `guide.person` unset, every note filed `--for` anyone, and a line that says so. Delta output like `sessions`. |
| `beekeeper guide watch [--once]` | The guide's feed, silent otherwise: `GUIDE DECISION` for each new open decision of the queue (only `guide.person`'s; with it unset, every `--for` note and one `GUIDE:` line that says so), `GUIDE WAITING` for each session of the queue newly waiting on its person (only an explicit `--waits`: the desktop's turn summary does not count), `GUIDE ANSWERED`, `GUIDE DEFAULTED` or `GUIDE CLOSED` for a note of the queue closed (`GUIDE CLOSED #<id> overtaken: <reason>` for an overtaken one, `GUIDE REPLACED: #<id> replaced by #<new>: <text>` for one a successor replaced), `GUIDE ORPHANED #<id> for <person>, its filing session "<name>" is archived; ask it, or close it with note done <id> --overtaken: <text>` for an open decision without `--ref` whose filing worker session is archived (`note add --ref`), and the guide's relay: `GUIDE RELAY DUE` once its context reaches `guide.relayAt` (150k), `GUIDE RELAY TAKEN`, `GUIDE RELAY EXPIRED`, `GUIDE RESTARTED`. Each is one line, once: what it said is kept in the state (`guide.fed`). |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. `register` names the session by its title (a `claude --bg` worker by its `-n` name) unless `--name` overrides it. A name is one agent's: registering under the name of a session that no longer runs (what `agents` shows as `not running`: stopped, closed or asleep) replaces its entry, saying so; a task the replaced entry left unfinished becomes the new entry's, with its assignment time, named in the `agents.register` event (`replaces <id>, takes over "<task>"`) and in the output, so the fresh session works it and a later `assign` to the name is refused as busy until `idle`. Re-registering keeps the session's own open task, so a session `agents start` registered busy stays busy when it registers itself (`register: <name> busy with "<task>"`); two dropped entries with open tasks are refused (exit 3). A name a running session's entry holds is refused (exit 3). `assign` and `remove` take a session id, a name or a unique part of one, and refuse a name several entries share. |
| `beekeeper agents start <name> <brief file> [--task t] [--model m] [--dir d] [--harness omp]` | Starts an agent session without a click: `claude -p` in `bypassPermissions` under a session id beekeeper chooses, the brief as its first prompt, in a transient user unit `beekeeper-agent-<id>` (`KillMode=process`, the user manager's environment). Before the session exists it records the id and mode as one of beekeeper's starts (the state's `starts`, kept 30 days) and registers it on the roster under the name, busy from its start with `--task` (by default the brief's first line) or with the open task of a stopped entry under that name. Once the transcript holds the first reply it imports the session into Claude Desktop (`claude://resume?session=<id>`, sidebar row `local_<id>` titled with the name: beekeeper appends the name's `custom-title` line to the transcript first, since the import reads only its last 256 KiB) and switches the desktop back to the session it showed before (`claude://code/continue`), the one the desktop log (`claude.desktopLog`) last names as focused. See [Agents started without a click](#agents-started-without-a-click). `--harness omp` starts an omp agent instead, see [omp agents](#omp-agents). |
| `beekeeper agents desktop [agent]` | Asks for an agent's desktop turn (the browser): its import or reopen shows it in the desktop without waiting for the window's focus, the person's typing holding it 1 minute at most; an agent with neither a CLI nor a waiting reopen is shown at once. By default the calling agent. See [Agents started without a click](#agents-started-without-a-click). |
| `beekeeper agents wake <agent> <message> [--permission-mode m]` | Messages a registered agent without Claude Desktop's `local_` route and its cap (a session's desktop sends pause after ten since its person last typed in it). A running CLI gets the message by name; a session with none is resumed headless, `claude -p --resume <id>` with the message as its turn, in its directory, permission mode (a start's bypass, else the desktop record's) and recorded model, in a transient unit `beekeeper-wake-<id>-<wake>`, one per wake; its `ExecStopPost` runs `agents reopen <local_ id>`, which warms the desktop's CLI again. `agents` shows `live, first turn running` or `live, wake turn running` while a headless turn is the session's CLI. See [Waking a session](#waking-a-session). |
| `beekeeper agents handover <agent> [--prompt] [--model m] [--dir d]` | Hands a registered agent over to a fresh session near its context limit, one line per step: asks it by peer message, or in a headless turn resumed from its transcript when its CLI does not run or take the message, for `beekeeper agents note "<what is in flight, what is next>"` (waiting `agents.noteWait` at most), builds the follow-up's prompt, starts the follow-up as `agents start` does under the agent's name (it takes over the roster entry, the task and the session record), stops the old session's CLI and the processes under it by PID (a `claude --bg` session through `claude stop` first, so its daemon does not resume it), archives the old session's desktop row once its CLI has exited, and logs `agents.handover`. `--prompt` prints the prompt only. `watch` says `HANDOVER DUE` once per agent session at `agents.relayAt`. See [Agents handed over near their context limit](#agents-handed-over-near-their-context-limit). |
| `beekeeper agents broadcast <message>` | Sends one message by name to every registered agent whose CLI runs, one after the other, the supervisor and the caller left out; a stopped agent is not resumed for it. One `agents.broadcast` event logs whom it reached. |
| `beekeeper agents note <text>` | The calling agent's hand-over note, logged as an `agents.note` event; the next `agents handover` puts the latest one into the follow-up's prompt. |
| `beekeeper note add\|answer\|done\|pin\|unpin` | Open items that outlive a session, of two kinds (`--kind`). A decision waits on a person with its deadline and what happens if nobody answers (`note add --for Ada --due 22:55 --status-quo "<what is true now>" --why "<why it needs Ada>" --default "the alert stays as is" <text>`); only open decisions reach the guide (`guide queue`, `guide watch`). A memo is a session's own record (a state summary, a board skip, a list of deferred work) that asks nobody; `note list` and the hand-over list it apart from the decisions. `--kind` defaults to `decision` for `guide.person` (any `--for` with it unset) and to `memo` otherwise; a note filed before kinds is classified the same way, a pinned one as a memo. A decision names who decides (`--for`) and is refused with exit 2 without `--due` and a `--default` that is an action. `--replaces <id>` closes the named open note in the same step (`note.replaced`) and carries its pin over, so a state memo is one open note at a time. A decision for `guide.person` (any `--for` with it unset) is refused, naming what it lacks, without `--status-quo`, `--why`, a `--default` that is an action (not `wait` or `none`), every `--option` as `<choice>: <consequence>`, the full URL of every `#N` or `owner/repo#N`, and `--checked "<source>"` for a claim that something is merged, green, released, rolled or closed, and while a pull request it links in a plans repository (`plans.repositories`) is open with its stage check (`plans.check`, `plan-stages`) red, pending or missing, naming what the check found; one on an issue or PR an open note for the same person names, with the same verb (the text's first word), folds into it (`note.folded`). `--kind login --until "<probe>"`: the watch runs the probe each tick and closes the note once it exits 0. A memo is not checked. A note for a person asking again what was answered for them within 72 hours (the same verb on one of the same issues or PRs) is filed with a warning that quotes the answer. `--pin` (or `note pin <id>`) makes a note a standing instruction: every hand-over, the supervisor's and the guide's, carries it until `note unpin` or `note done`. `watch` reports a note once when it is due; a decision due unanswered closes with its default (`NOTE DEFAULTED #<id>: <default>`, `note.defaulted`), and the session that filed it is told. A decision renders as one Slack message (`beekeeper serve --help`): it is refused with exit 2 without `--status-quo`, with a question over one line or 150 characters, a status quo over 3000, more than 10 `--option` or a label over 75 characters, or a `--recommend <n>` (1-based) that names none of them; `--for team:<name>` puts it to a team. `--ref owner/repo#n` (repeatable) links a note to the issues or PRs it asks about: once every one is closed or merged, `watch` closes it as overtaken (`NOTE OVERTAKEN`, `note.overtaken` with the reason; `watch --once` names it and writes nothing); a note without `--ref` is never closed by the watch: once the session that filed it is archived (a stopped session is not), `guide watch` names it to the guide as orphaned, to ask or close by hand (a role's run, current or relieved, never orphans its notes). Pinned and login notes, and the note the guide asks now, are never overtaken. `note done <id> --overtaken "<why>"` closes one so by hand. `note answer <id> [--choice <n>] [<answer>]` records the person's answer word for word (the option's label, their own words, or both) and how it came (`--via cli`, the default, or `slack`), closes the note and tells the session that filed it (`NOTE ANSWERED #<id> by <person>: <answer>`, which `watch` prints too); the `note.answered` event carries it for the owning session, the supervisor and the guide's feed. |
| `beekeeper reporter final\|pause\|resume` | Pauses the scheduled reporter: `final 06:45` (or `45m`) starts one last report at that time, covering the time since the last one, then pauses; `pause` pauses now; `resume` starts the current slot's report at the standby watch's next poll and the schedule again. |
| `beekeeper report [--since 1h\|15:04] [--until 15:04] [--tz zone]` | Renders the status report from beekeeper's own state, as Markdown that passes `reporter check`: the pull requests and promotions the gate merged in the window with title, release and rollout; the running work (roles, busy agents, sessions serving an issue) with the issue, what it waits on and its leases; what waits on `reporter.person` (open notes per owning session, sessions waiting on an answer, the open drafts of `reporter.reviews`); the merge lanes, timers and holds; the machine. Every time in `reporter.tz` (`--tz`; default the machine's zone); one GitHub request for the titles and drafts. See [The scheduled status reporter](#the-scheduled-status-reporter). |
| `beekeeper reporter check` | Checks a report on stdin as the reporter's post hook does (see [The scheduled status reporter](#the-scheduled-status-reporter)): one line per problem and exit 3, or `ok`. |
| `beekeeper reporter` | The scheduled status reporter (see [The scheduled status reporter](#the-scheduled-status-reporter)): its schedule, when the next one starts, and the current or last run with its outcome. |
| `beekeeper timer add\|done\|list` | Times to look at something: `timer add 22:55 "check the rollout"` (or a duration, `45m`). `watch` prints one line when a timer is due; it stays open until `timer done`. A timer can wait on a condition from its time instead: `--when "pr-merged owner/repo#n"` (also `issue-closed owner/repo#n`, `helmrelease-ready context/namespace/name`, `controlplane-ready context/namespace/name`) or `--probe "<command>"` (exit 0: it holds), checked by the watch every `--every` (5m), each distinct check once per tick however many timers share it and a GitHub one only above the budget floor; it fires within one tick of the condition holding. `--until` ends the wait: the timer then fires as timed out, or with `--expire` closes unfired (`TIMER EXPIRED`). `--wake <agent>` wakes the agent with the timer's text (as `agents wake`; `TIMER WAKE FAILED` when it cannot), `--run "<command>"` runs a command, its output in `timers/<id>.log` of the state directory and its exit code logged (`timer.ran`, `TIMER RAN`); without either the `TIMER` line is for the supervisor. Such a timer closes when it fires. |
| `beekeeper ps [name\|pid...]` | The process table with every command line masked: PID, parent, age, CPU time, resident memory, and the command line as the watch's `STACKED` line prints it (the program, its subcommands and flag names; flag values, `key=value` words, URLs and what follows a short option's letter left out). A masked line ends in `(masked)`, so a left-out value is never read as an empty one. A number narrows the list to a PID, a word to the processes whose masked line or name holds it. `--json` has the rows. The hook's refusal of a whole command-line read names it ([Secret reads](#secret-reads)). |
| `beekeeper sessions serve\|unserve` | A record for any session, registered agent or not: `sessions serve <session> <owner/repo#n> [--waits "<what>"]`, the issue or epic it serves and what it waits on; `sessions unserve <session|owner/repo#n>` removes it, by the issue for the one record serving it. `sessions` and `handover` show it; `watch` prints one line when the session ends, naming the issue to re-query. |
| `beekeeper handover [--prompt] [--section <name>]` | Everything the next supervisor needs, as Markdown, from the live state: leases and grants, holds and their exceptions, agents, session records, pinned notes, notes with their defaults, the decisions answered since the last relay, timers, the merge lanes with their queues and settling merges, and what the alert watch reads (the installations and why, the ignored alert names, the baseline). `--prompt` prints the successor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope, the pending state and the commands that read the live values; no live value (version, memory figure, pull request state). Both show what a successor acts on in its first minutes: the notes the guide serves (for `guide.person` and for the guide) are one count line, records of sessions ended over an hour ago one line, and the answered decisions those since the predecessor's start with a count of the rest of 72 hours. `--section <name>` prints one section with everything it holds (`notes`, `records`, `answers`, …). |
| `beekeeper log [--verb PREFIX]` | Every claim, grant, hold, registration, note, timer, session record and build run, as they happened; `--verb run.` shows only the runs. |
| `beekeeper run [--max SIZE] [--wait DURATION] -- <command>` | Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the `memcap` wrapper) inside a memory-capped systemd scope. A run of a session that holds a slot already joins it at once (its scope in the slot's slice, sharing the slot's cap) rather than taking a second one. When every slot is held it waits once, then exits 75 with the holders; when the cap fires the kernel kills the biggest process in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap killed:`. Each run leaves `run.start` and `run.end` in `beekeeper log` with its scope, session and command, so `snapshot` and `watch` name a cap kill's session and command after the run has ended. `MEMCAP_TEST=1` marks a test's run: its scope is `memcap-test-…`, and a kill in it is reported as a test kill (one quiet `test kill:` line in `watch`), never as a build's. |
| `beekeeper hook pretooluse` | The PreToolUse hook (matcher `Bash|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|mcp__.*`): rewrites build, test, lint and lab commands to `<this binary> run -- zsh -c '<command>'` with the command verbatim and the tool timeout at 10 minutes (a background run waits 60 minutes), and refuses a third kind cluster, listing the held leases. It puts the gate in front of every `devctl pr merge`, `pr wait`, `release wait` and `rollout wait`, behind prefix commands and in pipelines and lists, and refuses one hidden in a `-c` string (below); every other devctl command passes untouched. It refuses a delete that reaches further than it names ([Deletes](#deletes)), a command that would print secret values (below, [Secret reads](#secret-reads)) and a call that would send one off the machine ([What leaves the machine](#what-leaves-the-machine)), a kube context switch and a write to production ([Kube contexts and production writes](#kube-contexts-and-production-writes)), a command that opens a page in the person's browser (`muster auth login`, `gh auth login --web`, `xdg-open`) outside the session holding the `browser` lease, a command that loads a model on the host's model server outside the session holding `model-server` ([The model server](#the-model-server)), an `AskUserQuestion` call outside the guide's session ([Questions go to the guide](#questions-go-to-the-guide)), and in the guide's session the calls that do work and a question without its status quo, why and full links ([The guide asks and relays](#the-guide-asks-and-relays-it-never-works-itself)). A `SendMessage` to `the supervisor` or `the guide` goes to the session holding that role now, by the name its running CLI answers to (else its desktop session), so a brief names the role and a relay never makes it stale. A `SendMessage` to a desktop id (`local_…`) whose session has a running CLI goes to that CLI by name, so a headless turn gets no second copy beside it (below, [Waking a session](#waking-a-session)). A session's first write or `git commit` in another repository carries that repository's `CLAUDE.md`, `AGENTS.md`, rules and mandatory reads ([A repository's instructions on the first write](#a-repositorys-instructions-on-the-first-write)). |
| `beekeeper hook posttooluse` | The PostToolUse hook (matcher `*`): replaces a tool result that carries an indexed secret value or a token pattern with its redacted copy before the model sees it, logs `scan.redact` and files one rotation note per indexed reference (see [What reaches the model](#what-reaches-the-model)). |
| `beekeeper secret compare\|fingerprint\|copy\|set\|rotate` | The credential operations no agent runs itself: equality, keyed fingerprints, a SOPS file copied under a new name and namespace, a value into a SOPS path or a consumer's stdin, a generated value into the shared vault and a SOPS path, a rotation into every SOPS path that carried the old value. They answer key names, lengths, equality and fingerprints, never a value (see [Secret operations](#secret-operations)). |
| `beekeeper sandbox render\|install` | The agent sandbox: the policy Claude Code enforces on every session's commands, rendered as a managed-settings drop-in, and the root command that installs it (see [The agent sandbox](#the-agent-sandbox)). |
| `beekeeper scan [index\|add <ref>\|sweep]` | The transcript value scanner: without a subcommand what the fingerprint index holds; `index` rebuilds it from `scan.sops` and `scan.vaults`, `add` indexes one value from stdin, `sweep` counts each reference and token rule in every transcript, never a value (see [What reaches the model](#what-reaches-the-model)). |
| `beekeeper hook sessionstart` | The SessionStart hook: writes the agent shell's prelude into the session's environment file (`$CLAUDE_ENV_FILE`), which Claude Code sources before parsing each Bash command. It removes the aliases and shell functions of `agents.shell.unalias` (default `grep`, `find`, `ls`, `cp`, `mv`, `rm`, the harness's own `grep` and `find` shadows among them), so each name runs the tool on `PATH`, and with `agents.shell.globs: literal` (the default) an unmatched glob stays as written instead of failing the command with zsh's `no matches found`. The directories of `agents.shell.path` go first on `PATH`, in their order and once each: the agent's own programs, such as a `gh` link to devctl that acts with the devctl App's short-lived token in place of the person's long-lived `gh` login. The person's interactive setup stays theirs; an agent writes its commands for the plain tools. |
| `beekeeper lint briefs <file or folder>...` | Refuses dated lines, "until X ships" clauses, notes on the release that fixed something, workarounds and role run numbers in skills and briefs (every Markdown file below a folder), one `path:line: rule: why` per finding, exit 3 on any. |
| `beekeeper hook permissionrequest` | The PermissionRequest hook: answers `allow` only for a session `agents start` started in bypass that now runs in `acceptEdits`; every other request gets no answer, so the person sees the normal card. Below. |
| `beekeeper free [--apply] [--only SECTIONS] [--summary]` | Show where the memory is and, with `--apply`, free what no running work needs: dead sessions' dirs in the tmpfs `/tmp` (the CLI is gone; an idle session's stay), throwaway temp dirs and orphaned jest or Claude workers. Kind clusters, idle CLIs, heavy or runaway processes and Chrome renderers are only reported. Each Claude CLI is listed with its session's title and id, its roster state (busy, parked, idle), its role (supervisor, guide, relay spare) and the hours since its transcript changed; one whose session is untouched for `--cli-stale-hours` (12) with no role and not busy or parked is marked stale with the memory its exit returns, and stays running. Never runs as root: the swap reset and root-owned leftovers are printed as the commands to run. `--summary` prints the TSV rows a desktop front end parses. |
| `beekeeper self-update` | Install the latest signed release over this binary; `--check` only asks. |

Every reading command is written for an agent whose context is its scarcest resource: it prints only what the caller has not read. `snapshot`, `sessions`, `lanes`, `agents` and `handover` keep a read mark per caller and command (`seen.<command>.<caller>.json` in the state directory; `snapshot.<caller>.json` for `snapshot`): a caller that has read before gets the facts that are new or changed since, the keys of those gone (`gone: …`), or one `no change since HH:MM` line. A moving figure (an age, a countdown, a memory size, a context in tokens) is no change. `--full` prints everything, `--json` is always complete, `handover --prompt` is always whole (a successor has read nothing), and a person at a terminal (no Claude session) always gets the whole text. `watch` says each event once and each lasting condition when it starts and when it ends, across restarts: it keeps its open conditions and one-time events (runaways, stale leases) in the caller's mark (`seen.watch.<caller>.json`), so a watch restarted after an install resumes silently and says only an `ENDED` or what is new; `watch --once` keeps no mark and says every condition it finds. Its lines carry no restated thresholds or explanations, those stay in `--help` and here.

Marks are per host session (`CLAUDE_CODE_HOST_SESSION_ID`, else the session): a subagent or `claude -p` worker inside a session shares its parent's marks, so its reads advance what the parent will see as read. A subagent reading beekeeper passes `--full` or `--json`.

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor), 4
relieved (`supervisor status` in the session a relay relieved), 69 the central instance is
unreachable (a central verb, see [The central instance](#the-central-instance)), 125 a newer
release is out (`self-update --check`). A claim gates the action it guards:
`beekeeper lease claim staging -p "database migration" && kubectl …`, never a `;` between them.

`--json` prints any command's result as JSON. A session is identified by the environment
Claude Code gives its tool commands, and named by its desktop title, the environment's name or
the name its CLI's record holds (a `claude --bg` worker's `-n` title); a person or a script
passes `--as <name>`.

Every session is a Claude Code CLI of its own: a desktop session, a `claude --bg` worker (its
daemon and terminal hosts are no session) or a headless `claude -p`. A session is listed under
the id and name its CLI's record names, the session the process runs now: a `--bg` worker the
daemon started or woke in its pre-started spare CLI, whose command line names no session, and a
resumed CLI that went on under a new id are listed as what they run; a spare no session has
claimed is not listed. A CLI that a session started, from its tool shell or through
`systemd-run`, is listed under its own id and name, `started by` that session, never as that
session restarting; a `claude -p` its tool shell runs without an id of its own is one of that
session's commands.

## The model server

The host's model servers (ollama, and Lemonade Server where `lemonade.url` is set) keep a
loaded model in the iGPU's memory: system RAM the GPU driver pins, which no cgroup counts
or caps. One lab request that names a large model
can take the desktop into systemd-oomd. So the model server is a resource like a lab:
a session that loads a model on it holds the `model-server` lease, and the claim says how
much it may load.

```sh
beekeeper lease claim model-server -p "models-test for model-manager#219" --gib 10 && agentlab models-test
beekeeper lease release model-server
```

- The budget defaults to `ollama.budgetGiB` (12); a claim above `ollama.maxBudgetGiB` (24) is refused.
- `beekeeper watch` compares ollama's loaded models (`/api/ps`) with the lease on every
  sample. Ollama's journal names the client of each load once its request ends; while it
  still runs, the one client connected to the model server's port is its client (none when
  several are connected). A kind node's address maps to
  its lab (the cluster's name, or `<name>-1` for the first of a numbered series) and to
  the lab's holder. A model loaded while nobody holds `model-server`, loaded by a lab
  another session holds, or beyond the holder's budget (the largest first) is a
  `MODEL SERVER` line. The watch unloads it (`keep_alive: 0`; the weights stay on disk),
  unless `ollama.nameOnly` is set. A load from the host itself without a lease is only
  named: a person may be using it.
- Lemonade's loaded models (`/api/v1/health`, sized from `/api/v1/models`) count against
  the same lease and budget as ollama's, and the watch unloads them through
  `/api/v1/unload`. Lemonade logs no client address: a model's client is the one client
  connected to its port, none when several are. `snapshot` and the `IGPU GTT` line list
  every server's models under its name; one server not answering hides none of the
  other's.
- The PreToolUse hook refuses, outside the session holding `model-server`,
  `ollama run|pull|create|push|cp`, `lemonade run|launch|chat|load|pull|bench`, an HTTP
  request to a model server's port on a generate, chat, embed, pull or `/v1/` path (for
  Lemonade `/api/v1/` chat, completions, responses, embeddings, reranking, load, pull,
  audio and images), and the agentlab tests in `ollama.labTests`. `ollama ps`,
  `ollama stop`, `/api/ps`, `lemonade list|unload` and `/api/v1/health` pass.

## Kube contexts and production writes

With `kube.production` set, the machine kubeconfig (`~/.kube/config`) keeps no current context: a
`kubectl`, `helm` or `flux` command without an explicit target fails instead of reaching production.
The PreToolUse hook keeps it that way and refuses, with the reason and the explicit form (unset, the
kube guard is off and `beekeeper snapshot` says so):

- A context switch: `kubectl config use-context`, `kubectl config set current-context`, `kubectl ctx`,
  `kubectx <name>`, `kubectl gs login` without `--self-contained`; and `tsh kube login`, `tsh login
  --kube-cluster`, `kind create cluster` and `kind export kubeconfig` when they write the machine
  kubeconfig (a `KUBECONFIG` or `--kubeconfig` of the command's own passes). `beekeeper snapshot` names a
  current context the machine kubeconfig gained anyway.
- A write to production, the clusters of the installation `kube.production` (a context or cluster
  name with it as a component, its workload clusters included): `kubectl`
  apply/create/patch/edit/delete/replace/scale/annotate/label/cordon/uncordon/drain/taint/set/expose/
  autoscale/run/exec/cp/attach/debug, `rollout restart|undo|pause|resume`; `helm`
  install/upgrade/uninstall/rollback/test; `flux` suspend/resume/reconcile/create/delete/bootstrap/
  install/uninstall. The target is the `--context`/`--kube-context`/`--cluster` given, else the current
  context of the kubeconfig the command uses (`--kubeconfig`, `KUBECONFIG` in the command or the
  session). Changes to production go through GitOps pull requests and platformctl; there is no
  break-glass. Reads (`get`, `describe`, `logs`, `top`, `auth can-i`, `diff`, `--dry-run=server`;
  `helm list|status|get|template`; `flux get|logs`) and writes to any other cluster pass.
- A kubectl plugin's write to production: `kubectl <plugin> …` (a first word that is no kubectl
  command) and a `kubectl-<plugin>` binary (a path or `go run ./cmd/kubectl-<plugin>` included), with
  the same targets as kubectl. `kubectl ate delete actor …`, `kubectl-ate admin make-ca-pool …` and
  `kubectl gs update app …` are refused; a plugin subcommand that reads (`get`, `list`, `describe`,
  `logs`, `top`, `status`, `version`, `template`, …) and the read-only plugins (`tree`,
  `access-matrix`, `resource-capacity`, `who-can`, `neat`, `krew`, `oidc-login`, `ns`) pass. Every other
  plugin subcommand counts as a write.

Each guard sees through prefix commands, pipelines and lists, `$( )`, shell `-c` strings and
here-documents fed to a shell, and kubectl or a kubectl plugin run through a shell function or a variable. Quoted text
(a commit message, an issue body) passes.

## The Teleport login

When the kube contexts reach the installations through Teleport (`tsh kube credentials` as their exec
plugin), all of them hang on one `tsh` login, and an SSO login's certificate lives only as long as
the role's maximum session TTL. With `teleport.proxy` set (and `teleport.auth`, the SSO connector),
beekeeper keeps that login:

- `beekeeper teleport` prints the active profile's user, cluster and expiry from `tsh status
  --format=json` (metadata only, never key material) and what the keeper last did; it exits 3 while
  the login needs a person (expired, under `teleport.warnBefore`, or the keeper's renewal failed).
  `beekeeper snapshot` shows the same line, and `beekeeper watch` says `TELEPORT LOGIN` once under
  `teleport.warnBefore` (default 1h), `TELEPORT LOGIN EXPIRED` once it expired and `TELEPORT RENEWAL
  FAILED` after a failed renewal, each with one ENDED line once renewed. A merge gate refusal for an
  unreadable installation names an expired login as its cause.
- `beekeeper install` writes the keeper, `beekeeper-teleport.timer`, which runs `beekeeper teleport
  renew --keeper` every `teleport.every` (default 10m); it renews once less than
  `teleport.renewBefore` (default 90m) is left. `uninstall` stops and removes it with the other units.
- A renewal runs `tsh login --proxy=<proxy> --auth=<connector>` in an empty staging home, because a
  valid profile makes `tsh login` print its status only. tsh opens the login URL in the default
  browser, and a browser holding the SSO session completes it without a click; nothing types
  credentials. Once the new profile is valid, the staging home and the profile directory
  (`teleport.home`, default `$TELEPORT_HOME` or `~/.tsh`) swap in one step, so the old certificate
  serves until the new one is in place; the profile before is kept as `teleport/previous` in the
  state directory, and the person's kubeconfig is not touched. tsh's output carries the one-time
  login URL: it goes to `teleport/login.log` (mode 0600) and is never printed.
- The renewal holds the browser lease. A session renewing by hand (`beekeeper teleport renew`) claims
  it first; the keeper claims it itself when it is free and granted to nobody, and otherwise waits
  for its next run, which the `TELEPORT LOGIN` line names.
- A login that does not complete within `teleport.loginTimeout` (default 3m: the browser's SSO
  session is gone, or the sign-in needs a click) leaves the profile as it was. The keeper does not
  try that login again: it leaves a sign-in note for `guide.person` that closes by itself once the
  login is renewed, and the watch says `TELEPORT RENEWAL FAILED`.
- A login whose SSO callback exchange with the proxy timed out after the browser's sign-in
  (`identity provider callback failed` … `Client.Timeout exceeded while awaiting headers`) is
  retried once, in the same staging home, before the renewal fails: the first attempt is logged as
  `teleport.retry`, and the second appends its output to `teleport/login.log`.

What the keeper needs: a graphical session whose default browser holds the SSO session (the keeper's
unit starts after `graphical-session.target` and inherits the user manager's display variables), and
the profile directory on the same filesystem as the state directory. A headless desk needs a
Machine ID bot (`tbot`) from the Teleport administrators instead; beekeeper does not run one.

## Deletes

A delete under a variable that turns out empty removes what it never named: after a failed
`cd … && D=$(mktemp -d)`, the cleanup `shred -u "$D"/*` runs as `shred -u /*`. The PreToolUse hook
refuses an `rm`, `shred`, `unlink`, `truncate` or `find` with `-delete` or `-exec rm` (also behind
`sudo`, `xargs` and the other wrappers, in a shell's `-c` string, a here-document a shell reads and a
command substitution) whose target is:

- a path after a variable or command substitution that may be empty: `"$D"/*`, `$D/*`, `$DIR/`,
  `"${X}/…"`, `"$1"/x`, `"$(…)"/build`, unless the variable is guarded (`${D:?}`, `${D:-/default}`)
  or `set -u` (`set -euo pipefail`, `set -o nounset`) comes before it in the command;
- the root, a top-level directory, the home directory, or every entry of one of them, as written or
  after a literal assignment in the command: `/`, `/*`, `/home`, `/usr/`, `/tmp/*`, `/tmp/"$D"`, `~`,
  `~/*`, `"$HOME"`.

The refusal names the command and the target, what the target becomes, and the safe forms: guard the
variable (`rm -rf "${D:?}"/*`), run the cleanup from a `set -euo pipefail` script with an `EXIT` trap,
or move the files aside instead of deleting them. `rm -rf "$D"` alone, `$HOME` and `$PWD` paths, `git
clean`, `git rm`, `klausctl stop|delete` and beekeeper's own commands pass.

## Secret reads

A value a command prints lands in the session's transcript and goes to the model API with the next
turn; a value written to a file, a variable or the clipboard, hashed or diffed, is one any later
command can read or brute-force. One exposed credential is one rotation. For credential tools the
PreToolUse hook is an allow list: it refuses every command that touches a secret except the forms
below, names the part it refused and gives the safe forms. Every refusal names `beekeeper secret`:
`compare` or `fingerprint` for equality, `copy` or `set` for changes, `rotate` to replace a value
everywhere it is carried ([Secret operations](#secret-operations)).

Refused in every form, since beekeeper is the only process that reads, creates, rotates, encrypts and
decrypts secrets:

- `sops` (decrypting and encrypting alike, `helm secrets` included) and `op` (`op read`, `op item`,
  `op whoami`, `op run --no-masking`, …), also behind `sudo`, `env`, `timeout`, `xargs` and
  `beekeeper run`; `age -d` and `gpg --decrypt`. The one exception until `beekeeper secret rotate` ships:
  `op run -- <command>`, whose output op masks; the command it runs is checked as a command of its
  own (`op run -- sops -d x` and `op run -- kubectl get secret x -o yaml` are refused).
- `kubectl edit` of a Secret and `kubectl view-secret`.
- A hash (`sha*sum`, `md5sum`, `b2sum`, `cksum`, `openssl dgst`) or a diff (`diff`, `cmp`, `git diff
  --no-index`, …) of a file whose name says it holds secrets in plaintext (`secrets.yaml`,
  `token.txt`, a kubeconfig; code files and SOPS-encrypted `*.enc.*` or `*.sops.*` files pass).
- `vault` beyond `status`, `version`, `list`, `kv list`, `kv metadata get` and the `kv get`/`read`
  forms below.

Refused unless their output ends in an allowed form:

- `kubectl get` of Secrets (by kind, `secret/<name>`, in a list such as `cm,secret`, by `--raw` path)
  with `-o yaml|json` or a `jsonpath`, `go-template` or `custom-columns` template that names more than
  the kind, the type and the metadata other than annotations (which carry the last applied object);
  `kubectl create|apply|replace|patch secret … -o yaml|json`; also through a shell function or variable
  that runs kubectl (`kc(){ kubectl --context x "$@"; }`, `K="kubectl …"`), and `kubectl get -o yaml` of
  names piped in from a Secret listing.
- `vault kv get` and `vault read`; `base64 -d` of a secret's `.data`.
- A render fed with secret values: `helm template|install|upgrade` with a values file (`-f`,
  `--values`, `--set-file`) whose name says it holds secrets, `kustomize build` or `kubectl kustomize`
  with `--enable-alpha-plugins` or `--enable-exec` (which run decrypting plugins).
- Whole command lines and environments, readable by every process: a program started with
  `-e PASSWORD=…`, `--token …` or `-p …` carries the credential there. `ps` with its `args`/`cmd`/`command`
  column (`ps aux`, `ps -ef`, `ps -eo pid,args`, BSD-style words without `c`, BSD `e`), `pgrep -a`,
  `pstree -a`, a read of `/proc/<pid>/cmdline` or `environ` (`ls`, `stat` and `wc` pass), `docker|podman
  inspect` without a `--format` limited to names and state, and `docker ps --no-trunc` with the
  command column. The safe forms: `beekeeper ps` (below), `ps -eo pid,ppid,etime,time,rss,comm`,
  `pgrep -l`, `pgrep -c`, `docker inspect --format '{{.Name}} {{.State.Status}}'`.

The allowed forms: a `jq`/`yq` filter that keeps only keys or metadata (`jq '.data|keys'`,
`.items[].metadata.name`, `(.value|length)`; no `del(…)` or assignment, which print the rest), `wc`,
a consumer that prints nothing of it (`kubectl apply|create|replace -f -` without `-o`,
`--password-stdin`, `gh secret set`, `kubeseal`, `openssl x509 -noout`, `age-keygen -y`), or
`> /dev/null`; for a render also a filter that blanks the Secret data (`yq 'del(.data, .stringData)'`,
`.data |= keys` with `.stringData |= keys`), so a diff of two blanked renders passes. A file, `tee`,
a variable or a flag's value (`T=$(…)`, `--from-literal=k=$(…)`), a hash, `grep -c|-q` and the
clipboard are no allowed end. `-o name`, `-o wide`, the table and `kubectl describe secret` (sizes
only) pass.

The same holds inside `sh|bash|zsh -c`, `ssh`, `eval` and `watch` strings and here-documents fed to a
shell. Quoted text, comments and other here-documents (a commit message, an issue body) are not
commands and pass.

## Secret operations

`beekeeper secret` is how an agent gets a credential job done without reading a value: sops and op
run in beekeeper's process, a value stays in its memory for the one operation and is written nowhere
in plaintext (sops gets the plaintext on stdin, the file is written from its ciphertext), and an
answer is key names, lengths, equality or keyed fingerprints. Every call is a `secret.<operation>`
event in `beekeeper log` with the session, the references and the outcome, never a value.

A reference is a SOPS file (every value in it), one value of a SOPS file (`file#a.b.c`, the dotted key
path; `sops://` in front optional) or a field of the shared 1Password vault
(`op://<vault>/<item>/<field>`). beekeeper reads only the vault `secret.vault` names, and only as its
service account, whose token it reads from `secret.tokenFile` and gives to its own `op` calls alone;
with either unset, an `op://` reference is refused. Every command exits 78 when the vault cannot give a
value: none configured, no token, or `op` failing or answering nothing within a minute. A SOPS file is encrypted under the creation rules
of the `.sops.yaml` nearest above it, run from that directory, so a `path_regex` relative to the
repository matches.

| Command | What it does |
|---|---|
| `compare <a> <b>` | `equal` or `different` for two values, and for two files each key's state (`equal`, `different`, `only in a`, `only in b`); exit 1 when anything is not equal. |
| `fingerprint <ref>` | HMAC-SHA256 of each value under the value scanner's key (`scan/key`), cut to 16 hex digits: equal values, equal fingerprints; only beekeeper can make one. |
| `copy <src.sops.yaml> <dst.sops.yaml> [--name n] [--namespace ns]` | A new SOPS file with src's values, encrypted under dst's rules; `--name` and `--namespace` rewrite a Kubernetes object's metadata. It answers each key and its length (a Secret's `data` decoded); dst must not exist. |
| `copy <ref> <file#path>` | One value into a SOPS path, creating the file or the key when absent, the file's other values kept. |
| `copy <ref> -- <consumer…>` | One value on the stdin of `gh secret set`, a command with `--password-stdin` or one with `--secret <name>=-`; any other consumer is refused. It answers the consumer's exit code and its output with the value redacted. |
| `copy <ref> --to-secret <context>/<namespace>/<name>/<key>` | One value into a key of a Secret in a kind lab (`kind-<cluster>`) whose lab lease (`labs`) the caller holds: a server-side apply under the field manager `beekeeper-secret` that creates the Secret when absent and keeps its other keys, through the admin kubeconfig `kind get kubeconfig` answers, which stays in beekeeper's memory like the value. Any other context, and a lab the caller does not hold, is refused (exit 3). It answers the value's length. |
| `set <file> <path> --generate --vault op://…` | A new value (`--length`, 32; `--charset`, `alnum`, `hex` or `ascii`) written to the vault field first (the item or field created when absent, the item passed as a JSON template on stdin, never on a command line), then into the SOPS path; it answers the fingerprint. |
| `rotate op://… --generate` | A value beekeeper made gets a new one (`--length`, `--charset` as for `set`): the vault field first, then every path of the SOPS files `scan.sops` names that carried the old value, the value itself or its base64 form (a Secret's `data`), matched by fingerprint. |
| `rotate op://…` | A value a third party issues: the person rotates it at its issuer into the vault field, and `rotate` writes the vault's new value into every path that carried the old one, known by the fingerprint `beekeeper scan index` recorded before the change. It refuses while the vault still holds the recorded value. |
| `rotate platform://<installation>/<capability>/<name> --reason <text>` | A credential the platform manager generates: `platformctl installation reconcile <installation> <capability> --commit --rotate <name>` on the host, the manager writing the new value into the installation's SOPS files in a pull request; nothing is decrypted and no copy reaches the vault. `--dry-run` shows the files that hold it. |

`rotate` reads every SOPS file before it writes anything, so one it cannot read stops the rotation with
nothing changed; it answers the new value's fingerprint and the paths it went to, indexes the new value
for the scanner, and closes the open rotation notes of the reference and of each path.

```console
$ beekeeper secret copy team-a/app.sops.yaml team-b/app.sops.yaml --name app-copy --namespace team-b
wrote team-b/app.sops.yaml: 2 keys
  data.token                                                 40 bytes
  stringData.password                                        32 bytes
```

## The agent sandbox

An agent's commands run as the person's Unix user. A deny list of credential paths leaves every path
nobody listed readable, so the agent sandbox turns it around: the home directory is denied and only what
a session needs is re-allowed. `beekeeper sandbox render` prints the policy as Claude Code settings;
`beekeeper sandbox install` stages it and prints the one root command that puts it into Claude Code's
managed settings (`/etc/claude-code/managed-settings.d/beekeeper-sandbox.json` on Linux,
`/Library/Application Support/ClaudeCode/managed-settings.d/` on macOS). From there it holds every
Claude Code session on the machine, the headless turns beekeeper starts and the CLI the desktop app
spawns alike, enforced by Anthropic's sandbox runtime (bubblewrap on Linux, Seatbelt on macOS). The
same file passed to `claude --settings` holds one session to it, to try a change first.

- **Reads:** the home directory is denied; beekeeper's binary, config and state, the harness's
  transcripts, plans, skills and plugins, git's config and `sandbox.allowRead` are re-allowed.
  Kubeconfigs, the Teleport profile, the GitHub CLI's token file and every other credential nobody
  listed stay unreadable. Only the managed settings' read paths count.
- **Writes:** the session's working directory (when it is readable itself), the temporary directory,
  beekeeper's state and `sandbox.allowWrite`.
- **Egress:** GitHub and `sandbox.domains`, nothing else, with no prompt to widen it.
- **The GitHub token:** `GH_TOKEN` and `GITHUB_TOKEN` (`sandbox.mask`) are masked: commands see a
  placeholder, and the sandbox proxy puts the real value into requests to GitHub's hosts only.
- **No way out:** unsandboxed retries are off, and a session whose sandbox cannot start does not start.
- **The file tools.** Read, Grep, Glob, Edit, Write and NotebookEdit run in the harness, outside the
  sandbox. The policy runs `beekeeper hook pretooluse` for them and sets `BEEKEEPER_SANDBOX`, and the
  hook holds them to the same lists in every session, in the hooks' scope or not: a path is resolved
  (symlinks included, a missing one through its nearest parent) and refused outside the lists. The file
  tools may also write the sessions' memory and plans, which no command may.

## What leaves the machine

The same holds for what a session sends out: an issue body, a commit, a chat post, a plan. The
PreToolUse hook scans what a call would send for secret values and refuses the call, naming the rule
or the phrase that matched and never the match, since the refusal lands in the transcript too:

- the command line, here-documents included, of `gh`, `devctl`, `git commit`, `git tag` and
  `git remote`, also inside `sh|bash|zsh -c` strings; the body of `curl`, `wget`, `http` or `xh` (its
  data flags; a header or URL carries the client's own credential to its service and passes);
- the files these send: `--body-file`, `-F`, `--notes-file`, `--input`, `@file`, `field=@file` and
  `$(cat file)`;
- for `git push`, the messages and added lines of the commits no remote has yet (removed lines pass);
- the input of every connector tool (`mcp__…`: Slack, GitHub, mail, documents), which needs
  `|mcp__.*` in the hook's matcher;
- a `Write`, `Edit`, `MultiEdit` or `NotebookEdit` of a file under `outbound.paths` (what it writes,
  not what it replaces).

The patterns are gitleaks' rules by their names: GitHub (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`,
`github_pat_`), GitLab, AWS, GCP, Slack tokens and webhooks, Anthropic, OpenAI, npm, 1Password service
account and age keys, private keys, JWTs and a password in a URL (a placeholder such as `${TOKEN}` or
`<token>` passes); `outbound.phrases` adds what is specific to the machine (case-insensitive, named
by number). A line marked `gitleaks:allow` is skipped, for a test fixture that only looks like a token.

An `op item|document create|edit` or a `vault kv put|patch` that an `outbound.storeDeny` rule matches
(vault and item globs; a write naming no vault may go to the default one and matches) is refused: some
credentials do not belong in a shared store. In an agent session the Secret guard refuses these writes
first ([Secret reads](#secret-reads)).

The watch sweeps `outbound.sweepRoots` (the home directory) `outbound.sweepDepth` (5) levels deep every
interval for world-readable private keys (`id_*`, `*.pem`, `*.key`, `*.ppk` with a private key header)
and git repositories whose remote URL carries a credential, skipping caches and dependency trees: one
`EXPOSED <path>: <what>` line each, never the content, and an ENDED line once it is fixed.

## What reaches the model

The guards above refuse command shapes they know; a shape they do not know still prints. The value
scanner matches the values themselves. beekeeper keeps an index of keyed fingerprints (HMAC-SHA256
under a key only it reads, `scan/key` in the state directory, both files `0600`) of every value it can
reach, each naming the reference to rotate. The index never holds a value. `beekeeper scan index`
fills it from the SOPS files `scan.sops` names (decrypted by `sops -d` in beekeeper's own process) and
the concealed fields of the 1Password vaults `scan.vaults` names (`op item get --reveal`), replacing
what earlier runs indexed from them; `beekeeper scan add <ref>` indexes one value read from stdin.
Each value is indexed as written, in its base64 forms, line by line (and the value of a `key: value`
or `KEY=value` line), and, for base64 text, decoded; values shorter than `scan.minLength` (12) are
not, since they would match ordinary output. `beekeeper scan` prints what the index holds.

The PostToolUse hook, `beekeeper hook posttooluse` (`beekeeper install` registers it for every
tool), runs on each tool result before the model sees it. It splits every string of the result into
candidate strings (the tokens between whitespace, quotes and brackets, and their parts between `=`,
`:`, `@` and `/`), fingerprints the candidates of an indexed length, and runs the outbound guard's
token patterns beside them for values the index does not hold (a line marked `gitleaks:allow` keeps
its pattern matches). With a hit, the hook replaces the result with the same result, each hit
replaced by `[redacted: <reference or rule>]`, and the session goes on: the value never reached the
model, so ending the session would protect nothing more. Claude Code writes the replaced result to
the transcript on disk as well (checked with Claude Code 2.1.286), so the value is in neither. Each
redaction is a `scan.redact` event naming the tool and the references and rules with their counts;
an indexed reference also gets a rotation note for `guide.person`, one open note per reference. It is
still a filter, matching known values in known encodings: a value split across tokens or encoded
otherwise passes.

`beekeeper scan sweep` reads every transcript under `claude.projectsDir` (the sessions' and
subagents' `.jsonl` files, each line's strings decoded, and the spilled tool results in
`tool-results/`) and prints each reference and token rule it finds, with how often and in how many
files, never a value or where it was: the list of what leaked before the hook ran.

## Questions go to the guide

A question put to the person with `AskUserQuestion` blocks the session until the person answers, and
the person talks to one session only: the guide's. The PreToolUse hook refuses `AskUserQuestion` in
every session but the one holding the guide role (`beekeeper guide`, matched by the CLI session id
or the desktop id), and tells the agent to file the question for the guide and carry on:

```
beekeeper note add --for <guide.person> "<the question, every issue or PR as its full URL>" --status-quo "<what is true now>" --why "<why it needs them>" --option "<choice>: <consequence>" --due "<when the default runs>" --default "<the action if nobody answers>"
```

With no guide running, every session is refused. The hook reads the state only for this tool and for
the calls below that do work, and a state it cannot read counts as not the guide.

## The guide asks and relays, it never works itself

The guide's job is to put decisions to its person one at a time and relay the answers word for word.
In the guide's session the hook refuses the calls that do work: `Edit`, `Write`, `NotebookEdit`,
`git commit` and `git push`, `devctl pr merge` and `release promote`, `gh pr merge` and `review`
(at a command position or inside a `-c` string), a GitHub connector tool that merges, reviews or
pushes, and a browser action (`claude-in-chrome`) other than opening, reading or looking at a page
(navigate, read, find, a screenshot or scroll). The refusal says to hand the work to the supervisor
in one line. Questions, notes, `SendMessage`, reads and `beekeeper` pass; other sessions are not
affected.

The guide's own `AskUserQuestion` gets the checks of `note add --for <person>`: each question asks,
carries a `Status quo: …` and a `Why: …` part in its text, a `Checked: …` part for a claim that
something is merged, green, released, rolled or closed, and the full URL of every `#N` or
`owner/repo#N`; each option's description is its consequence. A question that lacks one is refused,
naming what it lacks.

`beekeeper guide next` serves one decision at a time: the open note for the person due first (by due
time, then the undated in filing order), marked as being asked. It serves the same note until it is
answered (`note answer`) or closed (`note done`); only then comes the next. Only the guide's session
serves decisions.

## The merge gate

Sessions keep typing `devctl pr merge <owner/repo> <n> [flags]`; beekeeper never replaces devctl.
`devctl release promote <owner/repo>` (one repository, no `--dry-run`) passes the same gate: it
queues in its repository's lane and runs like a merge, and the stable release it dispatches
settles the lane as a merge's release does; a promotion of several repositories or of a `--team`
runs ungated.
The PreToolUse hook rewrites the call to `<this binary> gate -- devctl pr merge …` (a background
call gets `--wait 30m`), and the gate decides. The hook finds the merge wherever it runs as a
command: in any part of a pipeline or a `;`, `&&` or `||` list, in a subshell or a loop, behind the
prefix commands that run their arguments (`flock <lock>`, `nohup`, `setsid`, `stdbuf`, `ionice`,
`chrt`, `nice`, `timeout`, `env`, `time`, `command`, `exec`, `VAR=value`, each with its options), and
with devctl named by path (`~/bin/devctl`, `./devctl`, `$HOME/bin/devctl`). It wraps only the devctl
invocation: `flock m.lock devctl pr merge o/r 7 | tee m.json | jq .verdict` becomes `flock m.lock
<this binary> gate -- devctl pr merge o/r 7 | tee m.json | jq .verdict`, so the pipeline and
`pipefail` behave as written and the gate's exit code and devctl's document reach the rest of it.
A merge inside a `sh`, `bash` or `zsh -c` string that the rewrite cannot reach is refused, the
refusal naming the command with the gate written in.

- **Refused, exit 77**, one line starting `beekeeper gate: refused,` that says why and what to do:
  the repository, its lane, `merges` or `github` is held, or a cluster upgrade runs on the lane's
  installation (the hold's reason); the GitHub budget is unknown, checked before devctl makes a
  single request (under `github.floor` the line says `queued` instead: the merge waits for the
  reset in a run of its own, as for exit 76 below, and wakes its owner); the lane's
  installation cannot be read (a lapsed `tsh` login). Nothing else stops a merge: a lane whose
  release has not rolled past `merge.settleTimeout` keeps it waiting, and `watch` says `LANE STUCK`.
- **Queued, exit 76**, one line starting `beekeeper gate: queued,` with the merge's position and
  whom it waits behind (when the machine-wide devctl cap is the reason, `<n> devctl processes run
  machine-wide (cap <n>), not a lane problem`), once the merge has waited `--wait` (2 minutes, 30
  in the background). The merge is not dropped: the gate hands it to a run of its own outside the
  caller, the same gate under `--queued`, which keeps its place for up to `merge.seedTTL`, runs
  devctl when its turn comes and wakes the owner with the outcome (`devctl.unheard`, as below).
  Nobody runs it again; a second `devctl pr merge` of the pull request while it waits is refused
  with exit 3.
- **Otherwise devctl runs once**, its JSON document and exit code (devctl's own 0–9) unchanged,
  and the event log records `merging` and `merged` with the release. devctl serves the
  repositories of `merge.devctlOwners` only (its GitHub App login reaches the giantswarm
  organisation); any other owner's repository takes the **plain squash merge** instead, in the
  same place and unit: as the gh login, it waits up to `--timeout` (45m) for the head's checks,
  refuses what devctl refuses before its wait (a draft, a closed, merged or conflicting pull
  request: 3; another person's: 5), and squash-merges green with the judged head as the expected
  one and `<title> (#<n>)` as the subject, then deletes the branch; red is 1, a timeout 2, a gh
  failure 7, and it waits for no release. Each run's stderr and document are kept in
  `<state>/merge-output/<repo>-<n>-<time>.log` for 7 days, named in its event. A run that ends with nothing
  merged (exit 1–4, 7, 8) is `merge.failed` and keeps its place: `lanes` shows it `retrying`, and
  the session's retry of the same pull request runs before every merge that was behind it. It
  holds the lane for `merge.queueTTL` from the failure, then keeps its place for `merge.seedTTL`
  without holding up a free lane; devctl's refusal (exit 5) leaves the lane.

A merge runs when no merge before it in its lane's queue holds its place, nothing else of the lane runs, fewer than
`merge.cap` devctl processes run on the machine, and the lane's installation is ready: every
HelmRelease of the lane's charts Ready, and the previous merge rolled. Rolled means each
HelmRelease of the merged repository's chart that ran the newest version when the merge started
now reports the released version, or follows a range that never admits it: the HelmRelease's
OCIRepository ref (`semver`, a `tag` as that one version) or its chart template's `version`. A
merge that cuts only a release candidate (`v4.105.0-rc.3`) under a stable range (`>=4.0.0 <5.0.0`)
settles at once, and the gate says `production does not follow 4.105.0-rc.3: flux-giantswarm/…
follows semver >=4.0.0 <5.0.0`; a wait names the range. A release devctl could not confirm (exit
9, a run killed by the tool timeout) settles for `merge.settle` instead. Versions compare as semver: a tag `v4.74.0`
matches a chart version `4.74.0+971d12027db0`. The installation is read with `kubectl
--context <lanes[].context>` (default: the kubeconfig context named after the installation or
ending in `-<installation>`). A merge whose lane has no installation has nothing to roll and
leaves its lane when devctl returns. A settling merge leaves its lane once the lane has settled:
`watch` checks every poll, logs `lane.settled`, and `lanes` then shows the lane free before its
next merge arrives.

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
merge leaves the lane. The same holds for every place whose merge is not in the gate (a seed, a
failed run's retry place, a place kept after its gate left): `watch` asks GitHub about it at every
poll, a pull request merged outside the gate settles the lane (`merged … outside the gate`) and a
closed one drops the place (`merge.dropped`), so no place waits for a `lanes drop`.

A gate call keeps deciding by the code it started with only until the binary is replaced: a call
waiting for its turn when `beekeeper self-update` renames a new binary over its path re-executes
it, the same process, arguments, stdio and deadline, and the new code finds the merge's place by
its session, repository and number. It says `continuing under beekeeper <version>`. A call whose
devctl already runs is never re-executed. A lane that stays stuck anyway (a seed whose session is
gone) is flagged: `lanes` shows it `stalled` and `watch` says `LANE STALLED` once the first
arrived merge has waited `merge.stallAfter` behind places whose merges are not in the gate.

A merge survives its caller. A harness that stops a command kills its process tree, and a
session run as a unit takes its cgroup down with it, so the gate runs devctl outside both: a
transient user service (`beekeeper-merge-<repo>-<n>-…`, through `systemd-run`) runs the hidden
`beekeeper merge-child`, which runs devctl with the caller's environment and directory, its
document, stderr and exit code in files under the state directory (`merges/`); without a user
service manager, merge-child runs in a session of its own. The gate follows devctl's stderr onto
its own output and records the outcome. When the caller ends mid-merge (SIGTERM, SIGHUP, its pipes
closed), devctl merges on and waits for the release, and the gate waits on to record it; when the
gate is killed too, `watch` records the outcome from the files once devctl ended (`MERGE
RECORDED`, a `merged` or `merge.failed` event naming the gone gate). Only SIGINT, a person's
Ctrl-C, reaches devctl. A run that ends without its document or
by a signal (exit 128+n) is judged by GitHub (`gh pr view`), never by its exit code: merged, it
settles its lane with its release unconfirmed and the gate line names `devctl release wait
<owner/repo> --pr <n>`, with no retry place; not merged, it keeps its place for the retry; with
GitHub unanswered, the lane settles as for a lost merge.

devctl's blocking waits, `pr wait`, `release wait` and `rollout wait`, run the same way outside
their caller, without a queue, their files under `runs/`. One poller per command on the machine:
a wait whose identical command line already runs follows that run (the hidden `follow-run`, its
stderr, document and exit code) instead of starting a second devctl, so five sessions waiting on
one pull request cost the budget of one. Whichever of the four it is, its outcome
reaches the session that started it. A caller still listening sees the output and exit code as
ever; the gate then leaves merge-child a marker. When it does not (a headless turn that ended with
the command in the background, a caller killed, its CLI gone), merge-child logs `devctl.unheard`
and wakes the owner, a registered agent, with one line, `<command> exit N: <reason> (output in
<file>)`, through `agents wake`: by name when its CLI runs, else as a headless turn. The reason is
the document's verdict and reason (a merge's release), else devctl's last stderr line; the output
is kept under `merge-output/` for seven days. A second `devctl pr merge` of a pull request whose
merge runs is refused with exit 3, naming that run's start, owner and last line.

A running merge whose gate process and devctl are both gone (killed, or lost with the machine in
a reboot) is lost: whether it merged is unknown. `watch` turns it into the lane's settling merge with an
unknown release, one `MERGE LOST` line and a `merge.lost` event, so the lane settles for
`merge.settle` and frees once its HelmReleases are Ready; `lanes clear <lane>` drops it at once.

A running merge whose devctl runs on after its pull request merged (a hung release wait) holds
its lane for nothing. `watch` asks GitHub about each run older than `merge.hungAfter` (45m,
devctl's own `--timeout`) and ends the devctl of one whose pull request merged longer than that
ago, or closed: SIGTERM to its merge-child, one `MERGE HUNG` line and a `merge.hung` event. The
run is then recorded like any devctl ended by a signal, by its gate or by the next poll: merged,
it settles its lane with its release unconfirmed. `lanes drop <owner/repo> <n>` does the same at
once for a running merge whose pull request merged or closed, and refuses one whose pull request
is open.

A merge of giantswarm/devctl opens a tool-release window by itself: a `merges` hold with
giantswarm/devctl excepted, since the release makes every in-flight devctl run refuse until
updated. It lifts once no devctl merge runs and the local `devctl version` reports another
version than when the window opened, or once its pull request did not merge: the gate lifts it
after a run with nothing merged or no release warranted, and `watch` (or the next gate call) asks
GitHub about a window whose merge ended unrecorded (its gate killed) and lifts it when the pull
request is open or closed. Once its merge merged, beekeeper installs the release itself: the gate
runs `devctl version update` right after the merge, and `watch` retries every 2 minutes while
devctl still reports the old version, logging a failed update as `hold.update`. Nobody has to
update devctl or lift the window by hand.

The budget floor uses the last reading in the state when it is younger than `merge.budgetFresh`
(1m, less than one merge's draw at the floor's margin), else a fresh conditional request, which
costs nothing when GitHub answers 304; a failed read refuses.

## Cluster upgrades

While an installation's clusters upgrade, work on it waits. `watch` reads the Cluster API
clusters of `alerts.installations` every `upgrades.every` (5m), read-only: one list of
Clusters, KubeadmControlPlanes, MachinePools and MachineDeployments (`v1beta2`) per
installation in one `kubectl` call, in parallel, each installation within `alerts.timeout`; an
installation that does not serve one of the kinds is read one call per kind, and one that
serves none of them has no cluster to upgrade. An installation an upgrade runs on or holds is
read every `watch.interval` (30s) until it ends, so `UPGRADE ENDED` and the hold's lift come
within one interval. That is one `kubectl` per installation per 5 minutes while nothing
upgrades, and one list of a cluster's events when its upgrade begins. The readings are kept in
`upgrades.json` in the state directory: a second watch, `snapshot` and `ui` use a reading younger
than `upgrades.every` instead of reading the installation again. The supervisor's watch reads them; while none
runs (a relay, a crash, a frozen desktop) the [standby watch](#desktop-notifications) does, so
an upgrade is held within one `upgrades.every` either way.

A cluster's upgrade begins when its release changes: its `release.giantswarm.io/version` label
differs from the release cluster-api-events last recorded
(`giantswarm.io/last-known-cluster-upgrade-version`), cluster-api-events marks it upgrading
(`giantswarm.io/cluster-upgrading: "true"`, from the release change until its control plane and
workers have rolled), or its scheduled upgrade is due (`alpha.giantswarm.io/update-schedule-target-release`
differs from the label and `…-target-time` has passed). It ends once none of that holds and its
control plane and node pools have rolled (every KubeadmControlPlane at its `spec.version`, every
control plane and node pool with all replicas up to date, no `RollingOut` condition True). A
workload cluster on an older release than its management cluster is not upgrading. The release it
upgrades from is the one the label changed from, else the one cluster-api-events' `Upgrading…`
event names.

While it runs, the installation is held: a hold `upgrade:<installation>/<cluster>` by
`beekeeper watch`, until the upgrade ends, whose reason names the cluster and both releases. The
merge gate refuses the merges of every lane whose `installation` it is (exit 77, with the
reason), `lease claim <installation>` is refused unless an upgrade-unblock grant admits it (below), `lanes` shows the lanes held and `snapshot` the
upgrade with its progress. The watch whose update sets the hold says `UPGRADE …`, the one whose
update lifts it `UPGRADE ENDED …`, so a second or restarted watch says neither again. An
unreadable installation is one `UPGRADES <installation> unreadable: …` line until it answers
again and keeps its holds as they are: it counts as neither upgrading nor quiet. `hold set`
refuses an `upgrade:` target; `hold lift upgrade:<installation>/<cluster>` lifts one by hand
(a workload cluster's roll stuck on a drain need not stop the installation's merges): the hold
stays lifted, recorded with who lifted it, until that upgrade ends, and only a different upgrade
of the cluster (another target release) holds the installation again. `hold list` and `status`
show the lifted hold and who lifted it.


The one claim an upgrade admits is the work that unblocks it: a node drain stalled on a
single-replica PodDisruptionBudget, for example, needs a pod deleted on that installation. The
supervisor grants it with `beekeeper lease grant <installation> <session> --upgrade-unblock
"<why>"`; beekeeper refuses that grant from any other session and while no upgrade holds the
installation. During the upgrade only such grants are claimed, in their order; a plain grant,
the supervisor itself and a person stay refused. The reason is kept on the grant and on the
lease: `lease list`, `handover` and the log's `lease.grant` and `lease.claim` show it next to
the claim's purpose.

A lab lease names its kind cluster under `labs` (`agentlab-1: agentlab`). `lease list` and
`handover` then show each lab lease with its cluster, whether that cluster runs and who holds
the lease, and list a running kind cluster no lab lease maps as unmapped; a claim of a lab
lease names the cluster it covers, and `free` marks a running cluster whose lease is free as
idle, with its last holder from the event log. Without `labs` nothing asks docker.

## Desktop notifications

`beekeeper watch --notify` sends the events that need a person to the desktop's notification
service (`org.freedesktop.Notifications` on the session bus: dunst, mako, GNOME, KDE) and still
prints every line. Each notification carries a summary, the session or resource involved and the
command that shows more.

| Kind | Event | Urgency |
|---|---|---|
| `due` | a note or a timer falls due | normal |
| `budget` | the GitHub budget under the floor | normal |
| `stale-lease` | a lease whose holder's session is gone | normal |
| `no-supervisor` | a supervisor whose CLI stayed gone past `supervisor.restartGrace` with no relay open: claims stay gated until a successor starts; again after `notify.repeat` while it lasts | critical |
| `page-unowned` | a firing alert at `alerts.pageSeverity`, of `alerts.team` when set, that no session has owned (`beekeeper alerts own`) for `alerts.ownerGrace`; again every `alerts.ownerGrace` while it stays unowned | critical |

Nothing routine notifies: sessions starting or ending, alerts other than an unowned page, relays. The machine's lines never
notify: memory, swap, `OOMD IMMINENT`, OOM kills, load, CPU, processes, tmpfs and disk are the
supervisor's to act on, and its watch says them; a person gets no pop-up they cannot act on. Each event is one notification however many watches run `--notify` on the same state: the
first to claim it in `notify.json` (under `notify.lock`) sends it. A lasting condition (`budget`) and a supervisor gone (`no-supervisor`, per supervisor) notify again after `notify.repeat`. Quiet hours hold every notification that is not
critical and send what they held as one notification when they end. With no notification service
on the bus the watch runs on, prints its lines and says so once. `notify.json` also keeps the
last 20 deliveries with the id the service returned.

When no supervisor runs, the same watch runs as a systemd user unit,
[`contrib/systemd/beekeeper-notify.service`](contrib/systemd/beekeeper-notify.service), with
`--standby`: while a supervisor's session runs it leaves the notes, timers, session records and
relays to the supervisor's watch and never reads the alerts, so it takes nothing from the
supervisor's view; what both see (the budget, stale leases) is sent once.
The upgrade holds never wait on a supervisor: while no supervisor's watch has begun an upgrade
cycle within five `watch.interval` (it says so in `upgrades-watch.json`), the standby watch reads
the upgrades on the same shared schedule, sets and lifts their holds and says
`UPGRADES read by the standby watch` once; beside a running supervisor's watch it reads none.
A supervisor runs its own watch with `--notify` too. `beekeeper install` writes and starts the
unit with the binary's path; `journalctl --user -u beekeeper-notify -f` shows its lines.

### A supervisor gone

Claims stay gated from the first `supervisor start` until a deliberate `supervisor stop`: a
supervisor whose CLI crashed keeps the grant rule in force until a successor's `supervisor
start`, so no claim goes ungated in the gap. Once its CLI has been gone longer than
`supervisor.restartGrace` with no relay open, the watch says `SUPERVISOR GONE` once, naming the
supervisor and that claims are gated, and sends a critical `no-supervisor` notification, again
after `notify.repeat` while the gap lasts. A supervisor back, the same one or a successor, is
`SUPERVISOR BACK`, once. `beekeeper handover --prompt` is what a successor starts from.

### Fresh successors, and a supervisor started again without a click

Every run of a role is numbered: "Supervisor run N" and "Guide run N", N one above the highest
run recorded (the state's run, the names of the holder, its relay and the holders it relieved,
and the holder's desktop title). `supervisor status` and `guide status` name the run. A start
in a session not named as that run is the next run: the session keeps the name its CLI takes
messages under (its `-n` name), and a steward sets its desktop title to the run's name. No session
is kept in reserve or repurposed: a relay and a crash both start a fresh session.

- **Relay:** `supervisor relay` (`guide relay`) starts "<Role> run N+1" and opens the relay to
  it. Its brief has its first turn, which runs headless, take the role with `<role> start` and
  end at once: a headless turn that arms a watch never ends, so the desktop never gets the
  session's CLI, and opening its row would start a second CLI on the same session. The reopen
  after that turn warms its desktop CLI, and the standby watch sees the holder's CLI back under a
  new PID and sends it `beekeeper: your CLI restarted (…) and its watch is gone. Run beekeeper
  handover --prompt and follow it.` (`RESUME`).
- **Crash or CLI exit:** once the holder's CLI has been gone past its `restartGrace` (30s, a
  debounce for a session someone woke: the app never restarts a crashed CLI) with no relay open,
  the standby watch starts the next run the same way, with the relay from the gone holder, once:
  the open relay keeps later polls and a restarted watch from starting another. `SUPERVISOR
  GONE` (and `GUIDE GONE`) say so, and `SUCCESSOR` (`SUCCESSOR FAILED`) says how the start went.
  A session whose headless turn of beekeeper's start or wake runs is not gone; the reopen after
  that turn, which waits while the desktop's window has the focus (up to 25 minutes), is no turn:
  the `restartGrace` covers it. Claims stay gated until the successor's `supervisor start`.
- **A successor that does not come up:** a successor starts in `<role>.dir`, else in the folder
  its predecessor's desktop session started from: never in the worktree the desktop made for
  the predecessor, whose branch the desktop cannot check out for a second worktree, so it would
  never warm the successor's CLI. The resume goes to the desktop CLI, never to a headless turn
  of beekeeper's. A successor whose first turn ended with no desktop CLI past the `restartGrace`
  (its reopen waits on a person working in the desktop's window, or the desktop did not warm
  it) is not replaced: the standby watch resumes it headless with the same message (`agents
  wake`, `RESUME`), and that turn keeps the role's watch and its CLI, with no click or decision
  by a person. The pending reopen then yields to that turn, so the desktop warms no second CLI
  beside it; the wake's own reopen follows the turn, and each later gap without a CLI is resumed
  headless again until the desktop runs the holder's CLI. One whose resume never ran did not
  come up (`SUCCESSOR DOWN`): the first is one
  note for `guide.person`, the next successor starts 5 minutes later, the one after 10 minutes,
  and after three none starts until a holder runs again.
- **Reboot or app restart:** the login unit
  [`contrib/systemd/beekeeper-supervisor-open.service`](contrib/systemd/beekeeper-supervisor-open.service)
  runs `beekeeper supervisor reopen`, which starts the app on the recorded supervisor's session
  (`claude://code/continue?session=local_…`); the standby watch opens it the same way once when
  it sees the app started after the supervisor's CLI stopped: after the CLI was first seen gone,
  or with the watch never having seen that CLI run under this app, as after a reboot, where the
  app starts at login before the standby watch's first poll. Every CLI is cold after an app
  start, so the focus starts the supervisor's; the standby watch sees its CLI back under a new
  PID and sends it the same `RESUME`. A supervisor not back within 3 minutes of the reopen gets a
  fresh successor.
- **Stopped workers:** a registered agent with a task whose CLI does not run (a `claude --bg`
  worker a reboot stopped, a desktop session closed) does not come back by itself. `watch` says
  `AGENTS STOPPED` once per agent, and the `handover --prompt` Agents section marks it, each with
  how to resume it: `claude --bg --resume <session> "…"` for a background worker, its
  `claude://code/continue` link for a desktop session. Nothing is resumed automatically: a burst
  of resumed workers after a login is the supervisor's call against the machine's memory. The
  line tells a worker parked on a person (its serve record's `--waits` names the person, a note,
  the supervisor or the guide: `parked on a person: …`) apart from one whose headless turn ended
  on a background wait again after the reopen resumed it once (`ended on a wait again after its
  resume at …`).
- **Capacity:** the supervisor keeps `capacity.floor` to `capacity.ceiling` agents busy (5 and 10).
  Busy is a roster agent with a task that is neither parked nor kept: an agent kept on purpose
  (`agents keep`), one a timer wakes and one whose session waits on its person are parked; the
  supervisor, the guide and a role's successor are not counted. `beekeeper capacity` (and
  `--json`) prints the count, the headroom that bounds a new start (MemAvailable against
  `capacity.availMinMiB`, the machine swap's growth over the watch's readings against
  `capacity.swapGrowthMaxMiB`, never the swap in use, the free build slots, the kind labs against their
  cap) and one verdict, `room for N starts` or what blocks one; it reads, changes nothing and makes
  no GitHub call. `watch` says `CAPACITY LOW <busy> of <floor>` once while busy stays under the
  floor with room for a start, and `CAPACITY FULL <busy> of <ceiling>` at the ceiling, each with
  the headroom line and one `ENDED` line when the count recovers. The `handover --prompt` carries
  the count, `status` shows `busy <n>/<floor>-<ceiling>`.

The command-line send is one headless `claude -p` turn whose only tool is SendMessage, addressed
by the name ListAgents shows (the session's title): Claude Code has no send command, and its
peer messaging is not a documented interface, so it is isolated in `internal/peer`.

```sh
curl -fsSL https://raw.githubusercontent.com/giantswarm/beekeeper/main/contrib/systemd/beekeeper-supervisor-open.service |
  sed "s|%h/.local/bin/beekeeper|$(command -v beekeeper)|" > ~/.config/systemd/user/beekeeper-supervisor-open.service
systemctl --user daemon-reload
systemctl --user enable beekeeper-supervisor-open.service   # runs at the next login
```

`beekeeper status --bar` prints one row for a desktop bar (waybar, polybar, i3blocks), the same
shape as the rows of `free --summary`, so one bar module reads both. The contract is fixed: six
tab-separated fields, always present, in this order.

| Field | Value |
|---|---|
| 1 | `beekeeper`, the row's key |
| 2 | the supervising session's name; `-` when none is recorded; `!<name>` when the recorded one's session no longer runs |
| 3 | the number of leases held |
| 4 | the number of holds in force |
| 5 | the number of notes and timers whose time has come |
| 6 | the busy agents against `capacity.floor`, `<n>/<floor>` |

### Agents started without a click

`beekeeper agents start <name> <brief file>` starts an agent the way a person would start a
session and hand it a brief, without the click. The first prompt is the worker rules beekeeper
ships (the `worker-rules` skill, under the binary's version), then the brief as the task, so a
brief carries only its task and reports to `the supervisor`, whoever holds the role by then; a
hand-over puts the rules ahead of the follow-up's prompt again. The first turn runs from the command
line in bypass, and the roster shows it busy with its `--task` from the moment it is started. The session is then a desktop session as well: the person reads and answers it in the sidebar.

**The roster is the view of which agents work, not the sidebar.** Claude Desktop marks a row
working only while its own CLI of the session runs a turn; its session record holds no state another
process could set. A first turn (and a wake turn of `agents wake`) is a `claude -p` outside the
desktop, so the row shows the session idle while it works. The desktop has no route that runs the
brief as a desktop turn of the imported session instead: `claude://resume` imports a session without
a prompt, `claude://code/new?q=` fills the composer of a new session under an id of the desktop's,
and a message to `local_<id>` (`SendMessage`, the session tools' `send_message`) is a turn another
desktop session's model sends, labelled as that session's message, under the desktop's cap of CLIs
and of sends since the person last typed. `beekeeper agents` shows such an agent `live, first turn
running` or `live, wake turn running` and says under its table how many agents are in a headless
turn; `--json` marks each with `headlessTurn`.

The import makes the desktop warm a CLI of its own for the session (`--resume=<id>`) while the
first turn still runs. Two CLIs on one session id are two peers under one name, and a message by
name could reach the desktop's copy, which would run a turn beside the first turn. So once the
import has shown the session, beekeeper stops the desktop's CLI (it waits up to 15s for it) while
the first turn runs: the first turn is then the session's only CLI, and a message by name, such as
a grant or a clearance, reaches it at its next tool call. Once the first turn has ended, the unit's
`ExecStopPost` runs `beekeeper agents reopen <id>`: it shows the session in the desktop for a
moment and switches back, which warms the desktop's CLI of it, so the session is a peer again and
takes a follow-up task by message as a desktop turn. It reopens only a start the roster still
holds, never one a hand-over or `agents remove` took off. A reopen the desktop did not take (the
session not shown, its title not restored) is an `agent.reopen` event and ends the unit
successfully: the turn ended as it should.

A headless turn's end is the end of its process, so a background Bash it launched never wakes it.
When a start's or wake's turn ended with its task open on a background wait (a `run_in_background`
Bash its transcript holds no completion notice for, or a process the unit still runs), the reopen
resumes the session headless instead, once per task, with a turn that names the wait and tells it
to wait in the foreground; the event log says `agent.resumed-wait`, and the resumed turn's reopen
shows it in the desktop. A worker done, kept, or parked on a person is left alone, and so is a
`devctl` wait or merge the gate runs, whose outcome wakes its owner by itself (`devctl.unheard`).

The desktop handles each `claude://resume` link twice. When the second delivery arrives while the
first import still runs, both import, the second drops the transcript's title and model as stale
(the first touched the file), and the desktop keeps its untitled record: the session shows
untitled in the sidebar and to ListAgents under a default name (`<dir>-<n>`), which a message by
its name does not reach, even when the record file showed the title for a moment. The desktop
rereads neither the transcript nor its record files, and a record without a model gets no CLI
when the desktop shows it (only a preview shell), so the session cannot be asked to retitle
itself. Every CLI the desktop runs, though, has the desktop's session tools
(`mcp__ccd_session_mgmt__set_session_title`, `archive_session`), and they act on any session by
its id. So `agents reopen` checks the record once the first turn has ended and, when it lacks the
roster name, asks a steward through its socket (`$XDG_RUNTIME_DIR/cc-socks/<pid>.sock`) to set
it, then waits up to 80 seconds for the desktop to record it. A steward is a model and can
decline a request from another session, so an unanswered request goes to the next steward, three
at most (the unit's `TimeoutStopSec` is 5 minutes). The steward is the session's own desktop CLI
when the desktop warmed one. Otherwise it is the idle desktop CLI of another session beekeeper
started, a finished worker off the roster before an idle roster agent (whose brief can forbid the
call), each idle longest: transcript quiet for
30 seconds, no tool command, no headless turn, no task on the roster. It is never the supervisor
or the guide, nor one relieved within 7 days (it follows its role's rules still), nor
a session its person started, nor a session handed over (a later start or another roster
session carries its name): a request would have the desktop run a turn of it, and start its CLI
again, beside its follow-up under the same name. A title set that way is the desktop's "set
by an agent", which its own titling never overwrites.

`agents remove` archives the removed agent's desktop session the same way (`archive_session`;
the desktop's Archived list brings it back), and prints and logs (`agents.archive`) what it did.
It does so only for a session beekeeper started that runs no turn and holds or held no role, unless
a relay relieved it;
`--keep-desktop` leaves it in the sidebar. When no steward is idle it says so and the removal
still stands; the doctor then owes the archive (below).

`beekeeper doctor` does these chores by rule, and every watch poll (not `--once`) runs it in the
background, one `DOCTOR` line per thing it did. It takes an agent off the roster once it reported
its work finished with `agents idle --done` and its CLI runs no turn, once it was relieved of the
supervisor's or the guide's role (a relieved role relays, it never hands over), or once it stayed
idle `agents.staleAfter` (24h) with no CLI running; the desktop sessions beekeeper started for the
finished and the stale ones are archived in one steward's turn, under the rules above. An entry
kept on purpose is never removed for staleness and its desktop session never archived by the
doctor: `beekeeper agents keep <agent> [--until <time>] [--reason <text>]` marks a judge or
reviewer session a person returns to, a spare or a worker parked on a long external wait, and an
open timer that wakes the agent by name (`timer add --wake`) keeps it the same way until the timer
is done. `agents` shows what keeps an entry in its KEPT column, `doctor --dry-run` names each kept
entry it leaves, the watch says no `AGENTS STOPPED` line for one, and `board next` counts its
`sessions serve` record as covering its item. `agents keep <agent> --no-keep` lifts the marker and
returns the entry to `agents.staleAfter`. An archive
that stayed (the CLI ran a turn, no steward recorded it, also after `agents remove`) is owed: the
doctor asks for it again on its later runs while the CLI runs no turn, in the same steward's turn,
until the desktop records it, up to 5 stewards' turns 10 minutes apart within 24 hours, each try a
`DOCTOR` line and an `agents.archive` event with its reason; an agent back on the roster or the
session archived by hand is owed nothing. The old session of an `agents handover` and the run of
the supervisor or the guide a relay relieved (once its successor took the role) are owed the same
way, so a relieved run frees its desktop CLI slot. A session
beekeeper started whose desktop record shows another title than its roster name gets the name
back through a steward, at most every 30 minutes per session. Each fault of `doctor.faults` is
probed (the probe exits 0 while the fault is absent); a failing one whose remedy may run
unattended is remedied and probed again, any other only with `doctor --fault <name>`, and a fault
still failing is one note for `guide.person`, closed once its probe passes, and one `DOCTOR FAULT`
line while it lasts. `--dry-run` says what it would do. A note for the person that asks nothing
(no question mark, no `--option`, no request verb opening it) is refused: a status line goes to
`beekeeper log add "<text>"`.

The desktop's import takes the session's model from the transcript's last reply and falls back to
its own default model without one. So beekeeper imports the session only once the transcript
holds its first reply and has stayed unchanged for 2 seconds (up to 5 minutes), and says which
model the desktop recorded for the desktop turns. Claude Desktop on Linux currently handles each
`claude://` link twice, and the second of the two concurrent imports records no title and no
model. So once the import is done, the start has a steward, an idle desktop CLI of another
session beekeeper started (the desktop refuses a session's switch of its own model), set what
the record dropped: the title to the agent's name and the model to the one its first turn ran
on (`set_session_title`, `set_session_model`), and says which steward did, or why none could;
the reopen after the first turn tries again for a model still missing.

The import switches the desktop's main window to the new session. Once it has (up to 15s),
beekeeper switches the window back to the session it showed before, the last focus change in the
desktop's log (`claude.desktopLog`, `~/.config/Claude/logs/main.log`), so the person keeps working
where they were; the new session waits in the sidebar. A desktop that showed no session, or that
the start had to launch, stays on the new one.

Neither link goes while the desktop's window has the focus (Hyprland's active window, `hyprctl
activewindow`, app id `com.anthropic.Claude`): a switch there would land under someone reading or
typing, whose keystrokes then reach another session. Nor does one go until the person's keyboards
and pointers have been idle for `desktop.typingQuiet` (30s): a stray keystroke would go into the
window the link opens. beekeeper reads only when an event arrives on `/dev/input/event*` (the
devices with keys or relative motion; the user needs the `input` group), never which, and grabs no
device; it watches from the start, so a first reply after quiet input imports at once. Input that
cannot be read stops the start before it runs anything, with the reason; `desktop.typingQuiet: -1s`
opens the links without watching the input. The import waits up to 2 minutes for the
window to lose the focus and the input to go quiet; past that the start ends without it
(`not imported yet`, with what held it), and the reopen
after the first turn imports the session instead, waiting up to 25 minutes more (the unit's
`TimeoutStopSec` covers the wait). While it waits, the agent records what holds it and until when:
`agents` shows it `not running, import waits until <t>`, the watch says `IMPORT WAITS` once per
wait, and a `lease grant` or a message by name to the agent says that no CLI of it runs and that
its import is pending. A reopen that waits it out is recorded as missed. Without a Hyprland session
there is no focus to ask, and the links go at once. A locked screen is no focus: under a running
screen locker (hyprlock, swaylock, gtklock, waylock) the compositor still names the window focused
before, nobody reads or types there and keystrokes go to the locker, so the links go at once,
without the `desktop.typingQuiet` wait.

A worker whose next turn needs the desktop (its browser: the Claude in Chrome tools exist only in a
desktop CLI, never in a headless turn) does not wait for the window's focus. It is started with
`agents start --desktop`, or asks with `beekeeper agents desktop` before its headless turn ends
(`beekeeper agents desktop <agent>` asks for another agent): its import and reopen wait only for the
person's input to pause, 1 minute at most, show the session for a moment and switch the window back.
A reopen already waiting goes ahead within a second, and an agent with neither a CLI nor a waiting
reopen is shown at once; the ask ends once the desktop runs its CLI, so its next message or grant
reaches that CLI.

A reopen checks for the CLI its show warms (up to 15s). The desktop runs at most a cap of CLIs (its
`CliGovernor`, 28 here): at the cap it starts none for a show and logs `at cap=<n>; yielding warm
spawn`. The reopen reads that line and reports it as missed (`the desktop warmed no CLI … it runs its
cap of <n> CLIs`), and the agent's ask for a desktop turn stays: the CLI starts once a person opens
the session or one of the desktop's CLIs ends (`beekeeper free` lists the stale ones).

A follow-up by message crosses permission modes: the started session runs in `acceptEdits`, a
bypass supervisor sends to it, and it answers back. Claude Code decides each cross-session message
on the receiving side by its `crossSessionInbound` setting; unset, it holds a message from the other
permission class for the person's approval, and a headless receiver lets it expire. Set it once in the
user-level `~/.claude/settings.json` (a project or local settings file can only tighten it):

```json
"crossSessionInbound": "accept"
```

Every message between the user's own sessions is then delivered, whatever the two modes; each
session's tool permissions stay its own.

Claude Desktop's import turns `bypassPermissions` into `acceptEdits` for every desktop turn, with
no setting to change that, and raising the mode again takes the person's approval card each time.
So in its desktop turns such an agent would stop at the first request no allow rule covers.
`beekeeper hook permissionrequest` answers those requests.

**What the hook allows:** every request that would otherwise show a permission card, and nothing
else, only in sessions whose id `beekeeper agents start` recorded together with the mode
`bypassPermissions` it passed at start, and only while such a session runs in `acceptEdits`, the
mode the import gives it. That is exactly what the session's bypass start already allowed. Its
subagents share its session id and are answered the same way. Each allow is a `hook.allow` event
in `beekeeper log`, naming the session and the tool.

**What it leaves to the person:** every other session gets no answer and the normal card: desktop
sessions, sessions started any other way (a `claude -p` in bypass that beekeeper did not start
included), beekeeper's starts the person set to `default` or `plan`, and starts older than 30
days. Deny rules still win: Claude Code refuses a denied call before it asks, so the hook never
sees it. The desktop's own consent cards (raising a mode, deleting a session) are the app's, not
permission requests, and the hook cannot answer them. On malformed input or an unreadable
configuration or state the hook gives no answer, never an allow; a request in any mode but
`acceptEdits` is decided without reading the state, and the state is read without its lock, so
a permission request never waits on beekeeper.

A session cannot add itself: its id enters the record only through the start that created it,
written under the state lock before the session existed.

**The browser is the desktop's, not a permission request.** Claude in Chrome's site requests
(`browser:navigate`, one per session and site) are held by Claude Desktop itself, in the session's
desktop row; Claude Code's permission layer never sees them, so neither `bypassPermissions` nor the
hook answers them. The import gives a start the Chrome permission mode
`skip_all_permission_checks` only when the desktop allows all browser actions, a desktop-wide
choice a person makes with "Allow all sites" on one site request; otherwise it records no Chrome
mode, and each navigate to a site the session was not allowed on yet waits for a person (with
nobody there, until the desktop aborts it hours later and the model reads "Claude in Chrome is not
connected"). Read-only calls such as `tabs_context_mcp` never ask. beekeeper does not raise the mode
itself: switching another session's permissions takes the person's approval in the desktop.
`agents start` says which Chrome mode the desktop recorded, and `beekeeper agents` shows it per
agent in its BROWSER column: `asks` or `skips`.

The same hook serves the person's take-over on `beekeeper ui`: while a running screen has taken a
session over (a flag file per session in the state folder's `takeover/`, naming the screen's
process), the hook holds that session's request for the screen and answers with the screen's allow
or deny. It gives up itself after 290 s, and at once when the take-over is released or the screen
is gone: no answer, and the request goes to the session's own window. Telling a session not taken
over reads that one file, without a lock; nothing waits on beekeeper.

Install it in `~/.claude/settings.json`, next to the PreToolUse hook (`beekeeper install` writes
it); the timeout is above the hook's own 290 s, so the hook, not Claude Code, ends a held request:

```json
"PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command",
  "command": "~/.go/bin/beekeeper hook permissionrequest", "timeout": 300}]}]
```

### Waking a session

Claude Desktop delivers a `SendMessage` to a desktop id (`local_<id>`) itself: it starts the
session's CLI when none runs, which makes it the only route that wakes a stopped desktop session,
and it caps the route. After ten such sends since its person last typed in the sending session,
the desktop answers "Paused until your user's next message", so a supervisor nobody types in
loses it within hours. A send by name (a UDS peer) is not capped, but it reaches only a running
CLI.

`beekeeper agents wake <agent> "<message>"` covers both. A session whose CLI runs gets the
message by name. For a session with none, beekeeper resumes the session's own CLI from the
command line: one headless turn, `claude -p --resume <id>`, continuing its transcript under the
same id, as `agents start` runs a first turn. Once the turn ends, `agents reopen` shows the
session in the desktop for a moment, which warms its desktop CLI, and later messages by name
reach it. A wake while the unit starts, before its CLI is a peer, is refused (exit 3): send
again in a minute.

The desktop also starts a CLI for a `local_` send while a headless turn of that session runs (a
first turn, a wake): two copies of one session then run turns side by side. The PreToolUse hook
therefore sends a `SendMessage` to a `local_` id whose session has a running CLI to that CLI by
its name (`updatedInput`), which queues the message in the running turn. A name that two running
CLIs carry is refused with their PIDs, since a send by it reaches neither for sure. A send to a
session with no running CLI passes to the desktop, which starts it.

### A repository's instructions on the first write

Claude Code loads the instructions of the project a session started in, not those of another
repository the session goes on to change. The hook closes that gap: a session's first `Edit`,
`Write` or `NotebookEdit`, or first `git commit` (in the call's directory, behind `cd <dir> &&`
or with `git -C <dir>`), in a git repository other than `$CLAUDE_PROJECT_DIR` carries that
repository's instructions as `additionalContext`, once per session, subagent and repository:

- `CLAUDE.md`, `AGENTS.md` and `.claude/CLAUDE.md`, with their `@path` imports three levels deep;
- `.claude/rules/**/*.md`;
- the `hooks` and `permissions` of `.claude/settings.json`, which do not run in the session;
- the files these name, linked or in backticks, on a line that makes them mandatory ("must read",
  "read … first", "mandatory", "always read", "required reading").

The context is at most 10,000 bytes: a file that does not fit is cut with a note naming it, the
files after it are listed for the agent to read. Only regular text files inside the repository
are read: no symlink out of it, no `.git`, and no name that holds or may hold a secret or a
person's own setup (`settings.local.json`, `*.local.md`, `.env*`, keys and certificates, sops
files, anything named secret, credential, password, token or kubeconfig). The call's permission
is untouched: a rewrite such as the build slot or the merge gate carries the context beside it, a
refused call carries none and does not count as the first write. The markers are one file per
session and repository under `<stateDir>/reads/`, created exclusively so two concurrent calls
inject once; a session's markers go 7 days after its last first write.

Register the hook for its six tools:

```json
"PreToolUse": [{"matcher": "Bash|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|mcp__.*", "hooks": [{"type": "command",
  "command": "~/.go/bin/beekeeper hook pretooluse"}]}]
```

### Agents handed over near their context limit

Every registered agent's context (the CTX column) is watched, not only the supervisor's and the
guide's. Once an agent session's context reaches `agents.relayAt` (default `supervisor.relayAt`),
`watch` says `HANDOVER DUE "<agent>" at <n>k: beekeeper agents handover "<agent>"` at the
agent's first quiet moment: no tool command of its own running and no gated merge of its own in
flight. An agent with a task whose CLI no longer runs (its headless turn ended while it waits on a
grant or a person) is measured by its transcript and is due the same way, with `(its CLI does not
run: …)` in the line. It says it once per agent session, across the watch's restarts. An agent
holding the supervisor's or the guide's role moves by relay instead.

The supervisor then runs `beekeeper agents handover "<agent>"`. It refuses (exit 3, before it
asks for anything) unless a PermissionRequest hook for every tool runs `beekeeper hook
permissionrequest` where the follow-up starts: in the user settings or in the `.claude/settings.json`
or `settings.local.json` of its folder or of the checkout that folder is in. Without it the
follow-up would stop at its first card after the old session was stopped. Otherwise it prints one
line per step:

1. **The note.** A peer message asks the agent to record what is in flight and what is next with
   `beekeeper agents note "<...>"` and end its turn; the hand-over waits `agents.noteWait` (3m)
   for it and carries on without it after that. The ask is one headless relay turn; a turn that
   called no SendMessage sent nothing and is run again, three turns at most. An agent whose CLI
   does not run, or does not take the message by name, is asked in one headless turn resumed from
   its transcript instead (as `agents wake` does, without the desktop's reopen), which is stopped
   once the note is written or the wait is over. Without a note the step says what the follow-up's
   prompt rests on: the task, the `sessions serve` record (the issue and what it waited on) and the
   last events. The command exits non-zero whenever no follow-up was started.
2. **The prompt.** `beekeeper agents handover --prompt "<agent>"` prints it: the roster task (of an
   idle agent the last task it reported idle on, which a repeated `agents idle` or a re-register
   keeps), the brief the agent was started with (a follow-up's brief is its predecessor's, not the prompt
   around it), its `sessions serve` record, its gated merges, its last events and its note. No
   standing rules and no live values: the follow-up is told to check the live state first.
3. **The start.** The follow-up is started as `agents start` starts one, in the old session's
   folder and model, under the agent's name: it takes over the roster entry, the task and the
   session record, busy with the task. It shows in the sidebar and runs its first turn from the prompt alone.
4. **The end.** Once the follow-up's transcript holds its first reply, the old session's CLI and every
   process under it get SIGTERM, then SIGKILL after 10 s, by PID: the desktop shows it stopped. A start
   or wake unit of the old session still running its reopen is stopped too, so the desktop does not
   warm a CLI of it again.
5. **The archive.** Once that CLI has exited, the old session's desktop row is archived through a
   steward, as the doctor archives a finished worker's, so only the follow-up's row carries the
   agent's name, also along a chain of hand-overs. A row it cannot archive now (no steward idle, a
   CLI of the old session running again) the doctor owes and asks for while that CLI runs no turn.
6. **The log.** One `agents.handover` event in `beekeeper log`.

A configuration named with `--config` or `$BEEKEEPER_CONFIG` is passed to the started session
as `$BEEKEEPER_CONFIG` and named in the note's command, so both write to the same state.

### The scheduled status reporter

With `reporter.every` and `reporter.brief` set, the standby watch (`watch --standby`,
`beekeeper-notify.service`) starts a one-off reporter session once per interval, on the interval's
multiples (on the hour for `1h`), whether or not a supervisor runs:

1. **The start.** One headless `claude -p` turn in `bypassPermissions` in a transient user unit
   (`beekeeper-report-<id>`, its output in `journalctl --user -u <unit>`), in `reporter.dir` with
   `reporter.model`, registered on the roster as `Status report HH:MM`, busy with the brief's first
   line. Its prompt names `reporter.person` and the interval it covers, then the brief. It is not
   imported into the desktop: the command-line turn has the claude.ai connectors, and the person's
   window is not switched every hour. `reporter.start` in the log, one watch line.
2. **The post.** The session runs `beekeeper report --since <start> --until <end>` for its interval,
   which renders the whole report from beekeeper's state in seconds, and posts its output with the
   Slack connector, adding at most three sentences of its own judgement: two tool calls. The
   time range is in `reporter.tz`, else the machine's zone, read for each run as `timedatectl`
   sets it (a running watch keeps no stale zone). The session starts with `--settings` adding a PreToolUse
   hook on `slack_send_message` (`beekeeper hook reportcheck`) that refuses a post failing
   `beekeeper reporter check`, with what to fix: the connector takes standard Markdown, so every pull
   request or issue is a link `[<repo>#<n>](https://github.com/<owner>/<repo>/pull/<n>)` whose label
   names the repository and number it links; no bare `#<n>` or `repo#<n>`, session names included
   (a beekeeper note is `note <n>`), no Slack `<url|label>` syntax, a first line naming the
   zone (`EEST`), no time in UTC. beekeeper sees the post in the session's transcript: a
   `slack_send_message` call that returned without an error.
3. **The end.** Once it posted, beekeeper takes the reporter off the roster and stops what still
   runs of its turn by PID (SIGTERM, then SIGKILL after 10 s): `reporter.posted`. A turn that
   ended without a post is ended the same way (`reporter.unposted`), and one that has not posted
   within `reporter.timeout` (20m) is stopped (`reporter.timeout`); each is one watch line.
4. **A pause.** `beekeeper reporter final <time>` starts one last report at that time, outside the
   slots, covering the time since the last report started and saying that the reports pause; with
   its start no scheduled report starts until `beekeeper reporter resume` (`reporter pause` pauses
   at once). A running reporter still posts and is ended. `reporter.final`, `reporter.pause` and
   `reporter.resume` in the log.
5. **No overlap.** While a reporter runs, the next slot starts none: `reporter.skip` once. The slot
   after its end starts the next one; the state's `report` keeps the current or last run, so two
   standby watches start one reporter per slot.

## omp agents

omp (oh-my-pi) is beekeeper's second local harness beside Claude Code, for agents on a locally served
open-weights model or any other provider omp speaks. Every omp process that runs a session (no
helper subcommand such as `omp models` or `omp ps`, no subagent of another omp) shows in
`sessions`, `snapshot` and `free` with harness `omp`, its state (`busy` while a turn runs, `idle`
while it waits for a message), its title, model, checkout and memory, the tool commands its shells
run, and its last hour from its session file (turns, tool calls and failed ones, GitHub calls,
tokens, and the cost omp itself counted). `tail <session>` follows it: the person's and beekeeper's
messages and its replies, without thinking and tool output.

omp keeps no record of which process runs which session file and holds none open; its session files
(`omp.sessionsDir`, `~/.omp/agent/sessions/<folder per cwd>/<time>_<id>.jsonl`) start with a title
line and a header naming the session id, its creation time and its working directory. beekeeper
gives each process the file of its working directory it created (the first created at or after the
process started), and a process without one the file there written last since it started (a session
it resumed). omp writes the file at the first reply, so a session not yet answered is listed
without a transcript.

`agents start <name> <brief> --harness omp [--model m]` starts `omp --mode rpc --no-ui
--approval-mode yolo --model <m>` on `--model`, else `omp.model`: an exact selector `omp models`
lists (`provider/id`, such as `ollama/qwen3.5:9b`). With neither, or a model omp does not list, the
start is refused before anything is recorded: omp's own default and a fuzzy pattern would pick a
model nobody named, which may not be served right now. `agents` and `sessions` show each session's
model, an omp session's from its command line until its session file records one. It runs in a transient user unit
`beekeeper-omp-<id>`, with its stdin on a FIFO inbox (`<stateDir>/omp/<id>.in`) the unit opens for
reading and writing, so the inbox never ends while the agent runs. The agent carries its start id and
name in its environment (`BEEKEEPER_OMP_AGENT`, `BEEKEEPER_AGENT_NAME`) and is registered on the
roster as `omp_<id>`. A message is one line of omp's rpc protocol, a `steer` command: omp delivers
it at the agent's next tool round while a turn runs and starts a turn with it while the agent is
idle. The brief is the first message; `agents wake <name> <message>` writes the next. Writers take a
lock file beside the inbox, so two senders' lines never interleave. A wake of an agent whose process ended is
refused (exit 3): nothing reads its inbox, and nothing resumes it. Taking the agent off the roster
(`agents remove`, the doctor) stops its unit and removes its inbox and lock file: nothing resumes an omp agent,
so a process left running would only hold its model. The agent's own beekeeper commands act as its roster entry
(`BEEKEEPER_OMP_AGENT` wins over any session variable its tool shell carries), so it reports its work done with
`agents idle --done` like any worker. `agents handover` refuses omp
agents, and no desktop import happens.

An omp session the person started in a terminal shows in `sessions` and can be followed, but takes
no messages: omp offers no way into a running interactive session except its collab link, which goes
through omp's relay service.

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

## The central instance

The leases of the installations a team shares live in one place, `beekeeper serve` behind
muster, so two people's machines see one holder. `central: {context: <muster context>}` turns it
on: `lease claim|release|status` of a name that is none of the machine's own resources
(`resources`, the model server, the browser) goes to the central instance as the person, through
muster's `call_tool` (`x_<central.server>_<tool>`). The person signs in once with `muster auth login --context <context>`; each call takes the context's endpoint and the person's token from
`muster` and keeps neither. `lease list` shows the machine's leases, then the central ones. A
claim another person's agent holds is refused (exit 3) with its holder, `"<person>/<agent>"`; a
name the central instance does not know is a usage error (exit 2). While a supervisor runs, a
central claim still needs its grant, as a local one does.

There is no local copy to fall back on: a central verb that does not reach the central instance
(no sign-in, muster or `beekeeper serve` not answering) is refused with the reason and exit 69,
and the machine's own resources are unaffected. `beekeeper watch` publishes the machine's agents
to the central roster on their transitions only (registered, a task taken or ended, done, gone;
`agents_register` with `task` and `done`, `agents_leave`), and says `CENTRAL UNREACHABLE
<context>: <reason>` once when the central instance stops answering, and its `ENDED` line once it
answers again; what was not published meanwhile is published then. A watch with nothing to
publish asks every five minutes.

A lane whose installation is central queues centrally too, so two people's merges into it roll
one after the other. The gate queues the merge in the central lane (`lane_queue`) as well as
the machine's, asks for its turn there every 15 seconds while it waits, and runs devctl only when
the merge is next in both. It makes it the central lane's running merge (`lane_settle`), reports
its outcome (merged: the lane settles; otherwise it leaves with `lane_leave`), and the watch takes
it out once its release rolled on the installation. A waiting place whose gate stopped asking
for `merge.queueTTL` holds up nobody. A hold on a central lane (`hold set --lane`) or on a
repository in one is central: `hold set|lift|check|list` go to the central instance, and its gate
refuses the merge with the hold (exit 77) on every machine; `--lift-when`, a probe on one
machine, is refused for a central target. `lanes central` lists the central lanes,
and `lanes leave owner/repo#n` takes a merge out of its central lane by hand (its person's, or the
team's supervisor role's). A gate that cannot reach the central instance refuses the merge with
exit 69 and queues nothing.

## Configuration

`$XDG_CONFIG_HOME/beekeeper/config.yaml` (or `--config`, or `$BEEKEEPER_CONFIG`). Every field is
optional, and an empty configuration works: `watch`, `snapshot`, `free`, the hook and the
coordination commands run with neutral defaults. Memory and disk thresholds default to fractions of
what the machine has, and every organisation or desk choice is unset until configured.
[`docs/examples/config.yaml`](docs/examples/config.yaml) is a complete desk's configuration, every
key annotated with what it guards and its default.

The organisation and desk keys, and their defaults:

| Key | Default | What it sets |
|---|---|---|
| `kube.production` | unset: the kube guard is off | The installation whose clusters agents never write to, and the no-current-context rule of the machine kubeconfig |
| `kube.contextTemplate` | unset: the context named after the installation, else `*@<name>` | An installation's kube context, `{installation}` its name (`login.example.com-{installation}`) |
| `alerts.installations`, `lanes`, `resources` | none | The installations whose alerts are read, the merge lanes, the leasable environments |
| `alerts.installations[].floor` | every severity | The lowest severity printed of an installation's alerts |
| `alerts.ignore` | `[Watchdog]` | Alert names that never appear |
| `alerts.quiet` | `[{severity: notify}]` once `alerts.team` is set, else none | Alerts whose changes are logged, not said |
| `alerts.tenant` | unset: only kube-prometheus-stack's Alertmanager | The Mimir tenant (`X-Scope-OrgID`) whose Alertmanager is read first |
| `github.probeRepo` | `giantswarm/beekeeper` | The repository whose conditional GET reads the budget |
| `github.floor` | `2500` | The budget under which GitHub work stops |
| `merge.devctlOwners` | none: every merge is the plain squash merge | The owners whose repositories `devctl pr merge` serves |
| `supervisor.skill`, `guide.skill`, `guide.person` | unset | The roles' skills and the person the guide walks through their notes |
| `claude.desktopApp` | `claude-desktop` | The desktop app's executable, which opens `claude://` links and starts the app |
| `shell` | `$SHELL`, else `sh` | The shell the hook runs a rewritten build, test or lint command in |
| `maxKindClusters` | one per 40 GiB of RAM, at least one | The kind clusters the hook lets the machine run |
| `memcap.max` | 14% of RAM | A `beekeeper run` command's MemoryMax |
| `watch.availMinMiB`, `watch.scopeAnonMaxMiB`, `watch.gttMaxMiB` | 12%, 32%, 28% of RAM | LOW RAM, DESKTOP SCOPE, IGPU GTT |
| `watch.swapMaxMiB`, `watch.oomdHeadroomMinMiB` | 60%, 6% of swap | SWAP (disk swap, zswap's share not counted, while it grows and MemAvailable falls), and the headroom under which OOMD IMMINENT is said (only while systemd-oomd watches a cgroup for swap) |
| `watch.toolProcsMax`, `watch.tools` | `1000`; kubectl, helm, tsh, gh, flux, devctl | LOAD over this many processes of these CLIs machine-wide; negative: off |
| `desktop.typingQuiet` | `30s` | How long the person's input stays idle before a `claude://` link switches the desktop's window; negative: links do not wait for it |
| `watch.tmpMaxMiB`, `watch.diskMinMiB` | 45% of `/tmp`, 5% of `/` | TMPFS, LOW DISK |
| `ollama.url`, `lemonade.url` | unset: no model server | The host's model servers, watched and guarded under the `model-server` lease |
| `outbound.phrases`, `outbound.paths`, `outbound.storeDeny` | none | What never leaves the machine, the plan files whose writes are outbound, the refused secret-store writes ([What leaves the machine](#what-leaves-the-machine)) |
| `secret.vault`, `secret.tokenFile` | none | The shared 1Password vault `beekeeper secret` reads and writes, and the file with its service account's token ([Secret operations](#secret-operations)) |
| `sandbox.allowRead`, `sandbox.allowWrite`, `sandbox.domains`, `sandbox.mask` | none; `GH_TOKEN` and `GITHUB_TOKEN` to GitHub | The paths under the home directory the agent sandbox re-allows for reading and writing, the hosts commands reach besides GitHub, the masked environment variables and their hosts ([The agent sandbox](#the-agent-sandbox)) |
| `scan.sops`, `scan.vaults`, `scan.minLength` | none, none, 12 | The SOPS file globs and 1Password vaults `beekeeper scan index` fingerprints, and the shortest value it takes ([What reaches the model](#what-reaches-the-model)) |
| `plans.repositories`, `plans.check` | none, `plan-stages` | The plans repositories whose open pull requests a note for `guide.person` links only once their stage check is green |
| `outbound.sweepRoots`, `outbound.sweepDepth` | the home directory, 5 | Where the watch looks for exposed keys and credentials in remote URLs |

State lives in `$XDG_STATE_HOME/beekeeper/` (`state.json`, which an older beekeeper still running
writes back with the fields it does not know at every level, `events.jsonl`, whose `at` is RFC 3339 in UTC while
`beekeeper log` prints local times, each caller's last
snapshot, the alert baseline `alerts.json` with its owner's `alerts.lock`, the notification ledger
`notify.json` with `notify.lock`) and leases in `leases/`, one directory per held resource.

## Development

See [docs/development.md](docs/development.md).
