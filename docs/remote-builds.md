# Remote build slots

`beekeeper run` can offload Go and Cargo builds to a desk-configured Kubernetes
cluster. The default remains the local memory-capped slot. Enable remote builds
only after provisioning the namespace, images, access and quotas:

```yaml
run:
  remote:
    enabled: true
    kubeconfig: /path/to/build-kubeconfig
    context: build-cluster
    namespace: build-workers
    images:
      go: registry.example/build-go:stable
      cargo: registry.example/build-rust:stable
    cpu: "2"
    memory: 4Gi
    maxPods: 8
    placementTimeout: 1m
    idleTimeout: 1h
```

Every Kubernetes call names all three of kubeconfig, context and namespace;
there is no implicit current-context selection. The caller needs `kubectl`, Git,
and permission to get/list/create/delete Jobs, get/list/watch pods and Jobs,
and exec into pods. The namespace must enforce a pod ResourceQuota at or below
`maxPods`: the client's count is a preflight, not a race-safe admission limit.
When several namespaces share a build budget, provision aggregate admission or
partition their quotas so their sum stays within that budget. Warm idle pods
count toward the quota; a full namespace falls back locally.

Each host/worktree pair gets one Job with a name derived from a hash; names
contain no host or path text. Its pod keeps Go build/module caches and Cargo
home/target between runs. The Job exits after `idleTimeout` without activity;
the Kubernetes TTL controller removes it and its pod within another minute.
Completed Jobs awaiting TTL cleanup fall back locally. Image or resource configuration changes also fall
back until the old idle Job is removed. A failed placement removes only the Job
created by that attempt, never another run's existing builder.

A run snapshots tracked files and untracked files that Git does not ignore,
including uncommitted edits and symlinks; deleted files and Git administration
are absent. Submodule worktrees fall back locally. It replaces the pod's source
copy and runs in the matching subdirectory. Source outputs are not copied back.
The image supplies the command's environment; host environment variables and
credentials are not forwarded. Caller GOFLAGS/RUSTFLAGS require local execution.

Single `go build`, `go test`, `go vet`, `cargo build`, `cargo test`, `cargo check`,
`cargo clippy`, and `cargo fmt --check` invocations are eligible, including a
single literal invocation inside `sh -c` or `bash -lc`. Shell operators,
assignments, substitutions, output redirects, absolute path arguments,
`--fix`, Cargo configuration/path overrides and Go execution helpers stay local.
Other tools and arbitrary scripts stay local. Tests that need host services,
local credentials or worktree edits should run with `beekeeper run --local -- ...`.
Formatters that write stay local automatically. Benchmark/lab classes, sandboxed
runs and managed executions with a coordinator stop unit also remain local.

Setup, admission, capacity, upload and bounded placement failures emit one line
and use the unchanged local slot. After a command might have started, it never
falls back: output resumes from the last delivered byte, with four bounded
connection attempts, and the command's exact exit code is relayed. An
unrecoverable connection returns 125. Interrupts stop the remote process group
and return 130; inability to confirm cancellation is reported. The watchdog
reclaims abandoned uploads and stops orphaned process groups. Remote stdout and
stderr form one ordered stream on the caller's stdout.

## Builder image contract

Images must support Linux amd64 and nonroot UID/GID 1000, with a writable `/work`
emptyDir. Include Bash, Git, tar, GNU coreutils (including `dd` byte-offset
options), `nohup`, and util-linux `setsid`, along with the selected toolchain.
No privileged mode, host mount, service-account token or cluster client is
available in the pod. CPU and memory requests equal their limits. Build
parallelism follows the whole cores in the CPU limit, at least one. The
workspace is limited to 30 GiB.

Shared cache storage and lifecycle cleanup on worktree/PR removal are separate
slices; this implementation keeps caches only within each warm pod. Remote
builds require a live proof against the configured cluster before rollout.
