#!/usr/bin/env bash
# Keeps README.md a front page and the Markdown in docs/ readable:
# README.md stays within its line and byte budget, and no line of README.md
# or docs/**/*.md outside a fenced code block is longer than the line limit
# (in bytes, so the result does not depend on the locale).
# Behaviour belongs on its docs/ topic page, not appended to README.md.
#
# Usage: check-markdown-budget.sh [repository root]
set -euo pipefail

root="${1:-.}"
readme_lines=150
readme_bytes=10000
line_limit=200

cd "${root:?}"
status=0

lines=$(wc -l < README.md)
bytes=$(wc -c < README.md)
if (( lines > readme_lines )); then
  echo "README.md: ${lines} lines, budget ${readme_lines}; move the detail to its docs/ page" >&2
  status=1
fi
if (( bytes > readme_bytes )); then
  echo "README.md: ${bytes} bytes, budget ${readme_bytes}; move the detail to its docs/ page" >&2
  status=1
fi

files=(README.md)
while IFS= read -r -d '' f; do
  files+=("$f")
done < <(find docs -name '*.md' -type f -print0 | sort -z)

if ! LC_ALL=C awk -v limit="${line_limit}" '
  FNR == 1 { fence = "" }
  {
    if (match($0, /^[ \t]*(```|~~~)/)) {
      tok = substr($0, RSTART + RLENGTH - 3, 3)
      if (fence == "") { fence = tok } else if (fence == tok) { fence = "" }
      next
    }
    if (fence == "" && length($0) > limit) {
      printf "%s:%d: %d bytes, limit %d; wrap the line or split the table into a list\n", FILENAME, FNR, length($0), limit > "/dev/stderr"
      bad = 1
    }
  }
  END { exit bad }
' "${files[@]}"; then
  status=1
fi

if (( status == 0 )); then
  echo "Markdown budget ok: README.md ${lines}/${readme_lines} lines, ${bytes}/${readme_bytes} bytes; ${#files[@]} files within ${line_limit} bytes per line"
fi
exit "${status}"
