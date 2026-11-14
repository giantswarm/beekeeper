#!/usr/bin/env bash
# The CRDs and deepcopy code come from api/ (controller-gen); the chart ships a
# copy of config/crd in helm/beekeeper/crds.
#
#   hack/crds.sh generate   regenerate deepcopy and config/crd, copy the CRDs into the chart
#   hack/crds.sh check      fail when api/'s deepcopy, config/crd or the chart's copy is not
#                           what controller-gen generates now (writes nothing in the tree)
set -euo pipefail

: "${CONTROLLER_GEN_VERSION:?}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

controller_gen() {
	go run "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}" "$@"
}
# The deepcopy code is formatted as pre-commit's go-imports hook (and make lint) want it,
# with the version the generated Makefile pins.
goimports() {
	go run golang.org/x/tools/cmd/goimports@v0.50.0 -local github.com/giantswarm/beekeeper -w "$@"
}
crd_options="crd:crdVersions=v1,allowDangerousTypes=true"

case "${1:-}" in
generate)
	controller_gen object paths=./api/... "$crd_options" output:crd:dir=./config/crd
	goimports api/*/zz_generated.deepcopy.go
	mkdir -p helm/beekeeper/crds
	rm -f helm/beekeeper/crds/*.yaml
	cp config/crd/*.yaml helm/beekeeper/crds/
	;;
check)
	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT
	controller_gen paths=./api/... "$crd_options" output:crd:dir="$work/crd"
	failed=0
	for dir in config/crd helm/beekeeper/crds; do
		if ! diff -ru "$dir" "$work/crd"; then
			echo "check-crds: $dir is not what api/ generates" >&2
			failed=1
		fi
	done
	# One output directory per API package, so their deepcopy files stay apart.
	for file in api/*/zz_generated.deepcopy.go; do
		[ -e "$file" ] || continue
		pkg="$(dirname "$file")"
		controller_gen object paths="./$pkg/..." output:object:dir="$work/object/$pkg"
		goimports "$work/object/$pkg/zz_generated.deepcopy.go"
		if ! diff -u "$file" "$work/object/$pkg/zz_generated.deepcopy.go"; then
			echo "check-crds: $file is not what controller-gen generates" >&2
			failed=1
		fi
	done
	if [ "$failed" -ne 0 ]; then
		echo "check-crds: run 'make generate' and commit the result" >&2
		exit 1
	fi
	echo "check-crds: the CRDs (config/crd, helm/beekeeper/crds) and deepcopy code are current"
	;;
*)
	echo "usage: $0 generate|check" >&2
	exit 2
	;;
esac
