# Operating SSH execution hosts

Managed, headless Claude Code sessions run on an explicitly selected SSH host.
Each execution gets its own linked worktree and branch backed by an isolated
bare repository copy, an owned user service,
a conversation identifier, and one coordinator roster registration. Omitting
`--host`, or selecting `--host local`, keeps the local launch path.

Read the [design](ssh-execution-hosts.md) for the ownership and failure contract.
Remote wake and resume use durable message receipts as described below. New
executions require expiring coordinator leases and live supervisor merge grants.
The component command below is the supported remote merge path; local approvals
and raw merge commands do not authorize an SSH execution.

For remote starts, `--force` overrides coordinator capacity only. Execution-host
admission still enforces memory, swap, load, disk, build slots (with `--builds`)
and `maxBusy`. The CLI explains these limits before remote preparation; a
passing host check does not reserve room or guarantee that preparation will
succeed. Authentication, identity, version and policy checks remain required.
If the host refuses preparation before acknowledging a start and inspection
confirms no execution exists, the coordinator releases the reservation: no
worker remains registered and no host or desk slot is held. Start anew after
resolving the host's refusal.

## Retained execution routes

New reservations persist the approved SSH destination, executable and configuration
path with the execution. While that record is retained, catalog operations refuse
a different endpoint or host identity for its selector (or a different endpoint
under another selector for the same physical host). Restore the original catalog
route to inspect, stop, reconcile or recover those executions. Policy updates do
not change this route pin. Exited records retain the protection because their
journals and transcripts can still be needed. Capacity remains counted by physical
host identity, independent of selectors and endpoints.

Records created before endpoint pins existed have no reconstructible original
route and retain their previous catalog behavior. Keep their original catalog
routes until they have been reconciled. The pin identifies the configuration
path, not its contents: do not repoint an existing host configuration path to a
new execution-state directory while its journals are retained.

## Provision the components independently

Install the same beekeeper version on the coordinator and execution host. The
host installation may sit at any absolute path, including a per-user one; the
pinned configuration and prepared policy bind that exact executable. A managed
worker calls its component by bare `beekeeper`, and the runner places the
bound installation's directory first on the harness PATH, so a system-wide
installation is not required. Install the prepared executable under the name
`beekeeper`: `agents hosts check` refuses a host where the bare command does
not resolve to the very executable the prepared policy binds (missing, not
executable, or shadowed by a stale sibling), because that worker could not
report or park. The host needs Linux with a working user service manager,
Git, and a pinned Claude Code version
in the supported family (`>= 2.1.268, < 3.0.0`), a pinned Codex CLI (see
"Codex on an execution host"), or both. Provision its subscription login
there independently. `agents hosts check` reads version and authentication
metadata; it makes no model request. Its `github` field answers for a worker's
reads: without `github.workerApp` on the coordinator it follows the host's own
`gh` login (`githubLogin`), and a logged-out host fails the check and every
start, naming the reason. Starting a session never copies a provider
credential store or forwards an authentication agent.

Provision the worker toolchain the same way on every host, and pin it under
`tools`: the GitHub CLI (`gh`) everywhere, and Go for each served repository
whose source checkout has a `go.mod`, at least at the version its `toolchain`
line (or, without one, its `go` line) names. Each pin is an absolute executable
named `go` or `gh` and the version it reports (`go version` with
`GOTOOLCHAIN=local`, `gh --version`). `agents hosts check`, and every start,
refuses a host whose required tool is unpinned, reports another version than its
pin, or is older than a served `go.mod` asks for, naming each one; a passing
check lists the pinned versions under `tools`. The runner puts each tool's
directory on the worker's PATH right after the component's, so a worker runs
`go` and `gh` by bare name. The tools are part of the pinned policy: changing
one, or raising Go when a `go.mod` moves to a newer toolchain, needs a new
`prepare` and digest.

Provision SSH authentication and known-host keys separately in both directions:
the coordinator calls the host's component, and the managed worker calls the
coordinator's component for controls. SSH runs with batch mode and strict host
key checking, without agent, X11, environment, or port forwarding. A missing key
or changed host key fails the operation rather than prompting. Prefer dedicated
SSH routes restricted to the component RPC commands; the configured coordinator
callback route must carry the execution host identity with `--host`.

All example paths below are operator-owned absolute paths. Source checkouts,
managed policy state, and workspace roots must be disjoint. Managed directories
must not resolve through symlinks. Existing uncommitted source work is preserved;
the component clones and pins a committed ref from the host-local source.

Begin with a coordinator configuration that has `stateDir` and an explicit
`identity.host`, without a host catalog entry yet:

```yaml
stateDir: /srv/coordinator/state
identity:
  host: coordinator
```

Create its signing key once and obtain only the public half:

```sh
beekeeper --config /etc/beekeeper/coordinator.yaml agents hosts key
```

Configure the execution host using that public key. Replace the public-key
placeholder and pin the version actually installed before validating it:

```yaml
stateDir: /srv/beekeeper/state
github:
  allowedOrgs: [example]
agents:
  execution:
    identity: host-a
    workspaceRoot: /srv/beekeeper/workspaces
    rulesFile: /etc/beekeeper/worker-rules.md
    maxBusy: 2
    repositories:
      example/repository: /srv/checkouts/repository
    harnesses:
      claude:
        executable: /usr/local/bin/claude
        version: 2.1.290
        configDir: /srv/beekeeper/claude
    tools:
      go:
        executable: /srv/tools/go1.27.3/bin/go
        version: 1.27.3
      gh:
        executable: /usr/bin/gh
        version: 2.83.0
    coordinators:
      coordinator:
        publicKey: <coordinator-public-key>
        callback:
          ssh: coordinator.example
          executable: /usr/local/bin/beekeeper
          config: /etc/beekeeper/coordinator.yaml
```

The source checkout's origin must identify `example/repository` on GitHub.
`rulesFile` contains host-specific worker instructions, not authentication
configuration. Known credential files and credential-shaped content are refused.
The prepared policy binds those instructions, embedded worker rules, hooks,
binary version, and host security configuration. A host requiring an OS sandbox
is refused by this adapter; hooks do not provide that sandbox.

Export and inspect the host policy manifest, then install that exact manifest
using component commands on the execution host:

```sh
beekeeper --config /etc/beekeeper/host.yaml agents hosts bundle > /srv/beekeeper/bundle.json
beekeeper --config /etc/beekeeper/host.yaml agents hosts prepare --bundle /srv/beekeeper/bundle.json
```

`prepare` prints the policy digest. The coordinator records the digest it
pins in its own state, not in its configuration: once the host runs a release
with `agents hosts self-update`, run on the coordinator

```sh
beekeeper agents hosts update host-a
```

which has the host install the coordinator's release (below) and pins the
digest the host prepared once `hosts check` passes with it. The coordinator
configuration names the host and its limits only:

```yaml
agents:
  hosts:
    host-a:
      ssh: host-a.example
      executable: /usr/local/bin/beekeeper
      config: /etc/beekeeper/host.yaml
      identity: host-a
      workspaceRoot: /srv/beekeeper/workspaces
      repositories:
        example/repository: /srv/checkouts/repository
      harnesses: [claude]
      maxBusy: 2
      connectTimeout: 10s
      operationTimeout: 30s
```

An entry's own `policyDigest` is deprecated: it still pins a host beekeeper
never updated, and beekeeper's recorded pin replaces it.

### Host updates follow the coordinator

