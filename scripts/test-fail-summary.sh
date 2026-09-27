#!/usr/bin/env bash
#
# test-fail-summary.sh — print the "--- failures ---" summary for a failed
# `go test` log: the FAIL lines, panics, and under each "--- FAIL: <test>"
# the assertion-message lines that test logged ("    foo_test.go:218: ...").
#
# WHY (foci_todo #2063): the summary used to be a bare
# grep '^(--- FAIL:|FAIL)|panic:', i.e. test NAMES only. Confirming a fail-arm
# failed for the RIGHT reason then took a second trip into the full log.
#
# Handles both output shapes `go test` produces:
#   - default: a test's log lines follow its "--- FAIL: X" line;
#   - -v (integration, test-one V=1): log lines stream BEFORE the result
#     line, under "=== RUN|CONT|NAME X" headers — and passing tests' t.Logf
#     lines are there too, so lines are attributed to the test that printed
#     them and only FAILING tests' lines are shown.
# Only the first line of each message (the "<file>.go:<line>:" line) is kept;
# continuation lines stay in the full log. A test's t.Logf lines are the same
# shape as its t.Errorf lines and are kept too — they are usually the context
# the failure needs.
#
# Usage: test-fail-summary.sh <logfile>
# Env:   MAX_REASON_LINES (default 40) — total cap on message lines printed.
#        MAX_REASON_WIDTH (default 300) — per-line character cap.
# Exit:  always 0 — this is reporting; the caller owns the test status.
set -uo pipefail

case "${1:-}" in -h|--help) sed -n '2,25p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

LOG=${1:-}
[ -n "$LOG" ] && [ -r "$LOG" ] || exit 0

awk -v max="${MAX_REASON_LINES:-40}" -v width="${MAX_REASON_WIDTH:-300}" '
  # -v header: subsequent log lines belong to this test.
  match($0, /^=== (RUN|CONT|NAME) +/) {
    cur = substr($0, RLENGTH + 1); sub(/[[:space:]].*$/, "", cur); next
  }
  # Result line (top-level or indented subtest). In default mode the test
  # log lines follow it, so it also becomes the current test.
  match($0, /^[[:space:]]*--- (FAIL|PASS|SKIP): /) {
    cur = substr($0, RLENGTH + 1); sub(/[[:space:]].*$/, "", cur)
    if ($0 ~ /^[[:space:]]*--- FAIL: /) { out[++n] = $0; fname[n] = cur }
    next
  }
  # A message line. Deduped per test, ignoring indentation: seal-test.sh
  # re-runs a sealed failure unsealed into the SAME log (once in -v shape,
  # once in default shape), so every message would otherwise print twice.
  /^[[:space:]]+[^[:space:]]+\.go:[0-9]+: / {
    key = $0; sub(/^[[:space:]]+/, "", key)
    if (cur != "" && !seen[cur, key]++) reason[cur, ++nr[cur]] = $0
    next
  }
  /^FAIL|panic:/ { out[++n] = $0 }
  END {
    shown = 0; dropped = 0
    for (i = 1; i <= n; i++) {
      print out[i]
      t = fname[i]
      if (t == "" || done[t]++) continue
      for (j = 1; j <= nr[t]; j++) {
        if (shown >= max) { dropped++; continue }
        line = reason[t, j]
        if (length(line) > width) line = substr(line, 1, width) " ..."
        print line; shown++
      }
    }
    if (dropped) printf "    ... %d more message line(s) - see the full log\n", dropped
  }
' "$LOG"
exit 0
