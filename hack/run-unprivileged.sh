#!/bin/sh
# Runs the tests as an unprivileged user when make runs as root.
#
# The generated go-build CI job runs `make test` as root (the architect image),
# and Postgres' initdb refuses to run as root, so the embedded Postgres of the
# scenarios and the history tests could not start there. As root, make passes
# this script to `go test -exec`, and it runs each test binary as
# $BEEKEEPER_TEST_USER (nobody) with the environment unchanged (HOME too, so the
# test-deps cache resolves to the same place). For anyone else it only runs the
# binary.
#
#   hack/run-unprivileged.sh --setup     as root, once before the tests: lets the user reach
#                                        the checkout and own the beekeeper cache
#   hack/run-unprivileged.sh <binary> [args...]
set -eu

user="${BEEKEEPER_TEST_USER:-nobody}"

if [ "$(id -u)" != 0 ]; then
	[ "${1:-}" = --setup ] && exit 0
	exec "$@"
fi

if [ "${1:-}" = --setup ]; then
	# Every ancestor of the checkout and of the cache must be traversable.
	cache="${XDG_CACHE_HOME:-$HOME/.cache}/beekeeper"
	mkdir -p "$cache"
	for path in "$(pwd)" "$cache"; do
		while [ "$path" != / ] && [ -n "$path" ]; do
			chmod o+x "$path"
			path="$(dirname "$path")"
		done
	done
	chmod -R o+rX "$(pwd)"
	chown -R "$user" "$cache"
	exit 0
fi

# The test binary sits in go's work directory, which is private to root.
dir="$(dirname "$1")"
chmod o+rx "$dir" "$(dirname "$dir")"

if command -v python3 >/dev/null 2>&1; then
	exec python3 -I -c '
import os, pwd, sys
entry = pwd.getpwnam(sys.argv[1])
os.setgroups([])
os.setgid(entry.pw_gid)
os.setuid(entry.pw_uid)
os.execv(sys.argv[2], sys.argv[2:])
' "$user" "$@"
fi
if command -v setpriv >/dev/null 2>&1; then
	exec setpriv --reuid="$user" --regid="$(id -g "$user")" --clear-groups -- "$@"
fi
echo "run-unprivileged: neither python3 nor setpriv is available to drop root" >&2
exit 1
