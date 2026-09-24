# Developing on beekeeper

```bash
make test        # go test ./...
go build .       # ./beekeeper
```

CI (`architect/go-build`) runs `make test` and cross-compiles for Linux, macOS and Windows, so
every package has to compile everywhere. Linux-only reads stay behind `/proc` and cgroup paths
that simply fail elsewhere, and the one syscall (`statfs`) is split by build tag
(`internal/machine/disk_*.go`). Pre-commit runs golangci-lint with gosec and goconst.

Try a change against the live machine without touching the real state: point a scratch
configuration at a scratch state directory.

```bash
cat > /tmp/bk.yaml <<EOF
stateDir: /tmp/bk-state
resources: [kind-1, kind-2]
EOF
BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper sessions
```

## Layout

| Package | What it knows |
|---|---|
| `internal/proc` | The process table from `/proc`: parents, children, environment, start time. |
| `internal/claude` | Sessions: CLI processes, desktop session records, transcripts, git checkouts, what each is on and which overlap. |
| `internal/machine` | Memory, pressure, the desktop scope, disk, build slots, kind clusters, OOM kills. |
| `internal/github` | The budget from rate-limit headers. |
| `internal/lease` | Lease directories and the grant rule. |
| `internal/state` | The shared state document and the event log, under a file lock. |
| `internal/check` | External commands configured as checks. |
| `cmd` | The command line. |
