<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Reproducing a `make test` failure outside the harness

A test fails under `make test` (often in a `make land` gate) but not under a plain `go test`. The
difference **is** the answer, and it lives in `scripts/seal-test.sh`, not in the test.

## The environment `make test` actually uses

`make test` never runs `go test` directly. It calls `scripts/seal-test.sh unit …`, which does two
things a bare `go test` does not:

1. **Replaces the environment** — `env -i` (#2348): only what `TESTENV` lists gets through (system
   `PATH`, `USER`, `PWD`, `GO*`/`CGO_*`, `OPTIN_VARS`, the tmp/cache vars, `HOME` → `$TESTDIR/home`).
   Passes in your shell, fails here → a tool outside the system dirs, or an env var not passed.
2. **Seals the whole process tree under Landlock** via `bin/llbox -w <whitelist>`, so every write
   outside `$TESTDIR` / the go caches / a few `/dev` nodes is denied. Rules are inherited across
   fork+exec, so one seal at the top covers `go test`, every package binary, and anything they
   spawn (tmux, git, chrome…).

**One package under the harness env: `make test-one PKG=./internal/<pkg>/ [RUN=<Name>] [COUNT=N]`**
(always `-v`; same `TESTENV`/seal as `make test`). Agents cannot run bare `go build`/`test`/`vet`
(`permissions.deny`): the manual recipe below is for humans.

**To peel a layer off, copy `TESTENV` verbatim from `seal-test.sh`, never reconstruct it** (the
variable you omit is the one that mattered). Currently:

```bash
TESTDIR=$(mktemp -d /tmp/fgw/probe-XXXXXX); mkdir -p "$TESTDIR/home"
GC=$(go env GOCACHE); GMC=$(go env GOMODCACHE); GP=$(go env GOPATH)
WL="$TESTDIR,$GC,$GMC,/dev/null,/dev/ptmx,/dev/pts,/dev/shm"
TESTENV=(env -i "PATH=$(go env GOROOT)/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  "USER=$(id -un)" "PWD=$PWD" "TMPDIR=$TESTDIR" "FOCI_TMPDIR=$TESTDIR" "FOCI_TEST_TMPDIR=$TESTDIR" \
  "HOME=$TESTDIR/home" "GOCACHE=$GC" "GOMODCACHE=$GMC" "GOPATH=$GP")

# one package, sealed (= make test-one; kept as the base to peel layers off)
"${TESTENV[@]}" bin/llbox -w "$WL" -- go test -C /home/rich/git/foci ./internal/tools/tmux/ -count=1
```

Other differences from a bare `go test` (#2551), each a place to look when only the suite fails:
- **Linux: `$TESTDIR` is `/dev/shm/fgw-run-<user>` (tmpfs)**, not `/tmp/fgw/run-<user>`, so an
  fsync costs nothing. `FGW_RUN_PREFIX=/tmp/fgw/run-` puts it back on disk.
- **`-p` is 1.5 x the cores on Linux** (`NPROC`, 6 on the NUC), 8 on the Mac, and the slowest
  packages start first (`SLOW_FIRST` in `seal-test.sh`), so tests meet more load.
- **Every binary runs under `scripts/test-memcap.sh`** (`-exec`, 1 GB cap on Linux), which passes
  `-test.testlogfile` itself so results stay cached (#2559): unchanged packages say `(cached)`;
  `make test COUNT=1` runs all. On macOS it runs an MD5-named copy from
  `~/Library/Caches/foci-test-bins` (`os.Args[0]`; off: `FOCI_TEST_BIN_REUSE=0`).
- **The shell-script suites run in parallel with each other and `go test`.**

`FOCI_TEST_UNSEALED=1` makes `seal-test.sh` (and `make test-one`) skip Landlock — the cheapest
split of "sandbox artifact" from "real bug".

## Isolating which ingredient matters

Run the arms separately. Compare **rates**, not single runs.

| Arm | Isolates |
|---|---|
| `make test-one PKG=<pkg> RUN=<Test> COUNT=8` | the test's own logic |
| `make integration RUN=<Test> COUNT=20` (L2 tests: `test-one` lacks `-tags=integration`) | the test's own logic |
| full package, idle | intra-package interference |
| full package + `nproc+2` `nice -19` spinners | CPU contention |
| full package, sealed under `llbox` (above) | the Landlock seal |
| full `make test` | whole-suite context (~70 packages in parallel) |

A failure only in the last row needs the whole suite: say so, do not blame the last arm you ran.
The spinner arm is **weak**: `nice -19` under-loads compared with real test binaries.

## Gotchas that cost real time

- **`go test` without `FOCI_TMPDIR` panics**, and the panic
  **aborts the whole package**, so a "deterministic failure across 3 runs" can be three runs of
  nothing. Always set it.
- **Never rely on inherited cwd**: a bare run can die with `cannot find main module`. Use `make -C <repo> test-one …`.
- **`go` needs a writable `TMPDIR`.** Sealing without it fails at
  `go: creating work dir: mkdir /tmp/go-build…: permission denied` before a single test runs.
- **Never pipe the run into `tail`/`grep`** (the filter's status hides a failure); redirect to a file.
- **Confirm the arm ran**: look for `ok <pkg> <time>` or `--- FAIL`, not just the exit code.
- **`git` under the redirected HOME loses `~/.gitconfig`** (`safe.directory`), so it refuses the
  rich-owned checkout: seen as `error obtaining VCS status: exit status 128` (fix: `-buildvcs=false`).

## Reading the result

`seal-test.sh` re-runs a failing package unsealed, then (if that passed) sealed. Passing both prints
`DIAGNOSTIC: ... FLAKE`; failing sealed again prints `DIAGNOSTIC: ... writing outside the sandbox`.
That is real evidence, but it identifies *a* blocked write, not necessarily the cause. Confirm the
seal alone is sufficient before concluding it, or you will chase an irrelevant whitelist gap.

Only a `--- FAIL:` line is a failure verdict. A `foo_test.go:NNN:` line can be a benign `t.Logf`, and a
logged value may be an injected fake (strings are marked `FAKE-TEST`, #1562; injected numbers are not).

For "who broke main?" — a red `[foci-test-runner]` notification — see `ci-triage.md`.
