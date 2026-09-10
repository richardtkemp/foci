#!/usr/bin/env bash
#
# mdq-test.sh — assertions for shared/scripts/mdq's --raw/--source mode (#1705),
# plus a regression guard that the pre-existing no-flag behaviour is untouched.
#
# THE BUG THIS FIXES: mdq (the upstream yshavit/mdq binary) re-renders a
# selected section from its AST rather than slicing the source. That
# normalises formatting — source '*word*' comes back as '_word_'. Same
# characters to a reader, different bytes to an exact-match Edit old_string.
# --raw locates the same section mdq would select, then slices the ORIGINAL
# file bytes for that heading's line range instead of letting mdq print it.
#
# THE RED ARM this test demonstrates first (before asserting the fix): a
# fixture section containing '*emphasis*', where mdq's own render of that
# section provably differs from `sed -n` over the same line range — i.e. the
# exact repro named in the todo. Only after that's shown does the test assert
# --raw returns the sed-identical source bytes.
#
# THE OTHER THING THIS TEST GUARDS: shared/scripts/mdq sits on the live
# read path for every agent, right now. A regression in the default
# (no-flag) path breaks markdown reading for everyone the moment this
# script is deployed. So this test also diffs the CURRENT script's no-flag
# output against the PRE-CHANGE script's (fetched from `main` via git show)
# for representative invocations, and requires them to be byte-identical —
# asserted, not eyeballed.
#
# Requires: the real mdq binary (not this wrapper) and jq, both on a
# discoverable path; git (to diff against the pre-change script).
#
# Usage: mdq-test.sh [-h]
# Exit:  0 all assertions passed; 1 otherwise (prints which).
set -uo pipefail
case "${1:-}" in -h|--help) sed -n '2,25p' "$0" | sed 's/^#//; s/^ //'; exit 0;; esac

HERE=$(cd "$(dirname "$0")/.." && pwd)
MDQ="$HERE/mdq"
MDS="$HERE/mds"
RC=0

check() { # check <label> <expected> <actual>
    if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"
    else printf 'FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"; RC=1; fi
}

# --- locate the REAL mdq binary, never this wrapper (or the deployed copy of
# it) — resolving "mdq" via a PATH that contains shared/scripts/ would find
# our own wrapper first and test it against itself.
REAL_MDQ=""
for cand in /usr/local/bin/mdq /usr/bin/mdq "$HOME/.cargo/bin/mdq"; do
    [[ -x "$cand" ]] && { REAL_MDQ="$cand"; break; }
done
if [[ -z "$REAL_MDQ" ]]; then
    echo "SKIP: no real mdq binary found (checked /usr/local/bin, /usr/bin, ~/.cargo/bin)." >&2
    echo "      --raw delegates section-location to it; without it, nothing here can run." >&2
    exit 1
fi
command -v jq >/dev/null 2>&1 || {
    echo "SKIP: jq not found on PATH — --raw requires it to read mdq's JSON output." >&2
    exit 1
}
export MDQ_BIN="$REAL_MDQ"

TMP=$(mktemp -d /tmp/mdq-test-XXXXXX)
trap 'rm -rf "$TMP"' EXIT

FIX="$TMP/fixture.md"
cat > "$FIX" <<'EOF'
# Top

## Section A

Some *emphasis* text here and more.

### Subsection A1

Nested content.

## Section B

Other text.
EOF

# ---------------------------------------------------------------------------
# RED ARM: demonstrate the bug's repro exactly as the todo names it — compare
# mdq's render of a section against `sed -n` over the same line range and
# show the emphasis delimiters differ. The range is derived from the fixture
# with grep -n, not hardcoded, so it can't silently drift from the fixture
# text above: Section A runs from its own heading line up to (not including)
# the next same-or-shallower heading, "## Section B".
# ---------------------------------------------------------------------------
a_start=$(grep -n '^## Section A$' "$FIX" | head -1 | cut -d: -f1)
b_start=$(grep -n '^## Section B$' "$FIX" | head -1 | cut -d: -f1)
total_lines=$(wc -l < "$FIX")
a_end=$(( b_start - 1 ))

rendered=$("$MDQ" '## Section A' "$FIX")
sed_slice=$(sed -n "${a_start},${a_end}p" "$FIX")

case "$rendered" in
    *'_emphasis_'*) echo "ok   red arm: mdq's render normalises *emphasis* to _emphasis_";;
    *) echo "FAIL red arm: expected mdq's render to contain '_emphasis_' (repro didn't reproduce); got: $rendered"; RC=1;;
esac
case "$sed_slice" in
    *'*emphasis*'*) echo "ok   red arm: sed -n over the same lines keeps the source's '*emphasis*'";;
    *) echo "FAIL red arm: expected sed -n slice to contain '*emphasis*'; got: $sed_slice"; RC=1;;
esac
if [[ "$rendered" == "$sed_slice" ]]; then
    echo "FAIL red arm: render and sed slice should differ (that's the bug) but were identical"
    RC=1
else
    echo "ok   red arm: mdq's render and the raw sed slice differ in bytes"
fi

# ---------------------------------------------------------------------------
# GREEN ARM: --raw returns the source bytes exactly — same content as the
# sed -n slice used above, byte for byte.
# ---------------------------------------------------------------------------
raw_out=$("$MDQ" --raw '## Section A' "$FIX")
check "raw arm: --raw output equals sed -n slice byte-for-byte" "$sed_slice" "$raw_out"

case "$raw_out" in
    *'*emphasis*'*) echo "ok   raw arm: --raw output keeps the source's '*emphasis*' delimiters";;
    *) echo "FAIL raw arm: --raw output lost the source's '*emphasis*'; got: $raw_out"; RC=1;;
