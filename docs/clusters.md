# Clusters

Kube contexts, production writes, the Teleport login and cluster upgrades.

## Kube contexts and production writes

`kube.productionNoteDeletes` (off by default) lets an answered decision note authorise one
production object deletion. **Switching it on trusts every recorded answer: nothing proves a person
gave it, so an agent able to record an answer can forge the authority for its own deletion.** Off, a
command carrying the marker below meets the ordinary production refusal.

With it on, put the complete literal command in backticks in the note's question, for example
`kubectl --context production delete job stale --namespace monitoring`, and record the answer `yes`,
`approve`, `approved` or `delete it`. Carry the note's id as:

```sh
BEEKEEPER_PROD_NOTE=42 kubectl --context production delete job stale --namespace monitoring
```

The hook requires that exact command, in that order, with literal context, kind, name and
namespace. Additional flags, shell syntax, selectors and multiple objects are refused, and so is
any context `kube.protectedContexts` lists. An execution host's hook never takes a note. Answers
expire after 24 hours.
The local state consumes the note atomically before execution and records `production.write`
with the note id and command in `beekeeper log`. A failed execution still consumes its authority;
a retry needs a new decision. Stores unable to persist the consumption refuse the write.

With `kube.production` or `kube.protectedContexts` set, the machine kubeconfig (`~/.kube/config`)
keeps no current context: a `kubectl`, `helm` or `flux` command without an explicit target fails
instead of reaching production. The PreToolUse hook keeps it that way and refuses, with the reason
and the explicit form (unset, the kube guard is off and `beekeeper snapshot` says so):

- A command that reaches a cluster through the machine kubeconfig without an explicit context:
  `kubectl` (every command but `config`, `completion`, `kustomize`, `help`, `options`, `plugin`,
  `version --client`, `--dry-run=client`, `--local`; a `--server` names its target), `helm`
  install/upgrade/uninstall/rollback/test/list/status/get/history and `flux` (all but `build`,
  `envsubst`, `completion`, the artifact commands, `--export`, `version --client`) with no
  `--context` (helm: `--kube-context` or `HELM_KUBECONTEXT`) or with one that may expand to nothing:
  `--context ""`, `--context "$CTX"` when the command does not set `CTX` to a non-empty word, a
  command substitution.

  The tools treat an empty context as none and use the kubeconfig's current one, which may be
  production. `--context "${CTX:?no context}"` fails on an empty lookup and passes; so do a literal
  `--context kind-<lab>` or any other name, and a kubeconfig of the command's own (`--kubeconfig <file>`,
  `KUBECONFIG=<file>`, the session's own `KUBECONFIG`), which is the target itself. kubectl plugins
  are not covered. A person's own shells stay outside the rule through `hooks.scope`, as for every
  guard.
