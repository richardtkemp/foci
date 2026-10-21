#!/usr/bin/env bash
#
# check-golangci-go-test.sh — assertions for scripts/check-golangci-go.sh (#2502):
# older linter Go -> exit 1 naming both versions; same minor (any patch) or newer
# linter -> exit 0 silently; unreadable versions -> exit 0 with a skip note.
# Stubs `go` first on PATH: `go version` prints $TMP/pathgo, `go version BIN`
# prints "BIN: <contents of $TMP/lintgo>".
#
# Usage: check-golangci-go-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,10p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac
HERE=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d /tmp/check-golangci-go-test-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0
mkdir -p "$TMP/bin"
cat > "$TMP/bin/go" <<EOF2
#!/usr/bin/env bash
if [ "\$#" -eq 1 ]; then echo "go version \$(cat $TMP/pathgo) linux/amd64"
else echo "\$2: \$(cat $TMP/lintgo)"; fi
EOF2
chmod +x "$TMP/bin/go"

run() { # pathgo lintgo -> sets OUT, CODE
	echo "$1" > "$TMP/pathgo"; echo "$2" > "$TMP/lintgo"
	OUT=$(PATH="$TMP/bin:$PATH" bash "$HERE/check-golangci-go.sh" /x/golangci-lint 2>&1); CODE=$?
}
expect() { # name wantcode [wantsubstr]
	if [ "$CODE" -ne "$2" ]; then echo "FAIL $1: exit $CODE, want $2: $OUT"; RC=1; return; fi
	if [ -n "${3:-}" ] && [[ "$OUT" != *"$3"* ]]; then echo "FAIL $1: output lacks '$3': $OUT"; RC=1; return; fi
	echo "ok   $1"
}
run go1.27.3 go1.26.0;  expect "older linter fails" 1 "go1.26 but the go on PATH is go1.27"
[[ "$OUT" == *"high-memory mismatch"* ]] || { echo "FAIL message lacks 'high-memory mismatch'"; RC=1; }
[[ "$OUT" == *"first on PATH"* ]] || { echo "FAIL message lacks fix hint"; RC=1; }
run go1.26.10 go1.26.0; expect "same minor, older patch passes" 0
run go1.26.0 go1.27.1;  expect "newer linter passes" 0
run go1.27.3 go1.27rc1; expect "rc of same minor passes" 0
run go1.27.3 garbage;   expect "unreadable skips" 0 "skipping"
exit $RC
