#!/usr/bin/env bash
#
# heavy-lock-make-test.sh — assertions for the Makefile's heavy-lock options
# (foci_todo #2396), on `make -n` output (nothing runs):
#  - default: test / test-one / integration take /tmp/heavy up front and run
#    REAP_GRADLE under it, in the shared per-user test dir;
#  - FGW_HEAVY_LOCK=external: runs the Go cache can cover (test, test-one) take
#    no lock and never run REAP_GRADLE; runs it cannot cover (COUNT=N,
#    integration) still lock up front and pass HEAVY_LOCK_HELD;
#  - FGW_TESTDIR / FGW_SRCLINK replace the per-user test dir and symlink;
#    external without them, or a bad FGW_HEAVY_LOCK value, is refused;
#  - external: bin/llbox is rebuilt only when out of date.
#
# Usage: heavy-lock-make-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,15p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

REPO=$(cd "$(dirname "$0")/../.." && pwd -P)
RC=0
has() { grep -qF -- "$1" <<<"$2"; }
expect() { if has "$2" "$3"; then printf 'ok   %s\n' "$1"; else printf 'FAIL %s\n       missing: %s\n' "$1" "$2"; RC=1; fi; }
reject() { if has "$2" "$3"; then printf 'FAIL %s\n       unexpected: %s\n' "$1" "$2"; RC=1; else printf 'ok   %s\n' "$1"; fi; }

# dry <env assignments...> -- <make args...>: the recipe lines `make -n` prints,
# from a clean environment (this runs inside `make test`, maybe under a caller's
# FGW_* settings).
dry() {
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  env -u MAKEFLAGS -u MAKELEVEL -u MFLAGS -u FGW_HEAVY_LOCK -u FGW_TESTDIR -u FGW_SRCLINK -u HEAVY_LOCK_HELD \
    "${envs[@]}" make -n --no-print-directory -C "$REPO" "$@" 2>&1
}
user=$(id -un)
FLOCK='flock 9; echo ">>> acquired heavy lock'
REAP='heavy-lock-reap-gradle.sh'
EXT=(FGW_HEAVY_LOCK=external FGW_TESTDIR=/tmp/fgw/run-zz2396 FGW_SRCLINK=/tmp/fgw/src-zz2396)

for target in test "test-one PKG=./internal/zz/" "test-one PKG=./internal/zz/ COUNT=2" integration; do
  # shellcheck disable=SC2086 # the target holds make arguments
  out=$(dry -- $target)
  expect "default $target: takes the lock" "$FLOCK" "$out"
  expect "default $target: reaps under it" "$REAP" "$out"
  reject "default $target: no HEAVY_LOCK_HELD" "HEAVY_LOCK_HELD" "$out"
done
out=$(dry -- test-one PKG=./internal/zz/)
expect "default: the shared per-user test dir" "rm -rf /tmp/fgw/run-$user && mkdir -p /tmp/fgw/run-$user/home" "$out"
expect "default: the shared per-user symlink" "ln -sfn $REPO /tmp/fgw/src-$user" "$out"

for target in test "test-one PKG=./internal/zz/"; do
  # shellcheck disable=SC2086
  out=$(dry "${EXT[@]}" -- $target)
  reject "external $target: no lock" "$FLOCK" "$out"
  reject "external $target: no reap without the lock" "$REAP" "$out"
  expect "external $target: says the caller locks" "heavy lock: left to the caller (FGW_HEAVY_LOCK=external)" "$out"
  expect "external $target: the run's own test dir" "rm -rf /tmp/fgw/run-zz2396 && mkdir -p /tmp/fgw/run-zz2396/home" "$out"
  expect "external $target: the run's own symlink" "ln -sfn $REPO /tmp/fgw/src-zz2396 && touch -h -d @946684800 /tmp/fgw/src-zz2396 && cd /tmp/fgw/src-zz2396" "$out"
done
for target in "test-one PKG=./internal/zz/ COUNT=2" integration; do
  # shellcheck disable=SC2086
  out=$(dry "${EXT[@]}" -- $target)
  expect "external $target (not cacheable): takes the lock up front" "$FLOCK" "$out"
  expect "external $target: reaps under it" "$REAP" "$out"
  expect "external $target: tells the tests it is held" "HEAVY_LOCK_HELD=/tmp/heavy " "$out"
done

# llbox: rebuilt on every call by default; with external only when out of date (a link is always a Go cache miss,
# which would make the caller's helper take the lock on every call). -o bin/llbox: make takes it as up to date.
LLBOX_BUILD='go build -o ../../bin/llbox .'
out=$(dry -- -o bin/llbox test-one PKG=./internal/zz/)
expect "default: llbox is rebuilt every call" "$LLBOX_BUILD" "$out"
out=$(dry "${EXT[@]}" -- -o bin/llbox test-one PKG=./internal/zz/)
reject "external: an up-to-date llbox is not rebuilt" "$LLBOX_BUILD" "$out"
out=$(dry "${EXT[@]}" -- -W scripts/llbox/main.go test-one PKG=./internal/zz/)
expect "external: a stale llbox is rebuilt" "$LLBOX_BUILD" "$out"

out=$(dry FGW_HEAVY_LOCK=external -- test-one PKG=./internal/zz/)
expect "external without its own test dir: refused" "FGW_HEAVY_LOCK=external needs FGW_TESTDIR and FGW_SRCLINK" "$out"
out=$(dry FGW_HEAVY_LOCK=yes -- test)
expect "a bad FGW_HEAVY_LOCK: refused" 'FGW_HEAVY_LOCK must be unset or "external", not "yes"' "$out"

exit "$RC"
