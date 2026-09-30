#!/usr/bin/env bash
#
# seal-test-test.sh — assertions for scripts/seal-test.sh's diagnostic re-run
# (#2132): a sealed failure that passes on re-run both unsealed and sealed is
# a flake, and only one that fails sealed again is blamed on the sandbox.
#
# Stubs `go` (on PATH) and bin/llbox (in a scratch cwd) so no real test runs.
#
# Usage: seal-test-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,10p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

HERE=$(cd "$(dirname "$0")/.." && pwd)
SEALTEST="$HERE/seal-test.sh"
TMP=$(mktemp -d /tmp/seal-test-test-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0

# bin/llbox stub: drop the whitelist args, mark the tree as sealed, run it.
mkdir -p "$TMP/work/bin" "$TMP/stub" "$TMP/testdir/home"
cat > "$TMP/work/bin/llbox" <<'EOF'
#!/usr/bin/env bash
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
SEALED=1 exec "$@"
EOF
# go stub. STUB_MODE picks when it fails:
#   pass    never           flake   only on its first call
#   sandbox whenever sealed real    always
cat > "$TMP/stub/go" <<'EOF'
#!/usr/bin/env bash
n=$(( $(cat "$STUB_CALLS" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$STUB_CALLS"
pkg="${!#}"
fail=0
case "$STUB_MODE" in
flake) [ "$n" -eq 1 ] && fail=1 ;;
sandbox) [ -n "${SEALED:-}" ] && fail=1 ;;
real) fail=1 ;;
esac
if [ "$fail" = 1 ]; then printf 'FAIL\t%s\t0.01s\n' "$pkg"; exit 1; fi
printf 'ok  \t%s\t0.01s\n' "$pkg"
EOF
chmod +x "$TMP/work/bin/llbox" "$TMP/stub/go"

has() { printf '%s\n' "$2" | grep -qF -- "$1"; }
expect() { # expect <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n       missing: %s\n' "$1" "$2"; RC=1; fi
}
reject() { # reject <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'FAIL %s\n       unexpected: %s\n' "$1" "$2"; RC=1
  else printf 'ok   %s\n' "$1"; fi
}

# run <mode>: seal-test.sh `one` on a fake package; prints its output.
run() {
  rm -f "$TMP/calls"
  (cd "$TMP/work" && env -u SEALED -u FOCI_TEST_UNSEALED PATH="$TMP/stub:$PATH" \
    STUB_MODE="$1" STUB_CALLS="$TMP/calls" \
    bash "$SEALTEST" one "$TMP/testdir" "$TMP/log" 1 "$TMP/gocache" "$TMP/gomod" "$TMP/gopath" ./internal/zz2132/ \
    2>&1)
}

out=$(run pass)
reject "pass: no diagnostic" "DIAGNOSTIC" "$out"

out=$(run flake)
expect "flake: reported as a flake" "the failure is a FLAKE, not a sandbox write" "$out"
reject "flake: not blamed on the sandbox" "writing outside the sandbox" "$out"
expect "flake: the log has the sealed re-run" "--- sealed re-run: ./internal/zz2132/" "$(cat "$TMP/log")"

out=$(run sandbox)
expect "sandbox: blamed on the sandbox" "writing outside the sandbox" "$out"
reject "sandbox: not called a flake" "FLAKE" "$out"

out=$(run real)
expect "real: a real failure" "fails both sealed and unsealed" "$out"
reject "real: no diagnostic" "DIAGNOSTIC" "$out"

exit "$RC"
