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

`install` merges the PreToolUse and PermissionRequest hooks into Claude Code's user settings
(`~/.claude/settings.json`, or `$CLAUDE_CONFIG_DIR`) with the binary's absolute path, writes and
starts the [standby service](#desktop-notifications) (a systemd user unit, a launch agent on
macOS), and on systemd the memory guard sized to the machine's RAM: `memcap.slice` for `beekeeper
run`'s capped commands and a drop-in for the Claude Desktop scope that runs (run install again
with the app running when it does not). Without a config it writes a starter one, the [example
configuration](docs/examples/config.yaml) with every key commented out. A file already as install
writes it stays, and one that differs and that install did not write it keeps and names; a second
run changes nothing. A new Claude Code session picks the hooks up. `beekeeper uninstall` (also
with `--dry-run`) stops the service and removes exactly what install wrote, as `install.json` in
the state directory records it, and leaves the config and the state unless `--purge`. On Linux
without systemd install writes the hooks and the config and says the service is not available.

From then on `beekeeper self-update` keeps it current: it verifies the release binary's Sigstore
signature and renames it over the old one in one step, so a running `beekeeper watch` keeps
running, and a `devctl pr merge` gate call waiting for its turn re-executes the new binary at its
place. `beekeeper self-update --check` exits 125 while a newer release is out.

The session and machine views need Linux (`/proc`, cgroup v2, the journal). Leases, holds and
the budget work on any system.

### The role plugin

The commands are half of a desk; the roles that drive them are the other half. The repository is
also a Claude Code plugin marketplace whose plugin [`plugin/`](plugin) carries them as skills:
`/beekeeper:supervise` (the supervisor), `/beekeeper:guide` (the session that walks the person
through their decisions), `/beekeeper:register-agent` (an empty session joining the roster) and
`/beekeeper:worker-rules` (what every worker follows; a brief names it). Their text is generic:
the desk's installations, resources, lanes and person are the configuration's keys (below), and
the desk's own conventions — which board, which repositories, local traps — stay in the project
the sessions run in, which the skills defer to.

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
| `beekeeper status [--bar]` | One line: who supervises, the leases held, the holds in force and the notes and timers that are due. `--bar` prints it as the fixed tab-separated row a desktop bar reads (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper sessions` | Every running session: the issues and pull requests its latest turns are about, when it was last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left), its memory, how full its context is, its last hour (turns, tool calls and their errors, GitHub calls, cost), role and leases, after `archived` or `test` for a session the guide's feed leaves out. Overlaps name what more than one session is on. `--all` adds the paused ones: a message to them does not arrive. `--json` has every figure per session and their totals (see [Session metrics](#session-metrics)). |
| `beekeeper tail <session>` | A session's last turns without tool calls: what it said and what it was told. |
| `beekeeper snapshot` | One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots, kind clusters, the sessions' last hour and the three that spent the most in it, the commands sessions sit on, every kernel OOM kill since your last snapshot with whose limit it hit (a memcap scope no `run.start` names says `cap unknown`, never the default cap; a test run's scope is a test kill), leases, holds, the GitHub budget, the installations' alerts and their running upgrades (`upgrades: prod/mc 35.0.1 → 35.1.1 for 9m (control plane 3/4, node pools 15/15) · test none`). A caller inside a Claude session that has taken one before gets only what changed since, or one `no change` line; `--full` prints the whole screen and then what changed. |
| `beekeeper watch` | Silent until something needs a look, then one line: the source of a `Monitor`. A threshold breach, an unreadable source and a stalled lane are one line when they start and one `ENDED` line when they end, never repeated while they last; OOM kills, sessions that start, end or restart, stale leases and every NEW or RESOLVED alert of the installations are always reported. A note or timer that falls due, the end of a session with a record and a relay taken or expired are one line each, once: the state keeps that they were reported, so a second or restarted watch stays silent about them. Once the supervisor session's context (its transcript's last request, the `CTX` column) reaches `supervisor.relayAt` (400k tokens), `RELAY DUE` is said at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued, no relay open), once per supervisor and again only after a relay is cancelled or expires; `supervisor status` and `handover` show the supervisor's context. A recorded supervisor whose CLI has stayed gone past `supervisor.restartGrace` with no relay open is `SUPERVISOR GONE`, once: claims stay gated until a successor's `supervisor start`; a supervisor back after it is `SUPERVISOR BACK`, once; its CLI back under the same session with a new PID is `SUPERVISOR RESTARTED`. A session over a threshold in `metrics.runaway` is one `RUNAWAY` line per figure, once per watch. A fork rate `watch.forkRateMax` over the machine's usual one for two samples is one `PROCESS STORM` line, and more than `watch.stackMax` long-running copies of one command line one `STACKED` line each, with the commands, the parent and the owning session, and more than `watch.toolProcsMax` (1000) processes of the CLIs in `watch.tools` (kubectl, helm, tsh, gh, flux, devctl) machine-wide one `LOAD` line with their commands and the sessions that run them, and an `ENDED` line each. A stalled lane (see `lanes`) is one `LANE STALLED` line, repeated at most every 10 minutes while it lasts. A settling merge leaves its lane once its release rolled and the lane's HelmReleases are Ready, however late; one not settled past `merge.settleTimeout` is one `LANE STUCK` line with what the lane waits for (the HelmRelease off the release, the one not Ready, an installation that cannot be read), repeated at most every 10 minutes, and one `ENDED` line when it settles or the lane is cleared; `lanes` marks it `stuck since`. A cluster upgrade on an installation (see [Cluster upgrades](#cluster-upgrades)) is `UPGRADE <installation>/<cluster> <from> → <to>` when it begins and `UPGRADE ENDED … (since HH:MM)` when it ends, once each. The quiet rules keep what is noise for the supervisor out of its lines: other teams' alerts matching `alerts.quiet` (by default their e2e test clusters, `t-*`, and, once `alerts.team` is set, every other team's and team-less `notify` alert), an alert back after a reading that missed it with its old start (an Alertmanager restart, a reconnect), and the start, end and restart of the short-lived sessions in `watch.quietSessions` (by default beekeeper's tests, `test: *`). A rule never holds back an alert of `alerts.team`, one on an installation in play (leased, claimed or merged into within the last 30 minutes, or with a merge settling), or a page unless the rule names a cluster; everything else is said as before. What they hold back is logged (`beekeeper log --verb watch.quiet`) and counted in `snapshot`. `--notify` also sends the events that need a person to the desktop, `--standby` leaves a running supervisor's events to its watch (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper alerts watch\|snapshot\|import\|capture\|replay` | The installations' alerts, read from each Alertmanager through a bounded `kubectl port-forward` (Mimir's with the `giantswarm` tenant, else the plain one), in parallel: `watch` prints one line per NEW or RESOLVED alert since the baseline (pages and your team in capitals, a burst of one alertname as one line, one line when an installation stops or starts answering, the lease holder and the sessions working against it in brackets, and on a NEW line the merges into the installation's lanes and the lease claims on it of the last 30 minutes with their sessions, worded as timing (`during merging …`, `during merged … at …`), not as cause); `snapshot` the current set, grouped; `import` takes over another watcher's per-installation baseline. An alert below its installation's severity floor never appears in either, and an alert that keeps firing and resolving is one FLAPPING line, then quiet until it has been stable for the damper's window; neither a floor nor a damper change prints a burst of lines. `capture <dir>` records each installation's answer, `replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it: a floor or damper setting tried before the watch gets it. One process owns the baseline at a time, so two watches never split the lines. Every port-forward ends with the reading, on SIGINT or SIGTERM, and when beekeeper is killed. |
| `beekeeper budget` | The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), and every `gh` and `devctl` process with its session. `--gate` exits 3 under the floor. |
| `beekeeper lease claim\|release\|status\|grant\|revoke` | One holder per resource: the environments in the configuration, the host's model server (`model-server`, whose claim carries `--gib <n>`: the GiB its models may hold, see [The model server](#the-model-server)) and the browser. While a supervisor runs, a session claims only what the supervisor granted it, in grant order. While a cluster upgrade runs on the installation of the resource's name, nobody claims it except by the supervisor's `lease grant <installation> <session> --upgrade-unblock "<why>"`, for the work that unblocks the upgrade (see [Cluster upgrades](#cluster-upgrades)). |
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as a model load stops the lane whose components it exercises, not the others), every merge (`merges`) or every GitHub call (`github`) until lifted or a time passes. A merge hold lets one repository or pull request through with `--except owner/repo[#n]`: `hold set --lane serving --except giantswarm/model-manager#172` stops the lane but for the merge it waits for. A lane hold that excepts one pull request (`owner/repo#n`) is also its fix window: that merge runs without the lane's HelmReleases Ready or the previous release rolled, for the fix of a rollout only the fix can repair, logged as `merge.window`. The merge gate enforces them. |
| `beekeeper lanes [queue\|settle\|drop\|clear]` | Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones in turn order, so who is next is never prose. `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h; seeds keep their order, an arrived unseeded merge passes one whose merge has not arrived); a run with nothing merged stays in its place as `retrying` for its session's retry; `settle <owner/repo> <n> [--for <session>]` registers a merge run outside the gate (in flight when the gate went live, run without the hook): it heads its lane until it merges, then settles the lane like a gated merge; `drop` takes a waiting merge out (a place whose pull request merged or closed leaves by itself at the next `watch` poll); `clear` is the repair for a lane whose settling release will not roll, after a look at the installation. A lane where no merge runs and whose first arrived merge has waited longer than `merge.stallAfter` (5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left it) is `stalled`, with the waiting merge and those places named. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies from its start until a deliberate `supervisor stop` (which ends supervision and starts no successor) or a successor's `supervisor start`: a supervisor whose CLI crashed keeps it in force, its grants and queue stay recorded, and every claim waits for the successor. A CLI back under the same session id or desktop record within `supervisor.restartGrace` (30s) of beekeeper first seeing it gone (a claim or a watch's poll) is a restart and keeps the role and an open relay without a new start; `supervisor status` and `status` show it restarting, then gone. Another session's start is refused while it runs or restarts, unless the supervisor relayed the role to it; past the grace a successor's start takes the role. The start prints the holder's first look in a few KB: leases, holds, lanes, what waits on it (pinned and open notes, the guide's as a count, the decisions answered since the last relay, timers) and the agents with their tasks. A start that changes the holder runs `agents broadcast` in a transient unit, so every running agent learns the name that answers now. |
| `beekeeper supervisor reopen` | Open the recorded supervisor's desktop session, starting the app if needed (the login unit, see [Fresh successors](#fresh-successors-and-a-supervisor-started-again-without-a-click)). |
| `beekeeper supervisor relay [--cancel]` | Hand the role over without a gap: beekeeper starts the next run, "Supervisor run N+1", as a fresh session (as `agents start` does, so its desktop title, roster name and messaging name are one) and opens the relay to it; its first turn's `supervisor start` takes the role, the grant queue and the pending grants in one step, and the event log shows both. Until then the outgoing supervisor keeps the role and the grant rule; a relay not taken expires after `supervisor.relayTTL` (15m) or is withdrawn with `--cancel`, and a successor that does not start withdraws it. `supervisor status` in the relieved session exits 4, also after the successor relays onward, cancels a relay or is relieved in turn, until that session supervises again (or for 7 days). |
| `beekeeper guide start\|stop\|status\|relay` | The guide: the session that walks the person through the decisions waiting on them, next to the supervisor and never in its session (`guide start` in the supervisor's session and `supervisor start` in the guide's are refused, and neither relays to the other's holder). It has no grant power. Its role moves like the supervisor's (a relay to a fresh "Guide run N+1", its start, relief with exit 4, restart grace, a fresh successor after a crash) without touching the supervisor's record; `guide handover --prompt` is the successor's prompt (`guide.skill`, default `guide`, or `guide.instructions`, then its queue and an open relay). |
| `beekeeper guide queue` | The guide's queue: every open note filed for its person, `guide.person` (`note add --for`, or an older note's `[for <person>]` text prefix; any case), with its owning session (the one that filed it, and whether it still runs), deadline and default, then every session whose record says it waits on the person (`sessions serve … --waits "<person>: <ask>"`, the person in any case; with `guide.person` unset, every `--waits`), running or stopped (`(stopped)`), except the guide's own session, an archived one and a test (titled `test: …`). With `guide.person` unset, every note filed `--for` anyone, and a line that says so. Delta output like `sessions`. |
| `beekeeper guide watch [--once]` | The guide's feed, silent otherwise: `GUIDE DECISION` for each new open note of the queue (only `guide.person`'s; with it unset, every `--for` note and one `GUIDE:` line that says so), `GUIDE WAITING` for each session of the queue newly waiting on its person (only an explicit `--waits`: the desktop's turn summary does not count), `GUIDE ANSWERED` or `GUIDE CLOSED` for a note of the queue closed, and the guide's relay: `GUIDE RELAY DUE` once its context reaches `guide.relayAt` (150k), `GUIDE RELAY TAKEN`, `GUIDE RELAY EXPIRED`, `GUIDE RESTARTED`. Each is one line, once: what it said is kept in the state (`guide.fed`). |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. `register` names the session by its title (a `claude --bg` worker by its `-n` name) unless `--name` overrides it. A name is one agent's: registering under the name of a session that no longer runs (what `agents` shows as `not running`: stopped, closed or asleep) replaces its entry, saying so; a task the replaced entry left unfinished becomes the new entry's, with its assignment time, named in the `agents.register` event (`replaces <id>, takes over "<task>"`) and in the output, so the fresh session works it and a later `assign` to the name is refused as busy until `idle`. Re-registering keeps the session's own open task, so a session `agents start` registered busy stays busy when it registers itself (`register: <name> busy with "<task>"`); two dropped entries with open tasks are refused (exit 3). A name a running session's entry holds is refused (exit 3). `assign` and `remove` take a session id, a name or a unique part of one, and refuse a name several entries share. |
| `beekeeper agents start <name> <brief file> [--task t] [--model m] [--dir d]` | Starts an agent session without a click: `claude -p` in `bypassPermissions` under a session id beekeeper chooses, the brief as its first prompt, in a transient user unit `beekeeper-agent-<id>` (`KillMode=process`, the user manager's environment). Before the session exists it records the id and mode as one of beekeeper's starts (the state's `starts`, kept 30 days) and registers it on the roster under the name, busy from its start with `--task` (by default the brief's first line) or with the open task of a stopped entry under that name. Once the transcript holds the first reply it imports the session into Claude Desktop (`claude://resume?session=<id>`, sidebar row `local_<id>` titled with the name: beekeeper appends the name's `custom-title` line to the transcript first, since the import reads only its last 256 KiB) and switches the desktop back to the session it showed before (`claude://code/continue`), the one the desktop log (`claude.desktopLog`) last names as focused. See [Agents started without a click](#agents-started-without-a-click). |
| `beekeeper agents wake <agent> <message> [--permission-mode m]` | Messages a registered agent without Claude Desktop's `local_` route and its cap (a session's desktop sends pause after ten since its person last typed in it). A running CLI gets the message by name; a session with none is resumed headless, `claude -p --resume <id>` with the message as its turn, in its directory, permission mode (a start's bypass, else the desktop record's) and recorded model, in a transient unit `beekeeper-wake-<id>-<wake>`, one per wake; its `ExecStopPost` runs `agents reopen <local_ id>`, which warms the desktop's CLI again. `agents` shows `live, first turn running` or `live, wake turn running` while a headless turn is the session's CLI. See [Waking a session](#waking-a-session). |
| `beekeeper agents handover <agent> [--prompt] [--model m] [--dir d]` | Hands a registered agent over to a fresh session near its context limit, one line per step: asks it by peer message for `beekeeper agents note "<what is in flight, what is next>"` (waiting `agents.noteWait` at most), builds the follow-up's prompt, starts the follow-up as `agents start` does under the agent's name (it takes over the roster entry, the task and the session record), stops the old session's CLI and the processes under it by PID (a `claude --bg` session through `claude stop` first, so its daemon does not resume it), and logs `agents.handover`. `--prompt` prints the prompt only. `watch` says `HANDOVER DUE` once per agent session at `agents.relayAt`. See [Agents handed over near their context limit](#agents-handed-over-near-their-context-limit). |
| `beekeeper agents broadcast <message>` | Sends one message by name to every registered agent whose CLI runs, one after the other, the supervisor and the caller left out; a stopped agent is not resumed for it. One `agents.broadcast` event logs whom it reached. |
| `beekeeper agents note <text>` | The calling agent's hand-over note, logged as an `agents.note` event; the next `agents handover` puts the latest one into the follow-up's prompt. |
| `beekeeper note add\|answer\|done\|pin\|unpin` | Open items that outlive a session: a decision waiting on a person with its deadline and what happens if nobody answers (`note add --for Ada --due 22:55 --status-quo "<what is true now>" --why "<why it needs Ada>" --default "the alert stays as is" <text>`), a deadline. A note for `guide.person` (any `--for` with it unset) is refused, naming what it lacks, without `--status-quo`, `--why`, a `--default` that is an action (not `wait` or `none`), every `--option` as `<choice>: <consequence>`, the full URL of every `#N` or `owner/repo#N`, and `--checked "<source>"` for a claim that something is merged, green, released, rolled or closed; one on an issue or PR an open note for the same person names, with the same verb (the text's first word), folds into it (`note.folded`). `--kind login --until "<probe>"`: the watch runs the probe each tick and closes the note once it exits 0. Notes without `--for` are memos, unchecked. A note for a person asking again what was answered for them within 72 hours (the same verb on one of the same issues or PRs) is filed with a warning that quotes the answer. `--pin` (or `note pin <id>`) makes a note a standing instruction: every hand-over, the supervisor's and the guide's, carries it until `note unpin` or `note done`. `watch` reports a note once when it is due. `note answer <id> <answer>` records the person's answer word for word and closes the note; the `note.answered` event carries it for the owning session, the supervisor and the guide's feed. |
| `beekeeper reporter final\|pause\|resume` | Pauses the scheduled reporter: `final 06:45` (or `45m`) starts one last report at that time, covering the time since the last one, then pauses; `pause` pauses now; `resume` starts the current slot's report at the standby watch's next poll and the schedule again. |
| `beekeeper report [--since 1h\|15:04] [--until 15:04] [--tz zone]` | Renders the status report from beekeeper's own state, as Markdown that passes `reporter check`: the pull requests and promotions the gate merged in the window with title, release and rollout; the running work (roles, busy agents, sessions serving an issue) with the issue, what it waits on and its leases; what waits on `reporter.person` (open notes per owning session, sessions waiting on an answer, the open drafts of `reporter.reviews`); the merge lanes, timers and holds; the machine. Every time in `reporter.tz` (`--tz`; default the machine's zone); one GitHub request for the titles and drafts. See [The scheduled status reporter](#the-scheduled-status-reporter). |
| `beekeeper reporter check` | Checks a report on stdin as the reporter's post hook does (see [The scheduled status reporter](#the-scheduled-status-reporter)): one line per problem and exit 3, or `ok`. |
| `beekeeper reporter` | The scheduled status reporter (see [The scheduled status reporter](#the-scheduled-status-reporter)): its schedule, when the next one starts, and the current or last run with its outcome. |
| `beekeeper timer add\|done\|list` | Times to look at something: `timer add 22:55 "check the rollout"` (or a duration, `45m`). `watch` prints one line when a timer is due; it stays open until `timer done`. A timer can wait on a condition from its time instead: `--when "pr-merged owner/repo#n"` (also `issue-closed owner/repo#n`, `helmrelease-ready context/namespace/name`, `controlplane-ready context/namespace/name`) or `--probe "<command>"` (exit 0: it holds), checked by the watch every `--every` (5m), each distinct check once per tick however many timers share it and a GitHub one only above the budget floor; it fires within one tick of the condition holding. `--until` ends the wait: the timer then fires as timed out, or with `--expire` closes unfired (`TIMER EXPIRED`). `--wake <agent>` wakes the agent with the timer's text (as `agents wake`; `TIMER WAKE FAILED` when it cannot), `--run "<command>"` runs a command, its output in `timers/<id>.log` of the state directory and its exit code logged (`timer.ran`, `TIMER RAN`); without either the `TIMER` line is for the supervisor. Such a timer closes when it fires. |
| `beekeeper ps [name\|pid...]` | The process table with every command line masked: PID, parent, age, CPU time, resident memory, and the command line as the watch's `STACKED` line prints it (the program, its subcommands and flag names; flag values, `key=value` words, URLs and what follows a short option's letter left out). A masked line ends in `(masked)`, so a left-out value is never read as an empty one. A number narrows the list to a PID, a word to the processes whose masked line or name holds it. `--json` has the rows. The hook's refusal of a whole command-line read names it ([Secret reads](#secret-reads)). |
| `beekeeper sessions serve\|unserve` | A record for any session, registered agent or not: `sessions serve <session> <owner/repo#n> [--waits "<what>"]`, the issue or epic it serves and what it waits on. `sessions` and `handover` show it; `watch` prints one line when the session ends, naming the issue to re-query. |
| `beekeeper handover [--prompt] [--section <name>]` | Everything the next supervisor needs, as Markdown, from the live state: leases and grants, holds and their exceptions, agents, session records, pinned notes, notes with their defaults, the decisions answered since the last relay, timers, the merge lanes with their queues and settling merges, and what the alert watch reads (the installations and why, the ignored alert names, the baseline). `--prompt` prints the successor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope, the pending state and the commands that read the live values; no live value (version, memory figure, pull request state). Both show what a successor acts on in its first minutes: the notes the guide serves (for `guide.person` and for the guide) are one count line, records of sessions ended over an hour ago one line, and the answered decisions those since the predecessor's start with a count of the rest of 72 hours. `--section <name>` prints one section with everything it holds (`notes`, `records`, `answers`, …). |
| `beekeeper log [--verb PREFIX]` | Every claim, grant, hold, registration, note, timer, session record and build run, as they happened; `--verb run.` shows only the runs. |
| `beekeeper run [--max SIZE] [--wait DURATION] -- <command>` | Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the `memcap` wrapper) inside a memory-capped systemd scope. When every slot is held it waits once, then exits 75 with the holders; when the cap fires the kernel kills the biggest process in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap killed:`. Each run leaves `run.start` and `run.end` in `beekeeper log` with its scope, session and command, so `snapshot` and `watch` name a cap kill's session and command after the run has ended. `MEMCAP_TEST=1` marks a test's run: its scope is `memcap-test-…`, and a kill in it is reported as a test kill (one quiet `test kill:` line in `watch`), never as a build's. |
| `beekeeper hook pretooluse` | The PreToolUse hook (matcher `Bash|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|mcp__.*`): rewrites build, test, lint and lab commands to `<this binary> run -- zsh -c '<command>'` with the command verbatim and the tool timeout at 10 minutes (a background run waits 60 minutes), and refuses a third kind cluster, listing the held leases. It puts the gate in front of every `devctl pr merge`, `pr wait`, `release wait` and `rollout wait`, behind prefix commands and in pipelines and lists, and refuses one hidden in a `-c` string (below); every other devctl command passes untouched. It refuses a command that would print secret values (below, [Secret reads](#secret-reads)) and a call that would send one off the machine ([What leaves the machine](#what-leaves-the-machine)), a kube context switch, a write to production and `op item get` ([Kube contexts, production writes and op item get](#kube-contexts-production-writes-and-op-item-get)), a command that opens a page in the person's browser (`muster auth login`, `gh auth login --web`, `xdg-open`) outside the session holding the `browser` lease, a command that loads a model on the host's model server outside the session holding `model-server` ([The model server](#the-model-server)), and an `AskUserQuestion` call outside the guide's session ([Questions go to the guide](#questions-go-to-the-guide)). A `SendMessage` to a desktop id (`local_…`) whose session has a running CLI goes to that CLI by name, so a headless turn gets no second copy beside it (below, [Waking a session](#waking-a-session)). A session's first write or `git commit` in another repository carries that repository's `CLAUDE.md`, `AGENTS.md`, rules and mandatory reads ([A repository's instructions on the first write](#a-repositorys-instructions-on-the-first-write)). |
| `beekeeper hook permissionrequest` | The PermissionRequest hook: answers `allow` only for a session `agents start` started in bypass that now runs in `acceptEdits`; every other request gets no answer, so the person sees the normal card. Below. |
| `beekeeper free [--apply] [--only SECTIONS] [--summary]` | Show where the memory is and, with `--apply`, free what no running work needs: dead sessions' dirs in the tmpfs `/tmp` (the CLI is gone; an idle session's stay), throwaway temp dirs and orphaned jest or Claude workers. Kind clusters, idle CLIs, heavy or runaway processes and Chrome renderers are only reported. Never runs as root: the swap reset and root-owned leftovers are printed as the commands to run. `--summary` prints the TSV rows a desktop front end parses. |
| `beekeeper self-update` | Install the latest signed release over this binary; `--check` only asks. |

Every reading command is written for an agent whose context is its scarcest resource: it prints only what the caller has not read. `snapshot`, `sessions`, `lanes`, `agents` and `handover` keep a read mark per caller and command (`seen.<command>.<caller>.json` in the state directory; `snapshot.<caller>.json` for `snapshot`): a caller that has read before gets the facts that are new or changed since, the keys of those gone (`gone: …`), or one `no change since HH:MM` line. A moving figure (an age, a countdown, a memory size, a context in tokens) is no change. `--full` prints everything, `--json` is always complete, `handover --prompt` is always whole (a successor has read nothing), and a person at a terminal (no Claude session) always gets the whole text. `watch` says each event once and each lasting condition when it starts and when it ends, across restarts: it keeps its open conditions and one-time events (runaways, stale leases) in the caller's mark (`seen.watch.<caller>.json`), so a watch restarted after an install resumes silently and says only an `ENDED` or what is new; `watch --once` keeps no mark and says every condition it finds. Its lines carry no restated thresholds or explanations, those stay in `--help` and here.

Marks are per host session (`CLAUDE_CODE_HOST_SESSION_ID`, else the session): a subagent or `claude -p` worker inside a session shares its parent's marks, so its reads advance what the parent will see as read. A subagent reading beekeeper passes `--full` or `--json`.

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor), 4
relieved (`supervisor status` in the session a relay relieved), 125 a newer release is out
(`self-update --check`). A claim gates the action it guards:
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

## Kube contexts, production writes and op item get

With `kube.production` set, the machine kubeconfig (`~/.kube/config`) keeps no current context: a
`kubectl`, `helm` or `flux` command without an explicit target fails instead of reaching production.
The PreToolUse hook keeps it that way and refuses, with the reason and the explicit form (unset, the
kube guard is off, `beekeeper snapshot` says so, and only the `op item get` refusal stays):

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
- `op item get` in every form: `op read op://<vault>/<item>/<field>` into a pipe or a file, or `op run`.

Each guard sees through prefix commands, pipelines and lists, `$( )`, shell `-c` strings and
here-documents fed to a shell, and kubectl or a kubectl plugin run through a shell function or a variable. Quoted text
(a commit message, an issue body) passes.

## Secret reads

A value a command prints lands in the session's transcript and goes to the model API with the next
turn; one printed credential is one rotation. The PreToolUse hook refuses a Bash command that would
print secret values, names the part that would, and gives the safe forms:

- `kubectl get` of Secrets (by kind, `secret/<name>`, or in a list such as `cm,secret`) with
  `-o yaml|json`, or with a `jsonpath`, `go-template` or `custom-columns` template that prints the data
  or the whole object; `kubectl view-secret`; also through a shell function or variable that runs
  kubectl (`kc(){ kubectl --context x "$@"; }`, `K="kubectl …"`), and `kubectl get -o yaml` of names
  piped in from a Secret listing.
- `sops -d` / `sops decrypt` to stdout; `op read`, `op inject` and `op document get` without
  `--out-file`, `op run --no-masking` (`op item get` is refused in every form, [Kube contexts, production writes and op item get](#kube-contexts-production-writes-and-op-item-get)); `vault kv get` and
  `vault read`; `base64 -d` of a secret's `.data`.
- Whole command lines and environments, readable by every process: a program started with
  `-e PASSWORD=…`, `--token …` or `-p …` carries the credential there. `ps` with its `args`/`cmd`/`command`
  column (`ps aux`, `ps -ef`, `ps -eo pid,args`, BSD-style words without `c`, BSD `e`), `pgrep -a`,
  `pstree -a`, a read of `/proc/<pid>/cmdline` or `environ` (`ls`, `stat` and `wc` pass), `docker|podman
  inspect` without a `--format` limited to names and state, and `docker ps --no-trunc` with the
  command column. The safe forms: `beekeeper ps` (below), `ps -eo pid,ppid,etime,time,rss,comm`,
  `pgrep -l`, `docker inspect --format '{{.Name}} {{.State.Status}}'`.
- The same inside `sh|bash|zsh -c`, `ssh`, `eval` and `watch` strings and here-documents fed to a
  shell. Quoted text, comments and other here-documents (a commit message, an issue body) are not
  commands and pass.

A command passes when the values never reach the terminal: the pipeline ends in a hash (`sha256sum`,
`md5sum`, `openssl dgst`), `wc`, `grep -q|-c`, a `jq`/`yq` filter that keeps only keys or metadata
(`jq '.data|keys'`, `.items[].metadata.name`, `(.value|length)`), a consumer that prints nothing of it
(`kubectl apply -f -`, `--password-stdin`, `gh secret set`, `sops -e`, `openssl x509 -noout`,
`age-keygen -y`), or a file (`> file`, `sops -d --output`, `op read --out-file`); or when the output is
captured in a variable or a flag's value (`T=$(…)`, `--from-literal=k=$(…)`). A template ranging over
`.data` that prints only the keys passes, as do `-o name`, `-o wide`, the table and `kubectl describe
secret` (sizes only). Anything else that reads a secret is refused: false positives beat leaks, and
the refusal says how to write the command safely.

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
credentials do not belong in a shared store.

The watch sweeps `outbound.sweepRoots` (the home directory) `outbound.sweepDepth` (5) levels deep every
interval for world-readable private keys (`id_*`, `*.pem`, `*.key`, `*.ppk` with a private key header)
and git repositories whose remote URL carries a credential, skipping caches and dependency trees: one
`EXPOSED <path>: <what>` line each, never the content, and an ENDED line once it is fixed.

## Questions go to the guide

A question put to the person with `AskUserQuestion` blocks the session until the person answers, and
the person talks to one session only: the guide's. The PreToolUse hook refuses `AskUserQuestion` in
every session but the one holding the guide role (`beekeeper guide`, matched by the CLI session id
or the desktop id), and tells the agent to file the question for the guide and carry on:

```
beekeeper note add --for <guide.person> "<the question, every issue or PR as its full URL>" --status-quo "<what is true now>" --why "<why it needs them>" --option "<choice>: <consequence>" --default "<the action if nobody answers>"
```

With no guide running, every session is refused. The hook reads the state only for this tool, and a
state it cannot read counts as not the guide.

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

A merge of giantswarm/devctl opens a tool-release window by itself: a `merges` hold with
giantswarm/devctl excepted, since the release makes every in-flight devctl run refuse until
updated. It lifts once no devctl merge runs and the local `devctl version` reports another
version than when the window opened, or once its pull request did not merge: the gate lifts it
after a run with nothing merged, and `watch` (or the next gate call) asks GitHub about a window
whose merge ended unrecorded (its gate killed) and lifts it when the pull request is open or
closed.

The budget floor uses the last reading in the state when it is younger than `merge.budgetFresh`
(1m, less than one merge's draw at the floor's margin), else a fresh conditional request, which
costs nothing when GitHub answers 304; a failed read refuses.

## Cluster upgrades

While an installation's clusters upgrade, work on it waits. `watch` reads the Cluster API
clusters of `alerts.installations` every `watch.interval` (30s), read-only: one list each of
Clusters, KubeadmControlPlanes, MachinePools and MachineDeployments (`v1beta2`) per
installation, in parallel, each installation within `alerts.timeout`; an installation that
serves none of them has no cluster to upgrade. That is 4 lists per installation per 30s, and one
list of a cluster's events when its upgrade begins.

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

Nothing routine notifies: sessions starting or ending, alerts, relays. The machine's lines never
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
  A session between beekeeper's start or wake and its desktop CLI (its headless turn, or the
  reopen after it) is not gone. Claims stay gated until the successor's `supervisor start`.
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
  of resumed workers after a login is the supervisor's call against the machine's memory.

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
shape as the rows of `free --summary`, so one bar module reads both. The contract is fixed: five
tab-separated fields, always present, in this order.

| Field | Value |
|---|---|
| 1 | `beekeeper`, the row's key |
| 2 | the supervising session's name; `-` when none is recorded; `!<name>` when the recorded one's session no longer runs |
| 3 | the number of leases held |
| 4 | the number of holds in force |
| 5 | the number of notes and timers whose time has come |

### Agents started without a click

`beekeeper agents start <name> <brief file>` starts an agent the way a person would start a
session and hand it a brief, without the click. The first turn runs the brief from the command
line in bypass, and the roster shows it busy with its `--task` from the moment it is started. The session is then a desktop session as well: the person reads and answers it in the sidebar.

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
a session its person started. A title set that way is the desktop's "set
by an agent", which its own titling never overwrites.

`agents remove` archives the removed agent's desktop session the same way (`archive_session`;
the desktop's Archived list brings it back), and prints and logs (`agents.archive`) what it did.
It does so only for a session beekeeper started that holds no role and runs no turn;
`--keep-desktop` leaves it in the sidebar. When no steward is idle it says so and the removal
still stands.

`beekeeper doctor` does these chores by rule, and every watch poll (not `--once`) runs it in the
background, one `DOCTOR` line per thing it did. It takes an agent off the roster once it reported
its work finished with `agents idle --done` and its CLI runs no turn, once it was relieved of the
supervisor's or the guide's role (a relieved role relays, it never hands over), or once it stayed
idle `agents.staleAfter` (24h) with no CLI running; the desktop sessions beekeeper started for the
finished and the stale ones are archived in one steward's turn, under the rules above. A session
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
`claude://` link twice, and the second of the two concurrent imports records no model: until
the desktop handles a link once, the desktop turns run on the desktop's default model, and the
start says so.

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
`TimeoutStopSec` covers the wait). A reopen that waits it out is recorded as missed. Without a
Hyprland session there is no focus to ask, and the links go at once.

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

Install it in `~/.claude/settings.json`, next to the PreToolUse hook:

```json
"PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command",
  "command": "~/.go/bin/beekeeper hook permissionrequest", "timeout": 10}]}]
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
flight. It says it once per agent session, across the watch's restarts. An agent holding the
supervisor's or the guide's role moves by relay instead.

The supervisor then runs `beekeeper agents handover "<agent>"`. It refuses (exit 3, before it
asks for anything) unless a PermissionRequest hook for every tool runs `beekeeper hook
permissionrequest` where the follow-up starts: in the user settings or in the `.claude/settings.json`
or `settings.local.json` of its folder or of the checkout that folder is in. Without it the
follow-up would stop at its first card after the old session was stopped. Otherwise it prints one
line per step:

1. **The note.** A peer message asks the agent to record what is in flight and what is next with
   `beekeeper agents note "<...>"` and end its turn; the hand-over waits `agents.noteWait` (3m)
   for it and carries on without it after that. The ask is one headless relay turn; a turn that
   called no SendMessage sent nothing and is run again, three turns at most. A failed send changes
   nothing.
2. **The prompt.** `beekeeper agents handover --prompt "<agent>"` prints it: the roster task (of an
   idle agent the last task it reported idle on, which a repeated `agents idle` or a re-register
   keeps), the brief the agent was started with (a follow-up's brief is its predecessor's, not the prompt
   around it), its `sessions serve` record, its gated merges, its last events and its note. No
   standing rules and no live values: the follow-up is told to check the live state first.
3. **The start.** The follow-up is started as `agents start` starts one, in the old session's
   folder and model, under the agent's name: it takes over the roster entry, the task and the
   session record, busy with the task. It shows in the sidebar and runs its first turn from the prompt alone.
4. **The end.** Once the follow-up's transcript holds its first reply, the old session's CLI and every
   process under it get SIGTERM, then SIGKILL after 10 s, by PID: the desktop shows it stopped.
5. **The log.** One `agents.handover` event in `beekeeper log`.

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
| `watch.swapMaxMiB`, `watch.oomdHeadroomMinMiB` | 60%, 6% of swap | SWAP, and the headroom under which OOMD IMMINENT is said |
| `watch.toolProcsMax`, `watch.tools` | `1000`; kubectl, helm, tsh, gh, flux, devctl | LOAD over this many processes of these CLIs machine-wide; negative: off |
| `desktop.typingQuiet` | `30s` | How long the person's input stays idle before a `claude://` link switches the desktop's window; negative: links do not wait for it |
| `watch.tmpMaxMiB`, `watch.diskMinMiB` | 45% of `/tmp`, 5% of `/` | TMPFS, LOW DISK |
| `ollama.url`, `lemonade.url` | unset: no model server | The host's model servers, watched and guarded under the `model-server` lease |
| `outbound.phrases`, `outbound.paths`, `outbound.storeDeny` | none | What never leaves the machine, the plan files whose writes are outbound, the refused secret-store writes ([What leaves the machine](#what-leaves-the-machine)) |
| `outbound.sweepRoots`, `outbound.sweepDepth` | the home directory, 5 | Where the watch looks for exposed keys and credentials in remote URLs |

State lives in `$XDG_STATE_HOME/beekeeper/` (`state.json`, which an older beekeeper still running
writes back with the fields it does not know, `events.jsonl`, whose `at` is RFC 3339 in UTC while
`beekeeper log` prints local times, each caller's last
snapshot, the alert baseline `alerts.json` with its owner's `alerts.lock`, the notification ledger
`notify.json` with `notify.lock`) and leases in `leases/`, one directory per held resource.

## Development

See [docs/development.md](docs/development.md).
