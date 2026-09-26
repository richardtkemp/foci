#!/usr/bin/env bash
# seal-test.sh — runs the `make test` / `make integration` / `make test-one`
# go-test invocation sealed under a Landlock write-whitelist BY DEFAULT
# (foci_todo #1523, implementing the investigation in #1517). Invoked by the
# Makefile, not run directly by a person.
#
# Usage: seal-test.sh <unit|integration|one> <TESTDIR> <LOGFILE> <parallel-n> \
#          <GOCACHE_PIN> <GOMODCACHE_PIN> <GOPATH_PIN> [PKG] [RUN] [VERBOSE] [COUNT]
#
#   PKG and VERBOSE are only used by the `one` mode — see run_one below. RUN
#   and COUNT also apply to `integration` (e.g. looping one L2 test to repro a
#   flake, foci_todo #2044); PKG is ignored there.
#   VERBOSE (make's V=1) adds go test's own -v so a PASSING test's t.Logf
#   output actually reaches LOGFILE — foci_todo #1982: before this, the only
#   way to see it was to force a FAIL (t.Errorf), because a bare ARGS=-v on
#   the make command line isn't a real make var and was silently dropped.
#
#   foci_todo #1709: this is the ONE place
#   the harness environment (TESTENV/SEAL) is constructed; unit/integration/
#   one all consume the SAME arrays below rather than each deriving their
#   own, so a single-package run cannot silently diverge from `make test`'s
#   environment — the exact failure mode (a hand-rolled `go test ./<pkg>/`
#   dropping FOCI_TMPDIR et al.) this mode exists to make unnecessary.
#
#   TESTDIR must already exist, with a TESTDIR/home subdir (the Makefile
#   creates both — see #1521, which redirects $HOME there so tests can't
#   scribble into the live account's real home).
#   LOGFILE is truncated and then holds the FULL transcript of everything
#   this script ran (sealed pass, diagnostic re-runs) — the Makefile's own
#   PASS/FAILED summary greps it afterwards, same as before this script
#   existed.
#   GOCACHE_PIN/GOMODCACHE_PIN/GOPATH_PIN are the Makefile's own `go env`
#   values, resolved under the REAL $HOME at Makefile-parse time (#1521) —
#   passed in rather than re-resolved here because by the time this script
#   runs, HOME is about to be overridden to TESTDIR/home, and GOCACHE et al
#   default to HOME-relative paths, which would otherwise make every sealed
#   run start from an empty, freshly-whitelisted cache instead of the shared
#   warm one.
#
# ---- What "sealed" means here --------------------------------------------
# The whole `go test` process tree (go test itself, every per-package test
# binary it forks, and anything THEY spawn — tmux, git, chrome, ...) is
# wrapped in one `bin/llbox -w <whitelist> -- ...` invocation. Landlock rules
# are inherited across exec/fork, so one seal at the top covers the entire
# tree. bin/llbox degrades gracefully on its own (old kernel / Landlock LSM
# disabled / non-Linux) — see scripts/llbox — so this script does not need
# its own unsupported-kernel branch; it just always asks to be sealed.
#
# ---- Whitelist (verified empirically, foci_todo #1517) -------------------
#   TESTDIR                  all test-generated state; $HOME lives at
#                            TESTDIR/home (Landlock rules apply to
#                            subdirectories of an already-whitelisted anchor)
#   GOCACHE (GOCACHE_PIN)    go build/test binary cache
#   GOMODCACHE (GOMODCACHE_PIN) go module cache — NOT exercised by #1517 (modules
#                            were already resolved on that run), but a cold
#                            `go mod download` on a fresh clone needs this
#                            writable, so it's whitelisted proactively (Dick's
#                            explicit instruction, #1523) rather than waiting
#                            for it to bite a green-field CI run
#   /dev/null                write-class opens for redirect targets (shell
#                            scripts, `git init -q`, subprocess stdio, ...)
#   /dev/ptmx, /dev/pts      tmux PTY master alloc + dynamically-numbered
#                            slave device nodes
#   /dev/shm                 Chrome's shared-memory IPC segments
# GOCACHE/GOMODCACHE are mkdir -p'd below before being whitelisted: llbox
# opens each whitelist path to get an anchor fd, which fails if the
# directory doesn't exist yet — exactly the fresh-clone case GOMODCACHE is
# here to protect.
#
# ---- The diagnostic re-run --------------------------------------------------
# A sealed failure can surface as a MISLEADING error rather than an obvious
# permission-denied (e.g. a rename between two already-whitelisted
# directories fails with EXDEV — "invalid cross-device link" — if the
# Landlock REFER bit is missing; llbox sets it, but a future regression or a
# genuinely-new whitelist gap could reproduce the same shape of confusion).
# So: whenever a package fails under seal, re-run THAT package unsealed. If
# it then passes, print a loud, unmissable message — the failure was the
# sandbox, not the code — converting the worst failure mode (a misleading
# error that reads like a real bug) into the most informative one. This is
# purely diagnostic: it never changes the run's exit status, because "passes
# unsealed" means the sealed run's failure was real and needs a whitelist fix
# or a code fix, not that the test suite as a whole should be reported green.
set -u

