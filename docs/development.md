# Developing on beekeeper

```bash
make test        # go test ./...
go build .       # ./beekeeper
```

A plain build reports the version Go derives from the checkout's tag (`v0.2.0+dirty`), a test
binary `dev`, which `self-update` refuses. To try `self-update` against the latest release, stamp
an older version: `go build -ldflags "-X github.com/giantswarm/beekeeper/pkg/project.version=0.0.1" .`

Before a reviewed supervisor merge, the lane builds the pull request merged into the
freshly fetched base in a temporary worktree of the repository's local checkout (found
in `disk.checkouts`, `agents.dir` or `agents.roots`, `~/` expanded, as `<root>`,
`<root>/<name>` or `<root>/<owner>/<name>`) whenever the base has moved past the head.
For Go modules it runs configured lint and `go test ./...`, including unchanged tests and
reverse dependents even for dependency-only changes; `golangci-lint` must be on PATH. A
failing combination goes back to its owner. A failed fetch, a reviewed commit that cannot
be read or a missing lint tool refuses the merge with
`merge-result validation unavailable: <the condition that failed>`. A repository with no
local checkout is not validated: the merge's reason says so and the pull request's own
checks stand, as for a head that already contains its base.

Where the base branch has a GitHub merge queue (required CI on `merge_group` events), the
merge is submitted to the queue, which retests the combination if the base advances, and
never with `--admin`. Without a queue the lane's own check is the validation and the
desk's admin merge (`merge.admin`) applies; an unreadable queue answer refuses the merge.

A merge to `main` cuts a release candidate, `vX.Y.Z-rc.N` (a GitHub pre-release with signed assets);
`devctl release promote giantswarm/beekeeper` makes the stable release, and `self-update` installs
stable releases only. To run a candidate before its promotion, build it from its tag:
`go install github.com/giantswarm/beekeeper@<tag>` is refused because `go.mod` carries `replace`
directives, so check the tag out (`git worktree add <dir> <tag>`) and run `go install .` there; the
binary reports the tag's version. A binary installed by hand gets none of `self-update`'s restarts:
restart the standby watch and the sandbox broker onto it yourself
(`systemctl --user restart beekeeper-notify.service beekeeper-sandbox.service`).

CI (`architect/go-build`) runs `make test` and cross-compiles for Linux, macOS and Windows, so
every package has to compile everywhere. Everything beekeeper reads or starts on the machine goes
through `internal/platform`: its `Machine`, `Launcher`, `Capper`, `Opener` and `Notifier`
interfaces have one implementation per build, `linux_systemd` on Linux (`systemd_linux.go`,
`notifier_linux.go`) and a stub elsewhere and under `-tags nosystemd` (`stub.go`) whose parts return
a `platform.NotAvailableError`. `GOOS=darwin go build ./...` and `go build -tags nosystemd ./...`
check both. The remaining syscalls are split by build tag: `statfs` (`internal/machine/disk_*.go`),
the gate's re-exec of a replaced binary (`internal/platform/binary_*.go`, Linux only, a no-op
elsewhere) and the detached merge child (`internal/platform/detach_*.go`).

## Diagnosing a failed go-build without a CircleCI token

For this public repository, the CircleCI v1.1 job endpoint and each step's
`output_url` are readable without authentication. Take the numeric job ID from
the failed check's details link, then fetch the output (requires `curl` and `jq`):

```bash
job=13711 # replace with the failing job's number
curl -fsSL "https://circleci.com/api/v1.1/project/github/giantswarm/beekeeper/$job" > job.json
jq -r '.steps[].actions[] | .output_url // empty' job.json |
  while IFS= read -r url; do
    curl -fsSL "$url" | jq -r '.[].message'
  done > job.log
# Names, assertion diagnostics and cleanup errors are in the complete log.
rg -n -C 4 -- '--- FAIL:|panic:|test timed out|^FAIL' job.log
```

This also works for main and tag jobs; it does not depend on `devctl ci jobs`
or a worker receiving a CI credential. Private repositories require a separate
authorized log reader. Keep fetched logs local: diagnostics may contain
execution details that should not be copied into public comments.