Host updates follow the coordinator's release after probation. `agents hosts update <host>` connects
over the host's SSH route and runs `agents hosts self-update --version
<coordinator release>` there. With no execution running, the host holds its
admission, installs exactly that release once its Sigstore bundle verifies (as
`self-update` does), and has the installed release prepare its own policy.
The download is staged in a writable temporary directory beside the executable
and atomically renamed over it; no backup or hard link is created. The update
account needs write permission on that directory, rather than ownership of the
old binary. If a system installation directory is not writable, beekeeper verifies
the download in its own temporary directory, then uses `sudo -n` only to stage
and atomically rename it in the destination directory. Sudo never prompts;
if it is unavailable or denied, the failure is reported and the host stays held. A release that does not prepare leaves the host held for a retry;
one that does moves the host's watches and sandbox broker onto it, as
`self-update` does.
The coordinator looks the release up on GitHub itself, with its gh login (or
`GH_TOKEN`, `GITHUB_TOKEN`), never anonymously, and sends the lookup to the host
on the SSH command's standard input: the host downloads the binary and its
bundle from the release's public download URLs and asks GitHub's API nothing,
so a host without a GitHub login updates all the same, and no token leaves the
coordinator. A lookup failure delays the update, with a rate limit named with
its reset; the current host policy remains admitted. A host whose installed release predates this looks the release up
on its own once more.
The coordinator then runs `hosts check` with the digest the host answered and
records the release and digest in its state (`hostPins`): every start on the
host is pinned to it. No configuration change and no person is involved.

The supervisor loop runs the update for every `agents.hosts` entry whose pin
is not the coordinator's release, so a coordinator self-update carries the
hosts with it. During coordinator probation only updates wait. Starts continue
on older releases that pass the live protocol, capability, identity and pinned
policy checks; release equality is not the compatibility boundary:

- a host with an execution in a turn waits to update; compatible starts remain
  admitted. It updates once no execution is in a turn. An execution between turns (a
  parked worker waiting for its wake: ready, no turn running, no wake
  stored, no delivery uncertain) does not hold the update back. The host
  holds its admission through the install, so the next turn begins only
  after it, on the updated host, and the coordinator then rebinds that
  execution to the pinned policy as `agents recover` does; an execution it
  cannot rebind is named in the update's line with that retry;
- an execution whose process exited (a desk-review park closes its input) is
  not busy; it goes on when `agents wake` or `agents resume` names it, or when
  the wait it parked on settles. The host runs the same unit in the same
  workspace and conversation under a fresh coordinator receipt, inside its
  admission and room. It refuses a stopped or retired execution, one on omp,
  one with an unresolved delivery, and one whose bound rules are no longer
  installed: that one is not rebound, because only a running execution is;
- a host whose update fails (unreachable, no signed release, a prepare or
  check that fails) is held: no start there until an update succeeds. The
  loop hands the hold on once as a finding (`supervisor.file`, delivered by
  `guide watch`) and tries the host again hourly; the other hosts keep
  running. `beekeeper agents hosts update <host>` retries it at once;
- a host that drains or waits past more than one coordinator release (it did
  not follow one release and does not follow the next either) is handed on
  once as a finding, with the executions or the error that keep it behind.

A host first needs a release with `agents hosts self-update` installed by
hand; from then on it follows the coordinator. A development build of the
coordinator updates no host and gates no start on a host's release.

`hosts check` reports each host's release and tool versions and requires
the expected identity, protocol, pinned policy, harness and fresh resource
observation.

Changing the host binary, rules, settings, or protected host configuration
invalidates the policy: the host's `hosts check` fails on a digest mismatch
until `agents hosts update <host>` prepares and pins it again. Do not weaken
a required guard to make a check pass.

A running execution stays bound to the policy it started under. After such a
change its unit keeps running, but its `agents publish`, `agents merge` and
capped runs refuse until the coordinator rebinds it. Once the new bundle is
prepared on the host and its digest pinned in the coordinator catalog, rebind
each running execution from the coordinator:

```sh
beekeeper agents recover worker
```

Recovery inspects the same execution and never starts a replacement. It rebinds
only to the host's prepared policy, only when that is the catalog's pinned
digest, and only from the policy the execution is bound to; it records an
`execution.policy-rebound` event with both digests. The worker keeps its
workspace, commits, owned branch and pull request, and retries its publication.
While the host policy is unprepared, stale, or not yet pinned, `recover` exits 3
and says that the unit is reachable but its publication is unavailable. Only a
running execution whose owned unit is active is rebound: a stopped, completed,
exited, failed or never launched one is not, and recovery confirms an exited
execution as it is.

The pinned digest covers every host setting that changes what a worker may start
under or how much it may consume: the security sections, the execution host
configuration (`execution.maxBusy` included), and the start guards — the memory
floor (`capacity.availMinMiB`), the swap guard (`capacity.swapGrowthMaxMiB` and
`capacity.swapPSIMax` over `watch.swapWindow`), the load and disk limits
(`watch.loadMax`, `watch.loadPerCoreMax`, `watch.diskMinMiB`, `watch.diskFillWithin`),
the build slots and their caps (`memcap.slots`, `memcap.max`, `memcap.cpuQuota`,
`memcap.cpuWeight`) and the kind lab cap (`maxKindClusters`). Weakening one of
them on the host is a digest mismatch in `agents hosts check` until a matching
policy is prepared and pinned again (`agents hosts update <host>`). What stays host-local on purpose is the
machine's own watch and desk pacing, which never bounds a pinned remote worker:
the supervisor's target and pace (`capacity.ceiling` and the
rest of `capacity` beyond the guards above), watch's alert thresholds and
cadence (its `interval`, `repeat`, `availMinMiB`, `swapMaxMiB`, `oomdHeadroomMinMiB`,
`scopeAnonMaxMiB`, `gttMaxMiB`, `psiMax`, `cpuPSIMax`, `forkRateMax`, `stackMax`,
`toolProcsMax`, `tmpMaxMiB`, `diskCriticalMiB` and quiet/notify settings), and
memcap's slot location and hold bound (`slotDir`, `holdBound`). `hosts check`
reports the host's live resource observation (available memory, load, disk, slot
and lab pressure), so a coordinator can compare what the host runs with against
its own view of the machine.

## omp on an execution host

The host catalogue may list `omp` alongside `claude`, `codex` and `agy`. Configure
`agents.execution.harnesses.omp` on the host with an absolute `executable`, an
exact installed `version` in `>= 18.14.2, < 19.0.0`, and `configDir` naming
that host's own `PI_CODING_AGENT_DIR`. Never copy a coordinator credential
store. For a provider requiring login, the operator signs in once on that
host with `PI_CODING_AGENT_DIR=<configDir> omp login`; follow that provider's
device/browser flow. A keyless self-hosted provider needs its host-local model
configuration instead. Provider credentials and model configuration remain on
that host.

Prepare and pin a matching policy bundle before running `agents hosts check`.
The bundle binds an explicit omp extension. The check validates the pinned
version from the packaged CLI's `omp/<version>` output (or the previously
supported `omp v<version>` banner); it still requires an exact match to the
configured pin. It asks `omp models --json --no-extensions` for available model metadata
without a model call, and starts a bounded RPC session without sending a
prompt to verify the extension starts. A missing login/model, extension error
or absent readiness event leaves omp unavailable. No credential value is
returned. As with other adapters, a passing check advertises the version;
`unavailable.omp` explains a refused check.

The runner disables ambient extensions, explicitly installs the prepared
guard and enables only `bash`, `read` and `write`. Use foreground bash for
edits and searches; PTYs, services, asynchronous bash, URL reads and every
other tool are refused. The extension blocks on command failure, timeout,
invalid JSON and guard rewrites it cannot apply. Each allowed call passes the
shared workspace and execution guards and live coordinator authority check.
File tools accept one literal local path; reads require an existing target to
avoid omp spelling, suffix and approved-plan fallback resolution. URL shorthand
(`www.`) is refused. Home and `@` shorthands, whitespace,
path lists and patterns are refused because omp expands them differently from
the shared guard. Use guarded bash for filenames requiring those characters.
Hooks are not an OS sandbox for arbitrary code executed inside an allowed
shell command.

No task prompt is sent until the guard extension starts and the runner binds
its actual conversation identifier. A wake uses the same RPC process and
message id; an admission response, rather than a successful pipe write,
commits its receipt. Lost admission evidence remains uncertain for explicit
recovery. Prompt failures end the execution as failed. Inspect, logs, stop,
leases, publication and exact-head merge authorization use the shared
execution lifecycle. Provider usage readers and ceilings stay on the
coordinator; the host contributes machine room only.

## Codex on an execution host

Managed Codex execution logs include a `harness.stderr` JSON event after each
turn when the CLI wrote diagnostics. Capture is bounded to 64 KiB; truncation
is marked and incomplete trailing lines are omitted. Authentication-labelled
diagnostics are suppressed as a whole, and long opaque values (including long
paths and identifiers) are redacted. Startup errors remain visible when no thread event was
emitted; stderr does not count as first-task acceptance evidence.

A host may run Codex workers next to or instead of Claude. Install a pinned
Codex CLI in the supported family (`>= 0.165.0, < 1.0.0`) and add it to the
host's `harnesses`:

```yaml
agents:
  execution:
    harnesses:
      codex:
        executable: /usr/local/bin/codex
        version: 0.165.0
        configDir: /srv/beekeeper/codex
