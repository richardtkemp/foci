<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

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
   into the real account's home).
2. **Seals the whole process tree under Landlock** via `bin/llbox -w <whitelist>`, so every write
   outside `$TESTDIR` / the go caches / a few `/dev` nodes is denied. Rules are inherited across
   fork+exec, so one seal at the top covers `go test`, every package binary, and anything they
   spawn (tmux, git, chrome…).

**To run one package under the harness env, use `make test-one PKG=./internal/<pkg>/ [RUN=<Name>]
[V=1] [COUNT=N]`.** `COUNT=N` repeats the run (go test's `-count`) for flake checks. `V=1` adds `-v`,
so a PASSING test's `t.Logf` lines reach the log — without it a pass swallows them, and forcing a
`t.Errorf` just to see them fakes a failure. It calls `scripts/seal-test.sh one`, the exact
`TESTENV`/seal construction `make test` uses, so it cannot omit a variable. It always applies the full
env, though — to find which ingredient a failure depends on, use the manual recipe and isolation
table below.

**Agents cannot run bare `go build`/`test`/`vet`** (`permissions.deny`, incl. `/usr/bin/go` and
`go -C`): use `make build`/`vet`/`test-one`, which take the `/tmp/heavy` lock. The manual recipe is for
a human at a terminal.

**To peel a layer off, copy `TESTENV` verbatim out of `seal-test.sh` — never reconstruct it**; it
grows, and the variable you omit is the one that mattered. Currently:

```bash
TESTDIR=$(mktemp -d /tmp/fgw/probe-XXXXXX); mkdir -p "$TESTDIR/home"
GC=$(go env GOCACHE); GMC=$(go env GOMODCACHE); GP=$(go env GOPATH)
WL="$TESTDIR,$GC,$GMC,/dev/null,/dev/ptmx,/dev/pts,/dev/shm"
TESTENV=(env "TMPDIR=$TESTDIR" "FOCI_TMPDIR=$TESTDIR" "FOCI_TEST_TMPDIR=$TESTDIR" \
  "HOME=$TESTDIR/home" "GOCACHE=$GC" "GOMODCACHE=$GMC" "GOPATH=$GP")

# one package, sealed (= make test-one; kept as the base to peel layers off)
"${TESTENV[@]}" bin/llbox -w "$WL" -- go test -C /home/rich/git/foci ./internal/tools/tmux/ -count=1
```

`FOCI_TEST_UNSEALED=1` makes `seal-test.sh` (and `make test-one`) skip Landlock — the cheapest
split of "sandbox artifact" from "real bug".

## Isolating which ingredient matters

Run the arms separately; each rules out one variable. Compare **rates**, not single runs.

| Arm | Isolates |
|---|---|
| `make test-one PKG=<pkg> RUN=<Test> COUNT=8` | the test's own logic |
| `make integration RUN=<Test> COUNT=20` (L2 tests: `test-one` lacks `-tags=integration`) | the test's own logic |
| full package, idle | intra-package interference |
| full package + `nproc+2` `nice -19` spinners | CPU contention |
| full package, sealed under `llbox` (above) | the Landlock seal |
| full `make test` | whole-suite context (~70 packages in parallel) |

A failure that only appears in the last row needs the whole-suite context and no single ingredient
reproduces it — say that plainly rather than picking whichever arm you ran last.

The spinner arm is **weak**: `nice -19` yields to everything, under-loading vs real parallel test
binaries at equal priority.

## Gotchas that cost real time

- **`go test` without `FOCI_TMPDIR` panics** with a guard telling you to set it — and the panic
  **aborts the whole package**, so a "deterministic failure across 3 runs" can be three runs of
  nothing. Always set it.
- **Never rely on inherited cwd.** Background tool invocations do not reliably inherit the previous
  one's directory; a bare `go test ./internal/...` can run from your agent home and die with
  `cannot find main module, but found .git/config in /home/foci`. Always `make -C <repo> test-one …`.
- **`go` needs a writable `TMPDIR`.** Sealing without it fails at
  `go: creating work dir: mkdir /tmp/go-build…: permission denied` before a single test runs.
- **Never pipe the run into `tail`/`grep`.** The pipe reports the *filter's* status; a failed
  `make land` is notified as exit 0. Redirect to a file and read that.
- **Confirm the arm actually ran.** A run that never executed and a clean pass look identical from
  the exit code. Look for the positive evidence — `ok <pkg> <time>`, a `--- FAIL` line, an artifact
  on disk.
- **Anything shelling out to `git` behaves differently under the redirected HOME** — it loses
  `~/.gitconfig` (e.g. `safe.directory`, so git refuses the rich-owned checkout under the foci
  runner). Once seen as `error obtaining VCS status: exit status 128` (fixed with `-buildvcs=false`).

## Reading the result

`seal-test.sh` re-runs any failing package **unsealed** and prints
`DIAGNOSTIC: <pkg> passes UNSEALED — it is writing outside the sandbox`. That is real evidence (same
commit, minutes apart) — but it identifies *a* blocked write, not necessarily the cause. Confirm the
seal alone is sufficient before concluding it, or you will chase an irrelevant whitelist gap.

Only a `--- FAIL:` line is a failure verdict. A `foo_test.go:NNN:` line can be a benign `t.Logf`
however alarming its wording, and a value in a log line may be an injected test fake — those are
marked `FAKE-TEST` since #1562, but only where the fake is a *string*; values rendered from
injected numbers still read as real events.

For "who broke main?" — a red `[foci-test-runner]` notification — see `ci-triage.md`.
