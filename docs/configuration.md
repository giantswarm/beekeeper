# Configuration

The configuration keys, their defaults and what they set. A complete example is in
[examples/config.yaml](examples/config.yaml).

`$XDG_CONFIG_HOME/beekeeper/config.yaml` (or `--config`, or `$BEEKEEPER_CONFIG`). Every field is
optional. A non-empty `$BEEKEEPER_CONFIG` must name an existing file; a missing file is an error
before any state is opened. Pre-tool hooks report the failure as a deny decision. An unset or
empty variable selects the default path, where a missing file still uses defaults. An empty
configuration works: `watch`, `snapshot`, `free`, the hook and the coordination commands run
with neutral defaults. Memory and disk thresholds default to fractions of what the machine
has, and every organisation or desk choice is unset until configured.
[`docs/examples/config.yaml`](examples/config.yaml) is a complete desk's configuration, every
key annotated with what it guards and its default.

`beekeeper config check` validates the selected file without opening state or changing it. It exits
0 and names the file when valid, or exits 1 with each finding's dotted key path (sequence entries
use `[0]`). It checks unknown keys, YAML types, the loader's value rules, incomplete references, and
explicitly configured local input files: the supervisor's briefs, the guide's instructions, the
reporter's brief, harness configuration files, kubeconfigs, execution rules, token files and
`file://` age identities.

YAML merges follow their effective precedence: explicit keys override inherited values, and the
first mapping in a merge sequence wins. Provider usage accepts both inline fields and nested `usage`
mappings. Independent value-rule and incomplete-reference findings are collected alongside schema
and file findings; malformed fields are isolated from valid siblings. It only stats these files; it
reads no referenced content, resolves no secrets and runs no commands.

Output paths, globs, remote paths and unconfigured defaults need not exist. The selected config file
itself must exist, even at the default path. `--config` takes precedence over `$BEEKEEPER_CONFIG` as
usual.

Every session's hook loads this file on every tool call, so an edit is one write:
`beekeeper config set <key> <value> [<key> <value>]…` sets dotted keys (`secret.store.read`) to YAML
values in a single rename, validated before it replaces the file. A result that would not load is
refused with exit 3 and the file unchanged; comments, the other keys, the file's mode and a link to
it stay. Keys that reference each other (a `store://` age identity and `secret.store`) are set in
the same call; an editor saving them one after the other is the edit that can leave a window
between them. `config set` runs on a file that no longer loads, to repair it.

A temporary change has an owner and an end: `beekeeper config set --until <time|owner/repo#n>
--reason "<why>" -- <key> <value>…` records an override with each key's value before (or that the
file had none). `config overrides` and `beekeeper handover` list the overrides in effect. Once the
time comes, or the issue or pull request closes (read every 5 minutes, held under the GitHub
budget floor), the watch or the supervisor loop says so and restores the values before; the event
log records `config.override` at the set, `config.override.end` and `config.restored` at the end.
A key that changed since the override set it is not restored: `guide.person` gets a decision note,
answered with `config restore <id> --force` (the values before) or `config keep <id>` (the current
ones). A key another override in effect set is taken over, with that override's value before.

The organisation and desk keys, and their defaults:

- `kube.protectedContexts` (default none): Additional production-class contexts whose clusters
  refuse direct writes; include full test installation contexts
- `kube.production` (default unset): The installation whose clusters agents never write to, and the
  no-current-context rule of the machine kubeconfig
- `kube.readOnlyExecQueries` (default none): Exact queries `kubectl exec <pod> -- psql -X … -c
  '<query>'` may run on production and `kube.protectedContexts`, on any pod the context reaches;
  only `SELECT *`, a column list or `count(*)` from a named relation, optionally `LIMIT`
- `kube.contextTemplate` (default unset: the context named after the installation, else `*@<name>`):
  An installation's kube context, `{installation}` its name (`login.example.com-{installation}`)
