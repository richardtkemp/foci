#!/usr/bin/env bash
#
# seal-test-test.sh — assertions for scripts/seal-test.sh:
#  - the diagnostic re-run (#2132): a sealed failure that passes on re-run both
#    unsealed and sealed is a flake, and only one that fails sealed again is
#    blamed on the sandbox;
#  - FGW_HEAVY_LOCK=external (#2396): the diagnostic re-runs take the heavy
#    lock and pass HEAVY_LOCK_HELD;
#  - heavy-lock lines (#2412): go's `>>> waiting for/acquired heavy lock` lines
#    reach stderr as they come, every other line only LOGFILE, and the exit
#    status is still go test's;
#  - the fixed test environment (#2348): go test sees env -i plus a pinned
#    PATH, whatever the caller's PATH and env, and every run passes -v;
#  - the memory cap (#2559): every go test call runs its binaries under
#    scripts/test-memcap.sh (-exec); unit COUNT=N is -count=N.
#
# Stubs `go` (in a fake GOROOT, first on PATH) and bin/llbox (in a scratch
# cwd) so no real test runs. Tests start from env -i, so the stubs read their
# settings from files, not env vars.
#
# Usage: seal-test-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,20p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

HERE=$(cd "$(dirname "$0")/.." && pwd)
SEALTEST="$HERE/seal-test.sh"
TMP=$(mktemp -d /tmp/seal-test-test-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0

# bin/llbox stub: drop the whitelist args, mark the tree as sealed, run it.
# The command is `env -i <assignments> ...`, so SEALED goes in after the -i.
mkdir -p "$TMP/work/bin" "$TMP/goroot/bin" "$TMP/testdir/home"
cat > "$TMP/work/bin/llbox" <<'EOF'
#!/usr/bin/env bash
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
if [ "$1" = env ] && [ "$2" = -i ]; then shift 2; exec env -i SEALED=1 "$@"; fi
SEALED=1 exec "$@"
EOF
# go stub in a fake GOROOT: `go env GOROOT` names it. $GOROOT/mode picks when
# `go test` fails:
#   pass    never           flake   only on its first call
#   sandbox whenever sealed real    always
# Each `go test` call appends its arguments and environment to $GOROOT/seen.
cat > "$TMP/goroot/bin/go" <<'EOF'
#!/usr/bin/env bash
root=$(cd "$(dirname "$0")/.." && pwd)
if [ "$1" = env ] && [ "$2" = GOROOT ]; then echo "$root"; exit 0; fi
# go list ./... (the unit run's package list): two packages, the slow one second;
# $root/listfail makes it fail. Not a recorded call.
if [ "$1" = list ]; then
  [ -e "$root/listfail" ] && { echo "go: zz list failure" >&2; exit 1; }
  printf 'foci/zz-a\nfoci/internal/agent\n'; exit 0
fi
n=$(( $(cat "$root/calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$root/calls"
{ echo "--- call $n: $*"; env | grep -v '^SEALED=' | sort; } >> "$root/seen"
# Whether the heavy lock file named in $root/lockpath is held while this call runs.
if [ -s "$root/lockpath" ]; then
  if flock -n "$(cat "$root/lockpath")" true; then echo "call $n: lock free"; else echo "call $n: lock held"; fi >> "$root/locks"
fi
# $root/lines: lines to print on stderr first, as the build-cache helper does
# (@N@ -> the call number). $root/hold: then wait (up to 20 s) for $root/release.
if [ -s "$root/lines" ]; then sed "s/@N@/$n/g" "$root/lines" >&2; fi
if [ -e "$root/hold" ]; then
  for _ in $(seq 200); do [ -e "$root/release" ] && break; sleep 0.1; done
fi
pkg="${!#}"
fail=0
case "$(cat "$root/mode")" in
flake) [ "$n" -eq 1 ] && fail=1 ;;
sandbox) [ -n "${SEALED:-}" ] && fail=1 ;;
real) fail=1 ;;
esac
if [ "$fail" = 1 ]; then printf 'FAIL\t%s\t0.01s\n' "$pkg"; exit 1; fi
printf 'ok  \t%s\t0.01s\n' "$pkg"
EOF
chmod +x "$TMP/work/bin/llbox" "$TMP/goroot/bin/go"

# A here-string, not printf | grep -q: grep -q exits on the first match, the
# printf of a large haystack then dies of SIGPIPE, and pipefail turns the match
# into a miss (which made a reject() pass falsely).
has() { grep -qF -- "$1" <<<"$2"; }
expect() { # expect <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n       missing: %s\n' "$1" "$2"; RC=1; fi
}
reject() { # reject <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'FAIL %s\n       unexpected: %s\n' "$1" "$2"; RC=1
  else printf 'ok   %s\n' "$1"; fi
}

# run <stub mode> [seal-test.sh mode] [caller env assignments...]: seal-test.sh
# (`one` on a fake package by default); prints its output. Every arm starts from
# the default heavy-lock policy: `make test` itself may run under
# FGW_HEAVY_LOCK=external or hold the lock and pass HEAVY_LOCK_HELD (it exports
# both to everything it starts), and the default-mode arms below assert that
# seal-test.sh then takes no lock — so neither may leak in from the caller.
# Arms that test a policy set the vars themselves as caller assignments, which
# env applies after the -u flags, and SEALED/FOCI_TEST_UNSEALED are the stubs'
# own markers from an earlier arm.
run() {
  local stub=$1 mode=${2:-one}
  shift; [ "$#" -gt 0 ] && shift
  rm -f "$TMP/goroot/calls" "$TMP/goroot/seen"
  echo "$stub" > "$TMP/goroot/mode"
  (cd "$TMP/work" && env -u SEALED -u FOCI_TEST_UNSEALED -u FGW_HEAVY_LOCK -u HEAVY_LOCK_HELD -u FGW_HEAVY_ACQUIRED \
    PATH="$TMP/goroot/bin:$PATH" "$@" \
    bash "$SEALTEST" "$mode" "$TMP/testdir" "$TMP/log" 1 "$TMP/gocache" "$TMP/gomod" "$TMP/gopath" ./internal/zz2132/ \
    2>&1)
}

out=$(run pass)
reject "pass: no diagnostic" "DIAGNOSTIC" "$out"

out=$(run flake)
expect "flake: reported as a flake" "the failure is a FLAKE, not a sandbox write" "$out"
reject "flake: not blamed on the sandbox" "writing outside the sandbox" "$out"
expect "flake: the log has the sealed re-run" "--- sealed re-run: ./internal/zz2132/" "$(cat "$TMP/log")"

out=$(run sandbox)
expect "sandbox: blamed on the sandbox" "writing outside the sandbox" "$out"
reject "sandbox: not called a flake" "FLAKE" "$out"

out=$(run real)
expect "real: a real failure" "fails both sealed and unsealed" "$out"
reject "real: no diagnostic" "DIAGNOSTIC" "$out"

# ---- #2550: the shell-script suites run alongside go test --------------------
# The go stub holds until $root/release exists; the first fake suite creates it.
# Serial after go test, the stub would wait its full 20 s; alongside, the run
# ends in a couple of seconds. The second suite fails: the run must fail, the
# log must carry both suites in order in the format the loop wrote directly.
mkdir -p "$TMP/work/scripts/tests" "$TMP/work/shared/scripts/tests"
cat > "$TMP/work/scripts/tests/zz-a-test.sh" <<SUITE_A
#!/usr/bin/env bash
echo "zz-a ran"
touch "$TMP/goroot/release"
SUITE_A
cat > "$TMP/work/scripts/tests/zz-b-test.sh" <<'SUITE_B'
#!/usr/bin/env bash
echo "zz-b failing"
exit 1
SUITE_B
rm -f "$TMP/goroot/release" "$TMP/log"; touch "$TMP/goroot/hold"
t0=$(date +%s)
out=$(run pass unit); rc=$?
t1=$(date +%s)
rm -f "$TMP/goroot/hold" "$TMP/goroot/release" "$TMP/work/scripts/tests/zz-a-test.sh" "$TMP/work/scripts/tests/zz-b-test.sh"
if [ $((t1 - t0)) -lt 15 ]; then printf 'ok   %s\n' "scripts: run alongside go test (took $((t1 - t0)) s, not the stub's 20 s hold)"
else printf 'FAIL %s\n' "scripts: run alongside go test (took $((t1 - t0)) s)"; RC=1; fi
if [ "$rc" -ne 0 ]; then printf 'ok   %s\n' "scripts: a failing suite fails the run"
else printf 'FAIL %s\n' "scripts: a failing suite fails the run (rc=$rc)"; RC=1; fi
slog=$(cat "$TMP/log")
expect "scripts: first suite marker and output in the log" "=== script test: scripts/tests/zz-a-test.sh ===
zz-a ran" "$slog"
expect "scripts: second suite marker and FAIL line in the log" "=== script test: scripts/tests/zz-b-test.sh ===
zz-b failing
FAIL script test: scripts/tests/zz-b-test.sh" "$slog"
case "$slog" in
*"zz-a-test.sh ==="*"zz-b-test.sh ==="*) printf 'ok   %s\n' "scripts: suites are in order in the log" ;;
*) printf 'FAIL %s\n' "scripts: suites are in order in the log"; RC=1 ;;
esac

