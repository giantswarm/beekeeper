#!/usr/bin/env bash
# Retry Rekor's conflict lookup race, which the pinned architect orb does not
# classify as transient. The orb still signs and verifies every binary.
set -euo pipefail

for attempt in 1 2 3 4; do
  output=$(mktemp)
  if "${BEEKEEPER_COSIGN_BINARY:?}" "$@" 2>"$output"; then
    cat "$output" >&2
    rm -f "$output"
    exit 0
  else
    status=$?
  fi
  cat "$output" >&2
  if [[ "${1:-}" != sign-blob || "$attempt" == 4 ]] ||
     ! grep -Fq 'getLogEntryByUuidNotFound' "$output"; then
    rm -f "$output"
    exit "$status"
  fi
  rm -f "$output"
  delay=$(( ${BEEKEEPER_COSIGN_BACKOFF_SECONDS:-5} * (2 ** (attempt - 1)) ))
  echo "Rekor conflict lookup failed; retrying signing in ${delay}s (attempt ${attempt}/4)." >&2
  sleep "$delay"
done
