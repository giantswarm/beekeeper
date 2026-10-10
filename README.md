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
macOS), with `teleport.proxy` set the [Teleport login's keeper](#the-teleport-login), and on systemd the [agent sandbox](#the-agent-sandbox)'s broker (`beekeeper-sandbox.service`) and the memory guard sized to the machine's RAM: `memcap.slice` for `beekeeper
run`'s capped commands, with their CPU budget (`memcap.cpuQuota`, `memcap.cpuWeight`), and a drop-in for the Claude Desktop scope that runs (run install again
with the app running when it does not). Without a config it writes a starter one, the [example
configuration](docs/examples/config.yaml) with every key commented out. The standby service runs
`watch --standby`, and `watch --notify --standby` only with `watch.notify: true` (off by default).
A file already as install writes it stays; one an earlier install wrote, a unit that runs this
binary and a hook entry that runs this binary's hook command (an older matcher, a self-update's
settings) are updated in place, never added twice; one that differs and is none of these it keeps
and names, and it never touches another setting. A second run changes nothing, so install is safe
to run after every update: that is how a changed hook matcher reaches the settings. A new Claude Code session picks the hooks up. `beekeeper uninstall` (also
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
running, and a `devctl pr merge` gate call re-executes the new binary at its next step, waiting
for its turn or following its devctl, as does a `merge-child` before it hands its outcome to the
owner. A running watch keeps the code it started with: every watch, `--once` included, says
`WATCH STALE` once per watch whose binary was replaced, naming the version it runs and the one
installed, and that a re-arm (a restart) picks the new one up; no watch re-executes itself.
`beekeeper self-update --check` exits 125 while a newer release is out.

An install leaves the older binaries already running on their old code until they end or are
re-armed: the `agents start` of a worker started before it (its desktop reopen saves the state
after the first turn), a watch, the standby unit; `beekeeper self-update` names them (pid and
command), a gate call or `merge-child` among them marked as re-executing the installed binary at
its next step. The state carries the version of the newest release that saved it (`writer`), and
a release older than it does not save: the save is refused with the state unchanged, logged once
per process as `state.stale-writer` naming the process, its command and both versions, and the
command ends with that message (exit 3; a gate call re-executes the binary installed at its path
first and refuses, 77, only when it cannot, the lane untouched either way; a gate whose devctl
already ran leaves the run's document and exit code under `merges/` for the watch, which records
the outcome from them at its next poll, so no lane keeps a running entry for a finished merge).
`beekeeper doctor` (a `DOCTOR stale binary` line in the watch) lists each
such process while it runs; a watch is named by `WATCH STALE`. A build without a release version
(a development or release-candidate build) neither stamps the state nor is refused, and its saves
keep what a newer release recorded: every object of `state.json` that carries per-entry data (the
state itself, roster entries and their keep markers, notes, timers, grants, holds, lanes, session
records, starts, archives, the roles) writes the members its binary does not know back unchanged,
with its entry. A process older than this mechanism (v0.71.1 and before) keeps only the state's
top-level members and drops the nested ones.

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
| `beekeeper capacity [--builds]` | The busy, parked and idle agents against `capacity.floor` and `capacity.ceiling`, the headroom that bounds a new start (MemAvailable, disk swap's growth over the watch's readings weighed with memory pressure and MemAvailable, the 1-minute load against the watch's limit, the kind labs against their cap, and for a start that builds the free build slots) and one verdict: `room for N starts` or what blocks one, each guard that refuses named; a swap growth without pressure is a warning in parentheses after it. `--builds` judges a start whose task builds, tests or lints, which waits for a free build slot; without it the verdict is for a start that does not build, and a busy slot is a note after the verdict, never a `no start`. Read-only, no GitHub call. |
| `beekeeper status [--bar]` | One line: who supervises, the leases held, the holds in force, the notes and timers that are due and the busy agents against the target (`busy 4/5-10`). `--bar` prints it as the fixed tab-separated row a desktop bar reads (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper sessions` | Every running session, Claude Code's and omp's (marked `omp busy` or `omp idle`, see [omp agents](#omp-agents)): the issues and pull requests its latest turns acted on (its `sessions serve` record, its `gh` and `devctl` commands and GitHub tool calls, what it created; a ref it only quoted or read is a mention, kept in `--json` as `mentioned`), when it was last active, the commands it runs right now (a `devctl` wait, a bounded `sleep` with the time left), its memory, how full its context is, its last hour (turns, tool calls and their errors, GitHub calls, cost), role and leases, after `archived` or `test` for a session the guide's feed leaves out. Overlaps name what more than one session acts on. `--all` adds the paused ones and the omp sessions of the last 24 hours no process runs: a message to them does not arrive. `--json` has every figure per session and their totals (see [Session metrics](#session-metrics)). |
| `beekeeper tail <session>` | A session's last turns without tool calls: what it said and what it was told. |
| `beekeeper snapshot` | One tick: load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots and their CPU budget and use (`memcap.slice`'s `cpu.max` and `cpu.stat` over a 500 ms sample), kind clusters, the sessions' last hour and the three that spent the most in it, the commands sessions sit on, every kernel OOM kill since your last snapshot with whose limit it hit (a memcap scope no `run.start` names says `cap unknown`, never the default cap; a test run's scope is a test kill), leases, holds, the GitHub budget, the installations' alerts and their running upgrades (`upgrades: prod/mc 35.0.1 → 35.1.1 for 9m (control plane 3/4, node pools 15/15) · test none`). A caller inside a Claude session that has taken one before gets only what changed since, or one `no change` line; `--full` prints the whole screen and then what changed. |
| `beekeeper ui` | The person's screen, for a terminal rather than a session's context: six tabs over the live state — the machine (load, RAM, swap, pressure, the desktop scope, disks, build slots, kind labs, the waits and the OOM kills), every session, Claude Code and omp (its state — busy, idle, waiting on its person, omp's own —, idle age, memory, context fill, last hour, what it runs, what it waits on its person for — a waiting session stands out; `enter` opens its pane, which follows the session live: the history, then the current turn with its tool calls as they land, re-read every refresh; `k`/`j` scroll back and forward, `G` follows again; `m` writes the session a message, stamped as the person's through the screen: by name to a Claude Code CLI, into the inbox of an omp agent beekeeper started; `t` takes a Claude Code session over: its permission requests come to the pane with its turns beside them, `a` allows and `d` denies, and one left unanswered for 290 s, or when `t` hands it back or the screen quits, goes to the session's own window, which the pane says), the shares (held leases and their staleness, free resources, grant queues, holds, the merge lanes), the supervision (the supervisor and guide roles with their relay state, the agents, the open notes with their defaults, the timers, the session records), the installations' alerts by severity with the running upgrades and the GitHub budget and its pollers, and the event log. Refreshes every two seconds (the budget and the upgrades every minute, in the background); besides those messages and take-overs it reads only: it takes no lease and lifts no hold — those stay with the commands, whose exit codes the sessions gate on. |
| `beekeeper watch` | Silent until something needs a look, then one line: the source of a `Monitor`. A threshold breach, an unreadable source and a stalled lane are one line when they start and one `ENDED` line when they end, never repeated while they last; OOM kills, sessions that start, end or restart, stale leases and every NEW or RESOLVED alert of the installations are always reported. A note or timer that falls due, the end of a session with a record and a relay taken or expired are one line each, once: the state keeps that they were reported, so a second or restarted watch stays silent about them. Once the supervisor session's context (its transcript's last request, the `CTX` column) reaches `supervisor.relayAt` (400k tokens), `RELAY DUE` is said at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued, no relay open), once per supervisor and again only after a relay is cancelled or expires; `supervisor status` and `handover` show the supervisor's context. A recorded supervisor whose CLI has stayed gone past `supervisor.restartGrace` with no relay open is `SUPERVISOR GONE`, once: claims stay gated until a successor's `supervisor start`; a supervisor back after it is `SUPERVISOR BACK`, once; its CLI back under the same session with a new PID is `SUPERVISOR RESTARTED`. A session over a threshold in `metrics.runaway` is one `RUNAWAY` line per figure, once per watch. A fork rate `watch.forkRateMax` over the machine's usual one for two samples is one `PROCESS STORM` line, and more than `watch.stackMax` long-running copies of one command line one `STACKED` line each, with the commands, the parent and the owning session, and more than `watch.toolProcsMax` (1000) processes of the CLIs in `watch.tools` (kubectl, helm, tsh, gh, flux, devctl) machine-wide one `LOAD` line with their commands and the sessions that run them, and an `ENDED` line each. Free space on `/` is `LOW DISK` under `watch.diskMinMiB`, `DISK NEARLY FULL` under `watch.diskCriticalMiB`, and `DISK FILLING` while it would run out within `watch.diskFillWithin` at its current rate, with the commands and sessions that wrote. A stalled lane (see `lanes`) is one `LANE STALLED` line, repeated at most every 10 minutes while it lasts. A settling merge leaves its lane once its release rolled and the lane's HelmReleases are Ready, however late; one not settled past `merge.settleTimeout` is one `LANE STUCK` line with what the lane waits for (the HelmRelease off the release, the one not Ready, an installation that cannot be read), repeated at most every 10 minutes, and one `ENDED` line when it settles or the lane is cleared; `lanes` marks it `stuck since`. A cluster upgrade on an installation (see [Cluster upgrades](#cluster-upgrades)) is `UPGRADE <installation>/<cluster> <from> → <to>` when it begins and `UPGRADE ENDED … (since HH:MM)` when it ends, once each. The quiet rules keep what is noise for the supervisor out of its lines: other teams' alerts matching `alerts.quiet` (by default their e2e test clusters, `t-*`, and, once `alerts.team` is set, every other team's and team-less `notify` alert), an alert back after a reading that missed it with its old start (an Alertmanager restart, a reconnect), and the start, end and restart of the short-lived sessions in `watch.quietSessions` (by default beekeeper's tests, `test: *`). A rule never holds back an alert of `alerts.team`, one on an installation in play (leased, claimed or merged into within the last 30 minutes, or with a merge settling), or a page unless the rule names a cluster; everything else is said as before. What they hold back is logged (`beekeeper log --verb watch.quiet`) and counted in `snapshot`. `--notify` also sends the events that need a person to the desktop, `--standby` leaves a running supervisor's events to its watch (see [Desktop notifications](#desktop-notifications)). |
| `beekeeper alerts watch\|snapshot\|import\|capture\|replay\|own` | The installations' alerts, read from each Alertmanager through a bounded `kubectl port-forward` (Mimir's with the `giantswarm` tenant, else the plain one), in parallel: `watch` prints one line per NEW or RESOLVED alert since the baseline (pages and your team in capitals, a burst of one alertname as one line, one line when an installation stops or starts answering, the lease holder and the sessions working against it in brackets, and on a NEW line the merges into the installation's lanes and the lease claims on it of the last 30 minutes with their sessions, worded as timing (`during merging …`, `during merged … at …`), not as cause); `snapshot` the current set, grouped; `import` takes over another watcher's per-installation baseline. An alert below its installation's severity floor never appears in either, and an alert that keeps firing and resolving is one FLAPPING line, then quiet until it has been stable for the damper's window; neither a floor nor a damper change prints a burst of lines. `capture <dir>` records each installation's answer, `replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it: a floor or damper setting tried before the watch gets it. `own <installation/alertname>` records the caller (`--by` another running session, `--as` a person) as the owner of the firing alerts it names (`alert.owned`); ownership ends with the owning session or the alert's resolution. `beekeeper watch` says `PAGE UNOWNED <installation> <alertname> <where> for <duration>` (one line per alertname, `(+n)` for its other unowned alerts) for a firing alert at `alerts.pageSeverity` (default `page`), of `alerts.team` when it is set, that has had no owner for `alerts.ownerGrace` (15m), and again every `alerts.ownerGrace` while it stays unowned; the owner's session ending starts the count again, and a non-paging alert never gets the line. One process owns the baseline at a time, so two watches never split the lines. `beekeeper watch` keeps one port-forward per installation from tick to tick, a dropped one replaced at the next attempt, and reads the kubeconfig's contexts again only once it changed, so a tick is HTTP requests, no kubectl or `tsh` run; a one-shot reading's forwards end with it, and every forward ends on SIGINT or SIGTERM and when beekeeper is killed. The kept forwards are the watch's own, never a session's wait. PagerDuty is the second source: with `alerts.pagerduty.context` set, the watch reads the open (triggered and acknowledged) incidents of `alerts.pagerduty.services` every `alerts.pagerduty.every` (1m) through muster's read-only PagerDuty tools as the person (`x_pd_list_incidents`, and `x_pd_list_alerts_from_incident` once per new incident for its alert's `installation` label), so a page for the team is a watch line within a minute whether or not its installation's Alertmanager is read: `PAGERDUTY NEW <installation> <TEAM> #<number> <title> since <start>`, `PAGERDUTY ACKNOWLEDGED …` and `PAGERDUTY RESOLVED …` once it is no longer open; a failed listing is tried once more, and a PagerDuty that read and then does not answer for 3 minutes (muster answers `auth_required` for a reading or two while it reconnects the server) is one unreachable line, said again every 15 minutes, with its open incidents kept. `alerts watch` reads it once too, `snapshot` lists the open incidents, its baseline is `pagerduty.json` with an owner of its own. beekeeper holds no PagerDuty token: the person's muster sign-in is the only credential. |
| `beekeeper spend` | What the API requests of a window cost, from every transcript under `claude.projectsDir` read whole: the sessions' and their subagents', running, paused or archived. A request counts once by its message id, however many transcripts carry it (a fork copies its history). `--since` (default `00:00`) and `--until` take a time (`15:04`), RFC 3339 or, for `--since`, a duration; `--tz` the zone. One line per model with its tokens; a model without a price in `metrics.models` is named unpriced and left out of the total. `sessions` prices only the last 512 KiB of each live session's transcript: `spend` is the figure for a day. |
| `beekeeper budget` | The GitHub core budget from the headers of a real, conditional request (a 304 costs nothing), the GraphQL limit from a real `rateLimit` query (one point, read again after `merge.budgetFresh`), and every `gh` and `devctl` process with its session: the callers. A GraphQL refusal shows even while its counter looks healthy, named as the hourly limit spent or a secondary limit, with the time it ends when GitHub names one; the watch says it once with the callers, and its end. `--gate` exits 3 under the floor or while GraphQL is refused. |
| `beekeeper lease claim\|release\|status\|grant\|revoke` | One holder per resource: the environments in the configuration, the shared installations of the central instance when one is configured ([The central instance](#the-central-instance)), the host's model server (`model-server`, whose claim carries `--gib <n>`: the GiB its models may hold, see [The model server](#the-model-server)) and the browser. While a supervisor runs, a session claims only what the supervisor granted it, in grant order: the supervisor's `yours <resource>` (`<resource> yours`, `<resource> is yours`) in a `SendMessage` or an `agents wake` to the session records the grant itself, `lease grant <resource> <session>` records one without the word, and a claim refused for a missing grant names the supervisor and that exact command. While a cluster upgrade runs on the installation of the resource's name, nobody claims it except by the supervisor's `lease grant <installation> <session> --upgrade-unblock "<why>"`, for the work that unblocks the upgrade (see [Cluster upgrades](#cluster-upgrades)). |
| `beekeeper lease kubeconfig <lab>` | The kubeconfig of a kind lab whose lease you hold, written into the lease (`<leaseDir>/<lab>/kubeconfig`, mode 0600) and freed with it; prints `export KUBECONFIG=<path>`, never the content. A lab claim writes it already when the cluster runs. In the agent sandbox the broker writes it and the session points it at its sandbox proxy (see [The agent sandbox](#the-agent-sandbox)). |
| `beekeeper lease up\|down <lab>` | Create (`agentlab up`, asking nothing) or tear down (`agentlab down`) the kind cluster of a lab whose lease you hold, the cluster `labs` maps the lease to: in the lab's directory its `agentlab.yaml` must name that cluster, elsewhere agentlab runs the lab it knows by that name. Capped as `beekeeper run` caps it; `up` prints the lease's kubeconfig line, `down` drops it. In the agent sandbox the broker runs it on the host (see [The agent sandbox](#the-agent-sandbox)). |
| `beekeeper hold set\|lift\|check` | Stop merges into a repository (a broken main), one lane (`--lane serving`: a proving window such as a model load stops the lane whose components it exercises, not the others), every merge (`merges`) or every GitHub call (`github`) until lifted, a time passes or its probe passes (`--lift-when "<shell command>"`: the watch lifts the hold once the command exits 0 and logs it as `hold.lift`). A merge hold lets one repository or pull request through with `--except owner/repo[#n]`: `hold set --lane serving --except giantswarm/model-manager#172` stops the lane but for the merge it waits for. A lane hold that excepts one pull request (`owner/repo#n`) is also its fix window: that merge runs without the lane's HelmReleases Ready or the previous release rolled, for the fix of a rollout only the fix can repair, logged as `merge.window`. The merge gate enforces them. |
| `beekeeper lanes [queue\|settle\|urgent\|drop\|clear]` | Each merge lane: the running merge, the one settling until its release rolled, and the waiting ones in turn order, so who is next is never prose. `urgent <owner/repo> <n> --reason <why>` lets one privacy or security fix's merge run under the GitHub budget floor, once per reset window (see the merge gate). `queue <owner/repo> <n> --for <session>` gives a session's merge its place now so an agreed order carries over (kept until that merge runs, through refusals, for `merge.seedTTL`, 12h; seeds keep their order, an arrived unseeded merge passes one whose merge has not arrived); a run with nothing merged stays in its place as `retrying` for its session's retry; `settle <owner/repo> <n> [--for <session>]` registers a merge run outside the gate (in flight when the gate went live, run without the hook): it heads its lane until it merges, then settles the lane like a gated merge; `drop` takes a waiting merge out (a place whose pull request merged or closed leaves by itself at the next `watch` poll), and ends the devctl of a running merge whose pull request merged or closed (`watch` does so by itself `merge.hungAfter`, 45m, after the merge); `clear` is the repair for a lane whose settling release will not roll, after a look at the installation. A lane where no merge runs and whose first arrived merge has waited longer than `merge.stallAfter` (5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left it) is `stalled`, with the waiting merge and those places named. |
| `beekeeper board next [--claim]\|move` | The project board's next item of work: `next` walks `board.order` (steps by Status, Kind and labels; an epic's open sub-issues; blocked items whose blockers all closed; items created within a window; a GitHub search; a sub-issue that is a board item held to the order by its own Status, whatever its Team) over one read of the board and prints each item above the pick that was skipped on a line of its own before it, `skipped <owner/repo#n>: <reason>` (the same list as `--json`'s `skipped`; a note's reason names the note and its first line, `note #<id> "<first line>" (waits on <person>)`), then the first free item with why it is picked, without `--claim` the free items behind it in their order (the preview of the board's work) and the skipped items' count by kind (`served`, `note`, `assigned`, `blocked`, `stale`, `lease`, `order`; `--json` lists every item behind the pick as `after_pick`, with its skip reason or free, and the counts as `skipped_by`): served by a running session (a `sessions serve` record, a busy agent's task; one naming a pull request serves the issues it closes too, `… through owner/repo#<pr>`) or by an agent on the roster whose CLI is gone while it is busy with its task or kept, named by an open note (its text or its `--ref` links: by URL, as `owner/repo#n`, as `repo#n` of `board.owner`, as a repository before a list, `beekeeper: #524, #525`, or as a bare `#n` after the last repository named, `beekeeper#173, #176`; a bare `#n` before any repository names nothing, and `note #n`, `timer #n`, `memo #n` and `decision #n` are beekeeper's own items), assigned outside `board.people`, blocked by an open recorded blocker, without activity for `board.staleAfter`, or needing a lease another session holds (a `lease/<resource>` label on the issue, `lease/agentlab-1`: the item is passed over until the lease is free and the items behind it are offered meanwhile; a picked item's free lease is printed with it); a sub-issue offered through an epic passes the same checks, and a serve record, task or note naming the epic covers it (`…, on epic owner/repo#n`). `--claim` records the pick as the calling session's serve under the state lock, so two concurrent claims never get the same item; it changes nothing on the board, the caller moves the item with `board move` once it judged it, and ends with the session, `agents idle` or `sessions unserve`. A second `--claim` while the session serves an open issue or PR is refused (exit 3) with its record, unchanged; `--claim --replace` takes the next item and replaces the record. `move <issue> <status>` sets an item's Status by the board's canonical name or an unambiguous part ("up next"), and refuses anything else with the board's values. |
| `beekeeper person <login> [--org <org>] [--refresh]` | Whether a GitHub login is a member of the org (default `giantswarm`), private memberships included: prints `member` or `not a member` (exit 0), or `permission missing` (exit 3, the reason on stderr) when the token may not read the whole membership, never `not a member` then. The roster is read with the person's own `gh` login (the `gh` on `PATH` outside `agents.shell.path`, `GH_TOKEN` and `GITHUB_TOKEN` ignored), after the token's own membership proved active, and kept in the state directory for a day (`--refresh` reads it again); the token stays in the process. An agent asks this instead of `gh api orgs/<org>/members/<login>`, which the App's token answers 404 for a private member. In the sandbox the broker answers. |
| `beekeeper proof upload <image> --repo <owner/repo> [--alt <text>]` | Puts a proof screenshot where a public GitHub thread shows it and prints its Markdown image line to paste into the comment or PR body: the image (PNG, JPEG or GIF, at most 10 MiB) re-encoded as PNG, which drops its metadata, committed to the images-only branch `beekeeper-proofs` of the public repository `--repo` through the Git data API with the `gh` on PATH (an agent's gh carries the App's token; beekeeper never reads it), the link pinned to that commit. A repository that is not public is refused. See [the browser](#agents-started-without-a-click). |
| `beekeeper browse "<steps>" [--dir <dir>] [--model <model>] [--timeout <duration>] [--allow-deploy "<what, where>"]` | Runs browser steps in a headless `claude -p --chrome` turn that has the CLI's own Claude in Chrome tools and nothing else, so no navigate waits on a site approval, and prints the turn's report, the deploy the run declared (`allowed deploy:`), the calls its classifier refused (`refused:`, each in the call's own words with the classifier's reason), the screenshots that timed out on an undrawn window (`not rendered:`), then its transcript and each screenshot as an image file. For the desktop turns of the sessions beekeeper starts; hold the browser lease while it runs. `--allow-deploy` declares the one deploy or create click the task covers, which the turn's auto mode then allows for this run only. Steps that name an App on record (`beekeeper app`), by name or callback host, run with that App's auto mode rule and prompt words, the report's `allowed consent:` line naming the record; steps that name a loopback lab host (`*.127.0.0.1.nip.io`, `localhost`) run with the lab's rule (`allowed lab sign-in:`); the turn's start record has the hook act in it wherever it runs. `beekeeper browse --help`, and [the browser](#agents-started-without-a-click). |
| `beekeeper app allow <name> --client-id <id> --word "<the person's words>"`, `app list`, `app revoke <name>`, `app check <url> [--title <title>]` | The Apps the organisation declares and the person asked a consent for, on their word: the App's name as GitHub's consent page titles it, the OAuth client id its consent URL carries, the callback hosts of its declaration (the App's manifest in `apps.repo` at `apps.manifest`, `{name}` for the App's name; an App no manifest declares is not recorded), and the person's words (a note's answer), by whom and when (`app.allow`, `app.revoke` in the log). With one on record, `beekeeper hook pretooluse` answers a Chrome click on GitHub's consent page itself (below); `app check` prints that decision for a page, dry: `allow:`, `refuse:` or `no decision:` and the reason, `--title` the page's title where the Chrome tools redacted the URL's client id. |
| `beekeeper supervisor start\|stop` | Make a session the supervisor. The grant rule applies from its start until a deliberate `supervisor stop` (which ends supervision and starts no successor) or a successor's `supervisor start`: a supervisor whose CLI crashed keeps it in force, its grants and queue stay recorded, and every claim waits for the successor. A CLI back under the same session id or desktop record within `supervisor.restartGrace` (30s) of beekeeper first seeing it gone (a claim or a watch's poll) is a restart and keeps the role and an open relay without a new start; `supervisor status` and `status` show it restarting, then gone. Another session's start is refused while it runs or restarts, unless the supervisor relayed the role to it; past the grace a successor's start takes the role. The start prints the holder's first look in a few KB: leases, holds, lanes, what waits on it (pinned and open notes, the guide's as a count, the decisions answered since the last relay, timers) and the agents with their tasks. A start that changes the holder runs `agents broadcast` in a transient unit, so every running agent learns the name that answers now. |
| `beekeeper supervisor reopen` | Open the recorded supervisor's desktop session, starting the app if needed (the login unit, see [Fresh successors](#fresh-successors-and-a-supervisor-started-again-without-a-click)). |
| `beekeeper supervisor relay [--cancel]` | Hand the role over without a gap: beekeeper starts the next run, "Supervisor run N+1", as a fresh session (as `agents start` does, so its desktop title, roster name and messaging name are one) and opens the relay to it; its first turn's `supervisor start` takes the role, the grant queue and the pending grants in one step, and the event log shows both. Until then the outgoing supervisor keeps the role and the grant rule; a relay not taken expires after `supervisor.relayTTL` (15m) or is withdrawn with `--cancel`, and a successor that does not start withdraws it. `supervisor status` in the relieved session exits 4, also after the successor relays onward, cancels a relay or is relieved in turn, until that session supervises again (or for 7 days). Once the successor took the role, the doctor owes the relieved run's desktop row the archive and does it once that run's CLI runs no turn, which frees the desktop CLI slot it held (`guide relay` likewise); the successor's `start` line says so. |
| `beekeeper guide start\|stop\|status\|relay` | The guide: the session that walks the person through the decisions waiting on them, next to the supervisor and never in its session (`guide start` in the supervisor's session and `supervisor start` in the guide's are refused, and neither relays to the other's holder). It has no grant power. Its role moves like the supervisor's (a relay to a fresh "Guide run N+1", its start, relief with exit 4, restart grace, a fresh successor after a crash) without touching the supervisor's record; `guide handover --prompt` is the successor's prompt (`guide.skill`, default `guide`, or `guide.instructions`, then its queue and an open relay). |
| `beekeeper guide next` | The one decision the guide asks now: the open note for its person due first, marked as being asked (`guide.asking`), served again until `note answer` or `note done` closes it; only then the next. Only the guide's session calls it; `--json` the item. |
| `beekeeper guide queue` | The guide's queue: every open decision filed for its person, `guide.person` (`note add --for`, or an older note's `[for <person>]` text prefix; any case; a memo or a pinned note is none), with its owning session (the one that filed it, and whether it still runs), deadline and default, then every session whose record says it waits on the person (`sessions serve … --waits "<person>: <ask>"`, the person in any case; with `guide.person` unset, every `--waits`), while its CLI runs or stopped (`(stopped)`) less than `guide.waitingTTL` ago (default 2h; 0: never folds), except the guide's own session, an archived one, a test (titled `test: …`) and an agent that reported its task after the wait (`agents idle`, `--done`, or taken off the roster). The sessions stopped longer ago fold into one line, `<n> stopped session(s) waited on <person>`; `--full` lists them. `guide watch` says nothing for a session that only aged out. With `guide.person` unset, every note filed `--for` anyone, and a line that says so. Delta output like `sessions`. |
| `beekeeper guide watch [--once]` | The guide's feed, silent otherwise: `GUIDE DECISION` for each new open decision of the queue (only `guide.person`'s; with it unset, every `--for` note and one `GUIDE:` line that says so), `GUIDE WAITING` for each session of the queue newly waiting on its person (only an explicit `--waits`: the desktop's turn summary does not count), `GUIDE ANSWERED`, `GUIDE DEFAULTED` or `GUIDE CLOSED` for a note of the queue closed (`GUIDE CLOSED #<id> overtaken: <reason>` for an overtaken one, `GUIDE REPLACED: #<id> replaced by #<new>: <text>` for one a successor replaced), `GUIDE ORPHANED #<id> for <person>, its filing session "<name>" is archived; ask it, or close it with note done <id> --overtaken: <text>` for an open decision without `--ref` whose filing worker session is archived (`note add --ref`), and the guide's relay: `GUIDE RELAY DUE` once its context reaches `guide.relayAt` (150k), `GUIDE RELAY TAKEN`, `GUIDE RELAY EXPIRED`, `GUIDE RESTARTED`. Each is one line, once: what it said is kept in the state (`guide.fed`). |
| `beekeeper agents register\|assign\|idle` | The roster of empty sessions registered as spare capacity. `register` names the session by its title (a `claude --bg` worker by its `-n` name) unless `--name` overrides it. A name is one agent's: registering under the name of a session that no longer runs (what `agents` shows as `not running`: stopped, closed or asleep) replaces its entry, saying so; a task the replaced entry left unfinished becomes the new entry's, with its assignment time, named in the `agents.register` event (`replaces <id>, takes over "<task>"`) and in the output, so the fresh session works it and a later `assign` to the name is refused as busy until `idle`. Re-registering keeps the session's own open task, so a session `agents start` registered busy stays busy when it registers itself (`register: <name> busy with "<task>"`); two dropped entries with open tasks are refused (exit 3). A name a running session's entry holds is refused (exit 3). `assign` and `remove` take a session id, a name or a unique part of one, and refuse a name several entries share. `idle --done` requires the final report, `--report "<text>"` (`-` reads stdin), and its "Problems found": `--problem "<finding>"` once per broken function, workaround, follow-up or problem the task met (evidence and owning repository in the line), or `--problem none`; without them, or with `none` beside a finding, it is refused (exit 3). beekeeper delivers them itself: both are logged (`agents.report`, `agents.problem`) and the supervisor's watch prints them once, `WORKER REPORT by "<agent>" (task: …): <report>` and `PROBLEM FOUND by "<agent>": <line>` per finding, for the supervisor to file and hand to a worker. |
| `beekeeper agents start <name> <brief file> [--task t] [--model m] [--dir d] [--harness omp] [--builds] [--force] [--dry-run]` | Starts an agent session without a click that runs in Claude Desktop from its first turn. Under a worker profile (`agents.profile`) its task runs as a headless turn under the profile instead and no seed turn runs; `--dry-run` prints the profile applied and the first turn's command line and starts nothing. See [Worker profiles](#worker-profiles). The start is judged first as `beekeeper capacity` judges one, `--builds` marking a task that builds, tests or lints and so waits for a free build slot; a verdict without room refuses it with exit 3, the guards named, and no session is created, unless `--force`, which starts it and records the verdict overridden in the event log. Before the session exists it records the id beekeeper chose as one of beekeeper's starts (the state's `starts`, kept 30 days) and registers it on the roster under the name, busy from its start with `--task` (by default the brief's first line) or with the open task of a stopped entry under that name. A seed turn without tools (`claude -p` in a transient user unit `beekeeper-agent-<id>`) creates the transcript with the worker prompt; the session is then imported into the desktop (`claude://resume?session=<id>`, sidebar row `local_<id>` titled with the name), the desktop switched back to the session it showed (`claude://code/continue`), and the task runs as a desktop turn of the session's desktop CLI, or of one a steward's `send_message` starts; headless only where the desktop cannot run it, said with the reason. See [Agents started without a click](#agents-started-without-a-click). `--harness omp` starts an omp agent instead, see [omp agents](#omp-agents). |
| `beekeeper agents desktop [agent]` | Asks for an agent's desktop turn (the browser): its import or reopen shows it in the desktop without waiting for the window's focus, the person's typing holding it 1 minute at most; an agent with neither a CLI nor a waiting reopen is shown at once. By default the calling agent. See [Agents started without a click](#agents-started-without-a-click). |
| `beekeeper agents wake <agent> <message> [--permission-mode m]` | Messages a registered agent without Claude Desktop's `local_` route and its cap (a session's desktop sends pause after ten since its person last typed in it). A running CLI gets the message by name; a session with a desktop row and no CLI gets it as a desktop turn, a steward's `send_message` starting its desktop CLI; only where the desktop cannot run it (said with the reason) is it resumed headless, `claude -p --resume <id>` with the message as its turn, in its directory, permission mode (a start's bypass, else the desktop record's) and recorded model, in a transient unit `beekeeper-wake-<id>-<wake>`, one per wake; its `ExecStopPost` starts `agents reopen <local_ id>` in a unit of its own (`beekeeper-reopen-<id>-<reopen>`), which warms the desktop's CLI again. `agents` shows `live, first turn running` or `live, wake turn running` while a headless turn is the session's CLI. See [Waking a session](#waking-a-session). |
| `beekeeper agents handover <agent> [--prompt] [--model m] [--dir d]` | Hands a registered agent over to a fresh session near its context limit, one line per step: asks it by peer message, or in a headless turn resumed from its transcript when its CLI does not run or take the message, for `beekeeper agents note "<what is in flight, what is next>"` (waiting `agents.noteWait` at most), builds the follow-up's prompt, starts the follow-up as `agents start` does under the agent's name (it takes over the roster entry, the task and the session record), stops the old session's CLI and the processes under it by PID (a `claude --bg` session through `claude stop` first, so its daemon does not resume it), archives the old session's desktop row once its CLI has exited, and logs `agents.handover`. `--prompt` prints the prompt only. `watch` says `HANDOVER DUE` once per agent session at `agents.relayAt`, or `HANDOVER HELD` while the agent's task is in its last step (`agents.lastStepGrace`). See [Agents handed over near their context limit](#agents-handed-over-near-their-context-limit). |
| `beekeeper agents park [--on <#note\|owner/repo#n>] "<what>"`, `agents resume <agent>` | `park` parks the calling agent, its task kept, on what settles its wait: counted parked, not busy, in `agents` and `capacity`, it ends its turn. Once the note is closed or the issue or pull request merged or closed, the watch says `AGENT RESUMABLE` once; a park on a person (a note, or a wait naming `guide.person`, an answer, a review or a decision) the supervisor's watch says once as `PARKED ON A PERSON`, for the guide to tell the person; with `agents.autoResume` it resumes the agent with what settled it, a person's answer word for word. `resume` does it by hand. An agent without a task is refused. See [Agents started without a click](#agents-started-without-a-click). |
| `beekeeper agents archivable <local_id>...` | Confirms, per desktop session, that it is a finished worker beekeeper started (one of its starts, off the roster, no role a relay did not relieve, unarchived, its CLI in no turn but the caller's own) and exits 3 for any other, a session the person started included. A steward asked to archive runs it first and archives only what it confirms (see [Agents started without a click](#agents-started-without-a-click)). |
| `beekeeper agents broadcast <message>` | Sends one message by name to every registered agent whose CLI runs, one after the other, the supervisor and the caller left out; a stopped agent is not resumed for it. One `agents.broadcast` event logs whom it reached. |
| `beekeeper agents note <text>` | The calling agent's hand-over note, logged as an `agents.note` event; the next `agents handover` puts the latest one into the follow-up's prompt. |
| `beekeeper note add\|answer\|done\|pin\|unpin` | Open items that outlive a session, of two kinds (`--kind`). A decision waits on a person with its deadline and what happens if nobody answers (`note add --for Ada --due 22:55 --status-quo "<what is true now>" --why "<why it needs Ada>" --default "the alert stays as is" <text>`); only open decisions reach the guide (`guide queue`, `guide watch`). A memo is a session's own record (a state summary, a board skip, a list of deferred work) that asks nobody; `note list` and the hand-over list it apart from the decisions (`note list --for <person>` lists one person's). `--kind` defaults to `decision` for `guide.person` (any `--for` with it unset) and to `memo` otherwise; a note filed before kinds is classified the same way, a pinned one as a memo. A decision names who decides (`--for`) and is refused with exit 2 without `--due` and a `--default` that is an action. `--replaces <id>` closes the named open note in the same step (`note.replaced`) and carries its pin over, so a state memo is one open note at a time. A decision for `guide.person` (any `--for` with it unset) is refused, naming what it lacks, without `--status-quo`, `--why`, a `--default` that is an action (not `wait` or `none`), every `--option` as `<choice>: <consequence>`, the full URL of every `#N` or `owner/repo#N`, and `--checked "<source>"` for a claim that something is merged, green, released, rolled or closed, and while a pull request it links in a plans repository (`plans.repositories`) is open with its stage check (`plans.check`, `plan-stages`) red, pending or missing, naming what the check found; one on an issue or PR an open note for the same person names, with the same verb (the text's first word), folds into it (`note.folded`). `--kind login --until "<probe>"`: the watch runs the probe each tick and closes the note once it exits 0. A memo is not checked. A note for a person asking again what was answered for them within 72 hours (the same verb on one of the same issues or PRs) is filed with a warning that quotes the answer. `--pin` (or `note pin <id>`) makes a note a standing instruction: every hand-over, the supervisor's and the guide's, carries it until `note unpin` or `note done`. `watch` reports a note once when it is due; a decision due unanswered closes with its default (`NOTE DEFAULTED #<id>: <default>`, `note.defaulted`), and the session that filed it is told. A decision renders as one Slack message (`beekeeper serve --help`): it is refused with exit 2 without `--status-quo`, with a question over one line or 150 characters, a status quo over 3000, more than 10 `--option` or a label over 75 characters, or a `--recommend <n>` (1-based) that names none of them; `--for team:<name>` puts it to a team. `--ref owner/repo#n` (repeatable) links a note to the issues or PRs it asks about: once every one is closed or merged, `watch` closes it as overtaken (`NOTE OVERTAKEN`, `note.overtaken` with the reason; `watch --once` names it and writes nothing), when its worker closed them at the end of its task: an issue a pull request's closing keyword closed at its merge (`<ref> closed by <owner/repo#pr>'s closing keyword`), or any close while the worker that filed the note still runs its task, keeps the note open, said once per reason as `NOTE KEPT: #<id>, <reason>: <text>` (`note.kept`); the worker's `agents idle` ends its task, and from then on its settled issues overtake its notes, a keyword close included; a note without `--ref` is never closed by the watch: once the session that filed it is archived (a stopped session is not), `guide watch` names it to the guide as orphaned, to ask or close by hand (a role's run, current or relieved, never orphans its notes). Pinned and login notes, and the note the guide asks now, are never overtaken. `note done <id> --overtaken "<why>"` closes one so by hand. `note answer <id> [--choice <n>] [<answer>]` records the person's answer word for word (the option's label, their own words, or both) and how it came (`--via cli`, the default, or `slack`), closes the note and tells the session that filed it (`NOTE ANSWERED #<id> by <person>: <answer>`, which `watch` prints too); the `note.answered` event carries it for the owning session, the supervisor and the guide's feed. |
| `beekeeper reporter final\|pause\|resume` | Pauses the scheduled reporter: `final 06:45` (or `45m`) starts one last report at that time, covering the time since the last one, then pauses; `pause` pauses now; `resume` starts the current slot's report at the standby watch's next poll and the schedule again. |
| `beekeeper report [--since 1h\|15:04] [--until 15:04] [--tz zone]` | Renders the status report from beekeeper's own state, as Markdown that passes `reporter check`: the pull requests and promotions the gate merged in the window with title, release and rollout; the running work (roles, busy agents, sessions serving an issue) with the issue, what it waits on and its leases; what waits on `reporter.person` (open notes per owning session, sessions waiting on an answer, the open drafts of `reporter.reviews`); the merge lanes, timers and holds; the machine. Every time in `reporter.tz` (`--tz`; default the machine's zone); one GitHub request for the titles and drafts. See [The scheduled status reporter](#the-scheduled-status-reporter). |
| `beekeeper reporter check` | Checks a report on stdin as the reporter's post hook does (see [The scheduled status reporter](#the-scheduled-status-reporter)): one line per problem and exit 3, or `ok`. |
| `beekeeper reporter` | The scheduled status reporter (see [The scheduled status reporter](#the-scheduled-status-reporter)): its schedule, when the next one starts, and the current or last run with its outcome. |
| `beekeeper timer add\|done\|list\|check` | Times to look at something: `timer add 22:55 "check the rollout"` (or a duration, `45m`). `watch` prints one line when a timer is due; it stays open until `timer done`. A timer can wait on a condition from its time instead: `--when "pr-merged owner/repo#n"` (also `issue-closed owner/repo#n`, `helmrelease-ready context/namespace/name [version]`, `controlplane-ready context/namespace/name`) or `--probe "<command>"` (exit 0: it holds), checked by the watch every `--every` (5m), each distinct check once per tick however many timers share it and a GitHub one only above the budget floor; it fires within one tick of the condition holding. `controlplane-ready` reads a KubeadmControlPlane of Cluster API v1beta1 or v1beta2: every replica ready and up to date, or the v1beta1 `Ready` condition, once the status has observed the spec. `helmrelease-ready` with a version holds once the HelmRelease is Ready and runs that chart version: the newest release of `status.history`, or without a history the attempted revision while Ready (current Flux leaves the applied revision empty, so a `--probe` of it never holds). A condition that has not held for 24 h is one `TIMER STUCK` line (event `timer.stuck`) and the timer keeps waiting; `timer list -v` says since when. A reference the watch cannot read (an unknown context, no access, no such kind or object) is one `TIMER UNREADABLE` line, again when the reason changes, and the timer keeps waiting; `timer list -v` shows what each timer's last check found, `timer check <id>` runs it now. `--until` ends the wait: the timer then fires as timed out, or with `--expire` closes unfired (`TIMER EXPIRED`). `--wake <agent>` wakes the agent with the timer's text (as `agents wake`; `TIMER WAKE FAILED` when it cannot), `--run "<command>"` runs a command, its output in `timers/<id>.log` of the state directory and its exit code logged (`timer.ran`, `TIMER RAN`); without either the `TIMER` line is for the supervisor. Such a timer closes when it fires; with `--repeat daily`, `weekdays` or `weekly` a `--wake` or `--run` timer is re-armed at the same local time instead and stays open until `timer done` (a morning refresh, a Friday summary). |
| `beekeeper ps [name\|pid...]` | The process table with every command line masked: PID, parent, age, CPU time, resident memory, and the command line as the watch's `STACKED` line prints it (the program, its subcommands and flag names; flag values, `key=value` words, URLs and what follows a short option's letter left out). A masked line ends in `(masked)`, so a left-out value is never read as an empty one. A number narrows the list to a PID, a word to the processes whose masked line or name holds it. `--json` has the rows. The hook's refusal of a whole command-line read names it ([Secret reads](#secret-reads)). |
| `beekeeper sessions serve\|unserve` | A record for any session, registered agent or not: `sessions serve <session> <owner/repo#n> [--waits "<what>"]`, the issue or epic it serves and what it waits on; `sessions unserve <session|owner/repo#n>` removes it, by the issue for the one record serving it. `sessions` and `handover` show it; `watch` prints one line when the session ends, naming the issue to re-query. |
| `beekeeper handover [--prompt] [--section <name>]` | Everything the next supervisor needs, as Markdown, from the live state: leases and grants, holds and their exceptions, agents, session records, pinned notes, notes with their defaults, the decisions answered since the last relay, timers, the merge lanes with their queues and settling merges, and what the alert watch reads (the installations and why, the ignored alert names, the baseline). `--prompt` prints the successor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope, the pending state and the commands that read the live values; no live value (version, memory figure, pull request state). Both show what a successor acts on in its first minutes: the notes the guide serves (for `guide.person` and for the guide) are one count line, records of sessions ended over an hour ago one line, and the answered decisions those since the predecessor's start with a count of the rest of 72 hours. `--section <name>` prints one section with everything it holds (`notes`, `records`, `answers`, …). |
| `beekeeper log [--verb PREFIX]` | Every claim, grant, hold, registration, note, timer, session record and build run, as they happened; `--verb run.` shows only the runs. |
| `beekeeper run [--max SIZE] [--wait DURATION] -- <command>` | Run a build, test or lint command in one of the machine's build slots (memcap's, shared with the `memcap` wrapper) inside a memory-capped systemd scope, at nice 10, under `memcap.slice`'s CPU budget (`memcap.cpuQuota`, half the cores; `memcap.cpuWeight`, 50 against the desktop's 100), which every run sets again at the slot it takes and the slots share by equal weight. A run of a session that holds a slot already joins it at once (its scope in the slot's slice, sharing the slot's cap) rather than taking a second one. When every slot is held it waits once, then exits 75 with the holders; when the cap fires the kernel kills the biggest process in the scope only, and `run` exits 137 with one line starting `beekeeper run: the <SIZE> cap killed:`. Each run leaves `run.start` and `run.end` in `beekeeper log` with its scope, session and command, so `snapshot` and `watch` name a cap kill's session and command after the run has ended. `MEMCAP_TEST=1` marks a test's run: its scope is `memcap-test-…`, and a kill in it is reported as a test kill (one quiet `test kill:` line in `watch`), never as a build's. |
| `beekeeper hook pretooluse` | The PreToolUse hook (matcher `Bash|Read|Grep|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|TaskStop|mcp__.*`): rewrites build, test, lint and lab commands to `<this binary> run -- zsh -c '<command>'` with the command verbatim and the tool timeout at 10 minutes (a background run waits 60 minutes), and refuses a third kind cluster, listing the held leases. It puts the gate in front of every `devctl pr merge`, `pr wait`, `release wait` and `rollout wait`, behind prefix commands, in pipelines and lists and in a here-document a shell reads, and refuses one it cannot reach: hidden in a `-c` or `eval` string, in a script file the call runs, in an interpreter's code or in the program a shell or an interpreter reads from its input, naming the place and the line (below); every other devctl command passes untouched. It refuses a delete that reaches further than it names ([Deletes](#deletes)), a command that would print secret values (below, [Secret reads](#secret-reads)) and a call that would send one off the machine ([What leaves the machine](#what-leaves-the-machine)), a kube context switch and a write to production ([Kube contexts and production writes](#kube-contexts-and-production-writes)), a command that opens a page in the person's browser (`muster auth login`, `gh auth login --web`, `xdg-open`) outside the session holding the `browser` lease, a Chrome call that acts on GitHub's consent page (`github.com/login/oauth/authorize`: a click, a key, a typed text, a form value, a batch with one) of an App no `beekeeper app allow` record covers, naming the page and the record's form, while one on the consent page of an App on record, its client id (or its name in GitHub's title, where the tool redacts the id) with one of its declared callback hosts, is allowed with the record in the reason, as is a fixture user's sign-in or consent on the lab's own identity provider on a loopback host (`*.127.0.0.1.nip.io`, `localhost`); the page is the tab's last known URL and title in the session's transcript, and a call whose page it does not show gets no decision ([the browser](#agents-started-without-a-click)), a command that loads a model on the host's model server outside the session holding `model-server` ([The model server](#the-model-server)), a board-wide project read (`gh project item-list` without `--query`, a `gh api graphql` query over a `projectV2`'s `items` without a narrowing `query:`), naming its estimated cost, the GraphQL budget last read, and `gh project item-add`, `gh project item-edit --id`, `beekeeper board move` and a filtered read instead, in a session `agents start` started a `gh api` read of an org's members, a login's membership, an org's teams or a team's members, or a login's orgs (the REST paths, or the same query in a `gh api graphql` body), naming `beekeeper person <login>` instead, since the App's token an agent's `gh` carries sees public memberships only, in such a session a search over issues or pull requests that spans more than one repository (`gh search issues` or `prs` without exactly one `--repo` or `repo:`, or `gh api search/issues` likewise), naming the per-repository `gh issue list --repo <owner/repo> --search` instead, since GitHub drops a repository the token cannot read from such a search without an error, an `AskUserQuestion` call outside the guide's session ([Questions go to the guide](#questions-go-to-the-guide)), and in the guide's session the calls that do work and a question without its status quo, why and full links ([The guide asks and relays](#the-guide-asks-and-relays-it-never-works-itself)). A `SendMessage` to `the supervisor` or `the guide` goes to the session holding that role now, by the name its running CLI answers to (else its desktop session), so a brief names the role and a relay never makes it stale. The holder is beekeeper's own record, matched to its CLI by session id, never a title: while another live session carries the holder's title, the message goes to the holder's CLI by its own socket (`uds:…`), and `beekeeper doctor` and the watch (`SAME TITLE`) name every title two live sessions carry. A relay or a role's `start` refuses a run name another live session carries, naming that session. A `SendMessage` to a desktop id (`local_…`) whose session has a running CLI goes to that CLI by name, so a headless turn gets no second copy beside it (below, [Waking a session](#waking-a-session)). A `SendMessage` from the session holding the supervisor role that says `yours <resource>` records the grant of that resource to its target as `lease grant` does, and tells the supervisor what it recorded, or why nothing was. A session's first write or `git commit` in another repository carries that repository's `CLAUDE.md`, `AGENTS.md`, rules and mandatory reads ([A repository's instructions on the first write](#a-repositorys-instructions-on-the-first-write)). |
| `beekeeper hook posttooluse` | The PostToolUse hook (matcher `*`): replaces a tool result that carries an indexed secret value or a token pattern with its redacted copy before the model sees it, logs `scan.redact` and files one rotation note per indexed reference (see [What reaches the model](#what-reaches-the-model)). |
| `beekeeper secret compare\|fingerprint\|copy\|set\|rotate\|unset\|reveal` | The credential operations no agent runs itself: equality, keyed fingerprints, a SOPS file copied under a new name and namespace, a value into a SOPS path or a consumer's stdin, a generated value into the shared vault and a SOPS path, a rotation into every SOPS path that carried the old value, keys removed from a SOPS file. They answer key names, lengths, equality and fingerprints, never a value; `reveal` answers the configuration a SOPS file encrypts next to its secrets (ids, URLs, peer names) once the classifier agrees it is no secret; `store` is the person's own, a once-shown value from the clipboard or a download into a vault field (see [Secret operations](#secret-operations)). |
| `beekeeper sandbox render\|install` | The agent sandbox: the policy Claude Code enforces on every session's commands, rendered as a managed-settings drop-in, and the root command that installs it (see [The agent sandbox](#the-agent-sandbox)). |
| `beekeeper scan [index\|add <ref>\|sweep]` | The transcript value scanner: without a subcommand what the fingerprint index holds; `index` rebuilds it from `scan.sops` and `scan.vaults`, `add` indexes one value from stdin, `sweep` counts each reference and token rule in every transcript and names the files, never a value (see [What reaches the model](#what-reaches-the-model)). |
| `beekeeper hook sessionstart` | The SessionStart hook: writes the agent shell's prelude into the session's environment file (`$CLAUDE_ENV_FILE`), which Claude Code sources before parsing each Bash command. It drops the vault credentials (`OP_SESSION_*`, `OP_SERVICE_ACCOUNT_TOKEN`, `OP_CONNECT_TOKEN`) from the environment and removes the aliases and shell functions of `agents.shell.unalias` (default `grep`, `find`, `ls`, `cp`, `mv`, `rm`, the harness's own `grep` and `find` shadows among them), so each name runs the tool on `PATH`, and with `agents.shell.globs: literal` (the default) an unmatched glob stays as written instead of failing the command with zsh's `no matches found`. The directories of `agents.shell.path` go first on `PATH`, in their order and once each: the agent's own programs, such as a `gh` link to devctl that acts with the devctl App's short-lived token in place of the person's long-lived `gh` login. Outside the agent sandbox it records on the session's roster entry the `gh` its shell resolves then (so does `agents register`), and the watch says `GH UNBROKERED` for an agent whose `gh` is not in one of those directories: it acts on the person's own login, or not at all, instead of the App's token. The person's interactive setup stays theirs; an agent writes its commands for the plain tools. |
| `beekeeper lint briefs <file or folder>...` | Refuses dated lines, "until X ships" clauses, notes on the release that fixed something, workarounds and role run numbers in skills and briefs (every Markdown file below a folder), one `path:line: rule: why` per finding, exit 3 on any. |
| `beekeeper hook permissionrequest` | The PermissionRequest hook: answers `allow` only for a session `agents start` started in bypass that now runs in `acceptEdits`; every other request gets no answer, so the person sees the normal card. Below. |
| `beekeeper free [--apply] [--only SECTIONS] [--summary]` | Show where the memory is and, with `--apply`, free what no running work needs: dead sessions' dirs in the tmpfs `/tmp` (the CLI is gone; an idle session's stay), throwaway temp dirs and orphaned jest or Claude workers. Kind clusters, idle CLIs, heavy or runaway processes and Chrome renderers are only reported. Each Claude CLI is listed with its session's title and id, its roster state (busy, parked, idle), its role (supervisor, guide, relay spare) and the hours since its transcript changed; one whose session is untouched for `--cli-stale-hours` (12) with no role and not busy or parked is marked stale with the memory its exit returns, and stays running. Never runs as root: the swap reset and root-owned leftovers are printed as the commands to run. `--summary` prints the TSV rows a desktop front end parses. |
| `beekeeper self-update` | Install the latest signed release over this binary; `--check` only asks. |

Every reading command is written for an agent whose context is its scarcest resource: it prints only what the caller has not read. `snapshot`, `sessions`, `lanes`, `agents` and `handover` keep a read mark per caller and command (`seen.<command>.<caller>.json` in the state directory; `snapshot.<caller>.json` for `snapshot`): a caller that has read before gets the facts that are new or changed since, the keys of those gone (`gone: …`), or one `no change since HH:MM` line. A moving figure (an age, a countdown, a memory size, a context in tokens) is no change. `--full` prints everything, `--json` is always complete, `handover --prompt` is always whole (a successor has read nothing), and a person at a terminal (no Claude session) always gets the whole text. `watch` says each event once and each lasting condition when it starts and when it ends, across restarts: it keeps its open conditions and one-time events (runaways, stale leases) in the caller's mark (`seen.watch.<caller>.json`), so a watch restarted after an install resumes silently and says only an `ENDED` or what is new; `watch --once` keeps no mark and says every condition it finds. Its lines carry no restated thresholds or explanations, those stay in `--help` and here.

Marks are per host session (`CLAUDE_CODE_HOST_SESSION_ID`, else the session): a subagent or `claude -p` worker inside a session shares its parent's marks, so its reads advance what the parent will see as read. A subagent reading beekeeper passes `--full` or `--json`.

Exit codes: 0 done, 1 error, 2 usage, 3 refused (held, not granted, under the floor), 4
relieved (`supervisor status` in the session a relay relieved), 69 the central instance is
unreachable (a central verb, see [The central instance](#the-central-instance)), 125 a newer
release is out (`self-update --check`). A claim's first line is its outcome: `held by you since
<time>` (exit 0), `queued: number <n> behind <holder>` or `refused: <reason>` (exit 3); a kind
lab's held claim adds its kubeconfig's export line after it. A claim exits at once; `--wait
<duration>` claims again every 10 seconds until it is held or the duration has passed. A claim gates the action it guards:
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

- A command that reaches a cluster through the machine kubeconfig without an explicit context: `kubectl`
  (every command but `config`, `completion`, `kustomize`, `help`, `options`, `plugin`, `version --client`,
  `--dry-run=client`, `--local`; a `--server` names its target), `helm`
  install/upgrade/uninstall/rollback/test/list/status/get/history and `flux` (all but `build`, `envsubst`,
  `completion`, the artifact commands, `--export`, `version --client`) with no `--context` (helm:
  `--kube-context` or `HELM_KUBECONTEXT`) or with one that may expand to nothing: `--context ""`,
  `--context "$CTX"` when the command does not set `CTX` to a non-empty word, a command substitution.
  The tools treat an empty context as none and use the kubeconfig's current one, which may be
  production. `--context "${CTX:?no context}"` fails on an empty lookup and passes; so do a literal
  `--context kind-<lab>` or any other name, and a kubeconfig of the command's own (`--kubeconfig <file>`,
  `KUBECONFIG=<file>`, the session's own `KUBECONFIG`), which is the target itself. kubectl plugins are not
  covered. A person's own shells stay outside the rule through `hooks.scope`, as for every guard.
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

## Threat model

Agents act for the person but are not the person: a prompt injection, a confused plan or a wrong
command must not reach a credential. The parties:

| Party | Runs as | Reaches |
|---|---|---|
| The person | their Unix user, at their terminal | everything: the vault through their own sign-in, the keyring, the kubeconfigs |
| beekeeper's broker (`beekeeper sandbox broker`) | the person's user, a systemd user unit no agent starts, undumpable | the vault session (in memory), the SOPS keys, the container runtime, devctl's keychain login |
| A sandboxed agent session | the person's user inside the sandbox (bubblewrap, own mount namespace, no Unix sockets) | its working directory, the temporary directory, beekeeper's state outside `scan/`, GitHub and `sandbox.domains` through beekeeper's egress proxy; no credential |
| An unsandboxed agent session | the person's user | what the person reaches, held back by the PreToolUse hook only |
| Remote services (GitHub, the model API, chat) | elsewhere | what a session sends: the outbound guard refuses secret values ([What leaves the machine](#what-leaves-the-machine)); the mention guard refuses a gh post (issue or pr comment, create, edit, review; a comments, reviews, issues or pulls endpoint of gh api, its body files included) or a GitHub connector post that @-mentions someone: an agent's text goes out under a person's account |

The secrets and where they live:

| Secret | Where | Who reads it |
|---|---|---|
| The vault session (`secret.session`) | the broker's memory; for one call, the environment of the broker's `op` child | the broker, which signs in by itself (`secret.signinCommand`) |
| The vault's service account token (`secret.tokenFile`) | a 0600 file outside the sandbox's lists | beekeeper's own `op` calls |
| The SOPS keys | the person's key files, outside the sandbox's lists | sops in beekeeper's process |
| The GitHub App token of sandboxed sessions | the broker's memory | the egress proxy, into the `Authorization` header of requests to GitHub |
| The person's commit signing key | gpg and its agent on the host, whose socket the sandbox closes | gpg, run by the broker for a sandboxed git's signing call |
| The value scanner's key and index | `scan/` in beekeeper's state, denied to sandboxed sessions | beekeeper |
| Kubeconfigs, the Teleport profile, the GitHub CLI's token, the keyring | the person's home and session bus | the person; a lab kubeconfig for its lease holder |

Why an agent reads no value and opens no vault: a value leaves beekeeper only as a key name, a
length, an equality or a keyed fingerprint ([Secret operations](#secret-operations)); every op, sops
and decryption command, every vault sign-in or unlock and every keyring read is refused in agent
sessions ([Secret reads](#secret-reads)); the agent shell drops vault credentials from its environment;
the vault session exists in the broker alone and only the person's sign-in at their own terminal puts
it there ([The vault session](#the-vault-session)); a sandboxed session can neither read the
credential files nor reach a Unix socket, the keeper's and the session bus included
([The agent sandbox](#the-agent-sandbox)); and a tool result that carries a value is redacted before
the model sees it ([What reaches the model](#what-reaches-the-model)).

Residual risks:

- **An unsandboxed session is not a boundary.** It runs as the person's user, and the hook reads
  command lines: a compiled program, an interpreter's own code (`python -c`), a binary renamed or
  written on the fly reaches what the user reaches, the keyring, the token file, the keeper's socket
  (to lock it, or to hand it a session) and the environment of the broker's `op` child during a call.
  The sandbox is the boundary; the hook is a guard rail that catches mistakes and the plain forms.
- **Credentials in shell startup files.** A credential that shell startup files export comes back in
  every shell an agent's command starts (`zsh -c …` reads them again after the prelude). A vault
  session belongs to the broker only, never in startup files or a store every process of the user reads.
- **The keeper's socket.** `unlock` hands the session only to a listener running this beekeeper
  binary as this user; a process of the user that replaces the binary on disk defeats that check.
- **The egress proxy acts on GitHub with the App token** for every sandboxed session: a session cannot
  read the token, but it can make any call the App's permissions allow, as the person's merges and
  pushes do ([The agent sandbox](#the-agent-sandbox)).
- **The broker signs for every sandboxed session**: a session cannot read the signing key, but its git
  can have any commit or tag signed with it, as the person's own commits are.
- **A held lab's kubeconfig** is readable by every sandboxed session, not by its holder alone.
- **Desktop control.** An agent that drives the desktop could type into the person's terminal; the
  unlock still needs the account password, which no agent holds.

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

- `sops` (decrypting and encrypting alike, `helm secrets` included) and `op` in every form (`op read`,
  `op item`, `op whoami`, `op run`, …), also behind `sudo`, `env`, `timeout`, `xargs` and
  `beekeeper run`; `age -d` and `gpg --decrypt`.
- Every sign-in to or unlock of a vault: `op signin`, `op account add`, `op unlock`, the person's own
  unlock helpers (`secret.unlockCommands`, by name under any path; the broker alone runs one, as
  `secret.signinCommand`), `beekeeper secret unlock` and `beekeeper secret store`, which are the person's; and keyring reads (`secret-tool lookup|search`, macOS `security find-*-password`). No
  agent session holds or opens a vault session: it lives in beekeeper alone, and the refusal says so.
- `kubectl edit` of a Secret and `kubectl view-secret`.
- A hash (`sha*sum`, `md5sum`, `b2sum`, `cksum`, `openssl dgst`) or a diff (`diff`, `cmp`, `git diff
  --no-index`, …) of a file whose name says it holds secrets in plaintext (`secrets.yaml`,
  `token.txt`, a kubeconfig; code files and SOPS-encrypted `*.enc.*` or `*.sops.*` files pass).
- `vault` beyond `status`, `version`, `list`, `kv list`, `kv metadata get` and the `kv get`/`read`
  forms below.

Files known to hold secret values are never read whole: omp's provider configuration
(`~/.omp/agent/models.yml`, `config.yml`, `agent.db`), `~/.claude/.credentials.json`,
`~/.config/gh/hosts.yml`, `~/.docker/config.json`, `~/.netrc`, `~/.git-credentials`, the vault's
`secret.tokenFile`, and the files `secret.files` adds (globs and `~/` allowed). The hook refuses a
`Read` of one, a content `Grep` of it or of a directory holding it below the home directory, and a
command that names it (`cat`, `less`, `head`, `tail`, `cp`, `jq .`, `yq .`, `grep`, `sqlite3`, a
symlink to it, a glob that expands to it, a path relative to a `cd`), also behind `sh -c`. The
refusal names the read that passes, built from the key names the file holds (never a value): a `jq` or
`yq` filter whose first stage deletes every credential key wherever it stands
(`yq -y 'del(.. | .apiKey?, ."X-Auth-Token"?)' <file>`, piped or not), a keys-only filter
(`yq 'keys'`), `grep -c` or `-l`. Metadata commands pass (`ls`, `stat`, `wc`, `find`, `mv`, `chmod`),
and so do `omp` and `beekeeper`, which read the file without printing it. A file that is no YAML or
JSON document has no key-stripped read.

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

The same holds inside what a command line runs besides itself: every argument of `sh|bash|zsh` (a
`-c` or `-ic` string, a single word such as `zsh -ic <helper>` included, and `"$SHELL"`), `ssh` and
`watch`; `eval`'s words; the body of a shell function or group on the line (`f(){ …; }; f`); here-documents
fed to a shell; and a script it runs (`bash x.sh`, `./x.sh`, `source x.sh`, read from the session's
working directory, a text file up to 64 KiB, nested scripts four deep). Quoted text, comments and other
here-documents (a commit message, an issue body) are not commands and pass.

The agent shell prelude (`beekeeper hook sessionstart`) also drops `OP_SESSION_*`,
`OP_SERVICE_ACCOUNT_TOKEN` and `OP_CONNECT_TOKEN` from the environment before every command, and the
aliases and shell functions of `secret.unlockCommands`, so a vault credential in the environment never
reaches an agent's command.

## Secret operations

`beekeeper secret` is how an agent gets a credential job done without reading a value: sops and op
run in beekeeper's process, a value stays in its memory for the one operation and is written nowhere
in plaintext (sops gets the plaintext on stdin, the file is written from its ciphertext), and an
answer is key names, lengths, equality or keyed fingerprints. Every call is a `secret.<operation>`
event in `beekeeper log` with the session, the references, the outcome and the operation's duration,
never a value: the entry waits for a busy state lock, a call whose entry cannot be written fails with
the reason and the entry, and a call the broker refused, left waiting on the locked vault or ended on
its deadline is logged by the broker as the requester's.

A reference is a SOPS file (every value in it), one value of a SOPS file (`file#a.b.c`, the dotted key
path; `sops://` in front optional), a field of the shared 1Password vault
(`op://<vault>/<item>/<field>`) or, as a source, a key of a Kubernetes Secret
(`k8s://<context>/<namespace>/<name>/<key>[#<path>]`, the `copy` row below). beekeeper reads only the vault `secret.vault` names, and only as its
service account, whose token it reads from `secret.tokenFile` and gives to its own `op` calls alone;
with either unset, an `op://` reference is refused. `secret.session: true` reads and writes the vault
through the person's own `op` session instead (`tokenFile` unused, `setup` refused): the way to a vault
no service account can be granted, such as a person's Employee vault. That session lives in the broker's
memory alone ([The vault session](#the-vault-session)), so every call on an `op://` reference runs
there, from an agent session, a sandboxed one or the person's terminal alike. Every command exits 78
when the vault cannot give a value: none configured, no token, the vault locked past
`secret.unlockWait`, or `op` failing or answering nothing within a minute. A SOPS file is encrypted under the creation rules
of the `.sops.yaml` nearest above it, run from that directory, so a `path_regex` relative to the
repository matches. beekeeper reads YAML documents: a file whose name sops does not type (a Helm values patch,
`*.yaml.patch`) is read and written as YAML.

| Command | What it does |
|---|---|
| `compare <a> <b>` | `equal` or `different` for two values, and for two files each key's state (`equal`, `different`, `only in a`, `only in b`); exit 1 when anything is not equal. |
| `fingerprint <ref>` | HMAC-SHA256 of each value under the value scanner's key (`scan/key`), cut to 16 hex digits: equal values, equal fingerprints; only beekeeper can make one. |
| `fingerprint <ref> --encode <encoding>` \| `fingerprint --secret <context>/<namespace>/<name>/<key>` | The fingerprint of one value in an encoding (the form `copy --encode` writes), or of a key of a Secret in a kind lab whose lab lease the caller holds: a delivery checked without a value read. |
| `copy <src.sops.yaml> <dst.sops.yaml> [--name n] [--namespace ns]` | A new SOPS file with src's values, encrypted under dst's rules; `--name` and `--namespace` rewrite a Kubernetes object's metadata. It answers each key and its length (a Secret's `data` decoded); dst must not exist. |
| `copy <ref> <file#path>` | One value into a SOPS path, creating the file or the key when absent, the file's other values kept. |
| `copy k8s://<context>/<namespace>/<name>/<key>[#<path>] <file#path>` | A key of a Kubernetes Secret as the source of any copy (a SOPS path, `<ref>=<path>…`, a consumer, `--to-secret`), of `compare` and of `fingerprint`; `#<path>` picks one value inside what the key holds: a dotted key of its YAML or JSON mapping (tab-indented too), a line of its dotenv lines (`KEY=value`, an `export` in front and matching quotes stripped, `#` comments ignored) or key-value lines (`key = value` or `key: value` with any spacing, CRLF line ends, quotes stripped; a bare line right before them a section, `#<section>.<key>`, each line also by its own key), or, one level down, a value of the YAML mapping or dotenv lines a block scalar holds (`#config.yaml.oidc.clientID`; a key holding dots takes as many segments as it needs). A credential its owner placed in a cluster Secret (an identity provider's client secret for a connector, for one) reaches a SOPS path without a person and without its value reaching a session: one Secret GET in beekeeper's process, through kind for a lab's context (`kind-<cluster>`, its lab lease held by the caller) and through the kubeconfig files `kube.kubeconfig` names (`KUBECONFIG`'s, else `~/.kube/config`) for any other, a production context included, nothing written to that cluster. A Secret or key absent fails in one line naming it; a path that reaches nothing names the shape of what the key, or the node on the way, holds (a YAML or JSON mapping with its top-level keys, dotenv or key-value lines with their keys, a list's length, a scalar, text with each line's head before its first `:` or `=` and a line without either by its length, binary; its lines and bytes), a key named like a generated value counted, no content quoted; the answer is the value's length. |
| `copy <ref>=<path>… <new-file> [--name n --namespace ns]` | Several values (vault fields, SOPS paths) into a new SOPS file in one encryption: only the recipients of the nearest `.sops.yaml` are needed, nothing is decrypted, so no age identity of the new file. `--name` and `--namespace` start it as that Secret, a bare path under `stringData`. Answers key names and lengths. |
| `copy <ref> -- <consumer…>` | One value on the stdin of `gh secret set`, `garage json-api <endpoint> -`, `gitlab-rails runner [-e <env>] '<code>'` whose Ruby code reads `STDIN`, a command with `--password-stdin` or one with `--secret <name>=-`, or of one of them in a pod through `kubectl exec -i --context <context> <pod> -- <consumer…>` (no TTY, which would echo stdin, no `-v`, never a context of `kube.production`): the value travels on the exec stream, never on an argv. Any other consumer is refused. `--stdin-json '<object>' --stdin-field <key>` hands the consumer that JSON object with the value at `<key>` instead of the bare value, for a command that reads a JSON request, such as Garage's `garage json-api ImportKey -` with `--stdin-json '{"accessKeyId":"GK…","name":"app"}' --stdin-field secretAccessKey`; `kubectl exec` starts `/garage` itself, so an image without a shell takes it, and the CLI reaches the admin API over RPC with the pod's own configuration, no admin token. It answers the consumer's exit code and its output with the value redacted (its JSON-escaped and base64 forms included). |
| `copy <ref> --git-askpass -- git …` | A git operation over HTTPS (`git [-C <dir>] clone\|fetch\|push\|ls-remote …`) with the value as the password: `GIT_ASKPASS` names a helper beekeeper writes for the one call, which asks beekeeper on a socket only git's own processes reach; it answers `--git-user` (default `oauth2`) to the Username prompt and the value to the Password prompt. The value is on no argv, in no file, in no environment and not in the answer; git runs without the global and system configuration and without hooks, a repository whose configuration names a credential helper is refused, and so are `-c`, `--config`, `--upload-pack`, `--receive-pack` and `--exec`. A brokered call runs in the broker's directory, so `-C` names the repository by its absolute path. The output comes back redacted with the exit code. |
| `copy <ref> --to-secret <context>/<namespace>/<name>/<key>` | One value into a key of a Secret in a kind lab (`kind-<cluster>`) whose lab lease (`labs`) the caller holds: a merge patch of that one key under the field manager `beekeeper-secret`, which creates the Secret when absent and leaves its other keys, labels and annotations as they are (a second copy into another key of the same Secret keeps the first), through the admin kubeconfig `kind get kubeconfig` answers, which stays in beekeeper's memory like the value. Any other context, and a lab the caller does not hold, is refused (exit 3). It answers the value's length. |
| `set <file> <path> --generate [--vault op://…] [--to-secret <context>/<namespace>/<name>/<key> \| -- <consumer…>]` | A new value (`--length`, 32; `--charset`, `alnum`, `hex` or `ascii`), drawn in beekeeper's process, into the SOPS path (the file or key created when absent); it answers the fingerprint. A plaintext Kubernetes Secret without values (apiVersion, kind, metadata, an empty `stringData`), a skeleton, becomes the SOPS file, encrypted to its `.sops.yaml` recipients; any other plaintext file (a ConfigMap, a Secret holding a value, no YAML mapping) is refused by what it is, naming the way on, before sops sees it. A Secret's value goes under `stringData` unless the path names `data` or `stringData` (`set <skeleton> default` writes `stringData.default`). A path the file's `.sops.yaml` creation rule would leave in plaintext (outside its `encrypted_regex` or `encrypted_suffix`, matching its `unencrypted_regex` or `unencrypted_suffix`, or under a rule that decides by comments) is refused before any value is drawn, naming the rule. `--name` and `--namespace` start an absent file as that Secret (`type: Opaque`), ready for Flux. Without `--vault` the SOPS file is the value's only home, for a credential no vault may hold. `--vault` writes the vault field first (the item or field created when absent, the item passed as JSON on stdin, never on a command line). `--to-secret` (a held lab, as for `copy`) or a consumer (the `copy` consumers, its output redacted and its exit code answered) receives the same value in the same call, after the SOPS path; a delivery that fails leaves the value in the SOPS path, for `copy` to finish. A refused lab or consumer is refused before any value is drawn. |
| `capture <file#path> [--jq <filter>] [--encode …] [--vault op://…] [--name n --namespace ns] -- <producer…>` | A credential its issuer generates and prints once (`az acr token credential generate`, a token created over an API) into a SOPS path, and the shared vault's field first with `--vault`: the producer runs in beekeeper's process with the caller's environment minus the vault's credentials, `SOPS_AGE_KEY` and the names `secret.env` lists, its stderr passed through; the value is its stdout without the line end, or the one string `--jq` picks from its JSON (`.passwords[0].value`). Every check (references, creation rule, filter) runs before the producer; a non-zero exit, empty stdout or a filter that picks no single non-empty string is refused in one line, nothing stored. `--dockerconfigjson <registry> --username <user>` takes the file alone and writes a `kubernetes.io/dockerconfigjson` Secret, the value the registry's password. It answers the length as written and the keyed fingerprint, never the producer's output; `beekeeper log` records the producer's command and the references. |
| `setup [--service-account <name>]` | Gives beekeeper the shared vault, once: `secret.vault` created when the person's 1Password session finds none, a service account (default `beekeeper-<host>`) that reads and writes that vault only, and its token written from `op`'s output straight to `secret.tokenFile` (mode 0600). It runs `op` as the person, in the caller's signed-in session, and answers the vault, the account and the token's length; a token file that holds a token is refused (exit 3). |
| `import op://<vault>/<item>/<field> op://<shared>/<item>/<field>` | One field of a vault outside the shared one, read with the person's session, into a field of the shared vault, written as the service account (the item or field created when absent); it answers the length. From then on the shared reference is the one to use. With `secret.session` the broker runs it in the person's session it holds, for a sandboxed agent too; without, it runs on the host only. |
| `import op://<vault>/<item>/<field> --recipient <age1…>` | A SOPS recipient's age identity into the shared vault's `sops age key <recipient>` item: the source (an identity, or a `keys.txt`-shaped notes field with comments above it) is refused unless beekeeper derives that recipient from it, and only the identity's line is stored. |
| `rotate op://… --generate` | A value beekeeper made gets a new one (`--length`, `--charset` as for `set`): the vault field first, then every path of the SOPS files `scan.sops` names that carried the old value, the value itself or its base64 form (a Secret's `data`), matched by fingerprint. |
| `rotate op://…` | A value a third party issues: the person rotates it at its issuer into the vault field, and `rotate` writes the vault's new value into every path that carried the old one, known by the fingerprint `beekeeper scan index` recorded before the change. It refuses while the vault still holds the recorded value. |
| `unlock [--account <a>]` | The person's, in their own terminal: `op signin` on that terminal, the session handed to the broker ([The vault session](#the-vault-session)). Refused in an agent session and without a terminal; the hook refuses it in agent sessions too. |
| `lock` | The broker forgets the vault session. |
| `status` | Whether the broker holds the vault session, never the session. |
| `store op://<vault>/<item>/<field> [--from-file <path>] [--keep-file]` | The person's, in their own terminal: a value a web page showed once, or a file it downloaded, into a field of the shared vault (the item created when absent), from the Wayland clipboard (`wl-paste`, cleared afterwards) or from the file `--from-file` names (shredded afterwards unless `--keep-file`). The value travels in beekeeper's memory alone, with `secret.session` to the broker over the keeper's socket and from there to op's stdin: never on a command line, in a spool or state file or in `beekeeper log`, which records the reference, the length and the keyed fingerprint, which `store` prints too. While the broker holds no session it says so and stops (exit 78) before it asks for anything. Refused in an agent session and without a terminal, and by the hook in agent sessions, which names the way: a one-line question to the supervisor, the person runs it. |
| `store --github-app <item> [--from-file <key.pem>] [--keep-file]` | A GitHub App's credentials in one call: the client secret from the clipboard into the item's `client-secret`, the private key from the newest `*.private-key.pem` in `~/Downloads` (or `--from-file`) into `private-key`, the file shredded. It names the click that makes each (Generate a new client secret, Generate a private key) and waits for Enter while the clipboard is empty or the download absent. |
| `rotate platform://<installation>/<capability>/<name> --reason <text>` | A credential the platform manager generates: `platformctl installation reconcile <installation> <capability> --commit --rotate <name>` on the host, the manager writing the new value into the installation's SOPS files in a pull request; nothing is decrypted and no copy reaches the vault. `--dry-run` shows the files that hold it. |
| `recipients [<directory> \| <sops-file>]` | For a directory (the working directory by default), every age recipient of the creation rules of the `.sops.yaml` nearest above it, a gitops repository's installations' for one (`management-clusters/graveler/.*` → its recipient); for a SOPS file, the file's recipients (its metadata when encrypted, else its creation rule's). Each comes with where its identity is: sops' own sources, the entry of `secret.ageIdentities`, the shared vault's item per recipient or the installation's item `<installation>.agekey` in `secret.vault` or `secret.ageVaults` (found in the vaults' item listings, metadata only) or `none`, naming the items the vaults lack. No value is read; exit 1 when a recipient has no identity. With `secret.session` it runs in the broker, like a call on the vault. |
| `reveal <sops-file> <path>…` | The configuration a SOPS file encrypts next to its secrets, in plaintext: each path one scalar or every scalar under it (an OAuth client's `id`, `name`, `redirectURIs`, a `trustedPeers` list), for a split that moves the secrets into SOPS files of their own and the configuration into a plaintext values file. Every leaf is classified first, and one that looks secret refuses the call (exit 3), named by its path and the reason, never its value: a key on its path named like a secret (`secret`, `password`, `token`, `key`, `credential`, `private`, `cookie`, `salt`, `hmac`, `cert`), a value the value scanner matches (a token pattern or an indexed secret), a URL carrying a password, a run of 16 or more characters at a key's entropy, except in a client id (a leaf named `id`, `clientID` or `client_id`, an item of a `trustedPeers` or `peers` list), public in every authorize URL however random, which is refused instead when it equals a value under a secret-named key of the same file. `--json` answers `[{"path", "value"}]`, a tool's input. `--to <plain.yaml#path>` writes what one path names into a path of a plaintext YAML file instead (created when absent, its other keys kept), printing no value: a dry run without `--write`, `unchanged` when the destination holds it already; a SOPS file as the destination is refused. |
| `unset <sops-file> <path>… [--write]` | Keys out of a SOPS file (a value, a mapping or a list item; a number in the dotted path indexes a list), with `sops unset` in beekeeper's process and the file's age identity: the file keeps its recipients, sops renews its MAC and `lastmodified`. A dry run without `--write`. A path the file holds no longer is answered `absent`, so a second run removes nothing; a path under another named one goes with its parent, a list's later items first. It answers the paths removed, those absent and the key names the file keeps, never a value. |

### Encoded values

Some consumers inject a value verbatim: a gateway's egress credential that sends
`Authorization: Basic <value>` needs the Secret to hold `base64("<user>:<token>")`, not the token.
`--encode` on `copy` (one value: into a SOPS path, a consumer's stdin or a lab's Secret) and on `set`
makes that form in beekeeper's process, after the value is read or drawn and before it is written;
the encoded value is never printed, logged or returned, and the answer is its length.

| Encoding | Written |
|---|---|
| `base64` | the value in standard base64 |
| `basic:<user>` | `base64("<user>:<value>")`, an HTTP Basic credential (a user without a colon) |

An unknown encoding, a whole SOPS file and `copy <ref>=<path>…` are refused before any value is
read. `set --encode` writes the encoded form to the SOPS path, the Secret and the consumer, and keeps
the generated value in the vault field; the fingerprint it answers is the encoded form's. A delivery
is checked with two fingerprints:

```sh
beekeeper secret copy op://<vault>/<item>/<field> --encode basic:x-access-token \
  --to-secret kind-<lab>/<namespace>/<name>/<key>
beekeeper secret fingerprint --secret kind-<lab>/<namespace>/<name>/<key>
beekeeper secret fingerprint op://<vault>/<item>/<field> --encode basic:x-access-token
```

A beekeeper without `--encode` reaches the same form through a consumer that encodes on the host,
`copy <ref> -- <consumer…>`, as long as the consumer reads the value on stdin and prints nothing of it.

### Age identities

sops decrypts a file encrypted to age recipients with an identity from its own sources:
`SOPS_AGE_KEY`, the file `SOPS_AGE_KEY_FILE` names, and `sops/age/keys.txt` in the user's config
directory. Before sops runs, beekeeper reads the file's recipients from its plaintext metadata and
checks those sources for an identity of one of them; without one, it takes the entry of
`secret.ageIdentities` that names one of the recipients or the file's path, and for a recipient no
entry names, the shared vault's item per recipient (below). A file with none fails before sops, in
one line. A file with another key group (KMS, PGP, Vault, key groups), an SSH or plugin recipient,
or with `SOPS_AGE_KEY_CMD` or `SOPS_AGE_SSH_PRIVATE_KEY_FILE` set goes to sops unchecked.

`secret.ageIdentities` supplies an identity no local source holds, from the shared vault, from an
identity file on the host that is to stay out of every vault, or from the person's own credential
store:

```yaml
secret:
  ageIdentities:
    - recipient: age1…                       # the files encrypted to this recipient
      ref: op://<vault>/<item>/<field>        # the AGE-SECRET-KEY-1… identity
    - pathRegex: /installations/[^/]+/secrets/  # or every file under a path (absolute, unanchored)
      ref: op://<vault>/<item>/<field>
    - recipient: age1…
      ref: file:///home/<person>/<identity file>  # an age identity file (absolute path)
    - recipient: age1…
      ref: store://<entry>                    # an entry of the person's own credential store
    - recipient: age1…
      ref: store://                           # the entry the store's search finds for the recipient
  store:                                      # the person's own commands, run by the broker
    read: [<command>, <args>…]                # prints the entry appended as last argument
    search: [<command>, <args>…]              # prints the names of the entries matching the term
```

The first entry whose recipient is one of the file's, or whose `pathRegex` matches the file's
absolute path, is read like any `op://` reference (the vault must be `secret.vault` or one of
`secret.ageVaults`) or, for a
`file://` reference, from the file as `age-keygen` writes it (comment lines and several identities
allowed), checked to hold the identity of one of the file's recipients, and only that identity is
given as `SOPS_AGE_KEY` to the one sops call's environment, never written anywhere. With
`secret.session` such a call runs in the broker, like a call on an `op://` reference: an identity
file is read in the broker's process alone, without the vault session, and never copied, printed or
fingerprinted.

A `store://` reference reads an entry of the person's own credential store (a password manager, or
a keyring behind the freedesktop Secret Service API, `secret-tool lookup` for one) through the
person's own commands in `secret.store`, which the broker runs like a vault field without the vault
session: `read` prints the entry's secret on stdout, the entry appended as its last argument.
`store://` with no entry asks the store's own search (`search`, the term appended) for each of the
file's recipients, and reads the entries it names, one per line: an identity is found by its public
recipient, never by listing values. beekeeper never handles the store's password; the store shows
whatever unlock prompt it shows the person, and a command answers within five minutes or fails. Its
output stays in the broker; an error names the entry or the recipient, never a value.

A `store://` reference and `secret.store` are set together, in one `beekeeper config set` call (see
[Configuration](#configuration)). A reference whose `secret.store` is still missing leaves the
configuration loadable: every command works, and `beekeeper secret …` warns once per such reference
until the store is set.

#### The vault's item per recipient

A recipient no entry of `secret.ageIdentities` names, a test installation's for one, has its
identity in the shared vault (`secret.vault`) under one naming convention: an item titled
`sops age key <recipient>` (the recipient as the gitops repository's `.sops.yaml` names it,
`sops age key age1…`), whose `password` field holds the `AGE-SECRET-KEY-1…` identity. A person
creates the item once, as a Password item in the vault; the item is the grant. Nothing else is
configured: the broker resolves a file's recipients from its metadata and, for a file still to be
written, from the creation rule of the `.sops.yaml` for its path (`management-clusters/<installation>/…`
by `path_regex`), finds the item in the vault's item listing (titles only, metadata, no value) and
reads it like any `op://` field for the one sops call. `beekeeper secret recipients <directory>`
shows, for a gitops repository's path, each creation rule's recipient and where its identity is,
reading no value. A file whose recipient has neither an entry nor an item fails before sops in one
line, naming the installation (`alerts.installations`, by the directory of the file's path), the
recipient and the item the vault lacks: a Secret for it is one no agent can change, and no person
is asked to decrypt it. With `secret.session` the call runs in the broker, like a call on the vault.

#### The installation's item

A customer's gitops repository keeps an installation's files under `installations/<installation>/`,
and the identity of the installation's recipient lives in an item titled `<installation>.agekey`,
the identity file's text in one of its fields (a Secure Note's `notesPlain`, for one) or as the
document it is. beekeeper looks the item up by the installation the file's path names, in
`secret.vault` and in every vault of `secret.ageVaults`, the vaults the same session or service
account reads age identities from:

```yaml
secret:
  vault: <shared vault>
  ageVaults: [<vault holding the installations' .agekey items>]
```

The item is found in the vaults' item listings (titles only, metadata, no value); the broker reads
its fields in its own process for the one sops call and gives sops only the identity of the file's
recipient, so an item holding another recipient's identity is refused. No entry per installation is
configured. An `op://` entry of `secret.ageIdentities` may name a field of one of `secret.ageVaults`;
no other `op://` reference reads them. `beekeeper secret recipients` names the item and its vault, or,
when no vault holds it, the items the vaults lack.

### The vault session

With `secret.session`, no agent ever holds the vault session or opens one. It lives in the memory of
`beekeeper sandbox broker` (`beekeeper-sandbox.service`), a process of the host that no agent session
starts; never in a file, a keyring entry or an agent's environment. The broker makes itself undumpable
(no other process of the user reads its memory or environment), drops every vault credential from what
its calls inherit, and gives the session to its own `op` calls alone, in their environment.

The broker signs in by itself: it runs `secret.signinCommand` when it starts and whenever a call needs
the vault while it holds no session, one sign-in at a time that every waiting call shares. The command
signs in without the person (a helper that reads the account password from a local password store,
for example) and prints the session as `op signin` does (`export OP_SESSION_<id>="<token>"`) on
stdout, which the broker reads into its memory; its stderr goes to the broker's journal. It runs with
the broker's environment, the vault credentials removed; a command that needs more memory than the
broker's unit allows runs in a unit of its own (`systemd-run --user --pipe --wait --quiet -p
MemoryMax=1G -- <helper>`). The broker holds the session for `secret.sessionLifetime` (12 h), touches
it every 10 minutes with a vault listing (a call that reaches 1Password: `op whoami` does not reset
op's 30-minute idle timeout) so that op does not let it idle out, and forgets it at the end of the
lifetime. A session op no longer takes, found by the touch or by a call that op answers "not signed
in", is dropped and signed in again at once through `secret.signinCommand`, the call retried once
with the new session; the journal and the watch (`VAULT SESSION DROPPED at <t>: op no longer took it
(<reason>); the broker signs in again`, ended by the new sign-in) say so.

A sign-in that fails is tried again, after 30 s, 1 m, 2 m and then every 5 m, within `secret.unlockWait`
of the ask, each try bounded by two minutes. Before each try the broker asks the session bus for the
person's credential store (the freedesktop Secret Service: KeePassXC, GNOME Keyring, KWallet) and waits,
looking every 15 s, while it is locked and, within ten minutes of the boot, while nothing serves it yet;
later an absent store may be no Secret Service at all, and the command runs. The journal names each
failure's cause (`the credential store is locked`, `no credential store answers on the session bus`,
`the network did not reach 1Password`, `the sign-in did not finish in time`, `1Password rejected the
password`) with the command's last line, and the watch says `VAULT SIGN-IN RETRYING: <cause> (<line>);
try <n> at <t>`, or `… waiting for the credential store: <state>`, until the sign-in unlocks or gives
up. A sign-in that gives up is `VAULT SIGN-IN FAILED: <cause> after <n> tries: <reason>` and a
`vault.signin` line in `beekeeper log`; the broker signs in again at the next call on the vault. Only a
password 1Password rejected while the store answered unlocked (or could not be asked) is for the person:
one sign-in note for `guide.person`, which closes by itself once `beekeeper secret status` finds the
broker holding a session (exit 78 while it holds none). A store that was locked or absent is the cause
whatever the command said: a helper that read no password reports what 1Password said to the empty one,
and a note asking the person to fix an entry that is fine would be wrong.

While the broker holds no session, a call on the vault prints `vault locked: waiting for the broker's
sign-in` and waits up to `secret.unlockWait` (8 m; the hook gives such a call the Bash tool's 10
minutes), then exits 78. Nothing asks the person. The watch says `VAULT UNLOCKED: the broker holds the
vault session since <t> until <t>` for the session's lifetime and an ENDED line when the broker
forgets it; `VAULT SIGN-IN FAILED: <reason>` once a sign-in gave up; `VAULT LOCKED: <who> waits
on <ref>` for each waiting call, with an ENDED line once it goes on, or `VAULT LOCKED: <who>'s call on
<ref> timed out …, still locked` when it gives up. `beekeeper status` names the waiting sessions.
`beekeeper secret lock` forgets the session at once. The broker runs on Linux only, so
`secret.session` needs it there.

Without `secret.signinCommand`, the person hands the broker a session with `beekeeper secret unlock`
in their own terminal: `op signin` runs on that terminal and the person types the account password
into op's own prompt; the session goes from beekeeper's memory to the broker over a Unix socket in the
runtime directory (`$XDG_RUNTIME_DIR/beekeeper/vault.sock`, mode 0600 in a 0700 directory, which the
sandbox can neither reach nor write), after `unlock` checked that the listener is the main process of
`beekeeper-sandbox.service` running this beekeeper binary as this user (from systemd: the broker's own
`/proc/<pid>/exe` is root's, since it is undumpable). The socket answers only whether a session is
held. `unlock` refuses in an agent session (Claude Code's, omp's or the sandbox's variables set) and
without a terminal, and logs a refusal; the hook refuses it in agent sessions before it runs.

The 1Password desktop app's CLI integration (op asks the app, the app asks the person through the
system authentication prompt) is not used: it lets any process of the user that calls op raise the
prompt, so an unsandboxed agent calling op directly would raise the same prompt as beekeeper, and the
person could not tell them apart.

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
`beekeeper sandbox install` stages it and prints the root commands that put it into Claude Code's
managed settings (`/etc/claude-code/managed-settings.d/beekeeper-sandbox.json` on Linux,
`/Library/Application Support/ClaudeCode/managed-settings.d/` on macOS). On a fresh machine they create
the managed settings file (`managed-settings.json`, an empty `{}`) and its `managed-settings.d`
directory first: the sandbox mounts both read-only and cannot create them, so a session held by
`claude --settings` needs them as well. From there it holds every
Claude Code session on the machine, the headless turns beekeeper starts and the CLI the desktop app
spawns alike, enforced by Anthropic's sandbox runtime (bubblewrap on Linux, Seatbelt on macOS). The
same file passed to `claude --settings` holds one session to it, to try a change first.

- **Reads:** the home directory is denied; beekeeper's binary, config and state, the harness's
  transcripts, plans, skills and plugins, git's config and `sandbox.allowRead` are re-allowed. A
  symlinked config (the config directory, `~/.gitconfig`, beekeeper's config) is mounted at its target
  only, so the policy names it there (`XDG_CONFIG_HOME`, `GIT_CONFIG_GLOBAL`, `BEEKEEPER_CONFIG`).
  Kubeconfigs, the Teleport profile, the GitHub CLI's token file and every other credential nobody
  listed stay unreadable. Only the managed settings' read paths count.
- **Writes:** the session's working directory (when it is readable itself), the temporary directory,
  beekeeper's state, the build slots and `sandbox.allowWrite`. The sandbox mounts a readable path
  read-only over a writable one inside it, so `beekeeper sandbox install` refuses that layout and
  names both paths.
- **The harness's config directory** (`~/.claude`, or `CLAUDE_CONFIG_DIR`) is denied for writing:
  Claude Code mounts it writable in the sandbox and masks only the entries it knows, so a command could
  otherwise leave a new file there that the harness reads outside the sandbox. The file tools still write
  the sessions' memory and plans.
- **The scanner's key and index** (`scan/` in beekeeper's state) are denied for reading and writing
  inside the writable state directory: the narrower deny holds, so no session reads the fingerprint key
  or rewrites the index the redaction matches against. `beekeeper sandbox install` and the broker
  at its start create `scan/`, the mount point the sandbox cannot create itself.
- **Egress:** through beekeeper's egress proxy alone, to GitHub and `sandbox.domains`, nothing else,
  with no prompt to widen it. The policy points Claude Code's `httpProxyPort` and `socksProxyPort` at
  the broker's proxy (`127.0.0.1:<sandbox.proxyPort>`, 3190), so Claude Code bridges the sandbox's HTTP
  and SOCKS5 proxies there and runs no proxy of its own; the broker's proxy answers its own user only.
  An entry is a host, `*.<domain>` for its subdomains, or `host:port` for one port. Loopback is listed
  by port only (`127.0.0.1:<port>`, a lab's API server): a bare loopback host or a wildcard port would
  open every listener on the host (other sessions' port-forwards, local servers), so the configuration
  refuses it, and a name that resolves to a loopback, link-local, unspecified or multicast address is
  refused when the proxy dials it. A refused target gets a 403 (SOCKS: not allowed) and a line in the
  broker's log.
- **The runtime directory** (`$XDG_RUNTIME_DIR`) is denied for reading like the home directory (the
  container runtime's registry login, the agents' sockets), apart from the egress proxy's directory.
- **The GitHub token:** `gh`, `git push` and `devctl` act with devctl's App user token (eight hours,
  capped by the App's permissions), and the sandbox never holds it, not even as a placeholder. The
  broker reads it every five minutes with `devctl auth exec` (`sandbox.devctl`) into its memory. Its
  egress proxy terminates TLS for `github.com`, `api.github.com` and `uploads.github.com` with a CA it
  makes at start, its key in memory alone and name-constrained to those hosts, and sets each request's
  `Authorization` header itself (Basic for git on `github.com`, a token for the API), replacing what
  the client sent; bodies pass untouched, so no endpoint can echo the token back. Every other host is
  an opaque tunnel. The broker writes the CA, a bundle of the system's roots and the CA, and gh's login
  (a fixed word, `beekeeper-egress-proxy`, which the proxy replaces) to
  `$XDG_RUNTIME_DIR/beekeeper/egress`; the policy points `SSL_CERT_FILE`, `GIT_SSL_CAINFO`,
  `CURL_CA_BUNDLE` and `REQUESTS_CA_BUNDLE` at the bundle, `NODE_EXTRA_CA_CERTS` at the CA and
  `GH_CONFIG_DIR` at gh's login, and git's `gpg.program` at the signing script. These variables reach the harness process too; the bundle keeps the
  system's roots, and the CA is good for GitHub's three hosts alone. git reaches GitHub over HTTPS
  (`git@github.com:` is rewritten), needs no credential helper, since the proxy authenticates its first
  request, and runs none of the person's for GitHub. `agents.shell.path` stays off `PATH` in the
  sandbox, an inherited `PATH` included, since a `gh` link to devctl reads the keychain.
- **The policy's environment without its sandbox.** Claude Code can apply the policy's `env` and
  still run a session's commands unconfined (its `sandbox` block not in force). No proxy then
  completes gh's login, so the variables are only in force where the sandbox runtime holds the
  command (`SANDBOX_RUNTIME`, which the host never has). Elsewhere the agent shell's prelude, and
  beekeeper itself at start (outside its hooks), drop the egress variables and `BEEKEEPER_SANDBOX`:
  gh, git and devctl act as on the host, with `agents.shell.path` first on `PATH` as outside the
  sandbox.
- **Commit signing.** The sandbox reaches no gpg-agent, so the policy sets git's `gpg.program` to a
  script the broker writes into the egress directory, which runs `beekeeper sandbox gpg`: it hands
  the payload of git's signing call (`--status-fd=2 -bsau <key>`, nothing else) to the broker, which
  runs the person's gpg (`gpg.program` of their git config, else `gpg`) on the host and answers the
  signature and gpg's status lines. `commit.gpgsign` and `tag.gpgSign` work unchanged; the key and
  the agent never enter the sandbox.
- **devctl.** devctl's gated commands (`pr merge`, `pr wait`, `release promote`, `release wait`,
  `rollout wait`) read the keychain over the user bus and start user units, both closed in the sandbox,
  so a sandboxed `beekeeper gate` hands its command to the broker: it runs the same gate on the host, as
  the session and in its working directory, with `sandbox.devctl`'s directory first on `PATH`, and its
  output and exit code come back through the spool once it ends (up to three hours). Only those
  commands are brokered, and a queued merge's own run is refused from the sandbox.
- **The roles' host commands.** `beekeeper agents start`, `wake` and `resume` start transient user
  units over the user bus, the watch reads the installations through the person's kubeconfig and
  Teleport login, `beekeeper person` reads the org's roster with the person's own `gh` login, and a
  lab's creation needs the container runtime: all closed in the sandbox. A
  sandboxed call of each hands its command line to the broker, which runs it on the host as the
  session, in its working directory, with the command's own checks: a start's brief and `--dir` held
  to the session's lists, a lab only for the holder of its lease. Their output streams back while they
  run, and a call whose sandboxed command ended (a stopped `Monitor`) is ended with it. The watch moves
  into a scope of its own (512M), out of the broker's unit; a lab's `agentlab up` or `down` runs capped
  as `beekeeper run` caps it. In the sandbox the hook refuses `agentlab up|down` and `kind
  create|delete cluster` and names `beekeeper lease up|down <lab>` instead. Nothing of the person's
  kubeconfig or Teleport profile enters the sandbox: only the watch's lines and the lease's own lab
  kubeconfig do.
  A start, wake or resume with `--config <scratch>` (or under `$BEEKEEPER_STATE_FROM`) keeps the
  scratch configuration's state: the call still asks the host's broker, which takes only the scratch
  file's `stateDir` and `leaseDir`, held to the session's lists, and runs the installed beekeeper under
  the host's configuration with them (`$BEEKEEPER_STATE_FROM`, which the started agent keeps too). The
  roster entry and events land in the scratch state, the live ones untouched; nothing else of the
  scratch file reaches the host, whose commands would run outside the sandbox.
- **No way out:** unsandboxed retries are off, and a session whose sandbox cannot start does not start.
- **The file tools.** Read, Grep, Glob, Edit, Write and NotebookEdit run in the harness, outside the
  sandbox. The policy runs `beekeeper hook pretooluse` for them and sets `BEEKEEPER_SANDBOX`, and the
  hook holds them to the same lists in every session, in the hooks' scope or not: a path is resolved
  (symlinks included, a missing one through its nearest parent) and refused outside the lists. The file
  tools may also write the sessions' memory and plans, which no command may.
- **Capped runs.** On Linux the sandbox blocks every Unix socket (seccomp cannot filter them by path),
  the user bus included, so `beekeeper run` cannot start its scope there. It asks the broker instead,
  `beekeeper sandbox broker` on the host (the user unit `beekeeper-sandbox.service`, which `beekeeper
  install` puts in place), through request files in `<stateDir>/sandbox`. The broker caps the slot's
  slice and moves the asking process into its memcap scope; the command runs there, still in the
  sandbox. It answers each request as the one process of the user that holds it open, never a process
  the request names, and only for memcap's own slices and scopes. With no broker answering, a
  sandboxed `beekeeper run` refuses (exit 1) instead of running a build uncapped.
- **Secret operations.** The sandbox holds no sops key and no op session, so a sandboxed
  `beekeeper secret compare|fingerprint|copy|set|rotate|recipients|import` goes through the same broker: it runs on the
  host as the session that holds the request (its name and id, nothing else of its environment), in
  its working directory, and the SOPS files it reads and writes are held to the sandbox's lists. Its
  output and exit code come back through the spool, and that output is never a value. A consumer
  (`copy <ref> -- <command>`) would run on the host, outside the sandbox, so the sandbox refuses it,
  as it refuses `--as`, `--config`, the vault's `setup`, which the person runs, and `import` without `secret.session`. The
  broker tells a sandboxed requester by its mount namespace, which no process in the sandbox leaves:
  an unsandboxed session's vault call ([The vault session](#the-vault-session)) runs outside the
  sandbox's lists, its consumer included.
- **Lab kubeconfigs.** The machine kubeconfig stays denied, and kind needs the container runtime's
  socket, so a lab's kubeconfig exists only while its lease is held: the claim (or `beekeeper lease
  kubeconfig <lab>` once the lab runs) has the broker write it into the lease for the session that holds
  it, and release takes it away. The broker's calls run with its unit's PATH, the service manager's,
  so `kind` and the credential tools of a brokered `secret` call are found there or in the Go tool
  directories: beside the beekeeper binary, `$GOBIN`, `$GOPATH/bin`, `~/go/bin` and `~/.go/bin`.
  The lab's API server goes into `sandbox.domains` as `127.0.0.1:<port>`,
  so its port is fixed (agentlab's `apiServerPort`). Every HTTP client exempts loopback from the proxy
  environment, so the kubeconfig points the lab's cluster at the sandbox's SOCKS proxy (`proxy-url`),
  which ends at the egress proxy as an opaque tunnel held to the same allow list. In a session that
  holds a lab lease the hook puts `beekeeper lease kubeconfig --refresh <lab>` in front of every
  command, which points the lease's kubeconfig at this process's proxy. The policy is one for the
  machine, so a held lab's kubeconfig is readable by every sandboxed session, not by its holder alone.

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
`outbound.sweepEvery` (15m) for world-readable private keys (`id_*`, `*.pem`, `*.key`, `*.ppk` with a private key header)
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
its pattern matches). It decodes every base64 run of 32 characters or more (a Kubernetes Secret's
`data`, an attachment, `base64` output wrapped over lines, and a run within the decoded text once
more) and looks into the decoded text the same way: a run that carries an indexed value or a token
pattern (an age identity, a private key, a token) is replaced whole. With a hit, the hook replaces the result with the same result, each hit
replaced by `[redacted: <reference or rule>]`, and the session goes on: the value never reached the
model, so ending the session would protect nothing more. Claude Code writes the replaced result to
the transcript on disk as well (checked with Claude Code 2.1.286), so the value is in neither. Each
redaction is a `scan.redact` event naming the tool and the references and rules with their counts;
an indexed reference also gets a rotation note for `guide.person`, one open note per reference. It is
still a filter, matching known values in known encodings: a value split across tokens or encoded
otherwise passes.

`beekeeper scan sweep` reads every transcript under `claude.projectsDir` (the sessions' and
subagents' `.jsonl` files, each line's strings decoded, and the spilled tool results in
``tool-results/`) and prints each reference and token rule it finds, base64-wrapped ones included,
with how often and in which files, never a value or where in a file it was: the list of what leaked
before the hook ran. A maintenance pass, it runs on one core at nice 10, and the PreToolUse hook runs
a session's sweep through `beekeeper run`, in a build slot like a build.

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
runs ungated. A promotion is queued for one release candidate: the gate asks `devctl release
promote <owner/repo> --dry-run` for the newest one when the call arrives, records it on the place
(`merge.queued … for candidate vX.Y.Z-rc.N`), and asks again when the turn comes, right before
devctl runs. When the newest candidate is another by then (a merge behind it cut a new one), the
promotion is refused (exit 77, `… was queued for candidate A, the newest is candidate B now:
nothing promoted`), its place leaves the lane and the refusal wakes its owner, who decides again:
a promotion queued by one worker never promotes another worker's candidate. `beekeeper lanes drop
<owner/repo> promote` takes a waiting promotion out of its lane; its gate, a queued run of its own
included, refuses and wakes its owner, as for a merge's place taken out with `lanes drop
<owner/repo> <n>`.
The PreToolUse hook rewrites the call to `<this binary> gate -- devctl pr merge …` (a background
call gets `--wait 30m`), and the gate decides. The hook finds the merge wherever it runs as a
command: in any part of a pipeline or a `;`, `&&` or `||` list, in a subshell or a loop, behind the
prefix commands that run their arguments (`flock <lock>`, `nohup`, `setsid`, `stdbuf`, `ionice`,
`chrt`, `nice`, `timeout`, `env`, `time`, `command`, `exec`, `VAR=value`, each with its options), and
with devctl named by path (`~/bin/devctl`, `./devctl`, `$HOME/bin/devctl`). It wraps only the devctl
invocation: `flock m.lock devctl pr merge o/r 7 | tee m.json | jq .verdict` becomes `flock m.lock
<this binary> gate -- devctl pr merge o/r 7 | tee m.json | jq .verdict`, so the pipeline and
`pipefail` behave as written and the gate's exit code and devctl's document reach the rest of it.
A here-document a shell reads as its program (`bash <<'EOF'`, `cat <<'EOF' | sh`) is a command
line too: a merge in it is gated in place; a here-document that is content (a file written, an
issue body sent) stays as written. A merge the rewrite cannot reach is refused, the refusal naming
the place and the line and, where it has a place, the command with the gate written in: one inside
a `sh`, `bash` or `zsh -c` string or an `eval` string; one in a script file the call runs, by a
shell (`bash merge.sh`), by its path (`./merge.sh`), sourced, or by an interpreter (`python3
merge.py`), read relative to the call's working directory and followed into the scripts it runs in
turn, a script the call writes before it runs it judged by the call's own text; one in an
interpreter's code (`python3 -c`, `node -e`, `perl -e`, …); and one in the program a shell or an
interpreter reads from its input (`echo '…' | bash`, `bash <<< '…'`, `sh < merge.sh`, `python3 -
<<'EOF'`). A merge the gate precedes in the script or the code (`<this binary> gate -- devctl pr
merge …`) passes, and runs through the gate like a merge typed on the command line.

- **Refused, exit 77**, one line starting `beekeeper gate: refused,` that says why and what to do:
  the repository, its lane, `merges` or `github` is held, or a cluster upgrade runs on the lane's
  installation (the hold's reason); the GitHub budget is unknown, checked before devctl makes a
  single request; the lane's installation cannot be read (a lapsed `tsh` login). Nothing was
  queued: a 77 never says `queued`. Nothing else stops a merge: a lane whose
  release has not rolled past `merge.settleTimeout` keeps it waiting, and `watch` says `LANE STUCK`.
- **Queued, exit 76**, one line starting `beekeeper gate: queued,` with the merge's position and
  whom it waits behind (when the machine-wide devctl cap is the reason, `<n> devctl processes run
  machine-wide (cap <n>), not a lane problem`), once the merge has waited `--wait` (2 minutes, 30
  in the background), or at once with the budget and its reset when the GitHub budget is under
  `github.floor`. The merge is not dropped: the gate hands it to a run of its own outside the
  caller, the same gate under `--queued`, which keeps its place for up to `merge.seedTTL`, runs
  devctl when its turn comes and wakes the owner with the outcome (`devctl.unheard`, as below).
  Nobody runs it again; a second `devctl pr merge` of the pull request while it waits is refused
  with exit 3.
- **Left at the tool limit, exit 76**: a desktop Bash call stops its command at 10 minutes without
  an exit code, so a foreground call leaves `merge.toolLimit` (9m) after it started, whether the
  merge still waits for its turn (queued as above) or its devctl runs. A running devctl goes on
  outside the call (`beekeeper gate: o/r#7's devctl runs past this call's limit …`, logged as
  `merge.left`), its outcome wakes the owner (`devctl.unheard`, as below) and `watch` records the
  run once devctl ended. A merge expected to outlive the limit runs with `run_in_background`: the
  hook gives it `--limit 0` and a 30-minute wait.
- **Otherwise devctl runs once**, its JSON document and exit code (devctl's own 0–9) unchanged,
  and the event log records `merging <owner/repo>#<n>` (`promoting <owner/repo>` for a promotion)
  and `merged` with the release; a run that merged or promoted nothing says so in one gate line
  after devctl returns (`nothing promoted (not_built)`, devctl's state of the promotion). Its wait for the CI outcome
  is `merge.ciTimeout` (1h, passed as `--timeout`) unless the command names its own `--timeout`:
  devctl's default of 30m is shorter than a CI that `--update-branch` restarts from zero, and a
  timeout (exit 2) merges nothing. devctl serves the
  repositories of `merge.devctlOwners` only (its GitHub App login reaches the giantswarm
  organisation); any other owner's repository takes the **plain squash merge** instead, in the
  same place and unit: as the gh login, it waits up to `--timeout` (45m) for the head's checks,
  refuses what devctl refuses before its wait (a draft, a closed, merged or conflicting pull
  request: 3; another person's: 5), and squash-merges green with the judged head as the expected
  one and `<title> (#<n>)` as the subject, then deletes the branch; red is 1, a timeout 2, a gh
  failure 7, and it waits for no release. Each run's stderr and document are kept in
  `<state>/merge-output/<repo>-<n>-<time>.log` for 7 days, named in its event. A merge call (or
  branch update) GitHub answers with a 5xx, devctl's exit 7 naming `…/pulls/<n>/merge: 5xx`, is
  sent again while GitHub reports the pull request open, up to three times 10 s, 30 s and 1 m
  apart (`merge.retry` each), with the expected head as before; the caller reads one document,
  the last run's. A run that ends with nothing merged is `merge.failed` and leaves the lane with
  its run: a place dies with its process, and the session's retry joins the lane anew.

A merge runs when no merge before it in its lane's queue holds its place, nothing else of the lane runs, fewer than
`merge.cap` devctl processes run on the machine, the calling session runs fewer than `merge.sessionCap` merges
(default 2; each running devctl polls GitHub, so ten merges one session queued poll as two would, the others wait in
their lanes without reading GitHub and start in the order they queued; `beekeeper budget` lists each session's
polling and waiting merges), and the lane's installation is ready: every
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
devctl already runs re-executes the same way and follows the same devctl on from where its
stderr was copied (`following <merge>'s devctl (pid <n>) on`), so the installed release records
the outcome; the `merge-child` that hands the outcome to the owner re-executes before it does,
carrying the outcome. A lane that stays stuck anyway (a seed whose session is
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
RECORDED`, a `merged` or `merge.failed` event naming the gone gate). SIGINT, a person's Ctrl-C,
reaches devctl at once; a SIGTERM reaches it when it is aimed at the gate: its caller (the gate's
parent) is still there two seconds later and no SIGHUP came, so `kill <gate pid>` stops the merge. A
`TaskStop` of the background task a gate runs (its stdout is the task's output file, recorded on
the merge) is caught by the PreToolUse hook before the stop's SIGTERM, which the gate cannot tell
from its caller's session ending: it ends a running merge's devctl through its merge-child and
drops a waiting merge's place (a seeded place stays), `merge.stopped` each, and the gate or the
next watch tick records the run, nothing merged, which leaves the lane. The watch records a run
whose gate is gone once its devctl wrote its exit code, also while merge-child still hands the
outcome over. A run that ends without its document or
by a signal (exit 128+n) is judged by GitHub (`gh pr view`), never by its exit code: merged, it
settles its lane with its release unconfirmed and the gate line names `devctl release wait
<owner/repo> --pr <n>`; not merged, it leaves the lane; with
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

A merge that ends with nothing merged (red CI, a timeout, a refusal in devctl's document) always
reaches its worker, the same way: the gate leaves merge-child no marker, so the owner gets the wake
above also when the call listened (a foreground call reads the gate line as well), and a queued
merge's own run leaves it to the run that waits for it, so the owner is woken once. The line is
`devctl pr merge <owner/repo> <n> exit N: <verdict>: <reason>; failed: <checks> (output in
<file>)`, `failed:` naming the checks and CircleCI workflows the document reports failed that the
reason does not already name; the gate line and the `merge.failed` event carry the same reason.
`lanes` shows the failed attempt on its lane, `failed <owner/repo>#<n> by "<owner>" at <time>,
nothing merged, exit N: <reason>; shown until its next attempt`, until the pull request's next
`devctl pr merge` joins the lane or a merge of it is recorded; an attempt nobody retries leaves
after seven days.

A merge into a base branch without auto-release releases nothing by itself: a maintenance branch
whose tags are cut by hand, a fork line's backport branch. Before devctl's turn the gate reads the
pull request's base branch and the Auto-release workflow on it
(`.github/workflows/zz_generated.auto_release.yaml`), once per merge; when the workflow is absent
or its `on.push` branch filter does not name the branch, devctl runs with `--no-release-wait`, the
merge leaves its lane the moment devctl reports it merged (`merged … release none awaited
(<branch> has no auto-release)`), and the gate line and the wake text say no release is awaited.
A base branch with auto-release holds its lane until the release rolled, as above. GitHub not
answering for the base branch refuses the merge (exit 77).

A running merge whose gate process and devctl are both gone (killed, or lost with the machine in
a reboot) is lost: whether it merged is unknown. `watch` turns it into the lane's settling merge with an
unknown release, one `MERGE LOST` line and a `merge.lost` event, so the lane settles for
`merge.settle` and frees once its HelmReleases are Ready; `lanes clear <lane>` drops it at once.

A running merge whose devctl runs on after its pull request merged (a hung release wait) holds
its lane for nothing. `watch` asks GitHub about each run older than `merge.hungAfter` (45m)
and ends the devctl of one whose pull request merged longer than that
ago, or closed: SIGTERM to its merge-child, one `MERGE HUNG` line and a `merge.hung` event. The
run is then recorded like any devctl ended by a signal, by its gate or by the next poll: merged,
it settles its lane with its release unconfirmed. `lanes drop <owner/repo> <n>` does the same at
once for a running merge whose pull request merged or closed, and refuses one whose pull request
is open.

A merge of giantswarm/devctl opens a tool-release window by itself: a `merges` hold with
giantswarm/devctl excepted, since the release makes every in-flight devctl run refuse until
updated. It lifts once no devctl merge runs and the local `devctl version` reports the window's
release (another version than when the window opened, while the release is unknown), or once its
pull request did not merge: the gate lifts it
after a run with nothing merged or no release warranted, and `watch` (or the next gate call) asks
GitHub about a window whose merge ended unrecorded (its gate killed) and lifts it when the pull
request is open or closed. Once its merge merged, beekeeper installs the release itself: the gate
runs `devctl version update` right after the merge (also when the window was lifted by hand), and
`watch` retries every 2 minutes while devctl does not report the release, logging a failed update
as `hold.update`. Every merge or promotion that runs devctl starts with `devctl version update`,
which installs the latest release when the local devctl is behind it (a failure logged as
`merge.update`), and the next merge of giantswarm/devctl waits while the previous one's window
waits for its release: devctl refuses to run behind its latest release (exit 7). Nobody has to
update devctl or lift the window by hand.

The budget floor uses the last reading in the state when it is younger than `merge.budgetFresh`
(1m, less than one merge's draw at the floor's margin), else a fresh conditional request, which
costs nothing when GitHub answers 304; a failed read refuses.

A merge whose delay keeps something exposed, a privacy or security fix's, is the supervisor's to
mark: `beekeeper lanes urgent <owner/repo> <n> --reason "<why>"`. Its gate runs it under the floor
instead of queueing it for the reset, as long as the budget keeps `github.urgentBound` (200) for
it, and logs it as `merge.urgent` with who asked and what its run drew against that bound
(`merge.urgent.spent`); `beekeeper budget` says the same. A merge already queued for the reset
picks the mark up at its next check. One urgent merge runs under the floor per reset window: a
second mark in the window, or while one waits, is refused with the reason, and a mark that reaches
its gate after the window's run is dropped (`merge.urgent.refused`) and its merge waits for the
reset like any other.

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
`--standby`; `beekeeper install` adds `--notify` to it only with `watch.notify: true`, so a desk
without desktop notifications leaves the key unset: while a supervisor's session runs it leaves the notes, timers, session records and
relays to the supervisor's watch and never reads the alerts, so it takes nothing from the
supervisor's view; what both see (the budget, stale leases) is sent once.
The upgrade holds never wait on a supervisor: while no supervisor's watch has begun an upgrade
cycle within five `watch.interval` (it says so in `upgrades-watch.json`), the standby watch reads
the upgrades on the same shared schedule, sets and lifts their holds and says
`UPGRADES read by the standby watch` once; beside a running supervisor's watch it reads none.
A supervisor runs its own watch with `--notify` too. `beekeeper install` writes and starts the
unit with the binary's path; `journalctl --user -u beekeeper-notify -f` shows its lines. The unit
caps the watch at half a core (`CPUQuota=50%`): at rest a tick costs a stat per file it read
before, since the watches keep the parsed desktop records, the transcripts' places and context
sizes, and their place in the event log across ticks, and read again only what changed.

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
  headless again until the desktop runs the holder's CLI. A successor is imported into the
  desktop's sidebar under its run title without a click, whatever its turns do: its import goes
  ahead past the desktop window's focus as an `agents start --desktop` agent's does (it waits
  for the person's typing to pause, 1 minute at most), and a reopen that meets the resume of a
  session the desktop never imported imports it beside that turn, the turn frozen while the
  desktop reads the transcript and the desktop's CLI it warms stopped. A desktop at its cap of
  CLIs still writes the row; it warms no CLI for it. One whose resume never ran did not
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
  (`agents keep`), one parked on a decision (`agents park`), one a timer wakes and one whose session
  waits on its person are parked; the
  supervisor, the guide and a role's successor are not counted. `beekeeper capacity` (and
  `--json`) prints the count, the headroom that bounds a new start (MemAvailable against
  `capacity.availMinMiB`, disk swap's growth over the watch's readings of `watch.swapWindow` against
  `capacity.swapGrowthMaxMiB`, never the swap in use, the 1-minute load against the watch's HIGH
  LOAD limit (`watch.loadMax`, or `watch.loadPerCoreMax` × cores), the kind labs against their cap,
  and for a start that builds the free build slots) and one verdict, `room for N starts` or what
  blocks one, each guard that refuses named; it reads, changes nothing and makes no GitHub call.
  Disk swap's growth is weighed with memory pressure and MemAvailable, the three figures in its
  line: over the max it blocks a start under pressure (memory PSI, full avg60, over
  `capacity.swapPSIMax`, or MemAvailable under the floor; either unknown counts as pressure) and is
  a warning in parentheses after the verdict without (`room for 7 starts (disk swap growing +391
  MiB/h over 256, without pressure: memory PSI 0% (max 0%), MemAvailable 43.0 GiB (floor 20
  GiB))`): swap may sit full for days without a byte moving, zswap's writeback moves pages to disk
  without new swapping, and the window lets a single burst leave the rate.
  The build slot guards only a start that builds: `capacity --builds` and `agents start --builds`
  mark a task that builds, tests or lints (its commands take the slot under `beekeeper run`), and
  such a start is refused while no slot is free. A start without the mark does not build (a filer,
  a dispatcher, a reviewer, a promoter proving a release read-only) and has no use for the slot: it
  is judged on memory, swap, load and the labs alone, and a busy slot is a note in parentheses
  after its verdict, never a `no start`, so the machine's one slot held by a worker's compile
  refuses no start that only reads. `watch` weighs a start that does not build. `agents start`
  refuses a start whose verdict has no room (exit 3, the guards named, no session created) unless
  `--force`, which starts it and records the verdict overridden (`agents.start` in the log).
  `watch` says `CAPACITY LOW <busy> of <floor>` once while busy stays under the
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
hand-over puts the rules ahead of the follow-up's prompt again. The roster shows it busy with its
`--task` from the moment it is started, and the desktop shows it working from its first turn.

**The start gate.** Before anything is recorded, the start is judged as `beekeeper capacity`
judges one: the busy agents against `capacity.ceiling`, MemAvailable, disk swap's growth, the
1-minute load and the kind labs, and for a task that builds, tests or lints, marked `--builds`,
the free build slots. A start without the mark is one that does not build and is not held by a
busy slot. A verdict without room refuses the start with exit 3 and the guards named, and no
session is created; `--force` starts it anyway, the verdict overridden recorded in the event log.

**The task runs as a desktop turn.** Claude Desktop marks a row working only while its own CLI of
the session runs a turn, so a `claude -p` turn outside the desktop leaves the row idle while it
works. A start therefore runs no task headless: a seed turn (`claude -p` under the id beekeeper
chose, with `--tools ""` and `--strict-mcp-config`, the worker prompt as its first prompt and the
note that it only answers "ready") creates the transcript with its model and title, in a transient
user unit `beekeeper-agent-<id>`. Once it ended, beekeeper imports the session into the desktop
under its name, past the window's focus (the person's typing holds the link 1 minute at most); a
title or model the import dropped is restored by a steward, the title by the session's own desktop
CLI when no other steward runs. The desktop's CLI of the session, which the import warms, then takes
the message that starts the task as its turn; when the desktop runs none, a steward's `send_message`
through the desktop's session messaging starts one with it, the route a relay revives a role holder
by. Before either spawn, `makeRoom` keeps the desktop under its cap of CLIs by ending one of
beekeeper's own idle CLIs: a finished worker's or a role run a relay relieved, then a parked
worker's, and, when the CLI is for a role's holder or relay successor, a worker's idle on its task
(a message by name starts it again); never a role holder's or a person's session. A revived role
holder with no row in the desktop yet is imported first, so a relay successor gets its CLI at the
cap and while the person types. Only where the desktop
cannot run the turn (it does not run, it did not import the session, at its cap with no CLI of
beekeeper's to end or the person still typing, so the session has no row, or no steward took the
send) is the session resumed headless as `agents wake` does, and `start` says why; that turn's
reopen imports a session the desktop did not. A start that delivers no turn of the task fails: it
exits non-zero, logs `agents.start` "task not delivered" with the reason, and `agents` (REACHABLE)
and the watch's AGENTS STOPPED line say "task not delivered" until `agents wake` delivers a turn,
so a worker that never got its task is never silent. A role's relay successor keeps
its headless first turn, which only takes the role; its relay hands it the desktop turn.

#### Worker profiles

A session beekeeper starts otherwise carries the person's whole interactive tool surface: the
desktop's tools (artifacts, the browser pane, questions to the person, workflows), every desktop-app
and claude.ai connector, every plugin's skills, agents and SessionStart hooks. `agents.profile`
names the profile the workers `agents start` (and `agents handover`) creates get,
`supervisor.profile` and `guide.profile` those of the roles' relay successors; `agents.profiles`
defines the desk's profiles by name, and beekeeper ships `minimal`: the built-in tools Bash, Read,
Edit, Write, Skill, ToolSearch, SendMessage, ListAgents, TaskStop, WebFetch and WebSearch, the plugin
`beekeeper`, and the MCP server `claude-in-chrome`. A profile lists the built-in `tools` kept
(`--tools`), the `plugins` kept (by name or id; every other installed plugin, of any marketplace, is
disabled through `enabledPlugins`, its skills, agents, MCP servers and hooks with it), the
`mcpServers` kept (`allowedMcpServers`, claude.ai connectors included) and further `deny` rules
(`permissions.deny`). beekeeper writes them to `<state>/profiles/<name>.json` and passes the file
with `--settings`, whose flag settings Claude Code ranks over the person's user and project settings
for that process only; the person's settings files are never edited, and their hooks (beekeeper's
own among them) keep running.

Claude Desktop spawns the CLI of a desktop turn itself, with its own tool surface, so a profile
reaches only the turns beekeeper launches. A profiled worker's task therefore runs as a headless
turn under the profile (`claude -p --session-id <id> --chrome --tools … --settings …`, the worker
prompt as its first prompt) and no seed turn runs; the session is imported into the desktop beside
that turn, past the window's focus, and its reopen warms the desktop's CLI once the turn ends, so
later desktop turns are the desktop's. Every headless wake of one of beekeeper's starts runs under
its role's profile too. `start` and the log say `its task runs as a headless turn under profile
<name>`; without a profile they say `its task runs as a desktop turn`, as before.
`agents start --dry-run` prints the profile applied and the first turn's command line.

`beekeeper agents` shows an agent in a headless turn (a fallback, a successor's first turn, a wake
turn) `live, first turn running` or `live, wake turn running` and says under its table how many
agents are in one; `--json` marks each with `headlessTurn`. While such a turn runs beside an import,
beekeeper stops the desktop's CLI of the session (it waits up to 15s for it): two CLIs on one
session id are two peers under one name. The same holds later: a headless resume (`agents wake`,
a task turn the desktop does not run, a hand-over's note turn) whose session's desktop CLI runs
sends the message to that CLI instead, and a desktop send that starts the desktop's CLI (a wake, the
standby's revive) waits up to two minutes for the session's headless turn to end and is refused
while it still runs. The watch says `TWIN CLI` for any session that runs two CLIs anyway (the
person opened its row), naming each CLI's PID and directory. Once the headless turn has ended, its unit's
`ExecStopPost` starts `beekeeper agents reopen <id>` in a transient unit of its own
(`beekeeper-reopen-<id>-<reopen>`, 35 minutes of runtime at most, `RuntimeMaxSec`) and returns at
once, so the turn's unit stops within a minute (`TimeoutStopSec`, under the user manager's default)
and a shutdown never waits for a reopen's wait for the person. The reopen shows the session in the desktop for a
moment and switches back, so its desktop CLI is warm again. It reopens only a start the roster still
holds, never one a hand-over or `agents remove` took off. One reopen waits per session (a lock
under `<stateDir>/reopen/`): a later turn's reopen leaves the showing to the one that waits. A
waiting reopen reads the roster every 15 seconds and ends, showing nothing, once its agent reported
its work done, left the roster or is a relieved role run; a desktop CLI of the session that started
meanwhile (looked for every 30 seconds) ends the wait too, kept as the warmed one. A reopen the desktop did not take (the
session not shown, its title not restored) is an `agent.reopen` event and ends the unit
successfully: the turn ended as it should.

A headless turn's end is the end of its process, so a background Bash it launched never wakes it.
When a start's or wake's turn ended with its task open on a background wait (a `run_in_background`
Bash its transcript holds no completion notice for, or a process the unit still runs), the reopen
resumes the session headless instead, once per task, with a turn that names the wait and tells it
to wait in the foreground; the event log says `agent.resumed-wait`, and the resumed turn's reopen
shows it in the desktop. A worker done, kept, or parked on a person is left alone, and so is a
`devctl` wait or merge the gate runs, whose outcome wakes its owner by itself (`devctl.unheard`).

A session the desktop never imported has no row in its sidebar: nobody sees it there, reads its
transcript or types into it, and the roster and the event log are its only signs. The usual cause
is the desktop's cap of CLIs: at the cap the import, the show and the reopen start nothing
(`makeRoom`), the task runs headless, and the reopen after it misses. `beekeeper agents` marks such
an agent `no desktop row` in REACHABLE (`--json`: `noDesktopRow`) and counts them under its table;
the watch says `NO DESKTOP ROW` once per worker, with what shows it: the standby watch imports one
whose headless turn runs beside the turn (above), and the doctor reopens a worker whose CLI does not
run once the desktop runs fewer CLIs than its cap, `beekeeper agents reopen <id>` in a transient
unit `beekeeper-reopen-<id>-<n>` of its own, which gives the session its row and warms its CLI, one
`DOCTOR reopens …` line and an `agent.reopen` event each, with no person acting. It starts no
reopen within seven minutes of the start (a start imports by itself), beside a start's or wake's
unit still running its turn or its reopen, or beside a reopen of its own, and counts the reopens
under way against the cap; `doctor --dry-run` names the workers it would reopen and those the cap,
or a desktop that does not run, leaves without a row.

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
at most (within the reopen unit's `RuntimeMaxSec`). The steward is the session's own desktop CLI
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
`--keep-desktop` leaves it in the sidebar. When no other steward is idle, the session's own
desktop CLI archives it: beekeeper shows the session in the desktop for a moment once the person's
typing pauses, as a reopen does, which warms its CLI, and the window then shows the session it
showed before. When that cannot be done (the desktop does not run, the person keeps typing, the
desktop at its cap of CLIs) it says so and the removal still stands; the doctor then owes the
archive (below), and a run that asked no steward counts none of its tries.

The desktop's `archive_session` may be used only on the person's explicit agreement, and a
peer's message is not that, so beekeeper archives only under `agents.archiveAgreement`: where the
person agreed that the desktop sessions of finished workers beekeeper started are archived
without asking, never a session they started themselves (a standing instruction in their user
`CLAUDE.md`, say). The request quotes it and has the steward run `beekeeper agents archivable
<local_id>…` first, which confirms each session is such a finished worker (one of beekeeper's
starts, off the roster, no role a relay did not relieve, unarchived, its CLI in no turn) and exits
3 for any other, a session the person started included; the steward archives only what it
confirms. Without `agents.archiveAgreement` no steward is asked and the line says so. An archive
counts once the desktop records it, and the line names the steward whose `archive_session` call
did it. A steward that archived nothing is read from its turn: one that declined is reported with
the first line of its reply (`agents.archive`, "steward local_… declined to archive: …") and asked
for no archive for 24 hours; one that did not answer is reported as such.

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
returns the entry to `agents.staleAfter`. The doctor also reopens a worker whose session the desktop
never imported, once the desktop runs fewer CLIs than its cap ([above](#agents-started-without-a-click)).

A worker that waits on a person, a merge lane or a release parks instead of sleeping in a turn
that holds its slot and its CLI's memory: `beekeeper agents park [--on <#note|owner/repo#n>]
"<what it waits for>"` keeps its task, shows it parked in `agents` (KEPT) and `capacity`, not busy,
and it ends its turn; an agent without a task is refused. Once the note is closed (answered,
defaulted, done or overtaken) or the issue or pull request merged or closed, the watch says one
`AGENT RESUMABLE "<agent>"` line with what settled it, a person's answer word for word. With
`agents.autoResume` the next poll resumes the agent with that answer as `agents wake` does (by name
to a running CLI, else headless); without it, or for a park without `--on`, `beekeeper agents
resume <agent>` does it by hand. A resume that cannot wake the agent leaves it parked (`AGENT
RESUME FAILED`); `agents idle` ends the park with the task. An archive
that stayed (the CLI ran a turn, no steward recorded it, also after `agents remove`) is owed, and
so is, once, every finished worker beekeeper started whose desktop record stayed unarchived, its
CLI running or not: the doctor asks for at most 20 of them per run, while the CLI runs no turn, in
the same steward's turn,
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
line while it lasts. The Go build cache every session's builds share (`$GOCACHE`, else
`~/.cache/go-build`) is kept under `doctor.goCacheMaxGiB` (20 GiB): Go itself drops only entries
unused for five days, so a busy machine's cache grows without bound. Every watch, the standby
watch included, reads its size every `doctor.goCacheEvery` (1h) and, over the cap, removes the
least recently used entries down to three quarters of it; it never starts while one of the user's
`go` commands runs and stops when one begins, trying again five minutes later. One process trims
at a time, and each trim is one `gocache.trim` event with the size before and after; a trim that
waited `doctor.goCacheEvery` for the builds, or a cache it cannot read, is one `GO CACHE` line.
`beekeeper doctor` trims it the same way (`--dry-run` says the size). `--dry-run` says what it
would do. A note for the person that asks nothing
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
after the first turn imports the session instead, waiting up to 25 minutes more (the reopen unit's
`RuntimeMaxSec` covers the wait). While it waits, the agent records what holds it and until when:
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
hook answers them. The desktop skips them only for a session in auto or bypass mode; the import
turns bypass into acceptEdits, and raising a session's mode again takes the person's approval in
the desktop. (A desktop-wide "Allow all sites", `preferences.allowAllBrowserActions` in the
desktop's `claude_desktop_config.json`, does not reach an acceptEdits session on a Team or
Enterprise account either.) With nobody at the desktop such a navigate waits until the desktop
aborts it, sometimes hours later, and the model reads "Claude in Chrome is not connected".

So the sessions beekeeper starts never use the desktop's browser. In their desktop turns
(acceptEdits) `beekeeper hook pretooluse` refuses every `mcp__claude-in-chrome__*` call and names
`beekeeper browse "<steps>"`, which runs the steps in a headless `claude -p --chrome` turn that has
the CLI's own Claude in Chrome tools and nothing else (`--tools '' --strict-mcp-config
--permission-mode dontAsk --allowedTools 'mcp__claude-in-chrome__*'`: no shell, no file tools, no
other MCP server, every other call refused rather than asked). That connection takes the Chrome
extension's site permissions and never asks. It prints the turn's report,
its transcript and each screenshot it took as an image file under `<stateDir>/browse/<id>/`. Every
headless turn of a start (the task's when the desktop runs none, an `agents wake`) gets `--chrome`
too, on the same CLI connection and the same extension site permissions. That turn keeps its own
Claude in Chrome tools rather than going through `browse`, by design: it runs in bypassPermissions,
so its shell could run `beekeeper browse` or `claude -p --chrome` itself, and taking the Chrome tools
out of it would narrow nothing. A bypass turn's scope is the bypass; `browse`'s narrowing serves the
desktop turns, whose acceptEdits it keeps. So each kind of turn has one browser path: a desktop turn
browses through `browse`, a headless turn with its own Chrome tools. A person's own sessions are
untouched. `agents start` says which Chrome mode the desktop
recorded, and `beekeeper agents` shows it per agent in its BROWSER column: `asks` or `skips`.

**A consent for an App the person owns and asked for is beekeeper's own decision; every other grant
stays refused.** A consent page (an OAuth grant: GitHub's green Authorize button, an identity
provider's Allow) is a permission grant to the browse turn's classifier, which refuses the click, or
the whole task at its first call, and judges every run anew: it holds no record of whose App a page
names. beekeeper does. `beekeeper app allow <name> --client-id <id> --word "<the person's words>"`
records an App the organisation declares and the person asked a consent for: its name as GitHub's
consent page titles it, the OAuth client id its consent URL carries, the callback hosts of its
declaration, and the person's words (a note's answer). The declaration is the App's manifest
(GitHub's App manifest: `name`, `callback_urls`) in the repository the config names, read on its
default branch when the App is recorded:

```yaml
apps:
  repo: example/github                # the repository that declares the organisation's Apps
  manifest: apps/{name}/manifest.json # an App's manifest, {name} for the App's name
```

An App no manifest declares under its name is not recorded, and without `apps.repo` none is; the
record names the manifest and the commit it was read at. With it, `beekeeper hook pretooluse`,
which runs in the browse turn too, answers a Chrome call that acts on GitHub's consent page itself:
on the recorded App's page with one of its declared callback hosts (the client id, or the name in GitHub's
title, since the Chrome tools report the URL's client id as `REDACTED`) the click is allowed, the
record in its reason; on GitHub's consent page of any other App or callback host it is refused,
naming the page and the record's form. The page is the tab's last known URL and title in the turn's
transcript, which every Chrome tool result ends with; a call whose page the transcript does not show
gets no decision, as before. A sign-in or consent page of the lab's own identity provider on a
loopback host (`*.127.0.0.1.nip.io`, `localhost`) is allowed without a record: a fixture user's
sign-in there is the person's own lab's. The browse turn's own start record (`beekeeper browse`,
mode `dontAsk`, gone when the turn ends) has the hook act in it wherever its `--dir` lies.
Steps that name an App on record, by name or one of its callback hosts, give the turn that App's auto mode allow
rule after the shipped rules (`--settings`, this run only), so the classifier refuses neither the
task at its first call nor the click, and the prompt's words to click that page's Authorize once and
nothing else that grants access; steps that name a loopback host give it the lab's rule and the
prompt's exception for the fixture user's credential the steps give; the report says `allowed
consent: <name> (callback <host>, declared in <manifest>)` and `allowed lab sign-in:` first. `beekeeper app check <url>
[--title <title>]` prints the hook's decision for a page, dry. A call the classifier refused is named
below the report, one `refused:` line per call with the call's own words and the classifier's
reason; a `[Permission Grant]` line quotes the step that reads as a grant and names the record. A
page in a Chrome window the compositor does not draw (occluded, or on another workspace) is not
rendered: GitHub keeps its Authorize button disabled there and every screenshot times out, which
`browse` reports as `not rendered:` below the report; bring the window to the front and run the steps
again.

**A deploy click the task covers is declared; every other deploy stays refused.** A portal's deploy
or create click (a review step's Deploy button, which applies a release to a cluster) is a production
deploy to the browse turn's classifier, which refuses it, so a portal proof stops one click short.
`beekeeper browse --allow-deploy "<what, where>" "<steps>"` declares the one click the task covers,
the object and its target as the steps name them: the turn runs with an auto mode allow rule for
exactly that action after the shipped rules (`--settings '{"autoMode":{"allow":["$defaults",…]}}'`,
this run only; the desktop turns' rules are untouched), its prompt says to click that control once
and nothing else that deploys, and the first line below the turn's report says `allowed deploy:
<what, where>`. Without the flag the refusal stays: a `refused:` line names the click with the
classifier's reason (`[Production Deploy]`) and a line names the flag. A deploy the person did not
ask for is never declared.

**A proof reaches a public thread through `beekeeper proof upload`.** A screenshot `browse` saved (or
one taken from a transcript) goes into a public issue or pull request thread with `beekeeper proof
upload <image> --repo <owner/repo>`, the repository of that thread: it re-encodes the image as PNG,
which drops its metadata, commits it to that repository's images-only branch `beekeeper-proofs` with
the session's own `gh`, and prints `![<alt>](https://raw.githubusercontent.com/<owner>/<repo>/<commit>/<path>)`
to paste into the comment or the PR body. The gist the App token cannot create and the comment box's
file upload are not needed. Only public repositories are served; what the pixels show is the
worker's to check, since a public thread carries nothing internal in an image either.

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
"PreToolUse": [{"matcher": "Bash|Read|Grep|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|TaskStop|mcp__.*", "hooks": [{"type": "command",
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

An agent in its task's last step is held, not handed over: its `sessions serve --waits` names the
report or says the merge landed (`report`, `merged`, `release`, `rolled out`, `proof`), or the gate
saw a merge of its own land (settling, its release and rollout pending). `watch` says `HANDOVER HELD
"<agent>" at <n>k: last step, report expected (<the evidence>); due at <HH:MM> or at <ceiling>k`
once, and holds the hand-over for `agents.lastStepGrace` (30m) from that evidence or until the
context reaches `agents.lastStepCeiling` (default a quarter above `agents.relayAt`), whichever
comes first; past either, `HANDOVER DUE` follows and says how long it was held. An agent that
reported done (`agents idle --done`) is never handed over: the doctor archives it. A hand-over of a
worker at its report would start a fresh session that finds nothing to do while the original's
`WORKER REPORT DONE` arrives seconds later.

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

### Feedback on the reports from Slack

With `feedback.context` set, the standby watch reads the person's replies to the reports through
the Slack MCP server behind muster (`feedback.server`, default `slack`), as the person, with
central's muster binary and timeout:

1. **The thread.** A report's post (its `slack_send_message` result) names its channel and ts; the
   watch keeps it in the state's `reportThreads` for `feedback.window` (24h).
2. **The replies.** Every `feedback.every` (5m) it reads each thread (`x_<server>_read_thread`)
   beside the poll. A reply by the report's author that came after the last one delivered goes, as
   a message (`agents wake`), to the agent it names first, `<agent name>: <message>`, or else to the
   supervisor. A delivered reply gets an :eyes: reaction, `feedback.delivered` in the log and a
   `FEEDBACK to <agent>` line; one not delivered is said and read again the next time.
3. **Unreadable.** A thread that cannot be read is said once, `FEEDBACK UNREADABLE <context>:
   <reason>`, and `ENDED` once it reads again. The Slack connector reads what its scopes allow:
   a report posted as a direct message needs `im:history`, one in a public channel
   `channels:history`.

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

A provider's key never lies in a file. omp resolves a provider's `apiKey` in its models file
(`omp.modelsFile`, `~/.omp/agent/models.yml`) as the name of an environment variable first and as the
key itself otherwise, so the file names the variable (`apiKey: SPARK_API_KEY`) and beekeeper's
configuration names where the value lives, an `op://` reference of the shared vault
(`omp.providers.<provider>.apiKey: op://<vault>/<item>/<field>`; a value there is refused when the
configuration loads). `agents start --harness omp` on a model of such a provider reads the reference
through `beekeeper secret`'s own handling, in the process that holds the vault session (the host's
broker with `secret.session`, so the start runs there as a sandboxed start does; with a service
account, the caller's own), and writes `NAME=value` into the agent's inbox before omp runs: the unit's
shell reads the line, exports the variable and execs omp. No file, no command line and no unit
property carries the value; it lives in omp's environment alone. omp hands its whole environment to
the shells that run the agent's tool commands, so the unit names beekeeper's own bash as `SHELL`
(`<stateDir>/omp/bin/bash`), which drops the variables `BEEKEEPER_OMP_CREDENTIALS` lists before it
runs the command. A start is refused, before anything is recorded, when the file carries a value for a
provider whose reference the configuration names (the file would hold the key), when it names a
variable for a provider the configuration has no reference for (omp would send the name as the key),
when the configuration names a reference and the file no variable, and when the reference does not
answer or the vault stays locked: nothing starts on a key it does not have. The start's record and
output say which variable the agent got and from which reference (`$SPARK_API_KEY from
op://…`), never the value. A provider the configuration does not name keeps omp's own key handling.

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

Every session's hook loads this file on every tool call, so an edit is one write:
`beekeeper config set <key> <value> [<key> <value>]…` sets dotted keys (`secret.store.read`) to YAML
values in a single rename, validated before it replaces the file. A result that would not load is
refused with exit 3 and the file unchanged; comments, the other keys, the file's mode and a link to
it stay. Keys that reference each other (a `store://` age identity and `secret.store`) are set in
the same call; an editor saving them one after the other is the edit that can leave a window
between them. `config set` runs on a file that no longer loads, to repair it.

The organisation and desk keys, and their defaults:

| Key | Default | What it sets |
|---|---|---|
| `kube.production` | unset: the kube guard is off | The installation whose clusters agents never write to, and the no-current-context rule of the machine kubeconfig |
| `kube.contextTemplate` | unset: the context named after the installation, else `*@<name>` | An installation's kube context, `{installation}` its name (`login.example.com-{installation}`) |
| `kube.kubeconfig` | `KUBECONFIG`'s files, else `~/.kube/config` | The kubeconfig files a `k8s://` source of `beekeeper secret` resolves its context in, merged as kubectl merges a `KUBECONFIG` list (`~/` allowed); a kind lab's context reads through kind instead |
| `alerts.installations`, `lanes`, `resources` | none | The installations whose alerts are read, the merge lanes, the leasable environments |
| `alerts.installations[].floor` | every severity | The lowest severity printed of an installation's alerts |
| `alerts.ignore` | `[Watchdog]` | Alert names that never appear |
| `alerts.quiet` | `[{severity: notify}]` once `alerts.team` is set, else none | Alerts whose changes are logged, not said |
| `alerts.tenant` | unset: only kube-prometheus-stack's Alertmanager | The Mimir tenant (`X-Scope-OrgID`) whose Alertmanager is read first |
| `alerts.pagerduty.context` | unset: PagerDuty is not read | The muster context whose PagerDuty MCP server the watch reads the open incidents through, as the person (`muster auth login --context <it>`); beekeeper holds no PagerDuty token |
| `alerts.pagerduty.server`, `.services`, `.every` | `pd`, none (required with a context), `1m` | The server's muster name (`x_<server>_<tool>`), the ids of the team's PagerDuty services, the reading interval |
| `github.probeRepo` | `giantswarm/beekeeper` | The repository whose conditional GET reads the budget |
| `github.floor` | `2500` | The budget under which GitHub work stops |
| `github.urgentBound` | `200` | The budget an urgent merge (`lanes urgent`) keeps for itself under the floor |
| `merge.devctlOwners` | none: every merge is the plain squash merge | The owners whose repositories `devctl pr merge` serves |
| `supervisor.skill`, `guide.skill`, `guide.person` | unset | The roles' skills and the person the guide walks through their notes |
| `claude.desktopApp` | `claude-desktop` | The desktop app's executable, which opens `claude://` links and starts the app |
| `shell` | `$SHELL`, else `sh` | The shell the hook runs a rewritten build, test or lint command in |
| `maxKindClusters` | one per 40 GiB of RAM, at least one | The kind clusters the hook lets the machine run |
| `memcap.max` | 14% of RAM | A `beekeeper run` command's MemoryMax |
| `memcap.cpuQuota` | half the cores (`1200%` on 24; 100% is one core) | `memcap.slice`'s CPUQuota: the cores every `beekeeper run` command together may use, shared by the slots by equal weight; `install` writes it into the slice unit and every `run` sets it again at its slot |
| `memcap.cpuWeight` | `50` | `memcap.slice`'s CPUWeight against the desktop's slices (100 each): the runs' share of the cores while the desktop wants them too, a third by default |
| `watch.availMinMiB`, `watch.scopeAnonMaxMiB`, `watch.gttMaxMiB` | 12%, 32%, 28% of RAM | LOW RAM, DESKTOP SCOPE, IGPU GTT |
| `watch.swapMaxMiB`, `watch.oomdHeadroomMinMiB` | 60%, 6% of swap | SWAP (disk swap, zswap's share not counted, while it grows and MemAvailable falls), and the headroom under which OOMD IMMINENT is said (only while systemd-oomd watches a cgroup for swap) |
| `watch.swapWindow` | 10m | how far back swap's growth rate looks (SWAP, OOMD IMMINENT, `capacity`'s swap guard), at least 5m: a writeback burst older than it has left the rate |
| `watch.toolProcsMax`, `watch.tools` | `1000`; kubectl, helm, tsh, gh, flux, devctl | LOAD over this many processes of these CLIs machine-wide; negative: off |
| `desktop.typingQuiet` | `30s` | How long the person's input stays idle before a `claude://` link switches the desktop's window; negative: links do not wait for it |
| `watch.tmpMaxMiB`, `watch.diskMinMiB` | 45% of `/tmp`, 5% of `/` | TMPFS, LOW DISK |
| `watch.diskCriticalMiB` | 1% of `/` | DISK NEARLY FULL |
| `watch.diskFillWithin` | 2h | DISK FILLING: `/` would run full within it at the rate its free space fell over the last ten minutes; the line names the commands and sessions whose processes wrote most (`write_bytes` of `/proc/<pid>/io`) and the part of the loss no process the watch can read accounts for (another user's, such as dockerd, or ended ones) |
| `doctor.goCacheMaxGiB`, `doctor.goCacheEvery` | `20`; `1h` | The cap of the shared Go build cache and how often a watch reads its size; over the cap it is trimmed, least recently used first, to three quarters of it while no go build runs ([Agents started without a click](#agents-started-without-a-click)); negative: off |
| `ollama.url`, `lemonade.url` | unset: no model server | The host's model servers, watched and guarded under the `model-server` lease |
| `outbound.phrases`, `outbound.paths`, `outbound.storeDeny` | none | What never leaves the machine, the plan files whose writes are outbound, the refused secret-store writes ([What leaves the machine](#what-leaves-the-machine)) |
| `secret.vault`, `secret.tokenFile` | none | The shared 1Password vault `beekeeper secret` reads and writes, and the file with its service account's token ([Secret operations](#secret-operations)) |
| `secret.session` | `false` | Read and write `secret.vault` through the person's `op` session, held by the broker alone, instead of a service account ([The vault session](#the-vault-session)) |
| `secret.signinCommand` | none | The command the broker runs to sign in to the vault without the person; it prints the session as `op signin` does ([The vault session](#the-vault-session)) |
| `secret.sessionLifetime` | `12h` | How long the broker holds the vault session after a sign-in ([The vault session](#the-vault-session)) |
| `secret.unlockWait` | `8m` | How long a call on the vault waits for the broker's sign-in, and how long one sign-in waits for the credential store and tries again after a failure ([The vault session](#the-vault-session)) |
| `secret.ageIdentities` | none | Age identities in the shared vault, in an identity file (`file://`) or in the person's own credential store (`store://`), by recipient or `pathRegex`, for the SOPS files no local sops identity decrypts; a recipient no entry names has its identity in the vault's item `sops age key <recipient>` ([Age identities](#age-identities)) |
| `secret.ageVaults` | none | The vaults besides `secret.vault` that age identities are read from: an `op://` entry of `secret.ageIdentities` may name one, and a file under `installations/<name>/` finds its identity in the item `<name>.agekey` of `secret.vault` or one of them ([The installation's item](#the-installations-item)) |
| `secret.store.read`, `secret.store.search` | none | The person's own commands that read an entry of their credential store and search it by an age recipient, for `store://` age identities ([Age identities](#age-identities)) |
| `secret.files` | none | Files known to hold secret values (`~/` and globs allowed) that no agent reads whole, beside the built-in list ([Secret reads](#secret-reads)) |
| `secret.unlockCommands` | none | The person's own vault unlock helpers, refused in agent sessions like `op signin` and unaliased in the agent shell ([Secret reads](#secret-reads)) |
| `secret.env` | none | Names (globs, such as `*_TOKEN`) of environment variables that carry a secret: the producer of `beekeeper secret capture` runs without them, as without the vault's credentials and `SOPS_AGE_KEY` |
| `sandbox.allowRead`, `sandbox.allowWrite`, `sandbox.domains` | none | The paths under the home directory the agent sandbox re-allows for reading and writing, and the hosts commands reach besides GitHub ([The agent sandbox](#the-agent-sandbox)) |
| `sandbox.proxyPort` | `3190` | The port of the broker's egress proxy on `127.0.0.1`, the sandbox's only way out ([The agent sandbox](#the-agent-sandbox)) |
| `sandbox.devctl` | `devctl` on the broker's `PATH` | The devctl the broker runs on the host: it renews the egress proxy's GitHub token and runs their gated devctl commands ([The agent sandbox](#the-agent-sandbox)) |
| `scan.sops`, `scan.vaults`, `scan.minLength` | none, none, 12 | The SOPS file globs and 1Password vaults `beekeeper scan index` fingerprints, and the shortest value it takes ([What reaches the model](#what-reaches-the-model)) |
| `plans.repositories`, `plans.check` | none, `plan-stages` | The plans repositories whose open pull requests a note for `guide.person` links only once their stage check is green |
| `outbound.sweepRoots`, `outbound.sweepDepth`, `outbound.sweepEvery` | the home directory, 5, 15m | Where and how often the watch looks for exposed keys and credentials in remote URLs |

State lives in `$XDG_STATE_HOME/beekeeper/` (`state.json`, which an older beekeeper still running
writes back with the fields it does not know at every level, `events.jsonl`, whose `at` is RFC 3339 in UTC while
`beekeeper log` prints local times, each caller's last
snapshot, the alert baseline `alerts.json` with its owner's `alerts.lock`, the notification ledger
`notify.json` with `notify.lock`) and leases in `leases/`, one directory per held resource.

## Development

See [docs/development.md](docs/development.md).
