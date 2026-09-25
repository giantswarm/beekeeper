# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Changed

- `supervisor.relayAt` (tokens, default `400k`) replaces `supervisor.shift`: `watch` says `RELAY DUE` once the supervisor session's context, read from its transcript's last request like the `CTX` column, reaches it, at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued, no relay open). It is one line naming the context and `beekeeper handover --prompt`, said once per supervisor and again only after a relay is cancelled or expires. `supervisor status` and `handover` show the supervisor's context in tokens. The state's `relayDue` record replaces `shift`.

### Removed

- `supervisor.shift`: a supervisor is relayed at a context size, not after a time.

### Fixed

- The PreToolUse hook gates every `devctl pr merge` that runs as a command: behind `flock <lock>`, `nohup`, `setsid`, `stdbuf`, `ionice`, `chrt`, `nice`, `timeout` (with options), `env` (with `-u`), `time`, `command`, `exec` and `VAR=value`, in any segment of a pipeline or list, and with devctl named by path (`~/bin/devctl`, `./devctl`). The gate wraps only the devctl invocation, so pipelines and `pipefail` behave as written. 43 of the 577 real merge invocations in the lab machine's transcripts ran ungated before (`flock <lock> devctl pr merge … | tee … | ledger.sh record …` among them); all are pinned in `internal/guard/testdata/merges.jsonl`.
- A `devctl pr merge` inside a `sh`, `bash` or `zsh -c` string that the rewrite cannot reach is refused, naming the command with the gate written in, instead of running ungated.

- A cap kill in a memcap scope no `run.start` names is reported with `cap unknown` (and the owner unknown when its process is gone) instead of "memcap cap on one command", which read as a build hitting the default 12G cap.
- The guard test's deliberate 64M kill no longer reads as a build's: a run with `MEMCAP_TEST=1` (the capped-run tests set it) takes a `memcap-test-…` scope, and `snapshot` and `watch` report a kill in one as a test kill, `watch` as one quiet `test kill:` line instead of `KERNEL OOM`, `snapshot --changes` as `N test kills` apart from the kernel OOM kills.

- `agents register` replacing the entry of a session that no longer runs no longer drops the task that entry was assigned and never finished: the new entry takes it over with its assignment time, the `agents.register` event says `takes over "<task>"` and the output tells the fresh session to work it. Re-registering keeps the session's own open task, and a registration that would drop two entries with open tasks is refused (exit 3).
- `agents register` in a `claude --bg` worker registers it under its `-n` title instead of its session id: a session's name falls back to the name its CLI's record holds (`~/.claude/sessions/<pid>.json`) before its id, for every command that names the caller. `--name` still overrides.
- A fresh session registering under the name of an agent whose session no longer runs replaces that entry instead of adding a second one, so `agents assign <name>` reaches the new session; the output names the replaced session and a task it left unfinished. A name a running session's entry holds is refused (exit 3), and `agents assign` and `agents remove` refuse a name several entries share instead of taking the first.

- A `claude --bg` worker the daemon served from its pre-started spare CLI is listed in `sessions` under its session id and name, so `lease grant`, `agents` and every lookup by session find it: the second worker started through a daemon, and a worker woken with `claude --bg --resume <id>` while a spare was ready. A spare no session has claimed is not listed.
- A woken `claude --bg` worker is listed under its session id instead of its transcript's path.
- A resumed CLI that went on under a new session id is listed under that id, and its transcript and last activity are the new one's.
- A session's id and name come from the record its CLI keeps of the session it runs (`~/.claude/sessions/<pid>.json`, configurable as `claude.sessionsDir`), ahead of its command line and environment; a record a dead CLI left behind is not taken for a later process under its PID.

- A lane no longer reads `settling … until vX rolls` after its release rolled: `watch` drops a settling merge once its release rolled and the lane's HelmReleases are Ready (logged as `lane.settled`), so `lanes`, `handover` and the relay's quiet moment see the lane free before its next merge arrives. The roll check itself already matched a chart version with build metadata (`4.74.0+971d12027db0`) against its tag (`v4.74.0`); a test now pins it against a real `kubectl get helmreleases -A -o json` answer.
- A merge into a repository whose lane has no installation leaves nothing settling: its lane frees when devctl returns.

