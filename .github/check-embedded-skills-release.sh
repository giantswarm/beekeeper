#!/usr/bin/env bash
set -euo pipefail

title="${1:?missing commit or PR title}"
shift

for path in "$@"; do
  case "$path" in
    plugin/skills/*)
      # These types are releasing sections in the generated cliff.toml.
      if [[ ! "$title" =~ ^(feat|fix|refactor|perf|chore|build|ci|security)(\([^\)]+\))?!?:\ .+ ]]; then
        echo 'Embedded skills change product behavior. Use fix(rules): or another releasing commit type for the PR title and commits.' >&2
        exit 1
      fi
      ;;
  esac
done