# ---- #2348: the environment and flags go test sees -------------------------
# A factory snapshot dir first on PATH, and agent-shell vars, must not reach
# the tests: they would change go's test-cache key for the same code.
SYS="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
run pass one PATH="/srv/snap-aaa/bin:$TMP/goroot/bin:$PATH" CLAUDE_CODE_ZZ=1 FOCI_SOCK=/x LD_PRELOAD= \
  FOCI_OPENCODE_LIVE=1 GOFLAGS=-mod=mod >/dev/null
seenA=$(cat "$TMP/goroot/seen")
WANT_PATH="$TMP/goroot/bin:$SYS"
# macOS: seal-test.sh adds the Makefile's Homebrew dirs (DARWIN_TOOL_PATH_FIRST
# ahead, DARWIN_TOOL_PATH behind); still pinned, only longer.
if [ "$(uname -s)" = Darwin ] && [ -n "${DARWIN_TOOL_PATH:-}" ]; then
  WANT_PATH="${DARWIN_TOOL_PATH_FIRST:+$DARWIN_TOOL_PATH_FIRST:}$WANT_PATH:$DARWIN_TOOL_PATH"
fi
# macOS: and a fixed dir with sh -> /bin/dash goes first (#2549).
if [ "$(uname -s)" = Darwin ] && [ -x /bin/dash ]; then
  WANT_PATH="/private/tmp/fgw-shbin:$WANT_PATH"
  expect "env: macOS sh shim points at /bin/dash" "/bin/dash" "$(readlink /private/tmp/fgw-shbin/sh)"
