#!/usr/bin/env bash
#
# log-first-pass.sh — print a seal-test.sh log without its diagnostic re-runs.
#
# seal-test.sh re-runs a failed package into the SAME log, to tell a sandbox
# write from a flake. A failure in a re-run is evidence for that question, not a
# failure of the run: every failure of the run is already in the first pass. So
# what reports a run's failures reads this, not the log (foci_todo #2910, where
# a re-run listed 27 tests that never failed in the run).
#
# The section starts at seal-test.sh's "=== diagnostic re-run:" line and ends at
# the first "=== script test:" line (the script suites' output, written after it
# and part of the run) or at the end of the log.
#
# Usage: log-first-pass.sh <logfile>
# Exit:  0; 1 if the log cannot be read.
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,16p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac
awk '/^=== diagnostic re-run:/ { skip = 1 } /^=== script test:/ { skip = 0 } !skip' "${1:?usage: log-first-pass.sh <logfile>}"
