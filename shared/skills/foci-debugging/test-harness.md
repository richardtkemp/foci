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
   into the real account's home — #1521).
2. **Seals the whole process tree under Landlock** via `bin/llbox -w <whitelist>`, so every write
   outside `$TESTDIR` / the go caches / a few `/dev` nodes is denied. Rules are inherited across
   fork+exec, so one seal at the top covers `go test`, every package binary, and anything they
   spawn (tmux, git, chrome…).

**If you just want one package run under the harness env — not to isolate a specific ingredient —
use `make test-one PKG=./internal/<pkg>/ [RUN=<Name>] [V=1] [COUNT=N]` (foci_todo #1709) instead of anything
below.** `COUNT=N` repeats the run N times (go test's `-count`), for flake checks. `V=1` adds go test's own `-v`, so a PASSING test's `t.Logf` lines actually land in the log
(foci_todo #1982) — without it a pass swallows them silently, and forcing a `t.Errorf` just to see
them turns a passing test into a fake failure.

**Agents cannot run a bare `go build`/`go test`/`go vet` (Dick, 2026-09-23).** Claude Code's
`permissions.deny` refuses them, including the `/usr/bin/go` and `go -C <dir>` forms. Use
`make build`, `make vet`, `make test-one`: those take the `/tmp/heavy` lock. The manual recipe
below is reference for a human at a terminal.
It calls `scripts/seal-test.sh one`, which reuses the exact same `TESTENV`/seal construction as
`make test` — so it cannot omit a variable by hand-rolling mistake. The manual reconstruction below
still earns its place for the isolation table further down (running WITHOUT the seal, WITHOUT the
env redirect, etc., to find out which ingredient a failure actually depends on) — `test-one` cannot
do that, because it always applies the full harness env by design.

**When you DO need to peel a layer off (the isolation table below), read `TESTENV` out of
`seal-test.sh` and copy it verbatim. Do not reconstruct it** — it has grown over time and the
variable you omit is the one that mattered. As of #1523 it is:

```bash
TESTDIR=$(mktemp -d /tmp/fgw/probe-XXXXXX); mkdir -p "$TESTDIR/home"
GC=$(go env GOCACHE); GMC=$(go env GOMODCACHE); GP=$(go env GOPATH)
WL="$TESTDIR,$GC,$GMC,/dev/null,/dev/ptmx,/dev/pts,/dev/shm"
TESTENV=(env "TMPDIR=$TESTDIR" "FOCI_TMPDIR=$TESTDIR" "FOCI_TEST_TMPDIR=$TESTDIR" \
  "HOME=$TESTDIR/home" "GOCACHE=$GC" "GOMODCACHE=$GMC" "GOPATH=$GP")

# one package, sealed, exactly as make test would run it (this specific arm is now
# `make test-one PKG=./internal/tools/tmux/` — kept here only as the ingredient this
# manual form isolates: sealed-but-not-whole-suite)
"${TESTENV[@]}" bin/llbox -w "$WL" -- go test -C /home/rich/git/foci ./internal/tools/tmux/ -count=1
```

`FOCI_TEST_UNSEALED=1` makes `seal-test.sh` (and `make test-one`) skip Landlock entirely — the
cheapest way to split "sandbox artifact" from "real bug" without hand-building the command above.

## Isolating which ingredient matters

Run the arms separately; each rules out one variable. Compare **rates**, not single runs.

| Arm | Isolates |
|---|---|
| `make test-one PKG=<pkg> RUN=<Test> COUNT=8` | the test's own logic |
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
  `cannot find main module, but found .git/config in /home/foci`. Always `make -C <repo> test-one …`.
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

## Triaging a red-main CI-runner notification — "foci `<commit>` tests FAILED"

Different problem from the one above: not "why can't I reproduce it?" but "who broke main?"

**The commit named in the `[foci-test-runner]` notification is whatever was HEAD when the runner
ran — usually NOT the cause.** Several sessions land to a shared `main`; the runner just caught the
tip. Treat the name as a timestamp, not an accusation. Method, in order:

0. **FIRST, check for a same-commit flip — it decides whether any of the rest is worth doing.**
   ```
   grep ',unit,' ~/git/ci-runner/results.csv | tail -15
   ```
   If the SAME commit sha appears as `pass` and later as `fail`, **no commit caused this** and steps
   2–4 can only find nothing. The cause is environmental: something on the machine changed between
   the two runs. Costs one query; saves a bisect that was never going to converge. (2026-08-06:
   `448192bf` pass 16:17, fail 11:24/11:30/11:34 the next morning.)
1. **Read the real failure, don't trust the summary.** `grep -A25 <TestName> /tmp/fgw/test-<ts>.log`
   (the log path is in the notification). Get the actual assertion and its got-vs-want.
2. **Can the named commit even reach the failing package?** `git show <commit> --stat`. If its diff
   cannot touch the failing package (a log-only commit vs `internal/tools`, say), it is innocent and
   the cause is upstream of it.
3. **Bound the culprit range.** Find last-green in `~/git/ci-runner/results.csv` (grep for `,foci,`
   and `,unit,`), then
   `git log --oneline <last-green>..<failing> -- <failing/package/>` for the commit that actually
   touched it.
4. **Settle flake-vs-real with a control, never a guess.** Reproduce the single test
   (`make test-one PKG=./pkg/ RUN=TestX COUNT=10`), and a serialized low-load run (`-parallel=1`) to rule out
   self-induced timing. Deterministic ≠ your fault — it can be the tree's state.

**Recurring causes** (three in one day, 2026-07-21):

- **A shared-semantics change breaks a cross-package assertion.** A commit reworks a shared helper
  and updates *its own* package's test, missing an over-specific assertion in another package.
  When you change a shared function, grep other packages for tests asserting the old behaviour.
- **Error-string reformatting.** Wrapping a transport (e.g. `ratelimit.Transport`) makes net/http's
  `Client.Timeout` wrapper reword the message, so a `strings.Contains(err, "deadline exceeded")`
  test goes red — while `errors.Is(err, context.DeadlineExceeded)` and `net.Error.Timeout()` stay
  TRUE. Verify empirically with a throwaway `_test.go`, then fix the test to assert **semantics
  (`errors.Is`), never message text**.
- **Local main ahead of origin.** A peer committed to the shared main checkout without pushing, so
  the runner tests a red local HEAD while `origin/main` sits green-but-stale. Check with
  `git rev-list --left-right --count origin/main...HEAD`. Fix on a worktree off local HEAD, ff local
  main, push. (`make land` exists to prevent this class.)

### Environmental cause: a DEPLOY changed the machine under the tests

The cause behind a same-commit flip (step 0) is often that foci was **installed** between the two
runs. Production code that resolves a foci-own binary — `exec.LookPath("foci-cc-hook")`,
`exec.LookPath("foci-codex-hook")`, an `os.Executable()` sibling — finds whatever is deployed at
`/usr/local/bin`, so the unit suite silently reads the install. Under test `os.Executable()` is a
temp binary, so the sibling check always misses and it falls through to `$PATH`.

Confirm causally with a two-arm run rather than by correlation:

```
export FOCI_TMPDIR=$(mktemp -d)                       # else the guard panics, see Gotchas
env PATH=/usr/bin:/bin FOCI_TMPDIR=$FOCI_TMPDIR /usr/bin/go test ./pkg/ -run TestX -count=1
go test -C <repo> ./pkg/ -run TestX -count=1
```
Different verdicts from the two arms = the test is reading the install. Then date it:
`ls -la --time-style=full-iso $(command -v foci-codex-hook)` against the last-green CI timestamp.

Third-party lookups (`jq`, `rg`, `tmux`, `bash`, `python3`, `codex`) are NOT this class — they don't
change when we deploy. **Only foci-own binaries move under you.**

Fix belongs in `internal/delegator/hookbin`, whose `Resolve()` disables both lookups under
`testing.Testing()`; a `make lint` gate forbids raw `exec.LookPath("foci-*")` in production code.

Two traps found while fixing this, both worth reusing:

- **A `t.Skipf` keyed on environment is a coverage hole that reports green.** `TestResolveHookBinary_
  SiblingFound` skipped itself whenever the sibling existed — so on any deployed machine it asserted
  nothing, indefinitely, while reading as a pass. Grep `t.Skipf` whenever a test touches PATH or the
  filesystem. Fix by injecting the environment reads as parameters so the behaviour is assertable
  everywhere, not by deleting the test.
- **An assertion counting a PROXY must not name a mechanism it cannot observe.** "app-server
  initialize count = 2 — the batch spawned its own process" was wrong: there was one `launching:`
  line; the second `initialize` came from a trust probe. The message sends the next reader into the
  wrong subsystem. Check the message describes what the assertion can actually distinguish.
