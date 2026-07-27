# Reproducing a `make test` failure outside the harness

## Trigger

A test fails under `make test` (often only during a `make land` gate) but you cannot reproduce it
with a plain `go test`. The difference between the two **is** the answer — and it lives in
`scripts/seal-test.sh`, not in the test.

## The environment `make test` actually uses

`make test` never runs `go test` directly. It calls `scripts/seal-test.sh unit …`, which does two
things a bare `go test` does not:

1. **Redirects the environment** — `TMPDIR`, `FOCI_TMPDIR`, `FOCI_TEST_TMPDIR`, `HOME`, `GOCACHE`,
   `GOMODCACHE`, `GOPATH` are all overridden (`HOME` → `$TESTDIR/home`, so tests cannot scribble
   into the real account's home — #1521).
2. **Seals the whole process tree under Landlock** via `bin/llbox -w <whitelist>`, so every write
   outside `$TESTDIR` / the go caches / a few `/dev` nodes is denied. Rules are inherited across
   fork+exec, so one seal at the top covers `go test`, every package binary, and anything they
   spawn (tmux, git, chrome…).

**Read `TESTENV` out of `seal-test.sh` and copy it verbatim.** Do not reconstruct it — it has
grown over time and the variable you omit is the one that mattered. As of #1523 it is:

```bash
TESTDIR=$(mktemp -d /tmp/fgw/probe-XXXXXX); mkdir -p "$TESTDIR/home"
GC=$(go env GOCACHE); GMC=$(go env GOMODCACHE); GP=$(go env GOPATH)
WL="$TESTDIR,$GC,$GMC,/dev/null,/dev/ptmx,/dev/pts,/dev/shm"
TESTENV=(env "TMPDIR=$TESTDIR" "FOCI_TMPDIR=$TESTDIR" "FOCI_TEST_TMPDIR=$TESTDIR" \
  "HOME=$TESTDIR/home" "GOCACHE=$GC" "GOMODCACHE=$GMC" "GOPATH=$GP")

# one package, sealed, exactly as make test would run it
"${TESTENV[@]}" bin/llbox -w "$WL" -- go test -C /home/rich/git/foci ./internal/tools/tmux/ -count=1
```

`FOCI_TEST_UNSEALED=1` makes `seal-test.sh` skip Landlock entirely — the cheapest way to split
"sandbox artifact" from "real bug" without hand-building the command above.

## Isolating which ingredient matters

Run the arms separately; each rules out one variable. Compare **rates**, not single runs.

| Arm | Isolates |
|---|---|
| `go test -run <Test> -count=8` | the test's own logic |
| full package, idle | intra-package interference |
| full package + `nproc+2` `nice -19` spinners | CPU contention |
| full package, sealed under `llbox` (above) | the Landlock seal |
| full `make test` | whole-suite context (~70 packages in parallel) |

A failure that only appears in the last row needs the whole-suite context and no single ingredient
reproduces it — say that plainly rather than picking whichever arm you ran last.

Note the spinner arm is **weak**: `nice -19` yields to everything, so it under-loads compared to
real parallel test binaries competing at equal priority.

## Gotchas that cost real time

- **`go test` without `FOCI_TMPDIR` panics** with a guard telling you to set it — and the panic
  **aborts the whole package**, so a "deterministic failure across 3 runs" can be three runs of
  nothing. Always set it.
- **Never rely on inherited cwd.** Background tool invocations do not reliably inherit the previous
  one's directory; a bare `go test ./internal/...` can run from your agent home and die with
  `cannot find main module, but found .git/config in /home/foci`. Always `go test -C <repo>`.
- **`go` needs a writable `TMPDIR`.** Sealing without it fails at
  `go: creating work dir: mkdir /tmp/go-build…: permission denied` before a single test runs.
- **Never pipe the run into `tail`/`grep`.** The pipe reports the *filter's* status; a failed
  `make land` is notified as exit 0. Redirect to a file and read that.
- **Confirm the arm actually ran.** A run that never executed and a clean pass look identical from
  the exit code. Look for the positive evidence — `ok <pkg> <time>`, a `--- FAIL` line, an artifact
  on disk.
- **`make test` from the main checkout is its own trap.** Historically the `HOME` redirect dropped
  `~/.gitconfig`'s `safe.directory` exception, so `git` refused the rich-owned checkout under the
  foci runner and `buildvcs` stamping failed 13 tests with
  `error obtaining VCS status: exit status 128`. Fixed in `fd8bb5df` (#1561) with
  `-buildvcs=false`, but the shape recurs: **anything shelling out to `git` behaves differently
  under the redirected HOME.**

## Reading the result

`seal-test.sh` re-runs any failing package **unsealed** and prints
`DIAGNOSTIC: <pkg> passes UNSEALED — it is writing outside the sandbox`. That is a real experiment
(same commit, minutes apart) and worth trusting as evidence — but it identifies *a* blocked write,
not necessarily the cause of the failure. Confirm the seal is sufficient on its own before
concluding it, or you will chase a whitelist gap that has nothing to do with the bug.

Only a `--- FAIL:` line is a failure verdict. A `foo_test.go:NNN:` line can be a benign `t.Logf`
however alarming its wording, and a value in a log line may be an injected test fake — those are
marked `FAKE-TEST` since #1562, but only where the fake is a *string*; values rendered from
injected numbers still read as real events.
