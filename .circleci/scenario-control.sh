#!/usr/bin/env bash
# The scenario control (docs/scenarios.md): every behavioral scenario a pull
# request adds or changes must fail on an assertion against the candidate
# built from the pull request's merge base with main, so the scenario shows
# the change. A scenario that passes there, or fails only in the harness,
# fails this step; control: {none: <reason>} in the scenario opts it out for
# the review to check. A branch that adds or changes no scenario, main and
# tags pass at once without a build.
#
# Usage: scenario-control.sh [base branch, default main]
set -euo pipefail
branch="${1:-main}"
report="${BEEKEEPER_SCENARIO_REPORT:-/tmp/scenarios}"
scenarios=internal/scenario/scenarios

# The clone's origin/<branch> serves if the fetch fails: a fresh CI clone has it.
git fetch --quiet origin "+refs/heads/${branch}:refs/remotes/origin/${branch}" ||
  echo "control: fetching origin/${branch} failed; using the clone's"
base="$(git merge-base HEAD "origin/${branch}")"
echo "control: merge base with origin/${branch} is ${base}"
if [ "$base" = "$(git rev-parse HEAD)" ]; then
  echo "control: HEAD is on ${branch}; no pull request to control"
  exit 0
fi
if git diff --quiet "$base" HEAD -- "$scenarios"; then
  echo "control: the pull request adds or changes no scenario under ${scenarios}"
  exit 0
fi
git diff --stat "$base" HEAD -- "$scenarios"

src="$(mktemp -d)"
git worktree add --quiet --detach "$src" "$base"
trap 'git worktree remove --force "$src"' EXIT
mkdir -p "$report/control"
BEEKEEPER_SCENARIO_CONTROL_BASE="$src" BEEKEEPER_SCENARIO_REPORT="$report" \
  go test -tags scenario ./internal/scenario -run '^TestScenarioControl$' -count=1 -timeout=15m -v