```

`configDir` is the host's own `CODEX_HOME` holding its login, and it is
required. Logging in is a one-time step for the person, on the host, with
the device flow; credentials are never copied from the coordinator:

```sh
CODEX_HOME=/srv/beekeeper/codex codex login --device-auth
```

Workers never run in that home. `prepare` installs a managed `CODEX_HOME`
inside the policy directory with the embedded worker rules as `AGENTS.md`, a
`config.toml` with hooks and the code-mode host on and the browser, computer,
image and plugin tools off, and a `hooks.json` whose PreToolUse hook is the execution
guard in Codex's format. It links only `auth.json` from `configDir`. The policy
binds those files; a changed one fails `hosts check` until a matching bundle is
prepared again. Use a ChatGPT subscription login: an API-key login is refused.

The runner supplies workspace trust in a transient inline `projects={...}`
table override on first and resumed turns. Dynamic workspace paths are literal
keys inside the TOML value; quoted dotted CLI override keys are rejected by
the pinned CLI's strict configuration validator. The managed configuration
file is not changed to persist project trust.

`hosts check` probes each configured harness's version, login and guard
without a model call. It advertises only the harnesses that pass and names
the others, with their reason, under `unavailable`. The coordinator lists the
harnesses it may start there, for example `harnesses: [claude, codex]`, and
starts a harness only when both list it and the host's check passes; `--harness
auto` chooses among those.

A remote Codex worker runs one `codex exec --json` turn per message. The first
turn carries the brief and creates the thread; the runner records it, binds the
guard to it, and runs each stored wake as `codex exec resume` on that thread. A
wake is accepted once its turn has started on the recorded thread; a turn on
another thread, or one that ends before it starts, leaves the receipt uncertain
for explicit resolution. A finished report ends the runner after its turn.
Lease, publication, review, merge, stop and recovery are the same as for
Claude. Codex usage stays on the coordinator: the remote worker counts against
the configured `codex` provider's ceiling, and the coordinator's own usage
reading covers the shared subscription; a host adds machine room only.

### Codex lifecycle proof (#2281)

Track the first live lifecycle at
https://github.com/giantswarm/beekeeper/issues/2281. A passing fixture test is
not a live proof. Before starting, verify the trust and reviewer-placement
fixes integrated through PR #2291, the code-mode route in PR #2268, the
coordinator CI wait in issue #2242, and parked publication/review retention
from PR #2244 are merged, released and installed. Do not reproduce those
changes in the proof branch.

Use the desk's supervisor-approved execution-host allocation procedure. On a
desk using `supervisor.hosts` and `hostsFirst`, start on an allocated host with
`agents start --host <host>`; a separate resource lease is not required unless
the desk explicitly configures one. The signed execution lease and host
admission checks still apply. If the desk requires a resource lease, claim it
before starting; if no approved allocation is available, record the blocker.
Use the existing subscription login and installed tools without changing
accounts, logins or spending settings.

On the leased host, record these observations in order:

1. Record the installed beekeeper and Codex versions and subscription login
   mode, then run host checks including forbidden push/merge probes. Start a
   worker twice and verify the installed policy remains valid after each start.
2. Verify the isolated worktree and execution identity, and that the coordinator
   counts the worker once against the shared Codex ceiling. Have the worker
   execute a shell command through its supported tool route, read the task issue
   with `gh`, and run the repository's focused checks.
3. Publish only its owned branch and open the proof PR. Record its commit and
   PR URL. Request independent review, park on the coordinator's CI condition,
   and wake through the coordinator. Verify the recorded thread, publication
   and review request survive the wake, with one accepted wake receipt.
4. Have the coordinator obtain independent review and explicit go for the exact
   head before merging. Record the observed merge commit and URL. A proof run
   subject to a no-merge instruction stops before this step and remains incomplete.

Record a stable host alias, execution identity, thread metadata, versions,
commit, PR URL and observed result. Keep the alias-to-hostname mapping private;
public evidence must contain no hostnames, personal details, secret values or
login files. Inventory residual artifacts under
https://github.com/giantswarm/beekeeper/issues/2113. Keep #2281 open until the
linked proof PR is merged and the complete live lifecycle has been observed.

The runner requires `turn.completed` after acceptance before treating a turn
as successful. A clean exit without that event fails the execution while
preserving an already accepted wake receipt for coordinator recovery.

## Antigravity (agy) on an execution host

A host may also run Antigravity (`agy` / Gemini) workers. Install a pinned `agy`
CLI in the supported family (`>= 1.4.7, < 2.0.0`) and add it to the host's
`harnesses`:

```yaml
agents:
  execution:
    harnesses:
      agy:
        executable: /usr/local/bin/agy
        version: 1.4.7
        configDir: /srv/beekeeper/agy
