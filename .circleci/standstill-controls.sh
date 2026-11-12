#!/usr/bin/env bash
# CI-only negative controls. Keep both the successful run and the rejected
# historical mutations as artifacts; a compilation failure is not evidence.
set -euo pipefail
artifacts=/tmp/loop-workflow-artifacts/standstills
mkdir -p "$artifacts"
backup=$(mktemp -d)
cp internal/loop/fill.go "$backup/fill.go"
cp internal/loop/continuation.go "$backup/continuation.go"
restore() {
  cp "$backup/fill.go" internal/loop/fill.go
  cp "$backup/continuation.go" internal/loop/continuation.go
}
trap restore EXIT
go test ./cmd -run '^TestStandstillReplay$' -count=1 -timeout=10m -v > "$artifacts/healthy.log" 2>&1
python3 - <<'PY'
from pathlib import Path
p = Path('internal/loop/fill.go')
s = p.read_text()
old = 'return max(0, room-min(len(refs), room/2))'
assert s.count(old) == 1
p.write_text(s.replace(old, 'return max(0, room-len(refs))'))
PY
if go test ./cmd -run '^TestStandstillReplay/3457$' -count=1 -timeout=10m -v > "$artifacts/3457-defect.log" 2>&1; then
  echo 'reservation defect unexpectedly passed'; exit 1
fi
grep -q 'standstill: starts' "$artifacts/3457-defect.log"
restore
python3 - <<'PY'
from pathlib import Path
p = Path('internal/loop/continuation.go')
s = p.read_text()
old = 'if !Cmd.FreshPark(st, ag) || len(ag.Park.Continuation) == 0 {'
assert s.count(old) == 1
p.write_text(s.replace(old, 'if ag.Park != nil || !Cmd.FreshPark(st, ag) || len(ag.Park.Continuation) == 0 {'))
PY
if go test ./cmd -run '^TestStandstillReplay/3554$' -count=1 -timeout=10m -v > "$artifacts/3554-defect.log" 2>&1; then
  echo 'continuation defect unexpectedly passed'; exit 1
fi
grep -q 'standstill: continuation' "$artifacts/3554-defect.log"
