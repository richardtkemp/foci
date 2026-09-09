#!/usr/bin/env bash
#
# ci-failed-tests.sh — extract the names of FAILING tests from a test log, for
# column 6 of the shared CI history CSV (/home/rich/git/ci-runner/results.csv).
#
# WHY THIS EXISTS (foci_todo #1754): results.csv used to record only
# <time,repo,commit,target,pass|fail> — suite-level. "When did THIS test last
# pass?" was therefore unanswerable from the durable record and had to be dug
# out of /tmp artefacts, which are aged at 30 days. Without that history the
# honest answer to "is this failure new?" is unavailable, and the tempting
# substitute — "it passed the last N nights, so something changed" — is bad
# statistics: at a 10% flake rate, nine clean nights happens ~39% of the time.
#
# OUTPUT: one line, the failing test names joined by '|', empty if none.
# '|' rather than ',' because the consumer is a CSV. Test names themselves
# never contain either character in Go or JUnit.
#
# Usage: ci-failed-tests.sh go <logfile>
# Exit:  always 0 — a CI history row must never be lost because extraction
#        failed. An unreadable log yields an empty field, not an error.
set -uo pipefail

case "${1:-}" in -h|--help) sed -n '2,21p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

KIND=${1:-}
LOG=${2:-}
MAX=${MAX_FAILED_TESTS:-25}

[ -n "$LOG" ] && [ -r "$LOG" ] || { echo ""; exit 0; }

case "$KIND" in
  go)
    # `go test` prints "--- FAIL: TestName (0.00s)" and, for subtests, an
    # indented "    --- FAIL: TestName/sub (0.00s)". Take both: a subtest name
    # is the useful identifier when only one case of a table test breaks.
    names=$(grep -oE '^[[:space:]]*--- FAIL: [^[:space:]]+' "$LOG" 2>/dev/null \
            | sed -E 's/^[[:space:]]*--- FAIL: //' | awk '!seen[$0]++')
    ;;
  *)
    # An unrecognised kind is a caller bug, not a test result. Emit nothing
    # rather than guess at a format and record a wrong test name.
    echo ""; exit 0
    ;;
esac

[ -n "$names" ] || { echo ""; exit 0; }

total=$(printf '%s\n' "$names" | wc -l)
if [ "$total" -gt "$MAX" ]; then
  printf '%s\n' "$names" | head -n "$MAX" | paste -sd'|' - | sed "s/\$/|+$((total - MAX))-more/"
else
  printf '%s\n' "$names" | paste -sd'|' -
fi
