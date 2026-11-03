package remotebuild

// The image contract is bash, GNU coreutils (dd byte offsets), tar, flock and
// util-linux setsid. Journals survive exec disconnections, independently of
// kubectl. stdout and stderr share one ordered byte stream.
const upload = `set -eu
id=$1
mkdir /work/busy || exit 75
printf '%s' "$id" > /work/busy/id
printf '%s' "$$" > /work/busy/owner
touch /work/active
trap 'rm -rf /work/busy /work/incoming' EXIT
mkdir -p "/work/runs/$id" /work/incoming
: > "/work/runs/$id/log"
tar -xf - -C /work/incoming
trap - EXIT`

const wrapper = `id=$1 sub=$2 command=$3 tool=${4:-}
r=/work/runs/$id
rc=143 child=
finish() {
` + cacheFinish + `
 printf '%s\n' "$rc" > "$r/rc.tmp" && mv "$r/rc.tmp" "$r/rc"
 if [ "$(cat /work/busy/id 2>/dev/null)" = "$id" ]; then rm -rf /work/busy; fi
 touch /work/active
}
stop_child() {
 if [ -n "$child" ]; then
  kill -TERM -- "-$child" 2>/dev/null || true
  sleep 0.2
  kill -KILL -- "-$child" 2>/dev/null || true
  wait "$child" 2>/dev/null || true
 fi
}
trap 'stop_child; rc=143; finish; exit 143' TERM INT HUP
printf '%s' "$$" > "$r/pid"
printf '%s' "$$" > /work/busy/owner
rm -rf /work/src
mv /work/incoming /work/src || { rc=2; finish; exit 2; }
cd "/work/src/$sub" || { rc=2; finish; exit 2; }
` + cacheSetup + `
setsid bash -c "$command" >> "$r/log" 2>&1 < /dev/null &
child=$!; printf '%s' "$child" > "$r/child"
wait "$child"; rc=$?
stop_child
finish`

const start = `set -eu
[ "$(cat /work/busy/id)" = "$1" ]
nohup setsid bash -c "$4" -- "$1" "$2" "$3" "$5" >/dev/null 2>&1 </dev/null &`

// Emit exactly the bytes after the client's acknowledged offset. Once rc is
// published no process writes the log. Reading rc separately keeps exit status
// out of the user's output and avoids marker collisions with arbitrary output.
const follow = `set -eu
r=/work/runs/$1 offset=$2
for ((i=0;i<100;i++)); do
 [ -f "$r/pid" ] || [ -f "$r/rc" ] && break
 sleep 0.1
done
[ -f "$r/pid" ] || [ -f "$r/rc" ] || exit 125
while :; do
 size=$(stat -c %s "$r/log")
 if [ "$size" -gt "$offset" ]; then
  dd if="$r/log" bs=65536 skip="$offset" count="$((size-offset))" iflag=skip_bytes,count_bytes status=none
  offset=$size
 fi
 if [ -f "$r/rc" ]; then
  [ "$(stat -c %s "$r/log")" -le "$offset" ] && exit 0
 elif ! kill -0 "$(cat "$r/pid")" 2>/dev/null; then exit 125
 fi
 sleep 0.2
done`

const interrupt = `id=$1 r=/work/runs/$1
[ -f "$r/rc" ] && exit 0
[ "$(cat /work/busy/id 2>/dev/null)" = "$id" ] || exit 0
for f in child pid; do
 if [ -f "$r/$f" ]; then
  p=$(cat "$r/$f")
  case "$p" in ''|*[!0-9]*) exit 1;; esac
  kill -TERM -- "-$p" 2>/dev/null || true
 fi
done
# If the launch never reached the wrapper, release the reservation.
if [ ! -f "$r/pid" ]; then rm -rf /work/busy /work/incoming; fi`

const finish = `id=$1
[ -f "/work/runs/$id/rc" ] && rm -rf "/work/runs/$id"`
