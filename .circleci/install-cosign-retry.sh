#!/usr/bin/env bash
# Put cosign-retry.sh in front of cosign for the later steps of this CircleCI
# job, until architect's signing retry classifier includes Rekor's conflict
# lookup race. Every step sources $BASH_ENV, so the step that signs picks it up.
set -euo pipefail

dir="${BEEKEEPER_COSIGN_BIN_DIR:-/tmp/beekeeper-cosign-bin}"
cosign="$(command -v cosign)"

mkdir -p "$dir"
cp .circleci/cosign-retry.sh "$dir/cosign"
chmod 0755 "$dir/cosign"
printf 'export BEEKEEPER_COSIGN_BINARY=%q\n' "$cosign" >> "${BASH_ENV:?}"
printf 'export PATH=%q:"$PATH"\n' "$dir" >> "$BASH_ENV"