- `kube.kubeconfig` (default `KUBECONFIG`'s files, else `~/.kube/config`): The kubeconfig files a
  `k8s://` source of `beekeeper secret` resolves its context in, merged as kubectl merges a
  `KUBECONFIG` list (`~/` allowed); a kind lab's context reads through kind instead
- `alerts.installations`, `lanes`, `resources` (default none): The installations whose alerts are
  read, the merge lanes, the leasable environments
- `alerts.installations[].floor` (default every severity): The lowest severity printed of an
  installation's alerts
- `alerts.ignore` (default `[Watchdog]`): Alert names that never appear
- `alerts.quiet` (default `[{severity: notify}]` once `alerts.team` is set, else none): Alerts whose
  changes are logged, not said
- `alerts.tenant` (default unset: only kube-prometheus-stack's Alertmanager): The Mimir tenant
  (`X-Scope-OrgID`) whose Alertmanager is read first
- `alerts.pagerduty.context` (default unset: PagerDuty is not read): The muster context whose
  PagerDuty MCP server the watch reads the open incidents through, as the person (`muster auth login --context <it>`);
  beekeeper holds no PagerDuty token
- `alerts.discover.every` (default unset: nothing is discovered): How often the watch discovers the
  installations that carry a HelmRelease of `alerts.team` among Teleport's clusters; they join
  `alerts.installations` (needs `alerts.team`)
- `alerts.discover.label`, `.exclude` (default `application.giantswarm.io/team`, `["*-*", "*.*"]`):
  The HelmRelease label naming the team; the glob patterns of Teleport cluster names never read
- `alerts.pagerduty.server`, `.services`, `.every` (default `pd`, none (required with a context),
  `1m`): The server's muster name (`x_<server>_<tool>`), the ids of the team's PagerDuty services,
  the reading interval
- `github.probeRepo` (default `giantswarm/beekeeper`): The repository whose conditional GET reads
  the budget
- `github.floor` (default `2500`): The budget under which GitHub work stops
- `github.urgentBound` (default `200`): The budget an urgent merge (`lanes urgent`) keeps for itself
  under the floor
- `github.allowedOrgs` (default `[giantswarm]`): The organizations and accounts an agent writes to;
  the hook refuses every GitHub write to any other repository, `[]` turns it off ([Writes outside
  the allowed organizations](safety.md#writes-outside-the-allowed-organizations))
- `github.publicOnlyOwners` (default `[]`): Owners restricted to verified public repositories at
  task admission and GitHub write boundaries; unreadable visibility refuses access
- `github.upstreamReview` (default unset: "the team's upstream review list"): Where a change meant
  for upstream is queued (`owner/repo#n`), named in the refusal
- `merge.devctlOwners` (default none: every merge is the plain squash merge): The owners whose
  repositories `devctl pr merge` serves
- `merge.admin` (default none): The repositories (`owner/repo`) the desk merges with `--admin` past
  their branch ruleset after the approved desk review; only their merge go names `--admin --match-head-commit <head>`
- `guide.skill`, `guide.person` (default unset): The guide's skill and the person it walks through
  their notes
- `claude.desktopApp` (default `claude-desktop`): The desktop app's executable, which opens
  `claude://` links and starts the app
- `shell` (default `$SHELL`, else `sh`): The shell the hook runs a rewritten build, test or lint
  command in
- `maxKindClusters` (default one per 40 GiB of RAM, at least one): The kind clusters the hook lets
  the machine run
- `memcap.max` (default 14% of RAM): A `beekeeper run` command's MemoryMax
- `memcap.cpuQuota` (default half the cores (`1200%` on 24; 100% is one core)): `memcap.slice`'s
  CPUQuota: the cores every `beekeeper run` command together may use, shared by the slots by equal
  weight; `install` writes it into the slice unit and every `run` sets it again at its slot
- `memcap.cpuWeight` (default `50`): `memcap.slice`'s CPUWeight against the desktop's slices (100
  each): the runs' share of the cores while the desktop wants them too, a third by default
- `memcap.holdBound` (default `30m`): How long a run may hold a build slot before `watch` says `SLOT
  HELD LONG` with the slot, its holder and since when; a benchmark or a lab proof runs with
  `beekeeper run --class benchmark|lab`, which takes no slot
- `watch.availMinMiB`, `watch.scopeAnonMaxMiB`, `watch.gttMaxMiB` (default 12%, 32%, 28% of RAM):
  LOW RAM, DESKTOP SCOPE, IGPU GTT
- `watch.swapMaxMiB`, `watch.oomdHeadroomMinMiB` (default 60%, 6% of swap): SWAP (disk swap, zswap's
  share not counted, while it grows from new swap-outs, zswap's writeback not counted, and
  MemAvailable falls), and the headroom under which OOMD IMMINENT is said (only while systemd-oomd
  watches a cgroup for swap)
- `watch.swapWindow` (default 10m): how far back swap's growth rate and zswap's writeback rate look
  (SWAP, OOMD IMMINENT, `capacity`'s swap guard), at least 5m: a burst older than it has left the
  rate
- `watch.toolProcsMax`, `watch.tools` (default `1000`; kubectl, helm, tsh, gh, flux, devctl): LOAD
  over this many processes of these CLIs machine-wide; negative: off
- `desktop.typingQuiet` (default `30s`): How long the person's input stays idle before a `claude://`
  link switches the desktop's window; negative: links do not wait for it
- `watch.tmpMaxMiB`, `watch.diskMinMiB` (default 45% of `/tmp`, 5% of `/`): TMPFS, LOW DISK
- `watch.diskCriticalMiB` (default 1% of `/`): DISK NEARLY FULL
- `watch.diskFillWithin` (default 2h): DISK FILLING: `/` would run full within it at the rate its
  free space fell over the last ten minutes; the line names the commands and sessions whose
  processes wrote most (`write_bytes` of `/proc/<pid>/io`) and the part of the loss no process the
  watch can read accounts for (another user's, such as dockerd, or ended ones)
- `disk.prune`, `disk.every` (default `auto`; `1h`): Automatically reclaim unused build caches,
  ended tasks' output and worktrees, harness worktrees, stale run tmp and trash; pressure also
  triggers the step, at most once per five minutes. `ask` retains the prior doctor/on-demand
  behavior.
- `disk.caches` (default `go-build`, `sccache`, `beekeeper-tmp`): Cache policies of the disk step: a
  path or glob, a mode (`entries`, `files`, `command` with the tool's `command`, `off`),
  `olderThan`, `maxGiB`, `regenerable`; an entry named like a built-in changes it. See [cache
  policies](development.md#cache-policies).
- `disk.checkouts`, `disk.buildDirs`, `disk.buildOutputs`, `disk.trashDirs` (default known harness
  checkouts; standard build dirs; no external output; state trash dirs): Additional checkout places,
  regenerable directory names, external output with its owning worktree, and replacement trash
  places; see [automatic disk reclamation](development.md#automatic-disk-reclamation).
- `doctor.goCacheMaxGiB`, `doctor.goCacheEvery` (default `20`; `1h`): The cap of the shared Go build
  cache and how often a watch reads its size; over the cap it is trimmed, least recently used first,
  to three quarters of it while no go build runs, and past the cap by a quarter while builds run
  ([Agents started without a click](agents.md#agents-started-without-a-click)); negative: off
- `doctor.dockerImagesMaxGiB`, `doctor.dockerBuildCacheMaxGiB`, `doctor.dockerEvery` (default `40`;
  `20`; `1h`): The caps of Docker's images and build cache and how often a watch reads their sizes;
  over a cap the unused images created or tagged longest ago (a kind lab's node's copies last), or
  the least recently used build cache, go down to three quarters of it, never an image a container
  uses or one created or tagged within 24 hours or since a held lab lease's claim, nor while an
  image import runs or a lab lease is under an hour old ([Agents started without a
  click](agents.md#agents-started-without-a-click)); negative: off
- `doctor.worktreeRoots`, `doctor.worktreesEvery` (default `[~/.local/state/beekeeper/worktrees]`;
  `6h`): Where a watch looks for workers' git worktrees and how often: one whose pull request merged
  or closed six hours ago is removed, or loses only its build output while it holds uncommitted or
  unpushed work, as does any other untouched six hours; a live session's and a locked one stay
  ([Agents started without a click](agents.md#agents-started-without-a-click)); negative: off
- `doctor.freeEvery` (default `1h`): How often a watch frees dead sessions' scratch and orphaned
  go-build dirs in the tmpfs `/tmp` (`free --apply --only session-dirs,go-build`); negative: off
- `ollama.url`, `lemonade.url` (default unset: no model server): The host's model servers, watched
  and guarded under the `model-server` lease
- `outbound.phrases`, `outbound.paths`, `outbound.storeDeny` (default none): What never leaves the
  machine, the plan files whose writes are outbound, the refused secret-store writes ([What leaves
  the machine](threat-model.md#what-leaves-the-machine))
- `secret.vault`, `secret.tokenFile` (default none): The shared 1Password vault `beekeeper secret`
  reads and writes, and the file with its service account's token ([Secret
  operations](secrets.md#secret-operations))
- `secret.moveVaults` (default none): Explicit destination vault names or IDs for whole-item `secret
  move`, for example `["Team Shared"]`. Requires `secret.vault`. Does not grant general field writes
  or reads; sources must be in `secret.vault` or `secret.vaults`. Every move goes through the broker
  and desktop app authentication, even without `secret.session`
- `secret.vaults` (default none): The shared vaults besides `secret.vault` that an `op://` reference
  is read from (`copy`, `compare`, `fingerprint`), through the same session or service account, such
  as a team's shared vault; field writes go to `secret.vault` alone (`secret.moveVaults` permits
  whole-item moves separately), and a read of any other vault is refused, naming these. The list is
  the person's policy
- `secret.session` (default `false`): Read and write `secret.vault` through the person's `op`
  session, held by the broker alone, instead of a service account ([The vault
  session](secrets.md#the-vault-session))
- `secret.signinCommand` (default none): The command the broker runs to sign in to the vault without
  the person; it prints the session as `op signin` does ([The vault
  session](secrets.md#the-vault-session))
- `secret.sessionLifetime` (default `12h`): How long the broker holds the vault session after a
  sign-in ([The vault session](secrets.md#the-vault-session))
- `secret.unlockWait` (default `8m`): How long a call on the vault waits for the broker's sign-in,
  and how long one sign-in waits for the credential store and tries again after a failure ([The
  vault session](secrets.md#the-vault-session))
- `secret.ageIdentities` (default none): Age identities in the shared vault, in an identity file
  (`file://`) or in the person's own credential store (`store://`), by recipient or `pathRegex`, for
  the SOPS files no local sops identity decrypts; a recipient no entry names has its identity in the
  vault's item `sops age key <recipient>` ([Age identities](secrets.md#age-identities))
- `secret.ageVaults` (default none): The vaults besides `secret.vault` that age identities are read
  from: an `op://` entry of `secret.ageIdentities` may name one, and a SOPS file at any path finds
  its identity in the item `<installation>.agekey` of `secret.vault` or one of them holding the
  identity of its recipient ([The installation's item](secrets.md#the-installations-item))
- `secret.store.read`, `secret.store.search` (default none): The person's own commands that read an
  entry of their credential store and search it by an age recipient, for `store://` credential
  sources and age identities ([Age identities](secrets.md#age-identities))
- `secret.fileRoots` (default none): The directories (`~/` allowed) whose plain files a
  `file://<absolute path>#<path>` source reads one value of, a CLI's own credential directory for
  one; a file outside them, its symlinks resolved, is refused, naming them, and none configured
  refuses every `file://` source
- `secret.files` (default none): Files known to hold secret values (`~/` and globs allowed) that no
  agent reads whole, beside the built-in list ([Secret reads](secrets.md#secret-reads))
- `secret.unlockCommands` (default none): The person's own vault unlock helpers, refused in agent
  sessions like `op signin` and unaliased in the agent shell ([Secret
  reads](secrets.md#secret-reads))
- `secret.env` (default none): Names (globs, such as `*_TOKEN`) of environment variables that carry
  a secret: the producer of `beekeeper secret capture` and of a `cmd://` source runs without them;
  it keeps only the locale and `TZ` of the caller's environment in any case
- `sandbox.allowRead`, `sandbox.allowWrite`, `sandbox.domains` (default none): The paths under the
  home directory the agent sandbox re-allows for reading and writing, and the hosts commands reach
  besides GitHub ([The agent sandbox](sandbox.md#the-agent-sandbox))
- `sandbox.proxyPort` (default `3190`): The port of the broker's egress proxy on `127.0.0.1`, the
  sandbox's only way out ([The agent sandbox](sandbox.md#the-agent-sandbox))
- `sandbox.required` (default `false`): Refuse every tool call but `SendMessage` of a session the
  agent sandbox's policy does not hold ([The agent sandbox](sandbox.md#the-agent-sandbox))
- `sandbox.devctl` (default `devctl` on the broker's `PATH`): The devctl the broker runs on the
  host: it renews the egress proxy's GitHub token and runs their gated devctl commands ([The agent
  sandbox](sandbox.md#the-agent-sandbox))
- `scan.sops`, `scan.vaults`, `scan.minLength` (default none, none, 12): The SOPS file globs and
  1Password vaults `beekeeper scan index` fingerprints, and the shortest value it takes ([What
  reaches the model](threat-model.md#what-reaches-the-model))
- `plans.repositories`, `plans.check` (default none, `plan-stages`): The plans repositories whose
  pull requests a note for `guide.person` links only once merged, or open with their stage check
  green or red only for missing stage outputs (the grill stage)
- `apps.repo`, `apps.manifest` (default unset: `app allow` and `app create` refuse): The repository
  that declares the organisation's GitHub Apps and an App's manifest path in it
  (`apps/{name}/manifest.json`, `{name}` for the App's name); `installation.json` next to the
  manifest declares its installation ([Agents started without a
  click](agents.md#agents-started-without-a-click), [Secret
  operations](secrets.md#secret-operations))
- `apps.template` (default `apps.manifest`'s file as `manifest.template.json`): The path in
  `apps.repo` of a purpose's manifest template, `{name}` for the purpose, whose strings carry
  `{installation}` and `{baseDomain}` (`app create <purpose> --installation <name>`, [GitHub
  Apps](github-apps.md#github-apps))
- `apps.secrets` (default none: `secret github-app` refuses): By installation, `{file, name,
  namespace}`: the SOPS file (absolute, `~/` allowed) of the Kubernetes Secret `beekeeper secret
  github-app` writes an App's `private-key` and `client-secret` into, and the Secret an absent file
  starts as
- `outbound.sweepRoots`, `outbound.sweepDepth`, `outbound.sweepEvery` (default the home directory,
  5, 15m): Where and how often the watch looks for exposed keys and credentials in remote URLs

State lives in `$XDG_STATE_HOME/beekeeper/` (`state.json`, which an older beekeeper still running
writes back with the fields it does not know at every level, `events.jsonl`, whose `at` is RFC 3339
in UTC while `beekeeper log` prints local times, each caller's last snapshot, the alert baseline
`alerts.json` with its owner's `alerts.lock`, the notification ledger `notify.json` with
`notify.lock`) and leases in `leases/`, one directory per held resource.

`state.json` is written under `state.lock`, whose writer records itself in `state.lock.holder` while
it holds it; a command waits for the lock at most two minutes (a build run's log line one second,
the SessionStart hook two) and then fails naming the holder's PID, command and how long it has held
the lock. The desktop import freezes a session's headless turns only while no process holds the
lock, so a frozen turn never holds it.

Relayed instructions to a running Claude session are stored separately from the
peer notification. After `beekeeper install`, the session receives them as chat
context at its next tool call or turn boundary (`Stop` or `UserPromptSubmit`). A
`Stop` with pending instructions continues the session once with those
instructions. The peer notification only wakes an idle session; it cannot repeat
an instruction already delivered by a hook. `agents wake` and intercepted
`SendMessage` results report `queued` until a hook consumes the instruction, or
`delivered` when consumption has already occurred. These receipts indicate
injection into the session context, not completion of the requested action.

## Scoped alert ownership

`beekeeper alerts own AlertName --cluster cluster-a --namespace namespace-a`
owns only currently firing instances with both exact label values. Either
matcher can be omitted; an alertname without matchers still selects every
firing instance of that name. Ownership remains tied to each instance's
fingerprint and firing start, and ends when it resolves or its owner ends.
Use the same optional matchers with `beekeeper alerts disown` to release a
selection. `beekeeper alerts owners` lists ownership records with their
cluster and namespace matchers; an empty matcher means unrestricted.
