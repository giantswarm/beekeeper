#!/usr/bin/env bash
# CI-only negative control for the performance suite: the loop budgets must
# reject main as it was before the fix of
# https://github.com/giantswarm/beekeeper/issues/3657 (its parent commit of
# https://github.com/giantswarm/beekeeper/pull/3662), whose idle loop re-read
# the whole state and event log on every pass. A suite that passes it measures
# nothing.
set -euo pipefail
before=b11b33f64351950d7067306ed42b0dff0b8bfff3
artifacts=/tmp/performance/control
mkdir -p "$artifacts" /tmp/perf-control
git cat-file -e "${before}^{commit}" 2>/dev/null || git fetch --quiet origin "$before"
src=$(mktemp -d)
git worktree add --quiet --detach "$src" "$before"
trap 'git worktree remove --force "$src"' EXIT
(cd "${src:?}" && go build -o /tmp/perf-control/beekeeper .)
if BEEKEEPER_PERF=1 BEEKEEPER_PERF_STRICT=1 BEEKEEPER_PERF_BINARY=/tmp/perf-control/beekeeper BEEKEEPER_PERF_REPORT="$artifacts" \
  go test -tags perf ./internal/perfsuite -run '^TestLoopBudgets$' -count=1 -timeout=20m -v > "$artifacts/output.txt" 2>&1; then
  cat "$artifacts/output.txt"
  echo "control: the loop budgets accepted the loop before the fix of https://github.com/giantswarm/beekeeper/issues/3657"
  exit 1
fi
cat "$artifacts/output.txt"
if ! grep -Eq 'idle pass(, 4x event log)? read: [0-9.]+ MiB over the budget' "$artifacts/output.txt"; then
  echo "control: the run before the fix failed, but not on the idle loop's reads"
  exit 1
fi
echo "control: the loop budgets reject the idle loop's re-reads before the fix"
