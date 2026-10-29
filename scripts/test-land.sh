#!/usr/bin/env bash
# test-land.sh — scripted check of scripts/land.sh against a throwaway bare
# origin (#2583). Runs on Linux and on macOS; it never touches the real origin.
#
# Cases:
#   red      a failing gate: LAND_RESULT FAILED test gate failed, exit != 0,
#            origin main unchanged
#   green    a passing gate: LAND_RESULT SUCCESS <sha>, exit 0, origin main =
#            the feature tip rebased onto a moved origin main
#   nolock   no flock on PATH: FAILED naming the lock, exit != 0, no push
#   lockfail flock present but failing: FAILED naming the lock, no push
#   concur   two landings at once on one lock: the gates never overlap, both land
#
# The red, green and concur cases pass on the pre-#2583 script on Linux
# (characterisation). nolock and lockfail fail on it: it ignored flock's
# failure and pushed without the lock.
#
# usage: bash scripts/test-land.sh [path/to/land.sh]
#   default land.sh: the one next to this script. KEEP=1 keeps the temp dir.
# On the work Mac (ssh work-mac), from a checkout of this repo:
#   ssh work-mac 'cd ~/git/foci && PATH=/opt/homebrew/bin:$PATH bash scripts/test-land.sh'
set -u

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
	sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
	exit 0
fi

here=$(cd "$(dirname "$0")" && pwd)
LAND=${1:-$here/land.sh}
LAND=$(cd "$(dirname "$LAND")" && pwd)/$(basename "$LAND")
[ -f "$LAND" ] || { echo "no land.sh at $LAND" >&2; exit 2; }

# Isolate git from the host: no global hooks, signing or identity.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME="land test" GIT_COMMITTER_NAME="land test"
export GIT_AUTHOR_EMAIL=land@test.invalid GIT_COMMITTER_EMAIL=land@test.invalid
# A parent make's flags must not leak into the gate's make.
unset MAKEFLAGS MFLAGS MAKELEVEL

TMP=$(mktemp -d "${TMPDIR:-/tmp}/test-land.XXXXXX")
[ -n "${KEEP:-}" ] || trap 'rm -rf "$TMP"' EXIT
fails=0
pass() { echo "PASS $*"; }
fail() { echo "FAIL $*"; fails=$((fails + 1)); }

# setup NAME: a bare origin with main, a clone, and a feature worktree with one
# commit; then origin main moves on by one commit (so landing must rebase).
setup() {
	local d="$TMP/$1"
	mkdir -p "$d"
	git init -q --bare "$d/origin.git"
	git -C "$d/origin.git" symbolic-ref HEAD refs/heads/main
	git init -q -b main "$d/seed"
	cat >"$d/seed/Makefile" <<'EOF'
pass:
	@echo gate-pass
fail:
	@echo gate-fail; exit 1
slow:
	@echo "start $$GATE_NAME" >> "$$GATE_LOG"; sleep 2; echo "end $$GATE_NAME" >> "$$GATE_LOG"
EOF
	echo base >"$d/seed/base.txt"
	git -C "$d/seed" add -A && git -C "$d/seed" commit -q -m base
	git -C "$d/seed" push -q "$d/origin.git" main
	git clone -q "$d/origin.git" "$d/clone"
	git -C "$d/clone" worktree add -q -b feat "$d/feat" origin/main
	echo feature >"$d/feat/feature.txt"
	git -C "$d/feat" add -A && git -C "$d/feat" commit -q -m feature
	echo upstream >"$d/seed/upstream.txt"
	git -C "$d/seed" add -A && git -C "$d/seed" commit -q -m upstream
	git -C "$d/seed" push -q "$d/origin.git" main
}
origin_main() { git -C "$TMP/$1/origin.git" rev-parse main; }

# land NAME TARGET [PATH]: run land.sh from the feature worktree; sets rc, last.
land() {
	local d="$TMP/$1" p="${3:-$PATH}"
	(cd "$d/feat" && PATH="$p" LAND_LOCK="$d/merge.lock" LAND_TEST_TARGET="$2" bash "$LAND") >"$d/out.log" 2>&1
	rc=$?
	last=$(tail -n 1 "$d/out.log")
}

# --- red gate ---
setup red
before=$(origin_main red)
land red fail
if [ "$rc" -ne 0 ] && [ "$last" = ">>> LAND_RESULT: FAILED test gate failed" ] && [ "$(origin_main red)" = "$before" ]; then
	pass "red: rc=$rc, '$last', origin main unchanged"
else
	fail "red: rc=$rc last='$last' origin $(origin_main red) (was $before)"; cat "$TMP/red/out.log"
