#!/usr/bin/env bash
# Fetches the binaries the tests need into the test-deps cache, each only if it
# is missing, and checks every download against its published checksum.
#
#   hack/test-deps.sh [kine] [kube-apiserver] [envtest] [postgres] [promtool]
#
# Layout under $BEEKEEPER_TEST_DEPS (default ~/.cache/beekeeper/test-deps):
#   kine                                     k8senv's storage (a static release binary)
#   k8s-<v>/kube-apiserver                   k8senv's API server
#   envtest/k8s/<v>-<os>-<arch>/             etcd, kube-apiserver, kubectl (setup-envtest's layout)
#   postgres/embedded-postgres-binaries-<os>-<arch>[-alpine]-<v>.txz
#                                            embedded-postgres's cache file (its CachePath)
#   prometheus-<v>/promtool
#
# Versions come from the environment (Makefile.custom.mk sets them).
set -euo pipefail

: "${KINE_VERSION:?}" "${KUBE_APISERVER_VERSION:?}" "${ENVTEST_K8S_VERSION:?}" "${POSTGRES_VERSION:?}" "${PROMETHEUS_VERSION:?}"
deps="${BEEKEEPER_TEST_DEPS:-${XDG_CACHE_HOME:-$HOME/.cache}/beekeeper/test-deps}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "test-deps: unsupported machine $(uname -m)" >&2 && exit 1 ;;
esac

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fetch() { # url file
	curl -sSfL --retry 3 --retry-all-errors -o "$2" "$1"
}

sha() { # algorithm file -> hex digest
	if command -v "sha${1}sum" >/dev/null 2>&1; then
		"sha${1}sum" "$2" | cut -d' ' -f1
	else
		shasum -a "$1" "$2" | cut -d' ' -f1
	fi
}

verify() { # algorithm file expected
	local got
	got="$(sha "$1" "$2")"
	if [ "$got" != "$3" ]; then
		echo "test-deps: checksum mismatch for $2: got $got, want $3" >&2
		exit 1
	fi
}

install_file() { # source destination: atomic, so a concurrent reader never sees half a file
	mkdir -p "$(dirname "$2")"
	cp "$1" "$2.tmp.$$"
	mv -f "$2.tmp.$$" "$2"
}

kine() {
	local dst="$deps/kine"
	[ -x "$dst" ] && return
	[ "$os" = linux ] || { echo "test-deps: kine has release binaries for linux only; build it: go install github.com/k3s-io/kine@v${KINE_VERSION}" >&2 && exit 1; }
	local base="https://github.com/k3s-io/kine/releases/download/v${KINE_VERSION}"
	fetch "$base/kine-$arch" "$work/kine"
	fetch "$base/sha256sum-$arch.txt" "$work/kine.sha256"
	verify 256 "$work/kine" "$(awk -v f="kine-$arch" '$2 == f || $2 == "*"f {print $1}' "$work/kine.sha256")"
	chmod 0755 "$work/kine"
	install_file "$work/kine" "$dst"
	echo "test-deps: kine v${KINE_VERSION}"
}

kube_apiserver() {
	local dst="$deps/k8s-${KUBE_APISERVER_VERSION}/kube-apiserver"
	[ -x "$dst" ] && return
	local url="https://dl.k8s.io/v${KUBE_APISERVER_VERSION}/bin/$os/$arch/kube-apiserver"
	fetch "$url" "$work/kube-apiserver"
	fetch "$url.sha256" "$work/kube-apiserver.sha256"
	verify 256 "$work/kube-apiserver" "$(tr -d '[:space:]' <"$work/kube-apiserver.sha256")"
	chmod 0755 "$work/kube-apiserver"
	install_file "$work/kube-apiserver" "$dst"
	echo "test-deps: kube-apiserver v${KUBE_APISERVER_VERSION}"
}

