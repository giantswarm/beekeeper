# Developing on beekeeper

```bash
make test        # go test ./...
go build .       # ./beekeeper
```

A plain build reports the version Go derives from the checkout's tag (`v0.2.0+dirty`), a test
binary `dev`, which `self-update` refuses. To try `self-update` against the latest release, stamp
an older version: `go build -ldflags "-X github.com/giantswarm/beekeeper/pkg/project.version=0.0.1" .`

CI (`architect/go-build`) runs `make test` and cross-compiles for Linux, macOS and Windows, so
every package has to compile everywhere. Linux-only reads stay behind `/proc` and cgroup paths
that simply fail elsewhere, and the syscalls are split by build tag: `statfs`
(`internal/machine/disk_*.go`) and the gate's re-exec of a replaced binary (`cmd/reexec_*.go`,
Linux only, a no-op elsewhere). Pre-commit runs golangci-lint with gosec and goconst.

The `run` tests (`cmd/guard_test.go`) need zsh and a user systemd: they skip in CI and run on a
desktop. The test binary doubles as beekeeper (`BEEKEEPER_TEST_MAIN=1`), so the hook's rewrite runs
the build under test, and each test starts its command in a scope of its own: on a guarded machine
`go test` itself runs in a capped scope, where `run` takes no slot.

Try a change against the live machine without touching the real state: point a scratch
configuration at a scratch state directory.

```bash
cat > /tmp/bk.yaml <<EOF
stateDir: /tmp/bk-state
resources: [kind-1, kind-2]
EOF
BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper sessions
```

The merge gate runs against a fake devctl first on `PATH` (a script that sleeps and prints a
devctl-shaped document) and scratch state, never a real merge. A lane with an `installation` also
reads HelmReleases, which a fake `kubectl` can answer from a file. Feed the hook an event to see
the rewrite, and run what it prints:

```bash
echo '{"tool_name":"Bash","tool_input":{"command":"devctl pr merge giantswarm/marge 1"}}' |
  BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper hook pretooluse
PATH=/tmp/fake:$PATH BEEKEEPER_CONFIG=/tmp/bk.yaml CLAUDE_CODE_SESSION_ID=a \
  ./beekeeper gate --wait 10s -- devctl pr merge giantswarm/marge 1
BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper lanes
```

The permission hook answers from the state's `starts`. Feed it a request: it prints the allow
only for a session id recorded with `bypassPermissions` and a request in `acceptEdits`, and
nothing otherwise. `agents start` against a scratch configuration still starts a real session
and imports it into the desktop: prove it with a throwaway brief on the smallest model, in a
throwaway folder whose `.claude/settings.local.json` carries the hook, and archive the session
afterwards.