esac
case "$raw_out" in
    *'_emphasis_'*) echo "FAIL raw arm: --raw output was re-rendered ('_emphasis_' present) — not raw"; RC=1;;
    *) echo "ok   raw arm: --raw output was not re-rendered";;
esac

# --source is documented as an alias for --raw.
alias_out=$("$MDQ" --source '## Section A' "$FIX")
check "--source is an alias for --raw" "$raw_out" "$alias_out"

# Flag position shouldn't matter (it's stripped from argv wherever it is).
raw_out_prefix=$("$MDQ" --raw '## Section A' "$FIX")
raw_out_suffix=$("$MDQ" '## Section A' "$FIX" --raw)
check "--raw works before or after the selector/file" "$raw_out_prefix" "$raw_out_suffix"

# Multi-section selection: each matched section's raw bytes, concatenated in
# document order, no synthetic separator — i.e. exactly the two ranges' bytes
# back to back.
multi_raw=$("$MDQ" --raw '## Section' "$FIX")
multi_expect=$(sed -n "${a_start},${a_end}p;${b_start},${total_lines}p" "$FIX")
check "raw arm: multi-section match concatenates verbatim ranges in doc order" "$multi_expect" "$multi_raw"

# ---------------------------------------------------------------------------
# ERROR ARMS: --raw refuses to guess where it has no clean line range.
# ---------------------------------------------------------------------------
"$MDQ" --raw '## Section A' "$FIX" "$FIX" >/dev/null 2>/tmp/mdq-test-err-multi
check "raw refuses two files (ambiguous line numbers)" "1" "$?"

: > "$TMP/second.md"
if "$MDQ" --raw '## Section A' < "$FIX" >/dev/null 2>/tmp/mdq-test-err-stdin; then
    echo "FAIL raw should refuse stdin-only input (no file to slice)"; RC=1
else
    echo "ok   raw refuses stdin-only input (no file to slice)"
fi

if "$MDQ" --raw '# NoSuchHeadingAtAll' "$FIX" >/dev/null 2>/tmp/mdq-test-err-nomatch; then
    echo "FAIL raw should fail when the selector matches nothing"; RC=1
else
    echo "ok   raw fails cleanly when the selector matches nothing"
fi

# ---------------------------------------------------------------------------
# REGRESSION GUARD: the no-flag path is byte-identical to the pre-change
# script (fetched from `main`), across both of its branches (the "$1"
# starts-with-'#' heading-collapsing branch, and the passthrough branch).
# This is the "must not break the live read path for every agent" gate.
# ---------------------------------------------------------------------------
REPO=$(cd "$HERE/../.." && pwd)
OLD="$TMP/mdq-old"
if git -C "$REPO" show main:shared/scripts/mdq > "$OLD" 2>/tmp/mdq-test-err-gitshow; then
    chmod +x "$OLD"
    run_both() { # run_both <label> <args...>
        local label=$1; shift
        local out_old rc_old out_new rc_new
        out_old=$(MDQ_BIN="$REAL_MDQ" "$OLD" "$@" 2>&1); rc_old=$?
        out_new=$(MDQ_BIN="$REAL_MDQ" "$MDQ" "$@" 2>&1); rc_new=$?
        if [[ "$out_old" == "$out_new" && "$rc_old" == "$rc_new" ]]; then
            echo "ok   no-flag regression: $label (rc=$rc_old)"
        else
            echo "FAIL no-flag regression: $label — output or exit code changed"
            echo "       old (rc=$rc_old): $out_old"
            echo "       new (rc=$rc_new): $out_new"
            RC=1
        fi
    }
    run_both "heading selector, '#'-prefixed branch" '## Section A' "$FIX"
    run_both "heading selector, no match" '# NoSuchHeading' "$FIX"
    run_both "non-heading passthrough branch (raw selector string)" 'p: "Other text"' "$FIX"
    run_both "flags-only passthrough branch" -o json "$FIX"
    run_both "no args at all"
else
    echo "FAIL could not fetch pre-change shared/scripts/mdq from main to diff against"
    RC=1
fi

# ---------------------------------------------------------------------------
# mds parity: --raw forwards through mds's own extraction call to mdq --raw,
# and mds's own no-flag behaviour is likewise untouched.
# ---------------------------------------------------------------------------
export PATH="$HERE:$PATH"   # so mds's unqualified `mdq` call finds this wrapper
mds_raw=$("$MDS" "$FIX" "Section A" --raw)
check "mds --raw matches mdq --raw for the same section" "$raw_out" "$mds_raw"

OLDS="$TMP/mds-old"
if git -C "$REPO" show main:shared/scripts/mds > "$OLDS" 2>/tmp/mds-test-err-gitshow; then
    chmod +x "$OLDS"
    out_old=$(MDQ_BIN="$REAL_MDQ" "$OLDS" "$FIX" "Section A" 2>&1); rc_old=$?
    out_new=$(MDQ_BIN="$REAL_MDQ" "$MDS" "$FIX" "Section A" 2>&1); rc_new=$?
    if [[ "$out_old" == "$out_new" && "$rc_old" == "$rc_new" ]]; then
        echo "ok   mds no-flag regression: pattern match (rc=$rc_old)"
    else
        echo "FAIL mds no-flag regression: pattern match — output or exit code changed"
        RC=1
    fi
else
    echo "FAIL could not fetch pre-change shared/scripts/mds from main to diff against"
    RC=1
fi

if "$MDS" "$FIX" --raw >/dev/null 2>/tmp/mds-test-err-noraw; then
    echo "FAIL mds --raw with no pattern should refuse (no single section to slice)"; RC=1
else
    echo "ok   mds --raw with no pattern refuses cleanly"
fi

exit $RC
