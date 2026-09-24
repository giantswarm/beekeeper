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
| `internal/state` | The shared state document and the event log, under a file lock. |
| `internal/alerts` | The installations' alerts: bounded `kubectl port-forward`s in their own process group, the Alertmanager reading, the NEW/RESOLVED lines and the grouped snapshot (pure, tested against Alertmanager-shaped fixtures and a fake `kubectl`), and the baseline with its single owner. |
| `internal/guard` | The build guard: a capped run in a build slot, and the PreToolUse hook's rewrite and third-lab refusal. |
| `internal/free` | What can be freed (dead sessions' dirs, throwaway temp dirs, orphaned workers) and what is only reported, as a report or the front end's TSV. |
| `internal/update` | The latest release, its signature check and the one-rename install. |
| `cmd` | The command line. |
