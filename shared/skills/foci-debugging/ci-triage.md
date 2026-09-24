<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Triaging a red-main CI-runner notification — "foci `<commit>` tests FAILED"

Not "why can't I reproduce it?" (that is `test-harness.md`) but "who broke main?"

**The commit named in the `[foci-test-runner]` notification is whatever was HEAD when the runner
ran — usually NOT the cause.** Several sessions land to a shared `main`; treat the name as a
timestamp, not an accusation. Method, in order:

0. **FIRST, check for a same-commit flip** — `grep ',unit,' ~/git/ci-runner/results.csv | tail -15`.
   If the SAME sha shows `pass` then later `fail`, no commit caused this and steps 2–4 can only find
   nothing: the machine changed between runs (see below). One query saves a bisect that can't converge.
1. **Read the real failure, not the summary.** `grep -A25 <TestName> /tmp/fgw/test-<ts>.log` (path is
   in the notification) for the actual assertion and its got-vs-want.
   **In a PARALLEL package (all of `test/integration`) the `--- FAIL:` line is a bare verdict.** Each
   test's output sits in `=== NAME  <TestName>` blocks, possibly hundreds of lines earlier and usually
   several: one with the `t.Errorf`, a later one with the `gateway.go:NNN: foci-gw stderr:` dump.
   Read both — the causal clue is usually in stderr (e.g. `WARN [modelinfo] no pricing for model`
   explaining a $0.00 total). Grep the bare test name, never `--- FAIL: <TestName>`.
2. **Can the named commit even reach the failing package?** `git show <commit> --stat`. If its diff
   cannot touch that package, it is innocent and the cause is upstream.
3. **Bound the culprit range.** Last-green from `~/git/ci-runner/results.csv` (`,foci,` and `,unit,`),
   then `git log --oneline <last-green>..<failing> -- <failing/package/>`.
4. **Settle flake-vs-real with a control, never a guess:** `make test-one PKG=./pkg/ RUN=TestX
   COUNT=10`, plus a serialized low-load run (`-parallel=1`) to rule out self-induced timing.
   Deterministic ≠ your fault — it can be the tree's state.

## Recurring causes

- **A shared-semantics change breaks a cross-package assertion.** A commit reworks a shared helper
  and updates only its own package's test. When you change a shared function, grep other packages
  for tests asserting the old behaviour.
- **Error-string reformatting.** Wrapping a transport (e.g. `ratelimit.Transport`) makes net/http's
  `Client.Timeout` wrapper reword the error, so `strings.Contains(err, "deadline exceeded")` goes red
  while `errors.Is(err, context.DeadlineExceeded)` and `net.Error.Timeout()` stay true. Verify with
  a throwaway `_test.go`; assert **semantics (`errors.Is`), never message text**.
- **The test encodes a contract the commit deliberately CHANGED.** The failure points at the file the
  commit touched, so "fix the code until green" would undo the fix. If the test asserts on a field or
  default the commit demoted, the test is stale (bit twice on `TestL2_SlashCommands_CostTodayReadsAPILog`).
- **Fix a stale fixture to exercise the NEW path, not route around it.** After #1674 priced from
  token deltas, a fixture still seeding cost via the ignored `golden_cost_usd` would go green covering
  nothing; seed a model with real pricing instead of a pre-computed total.
- **Local main ahead of origin.** A peer committed to the shared checkout without pushing, so the
  runner tests a red local HEAD while `origin/main` is green. Check
  `git rev-list --left-right --count origin/main...HEAD`; fix on a worktree off local HEAD, ff local
  main, push. (`make land` exists to prevent this.)

## Environmental cause: a DEPLOY changed the machine under the tests

A same-commit flip often means foci was **installed** between the runs. Code resolving a foci-own
binary — `exec.LookPath("foci-cc-hook")`/`("foci-codex-hook")`, an `os.Executable()` sibling — finds
the deployed `/usr/local/bin` copy: under test `os.Executable()` is a temp binary, so the sibling
check misses and falls through to `$PATH`, and the unit suite silently reads the install.

Confirm causally with two arms, not by correlation — different verdicts = the test reads the install:
```
env PATH=/usr/bin:/bin make -C <repo> test-one PKG=./pkg/ RUN=TestX
make -C <repo> test-one PKG=./pkg/ RUN=TestX
```
Then date it: `ls -la --time-style=full-iso $(command -v foci-codex-hook)` vs the last-green CI time.
Third-party lookups (`jq`, `rg`, `tmux`, `bash`, `python3`, `codex`) don't change when we deploy —
**only foci-own binaries move under you.** The fix belongs in `internal/delegator/hookbin`, whose
`Resolve()` disables both lookups under `testing.Testing()`; a `make lint` gate forbids raw
`exec.LookPath("foci-*")` in production code.

Two traps from that fix, both reusable:

- **A `t.Skipf` keyed on environment is a coverage hole that reports green.** A test skipped itself
  whenever the sibling binary existed, so on any deployed machine it asserted nothing. Grep `t.Skipf`
  when a test touches PATH or the filesystem; inject the environment reads as parameters so the
  behaviour is assertable everywhere — don't delete the test.
- **An assertion counting a PROXY must not name a mechanism it cannot observe.** "initialize count =
  2 — the batch spawned its own process" was wrong: the second `initialize` came from a trust probe.
  Make the message describe only what the assertion can actually distinguish.
