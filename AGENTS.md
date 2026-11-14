<!-- BEGIN devctl-managed: edits in this block are overwritten -->
<!-- markdownlint-disable-file MD041 -->

## Giant Swarm conventions

devctl maintains this block from [its template][devctl-agents-block] and
overwrites changes to it. Instructions for this repository go in the section
below. Where they differ from this block, they take precedence.

[devctl-agents-block]: https://github.com/giantswarm/devctl/blob/main/pkg/gen/input/llm/internal/file/agents_block.md.template

- Never put customer names or other customer information in code, commit
  messages, issues or pull requests. Most Giant Swarm repositories are public,
  and private ones are kept ready to become public.
- Giant Swarm's style guides are in <https://github.com/giantswarm/fmt>, one
  directory per topic (go, helm, kubernetes, prometheus, …). Read the relevant
  guide before you write or review that kind of code.
- Use conventional commits for commit messages and pull request titles.
- If `.github/workflows/zz_generated.auto_release.yaml` exists, GitHub Releases
  are written from pull request titles: do not edit CHANGELOG.md. Otherwise,
  add your change to CHANGELOG.md as described in
  <https://github.com/giantswarm/fmt/blob/main/releases/changelog.md>.
- If `.pre-commit-config.yaml` exists, run
  `pre-commit run --files <changed files>` before committing; not every clone
  has the hooks installed.
- Do not add linters, formatters or other tooling unless asked; use what the
  repository already has.
- Put documentation in `docs/` rather than growing README.md, which gives an
  overview and links into `docs/`. When a change affects documented behaviour,
  update the documentation in the same pull request.
- If the skills of the `giantswarm/claude-code` plugin marketplace are not
  available to you and the user works at Giant Swarm, tell them once that
  they can install the `gs-base`, `gs-dev`, `gs-godev` and `gs-appdev`
  plugins from it.
- Go: run `goimports -local github.com/giantswarm/<repository>` on the files
  you change. CI with architect-orb fails when imports are grouped
  differently.

<!-- END devctl-managed -->

## This repository

beekeeper is a deterministic engine for the software development lifecycle: Kubernetes
controllers that observe a GitHub project board and its pull requests and decide with plain code.
Phase 1 is a read-only dry run. Read [README.md](README.md) first, then
[docs/phase1/scenarios.md](docs/phase1/scenarios.md) (the contract the scenarios encode) and the
[design](docs/design.md) for the why.

### Layout

| Path | What |
|---|---|
| `main.go`, `cmd/` | the one binary; flags and wiring per role (`engine`, `receiver`, `migrate`, `replay`, `version`), no logic |
| `api/v1alpha1/` | the CRD types (configuration: Installation, Team, Person; engine state: Item, Change, Budget) |
| `config/crd/` | the generated CRDs; `make generate` also copies them into `helm/beekeeper/crds/` |
| `internal/engine/` | wires one replica; the binary and the scenario harness build it the same way |
| `internal/controller/` | the reconcilers (Item, Change, Team, Installation, Budget) and the leader's observation loop |
| `internal/decide/` | the pure core: a view in, a decision (status, history rows, effects) out |
| `internal/decision/` | the decision points: their rules, inputs hashing, JEV in shadow |
| `internal/observe/`, `internal/wake/` | GitHub reads turned into observed facts, source freshness; deliveries mapped to wake refs |
| `internal/forge/`, `internal/forge/github/` | the read-only forge interface and the GitHub App client (REST, GraphQL, ETags, budgets) |
| `internal/receiver/`, `internal/inbox/` | the hardened webhook endpoint and the Postgres inbox the leader drains |
| `internal/history/`, `internal/migrations/` | events and decisions in Postgres, never blocking the engine; the embedded schema |
| `internal/clock/`, `internal/settle/` | the only source of time and the timer queue; tracking of every asynchronous unit of work |
| `internal/metrics/`, `internal/jev/`, `internal/replay/` | metrics and invariant evaluators; the JEV client; replay of recorded decisions |
| `internal/faults/` | engine bugs the scenarios inject to see the invariants fire; always off in production |
| `internal/rbactest/` | the chart's RBAC against envtest's kube-apiserver |
| `internal/testing/` | the scenario harness, the fake GitHub and JEV, `pgtest`, `scenariolint` and the scenarios |
| `helm/beekeeper/` | the chart; `hack/` the scripts behind `Makefile.custom.mk` |