- A relieved supervisor's `supervisor status` keeps exiting 4 after its successor relays onward, cancels a relay or is relieved in turn: the relief is recorded per party (`relieved` in the state) instead of read from the latest relay, and ends when that session supervises again or after 7 days. The message names who relieved it and who supervises now.
- A supervisor CLI restart no longer opens an ungated window for claims: the grant rule holds for `supervisor.restartGrace` (default 1m) after beekeeper first saw the supervisor's CLI gone (`supervisorCLI` in the state, recorded by a claim, the watch and the supervisor commands), and a CLI back under the same session id or desktop record within it keeps the role and an open relay. A refused claim says the supervisor is restarting and until when, `supervisor status` and `status` (`~<name>` in `--bar`) show the restarting state, another session's `supervisor start` stays refused, `SUPERVISOR GONE` waits for the grace, and the restart is logged (`supervisor.restart`) and said by `watch` (`SUPERVISOR RESTARTED`). A CLI that does not come back lifts the rule once the grace has passed, as before.

- A merge that ends with nothing merged (a branch behind, exit 3) keeps its place in its lane: `lanes` shows it `retrying`, and its session's retry runs before every merge that was behind it instead of joining at the end. It holds the lane for `merge.queueTTL` after the failure and keeps its place for `merge.seedTTL`; devctl's refusal (exit 5) leaves the lane.
- A free lane no longer waits for a seeded place whose merge has not arrived: an arrived merge that was not seeded runs ahead of it, and a seed never holds up an earlier pull request of its own session. Seeds keep their order among themselves, and `merging` names the places a merge passed.

- A session started from inside another session no longer reads as that session restarting: a CLI whose environment names a session is its child, listed under its own `--session-id` (or `--resume`) and `-n` name and `started by` it, and `watch` reports no restart of the parent. A `claude -p` its tool shell runs without an id of its own stays one of its commands, and its memory is no longer counted twice once the child is a session.
- `claude --bg` workers appear in `sessions` under their session id and name; the `claude daemon`, its terminal hosts, the spare CLI, the `--bg` launcher and the other `claude` subcommands (`stop`, `logs`, `mcp`, …) are no longer listed as sessions.
- `--session-id` on a CLI's command line names the session, ahead of the environment.

### Changed

- The owner hints on a NEW alert line read as timing, not cause: `[during merging … since …]`, `[during merged … at …]`, so a merge that merely coincides with an alert is not read as its cause.

### Added

- `lanes` marks a lane `stalled` when no merge runs and its first arrived merge has waited longer than `merge.stallAfter` (default 5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left it), naming the waiting merge and those places; `lanes --json` carries it as `stall`. `watch` says each stall as one `LANE STALLED` line, repeated at most every `watch.repeat` while it lasts.
- A `devctl pr merge` gate call waiting for its turn re-executes the new binary when `beekeeper self-update` (or any rename) replaces its own: the same process, arguments, stdio and deadline, the merge's place found again by session, repository and number, so a fix reaches calls that already wait. It says `continuing under beekeeper <version>`; a call whose devctl runs is never re-executed. Linux only.
- Per-session metrics, read from the transcripts, the process table, the memcap scopes, the event log and the leases, never from GitHub: busy and idle time, turns, tool calls and their errors (and the most repeated failing call), GitHub calls, tokens and cost, context size and fill, the capped runs' memory, `gh`/`devctl` processes, merges queued, merged, refused and failed, and lease times. `sessions` shows each session's context and last hour, `sessions --json` every figure over the transcript window and the last hour, `sessions --json` and `snapshot --json` the totals, `snapshot` the three sessions that spent the most in the last hour. Cost comes from `metrics.models` (the Claude API list prices by default); a model without a price is `cost unknown`. The transcript is read once for its work and its figures, all sessions in parallel.
- `watch` says `RUNAWAY` once per session and figure over `metrics.runaway`: GitHub calls in the last hour (1000), the same failing tool call repeating in it (10), the context's fill of its window (0.9).
- Desktop notifications: `watch --notify` sends the events that need a person to `org.freedesktop.Notifications` over D-Bus and still prints every line: a note or timer due, the machine near its OOM line, a kernel OOM kill outside a build slot or a systemd-oomd kill, the GitHub budget under the floor, a stale lease and a supervisor whose session ended with no successor (`notify.kinds`). Each event is one notification however many watches notify, claimed in `notify.json` under `notify.lock`; a lasting condition notifies again after `notify.repeat` (30m); `notify.quietHours` hold everything but a critical one (`notify.urgency`) and send what they held as one notification when they end. With no notification service the watch runs on and says so once.
- `watch --standby` and the user unit `contrib/systemd/beekeeper-notify.service` (shipped, not enabled): a watch for when no supervisor runs, which leaves a running supervisor's notes, timers, session records and relays to its watch and never reads the alerts.
- `watch` says `SUPERVISOR GONE` once when the recorded supervisor's session has ended with no relay open.
- `status [--bar]`: the supervisor, the leases held, the holds and what is due in one line; `--bar` is a fixed tab-separated row for a desktop bar, the shape of `free --summary`'s rows.

