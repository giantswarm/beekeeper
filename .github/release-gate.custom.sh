#!/usr/bin/env bash
# Repository stable release gate invoked by zz_generated.auto_release.yaml
# with the stable tag about to be created ($1, e.g. v0.172.0) at the target
# commit (HEAD). Refuses cutting a stable release when:
# 1. Any release candidate for the same version (${TAG}-rc.*) is not an
#    ancestor of HEAD, or
# 2. HEAD is a strict ancestor of any existing release or release-candidate
#    tag (v*.*.* or v*.*.*-rc.*).
set -euo pipefail

tag="${1:?missing release tag}"
head="$(git rev-parse HEAD)"

while IFS= read -r rc; do
  [ -n "$rc" ] || continue
  rc_commit="$(git rev-parse "${rc}^{commit}")"
  if ! git merge-base --is-ancestor "$rc_commit" "$head"; then
    echo "refusing ${tag} at ${head:0:7}: candidate ${rc} (${rc_commit:0:7}) is not an ancestor of HEAD" >&2
    exit 1
  fi
done < <(git tag --list "${tag}-rc.*" | grep -E "^${tag//./\\.}-rc\\.[0-9]+$" || true)

while IFS= read -r existing; do
  [ -n "$existing" ] || continue
  existing_commit="$(git rev-parse "${existing}^{commit}")"
  if [ "$existing_commit" != "$head" ] && git merge-base --is-ancestor "$head" "$existing_commit"; then
    echo "refusing ${tag} at ${head:0:7}: HEAD is an ancestor of existing release tag ${existing} (${existing_commit:0:7})" >&2
    exit 1
  fi
done < <(git tag --list 'v*.*.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' || true)