```

`configDir` is the directory holding the host's `agy` OAuth login token, and it
is required. Logging in is a one-time step for the person on the execution
host; credentials are never copied from the coordinator:

```sh
HOME=/srv/beekeeper/agy agy
```

(or point `configDir` at the host user's `~/.gemini` directory after logging in
once with `agy` on the host). `prepare` locates the OAuth token at
`<configDir>/.gemini/antigravity-cli/antigravity-oauth-token`,
`<configDir>/antigravity-cli/antigravity-oauth-token`, or
`<configDir>/antigravity-oauth-token` and links only that token into a managed
home inside the policy directory (`<policy>/agy/.gemini/`).

Workers never run in the host's own home. Inside `<policy>/agy/.gemini/config`,
`prepare` installs the embedded worker rules as `GEMINI.md`, empty
`mcp_config.json` and `skills.json` files (augmented per execution only with
the authenticated `coordinator` MCP proxy when `agents.remoteMCP` is enabled),
and a `hooks.json` in the `{"beekeeper":{"PreToolUse":[...]}}` format running
`beekeeper agents execution-guard --format agy`. The policy digest binds those
files; tampering with any of them fails `hosts check` until a matching bundle
is prepared again.

`hosts check` verifies `agy --version`, refuses `agy` while `agy` would
reject any entry of the managed `hooks.json` (it drops the whole file and
would run unguarded), probes login with `agy models` inside the managed home
without a model call, and runs the `agy` execution guard conformance probe. The host advertises `agy` only when all three pass, and a
coordinator listing `agy` in the host's `harnesses` (`harnesses: [claude,
codex, agy]`) starts `agy` workers there only when advertised.

A remote `agy` worker runs one `agy --input-format stream-json --output-format
stream-json` turn per message. The first turn sends the brief and records the
emitted `conversation_id`, binds the execution guard to that conversation, and
resumes stored wakes with `--conversation <id>`. Lease, publication, review,
merge, stop and recovery follow the same contract as Claude and Codex. `agy`
usage and pace stay on the coordinator: remote `agy` workers count against the
`agy` provider ceiling, not the coordinator's local ceiling (`local.ceiling`).

## Read each harness's readiness

`beekeeper agents hosts check <host>` includes a `readiness` entry for each
configured harness on a checked host. Each entry names its pinned `version`
and reports `versionCheck`, `login` and `guard` as `passed`, `failed` or
`not checked`. The version is the configured pin; `versionCheck: passed`
confirms that the executable matched it. A failed version check leaves the
login check `not checked`, rather than claiming a login failure.

When every configured harness fails, `hosts check` still prints its JSON
`readiness` and `unavailable` reports and exits nonzero. No failed harness is
advertised or admitted. A rejected agy hooks file reports a failed guard.
Policy, identity or transport failures before probing can prevent a readiness
report; they must not be interpreted as a passing check.

The guard result covers the installed policy and conformance check. For omp
it also requires the actual harness to load the bound extension without a
model prompt. If omp login fails, that extension check is `not checked`.
Authentication output and credential values are never included. Only
harnesses that pass every prerequisite appear in `harnesses`; `unavailable`
retains the explanation for each refused harness. A host with no passing
harness refuses the check and all starts.

## Start, inspect, and recover

Run the coordinator's commands against its configuration:

```sh
beekeeper agents hosts check host-a
beekeeper capacity --host host-a --builds
beekeeper agents start --host host-a --repo example/repository --ref main --harness claude --model opus --mode headless worker /srv/coordinator/brief.md
beekeeper agents inspect worker --json
beekeeper agents logs worker --json --limit 65536
```

Use `--dry-run` to validate placement and print the proposed start without a
reservation, workspace, or process. Remote starts require `--repo` and `--ref`;
`--dir`, desktop, and background modes are refused. Capacity uses the
coordinator's existing provider and bucket pool across hosts and the selected
host's physical guards. A host does not create another subscription budget.

`supervisor loop` places its worker starts on execution hosts. With
`supervisor.hosts` naming execution hosts (in order of preference), a start
whose repository a host maps, and for which `capacity --host <selector>` has
room, runs there pinned to the repository's default branch. This is the default
(`supervisor.hostsFirst: true`); explicit false retains local-first placement.
When no host can take the start, the coordinator is the fallback within its
ordinary local, provider, model and resource guards. Fallback preserves the
eligible model choices. A host's check weighs only the harnesses it can run.
`beekeeper capacity` and `beekeeper supervisor status` include each configured
host's last dated placement reason, repository and any update failure. A dated
placement is an observation, not a fresh capacity or admission grant. Hosts
absent from `supervisor.hosts` are identified as disabled for placement.
If no host takes a start for an hour while local workers have started, the loop
records one standing `GUIDE PROBLEM` per episode, retaining it across restarts
and coordinator releases. A successful remote start ends the episode. A desk
without `capacity.choices` never places remotely. Reviewers (including Codex)
follow the same host preferences and retain review capacity accounting, even
without `github.workerApp`. On the execution host, `beekeeper agents review`
reads the assigned PR and its closing issues, `beekeeper agents review --diff`
reads its diff, and `beekeeper agents review --body-file <file>` posts a comment
review through the coordinator. The body must begin with `Review: approved` or
`Review: changes requested` and include `Head reviewed: <full SHA>` for the
assigned head. Report the returned review URL in the authenticated completion
report. If the supervisor has not recorded the assignment yet, retry the read.
The relay requires a live execution lease and refuses a moved or closed PR;
retrying an identical review reconciles a lost response. No host GitHub
credential is needed. The coordinator authenticates the reviewer session,
exact head and reported review before a merge.

`capacity.local.ceiling` caps the busy agents the coordinator itself runs,
apart from `capacity.ceiling`: a desk whose ceiling counts the hosts' slots
too sets it to the coordinator's own share. Remote agents count against
their host's `maxBusy`, never against it; local self-hosted sessions count.
Every local start, the loop's and one by hand, is refused at it; a review may
use `capacity.reviewHeadroom` above it. A retired remote worker is
stopped and acknowledged before it leaves the roster.

Preparation creates no model process. The coordinator first records the stable
execution reference and handle, then signs a short-lived registration receipt.
The host verifies the receipt and checks live coordinator authority before the
owned service and harness turn start. Success requires first-task acceptance
under the expected conversation identifier.

After an interrupted start, inspect and recover the existing execution:

```sh
beekeeper agents recover worker
beekeeper agents hosts sync host-a
```

Recovery retains the execution reference and pinned workspace. It never starts
a replacement for an uncertain running unit. A failed or partial workspace is
preserved for inspection. Do not issue another start under a new identity to
escape an unknown outcome.

A start whose activation times out reads the host's journal before it answers:
a process that came up is the start succeeding, a prepared execution with no
process is named as such, and a host that still does not answer leaves the
outcome unknown. Repeating the same start, by name, task and host, activates
that execution rather than reserving another. One nobody retried is stopped by
the host sync `15m` after its start was sent, so it holds no slot and no host
update; its workspace stays.

Notes, parks, reports, and handover events from a managed worker reach the
coordinator through a durable ordered outbox. Failed delivery is explicit and
never falls back to host-local scheduling state. `hosts sync` and the watch
reconcile pending controls and stop intent. For a caller retry after lost shell
output, use the same `--request-id` and identical request; acknowledged controls
return their original receipts, including note numbers.

```sh
beekeeper agents stop worker --reason "operator stop"
beekeeper agents inspect worker --json
```

Stop intent is durable before SSH delivery. Only the recorded owned service may
be stopped. An unreachable host retains its reservation and task; reachability
is reported separately from the last process evidence. An acknowledgement of
termination is retained even if the subsequent inspection disconnects. Stopping
a process does not mark its task complete or remove its workspace.
Inspection, including acknowledgement of a lease-expired unit, also preserves
the workspace, its execution-owned Git repository and source bundle. Recover
unpublished commits and local edits at the workspace path printed by `inspect`;
stopping and repeating a stop do not authorize deletion.

### Retirement

The coordinator ends what an execution left through one resumable retirement.
A final report, a stop, a failed start and a crash all enter it; the watch
advances it, each execution on its own backoff, and records it under the
execution's `retirement`. A parked worker, an open desk review or a finished
round of an unfinished task does not enter it: the worktree and thread stay.

The execution's inventory is in its host journal under `resources`, recorded
before the resources exist: the worktree, its repository and branch, scratch,
temporary files, caches, Go module and toolchain downloads, source bundle and
credential dispatch directory. Workers run with `TMPDIR`, `XDG_CACHE_HOME`,
`GOCACHE`, `GOPATH` and `GOMODCACHE` inside `<workspaceRoot>/<execution>`, so
each build starts from a cold cache and leaves nothing in the host's home. Shared
provisioned sources and toolchains are never part of an inventory, and an
inventory naming anything but its own execution's paths refuses.

1. **Revocation** runs first and on its own: neither an unreachable host nor a
   GitHub outage blocks the other, and only a success records `revokedAt`. A
   failure keeps `revokeError` and retries with `nextAttempt`.
2. **Preservation** waits until the owned unit is confirmed ended, the log reached
   the transcript store and no publication needs the execution. Commits, including
   those only reflogs still reach, edits and untracked files that the starting
   commit does not hold become one recovery commit, recorded as `refs/beekeeper/recovery/<execution>/<generation>` and
   written as a verified bundle to `<state>/recovery/<execution>-<generation>.bundle`.
   Work that cannot be read fails the step and everything stays in place.
3. **Moving aside** renames `<workspaceRoot>/<execution>` to
   `<workspaceRoot>/.retired/<execution>-<generation>`, and the source and
   credential directories to `retired/<execution>-<generation>` beside the
   journal. Nothing is deleted at this step. A symlinked resource or parent, or
   one overlapping a provisioned path, refuses and stays in place.

The journal, log and control receipts stay as evidence, and so does the published
branch. `retention` under `agents.execution` (a Go duration, default `168h`)
sets `retainUntil` on the retired resources. A later coordinator retirement on
the host removes those of a retirement proved disposable, a few per call; see
[remote worker resource retirement](remote-worker-cleanup.md). Stopping and
inspecting never remove them.

To recover preserved work, fetch the bundle into a clone holding its base commit:

```bash
git fetch <state>/recovery/<execution>-<generation>.bundle \
  refs/beekeeper/recovery/<execution>/<generation>:refs/heads/recovered
