<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Reproducing a `make test` failure outside the harness

## Trigger

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

**To run one package under the harness env, use `make test-one PKG=./internal/<pkg>/ [RUN=<Name>]
[COUNT=N]`.** `COUNT=N` is `-count` for flake checks. Runs are always `-v` (#2348), so a passing
test's `t.Logf` lines are logged. It uses the exact `TESTENV`/seal of `make test`, all of it — to find
which ingredient matters, use the manual recipe and table below.

**Agents cannot run bare `go build`/`test`/`vet`** (`permissions.deny`, incl. `/usr/bin/go` and
`go -C`): use `make build`/`vet`/`test-one`, which take the `/tmp/heavy` lock. The manual recipe is for
a human at a terminal.

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
  packages start first (`SLOW_FIRST` in `seal-test.sh`), so a test meets more concurrent load.
- **Every binary runs under `scripts/test-memcap.sh`** (`go test -exec`). On macOS that wrapper
  runs it from an MD5-named copy in `~/Library/Caches/foci-test-bins`, so `os.Args[0]` is that
  path; `FOCI_TEST_BIN_REUSE=0` runs the binary go built.
- **The shell-script suites run at the same time as each other and as `go test`.**

`FOCI_TEST_UNSEALED=1` makes `seal-test.sh` (and `make test-one`) skip Landlock — the cheapest
split of "sandbox artifact" from "real bug".

## Isolating which ingredient matters

Run the arms separately; compare **rates**, not single runs.

| Arm | Isolates |
|---|---|
| `make test-one PKG=<pkg> RUN=<Test> COUNT=8` | the test's own logic |
| `make integration RUN=<Test> COUNT=20` (L2 tests: `test-one` lacks `-tags=integration`) | the test's own logic |
| full package, idle | intra-package interference |
| full package + `nproc+2` `nice -19` spinners | CPU contention |
| full package, sealed under `llbox` (above) | the Landlock seal |
| full `make test` | whole-suite context (~70 packages in parallel) |

A failure only in the last row needs the whole suite and no single ingredient reproduces it; say
so plainly rather than blaming whichever arm you ran last.

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
