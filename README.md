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

From then on `beekeeper self-update` keeps it current: it verifies the release binary's Sigstore
signature and renames it over the old one in one step, so a running `beekeeper watch` keeps
running, and a `devctl pr merge` gate call waiting for its turn re-executes the new binary at its
place. `beekeeper self-update --check` exits 125 while a newer release is out.

The session and machine views need Linux (`/proc`, cgroup v2, the journal). Leases, holds and
the budget work on any system.

## What it does

| Command | For |
|---|---|
| `beekeeper status [--bar]` | One line: who supervises, the leases held, the holds in force and the notes and timers that are due. `--bar` prints it as the fixed tab-separated row a desktop bar reads (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper sessions` | Every running session: the issues and pull requests its latest turns are about, when it was last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left), its memory, how full its context is, its last hour (turns, tool calls and their errors, GitHub calls, cost), role and leases, after `archived` or `test` for a session the guide's feed leaves out. Overlaps name what more than one session is on. `--all` adds the paused ones: a message to them does not arrive. `--json` has every figure per session and their totals (see [Session metrics](#session-metrics)). |
| `beekeeper tail <session>` | A session's last turns without tool calls: what it said and what it was told. |
| `beekeeper snapshot` | One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots, kind clusters, the sessions' last hour and the three that spent the most in it, the commands sessions sit on, every kernel OOM kill since your last snapshot with whose limit it hit (a memcap scope no `run.start` names says `cap unknown`, never the default cap; a test run's scope is a test kill), leases, holds, the GitHub budget, the installations' alerts and their running upgrades (`upgrades: prod/mc 35.0.1 → 35.1.1 for 9m (control plane 3/4, node pools 15/15) · test none`). A caller inside a Claude session that has taken one before gets only what changed since, or one `no change` line; `--full` prints the whole screen and then what changed. |
| `beekeeper watch` | Silent until something needs a look, then one line: the source of a `Monitor`. A threshold breach, an unreadable source and a stalled lane are one line when they start and one `ENDED` line when they end, never repeated while they last; OOM kills, sessions that start, end or restart, stale leases and every NEW or RESOLVED alert of the installations are always reported. A note or timer that falls due, the end of a session with a record and a relay taken or expired are one line each, once: the state keeps that they were reported, so a second or restarted watch stays silent about them. Once the supervisor session's context (its transcript's last request, the `CTX` column) reaches `supervisor.relayAt` (400k tokens), `RELAY DUE` is said at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued, no relay open), once per supervisor and again only after a relay is cancelled or expires; `supervisor status` and `handover` show the supervisor's context. A recorded supervisor whose CLI has stayed gone past `supervisor.restartGrace` with no relay open is `SUPERVISOR GONE`, once: claims stay gated until a successor's `supervisor start`; a supervisor back after it is `SUPERVISOR BACK`, once; its CLI back under the same session with a new PID is `SUPERVISOR RESTARTED`. A session over a threshold in `metrics.runaway` is one `RUNAWAY` line per figure, once per watch. A stalled lane (see `lanes`) is one `LANE STALLED` line, repeated at most every 10 minutes while it lasts. A settling merge leaves its lane once its release rolled and the lane's HelmReleases are Ready, however late; one not settled past `merge.settleTimeout` is one `LANE STUCK` line with what the lane waits for (the HelmRelease off the release, the one not Ready, an installation that cannot be read), repeated at most every 10 minutes, and one `ENDED` line when it settles or the lane is cleared; `lanes` marks it `stuck since`. A cluster upgrade on an installation (see [Cluster upgrades](#cluster-upgrades)) is `UPGRADE <installation>/<cluster> <from> → <to>` when it begins and `UPGRADE ENDED … (since HH:MM)` when it ends, once each. `--notify` also sends the events that need a person to the desktop, `--standby` leaves a running supervisor's events to its watch (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper alerts watch\|snapshot\|import\|capture\|replay` | The installations' alerts, read from each Alertmanager through a bounded `kubectl port-forward` (Mimir's with the `giantswarm` tenant, else the plain one), in parallel: `watch` prints one line per NEW or RESOLVED alert since the baseline (pages and your team in capitals, a burst of one alertname as one line, one line when an installation stops or starts answering, the lease holder and the sessions working against it in brackets, and on a NEW line the merges into the installation's lanes and the lease claims on it of the last 30 minutes with their sessions, worded as timing (`during merging …`, `during merged … at …`), not as cause); `snapshot` the current set, grouped; `import` takes over another watcher's per-installation baseline. An alert below its installation's severity floor never appears in either, and an alert that keeps firing and resolving is one FLAPPING line, then quiet until it has been stable for the damper's window; neither a floor nor a damper change prints a burst of lines. `capture <dir>` records each installation's answer, `replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it: a floor or damper setting tried before the watch gets it. One process owns the baseline at a time, so two watches never split the lines. Every port-forward ends with the reading, on SIGINT or SIGTERM, and when beekeeper is killed. |
| `beekeeper budget` | The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), and every `gh` and `devctl` process with its session. `--gate` exits 3 under the floor. |
| `beekeeper lease claim\|release\|status\|grant\|revoke` | One holder per resource: the environments in the configuration and the browser. While a supervisor runs, a session claims only what the supervisor granted it, in grant order. While a cluster upgrade runs on the installation of the resource's name, nobody claims it except by the supervisor's `lease grant <installation> <session> --upgrade-unblock "<why>"`, for the work that unblocks the upgrade (see [Cluster upgrades](#cluster-upgrades)). |
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as a model load stops the lane whose components it exercises, not the others), every merge (`merges`) or every GitHub call (`github`) until lifted or a time passes. A merge hold lets one repository or pull request through with `--except owner/repo[#n]`: `hold set --lane serving --except giantswarm/model-manager#172` stops the lane but for the merge it waits for. The merge gate enforces them. |
| `beekeeper lanes [queue\|settle\|drop\|clear]` | Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones in turn order, so who is next is never prose. `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h; seeds keep their order, an arrived unseeded merge passes one whose merge has not arrived); a run with nothing merged stays in its place as `retrying` for its session's retry; `settle <owner/repo> <n> [--for <session>]` registers a merge run outside the gate (in flight when the gate went live, run without the hook): it heads its lane until it merges, then settles the lane like a gated merge; `drop` takes a waiting merge out; `clear` frees a lane whose settling release will not roll, after a look at the installation. A lane where no merge runs and whose first arrived merge has waited longer than `merge.stallAfter` (5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left it) is `stalled`, with the waiting merge and those places named. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies from its start until a deliberate `supervisor stop` (which ends supervision and starts no successor) or a successor's `supervisor start`: a supervisor whose CLI crashed keeps it in force, its grants and queue stay recorded, and every claim waits for the successor. A CLI back under the same session id or desktop record within `supervisor.restartGrace` (30s) of beekeeper first seeing it gone (a claim or a watch's poll) is a restart and keeps the role and an open relay without a new start; `supervisor status` and `status` show it restarting, then gone. Another session's start is refused while it runs or restarts, unless the supervisor relayed the role to it; past the grace a successor's start takes the role. |
| `beekeeper supervisor spare <session>\|--clear` | Record the relay spare: the standby watch keeps it awake and hands it the role after a crash (see [The spare](#the-spare-and-a-supervisor-started-again-without-a-click)). `supervisor reopen` opens the recorded supervisor's desktop session, starting the app if needed (the login unit). |
| `beekeeper supervisor relay <successor>\|--cancel` | Hand the role over without a gap: the supervisor names its successor, the successor's `supervisor start` takes the role, the grant queue and the pending grants in one step, and the event log shows both. Until then the outgoing supervisor keeps the role and the grant rule; a relay not taken expires after `supervisor.relayTTL` (15m) or is withdrawn with `--cancel`. `supervisor status` in the relieved session exits 4, also after the successor relays onward, cancels a relay or is relieved in turn, until that session supervises again (or for 7 days). |
| `beekeeper guide start\|stop\|status\|relay <successor>` | The guide: the session that walks the person through the decisions waiting on them, next to the supervisor and never in its session (`guide start` in the supervisor's session and `supervisor start` in the guide's are refused, and neither relays to the other's holder). It has no grant power. Its role moves like the supervisor's (relay, successor's start, relief with exit 4, restart grace) without touching the supervisor's record; `guide handover --prompt` is the successor's prompt (`guide.skill`, default `guide`, or `guide.instructions`, then its queue and an open relay). |
| `beekeeper guide queue` | The guide's queue: every open note filed for its person, `guide.person` (`note add --for`, or an older note's `[for <person>]` text prefix; any case), with its owning session (the one that filed it, and whether it still runs), deadline and default, then every session whose record says it waits on the person (`sessions serve … --waits "<person>: <ask>"`, the person in any case; with `guide.person` unset, every `--waits`), running or stopped (`(stopped)`), except the guide's own session, an archived one and a test (titled `test: …`). With `guide.person` unset, every note filed `--for` anyone, and a line that says so. Delta output like `sessions`. |
| `beekeeper guide watch [--once]` | The guide's feed, silent otherwise: `GUIDE DECISION` for each new open note of the queue (only `guide.person`'s; with it unset, every `--for` note and one `GUIDE:` line that says so), `GUIDE WAITING` for each session of the queue newly waiting on its person (only an explicit `--waits`: the desktop's turn summary does not count), `GUIDE ANSWERED` or `GUIDE CLOSED` for a note of the queue closed, and the guide's relay: `GUIDE RELAY DUE` once its context reaches `guide.relayAt` (400k), `GUIDE RELAY TAKEN`, `GUIDE RELAY EXPIRED`, `GUIDE RESTARTED`. Each is one line, once: what it said is kept in the state (`guide.fed`). |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. `register` names the session by its title (a `claude --bg` worker by its `-n` name) unless `--name` overrides it. A name is one agent's: registering under the name of a session that no longer runs (what `agents` shows as `not running`: stopped, closed or asleep) replaces its entry, saying so; a task the replaced entry left unfinished becomes the new entry's, with its assignment time, named in the `agents.register` event (`replaces <id>, takes over "<task>"`) and in the output, so the fresh session works it and a later `assign` to the name is refused as busy until `idle`. Re-registering keeps the session's own open task, so a session `agents start` registered busy stays busy when it registers itself (`register: <name> busy with "<task>"`); two dropped entries with open tasks are refused (exit 3). A name a running session's entry holds is refused (exit 3). `assign` and `remove` take a session id, a name or a unique part of one, and refuse a name several entries share. |
| `beekeeper agents start <name> <brief file> [--task t] [--model m] [--dir d]` | Starts an agent session without a click: `claude -p` in `bypassPermissions` under a session id beekeeper chooses, the brief as its first prompt, in a transient user unit `beekeeper-agent-<id>` (`KillMode=process`, the user manager's environment). Before the session exists it records the id and mode as one of beekeeper's starts (the state's `starts`, kept 30 days) and registers it on the roster under the name, busy from its start with `--task` (by default the brief's first line) or with the open task of a stopped entry under that name. Once the transcript holds the first reply it imports the session into Claude Desktop (`claude://resume?session=<id>`, sidebar row `local_<id>` titled with the name: beekeeper appends the name's `custom-title` line to the transcript first, since the import reads only its last 256 KiB) and switches the desktop back to the session it showed before (`claude://code/continue`), the one the desktop log (`claude.desktopLog`) last names as focused. See [Agents started without a click](#agents-started-without-a-click). |
| `beekeeper agents handover <agent> [--prompt] [--model m] [--dir d]` | Hands a registered agent over to a fresh session near its context limit, one line per step: asks it by peer message for `beekeeper agents note "<what is in flight, what is next>"` (waiting `agents.noteWait` at most), builds the follow-up's prompt, starts the follow-up as `agents start` does under the agent's name (it takes over the roster entry, the task and the session record), stops the old session's CLI and the processes under it by PID (a `claude --bg` session through `claude stop` first, so its daemon does not resume it), and logs `agents.handover`. `--prompt` prints the prompt only. `watch` says `HANDOVER DUE` once per agent session at `agents.relayAt`. See [Agents handed over near their context limit](#agents-handed-over-near-their-context-limit). |
| `beekeeper agents note <text>` | The calling agent's hand-over note, logged as an `agents.note` event; the next `agents handover` puts the latest one into the follow-up's prompt. |
| `beekeeper note add\|answer\|done` | Open items that outlive a session: a decision waiting on a person with its deadline and what happens if nobody answers (`note add --for Timo --due 22:55 --default "the alert stays as is" <text>`), a deadline. `watch` reports a note once when it is due. `note answer <id> <answer>` records the person's answer word for word and closes the note; the `note.answered` event carries it for the owning session, the supervisor and the guide's feed. |
| `beekeeper reporter final\|pause\|resume` | Pauses the scheduled reporter: `final 06:45` (or `45m`) starts one last report at that time, covering the time since the last one, then pauses; `pause` pauses now; `resume` starts the current slot's report at the standby watch's next poll and the schedule again. |
| `beekeeper reporter check` | Checks a report on stdin as the reporter's post hook does (see [The scheduled status reporter](#the-scheduled-status-reporter)): one line per problem and exit 3, or `ok`. |
| `beekeeper reporter` | The scheduled status reporter (see [The scheduled status reporter](#the-scheduled-status-reporter)): its schedule, when the next one starts, and the current or last run with its outcome. |
| `beekeeper timer add\|done\|list` | Times to look at something: `timer add 22:55 "check the rollout"` (or a duration, `45m`). `watch` prints one line when a timer is due; it stays open until `timer done`. |
| `beekeeper sessions serve\|unserve` | A record for any session, registered agent or not: `sessions serve <session> <owner/repo#n> [--waits "<what>"]`, the issue or epic it serves and what it waits on. `sessions` and `handover` show it; `watch` prints one line when the session ends, naming the issue to re-query. |
| `beekeeper handover [--prompt]` | Everything the next supervisor needs, as Markdown, from the live state: leases and grants, holds and their exceptions, agents, session records, notes with their defaults, timers, the merge lanes with their queues and settling merges, and what the alert watch reads (the installations and why, the ignored alert names, the baseline). `--prompt` prints the successor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope, the pending state in full and the commands that read the live values; no standing rule and no live value (version, memory figure, pull request state). |
| `beekeeper log [--verb PREFIX]` | Every claim, grant, hold, registration, note, timer, session record and build run, as they happened; `--verb run.` shows only the runs. |
| `beekeeper run [--max SIZE] [--wait DURATION] -- <command>` | Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the `memcap` wrapper) inside a memory-capped systemd scope. When every slot is held it waits once, then exits 75 with the holders; when the cap fires the kernel kills the biggest process in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap killed:`. Each run leaves `run.start` and `run.end` in `beekeeper log` with its scope, session and command, so `snapshot` and `watch` name a cap kill's session and command after the run has ended. `MEMCAP_TEST=1` marks a test's run: its scope is `memcap-test-…`, and a kill in it is reported as a test kill (one quiet `test kill:` line in `watch`), never as a build's. |
| `beekeeper hook pretooluse` | The Bash tool's PreToolUse hook: rewrites build, test, lint and lab commands to `<this binary> run -- zsh -c '<command>'` with the command verbatim and the tool timeout at 10 minutes (a background run waits 60 minutes), and refuses a third kind cluster, listing the held leases. It puts the merge gate in front of every `devctl pr merge`, behind prefix commands and in pipelines and lists, and refuses one hidden in a `-c` string (below); every other devctl command passes untouched. |
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

## The merge gate

Sessions keep typing `devctl pr merge <owner/repo> <n> [flags]`; beekeeper never replaces devctl.
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
  installation (the hold's reason); the GitHub budget is
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
tool timeout) settles for `merge.settle` instead. Versions compare as semver: a tag `v4.74.0`
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
merge leaves the lane.

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
| `no-supervisor` | a supervisor whose CLI stayed gone past `supervisor.restartGrace` with no relay open: claims stay gated until a successor starts; again after `notify.repeat` while it lasts | critical |

Nothing routine notifies: sessions starting or ending, thresholds of load, tmpfs and disk, alerts,
relays. Each event is one notification however many watches run `--notify` on the same state: the
first to claim it in `notify.json` (under `notify.lock`) sends it. A lasting condition (`oom-line`,
`budget`) and a supervisor gone (`no-supervisor`, per supervisor) notify again after `notify.repeat`. Quiet hours hold every notification that is not
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

### A supervisor gone

Claims stay gated from the first `supervisor start` until a deliberate `supervisor stop`: a
supervisor whose CLI crashed keeps the grant rule in force until a successor's `supervisor
start`, so no claim goes ungated in the gap. Once its CLI has been gone longer than
`supervisor.restartGrace` with no relay open, the watch says `SUPERVISOR GONE` once, naming the
supervisor and that claims are gated, and sends a critical `no-supervisor` notification, again
after `notify.repeat` while the gap lasts. A supervisor back, the same one or a successor, is
`SUPERVISOR BACK`, once. `beekeeper handover --prompt` is what a successor starts from.

### The spare, and a supervisor started again without a click

The supervisor records its relay spare with `beekeeper supervisor spare <session>`; `supervisor
status` and `handover` show it, and the spare's own `supervisor start` clears it. The desktop app
arms a 30-minute idle timeout for the CLI of a session off screen (app 2.7032.0 was not seen to
fire it: an untouched session still answered after 35 minutes), and only a message from inside
the desktop starts a stopped session; a message from the command line reaches a session
only while its CLI runs. So the standby watch (`watch --standby`, the `beekeeper-notify` unit)
sends the spare a keep-awake from the command line whenever it sat idle for
`supervisor.keepAwake` (25m): `beekeeper keep-awake: reply "ok", nothing else`, one small turn
of the spare's and one headless sender turn on haiku. It is silent unless it fails
(`KEEP-AWAKE FAILED`, also when the spare ran no turn on it within 3 minutes); a spare with no
running CLI is one `SPARE ASLEEP` line.

- **Relay:** unchanged. The supervisor runs `supervisor relay <spare>` and sends the output of
  `handover --prompt` to the spare's `local_` id with the desktop's SendMessage, which starts
  even a stopped spare.
- **Crash or CLI exit:** once the supervisor's CLI has been gone past `supervisor.restartGrace`
  (30s, a debounce for a supervisor someone woke: the app never restarts a crashed CLI), the
  standby watch sends the running spare `beekeeper: the supervisor "…" is gone and you are its
  spare. Run beekeeper handover --prompt and follow it.` from the command line, one `HANDOVER`
  line; the `SUPERVISOR GONE` line and its critical notification name the spare. Claims stay
  gated until the spare's `supervisor start`.
- **Reboot or app restart:** the login unit
  [`contrib/systemd/beekeeper-supervisor-open.service`](contrib/systemd/beekeeper-supervisor-open.service)
  runs `beekeeper supervisor reopen`, which starts the app on the recorded supervisor's session
  (`claude://code/continue?session=local_…`); the standby watch opens it the same way once when
  it sees the app started after the supervisor's CLI stopped with no spare to take over: after the
  CLI was first seen gone, or with the watch never having seen that CLI run under this app, as
  after a reboot, where the app starts at login before the standby watch's first poll. Every CLI is
  cold after an app start, so the focus starts the supervisor's; the standby watch sees its CLI
  back under a new PID and sends it the same hand-over from the command line (`RESUME`), since a
  restarted CLI has lost its watch.
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
holds, never one a hand-over or `agents remove` took off.

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
   for it and carries on without it after that. A failed send changes nothing.
2. **The prompt.** `beekeeper agents handover --prompt "<agent>"` prints it: the roster task, the
   brief the agent was started with (a follow-up's brief is its predecessor's, not the prompt
   around it), its `sessions serve` record, its gated merges, its last events and its note. No
   standing rules and no live values: the follow-up is told to check the live state first.
3. **The start.** The follow-up is started as `agents start` starts one, in the old session's
   folder and model, under the agent's name: it takes over the roster entry, the task and the
   session record. It shows in the sidebar and runs its first turn from the prompt alone.
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
2. **The post.** The session posts its report itself, with the Slack connector. Its prompt gives the
   time range in the person's time zone, the machine's, read for each run as `timedatectl` sets it
   (a running watch keeps no stale zone). The session starts with `--settings` adding a PreToolUse
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
  stallAfter: 5m            # a lane's first arrived merge waits this long behind absent places, then the lane is stalled
alerts:
  installations:            # read always; a held lease whose name has a kube context is read too
    - production            # context teleport.giantswarm.io-<name>, else <name>, else *@<name>
    - {name: lab, context: admin@lab, floor: warning}   # floor: the lowest severity shown (none, info, warning, notify, critical, page)
  ignore: [Heartbeat, InhibitionOutsideWorkingHours, Watchdog]   # the default; setting it replaces it
  team: my-team             # marked in capitals and counted; its alerts that only InhibitionOutsideWorkingHours inhibits are read too
  collapse: 3               # more changes of one alertname in one reading are one line
  every: 5m
  timeout: 1m               # per installation, port-forwards included, a failed attempt tried again within it; also bounds the upgrade reading
  flap: {changes: 4, window: 1h}   # an alert's 4th change within 1h is one FLAPPING line, then quiet until stable for 1h
supervisor:                 # what handover --prompt tells the successor supervisor
  skill: supervise          # the skill it runs; or instructions: ~/supervisor.md, a file that opens the prompt
  scope: The lab machine's sessions, kind labs and merge lanes.   # default: the resources, lanes and installations configured
  relayAt: 400k             # watch says RELAY DUE once the supervisor's context reaches this, at a quiet moment
  relayTTL: 15m             # a relay not taken by the successor's start expires
  restartGrace: 30s         # a CLI back within this of first seen gone is a restart; past it, SUPERVISOR GONE (claims stay gated) and the hand-over to the spare
  keepAwake: 25m            # the standby watch sends the spare a keep-awake once it sat idle this long (under the desktop's 30-minute idle timeout)
guide:                      # the guide's role: what guide handover --prompt tells its successor, and its relay
  person: Timo              # guide queue and guide watch show only the notes for this person (any case, an older note's "[for Timo]" prefix too); unset, every --for note
  skill: guide              # the default; or instructions: ~/guide.md
  relayAt: 400k             # guide watch says GUIDE RELAY DUE once the guide's context reaches this
  relayTTL: 15m
  restartGrace: 1m
agents:                     # the hand-over of registered agents
  relayAt: 400k             # watch says HANDOVER DUE once an agent's context reaches this; default: supervisor.relayAt
  noteWait: 3m              # agents handover waits this long for the agent's note
reporter:                   # the scheduled status reporter the standby watch starts; off without every and brief
  every: 1h                 # one report per interval, on its multiples (on the hour)
  brief: ~/reporter.md      # what to report and where to post it
  model: claude-sonnet-5    # default: Claude Code's
  person: Ada               # who the report is for; default: guide.person
  dir: ~/                   # the session's working directory; default: home
  timeout: 20m              # a reporter that has not posted by then is stopped
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