Job 13711 identified `TestARebaseableMergeQueuesAsBefore/not_computed`: its
fake child acknowledged startup before finishing a spec copy, racing temporary
directory cleanup under load. The fixture now completes its writes before
publishing its PID, and callers read the spec immediately after that handoff.
Track further flakes in [#439](https://github.com/giantswarm/beekeeper/issues/439)
and [#1180](https://github.com/giantswarm/beekeeper/issues/1180).

## Checks before committing

Pre-commit runs golangci-lint (the version CI's `pre-commit` workflow pins) over every package, test
files included, with the linters of `.golangci.yaml`: a bare `golangci-lint run ./...` lints what CI
does, and main lints clean. `goconst` counts a string across a whole package, production and test
files together, so a change that adds or removes a string in `cmd/snapshot.go` can raise a finding
in an untouched `cmd/*_test.go`: that finding is the change's, and CI reports it too. A lint of a
worktree of `origin/main` tells main's findings from a change's.

Run `gitleaks detect --redact --log-opts=--all` from the repository root to scan the full
history, or `gitleaks detect --redact --no-git` to scan the working tree. Both load
`.gitleaks.toml`, which extends all default detection rules. Its signing-test exception
requires the exact fake GPG key ID and `cmd/sandboxsign_test.go` together, and applies
only to `generic-api-key`; other values and files remain scanned.

The Kubernetes store's tests (`internal/state/kube`) and `beekeeper serve`'s (`cmd/serve_envtest_test.go`,
every tool over streamable HTTP behind a test Dex from `internal/identity/identitytest`) run against
envtest's kube-apiserver and skip without `KUBEBUILDER_ASSETS`: `make test-envtest` downloads it and runs them, as CI's
`test-envtest` job does after `make check-crds`. HTTP authentication and body-limit tests run without Postgres,
including from a linked git worktree. The mailboxes' tests (`internal/mailbox`, and serve's, which need
both) run against the Postgres `BEEKEEPER_TEST_DATABASE_URL` names, each in a database of its own, and skip without it;
CI's job runs one beside it, and locally the same image does:

```bash
docker run -d --rm --name beekeeper-pg -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1:55432:5432 \
  gsoci.azurecr.io/giantswarm/postgres:18.6-alpine
BEEKEEPER_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable' make test-envtest
``` The CRDs in `config/crd` and the deepcopy code are
generated from `pkg/apis` by `make generate-crds`; edit the types, never the YAML.

The Kubernetes writer publishes `Ready=True` with reason `StatePersisted` on
Notes, Holds, RosterEntries, Environments and MergeLanes after writing their
status, including claims and releases. Each condition observes the object's
current generation. Ready means the writer persisted the state; an active hold,
a busy roster entry or a nonempty queue can all be Ready. Objects that have not
yet been written by beekeeper have no Ready condition. For example,
`kubectl wait --for=condition=Ready environment/<name>` waits for that first
successful write; use the appropriate resource and namespace for other kinds.

A Note stores its filer and filing time in its initial spec as well as status. Kubernetes
creates the object and writes its status separately; if the latter fails, the note still
lists as open and can be answered, withdrawn or closed by its default. Older objects
without either status or spec provenance remain visible with their namespace's team and
creation time, without an invented filer identity. Listing does not repair status during
another object's update. Apply the generated Note CRD before running the new server so
Kubernetes preserves these optional spec fields.

`note show <id>` reads one note in full, with its status, audience and retained answer;
`--json` returns the note fields plus `status`, `answer`, `reason` and `closure`. Open central
notes use `<context>#<n>`; a bare ID present locally and centrally is refused,
while `#<n>` selects the local record. The central instance's own CLI can also
read retained closed Note resources. File-backed stores can recover answered
notes while their full `answer.recorded` event remains, without a record-size cap.
New answer events retain the original audience separately from the answerer;
older events without that audience show it as unknown rather than naming the
answerer as the addressee. `note done <id> --reason "<why>"` (or the
`note_done` tool's required `reason`) retains the withdrawal reason and original
question; `note show` reads these from new file-backed withdrawal events too.
Older withdrawal events without this record remain unavailable. Other removed or expired
records are reported as unavailable, never reconstructed from a truncated log.

A note answer reaches its filing session by session or harness ID even when
that session is absent from the agent roster or has no display name. Harness
IDs select their own inbox rather than treating a UUID conversation ID as a
Claude session. The answer remains recorded when notification fails: the
answer command reports the failure and `note show` retains the last notification
attempt as `notification: failed` with its reason (`delivery` in JSON).
Successful attempts show `delivered`; a central mailbox shows `queued`, and a
role awaiting its next holder shows `waiting`. This is the recorded attempt,
not a claim that a queued message has since been consumed. Older notes without
a retained delivery event show no invented notification status.

`note list` and `guide queue` show open notes. An answer removes a note from both; a
successful filing followed by an empty listing can mean it was answered between those
calls. Check the server's `note_add` and `note_answer` audit lines before diagnosing a lost
note. A failed central read emits a warning on stderr and preserves the local listing;
`note list` also returns the central read's error, while `guide queue` serves the local queue.

The `run` tests (`cmd/guard_test.go`) need zsh and a user systemd: they skip in CI and run on a
desktop. The test binary doubles as beekeeper (`BEEKEEPER_TEST_MAIN=1`), so the hook's rewrite runs
the build under test, and each test starts its command in a scope of its own: on a guarded machine
`go test` itself runs in a capped scope, where `run` takes no slot.

`TestWatchStopsSandboxOrphan` additionally needs bubblewrap. It starts a real PID namespace,
enters a scratch scope through the broker, and leaves a background child adopted by the sandbox
init. Sandboxed `Capper.Reap` leaves cleanup to the host watch: the test proves a reporting-only
watch preserves the child and an active watch stops its scope after the orphan grace period.
Its scope and sandbox are stopped at test cleanup; no installed configuration is changed.

Try a change against the live machine without touching the real state: point a scratch
configuration at a scratch state directory.

To prove a candidate's secret verbs before installing it, run a command under
its temporary broker (Linux):

```sh
./beekeeper --config /tmp/test-config.yaml sandbox scratch -- sh -c '
  ./beekeeper secret fingerprint "file:///tmp/test-input.json#password"
'
```

`sandbox scratch` copies the selected configuration and replaces its state,
lease and runtime directories with private temporary directories. The child
inherits `BEEKEEPER_CONFIG`; secret calls use the candidate's broker, including
when `secret.session` is off. Use the candidate binary explicitly in the child.
The broker does not start an egress proxy or install sandbox mounts. Its spool
and vault socket are separate from the installed broker's. On command exit,
failure or interrupt, the runner stops and reaps the broker before removing the
temporary directories. Run outside the agent sandbox and use test secret sources
and destinations: the configured sources and any requested writes remain real.

The doctor releases a local lease when its session is no longer running,
preserving any lab configuration drift for the next claimant. Person-held
leases and remote holders are left alone. Each release is logged once as
`lease.release-gone`. Configured kind labs with no lease are stopped after
15 minutes (`lab.stop`); containers and their data stay intact. The idle
observation is persisted across watch restarts and reset by a claim.
Unmapped clusters are never stopped. A failed Docker read or stop is reported
and retried on the next doctor tick; `doctor --dry-run` changes nothing.

A handover successor's recorded start protects inherited leases throughout
the five-minute first-reply startup window, including when the doctor's
process snapshot predates the handover or launch. The original claim time
stays intact; after the startup window an absent successor's lease is
eligible for normal cleanup.

A successful lab claim starts existing stopped kind nodes (`lab.start`)
before returning the kubeconfig. It does not create a missing cluster. A
failed restart fails the command while retaining ownership for a claim retry.
Sandboxed claims run the restart through the host broker, which rechecks
ownership. Docker stop and start operations share the state lock with claims.
For a live lifecycle proof, use isolated state and disposable Docker containers
with a unique `io.x-k8s.kind.cluster` label: seed an idle timestamp 15 minutes
ago, run the doctor, then claim the lease and check that the same containers
restart with a marker file intact. Never point scratch state at another
worker's lab.

```bash
cat >| /tmp/bk.yaml <<EOF
stateDir: /tmp/bk-state
resources: [kind-1, kind-2]
EOF
BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper sessions
```

### GitHub App dry run with a built binary

`BEEKEEPER_GITHUB_API_URL` and `BEEKEEPER_GITHUB_WEB_URL` override the
in-process GitHub API and web base URLs at startup. Empty or unset values keep
the public GitHub defaults; trailing slashes are removed. These apply to manifest
conversion, App sign-in and the gate's in-process GitHub reads. They do not
configure the separate `gh` executable: put a fake `gh` first on `PATH` for
commands that invoke it (including App discovery and account lookup).

To exercise the binary's wiring, build with `go build .`, start a local fake
GitHub, and set both variables to its base URL. Use a scratch configuration and
state, a disposable manifest and SOPS target, and fake vault/SOPS tools on `PATH`
so the whole exercise uses test credentials. For example, with the fake server's
base URL in `FAKE_GITHUB_URL` and the scratch configuration in `BEEKEEPER_CONFIG`:

```bash
export BEEKEEPER_GITHUB_API_URL="$FAKE_GITHUB_URL"
export BEEKEEPER_GITHUB_WEB_URL="$FAKE_GITHUB_URL"
./beekeeper app create example --manifest /tmp/manifest.json --org example --no-open --timeout 30s
```

The scratch configuration's `apps.secrets.example` must name the disposable
SOPS target. Have the fake `gh` report the App absent and answer the account ID
lookup. Open the loopback page printed by the command and submit its manifest
form. The form must target the fake server's
`/organizations/example/settings/apps/new`, including the state query parameter.
The fake web handler reads the submitted manifest's `redirect_url` and redirects
to it with `code=example-code` and the unchanged state. Its API handler must see
an unauthenticated `POST /app-manifests/example-code/conversions` and return HTTP
201 with a JSON object containing `id`, `slug`, `client_id`, `client_secret` and
`pem` (dummy values suffice for the conversion; use a fixture private key for
subsequent sign-in). The browser then reaches the fake server's installation
page, and only the disposable SOPS target receives the dummy credentials.
`--no-open` lets the test driver control the browser; it still performs the
conversion and write. `secret github-app --dry-run` only validates metadata and
does not exercise HTTP wiring.

The fresh-process regression test can also be run without vault or SOPS tools:
`go test ./cmd -run '^TestGitHubURLsFromEnvironment$' -count=1`. It verifies the
public defaults, the manifest form action, conversion against a local fake API
and the device flow against a local fake web endpoint.

The merge gate runs against a fake devctl first on `PATH` (a script that sleeps and prints a
devctl-shaped document) and scratch state, never a real merge. A lane with an `installation` also
reads HelmReleases, which a fake `kubectl` can answer from a file. Feed the hook an event to see
the rewrite, and run what it prints. A hook proof requires a **real in-scope
session ID**, never a made-up ID: out-of-scope events pass through without
exercising the guard. Run `beekeeper sessions` with the configuration being
proved and copy the ID of a session beekeeper started or knows; in a Claude
Code tool shell, `$CLAUDE_CODE_SESSION_ID` supplies the current session ID.
For agy, use the real conversation ID from its session record. Include that
session's actual `cwd` (agy: `workspacePaths`). Scope also includes directories
under `hooks.scope.dirs`; when that setting is unset, every session is in scope.
A scope check verifies membership, not session liveness, so obtaining a real ID
is still required.

Save the exact event and check it before each proof. Use the same configuration,
state and environment for the preflight and hook. `check-scope` exits nonzero
with an explicit out-of-scope error rather than treating a pass-through as proof:

```bash
# Set SESSION_ID to the real ID obtained above, and SESSION_CWD to its cwd.
: "${SESSION_ID:?real in-scope session ID required}"
: "${SESSION_CWD:?session working directory required}"
jq -n --arg id "$SESSION_ID" --arg cwd "$SESSION_CWD" \
  '{session_id:$id,cwd:$cwd,tool_name:"Bash",tool_input:{command:"devctl pr merge giantswarm/marge 1"}}' > /tmp/bk-event.json
BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper hook check-scope < /tmp/bk-event.json &&
  BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper hook pretooluse < /tmp/bk-event.json
# For agy, save its event shape with conversationId and workspacePaths instead;
# check the same event, then invoke pretooluse --format agy.
PATH=/tmp/fake:$PATH BEEKEEPER_CONFIG=/tmp/bk.yaml CLAUDE_CODE_SESSION_ID="$SESSION_ID" \
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
jq -n --arg id "${SESSION_ID:?real started session ID required}" \
  '{hook_event_name:"PermissionRequest",session_id:$id,permission_mode:"acceptEdits",tool_name:"WebSearch"}' > /tmp/bk-permission.json
BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper hook check-scope < /tmp/bk-permission.json &&
  BEEKEEPER_CONFIG=/tmp/bk.yaml ./beekeeper hook permissionrequest < /tmp/bk-permission.json
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
document, the gate run from a script (so the hook does not rewrite it) as a Claude Code background
command against scratch state, and the harness's stop of that command mid-merge, which kills the
command's process tree. devctl runs on in its `beekeeper-merge-…` unit (`systemctl --user
list-units 'beekeeper-merge-*'`, its `/proc/<pid>/cgroup`); `watch --once` says nothing while it
runs and `MERGE RECORDED … whose gate … is gone` once it ended. Live, a background `devctl pr
merge` of a docs pull request whose command is stopped while it waits for the checks is logged
`merged` once the checks pass, and the lane is free.

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
`lanes`; a call whose devctl runs prints it within a poll and follows the same devctl on. The
test binary is not re-executed: `internal/platform/binary_linux_test.go` covers noticing the
replacement, and `cmd/gatereexec_test.go` the hand-over on both sides with a binary whose re-exec
fails.

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
short life. A scratch standby watch (`watch --notify --standby`) run inside `dbus-run-session`,
with `dbus-monitor` recording the `Notify` calls so nothing reaches the desktop, restarts the
scratch configuration's supervisor loop service once it stopped and says `SUPERVISOR LOOP DOWN`
once it stayed down past two minutes, then `SUPERVISOR LOOP BACK` once `supervisor loop` runs again;
no session is ever started for the supervisor. The spare's keep-awake and the crash hand-over need desktop sessions, never the live ones:
throwaway sessions titled `test: …` on the smallest model, made with `claude -p --session-id <id>`
in a scratch folder and imported with `claude://resume?session=<id>` (then
`claude://code/continue?session=<previous>` puts the window back). A `.claude/settings.local.json`
in that folder sets `BEEKEEPER_CONFIG` to the scratch configuration and allows `Bash(beekeeper *)`,
so their commands never touch the live state. `BEEKEEPER_PEER_TEST=<title> go test -run TestLive
./internal/peer/` sends one message to such a session from the command line and checks that an
absent name is unreachable. A scratch `watch --standby` with the spare in the scratch state keeps
one session awake past 35 idle minutes while an untouched one is dropped at 30; a stand-in
guide killed by its CLI's PID shows `GUIDE GONE` naming the spare and the spare's `guide start`.
Archive the sessions afterwards. `guide handover --prompt` on a copy of the live state (`cp -r` of
the state and lease directories into the scratch configuration) shows what a successor would get.

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

`alerts snapshot` groups text rows by severity, team, alert name, cluster and namespace;
rows with a namespace label append `namespace=<value>`. `alerts snapshot --json` returns
an object with `lines` (the text view, including PagerDuty) and `installations`. Each
installation carries `installation`, `reachable`, optional `why` and `noContext`, and
an `alerts` array with every visible alert's full `labels` map. Ignored, silenced,
inhibited and below-floor alerts follow the same filtering as the text view. This
replaces the previous JSON array of formatted strings; scripts reading that array
should read `lines` instead.

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

A long-running command's CPU is profiled where it runs: `BEEKEEPER_CPUPROFILE=<file>` writes the
process's CPU profile to the file until it exits (a watch, on SIGINT or SIGTERM), for
`go tool pprof -top -cum <binary> <file>`. Profile a watch against a copy of the state directory
(`stateDir` in a copy of the configuration, `BEEKEEPER_CONFIG`), so that it takes no line, note or
timer from the running watches.

## Layout

| Package | What it knows |
|---|---|
| `internal/platform` | The seam to the machine: `Machine` (memory, pressure, processes, the desktop and capped-run scopes, OOM kills), `Launcher` (detached units: agent, wake and merge units), `Capper` (the capped scope of `beekeeper run`), `Opener` (`claude://` links to the desktop app), `Notifier` (the D-Bus sender to `org.freedesktop.Notifications`, godbus, never `notify-send`, never an autolaunched bus). Commands call only these; the build tag picks the implementation. |
| `internal/proc` | The process table from `/proc`, or a copy of it in testdata: parents, children, environment, start time. |
| `internal/claude` | Sessions: CLI processes (desktop, background, headless, and the children a session starts), desktop session records, transcripts, git checkouts, what each acted on (its commands and tool calls, not the refs it only mentions) and which overlap, and how each has been doing (turns, tool calls and errors, tokens and cost, context), read from the same 512 KiB window of its transcript in one pass. |
| `internal/agy` | `agy` (Antigravity CLI) sessions: persistent FIFO inbox and `beekeeper agy-run` supervisor, `PreInvocation` mid-turn step injection with at-least-once inflight tracking, session discovery from `agy` processes and brain transcripts, `tail`/`follow`, transcript metrics and `spend`. |
| `internal/machine` | The Linux readers and parsers behind `platform.Machine` (memory, pressure, the desktop scope, OOM kill lines), disk, build slots, kind clusters, the model servers. |
| `internal/github` | The budget from rate-limit headers, the GraphQL limit and its refusal from a `rateLimit` query, the search limits from `/rate_limit` (`BEEKEEPER_GITHUB_API` points the probes at a test's server); a pull request's state from `gh pr view`. |
| `internal/lease` | Lease directories and the grant rule; a lab's claim-time `agentlab.yaml`, the keys a release finds changed and the change left for the next claim. |
| `internal/peer` | The command-line send to a running Claude session: one headless `claude -p` turn whose only tool is SendMessage, by the session's ListAgents name. It rests on Claude Code's undocumented peer messaging, so it is kept here with a live test. |
| `internal/state` | The shared state document (supervisor and its relay, the guide's role record (`Role`, the same shape the supervisor's flat fields read as through `SupervisorRole`), the relay due the watch reported, grants, holds, agents, notes, timers, session records, merges, the sessions `agents start` started with their mode) and the event log, under a file lock (`Peek` reads it without the lock, for the permission hook); `Log` appends the events that change no state (build runs) with a bounded wait for the lock. |
| `pkg/apis/beekeeper/v1alpha1` | The `beekeeper.giantswarm.io` kinds of `beekeeper serve`, little spec and lots of status: `Environment`, `MergeLane` and `Hold` cluster-scoped, `Note` and `RosterEntry` in the team's namespace `beekeeper-<team>`; queues capped at `MaxQueue`. `config/crd` embeds their CRDs. |
| `internal/state/kube` | The Kubernetes `state.Store`: the shared records of the state document (grants and release times as Environments, merges as MergeLanes, holds, notes, agents) as those resources. An update writes the one object it changes, or the Notes it changes one by one, under the resourceVersion it read and reruns on a lost race after a growing, jittered backoff, the Notes written before it kept, until a deadline (20s) past which it fails with `ErrContended`; an update of two objects other than Notes, or of anything the resources do not carry (the supervisor, timers, side files), is refused. `Claim` and `Release` set an Environment's holder by compare-and-swap, the loser told the holder. Events are Kubernetes Events on the object. |
| `internal/identity` | Who calls `beekeeper serve`: a Dex ID token checked against the issuer's JWKS and `serve.clientIDs`, the person its verified email (never `sub`), a member of `serve.organization` or one of `serve.organizations` (a refusal names the groups the token carries), the team of its group in `serve.teams`, a team's supervisor role its group in `serve.supervisors`; `identitytest` is the Dex stand-in of the tests. |
| `internal/feed` | The versioned shape of `beekeeper://feed` ([docs/feed.md](feed.md)): event ids, kinds, subject, actor, time and the line the watch prints. |
| `internal/mailbox` | The mailboxes of `beekeeper serve` in Postgres: one `messages` table and a dedupe table, migrated at start; at least once and in order, `Cap` per mailbox, expiry with a notice to the sender, `LISTEN`/`NOTIFY` on every change; `mailboxtest` gives a test a database of its own. |
| `internal/notify` | Desktop notifications: the kinds, urgencies and quiet hours and the ledger `notify.json` under `notify.lock` that makes each event one notification across watches and holds the quiet hours' ones; the sender is `platform.Notifier`. |
| `internal/alerts` | The installations' alerts: bounded `kubectl port-forward`s in their own process group, kept from tick to tick by the watch, the Alertmanager reading, the NEW/RESOLVED/FLAPPING lines with the severity floors and the flap damper, and the grouped snapshot (pure, tested against Alertmanager-shaped fixtures, recorded answers in `testdata/` and a fake `kubectl`), the baseline with the damper's records and its single owner, and recorded answers for `alerts replay`. |
| `internal/upgrade` | Cluster upgrades: the detection over an installation's Clusters, control planes and node pools (pure, tested against a real upgrade replayed from `testdata/prod/<phase>/`, stripped to the fields read), the bounded parallel `kubectl` reading with the events that name the release upgraded from, and the automatic `upgrade:<installation>/<namespace>/<cluster>` holds the merge gate and `lease claim` read. |
| `internal/guard` | The build guard: a capped run in a build slot with its `run.start`/`run.end` events, the PreToolUse hook's rewrite and refusals (a third lab, the person's folders, default-branch and force pushes, …) in both Claude Code (`--format claude`) and `agy` (`--format agy`) formats, and the PermissionRequest hook's decision. |
| `internal/runrecord` | The shape of a run's event-log records (`run.start` / `run.end` / `run.fallback`), written by `internal/guard` for a local run and `internal/remotebuild` for a builder pod's, and read by `run stats`: where the run ran, what it built, its duration, exit and failure tail. A leaf so both writers share it. |
| `internal/free` | What can be freed (dead sessions' dirs, throwaway temp dirs, orphaned workers) and what is only reported, as a report or the front end's TSV. |
| `internal/update` | The latest release, its signature check and the one-rename install. |
| `cmd` | The command line. The supervisor and the guide are one `role` type (`cmd/relay.go`): start, relay, relief, restart grace and relay due are written once and parameterised by the role's name, record and configuration; only the supervisor's start moves the grants. The guide's queue (`guideQueue`) holds the notes for `guide.person` (`noteFor`: a note's `For`, or an older note's `[for <person>]` text prefix, compared without case) and the waiting sessions, running ones from their desktop record and stopped ones from `claude.StoppedWaiting` (a record not parsed unless it holds `"blocked"`), without the guide's own session, an archived one or a test (`Session.Aside`, `Record.Aside`); its feed drops a note it said before that is still open but no longer in the queue without a line. |

## Automatic disk reclamation

### Cache policies

`disk.caches` lists the caches the disk step trims. Beekeeper's own caches are
its default entries: `go-build` (mode `go`, cap `doctor.goCacheMaxGiB`),
`sccache` (mode `sccache`, cap `disk.sccacheMaxGiB`) and `beekeeper-tmp`
(`stateDir/tmp`, mode `entries`, `olderThan: 1h`). An entry with a built-in's
name changes the fields it sets (`maxGiB`, `olderThan`, `path`); `mode: off`
drops it. Any other entry needs a `path` and a rule:

```yaml
disk:
  caches:
    - path: ~/.cache/some-app       # each direct child is removed whole
      olderThan: 7d                 # when nothing in it was written for a week
      regenerable: true             # under disk pressure: everything older than 1h
    - path: ~/.cache/pkg-manager
      mode: files                   # single files, least recently written first
      maxGiB: 2                     # until the cache fits two GiB
    - path: ~/.cache/*-dev          # a glob: each match is a cache root
      maxGiB: 1
    - path: ~/.cache/tool
      mode: command                 # the tool's own cleaner, which keeps its metadata
      command: tool cache prune     # words, no shell; runs in the cache root
      maxGiB: 4                     # over the budget, and under disk pressure
```

`path` is absolute or `~/…` and may be a glob; each match is a cache root. In
`entries` mode (the default) a root's direct children are the units, each
removed whole; in `files` mode its regular files, and directories a trim
empties go with them. A unit untouched for `olderThan` (a Go duration or whole
days, `7d`) goes; past `maxGiB` the least recently written go until the root
fits; a `regenerable` cache drops everything older than an hour under disk
pressure. In `command` mode beekeeper removes nothing itself: it runs the
tool's cleaner (at most ten minutes) once the root holds more than `maxGiB`,
and on every pressure run (without `maxGiB`, only those); the bytes it freed
are measured after it, and a dry run says `cleaner would run`. A root inside another policy's root is governed by its own policy
alone: the outer one neither counts nor removes the entry holding it. Whatever
a policy says, the step keeps:

- anything written within the last hour;
- a unit any process of the user references (cwd, open file, mapping); in
  `files` mode one reference anywhere in the root keeps the whole root;
- a unit holding a `.git` (a checkout, so possibly uncommitted or unpushed
  work); in `files` mode such a root is kept whole, and a root that is itself
  a checkout is never trimmed;
- a root that is a symbolic link, or that holds the home, the state directory,
  a configured checkout or a worktree root;
- everything while some process's references are unreadable.

The scan of a large cache takes a while, so before removing anything the
step reads the processes' references again, and right before it removes an
entry it inspects that entry again. In `files` mode it also rechecks the whole
root for checkouts after refreshing process references, before deleting files.
An entry a process opened meanwhile, one
written since the scan, or one that now holds a checkout stays. A file is
removed only if unchanged since the scan. The tools' own locks are only
known to their cleaners: a cache whose tool locks it belongs in `command`
mode.

`beekeeper disk prune` (or `--dry-run`) lists each cache root with what it
holds and would free; `--report` adds the largest entries of the user cache
directory no policy covers, with their last write, so the list can be
extended. A pressure run's `DISK PRUNE` line names the caches still holding
the most, and when the disk is still under the free-disk floor after it, the
largest directories (100 MiB or more) of the user cache directory no policy
covers, with their last write. The supervisor's "Disk remains low after
pruning" finding carries the same; it goes to the guide to file, never to
the person as a decision.

### The disk step

`disk.prune: auto` is the default. A long-running watch runs the disk step
hourly (`disk.every`) and within five minutes of `LOW DISK` or `DISK FILLING`.
`watch --once` reports disk pressure without deleting anything. Watches share
a lock and a last-run mark, so they do not prune the same data concurrently.
Every disk step prints one `DISK PRUNE` line with bytes freed, retained data
and the reasons for keeping it. `beekeeper disk prune` previews the step;
`beekeeper disk prune --apply` runs it explicitly.

Under disk pressure, the step also reclaims unused Docker images and Docker
build cache even when they are below the doctor size caps. Docker retains
containers, images tagged recently, active builds and imports, and lab lease
safeguards. Negative Docker caps disable the corresponding cleanup. The
pressure report includes Docker reclamation and blockers; persistent low disk
raises `GUIDE PROBLEM` with free space and the start floor.

On healthy scheduled runs, the step trims only caches over their caps:
`doctor.goCacheMaxGiB` (20 GiB) trims Go entries least recently used first
to three quarters of the cap; `disk.sccacheMaxGiB` (10 GiB) trims sccache's
local cache (`SCCACHE_DIR`, else `sccache` in the user's cache directory) the
same way, least recently used entries first. A negative cap disables its
scheduled cleanup. Under `LOW DISK` or `DISK FILLING`, both unused caches are
reclaimed immediately. An idle sccache server is stopped first, through its
own executable and server address (`sccache --stop-server`), so it counts
the trimmed cache afresh when a build starts it again; a server that does not
stop keeps the cache, and one that starts during the trim ends it.
No cache is reclaimed while builds use it. New
`beekeeper run` commands share a build-cache lock, including Rust builds and
runs with `GOCACHE=off`; Go's existing cache-use lock also protects older
runs and `cache clear go`. Unwrapped compiler processes and open files keep
caches intact. The module cache is never cleared by this step.

A direct `beekeeper run -- go build ./...` that exits 137 is retried once
with `-p=2` appended to `GOFLAGS` and `GOMAXPROCS=4`, reducing peak compiler
memory under the cap. The retry logs both bounds and preserves unrelated
`GOFLAGS`. Exit 137 is treated as a possible cap kill; no kernel journal
access is required. Other failures and commands are not retried. Shell
wrappers such as `sh -c 'go build ./...'` do not use this retry.

Finished worktrees under `doctor.worktreeRoots` and the state's `worktrees`
place are removed with `git worktree remove` after the existing six-hour
grace. A task is finished when its pull request merged or a successful
remote query confirms a previously published branch is gone. A branch
that never tracked a remote branch and has no pull request stays. Open
pull requests and those closed within the grace stay, including harness
worktrees. For unmerged tasks, publication must be verified against
current remote heads, not stale local tracking refs or a closed pull
request's head. A network failure keeps the worktree.
Running sessions, desktop sessions active in the last seven days, active or
parked roster agents, process references, locks, uncommitted changes and
unpushed commits prevent removal. Finished workers holding work lose only
regenerable build output. Unlike the legacy worktree doctor, the automatic
disk step keeps unfinished tasks' build output.

The watch also finds `<checkout>/.claude/worktrees` under `disk.checkouts`,
the hook scope's places and checkouts known from harness records and
transcripts. An unused harness worktree can be removed after the grace
when it has no uncommitted or unpublished work. Main checkouts are never
removed. A dangling worktree is moved aside, never recursively deleted.

Additional build directory names can be listed in `disk.buildDirs`.
External build output needs an explicit owner, using absolute paths to the output and owning worktree:

```yaml
disk:
  prune: auto
  every: 1h
  buildDirs: [generated-output]
  buildOutputs:
    - path: /scratch/build-output/target
      worktree: /scratch/worktrees/app/feature
```

An external output directory is disposable only after its owner's task
ends, and only while no process references it. If it must stay, its owner
stays too so a later step can retry. Never configure source or shared
persistent data as build output. Build output is renamed into the state's
`trash` directory on the same filesystem. A move does not count as freed
space; output on another filesystem is retained. Trash retention begins
when output is moved, and pressure runs may reclaim it immediately once
no process references it.

A run removes its own temporary directory at exit, including read-only
build files, and reports a cleanup failure. Disk pruning also reclaims run
tmp entries unchanged for one hour, and trash entries unchanged for one
hour. Disk pressure bypasses trash retention, while tmp retains its
one-hour inactivity window. The state's `trash` directory is always
inspected. `disk.trashDirs` adds other trash places; its default also
includes the XDG state root's `trash` directory.
Recent descendants, open files, working directories, executable mappings
and build/temp environment references keep entries alive. Symbolic links
and directories containing checkout metadata are kept. An unreadable
process root stops the disk step. A process with unreadable dumpable
references or unknown ownership does not: the step it gates (caches, tmp,
one worktree, trash) keeps its candidates as `process references unknown`,
the `DISK PRUNE` line names the process, and the next step reads the
processes again. An exited zombie holds no references and is skipped; a
zombie thread-group leader is read through its live threads
(`/proc/<pid>/task/<tid>`). Expected kernel permission denials on
non-dumpable helpers or helpers with different credentials or capabilities
do not stop pruning; their readable references still protect data. A
process of the person's uid the kernel refuses (EACCES) in another pid
namespace (`NSpid` lists more than one id) or user namespace (its `uid_map`
differs) is a container's, a kind pod's pause container with the pod's
`fsGroup` for one: its paths name the container's filesystem, so it is
skipped and the `DISK PRUNE` line counts it (`skipped N unreadable processes
of other namespaces`); refused in the person's own namespaces it stays
unknown. Ownership
of ptrace-gated child entries detects changes in dumpability even when the
process directory still has its original owner.

`beekeeper doctor` uses this automatic policy too, including its dry run.
`disk.prune: ask` disables this disk step and retains the pre-existing
on-demand commands and the doctor's Go-cache and worktree trims. Tmpfs
freeing (`doctor.freeEvery`) and Docker reclamation remain independent
in both modes.

### State holders

The state home (`disk.holders.root`, default `~/.local/state`) collects
holders no disk step owns: a task's `GOCACHE` or `CARGO_TARGET_DIR`, its
scratch named after its issue, saved release-candidate binaries, worktrees
moved aside. `beekeeper disk holders` lists each entry of the root, and
each entry of its aside directories (`disk.holders.asideDirs`, default
`aside` and `moved-aside`), largest first, with its size, when anything in
it was last written, its owner and whether it is eligible for removal.
`--json` has every holder; `--min-mib` (100) hides the small ones.

`beekeeper disk usage` is the wider view of what holds the disk: beekeeper's
state by subdirectory, the user cache by program and every worktree, each with
its owner. Process write counters name who wrote, not whose data stayed, so a
reclaim starts from this view and from the `fastest-growing path` the watch adds
to `DISK FILLING`. The watch reads the same places every two minutes; each root
has its own entry budget and each directory a fair share of it, so a large state
directory cannot leave the cache unmeasured.

The owner is read from the name: a task number of three digits or more
(`ao1011`, `go-build-153`) names the roster agent or session record whose
task or issue is that issue or pull request number, open tasks first; a name without one is its
program's (`~/.local/state/<program>`), and a number nobody mentions is
`unowned`. A holder is eligible once nothing in it was written for
`disk.holders.staleAfter` (72h; an aside entry: its retention window
`disk.holders.asideRetention`, 168h), no running process uses it, its task
is not open, it holds no git checkout outside an aside directory, and it
is neither beekeeper's own state nor named in `disk.holders.keep` (model
weights, say).

Nothing is removed: the listing tells a person or a worker what to
reclaim. Every `disk.holders.every` (6h) one watch lists the holders and
says `STATE HOLDERS` with the largest eligible ones while the eligible
holders take `disk.holders.warnGiB` (10) or more; one that holds a
checkout is only counted there, its work to be checked first. While a
process cannot be read, no holder is judged eligible and the watch says
`STATE HOLDERS unknown` with the reason. A negative `warnGiB` turns the
look off; a negative duration is refused. "Unused" means not written: a
reader that never writes (a model server reading its weights) does not
count, so such data goes into `disk.holders.keep`.

```yaml
disk:
  holders:
    keep: [hf-cache]
    staleAfter: 72h
    asideRetention: 168h
    warnGiB: 10
```

## Central note closure audit

A central Note closes only with a recognised close event (`answer.recorded`,
`note.defaulted`, `note.overtaken` or `note.done`). Its status stores that audit
line atomically with the outcome. Answered Notes also retain the verbatim answer
in `status.answer`, the caller's verified email in `status.answeredBy`, and the
answer time in `status.answeredAt`. Closed Notes remain as history, outside the
open notes and the 24-hour default-answer window, for 30 days after closure.
Note-only updates, including the periodic default tick, collect older closed
records; unrelated object updates leave collection to that tick. Cleanup first
preserves the note counter, so expired numbers are never issued again.
`beekeeper log` includes the retained line even after Kubernetes Events expire
or an Event write fails. Due decisions with legacy passive or empty defaults also close and retain the
24-hour late-answer window. Filing a new decision still requires an action.
An answer after the default-answer window explicitly
reports that the note was closed by its default. Folding a new question into an
open note leaves the existing note open and logs `note.folded`.

The original trigger of an already missing note cannot be recovered once its
old logs and Events have expired. Retaining the close record makes subsequent
closures attributable without relying on that retention window.


The supervisor loop executes a reviewed merge through `beekeeper gate --reviewed-head <sha>`.
The gate keeps its queue, holds, readiness, review and settlement checks; its mutation child
uses `gh pr merge --match-head-commit <sha> --delete-branch`. `merge.methods` maps repository
names to `squash` (the default), `rebase`, or `merge`. The coordinator never passes
`--admin`, which bypasses the required server-side merge queue.
A refusal that names the base branch policy and suggests `--admin` is a configuration
refusal, including when the merge state is `BEHIND`. The loop reports it once per
reviewed head, naming `merge.admin`, and retries on the next round without a worker
wake, branch repair or another review. This never authorizes bypassing the server-side
merge queue: the corrected policy must still admit its required checks. A loop that finds
a branch repair an older loop requested for such a refusal closes it on start
(`supervisor.unstage` for the repair stage, `supervisor.unclaim` for the go it completed),
so the unchanged approved head rejoins the merge lane; any other repair stage stands.

After approval only the merge moves the head. A branch that is only behind its base merges
at the reviewed head as it stands: GitHub merges it onto the current base, with no branch
update and no second CI run. Only a base that requires up-to-date branches refuses that
merge (GitHub's merge state `BEHIND`). Unless the repository is in `merge.admin`, the
mutation child updates the branch itself, once its authority, stop and hold checks pass:
GitHub's update-branch
merges the base into it, pinned to the reviewed head, and the child waits up to the squash
timeout for the new head's checks (every check the reviewed head had, all green), confirms
that the approval covers the new head (`github.ApprovalCovers`: it only merges the base),
checks the stop and holds again and merges that head. The loop accepts a merged head the
approval covers; its planner gives the go for such a head without a fresh park. A rebase
merge method cannot take a base merge, so its branch behind such a base, a conflicting or
draft branch,
red checks on the updated head or an updated head the approval does not cover go back to the
worker for repair and another green published head. A restart reads GitHub before repeating
a mutation whose completion record was lost. Successful coordinator merge actions are `supervisor.merge`,
not worker merge grants. Tooling failures remain eligible for retry with failure backoff;
the configuration refusal above retries each round. Neither grants a direct worker merge
around queue validation. No person is asked whether to merge: the loop merges after its
reviews and their fixes, and `note add` refuses a decision that asks whether to merge.
For an SSH worker's fixes, the loop retains the authenticated independent review result
and its original head, author, verdict and body digest. The loop records its exact-head
authorization before the executor accepts the new green head without another approval
round; a pending ownership claim alone is insufficient. Worker merge grants continue
to require approval of their head; an empty grant ID alone confers no loop authority.
An already queued or running gate is observed without submitting
another merge. Gate queue and policy refusals never become branch repair requests from
words in their messages; only the pinned mutation child's structured result can request repair.

On auto-release branches, the pinned child follows its merge with `devctl release wait --pr`
and saves the release verdict in its durable result. Lane settlement retains the confirmed
release and roll target, a no-release merge frees its lane, and a candidate or no-release
merge lifts the devctl tool-release hold. An unreadable release result remains explicitly
unconfirmed. The wait never repeats the merge.

A review park declares remaining work with `--post-merge "<release, rollout or proof>"`.
Omitting it on a new park declares that the task ends at merge: the loop records the final
report and closes the task without waking its worker. Older parks retain their existing
release-containment behavior. The standby leaves new review parks to the loop, avoiding a
second wake when a review or merge arrives. Only rostered review parks carry this declaration;
plain waits and off-roster sessions keep the normal watcher lifecycle. A closed pull request,
or a merged pull request whose served issue remains open or unreadable, wakes its worker to
continue or verify the task rather than leaving it parked.

An ended worker's saved review park still lets the loop request a fixer when that
parked head conflicts with its base or fails CI. The saved park alone does not
keep the worker's board item served after the worker ends. This recovery requires
a tracked pull and saved review ownership; it does not discover every open pull
or recover every ownerless In Progress item. The broader 15-minute orphan sweep
remains tracked in https://github.com/giantswarm/beekeeper/issues/2127, including
pulls without saved review metadata and stale serve records from relieved or
off-roster sessions.

The guide defaults to a 200k relay point. When a feed line arrives after an hour
of guide inactivity, context above half that point also emits `GUIDE RELAY DUE`
before the expired cache is warmed again. Unknown activity times and active
subagents do not count as idle. The supervisor loop is a service with no model
context; retired supervisor-session relay settings remain ignored.

The guide successor prompt references its skill or configured instruction file
and the desk's standing file once. Queue rows are shortened; `guide queue --full`
reads them in full, and `handover --full` retains live details and recent history.
`TestGuidePromptBudget` exercises a typical desk with 40 verbose decisions and
a large standing file, using a conservative 25,000-byte prompt budget (no more
than 25,000 byte-level tokens). The role skill and project instructions loaded
by the harness remain outside the generated prompt's budget.

## Task kinds

`agents.kinds` is a list of `{name, match, maxBusy, minInterval, dailySpend}`:
`match` is a regexp over the agent's name, and every kind whose regexp matches
holds a start, so a filer kind and a meta kind over filers and scans cap both.

- `maxBusy` (0: none) counts the kind's busy agents as capacity counts them
  (parked, waiting and idle agents hold no slot).
- `minInterval` (0: none) is measured from the kind's last recorded start
  (`state.starts`, kept 30 days), done or not.
- `dailySpend` (USD, 0: none) is the day's figure at which the watch prints
  `SPEND <kind>` once; the mark is kept in the watch's seen file, so a restarted
  watch does not repeat it. The watch sums the day every 10 minutes off the poll.

`beekeeper capacity` prints `no start <kind>: <reason>` after the verdict (and
`kindBlocks` in `--json`). `agents start` checks the kinds before the start gate
and again under `kinds.lock` in the state directory, which it holds until the
start is recorded: concurrent starts of a kind are admitted one at a time, for
every harness and for `--host` starts. A refusal is exit 3; `--force` does not
pass a kind's cap. Hand-overs and role successors replace an agent and are not
held.

`beekeeper sessions --by kind [--day 2006-01-02] [--tz <zone>]` prints the day's
starts, sessions, hours (each session's first to last request of the day) and
spend per kind, priced from the whole transcripts as `beekeeper spend` prices
them. Spend is attributed by the session id a Claude Code transcript's path
names; transcripts of no recorded start of a kind's agent, or of harnesses whose
files name no session beekeeper recorded (omp, agy, codex), are summed as "of no
kind".

## Task model policy

`agents.models` is an ordered list of `{match: <name regexp>, model: <selector>}`,
optionally restricted by `kind`. A start's `--kind` defaults to `worker`, or
`review` with `--review`. The first matching rule with a nonempty model selects it before
provider capacity is evaluated. `--model` overrides the rules. Without a match,
Claude uses `agents.model`, Codex uses `codex.model`, and omp uses `omp.model`;
with capacity choices and no explicit model, the capacity pick supplies both
harness and model. `agents.requireModel: true` refuses an unresolved model with
exit 3, including dry runs, rather than allowing a harness default.
Rules without a model are skipped for model selection, allowing independent
settings to match without masking a later model rule or the fallback.

`guide.model` selects the model for both relayed and standby-started successors,
ahead of the previous holder's model. Without it the holder's model is retained,
then name rules and `agents.model` supply a fallback. The supervisor is a service
loop and starts no supervisor model session; its workers and reviewers use the
same start policy as manual starts.

Each `capacity.choices` entry may carry a desk-maintained `class` (nonnegative
capability rank), `cost` (positive relative cost), `family`, `kinds` (allowed
kinds; empty allows all, except that a choice on a self-hosted provider takes
the `worker` kind, code changes, only when its `kinds` list it), and `reviewReserve` (0–100 percent of the weekly
window kept for reviews). `capacity.taskPolicies.<kind>.minClass` and `maxCost`
constrain the choices. A positive maximum rejects unknown costs. Class 0 means
unclassified. No changing provider price or capability claim is shipped.

`capacity --kind audit` and `capacity pick --kind audit --json` use the same
policy as `agents start --kind audit`. Both capacity commands and starts accept
`--author-model <configured selector>` with a review to exclude that model's
family; unknown author or candidate families refuse selection.
`capacity.taskPolicies.review.differentFamily: true` requires this for every
review. The supervisor loop supplies the author's recorded model automatically;
manual reviews must supply `--author-model`. Unclassified explicit targets
cannot bypass a configured capability, cost or family requirement.

Review reserve uses the choice's usage bucket, or its provider's weekly window. It adds a stop
for non-review tasks; reviews still obey provider window and pace limits.
Provider room and weekly pace are decided first, with lower positive configured
cost breaking equal pace between subscription choices. Configured order breaks
remaining ties and keeps self-hosted choices as fallbacks.

Capacity JSON includes `taskKind`, `decision`, and every rejected candidate's
reason. Successful starts log the kind, harness, model and choice under
`agents.start`; the roster retains the model for comparing outcomes. Explicit
models select a target but cannot make an automatic pick ignore a model-policy
rejection, even with `--force`.

## Worktree orphan cleanup

The doctor stops reparented process trees left in linked worktrees by known local
workers that have left the roster. It sends SIGTERM, then SIGKILL after
two seconds, checking the process start time, user and worktree before each
signal. Each targeted PID and masked command is recorded as `agents.archive`.
`doctor --dry-run` reports candidates without signalling; `ps` marks the same
candidates `[orphan: <worktree>]` (JSON: `orphan`, `worktree`), beside their age.
A roster entry, including a done worker awaiting removal, or a live CLI protects
its worktree. Main checkouts, arbitrary directories, unknown starts and remote
executions are excluded. Inherited worker IDs also identify servers whose tool
shell changed into a different worktree after the session started. Cleanup is
retried by later doctor runs if a live CLI still protects a finished worktree.

`TestStopWorktreeOrphansLive` starts an adopted, TERM-ignoring server in a scratch
worktree and proves dry-run preservation, escalation and archive logging.
`TestWorktreeOrphans` covers shared worktrees, rostered/done sessions, live CLIs,
remote starts, older processes and changed working directories.

Removed manually registered workers retain their identity and registration time
in `endedWorkers`, so their inherited session ID also identifies leftovers.
This cleanup history grants no permissions to the start or permission hooks.
Ownership and roster protection both use Claude session IDs or normalized
Codex, omp and agy host-session IDs; a hosted harness takes precedence over
a launching Claude session inherited in the same environment.

## Session effort

An `agents.models` rule may carry `effort: low|medium|high|xhigh|max` beside
or instead of its model; the first matching rule (same `match` and `kind`
semantics as the model) with a nonempty effort selects it. Rules without an
effort are skipped for effort, so model-only and effort-only rules compose.
`agents start --effort <level>` overrides the rules, independently of
`--model`. Without either, no effort option is sent and the harness keeps
its own default. The `agents.start` event names the effort, or
`harness default`.

```yaml
agents:
  models:
    - {match: "^(scan|file|draft|close)", effort: high}
    - {match: ".", kind: review, model: opus, effort: xhigh}
```

Claude Code receives `--effort`, omp receives `--thinking`, and Codex receives
`-c 'model_reasoning_effort="<level>"'` on both its first and resumed turns.
The selected model must support the level. agy has no session effort option:
an effort from the flag or a rule is refused instead of silently ignored, on
local and SSH starts alike, also when `--harness auto` chooses agy. SSH Claude,
Codex and omp starts carry effort in the owned execution request and reuse it
for later turns.

A Claude desktop-mode start with an effort runs its task headless, with its
normal tool profile and sidebar import. Its wakes stay headless on that
effort, since a desktop-spawned CLI cannot be given this override.
Background starts keep Remote Control. Start records persist effort for
wakes and handovers; no user settings file is changed.

`guide.effort` selects the guide's session effort, ahead of the rules. The
supervisor is a loop, so `supervisor.effort` is the default it passes to the
sessions it starts; a matching rule's effort wins over it. Both are optional.

### Owned issue monitor

`beekeeper issues poll` reconciles all open issues in repositories declared for
`team-bumblebee` and `team-planeteers` by `giantswarm/github/repositories`.
`--ownership owner/repo/directory` selects a different ownership record, never a
repository allowlist. Run the command periodically from a trusted service with
GitHub issue-write and team-membership-read permissions. Configure the two team
Slack incoming webhooks in `BEEKEEPER_BUMBLEBEE_WEBHOOK` and
`BEEKEEPER_PLANETEERS_WEBHOOK`; `--channels` maps team names to different environment
variable names. Webhooks and Slack responses are never printed. Delivery goes only
to `https://hooks.slack.com/`: the environment variable selects the webhook's
`/services/...` path, and any other destination is refused.

The monitor requests metadata only, never issue bodies or comments. Every poll
checks active membership of the employees team live, including when an author
was previously an employee. API failures classify human authors as external.
Bots have a separate class. External issues receive `beekeeper-triage` and a plain
Slack message containing only repository, title, author and canonical link.
External titles are never persisted, included in reports or sent to workers.
A person must re-file worthwhile external work in their own words.

`issues queue` shows employee issue pointers with source `issue`. The supervisor
consumes these before its project board, checks open state and membership again,
and removes a pointer after starting its worker. `issues contributors` lists
external logins per repository. `issues cleanup` groups the first scan's pointers
by stale (180 days), duplicate label, overtaken label, or still-valid candidate.
These are human review candidates: an overtaken label requires confirmation of
the merged PR; the monitor does not infer semantic duplication or validity from
untrusted prose. The cleanup snapshot is retained after subsequent scans.
Unassigned bot issues are reported for cleanup; employee issues are queued;
assigned employee/bot issues are owned; external issues always require triage.

State and notification receipts live under `stateDir/issue-monitor`, serialized
with a file lock and replaced atomically. Keep this directory across restarts.
Successful notifications are checkpointed individually, so ordinary repeat polls
and restarts do not repost them. An incoming webhook has no transactional
idempotency receipt: a process crash after Slack accepts a message but before
its local checkpoint can produce a duplicate. A failed poll should be retried;
it does not claim that unreconciled repositories are covered.

### Session worktree placement

Configure clone discovery and session placement with:

```yaml
projects:
  dir: ~/projects
  worktrees: [~/projects/worktrees]
```

`projects.worktrees` supplies ordered external roots; session hooks use the first root.
Run `beekeeper install` to register Claude Code's `WorktreeCreate` and
`WorktreeRemove` hooks. Creation uses
`<projects.worktrees>/<repo>/worktree-<session-name>`, starting at the
session's HEAD. Existing branches or paths cause an error rather than reuse.
The worktree hook owns creation even outside the normal tool-hook scope,
because Claude's creation hook replaces its default worktree implementation.
The Bash guard rejects nested `.claude/worktrees` targets, including targets
reached through symlinks, and names the configured external root.

`beekeeper free --only worktrees` reports linked worktrees nested in clones
under `projects.dir`. Add `--apply` to remove clean trees integrated into the
default branch and move other trees into the external root. Git's merge
simulation recognizes squash merges; conflicts or uncertain results retain
the tree. The default branch is origin's default ref, falling back to local
main or master. Refresh remote refs before cleanup when they are stale.
Branches are always retained, including squash-merged branches. Locked trees
and destination collisions are left for manual resolution. Migration refuses
live worktrees and stops if process or roster information cannot be read.
The removal hook similarly retains dirty or unmerged work and never forces
removal.

### Envtest assets for local runs

`beekeeper run --local -- go test ./path/to/package` automatically provisions
Kubernetes 1.37.0 envtest binaries using `setup-envtest` (release-0.25) into
`<stateDir>/envtest` and exports `KUBEBUILDER_ASSETS` to the command. Local
runs reuse the cached binaries; concurrent first runs share a setup lock.
An explicit `KUBEBUILDER_ASSETS` takes precedence. First-time setup requires
Go and network access; failures stop the run with a setup error. Commands
that only print help or a version, and trivial commands, skip provisioning.
Remote builder images must supply their own envtest assets and environment;
use `--local` for tests that need the managed assets.

## Supervisor workflow regression

Issue #2255's `test-loop-workflow` CircleCI job runs on every branch and PR,
without path filters, and on release tags. Run it locally with
`make test-loop-workflow` (Go and Git are required). It builds the candidate and
runs the public `supervisor loop --once`, `board next` and worker-start commands
in subprocesses. The test executable supplies only the external GitHub CLI and
Git trace wrapper; a local HTTP adapter supplies the budget API. The planner,
`readLoopPull`/`parseLoopPull`, merge-result validation and gate are production
code. Raw pull responses include base, head, branch, checks and reviews; raw
project responses include metadata and a selectable issue. The merge adapter
actually squashes the branch and pushes a local bare origin. Success requires
that origin's base to match the reported merge commit, exactly one mutation,
and durable completion of the saved review park on the next loop pass.

This boundary fixture starts with a durable published review park and review
round, rather than replacing parsed pulls with in-memory planner objects. The
full worker/reviewer lifecycle harness is tracked separately in #2254. A failed
public worker start is also exercised with its worker executable unavailable: it must not
create a registration or merge proof. The fixture caps starts at zero to avoid
any dependence on the runner's available capacity or subscriptions.

The matrix covers the supported no-checkout policy, non-Go checkouts, current
and diverged Go checkouts, a combined Go tree that fails while each branch's
changes are valid, a head moving after planning, red checks, missing base,
unavailable validation with no server queue, an unreadable queue, budget hold
and failed start. A separate control builds a candidate through a Go overlay
that removes the parsed base assignment (the historical regression); the same
complete response must then fail before merge. Unknown GitHub calls fail the
suite even if the application handles their error.

Every subprocess gets an explicit environment, private configuration/state,
file-only Git transport, offline module resolution and a small command PATH.
No account credentials or production state are inherited. The fixture uses
`LANG=C.UTF-8`; builds have a five-minute deadline, scenarios a ninety-second
deadline, Go tests a twelve-minute deadline and the CI command a fifteen-minute
outer deadline. There is no opt-in environment switch or skip path.

On failures, the job stores sanitized command/event traces, command output and
Git refs (both a listing and a bundle of the fixture origin) under the
`loop-workflow` artifacts. `BEEKEEPER_TEST_LOOP_ARTIFACTS`
selects the local artifact directory. Only known fixture files are retained;
the fixture root is replaced by `<fixture>` and the configured identity is
synthetic. Credentials and the runner environment are never uploaded.

Required repository configuration prerequisite: after its first pipeline,
add **`ci/circleci: test-loop-workflow`** to the required status checks in the
branch protection/ruleset for main and protected release branches, using the
CircleCI check's exact name as reported on the PR and selecting CircleCI as its
source where supported. Keep this check required even for CI-only changes.
Adding YAML does not change GitHub branch protection; a repository administrator
must configure that rule before treating the check as a merge requirement.

Release publication is also guarded without changing generated workflows:
`make test` depends on the same uncached `test-loop-workflow` target. The
generated `go-build` job invokes `make test`, and release asset/image publishing
requires `go-build` (chart publication depends on the image publication).
Consequently a workflow regression fails the release prerequisite, including
on tags. The dedicated job gives PRs a stable required-check name and retained
failure artifacts. Do not add path filters or a skip-on-missing-tool fallback.

## State/event recovery and guide continuity

`FileStore.Update` commits state and an `EventOutbox` together using an atomic
rename after syncing the temporary file. The directory is synced after renames.
The outbox records the event-log offset and exact serialized append. Recovery
accepts only that append's existing prefix, writes its missing suffix, syncs and
closes the log, then clears the outbox atomically. Open failures happen before
state commits. Later I/O failures retain the committed append in state; callers
must inspect/recover after an error rather than assume the callback was undone.
`Read`, `Update`, `Log` and `Record` recover under the exclusive state lock;
`Peek` stays nonblocking and exposes any retained outbox. An unrelated log edit
refuses recovery without deleting evidence.

Lease renewal uses `state.commit.lock`, a short transaction lock shared by all
state commits and outbox recovery. It reads and changes the authoritative
`state.json` under that lock, without waiting for desk readers or slow
`Update` callbacks under `state.lock`. An ordinary update carries forward any
newer lease for the same generation and registration at commit, so it cannot
undo a concurrent renewal. Stop, revocation and registration changes linearize
at that same commit boundary. `Exclusive` and binary recovery hold both locks.
The lock order is always desk lock then commit lock; renewal never acquires the
desk lock or appends events. Commit-lock acquisition is bounded to five seconds
and callback deadlines remain in force. Lease lifetime and replay rules are
unchanged.

This lock protocol requires restarting **all coordinator state writers** when
installing this change; a running older writer does not take the commit lock.
Do not run a mixed-version coordinator or roll back its binary with writers
still running. The host journal retains the last renewal failure's time,
elapsed duration, stage, fixed failure category, last sequence and failure
count. It retains no callback error text, SSH output, control keys or routes;
a successful retry does not erase the earlier failure. Failure diagnostics
make one nonblocking journal-lock attempt even when the callback context has
expired; a busy journal may drop the diagnostic rather than delay expiry.

For the live proof, park a remote worker, retain its execution/generation and
thread identities, exercise concurrent desk reads and updates for longer than
90 seconds, and verify advancing lease sequences without a stop request.
Resume it and verify the same thread. Record only those identities and lease
metadata, then take the required alert snapshot; never attach raw transcripts
or host routes. Deterministic tests cover the lock overlap separately from this
post-release proof.

The outbox fault-injection suite covers failed and partial writes, sync and close
errors, both sides of the append, reopen and duplicate prevention. The actual
update path also exercises a failing event device and recovery after replacement.
This establishes recoverable state/event commits within the filesystem's sync and
rename guarantees, not protection from storage corruption or hardware that lies
about durability.

The independent desk watch retains new unreported findings as `GuideWork`,
notifies the current guide and retries failed delivery. It respects the global
stop and uses the standby watch only when the supervisor's watch is absent.
Guide-feed heartbeats are persisted at most every 30 seconds; after the greater
of two minutes or three watch intervals without one, the independent watch emits
and delivers a lapse warning. Work belongs to the role, so relaying a guide does
not discard tasks, goals or restoration times. The guide Stop hook requires a per-turn `guide work check` and flags due
non-goal work; future work has an explicit next action time. A new user prompt
invalidates the check, as do work mutations and newly retained findings. The long transcript
tail test checks that large requests expand beyond the initial 2 MiB window.
