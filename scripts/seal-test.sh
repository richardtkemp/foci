#!/usr/bin/env bash
# seal-test.sh — runs the `make test` / `make integration` go-test invocation
# sealed under a Landlock write-whitelist BY DEFAULT (foci_todo #1523,
# implementing the investigation in #1517). Invoked by the Makefile, not run
# directly by a person.
#
# Usage: seal-test.sh <unit|integration> <TESTDIR> <LOGFILE> <parallel-n> \
#          <GOCACHE_PIN> <GOMODCACHE_PIN> <GOPATH_PIN>
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

MODE="${1:?usage: seal-test.sh <unit|integration> <TESTDIR> <LOGFILE> <parallel-n> <GOCACHE_PIN> <GOMODCACHE_PIN> <GOPATH_PIN>}"
TESTDIR="${2:?}"
LOGFILE="${3:?}"
PARALLEL="${4:?}"
GOCACHE_DIR="${5:?}"
GOMODCACHE_DIR="${6:?}"
GOPATH_DIR="${7:?}"

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

run_unit() {
  echo "=== sealed unit suite ===" >> "$LOGFILE"
  "${SEAL[@]}" "${TESTENV[@]}" nice -n 19 go test -p="$PARALLEL" -parallel=16 ./... >> "$LOGFILE" 2>&1
  local status=$?

  diagnostic_rerun
  return "$status"
}

run_integration() {
  echo "=== sealed integration suite ===" >> "$LOGFILE"
  "${SEAL[@]}" "${TESTENV[@]}" nice -n 19 go test -tags=integration -count=1 -timeout 600s \
    -parallel="$PARALLEL" -v ./test/integration/... ./internal/testharness/... >> "$LOGFILE" 2>&1
  local status=$?

  diagnostic_rerun -tags=integration
  return "$status"
}

case "$MODE" in
unit) run_unit ;;
integration) run_integration ;;
*)
  echo "seal-test.sh: unknown mode '$MODE' (want unit|integration)" >&2
  exit 2
  ;;
esac
