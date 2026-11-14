# Contributing

## Commits and pull requests

- Sign off every commit (`git commit -s`): it certifies the [Developer Certificate of Origin](DCO).
- Use [conventional commits](https://www.conventionalcommits.org/) for commit subjects and the pull
  request title (`feat: …`, `fix: …`, `docs: …`, `ci: …`, `chore: …`; `!` for a breaking change).
  Releases are cut from pull request titles with git-cliff (`cliff.toml`): do not edit
  `CHANGELOG.md`.
- A change of behaviour comes with a scenario that fails without it (see below), and documentation
  that changed with it in the same pull request. README.md stays a front page; details go to
  `docs/`.
- Pull requests need green CI and a review.

## Running the suite

You need Go 1.27, `make`, and `helm` for the chart's tests.

```sh
make test-deps        # once: kine, kube-apiserver, envtest's assets and an embedded Postgres
make test             # unit tests, the scenarios, the RBAC tests and the chart's unit tests
make lint             # goimports and golangci-lint, as CI runs them
```

The test binaries are cached in `~/.cache/beekeeper/test-deps` (`BEEKEEPER_TEST_DEPS`); the tests
never download. While you work, run the package you change, and keep it light on a shared machine:

```sh
GOMAXPROCS=2 GOFLAGS=-p=2 go test ./internal/decide/ -count=1
GOMAXPROCS=2 GOFLAGS=-p=2 go test ./internal/testing/harness/ -run TestScenarios -scenario.concept wait -parallel 2
```

The CI jobs map to make targets: `make test-race` (with the race detector), `make test-envtest` and
`make check-crds`, `make test-large-history`, `make test-alerts` and `make helm-template-prod`
(`.circleci/custom.yml` lists them all).

## pre-commit

CI runs the hooks of `.pre-commit-config.yaml`: formatting, `go mod tidy`, golangci-lint, goimports,
conventional commit messages, and the chart's `values.schema.json` and README checks. Install
[pre-commit](https://pre-commit.com/) and run them before you push:

```sh
pre-commit install    # the pre-commit and commit-msg hooks
pre-commit run --all-files
```

After changing `helm/beekeeper/values.yaml`, regenerate the schema and the chart's README with
`make chart-docs`. After changing `api/`, run `make generate` (the deepcopy code, `config/crd` and
the chart's copy of the CRDs).

## Adding a scenario

Scenarios are the specification of the engine's behaviour. They are YAML files in
`internal/testing/scenarios/<concept>/`, in the format of
[docs/phase1/scenarios.md](docs/phase1/scenarios.md) §4.

1. Pick the concept (`board`, `eligibility`, `wait`, …; the list is in §2 of that document) and a
   name: kebab-case, prefixed by the concept, equal to the file name, for example
   `wait/wait-ci-wakes-on-check-run.yaml`.
2. Write the `description` as Given/When/Then. Start from `fixture: base`
   (`internal/testing/scenarios/fixtures/base.yaml`) and add only what the scenario needs under
   `pre_configuration`.
3. Write the steps: each calls a step tool (§4.2) with its arguments and states what it `expected`
   (§4.3). Assert on the vocabulary of §5: object fields, event types and metrics.
4. Run it, and see it fail for the right reason before the change and pass after it:

   ```sh
   go test ./internal/testing/harness/ -run TestScenarios/wait-ci-wakes-on-check-run -v
   ```

   `-scenario.phases` logs the set-up phases and `-scenario.logs` the engine's output.
5. Regenerate the catalogue with `make catalogue` and commit `CATALOGUE.md` with the scenario.

The loader is strict: an unknown key, tool, argument, event type, metric or invariant fails the
load. A new step tool or new vocabulary is registered in `internal/testing/harness/` and documented
in the contract in the same pull request.

## Security

Report vulnerabilities as described in [SECURITY.md](SECURITY.md), not in public issues.
