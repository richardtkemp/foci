#!/usr/bin/env bash
#
# test-memcap-test.sh — assertions for scripts/test-memcap.sh, the go test -exec
# wrapper that caps a test binary's own memory (foci_todo #2436):
#  - the binary's exit status comes back unchanged (0 and non-zero);
#  - the wrapper returns as soon as the binary exits, not at its next poll
#    (it used to add up to 0.2 s to every package, foci_todo #2551);
#  - on Linux, a binary over the cap gets SIGQUIT, the run fails, and both
#    test-memcap lines are printed.
# A background job of a non-interactive bash starts with SIGQUIT ignored, so the
# hog below restores the default action, as a Go test binary does.
#
# Usage: test-memcap-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,14p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

MEMCAP="$(cd "$(dirname "$0")/.." && pwd)/test-memcap.sh"
RC=0
ok()   { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n       %s\n' "$1" "$2"; RC=1; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d", time * 1000'; }

bash "$MEMCAP" bash -c 'exit 0'; rc=$?
[ "$rc" -eq 0 ] && ok "exit 0 passes through" || fail "exit 0 passes through" "got $rc"
bash "$MEMCAP" bash -c 'exit 7'; rc=$?
[ "$rc" -eq 7 ] && ok "exit 7 passes through" || fail "exit 7 passes through" "got $rc"

# The wrapper must return when the binary exits, not at its next poll (the old
# one slept 0.2 s in the foreground after every binary). A wall-clock bound is
# at the mercy of a loaded host, so instead every `sleep` the wrapper runs takes
# 30 s here: a wrapper that waits on one is cut off by `timeout 10`, one that
# does not returns at once. TMP holds the slow sleep; the real one is named
# by path.
TMP=$(mktemp -d /tmp/test-memcap-test-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
REAL_SLEEP=$(command -v sleep)
printf '#!/bin/sh\nexec %s 30\n' "$REAL_SLEEP" > "$TMP/sleep" && chmod +x "$TMP/sleep"
# The "binary" lives 0.3 s, so a polling wrapper is inside its loop when it exits.
LIVE=(perl -e 'select(undef, undef, undef, 0.3)')
PATH="$TMP:$PATH" timeout 10 bash "$MEMCAP" "${LIVE[@]}"; rc=$?
[ "$rc" -eq 0 ] && ok "returns when the binary exits, not after a poll sleep" \
  || fail "returns when the binary exits, not after a poll sleep" "rc $rc (124: it waited on a 30 s sleep)"

# go test reads the binary's stdout until EOF: nothing the wrapper leaves
# behind (such as a poll sleep) may hold that pipe open.
out=$(PATH="$TMP:$PATH" timeout 10 bash "$MEMCAP" perl -e 'select(undef, undef, undef, 0.3); print "hi\n"'); rc=$?
[ "$rc" -eq 0 ] && [ "$out" = hi ] && ok "the output pipe closes with the binary" \
  || fail "the output pipe closes with the binary" "rc $rc out='$out' (124: the pipe stayed open on a 30 s sleep)"

if [ -r /proc/self/status ]; then
  t0=$(now_ms)
  out=$(FOCI_TEST_MEM_LIMIT_MB=20 bash "$MEMCAP" python3 -c 'import signal, time; signal.signal(signal.SIGQUIT, signal.SIG_DFL); a = bytearray(80 * 1024 * 1024); time.sleep(30)' 2>&1); rc=$?
  ms=$(( $(now_ms) - t0 ))
  [ "$rc" -ne 0 ] && ok "over the cap: the run fails (rc $rc)" || fail "over the cap: the run fails" "rc 0"
  case "$out" in *"passed the 20 MB memory cap"*) ok "over the cap: says it tripped";; *) fail "over the cap: says it tripped" "$out";; esac
  case "$out" in *"stopped by the 20 MB memory cap"*) ok "over the cap: says it stopped the binary";; *) fail "over the cap: says it stopped the binary" "$out";; esac
  # The hog dies of SIGQUIT (a Go test binary dumps its goroutines and exits), so
  # the 5 s SIGKILL fallback is not reached.
  [ "$ms" -lt 4000 ] && ok "over the cap: stopped by SIGQUIT in ${ms} ms" || fail "over the cap: stopped by SIGQUIT promptly" "${ms} ms"
else
  ok "no /proc here: the cap is not enforced (skipped the over-the-cap checks)"
  # macOS: byte-identical binaries run from one MD5-named copy (foci_todo #2551).
  mkdir -p "$TMP/b"
  # shellcheck disable=SC2016 # $0 and $* belong to the script being written
  printf '#!/bin/sh\necho "$0 $*"\nexit 5\n' > "$TMP/b/one.test"
  cp "$TMP/b/one.test" "$TMP/b/two.test"
  # shellcheck disable=SC2016
  printf '#!/bin/sh\necho "$0 $*"\nexit 6\n' > "$TMP/b/three.test"
  chmod +x "$TMP/b"/*.test
  r1=$(FOCI_TEST_BIN_CACHE="$TMP/cache" bash "$MEMCAP" "$TMP/b/one.test" -x y); rc1=$?
  r2=$(FOCI_TEST_BIN_CACHE="$TMP/cache" bash "$MEMCAP" "$TMP/b/two.test" -x y)
  r3=$(FOCI_TEST_BIN_CACHE="$TMP/cache" bash "$MEMCAP" "$TMP/b/three.test"); rc3=$?
  r4=$(FOCI_TEST_BIN_CACHE="$TMP/cache" FOCI_TEST_BIN_REUSE=0 bash "$MEMCAP" "$TMP/b/one.test")
  case "$r1" in "$TMP/cache/"*"/one.test -x y") ok "reuse: runs a copy in the cache, with the args";; *) fail "reuse: runs a copy in the cache, with the args" "got '$r1'";; esac
  [ "$rc1" -eq 5 ] && [ "$rc3" -eq 6 ] && ok "reuse: the exit status passes through" || fail "reuse: the exit status passes through" "rc $rc1, $rc3"
  [ "${r2%/two.test -x y}" = "${r1%/one.test -x y}" ] && ok "reuse: identical bytes share one copy" || fail "reuse: identical bytes share one copy" "'$r1' vs '$r2'"
  [ "$(dirname "${r3% }")" != "$(dirname "${r1% -x y}")" ] && ok "reuse: different bytes get their own copy" || fail "reuse: different bytes get their own copy" "'$r1' vs '$r3'"
  [ "$r4" = "$TMP/b/one.test " ] && ok "reuse: FOCI_TEST_BIN_REUSE=0 runs the binary itself" || fail "reuse: FOCI_TEST_BIN_REUSE=0 runs the binary itself" "got '$r4'"
fi

exit "$RC"