### The seams

`engine.Options` takes every edge of the engine as an interface, and the harness swaps each one:

- `clock.Clock` (real or fake) and its timer queue;
- `forge.Reader` (the GitHub client, or the client pointed at the fake GitHub);
- `history.Writer` and `history.Reader`, `inbox.Drainer` (Postgres; a fresh database per scenario);
- `jev.Client` (the HTTP client, or the fake JEV);
- `settle.Tracker` (nil in production) and `faults.Faults`.

Nothing else is faked: the controllers, the receiver, the GitHub client and the Postgres writer are
the real ones, against a real kube-apiserver (k8senv) and a real Postgres.

### Scenario first

- A change of behaviour starts with a scenario under `internal/testing/scenarios/<concept>/` that
  fails without it, or with an edit to the scenario that describes the behaviour. Then the code.
  Pure pieces (`decide`, a rule, the deadline arithmetic, the receiver's checks) get table-driven
  unit tests next to them as well.
- The loader is strict: an unknown key, tool, argument, event type, metric or invariant fails the
  load. New vocabulary goes into `internal/testing/harness/vocabulary.go` and the contract
  ([docs/phase1/scenarios.md](docs/phase1/scenarios.md) §4–§5) in the same pull request.
- After adding or changing a scenario, run `make catalogue`; `TestCatalogueIsCurrent` fails on a
  stale `CATALOGUE.md`. [CONTRIBUTING.md](CONTRIBUTING.md) walks through adding one.

### Rules

- **GitHub is read-only.** The engine gets a `forge.Reader`; do not add mutating methods or REST
  writes in phase 1. Every scenario checks `no_github_writes`, which cannot be switched off.
- **All time goes through the clock.** Engine code never calls `time.Now`, `time.Sleep`,
  `time.After`, `time.NewTimer` or `RequeueAfter`: it reads `clock.Clock` and puts deadlines on
  the timer queue. Only client-go's leader election, kube API retries and the Postgres
  reconnect retries run on real time.
- **Everything asynchronous settles.** Each goroutine, queued unit of work or outstanding call
  begins and ends on the `settle.Tracker`, or reports through a probe, so the harness waits for
  exact idleness. A scenario that does not settle within 5 s fails with what was busy.
- **`decide` stays pure:** no clock reads, no calls, no Kubernetes. The reconcilers assemble the
  view and apply the decision. `cmd/` only parses flags and wires.
- **Generated files are regenerated, not edited:** `make generate` after changing `api/`
  (`make check-crds` verifies), `make chart-docs` after changing `helm/beekeeper/values.yaml`,
  `make catalogue` after changing a scenario.
- **Keep test runs CPU-friendly.** Machines that run several agents are shared: use `-parallel 2`
  and the default `-scenario.pool 2`, set `GOMAXPROCS=2 GOFLAGS=-p=2` for `go test` and
  `golangci-lint run -j 2`, run one heavy command at a time, and run the package you change
  before the whole suite. `make test-deps` once; the tests never download.
- **Nothing personal in this public repository:** no hostnames, cluster or installation names,
  home paths, private repositories or people. Scenarios use `alice`, `bob` and `carol`.
- README.md is the front page and links into `docs/`. `.github/check-markdown-budget.sh` fails a
  README over 150 lines or any Markdown line over 200 bytes in README.md or `docs/`; wrap prose at
  100 columns.
