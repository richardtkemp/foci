#!/usr/bin/env bash
#
# remote-hosts-test.sh — tests for the several-build-hosts .remote.mk (foci_todo
# #2526): the host choice in mk/remote.mk and the optional per-host build lock.
#
# Hermetic: copies Makefile, mk/ and scripts/remote-run.sh into temp dirs, each
# with its own .remote.mk, and puts a fake `ssh` and a fake `go` first on PATH.
# The fake ssh fails (exit 255, after 1s) for a host whose name contains "down",
# answers late for "slow", and otherwise runs the command LOCALLY, so a remote-*
# target really rsyncs into a temp "remote" dir and runs the Mac-side make there.
# The fake go makes `go vet` log its start and end and sleep, so the lock is
# observed, not inferred. Nothing touches a real Mac.
#
# Checks: the first reachable host in REMOTE_HOSTS order wins (a dead first host
# falls through, a faster later one does not jump the queue, a dead later one
# costs no wait); none reachable → local, said out loud; REMOTE_HOSTS= and
# REMOTE_PICK= overrides; non-dispatch goals never probe; the dispatch line names
# the host; the Mac-side make gets the host's lock file (shared on a locked host,
# per worktree plus FGW_RUN_TAG on an unlocked one); and two simultaneous
# `make vet` from two checkouts serialise on a locked host and overlap on an
# unlocked one.
#
# Usage: remote-hosts-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,24p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