MODE="${1:?usage: seal-test.sh <unit|integration|one> <TESTDIR> <LOGFILE> <parallel-n> <GOCACHE_PIN> <GOMODCACHE_PIN> <GOPATH_PIN> [PKG] [RUN]}"
TESTDIR="${2:?}"
LOGFILE="${3:?}"
PARALLEL="${4:?}"
GOCACHE_DIR="${5:?}"
GOMODCACHE_DIR="${6:?}"
GOPATH_DIR="${7:?}"
PKG="${8:-}"
RUNFILTER="${9:-}"
VERBOSE="${10:-}"
COUNT="${11:-}"

LLBOX="bin/llbox"

: > "$LOGFILE"

# GOMODCACHE in particular may not exist yet on a fresh clone before the
# first `go mod download` — mkdir -p it defensively so llbox's whitelist
# open() (which needs the anchor to already exist) doesn't fail on exactly
# the green-field run this is meant to protect (foci_todo #1523).
mkdir -p "$GOCACHE_DIR" "$GOMODCACHE_DIR"

WHITELIST="$TESTDIR,$GOCACHE_DIR,$GOMODCACHE_DIR,/dev/null,/dev/ptmx,/dev/pts,/dev/shm"

TESTENV=(env "TMPDIR=$TESTDIR" "FOCI_TMPDIR=$TESTDIR" "FOCI_TEST_TMPDIR=$TESTDIR" "HOME=$TESTDIR/home" \
  "GOCACHE=$GOCACHE_DIR" "GOMODCACHE=$GOMODCACHE_DIR" "GOPATH=$GOPATH_DIR")

if [ -n "${FOCI_TEST_UNSEALED:-}" ]; then
  echo ">>> FOCI_TEST_UNSEALED=1 set — skipping Landlock sealing entirely" | tee -a "$LOGFILE" >&2
  SEAL=()
else
  SEAL=("$LLBOX" -w "$WHITELIST" --)
fi

# diagnostic_rerun <extra go-test flags...>
# Scans $LOGFILE (built up so far) for go test's own `FAIL <pkg>` summary
# lines and re-runs each one unsealed to tell a real failure from a sealing
# artifact. Never touches the caller's exit status — purely explanatory.
diagnostic_rerun() {
  local extra_flags=("$@")
  local failed
  failed=$(grep -oP '^FAIL\t\K\S+' "$LOGFILE" 2>/dev/null | sort -u || true)
  [ -z "$failed" ] && return 0

  {
    echo ""
    echo "=== diagnostic re-run: retrying sealed failures UNSEALED to isolate a sandbox artifact from a real bug ==="
  } >> "$LOGFILE"
  local pkg
  while IFS= read -r pkg; do
    [ -z "$pkg" ] && continue
    echo "--- unsealed re-run: $pkg ---" >> "$LOGFILE"
    if "${TESTENV[@]}" nice -n 19 go test -count=1 "${extra_flags[@]}" "$pkg" >> "$LOGFILE" 2>&1; then
      echo ">>> DIAGNOSTIC: $pkg passes UNSEALED — it is writing outside the sandbox. Add the path to the whitelist in scripts/seal-test.sh, or stop writing there." | tee -a "$LOGFILE" >&2
    else
      echo ">>> $pkg fails both sealed and unsealed — a real test failure, not a sealing artifact." | tee -a "$LOGFILE" >&2
    fi
  done <<<"$failed"
}