The hand-over is proven the same way: a scratch configuration with a small `agents.relayAt`
(20k is past a fresh session's first turn), a throwaway agent started with `agents start` on a
multi-step brief that outlives the hand-over, `watch --once` saying `HANDOVER DUE` for it once
it is quiet, then `agents handover`. The started sessions get the scratch configuration as
`$BEEKEEPER_CONFIG`, and the note's command names it, so nothing reaches the real state.
`agents handover --prompt` shows the prompt without starting anything.

```bash
echo '{"hook_event_name":"PermissionRequest","session_id":"<id>","permission_mode":"acceptEdits","tool_name":"WebSearch"}' |
  BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper hook permissionrequest
```

The hook's merge rewrite is pinned against real commands: `internal/guard/testdata/merges.jsonl`
holds every statement with a `devctl pr merge` from the lab machine's Claude Code transcripts,
reduced to its shell skeleton (every word outside the shell structure is `x`, every repository
`o/r`, so nothing internal lands in this public repository) with the number of merges a shell
parser (mvdan.cc/sh) counts in it. `TestHookGatesRealMerges` requires every one gated and the
rewrite to add nothing but the gates. To refresh it, extract the Bash `tool_use` commands matching
`devctl pr merge` from `~/.claude/projects/*/*.jsonl`, keep the top-level statements the parser
finds a merge in, reduce them the same way and deduplicate. Before a release that touches
`internal/guard`, feed the old and the new binary every distinct Bash command of the transcripts
and compare their answers: no build rewrite may change.

`FAKE_EXIT=3` on such a fake plays a run with nothing merged (the document's `mergeCommitSha`
empty): the merge stays in `lanes` as `retrying`, and the next gate call for it runs first. The
queue rule itself is tested by replaying a real event trail (`internal/merge/testdata/*.jsonl`,
the sessions' ids left out) through `merge.Queue`, `Lane.Ahead` and `merge.Failed`.

A merge that outlives its caller is proven the same way: a fake devctl that sleeps before its
document, the gate started under `setsid zsh -c '…'` with its PIDs captured at spawn, then
`kill -HUP` and `kill -TERM` to the caller's process group mid-merge. The shell dies, devctl (in a
session of its own, `ps -o sid=`) and the gate run on and log `merged`. A `kill -KILL` to the whole
group, the gate included, leaves devctl running: `watch --once` does not call the merge lost until
devctl ended. Live, a background `devctl pr merge` of a docs pull request whose command is stopped
while it waits for the checks is logged `merged` once the checks pass.

`lanes settle` and the gate's check of a settled outside merge ask GitHub through `gh pr view
<n> --repo <owner/repo> --json state,mergedAt`; a fake `gh` first on `PATH` that prints
`{"state":"OPEN","mergedAt":null}` (or `MERGED` with a time, or `CLOSED`) from a file and hands
every other call to the real one plays a pull request merging outside the gate.

The roll check is tested against a real answer: `internal/merge/testdata/helmreleases-gazelle.json`
is `kubectl get helmreleases -A -o json` of an installation, trimmed to chart names, versions (with
their build metadata) and Ready conditions. `watch --once` against scratch state with a settling
merge and a fake `kubectl` answering such a file shows the lane freed (`lane.settled`).

A stalled lane plays with a short `merge.stallAfter` (10s): seed two places with `lanes queue`,
run the second one's `beekeeper gate --wait 1m` in the background, and after 10s `lanes` shows
the lane `stalled` and `watch --once` prints one `LANE STALLED` line. The re-exec plays with two
builds stamped with different versions (`-ldflags "-X …/pkg/project.version=…"`): start a
waiting gate call from build A's path, rename build B over that path (`mv -f`, as `self-update`
does), and within 5s the call prints `continuing under beekeeper <B>` and keeps its position in
`lanes`. The test binary is not re-executed: `cmd/reexec_linux_test.go` covers noticing the
replacement.

`watch` finds a session by its process: a `claude` binary that is no subcommand (`daemon`,
`bg-pty-host`, `stop`, …) and not the `--bg` launcher, running under no other session's CLI
unless it has an id of its own (`--session-id`, `--resume`, a woken worker's transcript path),
or a daemon's spare (`claude bg-spare`) once the daemon has handed it a session. The CLI's record
in `claude.sessionsDir` (`~/.claude/sessions/<pid>.json`, what `claude agents` lists) names the
session it runs, and counts only while its `procStart` is the process's start time in clock
ticks, so a record a killed CLI left behind names no later process under its PID. Without a
record, the command line and `CLAUDE_CODE_HOST_SESSION_ID` and `CLAUDE_CODE_SESSION_NAME` name
it. A CLI whose environment has
`CLAUDE_CODE_SESSION_ID` was started from inside that session and inherited its host id:
beekeeper takes that session as its parent and ignores the host id. A copy of `sleep` named
`claude`, started detached (`setsid -f`) with those two variables and without
`CLAUDE_CODE_SESSION_ID` (`env -u CLAUDE_CODE_SESSION_ID`, since every tool shell has it),
stands in for a session that ends when the sleep does, so `sessions serve` and the `SESSION ENDED` line can be tried against
scratch state. The live supervisor's watch sees it too: give it a name that reads as a test and a
short life. Two such stand-ins play a supervisor hand-over: each runs `supervisor start`,
`supervisor relay` or `supervisor status` with its own `CLAUDE_CODE_SESSION_ID`,
`CLAUDE_CODE_HOST_SESSION_ID` and `CLAUDE_CODE_SESSION_NAME` in the environment; a short
a small `supervisor.relayAt` and a short `supervisor.relayTTL` in the scratch configuration,
a supervisor stand-in started with `--session-id <id>` whose transcript (a real one with its
content stripped, as `<claude.projectsDir>/<project>/<id>.jsonl`) reports a context above it,
and a settling merge written into the scratch `state.json` show `RELAY DUE` held back and then
said once by `watch --once`. A supervisor stand-in killed by its PID and started again with the same
`--resume <id>` within `supervisor.restartGrace` is a CLI restart: a claim loop from a third
identity stays refused throughout and `watch --once` says `SUPERVISOR RESTARTED`; one not started
again keeps the claim loop refused past the grace, until a successor's `supervisor start`. A
scratch standby watch (`watch --notify --standby`) run inside `dbus-run-session`, with
`dbus-monitor` recording the `Notify` calls so nothing reaches the desktop, says `SUPERVISOR
GONE` once past the grace and `SUPERVISOR BACK` once a successor stand-in starts; a stand-in
that ran `supervisor stop` before its kill gives neither and leaves claims ungated. The spare's keep-awake and the crash hand-over need desktop sessions, never the live ones:
throwaway sessions titled `test: …` on the smallest model, made with `claude -p --session-id <id>`
in a scratch folder and imported with `claude://resume?session=<id>` (then
`claude://code/continue?session=<previous>` puts the window back). A `.claude/settings.local.json`
in that folder sets `BEEKEEPER_CONFIG` to the scratch configuration and allows `Bash(beekeeper *)`,
so their commands never touch the live state. `BEEKEEPER_PEER_TEST=<title> go test -run TestLive
./internal/peer/` sends one message to such a session from the command line and checks that an
absent name is unreachable. A scratch `watch --standby` with the spare in the scratch state keeps
one session awake past 35 idle minutes while an untouched one is dropped at 30; a stand-in
supervisor killed by its CLI's PID shows `SUPERVISOR GONE` naming the spare, the `HANDOVER` line
and the spare's `supervisor start`, with claims refused until then. `supervisor reopen` against a
scratch state whose supervisor is a stopped throwaway session starts its CLI through the deep
link. Archive the sessions afterwards. `handover --prompt` on a copy of the live state (`cp -r` of the state and lease
directories into the scratch configuration) shows what a successor would get.