fi

# --- green gate ---
setup green
upstream=$(origin_main green)
land green pass
after=$(origin_main green)
short=$(git -C "$TMP/green/origin.git" rev-parse --short main)
if [ "$rc" -eq 0 ] && [ "$last" = ">>> LAND_RESULT: SUCCESS $short" ] &&
	[ "$(git -C "$TMP/green/origin.git" rev-parse main~1)" = "$upstream" ] &&
	[ "$(git -C "$TMP/green/origin.git" show main:feature.txt)" = feature ] &&
	[ "$(git -C "$TMP/green/origin.git" log -1 --format=%s main)" = feature ]; then
	pass "green: rc=0, '$last', origin main = feature rebased onto upstream"
else
	fail "green: rc=$rc last='$last' origin $after (upstream $upstream)"; cat "$TMP/green/out.log"
fi

# --- no lock tool on PATH ---
# A PATH holding every tool land.sh and the gate use, except flock.
shim="$TMP/shim"
mkdir -p "$shim"
for t in git make bash sh date awk comm sort sed cat tail env echo sleep true false; do
	src=$(type -P "$t") || { fail "nolock: setup: no $t on PATH"; continue; }
	ln -s "$src" "$shim/$t"
done
setup nolock
before=$(origin_main nolock)
land nolock pass "$shim"
case "$last" in
">>> LAND_RESULT: FAILED "*lock*) okmsg=1 ;;
*) okmsg=0 ;;
esac
if [ "$rc" -ne 0 ] && [ "$okmsg" = 1 ] && [ "$(origin_main nolock)" = "$before" ] && ! grep -q gate-pass "$TMP/nolock/out.log"; then
	pass "nolock: rc=$rc, '$last', gate not run, origin main unchanged"
else
	fail "nolock: rc=$rc last='$last' origin $(origin_main nolock) (was $before)"; cat "$TMP/nolock/out.log"
fi

# --- flock present but failing ---
badlock="$TMP/badlock"
mkdir -p "$badlock"
printf '#!/bin/sh\necho "flock: simulated failure" >&2\nexit 1\n' >"$badlock/flock"
chmod +x "$badlock/flock"
setup lockfail
before=$(origin_main lockfail)
land lockfail pass "$badlock:$PATH"
case "$last" in
">>> LAND_RESULT: FAILED "*lock*) okmsg=1 ;;
*) okmsg=0 ;;
esac
if [ "$rc" -ne 0 ] && [ "$okmsg" = 1 ] && [ "$(origin_main lockfail)" = "$before" ] && ! grep -q gate-pass "$TMP/lockfail/out.log"; then
	pass "lockfail: rc=$rc, '$last', gate not run, origin main unchanged"
else
	fail "lockfail: rc=$rc last='$last' origin $(origin_main lockfail) (was $before)"; cat "$TMP/lockfail/out.log"
fi

# --- two concurrent landings share one lock ---
setup concur
d="$TMP/concur"
git -C "$d/clone" worktree add -q -b feat2 "$d/feat2" origin/main
echo two >"$d/feat2/two.txt"
git -C "$d/feat2" add -A && git -C "$d/feat2" commit -q -m two
export GATE_LOG="$d/gates.log"
(cd "$d/feat" && GATE_NAME=A LAND_LOCK="$d/merge.lock" LAND_TEST_TARGET=slow bash "$LAND") >"$d/a.log" 2>&1 &
pa=$!
(cd "$d/feat2" && GATE_NAME=B LAND_LOCK="$d/merge.lock" LAND_TEST_TARGET=slow bash "$LAND") >"$d/b.log" 2>&1 &
pb=$!
wait "$pa"; ra=$?
wait "$pb"; rb=$?
seq=$(tr '\n' ' ' <"$GATE_LOG")
case "$seq" in
"start A end A start B end B " | "start B end B start A end A ") serial=1 ;;
*) serial=0 ;;
esac
if [ "$serial" = 1 ] && [ "$ra" -eq 0 ] && [ "$rb" -eq 0 ] &&
	[ "$(git -C "$d/origin.git" show main:feature.txt)" = feature ] &&
	[ "$(git -C "$d/origin.git" show main:two.txt)" = two ]; then
	pass "concur: gates ran one after the other ($seq), both landed"
else
	fail "concur: gates '$seq' rcA=$ra rcB=$rb"; cat "$d/a.log" "$d/b.log"
fi

echo "test-land: $(uname -s), $(bash --version | head -n 1), flock $(command -v flock || echo MISSING), $fails failure(s)"
[ "$fails" -eq 0 ]