SRC=$(cd "$(dirname "$0")/../.." && pwd -P)
TMP=$(mktemp -d /tmp/remote-hosts-test.XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0
ok()   { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; [ -n "${2:-}" ] && printf '       %s\n' "$2"; RC=1; }
expect() { if grep -qF -- "$2" <<<"$3"; then ok "$1"; else fail "$1" "missing: $2"; printf '%s\n' "$3" | sed 's/^/       | /'; fi; }
reject() { if grep -qF -- "$2" <<<"$3"; then fail "$1" "unexpected: $2"; else ok "$1"; fi; }
user=$(id -un)

# Two checkouts (repoA, repoB), files only: `cp -r` would carry a worktree's
# setgid dir mode, which /tmp refuses.
for r in repoA repoB; do
  mkdir -p "$TMP/$r/mk" "$TMP/$r/scripts"
  cp "$SRC/Makefile" "$TMP/$r/" && cp "$SRC"/mk/*.mk "$TMP/$r/mk/" && cp "$SRC/scripts/remote-run.sh" "$TMP/$r/scripts/" \
    || { echo "FAIL could not copy the Makefile into $TMP/$r"; exit 1; }
done
REPO=$TMP/repoA
mkdir -p "$TMP/bin" "$TMP/remote"

cat > "$TMP/bin/ssh" <<'EOF'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case $1 in -o|-l|-p) shift 2 ;; -*) shift ;; *) break ;; esac
done
host=$1; shift
echo "$host" >> "$FAKE_SSH_LOG"
case $host in *down*) sleep 1; echo "ssh: connect to host $host port 22: Operation timed out" >&2; exit 255 ;; esac
case $host in *slow*) sleep 2 ;; esac
# A real ssh carries no make state to the remote.
unset MAKEFLAGS MAKELEVEL MFLAGS MAKEOVERRIDES REMOTE_PICK
cd "$FAKE_REMOTE_HOME" || exit 255
exec bash -c "$*"
EOF
cat > "$TMP/bin/go" <<'EOF'
#!/usr/bin/env bash
case $1 in
  env) echo "$FAKE_GO_DIR" ;;
  vet) echo "$FAKE_GO_ID start $(date +%s%N)" >> "$FAKE_GO_LOG"; sleep 2; echo "$FAKE_GO_ID end $(date +%s%N)" >> "$FAKE_GO_LOG" ;;
  *) echo "fake go: $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$TMP/bin/ssh" "$TMP/bin/go"
export FAKE_SSH_LOG=$TMP/ssh.log FAKE_REMOTE_HOME=$TMP/remote FAKE_GO_DIR=$TMP/go FAKE_GO_LOG=$TMP/vet.log
export PATH=$TMP/bin:$PATH

remote_mk() { # remote_mk <REMOTE_HOSTS>: the same .remote.mk in both checkouts
  for r in repoA repoB; do
    cat > "$TMP/$r/.remote.mk" <<EOF
REMOTE_HOSTS := $1
REMOTE_USER  := tester
REMOTE_DIR   := ~/git/foci
locked.REMOTE_BUILD_LOCK := yes
free.REMOTE_BUILD_LOCK   := no
slowfree.REMOTE_BUILD_LOCK := no
gone.REMOTE_HOST := tester@host-down
EOF
  done
}
# mk [-C dir] <make args...>: make from a clean make environment.
mk() {
  local dir=$REPO
  if [ "$1" = -C ]; then dir=$2; shift 2; fi
  env -u MAKEFLAGS -u MAKELEVEL -u MFLAGS -u REMOTE_PICK -u FGW_HEAVY_LOCK -u FGW_TESTDIR -u FGW_SRCLINK -u HEAVY_LOCK_PATH -u FGW_RUN_TAG \
    make --no-print-directory -C "$dir" "$@" 2>&1
}
DISPATCH='test-one PKG=./internal/zz/'

# --- the choice -----------------------------------------------------------
remote_mk "gone locked free"
# shellcheck disable=SC2086 # DISPATCH holds make arguments
out=$(mk -n $DISPATCH)
expect "dead first host falls through" "running on build host locked: tester@locked, build lock" "$out"
expect "says which host was passed over" "build host(s) gone unreachable; using locked" "$out"
expect "the dead host's <name>.REMOTE_HOST was probed" "tester@host-down" "$(cat "$FAKE_SSH_LOG")"

remote_mk "locked gone"
t0=$(date +%s%N)
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH)
t1=$(date +%s%N)
ms=$(( (t1 - t0) / 1000000 ))
expect "an answering first host is chosen" "running on build host locked:" "$out"
[ "$ms" -lt 900 ] && ok "a dead LATER host costs no wait (${ms}ms)" || fail "a dead LATER host costs no wait" "took ${ms}ms (the fake dead host answers after 1000ms)"

remote_mk "slowfree locked"
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH)
expect "list order wins over probe speed" "running on build host slowfree: tester@slowfree, no build lock" "$out"

remote_mk "gone down2"
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH)
expect "none reachable: runs LOCALLY and says why" "running LOCALLY (no build host reachable (tried: gone down2))" "$out"
out=$(mk -n remote-test-one PKG=./internal/zz/)
expect "a remote-* target errors when none is reachable" "every remote build host is UNREACHABLE (tried: gone down2)" "$out"

remote_mk "free locked"
: > "$FAKE_SSH_LOG"
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH REMOTE_HOSTS=)
expect "REMOTE_HOSTS= forces local" "running LOCALLY (no REMOTE_HOSTS in .remote.mk)" "$out"
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH REMOTE_PICK=locked)
expect "REMOTE_PICK= picks without probing" "running on build host locked:" "$out"
[ -s "$FAKE_SSH_LOG" ] && fail "the overrides probe nothing" "$(cat "$FAKE_SSH_LOG")" || ok "the overrides probe nothing"
out=$(mk -n clean)
[ -s "$FAKE_SSH_LOG" ] && fail "a non-dispatch goal probes nothing" "$(cat "$FAKE_SSH_LOG")" || ok "a non-dispatch goal probes nothing"
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH REMOTE_PICK=nosuch)
expect "REMOTE_PICK= must name a listed host" "REMOTE_PICK=nosuch is not one of REMOTE_HOSTS (free locked)" "$out"

# --- what the Mac-side make is told ---------------------------------------
out=$(mk -n remote-test-one PKG=./internal/zz/ REMOTE_PICK=locked)
expect "locked host: the shared host lock" "'HEAVY_LOCK_PATH=/tmp/foci-heavy-remote.lock'" "$out"
reject "locked host: the shared test dir" "FGW_RUN_TAG" "$out"
expect "locked host: the per-worktree run dir" "'tester@locked' '~/git/foci-wt/repoA' local-test-one" "$out"
out=$(mk -n remote-test-one PKG=./internal/zz/ REMOTE_PICK=free)
expect "unlocked host: a per-worktree lock" "'HEAVY_LOCK_PATH=../repoA.lock' 'FGW_RUN_TAG=wt-repoA'" "$out"
out=$(mk -n local-test-one PKG=./internal/zz/ HEAVY_LOCK_PATH=../repoA.lock FGW_RUN_TAG=wt-repoA)
expect "the Mac-side make flocks HEAVY_LOCK_PATH" ") 9<../repoA.lock" "$out"
expect "the Mac-side make keys its test dir on FGW_RUN_TAG" "/run-$user-wt-repoA/home" "$out"
expect "the Mac-side make keys its source link on FGW_RUN_TAG" "/src-$user-wt-repoA" "$out"
out=$(mk -n local-test-one PKG=./internal/zz/)
expect "unchanged here: /tmp/heavy" ") 9</tmp/heavy" "$out"
expect "unchanged here: the shared per-user test dir" "/run-$user/home" "$out"

# --- two simultaneous runs, end to end ------------------------------------
overlap() {
  awk '{ t[$1" "$2] = $3 } END {
    if (!(("A start") in t) || !(("B end") in t)) { print "incomplete"; exit }
    print ((t["A start"] < t["B end"] && t["B start"] < t["A end"]) ? "yes" : "no") }' "$1"
}
both() { # both <make args...>: `make vet` from repoA and repoB at once
  : > "$FAKE_GO_LOG"
  FAKE_GO_ID=A mk -C "$TMP/repoA" vet FGW_ROOT="$TMP/fgw" REMOTE_HEAVY_LOCK="$TMP/heavy.lock" "$@" > "$TMP/A.out" &
  local a=$!
  sleep 0.3
  FAKE_GO_ID=B mk -C "$TMP/repoB" vet FGW_ROOT="$TMP/fgw" REMOTE_HEAVY_LOCK="$TMP/heavy.lock" "$@" > "$TMP/B.out" &
  local b=$!
  wait $a; wait $b
  overlap "$FAKE_GO_LOG"
}
remote_mk "locked free"
r=$(both REMOTE_HOSTS=locked)
[ "$r" = no ] && ok "locked host: two runs serialise" || { fail "locked host: two runs serialise" "overlap=$r"; cat "$FAKE_GO_LOG" "$TMP/A.out" "$TMP/B.out"; }
expect "locked host: the run says where it ran" "vet ran on locked: tester@locked, build lock, exit 0" "$(cat "$TMP/A.out")"
r=$(both REMOTE_HOSTS=free)
[ "$r" = yes ] && ok "unlocked host: two runs overlap" || { fail "unlocked host: two runs overlap" "overlap=$r"; cat "$FAKE_GO_LOG" "$TMP/A.out" "$TMP/B.out"; }
[ -f "$TMP/remote/git/foci-wt/repoA/Makefile" ] && [ -f "$TMP/remote/git/foci-wt/repoB/Makefile" ] && ok "each checkout ran in its own remote dir" || fail "each checkout ran in its own remote dir" "$(ls -R "$TMP/remote" | head)"

# --- the old format -------------------------------------------------------
printf 'REMOTE_HOST := mac\nREMOTE_DIR := ~/git/foci\n' > "$REPO/.remote.mk"
# shellcheck disable=SC2086
out=$(mk -n $DISPATCH)
expect "the single-host format warns" "the single-host format is no longer read" "$out"
expect "the single-host format runs local" "running LOCALLY" "$out"

exit $RC
