#!/usr/bin/env bash
# Renders the chart's PrometheusRule with hack/alerts/values.yaml, checks the
# rules and runs their promtool tests (hack/alerts/*.test.yaml).
#
#   PROMTOOL=<path> hack/test-alerts.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
promtool="${PROMTOOL:?PROMTOOL must name the promtool binary}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

helm template beekeeper "$root/helm/beekeeper" --namespace beekeeper \
	--values "$root/hack/alerts/values.yaml" \
	--show-only templates/prometheusrule.yaml >"$work/prometheusrule.yaml"

# The rule file is the PrometheusRule's spec: the lines below `spec:`, one
# level less indented.
awk '/^spec:$/ { inspec = 1; next } inspec { sub(/^  /, ""); print }' "$work/prometheusrule.yaml" >"$work/rules.yaml"
grep -q '^groups:' "$work/rules.yaml" || {
	echo "test-alerts: no groups below spec: in the rendered PrometheusRule" >&2
	exit 1
}

cp "$root"/hack/alerts/*.test.yaml "$work/"
"$promtool" check rules "$work/rules.yaml"
cd "$work"
"$promtool" test rules ./*.test.yaml