```

Files ignored by Git are not in the bundle; they stay in the moved-aside tree. Its
worktree link is stale after the move: run `git worktree repair` from the moved
repository before using it. A desk stop holds the host's part until it is lifted
and never holds revocation.

Capped runs verify their worker identity, installed policy and live coordinator
authority before taking a slot. Their scopes stop with the verified owned
service and require that service to be active; they do not restart an inactive
owner. Local runs acquire no such dependency. The remote guard also inspects
commands nested inside `beekeeper run`, so wrapping an unavailable lifecycle or
merge operation does not authorize it.

The component does not adopt or archive personal sessions. Interactive remote
tools and remote transcript reads through coordinator-local paths are unavailable. Use owned execution
logs for output. Hooks inspect the supported tool surface and deny unavailable
authority; arbitrary code and account-owner changes require OS isolation for a
stronger boundary. New executions stop on coordinator lease expiry as described below.

## Deliver and reconcile messages

New SSH starts use one owned runner and one streaming Claude conversation. A
wake, note answer, timer, or park resume enters the coordinating outbox before
transport delivery. The host stores the envelope before returning `stored`.
Only a replay event with the exact message UUID and conversation ID establishes
`accepted`; worker mutations retain their separate `applied` control receipts.
The original sender, execution host, worker and generation remain attached to
the envelope. Local wake behavior is unchanged.

```sh
beekeeper --request-id follow-up-one agents wake worker "Continue the task"
beekeeper agents messages worker
beekeeper agents hosts sync host-a
beekeeper agents inspect worker --json
```

`agents messages` shows coordinating delivery state without message text.
`agents inspect` shows host receipts, ordered acceptance cursor and process
reachability. States are `pending` (coordinator only), `stored`, `uncertain`
(a harness write was attempted), `accepted`, `resolved` (an operator skipped a
blocked receipt without claiming acceptance), and `failed` (an unsent message found
its runner already completed or ended). A failed receipt blocks later delivery
and is not retried automatically. An unavailable or ended process
is also visible through inspection; a transport failure never proves it ended.
Wake exits nonzero until acceptance is confirmed. `hosts sync` and the watch
reconcile the same durable identities, retry only unacknowledged envelopes, and
never create a replacement process. A park remains until its resume is accepted.

An explicit request ID binds the sender and text; conflicting reuse is refused.
Without one, each CLI wake is a distinct send, including identical text. Use an
explicit request ID for CLI retries, or let `hosts sync` retry the persisted
outbox. Timer fires use their timer ID and original due time, note answers use
their note/decision identity, and resumes use their original park identity.
Repeating timer fires and separate CLI sends therefore remain distinct. Receipts
and the acknowledged cursor remain for the execution's lifetime; sequence gaps
stay queued and cannot overtake an uncertain message.

The runner persists `uncertain` before writing to the harness and commits an
ID-bearing acceptance evidence file before the journal receipt. Synchronization
recovers a receipt lost after that evidence. If the harness might have consumed
a message but no durable evidence survived, it remains uncertain for operator
reconciliation: inspect the owned logs, then explicitly resolve the oldest
blocked receipt if its outcome has been reconciled:

```sh
beekeeper agents messages resolve worker <message-uuid> --reason "Reconciled outcome; skip without replay"
beekeeper agents hosts sync host-a
```

This records the reason in both journals, retains the receipt, and advances the
cursor so later messages can flow. An uncertain write may already have been
consumed, or may still finish; resolution neither cancels it nor resends it.
Resolution does not count as acceptance or clear a park. If a deliberate new
resume is needed, use a new explicit request ID. Resolve retries reuse the same
message ID and reason. A different reason is refused. Completed and terminal
executions skip message delivery; control reconciliation and inspection continue
despite any message failure. Resolution never restarts an exited process.
Accepted and resolved coordinator receipts discard message text while retaining
the digest required to reject conflicting explicit retries.

No automatic replay or new process can resolve ambiguous consumption safely.
This is not a claim of crash-atomic model
execution. Journal admission refuses before exceeding its bounded storage.

Executions created before streaming-message support retain their original
single-turn behavior and explicitly refuse remote wakes. Recovery does not
upgrade or restart them.

## Coordinator leases and disconnected stops

New starts require a signed 90-second coordinator lease, renewed every 20 seconds.
The grant names the coordinator, execution host, execution ID and process generation.
The coordinator stores its sequence before replying; a lost reply retries the same
nonce and receives the original deadline. Stop intent and global stop revoke renewal
in the same coordinating state transaction. An unreachable stop keeps its capacity
reservation and unfinished task until termination is observed.

The runner measures its deadline from before sending the renewal using elapsed
monotonic time. A retry retains that original start time, so a delayed or repeated
reply cannot extend authority. Host/coordinator wall-clock offset does not extend
runtime. The signed issue/expiry times shown by inspection use coordinator UTC.
The runner checks expiry once per second, records stop intent, and asks the user
service manager to stop only its owned service and dependent build scopes. With a
responsive service manager, the disconnected bound is 90 seconds plus the one-second
check interval and the service's 60-second termination timeout. Service-manager
failure remains explicitly unconfirmed; it is not a termination acknowledgement.
A stop of its own unit is an orderly owned stop, not a crash: the runner records
the journal as `exited` with the recorded stop reason and exits 0, so a lease-expiry
stop leaves no failed unit to mistake for a harness crash.
Host suspension pauses monotonic elapsed time on platforms where the OS does so;
this bound assumes a running host and functioning user service manager.

A runner restart cannot reuse runtime authority: the recorded phase refuses another
runner, and systemd ends the original service's child processes when it exits.
Coordinator restarts retain the signed sequence and stop intent. Reconnection uses
`agents hosts sync <host>` and `agents recover <worker>` to inspect the same execution;
it does not replace an uncertain process. Inspection exposes the last grant, expiry,
reachability and stop acknowledgement. Older executions remain unchanged and require
an acknowledged explicit stop; recovery does not silently upgrade their contract.

## Live merge authorizations

Only the supervisor loop, or a person on the coordinator (`--as <person>`), issues or
revokes authorizations; a session never does. A green exact
head and an authenticated independent desk review result are prerequisites. The
component records the exact reviewer session when the supervisor starts the round.
That session must complete with the same `Review: approved` or
`Review: changes requested` verdict on the first report line, followed by the
exact public review URL. The reported verdict must match the public review.
An identical completion retry retains the original captured result; a later
shared-account edit cannot rebind that retry to another verdict. The
coordinator persists the GitHub review ID, author, head and body digest before
report delivery can prune the report. A shared GitHub login or a worker-authored
approval comment alone grants nothing; `--as` cannot submit the reviewer's result.
Issuance and live use require that same persisted result, and grants pin the
round ID, review ID and authenticated body digest. Edited reviews and new rounds invalidate old grants. Older
rounds without component-recorded reviewer provenance need a fresh review. The supervisor loop issues a two-minute grant when it gives a
remote worker merge go; a person can also issue or revoke it explicitly:

```sh
beekeeper --as <person> agents authorizations grant worker example/repository 42 --head <reviewed-head>
beekeeper --as <person> agents authorizations inspect <authorization-id>
beekeeper --as <person> agents authorizations revoke <authorization-id>
```

The remote worker uses the component command supplied with its go:

```sh
beekeeper agents merge example/repository 42 --head <reviewed-head> --authorization <authorization-id>
```

The command verifies owned process and repository identity on the execution host,
then authenticates its exact execution and generation to the live coordinating
service. It uses the coordinator's independently provisioned GitHub login: no host
credential is copied or forwarded. The actual merge runs synchronously at the
coordinator. Host hooks continue to deny raw `gh`/`devctl` merges and API mutations.
This narrow component path is the only supported remote merge surface; unsupported
harnesses and older executions remain denied.

The service checks the exact tuple, unexpired and unrevoked grant, current head,
CI, authenticated review, execution lease and stop state immediately before the
attempt. Holds use the same repository, lane and installation scope, PR/session
exceptions and explicit repair-window readiness waiver as the existing merge gate. It retains the coordinating state lock through a bounded synchronous
attempt (at most 20 seconds, also bounded by grant expiry), serializing stop,
hold changes and revocation against use. Those operations may report a busy state
lock during that interval and must be retried; they are never reported committed
early. The service consumes the grant durably before calling GitHub once and
never retries a merge mutation. A moved head needs a fresh review and go. A cached
grant provides no authority without this live service. Both host and coordinator
retain the authorization ID in their audit evidence.

A lost reply is an uncertain outcome, not permission to retry a merge. A person on the
coordinator queries the result and retains the consumed authorization:

```sh
beekeeper --as <person> agents authorizations reconcile <authorization-id>
```

Reconciliation never issues another merge. If GitHub cannot confirm the exact merged
head, the outcome remains unconfirmed and needs operator investigation. A failed or
consumed grant does not become reusable. Hooks cannot constrain arbitrary unhooked
programs or an account owner changing binaries, configuration or state. Dedicated
accounts or OS isolation are required for a stronger same-account boundary.

## Publish a branch and pull request

A managed Claude worker needs no GitHub write credential on the execution host.
Commit changes in the owned workspace, then run:

```sh
beekeeper agents publish --title "fix: resolve the defect" --body-file /srv/beekeeper/workspaces/<execution>/workspace/.beekeeper-scratch/pr.md
```

The coordinator receives a Git bundle over the authenticated callback, verifies
its HEAD and ancestry from the pinned execution commit, and pushes with its own
GitHub login. The remote branch is always
`beekeeper-execution/<execution-id>/<generation>` in the execution's repository;
a worker cannot select a repository, remote, destination ref, default branch,
force option or arbitrary GitHub API operation. The coordinator resolves the
repository's default branch and creates the PR against it. Both PR text and
new commit text pass the ordinary outbound rules; complete commit messages are
scanned separately from patch content, so message bullets are never mistaken
for patch deletions. PR text cannot ping accounts.

The first publication requires a title and body file. Once a PR exists, review
fixes need only `beekeeper agents publish`: the same branch advances by a normal
fast-forward push and the existing PR is returned without changing its text or
discussion. Closed PRs cannot be reused. For a lost push, create or callback reply,
repeat the publication: Git rejects history rewrites, and the coordinator looks
up the exact owned branch before creating a PR. A create attempt is never retried
blindly. PR editing and commenting continue to use coordinating workflows;
this command grants no general GitHub API authority.

To bring the latest default branch into the workspace before publishing again
(before the desk review approves the PR; after approval only the merge moves the
head: the supervisor's reviewed merge takes a behind branch as it stands, or
updates it itself where the base requires up-to-date branches and the desk has
no admin merge, and a sync is left for a conflict the supervisor reports):

```sh
beekeeper agents publish --sync-base
```

This transfers upstream commits through the same coordinator, fetches them into
the owned workspace and merges them with hooks disabled. The merge resolves
the worker's usual author and committer identities, including global Git identity,
and supplies only those identity fields to the isolated Git operation. Other
global configuration stays disabled. A host whose execution user has no identity
prepares its workspaces with `beekeeper <beekeeper@execution.invalid>`, so a
worker commits and merges there without supplying one. Resolve conflicts and
commit locally, then publish. No source checkout is modified and no host GitHub
credential or SSH agent is forwarded. This command creates no review or merge
authorization; park on the returned PR and follow the independent review gate.

Each transfer uses authenticated 256 KiB chunks, within the existing wire bound,
and a SHA-256 digest over the complete bundle. A bundle is bounded to 64 MiB;
a larger transfer is explicitly refused rather than truncated or sent with a
credential workaround. Chunk retries verify existing bytes; partial or corrupt
bundles cannot be published. Transfer files remain under the coordinator's
`execution-publications/transfers` state directory for recovery and may be moved
aside once the execution is retired. Failed staging directories remain for
inspection under the coordinator's `execution-publications/transfer-*` state
directories; successful staging directories are removed.

A republication with `--title` or `--body-file` updates the existing pull
request's title or body and lists the fields in `updated`; a flag left out keeps
the field, and a failed update is an error that an identical retry repeats.

A lost `gh pr create` answer settles by the owned branch: a pull request found
there is returned. None found, the error says GitHub created none and why. Only
a branch that cannot be read stays `PR creation outcome uncertain`. Creating or
updating and that read have their own 15-second limits, outside the attempt's
20 seconds. A write also ends with the lease and starts only with a live one;
only the read outlives it. An empty `--body-file` or `--title` is refused, since
a pull request's body cannot be cleared this way.

A failed Git step reports its stage and a fixed diagnosis, never Git's raw
output, which may carry URLs or credentials: for example `publication Git push
failed: GitHub refused a workflow file change: the coordinator's GitHub
credential lacks the workflow scope or permission`. That one is an
authorization barrier, not a transfer, ancestry or execution-policy error: the
coordinator neither retries it nor changes its authentication, and repeating
the publication fails the same until the coordinator's credential may change
workflow files. Other diagnoses name a protected branch or repository rule, a
non-fast-forward push, a rejected or missing credential, denied write access,
an unreachable GitHub or the 20-second limit; anything else is an
unclassified Git failure. The coordinator's event log records each denial as
`execution.publish-denied` with the head and the same diagnosis (`beekeeper
log`). A worker on a host runs `beekeeper log` too: its flags go to the
coordinator, which answers with up to 256 KiB of the newest events, for example
as the proof of a step that ran there after a merge. It is the only coordinator
read of its kind; `status`, `board` and `supervisor status` stay refused.

Every chunk and publication checks the exact execution generation, repository
allowlist, current lease, completion, execution/global stop and GitHub hold.
Publishing holds the coordinating state lock through a synchronous attempt of
at most 20 seconds; it returns an explicit uncertainty on disconnect. Stops and
holds serialize with that attempt. `agents hosts check` refuses hosts that do
not advertise publication support; upgrade and prepare an older host before
starting workers there. Existing raw merge and API guards remain in force.

### Source freshness at start

The coordinator fetches the requested `--ref` from GitHub before host preparation,
records its exact commit and a bundle digest in the start request, and transfers
the objects over the authenticated SSH route in bounded chunks. The host checks
the digest and commit before checking out the workspace. Its configured source
checkout stays unchanged and needs no GitHub credential. Recovery reuses the
saved commit and bundle even if the branch has advanced meanwhile. Both ends
must support source transfer; an older host refuses preparation rather than
starting from its stale ref. Bundles have a 64 MiB limit; oversized sources fail
before activation and preserve the reservation for inspection.

A pending reservation from before source transfer may already have a prepared
host journal whose reply was lost. Recovery inspects that journal before
pinning source and reuses its original request and commit. Only an explicit
missing-journal response permits a new source pin; an unreadable journal or
uncertain connection preserves the reservation for another recovery attempt.

## Provision repositories by owner

A host need not keep a preseeded checkout of every repository on the board.
Set `repositoryOrgs: [example]` under both the coordinator's
`agents.hosts.<host>` entry and the host's `agents.execution` configuration,
then prepare and pin the new policy with `agents hosts update <host>`.
Only list owners authorized for workers on that host. Explicit `repositories`
mappings continue to work and can be combined with `repositoryOrgs`.

Remote placement and `agents start --host <host> --repo example/another
--ref main` admit repositories under those owners. The coordinator fetches
and pins the requested commit using its own GitHub transport and sends the
existing bounded source bundle. The host verifies its digest and commit and
clones it into the execution's owned workspace, with the canonical GitHub
origin. No host GitHub credential or additional shared source checkout is
needed for provisioning. An unmapped repository without a pinned bundle is
refused. The workspace and transfer use the normal execution lifecycle and
cleanup; provisioning creates no extra repository or cache outside it.

Pin `gh` and Go as described above on every host. On an idle host,
`beekeeper agents hosts provision-tools` installs missing pinned tools from
the official Go and GitHub CLI releases, checking the published SHA-256
before extraction. Go needs a dedicated installation directory with its
executable at `<installation>/bin/go`; gh needs its configured executable
path. The provisioner refuses traversal, links and oversized archives, and
preserves an existing mismatched installation: configure a new versioned
path to upgrade it. The normal host update provisions missing tools before
preparing the current release's policy, so repeat updates also repair a
missing installation. Both paths refuse to change tools while executions
run. No provider or GitHub credential is used to download these public tools.

 In addition to
checking mapped checkouts, preparation validates the actual committed
workspace's `go.mod` against those tools. A newer toolchain requirement in a
new repository or ref refuses preparation before the harness launches; update
the installed toolchain and its policy pin before retrying. Tools remain
host installations shared by workers, rather than per-worker downloads.

Host sync also asks each used, pinned host for the active journal-backed
executions of this coordinator, including once when the coordinator has no
active execution records left on that host. An execution absent from the
coordinator's current records is stopped through its exact journal-backed
identity. The host retains its stopped journal, and naturally exited or failed
journals are excluded, so neither is reconciled again. Other coordinators'
executions are excluded.

A recorded execution whose host journal disappeared is settled as failed only
when the host also confirms its unit is inactive. Unknown or active unit state
remains unconfirmed; no process without its ownership journal is stopped.
The reconciliation writes an audit event and does not claim a stop acknowledgement.
Host update drain messages name the recorded workers and execution IDs; a busy
host also returns its blocking execution IDs. On a host predating inventory, a
recorded execution whose host journal is confirmed absent does not block
`agents hosts update` from upgrading the host, after which unit inactivity is
confirmed and the record settles.

### Per-worker GitHub credentials

The coordinator issues short-lived GitHub App user-to-server tokens (`ghu_...`)
so every worker action runs as `<user> via <App>` within the authorizing user's
own GitHub permissions. No GitHub App private key, client secret, refresh token
or permanent GitHub login is installed on an execution host; worker tokens are
never installation tokens. Configure only the coordinator:

```yaml
github:
  allowedOrgs: [example]
  workerApp:
    clientID: Iv23liExampleClientID
    refreshToken: op://Shared/worker-app/refresh-token
