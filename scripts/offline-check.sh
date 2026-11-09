#!/usr/bin/env bash
# Dress rehearsal + offline proof: starts voc on a fresh database, runs the
# demo script by voice (cmd/rehearse), and samples the server's sockets the
# whole time. Fails if the server ever connects anywhere but loopback.
#
#   scripts/offline-check.sh            # cloud via the local fake API
#   CLOUD=off scripts/offline-check.sh  # no cloud tool at all
#   WAIT_REMINDER=0 scripts/offline-check.sh   # skip the 1-minute reminder wait
#   VOC_FLAGS="-desktop=false" scripts/offline-check.sh   # extra server flags
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${PORT:-8091}
CLOUD=${CLOUD:-fake}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/voc-offline.XXXXXX")
trap 'kill "${PID:-}" "${SAMPLER:-}" 2>/dev/null || true' EXIT

go build -o bin/voc ./cmd/voc
go build -o bin/rehearse ./cmd/rehearse

./bin/voc -addr "127.0.0.1:$PORT" -db "$WORK/voc.db" -cloud="$CLOUD" ${VOC_FLAGS:-} >"$WORK/server.log" 2>&1 &
PID=$!
for _ in $(seq 1 60); do
  grep -q "llm ready" "$WORK/server.log" && break
  if ! kill -0 "$PID" 2>/dev/null; then cat "$WORK/server.log"; exit 1; fi
  sleep 0.5
done
grep -q "llm ready" "$WORK/server.log" || { echo "server did not become ready"; cat "$WORK/server.log"; exit 1; }

# Sample every 0.2 s: established and listening TCP/UDP sockets of the server.
( while kill -0 "$PID" 2>/dev/null; do lsof -nP -a -p "$PID" -i 2>/dev/null | tail -n +2; sleep 0.2; done ) >"$WORK/sockets.txt" &
SAMPLER=$!

status=0
./bin/rehearse -url "ws://127.0.0.1:$PORT/ws" -wait-reminder "${WAIT_REMINDER:-75s}" || status=$?

kill "$SAMPLER" 2>/dev/null || true
echo
echo "== network check ($(wc -l <"$WORK/sockets.txt" | tr -d ' ') socket samples)"
# Every endpoint must be loopback: 127.0.0.0/8, ::1, or localhost.
bad=$(awk '{print $9}' "$WORK/sockets.txt" | tr '>' '\n' | sed 's/-$//' | sort -u |
  grep -vE '^(127\.[0-9.]+|\[::1\]|localhost):[0-9*]+$' || true)
awk '{print $9}' "$WORK/sockets.txt" | sort -u | sed 's/^/   /'
if [[ -n "$bad" ]]; then
  echo "FAIL: non-loopback endpoints:"; echo "$bad" | sed 's/^/   /'
  exit 1
fi
echo "OK: only loopback connections (no internet used)"
[[ $status -eq 0 ]] || { echo "rehearsal reported problems (exit $status); server log: $WORK/server.log"; exit $status; }
echo "rehearsal OK; logs in $WORK"