The roster is tried with real `claude --bg` workers against scratch state: started through
`systemd-run --user` with the session variables cleared and `BEEKEEPER_CONFIG` naming the
scratch configuration, a haiku worker started with `-n "test: …"` registers under that title
without `--name` (its tool commands inherit no name: `caller` reads its CLI's record). Once it is
stopped (`claude stop <id>`), a second worker under the same title replaces its entry and gets
the next `agents assign`; a task assigned to the stopped worker's entry before the second one
registers is the second one's (`agents --json`, and `beekeeper log --verb agents.` shows
`takes over "<task>"`); a third session registering under the name of a running worker is
refused. `claude stop` and `claude rm` end the workers; the daemon and its spare are killed by
their PIDs.

A capped run records its events in `$XDG_STATE_HOME/beekeeper/events.jsonl`: the capped-run tests
point `XDG_STATE_HOME` at their temp dir, and a manual trial does the same
(`XDG_STATE_HOME=/tmp/bk MEMCAP_TEST=1 ./beekeeper run --max 64M -- python3 -c 'bytearray(200<<20)'`, then
`XDG_STATE_HOME=/tmp/bk ./beekeeper log --verb run.` and `snapshot`), so the machine's log keeps
only real builds. A run event's detail starts with the scope's unit name as the kernel prints it in
an OOM kill's memcg path; `cmd/testdata/oom-memcap-2516344.journal` holds such a kill from the lab
machine's journal, verbatim. The capped-run tests also set `MEMCAP_TEST=1`, so their scopes are
`memcap-test-…` and the exit-137 test's deliberate kill reads as a test kill in the machine's
`watch` and `snapshot`, which have no `run.start` for it; `cmd/testdata/oom-memcap-287208.journal`
is that kill from before the test scopes, which must read `cap unknown`. A trial of your own sets it
too.