# env_header <label>
# Logs the label plus the exact TESTENV assignments (skipping TESTENV[0],
# which is just the literal "env" command name) this run is about to use.
# Same array for every mode (see the top-of-file note) — this line is what
# lets a peer (or foci_todo #1709's own verification) DIFF two runs' logs and
# see the environment matched, instead of taking it on faith.
env_header() {
  echo "=== $1 (env: ${TESTENV[*]:1}) ===" >> "$LOGFILE"
}

run_unit() {
  env_header "sealed unit suite"
  "${SEAL[@]}" "${TESTENV[@]}" nice -n 19 go test -p="$PARALLEL" -parallel=16 ./... >> "$LOGFILE" 2>&1
  local status=$?

  diagnostic_rerun

  # Shell-script suites (shared/scripts/tests/*.sh). They ran nowhere before,
  # so mdq-test.sh sat red on main unnoticed (#1976). Run unsealed: they need
  # git and the real mdq/jq binaries. Each prints its own "FAIL ..." lines,
  # which the Makefile's failure grep picks up.
  local t
  for t in shared/scripts/tests/*.sh; do
    [ -e "$t" ] || continue
    echo "=== script test: $t ===" >> "$LOGFILE"
    if ! bash "$t" >> "$LOGFILE" 2>&1; then
      echo "FAIL script test: $t" >> "$LOGFILE"
      status=1
    fi
  done
  return "$status"
}

# check_count — COUNT (make's COUNT=N) repeats the run N times, e.g. to check
# a flake fix. It replaces a bare `go test -count=N`, which agents may not run.
check_count() {
  if [ -n "$COUNT" ] && ! [[ "$COUNT" =~ ^[1-9][0-9]*$ ]]; then
    echo "seal-test.sh: COUNT must be a positive integer, got '$COUNT'" >&2
    exit 2
  fi
}

# run_integration — the whole L2 suite, or (RUN set) only the matching tests,
# optionally repeated COUNT times (foci_todo #2044). The timeout scales with
# COUNT so a repeat loop isn't cut off by the single-pass budget.
run_integration() {
  check_count
  local count="${COUNT:-1}"
  env_header "sealed integration suite${RUNFILTER:+ -run $RUNFILTER} -count=$count"
  local extra=(-count="$count" -timeout "$((600 * count))s")
  local filter=()
  [ -n "$RUNFILTER" ] && filter=(-run "$RUNFILTER")
  "${SEAL[@]}" "${TESTENV[@]}" nice -n 19 go test -tags=integration "${extra[@]}" "${filter[@]}" \
    -parallel="$PARALLEL" -v ./test/integration/... ./internal/testharness/... >> "$LOGFILE" 2>&1
  local status=$?

  # A filter that matches nothing "passes" with zero tests run — refuse that
  # rather than report a green that tested nothing.
  if [ -n "$RUNFILTER" ] && ! grep -q '^=== RUN' "$LOGFILE"; then
    echo "seal-test.sh: RUN='$RUNFILTER' matched no integration tests" | tee -a "$LOGFILE" >&2
    return 1
  fi

  diagnostic_rerun -tags=integration "${filter[@]}"
  return "$status"
}

# run_one — the fail-arm iteration target (foci_todo #1709): one package
# (required), optionally narrowed with -run, through the IDENTICAL
# SEAL/TESTENV arrays run_unit uses — never re-derive them here. RUNFILTER
# empty means "whole package", same as `go test ./pkg/` with no -run. VERBOSE
# (foci_todo #1982) adds -v so t.Logf reaches LOGFILE even on a PASS.
run_one() {
  if [ -z "$PKG" ]; then
    echo "seal-test.sh: mode 'one' requires PKG (arg 8), e.g. ./internal/agent/" >&2
    exit 2
  fi
  check_count
  env_header "sealed single-package run: $PKG${RUNFILTER:+ -run $RUNFILTER}${VERBOSE:+ -v} -count=${COUNT:-1}"
  local extra=(-count="${COUNT:-1}")
  [ -n "$RUNFILTER" ] && extra+=(-run "$RUNFILTER")
  [ -n "$VERBOSE" ] && extra+=(-v)
  "${SEAL[@]}" "${TESTENV[@]}" nice -n 19 go test "${extra[@]}" "$PKG" >> "$LOGFILE" 2>&1
  local status=$?

  diagnostic_rerun
  return "$status"
}

case "$MODE" in
unit) run_unit ;;
integration) run_integration ;;
one) run_one ;;
*)
  echo "seal-test.sh: unknown mode '$MODE' (want unit|integration|one)" >&2
  exit 2
  ;;
esac