fi
expect "env: PATH is pinned" "PATH=$WANT_PATH" "$seenA"
reject "env: the caller's PATH entry is dropped" "/srv/snap-aaa" "$seenA"
reject "env: an agent-shell var is dropped" "CLAUDE_CODE_ZZ" "$seenA"
reject "env: FOCI_SOCK is dropped" "FOCI_SOCK" "$seenA"
reject "env: LD_PRELOAD is dropped" "LD_PRELOAD" "$seenA"
expect "env: USER is the account" "USER=$(id -un)" "$seenA"
expect "env: HOME is the sandbox home" "HOME=$TMP/testdir/home" "$seenA"
# The Makefile runs seal-test.sh from a fixed symlink to the checkout (#2238);
# go trusts $PWD for the cwd, so the LOGICAL path must survive env -i.
ln -sfn "$TMP/work" "$TMP/worklink"
(cd "$TMP/worklink" && echo pass > "$TMP/goroot/mode" && rm -f "$TMP/goroot/seen" &&
  PATH="$TMP/goroot/bin:$PATH" bash "$SEALTEST" one "$TMP/testdir" "$TMP/log" 1 "$TMP/gocache" "$TMP/gomod" \
    "$TMP/gopath" ./internal/zz2132/ >/dev/null 2>&1)