- Alert triage: `alerts.installations[].floor` is the lowest severity (none, info, warning, notify, critical, page) of an installation's alerts that `watch` and `snapshot` show; the alerts below it stay in the baseline, so a changed floor prints no burst. The flap damper (`alerts.flap`, default 4 changes within 1h) turns an alert that keeps firing and resolving into one `ALERT FLAPPING` line and holds its changes back until it has been stable for the window; its records live in `alerts.json`. A NEW line names the merges into the installation's lanes (`merging`, `merged`) and the lease claims on it of the last 30 minutes, each with its session.
- `alerts capture <dir>` records each installation's Alertmanager answer; `alerts replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it. `handover` shows the floors and the damper.

- `beekeeper run` records every capped run in the event log: `run.start` (the scope, the slot, the cap, the command) and `run.end` (the exit code, the duration, the cap's victims), both as the calling session. `snapshot` and `watch` name the session and the command of a memcap cap kill from its scope's `run.start`, also after the run and its session have ended. Logging never fails or delays the run: an event the log cannot take within a second is dropped.
- `log --verb <prefix>` shows only the matching events (`--verb run.`: the build runs); `handover` leaves the runs out of its latest events.

- `supervisor relay <successor>`: the supervisor names its successor, whose `supervisor start` takes the role, the grant queue and the pending grants in one step, so no claim goes ungated in between; the event log shows both steps. A relay not taken expires after `supervisor.relayTTL` (default 15m) or is withdrawn with `relay --cancel`; the outgoing supervisor keeps the role meanwhile. `supervisor status` exits 4 in the session a relay relieved. `watch` says `RELAY TAKEN` or `RELAY EXPIRED` once.
- `supervisor.shift`: `watch` says `RELAY DUE` once the supervisor has served its shift, at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued), and again only when a quiet moment follows a busy one.
- `note add --default "<what happens if unanswered>"`: a decision carries its default; `watch` reports a note once when it is due.
- `timer add <time> <what>`, `timer done`, `timer list`: `watch` prints one line when a timer is due.
- `sessions serve <session> <owner/repo#n> [--waits "<what>"]` and `sessions unserve`: a record for any session; `sessions` and `handover` show it, and `watch` prints one line when the session ends, naming the issue to re-query, instead of the plain `SESSIONS ended` line.
- `handover` shows the session records, the notes' defaults and the timers; `handover --prompt` prints the successor supervisor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope (`supervisor.scope`), the pending state in full and the commands that read the live values, without standing rules or live values.
- `state.json` keeps the fields a newer beekeeper wrote when an older one still running rewrites it.

