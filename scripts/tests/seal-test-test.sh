#!/usr/bin/env bash
#
# seal-test-test.sh — assertions for scripts/seal-test.sh:
#  - the diagnostic re-run (#2132): a sealed failure that passes on re-run both
#    unsealed and sealed is a flake, and only one that fails sealed again is
#    blamed on the sandbox;
#  - the fixed test environment (#2348): go test sees env -i plus a pinned
#    PATH, whatever the caller's PATH and env, and every run passes -v.
#
# Stubs `go` (in a fake GOROOT, first on PATH) and bin/llbox (in a scratch
# cwd) so no real test runs. Tests start from env -i, so the stubs read their
# settings from files, not env vars.
#
# Usage: seal-test-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,15p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

HERE=$(cd "$(dirname "$0")/.." && pwd)
SEALTEST="$HERE/seal-test.sh"
TMP=$(mktemp -d /tmp/seal-test-test-XXXXXX)
trap 'rm -rf "$TMP"' EXIT
RC=0

# bin/llbox stub: drop the whitelist args, mark the tree as sealed, run it.
# The command is `env -i <assignments> ...`, so SEALED goes in after the -i.
mkdir -p "$TMP/work/bin" "$TMP/goroot/bin" "$TMP/testdir/home"
cat > "$TMP/work/bin/llbox" <<'EOF'
#!/usr/bin/env bash
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
if [ "$1" = env ] && [ "$2" = -i ]; then shift 2; exec env -i SEALED=1 "$@"; fi
SEALED=1 exec "$@"
EOF
# go stub in a fake GOROOT: `go env GOROOT` names it. $GOROOT/mode picks when
# `go test` fails:
#   pass    never           flake   only on its first call
#   sandbox whenever sealed real    always
# Each `go test` call appends its arguments and environment to $GOROOT/seen.
cat > "$TMP/goroot/bin/go" <<'EOF'
#!/usr/bin/env bash
root=$(cd "$(dirname "$0")/.." && pwd)
if [ "$1" = env ] && [ "$2" = GOROOT ]; then echo "$root"; exit 0; fi
n=$(( $(cat "$root/calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$root/calls"
{ echo "--- call $n: $*"; env | grep -v '^SEALED=' | sort; } >> "$root/seen"
pkg="${!#}"
fail=0
case "$(cat "$root/mode")" in
flake) [ "$n" -eq 1 ] && fail=1 ;;
sandbox) [ -n "${SEALED:-}" ] && fail=1 ;;
real) fail=1 ;;
esac
if [ "$fail" = 1 ]; then printf 'FAIL\t%s\t0.01s\n' "$pkg"; exit 1; fi
printf 'ok  \t%s\t0.01s\n' "$pkg"
EOF
chmod +x "$TMP/work/bin/llbox" "$TMP/goroot/bin/go"

# A here-string, not printf | grep -q: grep -q exits on the first match, the
# printf of a large haystack then dies of SIGPIPE, and pipefail turns the match
# into a miss (which made a reject() pass falsely).
has() { grep -qF -- "$1" <<<"$2"; }
expect() { # expect <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n       missing: %s\n' "$1" "$2"; RC=1; fi
}
reject() { # reject <label> <needle> <haystack>
  if has "$2" "$3"; then printf 'FAIL %s\n       unexpected: %s\n' "$1" "$2"; RC=1
  else printf 'ok   %s\n' "$1"; fi
}

