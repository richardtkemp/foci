#!/usr/bin/env bash
#
# test-fail-summary-test.sh — assertions for scripts/test-fail-summary.sh (#2063).
#
# The fixture below is a REAL `make test-one V=1` log (sealed -v run followed by
# seal-test.sh's unsealed default-mode re-run), trimmed of its env header.
#
# Usage: test-fail-summary-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,9p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

HERE=$(cd "$(dirname "$0")/.." && pwd)
SUMMARY="$HERE/test-fail-summary.sh"
TMP=$(mktemp -d /tmp/test-fail-summary-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0

check() { # check <label> <expected> <actual>
  if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"; RC=1; fi
}
has()   { printf '%s\n' "$2" | grep -qF -- "$1"; }
count() { printf '%s\n' "$2" | grep -cF -- "$1"; true; }
expect() { # expect <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n       missing: %s\n' "$1" "$2"; RC=1; fi
}

cat > "$TMP/v.log" <<'LOG'
=== RUN   TestZZPass
    zz_test.go:5: passing test chatter
--- PASS: TestZZPass (0.00s)
=== RUN   TestZZFail
    zz_test.go:8: context line from failing test
    zz_test.go:9: got 1, want 2
        second line of message
--- FAIL: TestZZFail (0.00s)
=== RUN   TestZZSub
=== RUN   TestZZSub/ok
=== PAUSE TestZZSub/ok
=== RUN   TestZZSub/bad
=== PAUSE TestZZSub/bad
=== CONT  TestZZSub/ok
    zz_test.go:16: sub ok log
=== CONT  TestZZSub/bad
    zz_test.go:16: sub bad log
    zz_test.go:18: subcase bad broke
--- FAIL: TestZZSub (0.00s)
    --- PASS: TestZZSub/ok (0.00s)
    --- FAIL: TestZZSub/bad (0.00s)
FAIL
FAIL	foci/internal/zz2063	0.002s
FAIL

=== diagnostic re-run: retrying sealed failures UNSEALED to isolate a sandbox artifact from a real bug ===
--- unsealed re-run: foci/internal/zz2063 ---
--- FAIL: TestZZFail (0.00s)
    zz_test.go:8: context line from failing test
    zz_test.go:9: got 1, want 2
        second line of message
--- FAIL: TestZZSub (0.00s)
    --- FAIL: TestZZSub/bad (0.00s)
        zz_test.go:16: sub bad log
        zz_test.go:18: subcase bad broke
FAIL
FAIL	foci/internal/zz2063	0.005s
FAIL
panic: something exploded [recovered]
FAIL script test: shared/scripts/tests/mdq-test.sh
LOG
out=$(bash "$SUMMARY" "$TMP/v.log")

expect "failing test's assertion message shown" "zz_test.go:9: got 1, want 2" "$out"
expect "failing subtest's message shown (-v, attributed via === CONT)" "subcase bad broke" "$out"
check "passing test's t.Logf excluded" "0" "$(count "passing test chatter" "$out")"
check "passing sibling subtest's t.Logf excluded" "0" "$(count "sub ok log" "$out")"
check "continuation lines excluded" "0" "$(count "second line of message" "$out")"
check "message printed once despite the unsealed re-run" "1" "$(count "got 1, want 2" "$out")"
check "message sits directly under its FAIL line" "    zz_test.go:8: context line from failing test" \
  "$(printf '%s\n' "$out" | grep -A1 -F -- '--- FAIL: TestZZFail' | sed -n 2p)"
check "every FAIL line from the old grep is kept" \
  "$(grep -E '^(--- FAIL:|FAIL)|panic:' "$TMP/v.log")" \
  "$(printf '%s\n' "$out" | grep -E '^(--- FAIL:|FAIL)|panic:')"
expect "indented subtest FAIL line shown" "    --- FAIL: TestZZSub/bad" "$out"

: > "$TMP/many.log"
echo "--- FAIL: TestMany (0.00s)" >> "$TMP/many.log"
for i in $(seq 1 50); do printf '    many_test.go:%d: case %d\n' "$i" "$i" >> "$TMP/many.log"; done
many=$(MAX_REASON_LINES=40 bash "$SUMMARY" "$TMP/many.log")
check "message lines capped" "40" "$(count "many_test.go:" "$many")"
expect "cap reports what it dropped" "10 more message line(s)" "$many"

printf -- '--- FAIL: TestWide (0.00s)\n    wide_test.go:1: %s\n' "$(printf 'x%.0s' $(seq 1 500))" > "$TMP/wide.log"
wide=$(MAX_REASON_WIDTH=100 bash "$SUMMARY" "$TMP/wide.log" | sed -n 2p)
check "long message line truncated" "104" "${#wide}"

check "missing file yields nothing" "" "$(bash "$SUMMARY" "$TMP/nope.log")"
check "missing file exits 0" "0" "$(bash "$SUMMARY" "$TMP/nope.log" >/dev/null; echo $?)"

exit $RC
