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
Other tools and arbitrary scripts stay local. Explicit `--max`, `--swap` or
`--wait` also select the local slot so those overrides keep their existing
resource and queue semantics. Tests that need host services,
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

Lifecycle cleanup on worktree/PR removal is a separate slice. Remote builds
require a live proof against the configured cluster before rollout. Shared
cache storage is opt-in as described below.

## Shared build cache

Enable S3-compatible storage for both toolchains through `run.remote.cache`:

```yaml
run:
  remote:
    # The cluster, images and resource settings above still apply.
    cache:
      enabled: true
      secret: build-cache
      goHelper: cacheprog
      goPrefix: go/v1/
      rustPrefix: rust/v1/
      readOnly: false
```

The Secret belongs to the configured build namespace. Its known storage keys
are projected individually with `secretKeyRef`; no credential value goes into
configuration, the pod spec, command arguments or the run journal. The client
needs no permission to read Secrets. Use these keys:

| Toolchain | Secret keys |
| --- | --- |
| Go | `CACHEPROG_S3_BUCKET`, `CACHEPROG_S3_ENDPOINT`, `CACHEPROG_S3_REGION`, `CACHEPROG_S3_ACCESS_KEY_ID`, `CACHEPROG_S3_ACCESS_KEY_SECRET` |
| Rust | `SCCACHE_BUCKET`, `SCCACHE_ENDPOINT`, `SCCACHE_REGION`, `SCCACHE_S3_USE_SSL`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optionally `AWS_REGION` and `AWS_ENDPOINT_URL_S3` |

Optional `CACHEPROG_S3_SESSION_TOKEN` and `AWS_SESSION_TOKEN` support temporary
storage credentials. Both sets point at the same S3-compatible store. Separate object prefixes keep
Go and Rust entries apart. Go forces path-style S3 access. `readOnly: true`
disables Go remote writes and selects sccache's `READ_ONLY` backend mode. It is
a client setting; enforce access boundaries with storage permissions too.

The Go image must include a cacheprog-compatible helper; tested with
[`github.com/platacard/cacheprog/cmd/cacheprog@v1.3.1`](https://github.com/platacard/cacheprog/tree/v1.3.1/cmd/cacheprog).
The Rust image must include [sccache](https://github.com/mozilla/sccache).
Helpers are installed when building the image, never downloaded during a run.
Missing Secret keys or helpers, or a failed 15-second startup probe, select
the ordinary pod-local Go cache or the native Rust compiler before the command
starts. The command is not retried. Go remote calls have five-second bounds and
stop using the remote store after three consecutive errors; sccache uses
`SCCACHE_IGNORE_SERVER_IO_ERROR=1` to retain compiler fallback.

Each Go helper gets a disk directory beneath the run journal, passed explicitly
with `--root-directory`. The command removes it when it finishes; interrupted
runs do too. This avoids unbounded local growth in warm pods. Cargo's target
and Go's native fallback cache remain warm. Shared Rust caching sets `CARGO_INCREMENTAL=0`, required by sccache; the target
directory still stays warm. The sccache server is stopped after the command. Provider startup diagnostics are suppressed; only numeric helper
cache counters are added to the output. Compiler/test output is still streamed.
Changing cache configuration retires reuse of an incompatible existing Job;
remove an idle owned Job or let its idle timeout expire before using the new
configuration.

### Connecting CI to the same store

CI can share the same bucket and prefixes when its runner can reach the store.
Keep endpoints, bucket names and credentials in CI's secret settings, and use
an image containing the same pinned helper/toolchain. No CI provider account
or network exposure is configured by this feature.

For a trusted Go job, supply the `CACHEPROG_S3_*` keys above and set:

```sh
export CACHEPROG_REMOTE_STORAGE_TYPE=s3
export CACHEPROG_S3_FORCE_PATH_STYLE=true
export CACHEPROG_S3_PREFIX=go/v1/
export CACHEPROG_DISABLE_PUT=true  # read-only CI identity
export CACHEPROG_REMOTE_GET_TIMEOUT=5s CACHEPROG_REMOTE_PUT_TIMEOUT=5s
export CACHEPROG_REMOTE_STORAGE_MAX_CONSECUTIVE_ERRORS=3
export CACHEPROG_REMOTE_STORAGE_RETRY_AFTER=0s
export CACHEPROG_MAX_BACKGROUND_WAIT=2s
export CACHEPROG_MAX_CONCURRENT_REMOTE_GETS=4
export CACHEPROG_MAX_CONCURRENT_REMOTE_PUTS=4
export CACHEPROG_ROOT_DIRECTORY="$RUNNER_TEMP/go-cache"

# This wrapper keeps provider diagnostics out of public CI logs.
cat > "$RUNNER_TEMP/cacheprog-safe" <<'SH'
#!/bin/sh
exec cacheprog direct --root-directory "$CACHEPROG_ROOT_DIRECTORY/$$" 2>/dev/null
SH
chmod 700 "$RUNNER_TEMP/cacheprog-safe"
if timeout --foreground -k 1s 15s cacheprog direct \
    --root-directory "$CACHEPROG_ROOT_DIRECTORY" </dev/null >/dev/null 2>&1; then
  export GOCACHEPROG="$RUNNER_TEMP/cacheprog-safe"
else
  unset GOCACHEPROG
fi
trap 'rm -rf "$CACHEPROG_ROOT_DIRECTORY"' EXIT
go test ./...
```

For Rust, supply the `SCCACHE_*`/`AWS_*` storage keys, use the same Rust prefix,
and set `SCCACHE_S3_RW_MODE=READ_ONLY` and
`SCCACHE_IGNORE_SERVER_IO_ERROR=1`. Set `CARGO_INCREMENTAL=0` so Rust
compilations are cacheable. Probe `sccache --start-server` with a bounded
timeout and suppressed startup diagnostics before setting `RUSTC_WRAPPER`.
Stop the server when the job ends. Do not print environment variables, debug
credentials or enable shell tracing around credentials.

Untrusted fork jobs receive no private storage credentials. If a cache is
intentionally readable by untrusted jobs, use a separate prefix-scoped
read-only storage identity or an anonymous read-only gateway. A client
read-only flag cannot restrict credentials that still have write permission.
Runners without network access or credentials keep their existing local cache.


## Builder retirement

`beekeeper worktrees` also sweeps the initiating machine's builder journal.
It retires builders for missing worktrees and for inspected worktrees whose
pull requests merged, including worktrees retained for uncommitted work.
A builder holding its per-worktree run lock is busy and stays until a later
sweep. Dry runs make no cluster changes. A failed deletion retains its journal
for retry; each journal records the original explicit cluster connection so
changing desk configuration does not redirect cleanup to a different cluster.
Ownership records stay in the machine's state directory; neither hostnames nor
worktree paths are uploaded to Kubernetes. Other machines' builders stay.
Builders created before this journal was introduced retire through their idle
timeout. If the initiating machine disappears, the pod watchdog and completed
Job TTL remain the backstop.

Worktree retirement deletes only its builder Job and local ownership record.
Shared S3 objects survive it. Configure the store's native size quota or object
age/lifecycle policy independently for the configured Go and Rust prefixes;
workers do not receive bucket-administration permissions or enumerate/delete
shared objects during worktree cleanup.