# run <stub mode> [seal-test.sh mode] [caller env assignments...]: seal-test.sh
# (`one` on a fake package by default); prints its output.
run() {
  local stub=$1 mode=${2:-one}
  shift; [ "$#" -gt 0 ] && shift
  rm -f "$TMP/goroot/calls" "$TMP/goroot/seen"
  echo "$stub" > "$TMP/goroot/mode"
  (cd "$TMP/work" && env -u SEALED -u FOCI_TEST_UNSEALED PATH="$TMP/goroot/bin:$PATH" "$@" \
    bash "$SEALTEST" "$mode" "$TMP/testdir" "$TMP/log" 1 "$TMP/gocache" "$TMP/gomod" "$TMP/gopath" ./internal/zz2132/ \
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

# ---- #2348: the environment and flags go test sees -------------------------
# A factory snapshot dir first on PATH, and agent-shell vars, must not reach
# the tests: they would change go's test-cache key for the same code.
SYS="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
run pass one PATH="/srv/snap-aaa/bin:$TMP/goroot/bin:$PATH" CLAUDE_CODE_ZZ=1 FOCI_SOCK=/x LD_PRELOAD= \
  FOCI_OPENCODE_LIVE=1 GOFLAGS=-mod=mod >/dev/null
seenA=$(cat "$TMP/goroot/seen")
expect "env: PATH is pinned" "PATH=$TMP/goroot/bin:$SYS" "$seenA"
reject "env: the caller's PATH entry is dropped" "/srv/snap-aaa" "$seenA"
reject "env: an agent-shell var is dropped" "CLAUDE_CODE_ZZ" "$seenA"
reject "env: FOCI_SOCK is dropped" "FOCI_SOCK" "$seenA"
reject "env: LD_PRELOAD is dropped" "LD_PRELOAD" "$seenA"
expect "env: USER is the account" "USER=$(id -un)" "$seenA"
expect "env: HOME is the sandbox home" "HOME=$TMP/testdir/home" "$seenA"
# The Makefile runs seal-test.sh from a fixed symlink to the checkout (#2238);
# go trusts $PWD for the cwd, so the LOGICAL path must survive env -i.
ln -sfn "$TMP/work" "$TMP/worklink"
(cd "$TMP/worklink" && echo pass > "$TMP/goroot/mode" && rm -f "$TMP/goroot/seen" &&
  PATH="$TMP/goroot/bin:$PATH" bash "$SEALTEST" one "$TMP/testdir" "$TMP/log" 1 "$TMP/gocache" "$TMP/gomod" \
    "$TMP/gopath" ./internal/zz2132/ >/dev/null 2>&1)
expect "env: PWD is the logical (symlink) cwd" "PWD=$TMP/worklink" "$(cat "$TMP/goroot/seen")"
expect "env: an opt-in var passes" "FOCI_OPENCODE_LIVE=1" "$seenA"
expect "env: Go config passes" "GOFLAGS=-mod=mod" "$seenA"
expect "one: -v without V=1" "test -trimpath -ldflags=-w -parallel=16 -v " "$seenA"

# Two callers with different PATHs and shells: the same test env.
run pass one PATH="/srv/snap-bbb/bin:$TMP/goroot/bin:$PATH" OTEL_ZZ=1 FOCI_OPENCODE_LIVE=1 GOFLAGS=-mod=mod >/dev/null
seenB=$(cat "$TMP/goroot/seen")
if [ "$(grep -v '^--- call' <<<"$seenA")" = "$(grep -v '^--- call' <<<"$seenB")" ]; then
  echo "ok   env: identical for two different callers"
else
  echo "FAIL env: differs between two callers"; diff <(echo "$seenA") <(echo "$seenB"); RC=1
fi

# An opt-in value (an API key) reaches the test but never the log.
run pass one ANTHROPIC_API_KEY=sk-zz2348-secret >/dev/null
expect "opt-in: the key reaches the test" "ANTHROPIC_API_KEY=sk-zz2348-secret" "$(cat "$TMP/goroot/seen")"
reject "opt-in: the key is not in the log" "sk-zz2348-secret" "$(cat "$TMP/log")"
expect "opt-in: the log says it is set" "ANTHROPIC_API_KEY=<set>" "$(cat "$TMP/log")"

# make test (unit) and test-one pass the same test flags, so they share go's
# test cache: compare `go test <flags> -timeout <t>` without -p and the package.
run pass unit >/dev/null
unitargs=$(grep -m1 '^--- call' "$TMP/goroot/seen" | sed -E 's/^--- call [0-9]+: //; s/ -p=[0-9]+//; s/ \.\/\.\.\.$//')
run pass one >/dev/null
oneargs=$(grep -m1 '^--- call' "$TMP/goroot/seen" | sed -E 's/^--- call [0-9]+: //; s/ \.\/internal\/zz2132\/$//')
if [ -n "$unitargs" ] && [ "$unitargs" = "$oneargs" ]; then
  echo "ok   flags: unit and one pass the same test flags ($oneargs)"
else
  printf 'FAIL flags: unit [%s] vs one [%s]\n' "$unitargs" "$oneargs"; RC=1
fi

exit "$RC"