- A context switch: `kubectl config use-context`, `kubectl config set current-context`, `kubectl
  ctx`, `kubectx <name>`, `kubectl gs login` without `--self-contained`; and `tsh kube login`, `tsh
  login --kube-cluster`, `kind create cluster` and `kind export kubeconfig` when they write the
  machine kubeconfig (a `KUBECONFIG` or `--kubeconfig` of the command's own passes). `beekeeper
  snapshot` names a current context the machine kubeconfig gained anyway.
- A write to production or a context listed in `kube.protectedContexts` (including full test
  installations), the clusters of the installation `kube.production` (a context or cluster name with
  it as a component, its workload clusters included): `kubectl`
  apply/create/patch/edit/delete/replace/scale/annotate/label/cordon/uncordon/drain/taint/set/expose/
  autoscale/run/exec/cp/attach/debug, `rollout restart|undo|pause|resume`; `helm`
  install/upgrade/uninstall/rollback/test; `flux` suspend/resume/reconcile/create/delete/bootstrap/
  install/uninstall.

  The target is the `--context`/`--kube-context`/`--cluster` given, else the current context of the
  kubeconfig the command uses (`--kubeconfig`, `KUBECONFIG` in the command or the session). Changes
  to production go through GitOps pull requests and platformctl; there is no break-glass, except the
  single-object deletion `kube.productionNoteDeletes` switches on (above). The one exec that passes
  is a read: a query listed exactly in `kube.readOnlyExecQueries`, run as `kubectl exec <pod> --
  psql -X [-U <user>] [-d <database>] -c '<query>'`, without `-i` or `-t`; every other exec is
  refused with the reason named.

  Reads (`get`, `describe`, `logs`, `top`, `auth can-i`, `diff`, `--dry-run=server`; `helm
  list|status|get|template`; `flux get|logs`) and writes to any other cluster pass.
- A kubectl plugin's write to production: `kubectl <plugin> …` (a first word that is no kubectl
  command) and a `kubectl-<plugin>` binary (a path or `go run ./cmd/kubectl-<plugin>` included),
  with the same targets as kubectl. `kubectl ate delete actor …`, `kubectl-ate admin make-ca-pool …`
  and `kubectl gs update app …` are refused; a plugin subcommand that reads (`get`, `list`,
  `describe`, `logs`, `top`, `status`, `version`, `template`, …) and the read-only plugins (`tree`,
  `access-matrix`, `resource-capacity`, `who-can`, `neat`, `krew`, `oidc-login`, `ns`) pass.

  Every other plugin subcommand counts as a write. `kubectl-gs template cluster` passes against any
  context, including with value-taking global flags before `template` (`--cache-dir`,
  `--certificate-authority`, `--client-certificate`, `--client-key`, `--as-uid`,
  `--tls-server-name`). Those values never select a read or write subcommand.

Each guard sees through prefix commands, pipelines and lists, `$( )`, shell `-c` strings and
here-documents fed to a shell, and kubectl or a kubectl plugin run through a shell function or a
variable. Quoted text (a commit message, an issue body) passes.

## The Teleport login

When the kube contexts reach the installations through Teleport (`tsh kube credentials` as their
exec plugin), all of them hang on one `tsh` login, and an SSO login's certificate lives only as long
as the role's maximum session TTL. With `teleport.proxy` set (and `teleport.auth`, the SSO
connector), beekeeper keeps that login:

- `beekeeper teleport` prints the active profile's user, cluster and expiry from `tsh status
  --format=json` (metadata only, never key material) and what the keeper last did; it exits 3 while
  the login needs a person (expired, under `teleport.warnBefore`, or the keeper's renewal failed).
  `beekeeper snapshot` shows the same line, and `beekeeper watch` says `TELEPORT LOGIN` once under
  `teleport.warnBefore` (default 1h), `TELEPORT LOGIN EXPIRED` once it expired and `TELEPORT RENEWAL
  FAILED` after a failed renewal, each with one ENDED line once renewed. A merge gate refusal for an
  unreadable installation names an expired login as its cause.
- `beekeeper install` writes the keeper, `beekeeper-teleport.timer`, which runs `beekeeper teleport
  renew --keeper` every `teleport.every` (default 10m, rounded down to whole minutes, or whole hours
  from 1h); it renews once less than `teleport.renewBefore` (default 90m) is left. The timer is a
  calendar event on the wall clock (`OnCalendar=*:0/10`, `Persistent=true`), which goes on through a
  suspend: the tick missed while the machine slept fires within seconds of resume, and renews a
  login that expired meanwhile. `uninstall` stops and removes it with the other units.
- A renewal runs `tsh login --proxy=<proxy> --auth=<connector>` in an empty staging home, because a
  valid profile makes `tsh login` print its status only. tsh opens the login URL in the default
  browser, and a browser holding the SSO session completes it without a click; nothing types
  credentials. Once the new profile is valid, the staging home and the profile directory
  (`teleport.home`, default `$TELEPORT_HOME` or `~/.tsh`) swap in one step, so the old certificate
  serves until the new one is in place; the profile before is kept as `teleport/previous` in the
  state directory, and the person's kubeconfig is not touched. tsh's output carries the one-time
  login URL: it goes to `teleport/login.log` (mode 0600) and is never printed. Backup cleanup and
  preparation finish before the active profile changes; a failure preparing the backup leaves the
  active login available.
- The renewal holds the browser lease. A session renewing by hand (`beekeeper teleport renew`)
  claims it first; the keeper claims it itself when it is free and granted to nobody, and otherwise
  waits for its next run, which the `TELEPORT LOGIN` line names. Only one renewal runs at a time on
  a desk, including commands from the same session. A second manual command is refused while it
  runs; the keeper leaves that renewal in charge and tries at its next run.
- A login that does not complete within `teleport.loginTimeout` (default 3m: the browser's SSO
  session is gone, or the sign-in needs a click) leaves the profile as it was. Re-run `beekeeper
  install` after increasing this timeout so the keeper's unit allows both login attempts and
  cleanup. The keeper does not
  try that login again: it leaves a sign-in note for `guide.person` that closes by itself once the
  login is renewed, and the watch says `TELEPORT RENEWAL FAILED`. The run after a missed tick (more
  than two `teleport.every` since the keeper's last run) tries a failed login again, and a failure
  of that run, which may meet a network not yet back from the suspend, is tried once more at the
  next run before the note; the watch says `TELEPORT RENEWAL FAILED` meanwhile.
- A login whose SSO callback exchange with the proxy timed out after the browser's sign-in
  (`identity provider callback failed` … `Client.Timeout exceeded while awaiting headers`) is
  retried once, in the same staging home, before the renewal fails: the first attempt is logged as
  `teleport.retry`, and the second appends its output to `teleport/login.log`.

What the keeper needs: a graphical session whose default browser holds the SSO session (the keeper's
unit starts after `graphical-session.target` and inherits the user manager's display variables), and
the profile directory on the same filesystem as the state directory. A headless desk needs a
Machine ID bot (`tbot`) from the Teleport administrators instead. Configure bot mode below
for a supervised user service with no browser or browser lease.

### Teleport Machine ID

Ask the Teleport administrators for a bot with a role restricted to the required Kubernetes
clusters, Kubernetes users/groups and permissions, plus a join token and the joining instructions.
Install a `tbot` version compatible with the Teleport cluster. Have the administrators supply a
`tbot` configuration with persistent bot storage, onboarding using that token, a Kubernetes
output for the required clusters, and an identity output containing a public TLS certificate.
Keep this configuration and its token private (mode 0600); the token belongs in that file or a
protected token file it references, never in beekeeper's command line. Preserve the persistent
bot storage across service restarts; do not share it between bot instances.

```yaml
teleport:
  bot:
    config: ~/.config/teleport/tbot.yaml
    binary: tbot
    certificate: ~/.local/share/teleport/identity/tlscert
    kubeconfig: ~/.local/share/teleport/kubernetes/kubeconfig.yaml
```

`bot.config` selects Machine ID instead of SSO, even if `teleport.proxy` remains set.
`bot.binary` defaults to `tbot`. `bot.certificate` must name the public certificate in the
configured identity output (commonly `tlscert`; use `tls.crt` if that output enables specific TLS
extensions). `bot.kubeconfig` must name the generated Kubernetes output kubeconfig. All three
file paths are required and accept `~/`. The output paths must match the administrator's tbot
configuration. Its Kubernetes output defines the available context names; use those names in
beekeeper's installation context configuration.

Run `beekeeper install` after changing modes or configuration. On Linux with systemd,
it installs `beekeeper-teleport.service` running `tbot start --config=...`, enabled at user manager
startup and restarted after exit. The existing timer can also start the service; it performs no
SSO renewal in bot mode. To keep it running without a logged-in session, arrange user lingering
with the system administrator. Bot supervision requires a systemd user manager.

Beekeeper prepends the generated kubeconfig to its installation and alert kubeconfig lists.
For other tools, include that file in `KUBECONFIG` or select it explicitly; existing user contexts
are preserved. `beekeeper teleport`, `watch`, and `snapshot` read the public certificate's expiry
and report an inactive bot service, missing/invalid certificate, or approaching/expired identity.
`teleport.warnBefore` controls the warning threshold. `beekeeper teleport renew` refuses SSO
renewal in bot mode; tbot owns renewal. Inspect the user service locally for diagnostics if it
fails. If the bot identity has expired, ask the administrators to reset/rejoin the bot as required.

See the [Teleport Machine ID configuration
reference](https://goteleport.com/docs/reference/machine-id/configuration/) for the tbot storage,
onboarding and output settings.

## Cluster upgrades

While an installation's clusters upgrade, work on it waits. `watch` reads the Cluster API clusters
of `alerts.installations` every `upgrades.every` (5m), read-only: one list of Clusters,
KubeadmControlPlanes, MachinePools and MachineDeployments (`v1beta2`) per installation in one
`kubectl` call, in parallel, each installation within `alerts.timeout`; an installation that does
not serve one of the kinds is read one call per kind, and one that serves none of them has no
cluster to upgrade.

An installation an upgrade runs on or holds is read every `watch.interval` (30s) until it ends, so
`UPGRADE ENDED` and the hold's lift come within one interval. That is one `kubectl` per installation
per 5 minutes while nothing upgrades, and one list of a cluster's events when its upgrade begins.
The readings are kept in `upgrades.json` in the state directory: a second watch, `snapshot` and `ui`
use a reading younger than `upgrades.every` instead of reading the installation again.

The supervisor's watch reads them; while none runs (a relay, a crash, a frozen desktop) the [standby
watch](notifications.md#desktop-notifications) does, so an upgrade is held within one
`upgrades.every` either way.

A cluster's upgrade begins when its release changes: its `release.giantswarm.io/version` label
differs from the release cluster-api-events last recorded
(`giantswarm.io/last-known-cluster-upgrade-version`), cluster-api-events marks it upgrading
(`giantswarm.io/cluster-upgrading: "true"`, from the release change until its control plane and
workers have rolled), or its scheduled upgrade is due
(`alpha.giantswarm.io/update-schedule-target-release` differs from the label and `…-target-time` has
passed).

It ends once none of that holds and its control plane and node pools have rolled (every
KubeadmControlPlane at its `spec.version`, every control plane and node pool with all replicas up to
date, no `RollingOut` condition True). A marked upgrade is over, though the mark stays, once the
cluster reports the release cluster-api-events recorded and has rolled out for an hour since
cluster-api-events stamped its start (`giantswarm.io/upgrade-start-time`), or that start is a day
ago: a mark cluster-api-events never clears (a managed AKS control plane) or a stalled rollout holds
the installation no longer.

A rollout that runs on after the mark cleared keeps its hold at most a day from the hold's begin.
Either way the hold lifts on that reading, whether the release it upgraded from was read or not. A
workload cluster on an older release than its management cluster is not upgrading. The release it
upgrades from is the one the label changed from, else the one cluster-api-events' `Upgrading…` event
names.

While it runs, the installation is held: a hold `upgrade:<installation>/<namespace>/<cluster>` by
`beekeeper watch`, until the upgrade ends, whose reason names the cluster and both releases. The
merge gate refuses the merges of every lane whose `installation` it is (exit 77, with the reason),
`lease claim <installation>` is refused unless an upgrade-unblock grant admits it (below), `lanes`
shows the lanes held and `snapshot` the upgrade with its progress.

The watch whose update sets the hold says `UPGRADE …`, the one whose update lifts it `UPGRADE ENDED
…`, so a second or restarted watch says neither again. An unreadable installation is one `UPGRADES <installation>
unreadable: …` line until it answers again and keeps its holds as they are: it counts as neither
upgrading nor quiet. An installation whose kube context is not in the kubeconfig kubectl reads is
not read and keeps its holds too; the watch names all of them once, in one `UPGRADES: <n>
installations without a kube context, not read: …` line (`ALERTS: …` for the alerts reader), and
`snapshot` in one entry of its upgrades and one line of its alerts, never one line each.

`hold set` refuses an `upgrade:` target; `hold lift upgrade:<installation>/<namespace>/<cluster>`
lifts one by hand (a workload cluster's roll stuck on a drain need not stop the installation's
merges): the hold stays lifted, recorded with who lifted it, until that upgrade ends, and only a
different upgrade of the cluster (another target release) holds the installation again. `hold list`
and `status` show the lifted hold and who lifted it; `hold list` marks an upgrade hold that has
stood for a day `active, stale`, longer than the watch lets an upgrade it reads run: worth a look.

Legacy targets without a namespace are retired on the next successful reading. Detected upgrades
receive fresh active holds: an old lift cannot safely be assigned to a namespace.

The one claim an upgrade admits is the work that unblocks it: a node drain stalled on a
single-replica PodDisruptionBudget, for example, needs a pod deleted on that installation. The
supervisor grants it with `beekeeper lease grant <installation> <session> --upgrade-unblock
"<why>"`; beekeeper refuses that grant from any other session and while no upgrade holds the
installation. During the upgrade only such grants are claimed, in their order; a plain grant,
the supervisor itself and a person stay refused. The reason is kept on the grant and on the
lease: `lease list`, `handover` and the log's `lease.grant` and `lease.claim` show it next to
the claim's purpose.

A lab lease names its kind cluster under `labs` (`agentlab-1: agentlab`). `lease list` and
`handover` then show each lab lease with its cluster, whether that cluster runs and who holds
the lease, and list a running kind cluster no lab lease maps as unmapped; a claim of a lab
lease names the cluster it covers, and `free` marks a running cluster whose lease is free as
idle, with its last holder from the event log. Without `labs` nothing asks docker.

The doctor releases leases of ended local sessions, logging each release once.
It stops configured kind labs after 15 minutes without a lease, keeping their
containers and data. The next successful claim restarts stopped nodes before
returning the kubeconfig; a failed restart fails the command and keeps the lease
for a retry. Person-held leases, remote holders and unmapped clusters stay alone.