```

The `refreshToken` field may name a 1Password reference (`op://<vault>/<item>/<field>`),
or a SOPS path (`sops://<file>#<path>`). Read-only source references are rejected
before authorization or refresh. If persisting a rotated refresh token fails,
repeat `beekeeper agents hosts github-login` to restore authorization.
Authorize the coordinator once through GitHub's OAuth device flow:

```sh
beekeeper agents hosts github-login
```

The command prints the verification URL (`https://github.com/login/device`) and
one-time user code, waits for authorization, verifies the resulting user-to-server
token has expiration enabled, and writes the refresh token to the configured
secret reference. This configuration enables GitHub for newly started executions.
Existing executions retain their recorded publication mode.

In an enabled execution `gh issue`, `gh pr` and `gh api` commands use a
short-lived credential fetched over the authenticated SSH callback. Cross-repo
calls name `--repo owner/repo`; `gh api` names `repos/owner/repo/...`. Reads may
reach any repository the user can access. Writes (`git push`, `gh pr create`,
`gh pr edit`, `gh pr comment`, `gh issue create`, `gh issue edit`, `gh issue comment`,
`gh issue close`, `gh issue reopen`) are restricted by the wrappers to owners in
`github.allowedOrgs` (an empty list allows no writes or GitHub-enabled starts). A
write names one repository, as `--repo owner/repo` or a URL target, never two.
Every other `gh` subcommand is refused unless the wrapper lists it as a read
(`list`, `view`, `status`, `pr checks`, `pr diff`, `pr checkout`, `repo clone`,
`run watch`, `run download`, `release download`, `release verify`, `release
verify-asset`). `gh api` runs GET and HEAD only: `-f`/`-F` need an explicit
`-X GET`, since gh otherwise sends POST, and `--input`, `--hostname` and
GraphQL's POST is refused, except a `gh api graphql` read whose query and
variables are all literal and that holds no mutation, which the wrapper and the
execution guard both pass. An empty `projectItems` is no proof an issue is off a board: it can omit another owner's.
A worker may work in and
open pull requests across multiple repositories in one task (using its primary
workspace or additional checkouts cloned under `.beekeeper-scratch`). The runner
installs dispatch scripts for `gh` and `git` ahead of the pinned tools; scripts
and journals contain no token. Only the launched GitHub client receives it,
through its environment.

