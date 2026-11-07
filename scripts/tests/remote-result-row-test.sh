#!/usr/bin/env bash
#
# remote-result-row-test.sh — a `make test` on the build Mac gets its result back
# into the CI_HOOK history, so a runner tests one commit once (foci_todo #2907).
#
# Hermetic: copies Makefile, mk/ and scripts into a temp git repo and puts a fake
# ssh, rsync and go first on PATH. The fake ssh plays the Mac's `make local-test`
# from $SCENARIO. Nothing touches a real Mac.
#
# Checks: a pass and a fail each write exactly one row, with the commit and the
# failing names (from the copied log, or from the output if the copy failed); a
# lost connection or a run with no verdict line writes none; a dirty tree, a -race
# run or an unset hook writes none; with no host reachable the local recipe still
# writes its one row.
#
# Usage: remote-result-row-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,17p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

SRC=$(cd "$(dirname "$0")/../.." && pwd -P)
TMP=$(mktemp -d /tmp/remote-result-row-test.XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0
same() {
  if [ "$2" = "$3" ]; then echo "ok   $1"; return; fi
  echo "FAIL $1"; echo "       want: $2"; echo "       got:  $3"; RC=1
  [ -r "$TMP/last-out" ] && tail -6 "$TMP/last-out" | sed 's/^/       | /'
}

mkdir -p "$TMP/bin" "$TMP/repo" "$TMP/remote"
cat > "$TMP/bin/ssh" <<'FAKE'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do case $1 in -o|-l|-p) shift 2 ;; -*) shift ;; *) break ;; esac; done
host=$1; shift
case $host in *down*) exit 255 ;; esac
# Anything but the Mac's make runs here, so the real rsync works through this ssh.
case "$*" in
  *local-test*) ;;
  *) unset MAKEFLAGS MAKELEVEL MFLAGS MAKEOVERRIDES REMOTE_PICK GOFLAGS; cd "$TMP_ROOT/remote" || exit 255; exec bash -c "$*" ;;
esac
echo "$*" >> "$TMP_ROOT/remote-cmds"
macl=$TMP_ROOT/remote/test-1.log
case $SCENARIO in
  pass)    : > "$macl"; echo "PASS — full log: $macl"; exit 0 ;;
  fail)    printf -- '--- FAIL: TestFromMacLog (0.1s)\n    --- FAIL: TestFromMacLog/sub (0.1s)\n' > "$macl"
           echo "FAILED — full log: $macl"; echo '--- failures ---'; echo '--- FAIL: TestFromSummary (0.1s)'; exit 2 ;;
  nocopy)  echo "FAILED — full log: $TMP_ROOT/remote/gone.log"; echo '--- failures ---'; echo '--- FAIL: TestFromSummary (0.1s)'; exit 2 ;;
  lost)    echo 'running ...'; exit 255 ;;
  noverdict) echo 'make: *** [test-linux-compile] Error 1'; exit 2 ;;