expect "env: PWD is the logical (symlink) cwd" "PWD=$TMP/worklink" "$(cat "$TMP/goroot/seen")"
expect "env: an opt-in var passes" "FOCI_OPENCODE_LIVE=1" "$seenA"
expect "env: Go config passes" "GOFLAGS=-mod=mod" "$seenA"
expect "one: -v without V=1" "test -trimpath -ldflags=-s -w -parallel=16 -v " "$seenA"

# Two callers with different PATHs and shells: the same test env.
run pass one PATH="/srv/snap-bbb/bin:$TMP/goroot/bin:$PATH" OTEL_ZZ=1 FOCI_OPENCODE_LIVE=1 GOFLAGS=-mod=mod >/dev/null
seenB=$(cat "$TMP/goroot/seen")
if [ "$(grep -v '^--- call' <<<"$seenA")" = "$(grep -v '^--- call' <<<"$seenB")" ]; then
  echo "ok   env: identical for two different callers"
else
  echo "FAIL env: differs between two callers"; diff <(echo "$seenA") <(echo "$seenB"); RC=1
fi

# An opt-in value (an API key) reaches the test but never the log.
run pass one ANTHROPIC_API_KEY=sk-zz2348-secret >/dev/null
expect "opt-in: the key reaches the test" "ANTHROPIC_API_KEY=sk-zz2348-secret" "$(cat "$TMP/goroot/seen")"
reject "opt-in: the key is not in the log" "sk-zz2348-secret" "$(cat "$TMP/log")"
expect "opt-in: the log says it is set" "ANTHROPIC_API_KEY=<set>" "$(cat "$TMP/log")"

# make test (unit) and test-one pass the same test flags, so they share go's
# test cache: compare `go test <flags> -timeout <t>` without -p and the package.
run pass unit >/dev/null
unitcall=$(grep -m1 '^--- call' "$TMP/goroot/seen")
unitargs=$(sed -E 's/^--- call [0-9]+: //; s/ -p=[0-9]+//; s/ foci\/internal\/agent foci\/zz-a$//' <<<"$unitcall")
# foci_todo #2551: the unit run names go list's packages, a SLOW_FIRST one first.
case "$unitcall" in
*" foci/internal/agent foci/zz-a") echo "ok   order: the unit run starts the slow packages first" ;;
*) echo "FAIL order: want '... foci/internal/agent foci/zz-a', got: $unitcall"; RC=1 ;;
esac
: > "$TMP/goroot/listfail"
run pass unit >/dev/null
expect "order: a failing go list falls back to ./..." " ./..." "$(grep -m1 '^--- call' "$TMP/goroot/seen")"
rm -f "$TMP/goroot/listfail"
run pass unit >/dev/null
run pass one >/dev/null
oneargs=$(grep -m1 '^--- call' "$TMP/goroot/seen" | sed -E 's/^--- call [0-9]+: //; s/ \.\/internal\/zz2132\/$//')
if [ -n "$unitargs" ] && [ "$unitargs" = "$oneargs" ]; then
  echo "ok   flags: unit and one pass the same test flags ($oneargs)"
else
  printf 'FAIL flags: unit [%s] vs one [%s]\n' "$unitargs" "$oneargs"; RC=1
fi

# ---- #2559: every run is capped (-exec); COUNT on unit ---------------------
WRAPPER="-exec $TMP/work/scripts/test-memcap.sh"
for mode in one unit integration; do
  run pass "$mode" >/dev/null
  expect "memcap $mode: the binaries run under the wrapper" "$WRAPPER" "$(grep '^--- call' "$TMP/goroot/seen")"
