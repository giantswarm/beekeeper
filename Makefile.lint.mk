# Before the round's one push, run through `beekeeper run -- make lint-changed`.
# BASE may name another PR target; fetch it first. This runs the configured CI
# linters on entire changed packages, so cross-file goconst findings cannot hide
# behind a green main or a lint of individual changed files.
BASE ?= origin/main

.PHONY: lint-changed
lint-changed: ## Lint changed Go packages, including unchanged test files (BASE=origin/main).
	python3 .github/lint-changed-go.py "$(BASE)"
