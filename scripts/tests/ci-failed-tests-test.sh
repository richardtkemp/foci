#!/usr/bin/env bash
#
# ci-failed-tests-test.sh — assertions for scripts/ci-failed-tests.sh (#1754).
#
# The extractor feeds column 6 of the shared CI history CSV. A wrong value there
# is worse than an empty one: it names a test that did not fail, and the whole
# point of the column is to answer "did THIS test fail before?" truthfully.
#
# Usage: ci-failed-tests-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,10p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

HERE=$(cd "$(dirname "$0")/.." && pwd)
EXTRACT="$HERE/ci-failed-tests.sh"
TMP=$(mktemp -d /tmp/ci-failed-tests-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0

check() { # check <label> <expected> <actual>
  if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"; RC=1; fi
}

run() { bash "$EXTRACT" "$@"; }

printf 'ok  \t0.01s\nPASS\nok \tfoci/internal/config\t0.10s\n' > "$TMP/pass.log"
check "green log yields empty field" "" "$(run go "$TMP/pass.log")"

cat > "$TMP/fail.log" <<'LOG'
--- FAIL: TestAlpha (0.01s)
    --- FAIL: TestAlpha/sub_case (0.00s)
--- FAIL: TestBeta (0.02s)
FAIL
LOG
check "names top-level and subtests, in order" \
  "TestAlpha|TestAlpha/sub_case|TestBeta" "$(run go "$TMP/fail.log")"

printf -- '--- FAIL: TestDup (0.01s)\n--- FAIL: TestDup (0.01s)\n' > "$TMP/dup.log"
check "duplicate names collapse" "TestDup" "$(run go "$TMP/dup.log")"

case "$(run go "$TMP/fail.log")" in
  *,*) echo "FAIL field contains a comma — would corrupt the CSV"; RC=1;;
  *)   echo "ok   field contains no comma";;
esac

: > "$TMP/many.log"
for i in $(seq 1 30); do printf -- '--- FAIL: Test%02d (0.00s)\n' "$i" >> "$TMP/many.log"; done
many=$(MAX_FAILED_TESTS=25 run go "$TMP/many.log")
case "$many" in
  *"|+5-more") echo "ok   over-long list is truncated with a remainder marker";;
  *) echo "FAIL truncation marker missing; got: $many"; RC=1;;
esac
# printf '%s' leaves the final field unterminated, which wc -l does not count;
# '%s\n' is what makes the count the number of FIELDS.
check "truncated list holds exactly MAX names plus the marker" \
  "26" "$(printf '%s\n' "$many" | tr '|' '\n' | wc -l)"

check "missing file yields empty, not an error" "" "$(run go "$TMP/nope.log")"
check "missing file exits 0" "0" "$(run go "$TMP/nope.log" >/dev/null; echo $?)"
check "unknown kind yields empty" "" "$(run junit-xml "$TMP/fail.log")"
check "unknown kind exits 0" "0" "$(run junit-xml "$TMP/fail.log" >/dev/null; echo $?)"
check "no args exits 0" "0" "$(run >/dev/null 2>&1; echo $?)"

printf 'FAIL\tfoci/internal/config\t0.10s\nFAIL\n' > "$TMP/summary.log"
check "suite-level FAIL lines are not mistaken for test names" "" "$(run go "$TMP/summary.log")"

exit $RC