done
run sandbox one >/dev/null
expect "memcap: the unsealed re-run is capped" "$WRAPPER" "$(grep '^--- call 2' "$TMP/goroot/seen")"
expect "memcap: the sealed re-run is capped" "$WRAPPER" "$(grep '^--- call 3' "$TMP/goroot/seen")"
run sandbox one >/dev/null
expect "re-run: still -count=1 (runs for real)" "-count=1" "$(grep '^--- call 2' "$TMP/goroot/seen")"
run pass unit >/dev/null
reject "unit: no -count by default (cached)" "-count" "$(grep '^--- call' "$TMP/goroot/seen")"
reject "unit: the log header does not say UNCACHED" "UNCACHED" "$(cat "$TMP/log")"
rm -f "$TMP/goroot/calls" "$TMP/goroot/seen"; echo pass > "$TMP/goroot/mode"
out=$(cd "$TMP/work" && PATH="$TMP/goroot/bin:$PATH" bash "$SEALTEST" unit "$TMP/testdir" "$TMP/log" 1 \
  "$TMP/gocache" "$TMP/gomod" "$TMP/gopath" "" "" "" 1 2>&1)
expect "unit COUNT=1: go test gets -count=1" " -count=1 " "$(grep -m1 '^--- call' "$TMP/goroot/seen")"
expect "unit COUNT=1: the log header says UNCACHED" "sealed unit suite -count=1 (UNCACHED)" "$(cat "$TMP/log")"
expect "unit COUNT=1: the caller is told" "go's test cache is bypassed, every package runs" "$out"
out=$(cd "$TMP/work" && PATH="$TMP/goroot/bin:$PATH" bash "$SEALTEST" unit "$TMP/testdir" "$TMP/log" 1 \
  "$TMP/gocache" "$TMP/gomod" "$TMP/gopath" "" "" "" x 2>&1); rc=$?
if [ "$rc" = 2 ]; then echo "ok   unit COUNT=x: refused"; else echo "FAIL unit COUNT=x: rc $rc, want 2"; RC=1; fi

# ---- #2396: the heavy lock with FGW_HEAVY_LOCK=external -------------------
# The caller locks lazily (on a Go cache miss), so seal-test.sh must take the
# lock itself for the -count=1 diagnostic re-runs, which the cache cannot cover,
# and tell them with HEAVY_LOCK_HELD.
: > "$TMP/heavy"
echo "$TMP/heavy" > "$TMP/goroot/lockpath"
rm -f "$TMP/goroot/locks"
run sandbox one FGW_HEAVY_LOCK=external SEAL_TEST_HEAVY_LOCK="$TMP/heavy" >/dev/null   # sealed fails, unsealed passes: both re-runs run
locks=$(cat "$TMP/goroot/locks"); seen=$(cat "$TMP/goroot/seen")
expect "external: the sealed run is not locked by seal-test.sh" "call 1: lock free" "$locks"
expect "external: the unsealed re-run holds the lock" "call 2: lock held" "$locks"
expect "external: the sealed re-run holds the lock" "call 3: lock held" "$locks"
expect "external: the re-runs are told the lock is held" "HEAVY_LOCK_HELD=$TMP/heavy" "$(sed -n '/^--- call 2/,$p' <<<"$seen")"
reject "external: the first run is not told so" "HEAVY_LOCK_HELD" "$(sed -n '/^--- call 1/,/^--- call 2/p' <<<"$seen")"
# Default mode: the Makefile holds the lock around all of it; seal-test.sh takes nothing.
rm -f "$TMP/goroot/locks"
run real one SEAL_TEST_HEAVY_LOCK="$TMP/heavy" >/dev/null
reject "default: no re-run lock" "lock held" "$(cat "$TMP/goroot/locks")"
reject "default: no HEAVY_LOCK_HELD" "HEAVY_LOCK_HELD" "$(cat "$TMP/goroot/seen")"
# A caller already holding it (make integration, COUNT=N) passes HEAVY_LOCK_HELD to the
# tests; seal-test.sh then takes nothing for the re-runs.
rm -f "$TMP/goroot/locks"
run real one FGW_HEAVY_LOCK=external HEAVY_LOCK_HELD=/tmp/heavy SEAL_TEST_HEAVY_LOCK="$TMP/heavy" >/dev/null
reject "held by caller: no re-run lock" "lock held" "$(cat "$TMP/goroot/locks")"
expect "held by caller: the tests see HEAVY_LOCK_HELD" "HEAVY_LOCK_HELD=/tmp/heavy" "$(sed -n '/^--- call 1/,/^--- call 2/p' "$TMP/goroot/seen")"
: > "$TMP/goroot/lockpath"

