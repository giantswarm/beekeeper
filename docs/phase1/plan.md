# Phase 1 implementation plan

Status: **approved** · 2026-11-14
Builds what the phase 1 contract, [scenarios.md](scenarios.md), specifies: the foundation and a short dry run for team Bumblebee🐝, then phase 2. Bare section
numbers (§17) refer to the [design](../design.md), "contract §n" to the contract.

## 1. Where everything goes

| What | Where | Why |
|---|---|---|
| Code, CRDs, chart, scenarios, docs, the design | `giantswarm/beekeeper` (public), one PR from a worktree on branch `feat/engine` | the engine replaces the old system there |
| Deployment: namespace, Postgres, secrets, config objects, webhook route, runbook | the deployment repository (private) | deployment details stay out of the public repo |

- **Before the merge:** the old system's services are stopped on every machine that runs them (or their auto-install is paused), and `v0.175.0-rc.513` is
  promoted (`devctl release promote giantswarm/beekeeper`) as the old line's last stable release. Otherwise old installations, which follow release candidates,
  would try to install `v1.0.0-rc.1` on every pass. The old central instance (`beekeeper serve` behind muster, from the chart in giantswarm-catalog) must not
  pick up `v1` either: pin it or remove it.
- **The replacing PR.** One PR deletes the old code and adds the new. Its title `feat!: replace beekeeper with the deterministic engine` makes auto-release tag **`v1.0.0-rc.1`**
  as a pre-release (git-cliff bumps 0.174.6 to 1.0.0; the old `v0.175.0` cycle can't be promoted after that).
- **What stays:** LICENSE, DCO, SECURITY.md, CODEOWNERS, `.github/pull_request_template.md`, `.gitattributes`, `.gitignore`, the gitleaks, yamllint and nancy
  files, `renovate*.json5`, `cliff.toml`, `.github/release-gate.custom.sh`, the devctl-generated files (`zz_generated.*` workflows, `Makefile`,
  `Makefile.gen.*.mk`, `.circleci/config.yml` and `workflows.yml`, `.devctl/ci-batch.yaml`, `.pre-commit-config.yaml`, `.golangci.yaml`), the Dockerfile,
  `pkg/project` (the ldflags target) and the chart name `helm/beekeeper`.
- **What's rewritten:** `.circleci/custom.yml`, `Makefile.custom.mk`, `README.md`, `AGENTS.md` (keeping its devctl block), `CONTRIBUTING.md`, the chart's
  contents, `docs/`. The old `plugin/`, `.claude-plugin/`, `contrib/systemd/`, `config/`, `cmd/`, `internal/`, `pkg/apis/` and old CRDs go.
- **Required checks.** The repo's ruleset requires the CircleCI jobs `pin-version`, `go-build`, `build-chart`, `build-image` (generated, unchanged) and
  `test-envtest`, `test-stable-version`, `test-loop-workflow`, `test-large-history`, `chart-test` (repo-owned). The PR keeps those names, each running a real
  part of the new suite (§9), so the ruleset needs no change now. Renaming them is a later change to Giant Swarm's repository configuration.

## 2. Go module and layout

Module `github.com/giantswarm/beekeeper` (unchanged), Go 1.27, controller-runtime 0.25, client-go 0.37. One binary, `beekeeper`, with roles as subcommands:
`engine`, `receiver`, `migrate`, `replay`, `version`.

```
main.go                      → cmd.Execute()
cmd/                         flags and wiring per role (no logic)
api/v1alpha1/                Installation, Team, Person, Item, Change, Budget + deepcopy
config/crd/                  generated CRDs (controller-gen), copied into the chart
internal/clock/              Clock, fake clock, timer queue (channel source for controllers)
internal/decide/             pure functions: phases, eligibility, order, picks, gates, waits, deadlines
internal/decision/           decision points: rules, modes, inputs hashing, JEV requests as effects
internal/forge/              read-only forge interfaces and types
internal/forge/github/       REST and GraphQL client: App JWT, installation tokens, ETags, budget buckets
internal/observe/            board sync, change sync, freshness (delivery log, probe), polling
internal/wake/               refs from deliveries, field indexes for the wake index
internal/inbox/              Postgres inbox: receiver writes, the leader drains (LISTEN/NOTIFY)
internal/receiver/           the hardened webhook handler
internal/history/            events and decisions writer with an in-memory buffer; embedded migrations
internal/controller/         reconcilers: Item, Change, Team (decision pass), Installation (sources), Budget
internal/metrics/            collectors; invariants computed at collection
internal/jev/                JEV (TypeSafe `systemone`) client
internal/testing/harness/    runner, strict loader, step tools, k8senv pool, embedded Postgres
internal/testing/fakes/      GitHub (REST, GraphQL, webhooks, delivery log, faults), JEV
internal/testing/scenarios/  the YAML scenarios
helm/beekeeper/              the chart
docs/                        design, phase 1 docs, operations
```

- Few dependencies: controller-runtime, client-go, pgx, prometheus client, yaml.v3, golang-jwt. No GitHub SDK: the subset of REST and GraphQL the engine uses is
  small, and owning it keeps ETags, budgets and operation names explicit.
- Test-only dependencies: k8senv, embedded-postgres, setup-envtest (for the RBAC tests).

## 3. CRD types and `decide`

- **Types** as in contract §5.1, group `beekeeper.giantswarm.io/v1alpha1`. That's the old group too, but none of the old kinds are reused, and the old CRDs are removed from the chart.
  - `spec` is configuration, applied by Flux: Installation (org, board, mode, intervals, waits, budget, decision-point modes, JEV), Team (board option, opt-ins, cap), Person (login, teams, WIP).
  - Item, Change and Budget are engine-owned: an empty `spec`, everything in `status`, identity in labels. Names are derived from refs (`giantswarm-roadmap-101`, `giantswarm-klaus-pr-55`).
- **`decide` is pure.** Three functions, each taking a view assembled from the informer cache plus recorded answers, and returning a decision:
  - `decide.Item(ItemView) ItemDecision`: phase, eligibility, dependency wait.
  - `decide.Change(ChangeView) ChangeDecision`: CI and review verdicts, merge result, wait.
  - `decide.Team(TeamView) TeamDecision`: order, picks, held items, would-actions, decision-point questions.

  A decision holds the status to write, the history rows to append and the **effects** to run: reads to schedule, timers to set, JEV questions to ask.
  Reconcilers are the impure shell that applies them. No clock reads inside `decide`: `now` is part of the view.
- **Replay.** `beekeeper replay` re-runs every recorded decision from its stored inputs and reports mismatches; the `replay` scenario tool calls the same code.

## 4. The webhook receiver (§17)

- A separate Deployment (`beekeeper receiver`, 2 replicas) with its own ServiceAccount, which may do nothing in Kubernetes. It's reachable only through the public route.
- Only `POST /github/webhook`. The body is capped at 10 MiB (`MaxBytesReader`). `X-Hub-Signature-256` is checked with a constant-time compare against the secret
  mounted from a Secret file. Deliveries are deduplicated by `X-GitHub-Delivery` (`INSERT … ON CONFLICT DO NOTHING`). Stored, then 2xx; 503 if Postgres can't
  take it. Nothing from the payload is trusted beyond the ids the engine reads again.
- Health and metrics on a second port that the route doesn't expose.
- Its database role may only `INSERT` into `inbox`.
- At the gateway: one hostname, one path, TLS, a request-size limit and a local rate limit (Envoy `BackendTrafficPolicy`). Source-IP allowlisting isn't reliable
  where the CNI rewrites source addresses across nodes (as Cilium can), so the signature is the control that counts.

## 5. Postgres

One CloudNativePG `Cluster` (1 instance, 10Gi), databases and roles created by the operator. Migrations are embedded and run by `beekeeper migrate` as an init container of the engine.

| Table | Columns | Notes |
|---|---|---|
| `inbox` | `delivery_id` (pk), `event`, `action`, `received_at`, `payload` jsonb, `processed_at` | a `NOTIFY` trigger wakes the leader; processed rows go after 3 days (GitHub's log retention) |
| `events` | `id`, `at`, `team`, `item`, `change`, `type`, `source`, `data` jsonb | append-only; indexes on `(item, at)`, `(change, at)`, `(type, at)` |
| `decisions` | `id`, `at`, `point`, `subject`, `other`, `inputs` jsonb, `inputs_hash`, `rule`, `jev`, `jev_confidence`, `jev_error`, `chosen`, `mode`, `outcome` | unique, see below |
| `durations` | `kind` (ci/stage), `key` (repo or team+phase), `value`, `at` | the deadline arithmetic's history |

`decisions` is unique on `(point, subject, inputs_hash)`, and replay reads it.

Roles: `engine` (all of the above) and `receiver` (`INSERT` on `inbox`). While Postgres is down, history rows are buffered in memory (10,000, then dropped and counted).

## 6. Metrics and alerts

- The metrics of contract §5.3, plus controller-runtime's own. Invariants are computed when Prometheus scrapes, from the cache and the clock.
- A `PrometheusRule` in the chart: any invariant above 0 for 10 minutes, reconcile errors, no leader, a source degraded for 30 minutes, receiver 5xx, dropped
  history, and a watchdog for the dead-man's switch. The rules have `promtool test rules` tests in the repo. Labels and receivers that follow the cluster's
  conventions (such as `cluster`, `layer`, a heartbeat receiver) come from values.
- In the deployment repository, alert rules are merged only after the first scrape, as its CI requires.

## 7. Configuration through Flux

In the deployment repository, following its rules (branch, PR, green checks, squash merge, no `--admin`, then confirm Flux applied it):

- The app's directory: the namespace (pod security baseline), network policies, the CNPG cluster, an `OCIRepository` for the chart (semver range `>=1.0.0-0`, so
  release candidates roll in) and the `HelmRelease`.
- Secrets, SOPS-encrypted with the cluster's age recipient: the GitHub App's private key, its webhook secret, and the JEV key. The operator enters them through
  a short script that encrypts what is pasted; no agent sees the values, and nobody needs the private age key for this.
- The configuration objects (Installation for `giantswarm` and board 273, Team `bumblebee`, the Persons) in a child Kustomization that `dependsOn` the
  HelmRelease, so a missing CRD never blocks the cluster-wide dry run that Flux does.
- **The public webhook route:** `<webhook-host>`, with a listener on a public Envoy gateway, the name on its certificate, a DNS record, and an `HTTPRoute` plus
  `BackendTrafficPolicy` in the `beekeeper` namespace.
- The configuration: Installation `giantswarm` (board 273, `mode: dry-run`), Team `bumblebee` (`Bumblebee🐝`, no cap), and one Person, the participant (`wip: {items: 1, epics: 1}`).

## 8. Network policies (CiliumNetworkPolicy, default-deny cluster)

| Component | Ingress | Egress |
|---|---|---|
| engine | Prometheus (`fromEndpoints`, mutual auth), kubelet probes | kube-dns 53/1053, kube-apiserver, `api.github.com` and `api.typesafe.ai` (`toFQDNs` with a DNS rule), Postgres 5432 |
| receiver | the Envoy gateway (`fromEndpoints`, mutual auth), Prometheus, probes | kube-dns, Postgres 5432 |
| Postgres | engine, receiver, the CNPG operator, Prometheus | kube-dns, kube-apiserver |

Every `fromEndpoints` ingress rule carries `authentication.mode: required`, as the cluster's Kyverno policy requires. The engine sets
`automountServiceAccountToken: true` explicitly, because that policy turns it off by default. The chart ships the policies; their selectors for Prometheus and
the gateway come from values.

## 9. Tests and CI

- `make test` runs unit tests and the scenarios (`go test ./...`). The k8senv binaries (kube-apiserver, kine) and the Postgres binary are fetched once into a cache by `make test-deps`.
- The required CircleCI job names map to the new suite:

  | Job | Runs |
  |---|---|
  | `go-build` | `make test` (unit tests and scenarios) and the cross-builds |
  | `test-loop-workflow` | the scenarios again with `-race` |
  | `test-envtest` | the envtest RBAC tests and the chart's CRDs against the API types (`make check-crds`) |
  | `test-large-history` | replay over a generated history of 10,000 decisions |
  | `test-stable-version` | `promtool test rules` and `helm template` with production values |
  | `chart-test` | the chart's own tests |

## 10. Order of work, and what runs in parallel

The lead session keeps the API types, the interfaces (`forge.Reader`, `Clock`, the `decide` signatures, the harness's tool registry) and the integration. Subagents build pieces against them:

1. **Lead session:** the worktree, the deletion, `api/v1alpha1`, the interfaces, the module skeleton.
2. **In parallel:**
   - clock and timer queue
   - `decide` and the decision-point rules, with table tests
   - the fake GitHub
   - the GitHub client
   - receiver, inbox, history and migrations
   - JEV client
   - metrics and invariants
   - chart, network policies, alert rules with promtool tests
3. **Lead session:** the controllers, the harness runner and tools, wiring the roles.
4. **Until green:** failing scenarios go to subagents by concept; each fix has to keep the whole suite green and fast.
5. The design and the phase 1 docs in `docs/`, without deployment-specific details.
6. An independent review of the PR (a fresh reviewer that sees only the code and the contract), then the maintainer's go to merge.

## 11. The dry run

1. The deployment PR with the namespace, Postgres, policies and route; the secrets entered by the operator.
2. The App's webhook URL and secret set to the new route, and its installation checked against the permissions in §12.
3. `v1.0.0-rc.1` rolls in; the engine starts in `mode: dry-run`.
4. A few days of observation. Exit criteria: no invariant violated, every webhook gap detected and caught up, the API spend inside the bounds the budget
   scenarios assert, and the queue it would work through for the participant looks right to them.
5. **Done:** a short runbook in the deployment repository, and a report of the GitHub API calls per hour it actually made (by API, class and operation, from Prometheus).

## 12. The GitHub App

The engine uses beekeeper's **worker app**, which so far only served device-flow user logins. For the engine it acts as itself, with installation tokens:
- **The App's owner, in its settings:** set the webhook URL (`https://<webhook-host>/github/webhook`) and a new webhook secret, subscribe the events of contract
  §1.3, generate a private key, and check that the App is installed on the giantswarm org for the board's repos.
- **Permissions it needs for phase 1, all read:** Metadata, Contents, Issues, Pull requests, Checks, Commit statuses, Actions, and Organization → Projects.
  Phase 1 never writes, whatever else the App may do.
- **Into SOPS, entered by the operator:** the App id, the installation id, the private key, the webhook secret, and the JEV key.

## 13. Platform checks (asked for before touching any repo)

- **§7.3, the App bypass:** whether `giantswarm-devctl` acting as itself can bypass the review ruleset on one test repo. That needs its private key (handed
  over by a person) and a throwaway PR on a test repo. It only matters for phase 2's merges.
- **§8.4, devctl's verdict as a library:** today it can't be imported usefully. The logic is pure, but it's unexported, behind a blocking poll, and importing
  `pkg/prwait` drags in about 690 packages through `reposetup`. The smallest change is to export `Judge(Signals) Verdict` from a leaf package that depends only
  on go-github and `circleciconfig`, with `Wait` calling it. That's a devctl PR, for phase 2's merge gate.
