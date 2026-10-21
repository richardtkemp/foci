#!/usr/bin/env bash
# check-golangci-go.sh — fail fast when golangci-lint was built with an older Go
# than the `go` on PATH (foci_todo #2502).
#
# Usage: check-golangci-go.sh [-h|--help] <golangci-lint-binary>
#
# Why: golangci-lint only supports Go versions up to the one it was built with.
# A linter built with Go 1.26 run on a Go 1.27 tree grew past 12 GB and was
# killed by memory_guard (#2501), leaving a failed pre-commit with no clue why.
# Only major.minor is compared (1.26.0 linter with 1.26.10 go is fine: proven).
# The build Go is read with `go version <binary>`, which inspects the binary
# without running it.
#
# Exit: 0 linter Go >= PATH Go (or versions could not be read: not our call);
#       1 mismatch (message on stderr).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0;; esac
BIN=${1:?usage: check-golangci-go.sh <golangci-lint-binary>}

# Print "major minor" of a go version string such as go1.26.10 or go1.27rc1.
mm() { sed -nE 's/.*go([0-9]+)\.([0-9]+).*/\1 \2/p' <<<"$1" | head -n1; }

path_go=$(go version 2>/dev/null)
lint_go=$(go version "$BIN" 2>/dev/null)
path_mm=$(mm "${path_go#go version }")
lint_mm=$(mm "${lint_go#"$BIN": }")
if [ -z "$path_mm" ] || [ -z "$lint_mm" ]; then
	echo "check-golangci-go: could not read Go versions (go on PATH: '$path_go'; linter: '$lint_go'); skipping check" >&2
	exit 0
fi
read -r pmaj pmin <<<"$path_mm"
read -r lmaj lmin <<<"$lint_mm"
if [ "$lmaj" -lt "$pmaj" ] || { [ "$lmaj" -eq "$pmaj" ] && [ "$lmin" -lt "$pmin" ]; }; then
	cat >&2 <<MSG
ERROR: $BIN was built with go$lmaj.$lmin but the go on PATH is go$pmaj.$pmin.
golangci-lint built with an older Go than the one analysing the tree is the known
high-memory mismatch (>12 GB, killed by memory_guard; foci_todo #2501/#2502).
Fix: install a golangci-lint built with go$pmaj.$pmin or newer, or put the older Go
first on PATH, e.g. PATH=/usr/lib/go-$lmaj.$lmin/bin:\$PATH GOROOT=/usr/lib/go-$lmaj.$lmin
MSG
	exit 1
fi
