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
that simply fail elsewhere, and the one syscall (`statfs`) is split by build tag
(`internal/machine/disk_*.go`). Pre-commit runs golangci-lint with gosec and goconst.

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

`lanes settle` and the gate's check of a settled outside merge ask GitHub through `gh pr view
<n> --repo <owner/repo> --json state,mergedAt`; a fake `gh` first on `PATH` that prints
`{"state":"OPEN","mergedAt":null}` (or `MERGED` with a time, or `CLOSED`) from a file and hands
every other call to the real one plays a pull request merging outside the gate.

`watch` finds a session by its process: a `claude` binary not running under another session,
named by `CLAUDE_CODE_HOST_SESSION_ID` and `CLAUDE_CODE_SESSION_NAME`. A copy of `sleep` named
`claude`, started detached (`setsid -f`) with those two variables, stands in for a session that
ends when the sleep does, so `sessions serve` and the `SESSION ENDED` line can be tried against
scratch state. The live supervisor's watch sees it too: give it a name that reads as a test and a
short life. Two such stand-ins play a supervisor hand-over: each runs `supervisor start`,
`supervisor relay` or `supervisor status` with its own `CLAUDE_CODE_SESSION_ID`,
`CLAUDE_CODE_HOST_SESSION_ID` and `CLAUDE_CODE_SESSION_NAME` in the environment; a short
`supervisor.shift` and `supervisor.relayTTL` in the scratch configuration and a settling merge
written into the scratch `state.json` show `RELAY DUE` held back and then said by
`watch --once`. `handover --prompt` on a copy of the live state (`cp -r` of the state and lease
directories into the scratch configuration) shows what a successor would get.

A capped run records its events in `$XDG_STATE_HOME/beekeeper/events.jsonl`: the capped-run tests
point `XDG_STATE_HOME` at their temp dir, and a manual trial does the same
(`XDG_STATE_HOME=/tmp/bk ./beekeeper run --max 64M -- python3 -c 'bytearray(200<<20)'`, then
`XDG_STATE_HOME=/tmp/bk ./beekeeper log --verb run.` and `snapshot`), so the machine's log keeps
only real builds. A run event's detail starts with the scope's unit name as the kernel prints it in
an OOM kill's memcg path; `cmd/testdata/oom-memcap-2516344.journal` holds such a kill from the lab
machine's journal, verbatim.

The alert triage is tried on recorded answers: `alerts capture <dir>` a few times, readings apart,
then `alerts replay <dir>...` with a scratch configuration whose `stateDir` holds a copy of the live
`alerts.json` (and, for the owner hints, an `events.jsonl` with a `merged` event into a lane of the
installation) prints what the watch would, floors and damper applied, and writes nothing.
`internal/alerts/testdata/` holds such readings of two installations and `cmd/testdata/` the merge
and lease events of the lab machine's log; both are reduced to what beekeeper reads, and the
recorded answers have person, organization and workload-cluster names and node addresses
replaced: this repository is public.

`free` takes its temp dir from `TMPDIR` (Claude Code's session dirs are `$TMPDIR/claude-<uid>`), so
`--apply` can be tried on a fixture instead of the real `/tmp`:
`TMPDIR=/tmp/fixture ./beekeeper free --apply --only session-dirs,tmp-dirs`.

## Layout

| Package | What it knows |
|---|---|
| `internal/proc` | The process table from `/proc`: parents, children, environment, start time. |
| `internal/claude` | Sessions: CLI processes, desktop session records, transcripts, git checkouts, what each is on and which overlap. |
| `internal/machine` | Memory, pressure, the desktop scope, disk, build slots, kind clusters, OOM kills. |
| `internal/github` | The budget from rate-limit headers; a pull request's state from `gh pr view`. |
| `internal/lease` | Lease directories and the grant rule. |
| `internal/state` | The shared state document (supervisor and its relay, the shift the watch reported, grants, holds, agents, notes, timers, session records, merges) and the event log, under a file lock; `Log` appends the events that change no state (build runs) with a bounded wait for the lock. |
| `internal/alerts` | The installations' alerts: bounded `kubectl port-forward`s in their own process group, the Alertmanager reading, the NEW/RESOLVED/FLAPPING lines with the severity floors and the flap damper, and the grouped snapshot (pure, tested against Alertmanager-shaped fixtures, recorded answers in `testdata/` and a fake `kubectl`), the baseline with the damper's records and its single owner, and recorded answers for `alerts replay`. |
| `internal/guard` | The build guard: a capped run in a build slot with its `run.start`/`run.end` events, and the PreToolUse hook's rewrite and third-lab refusal. |
| `internal/free` | What can be freed (dead sessions' dirs, throwaway temp dirs, orphaned workers) and what is only reported, as a report or the front end's TSV. |
| `internal/update` | The latest release, its signature check and the one-rename install. |
| `cmd` | The command line. |
