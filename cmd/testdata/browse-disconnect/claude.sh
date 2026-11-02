#!/bin/sh
# A bounded CLI replay host: append one turn to the session transcript.
set -eu
n=1
if [ -f "$BROWSE_REPLAY_ROOT/count" ]; then
  n=$(($(cat "$BROWSE_REPLAY_ROOT/count") + 1))
fi
printf '%s\n' "$n" > "$BROWSE_REPLAY_ROOT/count"
[ "$n" -le 2 ] || { echo 'unexpected third Claude turn' >&2; exit 70; }
id= prev=
for arg in "$@"; do
  printf '%s\0' "$arg" >> "$BROWSE_REPLAY_ROOT/argv.$n"
  case "$prev" in --session-id|--resume) id=$arg;; esac
  prev=$arg
done
[ -n "$id" ]
mkdir -p "$BROWSE_REPLAY_PROJECT/replay"
if [ "$n" = 1 ]; then
  turn=disconnect
else
  turn=$BROWSE_REPLAY_OUTCOME
fi
cat "$BROWSE_REPLAY_FIXTURE/$turn.jsonl" >> "$BROWSE_REPLAY_PROJECT/replay/$id.jsonl"
cat "$BROWSE_REPLAY_FIXTURE/$turn.txt"