Push from the owned workspace (or a secondary checkout under `.beekeeper-scratch`)
with `git push origin HEAD:refs/heads/beekeeper-execution/<execution>/<generation>`,
then open the PR with `gh pr create`. The adapter supplies that same head branch,
and the coordinator verifies it against GitHub and the parked checkout commit
before requesting desk review. Force pushes, other branches and alternate push
remotes are refused. Local Git commands run without a credential. Network Git
uses the pinned HTTPS origin and disables inherited credential helpers, global
Git configuration, hooks and redirects. Authenticated pushes use a disposable
bare repository with only the resolved checkout commit and object storage;
workspace Git configuration and refs cannot add tags, mirror other branches or
redirect the transport. Inherited Git environment overrides are removed.
Merges continue through the coordinator's review and exact-head authorization,
never a worker's `gh pr merge`.

The same write-capable user token serves reads and writes across repositories.
It is not scoped by repository or by the wrapper's requested operation.
`allowedOrgs` restricts managed wrapper commands, not the token itself: GitHub's
actual authority boundary is the App's installations intersected with the
user's permissions.

GitHub user-to-server tokens expire after eight hours. The coordinator caches the
current user token until five minutes before expiry and records issued tokens in
a private per-execution ledger under its own state directory, outside scheduling
state and event logs. Every issuance consults the coordinator cache so refresh
never reissues a stale per-execution entry. Runner exit, completion, explicit
stop and terminal inspection immediately deny further callbacks. Because the
access token is shared, they rotate the coordinator's refresh token only when
no other live GitHub-enabled execution remains. Until then a stopped worker's
already-issued token remains valid; managed guards and host process termination
remain necessary. The last execution rotates and persists the refresh token,
invalidating the old access token at GitHub. Failed revocation retains the ledger
and reports an error for retry. An unreachable host does not prevent coordinator
revocation. Device-wide revocation rotates or revokes that coordinator's refresh
token; user-wide revocation removes the App authorization in GitHub settings.
The managed command
hooks are the same policy boundary as the rest of SSH execution, not an OS
sandbox: a GitHub token cannot itself limit Contents writes to one branch. Hosts
therefore still require the managed guards and owned execution identity.

