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

- README.md is the front page: what beekeeper is, install, the first commands
  and links into `docs/`. Behaviour goes on its `docs/` topic page, listed in
  `docs/README.md`. `.github/check-markdown-budget.sh` (the `markdown-budget`
  workflow) fails a README over 150 lines or any Markdown line over 200 bytes
  in README.md or `docs/`; wrap prose at 100 columns.
- A pull request that fixes or changes behavior of beekeeper's core (the binary's Go code under
  `cmd/`, `internal/` and `pkg/`; not docs, CI, skills, the chart or test code) adds or extends a
  behavioral scenario under `internal/scenario/scenarios/` that fails without the change
  ([docs/scenarios.md](docs/scenarios.md)). A bug's scenario names the bug's issue (`issue:`, the
  full URL). A change that cannot be expressed as a scenario says why in its pull request and
  links the follow-up issue that gives the harness the missing step. CI's `scenario-control` step
  runs each added or changed scenario against the merge base, where it must fail on an assertion;
  a scenario that cannot fail there, such as one of behavior main already has, says why with
  `control: {none: <reason>}`. The same step runs the `Test…` functions a pull request adds or
  changes in any `_test.go` against the merge base's code with its test files applied: at least
  one must fail there and pass on the change, and a package that does not build on the base shows
  nothing. A test that cannot fail there says why in its doc comment with a
  `control: none: <reason>` paragraph. Reviews reject a core fix without a scenario.
- The core starts processes through `internal/proc` (`proc.CommandContext`, `proc.Cmd`,
  `proc.ExitError`, `proc.LookPath`), never `os/exec`, so that the behavioral scenarios can answer
  every start in their test process ([docs/scenarios.md](docs/scenarios.md#the-process-seam)).
  `internal/proc/coreguard` fails on an `os/exec` import in a core package.