esac
FAKE
cat > "$TMP/bin/go" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
chmod +x "$TMP"/bin/*

R=$TMP/repo
# Files only: `cp -r` would carry the setgid bit of a shared worktree, and mkdir then fails.
mkdir -p "$R/mk" "$R/scripts/llbox"
cp "$SRC/Makefile" "$R/"; cp "$SRC"/mk/*.mk "$R/mk/"
for f in remote-run.sh ci-failed-tests.sh log-first-pass.sh test-fail-summary.sh restore-mtime.py; do cp "$SRC/scripts/$f" "$R/scripts/"; done
# On a Mac the Makefile finds the real go first, so the local recipe's builds need a module.
printf 'module x\n\ngo 1.21\n' > "$R/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$R/scripts/llbox/main.go"
# The local recipe's suite: a stub, so the fallback case runs no real tests.
printf '#!/usr/bin/env bash\necho "$2" > "$TMP_ROOT/seal-testdir"\necho ran > "$3"\nexit 0\n' > "$R/scripts/seal-test.sh"
cat > "$R/.remote.mk" <<'MK'
REMOTE_HOSTS := pro
pro.REMOTE_HOST := me@pro
pro.REMOTE_BUILD_LOCK := no
REMOTE_DIR := git/foci
MK
printf '.remote.mk\n.ci.mk\nbin/\n' > "$R/.gitignore"
git -C "$R" init -q -b main
git -C "$R" add -A
git -C "$R" -c user.name=t -c user.email=t@t commit -q -m base
SHA=$(git -C "$R" rev-parse --short HEAD)
CSV=$TMP/results.csv

# This test runs inside `make test` too: an outer make's state must not reach these.
clean_env() {
  env -u GOFLAGS -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEOVERRIDES -u REMOTE_PICK -u REMOTE_HOSTS \
    -u CI_HOOK -u GIT_COMMIT -u HEAVY_LOCK_HELD -u FGW_HEAVY_LOCK -u FGW_RUN_TAG PATH="$TMP/bin:$PATH" "$@"
}

# Without these the fallback case would rm the REAL shared test dir and repoint the
# real source link: this test runs inside `make test`, and that broke the outer run.
OWN_DIRS=(HEAVY_LOCK_PATH="$TMP/heavy" FGW_ROOT="$TMP/fgw" FGW_TESTDIR="$TMP/testdir" FGW_SRCLINK="$TMP/srclink")

# run <scenario> [make args...]: one `make test`, a fresh history. Prints nothing.
run() {
  local scenario=$1; shift
  rm -f "$CSV"
  clean_env TMP_ROOT="$TMP" SCENARIO="$scenario" \
    make -C "$R" --no-print-directory test CI_HOOK="$CSV" "${OWN_DIRS[@]}" "$@" > "$TMP/out-$scenario.log" 2>&1
  cp "$TMP/out-$scenario.log" "$TMP/last-out"
}
rows()  { if [ -e "$CSV" ]; then wc -l < "$CSV" | tr -d ' '; else echo 0; fi; }
# The row, without its time.
row()   { cut -d, -f2- "$CSV"; }

run pass
same "pass: one row with the commit" "foci,$SHA,unit,pass," "$(row)"
run fail
same "fail: one row, names from the Mac's log" "foci,$SHA,unit,fail,TestFromMacLog|TestFromMacLog/sub" "$(row)"
run nocopy
same "fail, log not copied: names from the output" "foci,$SHA,unit,fail,TestFromSummary" "$(row)"
run lost
same "lost connection: no row" 0 "$(rows)"
run noverdict
same "the suite never started: no row" 0 "$(rows)"

run pass GOFLAGS='-race -count=1'
same "a -race run: no row" 0 "$(rows)"
same "a -race run still reached the host" 1 "$(grep -c 'GOFLAGS=-race' "$TMP/remote-cmds")"

echo stray > "$R/untracked-stray"
run pass
same "an untracked stray does not stop the row" "foci,$SHA,unit,pass," "$(row)"
rm -f "$R/untracked-stray"

echo '# edit' >> "$R/Makefile"
run pass
same "uncommitted edit: no row" 0 "$(rows)"
git -C "$R" checkout -q Makefile

rm -f "$CSV"
clean_env TMP_ROOT="$TMP" SCENARIO=pass \
  make -C "$R" --no-print-directory test CI_HOOK= "${OWN_DIRS[@]}" > "$TMP/out-nohook.log" 2>&1
same "no hook: no row" 0 "$(rows)"

printf "REMOTE_HOSTS := pro\npro.REMOTE_HOST := me@down\nREMOTE_DIR := git/foci\n" > "$R/.remote.mk"
run pass
same "no host reachable: runs locally" 1 "$(grep -c 'running LOCALLY' "$TMP/out-pass.log")"
same "no host reachable: the local recipe writes one row" "foci,$SHA,unit,pass," "$(row)"
same "the local recipe used this test's own dir" "$TMP/testdir" "$(cat "$TMP/seal-testdir")"

exit $RC