Post-merge operations for this migration include creating/configuring one public
GitHub App owned by the organization, authorizing each coordinator through device
flow, updating coordinator and execution hosts, and proving multi-worker GitHub
access live. After that proof, retire the previous worker-access App and delete
its private key from the coordinator secret store.

## Rerun failed CI without a new commit

From a GitHub-enabled execution's workspace, run
`beekeeper agents rerun <pull-request-number>`. The authenticated callback
asks the coordinator to run `devctl pr rerun` for the execution's repository.
Only a PR on that execution's assigned branch is accepted. Stopped, completed,
expired-lease and GitHub-held executions cannot request a rerun.

The coordinator needs its own CircleCI login with Write access. CI credentials
stay there; the worker receives devctl's output, diagnostics and exit code
(including authentication-required exit 8). The command starts failed-workflow
reruns without waiting for completion. Read the failure first and use this for
transient CI failures; code failures still need a fix. A lost callback reply is
not proof that no rerun started: inspect CI before trying again.

The coordinator records `execution.ci-rerun-requested` before invoking devctl
and `execution.ci-rerun-result` when it returns. These audit events name the
repository and PR, with the result's exit code and whether the command errored;
command output and credentials are excluded.

The generated CircleCI configuration invokes `architect/go-build`; retrying a
transient Rekor signing failure inside that job belongs in the architect orb,
not an edit to Beekeeper's generated workflow files.

### Resource audit and bounded retention

`beekeeper agents hosts check <host>` prints capabilities with the read-only
resource inventory in `resourceAudit`. Each entry has an execution ref and coordinator
when ownership can be verified, a state (`active`, `parked`, `terminal`, `retired`,
`unknown`, or `shared`), reason and retry action. The desk joins this with its registered execution inventory
to flag owned orphan resources without changing their observed state. Managed
worker refs in private execution repositories are listed as metadata; orphan
refs retain an ownership hold. An unreachable host fails the
check; it is unknown, never evidence that its resources are safe to remove.
The audit reads journal metadata and directory entries, never credential values
or log/workspace contents. Symlinks and malformed or unregistered ownership
records remain unknown. Immediate children of the managed workspace, source,
control, dispatch, retired, recovery and bundle containers are included; execution
contents are not swept. A retirement that stopped halfway is `terminal` with its
recorded error; a finished one is `retired`, with `eligibleAt` when it can go.
The audit also names, up to 256, the immediate children of the account's
`~/.local/state` other than beekeeper's own: a worker runs on a `HOME` in its
execution root, but one on the account's home or writing by absolute path leaves
them there, no execution owns them, and they stay `unknown` for the host owner.
More entries than that, or a directory that cannot be read, is an `unknown` row
naming the state directory, so an incomplete listing never reads as clean. Nothing
else under home is read.
The inventory refuses more than 4096 entries rather than silently truncating.

An operator can reconcile **one established execution owner** with:

```
beekeeper agents hosts reconcile <host> <execution-uuid> <generation>
```

This rechecks terminal supervisor state under the execution lock, reuses stop
cleanup, enters the same retirement a coordinator's report or stop does, retries
GitHub revocation and assigns the log to managed retention in the journal
(`retainedAt`). Retirement preserves unpublished work before anything moves aside,
and each call also removes a few earlier retirements proved disposable and past
their retention. Repeat calls preserve the original deadlines. Unknown or live units refuse
reconciliation. Failed revocation is recorded for the next audit and retry;
retention does not advance on that failure. Partial cleanup is retryable.

| Retained class | Duration | Accountable owner / release rule |
| --- | --- | --- |
| Execution log | At least 30 days after successful reconciliation | Owning coordinator's desk; review/recovery evidence remains held until explicit release |
| Workspace, private repository and execution refs, source bundle of a retirement proved disposable | Host `retention`, 7 days by default, after retirement | Owning coordinator's desk; removed by a later retirement or reconciliation, see [remote worker resource retirement](remote-worker-cleanup.md) |
| The same with recovery work, ignored files or no proof, and recovery bundles | No expiry | Owning coordinator's desk; publish or recover the work, then remove it by hand |
| Coordinator policy bundle | Host `retention` once no unretired execution is bound to it | Execution host owner; removed by a later bundle sync |
| Journal, control receipts, accepted-message evidence | Held for review/recovery | Owning coordinator's desk; never deleted by retention |
| Disposable MCP proxy home | Removed by acknowledged lifecycle stop | Execution host owner; existing lifecycle cleanup |
| Shared sources, policy, toolchains and authentication homes | Host policy lifetime | Execution host owner; no automatic deletion |
| Unknown ownership, symlinks and unrelated files | Indefinite ownership hold | Execution host owner must establish provenance before any cleanup |

After the desk verifies a log is no longer needed for review, unpublished work
or recovery, it may explicitly attest that fact:

```
beekeeper agents hosts reconcile <host> <execution-uuid> <generation> --release-evidence
```

Only a regular log older than its retention deadline is then eligible for
removal. The flag does not release workspace or source evidence. Reconciliation
is bounded to one execution, its log and a small purge batch, and removes nothing
from home or a global `/tmp`.
Recovery holds deliberately override disk reclamation; owners must resolve
holds rather than rely on automatic expiration to erase unpublished work.

For the end-to-end proof on an explicitly leased host, save `hosts check`
output before reconciliation, reconcile a terminal execution, and save the
after inventory showing `managed retention` for what moved aside and `managed log
retention` with its journal timestamp for the log. Past `eligibleAt`, verify a
disposable retirement is removed. After 30 days, verify `log past retention`, review evidence, release it, and
capture the final inventory. Also verify active, parked, unknown, shared and
recovery resources remain. Epic completion requires all three execution hosts
to pass this audit after that proof. This procedure does **not** authorize fleet
cleanup; ownership must be established first.

## Remote harness acceptance evidence

Adapter tests use temporary repositories and fake harnesses and transports;
they do not establish a live provider or fleet proof. Complete the following
on a provisioned host, in order: Codex, omp, then agy with accepted guard hooks.
Use the independently provisioned host login described above; never transfer
coordinator credentials.

For each harness, retain the host identity, installed beekeeper and harness
versions, `hosts check` report, execution ID/generation, published commit and
PR URL. Start with explicit `--host`, `--harness` and `--model`; verify inspect,
logs, a parked wake on the same conversation, and acknowledged stop on a
separate disposable execution. Confirm the worker occupies its coordinator
provider pool and does not introduce another usage reader or ceiling.

Have the worker publish through `agents publish`. Record independent review
of the exact published head, the coordinator's go, and the merge receipt/URL
from `agents merge`. Verify raw host merge commands remain denied. Record
failed steps explicitly; a successful launch or fake-harness test alone does
not complete the PR/review/go/merge proof for that harness.

Repository visibility restrictions also apply at SSH task admission and coordinator write boundaries. Set `github.publicOnlyOwners: [example]` to require verified public visibility for that owner. Private, internal, missing and unreadable visibility results are refused before starting work and when granting write authority, publishing or merging. Owners not listed retain the existing policy.
