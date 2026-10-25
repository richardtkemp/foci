#!/bin/bash
# test-memcap.sh — `go test -exec` wrapper that caps a test binary's own memory.
#
# Runs the test binary given as arguments and polls its resident memory (VmRSS,
# the binary alone: children such as a foci-gw it starts or a `go build` it
# runs are NOT counted). If it passes the limit, the binary gets SIGQUIT (Go
# prints every goroutine's stack and exits 2) and, if still alive 5 s later,
# SIGKILL. The package then fails like any other test failure.
#
# Why: factories (and agents) run tests against broken code. A test whose
# failure mode is "spin and allocate" otherwise grows to ~10 GB until foci's
# memory_guard kills it under box-wide memory pressure (foci_todo #2435, #2436).
#
# Limit: FOCI_TEST_MEM_LIMIT_MB, default 1024 (Dick, 2026-10-15). Measured
# 2026-10-15: the largest test binary's own peak was 546 MB (browser.test).
#
# seal-test.sh passes it as -exec to EVERY go test call, on the NUC and the Mac,
# normal and factory runs alike (foci_todo #2559, Dick 2026-10-25: "cap
# everywhere + cached"). go test passes -test.testlogfile only when there is no
# -exec program, and caches a result only from that log, so this wrapper adds
# the flag itself (below): capped runs stay cacheable.
#
# Usage: go test -exec /abs/path/scripts/test-memcap.sh ...   (go appends the
# test binary and its flags). -h/--help prints this header.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

limit_mb=${FOCI_TEST_MEM_LIMIT_MB:-1024}
limit_kb=$((limit_mb * 1024))

# go's test-result cache (foci_todo #2559): go test caches a result only from
# the log its test binary writes to -test.testlogfile (the env vars and files
# the tests read), and passes that flag only when there is no -exec program, so
# under a plain wrapper no result was ever cached. go 1.26 and 1.27 name that
# log <the binary's dir>/testlog.txt (cmd/go/internal/test/test.go:
# runAction.Objdir is the test binary's dir) and read it back after the run
# whatever ran the binary, so the flag is added here. Under -count or another
# uncacheable flag go ignores the file. A flag go already passed is left alone.
if [ -f "${1:-}" ] && [[ "$1" == *.test ]]; then
  case " $* " in
  *" -test.testlogfile="*) ;;
  *) set -- "$1" "-test.testlogfile=$(dirname "$1")/testlog.txt" "${@:2}" ;;
  esac
fi

# No /proc (macOS): nothing to poll, so run the binary directly, as before
# (the poll below read nothing there and never tripped).
#
# macOS also scans every NEW executable file with XProtect before its first run,
# one file at a time, system-wide: about 40 MB/s on the Pro, so first-running
# the suite's 90 test binaries (890 MB) took 23 s, the whole of go test's time
# there (a second run of the same files: 0.14 s). go test links each binary into
# a new file on every run, so every run paid it, even for byte-identical
# binaries (foci_todo #2551). With FOCI_TEST_BIN_CACHE set (seal-test.sh sets
# it on macOS), the binary is run from a copy named by its MD5 in that dir:
# the same bytes are the same program, and a copy that was run before is not
# scanned again. A new binary is copied (an APFS clone) and scanned once, as
# before. FOCI_TEST_BIN_REUSE=0 turns this off.
if [ ! -r /proc/self/status ]; then
  if [ -n "${FOCI_TEST_BIN_CACHE:-}" ] && [ "${FOCI_TEST_BIN_REUSE:-1}" != 0 ] && [ -f "$1" ]; then
    bin=$1
    if sum=$(md5 -q "$bin") && [ -n "$sum" ]; then
      dir=$FOCI_TEST_BIN_CACHE/$sum
      copy=$dir/$(basename "$bin")
      if [ ! -x "$copy" ]; then
        mkdir -p "$dir" && { cp -c "$bin" "$copy.$$" 2>/dev/null || cp "$bin" "$copy.$$"; } && mv -f "$copy.$$" "$copy"
      fi
      if [ -x "$copy" ]; then
        touch -c "$dir" # seal-test.sh's sweep removes copies not run for 12 h
        shift
        exec "$copy" "$@"
      fi
    fi
  fi
  exec "$@"
fi

"$@" &
pid=$!
# go test stops a hung run by signalling this wrapper; pass it on.
trap 'kill -TERM "$pid" 2>/dev/null' TERM
trap 'kill -INT "$pid" 2>/dev/null' INT

# The poller runs beside the binary, and this shell waits on the binary itself,
# so the wrapper returns the moment the binary exits. (It used to poll in the
# foreground and notice the exit only at its next 0.2 s sleep: up to 0.2 s on
# every package, foci_todo #2551.) The poller exits 3 if it has tripped, else 0.
# Its sleeps run in the background with their stdio on /dev/null, so a sleep
# left over when the poller is stopped holds no pipe go test is reading.
(
  tripped=
  sp=
  trap 'kill "$sp" 2>/dev/null; [ -n "$tripped" ] && exit 3; exit 0' TERM
  # shellcheck disable=SC2217 # the stdin redirect is so the sleep holds no inherited fd
  nap() { sleep "$1" </dev/null >/dev/null 2>&1 & sp=$!; wait "$sp"; }
  while :; do
    rss=0
    # Builtin read only: no fork per poll.
    while read -r key val _; do
      if [ "$key" = "VmRSS:" ]; then rss=$val; break; fi
    done 2>/dev/null < "/proc/$pid/status" || exit 0
    if [ "$rss" -gt "$limit_kb" ]; then
      tripped=1
      echo "test-memcap: $(basename "$1") passed the ${limit_mb} MB memory cap (RSS $((rss / 1024)) MB); sending SIGQUIT for a goroutine dump (see scripts/test-memcap.sh)" >&2
      kill -QUIT "$pid" 2>/dev/null
      nap 5
      kill -KILL "$pid" 2>/dev/null
      exit 3
    fi
    nap 0.2
  done
) &
poller=$!
# wait returns early (status > 128) when a trapped signal arrives: wait again
# until the binary itself has gone.
while :; do
  wait "$pid"
  rc=$?
  kill -0 "$pid" 2>/dev/null || break
done
kill "$poller" 2>/dev/null
wait "$poller"
if [ $? -eq 3 ]; then
  echo "test-memcap: $(basename "$1") stopped by the ${limit_mb} MB memory cap" >&2
  [ "$rc" -eq 0 ] && rc=1
fi
exit "$rc"