# ---- #2369: the log records when the Makefile took the heavy lock ----------
: > "$TMP/goroot/lines"
run pass one FGW_HEAVY_ACQUIRED=2026-10-21T16:22:00+0000 >/dev/null
expect "acquired stamp: first line of the log" ">>> acquired heavy lock [2026-10-21T16:22:00+0000]" "$(head -n1 "$TMP/log")"
run pass one >/dev/null
reject "no stamp when the Makefile took no lock" ">>> acquired heavy lock" "$(cat "$TMP/log")"

# ---- #2412: the heavy-lock lines reach stderr, live ------------------------
# gocache-overlay -lock prints them on go's stderr; go's output goes to LOGFILE,
# which no caller shows. The factory's timeline charts read them from stderr.
WAITL='>>> waiting for heavy lock ... [12:00:0@N@] (gocache-overlay: /tmp/heavy; another build may be running)'
GOTL='>>> acquired heavy lock [12:00:1@N@] (gocache-overlay: /tmp/heavy)'
printf '%s\n' "$WAITL" "$GOTL" '>>> some other status line' 'a test line: >>> acquired heavy lock [12:00:00]' \
  > "$TMP/goroot/lines"
for mode in one unit integration; do
  rc=0; out=$(run pass "$mode") || rc=$?
  log=$(cat "$TMP/log")
  expect "lines $mode: the waiting line reaches stderr" "${WAITL//@N@/1}" "$out"
  expect "lines $mode: the acquired line reaches stderr" "${GOTL//@N@/1}" "$out"
  reject "lines $mode: other >>> lines stay in the log" "some other status line" "$out"
  reject "lines $mode: a line that only contains the text stays in the log" "a test line" "$out"
  reject "lines $mode: go's own output stays in the log" "0.01s" "$out"
  expect "lines $mode: the log keeps the waiting line" "${WAITL//@N@/1}" "$log"
  expect "lines $mode: the log keeps the other lines" "a test line: >>> acquired" "$log"
  expect "lines $mode: the log keeps go's output" "0.01s" "$log"
  if [ "$rc" = 0 ]; then echo "ok   lines $mode: a pass exits 0"; else echo "FAIL lines $mode: a pass exits $rc"; RC=1; fi
done
rc=0; out=$(run real one) || rc=$?
if [ "$rc" = 1 ]; then echo "ok   lines: a failure still exits 1"; else echo "FAIL lines: a failure exits $rc, want 1"; RC=1; fi
# The diagnostic re-runs (calls 2 and 3) relay theirs too.
out=$(run sandbox one)
expect "lines: the unsealed re-run's lines reach stderr" "${WAITL//@N@/2}" "$out"
expect "lines: the sealed re-run's lines reach stderr" "${GOTL//@N@/3}" "$out"
# Live: the waiting line is on stderr while go test is still running.
: > "$TMP/goroot/hold"; rm -f "$TMP/goroot/release" "$TMP/live.out"
run pass one > "$TMP/live.out" &
pid=$!
live=""
for _ in $(seq 100); do
  if grep -qF -- "${WAITL//@N@/1}" "$TMP/live.out"; then live=1; break; fi
  sleep 0.1
done
: > "$TMP/goroot/release"
wait "$pid"
if [ -n "$live" ]; then echo "ok   lines: the waiting line arrives while go test runs"
else echo "FAIL lines: the waiting line did not arrive while go test ran"; RC=1; fi
rm -f "$TMP/goroot/lines" "$TMP/goroot/hold" "$TMP/goroot/release"

exit "$RC"