envtest() {
	local dst="$deps/envtest/k8s/${ENVTEST_K8S_VERSION}-$os-$arch"
	[ -x "$dst/kube-apiserver" ] && [ -x "$dst/etcd" ] && return
	local tgz="envtest-v${ENVTEST_K8S_VERSION}-$os-$arch.tar.gz"
	local url="https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v${ENVTEST_K8S_VERSION}/$tgz"
	fetch "$url" "$work/$tgz"
	fetch "$url.sha512" "$work/$tgz.sha512"
	verify 512 "$work/$tgz" "$(awk '{print $1}' "$work/$tgz.sha512")"
	mkdir -p "$work/envtest"
	tar -xzf "$work/$tgz" -C "$work/envtest"
	mkdir -p "$(dirname "$dst")"
	rm -rf "$dst.tmp.$$"
	mv "$work/envtest/controller-tools/envtest" "$dst.tmp.$$"
	rm -rf "$dst"
	mv "$dst.tmp.$$" "$dst"
	echo "test-deps: envtest ${ENVTEST_K8S_VERSION}"
}

postgres() {
	# embedded-postgres's names: arm64 is arm64v8, and Alpine gets the musl build.
	local parch="$arch"
	[ "$arch" = arm64 ] && [ "$os" = linux ] && parch=arm64v8
	[ "$arch" = arm64 ] && [ "$os" = darwin ] && parch=arm64v8
	[ -f /etc/alpine-release ] && [ "$os" = linux ] && parch="$parch-alpine"
	local name="embedded-postgres-binaries-$os-$parch"
	local dst="$deps/postgres/$name-${POSTGRES_VERSION}.txz"
	[ -s "$dst" ] && return
	local url="https://repo1.maven.org/maven2/io/zonky/test/postgres/$name/${POSTGRES_VERSION}/$name-${POSTGRES_VERSION}.jar"
	fetch "$url" "$work/postgres.jar"
	fetch "$url.sha256" "$work/postgres.jar.sha256"
	verify 256 "$work/postgres.jar" "$(tr -d '[:space:]' <"$work/postgres.jar.sha256")"
	mkdir -p "$work/postgres"
	if command -v unzip >/dev/null 2>&1; then
		unzip -q -o "$work/postgres.jar" '*.txz' -d "$work/postgres"
	elif command -v bsdtar >/dev/null 2>&1; then
		bsdtar -xf "$work/postgres.jar" -C "$work/postgres" '*.txz'
	else
		python3 -I -c 'import sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
z.extractall(sys.argv[2], [n for n in z.namelist() if n.endswith(".txz")])' "$work/postgres.jar" "$work/postgres"
	fi
	local txz
	txz="$(find "$work/postgres" -name '*.txz' -type f | head -n 1)"
	[ -n "$txz" ] || { echo "test-deps: no .txz in $url" >&2 && exit 1; }
	install_file "$txz" "$dst"
	echo "test-deps: postgres ${POSTGRES_VERSION} ($name)"
}

promtool() {
	local dst="$deps/prometheus-${PROMETHEUS_VERSION}/promtool"
	[ -x "$dst" ] && return
	local dir="prometheus-${PROMETHEUS_VERSION}.$os-$arch"
	local base="https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}"
	fetch "$base/$dir.tar.gz" "$work/$dir.tar.gz"
	fetch "$base/sha256sums.txt" "$work/sha256sums.txt"
	verify 256 "$work/$dir.tar.gz" "$(awk -v f="$dir.tar.gz" '$2 == f {print $1}' "$work/sha256sums.txt")"
	tar -xzf "$work/$dir.tar.gz" -C "$work" "$dir/promtool"
	chmod 0755 "$work/$dir/promtool"
	install_file "$work/$dir/promtool" "$dst"
	echo "test-deps: promtool v${PROMETHEUS_VERSION}"
}

[ "$#" -gt 0 ] || set -- kine kube-apiserver envtest postgres promtool
for dep in "$@"; do
	case "$dep" in
	kine) kine ;;
	kube-apiserver) kube_apiserver ;;
	envtest) envtest ;;
	postgres) postgres ;;
	promtool) promtool ;;
	*) echo "test-deps: unknown dependency $dep" >&2 && exit 1 ;;
	esac
done