The output is a context budget. `cmd/testdata/watch-hour.txt` is a real hour of a supervisor's
`watch` under v0.15.5 (anonymised: installations, nodes, pods, sessions and pull requests renamed),
6977 bytes in 74 lines; `TestWatchReplayedHourSaysEachFactOnceInBudget` replays it through the
watch's session tracking, alert lines and brackets and fails when a fact is said twice or dropped,
or when the hour prints more than `hourBudget` bytes (5862 now). Raise the budget only for a line
that carries new information. A second read of a reading command with nothing changed is one
`no change` line (`TestSecondReadSaysNoChange`); measure a change of your own the same way:
`--full` and a first read against a scratch copy of the state (`BEEKEEPER_CONFIG` naming a config
whose `stateDir` is the copy), then a second read, bytes by `wc -c`.

Session discovery is tested against real process trees in `internal/claude/testdata/proc/`: a
desktop session with a `claude -p` its tool shell runs and one it left to the user manager, and
a `claude --bg` worker started through `systemd-run` with its daemon, terminal hosts and spare,
two workers started one after the other through the same daemon (the second in the claimed
spare) and a worker stopped and woken with `claude --bg --resume <id>`. The CLI records of the
last two are in `internal/claude/testdata/sessions/`, reduced to the keys beekeeper reads and a
few more. Each process is its `stat`, its `cmdline` and its environment reduced to the `CLAUDE*` keys, with
values only for the identity keys (session and host ids, name, kind, entry point); paths are
neutral, ids fake and names `test: …`. `proc.ReadAt` reads such a tree as it reads `/proc`. A new
shape is captured the same way, from workers named `test: …` started through
`systemd-run --user` (the user manager's environment has no session ids), stopped and removed
after. The second `claude --bg` through a running daemon lands in its spare; a wake lands in the
spare too, or in a fresh `claude --resume <transcript>` when a new daemon serves it, and a
`--resume` with flags of its own starts a copy instead of waking the session. The daemon exits
once its last worker stops.

The alert triage is tried on recorded answers: `alerts capture <dir>` a few times, readings apart,
then `alerts replay <dir>...` with a scratch configuration whose `stateDir` holds a copy of the live
`alerts.json` (and, for the owner hints, an `events.jsonl` with a `merged` event into a lane of the
installation) prints what the watch would, floors and damper applied, and writes nothing.
`internal/alerts/testdata/` holds such readings of two installations and `cmd/testdata/` the merge
and lease events of the lab machine's log; both are reduced to what beekeeper reads, and the
recorded answers have person, organization and workload-cluster names and node addresses
replaced: this repository is public.

Desktop notifications are tried against the real notification service on a scratch configuration
whose `stateDir` holds a copy of the live state and whose `notify.kinds` names only the kinds under
trial, so a trial sends few notifications and never takes the live watch's events or alert baseline:
a scratch timer due in 30 seconds (`timer add 30s "TEST …"`) under `watch --notify` is one
notification, and `notify.json`'s `log` shows the id the service returned. `quietHours` covering now
holds a normal one and passes a critical one (`urgency: {stale-lease: critical}` with a scratch
lease whose holder is a finished process); `DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent` shows
the watch running on and saying so once; two scratch watches with `--notify` on the same state send
one notification per event. The unit test's fake `notify.Sender` covers the same rules without a
bus. Label every trial notification as a test in its text.

`free` takes its temp dir from `TMPDIR` (Claude Code's session dirs are `$TMPDIR/claude-<uid>`), so
`--apply` can be tried on a fixture instead of the real `/tmp`:
`TMPDIR=/tmp/fixture ./beekeeper free --apply --only session-dirs,tmp-dirs`.

Session metrics are tested against `internal/claude/testdata/transcript/window.jsonl`, the last
512 KiB of a real transcript of the lab machine stripped to what the figures read: each entry's
type, timestamp and `isMeta`, the message id (renumbered), model and usage fields, the content
blocks' types, tool names, tool-use ids (renumbered) and `is_error`, text replaced by `x`. A new
fixture is made the same way, never written by hand, and its expected figures computed with `jq`
over the file (usage summed once per message id, the last line of an id being its final usage).
The repeats, GitHub calls and turn rules are tested on synthetic lines without usage. Timing is
measured with `beekeeper sessions --json` against the installed release on the same sessions.

## Layout

| Package | What it knows |
|---|---|
| `internal/proc` | The process table from `/proc`, or a copy of it in testdata: parents, children, environment, start time. |
| `internal/claude` | Sessions: CLI processes (desktop, background, headless, and the children a session starts), desktop session records, transcripts, git checkouts, what each is on and which overlap, and how each has been doing (turns, tool calls and errors, tokens and cost, context), read from the same 512 KiB window of its transcript in one pass. |
| `internal/machine` | Memory, pressure, the desktop scope, disk, build slots, kind clusters, OOM kills. |
| `internal/github` | The budget from rate-limit headers; a pull request's state from `gh pr view`. |
| `internal/lease` | Lease directories and the grant rule. |
| `internal/peer` | The command-line send to a running Claude session: one headless `claude -p` turn whose only tool is SendMessage, by the session's ListAgents name. It rests on Claude Code's undocumented peer messaging, so it is kept here with a live test. |
| `internal/state` | The shared state document (supervisor and its relay, the guide's role record (`Role`, the same shape the supervisor's flat fields read as through `SupervisorRole`), the relay due the watch reported, grants, holds, agents, notes, timers, session records, merges, the sessions `agents start` started with their mode) and the event log, under a file lock (`Peek` reads it without the lock, for the permission hook); `Log` appends the events that change no state (build runs) with a bounded wait for the lock. |
| `internal/notify` | Desktop notifications: the kinds, urgencies and quiet hours, the ledger `notify.json` under `notify.lock` that makes each event one notification across watches and holds the quiet hours' ones, and the D-Bus sender to `org.freedesktop.Notifications` (godbus, never `notify-send`, never an autolaunched bus). |
| `internal/alerts` | The installations' alerts: bounded `kubectl port-forward`s in their own process group, the Alertmanager reading, the NEW/RESOLVED/FLAPPING lines with the severity floors and the flap damper, and the grouped snapshot (pure, tested against Alertmanager-shaped fixtures, recorded answers in `testdata/` and a fake `kubectl`), the baseline with the damper's records and its single owner, and recorded answers for `alerts replay`. |
| `internal/upgrade` | Cluster upgrades: the detection over an installation's Clusters, control planes and node pools (pure, tested against a real upgrade replayed from `testdata/prod/<phase>/`, stripped to the fields read), the bounded parallel `kubectl` reading with the events that name the release upgraded from, and the automatic `upgrade:<installation>/<cluster>` holds the merge gate and `lease claim` read. |
| `internal/guard` | The build guard: a capped run in a build slot with its `run.start`/`run.end` events, the PreToolUse hook's rewrite and third-lab refusal, and the PermissionRequest hook's decision. |
| `internal/free` | What can be freed (dead sessions' dirs, throwaway temp dirs, orphaned workers) and what is only reported, as a report or the front end's TSV. |
| `internal/update` | The latest release, its signature check and the one-rename install. |
| `cmd` | The command line. The supervisor and the guide are one `role` type (`cmd/relay.go`): start, relay, relief, restart grace and relay due are written once and parameterised by the role's name, record and configuration; only the supervisor's start moves the grants. The guide's queue (`guideQueue`) holds the notes for `guide.person` (`noteFor`: a note's `For`, or an older note's `[for <person>]` text prefix, compared without case) and the waiting sessions, running ones from their desktop record and stopped ones from `claude.StoppedWaiting` (a record not parsed unless it holds `"blocked"`), without the guide's own session, an archived one or a test (`Session.Aside`, `Record.Aside`); its feed drops a note it said before that is still open but no longer in the queue without a line. |
