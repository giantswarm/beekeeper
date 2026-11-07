#!/usr/bin/env bash
set -euo pipefail

case "$(cat internal/update/sandbox-proof-required.txt)" in
  false) echo "Sandbox proof publication temporarily disabled"; exit 0 ;;
  true) ;;
  *) echo "Invalid sandbox proof policy" >&2; exit 1 ;;
esac

if [ -z "${BEEKEEPER_CANARY_REPO:-}" ] || [ -z "${BEEKEEPER_CANARY_ALLOW_ORG:-}" ] || [ -z "${BEEKEEPER_CANARY_PROJECT:-}" ] || [ -z "${BEEKEEPER_CANARY_GITHUB_TOKEN:-}" ]; then
  echo "publish-canary-evidence: BEEKEEPER_CANARY_REPO, BEEKEEPER_CANARY_ALLOW_ORG, BEEKEEPER_CANARY_PROJECT, and BEEKEEPER_CANARY_GITHUB_TOKEN are required; refusing to fall back to recorded fixture mode" >&2
  exit 1
fi

candidate="${BEEKEEPER_CANARY_CANDIDATE:-./beekeeper-linux-amd64}"
bundle="${BEEKEEPER_CANARY_BUNDLE:-${candidate}.bundle}"
artifacts_dir="${BEEKEEPER_CANARY_ARTIFACTS_DIR:-/tmp/canary-artifacts}"
blobs_file="${BEEKEEPER_CANARY_BLOBS_FILE:-/tmp/.cosign_canary_blobs}"

if [ ! -f "$candidate" ]; then
  echo "publish-canary-evidence: staged candidate binary $candidate not found" >&2
  exit 1
fi
if [ ! -f "$bundle" ]; then
  echo "publish-canary-evidence: candidate signature bundle $bundle not found" >&2
  exit 1
fi

mkdir -p "$artifacts_dir"
chmod +x "$candidate"

runner_bin="$(mktemp "${TMPDIR:-/tmp}/canary-runner.XXXXXX")"
trap 'rm -f "$runner_bin"' EXIT
cp "$candidate" "$runner_bin"
chmod 0755 "$runner_bin"

siblings=()
for bin in ./beekeeper-*-*; do
  case "$bin" in
    *.bundle|*.canary.json|./beekeeper-canary-runner*|"$candidate") continue ;;
  esac
  if [ -f "$bin" ]; then
    if [ ! -f "${bin}.bundle" ]; then
      echo "publish-canary-evidence: sibling signature bundle ${bin}.bundle not found" >&2
      exit 1
    fi
    siblings+=(--canary-sibling "$bin")
  fi
done

"$runner_bin" self-update \
  --stage-canary "$candidate" \
  --canary-bundle "$bundle" \
  --canary-repo "$BEEKEEPER_CANARY_REPO" \
  --canary-allow-org "$BEEKEEPER_CANARY_ALLOW_ORG" \
  --canary-project "$BEEKEEPER_CANARY_PROJECT" \
  --canary-run "circleci-${CIRCLE_WORKFLOW_ID:-local}-${CIRCLE_BUILD_NUM:-0}" \
  --canary-output "${candidate}.canary.json" \
  "${siblings[@]}"

: > "$blobs_file"
for ev in ./beekeeper-*-*.canary.json; do
  echo "${ev} ${ev}.bundle" >> "$blobs_file"
  cp "$ev" "$artifacts_dir/"
done
