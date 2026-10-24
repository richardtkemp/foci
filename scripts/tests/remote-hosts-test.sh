#!/usr/bin/env bash
# shellcheck disable=SC2317 # the g_* scenario functions are called through run_group "$g"
#
# remote-hosts-test.sh — tests for the several-build-hosts .remote.mk (foci_todo
# #2526): the host choice in mk/remote.mk and the optional per-host build lock.
#
# Hermetic: copies Makefile, mk/ and scripts/remote-run.sh into temp dirs, each
# with its own .remote.mk, and puts a fake `ssh` and a fake `go` first on PATH.
# The fake ssh fails (exit 255, after 1s; 10s for "verydown") for a host whose name contains "down",
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
# Most of the time is the fake hosts' sleeps, so the scenarios run AT ONCE, each
# in its own group dir (its own two checkouts, .remote.mk and fake-host logs);
# their output is printed afterwards in this file's order (foci_todo #2551).
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

mkdir -p "$TMP/bin"

cat > "$TMP/bin/ssh" <<'EOF'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case $1 in -o|-l|-p) shift 2 ;; -*) shift ;; *) break ;; esac
done
host=$1; shift
echo "$host" >> "$FAKE_SSH_LOG"
case $host in *verydown*) sleep 10; echo "ssh: connect to host $host port 22: Operation timed out" >&2; exit 255 ;; esac
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
export PATH=$TMP/bin:$PATH
# On macOS the Makefile puts $(BREW_PREFIX)/bin AHEAD of PATH (its GNU tools),
# and Homebrew's go lives there, so the real go shadowed the fake one and the
# "remote" make vetted an empty dir (#2544). A stand-in prefix: Homebrew's own
# opt/ and sbin/, and a bin/ of links to its tools, with go swapped for the fake.
if [ "$(uname -s)" = Darwin ]; then
  brew=$(brew --prefix 2>/dev/null || echo /opt/homebrew)
  mkdir -p "$TMP/brew/bin"
  ln -s "$brew/opt" "$TMP/brew/opt" && ln -s "$brew/sbin" "$TMP/brew/sbin"
  for f in "$brew"/bin/*; do ln -s "$f" "$TMP/brew/bin/"; done
  ln -sf "$TMP/bin/go" "$TMP/brew/bin/go"
  export BREW_PREFIX=$TMP/brew
fi

# new_group <name>: a group's own two checkouts (files only: `cp -r` would carry
# a worktree's setgid dir mode, which /tmp refuses) and fake-host state. Called
# inside the group's subshell, so REPO, G and the FAKE_* settings stay there.
new_group() {
  G=$TMP/$1
  local r
  for r in repoA repoB; do
    mkdir -p "$G/$r/mk" "$G/$r/scripts"
    cp "$SRC/Makefile" "$G/$r/" && cp "$SRC"/mk/*.mk "$G/$r/mk/" && cp "$SRC/scripts/remote-run.sh" "$G/$r/scripts/" \
      || { echo "FAIL could not copy the Makefile into $G/$r"; exit 1; }
  done
  REPO=$G/repoA
  mkdir -p "$G/remote"
  export FAKE_SSH_LOG=$G/ssh.log FAKE_REMOTE_HOME=$G/remote FAKE_GO_DIR=$G/go FAKE_GO_LOG=$G/vet.log
  : > "$FAKE_SSH_LOG"
}
remote_mk() { # remote_mk <REMOTE_HOSTS>: the same .remote.mk in both checkouts
  for r in repoA repoB; do
    cat > "$G/$r/.remote.mk" <<EOF
REMOTE_HOSTS := $1
REMOTE_USER  := tester
REMOTE_DIR   := ~/git/foci
locked.REMOTE_BUILD_LOCK := yes
free.REMOTE_BUILD_LOCK   := no
slowfree.REMOTE_BUILD_LOCK := no
gone.REMOTE_HOST := tester@host-down
deadslow.REMOTE_HOST := tester@host-verydown
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
g_dead_first() {
  new_group dead_first; remote_mk "gone locked free"
  # shellcheck disable=SC2086 # DISPATCH holds make arguments
  out=$(mk -n $DISPATCH)
  expect "dead first host falls through" "running on build host locked: tester@locked, build lock" "$out"
  expect "says which host was passed over" "build host(s) gone unreachable; using locked" "$out"
  expect "the dead host's <name>.REMOTE_HOST was probed" "tester@host-down" "$(cat "$FAKE_SSH_LOG")"
}
g_dead_later() {
  # The dead later host takes 10 s to fail: a probe that waited for it would
  # take that long, far above any make's parse time on a busy host.
  new_group dead_later; remote_mk "locked deadslow"
  t0=$(date +%s%N)
  # shellcheck disable=SC2086
  out=$(mk -n $DISPATCH)
  t1=$(date +%s%N)
  ms=$(( (t1 - t0) / 1000000 ))
  expect "an answering first host is chosen" "running on build host locked:" "$out"
  [ "$ms" -lt 5000 ] && ok "a dead LATER host costs no wait (${ms}ms)" || fail "a dead LATER host costs no wait" "took ${ms}ms (the fake dead host answers after 10000ms)"
}
g_order() {
  new_group order; remote_mk "slowfree locked"
  # shellcheck disable=SC2086
  out=$(mk -n $DISPATCH)
  expect "list order wins over probe speed" "running on build host slowfree: tester@slowfree, no build lock" "$out"
}
g_none() {
  new_group none; remote_mk "gone down2"
  # shellcheck disable=SC2086
  out=$(mk -n $DISPATCH)
  expect "none reachable: runs LOCALLY and says why" "running LOCALLY (no build host reachable (tried: gone down2))" "$out"
  out=$(mk -n remote-test-one PKG=./internal/zz/)
  expect "a remote-* target errors when none is reachable" "every remote build host is UNREACHABLE (tried: gone down2)" "$out"
}
g_overrides() {
  new_group overrides; remote_mk "free locked"
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
}

# --- what the Mac-side make is told ---------------------------------------
g_told() {
  new_group told; remote_mk "free locked"
  out=$(mk -n remote-test-one PKG=./internal/zz/ REMOTE_PICK=locked)
  expect "locked host: the shared host lock" "'HEAVY_LOCK_PATH=/tmp/foci-heavy-remote.lock'" "$out"
  reject "locked host: the shared test dir" "FGW_RUN_TAG" "$out"
  expect "locked host: the per-worktree run dir" "'tester@locked' '~/git/foci-wt/repoA' local-test-one" "$out"
  out=$(mk -n remote-test-one PKG=./internal/zz/ REMOTE_PICK=free)
  expect "unlocked host: a per-worktree lock" "'HEAVY_LOCK_PATH=../repoA.lock' 'FGW_RUN_TAG=" "$out"
  # The tag is a short hash, not the worktree's name: the Mac's test dir is every
  # test's TMPDIR, and macOS caps a unix socket path at 103 bytes (#2544).
  tagA=$(grep -o "'FGW_RUN_TAG=[^']*'" <<<"$out" | head -1 | sed "s/'FGW_RUN_TAG=//; s/'//")
  if [[ "$tagA" =~ ^[0-9a-f]{8}$ ]]; then ok "unlocked host: the run tag is 8 hex digits ($tagA)"
  else fail "unlocked host: the run tag is 8 hex digits" "got: $tagA"; fi
  out=$(mk -C "$G/repoB" -n remote-test-one PKG=./internal/zz/ REMOTE_PICK=free)
  tagB=$(grep -o "'FGW_RUN_TAG=[^']*'" <<<"$out" | head -1 | sed "s/'FGW_RUN_TAG=//; s/'//")
  if [ -n "$tagB" ] && [ "$tagA" != "$tagB" ]; then ok "unlocked host: each worktree its own tag"
  else fail "unlocked host: each worktree its own tag" "repoA=$tagA repoB=$tagB"; fi
  out=$(mk -n local-test-one PKG=./internal/zz/ HEAVY_LOCK_PATH=../repoA.lock FGW_RUN_TAG=0badcafe)
  expect "the Mac-side make flocks HEAVY_LOCK_PATH" ") 9<../repoA.lock" "$out"
  expect "the Mac-side make keys its test dir on FGW_RUN_TAG" "run-0badcafe/home" "$out"
  expect "the Mac-side make keys its source link on FGW_RUN_TAG" "/src-0badcafe" "$out"
  out=$(mk -n local-test-one PKG=./internal/zz/)
  expect "unchanged here: /tmp/heavy" ") 9</tmp/heavy" "$out"
  expect "unchanged here: the shared per-user test dir" "run-$user/home" "$out"
}

# --- two simultaneous runs, end to end ------------------------------------
overlap() {
  awk '{ t[$1" "$2] = $3 } END {
    if (!(("A start") in t) || !(("B end") in t)) { print "incomplete"; exit }
    print ((t["A start"] < t["B end"] && t["B start"] < t["A end"]) ? "yes" : "no") }' "$1"
}
both() { # both <make args...>: `make vet` from repoA and repoB at once
  : > "$FAKE_GO_LOG"
  FAKE_GO_ID=A mk -C "$G/repoA" vet FGW_ROOT="$G/fgw" REMOTE_HEAVY_LOCK="$G/heavy.lock" "$@" > "$G/A.out" &
  local a=$!
  sleep 0.3
  FAKE_GO_ID=B mk -C "$G/repoB" vet FGW_ROOT="$G/fgw" REMOTE_HEAVY_LOCK="$G/heavy.lock" "$@" > "$G/B.out" &
  local b=$!
  wait $a; wait $b
  overlap "$FAKE_GO_LOG"
}
g_both_locked() {
  new_group both_locked; remote_mk "locked free"
  r=$(both REMOTE_HOSTS=locked)
  [ "$r" = no ] && ok "locked host: two runs serialise" || { fail "locked host: two runs serialise" "overlap=$r"; cat "$FAKE_GO_LOG" "$G/A.out" "$G/B.out"; }
  expect "locked host: the run says where it ran" "vet ran on locked: tester@locked, build lock, exit 0" "$(cat "$G/A.out")"
}
g_both_free() {
  new_group both_free; remote_mk "locked free"
  r=$(both REMOTE_HOSTS=free)
  [ "$r" = yes ] && ok "unlocked host: two runs overlap" || { fail "unlocked host: two runs overlap" "overlap=$r"; cat "$FAKE_GO_LOG" "$G/A.out" "$G/B.out"; }
  [ -f "$G/remote/git/foci-wt/repoA/Makefile" ] && [ -f "$G/remote/git/foci-wt/repoB/Makefile" ] && ok "each checkout ran in its own remote dir" || fail "each checkout ran in its own remote dir" "$(ls -R "$G/remote" | head)"
}

g_rest() {
  new_group rest; remote_mk "locked free"
  # --- every local body a remote-* target runs exists -----------------------
  # remote-run.sh runs `make local-<target>` on the Mac; a missing one fails there
  # with "No rule to make target" (remote-test did, until local-test was added).
  for t in test test-one vet; do
    out=$(make -n -C "$REPO" --no-print-directory "local-$t" PKG=./internal/zz/ 2>&1)
    reject "local-$t exists (remote-$t runs it on the Mac)" "No rule to make target" "$out"
  done
  # local-lint by its rule, not `make -n`: its recipe line runs $(MAKE), which -n
  # still EXECUTES, and that line takes /tmp/heavy, which the `make test` running
  # this script holds (a deadlock, seen on #2544).
  if grep -q '^local-lint:' "$REPO/Makefile"; then ok "local-lint exists (remote-lint runs it on the Mac)"
  else fail "local-lint exists (remote-lint runs it on the Mac)" "no local-lint: rule in the Makefile"; fi

  # --- NPROC (go test -p): at most 8 on macOS, a command-line value wins (#2546) ---
  printf 'print-nproc:\n\t@echo $(NPROC)\n' > "$G/np.mk"
  nproc_of() { make -C "$REPO" --no-print-directory -s -f Makefile -f "$G/np.mk" print-nproc "$@" 2>&1; }
  if [ "$(uname -s)" = Darwin ]; then
    ncpu=$(sysctl -n hw.ncpu); want=$(( ncpu > 8 ? 8 : ncpu ))
    got=$(nproc_of)
    [ "$got" = "$want" ] && ok "macOS NPROC is min(hw.ncpu, 8) = $want" || fail "macOS NPROC is min(hw.ncpu, 8)" "want $want got '$got'"
  fi
  got=$(nproc_of NPROC=3)
  [ "$got" = 3 ] && ok "command-line NPROC=3 wins" || fail "command-line NPROC=3 wins" "got '$got'"

  # --- make land's gate and lint dispatch too (Dick 2026-10-24, #2544) -------
  for t in test lint; do
    out=$(mk -n "$t" REMOTE_PICK=locked)
    expect "$t dispatches to the build host" ">>> $t: running on build host locked:" "$out"
  done

  # A recipe of a non-dispatch goal that runs `make test` (land.sh, under `make
  # land`) must leave that make free to probe: an exported empty REMOTE_PICK
  # made it run locally, so make land's gate never reached the Mac (#2544).
  # (An extra -f, not --eval: Apple's make 3.81 has no --eval.)
  # shellcheck disable=SC2016 # a make recipe: $$ is make's, for the shell it runs
  printf 'zz-env: ; @echo "pick-exported=[$${REMOTE_PICK+yes}]"\n' > "$G/zz-env.mk"
  out=$(mk -f Makefile -f "$G/zz-env.mk" zz-env)
  expect "a non-dispatch goal does not export an empty REMOTE_PICK" "pick-exported=[]" "$out"

  # --- the old format -------------------------------------------------------
  printf 'REMOTE_HOST := mac\nREMOTE_DIR := ~/git/foci\n' > "$REPO/.remote.mk"
  # shellcheck disable=SC2086
  out=$(mk -n $DISPATCH)
  expect "the single-host format warns" "the single-host format is no longer read" "$out"
  expect "the single-host format runs local" "running LOCALLY" "$out"
}

# Run the groups, all at once. Each group prints into its own file and exits with its RC; a
# group that printed no assertion at all (it did not run) fails too.
SCENARIOS=(g_dead_first g_dead_later g_order g_none g_overrides g_told g_both_locked g_both_free g_rest)
run_group() { ( RC=0; "$1"; exit "$RC" ) > "$TMP/$1.out" 2>&1; echo $? > "$TMP/$1.rc"; }
for g in "${SCENARIOS[@]}"; do
  run_group "$g" &
done
wait
for g in "${SCENARIOS[@]}"; do
  cat "$TMP/$g.out"
  [ "$(cat "$TMP/$g.rc")" = 0 ] || RC=1
  grep -qE '^(ok|FAIL) ' "$TMP/$g.out" || { echo "FAIL scenario $g printed no assertion"; RC=1; }
done

exit $RC
