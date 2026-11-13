#!/usr/bin/env bash
# The scenario control (docs/scenarios.md): what a pull request adds or
# changes to show its change must fail without it, on the pull request's
# merge base with main.
#
# - Every behavioral scenario it adds or changes must fail on an assertion
#   against the candidate built from the merge base. A scenario that passes
#   there, or fails only in the harness, fails this step;
#   control: {none: <reason>} in the scenario opts it out for the review to
#   check.
# - The Test functions it adds or changes in any _test.go run against the
#   merge base's code with its test files applied (internal/testcontrol): at
#   least one must fail there and pass on the pull request's code. A package
#   that does not build on the base is reported; a "control: none: <reason>"
#   paragraph in a test's doc comment opts the test out for the review to
#   check. A pull request that changes the Go-test control also runs it on
#   the clock fix of https://github.com/giantswarm/beekeeper/pull/3832.
#
# A branch that adds or changes neither, main and tags pass at once without
# a build. Both parts run before the step fails, so one round reports both.
#
# Under BEEKEEPER_SCENARIO_SHARD=<index>/<total> (each container of the CI
# job) the scenario part controls that shard's scenarios, and only shard 0
# runs the Go-test part, so each runs once per pull request.
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

trees=()
cleanup() {
  local tree
  for tree in ${trees[@]+"${trees[@]}"}; do
    git worktree remove --force "$tree" || true
  done
}
trap cleanup EXIT
status=0

if git diff --quiet "$base" HEAD -- "$scenarios"; then
  echo "control: the pull request adds or changes no scenario under ${scenarios}"
else
  git diff --stat "$base" HEAD -- "$scenarios"
  src="$(mktemp -d)"
  git worktree add --quiet --detach "$src" "$base"
  trees+=("$src")
  mkdir -p "$report/control"
  BEEKEEPER_SCENARIO_CONTROL_BASE="$src" BEEKEEPER_SCENARIO_REPORT="$report" \
    go test -tags scenario ./internal/scenario -run '^TestScenarioControl$' -count=1 -timeout=15m -v ||
    status=1
fi

shard="${BEEKEEPER_SCENARIO_SHARD:-}"
if [ -n "$shard" ] && [ "${shard%%/*}" != 0 ]; then
  echo "control: shard ${shard} leaves the Go-test control to shard 0"
  exit "$status"
fi

if ! git diff --quiet "$base" HEAD -- internal/testcontrol .circleci/scenario-control.sh go.mod; then
  echo "control: the pull request changes the Go-test control; proving it on the clock fix first"
  BEEKEEPER_TEST_CONTROL_HISTORY=1 BEEKEEPER_TEST_CONTROL_REPORT="$report/go-control-history" \
    go test ./internal/testcontrol -run '^TestControlProvesTheClockFix$' -count=1 -timeout=10m -v ||
    status=1
fi

if git diff --quiet "$base" HEAD -- '*_test.go'; then
  echo "control: the pull request adds or changes no Go test file"
else
  git diff --stat "$base" HEAD -- '*_test.go'
  gosrc="$(mktemp -d)"
  git worktree add --quiet --detach "$gosrc" "$base"
  trees+=("$gosrc")
  BEEKEEPER_TEST_CONTROL_BASE="$gosrc" BEEKEEPER_TEST_CONTROL_REPORT="$report/go-control" \
    go test ./internal/testcontrol -run '^TestGoTestControl$' -count=1 -timeout=15m -v ||
    status=1
fi
exit "$status"
