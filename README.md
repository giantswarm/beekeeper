# beekeeper

beekeeper is a deterministic engine for the software development lifecycle. It runs in Kubernetes
as a set of controllers, observes a GitHub project board and the pull requests behind its items,
and decides what to work on next with plain code: every decision is a pure function of what the
engine observed, recorded with its inputs so it can be replayed. Agents are meant to be called only
for work that needs judgement or language; the engine itself never guesses.

The [design](docs/design.md) describes the whole system and the phases it is built in.

## Phase 1: a read-only dry run

This release is phase 1: the foundation and a short dry run for one opted-in team. The engine
observes the board and GitHub and records what it *would* do. It never writes to GitHub, sends no
messages and starts no work:

- the GitHub client it is built with is a read-only `forge.Reader`; the mutating methods do not
  exist on its type, and the only POSTs are GraphQL queries and installation-token minting;
- each tracked board item becomes an `Item` whose phase is derived from the board status, the
  issue and its pull requests; pull requests become `Change`s with their CI and review state;
- per team and person, the engine computes eligibility, order, WIP gates and picks, and records the
  would-actions once, in Postgres;
- decision points (`ready?`, `next?`, `parallel-safe?`, `bug-severity?`) are answered by a rule,
  with an optional second answer from JEV recorded next to it, in shadow;
- it watches its own health: source freshness, API budget, waits that do not wake, and invariants
  that raise alerts when they break.

What the dry run means in detail, and how it is tested, is in
[docs/phase1/scenarios.md](docs/phase1/scenarios.md).

## Architecture in brief

One binary, `beekeeper`, with roles as subcommands:

| Role | What it does |
|---|---|
| `engine` | the controllers (2 replicas, leader-elected): observation, decisions, timers, history, metrics |
| `receiver` | the hardened webhook endpoint: verifies each delivery and stores it in the Postgres inbox |
| `migrate` | applies the embedded Postgres schema (the engine's init container) |
| `replay` | re-runs every recorded decision from its recorded inputs and counts the ones that changed |
| `version` | prints the version |

- **Live state and configuration** are custom resources in `beekeeper.giantswarm.io/v1alpha1`
  ([`api/v1alpha1`](api/v1alpha1), CRDs in [`config/crd`](config/crd)). `Installation`, `Team` and
  `Person` are configuration, applied by Flux; `Item`, `Change` and `Budget` are owned by the
  engine and keep everything in `status`.
- **History** is an append-only event table and a decision table in Postgres. Webhook deliveries go
  through a Postgres inbox that the leader drains.
- **Time** comes from one clock ([`internal/clock`](internal/clock)); deadlines live on a timer
  queue that wakes the controllers. Tests use a fake clock that moves only when told to.
- **Decisions** are pure functions in [`internal/decide`](internal/decide): given a view of the
  cache, the clock and recorded answers, they return the status to write, history rows and effects.

The layout of the code is described in [AGENTS.md](AGENTS.md#layout).

## Build and test

Go 1.27 and `make`. The generated Makefile builds the binary for the host:

```sh
make build            # ./beekeeper
go run . version
```

The tests need a few binaries (kine and kube-apiserver for k8senv, envtest's assets for the RBAC
tests, and an embedded Postgres). `make test-deps` fetches each into `~/.cache/beekeeper/test-deps`
once, checked against its published checksum; `make test` runs it first. Tests never download.

```sh
make test-deps        # fetch the test binaries (once)
make test             # unit tests, the scenarios, the RBAC tests and the chart's unit tests
make test-race        # everything with the race detector
make test-envtest     # the chart's RBAC against envtest's kube-apiserver
make check-crds       # deepcopy code and CRDs match api/
make help             # every target
```

The behaviour is specified by YAML scenarios in
[`internal/testing/scenarios/`](internal/testing/scenarios), listed in their
[catalogue](internal/testing/scenarios/CATALOGUE.md). Each runs the real engine in-process against
a real kube-apiserver (k8senv), a real Postgres and fakes for GitHub and JEV, on a fake clock:

```sh
go test ./internal/testing/harness/ -run TestScenarios -parallel 2
go test ./internal/testing/harness/ -run TestScenarios/wait-ci-wakes-on-check-run -v
go test ./internal/testing/harness/ -run TestScenarios -scenario.concept wait
go test ./internal/testing/harness/ -run TestScenarios -scenario.tag learning-api-budget
go test ./internal/testing/harness/ -run TestScenarios/NAME -v -scenario.phases -scenario.logs
```

`-scenario.pool` (or `BEEKEEPER_SCENARIO_POOL`, default 2) sets how many API servers the scenarios
share; `-parallel` how many run at once. On a laptop, keep both at 2.

## Deploy

The chart is [`helm/beekeeper`](helm/beekeeper), published to the giantswarm-catalog; the image is
`gsoci.azurecr.io/giantswarm/beekeeper`. The chart installs the CRDs, the engine and the receiver
with their ServiceAccounts and RBAC, PodMonitors, a PrometheusRule with the engine's alerts, and
CiliumNetworkPolicies. It expects, in the release's namespace:

- a Postgres (for example a CloudNativePG `Cluster`) with an `engine` role and a `receiver` role
  that may only insert into the inbox (`postgres.*`);
- Secrets with the GitHub App's id, installation id and private key (`github.app.*`), its webhook
  secret (`webhook.*`) and, optionally, a JEV key (`jev.*`);
- a public route to the receiver's Service for GitHub's webhooks, on `receiver.path`.

```sh
helm install beekeeper helm/beekeeper --namespace beekeeper --create-namespace --values my-values.yaml
```

Then apply the configuration: an `Installation` (organisation, board, `mode: dry-run`, intervals),
the `Team`s and the participating `Person`s. Every value is listed in the
[chart's README](helm/beekeeper/README.md); [docs/phase1/plan.md](docs/phase1/plan.md) describes a
full deployment with Flux, Postgres, network policies and the GitHub App.

## Documentation

- [docs/](docs/README.md): the design, the phase 1 contract and the implementation plan
- [CONTRIBUTING.md](CONTRIBUTING.md): commits, the test suite and how to add a scenario
- [AGENTS.md](AGENTS.md): the layout, the seams and the rules for changing the code
- [helm/beekeeper/README.md](helm/beekeeper/README.md): the chart's values

## License

[Apache 2.0](LICENSE)
