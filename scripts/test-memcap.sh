#!/bin/bash
# test-memcap.sh — `go test -exec` wrapper that caps a test binary's own memory.
#
# Runs the test binary given as arguments and polls its resident memory (VmRSS,
# the binary alone: children such as a foci-gw it starts or a `go build` it
# runs are NOT counted). If it passes the limit, the binary gets SIGQUIT (Go
# prints every goroutine's stack and exits 2) and, if still alive 5 s later,
# SIGKILL. The package then fails like any other test failure.
#
# Why: factories run tests against broken code on purpose. A test whose
# failure mode is "spin and allocate" otherwise grows to ~10 GB until foci's
# memory_guard kills it under box-wide memory pressure (foci_todo #2435, #2436).
#
# Limit: FOCI_TEST_MEM_LIMIT_MB, default 1024 (Dick, 2026-10-15). Measured
# 2026-10-15: the largest test binary's own peak was 546 MB (browser.test).
#
# Usage: go test -exec /abs/path/scripts/test-memcap.sh ...   (go appends the
# test binary and its flags). -h/--help prints this header.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

limit_mb=${FOCI_TEST_MEM_LIMIT_MB:-1024}
limit_kb=$((limit_mb * 1024))

"$@" &
pid=$!
# go test stops a hung run by signalling this wrapper; pass it on.
trap 'kill -TERM "$pid" 2>/dev/null' TERM
trap 'kill -INT "$pid" 2>/dev/null' INT

tripped=
killer=
while kill -0 "$pid" 2>/dev/null; do
  rss=0
  # Builtin read only: no fork per poll.
  while read -r key val _; do
    if [ "$key" = "VmRSS:" ]; then rss=$val; break; fi
  done 2>/dev/null < "/proc/$pid/status"
  if [ -z "$tripped" ] && [ "$rss" -gt "$limit_kb" ]; then
    tripped=1
    echo "test-memcap: $(basename "$1") passed the ${limit_mb} MB memory cap (RSS $((rss / 1024)) MB); sending SIGQUIT for a goroutine dump (see scripts/test-memcap.sh)" >&2
    kill -QUIT "$pid" 2>/dev/null
    ( sleep 5; kill -KILL "$pid" 2>/dev/null ) &
    killer=$!
  fi
  sleep 0.2
done
wait "$pid"
rc=$?
[ -n "$killer" ] && kill "$killer" 2>/dev/null
if [ -n "$tripped" ]; then
  echo "test-memcap: $(basename "$1") stopped by the ${limit_mb} MB memory cap" >&2
  [ "$rc" -eq 0 ] && rc=1
fi
exit "$rc"