- The merge gate: the PreToolUse hook puts `beekeeper gate` in front of every `devctl pr merge`. Merges queue per lane (`lanes` in the configuration), one at a time, the next once the lane's HelmReleases are Ready and the previous release rolled; a held repository, lane, `merges` or `github`, a budget under the floor and an unreadable installation refuse (exit 77); a wait past its bound exits 76 and keeps the merge's place; the machine runs at most `merge.cap` devctl processes; a giantswarm/devctl merge opens the tool-release window. devctl's document and exit code pass through unchanged.
- `lanes [queue|drop|clear]`: each lane's running, settling and waiting merges, also in `handover`; `queue <owner/repo> <n> --for <session>` seeds a session's place so an agreed order carries over.
- `hold set|lift|check --lane <name>`, the `merges` target and `--except`.
- `lanes settle <owner/repo> <n> [--for <session>]`: a merge run outside the gate heads its lane until it merges, so its own `devctl pr merge` passes the gate while the lane's other merges wait; merged through the gate or outside it (GitHub asked at most once a minute), it settles the lane like a gated merge. `lanes` and `handover` show it.
- `hold set --except owner/repo#n` on a lane or repository hold: the hold refuses every merge but that pull request's; `hold check` takes `owner/repo#n`, and `hold`, `lanes` and `handover` show the exception.

- `sessions` and `tail`: every running Claude Code session from disk (processes, desktop records, transcripts, checkouts), what it is on, what it runs, overlaps.
- `snapshot` and `watch`: the machine tick and the Monitor source, with every OOM kill attributed, session starts, ends and restarts, stale leases, and the installations' alerts.
- `budget`: the GitHub budget from a conditional request's headers, with every `gh` and `devctl` process and its session.
- `lease`, `hold`, `supervisor`, `agents`, `note`, `handover`, `log`: shared resources and the supervisor's state on disk, with grants required while a supervisor runs.
- `run` and `hook pretooluse`: a build, test or lint command in one of memcap's build slots under a memory cap (exit 75 when the wait runs out, 137 with the victim when the cap fires), and the PreToolUse hook that routes heavy commands through it and refuses a third kind cluster with the held leases.
- `free [--apply] [--only …] [--summary]`: where the memory is, and what can be freed without touching running work (dead sessions' dirs in the tmpfs `/tmp`, throwaway temp dirs, orphaned workers), with the `free-memory` script's TSV summary for its desktop front end; a live session's dirs stay however idle it is, and what needs root is printed as the command to run.
- `alerts watch|snapshot|import` and the `alerts:` configuration: each installation's Alertmanager (Mimir's with the `giantswarm` tenant, else the plain one) read natively over bounded `kubectl port-forward`s, in parallel with a timeout each; NEW and RESOLVED lines with pages and the configured team marked, a burst of one alertname collapsed, one line when an installation stops or starts answering, and the lease holder and sessions on the installation named; `alerts.ignore` for known flappers; one baseline with one owner, and `import` to take over an existing one. `watch` and `snapshot` include them, and `handover` shows what the alert watch reads.
- `self-update [--check]`: install the latest release once its Sigstore bundle verifies, with one rename that leaves a running `watch` alone; `--check` exits 125 while a newer release is out.

### Removed

- The `checks:` configuration and `snapshot --no-checks`: the alerts it ran a script for are read natively (`snapshot --no-alerts` skips them).

### Fixed

- The merge gate reads the release where devctl's `pr merge` document reports it (`release.tag` beside `release.verdict`, the tag counted only when the verdict is `available`): a released merge holds its lane until the release rolled, no longer only for `merge.settle` as a merge of unknown release.
- A merged run whose release devctl could not confirm no longer keeps its lane until `merge.settleTimeout`: after `merge.settle` only the lane's HelmReleases decide.

- `watch` stopped by SIGINT or SIGTERM during a poll no longer reports the journal or the GitHub budget as unreadable.
- `hook pretooluse`'s third-lab refusal names each lease holder as `lease list` does (the claim's or the live session's name), no longer the `user@host` holder field.
- `hook pretooluse` leaves a command alone only when it invokes `memcap` or `beekeeper run` at a command position; a wrapper path it merely mentions (`ls …/memcap`, `M=…/memcap`, `grep -v memcap`) no longer lets its build run without a slot.

[Unreleased]: https://github.com/giantswarm/beekeeper/tree/main
